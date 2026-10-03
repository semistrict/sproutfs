# Dependent and random reads of a guest's memory on GCE, 2026-10-03

This extends [the cluster reads](gce-cluster-reads-2026-10-03.md). That run read
an 8 GiB guest in order, 16 pages at a time. The cluster was only 1.7 times as
fast as the store, because the reader ran out of CPU. A guest's faults are not
like that. A fault often depends on the one before: the guest follows a
pointer, and the next address is known only once the page is in. Such a chain
pays every read's whole latency, one after another. This run measures that
case, random reads, and the sequential read again, each from the cluster and
from the store, at both page sizes. It also profiles the reader.

## What ran

The hosts, the cluster and the store were the same as last time: six
disposable `n2-standard-4` hosts in us-east4-a (4 vCPUs, Intel Cascade Lake,
10 Gbps), each with one local NVMe SSD, under 4+2, against a GCS bucket in the
same region. `scripts/bench-restore-gce.sh` ran `cmd/sproutfs-restorebench`
built from 28510d90 with the `calibrate` mode of 20960974. Every host, disk
and object was deleted afterwards, and the script checked that none remained.
Raw results are in `gce-dependent-reads-2026-10-03/`.

Host 0 published two guests, each one checkpoint of one volume of noise:

- an 8 GiB guest of 4,096 pages of 2 MiB, the default RAM page;
- a 4 GiB guest of 1,048,576 pages of 4 KiB. A deployment may run RAM or
  PMEM at 4 KiB (`SPROUTFS_RAM_PAGE_BYTES`, `SPROUTFS_PMEM_PAGE_BYTES`), so
  this is a real case. A window is still 2 MiB: 512 pages.

Every page names two others in its first bytes. Bytes 0 to 7 name the next
page of one random cycle through all the pages. In the first page of each
8 MiB run, bytes 8 to 15 name the next run of one random cycle through all the
runs. The fills of both publications kept every stripe; none was dropped.

Host 1 read each guest back in these ways:

- **chain**: one read at a time, each at the page or run the bytes of the last
  read name, so no read can start before the last one ends. 2,000 hops of a
  page, or 400 of a run.
- **random**: 2,000 pages in a random order, none twice, 1, 4 or 16 at a time.
- **sequential**: every page in order, 16 at a time, as the last run did. At
  4 KiB a page at a time would be a million requests, so the 4 KiB guest was
  read by runs.

A **run** is 8 MiB, the read-ahead run a pager's fault reads
(`host/pager.go`, `readAheadBytes`): 4 pages of 2 MiB or 2,048 of 4 KiB. A
fault on a page the guest has not touched reads its whole run. So a chain of
runs is what a chain of a guest's faults costs today, and a chain of pages is
what it would cost if a fault read only its page.

Each case read from the cluster (through the cache) and from the store (no
cache, every page a GET). Three rounds ran every case from both sources, each
round in its own order. Before each case every host emptied its memory tiers
and dropped the kernel's page cache. No case read a page twice. The memory
tier served none of the pages; it served only page-table segments that more
than one read looked up at once.

The bench no longer makes noise and compares strings on each read. The reader
takes a CRC-32C of each page after the read and checks it against sums made
before the reads. That costs 0.09 ms of a 2 MiB page and is outside the read's
time. Every page of every case read back right. No read failed.

The reader timed each step of a read on one processor before the rounds
(`sproutfs-restorebench calibrate`). After the rounds it read the sequential
and chain cases once more under the CPU profiler. Those reads are not in the
tables.

## Results

### A chain of reads

Each hop's read, in milliseconds, the median over three rounds of each
round's percentile, and hops a second:

