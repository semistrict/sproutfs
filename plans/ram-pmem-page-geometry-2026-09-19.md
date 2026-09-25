# RAM and PMEM page geometry — 2026-09-19

**Status: steps 1 to 4, 6 and 8 are implemented, but for step 1's baseline
measurement, and so is keeping RAM's mappings whole — the arena's offsets, the
three rules and the budget behind them; steps 5 and 7 are planned. Step 4 has
one open defect against it — a fan-out panics a child's guest kernel at 4 KiB —
which [open-work.md](../docs/open-work.md) carries and which must be understood
before this geometry ships.**

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
Read-ahead must not make its neighbors writable or privately dirty. RAM
write-ahead was disabled with it initially, and that was wrong: write-ahead
serves fresh zeros and nothing else, and a hole is shared with nobody, so making
a store's neighbours private gives back no sharing at whatever page. RAM takes
the same 8 MiB run as PMEM, bounded by free slots, free dirty reservations and a
dirty budget that must hold 64 such runs, and what bounds its cost is the
untouched ahead page: it reads back as zeros, the publication gives it no object,
and the retire — which is where the pager learns what the volume holds no object
for — hands it back as the hole it was, slot and reservation with it. Checkpoint
protection and retirement must preserve the 4 KiB isolation too.

A mapping batch is not a promise of a physical huge page or huge translation.
The baseline RAM implementation uses ordinary backing that supports 4 KiB
replacement. Explicit HugeTLB backing stays in the PMEM arena. Transparent
huge-page promotion or a strategy for splitting true huge mappings is separate
performance work and must preserve the 4 KiB ownership contract.

## Keeping RAM's mappings whole — **done**

**What was built.** The arena's offsets and its pages are two numbers and the
memfd is sized to the first: `Config.ArenaOffsets` is the address space,
`Config.ResidentPages` the capacity, and the file is sparse, so an offset costs
nothing until a page is put there and `Release` punches it back out. The
supervisor sizes RAM's offset space at one extent per range any memory region it admits
may write into — `LogicalPages`, a range being 512 pages and an extent 512
offsets — plus `ResidentPages` for the read-ahead runs; PMEM keeps its offsets
and its pages one number. ATTACH's length is that offset space rather than the
capacity, which is a change of meaning, so the mapping protocol is at
**version 8** and the Rust client checks the descriptor's own size against it.

`slots.Space` carves the offsets past its pages into aligned extents and hands
them out and takes them back whole; a page put in one moves the page budget and
not the address. The host keeps an extent per `(memoryRegion, range)` and gives it
back when its last page goes, so a memory region owns an extent for exactly as long as
it has a page in that range. All three rules and the backstop are in
`vmmemory/placement.go` and `rules.go`, counted in
`placement_test.go`, `rules_test.go` and `internal/simtest/placement_test.go`,
whose model now accounts mappings the same way.

**Measured in Lima, aarch64, 4 KiB RAM.** The fork fan-out
(`TestFirecrackerForkFanOutServesBothChildrenAtOnce`, two children of one point,
each reading all of its memory and all of its root twice) holds this many
mappings in each child's VMM after the read phase:

| Fan-out at 4 KiB | before (`5d7029e`) | after |
| --- | --- | --- |
| child 0's VMM mappings | 4,485 | **3,299** |
| child 1's VMM mappings | 4,631 | **3,288** |
| the pager's copy-on-writes | 167,084 | 154,528 |

with 28 private extents, 2,723 pages copied by the two rules, and the backstop
never acting. A quarter to a third of a child's mappings go, and the faults go
with them: a gap closed in one fault is a run the guest stores into without
faulting again. Both readings are one run each on the same instance, so they
carry that instance's noise; what they are not is a controlled pair, because the
instance was busier for one of them — the run phase took 4m33 before and 3m40
after, and the bound it is measured against is a liveness bound rather than a
speed one.

**The boot survey at 16 GiB** (`TestBootSurveyOfPrivateRAMPages`, same instance)
says what a write-ahead run is worth at this page, and the rules do not touch
it: a store into fresh zeros has no page to copy from, so neither rule fires.

| A 16 GiB boot at 4 KiB | write-ahead 1 page | write-ahead 8 MiB |
| --- | --- | --- |
| faults | 109,825 | **218** |
| boot | 1 m 21 s | **21.6 s** |
| private pages | 109,825 | 111,616, of which 111,398 were written ahead |

