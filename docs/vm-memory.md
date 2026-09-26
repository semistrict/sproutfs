# Managed VM memory

A VM's RAM and PMEM are [volumes](volumes.md). The Go package `vmmemory` is the
pager. It handles shared resident pages, fault resolution, private
copy-on-write pages, scratch spill and eviction. A host runs one instance of it
per kind of memory region: one for its guests' RAM and one for their PMEM disks. Each
instance has a separate arena, spill file and page size. The independent Rust
library in `rust/sproutfs-vm-memory` owns the mappings inside one VMM process.
It has no Firecracker dependency. `vmmachine` prepares a VMM's memory and
drives the process that a Starter starts; see [hosting](hosting.md#running-the-vmm). `host`
pauses and seals a VM for a checkpoint or for a fork point. The `vmtest` package
is the low-level syscall fixture for mapping races and malformed commands.

Three parts of `vmmemory` are nested packages that only `vmmemory` can import:

- `internal/pageranges` is the interval map that holds page state.
- `internal/latency` holds the fixed log-scale histograms of the fault path.
- `internal/slots` holds the arena free set and the consecutive runs of it that
  one mapping command covers.

None of them uses any pager state: no host lock, no memory region and no page.
Everything else stays in `vmmemory`. The arena, the spill file, the UFFD session
and the binding blocks all read and write the pager's state under its metadata
lock, so they are files of one package and not separate packages.

## Why a pager of its own

The pager does for a guest's memory what the kernel's page cache and swap do
for a file. It decides which pages are resident and faults the rest in on
demand. It shares one resident page among every mapping that names it. It
tracks what the guest dirtied, writes it back, evicts pages and spills them. The
kernel already does all of this, but the integration does not use the kernel's
version. The first reason below is enough to rule it out. Each of the others
would also rule it out.

1. **The page cache duplicates what VMs share.** It caches per file. A reflinked
   file is a separate inode with its own cached pages. So once each VM has its
   own writable copy of an image, including a reflink, a fleet of VMs from one
   image holds one copy of the same bytes per VM. The kernel cannot tell that
   the copies are the same. The guest duplicates them again: a guest with a
   virtio-blk disk keeps its own page cache of that disk in its RAM. The same
   inherited bytes are then resident once more per VM, and the host cannot see
   them. The pager keeps one resident page per page identity, however many VMs
   map it. The guest reaches that page through PMEM over DAX, which maps the
   host's page directly. So the host's copy is the only copy. Three checks
   enforce this:
   - A VM has no block device, only PMEM.
   - The host refuses a guest command line without `rootflags=dax=always`. With
     that option, ext4 fails the mount where DAX is unavailable, so the boot
     fails.
   - The guest's witness refuses a file on a PMEM device that the kernel does
     not report as DAX.

   The guest's init also remounts the root `noatime`. Every file in an image
   has an access time no newer than its modification time. Under the kernel's
   `relatime`, a fork that only reads would therefore write the inode of every
   file it reads. That dirties pages the fork shares: guest memory for the
   inode, and root pages when the journal commits. `noatime` is a mount flag,
   not an ext4 option, so `rootflags=` cannot carry it. ext4 refuses the option
   and the root does not mount.
2. **Sharing is by name, across VMs and across checkpoints.** A child's memory
   is its parent's checkpoints plus its own writes, over tens of thousands of
   pages. As file mappings, that is one VMA per run. That counts against the
   kernel's mapping limit, and the mappings change at every checkpoint.
3. **Durability is one pause of the whole machine, not writeback.** The kernel
   writes dirty pages back when it chooses. A checkpoint needs every dirty page
   frozen at one moment while the guest keeps running on them. The pager does
   this with write protection and copy-on-write through userfaultfd. The pages
   take a new name once the upload lands.
4. **The backing is not only a file.** After a fork or a migration, a page's
   bytes can be on another host that still holds them. The page cache faults
   from a filesystem. A filesystem in front of the store can serve reads, but
   it cannot fault a page out of a peer host's memory. Also, every fault would
   go through the kernel and back out at 4 KiB.
5. **The budgets belong to the host.** The pager admits resident, logical and
   dirty pages explicitly. A guest that dirties pages faster than it publishes
   them is checkpointed early or stalled. It does not grow until the kernel
   swaps or kills something. The pages are HugeTLB, which the kernel never
   swaps, so the pager must handle eviction and spill anyway.
6. **It has to run inside the simulation.** The deployment's campaigns exercise
   the same pager on a simulated arena, disk and clock. The kernel's page cache
   cannot run inside a deterministic harness.

Upstream Firecracker's userfaultfd restore copies pages into each VM's anonymous
memory, so it cannot share a page between VMs. The integration replaces that
path with this one.

## One pager per kind of memory region

A pager instance is built with a fixed page size. Everything it does is counted
in pages of that size:

- its arena slots, its spill slots and the extent of its spill file
- its resident, logical and dirty budgets
- the read-ahead and write-ahead runs, which a deployment states in bytes and
  each instance converts
- its buffers
- the comparison a settle makes
- the byte conversions of `MemoryRegionStats` and `Sharing`
- the size and alignment it requires of a memory region
- the fault arithmetic of the Linux connection

The page size must be one that a volume can be published in.
`checkpoint.GeometryFor` is the only place that defines those sizes. So a pager
and the volumes it maps always agree about what a page number means. The pager
refuses to attach a volume published in another page size. This check also
catches a memory region that reached the wrong pager of a host.

A host assembles two pagers. `vmmemory.Pagers` holds the RAM pager and the PMEM
pager, and a memory region attaches to the pager of its kind. A RAM page and a PMEM
page can differ in size, so nothing adds their page counts together. Everything
a host reports across the two pagers is in bytes:

- The two arenas' capacities and the two dirty budgets add up to what the
  deployment gave the host.
- The sharing gauges are reported per kind under one metric name with a `kind`
  label.
- `Host.PrivateBytes` adds up a VM's memory regions across both pagers.

Pressure from either pager can ask for a checkpoint. That checkpoint seals the
whole VM once, because one pause seals every memory region the VM maps. A VM's loss
window is the oldest unpublished write across its memory regions in both pagers. If
neither pager can admit a store, only that VM is stopped.

### The ephemeral pager

A host may run a third pager, for [ephemeral disks](volumes.md#ephemeral-disks)
(`vmmemory.Config.Ephemeral`, `Pagers.Ephemeral`). An ephemeral disk is PMEM to
the guest, and the session states PMEM on the wire. It is a pager of its own
because its private pages are the disk's only copy. They must not take the
dirty budget or the arena of the disks that checkpoints publish, and they must
not bound a VM's loss window.

It is an ordinary pager with three differences:

- **Its dirty budget is its logical budget.** Both are the disk its spill file
  may fill. The host admits an ephemeral disk against the logical budget by its
  size, so every page of every admitted disk can be private at once, and a store
  into one never waits.
- **Its seal takes nothing.** A VMM asks every session to seal for a capture, so
  the seal succeeds and records no checkpoint. No capture, fork point or
  interval checkpoint reaches its pages.
- **It keeps no loss window.** `MemoryRegion.OnInterval` is false for its
  memory regions, as it is for RAM. The host leaves them out of the loss window,
  a flush of one completes at once, and pressure on its budget asks for no
  checkpoint.

A backing that states `Ephemeral` (`vmmemory.EphemeralBacking`, which a volume
and a peer backing implement) attaches only to the pager built for it, and that
pager maps nothing else. Its resident pages are evicted to its spill file and
read back from it like any other private page, so what stays in memory is
bounded by its arena.

**RAM's page is 4 KiB and PMEM's is 2 MiB**, on a real host and in the
simulation. The page size determines the memory behind it. A 2 MiB page comes
from the host's provisioned HugeTLB pool. A 4 KiB page comes from an ordinary
shared memfd in the pod's own memory, which a host with swap may swap. A session
states its memory region's page size and its arena's kind when it attaches. Both ends
check the pair before any guest memory exists. The two arenas of one host are
separate memfds and never draw on the same allotment.

## Bounded host pager

Each pager takes an arena, a dedicated scratch spill file, and explicit
resident, logical and dirty page budgets. Logical admission bounds metadata for
every attached memory region, including pages never touched. Every private page takes
a spill slot before a write can resume. The spill slot covers resident and
spilled dirty state together. The dirty budget sets the size of the spill file.
So the file's whole extent is the pager's fixed disk cap, and a store never
needs room from anywhere else. Concurrent page I/O has a separate limit. Each
permit covers one read-ahead or spill buffer. One extra permit is reserved so
that a checkpoint's read of a sealed set makes progress while cold faults use
all the others.

`LossWindow` bounds the same private state in time. For each memory region, the pager
records the age of the oldest page that no landed checkpoint covers. While that
age, across the memory regions of one VM, exceeds the window, the pager admits no
further dirty page for that VM while a sealed checkpoint of it is uploading.
The store waits in the same way as a store past the dirty budget. Zero
disables the window.

A store is never held while the checkpoint it would wait for still needs a
pause. A held store holds the vCPU that made it, inside its fault, and a pause
needs every vCPU, so such a store would wait for a pause it prevents. So a
store past the window goes through, and asks for the checkpoint through
`Pressure.Checkpoint`, when nothing of its VM is sealed yet, when a pause is
under way, and when a fork point's hold stands, since that hold ends in a
checkpoint of its own. The stores after it wait once the checkpoint has sealed.
The guest writes past the window for at most one pause. The owner's own clock
normally takes the checkpoint before the window runs out, at three quarters of
it. A store does not ask then: a guest that rewrites pages it has already
dirtied needs no new page, so no store of it would ever ask. Where no
checkpoint of the VM can be taken at all, the store past the window stalls,
and the VM's owner stops it.

The window belongs to the VM and not to the memory region, because the checkpoint that
ends it covers the whole VM. The pager knows nothing about VMs, so it asks the
memory region's owner through `Pressure.Oldest`. The age stamp moves with the pages it
measures:

- At the seal, it moves to the checkpoint.
- When that checkpoint is abandoned, it moves back to the memory region.
- In a handoff, `MemoryRegion.Handoff` reports the age and `MemoryRegion.SetUnpublishedAge`
  applies it.

So neither a failed publication nor a migration restarts the bound. If no
checkpoint of that VM will ever be taken, the wait ends with
`ErrWindowStalled`, in the same way as a budget stall. The memory region's owner then
stops that VM.

Allocation, sharing, copy-on-write, write protection, spill, eviction, mapping
commands and wire generations all use the instance's page size. Memory region
addresses and lengths must be aligned to it. An instance can run any page size
that a volume can be published in and that the transport maps. Both sets
contain the same two sizes.

### An arena's offsets and its pages

An arena is a set of files, and a resident page is one slot of one file. Each
file reads, writes, zeroes, compares and releases its own slots, and keeps its
own set of held offsets. The pager's capacity, `Config.ResidentPages`, is one
count across all of its files. `Config.Arena` (the host's `SPROUTFS_ARENA`)
picks how pages are divided between files:

- `shared`, the default, keeps every page in one file. The pager makes it when
  it starts, of `Config.ArenaOffsets` slots. Every VMM receives it read-write.
  The rest of this section describes this arena.
- `isolated` splits the pages by who may read them. See
  [the isolated arena](#the-isolated-arena).

An offset is an address in the arena. A page is memory. They are counted
separately. `Config.ArenaOffsets` is the number of addresses the arena has.
`Config.ResidentPages` is how many of them can hold memory at once. The memfd is
sized to `ArenaOffsets` and is sparse. So an offset costs nothing until a page is
put there, and `Release` punches it back out. `slots.Space` holds one bit per
offset and limits every allocation to the pages left. `Host.residentLeases` is a
map, because there is one resource reservation per page and not one per
address. `AllocatedBytes`, the memfd's allocated blocks, is the memory an arena
actually holds.

RAM needs many more offsets than pages. The reason is that RAM places a private
page at the same offset that the page has within its 2 MiB range. A range that
holds one private page therefore owns the whole run of 512 consecutive offsets
where its other pages would go. So the supervisor sizes RAM's offset space as
follows:

- one such extent for each range that any admitted memory region may have written
  into, which is `LogicalPages`, because a range is 512 pages and an extent is
  512 offsets
- plus `ResidentPages` for the read-ahead runs, which take their own
  consecutive offsets

PMEM does not place pages this way, so its offset count equals its page count.
A configuration that leaves `ArenaOffsets` zero describes that pager.

The host must provision a 2 MiB HugeTLB pool for the **PMEM** arena before it
starts VMs. The **RAM** arena is an ordinary memfd charged to the pod's memory.
So a node provisions the two separately, and the two arenas no longer divide
the pool. Each arena reserves virtual address space without reserving its whole
logical capacity. It then allocates each resident slot before it touches the
slot's mapping. The HugeTLB arena allocates with `fallocate`. The ordinary arena
allocates with a populating write fault (`MADV_POPULATE_WRITE`) through a
separate mapping. When the pool or the pod's memory is exhausted, allocation
returns an error. It does not fall back to another page size. Eviction punches a
whole slot only after it revokes every alias. HugeTLB pages cannot be swapped
and shmem pages can. In both cases the pager's own spill path handles reclaim.
The kernel must support missing, minor and write-protect faults on userfaultfd
for HugeTLB **and** for shmem, because a host runs a pager of each kind. All
HugeTLB arenas on the host share one pool, so deployment admission must budget
their combined resident capacity.

**The ordinary arena allocates only a zero run's whole 2 MiB blocks as huge
pages.** It maps its memfd twice:

- One mapping is advised never to use huge pages. Every access goes through it,
  and so does every allocation of a single page or of a run's partial ends.
- The other mapping is aligned to 2 MiB and advised to use huge pages. It is
  used only to allocate the blocks that a zero run covers completely.

The kernel allocates and clears each such block as one transparent huge page in
one step. With 512 ordinary pages instead, a boot's write-ahead run costs several
milliseconds per block. The arena still holds only the slots the pager counted.
A huge page for anything smaller than a whole block would hold memory for slots
nobody asked for. A mapping advised against huge pages is the only way to
refuse a huge page under any host policy. Releasing one slot of a huge page
splits the huge page and returns only that slot. The host's
`/sys/kernel/mm/transparent_hugepage/shmem_enabled` must be `advise` (or
`within_size` or `always`) for the blocks to be huge. Under `never` they are
ordinary pages, and the pager behaves the same. The guest's translations are
4 KiB in both cases, because `UFFDIO_CONTINUE` installs one page table entry at
a time.

When a configuration leaves read-ahead and write-ahead zero, each is one page,
which means no read-ahead and no write-ahead. A store page is the same unit as
this pager's page. Each volume is published in its own page size, and the pager
refuses to attach a volume whose page size differs from its own. So a sealed
pager page is one member of a checkpoint's part. Read-ahead is one policy per
pager: a power of two of that pager's pages, capped at 16 MiB. No memory region
overrides it. Population walks metadata in windows of at most 256 MiB,
independent of pager size. The Linux transport also bounds pending faults,
control requests and fault workers.

The production host sets all of these bounds to nonzero values. The supervisor
in `host` chooses them for each pager. It bases them on the share of
the arena the deployment gave that kind and on the node, not on one environment
variable per bound. The two runs are stated in bytes and each instance converts
them. A run is a buffer, and the same number of pages would be a different
amount of memory in each pager:

| bound | production value | why |
| --- | --- | --- |
| `ReadAheadPages` | 8 MiB of this pager's pages — four at 2 MiB, 2,048 at 4 KiB | a boot, a restore and a working set all walk memory forwards, so one fault serves what would otherwise take four. The run lands in consecutive arena slots, so one command installs it |
| `WriteAheadPages` | the same 8 MiB of this pager's pages — four at 2 MiB, 2,048 at 4 KiB — or one page where that pager's dirty budget holds fewer than 64 such runs | write-ahead serves only fresh zeros. No other memory region shares a hole, so making a store's neighbours private loses no sharing at any page size. The benefit is one fault where a guest writing fresh memory forwards would otherwise take many: 80 % of the pages a 16 GiB guest's boot makes private are two contiguous runs written in order. Every page of the run holds a dirty reservation until the next checkpoint, so a pager whose budget cannot hold 64 runs uses one page |
| `ConcurrentIO` | four per processor, held between 16 and 256, and never more read-ahead runs than that pager's arena has room for | each permit can hold one read-ahead or spill buffer. So the value sets both the parallelism a node can use and a bound on the buffers it costs |
| `SettleWorkers` | the node's processors, capped at 64 | a settle compares resident pages and takes no I/O permit, so it is limited by processors. The upload waits for the settle |
| `ConnectionConfig.FaultWorkers` | two per processor, held between 8 and 64 | a fault spends most of its time in a store read. The I/O budget bounds the reads |
| `ConnectionConfig.MaxVMAs` | half of `/proc/sys/vm/max_map_count`, disabled below 128 and capped at 2²⁰ | the VMM has mappings other than the pager's, so the budget is half the kernel's limit and the rest is headroom. If the limit cannot be read, the budget is disabled, as it is for a client without `/proc` |

A starting host logs each of these values per pager, next to that pager's page
size. So the log records what a node chose for each kind, next to the arena and
the budgets the deployment set.

Attaching a memory region admits its metadata and verifies writer authority before the
memory region is exposed. At first, the mapping must consist only of armed
missing-fault traps. A caller must not attach one writable volume to two
memory regions, because the pager is the only thing that changes the volume's contents
while it is attached. A volume implements the backing interface directly. Its
load and locate calls serve the handle's current view. Locate needs no I/O. A
cold load is a range read inside the part that holds the page. The pager never
writes to a volume. A memory region's dirty pages reach storage only through the
checkpoint that reads its sealed set. Verify confirms that the handle still owns
its VM. It makes nothing durable.

A memory region can also attach through a backing placed in front of its volume.
`vmmachine.Config.Backings` names such a backing per volume, and a
[migration](migration.md) destination binds one. Loads first ask the host that
still holds those pages, and the volume serves everything else. The volume
remains the memory region's identity: its name, its size, its writer, its page
identities and every write. So the substitution does not change a seal, a
checkpoint or a fence.

Two placements deliberately use an ordinary offset, and the code records both
where they happen (`vmmemory/placement.go`):
- A store that copies away from the copy a checkpoint froze cannot use its own
  offset, because that offset holds the bytes the upload is reading. The page
  stays outside its range's run until something releases it, and nothing
  moves it back.
- A page that a migration destination loads privately from the source arrives
  in its own run, like any other load. So a post-copy destination's private
  pages are not placed at all until the guest stores into them.

### The isolated arena

A VMM may be compromised, and it holds every descriptor its sessions are
given. In a shared arena that descriptor reaches every page of the pager. The
isolated arena (`SPROUTFS_ARENA=isolated`) splits the pages by who may read
them, so a VMM's descriptors reach its own VM's memory and the pages its tenant
may read, and nothing else. The design and its threat model are in
[the plan](../plans/isolated-arena-2026-09-25.md).

There are three kinds of file:

- **A private file per memory region.** It holds the region's private pages:
  dirty, sealed, written ahead, spilled back in, and loaded privately. Only that
  region's VMM receives it, read-write, as file 0. It has twice the region's
  pages. A page's own offset is its index, so private pages that are adjacent
  in the guest are adjacent in the file, in any write order. The second half is
  each page's other place. A store that cannot use the page's own offset,
  because a checkpoint's copy or the page it copies from is there, takes the
  other place. When both are taken, one holds a clean page nothing maps, and
  the store gives that page up. A range's extent is that range of the file, so
  the gap rule and the half-private rule work as in a shared arena, and nothing
  is carved out of an offset space.
- **A shared file per tenant.** It holds the pages another memory region of
  the tenant may map: pages loaded by identity, and published pages once
  another region inherits them. The tenant's VMMs receive it read-only, as
  file 1. The pager makes it when the tenant's first memory region attaches.
  It outlives the tenant's last region while it holds idle pages, and goes
  back to the arena with the last of them. It has the pager's whole offset
  space (`Config.ArenaOffsets`), so each tenant on a host costs the pager that
  much address space, though only its pages cost memory.
- **A fork file per fork point.** It holds the pages a fork point lends to
  children on this host. A child's populate or fault copies a lent page there,
  once, at the page's own index. Later children map the same copy. The children
  receive the file read-only, as file 2 or up, just before they first map from
  it. The parent keeps its page and is never remapped. When the seal ends, the
  children's mappings of the copies are revoked, each child is sent DROP_FILE,
  and the file goes back to the arena.

Each file is a memfd of the pager's kind, with mode 0600. A read-only file is
sent as a new open of the memfd with `O_RDONLY`. So the kernel refuses a VMM a
writable mapping of it, a write, a punch, a resize and a new seal.

**The tenant is the host's to state.** It attaches each memory region with the
tenant of its VM (`MemoryRegionBacking.Tenant`), which is the part of the VM's
identity before the slash. A page's identity is the checkpoint that published
it, and that checkpoint's VM names the tenant too. So a fault whose backing
names a page of another tenant fails with `ErrOtherTenant`, in either arena,
and so does a fork point that would name its pages under another tenant's
checkpoint. The sharing index is keyed by identity, so it never hands one
tenant's page to another. A page is only ever in its own tenant's shared file,
and a VMM of one tenant is never given another tenant's.

A page whose bytes no other region may inherit is loaded into the region's own
file, not the shared one. That is a page with no identity, a page a fork point
lends, and a page another host still holds. A clean one of these goes to its
other place, so its own offset stays free for the copy a store makes.

**A published page moves once another region inherits it.** A checkpoint that
publishes a page leaves it where it is, in its region's private file, and the
guest keeps mapping it. Most published pages are never inherited on this host,
so most are never copied. `MemoryRegionCheckpoint.ReadDirty` hashes each page
it reads with BLAKE3. The retire keeps that digest with the page while the page
is in a private file. A page without one is not named by its identity. When
another region wants the identity, the pager copies the page into a free slot
of its tenant's shared file and compares the copy's digest with the upload's:

- If they match, the copy is what the identity names. The owner's mapping of its
  private page is revoked, the owner's next fault maps the copy, and the private
  slot goes back. `Stats.MovedPages` counts these.
- If they differ, the owner's VMM wrote a page it holds read-only. Its own
  stores, including device writes, go through its registered mapping and copy,
  so only a compromised VMM does this. Its session ends with `ErrTampered`,
  `Stats.Tampered` counts it, and the identity is no longer named. The region
  that wanted the page reads it from its own volume.

A move that finds no free slot of the shared file does not wait. The page stops
being named by its identity, and the region that wanted it reads its volume.
`Stats.ForkCopies` counts the copies into fork files. They need no digest:
nothing read those pages before the copy.

**A private file outlives its region while it holds idle pages.** A region that
detaches leaves the published pages of its private file idle, for the next
region that inherits them. Such a page moves with the check like any other. The
file goes back to the arena with its last page.

**The files are counted.** A VMM can allocate pages in its own private file, and
a VMM that reads a hole of a shared memfd through a mapping makes the kernel
allocate a page there. So every verification of a session compares the private
file's allocated blocks with the pages the pager put there, under the lock the
pager takes and gives slots under. A file that holds more ends the session with
`ErrUncounted`. The same check on the tenant's shared file punches every
offset that holds no page, because such memory is nobody's. A detach does both for the
region's files.

## Sharing by identity

Resident pages are keyed by the [page identity](volumes.md#reads) that the
volume reports for a page. Any memory region in the same pager whose current identity
matches a resident page maps that page, regardless of which memory region loaded it. A
fork inherits its parent's identities until it writes a page, in the same way
that a forked process shares pages until copy-on-write. A first store allocates
a private page. A page is named by the checkpoint that published it. So
identical bytes written independently keep distinct identities. Checkpoint
publication may still recognize an all-zero page and store it sparsely. That is
separate from resident sharing.

A page that the parent held dirty at a fork point has no published identity.
Without one, a child would have to read each such page back through the seal.
Instead, the fork point names the pages it sealed. The point takes its own
reference and publishes nothing under it. So the identity it gives each of those
pages belongs only to the children of that point, permanently. The pager enters
the pages in the sharing index under that identity. A child on the parent's
host then maps them like any inherited page. This includes the eager restore
population, regardless of which run contains the pages. So a machine forked at a point maps
the pages its parent holds dirty before its vCPUs run.

The eager population's length test does not apply to these pages. A later fault
can map a published page from the same resident page, but it cannot map one of
these, because the name no longer exists by then. The population's run budget
does apply, and these pages take it before any other run. The pages stay the
parent's private dirty state under the name:

- Nothing is copied and nothing becomes durable.
- The parent still copies on write and still owns the reservation that spills
  them.
- The name lasts as long as the seal. The sealed bytes cannot change during
  that time.

Ending the seal removes the name. A published checkpoint replaces it with the
identity the volume then reports. A retired fork point hands the page back to
the guest as dirty state that the guest may store into in place. For that
reason, the retire takes the page away from any memory region still sharing it. By
then, every child that inherited the page has copied, published or pulled it
and reads it from there.

A resident page is one store page, so one identity covers the whole page. There
is no partial identity: a page is either published whole or not published. A
page with no published bytes, such as a migration destination's copy of the
source's unpublished pages, has no identity. It loads privately until a
checkpoint gives it one. A one-byte store still copies and charges the whole
page. So the pager's page size sets what a store costs its guest. Storage
compression does not compress mapped pages or change this accounting.

An explicit sparse zero has no arena slot and no page identity. Contiguous zero
ranges use Linux's shared zero page, so a large hole does not consume resident
slots. These sparse zero mappings and untouched missing-fault traps have no
resident page. Their anonymous page tables are the only memory not backed by the
arena. A zero range is one mapping command, however many pages it covers. So at
4 KiB a sparse hole does not cost a command or a mapping per page. The first
write replaces one page of it with a private arena page. The zero mapping is not
an identity, and nothing is shared under it.

## Faults and read-ahead

Ordinary volume reads on the fault path add no extra round trip. Migration
backings may request pages from the source peer, and cold volume loads may read
object storage. The supervisor's periodic verification confirms ownership.

A read fault serves its whole aligned read-ahead run when it can. The run is one
pager page by default. Pages already resident under the same page identity are
mapped without a read. The rest are loaded with **one backing read for the
window** into consecutive arena slots and installed with one mapping command.
Their page tables are pre-installed, which avoids missing-page faults while
those mappings remain valid. Read-ahead uses only free slots and never evicts.
Only the faulting page may cause an eviction. Eviction can later revoke a
mapping and require a refault. The read-ahead run is set per host, and every
memory region of a host uses it.

That one read asks only for the pages of the window that need bytes. It leaves
out the pages the memory region already holds, through `vmmemory.SparseLoader`, which
`volume.Volume` implements. It does not split into several reads around those
pages. A window is a run of a volume, and the volume decides how to read a run.
The wanted pages are still grouped by the part that holds their members, with
one ranged read per part. When a reader leaves out a few pages in the middle of
a run, the volume reads through them, in the same way as it reads through pages
that a later checkpoint rewrote. Splitting the read instead put that decision
in the pager and cost one request per stretch. For example, a 512-page window
with 64 already-resident pages scattered through it took 65 loads and 195 object
reads. It now takes one load and three object reads. A backing that cannot be
asked for part of a range, such as a migration destination's peer backing, is
still read one stretch of wanted pages at a time.

Before the memory region is exposed, attach populates the pages whose identity is
already resident in the same pager. It loads nothing. The Rust session serves
mapping commands after the descriptor exchange, and only then reports the memory region
addresses to the VMM. So eager mappings finish before VMM setup uses those
addresses and before any vCPU runs. This applies to cold boot and to snapshot
load. A restore still returns paused. Pages absent from the arena use the
ordinary fault path. An explicit later population requires quiescent guest
memory.

The populate is bounded. A mapping run costs one command whether or not the
guest ever reads it. A page the populate skips costs at most a fraction of a
command, because the fault that reaches it maps its whole read-ahead window from
the same resident pages. So a populate installs a run only when the run covers
at least one read-ahead window. It installs at most 128 runs and 16,384 pages in
total. Resident runs and holes share this one budget. A run costs more than its
command. The kernel installs the run's pages one at a time, as one
write-protected entry each, at about a microsecond per page. On 2026-09-23 on
GCE, a warm restore's 128 runs carried 839,196 pages and took 1.10 s. So a run
longer than the pages left in the budget is shortened to fit, and the fault that
reaches the rest maps its window. A hole is one run, however many pages it
covers. But a guest's address space contains many holes spread through it. So a
hole must meet the same length test as a resident run, and its pages count
against the budget in the same way.

The runs that a fork point names are exempt from the length test but not from
the budget. They are the parent's dirty state under a name that ending the seal
removes. So the attach is the only time a child can map them. They take the
budget before any other run, regardless of which run contains them.

**The walk that finds those runs stops when the budget runs out.** It goes over
the memory region window by window. For each window it asks the volume for the identity
of every page in it. For a 16 GiB guest at a 4 KiB page that is four million
pages, decoded from the index's segments. A window reached with no budget left
cannot install any run. So the populate stops there and does not read the rest
of the page table before the guest runs. Stopping loses one thing: the record of
which explicit zeros this memory region knows about. That record lets a sibling
attachment map holes without its own metadata. If a memory region's first windows
contain no holes, that record is lost, and the sibling's first fault finds the
holes instead.

Measured on GCE, an unbounded populate mapped 2,930,747 sibling-resident pages of
a 16 GiB guest in 21,698 runs before the guest ran. That took five seconds of a
restore whose bound is half a second, and the guest then took 723 faults.
Bounding only the resident runs left 14,447 runs over 2,166,194 pages. Only
123,056 of those pages were resident identities. Two million pages of scattered
holes still cost one command each. For this reason one budget covers every kind
of run.

The cost of each attach is recorded, not inferred. `AttachStats` covers a
session's whole `Connect`: descriptor exchange, admission, ATTACH, populate and
the READY round trip. It includes the populate's commands, runs, pages and
duration (`MemoryRegion.Populated`). `vmmachine.StartPhases` reports it next to three
other phases:

- the VMM process's start
- the snapshot load, inside which the sessions are built
- the round trip that proves the machine is up

So when a restore takes seconds, the record shows which of the four phases took
the time.

When the pager refuses a memory region, the two halves of the failure are on opposite
sides of the socket. The VMM builds its sessions inside its own boot or load
request. So a refusal fails that request, and the VMM knows only that the
attachment never came. The reason is on the pager's side, in a connect
result that the supervisor would otherwise never read. Both sides log the
failure where it happens. The VMM's log names which kind of attachment failure
it saw, because each kind points to a different end of the connection:

- the pager closed the session before attaching
- the pager closed it partway through a frame
- the pager sent a file without its descriptor, or a descriptor with a frame
  that carries none
- the descriptors did not fit this side's ancillary buffer
- a file whose descriptor is not what its frame states

A failed request to the VMM is reported together with every session's result.
The supervisor closes the process first, which ends the listeners and every
connect attempt. So those results are final, and joining them does not wait.
Every step of a start that could wait is bounded. The memory region listeners give the
VMM two minutes to connect. The wait for its API socket has the same bound,
because a process that neither binds the socket nor exits will never finish
starting. A start can fail after it takes its directory from the shared scratch.
Such a start goes through Close, so the scratch does not keep counting a process
it will never see stop.

A store into a page for which this memory region holds no memory first reads in its
whole read-ahead run, as a read fault does. It then copies the one page the
guest stored into. This is necessary because of how KVM behaves on x86-64. KVM
finishes a fault that had to wait for the pager from a worker thread. That
worker asks for the page writable, regardless of the guest's access type. So a guest
that only reads memory it inherited reaches the pager as a store. When a store
read only its own page, a fork's first pass over its memory cost one round trip
per 4 KiB. A GCE fan-out on 2026-09-22 took 21,130 faults, and 20,016 of them
were copy-on-writes.

The run is installed shared. Every page of it except the faulting one is mapped
read-only under the identity its volume gives it. So the pages the guest reads
next are served without a fault and stay shared. Only the page the guest stored
into becomes private. The faulting page is not mapped read-only first, because
its copy is about to replace it. So a store still costs no revocation. The
faulting page is also not bound to the shared page. This memory region holds the copy
at that position. A binding to the shared page would be a second owner of that
page's memory while the copy is made. A migration destination's backing is read
one page at a time. Only a load can tell whether the source still holds a page,
and it answers per page.

A store into fresh memory has no page to copy and nothing to fence. Fresh memory
is a zero-mapped page, or a hole in the volume that the guest has never touched.
So nothing is revoked. One mapping command replaces the zero mapping or the trap
with a private page. A zero mapping keeps serving reads until the replacement
lands. The page is a free arena slot. The slot was punched, so it already reads
as zeros. The Linux arena allocates it without writing zeros into it, the kernel
clears it during allocation, and the volume is not read.

**A store replaces a mapping; it never revokes one.** A copy-on-write of a page
that the guest maps read-only, such as a shared page or a page that a seal
write-protected, has a copy to put in that mapping's place. The protocol's MAP
over a range replaces whatever mappings its pages had. The client builds the new
mapping, registers it and `mremap`s it over the guest's addresses in one
command. So one command serves the store and the whole run that the two rules
copied with it. A revocation installs no page table and wakes nothing. It is used
only for these cases:

- a reclaim taking a victim
- a settle handing a page back to its origin
- a retire handing back a page for which the volume holds no object
- an abandoned checkpoint
- a store whose mapping the client refused

Until 2026-09-22, every private page first cost a revocation. A GCE fan-out of
three forks running `cargo test` spent 1,753 s of 5.4 million commands on
revocations: 2.6 revocations per fault, about one per page the guests wrote.

**Every revocation other than a store's is sent as one command per run.** A
settle re-shares thousands of pages at a checkpoint. An abandoned seal gives
thousands back. A retire hands back every write-ahead page its guest never
stored into. One round trip per page, serialized on the mapping lock, would be
a stall the guest notices. Until 2026-09-23 the retire sent one command per
page. A GCE fan-out of two forks spent 12,428 revocations on this, against
15,477 write-ahead pages. That is about one per page each fork wrote, so it
looked like a cost of the store path, but it was not. The retire now checks and
revokes a batch's handed-back pages together, before the walk that publishes the
rest (`MemoryRegion.revokeHandedBack`).

Replacement instead requires an ordering, which
`vmmemory/replacement.go` implements. A store removes its binding from
the page it copied from. Until its mapping command lands, the guest keeps
reading that page's offset, although nothing names the page any more. So the
page stays in place. No reclaim may take it. If this store held its last
binding, its memory is released after the command and not at the unlink. A
store may fail to map its run, because the client is out of mapping budget or a
command failed. The store then revokes the run instead, because it has nothing
to put in its place.

### Keeping a memory region's mappings whole

Every separately mapped run of a guest's memory is a mapping in its VMM process.
A private page written into the middle of an inherited run turns one mapping
into three. The kernel's cap of 65,530 mappings is only the final limit. Each
separate dirty run also costs a write-protect command in a checkpoint's pause.
The kernel's mapping operations slow down as the number of mappings grows. A
fragmented range can never get a huge mapping. Three rules always apply, and the
mapping budget backs them up.

**A private page lives at its own offset.** Each 2 MiB-aligned range of a memory region
that holds a private page owns one extent of the arena's offset space. An extent
is one range's worth of consecutive offsets. A private page of that range goes
at the same offset within the extent as it has within the range. So private
pages that are adjacent in the guest are adjacent in the arena and form one
mapping, in any write order. A range's mapping cost depends on
how often it alternates between shared and private pages, not on how many of
its pages are private. Extents are taken from the offset space and returned to
it whole. The arena counts pages, not extents. There are two intentional
exceptions:

- A store that copies away from the copy a checkpoint froze takes an ordinary
  offset. Its own offset holds the bytes that checkpoint is uploading.
- A page that a migration destination loads privately from the host that still
  holds it arrives in a run, like any other load.

`Stats.PrivateExtents` is the number of ranges that own an extent.

**A store closes a small gap once its memory region is near its mapping budget.** A
store may land within sixteen pages of a page that its range already holds. The
store then makes the pages between them private in the same fault, with one
mapping command, so the two runs become one. The worst case is a guest that
writes one page in every seventeen. That costs seventeen times what the guest
wrote, compared with 512 times at a 2 MiB page. The value sixteen comes from a
measurement. A 4 KiB fan-out on 2026-09-21 recorded 1,979 gaps between private
runs, and 1,532 of them (77 %) were sixteen pages or fewer. A gap is never
closed across a range's boundary, because the extent belongs to the range.

The rule saves mappings and costs memory. So it applies only when mappings run
short: a memory region closes gaps only after its process has refused it a mapping.
Before that, a scattered store gets a separate mapping. On 2026-09-23, forks of
a seeded database updating keys at random held 4.7 GiB each with the rule always
on. The guest's writes caused 345,000 page copies, and the rule caused
3,040,000. Plain Firecracker's clones held 0.69 GiB. A process gets half the
node's `vm.max_map_count`, which is 1,048,576 on a current distribution. So a
memory region is near its budget only after half a million mappings.

**A range that is half private becomes private.** When half a range's pages are
at their offsets in its extent, the pager copies the rest into the extent's
holes. The range is then one mapping, needs one write-protect command at a seal,
and can get a huge mapping. Pages already in the extent are not copied again. So
this costs at most twice what the guest wrote into that range.

Neither rule waits or evicts. The run ends at a page that has no free dirty
reservation or no offset of its own, or whose offset a checkpoint still holds.
That page is left unchanged. A page that a rule copied has an origin, like any
other copy. So the settle after a pause hands back the pages the guest never
wrote. The exception is a range that was made whole. A settle leaves such a range
whole, because handing one page back would split the range into three mappings
again for a page the guest is about to write. `Stats.RuleCopies` counts
the pages the rules copied, separately from the pages the guest stored into.

**The mapping budget turns the gap rule on.** The client can refuse a store's
mapping command because of its mapping-count budget. The store then makes the
range the guest is writing in whole, with one command, and is served again. From
then on its memory region closes gaps. `Stats.MappingMerges` counts how often this
happened. If a memory region keeps merging, its guest fragments memory faster than the
rules can keep it together, even near the budget.

PMEM's pager does none of this, because its page is the whole range. It has one
page per range, so there is nothing to place, no gap to close and nothing to
fill. Its offset count equals its page count.

A store into fresh memory also writes ahead. The fresh zero pages after it in its
read-ahead run get private pages in the same command. So do the pages before it,
when the run ends first. The total is at most `Config.WriteAheadPages` pages. On
a production host that is 8 MiB of the pager's pages, or one page when the dirty
budget is too small for runs of that size. The slots continue consecutively from
the previous page's slot where those slots are free. Like read-ahead,
write-ahead takes only free arena slots and free dirty reservations. It never
evicts or waits. Only the faulting page may.

The write-ahead run is mapped writable. So a guest writing fresh memory in order
faults once per run instead of once per page. As a result, the pager never
learns which pages of the run the guest stored into. Each page holds a dirty
reservation, spills under pressure and is written back by a checkpoint like a
stored page, including pages that are still zeros. A guest whose stores would
just fit the dirty budget can therefore run out of it earlier, by the number of
write-ahead pages it never used. A migration source serves those pages as held.
`Stats.WriteAheadPages` counts the pages that runs mapped beyond the faulting
pages. `Stats.WriteAheadZeroPages` counts the ones whose written-back bytes were
still all zero. That is the closest the bytes can show to "never stored into",
because a store of zeros looks the same.

**Both pagers write ahead, including the one with the small page.** Write-ahead
serves only fresh zeros. A hole and a zero mapping have no resident page and no
page identity. So nothing shares them, and making a store's neighbours private
loses no sharing. For that reason a 4 KiB RAM page uses the same 8 MiB run as
PMEM. The small page exists to protect the sharing of pages that a checkpoint
published, and this run never touches such a page. A guest's boot writes real,
contiguous data. A 16 GiB guest's boot on GCE made 103,035 pages private:

- 65,280 are the 256 MiB memmap, which the kernel writes page by page as it
  initialises it.
- 16,384 are swiotlb's 64 MiB bounce buffer, which is memset in one block.

So 80 % of that boot is two runs written forwards. With an 8 MiB run they take
32 faults instead of 81,664.

**A write-ahead page that the guest never stored into costs nothing after the
next checkpoint.** It reads back as zeros, so the publication gives it no object.
A page that reads as all zeroes is left out of the index, and that absence is
the only record that it is a hole. The volume then reports it as a hole. The
pager learns this in the retire. The retire looks up the identity the volume now
gives each page it is retiring. If the volume holds no object for a page, the
pager takes the page away from the guest, releases its resident page and
returns its dirty reservation. The page is then as untouched as before the
store. The settle cannot do this. It would duplicate the publication's own
zero-page test. Also, a settle compares a copy with the page it was copied from,
and a page made from zeros has no origin.

### A pulled VM's faults

A VM can be marked to [pull its whole memory](hosting.md#pulling-a-vms-memory)
onto its host's disk. The pager does nothing different for it. A fault asks
the volume for its window as always, and the volume reads a run through the
page cache. A run's pages that the cache holds in memory come from there, the
ones on its disk come from there, and only the rest are requests of the store
([the page cache's disk](volumes.md#the-page-caches-disk)). So once a pull is
complete, a cold fault costs a local read and a decode instead of a round trip.

This is also what an eviction costs such a VM. The pager drops a clean page
rather than spilling it: its volume holds its bytes. For a pulled VM those bytes
are on the local disk, so the refault reads them there. The spill file is not
the copy. It holds only private pages, and it bounds the dirty pages the pager
admits, not what a VM may read.

A migration's destination attaches through the peer backing. A page the source
still holds, and every page no checkpoint holds, comes from the source as
before. Every other page the peer backing reads from the destination's own
volume, which is what the pull fills. The pull never reads the source: the
pages only the source has become this host's own dirty pages when they arrive,
resident or spilled, and the next checkpoint publishes them.

## What the sharing is worth

`Stats` counts what the pager has done:

- `IdentityHits` counts every page mapped to an identity that was already
  resident.
- `CopyOnWrites` counts every page of which a store took a private copy.
- `UnchangedPages` counts every page that a settle found to hold the same bytes
  as the page it was copied from. Such a page came from a write fault that the
  guest never stored through, and no checkpoint publishes it.

These counters never decrease. So a host whose guests have all diverged shows
the same values as one whose guests share everything. The checkpoint's log line
reports the settle's count next to its dirty set, as `unchanged_pages`.

`Host.Sharing` is the matching gauge, per memory region kind:

- `UniqueBytes` is the host memory the arena holds. One resident page counts
  once, however many memory regions map it.
- `MappedBytes` is the sum, over memory regions, of the resident pages each memory region
  maps. A page that three memory regions map counts three times.
- `SavedBytes` is the difference. It is the memory this host did not have to
  provide.

Every alias counts in `MappedBytes`. That includes two memory regions of one VM, and a
checkpoint's copy of a page that the guest still shares with it. A page that no
memory region maps is still memory the arena holds. An example is the page a store
copied away from. It stays in the arena until it is compared with that copy or
inherited by the next memory region that names its identity. Such a page counts in
`UniqueBytes` and in no mapping, under the kind of the memory region that created it.
The gauge measures only resident sharing. A fork inherits all of its parent's
page identities. The ones that neither has faulted in are shared in the store
and on the wire and cost this host no memory, so the gauge does not include
them.

`MemoryRegionStats` reports the same for one memory region:

- `ResidentPages`: the pages that hold host memory.
- `PrivatePages`: the pages whose bytes belong to this memory region and not yet to its
  volume, whether resident, spilled or held by a checkpoint.
- `SharedPages`: the resident pages that at least one other memory region of this pager
  also maps.

All three are also reported in bytes. Page counts of different geometries cannot
be added, but bytes can.

A memory region has a kind, RAM or PMEM, which is passed to `Attach`. Inside one pager,
no fault, seal or page depends on the kind. The kind decides which of a host's
two pagers the memory region attaches to. It also lets a host report whether the
sharing is in its guests' memory or in their disks. The caller that attaches the
memory region states the kind. It is never inferred from a volume's name. A host that
decides which pager a VM's memory regions would be admitted to, before a machine
exists, states the kind itself.

The pager has memory regions but no concept of a VM, so the host adds the per-memory-region
numbers up per VM. `Host.PrivateBytes` in `host` is one VM's private
bytes across every memory region it maps. `/status`, `/metrics` and the VM listing
report it. The host reads each memory region through the pager of that memory region's kind.
The Prometheus gauges are `sproutfs_pager_unique_resident_bytes`,
`sproutfs_pager_mapped_resident_bytes` and `sproutfs_pager_shared_saved_bytes`.
Each carries `kind="ram"` or `kind="pmem"`. Every other pager series carries the
same label. The two pagers use different page sizes, so a sum of their page
counts would be meaningless. `sproutfs_pager_arena_bytes` is the only total, and
for the same reason it is in bytes.

Faults serialize only within one read-ahead run. Different runs and different
volumes proceed concurrently. A short host lock covers capacity accounting,
binding pointers and the shared index. No backing read, spill or mapping
acknowledgement holds it. Each resident page has its own transition lock for
mapping changes and reclaim. Read-ahead skips a page whose lock is busy instead
of waiting. A memory region's per-page state is kept under that memory region's binding map
lock. That state records whether a page is mapped, what it is dirty under and
which checkpoint holds it. This lock is needed because only one pair of holders
does not otherwise exclude each other: a reclaim that revokes its victim's pages
under a page's lock, and a seal that reads those pages under the memory region.

Reclaim uses fault and read-ahead recency. Accesses through page tables that are
already present do not update it. So eagerly mapped pages can be reclaimed while
they are hot. Evicting a shared page requires coordinating that page's aliases
across processes. An ambiguous mapping acknowledgement keeps its possibly live
slots allocated until the process exits.

## Seal, checkpoint and verification

A checkpoint is the only way a memory region's dirty pages become durable. The pager
never writes to a volume, even for a guest's flush. A virtio-pmem flush is a
FLUSH request that the device sends and holds. The guest's flush returns when
the host answers. The host answers at once if the VM holds no unpublished disk
write older than its flush bound. Otherwise the host asks for a disk checkpoint
outside the interval's schedule and answers once that checkpoint covers the
write ([hosting](hosting.md#the-checkpoint-loop)).

A seal also takes over the memory region's loss window. The age of the oldest page the
seal freezes becomes the sealed set's age. The memory region's own age restarts at its
next store. Retiring the set as published drops its age, because the store then
holds those bytes. Abandoning the set hands its age back to the memory region. A set is
abandoned when a publication does not land, or on an unseal. The memory region then
keeps the older of the returned age and the age of anything it has written
since. A window that restarted at every failed publication would bound nothing.
A host that cannot publish is the same host whose publications keep failing.

Sealing a memory region revokes write access to its dirty pages and returns. It
write-protects the pages the guest already has. Nothing is copied and no data
crosses the network. Write protection is applied in place, with one range
write-protect per run of consecutive dirty pages. This applies regardless of the memory
those pages use and of how many of the client's mappings the run spans. No
mapping is replaced and no page table is installed or dropped. So the guest
keeps reading the same pages through the same page tables, and only its next
store traps.

**A seal's pause consists only of those commands.** Once a run is protected, any
store to it traps and waits for the memory region. So the set is fixed at that point.
The seal's per-page work runs afterwards, as a walk, while the guest is already
running. The walk holds the memory region that the seal took. For each page it:

- moves the binding into the checkpoint
- hands the checkpoint that page's reservation and the page it was copied from

The memory region keeps its dirty set as runs for this purpose. Every transition that
changes whether the next seal would protect a page updates the runs. So the
pause reads O(runs), never O(pages), and takes the whole set in one step. A fault
of that memory region waits for the walk. Nothing else waits for it. The checkpoint's own
readers (the settle, the page list and the upload) run after the pause anyway,
and they wait for the walk there. On GCE, a capture of 2,204,672 sealed RAM
pages at a 4 KiB page paused for 2.14 s. The 2,264 protect commands took 0.18 s
and the walk took 1.97 s. `seal_ns` and `seal_walk_ns` report the two. Only
`seal_ns` is time during which the guest is stopped.

While a seal holds the memory region, only a reclaim can revoke a mapping. A reclaim
revokes its victim's pages under that page's lock only. The seal does not walk
the pages, so it does not hold those locks. So the memory region has a protection lock.
Every revocation holds it shared, and the seal's write-protect commands hold it
exclusively. Under this lock, every revocation has either finished, and is no
longer in the runs the seal reads, or has not begun. Nothing that holds this lock
then waits for the memory region or for a page.

If a seal's protection fails partway, the seal captures nothing and takes no
page. The pager revokes the mappings of the runs that were protected. So the
guest faults and maps them writable again, instead of resolving a store against
a read-only mapping. The next checkpoint takes the whole dirty set.

The seal waits for page-table work but never for data. A fault holds the memory region
shared during its planning, its metadata and its page-table commands. It
releases the memory region during the two steps that can be slow:

- the backing read: a volume load, or a migration source that keeps answering
  BUSY
- the reclaim that a new page may need, which revokes a victim's mappings and
  writes its bytes to the spill file

A seal taken during either step does not wait for it. The window's stripe still
owns that window for the whole fault. The stripe is taken before the memory region, so
there is one lock order. The memory region's exclusive holders are a seal, a retire, an
unseal, a handoff and a detach. Only the detach waits for faults. It waits on a
separate lock that a fault holds from start to end.

So a reclaim can hold a page that the seal is about to take. The seal never
waits for it. A reclaim is the only thing that can hold a page of the memory region
being sealed. It ends with the page nonresident and the page's bytes in that
page's own dirty reservation. So the seal joins the checkpoint's copy of the
page to that resident page without taking its lock, and lets the reservation
carry the bytes. For this reason, whether a reservation's slot holds its page's
bytes is state of the slot, not of the binding. The seal hands the reservation to the
copy while the reclaim is still writing to it, and the slot is the only thing
both of them name. The seal revokes such a page instead of write-protecting it.
The reclaim is revoking it anyway, and revocation is stronger. The reclaim reads
the page's aliases and then the reservations they name. The seal joins the copy
to the page before it hands that copy the reservation. So the reclaim walks the
alias set again if it has grown since the reservations were read. The alias that
the reservation moved to is then in the set. So the page's bytes reach a
reservation in either order.

Retiring a checkpoint walks a set as large as the capture's. So it walks it the
same way the seal does: in bounded batches, releasing the memory region between them.
A fault then waits for one batch, not for the whole walk. Each batch needs
volume metadata: the identity the volume now gives each page, which decides
whether the page joins the sharing index. That metadata is one lookup per
read-ahead window, done before taking the memory region or any page. Only one retire or
unseal runs at a time. A page already retired is skipped, so repeating a failed
retire finishes the remaining work.

The sealed pages are detached copies. They alias the pages the guest had, and
they own the dirty reservations under which those pages were admitted. A store
into a sealed page takes the write-protect fault and copies on write. The guest
gets a fresh private page with the current contents, and the seal keeps the
original. The sealed bytes never change. The page, including its memory, stays
with the checkpoint until the fresh page is bound. A store can fail before that,
for example when the arena cannot fill the slot or a needed reclaim is not
possible. The page then stays where the seal left it, and the guest faults
again. If the seal ends while that reclaim runs, the page gets back its own
reservation or clean state instead, and the store restarts its decision from
the beginning. The dirty budget counts sealed pages together with live private
pages. A guest that dirties pages faster than its checkpoint uploads them waits
at that budget. It does not fail or exceed the budget.

The dirty budget belongs to the host, so the wait does too. A store waits for
the next checkpoint that lands anywhere on the host, not only for its own
memory region's checkpoint. A migration destination's read of a page that the source
still holds waits in the same way. The load takes that page as this memory region's
dirty state. So the fault's window extents decide that it needs a reservation.
Before it loads anything, the fault releases the memory region, its page and its I/O
permit, and takes a reservation through the waiting path. Read-ahead around it
takes only free reservations. It leaves pages for which it finds none to a later
fault.

If no checkpoint is in flight, the pager asks for one instead of refusing the
store. `Pressure` is the pair of callbacks through which the memory region's owner
responds. The host offers the memory regions with the largest dirty sets first, because
those release the most. A fault that crosses three quarters of the budget asks
for a checkpoint before any store has to wait. So the checkpoint is already
running when the last quarter runs out. The crossing is measured when the fault
returns and holds no memory region, page or reservation. So every path that admits a
page counts against it:

- the reservation a store waits for
- the reservation a peer-served load takes
- the run of reservations that write-ahead takes without waiting, which can
  cross the mark by itself

A sealed memory region cannot take another checkpoint, so it is not offered one. It
answers the wait only when ending its checkpoint relieves the budget. A
publication's end does that: it returns its reservations, and if it holds none,
it lets the memory region take the checkpoint that will. A fork point's hold does
neither while the children it named are still reading it. So while a fork holds
a memory region here, every other memory region's pressure must be relieved by a checkpoint of
that other memory region.

A store fails only when no memory region can be checkpointed to free the budget. It
fails with `ErrDirtyStalled`, not `ErrCapacity`. The same pressure reports the
memory region to its owner, so that the VM is stopped deliberately and publishes what
it holds. A failed fault kills the VMM and loses those bytes without recording a
reason. This whole path exists to avoid that. For the same reason, a fault has no
separate deadline. The command timeout bounds each round trip inside it, and
the only thing that makes a fault long is this deliberate stall.
`Stats.DirtyWaits`, `CheckpointRequests` and `DirtyStalls` count the three
outcomes. A host that stalls has a budget or an interval too small for its
guests.

The publication reads the sealed set directly from those pages.
`MemoryRegion.Checkpoint` exposes that set as `volume.DirtySource`. It provides the
pager pages the set holds, one whole pager page at a time under an I/O permit
and that page's lock, and the retire that ends the set. A pager page is a store
page. So each sealed page is written as one member of the checkpoint's parts, at
the part and offset that the index records. Nothing copies those bytes into the
volume package on the way. The guest runs throughout. A read of a set that has
already ended fails with `ErrNotSealed`, or with the reason a detached memory region
discarded it. It does not answer from pages that belong to the guest again.

A write fault is not always a store, so a sealed page is not always dirty. On
x86-64, KVM finishes a guest fault that must wait for the pager from a worker
thread. That worker always asks for the page writable. So a cold **read**
reaches the pager as a write fault. On aarch64, the architecture reports the
guest kernel's cache maintenance on a page it executes for the first time as a
write. While these faults wait, the pager cannot tell them from real stores,
because the worker does not finish until the page is writable. So the pager
copies the page and checks afterwards.

**A sealed page whose bytes equal the page it was copied from is not dirty.**
The copy records its origin. When the pager serves a store by copying away from
a resident page that holds a published page identity, the binding keeps a
pointer to that resident page. It does not keep the identity. The pointer is
eight bytes per binding, and an origin that has been evicted is no longer an
origin. Some copies have no origin:

- a page copied from a checkpoint's held copy
- a page copied from the name a fork point lent a private page
- a page copied from another host's unpublished page
- a page made from zeros

A store into a page for which this memory region holds no memory first reads that page
in, under the identity its volume gives it. So the page it copies away from is
one the settle can compare with, and every memory region that inherits that identity
maps it instead of reading it. The page a store copied from stays in the arena
when the binding leaves it. Nothing holds it there, and the next reclaim that
needs a slot takes it like any other clean page.

`MemoryRegionCheckpoint.Settle` performs the comparison. The publication calls
`volume.DirtySource.Settle` once, after the pause and before it enumerates the
pages, while the guest is running. A published identity's bytes never change.
So comparing the sealed page with its origin, under both pages' locks, is a
single `bytes.Equal`. It uses no hash, because a hash would make a wrong answer
possible and would add work to the fault path. It does not read the store,
because that would double a checkpoint's I/O for the pages that did change. For
the same reason, the settle skips a sealed page that the pager has spilled. An
unchanged page leaves the checkpoint's set. So `DirtyPages` does not list it,
and it costs the store nothing. If the guest still shares the checkpoint's copy:

- the binding takes the origin as its resident page and becomes clean
- **the guest's mapping of the copy is revoked**
- the private page is released
- the dirty reservation is returned

If a store lands first, it copies away from the checkpoint as it does today, and
only the checkpoint's copy is released. If a settle leaves a checkpoint holding
nothing, that checkpoint also holds no unpublished write. So the loss window it
took at the seal ends there.

The settle revokes the mapping on purpose instead of replacing it under the
guest. A settle runs while the guest is running. It holds neither the memory region nor
the window that orders a page's mapping changes. So the only change it may make
is one that installs no page table and wakes nothing. It used to install the
origin in place of the copy: one command, no fence, identical bytes and a
write-protected page in both cases. In a fan-out of two children at a 4 KiB RAM
page, a child's guest kernel used to panic on a list entry that the guest itself
had removed. Revoking instead reduced the rate from about one run in five to
about one in thirty, but it was not the fix. The cause was elsewhere and is
fixed: see [a post-copy child's own published
pages](migration.md#a-post-copy-childs-own-published-pages). Revoking costs one fault per page that a settle re-shares. The guest would
take that fault at the page's next write anyway. On a host whose kernel reports
a cold read as a write fault, that fault copies the page again, and the next
settle undoes the copy again.

The settle runs in parallel and then applies its decisions. Each page is
compared on its own, under that page's lock and its origin's lock and nothing
wider. So a settle hands its pages to `Config.SettleWorkers` workers, which
default to the host's processors. The memory regions of one VM settle concurrently.
The comparison changes nothing. Its decisions are applied afterwards, in page
order and in bounded batches under the memory region, in the same way as a retire. This
lets the revocations go as **one command per run of consecutive pages**. A
settle at 4 KiB re-shares thousands of pages at every checkpoint. One round trip
per page, serialized on the mapping lock, would be a stall the guest notices.
The memory region is released between batches, so a fault waits for one batch and not
for the whole walk. The settle is limited by memory bandwidth, not by the
pager's I/O permits. It takes no permits, because it reads no disk and no store.
The workers share only the count of unchanged pages and the set the checkpoint
will list, both under the checkpoint's mutex. So the result does not depend on
the order in which workers finish, and the simulation can run the same code. An
arena that can compare two of its own slots does so in place. Every other arena
is read into two buffers per worker.

A fork point is not settled. It publishes nothing, and a child waits for its
pause. Its children inherit an unchanged page as an unpublished page. That is
correct, and no worse than not settling. Each child's next checkpoint settles
the page. This is a pager rule, so it applies to RAM and PMEM.

The settle does not prevent the copy. Between the fault and the next checkpoint,
the host holds the page twice. Preventing that needs a host kernel that passes
the guest's access type through, or KVM userfault. Both are TASK-32 in the
[backlog](../backlog/tasks).

The publication retires the seal. A sealed set whose checkpoint was selected
retires as published:

- A page that the guest has not stored into since the seal becomes ordinary
  clean state. Its resident page joins the sharing index under the identity the
  volume now reports, and stays mapped to the guest.
- For a page that the guest copied away from, only the sealed copy remains, and
  it is released.

In both cases the dirty reservation is returned. The page and its sealed copy
are retired together under the resident page they share. A sealed set whose
checkpoint never landed is abandoned instead. `MemoryRegion.Unseal` does the same.
Pages that the guest still shares take their reservations back and are dirty
again, so nothing is lost. Their mappings are revoked, so the next store faults
and maps them writable again instead of trapping on the protection the seal
left. The next checkpoint takes them.

A memory region has at most one outstanding seal. Sealing a sealed memory region reports
`ErrSealed`. Handing off a sealed memory region also reports `ErrSealed`, because a
publication is reading its pages under a volume handle that the handoff would
give away. A seal that fails partway captures nothing and takes no page. Its
half-protected pages have their mappings revoked and become the guest's ordinary
dirty state again. So the next seal takes everything that is dirty at that
time. Pages count as sealed as the walk after the pause takes them. So a capture
that never completes still reports the work that walk did.

The spill file is scratch storage. It is never an acknowledged crash-recovery
image. A starting process truncates it, because a restart is a host loss and
nothing in the file is valid afterwards. For the same reason, it is written but
not synced. Returning a released slot's blocks to the filesystem is optional and
not part of accounting. So a failed or unsupported hole punch costs nothing.

Verification checks writer authority even when cached accesses never fault. The
Linux connection runs it per volume on a timer with bounded deadlines. A failure
closes the control channel. The supervisor must then terminate the process
before mappings are detached. Verification checks authority at the time of the
call. It is not an expiring lease. The storage guarantee is that a fenced writer
cannot obtain another conditional write.

### A refault decides again after its reclaim

The probe build's `TestSealTakingAReclaimingPagesReservationKeepsItsBytes` once panicked under load. The cause was a refault that acted on a decision a checkpoint had already superseded. The pager's audit reported `probe bind: page N of memoryRegion … was given slot -1 from outside its own store path while it owned generation G, and now takes slot S — a lost write`. The report came from the store's own `takePrivate`, in about one lane in eight under contention.

No write was lost. At every step, the page the guest was bound to held the bytes the guest last stored. With the audit finding made non-fatal, thirty lanes ran to completion, and the test's own `reads %d, want the %d the guest stored` check never fired. The audit had caught something else: the pager granted a binding the right to store into memory after that binding's dirty epoch had already ended.

A reclaim for a private page releases the memory region while it looks for an arena slot. So a seal and a retire can both run inside a fault that has already decided what the page it serves is. The store path re-checks its decision across its own reclaim: `fault` compares the checkpoint's copy before and after. The spill refault in `loadOnce` did not re-check. A checkpoint taken in that window retires the page: the volume holds its bytes, the reservation that spilled them is returned, and the binding is clean. The refault then bound a private page into the binding anyway. That page has neither a reservation nor a checkpoint. `evictBatch` punches out a page in that state without writing it anywhere. Nothing names the page, so nothing that inherits the identity the checkpoint gave it can map it. Every other memory region of that volume reads its own copy of bytes this host already holds. The audit's generation bookkeeping is correct. The binding that owed the audit a newer generation was one the pager should never have granted.

`loadOnce` now reads the page's dirty state and the checkpoint's copy of the page together, before and after the reclaim. If either changed, it decides again from the start what the page is (`vmmemory/fault.go`, `bindings.go`, `privateEpoch`). `TestARefaultWhoseCheckpointRetiresWhileItReclaimsGivesThePageToTheVolume` drives the interleaving through a reclaim seam. Without the fix it fails on every run in both builds. The ordinary build fails with the second memory region reading its own copy. The probe build fails with the same panic and the same stack. Measured on 2026-09-22 on a fifteen-core machine, with 50 lanes each and a detector on the grant: **10 of 50 lanes before, 0 of 50 after**. At that rate, the chance of a clean result by luck is about 1 in 70,000. The panic that the lanes produce is rarer than the grant that causes it: about 1 lane in 50 on this machine, against 1 in 8 on the eight-core machine the earlier counts came from. After the fix the panic count is 0 of 150 lanes, but the grant's count is what supports the result.

## Ownership

The Go pager owns:

- The arena's memfds and their slots, page identities and alias references,
  and which file each session may read.
- Fault resolution, copy-on-write decisions and private page allocation.
- Spill, reload, and the decision to evict a page.
- Punching the arena and reusing a slot, only after accounting for every alias.

The Rust library owns:

- The stable host virtual address ranges identified as PMEM or RAM.
- UFFD creation, registration and descriptor transfer to the Go pager.
- Applying mapping changes in its own process and acknowledging them.
- Keeping mappings, descriptors and generation bookkeeping alive for the
  session.

PMEM and RAM share one mapping implementation and one durability contract. Both
become durable only through a checkpoint. A guest's PMEM flush does not make
anything durable. It waits for the host, which answers once a disk checkpoint
covers every disk write older than the host's flush bound. A local spill is not
durable either.

## Rust interface and lifetimes

A session maps one volume as one contiguous range. A VM's RAM is one session,
and each of its PMEM devices is another session, each over its own socket. A VM
has one RAM volume, `ram0`. The library knows nothing about guest addresses. The
VMM places a volume's bytes in the guest's address space, as described under
[the Firecracker build](#firecracker-build-and-process-lifecycle).

`Session::connect` reserves the memory region's range, creates the UFFD and exchanges
descriptors. The range is reserved before the page size is known. So it is
reserved at the largest page size this transport maps, which is aligned for both
sizes. The attachment then states this memory region's page size and what its files
are made of. The client checks three things:

- that it maps that page size
- that the private file's descriptor, and every file's after it, is that kind
  of memory: an explicit 2 MiB HugeTLB file, or an ordinary shared memfd of
  4 KiB pages
- that the memory region's length is a nonzero multiple of the page size

A mismatch ends the session before any address is exposed to the embedder.
`Session::page_size` reports the agreed page size. `Session::memoryRegion` returns an
address descriptor, not a Rust borrow. The session owns its lifetime.
`Session::run` services commands on a dedicated thread. The Go pager reads the
UFFD directly. Rust does not relay fault messages.

`Session::control` returns a cloneable device-side handle. Its `start_seal` asks
the host to seal this memory region while the session thread keeps serving mapping
commands. Sessions seal independently, with at most one request pending per
session. So a coordinated capture issues all the requests and then waits.

Only the host's deadline decides a seal. The host bounds the command. If it
cannot finish a seal, it answers with a failure, which means the checkpoint did
not happen. Every page the seal took goes back to the guest as ordinary dirty
state. The memory region stays usable, and the next checkpoint takes the whole dirty
set. The VMM answers its capture request with that failure and keeps running.
Its caller unseals and resumes. The client's own wait is much longer than the
host's: five minutes, against the host's command timeout. So it is only a
backstop for a host that has stopped answering. If the client's timer fired
first, it would close the control session and kill a guest whose checkpoint was
only slow. A wait that does expire closes the session.

All raw-pointer users must stop before the session is dropped. Ordinary Rust
references must not be held across mapping changes. External device and kernel
pins require coordination by the embedding process. The library provides no
long-lived pin interface. On a control or mapping error, the session becomes
terminal and keeps its UFFD and mappings until it is dropped. If it closed the
UFFD and kept running, anonymous fault traps would revert to ordinary
zero-filled memory. Both the fixture and the production supervisor terminate the
process on unexpected control loss. The supervisor holds every connection and
descriptor until `waitpid` confirms exit. For an orderly shutdown, stop all
memory users and unregister the KVM slots first. Then send STOP and let the
process release its session.

## Mapping replacement

Never expose a new mapping and then register or protect it. Another thread could
access it in between and bypass demand loading or copy-on-write. Instead, the
library does the following:

1. It builds the mapping away from the live address.
2. It registers the mapping with UFFD. If the mapping is immutable, it
   write-protects it. A mapping of a read-only file is always immutable.
3. It replaces the live range with `mremap(MREMAP_FIXED | MREMAP_MAYMOVE)`.
4. It acknowledges only after that syscall completes.

Some runs of one batch form a single stretch of the memory region. When there are three
or more such runs, they are built together in one reservation. The whole span
then takes one `MADV_DONTFORK`, one `UFFDIO_REGISTER` and one
`UFFDIO_WRITEPROTECT`, instead of one of each per run. Each run still takes its
own `mremap`, because `mremap` moves a single mapping, and two runs of a batch
are never one mapping. Runs that are adjacent in both the memory region and one file
have already been merged before this step. The remaining runs are adjacent only
in the memory region, and the kernel keeps those separate. A span may hold runs
of different files, and each run is mapped as its own file is. A batch of 64 such runs
costs 136 kernel calls instead of 320. Combining the `mremap`s would require the
wire protocol to express it, and it does not.

Nonresident ranges are anonymous readable and writable mappings registered for
missing and write-protect faults, with no populated pages. They are not
`PROT_NONE` ranges, because those would raise ordinary protection faults.

Resident ranges are views of a file, registered for missing, minor and
write-protect faults. The private file is mapped `MAP_SHARED`. A read-only file
is mapped `MAP_PRIVATE`, because the kernel refuses to register a shared
mapping of a read-only file with userfaultfd. On HugeTLB it is also mapped
`MAP_NORESERVE`, so it reserves no pool pages for copies that never happen. An
immutable page is armed for synchronous write protection before it becomes
accessible. `UFFDIO_CONTINUE` installs the file's own page, read-only in a
private mapping, so a read-only file's pages are shared as physically as the
private file's. A store into one traps to the pager, which replaces the
mapping. A store the kernel let through would copy into memory the pager never
sees, so the pager never clears write protection on a read-only file's range. The pager installs resident
backing with `UFFDIO_CONTINUE`, including its write-protect mode for shared
data. It does not copy bytes into a private anonymous destination. This
makes the page physically shared. The pager issues `UFFDIO_CONTINUE` over whole
mapped ranges, not only over faulting pages. So read-ahead and populated pages
get their page tables before any access. Present pages are skipped. A fault is
resolved over its whole pager page, including every host page in it. A range
`UFFDIO_CONTINUE` that meets a present host page reports only that page. So the
pager then finishes the range one host page at a time instead of trusting the
result. One mapping command covers a run of consecutive pages whose arena slots
are also consecutive. This keeps the process's mapping count near the number of
runs instead of the number of pages.

Removing write access is not a replacement. The pager issues one
`UFFDIO_WRITEPROTECT` over the whole run on the live addresses. The kernel
applies it to every registered mapping the range covers. A seal does this. It
is why a seal costs per run and not per page: nothing is built away from the
live address, nothing is remapped, and no page table is rebuilt.

## Kernel and VMM constraints

The library requires `UFFD_FEATURE_EVENT_REMAP`. Without it, Linux drops the
UFFD context on remap. With it, remap completion waits until the event is
consumed. **The Go UFFD reader must keep draining remap events while another
goroutine waits for a mapping acknowledgement.** The following features are also
required, all together:

- `UFFD_FEATURE_PAGEFAULT_FLAG_WP`
- missing and minor faults on HugeTLB (`UFFD_FEATURE_MISSING_HUGETLBFS`,
  `UFFD_FEATURE_MINOR_HUGETLBFS`)
- missing and minor faults on shmem (`UFFD_FEATURE_MISSING_SHMEM`,
  `UFFD_FEATURE_MINOR_SHMEM`)
- `UFFD_FEATURE_WP_HUGETLBFS_SHMEM`, which is write protection for both

The set is negotiated once, when the UFFD is created. This happens before the
attachment states which of the two geometries the session runs. A host runs a
pager of each kind, so a kernel that supports only one kind cannot run this
build. The negotiation fails with a clear error. A second descriptor asks the
kernel which features it supports, and the error names the missing features.
Both the negotiation and the per-range ioctl mask are checked. Anonymous write
protection appeared in Linux 5.7, shmem minor faults in 5.14 and shmem write
protection in 5.19. These are the versions that introduced the features. They
are not qualified versions.

A seal also requires `UFFDIO_WRITEPROTECT` to apply across every registered
mapping its range covers. The pages of a run of dirty pages are separate
mappings whenever their arena slots are not consecutive. For a real guest that
is the usual case. Linux walks the VMAs of the range. The suite tests this
directly, at the syscall fixture and through the real client. Suppose a kernel
required one mapping per call. It would fail the whole ioctl instead of
protecting part of a run. That fails the seal and undoes it. The seal would then
have to split its runs at mapping boundaries, which it does not currently track.

Faults must be visible to the kernel. `UFFD_USER_MODE_ONLY` does not cover KVM's
own accesses. So the deployment must grant kernel-fault UFFD through the
permitted syscall route or `/dev/userfaultfd`, and allow it under the actual
seccomp policy. The client's mapping-count budget is opt-in. Zero disables it and
reads nothing from `/proc`. A jailed process without `/proc` needs this to
attach. A nonzero limit is an admission bound. `/proc/self/maps` can only make it
less conservative.

Eviction cannot rely on `madvise`. Dropping anonymous contents leaves the shared
backing resident elsewhere. Punching the backing makes future accesses read
zeroes. So the pager revokes every alias and waits for the acknowledgements
before it releases a slot.

Guest PMEM is byte-addressable memory. Its stores become durable only through
the host's checkpoint. So a completed store is not durable. A guest flush
guarantees what the host's answer to it says. Guest DAX bypasses the guest page
cache but does not change the host backing. Firecracker requires 2 MiB PMEM
alignment. Its save order is fixed:

1. Pause the vCPUs.
2. Save devices, before KVM state, because device completion can inject
   interrupts.
3. Capture memory.

So a seal must tolerate device accesses after the vCPUs pause. Those accesses
copy on write like any other store, and the sealed bytes stay unchanged.

The adapter refuses any `huge_pages` setting for managed RAM. The pager owns
that memory and states its page size when the session attaches. A setting there
would state something about backing that the VM does not choose. The adapter
also rejects ballooning, memory hotplug, vhost-user and asynchronous block I/O
with managed RAM. A coordinated capture requires managed PMEM for every disk and
refuses ordinary block devices. A managed virtio-pmem flush waits for the
host's answer to the FLUSH request that the device sends for it. KVM slots are
unregistered before mappings are dropped. Linux mapping invalidation covers
ordinary CPU accesses and the tested KVM accesses. No Rust lock surrounds each
load. Upstream's UFFD restore copies pages into each VM's anonymous memory and
cannot share a page between VMs. This integration replaces it.

## Control protocol, version 10

Version 9 peers are rejected because the arena moved off ATTACH and into files.
ATTACH carries no descriptor and no length. The pager hands the client each
file it may map in a FILE frame, with its descriptor, and a MAP names the file
it maps from. A version 9 peer would read ATTACH as the arena and a MAP's file
number as a protection flag. Both ends refuse the other's version at HELLO and
ATTACH, before any guest memory exists.

Earlier versions were rejected for these reasons:

- Version 9 rejected version 8 because FLUSH was new. A pager ends a session
  when it receives a control message it does not know. So a version 8 pager
  would end a guest at its first flush.
- Version 8 rejected version 7 because ATTACH's length is the arena's offset
  space, not its capacity. The arena is a sparse file whose offsets are not its
  pages. So the number that the client checks the descriptor's size against,
  and uses to bound a MAP's arena offset, is the number of addresses. A version
  7 peer would read it as a promise of that much memory.
- Version 7 rejected version 6 because the page size was no longer one number
  that both ends know. ATTACH carries the page size of this session's memory region
  and the kind of memory its arena is made of. A 2 MiB page number read as a
  4 KiB page number names a different page.
- Version 6 rejected version 5 because of two removals. A session carries one
  memory region, so the `memoryRegion` field was removed from the frame and the frame is 56
  bytes. HELLO carries no page size.
- Before that, version 4 was rejected because its FLUSH request was removed,
  and SEAL took its frame kind.

The supervisor, Rust adapter and Firecracker integration must be deployed
together.

A Unix stream carries fixed 56-byte frames of seven little-endian `u64` fields:

```
kind, id, offset, length, backing, generation, flags
```

Frames must be read and written completely; stream boundaries are not message
boundaries. `SCM_RIGHTS` carries exactly one descriptor on HELLO and on FILE,
and none on any other frame. The client reads every frame with `recvmsg`, after
READY too, because a FILE can arrive at any time. A plain read that reached a
FILE would have the kernel close its descriptor. The client ends the session on
a descriptor with any other frame, on a FILE without one, and on truncated
ancillary data. A pager refuses a memory region by closing the socket instead
of sending ATTACH. The usual reason is a full logical-page cap. The client
reads this as an orderly close and reports it as the pager closing before
attaching, not as a malformed descriptor message. The client's report of why it
could not start must name the end that did not answer.

| Kind | Value | Fields |
| --- | --- | --- |
| HELLO | 1 | `id` protocol version, every other field zero; carries UFFD |
| MEMORY_REGION | 2 | `offset=host address`, `length=bytes`, `flags=kind` (1 PMEM, 2 RAM) |
| ATTACH | 3 | `id` protocol version, `offset=this memoryRegion's page size`, `length=0`, `backing=arena kind` (1 explicit 2 MiB HugeTLB, 2 ordinary shared memfd), `flags=mapping-count budget`; carries no descriptor. The files follow as FILE frames |
| MAP | 4 | Command ID, memory-region-relative offset and length, the offset in its file in `backing`, next generation; `flags` bit 0 is 1 for immutable and 0 for writable, and the bits above it are the file number. A writable MAP must name file 0 |
| REVOKE | 5 | Command ID, memory-region-relative offset and length, next generation; installs nonresident fault traps |
| ACK | 6 | Echoes command ID and generation; `flags=0` success or a positive Linux errno |
| STOP | 7 | Command ID; the embedder must have stopped all memory users |
| SEAL | 8 | Client request ID; write-protects this session's memory region's dirty set and records it, completing in page-table time |
| RESULT | 9 | Answers a SEAL or a FLUSH: echoes the request ID; `flags=0` success or a positive Linux errno |
| MAP_BATCH | 10 | `length=run count` (1–1024), followed by that many MAP or MAP_ZERO frames; one ACK for the batch |
| READY | 11 | Ends the mandatory attach population; acknowledged before `Session::connect` returns |
| MAP_ZERO | 12 | Explicit sparse zero range, `backing=0`, `flags=1`; installs populated anonymous shared-zero page tables under write protection |
| FLUSH | 13 | Client request ID, every other field zero; the client's guest flushed this session's memory region, which must be PMEM. RESULT answers it once the host has made the flush durable, and the guest's flush returns then |
| FILE | 14 | `id` file number, `length` its size in bytes, `backing` arena kind, `flags=1` writable or `0` read-only, every other field zero; carries the file's descriptor. Not acknowledged. A FILE that repeats a number with a larger length grows that file |
| DROP_FILE | 15 | `id` file number, every other field zero. Sent once every mapping of the file is revoked. The client closes its descriptor. Not acknowledged |

### Files

File 0 is the memory region's private file. It is the only file whose
descriptor is read-write, and the only one a writable MAP may name. Every other
file is read-only. A shared arena sends one file: the arena, as file 0,
read-write, and maps every page from it. A MAP of file 0 encodes as MAP did in
version 9. An [isolated arena](#the-isolated-arena) sends the region's private
file as file 0 and its tenant's shared file as file 1 when the session attaches. It
sends a fork point's file as file 2 or up in the middle of a session, just
before the first MAP that names it, and DROP_FILE when the point's seal ends.
Numbers are the session's own: another session may name the same fork file by
another number.

A session attaches with HELLO, MEMORY_REGION, ATTACH, FILE 0, any other files,
the populate's MAP_BATCH frames and READY. The client refuses READY before it
has file 0. The pager sends FILE and DROP_FILE under the lock it sends commands
with, so they reach the client in order with the MAPs that use them.

The client keeps a table of its files. For each FILE it checks:

- that the frame's arena kind is the attachment's, and its length is whole
  pages
- that file 0 is stated writable and every other file read-only
- that the descriptor is that kind of memory, as it checks the page with
  `fstatfs`
- that the file is at least the length stated, by `fstat`. Only the stated
  length bounds a MAP, so a file may grow before it is stated again
- that the descriptor is open read-write for file 0 and read-only for every
  other, by `fcntl(F_GETFL)`
- that a repeated number names the same file, by device and inode, at a
  larger length

A FILE that fails any of these ends the session. So does a DROP_FILE of file 0
or of a file the client does not hold. The client refuses a MAP that names a
file it does not hold, reaches past that file's stated length, or is writable
and names any file but file 0. It answers such a MAP with `EINVAL` and changes
nothing, as it does any other invalid command.

None of these checks keeps a VMM out. The pager's descriptors do that. The
checks keep a well-behaved client safe from a pager bug.

The attach READY handshake and batched mappings are mandatory. All batch ranges
are validated before any change is made, and they must be ordered and disjoint.
The Rust client populates sparse zero page tables before UFFD registration. The
pager installs arena page tables with ranged `UFFDIO_CONTINUE`. It retries the
transient `EAGAIN` that a concurrent mapping change causes.

Client request IDs are separate from mapping command IDs. They increase within
each session, so requests from independent memory regions can arrive out of global
order. The control reader dispatches requests without waiting for their work to
finish, because a seal can itself need revoke acknowledgements. Writes send
complete commands one at a time, including every frame of a batch.

A FLUSH is a request like a SEAL. Its ID comes from the same sequence. The
client takes the ID under the lock it writes with, so IDs reach the pager in
order. RESULT answers each FLUSH once. The guest waits for that answer, but the
VMM does not. The flow is:

1. The device takes a queue drain's flushes off the ring and holds them as one
   request.
2. `Control::start_flush` writes the request and returns.
3. The answer comes back to the VMM thread through an event.
4. The VMM thread completes the held flushes (status, used ring, interrupt) with
   the host's answer.

When a session ends, it answers every waiting flush with EPIPE. The guest reads
that as a failed flush and does not wait forever.

The pager's control reader counts each FLUSH in `Stats.Flushes`. It passes the
FLUSH and its memory region to the callback that `Host.SetFlushed` installed, together
with the `done` function that sends its RESULT. The callback runs on a goroutine
of the session, never on the reader. A checkpoint that the host takes for the
flush needs the reader for its seal. The callback returns at once. The host
calls `done` when the flush is durable, from any goroutine, at most once. With no
callback installed, every flush is answered at once. A host is not required to
call `done`. It drops the flushes of a VM that leaves it, because an answer
would write into guest memory that the destination now owns. Nothing in the
pager waits for these flushes when the session closes. The session ends on a
FLUSH on a RAM session, a FLUSH without a fresh ID, or a FLUSH with another field
set.

A flush the host never answered is not lost with the host. The device records
the flushes it holds in its snapshot (snapshot format version 14). A device
restored from that snapshot sends them to its own host when the guest resumes. A
handoff takes the flushes back from the host the guest is leaving. So when that
host's session ends, it completes none of them. If the handoff is abandoned and
the guest resumes where it was, the guest sends them again.

All ranges and backing offsets must be aligned to the page size the attachment
stated, and must be in bounds. Every command covers whole pages. Generations
start at zero and advance by one for every affected page. A command that spans
several pages requires all of them to be at the previous generation. Command IDs
increase across accepted commands. An identical retry of the immediately
preceding successful single-range command only repeats the acknowledgement.
Batches are never retried after an uncertain acknowledgement. Instead, the
connection becomes terminal. Stale generations, invalid ranges, overflow and
unknown operations are rejected before any change is made.

A syscall failure after a change has begun is terminal. It is not treated as a
rejected command, because a failed `mremap` can leave its destination unmapped.
A pager that does not receive the matching successful acknowledgement must not
assume that the old backing can be freed. Guests cannot access the protocol.

The pager does not trust the VMM. A VMM runs untrusted guest code, and an
embedder jails it because it may be compromised. So everything a VMM sends is
checked, and anything outside the protocol ends that session and no other:

- A HELLO carries exactly one descriptor, and every field but its version is
  zero. The descriptor may be anything. A read of it that is not a whole
  fault or remap event ends the session, and so does an ioctl it refuses.
- MEMORY_REGION must name the kind and the size the pager was given for the
  session, at an address aligned to its page.
- A fault must be inside the memory region and carry only the flags the kernel
  sets. The pending faults are bounded by `ConnectionConfig.QueuePages`.
- An ACK answers the one command awaiting it, once, with every field but its
  identifier and generation zero. An ACK that no command awaits ends the
  session. So does a second ACK of one command.
- SEAL and FLUSH take increasing request identifiers and no other field. At
  most one SEAL and 1,024 FLUSHes wait at a time.
- Descriptors passed with any frame after HELLO are dropped by the kernel,
  because the pager reads those frames without ancillary data.

A session that ends this way is closed like any other. Its pages go back to the
pager, and the host process keeps no descriptor of it.
`vmmemory/hostile_linux_test.go` plays each of these against a real pager,
beside a well-behaved process on the same pager, and `FuzzHostileSession` plays
arbitrary sequences of them. A VMM that faults its own memory over and over is
paced: see [repeated faults](#repeated-faults).

Those sessions send a made-up descriptor, so the pager cannot resolve a fault
and the session ends at the first one: seal, retire and settle never run under
them. `vmmemory/hostile_client_linux_test.go` closes that gap. A proxy sits on a
real client's control socket. It forwards the whole session, so the client's
guest faults its memory in on a real registered userfaultfd, and the test seals,
settles and retires that memory region as a capture does. Then the proxy sends
the pager one frame the honest client never would. The pager ends that session
and leaves its neighbour whole, as it does one that never resolved a fault.
`TestAHostileClientWithARealUFFDIsEndedThroughACapture` plays a frame each, and
`FuzzHostileClient` varies the number of captures and the frame.

In a shared arena the arena's descriptor gives a VMM more than the protocol
does. `vmmemory/reach_linux_test.go` plays a VMM that uses every descriptor it
is given. In a shared arena it reads another VM's dirty and published pages
through the one file it holds. The [isolated arena](#the-isolated-arena)
closes that. There the VMM finds none of another VM's private pages, before or
after that VM stores and publishes more. Of a VM of another tenant it finds
nothing at all: not the pages it loaded by identity, and not the page another
region of that tenant inherits. It does find the page its own tenant published
and another region of the tenant inherits. That is the one thing the design
concedes, and the test asserts it. Every way to write its read-only files
fails. A read-only descriptor refuses a write, but a file's mode is checked
again when it is opened, so the test also runs a helper as another user, as the
embedder's jailer runs a VMM. Holding the same files, the helper cannot reopen a
read-only one for writing through `/proc/self/fd`, and cannot fchmod any of
them: the pager makes every file with mode 0600. When the VMM allocates memory
in its own private file, verification ends its session with `ErrUncounted`.

So a rejected command is the only failure known to have changed nothing. The
pager treats it as a failed operation, not a failed session. In practice, the
refusal a guest meets is the mapping budget described above. The client checks
a command against the budget before it changes anything, and answers `ENOSPC`
as an ordinary acknowledgement. The pages that command would have mapped are not
recorded as mapped. Ambiguous failures require the opposite. A command whose
acknowledgement never arrives may have been applied, so its pages stay recorded
as mapped. Otherwise a revocation would skip the unmapped binding and release a
page that the guest still reads through. After a refusal, the fault fails and
the memory region keeps serving. Revocation frees the budget, and revocation is
other work of this pager. So the worker queues that fault again once the pager
has revoked a mapping, and not after any other change. The refused fault took
and gave back pages itself. Two refused faults that each waited for any change
would wake each other, and a client that refuses every command would keep the
pager serving it for as long as it lived. The guest waits there as it waits for
the dirty budget. The VM stays alive so that it can be checkpointed or migrated
off the host.

This is recoverable because of what the budget admits. Only replacements that
install a mapping are charged against it. A revocation installs none. The range
it replaces becomes the trap mapping that the memory region was attached as, and that
merges with the traps around it. So a revocation can only lower the count. It is
admitted regardless of the budget, and so is a batch of revocations of any size.
If a revocation were charged like a mapping, it would be refused when it
is needed most. After a refusal, less of the limit remains than one command reserves.
So the pager could not revoke anything, and the deferred fault would wait for a
command that could never be admitted. The kernel headroom that the limit leaves
covers the transient cost of a revocation. For this reason a limit above half of
`/proc/sys/vm/max_map_count` is refused at attachment.

A batch split across several commands is the exception. Once one of its commands
has landed, the pager knows the runs it sent but not the frames they became. So
a refusal after that is as ambiguous as a lost acknowledgement, and terminal in
the same way. `Stats.RefusedMappings` counts the deferred faults. A host that
refuses has given its client a budget too small for the number of mappings its
guest's access pattern creates.

### Repeated faults

A VMM can drop the page tables of its own memory with `MADV_DONTNEED` and read
the memory back. Each read traps, and the pager serves it again. That fault
changes nothing: the pager finds the page mapped and installs the same page
tables again. This is a repeated fault. A fault is repeated when its memory
region already maps the page for the access: mapped or zero-mapped for a read,
and mapped writable for a store.

Every other fault loads a page, maps it or copies it, and the resident, dirty
and mapping budgets bound those. A guest under memory pressure faults again
through loads after evictions, so its faults are not repeated. A guest meets a
repeated fault only when something outside the pager took its page tables
away, such as the kernel moving a page. A VMM can make repeated faults as fast
as it can drop page tables.

So each session's repeated faults are paced. A session may take 1,024 of them
at once, and 1,024 a second after that. A repeated fault past that budget
waits in its session's fault worker before it is served. The wait costs the
pager no work and holds up no other session. The page stays in flight while
the fault waits, so the VMM's threads that trap on it wait too. Pacing only
slows a session and never ends one, because a well-behaved guest can meet
repeated faults too.

Two vCPUs that fault one page at once raise two faults. The second is served
after the first and finds the page mapped. It is the first fault's twin, and
the session is not charged for it. Only a fault that changes something has a
free twin. The twin of a twin or of a repeated fault is charged, so twins
cannot follow each other for free.

`vmmemory/repeats.go` holds the budget and `vmmemory/faultqueue.go` the twins.
`Stats.RepeatedFaults` counts the repeated faults, and `Stats.PacedFaults`
those that waited for the budget.

Measured on the aarch64 Lima instance on 2026-09-26, one run each. A VMM with
64 fault workers and 128 threads dropped and read back 256 pages of 4 KiB over
and over, beside the hostile tests' well-behaved process, which stored into a
page and checkpointed it 400 times:

| pacing | the VMM's repeated faults | neighbour's median store, alone and beside | 90th percentile, alone and beside |
| --- | --- | --- | --- |
| off | 76,000 a second | 0.33 ms, 0.76 ms | 0.47 ms, 1.78 ms |
| on | 1,427 in 0.4 s | 0.28 ms, 0.28 ms | 0.37 ms, 0.41 ms |

Unpaced, such a VMM took three and a half of the instance's eight processors,
and in one run the neighbour's slowest store took 289 ms.
`TestAVMMRepeatingItsFaultsIsPacedAndItsNeighbourKeepsItsLatency` holds the
VMM to its budget and the neighbour's median store to half as long again as
it takes alone. `TestThreadsMeetingOnAPageRepeatNoFaultFree` runs threads that
race for the same pages and holds the pager to the budget and one free twin per
page brought in. The fuzzing cannot play this: its descriptors are not real
userfaultfds, so its session ends at the first page it resolves.

## Idle pages

When the last memory region that maps a published page goes away, the page still holds
the bytes its identity names. So it stays in the arena, idle, instead of being
released. The next memory region that inherits the identity maps it without reading the
volume. So a seeded guest that is checkpointed and stopped leaves its memory for
the forks of that checkpoint. A fork of a stopped template needs this. Plain
Firecracker gets the same effect from the host's page cache. A private page is
one memory region's state and is released with that memory region.

An idle page is memory that nothing uses. So it is the first memory given up,
and it is never a reason to wait. An allocation that needs a slot takes the
oldest idle page before it evicts anything a memory region maps. A store's write-ahead
run and a load's read-ahead take only free slots, and they give up idle pages to
free slots. Idle pages also act as the cache of the host budget, so the other
pager and the checkpoint cache take them before they wait. A page placed at its
own offset keeps that offset's extent while it is idle. So a memory region that finds
no free extent gives up the idle pages of an extent whose memory region has gone.
`DropIdle` gives up all idle pages at once. `Stats.IdlePages` is the number of
idle pages, and `Stats.IdleDrops` is the number given up.

## Eviction ordering

1. Hold the page transition so no new alias can be installed.
2. Revoke every mapped alias and wait for each matching acknowledgement.
3. Read the now-stable contents of private backing and write the spill file.
4. Punch the arena range and make its slot reusable.
5. Let queued faults reload and remap the current page identity.

A page whose backing can be reconstructed writes no spill data. If a spill fails,
the resident slot is kept even though its aliases were revoked, so a later fault
can map it again. Private pages are conservatively spilled again after another
writable residency period. No slot is reused only because a revoke was sent.

Step 2 can fail because another machine has died. A memory region whose memory session
has stopped answering cannot revoke a mapping. So this host can never reuse a
page that such a memory region can reach. The revocation that discovers this makes that
memory region terminal, and the page stays mapped there until the memory region is closed. But
the eviction that discovered it belongs to whichever machine needed a page. The
children of one fork share every page they inherited. If the eviction failed
that machine with the dead machine's error, it would end that machine too, and
then the next machine sharing a page with it. So the eviction takes another
victim instead. The terminal memory region excludes its pages from every later pass. If
the arena consists only of such pages, it reports capacity exhaustion. One
guest's death does not spread across the host.

### Choosing the victim

The victim is the least recently faulted page that leaves every protected
memory region its pages. The pager sees a guest's faults and none of its other
accesses, so fault order is the only recency it has. Alone, that order lets one
guest take the arena from all the others. A guest that cycles through more
memory than the arena holds faults on almost every store. Its pages are always
the newest, so its neighbours' working sets are always the oldest, and each
fault of the hog evicted one of them. A neighbour then held the arena only in
proportion to how often it faulted, which is to say only by thrashing.

So each attached memory region is owed a share: the arena's pages divided by
the memory regions attached. A memory region is protected while it holds no
more than its share and has asked for a page within the last turnover. A
turnover is as many evictions of mapped pages as the arena has pages. An
eviction for one memory region takes no page another protected memory region
maps. Where every candidate is protected, it takes the least recently faulted
page of all, so an allocation never waits on the rule.

A guest whose working set is resident asks for nothing, so it loses its
protection after a turnover. It then gives up its least recently faulted page,
and is protected again at its next fault. So a hog evicts its own pages, and
costs a neighbour within its share about one refault a turnover. An idle
guest's pages are anyone's to take, so a guest that is booting or growing can
still use memory its neighbours are not using. The share is a floor for a guest
that is using its memory, not a ceiling for one that is.

Each memory region counts the resident pages it maps. A page it reaches twice,
from its binding and from a checkpoint's copy, counts once. The alias changes
that bind, unbind and evict a page keep the count under the host lock, so the
rule reads it without walking anything.

The rule helps a neighbour only as far as its share holds its working set. On
the aarch64 Lima instance, two 128 MiB guests at 2 MiB pages, one storing into
80 MiB over and over:

| RAM arena | share | neighbour, rule off | neighbour, rule on |
| --- | --- | --- | --- |
| 64 MiB | 16 pages | `echo` through the agent past 30 s | `echo` through the agent past 30 s |
| 96 MiB | 24 pages | slowest `echo` 96 ms, 1,016 evictions | slowest `echo` 32 ms, 590 evictions |

A booted guest of this image holds 21 dirty 2 MiB pages, so a share of 16 is
below its working set whatever the rule does. Each figure is one run.

## Capture, fork and restore

A capture is the checkpoint's pause. The prepare step pauses the vCPUs, drains
device completions, saves device and register state and seals every memory
memory region. It returns each memory region's sealed set under the name of the volume the
memory region maps. The resume step restarts the vCPUs while the memory regions stay sealed.
The publication then writes those sealed pages with the captured VMM state. It
retires the seals when it lands. The guest's pause consists only of the state
capture and the seal. No data crosses the network during the pause.

The pause happens under the VM's publication lock. So an explicit capture and
the host's interval checkpoint are serialized there and do not race to seal the
same memory regions. If a phase fails before the publication starts, the VM is
released: every memory region is unsealed and the guest resumes.

The caller cannot cancel any of these operations. Pause, snapshot create, resume
and release run on a context owned by `vmmachine`, bounded by its own timeout.
The caller's context bounds only the wait for the process lock. A control
request abandoned halfway leaves the VMM's state unknown. The only response to
an unknown state is to kill the process. That loses every guest write since the
last checkpoint that landed. So the following must not reach the VMM as a
cancelled request:

- an HTTP client that disconnects
- a drain whose deadline passed
- a migration that gave up

The release that recovers from a failed capture especially must run on a live
context. A request that the VMM answers with a refusal is a different case. The
VMM is running and its vCPUs have not moved. So a refused pause or a refused
seal fails the checkpoint and leaves the guest unchanged.

The state file that the capture stages is never made durable:

- It is read back and removed before the guest resumes.
- A host that restarts wipes the directory that holds it.
- Its bytes become authoritative only once the checkpoint that carries them is
  published.

The VMM is also told not to sync the file. So nothing in the pause waits on a
disk for bytes that nothing will read. Once the publication owns the sealed
sets, no other step unseals them. The publication retires each set when it lands,
and hands their pages back to the guest when it does not land.

A fork publishes no checkpoint of the parent. `host.Seal` takes the same pause
(stop, save state, seal, resume) and returns a `volume.ForkPoint`. The fork
point contains:

- the checkpoint that the parent's control record already selects, pinned there
- the pages that no checkpoint holds, which are the pages the seal froze

Any number of children can start from one fork point, and each child holds it
once. So a fan-out costs the parent one pause. The parent stays sealed, and is
not checkpointed, until the last hold retires and hands every page back to its
guest. On the parent's host, a child reads the sealed pages through the point
and shares them in the same pager. On another host, the child's pager pulls
those pages from the parent's page server, as a migration destination does. The
child's first checkpoint publishes them as the child's own. Until that
checkpoint lands, opening the child on any host reports `volume.ErrForkPending`.

A host that never held the parent rebuilds the point from the pinned checkpoint
alone. A fork of a template works this way. A host restores a VM it never ran by
reading the VMM state of the published checkpoint. Restore replays those state
bytes with exact managed PMEM overrides. It never loads a full RAM image file.

Every VM a host runs is checkpointed on `host.Config.CheckpointInterval`. The
default is sixty seconds. Each wait is jittered by up to an eighth in either
direction, so that VMs do not checkpoint in lockstep. The wait is measured from
the completion of the previous publication. This interval is the only thing that
makes a running guest durable. Guest PMEM stores, guest RAM stores, live
registers and local scratch spill are all volatile until a checkpoint includes
them. Losing the host rewinds the VM to its last checkpoint.

## Live migration

The pager has two jobs in [live migration](migration.md): serving pages to the
destination, and giving up the volume. Migration is post-copy only. Nothing is
uploaded during the pause.

`MemoryRegion.ReadResident` copies one page for a peer. If this host does not hold the
page, it says so, and the destination reads from the volume instead. It also
reports whether the served page is this memory region's own state and not the volume's.
It never loads. A page server that loaded would turn a destination's fault into
a volume read on the source. "Held" means the page is in host memory or is
private state of this host. "Unpublished" means the page is dirty here, so no
checkpoint has it. `Resident` lists the pages this host holds.
`MemoryRegion.Unpublished` lists the subset that no checkpoint has. Both are
snapshots. A page reclaimed before a stream asks for it is reported absent.

On the destination's side of the same protocol, a load is not an install. A
backing that reports pages as unpublished (`vmmemory.UnpublishedLoader`) fills a
buffer, but the pager may still not keep the page. On a full dirty budget, a
read-ahead page is dropped so that the fault that carried it does not fail. The
bytes are then lost, because the volume's bytes for that page are older than the
guest's write. A backing that also implements `UnpublishedInstaller` is told,
after the load's pages have been bound, which of them this memory region now holds as
its own dirty state. Only those pages may be removed from the set that the
destination still needs from the source. A page the pager dropped is requested
again. A read that cannot get it fails. It does not fall back to the volume.

`MemoryRegion.Handoff` gives up the volume but keeps the pages. It must run after the
guest is stopped. The control record is about to be released so that another
host can take it. So verification stops checking authority that this host no
longer has. A seal, a population or any fault then reports `ErrHandedOff`,
because the guest is stopped and a fault would mean it is running. Serving
continues until `Detach` releases the pages. A sealed memory region cannot be handed
off. A publication is reading its pages under the handle that the handoff would
give away. So the handoff reports `ErrSealed` and the memory region keeps its volume.

The supervisor exposes this per VM. `Process.MemoryRegions` names every memory region by the
volume it maps: `ram0`, and one per PMEM device id. The destination opens the
same volumes under these names. `Process.Stop` is the stop phase. It pauses the
vCPUs, drains device completions and returns the VMM state, and it leaves the VM
paused. It seals nothing and waits for nothing, because the destination fetches
the pages it leaves behind. It differs from `Prepare`, which seals for a capture
that the same VM resumes from. A migration abandoned after `Stop` can still be
released, which resumes the guest.

## Firecracker build and process lifecycle

The integration is a commit on the `sproutfs` branch of the
[fork](https://github.com/semistrict/firecracker.git), and the gitlink pins it:

```sh
git submodule update --init third_party/firecracker
```

Edit source inside the submodule and commit it on the fork's `sproutfs` branch.
The integration covers the feature, API schema, mapping owners, PMEM worker,
snapshot and restore paths, and the seccomp policy source. Since version 13, the
snapshot format records managed backing. So an ordinary memory-file snapshot
cannot silently capture a managed VM, and a build without the feature rejects
managed configuration. Version 14 records a managed PMEM device's waiting
flushes. A managed capture is always a full snapshot with no memory file. A
managed restore requires fixed RAM with no huge-page setting, in this
architecture's own layout.

The Firecracker fork has to be rebuilt for mapping protocol version 10. Its
seccomp policy lets the memory thread read frames with `recvmsg` and check a
file it is handed mid-session with `fstat`, `fstatfs` and `fcntl(F_GETFL)`. It
lets the VMM thread check a file's access mode and map a read-only file's runs
`MAP_PRIVATE | MAP_NORESERVE`. The crate is vendored into the VMM by path. So a cached qualification build
keeps speaking an older version, and every session it opens fails with
`invalid managed-memory hello` before a guest starts. That failure is the
version check working as intended. It is also the first thing to check when a
Lima or GCE run that used to pass stops attaching: rebuild the VMM, and do not
reuse `~/.cache/sproutfs-fanout`. The VMM's snapshot format is also at
version 14. So VMM state that an older build captured is refused on restore,
instead of being read without its PMEM devices' waiting flushes.

Starting a machine requires:

- the shared pager
- the VM whose volumes it maps
- a feature-enabled binary
- an explicit compiled seccomp policy, including the VMM's memory-worker filter

RAM binds to the single volume named `ram0`. Its size must be a multiple of the RAM
pager's page size. Each PMEM device binds to the volume named by its device ID.
Its size must be a multiple of 2 MiB, which is Firecracker's alignment and the
PMEM pager's page size. Cold boot also supplies a kernel, an optional initrd and
boot arguments. A root PMEM device boots directly with a suitable Linux kernel.
The qualification guest uses ext4 with `dax=always`.

RAM is one volume and one session, but the guest's physical address space is
not contiguous. Each architecture reserves holes in it for MMIO. On x86_64, the
hole from 3 GiB to 4 GiB splits the RAM of any VM with more than 3 GiB into two
memory regions. A second hole at 256 GiB splits a larger VM's RAM again. The volume
does not contain the holes. The VMM maps the volume's single byte range onto the
guest's memory regions in ascending guest order. So on x86_64, volume bytes
`[0, 3 GiB)` are guest addresses `[0, 3 GiB)`, and the rest begins at guest
address 4 GiB. Volume offset 3 GiB + x is guest address 4 GiB + x on every path
the volume takes: fault, seal, checkpoint, restore, fork and migration. On
aarch64, RAM below 256 GiB is one memory region, and the volume is that memory region. The
split follows Firecracker's architectural layout for the memory size. A restore
whose snapshot records any other layout is refused. It is not read at shifted
offsets. Only the VMM knows about the holes. The volume, the index, the pager and
the control protocol see one contiguous byte range. An x86_64 guest has no 3 GiB
limit.

The supervisor creates private Unix sockets and checks the peer credentials
against its child process. Attachments initialize concurrently. A managed PMEM
device holds a guest flush and asks the host over that disk's memory session. It
completes the flush on the VMM thread when the answer arrives. The answer may
come after a disk checkpoint taken under a vCPU pause. Meanwhile, the VMM thread
keeps serving every other queue and device. A timeout, pager loss or authority
failure terminates the VMM, because the VMM cannot keep running against
anonymous replacement pages. Failed starts and shutdown remove private sockets
and state files, detach logical pages and release arena allocation. Console
output is held in memory and is lost with the process. Close the process before
closing the volumes, the spill file or the arena.

## Qualification

The [2 MiB HugeTLB qualification](measurements/hugetlb-2026-09-11.md) records
the current x86_64 execution, performance comparison and pool cleanup.

```sh
SPROUTFS_GCE_PROJECT=your-project bash scripts/bench-memory-gce.sh all
```

The Linux suites exercise real UFFD and KVM under strict seccomp. The GCP
qualification script provisions a disposable x86_64 host with a HugeTLB pool, a
renewable 24-hour lease and a hard 24-hour deletion deadline. Its `all` command
deletes the VM and boot disk on exit and verifies that they are gone. Lima
qualification requires a separately provisioned 2 MiB HugeTLB pool.

```sh
scripts/test-vm-memory-lima.sh
SPROUTFS_VM_MEMORY_REPEAT=20 scripts/test-vm-memory-lima.sh
scripts/test-firecracker-lima.sh
```

Every suite builds its pagers in the arena mode `SPROUTFS_ARENA` names, `shared`
when it is unset, and is run in both: the ordinary Go suites
(`SPROUTFS_ARENA=isolated go test ./...`) and both Lima scripts, which pass it
through. A test of what one mode does pins that mode. In the isolated mode the
simulated arena (`internal/testpager`) holds the pager to who may read each
file: a file given writable is given to one memory region only, as its file 0,
and to nobody read-only, and every map names a file its session holds, writable
only for file 0. So every campaign checks the split under forks, migrations,
eviction and spill.

Use `SPROUTFS_LIMA_INSTANCE` to select an existing instance. The host needs Go,
Cargo and `limactl`, plus Python 3 for the full-guest suite. The guest needs
Cargo, Clippy, a source mount, KVM and kernel-fault UFFD support. For the
full-guest suite it also needs a C compiler with static libc, libseccomp, curl
and e2fsprogs. The dedicated test process runs through `sudo -n` for UFFD, KVM
and physical-page inspection. Neither script changes device permissions, sysctls
or the VM configuration. Both remove their temporary artifacts on exit. The
ordinary Go suite skips these tests unless `SPROUTFS_VM_MEMORY_CLIENT` names the
built Rust adapter. When it is set, a missing capability or inaccessible
physical-page information is an error and not a silent pass.
`SPROUTFS_PAGER_MEASURE` and `SPROUTFS_FRAGMENT_MIB` enable the opt-in
measurement runs.

`SPROUTFS_FIRECRACKER_RESIDENT_PAGES` sets the full-guest resident budget in
pages of the larger page size, 2 MiB. It defaults to 48 pages (96 MiB), and each
pager's arena is that many bytes. At that budget, the source and the fork each
dirty 48 MiB of guest RAM. After both allocations, they verify markers in every
4 KiB subpage, which forces eviction, spill and refault. The earlier 32 MiB
budget is below this fixture's working set with 2 MiB pages. Even 64 MiB
thrashes when both forks run. The 96 MiB qualification requires that eviction,
spill and refault are observed.

The memory suite uses a PMEM pager's 2 MiB page throughout. It also runs one
4 KiB RAM pager in `small_page_linux_test.go`. The full-guest suites run the
production pair: a 2 MiB RAM pager and a 2 MiB PMEM pager over the pool. Where
`SPROUTFS_RAM_PAGE_BYTES=4096` is set, they run a 4 KiB RAM pager over an
ordinary memfd instead. One fault installs a whole pager page. A store copies
the whole page, a seal write-protects it, and a checkpoint publishes it as one
part member. A spilled page comes back whole. Migration requests default to one
2 MiB page, with an 8 MiB per-peer in-flight byte budget. A reply is counted in
the page size of the volume it answers for.

`small_page_linux_test.go` tests the 4 KiB contract against the real kernel:

1. A parent reads one byte of a 512-page run.
2. Read-ahead loads the whole 2 MiB into consecutive arena slots.
3. A child of the same fork point maps that run with one mapping command, not
   512, and reads nothing.
4. A one-byte store into one of the child's pages makes one page private. That
   costs 4 KiB of arena, 511 pages of the run stay shared, and the parent's
   memory is untouched.

The pager suite covers:

- two processes physically sharing pages, verified through pagemap
- private writes, including a first access that is a write
- shared eviction that revokes every alias and releases arena allocation
- dirty spill, refault and backing-slot reuse
- a failed spill that keeps the only current copy
- concurrent loads racing eviction
- kernel access as the first accessor, without synthetic prefaulting
- tiny KVM guests using both memory region kinds across eviction and refault
- stale, overflowing and out-of-range commands, identical retries, control loss
  and teardown
- fragmented mappings with reported mapping counts
- eager sparse zeros, first-store copy-on-write and mixed zero-and-data
  read-ahead
- two restores of an unpublished point plus a nested fork after private writes.
  Every resident page must map during connect, with no load and no fault on the
  first KVM reads.
- a seal of a run of pages whose slots descend. It must be one range
  write-protect covering several of the client's mappings. The guest must keep
  reading those pages without a fault, and the next store must still trap and
  copy.
- what the isolated arena needs of read-only files, in
  `readonly_linux_test.go`. The test plays both the pager and the VMM, without
  the Rust client. See step 1 of
  [the plan](../plans/isolated-arena-2026-09-25.md#steps).
- the same through the Rust client, in `files_linux_test.go`. The fixture puts
  the pages it shares in a second file that the client receives read-only, and
  the sharing, eviction, spill and KVM cases run over both layouts. A page of
  the read-only file is the pager's own page in the client, its private mapping
  reserves no HugeTLB pool page, and a store from a thread or from KVM traps to
  the pager and leaves the file unchanged. The client refuses a writable MAP of
  the file, a MAP of a file it was never given and a MAP past the file, and a
  DROP_FILE closes its descriptor.

The simulated pager tests require the following of an isolated arena, in
`vmmemory/isolation_test.go` and `vmmemory/tenant_test.go`:

- A page two regions of a tenant inherit is in the tenant's shared file, which
  each maps as file 1. A store copies it into the storing region's file 0, at
  its own offset.
- A region of another tenant reads the same bytes from a shared file of its
  own, which the first tenant's regions are never given.
- A published page moves into its tenant's shared file when another region
  inherits it.
  The inheritor reads nothing from its volume, the owner's mapping of its
  private page is revoked, and the private slot goes back.
- A published page whose VMM changed it through its private file ends that
  session with `ErrTampered`. The inheritor reads the page from its volume.
- A fork point's page is copied into the point's file once, and two children
  map that copy as file 2. Ending the seal revokes their mappings, drops the
  file from both and gives it back.
- A detached region's private file lasts as long as its idle pages, and so does
  a tenant's shared file once the tenant's last region has detached.
- A fault on a page of another tenant fails with `ErrOtherTenant`.
- Verification ends a region whose private file holds a page the pager never
  put there, and punches such a page out of the tenant's shared file.

The simulated pager tests also require the following of seals:

- A seal returns before anything reads the sealed set.
- A store into a sealed page runs at once and leaves the published bytes
  unchanged.
- The sealed set survives reclaim and refault of its pages.
- A sealed page refaulted from spill shares its memory again. So retirement
  retires it, and does not leave behind a private page that no reservation
  covers.
- Sealed pages hold the dirty budget, so an over-budget store waits for the
  publication instead of failing.
- An abandoned seal keeps every page for the next seal.
- Retiring a page never leaves its private page reachable from a binding that
  owns neither a reservation nor a seal. This test runs against a concurrent
  eviction.

The tests also cover a store into a page that an abandoned seal handed back, a
seal cancelled partway, and an abandon racing another volume's faults. The
concurrent cases run under the race detector.

They require the following of the settle:

- A memory region that shares pages with a sibling takes one page writable and stores
  nothing. The memory region publishes no page. The page ends up shared again, with its
  private bytes zero, its reservation returned and its mapping of the copy gone.
  A read of it takes one fault onto the sibling's page.
- A page the guest really stored into is published as before.
- A store that lands between the seal and the settle leaves the guest its own
  copy for the next checkpoint, and this checkpoint publishes nothing.
- These copies are all published unchanged: a copy whose origin was evicted, one
  made from zeros, one made from the checkpoint's own held copy, one made from
  the name a fork point lent a page, and one made from a page only another host
  holds.
- A memory region whose only private pages were unchanged lets a store that waits on
  the loss window proceed.
- One worker and sixteen workers leave the same set.

For migration, the simulated tests require that a seal issues one range
protection per run and no mapping command, and keeps every page where it was.
They require the following of the page server:

- It returns a held page's current bytes: the sealed copy for a page still
  sharing one, and the guest's own copy for a page stored into since the seal.
- It reports which served pages no checkpoint has.
- It reports a never-touched page, and a page reclaimed since it was listed, as
  absent, without reading the volume.

A destination's pager must hold every peer-served unpublished page as private
dirty state and publish it in its next checkpoint. At a full dirty budget it
must wait for a checkpoint to free space, and not fail the post-copy read.
Finally, the tests run a memory region against a volume on which every call fails, as
a handed-off host's volume does. Verification, listing and serving must all
continue. A seal or a fault must report `ErrHandedOff`. Detaching must release
every resident page, reservation and logical page the memory region owns.

The full-guest suite builds the feature-enabled VMM and the restrictive aarch64
seccomp policy. It downloads a pinned official Firecracker CI kernel with a
recorded checksum. It creates an ext4 PMEM root containing a static guest
workload, and boots without an initrd or a memory file. It uses real KVM and
real TCP over loopback. Object storage is simulated. The guest verifies actual
DAX with `statx`, performs mapped stores, `msync` and `fsync`, and reports its
extent, so the host can check acknowledged bytes before the capture. The test
then does the following:

1. It captures known disk and RAM values and changes the source.
2. It restores a fork at the original values.
3. It verifies isolated fork writes and shared physical pages.
4. It replaces the fork's PMEM writer and requires the stale VMM to exit.
5. It migrates the source away. It names both memory regions by their volumes and stops
   the VM. It requires that the stop seals nothing and that the stopped source
   still serves the pages it lists.

A failed start and the final shutdown check private-file removal, zero logical,
dirty and resident pages, and zero memfd blocks.

The suite also runs the fan-out. Neither the simulated campaigns nor the other
suites test this shape. It uses one fork point and two children. Both children
are received onto a second pager and page server one after the other, in the
same way as the orchestrator does it. Each child's root index is published and
its hold on the parent is released before the next child starts. Both guests
then read every page of their memory and their whole root volume at the same
time, while both are checkpointed on an interval.

The page server runs at a deployment's budgets. One peer is one destination
host, and both children are on that host, so their memory regions share every budget
counted per peer. The destination's RAM arena is a quarter of the memory the two
children map. So every one of those reads involves eviction, spill and refault.
Its PMEM arena holds both roots, because also putting pressure on the disk would
measure the disk instead. Both children must answer. A child whose read never
returns cannot be told apart from a dead guest. At 4 KiB this phase takes
minutes, not seconds. Read-ahead takes only free arena slots, so under a
quarter-sized arena every page of a scan takes its own fault. The phase's time
bound is sized for this.

The suite also runs hostile neighbours, in `neighbours_linux_test.go`. Each
test runs two guests on one real `host.Host` over one pair of pagers, so the
host's loop, its loss window and its answers to pressure and to flushes are the
deployment's. One guest runs a load from the guest init's `hog` command in a
child, and the other goes on working: a RAM store, a DAX store it syncs, both
read back, and an exec through its agent, each within 30 seconds. Every test
then checks that no pager's peak dirty or resident pages passed its budget or
its arena. The loads are:

- `hog ram`, a guest storing into all of its RAM over and over, on a RAM arena
  three eighths of what the two guests map. The neighbour's working set must
  fault back in whole, its agent must answer, and nothing is stopped.
- `hog disk` and then `hog ram`, with the interval an hour away. The disk hog
  runs past the PMEM dirty budget and must be checkpointed out of turn. The RAM
  hog runs past the RAM dirty budget, which nothing relieves. It must be
  stopped, with the logged reason, and the neighbour must not.
- A small `hog disk` past a two-second loss window, which only checkpoints out
  of turn relieve. Its stores used to hold a vCPU that the relieving
  checkpoint's pause then waited on; see the loss window in
  [the bounded host pager](#bounded-host-pager).
- `hog sync`, an fsync loop. It may cost at most one checkpoint of the storming
  guest per flush bound beside the interval's own, and the neighbour's flushes
  must complete.
- `hog vsock`, a guest connecting to the host over and over where nothing
  listens. The host's exec must still reach that guest's agent.

Both suites report residency, sharing, load, mapping and fault counters. These
are observations of small workloads, not throughput or latency targets. Recorded
on 2026-09-10 on the Lima aarch64 instance:

- A second machine restored from a point its sibling had already loaded, with
  two memory regions of 512 pages, mapped all 1,024 pages with 2 commands. It loaded
  nothing and took no fault.
- The full-guest fork of a machine with 128 MiB RAM and 64 MiB PMEM mapped
  47,600 pages with 4 commands before resume. It loaded 15 pages and took 3
  faults.

The same run's capture of 8,495 dirty pages paused the guest for 144 ms. Pausing
the vCPUs, saving the VMM state and sealing took 142 ms, and resuming took 2 ms.
Moving the sealed pages to storage took a further 373 ms while the guest was
running. Selecting the checkpoint then took 0.4 ms. The seal's share of the
pause scales with the number of runs in the dirty set. The 373 ms scales with
bytes and object-store latency. Separating the two is the purpose of the design.
Against a real object store and a larger guest, the ratio is much larger than
this local simulation shows.

Migrating the same source away took a stop pause of 7 ms. That is only the state
capture, because the stop seals nothing and uploads nothing. The stopped source
then listed 7,599 resident RAM pages for its destination to stream.

A migration's pause pays the seal cost for its dirty set. So seal cost is
measured separately with `SPROUTFS_PAGER_MEASURE=1`. The measured guest dirties
8,192 or 200,000 pages, either contiguously or with no two dirty pages adjacent.
The scattered shape is the worst case for run coalescing, and a guest that
touches memory sparsely produces it. The contiguous shape is the best case for
the mapping replacement that range write-protection replaced. Recorded on
2026-09-10 on the Lima aarch64 instance, before and after replacing per-run
mapping replacement with range write-protection:

| dirty pages | runs | commands before / after | seal before | seal after |
| --- | --- | --- | --- | --- |
| 8,192 contiguous | 8 | 8 maps / 8 protects | 5.4 ms, 654 ns/page | 3.1–3.4 ms, 0.4 µs/page |
| 8,192 scattered | 8,192 | 8 maps / 8,192 protects | 610 ms, 74.4 µs/page | 12–14 ms, 1.5–1.7 µs/page |
| 200,000 contiguous | 196 | 196 maps / 196 protects | 189 ms, 943 ns/page | 122–185 ms, 0.6–0.9 µs/page |
| 200,000 scattered | 200,000 | 196 maps / 200,000 protects | 20.4 s, 102 µs/page | 338–367 ms, 1.7–1.8 µs/page |

Each figure is one run of a single-threaded measurement on a shared laptop VM.
The "after" column shows the spread of two such runs. The contiguous rows vary
by another half as much between runs. The scattered rows do not need this
caution, because a difference of two orders of magnitude is not measurement
noise.

A real guest falls between the two shapes, and closer to the contiguous shape
than the worst case suggests. The full-guest seal of 7,827 mapped dirty pages
took 263 range protections, about thirty pages each. A guest writing memory
tends to write neighbouring pages. That capture's pause barely changes. The
change matters for a case this qualification cannot produce on a 128 MiB guest:
a large VM with a scattered dirty set. There it is the difference between a
sub-second stop and a pause of tens of seconds.

A run is limited by the seal's page-lock batch of 1,024. So 200,000 contiguous
pages take 196 commands, not one. The batched mapping replacement issued few
commands even for a scattered dirty set. But each of its runs was a new mapping
whose page tables had to be installed again. That caused the 20 seconds. With
200,000 pages of scattered dirty memory, a migration's pause was unusable.
Protecting the pages in place reduces it to a third of a second. The remaining
cost, under a microsecond per page in the contiguous cases, is the per-page
bookkeeping of adding the pages to the sealed set. It is not kernel work.

Smaller pager pages were measured on 2026-09-11 on the same instance, before the
2 MiB page was fixed. The runs covered boot, pnpm-install, a capture and a
fan-out of four forks at 4, 16 and 64 KiB
([records](measurements/page-unit-2026-09-11.json)). Further work includes other
kernels and architectures, jailer namespaces and cgroups, scheduling fairness
across many guests under memory and I/O pressure, and real build or filesystem
workloads with HugeTLB backing.
