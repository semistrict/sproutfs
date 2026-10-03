# Whole reads against 4+1 and 4+2 stripes on GCE, 2026-10-03

The first measurement of [the disk cache plan](../../plans/disk-cache-2026-10-02.md)
(TASK-81): does reading k of k+m stripes from several hosts beat one whole read
from one host, and does 4+2 beat 4+1 when hosts are drained and slow?

## What ran

Six disposable `n2-standard-4` hosts in us-east4-a, each with one local NVMe
SSD. The project's CPU quota is 32 vCPUs and another VM held 8, so the hosts
are `n2-standard-4` (10 Gbps) rather than the planned `n2-standard-8`
(16 Gbps). Every host was deleted afterwards, with its disks, and the script
checked that none remained.

Each host ran `cmd/sproutfs-stripebench` as a server holding its stripes of the
same 4096 objects of 350,000 bytes, placed by rendezvous hashing. A client asks
an object's first k+m ranks at once and finishes at the first k stripes. The
source was a79a77d7 with the script's machine-type setting. Raw results are in
`gce-stripes-2026-10-03/`.

Codes: whole (1+0), 4+1 and 4+2. Conditions: healthy; one server slow by
20 ms; one server drained (removed from the list, so the next rank answers a
miss); drained and slow; drained and stalled (never answers). Four passes:

- idle-memory: one client at 500 reads/s, stripes served from the page cache;
- full-memory: all six clients at once at 1500 reads/s each;
- idle-disk: one client at 500 reads/s, every stripe read from the SSD;
- full-disk: all six clients at 500 reads/s each, every stripe from the SSD.

Each case ran 20 s. Latencies are of hits, in milliseconds, and include
decoding.

## Results

Idle, from the SSD (the memory pass is within a few hundredths of it):

| Case | Whole p50 / p99.9 | 4+1 p50 / p99.9 | 4+2 p50 / p99.9 |
| --- | --- | --- | --- |
| healthy | 0.94 / 1.98 | 0.94 / 1.88 | 0.96 / 1.98 |
| slow | 1.07 / 22.8, p90 21.2 | 0.91 / 1.92 | 0.93 / 1.95 |
| drained | 17 % misses | 0.93 / 1.88 | 0.94 / 1.95 |
| drained and slow | 17 % misses, p90 21.8 | 21.2 / 22.8 | 0.89 / 1.95 |
| drained and stalled | 14 % misses, 19 % timeouts | 67 % timeouts | 0.94 / 1.95, no timeouts |

All six clients at once, from the SSD at 500 reads/s each: the same picture.
4+2 keeps p99.9 at 2.0 ms or better in every condition. 4+1 loses the
drained-and-slow case (p50 20.7 ms) and times out 67 % of reads when drained
and stalled.

All six clients at once, from memory at 1500 reads/s each:

| Case | Whole p99 / p99.9 | 4+1 p99 / p99.9 | 4+2 p99 / p99.9 |
| --- | --- | --- | --- |
| healthy | 29.1 / 62.4 | 2.1 / 30.1 | 110 / 466 |
| drained | 29.6 / 58.2 | 1.9 / 2.3 | 87 / 237 |
| drained and slow | 37.2 / 66.6 | 64.5 / 145 | 141 / 233 |

## What it shows

**Striping costs nothing at the median.** Whole, 4+1 and 4+2 all read at
about 0.9 ms, from memory or from the SSD. Decoding a 350 KB object is not
visible at this resolution.

**4+2 is the right code.** Only 4+2 survives a drained host and a slow or
stalled one together, which is what a rolling restart with one bad host looks
like. 4+1 survives one or the other. A whole read survives neither: a drained
host costs 17 % of reads, and a slow one puts 17 % of them at 21 ms.

**Asking all k+m holders costs too much under load.** At 9,000 reads/s across
the six hosts, 4+2's tail grew to 110 ms at p99 and 466 ms at p99.9, worse
than whole reads. Asking all six holders sends 525 KB per 350 KB read, and
every holder sends its stripe whether or not the reader still needs it. That
is about 790 MB/s from each host, most of a 10 Gbps link, and the queues show
in the tail. The drained case, which sends 438 KB per read, is better, and 4+1
at 438 KB is better still. The bytes, not the code, are the cost. This is the
lesson the FoundationDB note draws from its own load balancer
([research](../research/foundationdb-transport-2026-10-03.md)): ask fewer
holders and hedge to the rest.

**The SSD is not the bottleneck.** The disk passes match the memory passes at
the same rate. At 500 reads/s from six clients, each SSD served about 260 MB/s
of 4+2 stripes and no tail moved.

## What the plan should change

The plan asks all k+m ranks at once. The measurement says to ask k+1 ranks
first, and the remaining ranks only after a short adaptive delay under a
budget, as FoundationDB hedges. That keeps 4+2's protection against a drained
and a slow host, at the bytes of 4+1. A second run of the full-memory pass
with that read pattern checks it before step 7 is built.