| Guest | Read | Source | p50 | p90 | p99 | p99.9 | max | hops/s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | page | cluster | 10.3 | 11.5 | 14.4 | 18.4 | 19.2 | 93 |
| 2 MiB | page | store | 41.5 | 49.8 | 68.0 | 172.1 | 251.0 | 23.5 |
| 2 MiB | run | cluster | 20.2 | 24.8 | 30.9 | 33.7 | 33.7 | 46 |
| 2 MiB | run | store | 58.3 | 68.0 | 120.5 | 222.0 | 222.0 | 16.2 |
| 4 KiB | page | cluster | 0.65 | 0.72 | 8.4 | 10.4 | 11.0 | 1,106 |
| 4 KiB | page | store | 24.6 | 32.3 | 60.5 | 277.0 | 367.1 | 38.8 |
| 4 KiB | run | cluster | 39.3 | 53.9 | 71.2 | 83.2 | 83.2 | 23.1 |
| 4 KiB | run | store | 84.6 | 114.2 | 181.7 | 402.9 | 402.9 | 11.0 |

Hops in each band of milliseconds, over the three rounds:

| Guest | Read | Source | <0.5 | 0.5–1 | 1–2 | 2–4 | 4–8 | 8–16 | 16–32 | 32–64 | 64–128 | 128–256 | 256–512 | 512+ |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | page | cluster | 0 | 0 | 0 | 0 | 0 | 5,972 | 28 | 0 | 0 | 0 | 0 | 0 |
| 2 MiB | page | store | 0 | 0 | 0 | 0 | 0 | 0 | 160 | 5,695 | 138 | 6 | 1 | 0 |
| 2 MiB | run | cluster | 0 | 0 | 0 | 0 | 0 | 0 | 1,189 | 11 | 0 | 0 | 0 | 0 |
| 2 MiB | run | store | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 685 | 501 | 13 | 1 | 0 |
| 4 KiB | page | cluster | 16 | 5,790 | 2 | 0 | 13 | 179 | 0 | 0 | 0 | 0 | 0 | 0 |
| 4 KiB | page | store | 0 | 0 | 0 | 0 | 0 | 118 | 4,343 | 1,434 | 98 | 2 | 5 | 0 |
| 4 KiB | run | cluster | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 1,171 | 29 | 0 | 0 | 0 |
| 4 KiB | run | store | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 74 | 1,076 | 46 | 3 | 1 |

Every distribution has one mode but one. The 4 KiB chain from the cluster
has a second: its 192 hops past 4 ms are exactly 64 a round, the first read
in each of the guest's 64 page-table segments. That read fetches the segment,
about 330 KB, checks it and decodes 16,384 entries before it reads the page.

### Random reads

Each read, in milliseconds, median over the rounds, and reads a second:

| Guest | At a time | Source | p50 | p90 | p99 | p99.9 | max | reads/s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | 1 | cluster | 10.3 | 12.8 | 14.8 | 16.6 | 19.0 | 92 |
| 2 MiB | 1 | store | 41.5 | 50.4 | 83.9 | 418.0 | 975.1 | 23.0 |
| 2 MiB | 4 | cluster | 15.4 | 18.9 | 24.9 | 29.6 | 33.7 | 249 |
| 2 MiB | 4 | store | 44.0 | 56.1 | 84.3 | 366.7 | 522.6 | 86 |
| 2 MiB | 16 | cluster | 59.1 | 96.3 | 134.4 | 180.5 | 183.9 | 251 |
| 2 MiB | 16 | store | 140.3 | 159.3 | 206.9 | 474.2 | 716.6 | 113 |
| 4 KiB | 1 | cluster | 0.68 | 0.75 | 8.8 | 14.6 | 17.7 | 1,039 |
| 4 KiB | 1 | store | 24.2 | 32.1 | 61.8 | 217.4 | 573.1 | 38.7 |
| 4 KiB | 4 | cluster | 0.66 | 0.90 | 14.7 | 21.5 | 21.6 | 3,144 |
| 4 KiB | 4 | store | 22.5 | 30.9 | 63.2 | 161.6 | 178.6 | 158 |
| 4 KiB | 16 | cluster | 1.6 | 3.1 | 43.3 | 121.0 | 121.4 | 4,266 |
| 4 KiB | 16 | store | 26.1 | 35.5 | 79.2 | 116.3 | 175.6 | 567 |

