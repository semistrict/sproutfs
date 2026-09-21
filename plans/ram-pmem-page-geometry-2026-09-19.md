# RAM and PMEM page geometry — 2026-09-19

**Status: steps 1 to 4 and 6 are implemented, but for step 1's baseline
measurement and step 4's mapping budget; steps 5 and 7 are planned.**

## Decision

Separate RAM and PMEM backing and make RAM's unit of ownership and write
isolation 4 KiB. Keep large read-only mapping batches: a contiguous 2 MiB run
of inherited RAM can be mapped together, while a store copies and replaces
only the affected 4 KiB page. The other 511 pages retain their identities and
remain shared, including after the writer publishes a checkpoint.

| Property | RAM | PMEM |
| --- | --- | --- |
| Resident ownership, COW, dirty tracking and publication | 4 KiB | 2 MiB, unchanged |
| Arena backing | Separate ordinary memfd | Separate 2 MiB HugeTLB memfd |
| Read-only mapping | Batch contiguous pages into runs, targeting 2 MiB | Whole pages, optionally batched |
| Storage | Separate RAM volume and page table | Separate volume and page table per device |

RAM and PMEM pages never need to share a memfd or an allocator. Use one pager
implementation instantiated with fixed geometry for each arena. Keep the
host's aggregate resource accounting in bytes. Page counts belong to their
individual pager or volume and cannot be added as though their units matched.

The storage layer already has `ram0` and separate PMEM volumes. Give each
volume explicit, immutable page geometry, carried through its checkpoints,
forks and migrations. The host selects the geometry when creating a volume;
the storage implementation should not infer it from a string such as `ram0`.
One VM checkpoint continues to capture and publish all volumes together.

PMEM does not change: its page stays 2 MiB, on the explicit HugeTLB arena it
has today, and this plan touches only RAM. The divergence is in RAM — of the
128 MB a fork held privately in the measurement below, 114 MB was RAM and
10 MB was the root — and the root is mounted `noatime` so that a fork that only
reads does not write the inodes of what it reads.

The RAM arena is ordinary memory, which a host with swap may swap. The host
does not refuse to start with swap on.

## Mapping size and ownership size

A read-only 2 MiB mapping batch consists of 512 independently owned RAM pages.
One contiguous mapping requires contiguous guest addresses and arena offsets;
fragmented backing is represented by multiple runs. The existing mapping
protocol already expresses runs and batches, although its validation and
arithmetic currently assume a universal 2 MiB page.

The write path must reserve, copy, remap and charge only one 4 KiB RAM page.
Read-ahead must not make its neighbors writable or privately dirty. In
particular, disable RAM write-ahead initially: the current optimization grants
writable private pages to neighboring fresh zero pages before they are used.
Checkpoint protection and retirement must preserve this isolation too.

A mapping batch is not a promise of a physical huge page or huge translation.
The baseline RAM implementation uses ordinary backing that supports 4 KiB
replacement. Explicit HugeTLB backing stays in the PMEM arena. Transparent
huge-page promotion or a strategy for splitting true huge mappings is separate
performance work and must preserve the 4 KiB ownership contract.

## Keeping RAM's mappings whole

Every separately mapped run of a guest's RAM is a mapping in its VMM process,
and a private 4 KiB page written into the middle of an inherited run makes three
of one. The kernel caps a process at 65,530 mappings by default, which a guest
scattering small writes over gigabytes can pass, but the cap is the last of what
fragmentation costs: each separate dirty run is one write-protect command in the
checkpoint's pause, the kernel's own mapping operations slow as their number
grows, and a fragmented range can never be given a huge mapping. So the design
is not a budget that merges when it is exceeded — that does nothing until the
limit and then puts a copy of up to 2 MiB on the fault path of every store, at
the moment the guest is busiest, and has to search every range for the one to
merge. It is three rules that hold all the time, and a budget behind them.

- **A private page lives at its own offset.** Each 2 MiB-aligned range of a
  region that holds a private page has one private extent in the RAM arena, 2 MiB
  of arena offsets of which only the pages stored into hold memory, and a
  private page of that range is put at the offset within the extent that it has
  within the range. Private pages that are adjacent in the guest are then
  adjacent in the arena and are one mapping, whatever order they were written
  in: what a range costs in mappings is how often it alternates between shared
  and private, not how many pages of it are private. The arena accounts pages,
  not extents; an extent is offsets.
