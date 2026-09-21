# A private page that did not change — 2026-09-19

**Status: done**, the measurement on real hosts included.

Three things the code showed the plan was wrong about, all of them recorded in
the text below:

- **A copy needs a page to be made from, and a cold write fault had none.** The
  plan says the copy remembers "the resident page it was copied from", but a
  store into a page its region holds no memory for read the volume straight
  into its private page and had nothing to remember — which is exactly the
  x86-64 case the defect is about. Such a store now reads the page in first,
  under the identity its volume gives it, and copies away from that: one extra
  resident page for the fault, and an identity every other region inheriting it
  maps rather than reads.
- **The page a store copies from is left in the arena rather than released.**
  Unlinking the binding from it released it whenever the binding was its last
  alias, which is the ordinary case, so the origin would have been gone before
  the seal. Nothing pins it — it is clean, and the next reclaim short of a slot
  takes it like any other page — but a store no longer releases it, and the
  region that left it there gives it up when it detaches. `Host.Sharing` counts
  such a page in `UniqueBytes` under the kind of the region that made it,
  because it is memory the arena holds.
- **The settle takes the region live, not the region lock.** "Nothing wider"
  than the two page locks is what the plan asks for and what the code does; the
  region is held only in the sense a fault holds it against a detach, so a
  seal, a retire or a detach waits for a settle and never for a page of one.
  A sealed page the pager has spilled is not compared either, for the same
  reason the store is not read: the settle takes no I/O permit.

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
  page, or made from zeros has no origin. A store into a page its region holds
  no memory for reads that page in first, so that there is a resident page
  under the volume's identity to copy away from and to remember; the page is
  left in the arena when the binding takes its private copy instead, and is an
  ordinary reclaim candidate from then on.
- **The comparison is exact and in memory.** A published identity's bytes are
  immutable, so comparing the sealed page with its origin under both pages'
  locks is a `bytes.Equal` and nothing else: no hash, which would make a wrong
  answer possible and would put work on the fault path, and no read of the
  store, which would double a checkpoint's I/O for the pages that did change.
  An origin that is no longer resident is not compared, and neither is a sealed
  page the pager has spilled, which would be a read the settle does not take an
  I/O permit for; either way the page is published as it is today. Nothing is
  pinned to keep an origin resident.
- **It happens behind the pause.** The seal is unchanged. The publication asks
  each dirty source to settle before it enumerates its pages —
  `DirtySource.Settle(ctx)`, called once, off the pause path, with the guest
  running — and `RegionCheckpoint.Settle` compares every held page that has an
  origin. An unchanged page leaves the checkpoint's set, so `DirtyPages` does
  not list it and it costs the store nothing.
- **The settle is parallel.** Each page is settled alone — its comparison and
  its re-sharing take that page's lock and its origin's and nothing wider — so
  a settle hands its pages to `Config.SettleWorkers` workers — the host's
  processors, which `internal/host` chooses; a configuration that leaves it
  zero settles on the caller's own goroutine — and the regions of one VM settle
  at the same time as each other. What bounds it is memory bandwidth and not the pager's I/O
  permits, which it does not take: it reads no disk and no store. A thousand
  2 MiB pages are about a tenth of a second of comparing on one processor, and
  that is time the upload waits for, so it is divided rather than queued. The
  workers share nothing but the counter of unchanged pages and the set the
  checkpoint will list, both under the checkpoint's own mutex, so the result
  does not depend on the order they finish in — which is what lets the
  simulation run the same code.
- **An unchanged page is re-shared at once.** Under the origin's lock and the
  private page's: if the guest still shares the checkpoint's copy, the binding
  takes the origin as its resident page and becomes clean, the guest's mapping
  of the copy is revoked, the private page is released and the dirty reservation
  returned. A store that lands first copies away from the checkpoint as it does
  today, and then only the checkpoint's copy is released. **The mapping is
  revoked rather than replaced**, which this plan originally had the other way
  round: a settle runs with the guest running and holds neither the region nor
  the window that serializes a page's mappings, so the only replacement it may
  issue is the one that installs no page table and wakes nothing. Installing the
  origin in its place corrupted a guest once the RAM page was 4 KiB and a
  fan-out settled thousands of pages an interval; see the settle's entry in
  [vm-memory.md](../docs/vm-memory.md). The guest's next access maps the origin
  through the fault path instead, which is one fault per page re-shared.
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
- On GCE and in Lima: the 2026-09-19 fan-out repeated with a checkpoint taken in
  each fork, which the fan-out now records as `fork_checkpoints`. Taken on
  2026-09-19, four forks each time
  (`docs/measurements/gce-fanout-settled-2026-09-19/`,
  `fanout-git-lima-settled-2026-09-19.json`):

  | | x86-64, `busybox uname` | x86-64, `memprobe 16` | aarch64, `git --version` |
  | --- | --- | --- | --- |
  | Root pages a fork owned before its checkpoint, none of them written | 2 | 2 to 6 | 2 |
  | Pages its settle dropped, root and RAM | 2 to 5 | 2 to 7 | 2 to 3 |
  | Root pages it owned afterwards | 0 | 0 | 0 |
  | Pages its checkpoint published, all RAM | 27 to 29 | 37 to 38 | 27 to 29 |

  Every page a settle dropped was one the comparison before the checkpoint had
  found unwritten — a fork's root pages, and the one to three RAM pages in the
  same state — and every page a fork had really written was published. The
  first fork to checkpoint reports none of its root pages as shared, because
  its siblings still held their own copies at that moment; every later one
  shares all of them.

## Order

After step 2 of the [page-geometry plan](ram-pmem-page-geometry-2026-09-19.md)
and before its step 3: step 3 parameterises the whole of `internal/vmmemory` by
page size, and this change is in the same files. It was taken there, against
`vmmemory.PageSize`.
