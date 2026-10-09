# Where the pager's policies differ from Zircon and Fxfs — 2026-10-09

The Zircon port (`plans/zircon-pager-port-2026-10-05.md`) took Zircon's
mechanisms: VmCowPages, the page queues, the evictor, slot storage and page
requests. It kept the pager's own policies wherever the port said "stays
ours", and every one of those was tuned for 2 MiB pages read from the cluster.
At 4 KiB they cost the embedder's PostgreSQL benchmark most of its gap to plain
Linux. This lists each policy beside what Zircon's kernel and Fxfs, the user
pager Fuchsia runs, do, with the source of both. Each entry is removed, or kept
with its reason.

Fuchsia sources are at fuchsia 90e54e09 under `~/src/fuchsia`.

## Fault-around

- Zircon maps at most 16 present pages around a fault and allocates nothing
  for them (`kPageFaultMaxOptimisticPages`,
  `zircon/kernel/vm/include/vm/vm_address_region.h:1047`;
  `zircon/kernel/vm/vm_mapping.cc:1272`).
- Ours maps every resident page of the read-ahead window when the fault
  follows a recent one: 2,048 pages at 4 KiB, located and looked up per fault
  (`vmmemory/fault.go` planFault, `vmmemory/window.go` survey).
- Decision: taken (d361afe5), with its rule that a page mapped around a fault is not counted used.

## Read-ahead

- Zircon reads nothing ahead; it asks the user pager for the missing range.
  Fxfs widens every page-in to an aligned 128 KiB
  (`src/storage/fxfs/platform/src/fuchsia/volume.rs:62`, `pager.rs:548`).
- Ours read the rest of the 8 MiB window behind any second fault in it. Since
  TASK-122.11 it reads by streams, after Linux (`vmmemory/readahead.go`).
- Decision: kept. Read-ahead is the user pager's choice in Fuchsia, and a page
  read here costs an eviction later, so 128 KiB a random read is waste.

## Writeback

- Fxfs writes a file's dirty pages back in the background once it has more
  than 10 MiB of them (`BACKGROUND_FLUSH_THRESHOLD`,
  `src/storage/fxfs/platform/src/fuchsia/paged_object_handle.rs:39`;
  `file.rs:814`), and every volume's every 20 s (`volume.rs:85`), bracketing
  each write with `ZX_PAGER_OP_WRITEBACK_BEGIN/END` (`pager.rs:375`). The
  pages become clean, and the kernel may evict them.
- Ours has no writeback. A page is clean only once a checkpoint publishes it.
- Decision: take it, to the local log (TASK-122.9). Open.

## Dirty limits

- Fxfs bounds dirty bytes only under critical memory pressure, at 1% of
  physical memory (`volumes_directory.rs:394`, `:897`); otherwise it marks
  pages dirty at once.
- Ours bounds dirty pages by the spill file's slots, and a store past the
  bound waits for a checkpoint's upload: 539,457 waits in one 4 KiB run.
- Decision: a small budget of pages the writer has not reached, with uploads
  read from the log (TASK-122.10). Open.

## When eviction runs

- Zircon's memory watchdog evicts asynchronously towards free-memory targets
  (`zircon/kernel/object/memory_watchdog.cc:177`, `:427`). An allocation that
  can wait waits for that, rather than evicting itself
  (`PMM_ALLOC_FLAG_CAN_WAIT`, `zircon/kernel/vm/include/vm/pmm.h:63`).
- Ours evicts inside the fault that needs a slot (`vmmemory/allocation.go:350`,
  `vmmemory/store.go:269`, both through `evictOne`, `vmmemory/evict.go:125`).
  `EvictAsynchronous` is never called.
- Decision: take it, with writeback (TASK-122.9). Open.

## What eviction takes

- Zircon evicts only clean pages of pager-backed objects. Anonymous dirty
  pages are compressed in memory; a pager-backed dirty page stays until the
  pager writes it back.
- Ours spills dirty pages to the spill file, and writes a published page to
  it as a version as it evicts it (`vmmemory/evict.go` evictPages).
- Decision: once writeback lands, evict clean pages only, as Zircon does.
  Open.

## Reads from the local disk

- Fxfs reads a page-in as one request of its aligned range.
- Ours reads a published version one page at a time, one `ReadAt` per page
  (`vmmemory/internal/zirconvm/spillstorage.go:261`), and the versions of
  consecutive pages are in no particular slots.
- Decision: open. It matters once streams read ahead far from the local disk.

## Page size

- Zircon's base page is 4 KiB; large pages are an optimisation over it.
- Ours defaults PMEM to 2 MiB (`host.DefaultPMEMPageSize`), and 4 KiB is
  measured beside it (TASK-110).
- Decision: open, decided by the benchmark once the entries above are done.