**Three departures, each in the plan's own terms.** The extents are carved from
the offsets past the capacity rather than found anywhere in the space, so an
extent's base is a multiple of its size and a private page's offset and its page
number agree modulo the extent — which is the invariant the tests read, and
which makes the allocation a free list rather than a scan. A store that copies
away from the copy a checkpoint froze takes an ordinary offset, because its own
offset is holding the bytes that checkpoint is uploading and moving them would
mean a copy and a fence in the middle of an upload; so does a page a migration
destination loads privately from the host that still holds it, which arrives in
a run like any other load. And the backstop makes the range the guest is writing
in whole rather than the range with the most mappings: the pager does not count
a memory region's mappings — the client does, which is what refuses the command — and
the range the store is in is the one whose alternations that store is adding to.

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
  memory region that holds a private page has one private extent in the RAM arena, 2 MiB
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
- **The budget is a backstop.** A memory region still counts its mappings against a
  budget below the kernel's cap, and a store that would pass it makes the range
  with the most mappings private first. It is expected never to act, and a
  counter says when it has.

**`gap` is 16 pages.** It was to be measured rather than chosen, and it now is.
The fan-out records where a fork's private pages lie, not only how many each
range holds: `fork_ram_geometry` carries, per memory region, the number of private
runs, a histogram of their lengths, a histogram of the gaps between consecutive
runs of one range, how many ranges hold a private page and how many are at least
half private, in the fixed buckets 1, 4, 16, 64, 256 and 512 pages
(`newPrivateGeometry` in `vmmachine/bench_linux_test.go`). The
2026-09-21 4 KiB smoke on the qualification instance recorded 1,979 gaps, of
which 1,532 — 77 % — are 16 pages or fewer, and 19 of the 197 ranges a fork
touched were already half private. Sixteen is therefore the bucket boundary at
which three quarters of the alternations a fork holds stop being alternations,
and it bounds the worst case at 17 times what a guest wrote, against 512 times
at a 2 MiB page. Re-measure it by re-running the fan-out at 4 KiB and reading
`fork_ram_geometry.gaps` out of the record: the right `gap` is the smallest
bucket boundary past which the run count stops falling, and the buckets are
carried in the record beside the counts so two runs compare directly.

The unchanged-pages settle undoes what these rules copied and the guest never
wrote: a page made private to close a gap or to fill a range has its origin like
any other copy, and a settle that finds it unchanged hands it back — unless its
range has been made whole, which a settle leaves whole.

It is a step of its own, after step 4: the pager's placement and the three rules
first, in the simulation and on Linux; then the same accounting in the simulated
mapping model, so the campaigns exercise them under scattered stores. It is
proved by exact counts — the mappings a range holds after each pattern of
stores, what a store copied, what a seal protected in how many commands — by the
sharing gauges before and after, by the byte model in every campaign, and on GCE
by the mappings, the protect commands and the pause of the codex workload with
the rules on and off.

**What had to change first, and was not known when this was written — done.**
The placement rule needs an arena whose *offsets* are not its *pages*. A memory region's
range that holds one private page owns 512 consecutive arena offsets, of which
one holds memory, so the offsets a host's guests need are bounded by the ranges
they have written into and not by the memory the arena may hold: at the
deployment's 9 GiB RAM dirty budget the worst case is 2,359,296 extents, which
is terabytes of offsets behind gigabytes of pages. The two used to be one number
— `Config.ResidentPages` was both the arena's slot count and its capacity,
`slots.Set` held one bit per slot, `Host.residentLeases` one entry per slot,
the Linux arena's memfd was sized to it, and ATTACH stated that size on the wire
for the client to check. Decoupling them was the first piece of this step:
the arena is a sparse offset space with a page budget enforced where it already
was, in the resource lease `takeFree` acquires. Until that was done,
rules 2 and 3 bought nothing on their own — a run of private pages is one mapping
only if its arena slots are consecutive, which is what rule 1 is for.

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

  **That one page in 512 was not what a fork paid until 2026-09-22.** Read-ahead
  never ran for those faults: the store path read the one page it was copying
  from and nothing else, so a fork's first pass over its memory was one fault,
  one round trip and one private page per 4 KiB — the GCE fan-out that day took
  21,130 faults of which 20,016 were copy-on-writes, and 10,313 loads for 35,257
  pages. A store reads its whole window in now, exactly as a read fault does and
  on the same terms — free slots only, one call to the volume, the run installed
  shared and read-only — and then copies the one page the guest stored into,
  which is never mapped read-only first, so a store still costs no revocation.
  A migration destination's backing stays a page at a time: whether the source
  still holds a page is an answer only a load gives, and it gives it per page.
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
   split by RAM and PMEM; a memory region carries its kind, stated by whoever attaches
   it. `MemoryRegionStats` adds the resident pages another memory region maps, and the host
   adds a VM's private bytes up across its memory regions for `/status`, `/metrics` and
   the VM listing. The gauges count every alias, including two memory regions of one VM,
   and resident sharing only: inheritance of pages neither VM has faulted in is
   not in them. `Stats.IdentityHits` and `Stats.CopyOnWrites` stay as they were.
   The fork fan-out records the per-memory-region gauges beside `fork_memory_region_pages`.
   What is left is recording an unchanged baseline with the existing workload
   flow, which needs a KVM host.