### The guest in order

| Guest | Read | Source | seconds | MB/s | p50 | p99 | max | reader CPUs |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 8 GiB, 2 MiB | page, 16 at a time | cluster | 15.6 | 551 | 57.0 | 132.2 | 181.1 | 3.88 |
| 8 GiB, 2 MiB | page, 16 at a time | store | 24.8 | 346 | 95.4 | 145.3 | 1,193 | 2.72 |
| 4 GiB, 4 KiB | run, 16 at a time | cluster | 15.0 | 286 | 448.5 | 891.4 | 1,036 | 3.87 |
| 4 GiB, 4 KiB | run, 16 at a time | store | 20.2 | 213 | 624.4 | 913.6 | 988.7 | 2.27 |

The 2 MiB guest took 15.6 s from the cluster against 16.4 s last time, and
24.8 s from the store against 28.1 s. The reader's CPU was 14.8 ms a page
against 15.6 ms: the bench's own check was about 1 ms of it.

### Where the reader's CPU goes

The steps of a read, timed alone on one processor of the reader:

| Step | 2 MiB page | 4 KiB page |
| --- | --- | --- |
| SHA-256 | 5.67 ms | 11.1 µs |
| decode (framing and SHA-256; noise is stored raw) | 5.56 ms | 11.2 µs |
| join from 4 data stripes | 0.35 ms | 1.1 µs |
| join from 2 data and 2 parity | 0.61 ms | 2.4 µs |
| split under 4+2 | 0.42 ms | 1.9 µs |
| CRC-32C | 0.09 ms | 0.2 µs |
| copy | 0.22 ms | 0.05 µs |
| make the page's noise (what the bench used to do) | 0.51 ms | 1.0 µs |

The profiled reads, CPU per read in milliseconds (per page at 4 KiB run
reads, in microseconds), by what the samples were doing:

| Step | 2 MiB chain, cluster | 2 MiB chain, store | 2 MiB in order, cluster | 2 MiB in order, store | 4 KiB chain, cluster | 4 KiB chain, store | 4 KiB in order, cluster (µs a page) | 4 KiB in order, store (µs a page) |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| SHA-256 | 6.14 | 6.45 | 9.21 | 8.93 | 0.07 | 0.06 | 19.1 | 18.2 |
| copies | 1.73 | 1.21 | 2.58 | 1.73 | 0.01 | 0.01 | 3.3 | 3.2 |
| zeroing new buffers | 0.66 | 1.11 | 0.81 | 0.97 | 0.02 | 0.03 | 1.6 | 2.0 |
| Reed-Solomon | 0.32 | 0 | 0.23 | 0 | 0 | 0 | 1.2 | 0 |
| CRC-32C of stripes | 0.10 | 0 | 0.18 | 0 | 0.01 | 0 | 0.6 | 0 |
| TLS | 0 | 1.22 | 0 | 1.13 | 0.01 | 0.06 | 0.5 | 2.7 |
| system calls | 0.69 | 1.56 | 0.69 | 1.21 | 0.07 | 0.06 | 2.1 | 2.2 |
| allocation and GC | 0.58 | 0.47 | 0.52 | 0.43 | 0.04 | 0.17 | 12.0 | 4.6 |
| protobuf | 0.03 | 0 | 0.02 | 0 | 0.07 | 0.05 | 1.6 | 0.9 |
| scheduler | 0.26 | 1.29 | 0.03 | 0.57 | 0.10 | 0.09 | 0.1 | 1.4 |
| other | 0.43 | 1.26 | 0.38 | 1.15 | 0.30 | 0.46 | 13.3 | 7.3 |
| bench's CRC-32C | 0.16 | 0.17 | 0.35 | 0.24 | 0 | 0 | 0.6 | 0.6 |
| **total** | **11.1** | **14.7** | **15.0** | **16.4** | **0.70** | **0.97** | **56.0** | **43.0** |

