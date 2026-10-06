# A Go port of Zircon's page layer — 2026-10-05

**Status, 2026-10-06:** steps 1 to 12 are done. The owner kept the ported core
without the step 13 GCE measurements, and the old core, the switch and the
second test pass are deleted (step 14, main 0c0f3682). What follows is the plan
as written.

The owner has decided to replace the page layer of the pager (`vmmemory`, about
35,000 lines with its tests) with a Go port of the page layer of Zircon's VM.
The port copies Zircon's code. It does not redesign it. This plan says what
comes from where, what stays ours and why, how the tests move, and the steps.
It changes no code. The work is TASK-92 and its subtasks.

Every Zircon reference is to `~/src/fuchsia` at revision
`90e54e090540aaac37a81494884f4b8a68bc269c`, under `zircon/kernel/`. The C++ in
`vm/*.cc` is the implementation that is ported. The `.rs` files are a partial
Rust migration and are not used. FreeBSD's `sys/vm` (`~/src/freebsd-src`) is
cited for comparison only. Its files are BSD-licensed, some of them under the
four-clause licence, which is not OSI-approved, so nothing of it is copied.

## Why Zircon

1. **Its dirty tracking is our seal.** A page of a VMO that a user pager backs
   is Clean, Dirty or AwaitingClean (`vm/include/vm/vm_cow_pages.h:470-493`). A
   write makes a Clean page Dirty. A writeback begins by making the Dirty pages
   AwaitingClean and taking write access away from every mapping of the range
   (`vm/vm_cow_pages.cc:6653-6779`). It ends by making the AwaitingClean pages
   Clean (`vm/vm_cow_pages.cc:6781-6905`). The transitions are checked in one
   place (`vm/vm_cow_pages.cc:3050-3133`). That is what our seal does while the
   guest runs: freeze the dirty set, write it out, and make it clean when the
   checkpoint lands.
2. **Its eviction splits pages a pager backs from anonymous pages.** A page a
   user pager backs is evicted when it is clean and read again from the pager
   (`vm/vm_cow_pages.cc:7206-7340`). An anonymous page is compressed
   (`vm/vm_cow_pages.cc:7393-7572`). `ReclaimPage` chooses between the two
   (`vm/vm_cow_pages.cc:7574-7616`), the page queues order the candidates
   (`vm/page_queues.cc`), and the evictor drives it (`vm/evictor.cc`). Ours has
   the same split: a named page is dropped and read again from its volume, and
   an overlay page is spilled.
