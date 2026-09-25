# An isolated arena — 2026-09-25

**Status: planned. Nothing is built. TASK-2 waits for the owner's decision. The
decisions are at the end.**

## The hole

Each pager keeps every resident page in one memfd, the arena
(`vmmemory.LinuxArena`, made in `host/supervisor_linux.go`). ATTACH sends that
memfd to every VMM that attaches a memory region (`Connect` in
`vmmemory/connection_linux.go`). The memfd is sealed against shrinking, growing
and further seals (`vmwire.ArenaMemfd`), and against nothing else. The VMM's
descriptor is read-write.

The protocol does not need this. The client maps the runs the pager names, at
the offsets the pager names. But the descriptor lets the VMM map any offset
itself.

## Threat model

An embedder runs Firecracker under a jailer because it runs untrusted tenants'
guests. A guest that escapes into its VMM is the case the jailer is for. So this
plan assumes a compromised VMM: code of the attacker's choosing, running in the
VMM process, with every descriptor the VMM holds and every system call its
seccomp policy allows.

### What a compromised VMM can do today

Through the arena descriptor, on the RAM pager and on the PMEM pager alike:

- Read every resident page on the pager. That is every VM's dirty pages, its
  sealed pages, and every published page, of every tenant.
- Write any of them. A write into another VM's dirty page changes that VM's
  memory. A write into a sealed page while its checkpoint uploads puts the
  attacker's bytes into that VM's durable state, which outlives the host and
  moves with the VM. A write into a published page changes every VM that maps
  it, and every VM that inherits it on this host later.
- Punch any page out. The VMs that map it read zeros, and so do the pager's own
  spill and upload of it.
- Allocate any hole. That takes pod memory, or HugeTLB pool pages, that the
  pager never counted.
- Keep a mapping after it answers REVOKE. The pager then gives that slot to
  another VM's page, and the old mapping reaches it. This needs no descriptor
  once the mapping exists.

`vmmemory/hostile_linux_test.go` shows that the protocol holds against a hostile
VMM. Its header says it does not cover the descriptor.

### What it must not be able to do after

1. Read or write another VM's private pages: its dirty pages, its sealed pages,
   and the pages a fork point lends to children it is not one of. This holds
   inside a tenant too.
2. Write any page another memory region may map. That includes the published
   pages of its own VM.
3. Read a shared page of another tenant.
4. Reach a slot after the pager gives it to someone else, through a kept
   mapping or through a descriptor.
5. Make a page the pager shares differ from the bytes its identity names.

Inside its own tenant it can still read every published page resident on the
pager, including pages its VM does not inherit. The tenant is the trust domain
for shared pages. This is the one thing the design concedes, and it is the
owner's to accept (decision 2).

### What stays out of scope

- Its own VM's memory. The VMM runs the guest and can write that memory anyway.
- What its own descendants inherit. A fork trusts its parent's memory. A
  parent's VMM that writes a page before a child reads it changes what the child
  inherits. Its guest could have done the same before the pause.
- The CPU it costs the pager by faulting its own memory: TASK-20.
- Memory it allocates in its own private file behind the pager's back. The
  file's fixed size bounds it, the VMM's cgroup is charged for it, and the pager
  ends the session when it finds it (see accounting below).
- HugeTLB pool pages it takes for itself, by any means. The jailer's hugetlb
  cgroup limit bounds them, as it does today.
- A VMM that runs as the pager's user or as root. Such a VMM can reopen a
  read-only descriptor for writing through `/proc/self/fd`, or ptrace the host.
  The split protects a jailed VMM only (decision 4).

## Decision

Split each pager's arena into files by who may read them:

- **A private file per memory region.** It holds the region's private pages:
  dirty, sealed, written ahead, spilled back in, and loaded privately from a
  migration source. Only that region's VMM receives it, read-write.
- **A shared file per tenant.** It holds the tenant's pages that another memory
  region may map: pages loaded by identity, and published pages once another
  region maps them. The tenant's VMMs receive it read-only.
- **A fork file per fork point and memory region.** It holds the pages a fork
  point lends to same-host children. The children receive it read-only.