2. **Make volume geometry durable. Done.** Extend volume specifications and
   checkpoint volume metadata with a validated page size. Carry it through
   create, open, fork, cold boot/resize and compaction. Update overlay dirty
   enumeration, `Locate`, page identity offsets, publication and reads to use
   that volume's geometry. Compaction preserves the original identity and
   page size. Changed 4 KiB pages must not give unchanged neighbors a new
   identity. Start in `volume` and `checkpoint`.

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
   checkpoints, forks, compaction and reclamation in `volume` and
   `checkpoint` instead, and reaches the campaigns when step 3 gives
   the pager its own geometry.

3. **Separate pager instances and arenas. Done.** Parameterize
   `vmmemory` by a fixed page size per instance, including its arena,
   spill slots, reservations, identities, read-ahead and buffer bounds. Assemble
   RAM and PMEM instances in `host`, and route each memory region to the
   correct one. Share the host's byte resource budget without double counting
   its capacities. Audit dirty/logical budget configuration, memory admission,
   pressure callbacks and shutdown for both instances. A checkpoint requested
   by either pager still seals the entire VM; the loss window spans both.

   **What was built.** `vmmemory.Config.PageSize` is a pager instance's page,
   validated by `checkpoint.GeometryFor` so that a pager and the volumes it maps
   cannot disagree about what a page number means; the package constant is gone
   and every arena slot, spill slot, budget, buffer, gauge conversion, memory region
   check and fault calculation reads the instance's. `vmmemory.Pagers` is a
   host's pager per kind, and `host`, `vmmachine` and
   `internal/simtest` route each memory region to the pager of its kind. The host
   answers both pagers' pressure through the same three callbacks, which act on
   the VM a memory region belongs to: one pause seals every memory region it maps in both
   pagers, its loss window is the oldest unpublished write across both, and a
   stall in either stops that VM alone. `Host.AdmitMemoryRegions` charges each memory region
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
   ATTACH, which now carries this memory region's page size and the kind of memory its
   arena is made of — 1 for an explicit 2 MiB HugeTLB memfd, 2 for an ordinary
   shared memfd — beside the arena and the mapping-count budget. The pager
   refuses a page this transport does not map and an arena whose slot is not its
   own page; the client refuses a page it does not map, an arena kind that is
   not the page's, a descriptor whose filesystem is not what was claimed, and a
   memory region of its own that is not whole pages of it, all before it exposes an
   address to the VMM. A version 6 peer fails on the version. The client reserves
   its memory region at the larger of the two pages before the attachment arrives, which
   is aligned for both, so the frame order did not change; `vmwire` split into a
   portable half, so the frames and the geometry checks are tested anywhere.

   **The arena.** `NewLinuxArena` is parameterised: a 2 MiB slot is a HugeTLB
   memfd of the pool and a 4 KiB slot an ordinary one, named after its page so
   `/proc` says which memory a guest's mapping is really on. `ramPageSize` is
   4 KiB, `host.RAMPageSize` and `host.PMEMPageSize` are the one place either is
   stated, and RAM's write-ahead was one page whatever its budget — which step 8
   below undid, because the run only ever touches fresh zeros and those are
   shared with nobody. Runs and per-run protection
   needed nothing new — the plan already batched a run of consecutive pages in
   consecutive slots into one command, the slot allocator already prefers
   consecutive runs, and a seal already protects per run — so at 4 KiB a fork
   maps a 512-page run with one command and a store copies one page.

   **The crate.** `PAGE_SIZE` became `MIN_PAGE_SIZE`/`MAX_PAGE_SIZE` and a
   per-session `page_size()`; every offset, length, backing offset and generation
   is counted in it. The UFFD negotiates HugeTLB *and* shmem missing, minor and
   write-protect features together, because a host runs a pager of each kind, and
   names the ones a kernel lacks by asking a second descriptor what it supports.
   A trap range prepared out of the middle of the memory region keeps its phase within
   the larger page, so a revoked 4 KiB page still merges with the traps around it
   instead of costing a mapping.

   **Firecracker.** Managed RAM takes no `huge_pages` setting at all — the pager
   owns that memory and states its page when the session attaches — on the boot
   path and the restore path alike, and `volume_ranges` checks the guest's
   memory regions against the page the session states rather than a constant. PMEM is
   unchanged. Snapshot save and restore of a managed VM are unchanged and pass.

   **The settle, and the defect this uncovered.** Qualifying at 4 KiB found a
   defect the 2 MiB page had hidden, and it was not the settle's: a post-copy
   child was told by its own volume that the pages it had just published had no
   object, so the pager's retire gave up the only copy of bytes the guest had
   written and the guest died on them a second later. The 4 KiB page did not
   cause it — it made the handoff's set five hundred times larger, so the same
   mistake was made five hundred times as often. It is fixed, with the rule and
   the guards recorded in [open-work.md](../docs/open-work.md).

   The settle changed on the way there and the change stands on its own: a
   settle only revokes an unchanged page's mapping and the guest's next access
   maps the origin through the fault path, rather than installing the origin
   over a page the guest still maps with one command and no fence. That was
   never the defect, and it was wrong to describe it as most of it.

   **The mapping budget, since.** `ConnectionConfig.MaxVMAs` was only the
   client's admission limit when this step landed. It is the backstop behind the
   three rules above now: a store whose mapping command the client refuses makes
   the range the guest is writing in whole, in one command, and is served again,
   with `Stats.MappingMerges` saying it acted. What is left beside it, in
   [open-work.md](../docs/open-work.md), is the read-ahead run that becomes one
   page under arena pressure.

   **Two costs the 2026-09-22 GCE run measured, and what they are now.** A
   fan-out of three forks each running `cargo test` — 1.8 M pages published per
   fork — issued **5,450,465 revocation commands over 5,481,191 pages, 1,753 s**:
   2.6 a fault, about one per page the guests wrote, because every private page
   was revoked before its copy was mapped. A store replaces that mapping
   instead — the protocol's MAP over a range replaces whatever the pages of it
   had — so a copy-on-write is one command and no revocation, and so is a store
   that closes a gap or fills a range, however many pages it copied. What
   replacing costs is an ordering, which `vmmemory/replacement.go` is:
   the page a store copied from stays where it is, unreclaimable and unreleased,
   until that store's command lands. A store revokes only where the client
   refused its mapping or the command failed, because then there is nothing to
   put in its place.

   And a capture of **2,204,672 sealed RAM pages paused 2.14 s**, of which
   0.18 s was its 2,264 protect commands and 1.97 s — 0.9 µs a page — was the
   seal's per-page bookkeeping. The pause is the commands now. The memory region keeps
   its dirty set as runs as well as as pages, so the seal reads O(runs) and
   write-protects them, takes the whole set in one step and returns; the walk
   that moves each page into the checkpoint runs afterwards, with the guest
   already running, holding the memory region the seal took. Only a fault of that
   memory region waits for it, and `seal_walk_ns` is what it took. Because the seal no
   longer holds the pages' locks, the memory region carries a protection lock that keeps
   its write-protect commands apart from the one thing that can take a mapping
   away meanwhile: a reclaim revoking its victim.

