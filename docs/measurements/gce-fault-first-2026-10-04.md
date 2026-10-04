# A fault that reads its own page first, on GCE, 2026-10-04

[The dependent reads](gce-dependent-reads-2026-10-03.md) showed what a
fault's read-ahead run costs a guest that follows pointers. A fault read its
whole 8 MiB run before it installed its page, so every hop of a chain of
faults paid the run. From the cluster a 4 KiB page took 0.65 ms and its run
39 ms. The pager now reads the faulting page alone, installs it and wakes the
guest, and reads the rest of the run behind it as a prefetch
([faults and read-ahead](../vm-memory.md#faults-and-read-ahead)). This run
measures that change through the real pager, before and after, from the
cluster and from the store, at both page sizes.

## What ran

Six disposable `n2-highmem-4` hosts in us-east4-a (4 vCPUs, 32 GB, Intel Ice
Lake, 10 Gbps), each with one local NVMe SSD, under 4+2, against a GCS bucket
in the same region. `scripts/bench-restore-gce.sh` ran
`cmd/sproutfs-restorebench`. Host 0 published an 8 GiB guest of 2 MiB pages
and a 4 GiB guest of 4 KiB pages, as last time; every fill was kept. Host 1
read them back. Three rounds ran every case from both sources, each round in
its own order, with every host's memory tiers and page cache emptied before
each case. Every page of every case read back right, and no read failed.
Raw results are in `gce-fault-first-2026-10-04/`.

The bench now reads a guest through a real pager as well as straight from the
store (`cmd/sproutfs-restorebench/pager.go`). The pager has the host's 8 MiB
read-ahead run and an arena large enough for the whole guest, so nothing is
evicted. The backing is the checkpoint read through the store, the arena is
the process's memory, and the mapping records what the pager maps. Each read
faults the page in where the pager does not map it and copies it out of the
arena. The two ways of faulting are:

- **before**: every fault marked as a post-copy stream's
  (`vmmemory.WithStream`), which reads its whole run before the page is
  installed, as every fault did until this change;
- **after**: the pager as built.

Both ran in the same binary on the same hosts, round by round, so host and
network noise falls on both alike. The page and run reads straight from the
store are last run's cases, for reference: a chain of pages is what a fault
would cost if it read only its page.

A chain is 400 hops through the pager, each to the page the bytes of the last
one name. Some hops land on a page a hop before already brought in, more at
4 KiB, where a run is 2,048 pages, than at 2 MiB, where it is four. Those hops
take microseconds and are left out of the latencies below, which are of the
hops that faulted. The chains read straight from the store are 2,000 hops of
pages and 400 of runs, as last time. In order is the whole guest, page by
page, one read at a time.

Two builds ran. The first prefetched behind every fault (`dcc2eb8a`). It
showed that a prefetch can delay the faults after it (see below), and the
second (`38d55682`) prefetches only behind a fault that follows one of its
memory region's recent faults. The tables give both. A first attempt at the
first build was lost: one GET of GCS waited 52 minutes for the response
headers of its HTTP/2 stream and held the run, so the bench now fails a read
that takes longer than two minutes.

## Results

### A chain of faults

Each hop that faulted, in milliseconds: the median over three rounds of each
round's p50, p90 and p99.

| Guest | Source | Read | p50 | p90 | p99 |
| --- | --- | --- | --- | --- | --- |
| 2 MiB | cluster | page straight from the store | 6.06 | 7.18 | 9.45 |
| 2 MiB | cluster | run straight from the store | 11.99 | 15.35 | 19.95 |
| 2 MiB | cluster | before: run first | 13.51 | 20.49 | 28.08 |
| 2 MiB | cluster | prefetch behind every fault | 10.35 | 14.42 | 19.84 |
| 2 MiB | cluster | after | 7.11 | 10.43 | 12.22 |
| 2 MiB | store | page straight from the store | 38.07 | 48.19 | 79.97 |
| 2 MiB | store | run straight from the store | 54.19 | 64.67 | 116.75 |
| 2 MiB | store | before: run first | 57.12 | 77.03 | 118.04 |
| 2 MiB | store | prefetch behind every fault | 44.06 | 55.21 | 80.42 |
| 2 MiB | store | after | 37.80 | 49.06 | 93.40 |
| 4 KiB | cluster | page straight from the store | 0.67 | 0.73 | 7.97 |
| 4 KiB | cluster | run straight from the store | 32.66 | 47.30 | 61.50 |
| 4 KiB | cluster | before: run first | 40.91 | 60.11 | 92.40 |
| 4 KiB | cluster | prefetch behind every fault | 24.26 | 50.12 | 92.24 |
| 4 KiB | cluster | after | 3.22 | 11.93 | 25.54 |
| 4 KiB | store | page straight from the store | 24.93 | 35.03 | 65.29 |
| 4 KiB | store | run straight from the store | 68.15 | 95.15 | 133.73 |
| 4 KiB | store | before: run first | 80.24 | 114.17 | 155.53 |
| 4 KiB | store | prefetch behind every fault | 31.36 | 62.95 | 79.72 |
| 4 KiB | store | after | 29.24 | 59.44 | 80.25 |

The chains through the pager, hops a second over all 400 hops, and seconds
each round took:

| Guest | Source | Before | After | Before, seconds | After, seconds |
| --- | --- | --- | --- | --- | --- |
| 2 MiB | cluster | 73.4 | 121.9 | 5.45, 5.10, 5.90 | 3.47, 3.28, 3.28 |
| 2 MiB | store | 18.4 | 25.4 | 35.23, 21.70, 20.50 | 29.36, 15.75, 15.23 |
| 4 KiB | cluster | 32.1 | 190.6 | 12.67, 12.47, 12.44 | 2.10, 1.93, 2.32 |
| 4 KiB | store | 16.6 | 29.6 | 24.12, 23.15, 26.48 | 21.14, 12.55, 13.50 |

What the pager did in a chain, the median over the rounds:

| Guest | Source | Read | Faults | Loads | Pages loaded | Prefetches | Runs left unread |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | cluster | before | 346 | 346 | 1,384 | 0 | 0 |
| 2 MiB | cluster | after | 399 | 407 | 422 | 8 | 391 |
| 4 KiB | cluster | before | 277 | 277 | 567,296 | 0 | 0 |
| 4 KiB | cluster | every fault | 282 | 552 | 565,248 | 276 | 0 |
| 4 KiB | cluster | after | 394 | 410 | 29,226 | 16 | 378 |

Run first, 400 hops of 4 KiB loaded 567,296 pages, half the guest, to use
277 of them. After, they loaded 29,226.

### The guest in order

The whole guest through the pager, one page at a time, seconds each round:

| Guest | Source | Before | After |
| --- | --- | --- | --- |
| 8 GiB, 2 MiB | cluster | 17.04, 16.74, 15.83 | 15.91, 15.53, 16.15 |
| 8 GiB, 2 MiB | store | 71.16, 63.22, 67.66 | 95.62, 62.72, 60.04 |
| 4 GiB, 4 KiB | cluster | 23.89, 24.29, 24.66 | 25.95, 27.65, 26.74 |
| 4 GiB, 4 KiB | store | 48.86, 46.82, 45.84 | 66.72, 47.62, 48.79 |

In order the pager read every page once either way. Before, it took one
fault and one load a run: 1,024 at 2 MiB and 512 at 4 KiB. After, it took
two faults and two loads a run, and the second fault of each run waited for
the prefetch already reading its page: 2,048 loads and about 1,000 waits at
2 MiB, 1,024 loads and 512 waits at 4 KiB. The first round from the store
was slow in both builds and both ways, a store warming up rather than either
read.

### Where a 4 KiB hop's 3 ms goes

A hop of 4 KiB from the cluster takes 3.2 ms through the pager and 0.67 ms
straight from the store. The reader's profile of the chain puts 0.62 s of the
1.42 s its faults spent in planning the window: before it reads anything, a
fault asks the volume for the identity of every one of the window's 2,048
pages, and that decodes the page-table segment each time (0.38 s). The
prefetches the hops still start take the rest.

## What it shows

**A chain of dependent faults now waits for about one page a hop.** From the
cluster a 4 KiB hop took 3.2 ms against 41 ms run first, and a 2 MiB hop
7.1 ms against 13.5 ms. From the store a 4 KiB hop took 29 ms against 80 ms,
and a 2 MiB hop 38 ms against 57 ms, the same as a page read straight from the
store. The chains ran 1.4 to 5.9 times as many hops a second.

**A prefetch behind every fault delays the faults after it when reads are
bound by processors.** With every fault prefetching its run, a 4 KiB hop from
the cluster took 24 ms. Each prefetch of 2,047 pages is about 100 ms of
checking and decoding, and the prefetches of the hops before held the four
processors the next hop's read needed. From the store, whose reads wait on
the network, the same chain took 31 ms, near the page's 25. So a fault now
prefetches only when it follows one of its memory region's last eight faults
in its own run or the one before, or is the region's first; a chain at
random prefetched 7 to 16 times in 400 hops.

**Reading forwards still reads in runs, at about the same speed.** No page was
read twice, and each run was two reads instead of one. The 8 GiB guest of
2 MiB pages took 16 s from the cluster either way. The 4 GiB guest of 4 KiB
pages took 26.7 s after against 24.3 s before from the cluster, 10 % more,
and the same from the store. The second fault of each run plans the window
again once the prefetch lands, which at 4 KiB is the segment decode above.

**Planning a 4 KiB window is now the larger part of a hop from the cluster.**
A fault decodes the identities of all 2,048 pages of its window before it
reads its page. Planning the faulting page first, or keeping decoded segments,
would take a 4 KiB hop from 3.2 ms toward the 0.67 ms of the page.

## Cleanup

Every host, its disks and the run's objects were deleted, for all three
attempts. `gcloud compute instances list` and `gcloud compute disks list`
show no `sproutfs-restore-faultfirst` host or disk, and the bucket holds no
object under `sproutfs-bench/sproutfs-restore-faultfirst`.