Only the pager writes the shared and fork files. A published page moves from a
private file to the shared file by copy, once, the first time another memory
region maps it. It carries a SHA-256 digest of the bytes its upload read, and
the move checks it. A mismatch means the VMM wrote a page it holds read-only,
and ends that session.

A private slot only ever holds its own region's pages. A shared slot only ever
holds its tenant's pages. A fork file is never reused. So a mapping kept past
REVOKE reaches nothing the VMM's descriptors do not already reach.

## Design

### Three kinds of file

| File | Holds | Who gets it | Size |
| --- | --- | --- | --- |
| private | the region's private pages, and its published pages until another region maps them | its VMM, read-write | twice the memory region |
| shared | one tenant's pages loaded by identity, and its published pages another region maps | the tenant's VMMs, read-only | the pages the tenant's attached regions map, grown at each attach |
| fork | the pages a fork point lends, as a same-host child's populate maps them | the children, read-only | the parent's memory region |

The spill file stays one per pager, and no VMM ever receives it.

Each file is a memfd of the pager's kind: HugeTLB for PMEM, an ordinary memfd for
RAM. The pager creates it, sets its mode to 0600, and seals it against shrinking
and against further seals. A private or fork file is also sealed against growing.
A shared file can grow, and only the pager's descriptor can grow it.

### Read-only descriptors, and how the VMM maps them

The pager sends a shared or fork file as a new open of the same memfd with
`O_RDONLY`, made through `/proc/self/fd`. With that descriptor the kernel refuses
a writable shared mapping, write(2), fallocate, ftruncate and new seals.

Two things follow from that.

First, the client cannot map a read-only file `MAP_SHARED` and register it with
userfaultfd. The kernel clears `VM_MAYWRITE` on a shared mapping of a read-only
file, and `UFFDIO_REGISTER` refuses a mapping without it. So the client maps a
read-only file `MAP_PRIVATE`, readable and writable. It registers the mapping for
missing, minor and write-protect faults, and write-protects it before it is
exposed, as it arms a shared page today. `UFFDIO_CONTINUE` installs the
page-cache page read-only in a private mapping. The page is still the pager's
page, so the sharing stays physical. A store traps on the write protection and
reaches the pager, which replaces the mapping as it does today.

A store the kernel let through would copy into anonymous memory the pager never
sees. So the write protection must be armed before the range is reachable, as it
is today, and the pager must never clear write protection on a range backed by a
read-only file. On HugeTLB the private mapping also needs `MAP_NORESERVE`.
Without it the kernel reserves pool pages for copies that never happen.

Step 1 proves each of these on the qualification kernels before anything else is
built.

Second, a reopen through `/proc/self/fd` checks the inode's mode against the
caller. A memfd is created with mode 0777, which is why the pager sets 0600. A
VMM that runs as another user then cannot reopen any file for writing. A VMM
that runs as the pager's user can. That is why the protection assumes a jailed
VMM.

### The tenant

The pager takes a tenant with each memory region, in the same way it takes the
kind: `MemoryRegionBacking.Tenant`, an opaque string. The host states it, and the
pager never infers it. The sharing index (`Host.clean`) is keyed by tenant and
page identity. The pager puts a page only in its own tenant's shared file, and a
region maps only its own tenant's pages.

TASK-18 puts a tenant prefix in every object key. It also forbids forks,
inheritance and page sharing across tenants. The host sets `Tenant` to that
prefix, and the pager checks it as well. Once a page identity names its tenant,
the pager refuses to index an identity whose tenant is not the region's, and
fails the fault. Until TASK-18 lands, every VM is in one tenant. The private
files close reach into other VMs' private pages, and every write to a shared
page, without it.

Templates follow TASK-18: a template belongs to a tenant. Two tenants that
start VMs from one image on one host each hold that image's resident pages
(decision 3).

### Where a page lives

| Page | File |
| --- | --- |
| a page loaded by identity: read fault, read-ahead, populate | the tenant's shared file |
| a store's copy, a write-ahead page, a page a mapping rule copied | the private file |
| a sealed page | wherever it was when sealed |
| a page a checkpoint published | the private file it was in, until another region maps it |
| a page a migration destination loads privately from its source | the private file |
| a page a spill brings back | the private file |
| a page a fork point lends to a same-host child | the fork file |
| an explicit zero | none, as today |