5. **Carry geometry through handoff and migration.** Make page size a property
   of each served volume/memory region rather than the whole page server. Update
   request validation, page numbering, resident listings, unpublished runs,
   transfer completion and destination checks in `vmmigrate`.
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
   `MemoryRegion.Fault` on a VM a second host has just opened — went from **513
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

   **What the pager was not asking for.** The grouping above is what a run
   costs *if the run is asked for*, and until 2026-09-22 the pager never asked
   for one. A fault's plan read each stretch of consecutive pages it had
   reserved with a backing read of its own, so the pages a memory region already held —
   the ones its populate mapped, and every page a sibling fork had made resident
   — cut a window into stretches and each stretch paid its own request per part.
   The fork fan-out measured on GCE that day spent 8,660 loads bringing 31,867
   pages, 3.7 pages a load, and 7,365 object GETs on them, against 1,326 loads
   of 16 pages each on the same run's cold restore, which has no resident pages
   to cut its windows up. Counted as a unit test over a 512-page window
   published by three checkpoints with 64 of its pages already resident: **65
   loads and 195 object reads, against one load and three**.

   So a read of part of a range is one operation the whole way down.
   `Store.ReadPages` takes the pages of a range that are wanted, one element per
   page: the rest are skipped whole, costing neither a request nor a segment
   lookup, and what is left is grouped exactly as before, so a hole a reader
   leaves is read through or split at on the same rule as a hole the volume
   itself has. `Volume.LoadPages` carries that mask through the overlay and
   through a seal — a masked read is page-wise where an unmasked one is
   byte-wise, so the inherited checkpoint is asked once for every wanted page
   the overlay does not already hold whole. `vmmemory.SparseLoader` is the
   pager's name for a backing that can be asked this way and one fault's plan
   makes one call; a migration destination's peer backing, which answers a
   second thing per page, is read stretch by stretch as before. The bytes a
   window fetches rise where its holes are small, because reading through them
   is what the store already chose for a hole a later checkpoint left.

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

