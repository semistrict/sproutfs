# A fault at random that plans its page alone, on GCE, 2026-10-04

[Fault planning](gce-fault-planning-2026-10-04.md) left a dependent 4 KiB
fault from the cluster at 1.05 ms, against 0.67 ms for its page's read alone.
What was left was the window's planning beside the read: a fault located all
2,048 pages of its window and looked each up among the resident pages, about
0.75 ms of processor a fault. A fault at random used none of it but the
resident pages it mapped beside its own. This run measures three changes
against that, through the real pager, before and after
([planning a fault](../vm-memory.md#planning-a-fault)):

- **A fault at random plans its page alone.** Its plan is its page, located
  in one lookup of one page. It locates nothing else of its window and maps
  nothing beside its page. The next fault in that window follows it and plans
  the window, so this costs a guest at most one more fault a window.
- **A fault reading forwards looks at its window once.** It locates the window
  in one lookup, takes its bindings under one lock and looks every page up
  among the resident and in-flight pages under another, once. A page's
  identity is an index into the window's extents rather than a search.
- **A range is one lookup of each segment.** The index reads a range's
  entries off each segment's page table in order
  ([reads](../volumes.md#reads)).

## What ran

Six disposable `n2-highmem-4` hosts in us-east4-a (4 vCPUs, 32 GB, Intel Ice
Lake, 10 Gbps), each with one local NVMe SSD, under 4+2, against a GCS bucket
in the same region, as last time. `scripts/bench-restore-gce.sh` ran
`cmd/sproutfs-restorebench` twice on the same hosts:

- **before**: main at 7dca6c9e, built apart (`SPROUTFS_RESTORE_BINARY`);
- **after**: this change, 86e08f63.

Host 0 published a 4 GiB guest of 4 KiB pages, and every fill was kept. Host
1 read it back, three rounds of every case from the cluster and from the
store, each round in its own order, with every host's memory tiers and page
cache emptied before each case, and then the chain through the pager once
more with its reader's CPU profiled. Every page of every case read back right,
and no read failed. The page tables load lazily, as the first lookup of each
segment needs them. Raw results were kept with the run, outside the
repository.

The cases are those of last time at 4 KiB. A chain through the pager is 400
hops, each to the page the bytes of the last name, faulted in where the pager
does not map it; the latencies are of the hops that faulted, those over
0.2 ms. A chain of pages is 2,000 hops read straight from the store's API,
which is what a fault would cost if it were only its page's read. In order is
the whole guest through the pager, page by page.

## Results

### A chain of faults

Each hop that faulted, in milliseconds: the median over three rounds of each
round's p50, p90 and p99.

| Source | Read | p50 | p90 | p99 |
| --- | --- | --- | --- | --- |
| cluster | page straight from the store, before | 0.66 | 0.71 | 4.89 |
| cluster | page straight from the store, after | 0.66 | 0.72 | 4.87 |
| cluster | through the pager, before | 0.98 | 4.37 | 9.82 |
| cluster | through the pager, after | 0.83 | 5.27 | 13.41 |
| store | page straight from the store, before | 24.53 | 32.81 | 55.15 |
| store | page straight from the store, after | 24.52 | 32.76 | 54.51 |
| store | through the pager, before | 27.51 | 53.11 | 66.68 |
| store | through the pager, after | 28.05 | 53.39 | 72.48 |

The faulted hops from the cluster, each round:

| | Round | p25 | p50 | p75 | mean | over 1 ms | over 2 ms | over 5 ms |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| before | 1 | 0.84 | 1.00 | 2.76 | 2.10 | 198 | 119 | 31 |
| before | 2 | 0.84 | 0.92 | 2.13 | 1.87 | 160 | 103 | 22 |
| before | 3 | 0.83 | 0.98 | 2.32 | 2.15 | 191 | 117 | 36 |
| after | 1 | 0.75 | 0.83 | 2.43 | 2.09 | 143 | 109 | 43 |
| after | 2 | 0.70 | 0.75 | 1.58 | 1.71 | 113 | 94 | 31 |
| after | 3 | 0.83 | 0.94 | 2.24 | 2.51 | 157 | 101 | 50 |

The chains through the pager, hops a second over all 400 hops, and the seconds
each round took:

| Source | Before | After | Before, seconds | After, seconds |
| --- | --- | --- | --- | --- |
| cluster | 499 | 484 | 0.83, 0.74, 0.84 | 0.82, 0.67, 0.98 |
| store | 30.9 | 30.7 | 14.39, 12.11, 12.31 | 14.16, 12.18, 12.73 |

The pager did the same work in both, round by round: in the chain from the
cluster, 392 to 394 faults, 404 to 409 loads and 10 to 15 prefetches; 376 to
384 faults were at random and read their page alone.

### Where a 4 KiB hop's time goes now

The reader's profile of the chain from the cluster, once more after the rounds:

| | Before | After |
| --- | --- | --- |
| profiled read | 1.24 s | 0.74 s |
| samples, every goroutine | 3.38 s | 1.47 s |
| the faults | 0.41 s | 0.05 s |
| planning the window beside the read | 0.33 s | none |
| the garbage collector's marking | 0.73 s | none sampled |
| the prefetches | 0.27 s | 0.20 s |
| decoding segments | 0.14 s | 0.08 s |

### Planning a fault, on one host

`BenchmarkARandom4KiBFault` and `BenchmarkAForward4KiBFault`
(`vmmemory/fault_planning_bench_test.go`) fault a 4 KiB pager with the host's
2,048-page window over a real checkpoint index of three segments, its tables
decoded and held. The backing locates through the index and makes up the
bytes of a read, so a fault's time is its planning and the pager's own work.
A fault at random is one in a window none of the eight before it touched. A
fault reading forwards plans its window, and the pager's one prefetch is held
reading, so it then reads its page alone: the slots it took for the prefetch
go back each time. Both builds ran on host 5 once the reads were over, three
rounds each, interleaved, 20,000 faults a run:

| | Processors | Before | After | Before, allocated | After, allocated |
| --- | --- | --- | --- | --- | --- |
| at random | 1 | 572 to 577 µs | 10.2 to 10.4 µs | 534 KB in 67 | 7 KB in 37 |
| at random | 4 | 545 to 551 µs | 8.6 to 9.1 µs | 539 KB in 67 | 8 KB in 37 |
| reading forwards | 1 | 2.20 to 2.22 ms | 1.03 to 1.04 ms | 988 KB in 1,789 | 712 KB in 1,781 |
| reading forwards | 4 | 2.14 to 2.16 ms | 0.99 to 1.00 ms | 1.0 MB in 1,789 | 736 KB in 1,781 |

What a forward fault has left here is mostly the slots it takes for its
prefetch and gives back when the prefetch is refused, one at a time, against
the host's budget, which a fault whose prefetch runs does not give back.

### The guest in order

The whole 4 GiB guest through the pager, one page at a time, seconds each
round:

| Source | Before | After |
| --- | --- | --- |
| cluster | 25.98, 26.55, 26.05 | 25.53, 25.59, 25.53 |
| store | 57.45, 50.56, 48.36 | 50.01, 46.48, 44.28 |

Every run read every page once: 1,024 loads and 512 prefetches.

## What it shows

**A fault at random no longer plans its window.** On one Ice Lake processor
it costs 10 µs against 575 µs, and allocates 7 KB against 534 KB. In the
profiled chain the faults' processor time went from 0.41 s to 0.05 s, the
planning of the window beside the read, 0.33 s, is gone, and so is the
garbage collector's marking that its allocations made.
The median hop from the cluster went from 0.98 to 0.83 ms, against 0.66 ms for
the page alone; from the store, whose reads wait on the network, it did not
move.

**What is left of a 4 KiB hop from the cluster is its tail, and it is not
planning.** About a quarter of the hops took over 2 ms, before and after
alike. They are the first fault in each of the guest's 64 segments, which
fetches and decodes the segment's table, and the hops beside the ten to
fifteen prefetches a chain starts, each of 2,047 pages. The tail moved with
the rounds, not with the change; loading the tables when the checkpoint opens
takes the first out ([fault planning](gce-fault-planning-2026-10-04.md)).

**A fault reading forwards plans its window in half the time, and reading in
order did not get slower.** It still plans its whole window, now in one look:
1.03 ms against 2.21 ms on one processor in the benchmark. The guest in order
took 25.5 s from the cluster after against 26.0 to 26.6 s before, and 44 to
50 s from the store against 48 to 57 s.

## Cleanup

Every host, its disks and the run's objects were deleted.
`gcloud compute instances list` and `gcloud compute disks list` show no
`sproutfs-restore-plan81` host or disk, and the bucket holds no object under
`sproutfs-bench/sproutfs-restore-plan81`.