SHA-256 is one call a page, in `blob.Decode`. It costs 9.2 ms in order and
6.1 ms in the chain against 5.7 ms alone, because an `n2-standard-4` is two
cores with two threads each, and four busy threads share two cores' vector
units. The copies of a 2 MiB page read from the cluster are seven: each
stripe out of its reply (`windowRead.parse`), the stripes into the envelope
(`stripe.rebuild`), the page out of the envelope and the envelope kept for
repair (`windowRead.join`), the page into the memory tier (`Cache.run`), and
into the caller's buffer (`fillPages`), which first zeroes it. From the store,
`io.ReadAll` grows its buffer by doubling, zeroing each one, and the HTTP/2
body is copied twice.

At 4 KiB the cluster's "other" is the read's bookkeeping per page: a linear
search of the window's pages for each stripe that arrives
(`windowRead.take`, 2.1 µs a page), map lookups, context lookups of the
simulation's runtime from Buggify sites (0.8 µs), and decoding page-table
segments that 16 reads load at once.

### Reading a 2 MiB page from the cluster, step by step

For the 2 MiB chain from the cluster, 10.3 ms a hop:

- SHA-256 of the envelope: 6.1 ms;
- copies and zeroing: 2.4 ms;
- Reed-Solomon, CRC-32C, allocation and GC: about 1 ms;
- the rest, about 1 ms, is waiting. Four stripes of 512 KiB take 0.4 ms on a
  10 Gbps link, and each holder reads its stripe from its own SSD.

So about 90 % of a hop from the cluster is the reader's own CPU. From the
store a hop takes 41.5 ms, of which 14.7 ms is CPU, 6.5 ms of it SHA-256. At
least 27 ms is waiting for GCS.

### The serving hosts

In every 2 MiB case and every case of runs, the five holders each served
within 3 % of the others. A holder serving the 2 MiB chain served 39 MB/s of
stripes and used 0.1 CPU. No chain case asked a second request. No server
answered BUSY.

The reader read the store past the bound 3 to 8 times in 12 of the 36 cluster
cases, and the bucket refused 156 more: it holds five and earns a twentieth a
read. Every
one of those cases followed a case whose reads were quicker: the first case of
the run, a 2 MiB case after a 4 KiB one, runs after pages, or 16 at a time
after one at a time. The delay and the bound are one estimate for all the
host's reads, updated every 32. After 4 KiB reads, whose stripes arrive in
0.5 ms, the bound sits at its floor of 10 ms, and the first 2 MiB reads,
10 ms each, pass it and read the store as well. Ten of those store reads
answered first. No stripe was wrong, no request timed out and no host was
marked down.

### Ice Lake, with SHA instructions

Cascade Lake has no SHA instructions; Ice Lake does, and us-east4-a offers it
for n2 hosts. Six hosts with `--min-cpu-platform="Intel Ice Lake"` ran the
chain and sequential cases, three rounds, built from 20960974. They were
`n2-highmem-4`: the same 4 vCPUs and 10 Gbps, with 32 GB. On an
`n2-standard-4` the faster publisher outran its keeps: the fills overflowed
their 4 GiB queue and dropped 5,436 stripes, and the kernel killed the
publisher for memory in the second publication. The drive now refuses a
publication that dropped a stripe, and the node takes the queue's size as a
flag. With 32 GB and a 12 GiB queue no stripe was dropped. Raw results are in
`gce-dependent-reads-2026-10-03-icelake-highmem/`.

SHA-256 of a 2 MiB page took 1.67 ms alone, against 5.67 ms on Cascade Lake;
of a 4 KiB page, 3.3 µs against 11.1 µs. The other steps took about as long.

Each hop, p50 and p99 in milliseconds, and hops a second:

