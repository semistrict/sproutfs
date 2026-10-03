# Reading a guest's memory from the cluster on GCE, 2026-10-03

Measurement 3 of [the disk cache plan](../../plans/disk-cache-2026-10-02.md#how-it-is-proved),
after step 7 ([reading from the cluster](../hosting.md#reading-from-the-cluster)):
the memory of an 8 GiB guest read back on another host from the cluster
cache, from the store, and from the cluster with one host lost during the
read.

## What ran

Six disposable `n2-standard-4` hosts in us-east4-a (4 vCPUs, Intel Cascade
Lake, 10 Gbps), each with one local NVMe SSD for its cache. The project's CPU
quota is 32 vCPUs with 8 used elsewhere. The store was a real GCS bucket in
the same region. `scripts/bench-restore-gce.sh` ran it with
`cmd/sproutfs-restorebench`, built from 980cc001 and this branch's bench.
Every host was deleted afterwards, with its disks and its objects, and the
script checked that none remained. Raw results are in
`gce-cluster-reads-2026-10-03/`.

This is a node bench, not a guest. Each host ran one process with what a host
runs for the cache: a `checkpoint.Cache` over its SSD with every window in the
cluster share, a peer table and a peer server serving stripes at up to
500 MB/s, all on the list of the six caches under 4+2. There is no
Firecracker and no guest. The memory tier holds 64 MiB, so every page is
read from where it is kept, not from what an earlier case left in memory.

Host 0 published a checkpoint of one volume of 4,096 pages of 2 MiB (8 GiB)
of noise, so nothing compresses. The publication's fills put each window's
stripes on its six ranks. Host 1 then read every page back through
`checkpoint.Store.Read`, 16 pages at a time, and checked each against the
noise. Each case first dropped the kernel's page cache on every host.

- **cluster**: through the cache.
- **store**: through a store with no cache, so every page is a GET.
- **cluster-lost**: through the cache, with host 3's peer server closed two
  seconds into the read and started again after it.

Three rounds ran each case: round 0 in the order cluster-lost, cluster,
store; rounds 1 and 2 in the order store, cluster, cluster-lost.

## Results

The publication took 45 s and its fills settled 20 s later. Host 0 sent
20,560 stripes, 10.7 GB, and the hosts kept all of them. None was dropped.

Each page's read, in milliseconds, the median over the three rounds of each
round's percentile:

| Case | seconds | p50 | p90 | p99 | p99.9 | max |
| --- | --- | --- | --- | --- | --- | --- |
| cluster | 16.4 | 57.7 | 95.5 | 135.7 | 176.0 | 201.8 |
| cluster-lost | 16.4 | 57.0 | 94.3 | 134.1 | 165.4 | 200.2 |
| store | 28.1 | 105.8 | 137.2 | 217.8 | 316.9 | 844.6 |

The p99 of each round:

| Case | round 0 | round 1 | round 2 | spread |
| --- | --- | --- | --- | --- |
| cluster | 133.0 | 135.7 | 136.3 | 3.3 |
| cluster-lost | 134.1 | 134.0 | 138.5 | 4.5 |
| store | 217.8 | 225.9 | 174.7 | 51.2 |

The whole guest took 16.3 to 16.6 s from the cluster, 524 MB/s, and 27.6 to
28.6 s from the store, 306 MB/s. Every page read back right in every case. No
read failed.

The shape of the distribution, pages in each band over the three rounds
(12,288 pages a case):

| Case | 8–16 ms | 16–32 | 32–64 | 64–128 | 128–256 | 256–512 | 512–1024 | 1024–2048 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cluster | 34 | 1,432 | 5,924 | 4,713 | 185 | 0 | 0 | 0 |
| cluster-lost | 35 | 1,355 | 5,996 | 4,705 | 197 | 0 | 0 | 0 |
| store | 0 | 0 | 269 | 10,220 | 1,770 | 24 | 4 | 1 |

Each has one mode. The cluster's is between 32 and 64 ms; the store's is
between 64 and 128 ms and has a tail past 256 ms that the cluster lacks.

Stripe bytes each host served, round 1, in MB (host 1 is the reader):

| Case | host 0 | host 1 | host 2 | host 3 | host 4 | host 5 |
| --- | --- | --- | --- | --- | --- | --- |
| cluster | 1,699 | 0 | 1,714 | 1,739 | 1,725 | 1,718 |
| cluster-lost | 2,090 | 0 | 2,099 | 206 | 2,090 | 2,102 |

In the other rounds every host still serving at the end is within 0.5 % of
these, and host 3 served 191 and 209 MB before it was lost. The five holders served within
2.5 % of each other. The reader read its own stripe of each window from its
SSD and asked four peers, k+1 stripes in all: 16,454 requests for 4,112
windows. So the network carried the guest once, 8.6 GB. After host 3 was
lost, the other four served its share.

CPU each process used over a case, in seconds, round 1:

| Case | host 0 | host 1 (reader) | host 2 | host 3 | host 4 | host 5 |
| --- | --- | --- | --- | --- | --- | --- |
| cluster | 2.3 | 64.0 | 4.8 | 4.5 | 4.8 | 5.5 |
| cluster-lost | 2.6 | 64.3 | 5.7 | 0.6 | 6.2 | 6.8 |
| store | 0.0 | 72.5 | 0.0 | 0.0 | 0.0 | 0.0 |

A holder serving 105 MB/s used about 0.3 CPUs. The serving hosts' memory
was not recorded.

The reader's counters:

| Case | round | second requests | refused | replaced | store hedges | store GETs | marked down |
| --- | --- | --- | --- | --- | --- | --- | --- |
| cluster | 0, 1, 2 | 6, 8, 11 | 0 | 0 | 0 | 1 | 0 |
| cluster-lost | 0 | 5 | 40 | 8 | 6 (40 refused) | 7 | 1 |
| cluster-lost | 1, 2 | 0 | 0 | 6, 4 | 0 | 1 | 1 |

The one GET in each cluster case is the checkpoint's index. No server answered
BUSY and no stripe was wrong.

## What it shows

**The cluster reads a guest 1.7 times as fast as the store, with a tighter
tail.** The median page took 58 ms against 106 ms, and the p99 136 ms
against 218 ms. The store's p99 moved by 51 ms between rounds and its worst
page took 0.6 to 1.2 s. The cluster's p99 moved by 3 to 5 ms and no page took
over 222 ms.

**Losing a host costs nothing.** With host 3 gone two seconds in, the read
took as long, with the same shape, and read no page from the store. The
reader replaced the requests that host refused at once, marked it down on the
refused connection, and asked the other ranks from then on. The probe cleared
the mark before the next case began.

**Round 0's hedges came from the start, not from the loss.** That case was
the first read of the run. The reader's delay starts at its floor of 0.5 ms
and its bound at 10 ms, and both adapt only after 32 windows. The first reads
were past both, so they spent the budget of second requests and the bucket
of store reads, five each, and the rest were refused. None of the six store
reads won. In rounds 1 and 2 the loss caused no second request and no store
read. Under this load the delay settled at about 100 ms, the p95 of the time
to k stripes, and the bound at 400 ms.

**The reader's CPU is the limit.** The reader used 3.9 of its 4 CPUs for the
whole cluster read. It carried 4.2 Gbps of a 10 Gbps link. Its CPU went to
decoding stripes, to the page codec (`internal/blob` checks every page with
SHA-256, and these hosts have no SHA instructions) and to making the noise
each page is compared with. This run did not profile the reader, so how that
divides is not measured. A guest's restore does not compare its pages, so
it would spend less.

**The holders' load is even.** Each window's reader asks k+1 of its first
k+m ranks, chosen by a hash of reader and window. Over 4,096 windows that
spread the read within 2.5 % over the five other hosts.

## What it does not show

- A guest. There is no Firecracker here: a restore's faults and its post-copy
  stream would read the same path, but this run reads pages in order, 16 at a
  time, from one process. [Dependent and random reads](gce-dependent-reads-2026-10-03.md)
  measures faults that wait on each other, at 2 MiB and 4 KiB, and where the
  reader's CPU goes.
- The case with the pages in memory, which the plan lists first. The memory
  tier is a local read and is not what step 7 changed.
- More than one reader. Every host restoring at once would put each holder's
  serving budget, 500 MB/s, to work. Here each holder served 105 MB/s.
- The serving hosts' memory.

## A bug it found

The first run of this bench kept no stripe at all: every keep a host sent was
reset by its peer. The peer server ended a request's receive context before
it read the request's payload. Over TCP a payload is read under the socket's
deadline, which that context sets, so the read failed and the server closed
the connection. The simulated stream does not read under a deadline, which is
why no simulation found it. 980cc001 fixed it, with a test of a keep over a
loopback socket (`TestAKeepIsKeptOverTCP`) and a guard,
`peer-payload-after-its-receive`, that brings the bug back and fails that
test.

## Cleanup

Every host, its disks and the run's objects were deleted.
`gcloud compute instances list` and `gcloud compute disks list` show no
`sproutfs-restore-` host or disk.