8. **Zero write-ahead for RAM. Done.** Give RAM the same write-ahead run as
   PMEM, and make an ahead page the guest never stored into cost nothing past
   the next checkpoint.

   **Why the restriction was wrong.** Write-ahead is only ever reached by
   `MemoryRegion.storeFresh`, which serves a store into a hole or a zero mapping.
   Neither has a resident page or a page identity, so nothing shares them and a
   run of them takes no sharing away from anybody — the reasoning that disabled
   it for RAM is about pages a checkpoint published, which this path never
   touches.

   **The run.** 8 MiB of the pager's own pages, which is the read-ahead run:
   four at 2 MiB and 2,048 at 4 KiB. It is the read-ahead run because
   `zeroRun` cannot leave the faulting page's read-ahead window anyway, and
   because both are buffers a deployment states in bytes. A pager whose dirty
   budget cannot hold 64 such runs keeps one page, which is the bound PMEM
   already had: the deployment's 9 GiB of dirty RAM is 2,359,296 pages against
   the 131,072 that bound needs, so a production RAM pager takes the whole run.
   Beyond that, a run shrinks to the free dirty reservations and the consecutive
   free arena slots it finds, and waits for neither.

   **What it buys.** The boot survey of 2026-09-22 (`TestBootSurveyOfPrivateRAMPages`)
   says a 16 GiB guest's boot makes 103,035 RAM pages private, and that they are
   real kernel writes rather than spurious read faults: 65,280 of them are the
   256 MiB memmap, written page by page by `__init_single_page`, and 16,384 are
   swiotlb's 64 MiB bounce buffer, memset to zero in one block. Both are
   contiguous and written forwards, so at 8 MiB a run those two cost 32 faults
   instead of 81,664.

   **What bounds its cost, and where.** An ahead page the guest never stored
   into reads back as zeros, so `Publication.writeEdits` gives it no object —
   a page that reads as all zeroes leaves the index — and the volume reports it
   as a hole from then on. The **retire** is what hands it back:
   `finalizeCheckpoint` looks up the identity the volume now gives each page,
   and `publishLocked` drops a page the volume holds no object for, revoking the
   guest's mapping, releasing the resident page and returning the dirty
   reservation. The page is then as untouched as it was before the store. The
   settle is the wrong place: it compares a copy with the page it was copied
   from, and a page made from zeros has no origin — deciding it there would mean
   a second zero test of every ahead page's bytes, duplicating the one the
   publication already makes.

   **Proved by exact counts.** `TestZeroWriteAheadPagesTheGuestNeverStoredIntoAreGivenBack`
   runs the worst pattern for the run — the guest stores into one page of every
   512 of a 4,096-page memory region of holes — and requires 8 faults, 8 private runs,
   8 mapping commands, no revocation and no volume read; 4,096 private pages and
   4,096 dirty; then, across the checkpoint, 4,096 sealed pages, 4,088 of them
   write-ahead zeros, 8 pages and 32,768 bytes published, and afterwards 0 dirty
   pages, 8 resident and 8 mapped, with every page reading back as the store the
   guest made or the hole it was. `TestAGivenBackZeroAheadPageStoresAsAHoleAgain`
   requires the next store into a given-back page to be a fresh-zero store again.
   In the simulation, `TestZeroWriteAheadPublishesOnlyWhatTheGuestStored` runs
   the same pattern through a cold-started VM on the deployment's own stack and
   requires the byte model to hold through the guest's mappings and through the
   volume, with the host holding the whole memory region privately before the checkpoint
   and nothing after it. `host/pager_test.go` pins both pagers' runs and
   the dirty-budget bound, on either platform.

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
