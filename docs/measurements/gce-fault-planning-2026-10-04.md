# A fault that plans its own page first, on GCE, 2026-10-04

[Fault first](gce-fault-first-2026-10-04.md) left a dependent 4 KiB fault
from the cluster at 3.2 ms, against 0.67 ms for its page's read alone. The
profile put 0.62 s of the 1.42 s a chain's faults spent in planning: before it
read anything, a fault located and planned all 2,048 pages of its window, and
the lookup decoded the window's page-table segment. This run measures two
changes against that, through the real pager, before and after, from the
cluster and from the store:

- **A fault plans its own page first.** It locates its page alone, takes it,
  and starts its read. Only then does it locate the rest of its window and
  plan it, while the read is under way
  ([planning a fault](../vm-memory.md#planning-a-fault)). A fault at
  random reserves nothing for its neighbours.
- **A segment is decoded once.** The page cache keeps each segment's page
  table, decoded, under the segment's identity, for every index that reads it
  ([page cache](../volumes.md#page-cache)). It is parsed straight off the wire
  instead of through one protobuf message a page.

It also decides when a 4 KiB volume's page tables should load: when the
checkpoint opens, or when the first fault touches each segment.

## What ran

Six disposable `n2-highmem-4` hosts in us-east4-a (4 vCPUs, 32 GB, Intel Ice
Lake, 10 Gbps), each with one local NVMe SSD, under 4+2, against a GCS bucket
in the same region, as last time. `scripts/bench-restore-gce.sh` ran
`cmd/sproutfs-restorebench` three times on the same hosts:

- **before**: main at 40ee48de, built apart (`SPROUTFS_RESTORE_BINARY`);
- **after**: this change, at 0c5e6cbc;
- **eager**: the same build, with every read loading all of its volume's page
  tables once the checkpoint is open and before its reads (`-tables eager`).

Host 0 published an 8 GiB guest of 2 MiB pages and a 4 GiB guest of 4 KiB
pages, and every fill was kept. Host 1 read them back. Each run read every case
from both sources three rounds, each round in its own order, with every host's
memory tiers and page cache emptied before each case. Every page of every case
read back right, and no read failed. Raw results are in
`gce-fault-planning-2026-10-04/`.

The cases are those of last time. A chain through the pager is 400 hops, each
to the page the bytes of the last name, faulted in where the pager does not
map it; the latencies are of the hops that faulted. A chain of pages is 2,000
hops read straight from the store's API, which is what a fault would cost if it
were only its page's read. In order is the whole guest through the pager, page
by page.

## Results

### A chain of faults

Each hop that faulted, in milliseconds: the median over three rounds of each
round's p50, p90 and p99.

| Guest | Source | Read | p50 | p90 | p99 |
| --- | --- | --- | --- | --- | --- |
| 4 KiB | cluster | page straight from the store | 0.67 | 0.74 | 5.31 |
| 4 KiB | cluster | before | 3.07 | 11.86 | 19.25 |
| 4 KiB | cluster | after | 1.05 | 5.17 | 11.82 |
| 4 KiB | cluster | after, tables loaded at open | 1.02 | 3.11 | 15.73 |
| 4 KiB | store | page straight from the store | 23.04 | 30.51 | 53.00 |
| 4 KiB | store | before | 29.09 | 56.98 | 71.55 |
| 4 KiB | store | after | 25.74 | 50.74 | 74.41 |
| 4 KiB | store | after, tables loaded at open | 22.53 | 29.79 | 46.90 |
| 2 MiB | cluster | page straight from the store | 6.22 | 8.67 | 9.78 |
| 2 MiB | cluster | before | 7.06 | 10.28 | 13.28 |
| 2 MiB | cluster | after | 6.91 | 10.56 | 13.32 |
| 2 MiB | store | page straight from the store | 34.63 | 42.82 | 66.74 |
| 2 MiB | store | before | 37.69 | 46.55 | 61.10 |
| 2 MiB | store | after | 36.38 | 45.03 | 67.72 |

The chains through the pager, hops a second over all 400 hops, and the seconds
each round took:

| Guest | Source | Before | After | Before, seconds | After, seconds |
| --- | --- | --- | --- | --- | --- |
| 4 KiB | cluster | 201 | 442 | 1.81, 2.03, 1.99 | 0.98, 0.76, 0.90 |
| 4 KiB | store | 29.9 | 33.8 | 18.78, 13.37, 12.77 | 16.96, 11.84, 11.11 |
| 2 MiB | cluster | 119 | 127 | 3.35, 3.71, 3.13 | 3.15, 3.17, 2.84 |
| 2 MiB | store | 26.0 | 26.6 | 21.55, 14.27, 15.38 | 22.09, 13.88, 15.04 |

The pager did the same work in both: in the 4 KiB chain from the cluster, 395
faults, 408 loads and 13 prefetches; 382 faults followed no recent fault and
read their page alone.

### Where a 4 KiB hop's time goes now

The reader's profile of the 4 KiB chain from the cluster, once more after the
rounds:

| | Before | After |
| --- | --- | --- |
| profiled read | 2.04 s | 0.83 s |
| the faults, on their own goroutines | 1.46 s | 0.36 s |
| locating and planning the window before the read | 0.73 s | none |
| decoding segments | 0.55 s | 0.09 s |
| planning the window while the read is under way | none | 0.30 s |

What is left is the window's planning, now beside the read rather than before
it: locating 2,048 pages (0.12 s) and looking each up among the resident pages
(0.14 s), about 0.75 ms a fault, which is as long as the page's read. A hop
takes the longer of the two, 1.05 ms against 0.67 ms for the page alone.

### Decoding a segment

On one processor of an Apple M5, a whole segment of a 4 KiB-page volume, 16,384
pages, decodes in 0.64 ms with 5 allocations of 328 KB in all, against 2.3 to
3.8 ms and 16,559 allocations of 7.6 MB through the protobuf messages
(`BenchmarkDecodeAWholeSegmentOf4KiBPages`). In the chain of 4 KiB pages read
straight from the cluster, the first read in each of the guest's 64 segments
fetches and decodes it:

| | before | after | after, tables loaded at open |
| --- | --- | --- | --- |
| p99 of a hop | 8.21 ms | 5.31 ms | 0.79 ms |
| the slowest hop | 10.65 ms | 5.86 ms | 1.17 ms |
| hops past 4 ms, of 6,000 | 192 | 113 | 0 |
| hops past 8 ms, of 6,000 | 113 | 0 | 0 |

### The guest in order

The whole guest through the pager, one page at a time, seconds each round:

| Guest | Source | Before | After | After, tables loaded at open |
| --- | --- | --- | --- | --- |
| 4 GiB, 4 KiB | cluster | 27.34, 27.35, 27.19 | 26.24, 27.03, 26.03 | 25.80, 26.28, 26.35 |
| 4 GiB, 4 KiB | store | 50.33, 46.08, 41.61 | 47.83, 45.71, 41.31 | 52.10, 44.61, 46.83 |
| 8 GiB, 2 MiB | cluster | 15.58, 16.09, 15.49 | 15.61, 15.76, 15.93 | |
| 8 GiB, 2 MiB | store | 71.26, 59.56, 53.13 | 67.67, 56.31, 51.03 | |

Every run read every page once: 1,024 loads and 512 prefetches at 4 KiB, 2,048
loads and 1,024 prefetches at 2 MiB.

### When the page tables load

Opening the checkpoint took 15 to 35 ms in every run, a GET of the end of its
index object. Loading all 64 page tables of the 4 GiB guest of 4 KiB pages
after it, sixteen at a time, took 83 to 103 ms from the cluster and 125 to
141 ms from the store, with one load of 379 ms. That is what a restore would
wait for before its guest ran if the tables loaded when the checkpoint opens.
It grows with the guest: 1,024 segments for 64 GiB, about 1.4 s from the
cluster, and 320 MiB of page tables in the page cache's memory.

Left to the first fault that touches each segment, the same tables cost the
faults that load them. In the chain of 4 KiB pages from the cluster that was
2 to 6 ms more on 64 hops a round, one a segment, where it was 4 to 16 ms
before the change. In the chains of faults, loading them first moved the p90
of a hop from 5.2 to 3.1 ms from the cluster and from 51 to 30 ms from the
store, but not the p99, which is the prefetches of the hops before. A guest
pays the lazy cost once a segment it touches, and only for those.

## What it shows

**A dependent 4 KiB fault from the cluster now takes about a page's read and
a half.** The median hop went from 3.07 to 1.05 ms, the p90 from 11.9 to 5.2 ms
and the p99 from 19.2 to 11.8 ms, and the chain ran 2.2 times as many hops a
second. From the store, whose reads wait on the network, the median went from
29.1 to 25.7 ms against 23.0 ms for the page alone. A 2 MiB window is four
pages, so its planning was never the cost, and a 2 MiB hop did not move.

**The window's planning is no longer in front of the read, but it is still as
long as the read at 4 KiB.** A fault locates 2,048 pages and looks each up
among the resident pages in about 0.75 ms. It overlaps the page's 0.67 ms
read, so a hop is about 1 ms. Taking the rest of the 4 KiB hop would need a
fault at random to skip the window's resident pages too, or to find them
without a lookup a page.

**A segment is decoded once, and four to six times as fast.** The profile's decoding
went from 0.55 s to 0.09 s, and the first read in each segment from up to
16 ms to under 6 ms.

**Loading a 4 KiB volume's page tables when the first fault touches them is
the better default.** Loading them all when the checkpoint opens would add
85 ms from the cluster and 130 ms from the store to the restore of a 4 GiB
guest, more for a larger one, and hold every table in memory whether or not
the guest touches its pages. Loading them lazily costs 2 to 6 ms on the first
fault in each segment the guest touches, 64 faults for a guest that touches
all of a 4 GiB volume. The decision is recorded in
[opening](../volumes.md#opening).

**Reading in order did not move.** The 4 GiB guest of 4 KiB pages took 26.2 to
27.0 s from the cluster after against 27.2 to 27.4 s before, and the store and
the 2 MiB guest were within their rounds' spread.

## Cleanup

Every host, its disks and the run's objects were deleted.
`gcloud compute instances list` and `gcloud compute disks list` show no
`sproutfs-restore-faultplan` host or disk, and the bucket holds no object.