- **A store closes a small gap.** When a store lands within `gap` pages of a
  private run of the same range, the shared pages between them are made private
  in the same fault, in one copy and one mapping command. Writes cluster, so the
  unit a store copies grows where the guest is writing and stays 4 KiB where a
  write is alone. A store never closes a gap across a range's boundary. The
  worst case is a guest writing one page in every `gap + 1`, which costs
  `gap + 1` times what it wrote, against 512 times at a 2 MiB page.
- **A range that is half private becomes private.** When a range's private
  pages reach 256, the rest are copied into the holes of its extent and the
  range is one mapping, one write-protect command at a seal, and eligible for a
  huge mapping. Nothing that was already private is copied, because it is
  already where it belongs. It costs at most twice what the guest wrote there.
  The 2026-09-19 and 2026-09-21 fan-outs say this is nearly free, because writes
  are bimodal: of the 97 ranges a fork running `git grep` in a 16 GiB guest wrote
  into, 17 were at least half written and held 57 % of every changed page, and
  making those whole would have added 5 MiB to 49; of the 38 a fork running
  `memprobe 16` wrote into, 8 held 75 %, and the cost would have been 1 MiB. The
  other ranges are barely touched — a median of 8 to 52 pages of 512 — and they
  are what a 4 KiB page is for.
- **The budget is a backstop.** A region still counts its mappings against a
  budget below the kernel's cap, and a store that would pass it makes the range
  with the most mappings private first. It is expected never to act, and a
  counter says when it has.

`gap` is measured rather than chosen: the fan-out records how many changed pages
each range holds but not where they are, so it gains the lengths of the private
runs and of the gaps between them, and `gap` is set from the codex workload —
the smallest value past which the mappings a fork holds stop falling. The
unchanged-pages settle undoes what these rules copied and the guest never wrote:
a page made private to close a gap or to fill a range has its origin like any
other copy, and a settle that finds it unchanged hands it back — unless its
range has been made whole, which a settle leaves whole.

It is a step of its own, after step 4: the pager's placement and the three rules
first, in the simulation and on Linux; then the same accounting in the simulated
mapping model, so the campaigns exercise them under scattered stores. It is
proved by exact counts — the mappings a range holds after each pattern of
stores, what a store copied, what a seal protected in how many commands — by the
sharing gauges before and after, by the byte model in every campaign, and on GCE
by the mappings, the protect commands and the pause of the codex workload with
the rules on and off.

## Why this change needs measurement

The concern is retained sharing during ordinary work: search, git commands,
dependency installation, builds and idle guest activity. The
[recorded workload](../docs/measurements-2026-09-14-workload.md) measured dirty
bytes at 2 MiB granularity, but did not measure the corresponding 4 KiB dirty
set or retained physical sharing. Its `shared` counter counts identity hits;
it is not a gauge of how much memory remains shared. Checkpoint dirty totals
also include repeated writes and newly allocated pages, so they do not measure
loss of inherited sharing.

One measurement exists. Four forks of one parent each ran `git grep` over a
repository, in Lima, a single run per page size, eight days of code apart
(`docs/measurements/page-unit-2026-09-11.json` for 4 KiB, before the page was
fixed; `page-unit-2mib-2026-09-19.json` for 2 MiB):

| Per fork | 4 KiB page | 2 MiB page |
| --- | --- | --- |
| RAM private | 8.7 MB (2,221 pages) | 114 MB (57 of the 67 pages it touched) |
| Root private | 40 KB (10 pages) | 10 MB (5 of 18) |

At 2 MiB a running fork owns 85 % of the RAM it touches. The same 4 KiB run is
what the small page costs: 56,694 of its 56,703 boot faults were copy-on-writes
— reads were already batched by read-ahead — and its capture paused for 1.4 s
over 88,407 sealed pages, against 22 ms over 239 today. Those are the numbers
the write path and the seal of step 4 have to bring down.