### Placement gets simpler

A private page with index i lives at offset i of its region's private file.
Private pages that are adjacent in the guest are adjacent in the file, in any
write order, so they are one mapping. Extents do this today. The private file
does it without them. `slots.Space` loses its extents. `Config.ArenaOffsets`
goes away, and so do `reclaimExtent` and the supervisor's sizing of RAM's offset
space by `LogicalPages`.

Offset i is the page's home. The second half of the file holds each page's other
place: offset N + i, for a region of N pages. A store that copies away from the
copy a checkpoint froze, or away from its own published page, takes the other
place. A page never needs a third place. When both are taken, one of them holds
a clean page the guest no longer maps, and the store gives that page up.

The second placement exception of today goes away. A migration destination's
private loads land at their own offsets, and those are consecutive anyway.

The gap rule and the half-private rule stay as they are. They read offsets of
the private file, which are page indexes. PMEM's page is the whole range, so
nothing changes for PMEM's placement.

### Copy-on-write across files

A store into a page mapped read-only copies it and replaces the guest's mapping,
as today. The page it copies from can be in any file: the shared file, a fork
file, or the private file itself. The copy always goes into the private file. So
a guest's stores reach only its own region's file, and so does the VMM's write
access.

The settle compares a sealed page with its origin, and the two may now be in
different files. The pager maps every file, so `Equal` stays one comparison in
place.

### A page a checkpoint publishes

Today the retire leaves a published page where it is and enters it in the
sharing index. With the split it still stays where it is, in its region's
private file, and the guest keeps mapping it. Nothing is copied at retire. Most
published pages are never mapped by another region, so most are never copied.

The index entry records that the page is in a private file. When another memory
region wants that identity, the pager moves the page:

1. It takes a slot in the tenant's shared file and copies the page there.
2. It computes the SHA-256 of the copy and compares it with the digest the
   upload recorded.
3. If they match, the copy becomes the resident page for that identity, and the
   wanting region maps it. The owner's mapping of its private copy is replaced
   with the shared copy, read-only, in the same way a store replaces a mapping
   (`vmmemory/replacement.go`). The private slot is released when that command
   lands. If the owner's client refuses the command, the pager revokes instead,
   and the owner refaults onto the shared copy.
4. If they differ, the owner's VMM wrote a page it held read-only. A
   well-behaved VMM never does this. Its own writes, including device writes
   into guest memory, go through the registered mapping, trap and copy. So the
   pager ends the owner's session with `ErrTampered`, drops the copy and removes
   the identity from the index. The wanting region loads the page from its
   volume. `Stats.Tampered` counts it.

`MemoryRegionCheckpoint.ReadDirty` takes the digest. It already copies each
sealed page into the upload's buffer, and it hashes that buffer, so the digest
is of exactly what the store receives. The retire moves the digest onto the
resident page. It is kept only while a published page sits in a private file.

A check is needed because a published page is read twice: once by the upload,
and again by whoever inherits it on this host. Between the two reads, only the
owner's VMM can change it. Without the check, a VM on this host would see other
bytes than the same VM on another host, and a descendant's memory would change
under it when it migrates. The check makes the resident copy the store's copy.

A published page leaves a private file in three other ways, and none of them
needs a check:

- An eviction punches it. The next fault loads it from the volume into the
  shared file.