3. **It comes with its tests.** `vm/unittests` defines 261 `VM_UNITTEST`
   cases: 81 for VMOs, 55 for the page list, 45 for address spaces, 29 for
   continuous attribution, 23 for the physical memory manager, 10 for the page
   queues, 7 for compression, 7 for the evictor and 4 for the slab allocator.
   A search for `VM_UNITTEST` finds 262, because the macro's own definition in
   `vm/unittests/test_helper.h` matches too. About half of them port (see
   [the tests](#which-zircon-tests-port)).
4. **It is designed for a pager outside the kernel.** A VMO's pages can come
   from a user-space pager through page requests (`vm/page_source.cc`,
   `object/pager_dispatcher.cc`, `object/pager_proxy.cc`). The pager supplies
   pages, is asked before a clean page becomes dirty, and writes dirty pages
   back. Our pager is that user-space pager, and its sources are the store, the
   cluster and a peer.

## Zircon's objects and ours

**`VmObjectPaged` is a memory region.** A `VmObjectPaged`
(`vm/vm_object_paged.cc`) is what a mapping maps. A fault on it enters at
`VmAspace::PageFault` (`vm/vm_aspace.cc:553`) and reaches the object through
`VmMapping::PageFaultLockedObject` (`vm/vm_mapping.cc:1174-1375`), which asks
the lookup cursor for the faulting page and maps the pages around it that are
already present (`vm/vm_mapping.cc:1334-1360`). A `MemoryRegion`
(`vmmemory/region.go:22-164`) is what one VMM session maps, and its faults enter
at `Fault` (`vmmemory/fault.go:14`).

**`VmCowPages` is two things here.** A `VmCowPages` holds the pages of a VMO in
a page list and knows its parent (`vm/vm_cow_pages.cc`). We use it twice:

- **A region's own layer.** Each memory region has one `VmCowPages` that holds
  only the pages the region owns: dirty pages, pages a checkpoint holds, pages
  written ahead, and pages loaded privately. Its page source is our pager, as a
  user pager's would be, so its pages are dirty-tracked.
- **An identity root.** Each published `(checkpoint, volume)` that a region on
  this host reads is a `VmCowPages` whose pages are Clean and never change. It
  is the root a region's lookup falls through to. Zircon finds a page's content
  by walking up the parent chain (`FindPageContentLocked`,
  `vm/vm_cow_pages.cc:2958-3036`). A region's content is a mosaic of many
  checkpoints, and no chain of parents describes it. So the walk is replaced by
  one lookup: `Locate` names the identity (`vmmemory/window.go:116-138`), and
  the identity names the root and the page in it. The sharing index
  (`Host.clean`, `vmmemory/host.go:67`) becomes the index of roots.

**`vm_page_t` is a resident page.** `vm_page_t` (`vm/include/vm/page.h:27-105`)
carries the queue link, the object and offset it belongs to, a share count, the
queue it is in, a pin count, the `always_need` hint and the dirty state. Our
`resident` (`vmmemory/resident.go:24-57`) carries its file and slot, its
identity, whether it is private, its recency and idle links, the count of stores
replacing it, and its cold copies. The port gives `resident` Zircon's queue
link, queue, pin count and dirty state. It keeps `fileSlot` in place of the
physical address, because the arena is a set of memfds. It keeps the alias set
(`vmmemory/aliases.go:11-82`) in place of the single back link. A Zircon page
has one owner. A resident page here is mapped by every region that reads its
identity, and an eviction must revoke all of them.

**`VmPageOrMarker` is a binding's slot.** A slot of Zircon's page list holds a
page, a zero marker, a reference to compressed storage, or one end of a zero
interval with its own dirty state (`vm/include/vm/vm_page_list.h:60-300`). A
binding (`vmmemory/bindings.go:10-53`) records the same four things in its own
way: `resident`, `zero`, `spillSlot`, and the region's compressed zero runs
(`vmmemory/bindings.go:66-77`, `vmmemory/internal/pageranges`). The port puts
them in the ported page list. The fields Zircon has no place for stay in a
binding beside it: whether the page is mapped, the checkpoint's copy it shares,
the page it was copied from, whether it is cold, and whether write-ahead made
it.

**`PageSource` and `PageRequest` are a fault's read.** A fault that finds no
page creates a `READ` request for a range (`vm/page_source.cc:351-387`). The
source batches overlapping requests, sends them to its provider
(`vm/page_source.cc:393-414`), and wakes every waiter whose range a supply
covers (`vm/page_source.cc:94-221`). The user pager answers with
`SupplyPages` (`vm/vm_cow_pages.cc:6039-6274`). A write to a clean page creates
a `DIRTY` request first when the source traps dirty transitions
(`vm/include/vm/page_source.h:300-316`, `vm/vm_cow_pages.cc:3135-3370`). Here
the provider is in the same process: the volume, the cluster and a peer behind
`Backing` (`vmmemory/vmmemory.go:190-267`). `PagerProxy`'s port packets
(`object/pager_proxy.cc:59-170`) become a goroutine per request.

**Pages a pager backs are named pages, and anonymous pages are the overlay.** A
named page is the published bytes of a checkpoint. It lives in an identity root,
is Clean, and is dropped under pressure and read again
(`docs/vm-memory.md:1093-1097`). An overlay page is a region's own and lives in
the region's layer. Zircon never reclaims a Dirty page of a VMO a user pager
backs (`vm/vm_cow_pages.cc:7233-7240`), and compresses only pages of a VMO with no
page source (`vm/vm_cow_pages.cc:7585-7587`). We must spill dirty pages: RAM is
checkpointed only on request, so its dirty set is bounded by the spill and not
by a writeback (`docs/architecture.md:103-128`). That is departure D2 below.

**Clones are forks, and only one kind is needed.** Zircon has three snapshot
kinds (`vm/include/vm/vm_object.h:65-73`): a full snapshot, a snapshot of
modified pages, and a snapshot on write. A full snapshot of a VMO whose content
can change needs a hidden parent (`CloneNewHiddenParentLocked`,
`vm/vm_cow_pages.cc:1582-1780`), and the tree is merged when children go
(`MergeContentWithChildLocked`, `vm/vm_cow_pages.cc:2117-2254`). A
snapshot-on-write child of a VMO a pager backs needs neither
(`CloneChildLocked`, `vm/vm_cow_pages.cc:1782-1829`). Our named
pages never change, and the pages a fork point lends are frozen for the life of
its seal (`docs/vm-memory.md:510-540`). So a fork's child relates to every page
it inherits as a snapshot-on-write child relates to a parent that never
changes: it reads the parent's page until it writes, and then copies it
(`CloneCowPageLocked`, `vm/vm_cow_pages.cc:2671-2753`; `AllocateCopyPage`,
`vm/vm_cow_pages.cc:822-862`). The port takes the snapshot-on-write path and
nothing else: no hidden parents, no merge, no full or modified snapshot, no
slices (`vm/vm_object_paged.cc:567`), no references
(`vm/vm_object_paged.cc:615`). A fork point's sealed pages become a temporary
identity root under the name the point lends (`vmmemory/checkpoint.go:442-465`),
which goes when the seal ends.

**The page size is the pager's.** Zircon's page is the constant `kPageSize`, 4
KiB. A pager here has one page size for its life, 4 KiB or 2 MiB
(`vmmemory/vmmemory.go:425-431`, `vmmemory/host.go:155-249`). The port keeps
Zircon's byte offsets, so a ported function reads like its source, and replaces
`kPageSize` with the page size of the object's pager. A page list node holds 16
slots (`vm/include/vm/vm_constants.h:64`): 64 KiB of a 4 KiB pager and 32 MiB of
a 2 MiB pager. Nothing in the ported code depends on the page being 4 KiB.

### Departures

The port copies Zircon's code except at these five places. Each is recorded in
the ported file beside the line it changes, with the reason.

- **D1. A store into an AwaitingClean page splits it.** Zircon makes the page
  Dirty again in place (`vm/include/vm/vm_cow_pages.h:475-476`,
  `vm/vm_cow_pages.cc:3075-3087`). The writeback then reads bytes that have
  moved since it began, and a later writeback takes the newer ones. A
  checkpoint here must hold exactly the bytes of its pause, across every page
  of the VM, because a fork's children and a restore read them as one point in
  time (`docs/architecture.md:50-54`). So the AwaitingClean page stays
  with the checkpoint, unchanged, and the store gets a Dirty copy. That is what
  a store into a sealed page does today (`vmmemory/fault.go:156-229`,
  `vmmemory/bindings.go:403-420`). The spec in step 2 checks that Zircon's rule
  breaks this and ours does not.
- **D2. A Dirty or AwaitingClean page can be spilled.** Zircon refuses to
  reclaim them (`vm/vm_cow_pages.cc:7233-7240`). We spill them to the reference
  storage of step 6, as Zircon compresses an anonymous page
  (`vm/vm_cow_pages.cc:7393-7572`). FreeBSD launders dirty pages of objects a
  pager backs (`sys/vm/vm_pageout.c:717`) and is the comparison, not the source.
- **D3. The pause marks nothing.** `WritebackBeginLocked` walks every page of
  the range (`vm/vm_cow_pages.cc:6700-6764`) and then takes write access away
  (`vm/vm_cow_pages.cc:6766-6774`). Our pause only write-protects the runs of
  the dirty set, and the walk that records the set runs after the vCPUs resume
  (`vmmemory/checkpoint.go:149-160`, `vmmemory/checkpoint.go:244-334`). At 4 KiB
  the walk is 1.97 s of a 2.14 s capture (`docs/vm-memory.md:1229-1232`). So the
  port issues the range protection in the pause and makes the pages
  AwaitingClean in the walk behind it, with the region held, as today.
- **D4. A failed checkpoint gives its pages back.** Zircon has no abandon: a
  writeback that does not end leaves its pages AwaitingClean, and the next
  `WritebackBegin` takes them again. Here a publication that does not land, or
  an unseal, makes each page Dirty again with its own reservation and revokes
  its read-only mapping (`vmmemory/checkpoint.go:752-818`). The spec checks it.
- **D5. A dirty page holds its spill slot before it is dirty.** Zircon
  allocates compressed storage when it compresses, and compression can fail
  (`vm/compression.cc:74-137`). Here a store takes its slot of the spill file
  before the guest resumes, so a spill never needs space
  (`docs/vm-memory.md:165-178`, `vmmemory/pressure.go:45-110`). The reference
  storage keeps that rule.

## Part by part

### Arena and residency

**Stays ours.** The arena is a set of memfds of one page size
(`vmmemory/vmmemory.go:269-325`, `vmmemory/linux.go:26-475`), with a free set
per file (`vmmemory/internal/slots`) and an address space far larger than its
pages (`vmmemory/allocation.go:17-52`, `docs/vm-memory.md:222-277`). Zircon's
physical memory manager (`vm/pmm.cc`, `vm/pmm_node.cc`, `vm/pmm_arena.cc`)
hands out physical pages, and none of that applies to a memfd. The resident page
gains `vm_page_t`'s queue link, queue, pin count and dirty state, as above.

### Page identity and sharing

**Mostly ours, held in Zircon's objects.** Sharing by name across volumes
(`docs/vm-memory.md:498-556`) has no Zircon counterpart: Zircon shares a page
only within one tree of clones. The index from identity to resident page
(`vmmemory/host.go:67`, `vmmemory/resident.go:498-533`,
`vmmemory/resident.go:570-596`) becomes the index of identity roots. Binding to
a resident page under its identity (`vmmemory/window.go:326-397`) becomes a
lookup that falls through to the root, in place of
`FindPageContentLocked`'s walk (`vm/vm_cow_pages.cc:2958-3036`). Tenant checks
(`vmmemory/region.go:262-285`) stay ours.

### The fault queue and planning

**The queue stays ours. The lookup comes from Zircon.** The userfaultfd reader,
the queue of pending faults with their twins, and the pacing of repeated faults
(`vmmemory/connection_linux.go:639-937`, `vmmemory/faultqueue.go:12-102`,
`vmmemory/repeats.go:25-69`) replace Zircon's fault entry and have no
counterpart in it. A fault's lookup becomes Zircon's lookup cursor
(`vm/vm_cow_pages.cc:3372-3917`): `RequireReadPage`
(`vm/vm_cow_pages.cc:3888`) for a read and `RequireOwnedPage`
(`vm/vm_cow_pages.cc:3747`) for a store, with `ReadRequest`
(`vm/vm_cow_pages.cc:3565-3625`) building the request for a missing range.
Mapping the window's resident pages beside the faulting one
(`vmmemory/window.go:399-484`, `vmmemory/window.go:819-965`) is the mapping's
fault-around of present pages (`IfExistPages`, `vm/vm_cow_pages.cc:3708-3745`;
`vm/vm_mapping.cc:1334-1360`). Which pages a fault reads and in what order
stays ours: its page first, the rest of its run prefetched behind it, and a
fault at random alone (`vmmemory/faultfirst.go:11-104`,
`docs/vm-memory.md:720-778`). Zircon extends a read request up to a size and
lets the user pager decide; it has no notion of reading the faulting page
first.

### Prefetch

**The mechanics come from Zircon. The policy stays ours.** A prefetch is a
`READ` request a fault does not wait for, like `PrefetchRange`
(`vm/vm_object_paged.cc:219-254`). A fault that meets a page a prefetch is
reading waits for that request instead of reading again
(`vmmemory/prefetch.go:240-274`). Zircon's page source does the same with its
tree of outstanding requests and its early wake
(`vm/page_source.cc:104-150`, `vm/page_source.cc:152-221`). Cancelling a
prefetch for its slots (`vmmemory/prefetch.go:573-616`) becomes
`CancelRequest` (`vm/page_source.cc:470-520`) and failing the request's range
(`vm/vm_cow_pages.cc:6276-6297`). Which faults prefetch, the bound on prefetches
in flight, the bulk class of their reads, and mapping landed pages into the
region that asked stay ours (`vmmemory/prefetch.go:14-71`,
`vmmemory/prefetch.go:482-531`, `vmmemory/prefetch.go:673-728`).

### Population

**Partly from Zircon.** Mapping what is already resident before the guest runs
(`vmmemory/population.go:92-166`) is a commit of the range that loads nothing,
like `CommitRangeLocked` (`vm/vm_cow_pages.cc:3943-4020`) restricted to pages
present in an identity root. The budget of 128 runs and 16,384 pages and the
length test stay ours (`vmmemory/population.go:41-64`,
`docs/vm-memory.md:789-827`).

### Eviction, replacement, pressure and fair share

**Eviction comes from Zircon. Replacement, pressure and fair share stay ours.**

- The victim loop (`vmmemory/allocation.go:394-548`) becomes the evictor's
  synchronous path (`vm/evictor.cc:407-425`, `vm/evictor.cc:443-559`) over the
  page queues (`PeekIsolate`, `vm/page_queues.cc:1380-1436`). Recency is fault
  order, as it is now (`docs/vm-memory.md:1182-1186`): `MarkAccessed`
  (`vm/page_queues.cc:877-927`) is called on a fault. Zircon also ages pages by
  the accessed bits of page tables; a userfaultfd pager cannot read the VMM's
  accessed bits, so aging by them is not ported.
- An idle page (`vmmemory/resident.go:385-398`, `docs/vm-memory.md:2214-2235`)
  is a page in the don't-need queue (`MoveToReclaimDontNeed`,
  `vm/page_queues.cc:1065-1071`), which is taken first.
- The split between dropping and spilling is `ReclaimPage`
  (`vm/vm_cow_pages.cc:7574-7616`) with D2. The eviction itself
  (`vmmemory/spill.go:139-274`) becomes `ReclaimRangeForEviction`
  (`vm/vm_cow_pages.cc:7206-7340`) for a page of an identity root, and
  `ReclaimPageForCompression` (`vm/vm_cow_pages.cc:7393-7572`) for a page of a
  region's layer. Revoking every alias before the slot goes back
  (`docs/vm-memory.md:2237-2260`) is `RangeChangeUpdateLocked` with
  `UnmapAndHarvest` (`vm/vm_cow_pages.cc:6988-7046`) over every region in the
  alias set.
- **The fair share stays ours.** A region within its share that faulted in the
  last turnover is protected (`vmmemory/allocation.go:551-578`,
  `docs/vm-memory.md:2262-2304`). Zircon protects whole VMOs by priority and
  hints (`always_need`, `vm/include/vm/page.h:94-96`), not by a share of the
  arena. It is a filter on the evictor's candidates.
- **Replacement stays ours** (`vmmemory/replacement.go:27-153`,
  `docs/vm-memory.md:1691-1746`). Zircon changes page tables under the VMO's
  lock, in the kernel, in microseconds. Ours are commands to another process,
  and the page a store copied from must stay until the command that replaces it
  lands.
- **Pressure stays ours** (`vmmemory/pressure.go:15-264`,
  `vmmemory/losswindow.go:17-210`). The dirty budget, the checkpoint asked for at
  three quarters of it, the deliberate stop and the loss window are this
  system's durability rules. Zircon waits on the physical memory manager
  (`vm/pmm_node.cc`) and has nothing to ask for.

### Spill and give-back

**The spill's bookkeeping comes from Zircon's compressed references. The
give-back comes from Zircon's zero-page scan, widened.**

- A spilled page is a slot of the page list that holds a reference to storage,
  as a compressed page is (`vm/include/vm/vm_page_list.h:189-231`). The storage
  is `VmCompression`'s interface (`vm/compression.cc:74-201`) over the spill
  file, with a compressor that stores the page as it is and the checksum it is
  read back with (`vmmemory/spill.go:19-48`). Zircon's slot storage
  (`vm/slot_page_storage.cc:110`) is the shape it follows. LZ4 is not ported.
  The file's allocation, its truncation at start, and D5 stay ours
  (`vmmemory/spill.go:75-130`, `vmmemory/reservations.go:11-70`).
- A cold copy (`vmmemory/cold.go:1-56`) is a page that a store trap may have
  made without a store. Zircon has the same case for zero: a page copied from
  the zero page on a write fault goes in the zero-fork queue
  (`vm/page_queues.cc:1086-1133`). The scanner takes pages off it
  (`vm/scanner.cc:290-312`) and `DedupZeroPage` checks each one, takes write
  access away, checks again and puts the zero marker back
  (`vm/vm_cow_pages.cc:1363-1437`). The give-back is those steps with the page
  it was copied from in place of zero (`vmmemory/giveback.go:79-228`). The age
  of 200 ms, the give-back at eviction and the comparison against the volume
  when the origin is gone stay ours (`vmmemory/cold.go:58`,
  `vmmemory/cold.go:360-544`).

### Dirty tracking, seal, checkpoint and flush

**The state machine comes from Zircon, with D1, D3 and D4.**

- A store makes a page Dirty under `PrepareForWriteLocked`
  (`vm/vm_cow_pages.cc:3135-3370`) and `DirtyPages`
  (`vm/vm_cow_pages.cc:6299-6592`). It replaces `takeFromCheckpoint`,
  `setDirty` and `setDirtyMappedRun` (`vmmemory/bindings.go:403-543`).
- The runs a seal protects (`vmmemory/bindings.go:137-168`) are what
  `EnumerateDirtyRangesLocked` reports (`vm/vm_cow_pages.cc:6594-6651`),
  restricted to mapped pages.
- The seal (`vmmemory/checkpoint.go:174-223`) is `WritebackBegin` with D3. The
  pages the walk takes (`vmmemory/checkpoint.go:272-334`) become AwaitingClean.
- A page a checkpoint holds and the guest has since stored into is the
  checkpoint's copy (`vmmemory/bindings.go:25-30`, D1). Zircon has no such copy.
  It lives in the region's layer beside the guest's page, as an AwaitingClean
  page with no slot of its own, owned by the checkpoint.
- Retiring a published checkpoint (`vmmemory/checkpoint.go:630-696`) is
  `WritebackEnd` (`vm/vm_cow_pages.cc:6781-6905`), then a move of each page from
  the region's layer into the identity root of the checkpoint that published it,
  with `TakePages` and `SupplyPages` (`vm/vm_cow_pages.cc:5755-5999`,
  `vm/vm_cow_pages.cc:6039-6274`). A page the volume holds no object for is
  given back as a hole (`vmmemory/checkpoint.go:698-750`), which is a zero
  interval (`vm/vm_page_list.cc:783-994`).
- Abandoning a checkpoint is D4 (`vmmemory/checkpoint.go:752-818`).
- **The settle stays ours** (`vmmemory/settle.go:39-427`). It is the zero-page
  check with the origin in place of zero, as the give-back is, run behind the
  pause over many workers. Zircon has no counterpart that compares against a
  page other than zero.
- **The flush stays ours** (`vmmemory/flush.go:22-64`,
  `docs/vm-memory.md:2020-2053`). It is a protocol request the host answers.

### Fork sharing

**From Zircon's snapshot-on-write clone, and nothing else of the clone tree.** A
child maps an inherited page from the identity root until it stores, and then
copies it (`vm/vm_cow_pages.cc:2671-2753`). The name a fork point gives its
sealed pages (`vmmemory/checkpoint.go:428-465`, `vmmemory/resident.go:565-596`)
is a temporary identity root whose pages are the parent's AwaitingClean pages.
Ending the seal takes the root away and revokes every child that still maps it
(`vmmemory/resident.go:598-623`, `vmmemory/isolation.go:732-794`), which is
`RangeChangeUpdateCowChildren` (`vm/vm_cow_pages.cc:7048-7165`) over the alias
set. That a fork's hold relieves no dirty budget
(`vmmemory/checkpoint.go:127-140`) stays ours.

### Isolation

**Stays ours** (`vmmemory/isolation.go:1-875`, `docs/vm-memory.md:361-497`). It
divides memfds by who may read them: a private file per region, a shared file
per tenant, a public file, and a file per fork point, with digests checked when
a published page moves. Zircon protects VMOs with handle rights in the kernel.
The port changes which object a page belongs to; the file and slot a page sits
in, and what each VMM is given, do not change.

### The mapping protocol

**Stays ours** (`vmmemory/vmmemory.go:367-422`,
`vmmemory/connection_linux.go:105-637`, `docs/vm-memory.md:1899-2160`, the Rust
client). Zircon's address spaces and mappings (`vm/vm_aspace.cc`,
`vm/vm_mapping.cc`, `vm/vm_address_region.cc`) and its hardware page tables do
not apply. Zircon's range changes translate one for one: `RemoveWrite` is
`Protect`, `Unmap` and `UnmapAndHarvest` are `RevokeBatch`
(`vm/vm_cow_pages.cc:6988-7046`). Zircon's `DeferredOps`
(`vm/vm_cow_pages.cc:8122-8195`) collects range changes and frees pages after
the object's lock is released. The port uses it to issue mapping commands with
no object lock held (see [locking](#locking)).

### The userfaultfd connection

**Stays ours** (`vmmemory/connection_linux.go:126-1085`). Session setup, the
fault reader, the workers, the control reader, verification and teardown are
how faults reach the pager on Linux. Zircon's equivalents are its exception
path and `PagerProxy` (`object/pager_proxy.cc`), and neither applies.

## Locking

Zircon takes one lock per tree of `VmCowPages`, a lock per page queue list, and
the page source's lock (`vm/include/vm/page_source.h:385`). A fault holds the
VMO's lock across the lookup and the page-table update
(`vm/vm_mapping.cc:1174-1375`), and gives it up only to wait for a page
request. It then starts again from the top (`vm/vm_aspace.cc:553-648`).

Ours has a short host lock, a region lock held shared by faults and exclusively
by a seal, a lock per read-ahead window, a lock per resident page, and a lock
for the binding map (`vmmemory/region.go:22-164`, `vmmemory/host.go:25-136`,
`docs/vm-memory.md:1171-1180`). A fault gives the region up across its backing
read and across a reclaim (`vmmemory/region.go:507-530`).

The port keeps Zircon's lock per object for the page list, the dirty state and
the page queues. It keeps three rules of ours:

- **No object lock is held across a mapping command.** Zircon's page-table
  update takes microseconds in the kernel. Ours is a round trip to the VMM over
  a socket. Commands are collected under the lock as `DeferredOps` collects
  range changes, and issued after it is released. The window's lock, which a
  fault holds from start to end (`vmmemory/fault.go:72-79`), is what keeps two
  faults of one window from issuing commands out of order.
- **A fault gives everything up across a read and starts again**, as Zircon's
  page requests do. The re-check after a reclaim
  (`vmmemory/fault.go:807-823`, `docs/vm-memory.md:1603-1611`) is that rule.
- **The seal waits for page-table work and never for bytes**
  (`docs/vm-memory.md:1247-1260`).

Goroutines replace Zircon's kernel threads. The page queues' aging threads
(`vm/page_queues.cc:557-655`) and the evictor's thread
(`vm/evictor.cc:561-574`) become goroutines driven by `platform.Clock`, so a
simulation advances them on a `sim.Clock`.

## Which Zircon tests port

Each ported test keeps the repository's naming, and its comment names the
Zircon test and file it came from. The helper that makes a VMO a user pager
backs (`vm/unittests/test_helper.cc`) becomes a Go helper over
`internal/testbacking`, `internal/testpager` and a `sim.Disk` spill file.

| Zircon file | Cases | Port | Where |
| --- | --- | --- | --- |
| `vmpl_unittest.cc` | 55 | all 55 | the page list, step 3 |
| `page_queues_unittest.cc` | 10 | all 10 | the page queues, step 5 |
| `evictor_unittest.cc` | 7 | 6; not `evictor_discardable_test` | the evictor, step 7 |
| `compression_unittest.cc` | 7 | 4: `compression_smoke_test`, `compression_zero_test`, `compression_fail_test`, `compression_move_reference_test`; not the LZ4 and slot-size cases | the spill, step 6 |
| `vmo_unittest.cc` | 81 | 33, listed below | the region's layer and the roots, step 9 |
| `aspace_unittest.cc` | 45 | 2: `vm_mapping_page_fault_optimisation_test` (`:1566`), `vm_mapping_page_fault_range_test` (`:1715`), as tests of mapping a fault's resident neighbours | step 11 |
| `pmm_unittest.cc` | 23 | none: the arena is memfds | — |
| `continuous_attribution_unittest.cc` | 29 | none: ours counts sharing as unique, mapped and saved bytes (`vmmemory/stats.go:198-253`), not by process | — |
| `slab_unittest.cc` | 4 | none: Go allocates | — |

That is 110 of 261.

**The VMO cases that port:** `vmo_create_test`, `vmo_commit_test`,
`vmo_commit_compressed_pages_test`, `vmo_demand_paged_map_test`,
`vmo_dropped_ref_test`, `vmo_read_write_smoke_test`, `vmo_lookup_test`,
`vmo_lookup_clone_test`, `vmo_clone_removes_write_test`,
`vmo_clones_of_compressed_pages_test`, `vmo_move_pages_on_access_test`,
`vmo_eviction_hints_test`, `vmo_eviction_hints_clone_test`,
`vmo_reclamation_test`, `vmo_attribution_clones_test`,
`vmo_attribution_pager_test`, `vmo_attribution_dedup_test`,
`vmo_attribution_compression_test`, `vmo_lookup_compressed_pages_test`,
`vmo_write_does_not_commit_test`, `vmo_dirty_pages_test`,
`vmo_dirty_pages_writeback_test`, `vmo_dirty_pages_with_hints_test`,
`vmo_supply_compressed_pages_test`, `vmo_dedup_dirty_test`,
`vmo_prefetch_compressed_pages_test`, `vmo_skip_range_update_test`,
`vmo_zero_marker_transfer_test`, `vmo_pager_supply_test`,
`vmo_compress_to_marker_pager_test`, `vmo_compress_to_marker_anon_test`,
`vmo_lookup_readable_simple_test`, `vmo_lookup_readable_clone_test`. Where D1,
D2 or D4 changes what a case expects, the Go test expects our behaviour and its
comment says which departure. `vmo_dirty_pages_writeback_test`
(`vm/unittests/vmo_unittest.cc:3342`) is the one D1 changes most.

**The VMO cases that do not port**, by reason: pinning and DMA (`vmo_pin_*`,
`vmo_multiple_pin_*`, `vmo_pinning_*`, `vmo_zero_pinned_test`,
`vmo_pinned_wrapper_test`, `vmo_pin_race_loaned_test`,
`vmo_zero_partially_pinned_range_test`, `vmo_always_pinned_with_no_pages_test`,
`vmo_ever_pinned_*`); physical and contiguous memory
(`vmo_create_physical_test`, `vmo_physical_pin_test`,
`vmo_create_contiguous_test`, `vmo_contiguous_decommit_*`,
`vmo_attribution_ops_contiguous_test`, `vmo_precommitted_map_test`); cache
policy and kernel mappings (`vmo_cache_test`, `vmo_remap_test`,
`vmo_double_remap_test`, `vmo_clone_kernel_mapped_compressed_test`,
`vmo_apply_unmap_to_child_with_kernel_mapping_test`); loaned pages
(`vmo_always_need_evicts_loaned_test`, `vmo_unloan_test`,
`vmo_loaned_*`); discardable VMOs (`vmo_discardable_*`, `vmo_discard_*`,
`vmo_lock_count_test`); high priority (`vmo_high_priority_*`); clone kinds we do
not take (`vmo_lookup_slice_test`, `vmo_parent_merge_test`,
`vmo_snapshot_modified_test`, `vmo_dedup_hidden_zero_page_test`,
`vmo_reference_attribution_commit_test`); and the rest
(`vmo_create_maximum_size`, `vmo_unaligned_size_test`,
`vmo_user_stream_size_test`, `vmo_attribution_ops_test`,
`vmo_get_page_offset_test`). A step that finds a case on either list wrong moves
it and says why in its task.

### How they run

Every ported test runs in a `testing/synctest` bubble over `platform/sim`, as
the pager's campaigns do (`vmmemory/prefetch_campaign_test.go:73-111`). The
aging and eviction goroutines run on the bubble's `sim.Clock`, and
`synctest.Wait` reaches the point where they are idle. A test that Zircon runs
with threads racing runs with a `sim.Scheduler` releasing the steps, so a seed
replays.

## The tests we have

**The pager's own tests stay and keep passing.** 309 tests in 79 files
(`vmmemory/*_test.go`), 55 of them Linux-only, test the pager almost wholly
through its exported API. Steps 4 to 8 swap one layer at a time under that
API, so every one of them runs unchanged at every step. Steps 10 and 11 run a
growing named list of them under the new core beside the full suite under the
old one, and step 12 requires the whole suite under the new core. The seams
tests use (`vmmemory/export_test.go:11-165`) move with the code they reach: the
eviction seam to the evictor, the reclaim and seal seams to the region's
layer.

Four files test internals: `faultqueue_internal_test.go` and
`repeats_internal_test.go` test what stays ours and do not change.
`reservations_internal_test.go` is rewritten against the reference storage in
step 6, and `probe_internal_test.go` against the new core in step 12. No test is
deleted without the owner's word.

**Sites, probes and guards move with their behaviour.** Each keeps its name, so
`scripts/mutation/guards.json` and the campaigns that require every site to
fire do not change.

| Name | Today | Lands in |
| --- | --- | --- |
| `vmmemory/evict-past-a-free-slot` (Buggify) | `vmmemory/allocation.go:333,349`, `vmmemory/region.go:540`, `vmmemory/isolation.go:233` | the evictor, step 7 |
| `vmmemory/prefetch-slow`, `-refused`, `-failed` (Buggify) | `vmmemory/prefetch.go:187,353,359` | the page source's provider, step 8 |
| `spill-sparse`, `pager-forget-spill` (Bug) | `vmmemory/spill.go:76,241` | the reference storage, step 6 |
| `pager-prefetch-ignores-pressure` (Bug) | `vmmemory/allocation.go:438` | the evictor, step 7 |
| `pager-read-in-flight-again` (Bug) | `vmmemory/prefetch.go:242` | outstanding requests, step 8 |
| `pager-read-alone-at-random`, `pager-prefetch-every-fault` (Bug) | `vmmemory/prefetch.go:683-684` | fault policy, step 8 |
| `pager-plan-the-window-at-random`, `pager-read-the-run-first`, `pager-plan-the-window-first`, `pager-fault-waits-for-its-prefetch` (Bug) | `vmmemory/faultfirst.go:84,103,161,182` | fault policy, step 8, and the new core, step 11 |
| `pager-zero-new-page` (Bug) | `vmmemory/resident.go:182` | page creation in the new core, step 11 |
| `pager-give-back-changed-copy` (Bug) | `vmmemory/giveback.go:160` | the origin scan, step 12 |
| `vmmemory/eviction-during-publication` (Probe) | `vmmemory/spill.go:178` | the evictor, step 7 |
| the nine prefetch probes | `vmmemory/prefetch.go:73-106`, `vmmemory/faultfirst.go:145` | the page source's provider, step 8 |
| the probe build's audit: `stable`, `bind`, `granted`, `retired`, `reshared` | `vmmemory/probe_on.go:62-260` | the dirty-state transitions, step 12 |

The controlled points of a run (`sim.Admit` at `vmmemory/faultfirst.go:379,407`,
`vmmemory/allocation.go:449,543`) and the priced planning work
(`vmmemory/faultfirst.go:47-50`, `vmmemory/window.go:116-118`) move the same
way, with the same names, so `TestPrefetchCampaignReplaysItsSeeds` keeps
replaying. During steps 10 to 12, `scripts/check-guards.py` runs each guard
under both cores where its test is on the list.

## The TLA+ question

**One state machine deserves a spec: a page's dirty life across a checkpoint.**
It is where Zircon and we differ (D1 to D5), and where the pager has had its
worst defect: a refault that bound a private page after a checkpoint retired it
(`docs/vm-memory.md:1603-1611`). `spec/writeback/Writeback.tla` models a few
pages of one region: a guest that stores, a pause that seals, the walk behind
it, a settle, an upload that reads, a publication that lands or fails, the
retire and the abandon, an eviction that spills or drops, a refault, a reclaim
that a seal meets halfway (`vmmemory/checkpoint.go:286-309`), and a fork point
that holds the seal. Its invariants:

- `SealedBytes`: what an upload reads of a page is what the page held at the
  pause.
- `NoLostWrite`: a page reads what the guest last stored, through every
  eviction, refault, retire and abandon.
- `Reserved`: every page the guest may store into in place, and every page a
  checkpoint holds, owns exactly one spill reservation.
- `Budget`: the reservations taken never exceed the dirty budget.

Its mutants put back Zircon's in-place AwaitingClean to Dirty (must fail
`SealedBytes`), the refault that ignores a retire (must fail `Reserved`), and a
seal that skips a page a reclaim holds (must fail `NoLostWrite`). The
configurations under `MC*.cfg` run in seconds; two pages, two values and two
checkpoints. A `deep/` configuration with three pages and a fork hold stays
under two minutes, or it is made smaller.

The page requests do not get a spec. Their batching and waking are local to
one process and are covered by the ported tests and the prefetch campaign. The
arena's files keep `spec/arena`, and migration keeps `spec/postcopy`; neither
changes.

## Steps

Each step lands on main with `just check` passing. Steps 4 to 8 replace one
layer of the pager in place, with the whole existing suite as the check. Only
the fault and seal core (steps 10 to 12) runs beside the old code, behind a
switch, until it has been measured.

1. **The licence and the package (TASK-92.1).** `vmmemory/internal/zirconvm`
   with Zircon's `LICENSE`, a header rule and a test that holds every file to it
   (see [licence](#licence)). The determinism rule covers the package.
2. **The spec (TASK-92.2).** `spec/writeback` as above, with its mutants, in
   `just check-spec`, and its defects in `spec/bugs.md` if it finds any.
3. **The page list (TASK-92.3).** `vm/vm_page_list.cc` and
   `vm/include/vm/vm_page_list.h`, ported with their 55 tests. Nothing uses it
   yet.
4. **The bindings on the page list (TASK-92.4).** The binding blocks and the
   compressed zero runs (`vmmemory/bindings.go:59-114`,
   `vmmemory/bindings.go:268-306`, `vmmemory/internal/pageranges`) become the
   page list. The sealable runs become the page list's dirty runs. The whole
   suite passes unchanged, and `vmmemory/sparse_metadata_test.go` holds the
   metadata bound.
5. **The page queues (TASK-92.5).** `vm/page_queues.cc`, ported with its 10
   tests. The host's recency and idle lists (`vmmemory/pagelist.go:8-64`,
   `vmmemory/host.go:91-95`) and the pins of cold copies
   (`vmmemory/cold.go:62-88`) become the queues: the reclaim queues, the
   don't-need queue and the zero-fork queue.
6. **The spill as references (TASK-92.6).** `vm/compression.cc` and the shape of
   `vm/slot_page_storage.cc`, with D5 and four tests. The reservations and the
   spill's slots (`vmmemory/reservations.go`, `vmmemory/spill.go:19-130`) become
   references in the page list.
7. **The evictor (TASK-92.7).** `vm/evictor.cc`, ported with 6 tests. The victim
   loop of `allocate` (`vmmemory/allocation.go:394-548`) becomes the evictor's
   synchronous path, with the fair share as a filter on its candidates.
8. **Page requests (TASK-92.8).** `vm/page_source.cc` with `PagerProxy` as a
   goroutine. A fault's read and a prefetch become `READ` requests, and a fault
   meeting a prefetch waits on the request. The prefetch campaign replays its
   seeds.
9. **The region's layer (TASK-92.9).** `vm/vm_cow_pages.cc` and
   `vm/vm_object_paged.cc`: the lookup cursor, supply, take, the dirty states
   with D1 to D4, the zero intervals, the reclaim split with D2, and the
   snapshot-on-write copy, ported with the 33 VMO tests. Nothing uses it yet.
10. **One switch for the core (TASK-92.10).** `vmmemory.Config.Core` and
    `SPROUTFS_PAGER_CORE` (`current`, the default, or `zircon`). Each exported
    method of `Host` and `MemoryRegion` dispatches on it. No behaviour changes,
    and the `zircon` core answers nothing yet.
11. **Faults and stores in the new core (TASK-92.11).** Read faults, prefetch,
    population, stores, write-ahead, placement and the rules
    (`vmmemory/fault.go`, `vmmemory/faultfirst.go`, `vmmemory/window.go`,
    `vmmemory/population.go`, `vmmemory/placement.go`, `vmmemory/rules.go`) over
    the region's layer and the identity roots. `just check` runs the tests on
    the named list under the new core.
12. **Checkpoints and the rest in the new core (TASK-92.12).** Seal, walk,
    settle, retire, abandon, share, read dirty, the loss window, pressure,
    eviction, cold copies, give-back, isolation, moves, fork files, serving and
    handoff. At the end the whole pager suite, and the host, migration and
    simulation suites, pass under both cores, and every guard is killed under
    both.
13. **The switch-over (TASK-92.13).** The default becomes `zircon`, and the old
    core runs in the second pass. A GCE run repeats the recorded measurements
    under both cores: the dependent and forward 4 KiB and 2 MiB fault chains
    (`docs/measurements/gce-fault-first-2026-10-04.md`,
    `docs/measurements/gce-random-fault-planning-2026-10-04.md`), the 4 KiB
    capture pause (`docs/vm-memory.md:1229-1232`), a fork fan-out, and a warm
    restore. The benchmarks `BenchmarkARandom4KiBFault` and
    `BenchmarkAForward4KiBFault` run on the Mac at every step from 10 on.
14. **Deleting the old core (TASK-92.14).** See below. The switch and the second
    pass go.

## What gets deleted

At step 14, of the old core: `vmmemory/fault.go`, `vmmemory/faultfirst.go`,
`vmmemory/window.go`, `vmmemory/checkpoint.go`, `vmmemory/settle.go`,
`vmmemory/bindings.go`, `vmmemory/resident.go`, `vmmemory/allocation.go`,
`vmmemory/spill.go`, `vmmemory/reservations.go`, `vmmemory/pagelist.go`,
`vmmemory/aliases.go`, `vmmemory/prefetch.go`, `vmmemory/population.go`,
`vmmemory/cold.go`, `vmmemory/giveback.go`, `vmmemory/revocation.go` and
`vmmemory/internal/pageranges`, as far as each is not the policy code the new
core calls. What stays is listed under each part above: the arena, isolation,
placement and rules, replacement, pressure and the loss window, the settle, the
flush, the fault queue and repeats, the connection, the stats and the probes.
The test seams of the old core go with it. Tests are not deleted; any test that
only made sense of the old core is listed for the owner first.

## Licence

Zircon's kernel is under the MIT licence (`zircon/kernel/LICENSE`, and the
header of every file ported, for example `vm/vm_page_list.cc:1-5`). The
repository is under Apache-2.0 (`LICENSE`). MIT code may be included in an
Apache-2.0 project as long as its copyright and permission notice go with it.
The two licences are compatible in this direction, and both are OSI-approved.
Fuchsia's top-level `LICENSE` is BSD-3-Clause and covers files outside
`zircon/kernel`; nothing outside it is ported. Fuchsia's `PATENTS` is an
additional patent grant from Google and asks nothing of us.

- `vmmemory/internal/zirconvm/LICENSE` is a verbatim copy of
  `zircon/kernel/LICENSE`, with both of its copyright lines.
- Every ported file begins with its source's copyright line and these two
  lines, naming every Zircon file it draws from, with the revision:

  ```go
  // Copyright 2016 The Fuchsia Authors
  // Ported from zircon/kernel/vm/vm_page_list.cc and vm/include/vm/vm_page_list.h
  // at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.
  ```

- A test in the package fails on a Go file without those lines, or one that
  names a Zircon file that does not exist at that revision's path.
- `docs/vm-memory.md` names the port and its licence where it describes the
  package layout (`docs/vm-memory.md:14-24`).
- Nothing from FreeBSD is copied. `sys/vm/swap_pager.c` is under BSD-4-Clause,
  which is not OSI-approved.

## Risks, and what we lose and gain

**Fault latency.** A dependent 4 KiB fault from the cluster costs 0.83 ms at the
median against 0.66 ms for its read alone
(`docs/vm-memory.md:760-771`). Everything that made it so is policy that stays:
the page first, planning behind the read, a fault at random alone, no lock held
across a read or a command. The risk is in what the port adds per fault: the
page list's node lookups, the queue moves, and the request objects. The
benchmarks above and the GCE run in step 13 measure it. The switch-over waits
for them.

**Locking.** Zircon's design assumes a page-table update under the object's
lock. Ours cannot hold a lock across a command. Collecting commands as
`DeferredOps` does keeps the order within a window, but any ported path that
both changes a page and maps it must be checked for the order its commands
reach the VMM. Steps 11 and 12 run `go test -race` of the pager on the Mac,
and the hostile Linux suites (`vmmemory/hostile_linux_test.go`,
`vmmemory/hostile_client_linux_test.go`) on GCE, at each landing.

**The departures.** D1 to D5 change Zircon's rules in five places. The spec in
step 2 checks D1 and D4 before any code depends on them.

**The size of step 12.** It moves most of what a checkpoint does at once. If it
does not fit one reviewable change, it is divided at the boundary between the
seal and eviction.

**What we lose.** For as long as both cores exist, the policy code runs over two
cores and every fix lands twice. The pager's own measurements (fault planning,
seal pause, settle, prefetch) were taken on the old core and are repeated, not
carried over. The isolated arena, placement and the rules were written against
bindings and residents and must be re-attached to Zircon's objects.