The fan-out now records which pages each fork came to own and, against a fork
of the same point that never ran, which 4 KiB blocks of them changed
(`fork_own_pages`, `fork_changed_blocks`, `fork_changed_counts`). On an x86-64
GCE host (`gce-fanout-2026-09-19/`), four forks each running `busybox uname`:

- **RAM divergence is real writes.** A fork owned about 30 RAM pages, 60 MB,
  and all but one to three of them had been written — about 450 changed 4 KiB
  blocks, 1.8 MB. That is the saving a 4 KiB unit of ownership is for.
- **A write fault is not always a write.** Every root page a fork touched
  became its own, with not one byte changed. On x86-64, KVM finishes a guest
  fault that has to wait for the pager from a worker thread,
  `async_pf_execute`, which asks for the page writable whatever the guest's
  access was (`virt/kvm/async_pf.c`, unchanged in mainline 7.3-rc3; a 2020 patch
  to pass the access through was never merged, and `no-kvmapf` in the guest does
  not avoid the path). On aarch64 the smaller version is the guest kernel's
  cache maintenance on a page it executes for the first time, which the
  architecture reports as a write: in Lima a fork that ran `git --version`
  owned the pages holding `git` and `libpcre2`, unchanged. Only the page that
  faults is forced; pages mapped read-only ahead of a fault — populated at
  attach, or brought by read-ahead — are read without waiting and stay shared.
  So with a 4 KiB page under a 2 MiB read-ahead run this costs RAM one page in
  512, and it costs PMEM, at 2 MiB, a whole page for every cold fault. It is
  recorded in [open-work.md](../docs/open-work.md) and is not this plan's to fix.
- **Real writes scatter on the root.** One small file written and synced
  changed 24 blocks, 96 KB, in seven places — superblock, bitmaps, inode table,
  two directories, the data block and the journal — and made ten root pages,
  20 MB, the fork's own. PMEM stays 2 MiB by decision; this is what that costs.

The [earlier memory probe](../docs/measurements/hugetlb-2026-09-11.md) found
substantial first-touch benefits from huge pages in its nested-KVM environment.
Large mapping batches can reduce command overhead, but do not establish that
those translation and first-touch benefits will survive this change. Measure
memory savings and workload time together.

## Implementation sequence

1. **Establish the comparison.** *Done, except the baseline.* `Host.Sharing`
   reports unique resident bytes, mapped resident bytes and the difference,
   split by RAM and PMEM; a region carries its kind, stated by whoever attaches
   it. `RegionStats` adds the resident pages another region maps, and the host
   adds a VM's private bytes up across its regions for `/status`, `/metrics` and
   the VM listing. The gauges count every alias, including two regions of one VM,
   and resident sharing only: inheritance of pages neither VM has faulted in is
   not in them. `Stats.IdentityHits` and `Stats.CopyOnWrites` stay as they were.
   The fork fan-out records the per-region gauges beside `fork_region_pages`.
   What is left is recording an unchanged baseline with the existing workload
   flow, which needs a KVM host.

