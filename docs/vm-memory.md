# Managed VM memory

A VM's RAM and PMEM are [volumes](volumes.md). The Go package `vmmemory` is the
pager: it shares resident pages, resolves faults, makes private copy-on-write
pages, spills and evicts. A host runs one instance per kind of memory region,
one for RAM and one for PMEM, each with its own arena, spill file and page size.
The Rust library `rust/sproutfs-vm-memory` owns the mappings inside one VMM
process and has no Firecracker dependency. `vmmachine` prepares a VMM's memory
and drives the process a Starter starts; see
[hosting](hosting.md#running-the-vmm). `host` pauses and seals a VM for a
checkpoint or a fork point. `vmtest` is the syscall fixture for mapping races
and malformed commands. The fault path's histograms are in `internal/latency`.

Three nested packages are imported only by `vmmemory`:

- `internal/pageranges` is the interval map in which the Linux connection keeps
  the mapping generation of each run of its client's pages.
- `internal/slots` holds the arena free set and the consecutive runs of it that
  one mapping command covers.
- `internal/zirconvm` is the Go port of the page layer of Zircon's VM (TASK-92,
  [the plan](../plans/zircon-pager-port-2026-10-05.md)). Zircon's kernel is
  under the MIT licence, so the directory keeps Zircon's `LICENSE`. Every Go
  file in it begins with the copyright lines of the Zircon files it ports and a
  line naming those files at fuchsia `90e54e09`. A test checks each header
  against the list of files under `zircon/kernel` at that revision, which
  `scripts/zircon-sources.py` writes from a clone. The package holds the page
  list, VmCowPages and VmObjectPaged with their lookup cursor, supply, take,
  dirty states, writeback, zero intervals, reclaim and snapshot-on-write
  children; the page queues; the evictor (`evictor.go`); compression
  (`compression.go`), with a strategy that stores a page as it is in place of
  LZ4; the spill storage (`spillstorage.go`); and the page source, with
  `PagerProxy` as its provider (`pagerproxy.go`). The departures D1 to D4 are
  marked in `dirty.go` and `reclaim.go`. D5 is marked in `spillstorage.go` and
  `page.go`: a page holds a reference of the spill storage, its reservation,
  from before it is Dirty until it is clean, and is spilled into it.

`Host` and `MemoryRegion` hold the state, and each subject has one file:

- `host.go` and `region.go`: the pager and a memory region, attach, verify,
  detach and close. `vmmemory.go` holds the interfaces and `Config`.
- `frames.go`: a page's frame, the arena as the node's `Pmm`, the identity
  roots and their resolver, a frame's aliases, and idle pages.
- `bindings.go`: the binding beside the layer, and the page list of them.
- `fault.go`, `faultfirst.go`, `window.go`, `prefetch.go`, `population.go`
  and `pagerequests.go`: what a fault reads, in what order, and how a window
  is planned, read and mapped.
- `store.go`, `replacement.go`, `placement.go` and `rules.go`: what a store
  copies and where it puts the copy.
- `checkpoint.go` and `settle.go`: seal, read, share, retire, abandon, and the
  settle of unchanged pages.
- `evict.go`, `allocation.go`, `spill.go` and `pressure.go`: slots, the
  evictor, the spill and the dirty budget.
- `cold.go` and `giveback.go`: cold copies and their give-back.
- `isolation.go`, `revocation.go`, `serve.go`, `peer.go`, `stats.go`,
  `losswindow.go` and `flush.go`: the isolated arena, revocations, serving a
  migration, a peer backing, the statistics, the loss window and flushes.
- `connection_linux.go` and `linux.go`: the userfaultfd connection and the
  Linux arena.
- `faultqueue.go`, `repeats.go`, `guestfaults.go`, `changes.go`,
  `pageruns.go`, `aliases.go` and the `probe` files: the fault queue, repeated
  faults, the faults a guest waited for, changed blocks, runs of pages, a
  frame's alias set and the probe build's audit.

The arena and its files, isolation, placement, pressure, the loss window, the
flush and the connection are the pager's own code, not ported.

A page is a `zirconvm.VmPage` whose `Frame` is a slot of an arena file
(`frames.go`). The arena is the node's `Pmm`, but it makes no page itself: the
pager fills each page and supplies it, as a user pager supplies a VMO. A
published checkpoint's pages of one volume are an identity root, made when a
region first locates one of its pages and kept until the pager closes. A
region's own pages are its layer, which falls through to the identity root its
resolver names where Zircon walks up to a parent. Whether a page is mapped, and
which page it maps, is a binding beside the layer (`bindings.go`). A run of
zeros is an Untracked zero interval. In Zircon's terms: a read fault looks up
with the layer's lookup cursor (`RequireReadPage`); a store makes a layer page
Dirty (`DirtyPages`) through a page source that traps dirty transitions; a seal
starts writeback, making the layer's pages AwaitingClean; a store into a page a
checkpoint holds splits it (`SplitAwaitingClean`); and an idle page waits in the
don't-need queue and is given up by `ReclaimRangeForEviction`.

## Why a pager of its own

The pager does for guest memory what the kernel's page cache and swap do for a
file. The integration does not use the kernel's version, for these reasons:

1. **The page cache duplicates what VMs share.** It caches per file, and a
   reflinked file is a separate inode with its own cached pages, so VMs from one
   image hold one copy per VM. A guest with a virtio-blk disk also keeps its own
   page cache of the disk in its RAM. The pager keeps one resident page per page
   identity, however many VMs map it, and the guest reaches it through PMEM over
   DAX, which maps the host's page directly. Three checks enforce this:
   - A VM has no block device, only PMEM.
   - The host refuses a guest command line without `rootflags=dax=always`. With
     it, ext4 fails the mount where DAX is unavailable.
   - The guest's witness refuses a file on a PMEM device that the kernel does
     not report as DAX.

   The guest's init also remounts the root `noatime`. Every file in an image has
   an access time no newer than its modification time, so under `relatime` a
   fork that only reads would write the inode of every file it reads, dirtying
   shared pages. `noatime` is a mount flag, not an ext4 option, so `rootflags=`
   cannot carry it.
2. **Sharing is by name, across VMs and checkpoints.** A child's memory is its
   parent's checkpoints plus its own writes. As file mappings that is one VMA
   per run, counted against the kernel's mapping limit and changed at every
   checkpoint.
3. **Durability is one pause, not writeback.** A checkpoint needs every dirty
   page frozen at one moment while the guest keeps running. The pager does this
   with write protection and copy-on-write through userfaultfd.
4. **The backing is not only a file.** After a fork or a migration, a page's
   bytes can be on another host, and a filesystem cannot fault a page out of a
   peer host's memory.
5. **The budgets belong to the host.** The pager admits resident, logical and
   dirty pages explicitly. A guest that dirties pages faster than it publishes
   them is checkpointed early or stalled. HugeTLB pages are never swapped, so
   the pager must evict and spill anyway.
6. **It runs inside the simulation.** The campaigns exercise the same pager on a
   simulated arena, disk and clock.

Upstream Firecracker's userfaultfd restore copies pages into each VM's anonymous
memory, so it cannot share a page between VMs. The integration replaces that
path.

## One pager per kind of memory region

A pager instance has a fixed page size, and counts everything in it: its arena
and spill slots, its spill file's extent, its budgets, its read-ahead and
write-ahead runs (stated in bytes by a deployment), its buffers, a settle's
comparison, the byte conversions of `MemoryRegionStats` and `Sharing`, the
alignment it requires of a memory region and the Linux connection's fault
arithmetic. The page size must be one a volume can be published in, which
`checkpoint.GeometryFor` defines. The pager refuses to attach a volume published
in another page size, which also catches a memory region sent to the wrong
pager.

`vmmemory.Pagers` holds a host's RAM pager and PMEM pager, and a memory region
attaches to the pager of its kind. Their page sizes can differ, so a host reports
across them in bytes:

- The two arenas' capacities and the two dirty budgets add up to what the
  deployment gave the host.
- The sharing gauges are reported per kind under one metric name with a `kind`
  label.
- `Host.PrivateBytes` adds up a VM's memory regions across both pagers.

Pressure from either pager can ask for a checkpoint, which seals the whole VM in
one pause. A VM's loss window is the oldest unpublished write across its memory
regions in both pagers. If neither pager can admit a store, only that VM is
stopped.

**RAM's page and PMEM's are both 2 MiB** by default, on a real host and in the
simulation, unless a host's deployment names another
(`SPROUTFS_RAM_PAGE_BYTES`, `SPROUTFS_PMEM_PAGE_BYTES`). A 2 MiB page comes from
the host's provisioned HugeTLB pool. A 4 KiB page comes from an ordinary shared
memfd in the pod's own memory, which a host with swap may swap. A session states
its page size and its arena's kind when it attaches, and both ends check the
pair before any guest memory exists.

### The ephemeral pager