**What we gain.** A page list with zero intervals, references and dirty runs
that has 55 tests behind it, in place of binding blocks, compressed zero runs
and sealable runs kept in step by hand. A dirty state machine whose
transitions are checked in one function. Page queues with a don't-need queue
and a zero-fork queue in place of a recency list, an idle list and a pin set.
An evictor with targets, which a host can drive ahead of a shortage rather
than in the allocation that meets it. Page requests that batch and wake by
range. 110 tests of these parts from a pager that has run on many devices for
years. Memory per region is likely lower for sparse use: a page list node is 16
slots of 8 bytes, against a binding block of 256 bindings
(`vmmemory/bindings.go:59-61`).

## Decisions for the owner

1. **D1, a store into a page a checkpoint holds.** Recommendation: split it,
   as the pager does today. Zircon's in-place rule would let a capture hold
   bytes from after its pause, and a fork's child in a shared arena maps the
   parent's page directly, so a store in place would change it under the
   child. The spec shows it.
2. **D2, spilling dirty pages of named volumes.** Recommendation: yes. Without
   it a RAM pager's dirty set is bounded only by its arena, because RAM is not
   checkpointed on the interval.
3. **Byte offsets in the ported code.** Recommendation: keep Zircon's byte
   offsets with the pager's page size in place of `kPageSize`, so each ported
   function can be compared with its source line by line. The policy code keeps
   page indexes, and converts at the boundary.
4. **Aging.** Recommendation: port the page queues with aging driven by faults
   only. A userfaultfd pager cannot read the VMM's accessed bits, so Zircon's
   aging thread would only rotate queues on a timer and add nothing.
5. **The switch-over gate.** Which GCE runs, and how much slower a fault may
   be, if at all, before step 13 waits. Recommendation: the runs listed in step
   13, and no median fault slower by more than the spread of two runs of the
   old core.
6. **Test names.** Recommendation: the repository's sentence names, with the
   Zircon name in a comment, rather than Zircon's names.