2. **Make volume geometry durable. Done.** Extend volume specifications and
   checkpoint volume metadata with a validated page size. Carry it through
   create, open, fork, cold boot/resize and compaction. Update overlay dirty
   enumeration, `Locate`, page identity offsets, publication and reads to use
   that volume's geometry. Compaction preserves the original identity and
   page size. Changed 4 KiB pages must not give unchanged neighbors a new
   identity. Start in `internal/volume` and `internal/checkpoint`.

   Explicitly choose and version page-table segment geometry. Today's 256
   entries cover 512 MiB at 2 MiB per page but only 1 MiB at 4 KiB per page;
   keeping 512 MiB coverage instead requires 131,072 entries. Audit root and
   segment size limits, metadata budgets and supported VM sizes before fixing
   the layout. Do not silently reduce supported capacity by a factor of 512.

   **What was built.** `checkpoint.Geometry` is a volume's page size and the
   pages one segment of its page table covers; `checkpoint.GeometryFor` accepts
   4 KiB and 2 MiB and refuses everything else with `ErrInvalidConfig`. A
   volume's page size is stated in its `VolumeSpec` when the VM is created,
   recorded in the root beside the volume's size, read back from there by every
   reader, and inherited by open, fork, cold boot and compaction; nothing infers
   it from a volume's name. Index format 8 carries it, and a version 7 root is
   refused with the version it names before anything is mapped or served. The
   part layout did not change.

   **Segment geometry chosen:** 256 pages at 2 MiB per page, unchanged at
   512 MiB of volume per segment, and 16,384 pages at 4 KiB, which is 64 MiB
   per segment. That is sixteen root entries — about 240 bytes — per GiB of a
   4 KiB-page volume, an encoded segment of about 330 KiB and at most 560 KiB
   against a `maximumSegmentSize` of 1 MiB, and a `maximumRootSize` of 2 MiB
   that admits about 8.5 TiB of such a volume. A small page also brought
   `maximumIndexSize` within reach — a checkpoint writes about 5 MiB of segments
   per GiB of a 4 KiB-page volume it dirtied, and at 64 MiB one that dirtied
   more than about 12 GiB at once was refused, which would have stopped the VM.
   So the bound is 1 GiB, which no dirty budget reaches, and an open no longer
   reads the index object whole: the object ends with the record it begins
   with, and an open is one read of its last 256 KiB, as a part's table is.

   The pager, the wire protocols and the VMM are untouched, so every volume a
   host creates is still a 2 MiB-page volume and the pager refuses one of any
   other page size when it is attached. The simulation campaigns run through
   that pager, so they stay at 2 MiB; a 4 KiB-page volume goes through
   checkpoints, forks, compaction and reclamation in `internal/volume` and
   `internal/checkpoint` instead, and reaches the campaigns when step 3 gives
   the pager its own geometry.

3. **Separate pager instances and arenas. Done.** Parameterize
   `internal/vmmemory` by a fixed page size per instance, including its arena,
   spill slots, reservations, identities, read-ahead and buffer bounds. Assemble
   RAM and PMEM instances in `internal/host`, and route each region to the
   correct one. Share the host's byte resource budget without double counting
   its capacities. Audit dirty/logical budget configuration, memory admission,
   pressure callbacks and shutdown for both instances. A checkpoint requested
   by either pager still seals the entire VM; the loss window spans both.

   **What was built.** `vmmemory.Config.PageSize` is a pager instance's page,
   validated by `checkpoint.GeometryFor` so that a pager and the volumes it maps
   cannot disagree about what a page number means; the package constant is gone
   and every arena slot, spill slot, budget, buffer, gauge conversion, region
   check and fault calculation reads the instance's. `vmmemory.Pagers` is a
   host's pager per kind, and `internal/host`, `internal/vmmachine` and
   `internal/simtest` route each region to the pager of its kind. The host
   answers both pagers' pressure through the same three callbacks, which act on
   the VM a region belongs to: one pause seals every region it maps in both
   pagers, its loss window is the oldest unpublished write across both, and a
   stall in either stops that VM alone. `Host.AdmitRegions` charges each region
   to its own pager's cap, `Status.LogicalPagesFree` reports the two apart, and
   `hostapi.Pager` became a report per kind with byte totals across them; every
   pager metric now carries the `kind` label it already had on the sharing
   gauges, beside one `sproutfs_pager_arena_bytes` total.

   **Budget split chosen:** `SPROUTFS_ARENA_BYTES` stays the host's whole
   resident store and `SPROUTFS_SPILL_BYTES` its whole spill store;
   `SPROUTFS_RAM_SHARE_PERCENT` divides both, and the logical and dirty budgets
   with them, defaulting to 75 % to RAM — the 2026-09-19 fan-out measured a fork
   holding about 114 MB of RAM privately against 10 MB of root, and one shared
   root stands behind every fork's own RAM. PMEM takes the remainder rather than
   a second rounding, so the two shares come to exactly what the host was given;
   a share that cannot divide either store into whole pages of both pagers is
   refused. The logical and dirty caps are named per pager,
   `SPROUTFS_RAM_LOGICAL_PAGES` and its three siblings, because each is counted
   in its own pager's page.

   **Production page:** both pagers run 2 MiB. `vmwire`, the Rust adapter and
   Firecracker map that page and nothing else, and the Linux connection refuses
   a pager of any other page at session setup — the seam step 4 removes. The
   simulation has neither a HugeTLB pool nor that wire, so it runs the target
   geometry now: a 4 KiB RAM pager beside a 2 MiB PMEM one, with RAM volumes
   created at 4 KiB and their byte sizes shrunk so page counts, and run times,
   stay where they were.

   **Migration, partly:** a page number is now the page of the volume it names
   rather than the page server's, so `vmmigrate.Pages` states its page size, a
   reply and a resident listing are counted in it, a request's page cap is
   bytes, and a destination's peer backing takes its page from its own volume.
   `SourceConfig.PageSize` is only the budget's unit now. The handoff still
   carries one page size for the whole VM and the wire format is unchanged,
   which is step 5.