| Guest | Read | Source | Cascade Lake p50 | p99 | hops/s | Ice Lake p50 | p99 | hops/s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | page | cluster | 10.3 | 14.4 | 93 | 6.5 | 9.6 | 148 |
| 2 MiB | page | store | 41.5 | 68.0 | 23.5 | 37.3 | 86.7 | 25.3 |
| 2 MiB | run | cluster | 20.2 | 30.9 | 46 | 12.8 | 19.8 | 71 |
| 2 MiB | run | store | 58.3 | 120.5 | 16.2 | 51.7 | 102.2 | 17.9 |
| 4 KiB | page | cluster | 0.65 | 8.4 | 1,106 | 0.71 | 9.0 | 1,017 |
| 4 KiB | page | store | 24.6 | 60.5 | 38.8 | 24.4 | 61.2 | 38.8 |
| 4 KiB | run | cluster | 39.3 | 71.2 | 23.1 | 34.5 | 64.9 | 25.8 |
| 4 KiB | run | store | 84.6 | 181.7 | 11.0 | 68.7 | 165.0 | 13.2 |

The guest in order:

| Guest | Source | Cascade Lake seconds | MB/s | Ice Lake seconds | MB/s | Ice Lake reader CPUs |
| --- | --- | --- | --- | --- | --- | --- |
| 8 GiB, 2 MiB | cluster | 15.6 | 551 | 9.2 | 934 | 3.91 |
| 8 GiB, 2 MiB | store | 24.8 | 346 | 25.2 | 341 | 2.04 |
| 4 GiB, 4 KiB | cluster | 15.0 | 286 | 11.6 | 370 | 3.89 |
| 4 GiB, 4 KiB | store | 20.2 | 213 | 13.2 | 325 | 2.67 |

The reader's CPU a read, from the profiles: the 2 MiB chain from the cluster
took 7.5 ms, against 11.1 ms, of which SHA-256 was 2.1 ms against 6.1 ms and
copies and zeroing 2.4 ms, as before. A 2 MiB page read in order took 9.4 ms
of CPU against 15.0 ms. A 4 KiB page read in order by runs took 48 µs against
56 µs, of which SHA-256 was 4.8 µs; allocation and GC were 14 µs and the
read's bookkeeping 16 µs.

## What it shows

**A dependent read gains far more from the cluster than a read in order.**
A chain of 2 MiB pages ran 4.0 times as fast from the cluster as from the
store: 93 hops a second against 23.5. A chain of 4 KiB pages ran 28 times as
fast, 1,106 against 39, and its median hop was 38 times as fast, 0.65 ms
against 24.6 ms. The guest in order ran 1.6 times as fast. The tail moved
the same way: the store's p99.9 was 172 ms for a 2 MiB hop and 277 ms for a
4 KiB one, the cluster's 18 ms and 10 ms.

**Random reads in parallel gain less, for the reasons the sequential read
did.** At 16 at a time the reader ran out of CPU on 2 MiB pages (3.79 CPUs),
and the cluster was 2.2 times as fast. At 4 KiB the store's GETs overlap well:
16 at a time read 567 pages a second from the store against 39 for one. The
cluster still read 7.5 times as many.

**A fault reads 8 MiB, and that costs a chain most of what the cluster
saves.** At 4 KiB a page from the cluster took 0.65 ms, and the run a fault
reads took 39 ms: 2,048 pages at about 51 µs of CPU each, 105 ms of CPU
spread over 2.4 threads. A chain of faults at 4 KiB is 2.2 times as fast
from the cluster, not 38. At 2 MiB the run is four pages and the chain is 2.9
times as fast. The read-ahead was chosen for boots and restores, which walk
memory forwards. For a fault whose neighbours the guest will not touch soon,
it is 60 times the latency of the page.

**A 2 MiB read from the cluster is mostly the reader hashing and copying.**
Of 10.3 ms, 6.1 ms is SHA-256 and 2.4 ms copies, on processors with no SHA
instructions. The network and the holders' disks are about 1 ms.

