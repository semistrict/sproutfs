# RAM and PMEM page geometry — 2026-09-19

**Status: steps 1 and 2 are implemented, but for step 1's baseline measurement;
steps 3 to 7 are planned.**

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
   that admits about 8.5 TiB of such a volume. `maximumIndexSize` is the one
   bound a small page brings within reach: a checkpoint writes about 5 MiB of
   segments per GiB of a 4 KiB-page volume it dirtied, so one that changed
   every page of more than about 12 GiB at once is refused. That bounds one
   checkpoint's dirty set rather than the volume, and step 6 is where the
   packing that would relieve it belongs.

   The pager, the wire protocols and the VMM are untouched, so every volume a
   host creates is still a 2 MiB-page volume and the pager refuses one of any
   other page size when it is attached. The simulation campaigns run through
   that pager, so they stay at 2 MiB; a 4 KiB-page volume goes through
   checkpoints, forks, compaction and reclamation in `internal/volume` and
   `internal/checkpoint` instead, and reaches the campaigns when step 3 gives
   the pager its own geometry.

3. **Separate pager instances and arenas.** Parameterize `internal/vmmemory`
   by a fixed page size per instance, including its arena, spill slots,
   reservations, identities, read-ahead and buffer bounds. Assemble RAM and
   PMEM instances in `internal/host`, and route each region to the correct
   one. Share the host's byte resource budget without double counting its
   capacities. Audit dirty/logical budget configuration, memory admission,
   pressure callbacks and shutdown for both instances. A checkpoint requested
   by either pager still seals the entire VM; the loss window spans both.

4. **Support small RAM mappings with large runs.** Update `internal/vmwire`,
   `rust/sproutfs-vm-memory` and the Firecracker integration together. Validate
   each session's geometry and backing type, and configure RAM without the
   current mandatory HugeTLB setting. Use 4 KiB RAM generations, fault
   alignment, protection and replacement, while mapping/protecting contiguous
   runs in batches. Verify ordinary-memfd userfaultfd support and required
   missing/minor/write-protection features on the qualification kernels.
   Exercise sparse zeros, partial replacements and VMA-budget refusal.

5. **Carry geometry through handoff and migration.** Make page size a property
   of each served volume/region rather than the whole page server. Update
   request validation, page numbering, resident listings, unpublished runs,
   transfer completion and destination checks in `internal/vmmigrate`.
   Bound requests and in-flight data in bytes, allowing batches of 4 KiB RAM
   pages. Reject a mismatch before mapping or accepting guest data.

6. **Keep storage and network I/O batched.** Pack small dirty pages into
   bounded parts. Preserve 4 KiB identities without a PUT per page. Coalesce
   adjacent cold reads within a part so a 2 MiB RAM read-ahead does not
   automatically produce 512 object-store GETs. Measure member/table overhead,
   compression work, metadata growth and requests per workload. Shared parts
   and one checkpoint root can still hold both kinds of volume.

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
half of this and is done; the mapping and handoff formats are steps 4 and 5. Regenerate protocol code from its source
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