4. **Support small RAM mappings with large runs. Done, but for the mapping
   budget.** Update `internal/vmwire`,
   `rust/sproutfs-vm-memory` and the Firecracker integration together. Validate
   each session's geometry and backing type, and configure RAM without the
   current mandatory HugeTLB setting. Use 4 KiB RAM generations, fault
   alignment, protection and replacement, while mapping/protecting contiguous
   runs in batches. Verify ordinary-memfd userfaultfd support and required
   missing/minor/write-protection features on the qualification kernels.
   Exercise sparse zeros, partial replacements and VMA-budget refusal.

   **What was built.** The geometry is the session's, stated on the wire.
   Mapping protocol **version 7** moves the page out of the version and into
   ATTACH, which now carries this region's page size and the kind of memory its
   arena is made of — 1 for an explicit 2 MiB HugeTLB memfd, 2 for an ordinary
   shared memfd — beside the arena and the mapping-count budget. The pager
   refuses a page this transport does not map and an arena whose slot is not its
   own page; the client refuses a page it does not map, an arena kind that is
   not the page's, a descriptor whose filesystem is not what was claimed, and a
   region of its own that is not whole pages of it, all before it exposes an
   address to the VMM. A version 6 peer fails on the version. The client reserves
   its region at the larger of the two pages before the attachment arrives, which
   is aligned for both, so the frame order did not change; `vmwire` split into a
   portable half, so the frames and the geometry checks are tested anywhere.

   **The arena.** `NewLinuxArena` is parameterised: a 2 MiB slot is a HugeTLB
   memfd of the pool and a 4 KiB slot an ordinary one, named after its page so
   `/proc` says which memory a guest's mapping is really on. `ramPageSize` is
   4 KiB, `host.RAMPageSize` and `host.PMEMPageSize` are the one place either is
   stated, and RAM's write-ahead is one page whatever its budget: a run that made
   a store's neighbours privately dirty before the guest used them would give
   back exactly the sharing the small page buys. Runs and per-run protection
   needed nothing new — the plan already batched a run of consecutive pages in
   consecutive slots into one command, the slot allocator already prefers
   consecutive runs, and a seal already protects per run — so at 4 KiB a fork
   maps a 512-page run with one command and a store copies one page.

   **The crate.** `PAGE_SIZE` became `MIN_PAGE_SIZE`/`MAX_PAGE_SIZE` and a
   per-session `page_size()`; every offset, length, backing offset and generation
   is counted in it. The UFFD negotiates HugeTLB *and* shmem missing, minor and
   write-protect features together, because a host runs a pager of each kind, and
   names the ones a kernel lacks by asking a second descriptor what it supports.
   A trap range prepared out of the middle of the region keeps its phase within
   the larger page, so a revoked 4 KiB page still merges with the traps around it
   instead of costing a mapping.

   **Firecracker.** Managed RAM takes no `huge_pages` setting at all — the pager
   owns that memory and states its page when the session attaches — on the boot
   path and the restore path alike, and `volume_ranges` checks the guest's
   regions against the page the session states rather than a constant. PMEM is
   unchanged. Snapshot save and restore of a managed VM are unchanged and pass.

   **The settle.** Qualifying this at 4 KiB found a defect the 2 MiB page had
   hidden. A settle re-shared an unchanged page by installing the origin over
   the page the guest still mapped — one command, no fence — and that corrupted
   a guest: two children of one fork point, reading everything they inherited on
   one destination pager while each was checkpointed every 250 ms, panicked in
   the guest kernel's timer wheel on an already-removed list entry. Reducing it
   ruled out eviction, slot reuse, a shared page being written and a private
   page reaching two regions; revoking the page instead of replacing it removed
   it, and revoking *and then* replacing it did not, which is what says the
   replacement rather than the fence is the unsafe part. A settle now only
   revokes, and the guest's next access maps the origin through the fault path.
   The cost is one fault per page a settle re-shares.

   **What is left.** `ConnectionConfig.MaxVMAs` is still only the client's
   admission limit: the pager does not count the mappings a region holds and does
   not merge a scattered 2 MiB range into one private run when a store would
   exceed the budget. It is in [open-work.md](../docs/open-work.md), beside the
   read-ahead run that becomes one page under arena pressure.