**SHA instructions widen the cluster's lead; the store does not move.** On
Ice Lake the 2 MiB chain took 6.5 ms a hop from the cluster, 5.7 times as
fast as the store's 37.3 ms, and the guest in order 9.2 s against 25.2 s, 2.7
times. The reader was still out of CPU in order, at 934 MB/s. Hashing a 2 MiB
page now costs about what its copies cost. A read of the store is GCS's
latency, and faster hashing took 4 ms off it.

**Page tables make the 4 KiB tail.** A cold 4 GiB guest at 4 KiB has 64
page-table segments of about 330 KB. The first read in each fetches the
segment, checks its SHA-256 and decodes 16,384 entries, and takes 4 to 16 ms.
That is the cluster's whole p99 at 4 KiB.

**The hedge's one delay mixes sizes of read.** A host that reads 4 KiB pages,
2 MiB pages and 8 MiB runs keeps one delay for all of them. After a burst of
quick reads, the slower ones pass the bound and read the store as well, until
the bucket is spent. That happened in a third of the cluster cases here. It
cost little, because the bucket is small, but the bound is wrong for one size
whenever a host reads more than one.

## What the plan should change

- **Measure a restore by its dependent faults.** Measurement 3 in the plan
  reads the guest in order. That is the case the cluster helps least. The
  plan's estimates should be checked against the chain: 10 ms against 42 ms a
  2 MiB page, 0.65 ms against 25 ms a 4 KiB page, and 20 against 58 ms and
  39 against 85 ms for the runs a fault reads today.
- **Serve the faulting page first.** A fault should read its own page, map
  it and wake the guest, then read the rest of its run behind it. At 4 KiB
  from the cluster that is 0.65 ms a fault instead of 39 ms. This is a pager
  change, not a cache change, and it is where most of the cluster's gain for
  dependent faults is.
- **Run on processors with SHA instructions, then decide whether the hash
  can leave the fault's path.** Choosing hosts that have them is a
  deployment setting and needs no code: on n2 it is a least CPU platform of
  Ice Lake, and it took a 2 MiB fault from the cluster from 10.3 to 6.5 ms. Each
  stripe already carries a CRC-32C and the key it was kept under. The
  envelope's SHA-256 is what catches stripes that are each right but do not
  make one envelope, and a page from the store is checked by it too. A
  cheaper check of the envelope, recorded beside the SHA-256, would keep that
  guarantee for a fraction of the cost. That is a format change, and it
  should wait for the hosts' choice.
- **Cut the copies.** Seven copies or zeroings of a 2 MiB page are 2.4 ms a
  read on these hosts. `fillPages` zeroes a page it is about to overwrite
  whole, the envelope is cloned for a repair that rarely runs, and the page is
  copied out of the envelope although the envelope is raw.
- **Load a 4 KiB volume's page table at open.** Its segments make the tail of
  a cold restore at 4 KiB. Reading them all when the checkpoint opens, or
  behind the first fault, moves 64 slow faults out of the guest's way.
- **Give the hedge a delay per size of read.** The delay and the bound should
  be estimated for each page size and run length, or per byte, so a burst of
  one size does not set the bound for another.
- **Size a publisher's fill queue to what it publishes.** A host with SHA
  instructions published an 8 GiB guest faster than its keeps went, and the
  4 GiB queue overflowed. A queue that drops a publication's fills leaves
  windows the cluster never holds, and a queue large enough costs the host
  memory. Keeps that go to several holders at once, or a publication that
  waits for its fills past a bound, would keep up.

## Cleanup

Every host, its disks and the run's objects were deleted.
`gcloud compute instances list` and `gcloud compute disks list` show no
`sproutfs-restore-` host or disk, and the bucket holds no object under
`sproutfs-bench/sproutfs-restore-`. Two hosts made only to time the steps on
each processor were deleted as well.
