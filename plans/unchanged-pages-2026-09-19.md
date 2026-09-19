# A private page that did not change — 2026-09-19

**Status: planned; not implemented.**

## The defect

The pager answers a write fault on a shared page with a private copy, and from
then on the page is dirty: it is no longer shared, it holds a dirty reservation,
and the next checkpoint uploads it under a new identity. That is right when the
guest stored into the page. It is wrong when the guest did not, and a write
fault is not always a store:

- on x86-64, KVM finishes a guest fault that has to wait for the pager from a
  worker thread, `async_pf_execute`, which always asks for the page writable
  (`virt/kvm/async_pf.c`, unchanged in mainline 7.3-rc3; the guest cannot opt
  out), so a cold **read** reaches the pager as a write fault;
- on aarch64, the guest kernel's cache maintenance on a page it executes for
  the first time is reported by the architecture as a write.

Measured on 2026-09-19 (`docs/measurements/gce-fanout-2026-09-19/`,
`fanout-git-lima-2026-09-19.json`): every root page a read-only fork touched on
x86-64 became its own, and in Lima the pages holding `git` and `libpcre2` did,
with not one byte changed in any of them. Each is a 2 MiB copy the host holds
twice, and 2 MiB the next checkpoint uploads for nothing. Pages mapped ahead of
a fault are spared, because a read of a mapped page never waits.

The pager cannot tell these faults from real ones when they arrive: the worker
will not finish until the page is writable. It can tell afterwards.

## The decision

**A sealed page whose bytes equal the page it was copied from is not dirty.**
The checkpoint does not publish it, and when the seal ends the guest's page goes
back to sharing the page it was copied from.

- **The copy remembers its origin.** When a store is served by copying away
  from a resident page that holds a published page identity, the binding keeps
  a pointer to that resident page — `origin`. Not its identity: eight bytes per
  binding, and an origin that has been evicted is simply no longer an origin. A
  page copied from a checkpoint's held copy, from another host's unpublished
  page, or made from zeros has no origin.
- **The comparison is exact and in memory.** A published identity's bytes are
  immutable, so comparing the sealed page with its origin under both pages'
  locks is a `bytes.Equal` and nothing else: no hash, which would make a wrong
  answer possible and would put work on the fault path, and no read of the
  store, which would double a checkpoint's I/O for the pages that did change.
  An origin that is no longer resident is not compared; the page is published
  as it is today. Nothing is pinned to keep an origin resident.
- **It happens behind the pause.** The seal is unchanged. The publication asks
  each dirty source to settle before it enumerates its pages —
  `DirtySource.Settle(ctx)`, called once, off the pause path, with the guest
  running — and `RegionCheckpoint.Settle` compares every held page that has an
  origin. An unchanged page leaves the checkpoint's set, so `DirtyPages` does
  not list it and it costs the store nothing.
- **The settle is parallel.** Each page is settled alone — its comparison and
  its re-sharing take that page's lock and its origin's and nothing wider — so
  a settle hands its pages to `Config.SettleWorkers` workers, the host's
  processors by default, and the regions of one VM settle at the same time as
  each other. What bounds it is memory bandwidth and not the pager's I/O
  permits, which it does not take: it reads no disk and no store. A thousand
  2 MiB pages are about a tenth of a second of comparing on one processor, and
  that is time the upload waits for, so it is divided rather than queued. The
  workers share nothing but the counter of unchanged pages and the set the
  checkpoint will list, both under the checkpoint's own mutex, so the result
  does not depend on the order they finish in — which is what lets the
  simulation run the same code.
- **An unchanged page is re-shared at once, not left to fault again.** Under
  the origin's lock and the private page's: if the guest still shares the
  checkpoint's copy, its mapping is replaced by a read-only mapping of the
  origin's slot, the binding takes the origin as its resident page and becomes
  clean, the private page is released and the dirty reservation returned. The
  bytes are identical and the sealed page is write-protected, so the swap is
  invisible to a running guest; a store that lands first copies away from the
  checkpoint as it does today, and then only the checkpoint's copy is released.
  The page is mapped rather than left missing on purpose: a missing page's next
  read would wait, go through the same worker, and be copied again.
- **A fork point is not settled.** A fork point publishes nothing and its pause
  is what a child waits for; its children inherit an unchanged page as an
  unpublished one, which is correct and no worse than today. The next
  checkpoint of each settles it.
- It is the pager's rule, so it holds for RAM and PMEM, at any page size.

`Stats.UnchangedPages` counts the pages a settle found unchanged, and the
checkpoint's log line carries the count beside its dirty set.

## What it does not do

It does not stop the copy. Between the fault and the next checkpoint the host
holds the page twice; with RAM at 4 KiB under a 2 MiB read-ahead run that is one
page in 512, and for PMEM at 2 MiB it is a whole page per cold fault until the
checkpoint interval passes. Preventing the copy needs a host kernel that passes
the guest's access through, or KVM userfault; both are recorded in
[open-work.md](../docs/open-work.md).

## How it is proved

Red tests first, exact numbers, beside the code.

- `internal/vmmemory`: a region shares N pages with a sibling; it takes a write
  fault on one and stores nothing. After seal and settle: the checkpoint lists
  no page, the sharing gauges are what they were before the fault (unique N,
  saved N), the region's private bytes are zero, its dirty reservation is back,
  and a read of the page maps without a fault. The same with a real store: the
  page is listed and published exactly as today. A store that lands between the
  seal and the settle. An origin evicted before the settle: published as today.
  A page copied from a checkpoint's held copy, from zeros, and from an
  unpublished page: never compared. A settle of many pages gives the same
  result with one worker and with sixteen, and under the race detector.
- The loss window: a region whose only private pages were unchanged has no
  unpublished write after the settle, and a store waiting on the window is let
  through.
- `internal/simtest`: the simulated guest gains a write fault that stores
  nothing, drawn by the campaigns' drivers alongside its stores. Every existing
  property holds — the byte model most of all: a page re-shared must read what
  the guest last wrote — and one scenario of its own: forks that only read,
  through faults that all claim to be writes, publish checkpoints with no pages
  and end sharing everything they touched.
- `just check` green; `just soak 1 20` green.
- On GCE and in Lima, by whoever dispatches the work: the 2026-09-19 fan-out
  repeated with a checkpoint taken in each fork. Before: each fork owns every
  root page it touched and uploads it. After: `fork_own_pages` for the root is
  empty after the checkpoint, the checkpoint's put bytes carry no root page,
  and `fork_changed_counts` still reports every RAM page that was really
  written as the fork's own.

## Order

After step 2 of the [page-geometry plan](ram-pmem-page-geometry-2026-09-19.md)
and before its step 3: step 3 parameterises the whole of `internal/vmmemory` by
page size, and this change is in the same files.