5. **Carry geometry through handoff and migration.** Make page size a property
   of each served volume/region rather than the whole page server. Update
   request validation, page numbering, resident listings, unpublished runs,
   transfer completion and destination checks in `internal/vmmigrate`.
   Bound requests and in-flight data in bytes, allowing batches of 4 KiB RAM
   pages. Reject a mismatch before mapping or accepting guest data.

6. **Keep storage and network I/O batched. Done.** Pack small dirty pages into
   bounded parts. Preserve 4 KiB identities without a PUT per page. Coalesce
   adjacent cold reads within a part so a 2 MiB RAM read-ahead does not
   automatically produce 512 object-store GETs. Measure member/table overhead,
   compression work, metadata growth and requests per workload. Shared parts
   and one checkpoint root can still hold both kinds of volume.

   **What a run costs, before and after.** A read of a range of a volume is one
   run of pages, and it is now grouped by the part its members are in and by
   where in that part they sit. The three cases, counted through the object
   store, each over the 2 MiB run of 512 4 KiB pages a RAM pager loads at once
   and each including the one read of the segment that locates them:

   | A 2 MiB run of 512 4 KiB pages | Before | After |
   | --- | --- | --- |
   | All 512 published by one checkpoint in page order | 513 reads, 2,123,845 B | **2** |
   | Interleaved over three checkpoints | 513 reads, 2,123,949 B | **4** |
   | Sparse — half the pages never written | 257 reads, 1,062,050 B | **2** |

   End to end through the pager, a cold read-ahead run of 512 RAM pages — one
   `Region.Fault` on a VM a second host has just opened — went from **513
   requests to 2**. A run of one checkpoint's pages crossing a page-table
   segment boundary is 3: the segment boundary is a boundary of the page table,
   not of the part. A run spread over N parts is one request per part, fetched
   at once rather than one after another.

   **What was built.** A publication already wrote a volume's changed pages in
   ascending page order, so the members of consecutive pages are adjacent in its
   part by construction; that is now asserted of the part itself rather than
   assumed. `Store.Read` resolves a range to a run of pages, holding one segment
   across the pages it locates, and groups the members it needs by `(checkpoint,
   part)` and by offset into extents: one ranged read each, decoded out of that
   one buffer. Two members of one part separated by up to 64 KiB of bytes
   nothing wants are read through rather than split at — a request costs its
   latency and not its length, and 64 KiB is sixteen 4 KiB members, enough to
   hold a run together across the few pages a later checkpoint rewrote in the
   middle of it — and an extent is capped at 4 MiB, which admits a whole 2 MiB
   run with room for its envelopes. Independent parts are fetched at once,
   within the cache's own `MaxConcurrentLoads`. A run itself covers at most
   16 MiB of volume in whole pages, which bounds what one reader holds decoded:
   a longer read is several runs, and 16 MiB is the largest read-ahead run a
   pager may be configured with, so no pager's load is ever split.

   **The cached unit stays the member**, and the cache's batched path is what
   keeps it there: a run is one cache operation, holding one load slot however
   many pages it is missing, so the pages it fetched are retained under their own
   identities, two readers of one page share the one copy whichever run carried
   it, and concurrent readers still coalesce per page. An extent has no identity
   — which members it carries depends on which run asked for it — so caching
   extents would give two readers of overlapping runs two copies of the pages
   they share and make a half-cached run fetch the half it has.

   **Parts fill to their target at 4 KiB.** `maximumTableSize` is 1 MiB. A
   64 MiB part of 4 KiB pages holds 16,384 members, and one entry of a volume
   named as briefly as `ram0` costs about 29 bytes — the repeated field's tag
   and length prefix, the name, and the page, offset, length, state and two
   origin fields, which are written whether or not they are zero. A megabyte is
   therefore about 36,000 such entries, or about 3,700 of the widest kind a
   255-byte volume name makes, and a full part of small pages spends about
   470 KiB of it. At 256 KiB such a part was sealed after about 8,700 members,
   some 34 MiB, so a checkpoint of small pages cost about twice the PUTs its
   bytes needed. A reader still takes a part's whole table in one suffix read,
   now of 1 MiB plus the 32-byte trailer: still one round trip, and one nothing
   on the page path makes, because the root's segments carry the same offsets.

   **Compression stays per member.** Nothing was changed there: a member is its
   own raw-or-Zstandard envelope, which is what makes one page one decode out of
   an extent that holds many. The overhead a small page pays is the 48-byte
   envelope header plus the 29-byte table entry — 77 bytes on 4,096, about
   1.9 % of a checkpoint of whole 4 KiB pages — and grouping members into one
   compressed block would have to beat that while making every read of one page
   decode its neighbours too. It was not changed speculatively.

   **No format version moved.** No encoding changed: the part layout is version
   4 and the index format 8, and the committed fixtures are byte-identical. The
   table bound is the store's rather than the part layout's — the layout
   package does not know how large a part may be — so raising it is not a change
   to the format.