A host may run a third pager, for [ephemeral disks](volumes.md#ephemeral-disks)
(`vmmemory.Config.Ephemeral`, `Pagers.Ephemeral`). An ephemeral disk is PMEM to
the guest, and the session states PMEM on the wire. Its private pages are the
disk's only copy, so they must not take the dirty budget or the arena of the
disks that checkpoints publish, and must not bound a VM's loss window.

It differs from an ordinary pager in three ways:

- **Its dirty budget is its logical budget.** Both are the disk its spill file
  may fill. The host admits an ephemeral disk against the logical budget by its
  size, so every page of every admitted disk can be private at once, and a store
  never waits.
- **Its seal takes nothing.** A VMM asks every session to seal for a capture, so
  the seal succeeds and records no checkpoint.
- **It keeps no loss window.** `MemoryRegion.OnInterval` is false for its
  memory regions, as it is for RAM. The host leaves them out of the loss window,
  a flush of one completes at once, and pressure on its budget asks for no
  checkpoint.

A backing that states `Ephemeral` (`vmmemory.EphemeralBacking`, which a volume
and a peer backing implement) attaches only to the pager built for it, and that
pager maps nothing else. Its resident pages are evicted to its spill file and
read back like any other private page, so its memory is bounded by its arena.

## Bounded host pager

Each pager takes an arena, a scratch spill file, and resident, logical and dirty
page budgets. Logical admission bounds metadata for every attached memory
region, including pages never touched. Every private page takes a dirty
reservation before a write can resume: a reference of the spill storage
(`internal/zirconvm/spillstorage.go`) naming one page of the spill file, which
covers the page whether resident or spilled. A spill writes the page into it,
and a refault reads it back and checks the CRC32C the write recorded.

The spill file also keeps published versions, step 1 of
[the local writeback plan](../plans/local-writeback-2026-10-09.md). When a
checkpoint retires, a page whose bytes are in its reservation keeps them under
the identity it was published as, instead of giving the reservation back. A
load reads a version the spill file keeps before it reads the backing, so a
page this host published and then evicted comes back from the host's disk. The
bytes are a version only while no store can have changed the page since they
were written: the copy a checkpoint holds never changes, and a refault that
hands the guest its page as its own drops them, since its stores from then on
pass through nothing the spill file sees. Versions take only the slots
reservations leave: a reservation that finds none free takes the oldest
version's, with a second chance for one read since the queue last passed over
it. A version is kept under its page and a number for its checkpoint's volume,
sixteen bytes, and the index is bounded at 512 MiB: about 20 GiB of versions at
4 KiB pages. A version that fails its CRC32C is
dropped and the page read from its volume.

A published page an eviction takes is kept as its version too, step 2 of the
plan: before the evictor drops a page of an identity root, mapped or idle, it
writes the page to a free slot unless the spill file holds its version
already, so the page's next load reads the host's disk rather than its volume.
That write is the one thing an eviction of a clean page costs, and only the
first time, since a page loaded from its version is dropped again for free. A
page lent under a fork point's name that no checkpoint has published is not
kept. At most eight such writes are in flight, and the spill file has a slot
for each beside the dirty budget, so a version only ever takes a slot no
reservation can be refused for; an eviction that finds all eight busy drops
its page without one. The ephemeral pager keeps no versions, since nothing it
holds is published, and `MaxSpillVersions` below zero keeps none at all.

The dirty budget sets the size of the spill file. `New` allocates the whole
extent (`fallocate` on Linux) before the pager takes any work, and a filesystem
that cannot hold it refuses the pager, because a sparse file's unused space could
be taken by other writers on the node, and a spilled dirty page is the only copy
of what the guest wrote. Each concurrent I/O permit covers one read-ahead or
spill buffer. One extra permit is reserved so that a checkpoint's read of a
sealed set makes progress while cold faults use the others.

`LossWindow` bounds the same private state in time. For each memory region, the
pager records the age of the oldest page that no landed checkpoint covers. While
that age, across the memory regions of one VM, exceeds the window, the pager
admits no further dirty page for that VM while a sealed checkpoint of it is
uploading. Zero disables the window.

A store is never held while the checkpoint it would wait for still needs a
pause, because a held store holds its vCPU. So a store past the window goes
through, and asks for the checkpoint through `Pressure.Checkpoint`, when nothing
of its VM is sealed yet, when a pause is under way, and when a fork point's hold
stands. The stores after it wait once the checkpoint has sealed, so the guest
writes past the window for at most one pause. The owner's clock normally takes
the checkpoint at three quarters of the window. Where no checkpoint of the VM can
be taken, the store stalls and the VM's owner stops it.

The window belongs to the VM, because the checkpoint that ends it covers the
whole VM. The pager asks the memory region's owner through `Pressure.Oldest`.
The age stamp moves to the checkpoint at the seal, back to the memory region
when that checkpoint is abandoned, and across a handoff through
`MemoryRegion.Handoff` and `MemoryRegion.SetUnpublishedAge`. So neither a failed
publication nor a migration restarts the bound. If no checkpoint of the VM will
ever be taken, the wait ends with `ErrWindowStalled`, and the owner stops the
VM.

Memory region addresses and lengths must be aligned to the instance's page size.
An instance can run any page size that a volume can be published in and that the
transport maps; both sets are the same two sizes.

### An arena's offsets and its pages

An arena is a set of files, and a resident page is one slot of one file. Each
file reads, writes, zeroes, compares and releases its own slots, and keeps its
own set of held offsets. `Config.ResidentPages` is one count across all files.
`Config.Arena` (the host's `SPROUTFS_ARENA`) picks how pages are divided between
files:

- `isolated`, the default, splits the pages by who may read them, so a VMM
  reaches no other VM's memory. See [the isolated arena](#the-isolated-arena).
- `shared` keeps every page in one file of `Config.ArenaOffsets` slots, which
  every VMM receives read-write. It costs a fork's children less at a 4 KiB
  page, and any VMM reaches every guest's memory. The rest of this section
  describes this arena; the isolated arena builds on its file handling.

`Config.ArenaOffsets` is the number of addresses the arena has, and
`Config.ResidentPages` how many of them can hold memory at once. The memfd is
sized to `ArenaOffsets` and is sparse, so an offset costs nothing until a page
is put there, and `Release` punches it back out. `slots.Space` holds one bit per
offset and limits every allocation to the pages left. Each file's `leases` map
(`allocation.go`) holds one resource reservation per page. `AllocatedBytes`, the
memfd's allocated blocks, is the memory an arena holds.

RAM needs more offsets than pages: a private page goes at the same offset it has
within its 2 MiB range, so a range that holds one private page owns 512
consecutive offsets. The supervisor sizes RAM's offset space as one such extent
per range any admitted memory region may have written into (`LogicalPages`,
since a range is 512 pages and an extent 512 offsets), plus `ResidentPages` for
the read-ahead runs. PMEM's offset count equals its page count, which a
configuration that leaves `ArenaOffsets` zero describes.

The host must provision a 2 MiB HugeTLB pool for its arenas before it starts
VMs. An arena at 4 KiB pages is an ordinary memfd charged to the pod's memory.
Each arena reserves virtual address space without reserving its whole logical
capacity, and allocates each resident slot before it touches the slot's
mapping: the HugeTLB arena with `fallocate`, the ordinary arena with a
populating write fault (`MADV_POPULATE_WRITE`) through a separate mapping. When
the pool or the pod's memory is exhausted, allocation returns an error; it does
not fall back to another page size. HugeTLB pages cannot be swapped and shmem
pages can; in both cases the pager's spill handles reclaim. The kernel must
support missing, minor and write-protect userfaultfd faults for HugeTLB **and**
for shmem. All HugeTLB arenas on the host share one pool, so admission must
budget their combined resident capacity.

**The ordinary arena allocates only a zero run's whole 2 MiB blocks as huge
pages.** It maps its memfd twice: once advised never to use huge pages, for
every access and every other allocation, and once aligned to 2 MiB and advised
to use them, only to allocate blocks a zero run covers completely. The kernel
allocates and clears each such block as one transparent huge page; as 512
ordinary pages, a boot's write-ahead run costs several milliseconds per block.
Releasing one slot of a huge page splits it and returns only that slot.
`/sys/kernel/mm/transparent_hugepage/shmem_enabled` must be `advise`,
`within_size` or `always` for the blocks to be huge. Under `never` they are
ordinary pages and the pager behaves the same. The guest's translations are
4 KiB in both cases, because `UFFDIO_CONTINUE` installs one page table entry at
a time.

When a configuration leaves read-ahead and write-ahead zero, each is one page,
which means none. Read-ahead is a power of two of the pager's pages, capped at
16 MiB. Population walks metadata in windows of at most 256 MiB. The Linux
transport also bounds pending faults, control requests and fault workers.

The supervisor in `host` sets every bound per pager, from the share of the arena
the deployment gave that kind and from the node. The two runs are stated in
bytes and each instance converts them:

| bound | production value |
| --- | --- |
| `ReadAheadPages` | 8 MiB of this pager's pages: four at 2 MiB, 2,048 at 4 KiB |
| `PrefetchRuns` | `ConcurrentIO` |
| `PrefetchAtRandom` | on at 2 MiB, off at 4 KiB ([reading at random](#reading-at-random)) |
| `WriteAheadPages` | the same 8 MiB, or one page where the dirty budget holds fewer than 64 such runs |
| `ConcurrentIO` | four per processor, between 16 and 256, and never more read-ahead runs than the arena has room for |
| `SettleWorkers` | the node's processors, at most 64 |
| `ConnectionConfig.FaultWorkers` | two per processor, between 8 and 64 |
| `ConnectionConfig.MaxVMAs` | half of `/proc/sys/vm/max_map_count`, disabled below 128 or where the limit cannot be read, at most 2²⁰ |

A starting host logs each of these values per pager, next to its page size.

Attaching a memory region admits its metadata and verifies writer authority
before the region is exposed. The mapping must start as armed missing-fault
traps only. A caller must not attach one writable volume to two memory regions,
because the pager is the only thing that changes the volume's contents while it
is attached. A volume implements the backing interface: load and locate serve
the handle's current view, locate needs no I/O, and a cold load is a range read
inside the part that holds the page. Verify confirms that the handle still owns
its VM and makes nothing durable.

A memory region can also attach through a backing in front of its volume.
`vmmachine.Config.Backings` names one per volume, and a [migration](migration.md)
destination binds one. Loads first ask the host that still holds those pages,
and the volume serves the rest. The volume stays the region's identity (its
name, size, writer, page identities and every write), so seals, checkpoints and
fences are unchanged.

Two placements use an ordinary offset (`vmmemory/placement.go`):

- A store that copies away from the copy a checkpoint froze, because its own
  offset holds the bytes the upload is reading.
- A page that a migration destination loads privately from the source, which
  arrives in its own run. A post-copy destination's private pages are not placed
  until the guest stores into them.

### The isolated arena

A VMM may be compromised, and it holds every descriptor its sessions are given.
In a shared arena that descriptor reaches every page of the pager. The isolated
arena (`SPROUTFS_ARENA=isolated`, the default) splits the pages by who may read
them, so a VMM's descriptors reach its own VM's memory and the pages its tenant
may read. The design and its threat model are in
[the plan](../plans/isolated-arena-2026-09-25.md).

That holds only for a VMM that runs as neither root nor the pager's user, since
either could reopen a read-only descriptor for writing through `/proc/self/fd`.
The host's Starter therefore jails every VMM (`vmmachine.Jail`, the host's
`SPROUTFS_VMM_JAIL`), as Firecracker's jailer does: chrooted into a directory
holding the VMM, its policy, the kernel and its own `/dev`, with no `/proc`, as
its own user out of a range, in a group that alone may open the jail's devices.
A memory session whose peer is root, or not the user its placement names, is
refused (`checkPeer`). An embedder's Starter names its VMM's user in
`Placement.Owner`.

There are four kinds of file:

- **A private file per memory region.** It holds the region's private pages:
  dirty, sealed, written ahead, spilled back in, and loaded privately. Only that
  region's VMM receives it, read-write, as file 0. It has twice the region's
  pages. A page's own offset is its index, and the second half is each page's
  other place, which a store takes when the own offset holds a checkpoint's copy
  or the page it copies from. When both are taken, one holds a clean page
  nothing maps, and the store gives it up, or a page an eviction is taking,
  whose lock the store waits for before it looks again. A range's extent is that
  range of the file, so the gap and half-private rules work as in a shared
  arena.
- **A shared file per tenant.** It holds the pages another memory region of the
  tenant may map: pages loaded by identity, and published pages once another
  region inherits them. The tenant's VMMs receive it read-only, as file 1. It is
  made when the tenant's first region attaches and goes back to the arena with
  its last idle page. It has the pager's whole offset space
  (`Config.ArenaOffsets`) of address space, though only its pages cost memory.
- **One public file.** It holds the pages of public templates
  (`control.Public`): pages loaded by an identity whose checkpoint is a template
  of no tenant, and such pages moved out of the private file that published
  them. Every VMM receives it read-only, as file 2. No other page enters it.
- **A fork file per fork point.** It holds the pages a fork point lends to
  children on this host. A child's populate or fault copies a lent page there
  once, at its own index, and later children map the same copy. The children
  receive the file read-only, as file 3 or up, just before they first map from
  it. The parent keeps its page. When the seal ends, the children's mappings of
  the copies are revoked, each child is sent DROP_FILE, and the file goes back
  to the arena. The seal's end waits for a copy under way: it takes each
  page's lent name away under that page's lock, which the copy holds from start
  to end, before it takes the root's copies. That includes a page a retire
  published under another name, whose lent name stays until then.

Each file is a memfd of the pager's kind, with mode 0600. A read-only file is
sent as a new open of the memfd with `O_RDONLY`, so the kernel refuses a VMM a
writable mapping of it, a write, a punch, a resize and a new seal.

**The host states the tenant.** It attaches each memory region with its VM's
tenant (`MemoryRegionBacking.Tenant`), the part of the VM's identity before the
slash. A page's identity is the checkpoint that published it, whose VM names the
tenant too. A fault whose backing names a page of another tenant fails with
`ErrOtherTenant`, in either arena, and so does a fork point that would name its
pages under another tenant's checkpoint.

**Public templates are the exception.** A VM of any tenant may be created from a
template of no tenant. A region may read a page of its checkpoints (`mayRead`),
and the page goes in the public file. A fork point never names pages under a
public checkpoint, so no guest's writes become public. Tenants sharing these
physical pages is a timing channel that reveals at most which pages of a public
image another tenant reads.

A page no other region may inherit is loaded into the region's own file: a page
with no identity, a page a fork point lends, and a page another host still
holds. A clean one goes to its other place.

**A published page moves once another region inherits it.** A checkpoint leaves
a published page in its region's private file, where the guest keeps mapping
it. `MemoryRegionCheckpoint.ReadDirty` hashes each page it reads with BLAKE3,
and the retire keeps that digest with the page while it is in a private file. A
page without one is not named by its identity. When another region wants the
identity, the pager copies the page into a free slot of the tenant's shared file
and compares digests:

- If they match, the owner's mapping is replaced with the copy, read-only, and
  the copy is installed in the owner's page tables. The private slot goes back.
  `Stats.MovedPages` counts these.
- If they differ, the owner's VMM wrote a page it holds read-only. Its session
  ends with `ErrTampered`, `Stats.Tampered` counts it, and the identity is no
  longer named. The region that wanted the page reads it from its own volume.

**A move replaces the owner's mapping; it does not revoke it.** On x86-64 the
next fault on a revoked page is a store trap for any access
([cold copies](#cold-copies)), so revoking made the owner copy every moved page:
a GCE run of first inheritance at 2 MiB recorded 403 copies for 428 moves
(TASK-49). The move holds neither the owner's memory region nor its window. It
holds the page's lock, which every path of the owner that maps or resolves that
page also holds, and the owner maps the page read-only, so no store is in flight
into it. Where the owner's client refuses the MAP for want of mapping budget,
the move revokes the mapping instead. A page that leaves its root while its owner
still maps it goes back into the owner's layer, and keeps the owner's detach
out from the look at its mapping until it is there. A page whose owner is
detaching is taken from its regions and freed instead.

A move that finds no free slot of the shared file does not wait: the page stops
being named by its identity, and the region that wanted it reads its volume.
`Stats.ForkCopies` counts the copies into fork files, which need no digest.

A private file outlives its region while it holds idle pages, and goes back to
the arena with its last page.

**The files are counted.** A VMM can allocate pages in its own private file, and
reading a hole of a shared memfd through a mapping makes the kernel allocate a
page there. So every verification compares the private file's allocated blocks
with the pages the pager put there, under the lock the pager gives slots under,
and ends a session whose file holds more with `ErrUncounted`. The same check on
the tenant's shared file punches every offset that holds no page. A detach does
both.

## Sharing by identity

Resident pages are keyed by the [page identity](volumes.md#reads) that the
volume reports. Any memory region in the same pager whose current identity
matches a resident page maps that page, whichever region loaded it. A fork
inherits its parent's identities until it writes a page. A page is named by the
checkpoint that published it, so identical bytes written independently keep
distinct identities. Checkpoint publication may store an all-zero page sparsely;
that is separate from resident sharing.

A page the parent held dirty at a fork point has no published identity, so the
fork point names the pages it sealed. The point takes its own reference, and the
parent publishes the point under it behind the fork, so each page's name is the
identity it is published by, and a child that maps it goes on mapping it after
the point retires. The pager enters these pages in the sharing index, so a child
on the parent's host maps them like any inherited page, in the eager restore
population before its vCPUs run. They stay the parent's private dirty state:
nothing is copied or made durable, the parent still copies on write and owns
their reservations, and the name lasts no longer than the page is the seal's.
A published checkpoint replaces the name with the identity the volume then
reports. A retired fork point hands the page back to the guest as dirty state it
may store into in place, so the retire takes the page from any memory region
still sharing it. A retire, an unseal and a detach take each page's name away
under that page's lock as the page leaves (`unlend`), so no child maps a page
past its checkpoint or reads the parent's later stores. At the seal's end the
point's root becomes an ordinary root of its name: pages a child read under the
name and still maps stay in it. A share after the seal's end lends nothing.

A resident page is one store page, and one identity covers it whole. A page with
no published bytes, such as a migration destination's copy of the source's
unpublished pages, has no identity and loads privately until a checkpoint gives
it one. A one-byte store copies and charges the whole page. Storage compression
does not change this accounting.

An explicit sparse zero has no arena slot and no identity. Contiguous zero ranges
use Linux's shared zero page, one mapping command however many pages they cover.
These and untouched missing-fault traps have no resident page; their anonymous
page tables are the only memory not backed by the arena. The first write replaces
one page of a zero range with a private arena page.

A memory region keeps its per-page state in a page list, the port of Zircon's
`VmPageList`. A page with state of its own has a slot holding its binding: its
resident page, its reservation, and whether it is mapped, dirty, cold or held by
a checkpoint. A run of zero mappings is a zero interval, which holds its two
ends. An untouched page has no slot. A node of the list is 16 slots of 16 bytes,
so a page touched alone costs a 256-byte node, its entries in the list's tree
and map, and a 64-byte binding. A memory region that reads one page in 512 holds
389 bytes for each page it read, against 21,828 with the blocks of 256 bindings
the page list replaced (`sparse_metadata_test.go`, 2026-10-05). A binding never
leaves the list while its memory region is attached, because resident pages and
faults hold pointers to it. A bound page that a zero run covers records that in
its binding, because a slot holds a binding or lies in an interval, never both.

## Faults and read-ahead

A read fault that follows a recent one serves its aligned read-ahead run, its
own page first. Pages already resident under the same page identity are mapped
without a read. The faulting page is read alone, installed with those resident
pages in one mapping command, and its access resolved. The pages it reads ahead
are a **prefetch**: one backing read on its own goroutine, started beside the
fault's read, which the fault never waits for (`vmmemory/prefetch.go`). A fault
at random serves its page alone, except in a pager of 2 MiB pages
([reading at random](#reading-at-random)). Read-ahead uses only free slots and
never evicts; only the faulting page may cause an eviction.

How far a fault reads ahead is decided by streams, as Linux's on-demand
read-ahead does (`vmmemory/readahead.go`), not by the run. A fault that goes on
none of a region's streams starts one. One on a page after a stream's latest,
within one more read-ahead of what that fault brought in, goes on it. A stream's
first two faults read their pages alone; its third reads four pages ahead, and
each fault after that four times as many, until one reads its whole run. A
region's first fault reads its whole run. A fault maps the resident pages of the 16 from its own on, which
costs no read, whatever its stream has earned.

This is Zircon's split. Its kernel maps at most 16 present pages around a fault
and reads nothing else (`kPageFaultMaxOptimisticPages`); how much to read is
the user pager's, and Fxfs reads an aligned 128 KiB. A 4 KiB pager sees a guest's
read of 8 KiB as two faults on adjacent pages. While the second fault of a run
prefetched the rest of it, the embedder's PostgreSQL benchmark on GCE on
2026-10-09 loaded 44 pages and evicted as many for each fault of its random
reads, and read 252 times a second against 23,825 on plain Linux.

Reading the page first matters to a guest that follows pointers. On GCE a 4 KiB
page from the cluster took 0.65 ms and an 8 MiB run 39 ms; at 2 MiB, 10 ms and
20 ms ([dependent reads](measurements/gce-dependent-reads-2026-10-03.md)).

A prefetch's pages land as clean pages under their identities, idle and in the
sharing index. The prefetch then takes the window's locks, as a fault does, and
maps them read-only into the memory region whose fault asked for them, if the
guest still has nothing there and the page's identity is still the one that
landed. On x86-64 this matters, because KVM reports every page it waited for as
a store ([cold copies](#cold-copies)). A page that cannot land as a clean shared
page is left to its own fault: a page whose bytes go in the region's own file, a
page a migration's source still holds, and a page with no identity. A page the
source turns out to hold when the read returns is dropped.

Only a fault on a page a prefetch is already reading waits on it, and then plans
its window again ([page requests](#page-requests)). These rules keep a prefetch
from costing a fault:

- At most `Config.PrefetchRuns` prefetches read at once. Past that a fault reads
  its page and nothing else.
- A prefetch takes only free slots, giving up idle pages for them. Its landed
  pages are idle until something maps them.
- An allocation that finds no free slot and no idle page cancels every prefetch
  still reading and takes their slots, before it evicts a page a guest maps. It
  waits until each slot is back or holds a landed page.
  `TestAnAllocationCancelsAPrefetchRatherThanEvict` holds the cancelled
  prefetch's slots back a millisecond after its read ends
  (`SetPrefetchSettleSeam`) and requires the fault to wait for them.
- Its reads are marked with `checkpoint.WithPrefetch`. Their peer requests go
  over the bulk class, on their own connections and within the host's background
  budget. The page cache gives their loads their own slots. A read of the
  cluster for one asks no second request after the delay, never reads the store
  as a hedge, and is left out of the delay estimate.
- A detach cancels its memory region's prefetches and waits for them to end.
- A fault that waits on a prefetch whose supply already answered the page's
  request waits on nothing: its own request is answered at once.

A post-copy stream's faults read their whole run at once (`vmmemory.WithStream`).

`Stats.Prefetches` counts the prefetches and `Stats.PrefetchedPages` the pages
they landed; both are also counted in `Loads` and `LoadedPages`.
`PrefetchMapped` counts the landed pages mapped as they landed,
`PrefetchWaits` the faults that waited for a prefetch already reading their
page, `PrefetchRefused` the runs left unread at the bound, `PrefetchCancelled`
the prefetches an allocation cancelled, and `PrefetchDropped` the pages a
prefetch reserved a slot for that never landed. `Stats.Load` times the reads a
fault waits on, and `Stats.Prefetch` a prefetch's.

The prefetch read asks only for the pages of the window that need bytes, through
`vmmemory.SparseLoader`, which `volume.Volume` implements. The volume groups the
wanted pages by part, one ranged read per part, and reads through left-out
pages. A 512-page window with 64 scattered resident pages read one stretch at a
time took 65 loads and 195 object reads; it now takes the faulting page's load
and one more, and four object reads. A backing that cannot be asked for part of
a range, such as a migration destination's peer backing, is still read one
stretch at a time.

### Page requests

Every backing read of a fault or a prefetch answers a `READ` request to a page
source, as a Zircon VMO's missing pages do (`vmmemory/pagerequests.go`;
Zircon's `vm/page_source.cc`, ported in `internal/zirconvm`). A request that
starts inside one already sent waits on it, and a supply or failure of a
request's range wakes it and every request waiting on it. Where Zircon's
`PagerProxy` hands each request to a user pager in another process, here the
fault or prefetch that sends a request answers it: it reads the pages, then
supplies the range, or fails it if the read failed.

There are two kinds of source:

- **An identity root's.** A page's identity names its root, and its offset in
  the root is its own. A prefetch sends one request to the root of each run of
  its pages. Two prefetches of one page batch: the later one leaves the page to
  the earlier. A fault on a page a prefetch is reading waits on the prefetch's
  request, and plans again when the prefetch supplies or fails it. A page the
  prefetch failed to land is that fault's to read. Only a prefetch sends to a
  root's source, under the host's lock: a fault's lookup goes down into a root
  only while the root, under its own lock, still holds the page (`Holds`), and
  otherwise asks the region's own source. A prefetch's look at the reads under
  way and its send are one hold, so no request meets another. What the source
  did with a request is read as it is sent, under the source's lock
  (`PageSource.SendPages`): another region's fault supplies pages under the
  root's lock alone, and may complete the request the moment that lock goes.
- **A memory region's own.** A fault's own reads go there: the faulting page's
  read, a page read alone, a run, and a store's read of the page it copies. Only
  the faults of one window read its pages, one at a time, so these requests
  never batch, and a prefetch does not see them. A prefetch can still read a page
  another memory region's fault is reading, and the copy that lands second goes
  back (`TestAPrefetchedPageAnotherLoadMadeResidentFirstIsDropped`).

Which faults prefetch, which pages a fault reads and in what order, and how many
prefetches read at once are the pager's policy, not Zircon's.

### Reading forwards

A guest that reads its memory in order still reads it in runs. Its first fault
in a run reads that page and starts the prefetch of the rest. Its next fault
finds that page in flight, waits for the prefetch, and maps the whole run when
it lands. So a run costs two faults and two reads, no page is read twice, and a
run takes about as long as the run's read alone.
`TestAGuestReadingForwardsStillReadsItsMemoryInRuns` holds the pager to that;
the in-tree bug `pager-read-in-flight-again`, which reads a page a prefetch is
reading again, fails it.

### Reading at random

Only a fault that goes on a stream that has earned a read-ahead prefetches
([faults and read-ahead](#faults-and-read-ahead)). Any other fault reads its
page and nothing else, and `Stats.PrefetchRandom` counts it.

Every fault maps the pages from its own on that are resident under their
identities, up to 16 and its window's end, as Zircon's does, and reads none of
them. It does not count them used: a page mapped around a fault or landed by a
prefetch keeps its age and its queue, as Zircon's do
(`TestPagesMappedAroundAFaultKeepTheirAge`).
`TestAFaultMapsTheResidentPagesFromItsOwnOnReadingNone` holds the pager to the
16.

The reason is processor time: at 4 KiB, a prefetched run of 2,047 pages from the
cluster is about 100 ms of processor time to check and decode. On GCE on
2026-10-04, a chain of dependent 4 KiB faults from the cluster took 24 ms a hop
with every fault prefetching its run, 3.2 ms prefetching only behind a fault that
follows a recent one, 41 ms with the run read first, and 0.67 ms for a page read
alone. From the store the same four took 31 ms, 29 ms, 80 ms and 25 ms
([measurement](measurements/gce-fault-first-2026-10-04.md)).
`TestADependentChainOfFaultsWaitsForOnePageAHop` holds a chain at random to one
prefetch, its first hop's; the in-tree bug `pager-prefetch-every-fault` fails
it.

**A pager of 2 MiB pages prefetches at random too** (`Config.PrefetchAtRandom`,
which the host sets from `vmmemory.PrefetchesAtRandom` for a page of 2 MiB or
more). At 2 MiB a run is four pages, three to prefetch, and a guest reading at
random soon touches most of its runs. Valkey with a 4 GiB heap, restored on GCE
on 2026-10-04, faults in about 2,800 of its 4,096 pages over 20,000 dependent
GETs. Its GETs took 22.5 s from the cluster and 99.8 s from the store reading
each fault's page alone; 22.4 s and 55.6 s with each run read before its page;
and 11.7 s and 32.5 s prefetching behind every fault
([measurement](measurements/gce-real-app-restore-2026-10-04.md)). The chain of
2 MiB faults pays for it: 10.4 ms a hop from the cluster against 7.1 ms.
`TestAPagerThatPrefetchesAtRandomFaultsOnceARun` holds a guest touching every
page at random to one fault a run; the in-tree bug `pager-read-alone-at-random`
fails it.

### Planning a fault

A fault plans only what it reads (`vmmemory/faultfirst.go`):

- A fault on its window's last page with nothing to read ahead locates its
  page in one lookup of one page, takes it, bound to a resident page under its
  identity or to a slot to read it into, and reads it.
- Any other fault locates and takes its page alone and starts the page's read
  on its own task. Only then does it locate the 16 pages from its own on and
  what its stream has earned to read ahead, in one lookup of the volume, and
  plan them while the read is under way: it maps what is resident there, and
  reads what its stream has earned.
- A post-copy stream's fault plans its whole window first, because it reads the
  window with its page.

A fault that plans its window takes the window's bindings under the region's
binding lock once, looks up every page's identity among the resident pages under
the host's lock once, and in the same hold asks each identity root of the window
once which of its pages a prefetch is reading. The volume locates the window
with one lookup of each page-table segment it crosses
([reads](volumes.md#reads)). A fault holds no object lock while it reads or
while its mapping commands run, and its window's stripe keeps one window's
commands in order.

A fault that prefetches takes its own slot out of a run of free slots for the
whole window, at its page's place in the run, so the window is one run of slots.
The slots of pages the window turns out not to need go back once it is located.
A store into a page the guest never touched asks whether that page is a hole
before it locates the window for its write-ahead run.

Planning the whole window before reading anything cost a dependent 4 KiB fault
from the cluster 3.07 ms, and 1.05 ms once the planning ran beside the read,
against 0.67 ms for its page's read; from the store, 29.1 ms and 25.7 ms
([measurement](measurements/gce-fault-planning-2026-10-04.md)). The rest was the
window's planning, about 0.75 ms of processor a fault at 4 KiB, which a fault at
random does not need. Planning a fault at random's page alone took the median hop from the cluster from 0.98 to
0.83 ms, against 0.66 ms for the page alone, and the chain's fault processor
time from 0.41 s to 0.05 s
([measurement](measurements/gce-random-fault-planning-2026-10-04.md)). On one
Ice Lake processor a fault at random over a real index costs 10 µs of planning
and the pager's own work against 575 µs, and a fault reading forwards 1.03 ms
against 2.21 ms (`BenchmarkARandom4KiBFault`, `BenchmarkAForward4KiBFault`).

`TestADependentChainOf4KiBFaultsPaysOnePageReadAHop` runs a 4 KiB pager over a
real checkpoint store and holds every hop at random to one read, one lookup of
its own page and one of the 16 around it behind the read, with the segment
decoded once; the in-tree bugs `pager-fault-around-the-window` and
`checkpoint-decode-every-lookup` fail it.
`TestAForwardChainOf4KiBFaultsPlansItsWindowInOneLookup` holds every hop reading
forwards to one read and its page's planning, its window located in one lookup
beside the read; `pager-plan-the-window-first` fails it.

### Populating at attach

Before the memory region is exposed, attach populates the pages whose identity
is already resident in the same pager. It loads nothing. The Rust session
reports the region's addresses to the VMM only after it has served these mapping
commands, so they finish before VMM setup uses those addresses and before any
vCPU runs, on cold boot and on snapshot load. A restore still returns paused. An
explicit later population requires quiescent guest memory.

The populate is bounded. The kernel installs a run's pages one write-protected
entry at a time, about a microsecond per page: on GCE on 2026-09-23 a warm
restore's 128 runs carried 839,196 pages and took 1.10 s. A skipped page costs at
most a fraction of a command, because the fault that reaches it maps its whole
read-ahead window. So a populate installs a run only when it covers at least one
read-ahead window, installs at most 128 runs and 16,384 pages in total, and
shortens a run longer than the pages left. Holes meet the same length test and
budget. Measured on GCE, an unbounded populate mapped 2,930,747 sibling-resident
pages of a 16 GiB guest in 21,698 runs, taking five seconds of a restore whose
bound is half a second, and the guest then took 723 faults. Bounding only the
resident runs left 14,447 runs over 2,166,194 pages, of which 123,056 were
resident identities.

The runs a fork point names are exempt from the length test but not the budget,
and take the budget first, because ending the seal removes their name.

The walk that finds those runs asks the volume, window by window, for every
page's identity, and stops at the first window it has no budget left for.
Stopping loses the record of which explicit zeros this memory region knows,
which lets a sibling map holes without its own metadata.

`AttachStats` records the cost of a session's whole `Connect`: descriptor
exchange, admission, ATTACH, populate and the READY round trip, with the
populate's commands, runs, pages and duration (`MemoryRegion.Populated`).
`vmmachine.StartPhases` reports it beside the VMM process's start, the snapshot
load, and the round trip that proves the machine is up.

When the pager refuses a memory region, the VMM's boot or load request fails,
and the reason is on the pager's side. Both sides log the failure. The VMM's log
names which kind of attachment failure it saw:

- the pager closed the session before attaching
- the pager closed it partway through a frame
- the pager sent a file without its descriptor, or a descriptor with a frame
  that carries none
- the descriptors did not fit this side's ancillary buffer
- a file whose descriptor is not what its frame states

A failed request to the VMM is reported with every session's result. The
supervisor closes the process first, so joining the results does not wait. The
memory region listeners give the VMM two minutes to connect, and the wait for its
API socket has the same bound. A start that fails after it takes its directory
from the shared scratch goes through Close, so the scratch does not keep
counting the process.

### Stores

A store into a page this memory region holds no memory for first reads the page,
prefetching the rest of its run, as a read fault does, then copies the one page
the guest stored into. On x86-64 a guest that only reads memory it inherited
reaches the pager as a store ([cold copies](#cold-copies)); when such a store
read only its own page, a GCE fan-out on 2026-09-22 took 21,130 faults, 20,016
of them copy-on-writes. Every page of the run except the faulting one is mapped
read-only under its identity, so the pages the guest reads next need no fault and
stay shared. The faulting page is neither mapped read-only first nor bound to the
shared page, because its copy replaces it. A migration destination's backing is
read one page at a time, because only a load can tell whether the source still
holds a page.

That read can lose to another holder of the page: a prefetch reading it, a
fault or an eviction holding it, or its root giving it up before the store binds
it. The store waits and reads again, and its decision to make a page of its own
stands. Those losses are bounded as a load's are, 64, apart from the eight times
a fault may decide again whether it needs a page of its own: under eight forks
of one checkpoint on two vCPUs each, a store lost its read eight times running,
and counted as decisions they ended the guest's session with `ErrContended`
(`TestAStoreThatLosesItsReadTenTimesRunningStillStores`).

A store into fresh memory (a zero-mapped page, or a hole the guest has never
touched) has nothing to copy. One mapping command replaces the zero mapping or
the trap with a private page from a free slot, which reads as zeros because it
was punched; the volume is not read.

The page a store's copy replaces stays locked until the store's command lands.
Its alias goes before the command, so anything that took the page meanwhile,
such as the end of a fork point's seal dropping the copy it lent, would find
nothing to revoke and give back a slot, or drop a file, the guest still maps
(`TestTheEndOfASealLeavesACopyAChildsStoreStillReplaces`).

**A store replaces a mapping; it never revokes one.** The protocol's MAP replaces
the mappings its pages had: the client builds the new mapping, registers it
and `mremap`s it over the guest's addresses in one command. A revocation is used
only for:

- a reclaim taking a victim
- a settle handing a page back to its origin
- a retire handing back a page for which the volume holds no object
- an abandoned checkpoint
- a store whose mapping the client refused

When every private page first cost a revocation, a GCE fan-out of three forks
running `cargo test` spent 1,753 s of 5.4 million commands on revocations, 2.6
per fault.

**Every revocation other than a store's is sent as one command per run.** A
retire that sent one command per page cost a GCE fan-out of two forks 12,428
revocations for 15,477 write-ahead pages. The retire revokes a batch's
handed-back pages together, before the walk that publishes the rest
(`MemoryRegion.revokeHandedBack`).

Replacement requires an ordering (`vmmemory/replacement.go`). Until a store's
mapping command lands, the guest keeps reading the page it copied from, so that
page stays in place and no reclaim may take it. If the store held its last
binding, its memory is released after the command. A store that fails to map its
run, because the client is out of mapping budget or a command failed, revokes
the run instead.

### Keeping a memory region's mappings whole

Every separately mapped run of a guest's memory is a mapping in its VMM process.
A private page written into the middle of an inherited run turns one mapping
into three. The kernel caps mappings at 65,530, each separate dirty run costs a
write-protect command in a checkpoint's pause, and a fragmented range cannot get
a huge mapping. Three rules apply, and the mapping budget backs them up.

**A private page lives at its own offset.** Each 2 MiB-aligned range that holds a
private page owns one extent of the arena's offset space, and a private page goes
at the same offset within the extent as within the range. So private pages
adjacent in the guest form one mapping in any write order. The two exceptions
are the ordinary-offset placements under
[an arena's offsets](#an-arenas-offsets-and-its-pages).
`Stats.PrivateExtents` is the number of ranges that own an extent.

**A store closes a small gap once its memory region is near its mapping
budget.** A store within sixteen pages of a page its range already holds makes
the pages between them private in the same command, so the two runs become one.
The worst case, a guest that writes one page in every seventeen, costs seventeen
times what it wrote, against 512 times at a 2 MiB page. A 4 KiB fan-out on
2026-09-21 recorded 1,979 gaps between private runs, 1,532 of them (77 %)
sixteen pages or fewer. A gap is never closed across a range's boundary. The
rule applies only after the process has refused the region a mapping. With it
always on, forks of a seeded database updating keys at random held 4.7 GiB each
on 2026-09-23: the guest's writes caused 345,000 page copies and the rule
3,040,000. Plain Firecracker's clones held 0.69 GiB. Each memory region gets half
the node's `vm.max_map_count`, which is 1,048,576 on a current distribution.

**A range that is half private becomes private.** When half a range's pages are
at their offsets in its extent, the pager copies the rest into the extent's
holes. The range is then one mapping and one write-protect command, and can get a
huge mapping, at most twice what the guest wrote into it.

Neither rule waits or evicts. A run ends at a page with no free dirty reservation
or no offset of its own, or whose offset a checkpoint holds. The settle hands
back the pages a rule copied that the guest never wrote, except in a range made
whole, which it leaves whole. `Stats.RuleCopies` counts the pages the rules
copied. A rule takes a page of another read-ahead window only if it can take
that window's stripe at once, and a page only if it can take the page's lock at
once; it ends its run at either. It holds each page it joins to the run until
the store's command lands, so no eviction frees one first.

**The mapping budget turns the gap rule on.** When the client refuses a store's
mapping for its mapping-count budget, the store makes the range whole with one
command and is served again, and from then on its memory region closes gaps.
It does so only around a store whose page is at its own offset: a store that
copied away from a sealed page to another offset is refused and served again,
so the guest's page is never mapped onto the checkpoint's copy.
`Stats.MappingMerges` counts this.

PMEM's pager does none of this: its page is the whole range.

A store into fresh memory also writes ahead: the fresh zero pages after it in
its read-ahead run, and before it when the run ends first, get private pages in
the same command, at most `Config.WriteAheadPages`. Write-ahead takes only free
slots and free dirty reservations, and never waits. A run that finds no room
has one eviction step free a batch first and is placed in what it freed, so it
does not shrink to its faulting page once the arena is full. The run is mapped
writable, so a guest writing fresh memory in order faults once per run, and the
pager never learns which of its pages the guest stored into. Each holds a dirty
reservation and is written back like a stored page, so a guest can run out of
dirty budget earlier by the write-ahead pages it never used. A migration source
serves them as held. `Stats.WriteAheadPages` counts the pages runs mapped beyond
the faulting pages, and `Stats.WriteAheadZeroPages` those whose written-back
bytes were still all zero.

**Both pagers write ahead.** A hole has no identity, so making a store's
neighbours private loses no sharing, and a 4 KiB RAM page uses the same 8 MiB run
as PMEM. A 16 GiB guest's boot on GCE made 103,035 pages private, of which
65,280 are the 256 MiB memmap, written page by page, and 16,384 are swiotlb's
64 MiB bounce buffer, memset in one block. With an 8 MiB run those two take 32
faults instead of 81,664.

**A write-ahead page that the guest never stored into costs nothing after the
next checkpoint.** It reads back as zeros, so the publication gives it no object
and the volume reports a hole. The retire then takes the page away from the
guest, releases it and returns its dirty reservation.

### Faults read the cluster before the store

A cold fault, and a refault of a page the pager evicted, asks the volume for its
window, and the volume reads a run through the page cache. The pages the cache
holds in memory come from there. Inside the share the cluster cache is turned on
for, the rest come from the hosts' disks: this host's own stripes of each
window, then k+1 of the window's ranks, and the store only for a page fewer than
k stripes of which exist
([reading from the cluster](hosting.md#reading-from-the-cluster)). The pager
does nothing different. A fault waits for the cluster's read of its page as it
waits for the store's, and the cluster's read reads the store as well once it
has waited past its bound. The prefetch of the rest of the run reads the cluster
as background work over the bulk class, and never reads the store as a hedge
(`TestAPullsReadsOfTheClusterAreBulkWorkThatNeverHedges`). A page rebuilt from
stripes is checked by its envelope's SHA-256, as a page from the store is.

### A pulled VM's faults

A VM can be marked to [pull its whole memory](hosting.md#pulling-a-vms-memory).
Inside the share the cluster cache is on for, the pull makes sure the cluster
holds every window; outside it, it copies the pages onto its host's disk. The
pager does nothing different: a fault reads its window through the page cache,
from memory, then the hosts' disks, then the store
([the page cache's disk](volumes.md#the-page-caches-disk)). Once a pull is
complete, a cold fault costs a disk read and a decode instead of a round trip to
the store, while the disks hold the page.

A pull's reads are marked as a prefetch. It makes no request while a load of the
page cache is in flight, and it takes none of the cache's load slots.

An eviction drops a clean page rather than spilling it, because its volume holds
its bytes, so a pulled VM's refault reads the hosts' disks. The spill file holds
private pages, which bound the dirty pages the pager admits, and the published
versions of pages that were spilled before their checkpoints retired.

A migration's destination attaches through the peer backing. A page the source
still holds, and every page no checkpoint holds, comes from the source. Every
other page the peer backing reads from the destination's own volume, which the
pull fills. The pull never reads the source: the pages only the source has
become this host's dirty pages when they arrive, and the next checkpoint
publishes them.

## What the sharing is worth

`Stats` counts what the pager has done:

- `IdentityHits` counts every page mapped to an identity that was already
  resident.
- `CopyOnWrites` counts every page of which a store took a private copy.
- `UnchangedPages` counts every page that a settle found to hold the same bytes
  as the page it was copied from.

These counters never decrease. The checkpoint's log line reports the settle's
count next to its dirty set, as `unchanged_pages`.

`Host.Sharing` is the matching gauge, per memory region kind:

- `UniqueBytes` is the host memory the arena holds. One resident page counts
  once, however many memory regions map it.
- `MappedBytes` is the sum, over memory regions, of the resident pages each maps.
  Every alias counts, including two memory regions of one VM and a checkpoint's
  copy of a page the guest still shares with it.
- `SavedBytes` is the difference.

A page no memory region maps, such as the page a store copied away from, still
counts in `UniqueBytes`, under the kind of the region that created it. Inherited
identities that nothing has faulted in cost this host no memory and are not
counted.

`MemoryRegionStats` reports the same for one memory region, in pages and bytes:

- `ResidentPages`: the pages that hold host memory.
- `PrivatePages`: the pages whose bytes belong to this memory region and not yet
  to its volume, whether resident, spilled or held by a checkpoint.
- `SharedPages`: the resident pages that at least one other memory region of
  this pager also maps.

A memory region's kind, RAM or PMEM, is passed to `Attach` and never inferred
from a volume's name. Inside one pager, no fault, seal or page depends on it. A
host that decides which pager a VM's memory regions would be admitted to, before
a machine exists, states the kind itself.

The pager has no concept of a VM, so the host adds the numbers up per VM.
`Host.PrivateBytes` in `host` is one VM's private bytes across every memory
region it maps, reported by `/status`, `/metrics` and the VM listing. The
Prometheus gauges are `sproutfs_pager_unique_resident_bytes`,
`sproutfs_pager_mapped_resident_bytes` and `sproutfs_pager_shared_saved_bytes`.
Each carries `kind="ram"` or `kind="pmem"`, as every other pager series does.
`sproutfs_pager_arena_bytes` is the only total, and it is in bytes.

Faults serialize only within one read-ahead run. A short host lock covers
capacity accounting, binding pointers and the shared index; no backing read,
spill or mapping acknowledgement holds it. Each resident page has its own
transition lock for mapping changes and reclaim, and read-ahead skips a page
whose lock is busy. A page an object holds is bound under that object's lock, so
no idle drop takes it between a fault's lookup and its binding. A memory region's
per-page state (whether a page is mapped, what it is dirty under and which
checkpoint holds it) is kept under the region's binding lock, which guards its
page list, because a reclaim that revokes its victim's pages under a page's lock
and a seal that reads those pages under the memory region do not otherwise
exclude each other.

Reclaim uses fault and read-ahead recency. Accesses through page tables that are
already present do not update it, so the evictor harvests a page before it takes
it: it takes the page's mappings away and keeps the page, and the guest's next
access faults and marks it accessed ([Choosing the victim](#choosing-the-victim)).
An ambiguous mapping acknowledgement keeps its possibly live slots allocated until
the process exits.

## Seal, checkpoint and verification

A checkpoint is the only way a memory region's dirty pages become durable. The
pager never writes to a volume, even for a guest's flush, which the host answers
([the control protocol](#control-protocol-version-10)).

A seal takes over the memory region's loss window: the age of the oldest page it
freezes becomes the sealed set's, and the region's own age restarts at its next
store. Retiring the set as published drops its age. Abandoning it, when a
publication does not land or on an unseal, hands the age back, and the region
keeps the older of that and its own.

Sealing write-protects the pages the guest has, in place, with one range
write-protect per run of consecutive dirty pages, however many of the client's
mappings the run spans. Nothing is copied, no mapping is replaced and no page
table is installed or dropped. The guest keeps reading the same pages, and only
its next store traps.

**A seal's pause consists only of those commands.** Once a run is protected, the
set is fixed. The per-page work runs afterwards as a walk, while the guest runs,
holding the memory region the seal took. For each page it moves the binding into
the checkpoint and hands the checkpoint the page's reservation and the page it
was copied from. The region keeps its dirty set as runs, the Dirty intervals of a
page list of their own, so the pause reads O(runs), never O(pages). A fault of
that region waits for the walk, and so do the checkpoint's readers (the settle,
the page list and the upload). On GCE, a capture of 2,204,672 sealed RAM pages at
a 4 KiB page paused for 2.14 s: the 2,264 protect commands took 0.18 s and the
walk 1.97 s. `seal_ns` and `seal_walk_ns` report the two. Only `seal_ns` is time
during which the guest is stopped.

While a seal holds the memory region, only a reclaim can revoke a mapping, under
the victim page's lock only. So the region has a protection lock: every
revocation holds it shared, and the seal's write-protect commands hold it
exclusively, so every revocation has either finished or not begun. Nothing that
holds this lock waits for the memory region or for a page.

A seal that fails partway captures nothing and takes no page. The pager revokes
the mappings of the runs it protected, so the guest maps them writable again,
and the next checkpoint takes the whole dirty set.

A fault holds the memory region shared during its planning, its metadata and its
page-table commands, and releases it during the backing read (a volume load, or
a migration source that keeps answering BUSY) and during the reclaim a new page
may need. A seal taken during either step does not wait for it. The window's
stripe owns that window for the whole fault, and is taken before the memory
region. The region's exclusive holders are a seal, a retire, an unseal, a handoff
and a detach. Only the detach waits for faults, on a separate lock a fault holds
from start to end.

So a reclaim can hold a page the seal is about to take. The reclaim ends with
the page nonresident and its bytes in the page's own dirty reservation, so the
seal joins the checkpoint's copy to that resident page without taking its lock,
hands the copy the reservation, and revokes the page instead of write-protecting
it. Whether a reservation holds its page's bytes is therefore state of the spill
storage, not of the binding. The reclaim reads the page's aliases and then the
reservations they name, and walks the alias set again if it has grown since, so
the bytes reach a reservation in either order.

Retiring a checkpoint walks the set in bounded batches, releasing the memory
region between them. Each batch needs the identity the volume now gives each
page, one lookup per read-ahead window, done before taking the region or any
page. Only one retire or unseal runs at a time. A page already retired is
skipped, so repeating a failed retire finishes the remaining work.

The sealed pages are detached copies that alias the pages the guest had and own
their dirty reservations. A store into a sealed page copies on write: the guest
gets a fresh private page, and the seal keeps the original until the fresh page
is bound. If the store fails before that, for example when the arena cannot fill
the slot, the page stays where the seal left it and the guest faults again. If
the seal ends during that store's reclaim, the page gets back its own
reservation or clean state, and the store decides again from the beginning.

The dirty budget counts sealed pages with live private pages, and belongs to the
host. A guest that dirties pages faster than its checkpoint uploads them waits
for the next checkpoint that lands anywhere on the host. A migration
destination's read of a page the source still holds takes that page as dirty
state, so it waits the same way: before it loads anything, the fault releases the
memory region, its page and its I/O permit, and takes a reservation through the
waiting path. Read-ahead around it takes only free reservations.

If no checkpoint is in flight, the pager asks for one through `Pressure`, the
pair of callbacks through which the memory region's owner responds. The host
offers the memory regions with the largest dirty sets first. A fault that
crosses three quarters of the budget asks for a checkpoint before any store has
to wait. The crossing is measured when the fault returns, so every path that
admits a page counts: a reservation a store waits for, one a peer-served load
takes, and a write-ahead run's reservations, which can cross the mark by
themselves.

A sealed memory region is not offered a checkpoint. It answers the wait only
when ending its checkpoint relieves the budget. A publication's end does that. A
fork point's hold does not while its children are still reading it, so while a
fork holds a memory region here, every other region's pressure must be relieved
by a checkpoint of that other region.

A store fails only when no memory region can be checkpointed to free the budget,
with `ErrDirtyStalled`, not `ErrCapacity`. The same pressure reports the region to its owner, so
that the VM is stopped and publishes what it holds; a failed fault would kill the
VMM and lose those bytes. A fault has no separate deadline: the command
timeout bounds each round trip inside it. `Stats.DirtyWaits`,
`CheckpointRequests` and `DirtyStalls` count the three outcomes. A host that
stalls has a budget or an interval too small for its guests.

The publication reads the sealed set from those pages.
`MemoryRegion.Checkpoint` exposes it as `volume.DirtySource`: the pager pages
the set holds, one at a time under an I/O permit and that page's lock, and the
retire that ends the set. Each sealed page is written as one member of the
checkpoint's parts, at the part and offset the index records. A read of a set
that has already ended fails with `ErrNotSealed`, or with the reason a detached
memory region discarded it.

A sealed set whose checkpoint was selected retires as published. A page the
guest has not stored into since the seal becomes clean, joins the sharing index
under the identity the volume now reports, and stays mapped. For a page the guest
copied away from, the sealed copy is released. The dirty reservation is returned
in both cases. A sealed set whose checkpoint never landed is abandoned instead,
as `MemoryRegion.Unseal` does: pages the guest still shares take their
reservations back and are dirty again, and their mappings are revoked so the
next store maps them writable. The next checkpoint takes them.

A memory region has at most one outstanding seal. Sealing a sealed memory region
reports `ErrSealed`, and so does handing it off, because a publication is
reading its pages under the volume handle the handoff would give away. Pages
count as sealed as the walk takes them, so a capture that never completes still
reports the walk's work.

The spill file is scratch storage, never a crash-recovery image. A starting
process truncates it, and it is written but not synced. A released slot keeps
its blocks, because punching them would give them back to the filesystem and the
next page spilled there could find them taken. A released slot's old bytes are
never read.

Verification checks writer authority even when cached accesses never fault. The
Linux connection runs it per volume on a timer with bounded deadlines. A failure
closes the control channel, and the supervisor must then terminate the process
before mappings are detached. Verification checks authority at the time of the
call; it is not an expiring lease. A fenced writer cannot obtain another
conditional write.

**A sealed page whose bytes equal the page it was copied from is not dirty.** A
write fault is not always a store ([cold copies](#cold-copies)), so the pager
copies the page and checks afterwards. When the pager serves a store by copying
away from a resident page that holds a published identity, the binding keeps an
eight-byte pointer to that resident page, its origin. An evicted origin is no
longer an origin. Some copies have none:

- a page copied from a checkpoint's held copy
- a page copied from the name a fork point lent a private page
- a page copied from another host's unpublished page
- a page made from zeros

`MemoryRegionCheckpoint.Settle` compares. The publication calls
`volume.DirtySource.Settle` once, after the pause and before it enumerates the
pages. A published identity's bytes never change, so the comparison, under both
pages' locks, is one `bytes.Equal`, with no hash and no read of the store. The
settle skips a sealed page that the pager has spilled. An unchanged page leaves
the checkpoint's set, so `DirtyPages` does not list it. If the guest still shares
the checkpoint's copy, the binding takes the origin as its resident page and
becomes clean, **the guest's mapping of the copy is revoked**, the private page
is released and the dirty reservation is returned. If a store lands first, only
the checkpoint's copy is released. A checkpoint the settle leaves empty holds no
unpublished write, so its loss window ends there.

The settle revokes rather than replaces the mapping, because it holds neither
the memory region nor the window that orders a page's mapping changes. Revoking
costs one fault per re-shared page. On a host whose kernel reports a cold read
as a write fault, that fault copies the page again, and the next settle undoes
the copy again.

The settle compares in parallel on `Config.SettleWorkers` workers, by default
the host's processors, each page under its own lock and its origin's. The memory
regions of one VM settle concurrently. The decisions are applied afterwards, in
page order and in bounded batches under the memory region, so the revocations go
as one command per run and a fault waits for one batch. A batch never waits
for an origin's lock while it holds the region: a prefetch may hold that root's
page waiting for a window's stripe, which a fault holds waiting for the region.
A copy whose origin is held stays in the checkpoint
(`TestASettleNeverWaitsForTheLockOfThePageItsCopyWasMadeFrom`). The settle takes
no I/O permits. The workers share only the count of unchanged pages and the set the
checkpoint will list, under the checkpoint's mutex, so the result does not
depend on the order workers finish. An arena that can compare two of its own
slots does so in place; every other arena is read into two buffers per worker.

A fork point is not settled, because its children are reading its pages while
the parent publishes it. Its children inherit an unchanged page as an
unpublished page, and each child's next checkpoint settles it. This applies to
RAM and PMEM.

The settle does not prevent the copy: between the fault and the next checkpoint
the host holds the page twice. Preventing that needs a host kernel that passes
the guest's access type through, or KVM userfault. Both are TASK-32 in the
[backlog](../backlog/tasks).

### Capturing for a durable flush

In the [durable flush](architecture.md#durable-flush) mode, a flush of a disk
writes the disk's changed blocks to its host's journal
([the plan](../plans/fsync-journal-2026-10-06.md), `vmmemory/journal.go`).
Guest stores are CPU stores into mapped memory, which nothing logs, so the
pager is what knows which pages changed. One rule keeps that right:

> Every page the guest can store into without a fault is unjournaled.

Every path that gives the guest a writable page marks it unjournaled
(`noteStoredLocked`), and so does every page that becomes dirty state,
including a page a migration's source supplies. `MemoryRegion.Capture` takes
the unjournaled pages a flush has to cover. It holds the region exclusively,
as a seal's pause does, so no seal falls inside it:

1. It write-protects the pages the guest maps writable, one command per run,
   under the region's protection lock. It reads which pages those are under
   that lock too, as a seal does: an eviction takes a mapping away under the
   page's lock and the protection lock shared, not the region, so a page
   evicted after the capture took its pages is not mapped, and protecting it
   would be refused and end the session
   (`TestACaptureProtectsNoPageAnEvictionUnmappedAfterItTookThePages`).
2. It reads each page once, from the guest's page, its spill, or the sealed
   copy where the guest still shares it, and hashes each 4 KiB block with
   SHA-256 on `Config.SettleWorkers` workers.
3. It keeps the blocks whose digest differs from the one the region holds, and
   the region keeps the new digests.

The guest's next store to such a page takes a protect trap. For a page no seal
holds, the trap maps the page writable where it is and marks it unjournaled. It
copies nothing. A trap on a sealed page copies on write as before, and the copy
is unjournaled.

**Digests.** The region keeps the digest of each block of each page it has
captured, in memory: 16 KiB for a 2 MiB page. They describe what the journal
holds for the page, so an unchanged block is one a replay already restores. A
page with no entry since it became the region's own takes its digests from
what a replay starts from: the page it was copied from, while that is
resident and its lock is free, or zeros for a page made from zeros. Otherwise
its first capture writes it whole. The capture never waits for the origin's
lock: it holds the region exclusively, and a fault that holds the origin in
its plan takes the region back after its read before it lets the page go, so
the two would wait for each other
(`TestACaptureNeverWaitsForTheLockOfThePageItsCopyWasMadeFrom`). The GCE soak
hung there on 2026-10-08. A store that lands while a capture reads its block may or may
not be in the entry. The page is unjournaled again, so the next capture takes
it.

**Across a seal.**

- The seal takes the region's unjournaled pages as its checkpoint's
  unjournaled list, and drops their digests. The region's set starts empty.
- A capture while the seal stands takes the list too. It reads the sealed copy
  where the guest still shares it, and the guest's page where the guest has
  stored into it since, which takes the page off the list. So a flush answered
  while a checkpoint uploads covers the stores made just before the seal.
- A published checkpoint drops the list. A page still on it, which the guest
  has not stored into since, drops its digests, so its next copy takes them
  from the published page.
- An abandoned checkpoint gives every page still on the list back as
  unjournaled, with no digests.
- Every other page keeps its digests across the seal. `MCCapture` in
  `spec/journal` checks this rule, and fails it when an unjournaled page keeps
  its digests too ([model checking](testing.md#model-checking)).

**A failed batch.** A capture whose journal write or sync failed gives its
pages back as unjournaled, with no digests (`Captured.Fail`): its entry may or
may not be on the disk (B6 in [spec/bugs.md](../spec/bugs.md)).

RAM and ephemeral disks are not captured (`ErrNotJournaled`). The pager still
never writes a volume: the host writes the captured blocks to its journal.

### Giving back an unchanged copy

RAM is never checkpointed on the interval, so a RAM page a fork shares with its
parent could stay a private copy for the VM's life. So the pager gives a
[cold copy](#cold-copies) back with no checkpoint and no pause. Its session gives
it back 200 ms after it was made, and an eviction or a seal sooner. A copy of a
page the guest mapped is a store the guest made, and is left to the settle.

For each cold copy, the give-back:

1. takes the page the way a store fault does: its read-ahead window, the memory
   region shared, the origin's lock and then the copy's;
2. write-protects the copy with `UFFDIO_WRITEPROTECT`;
3. compares the copy with the origin, as the settle does;
4. if they are equal, maps the origin in the copy's place with one MAP, frees
   the copy and its dirty reservation, and installs the origin's page table
   read-only;
5. otherwise, takes the write-protection off again, and the copy forgets its
   origin so that it is never compared again.

The window keeps every fault of the page out and the memory region keeps a seal
out, so the copy cannot change while it is compared. A store that trapped is
served afterwards and copies again.

**The guest is pointed at the origin in place, not revoked.** The give-back
holds the window, so its MAP is the same command, under the same locks, as a
store's. A revoked page's next cold read on x86-64 would arrive as a write and
copy the page again; an installed page is present, so KVM maps it for a read
without asking the pager. The MAP and the install are the ones a
[move](#the-isolated-arena) uses, `mapInPlace`. A client that refuses the MAP
for want of budget keeps its copy, the write-protection comes off, and its
session tries again 200 ms later.

A spilled cold copy is compared from the spill, and one whose origin was evicted
is compared with its volume (below). `Stats.GiveBackCompares` counts the
comparisons and `Stats.GivenBackPages` the pages given back.

The give-back relies on every writer of guest RAM going through the VMM's page
tables. See [writers that bypass the page
tables](#writers-that-bypass-the-page-tables).

### Cold copies

On x86-64 KVM finishes a cold fault from `async_pf_execute`, a worker that asks
for the page writable for any guest access, so every page a guest reads of
what it inherited arrives as a store trap and is copied. On aarch64 the
architecture reports the guest kernel's cache maintenance on a page it executes
for the first time as a write. The pager cannot tell these faults from stores
while they wait.

So a copy a store trap makes of a published page is **cold**: private and
writable, but not yet known to be the guest's state (`vmmemory/cold.go`). It
becomes an ordinary dirty page only when a comparison with its origin finds the
guest changed it. A protect trap is a store into a page the guest maps, which
KVM reports only for a real store, so its copy is never cold. While a copy is
cold:

- **Its origin is pinned** in the page queues' zero-fork queue, which an
  eviction takes from last. If the origin is evicted anyway, its copies are
  compared with the bytes their volume holds for their page instead, a backing
  read (`MemoryRegion.volumeHolds`). An unchanged copy whose origin went is
  dropped.
- **An eviction gives it back rather than spill it,** once it is 200 ms old,
  taking each lock without waiting (`Host.giveBackVictim`). An unchanged copy
  goes back to its origin with nothing written to the spill; a changed one stops
  being cold and is spilled. A younger copy, or one whose give-back could not
  take a lock, is spilled cold.
- **Its session reads a spilled one back,** compares it and gives it back,
  unmapped, or makes it an ordinary dirty page.
- **Its session gives it back soon after it is made,** with the five steps of
  the give-back (`Connection.giveBackColdCopies`), for RAM and PMEM alike.
- **Every seal compares it.** The walk behind the pause leaves out of the set
  each cold copy whose bytes are still the origin's
  (`MemoryRegion.leaveOutColdCopies`), and `Stats.UnchangedPages` counts it. So
  no checkpoint, whether a capture, a disk's interval checkpoint or a fork
  point, holds a cold copy the guest did not change.

The worker waits until each copy is 200 ms old, because KVM's worker takes the
page writable before the vCPU retries its access, so a copy just made holds its
origin's bytes whether the guest meant to read or to store. Compared at once,
every cold store would be copied twice. By 200 ms the vCPU has retried.

The give-back leaves a range the rules made whole alone; the seal still leaves
its unchanged cold copies out. The pages the rules copy beside a store are not
cold.

### A fault refaults its own page that was spilled before its lookup

A page that is the region's own state, its dirty page or the checkpoint's it
shares, is a page of its layer or, once spilled, only its reservation. A fault
that finds such a page bound and not mapped, as an unseal leaves it, has
nothing to complete, gives the page's lock back, and looks the page up. An
eviction in between spills the page out of the layer. The lookup then went on
past the layer, to a root's page or a read of the volume, and bound those
bytes: the guest read what it had stored over, and its store was lost. The
lookup now asks, under the layer's lock, which an eviction takes to remove the
page, whether the page is the region's own state; where it finds anything but
the layer's page, it fails any request it sent, and the fault decides again
from the top and refaults the page from its reservation
(`TestAFaultRefaultsItsOwnPageAnEvictionSpilledBeforeItsLookup`, guard
`pager-look-a-spilled-page-up-past-its-layer`). The unscheduled soak found it
as an invalid resolution about once in seventy runs; the mapping audit
(docs/testing.md) named it.

### A refault decides again after its reclaim

A reclaim for a private page releases the memory region while it looks for an
arena slot, so a seal and a retire can both run inside a fault that has already
decided what its page is. If a checkpoint retires a spilled page in that window,
the volume holds its bytes, the reservation that spilled them is returned, and
the binding is clean. A refault that then bound a private page into the binding
would leave a page with neither a reservation nor a checkpoint, which the
eviction (`evictPage`) punches without writing anywhere, and which nothing names,
so other memory regions of the volume would read their own copies.

The refault (`refault` in `vmmemory/fault.go`) reads the page's dirty state and
the checkpoint's copy before and after the reclaim, as the store path does, and
if either changed, gives the slot back and decides again from the start.
`TestARefaultWhoseCheckpointRetiresWhileItReclaimsGivesThePageToTheVolume`
drives the interleaving through a reclaim seam. Without the re-check it fails on
every run: in the ordinary build with the second memory region reading its own
copy, and in the probe build with the audit's panic from the store's
`takePrivate`, which the probe build's
`TestSealTakingAReclaimingPagesReservationKeepsItsBytes` first hit under load.
On 2026-09-22 on a fifteen-core machine, with 50 lanes each and a detector on
the grant, 10 of 50 lanes granted before the re-check and 0 of 50 after; the
chance of that by luck is about 1 in 70,000. The panic is rarer than the grant
(about 1 lane in 50 there, 1 in 8 on an eight-core machine), and 0 of 150 lanes
panicked after.

### A run read decides again after its reclaim

A run read can give the region up in its reclaim, between locating its window
and taking the window's pages. A checkpoint that publishes and retires there
names a page the guest stored into anew, so the window's old name for it is its
parent's. The plan records which pages the region held nothing at when it
located the window (`plan.free`), and takes only those by their located names.
`TestARunReadTakesNoPageByANameAPublicationReplaced` runs the checkpoint inside
the reclaim; before the fix the guest read its parent's byte after its own
store.

A plan that holds a page of the region's own layer reads nothing more: it would
give the region up for the read with that page's lock held, and a retire that
holds the region waits for the lock. The rest of its run is left to its own
faults (`TestARunReadHoldingItsRegionsOwnPageLetsARetireRun`).

## Ownership

The Go pager owns:

- The arena's memfds and their slots, page identities and alias references, and
  which file each session may read.
- Fault resolution, copy-on-write decisions and private page allocation.
- Spill, reload, and the decision to evict a page.
- Punching the arena and reusing a slot, only after accounting for every alias.

The Rust library owns:

- The stable host virtual address ranges identified as PMEM or RAM.
- UFFD creation, registration and descriptor transfer to the Go pager.
- Applying mapping changes in its own process and acknowledging them.
- Keeping mappings, descriptors and generation bookkeeping alive for the
  session.

PMEM and RAM share one mapping implementation and one durability contract.

## Rust interface and lifetimes

A session maps one volume as one contiguous range. A VM's RAM, the one volume
`ram0`, is one session, and each of its PMEM devices is another, each over its
own socket. The library knows nothing about guest addresses; the VMM places a
volume's bytes in the guest's address space
([the Firecracker build](#firecracker-build-and-process-lifecycle)).

`Session::connect` reserves the memory region's range, at the largest page size
the transport maps, creates the UFFD and exchanges descriptors. The attachment
then states the region's page size and what its files are made of, and the
client checks that it maps that page size, that every file's descriptor is that
kind of memory (an explicit 2 MiB HugeTLB file, or an ordinary shared memfd of
4 KiB pages), and that the region's length is a nonzero multiple of the page
size. A mismatch ends the session before any address is exposed.
`Session::page_size` reports the agreed page size. `Session::memoryRegion`
returns an address descriptor, not a Rust borrow. `Session::run` services
commands on a dedicated thread. The Go pager reads the UFFD directly.

`Session::control` returns a cloneable device-side handle. Its `start_seal` asks
the host to seal this memory region while the session thread keeps serving
mapping commands. Each session has at most one seal request pending, so a
coordinated capture issues all the requests and then waits.

Only the host's deadline decides a seal. If the host cannot finish a seal within
its command timeout, it answers with a failure: every page the seal took goes
back to the guest as dirty state, and the VMM answers its capture request with
that failure and keeps running. The client's own wait is five minutes, a
backstop for a host that has stopped answering, so that it does not kill a guest
whose checkpoint was only slow. A wait that does expire closes the session.

All raw-pointer users must stop before the session is dropped. Ordinary Rust
references must not be held across mapping changes. External device and kernel
pins require coordination by the embedding process. On a control or mapping
error, the session becomes terminal and keeps its UFFD and mappings until it is
dropped, because closing the UFFD would turn fault traps into zero-filled
memory. The fixture and the production supervisor terminate the process on
unexpected control loss, and the supervisor holds every connection and
descriptor until `waitpid` confirms exit. For an orderly shutdown, stop all
memory users and unregister the KVM slots first, then send STOP.

## Mapping replacement

A new mapping is never exposed and then registered or protected, because another
thread could access it in between and bypass demand loading or copy-on-write.
The library:

1. builds the mapping away from the live address;
2. registers it with UFFD, and write-protects it if it is immutable (a mapping
   of a read-only file always is);
3. replaces the live range with `mremap(MREMAP_FIXED | MREMAP_MAYMOVE)`;
4. acknowledges only after that syscall completes.

Three or more runs of one batch that form a single stretch of the memory region
are built together in one reservation, which takes one `MADV_DONTFORK`, one
`UFFDIO_REGISTER` and one `UFFDIO_WRITEPROTECT` instead of one of each per run.
Each run still takes its own `mremap`. A batch of 64 such runs costs 136 kernel
calls instead of 320.

Nonresident ranges are anonymous readable and writable mappings registered for
missing and write-protect faults, with no populated pages. They are not
`PROT_NONE`, which would raise ordinary protection faults.

Resident ranges are views of a file, registered for missing, minor and
write-protect faults. The private file is mapped `MAP_SHARED`. A read-only file
is mapped `MAP_PRIVATE`, because the kernel refuses to register a shared mapping
of a read-only file with userfaultfd, and on HugeTLB also `MAP_NORESERVE`, so it
reserves no pool pages. `UFFDIO_CONTINUE` installs the file's own page, so a
read-only file's pages are physically shared. The pager never clears write
protection on a read-only file's range, because a store the kernel let through
would copy into memory the pager never sees. An immutable page is armed for
write protection before it becomes accessible. The pager issues
`UFFDIO_CONTINUE` over whole mapped ranges, so read-ahead and populated pages
get their page tables before any access. A range `UFFDIO_CONTINUE` that meets a
present host page reports only that page, so the pager then finishes the range
one host page at a time. One mapping command covers a run of consecutive pages
whose arena slots are also consecutive.

Removing write access is not a replacement: the pager issues one
`UFFDIO_WRITEPROTECT` over the whole run on the live addresses, and the kernel
applies it to every registered mapping the range covers.

## Kernel and VMM constraints

The library requires `UFFD_FEATURE_EVENT_REMAP`; without it, Linux drops the UFFD
context on remap. With it, remap completion waits until the event is consumed,
so **the Go UFFD reader must keep draining remap events while another goroutine
waits for a mapping acknowledgement.** These features are also required, all
together:

- `UFFD_FEATURE_PAGEFAULT_FLAG_WP`
- missing and minor faults on HugeTLB (`UFFD_FEATURE_MISSING_HUGETLBFS`,
  `UFFD_FEATURE_MINOR_HUGETLBFS`)
- missing and minor faults on shmem (`UFFD_FEATURE_MISSING_SHMEM`,
  `UFFD_FEATURE_MINOR_SHMEM`)
- `UFFD_FEATURE_WP_HUGETLBFS_SHMEM`, which is write protection for both

The set is negotiated once, when the UFFD is created, before the attachment
states the session's geometry, so a kernel that supports only one kind cannot
run this build. The error names the missing features. Both the negotiation and
the per-range ioctl mask are checked. Anonymous write protection appeared in
Linux 5.7, shmem minor faults in 5.14 and shmem write protection in 5.19; these
are not qualified versions.

A seal requires `UFFDIO_WRITEPROTECT` to apply across every registered mapping
its range covers, since a dirty run's pages are separate mappings whenever their
slots are not consecutive. Linux walks the VMAs of the range. The suite tests
this at the syscall fixture and through the real client. A kernel that required
one mapping per call would fail the ioctl and the seal.

`UFFD_USER_MODE_ONLY` does not cover KVM's own accesses, so the deployment must
grant kernel-fault UFFD through the permitted syscall route or
`/dev/userfaultfd`, and allow it under the actual seccomp policy. The client's
mapping-count budget is opt-in, and zero disables it. A nonzero limit bounds the
mappings the session's own memory region makes, which the client counts from the
commands it applied rather than from `/proc`, so a jailed VMM, which has no
`/proc`, keeps it as well as any other. Two pages side by side are one mapping
when they are traps, or the next page of one file; the zero page is counted apart
from the traps it may merge with. A test drives a real session and checks that
count against the kernel's after every command. Until 2026-10-09 a client without
`/proc/self/maps` counted six for every mapping command and never less, so a
jailed VMM refused every map after 87,381 of them: on GCE it held 326 mappings
when it did, and its guest froze while the pager waited for a revocation
(TASK-122.5).

Eviction cannot rely on `madvise`: dropping anonymous contents leaves the shared
backing resident elsewhere, and punching the backing makes future accesses read
zeroes. So the pager revokes every alias and waits for the acknowledgements
before it releases a slot.

Guest DAX bypasses the guest page cache but does not change the host backing.
Firecracker requires 2 MiB PMEM alignment. Its save order is fixed:

1. Pause the vCPUs.
2. Save devices, before KVM state, because device completion can inject
   interrupts.
3. Capture memory.

So a seal must tolerate device accesses after the vCPUs pause. Those accesses
copy on write like any other store.

The adapter refuses any `huge_pages` setting for managed RAM, because the pager
states its page size when the session attaches. It also rejects ballooning,
memory hotplug, vhost-user and asynchronous block I/O with managed RAM. A
coordinated capture requires managed PMEM for every disk and refuses ordinary
block devices. KVM slots are unregistered before mappings are dropped. Linux
mapping invalidation covers ordinary CPU accesses and the tested KVM accesses.
No Rust lock surrounds each load.

### Writers that bypass the page tables

The seal, the settle, a copy-on-write, an eviction and the
[give-back](#giving-back-an-unchanged-copy) all rely on every write into guest
RAM going through the VMM's page tables, where a write-protection or a
revocation stops it. A writer that pinned a page earlier and writes it later
through its physical address bypasses both. Its write lands in the page it
pinned, and if the pager has since frozen that page in a checkpoint, moved the
guest to a copy, or freed the page, the write is lost.

These are the writers of guest RAM, checked on 2026-09-26 against the
Firecracker fork and mainline Linux:

- **Firecracker's block device.** With managed RAM the fork refuses the io_uring
  engine and vhost-user drives at boot (`allocate_memory_regions` in
  `resources.rs`). The sync engine opens its file without `O_DIRECT` and reads
  into guest memory with `pread`. The kernel copies with `copy_to_user`, through
  the page tables, so a write-protected page traps. The restore path does not
  repeat the check, but a managed restore only loads state that a managed boot
  captured, so no restored drive is asynchronous either.
- **The embedder's drives.** A Starter gives them to Firecracker as ordinary
  drives, so the same rule applies. A writable one also makes every managed
  capture fail.
- **Firecracker's other devices.** Network, vsock, entropy, MMDS and the vmclock
  device are emulated in Firecracker's own threads. They write guest memory with
  ordinary stores or `readv`. Firecracker has no vhost-net and no vhost-vsock.
  Ballooning and memory hotplug are refused with managed RAM.
- **KVM on its own behalf.** Steal time, the async page fault token, PV EOI and
  the SMM state save area are written through the userspace address, with
  `copy_to_user`. kvmclock is written through a `gfn_to_pfn_cache`, which the MMU
  notifier invalidates; `UFFDIO_WRITEPROTECT` and every remap call the notifier.
  The cache refills with a GUP that asks for the page writable, which takes the
  userfaultfd fault. The CPU's own writes, including the accessed and dirty bits
  of the guest's page tables, go through the second-level page tables, which the
  same notifier write-protects.
- **KVM for a nested guest.** When the guest runs a hypervisor of its own, KVM
  maps pages of the guest's memory with `kvm_vcpu_map` and gives their physical
  addresses to the CPU, which the MMU notifier does not reach. On Intel these are
  the APIC-access page, the virtual-APIC page and the posted-interrupt
  descriptor of the nested guest (`nested_get_vmcs12_pages`), mapped only when
  the guest's VMCS turns on APIC-access virtualisation, TPR shadow or posted
  interrupts. The Firecracker fork never offers a nested guest those three
  controls, so KVM maps none of those pages, and refuses to enter a nested VM
  whose VMCS asks for one anyway. Every other access KVM makes to a nested
  guest's memory copies through the userspace address: the VMCS itself, its
  bitmaps and lists. The MSR bitmap is mapped read-only, and only for the length
  of one entry. This was checked against Linux 7.0's
  `arch/x86/kvm/vmx/nested.c`; see TASK-57. On AMD, `vmcb12` and the host save
  area are mapped for the length of one VMRUN or one exit, and nothing narrows
  that, so only an Intel host runs a nested VM (`vmmachine/nested.go`). aarch64
  is not exposed, because Firecracker never asks KVM for a vCPU with EL2.
- **Debuggers.** `process_vm_writev` and `/proc/<pid>/mem` pin a page and copy
  into it at once. Only a process allowed to ptrace the VMM can do this.

So every writer of guest RAM goes through the page tables, and the seal, the
settle, an eviction, a move and the give-back see every write.

### Nested VMs

A nested VM is experimental. Its guest may run VMs of its own, so it is offered
VMX, and every other guest is offered neither VMX nor SVM. `vmmachine` decides
this in the CPUID of the boot configuration (`vmmachine/nested.go`), because KVM
lets a guest turn VMX or SVM on only when its CPUID offers it. Only an Intel
x86_64 host runs a nested VM, and its RAM is paged like any other VM's; see the
writers above.

The x86_64 qualification tests it.
`TestOnlyANestedGuestIsOfferedHardwareVirtualisation`: the nested guest sees VMX
and creates a VM on `/dev/kvm`, and a plain guest sees neither flag and has no
`/dev/kvm`. The `NestedGuest` tests in `vmmachine/nested_l2_linux_test.go` run a
small L2 guest inside a nested VM (`sproutfs-guest-witness kvm`), check that L1
is not offered the three controls, and check that the L2 keeps running across a
capture and restore, a fork and a live migration. The Firecracker fork's own
tests of the narrowing and of nested state run beside them. The guests boot a
kernel with KVM built in, named by `SPROUTFS_FIRECRACKER_NESTED_KERNEL`; the GCE
qualification builds it from Firecracker's CI configuration
(`scripts/lib/nested-kernel.sh`). The CI kernel itself has no KVM, so it clears
the vmx flag it is offered.

## Control protocol, version 10

Both ends refuse the other's version at HELLO and ATTACH, before any guest
memory exists. The supervisor, Rust adapter and Firecracker integration must be
deployed together. Each version rejected the one before it:

- Version 10: the arena moved off ATTACH and into files. ATTACH carries no
  descriptor and no length; the pager hands the client each file it may map in a
  FILE frame, and a MAP names the file it maps from. A version 9 peer would read
  ATTACH as the arena and a MAP's file number as a protection flag.
- Version 9: FLUSH was new. A pager ends a session on a control message it does
  not know, so a version 8 pager would end a guest at its first flush.
- Version 8: ATTACH's length became the arena's offset space, not its capacity.
  A version 7 peer would read it as a promise of that much memory.
- Version 7: ATTACH carries the session's page size and the kind of memory its
  arena is made of. A 2 MiB page number read as a 4 KiB page number names a
  different page.
- Version 6: a session carries one memory region, so the `memoryRegion` field
  was removed and the frame is 56 bytes. HELLO carries no page size.
- Earlier, version 4 was rejected: its FLUSH request was removed, and SEAL took
  its frame kind.

A Unix stream carries fixed 56-byte frames of seven little-endian `u64` fields:

```
kind, id, offset, length, backing, generation, flags
```

Frames must be read and written completely; stream boundaries are not message
boundaries. `SCM_RIGHTS` carries exactly one descriptor on HELLO and on FILE,
and none on any other frame. The client reads every frame with `recvmsg`, after
READY too, because a FILE can arrive at any time and a plain read would have the
kernel close its descriptor. The client ends the session on a descriptor with
any other frame, on a FILE without one, and on truncated ancillary data. A pager
refuses a memory region by closing the socket instead of sending ATTACH, usually
because the logical-page cap is full. The client reports this as the pager
closing before attaching, not as a malformed descriptor message.

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

File 0 is the memory region's private file. It is the only file whose descriptor
is read-write, and the only one a writable MAP may name. A shared arena sends one
file, the arena, as file 0, and maps every page from it; a MAP of file 0 encodes
as MAP did in version 9. An [isolated arena](#the-isolated-arena) sends the
region's private file as file 0 and its tenant's shared file as file 1 when the
session attaches. It sends a fork point's file as file 3 or up in the middle of a
session, just before the first MAP that names it, and DROP_FILE when the point's
seal ends. Numbers are the session's own: another session may name the same fork
file by another number.

A session attaches with HELLO, MEMORY_REGION, ATTACH, FILE 0, any other files,
the populate's MAP_BATCH frames and READY. The client refuses READY before it
has file 0. The pager sends FILE and DROP_FILE under the lock it sends commands
with, so they reach the client in order with the MAPs that use them.

For each FILE the client checks:

- that the frame's arena kind is the attachment's, and its length is whole pages
- that file 0 is stated writable and every other file read-only
- that the descriptor is that kind of memory, by `fstatfs`
- that the file is at least the length stated, by `fstat`. Only the stated
  length bounds a MAP, so a file may grow before it is stated again
- that the descriptor is open read-write for file 0 and read-only for every
  other, by `fcntl(F_GETFL)`
- that a repeated number names the same file, by device and inode, at a larger
  length

A FILE that fails any of these ends the session, as does a DROP_FILE of file 0 or
of a file the client does not hold. The client answers `EINVAL`, changing
nothing, to a MAP that names a file it does not hold, reaches past that file's
stated length, or is writable and names any file but file 0. These checks guard
a well-behaved client against a pager bug; the pager's descriptors are what keep
a VMM out.

All batch ranges are validated before any change is made, and must be ordered
and disjoint. The Rust client populates sparse zero page tables before UFFD
registration. The pager installs arena page tables with ranged
`UFFDIO_CONTINUE` and retries the transient `EAGAIN` a concurrent mapping change
causes.

Client request IDs are separate from mapping command IDs, and increase within
each session. The control reader dispatches requests without waiting for their
work, because a seal can itself need revoke acknowledgements. Writes send
complete commands one at a time, including every frame of a batch.

A FLUSH is a request like a SEAL, with an ID from the same sequence, taken under
the lock the client writes with. RESULT answers each FLUSH once. The guest waits
for that answer, but the VMM does not:

1. The device takes a queue drain's flushes off the ring and holds them as one
   request.
2. `Control::start_flush` writes the request and returns.
3. The answer comes back to the VMM thread through an event.
4. The VMM thread completes the held flushes (status, used ring, interrupt) with
   the host's answer.

When a session ends, it answers every waiting flush with EPIPE.

The pager counts each FLUSH in `Stats.Flushes` and passes it, with its memory
region and the `done` function that sends its RESULT, to the callback
`Host.SetFlushed` installed. The callback runs on a goroutine of the session,
never on the reader, because a checkpoint the host takes for the flush needs the
reader for its seal. The host calls `done` once the flush is durable, at most
once: at once if the VM holds no unpublished disk write older than its flush
bound, and otherwise after a disk checkpoint it asks for outside the interval's
schedule ([hosting](hosting.md#the-checkpoint-loop)). With durable flush on,
it calls `done` once the disk's changed blocks are in its journal, and with an
error where they cannot be, which the guest reads as EIO
([capturing for a durable flush](#capturing-for-a-durable-flush)). With no
callback installed, every flush is answered at once. A host drops the flushes
of a VM that leaves it, because an answer would write into guest memory the
destination now owns. The
session ends on a FLUSH on a RAM session, a FLUSH without a fresh ID, or a FLUSH
with another field set.

The device records the flushes it holds in its snapshot (snapshot format version
14), and a device restored from it sends them to its own host when the guest
resumes. A handoff takes the flushes back from the host the guest is leaving. If
the handoff is abandoned, the guest sends them again.

All ranges and backing offsets must be aligned to the page size the attachment
stated, and in bounds. Every command covers whole pages. Generations start at
zero and advance by one for every affected page, and a command that spans several
pages requires all of them at the previous generation. Command IDs increase
across accepted commands. An identical retry of the immediately preceding
successful single-range command only repeats the acknowledgement. Batches are
never retried after an uncertain acknowledgement; the connection becomes
terminal. Stale generations, invalid ranges, overflow and unknown operations are
rejected before any change is made. A syscall failure after a change has begun
is terminal, because a failed `mremap` can leave its destination unmapped. A
pager that does not receive the matching successful acknowledgement must not
free the old backing. Guests cannot access the protocol.

The pager does not trust the VMM. Anything outside the protocol ends that
session and no other:

- A HELLO carries exactly one descriptor, and every field but its version is
  zero. A read of the descriptor that is not a whole fault or remap event ends
  the session, and so does an ioctl it refuses.
- MEMORY_REGION must name the kind and the size the pager was given for the
  session, at an address aligned to its page.
- A fault must be inside the memory region and carry only the flags the kernel
  sets. The pending faults are bounded by `ConnectionConfig.QueuePages`.
- An ACK answers the one command awaiting it, once, with every field but its
  identifier and generation zero.
- SEAL and FLUSH take increasing request identifiers and no other field. At most
  one SEAL and 1,024 FLUSHes wait at a time.
- Descriptors passed with any frame after HELLO are dropped by the kernel,
  because the pager reads those frames without ancillary data.

Such a session's pages go back to the pager, and the host process keeps no
descriptor of it. `vmmemory/hostile_linux_test.go` plays each of these against a
real pager, beside a well-behaved process on the same pager, and
`FuzzHostileSession` plays arbitrary sequences of them. Those sessions send a
made-up descriptor, so they end at the first fault.
`vmmemory/hostile_client_linux_test.go` puts a proxy on a real client's control
socket, so the guest faults its memory in on a real userfaultfd and the test
seals, settles and retires it as a capture does, before the proxy sends one
frame the honest client never would. The pager ends that session and leaves its
neighbour whole. `TestAHostileClientWithARealUFFDIsEndedThroughACapture` plays a
frame each, and `FuzzHostileClient` varies the number of captures and the frame.
A VMM that faults its own memory over and over is paced; see
[repeated faults](#repeated-faults).

`vmmemory/reach_linux_test.go` plays a VMM that uses every descriptor it is
given. In a shared arena it reads another VM's dirty and published pages. In the
[isolated arena](#the-isolated-arena) it finds none of another VM's private
pages, before or after that VM stores and publishes more, and nothing of a VM of
another tenant. It does find the page its own tenant published and another
region of the tenant inherits, which the test asserts. Every way to write its
read-only files fails. The test also runs a helper as another user, as the
embedder's jailer runs a VMM: holding the same files, it cannot reopen a
read-only one for writing through `/proc/self/fd`, and cannot fchmod any of them.
When the VMM allocates memory in its own private file, verification ends its
session with `ErrUncounted`.

A command refused for the mapping budget is the only failure known to have
changed nothing, so the pager treats it as a failed operation, not a failed
session. The client checks a command against the budget before it changes
anything and answers `ENOSPC`, and the pages it would have mapped are not
recorded as mapped. A command whose acknowledgement never arrives may have been
applied, so its pages stay recorded as mapped. After a refusal, the fault fails
and the memory region keeps serving. The budget is the region's own mappings,
so the worker makes room itself: it harvests every page the region maps whose
lock is free, which leaves the client one mapping for the region, and queues
the fault again. The guest's next touch of a harvested page maps it again from
its frame, with no read. A region that maps no page it could take back ends its
session on the refusal, since the fault would only be refused again. The worker
never waits for other work to revoke a mapping: on a host with no memory
pressure none comes, and before 2026-10-09 a guest waited there for good. Any
other errno in a refusal, such as `ESTALE` for a stale generation or `EINVAL`
for a range the client does not have, is the client finding the command wrong,
and it ends the session.

What the bindings say is mapped is the pager's record of page tables it cannot
read, and one place keeps it (`vmmemory/mapper.go` and `revocation.go`). Every
mapping command is sent there, and a run is recorded mapped once its command
has landed, while the caller holds its pages. A refusal records nothing. Any
other failure records the run mapped and ends the region. A revocation records
its pages unmapped once it lands. A store that made a page the region's own
before its writable mapping was refused takes the old read-only mapping away,
so the binding never says writable over a read-only page. A test fails if any
other file writes the record or sends a command. Before 2026-10-08 each path
recorded its runs before sending them and undid the record on a refusal; three
undid the wrong runs, and one of them ended an embedder's VM with
`UFFDIO_CONTINUE: invalid argument`.

Only replacements that install a mapping are charged against the budget, six
each over the region's count, for the mapping they are built in, the splits at
both edges and the reservation they are staged in. A revocation returns its range
to the trap mapping the region was attached as, which merges with the traps
around it. It is admitted regardless of the budget, as is a batch of revocations
of any size, because a refused mapping is answered with revocations and nothing
else frees the budget. One in the middle of a file's run splits it in three; that
and a revocation's transient cost are what the kernel headroom the limit leaves
covers, so a limit above half of `/proc/sys/vm/max_map_count` is refused at
attachment.

A batch split across several commands is the exception. Once one of its commands
has landed, a refusal is as ambiguous as a lost acknowledgement, and terminal.
`Stats.RefusedMappings` counts the deferred faults. A host that refuses has given
its client a budget too small for the mappings its guest's access pattern
creates.

### Repeated faults

A VMM can drop the page tables of its own memory with `MADV_DONTNEED` and read
the memory back. Each read traps, and the pager installs the same page tables
again. This is a repeated fault: one whose memory region already maps the page
for the access (mapped or zero-mapped for a read, mapped writable for a store).
Every other fault loads, maps or copies a page, which the budgets bound. A guest
meets a repeated fault only when something outside the pager took its page
tables away, such as the kernel moving a page.

Each session's repeated faults are paced: 1,024 at once, and 1,024 a second after
that. A repeated fault past that budget waits in its session's fault worker,
costing the pager no work and holding up no other session. Pacing never ends a
session. When two vCPUs fault one page at once, the second fault is the first's
twin, and the session is not charged for it. Only a fault that changes something
has a free twin.

`vmmemory/repeats.go` holds the budget and `vmmemory/faultqueue.go` the twins.
`Stats.RepeatedFaults` counts the repeated faults, and `Stats.PacedFaults` those
that waited for the budget.

Measured on the aarch64 Lima instance on 2026-09-26, one run each. A VMM with
64 fault workers and 128 threads dropped and read back 256 pages of 4 KiB over
and over, beside the hostile tests' well-behaved process, which stored into a
page and checkpointed it 400 times:

| pacing | the VMM's repeated faults | neighbour's median store, alone and beside | 90th percentile, alone and beside |
| --- | --- | --- | --- |
| off | 76,000 a second | 0.33 ms, 0.76 ms | 0.47 ms, 1.78 ms |
| on | 1,427 in 0.4 s | 0.28 ms, 0.28 ms | 0.37 ms, 0.41 ms |

Unpaced, the VMM took three and a half of the instance's eight processors, and
in one run the neighbour's slowest store took 289 ms.
`TestAVMMRepeatingItsFaultsIsPacedAndItsNeighbourKeepsItsLatency` holds the VMM
to its budget and the neighbour's median store to 1.5 times its time alone.
`TestThreadsMeetingOnAPageRepeatNoFaultFree` holds threads racing for the same
pages to the budget and one free twin per page brought in. The fuzzing cannot
test this, because its descriptors are not real userfaultfds.

## Idle pages

When the last memory region that maps a published page goes away, the page stays
in the arena, idle, and the next memory region that inherits the identity maps it
without reading the volume. So a seeded guest that is checkpointed and stopped
leaves its memory for the forks of that checkpoint. A private page is released
with its memory region.

An idle page is the first memory given up, and never a reason to wait. An
allocation that needs a slot takes the oldest idle page before it evicts
anything a memory region maps, and write-ahead and prefetch give up idle pages
for free slots. Idle pages also act as the cache of the host budget, so the other
pager and the checkpoint cache take them before they wait. A memory region that
finds no free extent gives up the idle pages of an extent whose memory region has
gone. `DropIdle` gives up all idle pages at once. `Stats.IdlePages` is the number
of idle pages, and `Stats.IdleDrops` the number given up.

An idle page chosen under the host's lock is looked at again under its own
lock before it is given up (`evictIfIdle`): a settle may hand it back to a
region in between, and an eviction's look ages a mapped page as readily as an
idle one.

## Eviction ordering

1. Hold the page transition so no new alias can be installed.
2. Revoke every mapped alias and wait for each matching acknowledgement.
3. Read the now-stable contents of private backing and write the spill file.
4. Punch the arena range and make its slot reusable.
5. Let queued faults reload and remap the current page identity.

A page whose backing can be reconstructed writes no spill data. If a spill
fails, the resident slot is kept, so a later fault can map it again. No slot is
reused only because a revoke was sent.

Step 2 fails when a memory region's session has stopped answering, for example
because its machine died. The revocation makes that memory region terminal, and
its pages stay mapped there until it is closed and are excluded from every later
pass. The eviction takes another victim instead of failing the machine that
needed the page, which may share pages with others. If the arena holds only such
pages, it reports capacity exhaustion.

An eviction of a memory region's own page holds that region live, shared, from
before it reads the page's reservation until the page is gone, so a detach
cannot give the reservation back and destroy the layer under it. One that
cannot take the hold leaves the victim, and a detach marks its region
detaching first so the next look passes over the region's own pages.

### Choosing the victim

The victim is the least recently used page that leaves every protected memory
region its pages. The pager sees a guest's faults and none of its other
accesses. Alone, that order lets a guest that cycles through more memory than the
arena holds evict its neighbours' working sets on every fault.

The order is kept by Zircon's page queues, ported in `internal/zirconvm`. A
resident page, clean or dirty, is in a reclaim queue by age, or in an isolate
queue once it has aged out of them. The queues age one generation for each fault
served. An idle page is in the don't-need queue, which is taken first. A page a
cold copy will be compared with is in the zero-fork queue, which is taken last.

Zircon also ages pages by the accessed bits of their page tables, which a
userfaultfd pager cannot read. A guest reads a page it maps without a fault, so
fault order alone ranks a page it reads all the time, as a DAX root's reads are,
below every page written once since. The evictor harvests instead
(`vmmemory/harvest.go`). Each step first takes the mappings of a few of the
oldest isolated pages away, keeps the pages, and moves them to the harvested
isolate queue. It keeps a quarter of the arena harvested ahead of its victims,
at most 64 pages a step. The guest's next access to a harvested page faults; the
fault maps the page again from its frame, with no read, and marks it accessed,
which moves it out of the isolate queues. A harvested page the evictor reaches
was not touched while a quarter of the arena was evicted ahead of it. A store
trap on a page the guest read until a harvest is served as a load: the page is
mapped read-only again, and a real store traps on that mapping and copies, so a
read KVM's worker asks for writable makes no cold copy.

On GCE (n2-standard-8, nested KVM, 2026-10-08), a guest on a 3 GiB DAX root
over a 256 MiB PMEM arena of 2 MiB pages wrote 2 GiB at 64 MiB a second while it
read one file at random, a 4 KiB `O_DIRECT` read every 2 ms
(`TestWhatAGuestReadsOfItsDAXRootWhileItWrites`). The reads during the 32 s of
writes, before the harvest and with it:

| file read | run | p50 | p90 | p99 | spill refaults | evictions |
|---|---|---|---|---|---|---|
| 32 MiB | before | 3 µs | 5 µs | 2,417 µs | 169 | 1,162 |
| 32 MiB | harvest | 4 µs | 7 µs | 430 µs | 13 | 997 |
| 128 MiB | before | 4 µs | 14 µs | 2,971 µs | 967 | 2,013 |
| 128 MiB | harvest | 9 µs | 315 µs | 560 µs | 54 | 1,086 |

Fault order loses a page the guest reads all the time once per turnover of the
arena, so before the harvest the cost was a slow read now and then: the p99,
and a refault of a whole page from the spill each time. The harvest keeps
those pages, and the p99 falls about five times. What it costs is a fault per
harvested page the guest touches again: 1,568 in the 128 MiB run, about
300 µs each under nested KVM, which is its p90. A page the guest reads less
often than once a turnover is lost either way; for it, what a refault reads,
a whole 2 MiB page, is the lever (TASK-110).

An allocation short of a slot evicts by the synchronous path of Zircon's
evictor (`internal/zirconvm/evictor.go`), which calls the pager's reclaim step
(`vmmemory/evict.go`) until a page is freed or the step finds nothing. A step
takes a batch of victims, 2 MiB of them or a sixteenth of the arena whichever
is fewer (512 at 4 KiB, one at 2 MiB), and evicts them together: one
revocation pass over the regions they are mapped in and one spill write. Before
2026-10-09 a step took one, and a 4 KiB guest writing through a full arena
faulted once a page, each fault a revocation round trip and a 4 KiB write
(TASK-122.7). Each victim is, in order: the oldest idle page; the slots of running
prefetches, by cancelling them; then, after it harvests, the oldest harvested
page the fair share lets it take, or the oldest isolated page it has not
harvested; the oldest page of all; and last a page a cold copy will be compared
with. It then gives a cold copy back to its origin, drops a
published page, or spills a memory region's own page. A page the fair share
protects, or whose lock another holds, is left where it is rather than moved to
the newest queue as Zircon's evictor does. A step that finds nothing older ages
the queues itself. Nothing evicts ahead of a shortage, so the evictor's
asynchronous path is not used.

Each attached memory region is owed a share: the arena's pages divided by the
memory regions attached. A memory region is protected while it holds no more than
its share and has asked for a page within the last turnover, which is as many
evictions of mapped pages as the arena has pages. An eviction for one memory
region takes no page another protected region maps. Where every candidate is
protected, it takes the least recently used page of all, so an allocation
never waits on the rule.

A guest whose working set is resident asks for nothing, so it loses its
protection after a turnover, gives up its least recently used page, and is
protected again at its next fault. So a hog evicts its own pages, and costs a
neighbour within its share about one refault a turnover. An idle guest's pages
are anyone's to take.

Each memory region counts the resident pages it maps, once per page, under the
host lock, so the rule reads it without walking anything.

The rule helps a neighbour only as far as its share holds its working set. On
the aarch64 Lima instance, two 128 MiB guests at 2 MiB pages, one storing into
80 MiB over and over:

| RAM arena | share | neighbour, rule off | neighbour, rule on |
| --- | --- | --- | --- |
| 64 MiB | 16 pages | `echo` through the agent past 30 s | `echo` through the agent past 30 s |
| 96 MiB | 24 pages | slowest `echo` 96 ms, 1,016 evictions | slowest `echo` 32 ms, 590 evictions |

A booted guest of this image holds 21 dirty 2 MiB pages, so a share of 16 is
below its working set. Each figure is one run.

## Capture, fork and restore

A capture is the checkpoint's pause. The prepare step pauses the vCPUs, drains
device completions, saves device and register state and seals every memory
region, returning each region's sealed set under the name of the volume it maps.
The resume step restarts the vCPUs while the regions stay sealed. The
publication then writes the sealed pages with the captured VMM state, and
retires the seals when it lands. The pause happens under the VM's publication
lock, so an explicit capture and the interval checkpoint do not race. If a phase
fails before the publication starts, every memory region is unsealed and the
guest resumes.

The caller cannot cancel any of these operations. Pause, snapshot create, resume
and release run on a context owned by `vmmachine`, bounded by its own timeout;
the caller's context bounds only the wait for the process lock. A control request
abandoned halfway leaves the VMM's state unknown, and the only response is to
kill the process, losing every write since the last checkpoint. So an HTTP
client that disconnects, a drain whose deadline passed or a migration that gave
up must not reach the VMM as a cancelled request. A request the VMM refuses
fails the checkpoint and leaves the guest unchanged.

The state file that the capture stages is never made durable or synced. It is
read back and removed before the guest resumes, and a host that restarts wipes
its directory. Once the publication owns the sealed sets, no other step unseals
them.

A fork publishes no checkpoint of the parent. `host.Seal` takes the same pause
and returns a `volume.ForkPoint`, which contains the checkpoint the parent's
control record already selects, pinned there, and the pages the seal froze. Any
number of children can start from one fork point, and each holds it once, so a
fan-out costs the parent one pause. The parent stays sealed, and is not
checkpointed, until the last hold retires. On the parent's host, a child reads
the sealed pages through the point and shares them in the same pager. On another
host, the child's pager pulls them from the parent's peer server, as a migration
destination does. The child's first checkpoint publishes them as its own; until
it lands, opening the child on any host reports `volume.ErrForkPending`.

A host that never held the parent rebuilds the point from the pinned checkpoint
alone, as a fork of a template does. A host restores a VM it never ran by reading
the VMM state of the published checkpoint, with exact managed PMEM overrides. It
never loads a full RAM image file.

Every VM a host runs is checkpointed on `host.Config.CheckpointInterval`, sixty
seconds by default, measured from the completion of the previous publication and
jittered by up to an eighth either way. This interval is the only thing that
makes a running guest durable. Guest PMEM stores, guest RAM stores, live
registers and local scratch spill are all volatile until a checkpoint includes
them. Losing the host rewinds the VM to its last checkpoint.

## A new generation and the right clock

Every child of one fork point resumes from the same guest memory, so its
kernel's random pool, every seed its programs drew and everything derived from
them are the same in every child. A restore of a checkpoint repeats them too.
Each restore therefore gives the guest a new generation ID, from which the
guest's kernel reseeds its random pool. A restore also moves the guest's clock on
by how long its state was stopped.

### The devices a VM gets

Firecracker gives every VM two devices for this, on both architectures, and
nothing turns them off (`builder.rs`, `attach_vmgenid_device` and
`attach_vmclock_device`):

- **VMGenID.** A 128-bit random generation ID in a 16-byte buffer of guest
  memory, with an interrupt. On x86_64 it is the ACPI device `VMGENCTR` in the
  DSDT. On aarch64 it is the device tree node `microsoft,vmgenid`.
- **VMClock.** A page of guest memory with a disruption marker and a VM
  generation counter, with an interrupt. On x86_64 it is the ACPI device
  `AMZNC10C`, and on aarch64 the node `amazon,vmclock`. Firecracker fills in no
  clock data, so it says only that a restore happened.

Every restore is a snapshot load: a fork's child, an open of a VM from its
checkpoint, a VM created from a template or a kept checkpoint, and a migration's
destination (`vmmachine.loadRequest`). On each load Firecracker draws a new
generation ID from the host's random source, writes it into the guest's memory
and raises the interrupt (`devices/acpi/vmgenid.rs`,
`device_manager/persist.rs`). It also adds one to VMClock's disruption marker
and generation counter (`devices/acpi/vmclock.rs`). Both writes are stores of
the VMM into guest RAM, which copy on write like any other (see
[writers that bypass the page tables](#writers-that-bypass-the-page-tables)).

A migration takes a new generation too, although it continues one guest, because
a handoff's state can run twice: a receive tried again after a destination ran
the guest runs it again elsewhere, and an abandoned migration resumes its source
after its destination may have run. A spurious new generation costs the guest
one reseed.

### What the guest kernel must have

- The VMGenID driver built in: `CONFIG_VMGENID=y`. A module would load too late
  for a guest whose init loads none. On a new generation the driver reseeds the
  kernel's random pool (`add_vmfork_randomness`), logs `crng reseeded due to
  virtual machine fork` and sends a `NEW_VMGENID=1` uevent.
- On x86_64, ACPI: the boot arguments must not carry `acpi=off` or `acpi=ht`.
  On aarch64 the driver reads the device tree, which Linux supports from 6.10.
- Optionally the VMClock driver, `CONFIG_PTP_1588_CLOCK_VMCLOCK`, which serves
  the VMClock page as `/dev/vmclock0`.

The pinned guest kernel, Firecracker's CI build of Linux 6.18.44, has all of them
built in on both architectures
(`third_party/firecracker/resources/guest_configs/microvm-kernel-ci-*-6.18.config`).

A host refuses to start (`vmmachine.CheckGuest`, `ErrGuestDevices`) when its
kernel has no VMGenID driver built in, or when its boot arguments hide the device
on x86_64. The check looks in the kernel image for the name the driver matches
the device by; Firecracker loads only uncompressed kernels, so the name is there
in plain bytes. A host also refuses a VMM of another API revision; revision 2
pairs the guest's clock with the wall clock (below).

### The reseed and its window

The interrupt is raised before the vCPUs resume, so the guest takes it first. On
x86_64 the kernel then runs the ACPI interpreter and the driver on its worker
threads, and the driver reseeds the pool. On aarch64 the driver reseeds in the
interrupt handler. So on x86_64 there is a window: a read of `/dev/urandom` or
`getrandom()` that runs before those workers returns bytes of the pool the state
was captured with, and two restores of one state that both read in it draw the
same bytes. The Firecracker tests ask each guest over its console as soon as it
runs, and measure when the reseed is seen
([measurement](measurements/gce-generation-2026-10-04.md)). All 18 of a fork's
children had reseeded by their first answer. Two of nine guests restored from a
checkpoint had not: their first answer came before the reseed, and their next,
20 ms later, after it. Every reseed was seen within 381 ms of the guest's
release, most of it the guest faulting its memory back.

### What a guest's programs must do themselves

The kernel reseeds only its own pool. A program that calls `getrandom()` or reads
`/dev/urandom` each time it needs bytes is safe once the reseed has run. A
program that drew bytes before the fork point and keeps them is not:

- a userspace random generator seeded once, such as a language runtime's seeded
  PRNG or a library's own DRBG;
- keys, nonces, counters and session tickets made ahead of time;
- UUIDs and node IDs derived from a stored seed.

Such a program must draw again on a new generation. It can wait for the
`NEW_VMGENID=1` uevent, which the driver sends after it has reseeded. Or it can
read VMClock's generation counter in `/dev/vmclock0`, which the VMM changes
before the guest resumes. A program that sees the counter change before the
uevent arrives is inside the window above and must wait for the uevent before it
draws. Nothing on the host can do this for it.

### The clock

On x86_64 the guest's clocks move on across a restore by the host's wall time
since the state was captured. The host asks for this on every load
(`clock_realtime`). The fork pairs the guest's kvmclock with the host's wall
clock when it saves the state; KVM does this itself only on a host whose own
clocksource is the TSC, so a nested host on kvm-clock would otherwise get no
pairing. On load the fork moves kvmclock on through KVM, and moves the guest's
TSC on by the same time at the guest's TSC frequency. A Linux guest prefers the
TSC as its clocksource where the TSC is invariant, and then reads its time from
the TSC alone. So from its first instruction a restored guest's wall clock is as
far from the host's as it was when its state was captured. Its monotonic clock
jumps forward by the stop as well, as when a VM is descheduled for that long. The
restore tells the guest's watchdogs to expect the jump (`KVM_KVMCLOCK_CTRL`).

A restore adds no error beyond the microseconds between two reads of the host's
clock, plus, across hosts, the hosts' disagreement, which NTP keeps well under a
millisecond. On the qualification host a guest booted 25 to 46 ms behind the
host, and every restore kept that offset to within the console's round trip
([measurement](measurements/gce-generation-2026-10-04.md)). Without moving the
TSC on, a fork's children were half a second to a second behind their parent. A
guest that needs a better clock, or one that runs for days, runs a time daemon
over the KVM PTP clock (`CONFIG_PTP_1588_CLOCK_KVM`, `/dev/ptp0`).

On aarch64 Firecracker cannot move the clock on, and a restored guest's clock
resumes where its state stopped it. vmmachine logs a warning at each restore
there. The guest must set its own wall clock, for example from the host over its
agent.

## Live migration

The pager has two jobs in [live migration](migration.md): serving pages to the
destination, and giving up the volume. Migration is post-copy only.

`MemoryRegion.ReadResident` copies one page for a peer. If this host does not
hold the page, it says so, and the destination reads from the volume instead. It
also reports whether the page is this memory region's own state rather than the
volume's. It never loads, so a destination's fault never becomes a volume read on
the source. "Held" means the page is in host memory or is private state of this
host. "Unpublished" means no checkpoint has it. `Resident` lists the pages this
host holds, and `MemoryRegion.Unpublished` the subset no checkpoint has. Both are
snapshots.

On the destination, a load is not an install. A backing that reports pages as
unpublished (`vmmemory.UnpublishedLoader`) fills a buffer, but on a full dirty
budget the pager drops a read-ahead page rather than fail the fault that carried
it. A backing that also implements `UnpublishedInstaller` is told, after the
load's pages are bound, which of them this memory region now holds as dirty
state. Only those may be removed from the set the destination still needs from
the source. A page the pager dropped is requested again. A read that cannot get
it fails; it does not fall back to the volume, whose bytes are older.

`MemoryRegion.Handoff` gives up the volume but keeps the pages. It must run after
the guest is stopped. Verification stops checking authority this host is about
to release. A seal, a population or any fault then reports `ErrHandedOff`.
Serving continues until `Detach` releases the pages. A sealed memory region
cannot be handed off; the handoff reports `ErrSealed`.

`Process.MemoryRegions` names every memory region by the volume it maps: `ram0`,
and one per PMEM device id. The destination opens the same volumes under these
names. `Process.Stop` pauses the vCPUs, drains device completions and returns the
VMM state, leaving the VM paused. It seals nothing and waits for nothing, because
the destination fetches the pages it leaves behind. `Prepare`, by contrast,
seals for a capture the same VM resumes from. A migration abandoned after `Stop`
can still be released, which resumes the guest.

## Firecracker build and process lifecycle

The integration is a commit on the `sproutfs` branch of the
[fork](https://github.com/semistrict/firecracker.git), and the gitlink pins it:

```sh
git submodule update --init third_party/firecracker
```

Edit source inside the submodule and commit it on the fork's `sproutfs` branch.
The integration covers the feature, API schema, mapping owners, PMEM worker,
snapshot and restore paths, and the seccomp policy source. Since version 13, the
snapshot format records managed backing, so an ordinary memory-file snapshot
cannot capture a managed VM, and a build without the feature rejects managed
configuration. Version 14 records a managed PMEM device's waiting flushes, so
VMM state an older build captured is refused on restore. A managed capture is
always a full snapshot with no memory file. A managed restore requires fixed RAM
with no huge-page setting, in this architecture's own layout.

The fork has an API revision, `SPROUTFS_API_REVISION`, which it prints when run
with `--sproutfs-api-revision`. It covers the fields of the boot configuration,
snapshot load and snapshot create that upstream Firecracker lacks.
`vmmachine.APIRevision` is the revision the host speaks, and a host refuses to
start unless its Starter's VMM reports the same one. A VMM that lacks a field the
host sends still boots, but fails at a checkpoint or a stop and loses every write
since its last checkpoint. Raise both revisions together whenever the host starts
sending a field, or relying on a behaviour, that the previous revision lacks.

The fork must be rebuilt for mapping protocol version 10. Its seccomp policy lets
the memory thread read frames with `recvmsg` and check a file it is handed
mid-session with `fstat`, `fstatfs` and `fcntl(F_GETFL)`, and lets the VMM thread
check a file's access mode and map a read-only file's runs
`MAP_PRIVATE | MAP_NORESERVE`. The crate is vendored into the VMM by path, so a
cached qualification build keeps speaking an older version, and every session it
opens fails with `invalid managed-memory hello`. When a Lima or GCE run that used
to pass stops attaching, rebuild the VMM and do not reuse
`~/.cache/sproutfs-fanout`.

Starting a machine requires the shared pager, the VM whose volumes it maps, a
feature-enabled binary and an explicit compiled seccomp policy, including the
VMM's memory-worker filter. RAM binds to the volume `ram0`, whose size must be a
multiple of the RAM pager's page size. Each PMEM device binds to the volume named
by its device ID, whose size must be a multiple of 2 MiB. Cold boot also supplies
a kernel, an optional initrd and boot arguments. A root PMEM device boots
directly. The qualification guest uses ext4 with `dax=always`.

The guest's physical address space has holes for MMIO, which the volume does not
contain. On x86_64, the hole from 3 GiB to 4 GiB splits the RAM of a VM with more
than 3 GiB into two memory regions, and a second hole at 256 GiB splits a larger
VM's RAM again. The VMM maps the volume's byte range onto the guest's memory
regions in ascending guest order, so on x86_64 volume offset 3 GiB + x is guest
address 4 GiB + x on every path. On aarch64, RAM below 256 GiB is one memory
region. The split follows Firecracker's architectural layout for the memory
size, and a restore whose snapshot records any other layout is refused. Only the
VMM knows about the holes.

The supervisor creates private Unix sockets and checks the peer credentials
against its child process. Attachments initialize concurrently. A managed PMEM
device holds a guest flush, asks the host over that disk's memory session, and
completes it on the VMM thread when the answer arrives, while the VMM thread
keeps serving every other queue and device. A timeout, pager loss or authority
failure terminates the VMM. Failed starts and shutdown remove private sockets
and state files, detach logical pages and release arena allocation. Console
output is held in memory and is lost with the process. Close the process before
closing the volumes, the spill file or the arena.

## Qualification

The [2 MiB HugeTLB qualification](measurements/hugetlb-2026-09-11.md) records the
current x86_64 execution, performance comparison and pool cleanup.

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

`scripts/test-vm-memory-gce.sh all` runs the vmmemory suite's Linux tests the
same way on a disposable GCE VM, with the crate's example client as the VMM and
a 2 MiB pool of `SPROUTFS_VM_MEMORY_HUGEPAGES` pages, and deletes the VM on
exit. `SPROUTFS_VM_MEMORY_RUN` selects tests by `-test.run` pattern.

Every suite builds its pagers in the arena mode `SPROUTFS_ARENA` names,
`isolated` when it is unset. `just check` runs the Go suites of the packages
whose pagers take the mode again under `SPROUTFS_ARENA=shared`, and the
qualification scripts pass it through. A test of what one mode does pins that
mode. In the isolated mode the simulated arena (`internal/testpager`) checks
that a file given writable is given to one memory region only, as its file 0,
and to nobody read-only, and that every map names a file its session holds,
writable only for file 0. So every campaign checks the split under forks,
migrations, eviction and spill.

Use `SPROUTFS_LIMA_INSTANCE` to select an existing instance. The host needs Go,
Cargo and `limactl`, plus Python 3 for the full-guest suite. The guest needs
Cargo, Clippy, a source mount, KVM and kernel-fault UFFD support, and for the
full-guest suite a C compiler with static libc, libseccomp, curl and e2fsprogs.
The test process runs through `sudo -n` for UFFD, KVM and physical-page
inspection. Neither script changes device permissions, sysctls or the VM
configuration, and both remove their temporary artifacts on exit. The ordinary Go
suite skips these tests unless `SPROUTFS_VM_MEMORY_CLIENT` names the built Rust
adapter. When it is set, a missing capability or inaccessible physical-page
information is an error. `SPROUTFS_PAGER_MEASURE` and `SPROUTFS_FRAGMENT_MIB`
enable the opt-in measurement runs.

`SPROUTFS_FIRECRACKER_RESIDENT_PAGES` sets the full-guest resident budget in
pages of 2 MiB. It defaults to 48 pages (96 MiB), and each pager's arena is that
many bytes. The source and the fork each dirty 48 MiB of guest RAM, then verify
markers in every 4 KiB subpage, which forces eviction, spill and refault, and
the qualification requires that all three are observed. 32 MiB is below this
fixture's working set with 2 MiB pages, and 64 MiB thrashes when both forks run.

The memory suite uses a PMEM pager's 2 MiB page throughout, and one 4 KiB RAM
pager in `small_page_linux_test.go`. The full-guest suites run the production
pair: a 2 MiB RAM pager and a 2 MiB PMEM pager over the pool, or with
`SPROUTFS_RAM_PAGE_BYTES=4096` a 4 KiB RAM pager over an ordinary memfd.
Migration requests default to one 2 MiB page, with an 8 MiB per-peer in-flight
byte budget. A reply is counted in the page size of the volume it answers for.

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
- two restores of an unpublished point plus a nested fork after private writes,
  with every resident page mapped during connect, and no load and no fault on
  the first KVM reads
- a seal of a run of pages whose slots descend, as one range write-protect
  covering several of the client's mappings; the guest keeps reading without a
  fault, and the next store still traps and copies
- what the isolated arena needs of read-only files, in `readonly_linux_test.go`,
  which plays both the pager and the VMM without the Rust client (step 1 of
  [the plan](../plans/isolated-arena-2026-09-25.md#steps))
- the same through the Rust client, in `files_linux_test.go`. The sharing,
  eviction, spill and KVM cases run over a layout with a second, read-only file.
  A page of that file is the pager's own page in the client, its private mapping
  reserves no HugeTLB pool page, and a store from a thread or from KVM traps to
  the pager and leaves the file unchanged. The client refuses a writable MAP of
  the file, a MAP of a file it was never given and a MAP past the file, and a
  DROP_FILE closes its descriptor.

The simulated pager tests in `vmmemory/isolation_test.go` and
`vmmemory/tenant_test.go` require of an isolated arena:

- A page two regions of a tenant inherit is in the tenant's shared file, which
  each maps as file 1. A store copies it into the storing region's file 0, at its
  own offset.
- A region of another tenant reads the same bytes from a shared file of its own.
- A published page moves into its tenant's shared file when another region
  inherits it. The inheritor reads nothing from its volume, the owner's mapping
  of its private page is revoked, and the private slot goes back.
- A published page whose VMM changed it through its private file ends that
  session with `ErrTampered`, and the inheritor reads its volume.
- A fork point's page is copied into the point's file once, and two children map
  that copy as file 3 or up. Ending the seal revokes their mappings, drops the file
  from both and gives it back.
- A detached region's private file lasts as long as its idle pages, and so does
  a tenant's shared file once the tenant's last region has detached.
- A fault on a page of another tenant fails with `ErrOtherTenant`.
- Verification ends a region whose private file holds a page the pager never put
  there, and punches such a page out of the tenant's shared file.

They require of seals:

- A seal returns before anything reads the sealed set.
- A store into a sealed page runs at once and leaves the published bytes
  unchanged.
- The sealed set survives reclaim and refault of its pages.
- A sealed page refaulted from spill shares its memory again, so retirement
  leaves no private page that no reservation covers.
- An over-budget store waits for the publication instead of failing.
- An abandoned seal keeps every page for the next seal.
- Retiring a page, against a concurrent eviction, never leaves its private page
  reachable from a binding that owns neither a reservation nor a seal.

They also cover a store into a page an abandoned seal handed back, a seal
cancelled partway, and an abandon racing another volume's faults, under the race
detector.

They require of the settle:

- A memory region that shares pages with a sibling and takes one page writable
  without storing publishes no page. The page ends up shared again, with its
  private bytes zero, its reservation returned and its mapping of the copy gone.
  A read of it takes one fault onto the sibling's page.
- A page the guest really stored into is published.
- A store between the seal and the settle leaves the guest its own copy for the
  next checkpoint, and this checkpoint publishes nothing.
- These copies are published unchanged: one whose origin was evicted, one made
  from zeros, one made from the checkpoint's own held copy, one made from the
  name a fork point lent a page, and one made from a page only another host
  holds.
- A memory region whose only private pages were unchanged lets a store that
  waits on the loss window proceed.
- One worker and sixteen workers leave the same set.

For migration, they require that a seal issues one range protection per run and
no mapping command. The peer server returns a held page's current bytes (the
sealed copy for a page still sharing one, the guest's own copy for a page stored
into since the seal), reports which served pages no checkpoint has, and reports a
never-touched page, and a page reclaimed since it was listed, as absent without
reading the volume. A destination's pager holds every peer-served unpublished
page as private dirty state, publishes it in its next checkpoint, and at a full
dirty budget waits rather than failing the post-copy read. Against a volume on
which every call fails, as a handed-off host's does, verification, listing and
serving continue, a seal or a fault reports `ErrHandedOff`, and detaching
releases every resident page, reservation and logical page.

The full-guest suite builds the feature-enabled VMM and the restrictive aarch64
seccomp policy. It downloads a pinned official Firecracker CI kernel with a
recorded checksum, creates an ext4 PMEM root containing a static guest workload,
and boots without an initrd or a memory file, on real KVM and real TCP over
loopback; object storage is simulated. The guest verifies actual DAX with
`statx`, performs mapped stores, `msync` and `fsync`, and reports its extent, so
the host can check acknowledged bytes before the capture. The test then:

1. captures known disk and RAM values and changes the source;
2. restores a fork at the original values;
3. verifies isolated fork writes and shared physical pages;
4. replaces the fork's PMEM writer and requires the stale VMM to exit;
5. migrates the source away, naming both memory regions by their volumes, and
   requires that the stop seals nothing and that the stopped source still
   serves the pages it lists.

A failed start and the final shutdown check private-file removal, zero logical,
dirty and resident pages, and zero memfd blocks.

The suite also runs the fan-out: one fork point and two children, received onto a
second pager and peer server one after the other, as the orchestrator does. Each
child's root index is published and its hold on the parent released before the
next child starts. Both guests then read every page of their memory and their
whole root volume at once, while both are checkpointed on an interval. The two
children share every per-peer budget. The destination's RAM arena is a quarter
of the memory the two children map, so the reads involve eviction, spill and
refault; its PMEM arena holds both roots. Both children must answer. At 4 KiB the
phase takes minutes, because under a quarter-sized arena every page of a scan
takes its own fault; its time bound is sized for this.

The suite also runs hostile neighbours, in `neighbours_linux_test.go`. Each test
runs two guests on one real `host.Host` over one pair of pagers. One guest runs
a load from the guest init's `hog` command, and the other must complete a RAM
store, a synced DAX store, both read back, and an exec through its agent, each
within 30 seconds. Every test then checks that no pager's peak dirty or resident
pages passed its budget or its arena. The loads are:

- `hog ram`, a guest storing into all of its RAM over and over, on a RAM arena
  three eighths of what the two guests map. The neighbour's working set must
  fault back in whole, its agent must answer, and nothing is stopped.
- `hog disk` and then `hog ram`, with the interval an hour away. The disk hog
  runs past the PMEM dirty budget and must be checkpointed out of turn. The RAM
  hog runs past the RAM dirty budget, which nothing relieves, and must be
  stopped with the logged reason; the neighbour must not.
- A small `hog disk` past a two-second loss window, which only checkpoints out of
  turn relieve; see the loss window in [the bounded host pager](#bounded-host-pager).
- `hog sync`, an fsync loop. It may cost at most one checkpoint of the storming
  guest per flush bound beside the interval's own, and the neighbour's flushes
  must complete.
- `hog vsock`, a guest connecting to the host over and over where nothing
  listens. The host's exec must still reach that guest's agent.

Recorded on 2026-09-10 on the Lima aarch64 instance, as observations of small
workloads:

- A second machine restored from a point its sibling had already loaded, with two
  memory regions of 512 pages, mapped all 1,024 pages with 2 commands. It loaded
  nothing and took no fault.
- The full-guest fork of a machine with 128 MiB RAM and 64 MiB PMEM mapped 47,600
  pages with 4 commands before resume. It loaded 15 pages and took 3 faults.
- The capture of 8,495 dirty pages paused the guest for 144 ms: pausing the
  vCPUs, saving the VMM state and sealing took 142 ms, and resuming 2 ms. Moving
  the sealed pages to storage took a further 373 ms while the guest ran, and
  selecting the checkpoint 0.4 ms.
- Migrating the same source away took a stop pause of 7 ms. The stopped source
  then listed 7,599 resident RAM pages for its destination to stream.

Seal cost is measured with `SPROUTFS_PAGER_MEASURE=1`. The measured guest dirties
8,192 or 200,000 pages, either contiguously or with no two dirty pages adjacent.
Recorded on 2026-09-10 on the Lima aarch64 instance, before and after per-run
mapping replacement was replaced by range write-protection:

| dirty pages | runs | commands before / after | seal before | seal after |
| --- | --- | --- | --- | --- |
| 8,192 contiguous | 8 | 8 maps / 8 protects | 5.4 ms, 654 ns/page | 3.1–3.4 ms, 0.4 µs/page |
| 8,192 scattered | 8,192 | 8 maps / 8,192 protects | 610 ms, 74.4 µs/page | 12–14 ms, 1.5–1.7 µs/page |
| 200,000 contiguous | 196 | 196 maps / 196 protects | 189 ms, 943 ns/page | 122–185 ms, 0.6–0.9 µs/page |
| 200,000 scattered | 200,000 | 196 maps / 200,000 protects | 20.4 s, 102 µs/page | 338–367 ms, 1.7–1.8 µs/page |

Each figure is one run of a single-threaded measurement on a shared laptop VM.
The "after" column shows the spread of two runs, and the contiguous rows vary by
another half as much between runs. A run is limited by the seal's page-lock batch
of 1,024, so 200,000 contiguous pages take 196 commands. The old replacement made
each run a new mapping whose page tables were installed again, which cost the
20 seconds. The remaining cost, under a microsecond per page in the contiguous
cases, is the per-page bookkeeping of the sealed set. A real guest is closer to
the contiguous shape: the full-guest seal of 7,827 mapped dirty pages took 263
range protections, about thirty pages each.

Smaller pager pages were measured on 2026-09-11 on the same instance, before the
2 MiB page was fixed: boot, pnpm-install, a capture and a fan-out of four forks
at 4, 16 and 64 KiB ([records](measurements/page-unit-2026-09-11.json)). Not yet
qualified: other kernels and architectures, jailer namespaces and cgroups,
scheduling fairness across many guests under memory and I/O pressure, and real
build or filesystem workloads with HugeTLB backing.