- An idle page is dropped.
- A region that detaches leaves its private file behind while the file still
  holds idle published pages. No VMM holds the file any more. Those pages move
  with the check when a region inherits them, like any other, and the file is
  closed when the last one goes. This keeps a stopped VM's memory for the forks
  of its checkpoint, which is what [idle pages](../docs/vm-memory.md#idle-pages)
  are for.

### A page a fork point lends

`MemoryRegionCheckpoint.Share` names a parent's sealed pages for same-host
children. A child can map them only in its attach populate
(`populateRun.named`). A later fault reads them out of the child's own backing.

With the split, `Share` creates the fork file. A child's populate copies each
named page it maps into the fork file, once, at the page's own index. A later
child finds it there. The parent keeps its private page and is never remapped,
so nothing changes for the parent. The fork file is closed when the seal's name
ends. Ending the seal already revokes the children's mappings of those pages.

These pages need no digest. Nothing read them before the copy, and every child
that maps one maps the same copy.

A remote child, and a local child's later fault, still read the parent's sealed
page through the page server (`MemoryRegion.ReadResident`). The parent's VMM can
change that page between two such reads, so two children can inherit different
bytes. Each child's copy is its own from then on, and each child trusts its
parent's memory, so this is not a reach into another VM.

### Slot reuse and REVOKE

A slot of a private file only ever holds that region's pages. A slot of a shared
file only ever holds that tenant's pages, and every VMM of the tenant can read
the whole file through its descriptor. A fork file only ever holds one fork
point's pages. So a VMM that answers REVOKE and keeps its mapping still reaches
only what its descriptors reach.

Eviction still revokes every alias and waits for the acknowledgements before it
punches a slot. A guest that read through a stale mapping would otherwise read
another page of its own region or tenant.

### Eviction, idle pages and spill

The resident budget stays one host-wide pool (`Host.resources`). The file a
page is in does not change what it costs. The least recently used list, the
idle list and the fair-share rule stay host-wide, and they choose among the
pages of every file. An eviction revokes the page's aliases, spills the page if
it is private, and punches its offset in its own file. The idle list gives up
the oldest idle page whatever its tenant, because it is a cache.

The spill file stays one per pager. It holds the private pages of every region.
No VMM receives it.

### Accounting

Each file keeps a set of its held offsets. A shared file allocates runs from its
set as the arena does today, with `LongestRun`. A private or fork file needs no
allocation, because a page's offset is its index. `AllocatedBytes` and
`sproutfs_pager_arena_bytes` add up the files.

A VMM can allocate pages in its own private file behind the pager's back. The
session's periodic verification (`ConnectionConfig.VerifyInterval`) compares the
private file's allocated blocks with the pages the pager put there, and ends a
session whose file holds more. A VMM with a read-only descriptor may still make
the kernel allocate pages by reading holes through a mapping of its own. The
kernel charges them to that VMM's cgroup. The same check finds any in a shared
file, and the pager punches its free offsets back.

### Mapping protocol version 10

| Kind | Value | Change |
| --- | --- | --- |
| ATTACH | 3 | Carries no descriptor, and `length` is zero. The files follow as FILE frames |
| MAP | 4 | `flags` bit 0 means immutable, as before. The bits above it are the file number. `backing` is the offset in that file. A writable MAP must name file 0 |
| FILE | 14 | `id` the file number, `length` its size in bytes, `backing` the arena kind, `flags` 1 for writable and 0 for read-only. Carries one descriptor. File 0 is the private file and the only writable one. File 1 is the tenant's shared file. Fork files are 2 and up. A FILE may repeat a number with a larger length, which grows that file |
| DROP_FILE | 15 | `id` the file number. Sent after every mapping of that file is revoked. The client closes its descriptor |

A session attaches with HELLO, MEMORY_REGION, ATTACH, FILE 0, FILE 1, any fork
files, the populate's MAP_BATCH frames and READY. A version 9 peer is refused at
HELLO, because ATTACH no longer carries the arena and MAP names a file.

The client:

- keeps a table of its files, and checks each descriptor against the stated
  kind, as it checks the arena today with fstatfs in `linux.rs`
- checks that file 0 is open read-write and every other file read-only
- maps file 0 `MAP_SHARED`, as it maps the arena today, and every other file
  `MAP_PRIVATE`, as described above
- refuses a MAP that names a file it was not given, an offset past that file's
  length, or a writable MAP of any file but file 0
- reads frames with `recvmsg` after READY too, because FILE and DROP_FILE arrive
  mid-session

None of the client's checks keeps a VMM out. The pager's descriptors do that.
The checks keep a well-behaved client safe from a pager bug.

### What the embedder must do

- Run the VMM as a user other than the host's, not as root, and without
  `CAP_DAC_OVERRIDE`, `CAP_FOWNER` or `CAP_SYS_PTRACE`.
- Cap the VMM's cgroup with `memory.max` and `hugetlb.2MB.max`.
- Rebuild Firecracker with the new crate. Allow in the VMM's seccomp policy what
  the client now does on its memory thread: `recvmsg` with a descriptor after
  setup, `close` of a dropped file, and private file mappings with
  `MAP_NORESERVE`.

The host checks the first requirement. The peer check in `vmmachine`
(`checkPeer` in `vmmachine/process_linux.go`) already matches the VMM's PID.
When a Starter's `Placement.Owner` names a user, the check will also require that
user, and refuse the host's own user and root. A Starter that names no owner,
such as `vmmachine.Firecracker`, runs an unjailed VMM. The host then logs once
that its VMMs are not isolated from each other.

## Costs

**Copies.** A published page is copied and hashed once, the first time another
memory region maps it, and the owner gets one mapping command per run moved. A
fork point's page is copied once, the first time a same-host child's populate
maps it. Nothing else is copied. Today neither of these is.

**Hashing.** Every page a checkpoint uploads is hashed once more, with SHA-256,
in `ReadDirty`. The upload already computes SHA-256 over each part for its digest
attribute (`checkpoint/store.go`) and compresses every member, so this adds more
of the same kind of work. Processors with SHA instructions (SHA-NI on x86-64,
SHA2 on arm64) hash at gigabytes a second per core. Step 6 measures a large RAM
capture's upload with and without it. `crypto/sha256` is in Go's standard
library, so this adds no dependency.

**Memory.** A digest is 32 bytes per published page that sits in a private file:
8 MiB per GiB of such pages at 4 KiB, and 16 KiB per GiB at 2 MiB. A fork point's
lent pages are held twice, once by the parent and once in the fork file, while
the name lasts. The parent's sealed set bounds that, and in practice so does the
populate budget of 16,384 pages.

**Accounting across many files.** One host-wide budget and one set of lists, as
today, plus one held-offset set per file. The resident leases are keyed by file
and slot instead of by slot.

**Fragmentation.** It gets better. Private pages sit at their own indexes. A
shared file has more offsets than its tenant can fill, so a read-ahead run finds
consecutive free slots. Fragmented offsets stop mattering, because only pages
cost memory.

**HugeTLB.** Every HugeTLB file draws from the node's one pool, as the arena
does. The pager still allocates every page itself with fallocate. There is no
pool per file. The VMM's private mappings of read-only HugeTLB files need
`MAP_NORESERVE`.

**Spill.** Unchanged.

**Eviction.** Unchanged, except that a victim can be in any file.

**The pager's address space.** The pager maps each file whole. That is twice each
region for its private file, about once more across the tenants' shared files,
and the fork files: about three times the logical memory the pager admits. A
47-bit address space is 128 TiB, so this holds about 40 TiB of attached guest
memory per host.

**Mappings in the VMM.** A region's private pages merge as well as extents make
them merge today. Runs from different files never merge, but runs whose slots
were not consecutive never merged either. One risk is new. The kernel gives a
private file mapping an anon_vma when it installs a page into it, and two
mappings with different anon_vmas do not merge. So adjacent read-only runs built
by separate commands may stay two mappings where today they become one. Step 1
measures this. The pager already sends runs whose slots are consecutive as one
command, and the mapping budget backs up the rest.

**Descriptors.** Two per session, plus one per fork point a child comes from.

**Protocol and rebuild.** Version 10 on both sides, deployed together. The
Firecracker fork takes the new crate and a seccomp change and must be rebuilt. A
cached qualification build keeps speaking version 9 and fails at HELLO, as
`docs/vm-memory.md` already warns.

**Size.** About eight to ten weeks for one engineer, in the steps below. The
pager's change is the largest: about 3,000 lines touched, across
`allocation.go`, `placement.go`, `rules.go`, `resident.go`, `fault.go`,
`population.go`, `checkpoint.go`, `settle.go`, `spill.go`, `serve.go`,
`connection_linux.go`, `linux.go`, `internal/slots`, `internal/vmwire` and the
simulation's arena in `internal/simtest`. The Rust client change is about 600
lines, in `wire.rs`, `lib.rs` and `linux.rs`.

## The test that proves the hole is closed

`TestAHostileVMMReachesNoOtherVMsBytes`, in `vmmemory/hostile_linux_test.go`,
plays a VMM that uses every descriptor it is given, not only the protocol. It
runs on one RAM pager at 4 KiB over an ordinary memfd, and again at 2 MiB over
HugeTLB in the Lima suite.

The setup:

- Neighbour A is a well-behaved process in another tenant. It stores a random
  marker into its pages. Some stay dirty. A checkpoint publishes others, and a
  second region of A's tenant maps them, so they are in A's shared file.
- Neighbour B is in the hostile VMM's own tenant. It stores a second marker into
  its pages. Some stay dirty. Others are sealed by a checkpoint whose upload the
  test holds open.
- A page that B's tenant published, and that a region of that tenant maps,
  carries a third marker.
- The hostile VMM attaches in B's tenant.

The hostile VMM then:

1. Maps every descriptor it holds, whole, both shared and private, and reads
   every byte. It must find neither of the first two markers. It must find the
   third. That is the tenant concession, held as a fact of the test.
2. Tries a writable shared mapping, write, fallocate in both modes, ftruncate
   and `F_ADD_SEALS` on every read-only descriptor. Each must fail with the errno
   the kernel gives a read-only descriptor.
3. Writes, punches and allocates every offset of its private file.
4. Answers every REVOKE without unmapping, and keeps every mapping it made. The
   test drives A and B through enough faults to evict and reuse every slot of
   the pager. The hostile VMM reads its kept mappings again, and must find
   neither of the first two markers.

A and B must then read back exactly what they stored, and their faults must stay
within the fixture's bound. The pager must end the hostile session for the
memory it allocated behind the pager's back. The fixture's end-of-round check
must hold, as it does for every hostile session.

A helper process running as another user tries what only a jailed VMM is held
to. It reopens each descriptor through `/proc/self/fd` for writing, and calls
fchmod on it. Both must fail. It runs in the Lima suite, which already runs its
test process through `sudo -n`.

The same moves join `FuzzHostileSession` as one more hostile operation. So
arbitrary sessions mix reaching through descriptors with breaking the protocol,
and after every round the fixture scans what the hostile VMM can reach for the
neighbours' markers.

The digest check has tests of its own, because the hostile sessions use fake
userfaultfds and end at their first resolve (TASK-21). A pager test on the
simulated arena publishes a page, writes its private slot directly as a VMM
would, and attaches a second region that inherits the identity. The second
region must read the published bytes from its volume. The owner must end with
`ErrTampered`, and `Stats.Tampered` must be one. A Linux test does the same with
a real client and a writer mapping of the private file.

## A switch, and one measurement at the end

The owner accepted the recommendations below and asked for the split to be
built behind a switch. `SPROUTFS_ARENA` (`vmmemory.Config.Arena`) is `shared`,
the single read-write arena of today, or `isolated`, the split. It is
`shared` by default until the measurement says otherwise. The pager and the
protocol run either mode on the same build, so one GCE run measures both on the
same workloads: fan-out time to first output, checkpoint pause, upload time and
CPU, restore time, mappings per guest, and the memory sharing saves. The page
digest is BLAKE3 rather than SHA-256, because it is several times faster per
core and as hard to forge.

Every step below keeps `shared` exactly as it is today: its suites pass
unchanged. Steps 2 and 3 add no behaviour to `isolated` beyond what they need.

## Steps

Each step lands on main with every suite passing.

1. **Prove the kernel does what this needs. Small, three days.** A Linux test in
   the `vmtest` fixture, for an ordinary memfd and for a HugeTLB memfd. It
   requires that an `O_RDONLY` reopen refuses writable shared mappings, write,
   fallocate, ftruncate and seals. It requires that a `MAP_PRIVATE` mapping of it
   registers for missing, minor and write-protect faults, that
   `UFFDIO_CONTINUE` installs it read-only, and that a store traps, from a thread
   and from KVM. It requires that pagemap shows the pager's physical page in both
   processes, that a HugeTLB private mapping with `MAP_NORESERVE` reserves no pool
   pages, and that another user's reopen through `/proc/self/fd` fails after
   fchmod 0600. It records how many mappings N adjacent read-only runs built by
   separate commands leave. It runs in Lima and on GCE. If any of it fails on a
   qualification kernel, the plan stops here and goes back to the owner.

2. **Make the arena a set of files, with one file. Medium, one to one and a half
   weeks.** A resident page's slot becomes a file and a slot. The `Arena`
   interface makes files, and each file reads, writes, zeroes, compares and
   releases its own slots. The Linux arena and the simulation's arena follow.
   Every page stays in one file, so nothing a test can see changes. It is proven
   by every existing suite passing unchanged: the simulation campaigns, the Linux
   pager suite, and the Firecracker suite in Lima.

3. **Mapping protocol version 10. Medium, one to one and a half weeks.** FILE,
   DROP_FILE and the file number in MAP, in `internal/vmwire`. The client's file
   table, private mappings of read-only files, and checks, in
   `rust/sproutfs-vm-memory`. The Firecracker fork is rebuilt with the crate and
   the seccomp change. The pager still sends one read-write file as file 0 and
   maps everything from it. It is proven by the Rust unit tests, the frame tests
   on both sides, new `vmtest` cases that map read-only files through the real
   client and store into them, the hostile suite, and the Firecracker suite in
   Lima.

4. **Private files and a read-only shared file. Large, three to four weeks.** It
   starts with the reach test above, which fails against step 3's pager. Then:
   a private file per memory region, placement by index and no extents; loads
   into the shared file, sent read-only; the digest in `ReadDirty` and the
   checked move; fork files for `Share`; detached private files kept for their
   idle pages; and the allocated-blocks check. Every VM is in one tenant.

   The simulation's arena records who may read each file. On every write and
   every map it checks that a page is only in a file its readers may read, that
   a writable map names the region's own private file, and that no page two
   regions map is in a private file. Every campaign then checks the split under
   forks, migrations, eviction and spill.

   It is proven by the reach test, the digest tests, the fuzz with its new
   operation, the placement and rules tests with their counts restated for
   private files, every campaign, the Linux pager suite, and the Firecracker
   suite and the fan-out in Lima at 4 KiB and at 2 MiB.

5. **Tenants. Small to medium, one week.** `MemoryRegionBacking.Tenant`, one
   shared file per tenant, the sharing index keyed by tenant, and the host
   passing the VM's tenant. It can land before TASK-18 with one tenant per
   deployment, and take TASK-18's prefix when that lands. It is proven by the
   reach test with its other-tenant neighbour, and by a simulation campaign in
   which two tenants fork their own templates of one image and no resident page
   is shared across tenants.

6. **Qualify and document. Medium, one week.** One GCE run covers everything. It
   records the fan-out's `SavedBytes` against today's, a child's start, the
   moves and fork-file copies, the VMM's mappings and the pager's address space,
   and a 16 GiB RAM capture's upload at 4 KiB with and without the digest.
   `docs/vm-memory.md`, the pager row of `docs/architecture.md`, the resident
   page in `docs/context.md`, "Running the VMM" in `docs/hosting.md` and the
   header of `hostile_linux_test.go` are updated. TASK-2 is closed.

Steps 2 and 3 do not depend on each other and can run side by side.

## Decisions for the owner

1. **Whether and when.** Recommendation: do it now, before any untrusted tenant
   runs. Steps 1 to 4 close reach into other VMs' private pages and every write
   to a shared page, without waiting for TASK-18. Step 5 lands with TASK-18.
2. **The tenant as the trust domain for shared pages.** A compromised VMM can
   read every published page of its own tenant that is resident on its host,
   including pages its VM does not inherit. Recommendation: accept it. TASK-18
   makes the tenant the unit that owns data. A finer domain would need a file per
   lineage of checkpoints, and would give up sharing between a tenant's
   unrelated VMs.
3. **Templates per tenant.** TASK-18 forbids sharing across tenants, so tenants
   that use one image each hold that image's resident pages on a host.
   Recommendation: accept it, as TASK-18 already does in the store.
4. **Jailed VMMs only.** The protection needs the VMM to run as another user,
   and not as root. Recommendation: the host refuses a session whose peer is the
   host's own user or root when the Starter names an owner, and logs that an
   unjailed deployment isolates nothing.
5. **A VMM that writes a page it holds read-only loses its VM.**
   Recommendation: yes. Only a compromised VMM does this, and every other break
   of the protocol already ends the session in the same way.