7. **Qualify and document.** Extend the simulation harness to run both page
   geometries in one VM and one host. Run the Linux pager and Firecracker
   suites, then repeat the baseline workload on the same host configuration.
   Update the architecture, terminology, volumes, VM-memory, hosting and
   migration documents when the implementation lands. Historical measurement
   reports retain their original geometry and environment.

## Format and rollout

There is no stored data to keep, so nothing is converted. Bump the version of
the checkpoint index and of the mapping and handoff formats, so an old 2 MiB
RAM page number cannot be read as a 4 KiB page number; a reader given an
earlier version fails clearly, before a guest starts, and that is the whole of
its support for one. The checkpoint index is at version 8, which is the store's
half of this and is done; the mapping format is at version 7, which is step 4's
and is done, and the handoff format is step 5's. Regenerate protocol code from its source
schemas with the repository's generator; do not edit generated files.

## Acceptance

- A fork initially shares an inherited 2 MiB RAM run. A write to one 4 KiB
  page consumes exactly 4 KiB of additional private resident backing, leaves
  the parent's bytes intact and keeps the other 511 pages shared. Assert this
  before and after checkpoint publication, spill/refault, restore and a
  subsequent fork. This is a correctness test, not a workload prediction.
- A read-only contiguous 2 MiB RAM run is installed with bounded batched
  command overhead, rather than one round trip per 4 KiB page. A fragmented
  run remains correct under mapping-budget pressure.
- Mixed RAM/PMEM checkpoints, same-host and remote forks, migrations and host
  loss preserve the byte model, page identities and the common checkpoint
  boundary. Cancellation and failure release both arenas' reservations.
- Real Linux tests verify physical sharing and 4 KiB replacement through KVM;
  simulation alone cannot qualify kernel mapping behavior. Mismatched
  geometry and incompatible formats fail before exposing memory.
- Run the repository's required `just check` gate, focused race tests and
  relevant simulation campaigns for implementation changes. New regression
  tests assert correct behavior and fail against the faulty implementation.
- Repeat idle, search, history, offline install and build workloads with the
  same fork points and placement. Report retained sharing, incremental bytes
  per fork, peak residency, spill, first-touch and workload time, mapping/fault
  counts, metadata memory, checkpoint bytes and object requests, separately
  for RAM and PMEM. Include warm forks taken after installation. Use those
  results to assess the memory/performance tradeoff; do not infer it from
  cumulative sharing hits or compressed upload size.
- Clean up any cloud resources created for qualification and verify removal.
