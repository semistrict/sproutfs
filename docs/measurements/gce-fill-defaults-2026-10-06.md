# The default rate of keeps and fill queue on GCE, 2026-10-06

A host sends a publication's keeps to its peers at `CacheConfig.FillBytesPerSecond`,
and holds its fills in a queue of `CacheConfig.FillQueueBytes`
([filling the cluster](../hosting.md#filling-the-cluster)). The defaults were
128 MiB/s and 64 MiB. The rate is there to protect the faults of the guests on
the hosts that fill. But it held an 8 GiB publication near the pace of one keep
at a time ([side by side](gce-fill-side-by-side-2026-10-04.md)). No report had
measured what a faster rate costs those faults. This one measures publication
time, memory and faults at several rates and queues, and sets the defaults from
them.

The rate of keeps is now 192 MiB/s. The queue stays at 64 MiB.

## What ran

Six disposable `n2-standard-4` hosts in us-east4-a with
`--min-cpu-platform="Intel Ice Lake"` (4 vCPUs, 16 GB), each with one local
NVMe SSD, under 4+2, against the bench's GCS bucket in the same region, as in
[the paced fills](gce-fill-backpressure-2026-10-04.md) and
[side by side](gce-fill-side-by-side-2026-10-04.md).
`scripts/bench-restore-gce.sh run` ran each case on fresh node processes and
fresh cache files, with a prebuilt binary of 6308d1ff. Every host ran with the
case's queue and rate. A rate of 4 GiB/s binds nothing, and stands for
unpaced. Two sets of hosts ran: the first ran the grid and the repeats at
256 MiB/s, the second the cases at 160 and 192 MiB/s and one more at
256 MiB/s.

Each case did this, in order:

1. Host 0 published a guest of 2 GiB and then one of 8 GiB, 1,024 and 4,096
   pages of 2 MiB of noise, each new to the cluster. The 8 GiB guest is the
   size of the earlier reports. A demo host holds 3.75 GiB of guests, so
   2 GiB is a realistic suspend. These rows give the publication's time, what
   its fills dropped, the queue's high-water mark and the publisher's peak
   RSS. The smaller guest goes first, so its peak RSS is its own.
2. Host 2 published a guest of 4 GiB for the faults to read.
3. With nothing publishing, host 0 and host 1 each ran a chain of 500 faults
   over that guest.
4. Host 0 published each guest once more. Host 0 and host 1 ran a chain of
   faults beside each publication, from its start until its fills settled.

Host 0 is the publisher, whose fills send the keeps. Host 1 is one of the
holders that write them. Every host's memory tiers and page cache were
emptied before each chain, so each fault read its stripes from the disks.

A chain of faults reads through the real pager, from the cluster, each hop to
the page the bytes of the last one name
([fault first](gce-fault-first-2026-10-04.md)). It waits 25 ms before each hop,
as a guest computes between faults, so it spans a publication without visiting
a page twice. It takes a new pager every 128 hops, so the pager's memory stays
under about 1 GiB. A hop under 1 ms found its page already in, brought by a
prefetch, and is left out; nine hops in ten faulted. The tables give the p99
of the hops that faulted, pooled over both publications of a case: 900 to
2,400 faults on each host.

The raw results and logs are kept outside the tree with the run.

## Results

Every stripe of every publication landed on its holder. No keep was dropped,
and no publication waited out the bound. Commit is the median over runs of the
publication alone. Peak RSS is the publisher's highest. Fault p99 is the median
over runs, with each run's value after it. Idle is host 0's chain with nothing
publishing; host 1's was within 2 ms of it in every case.

| Queue | Rate | Runs | 2 GiB commit s | 8 GiB commit s | Peak RSS GiB | Idle p99 ms | Publisher p99 ms | Holder p99 ms |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 64 MiB | 128 MiB/s | 2 | 20.5 | 83.7 | 1.69 | 11.4 | 27.0 (26, 28) | 10.6 (10, 11) |
| 64 MiB | 160 MiB/s | 2 | 16.6 | 64.5 | 1.75 | 12.3 | 31.7 (27, 37) | 12.0 (12, 12) |
| 64 MiB | 192 MiB/s | 3 | 14.0 | 53.9 | 1.80 | 12.1 | 31.2 (30, 37, 31) | 11.7 (13, 12, 11) |
| 64 MiB | 256 MiB/s | 4 | 11.0 | 41.6 | 1.81 | 11.8 | 50.1 (35, 53, 47, 68) | 13.8 (12, 13, 15, 15) |
| 64 MiB | 512 MiB/s | 1 | 8.2 | 32.4 | 1.88 | 11.6 | 73.0 | 56.7 |
| 64 MiB | 1 GiB/s | 1 | 10.9 | 47.7 | 1.83 | 10.9 | 49.4 | 13.7 |
| 64 MiB | 2 GiB/s | 1 | 9.4 | 34.8 | 1.83 | 12.0 | 59.6 | 52.7 |
| 64 MiB | unpaced | 1 | 10.2 | 41.2 | 1.84 | 12.4 | 45.3 | 29.0 |
| 256 MiB | 128 MiB/s | 1 | 19.0 | 78.9 | 2.30 | 11.7 | 25.0 | 10.9 |
| 256 MiB | 256 MiB/s | 3 | 10.7 | 40.9 | 2.44 | 12.3 | 58.6 (53, 59, 76) | 15.7 (16, 14, 16) |
| 256 MiB | 512 MiB/s | 1 | 10.0 | 37.7 | 2.61 | 12.3 | 52.3 | 17.1 |
| 256 MiB | 1 GiB/s | 1 | 8.7 | 31.4 | 2.60 | 12.2 | 63.3 | 22.7 |
| 256 MiB | 2 GiB/s | 1 | 8.4 | 32.2 | 2.57 | 12.5 | 58.5 | 17.2 |
| 256 MiB | unpaced | 1 | 10.8 | 45.7 | 2.56 | 11.9 | 40.6 | 15.4 |
| 1 GiB | 128 MiB/s | 1 | 14.1 | 73.4 | 4.60 | 11.5 | 27.5 | 10.7 |
| 1 GiB | 256 MiB/s | 1 | 9.7 | 38.2 | 4.29 | 12.1 | 36.4 | 13.4 |
| 1 GiB | 512 MiB/s | 1 | 9.9 | 32.8 | 3.33 | 12.5 | 52.4 | 13.7 |
| 1 GiB | 1 GiB/s | 1 | 8.9 | 30.0 | 2.91 | 11.0 | 89.2 | 34.4 |
| 1 GiB | 2 GiB/s | 1 | 9.7 | 37.0 | 2.98 | 13.2 | 47.6 | 17.0 |
| 1 GiB | unpaced | 1 | 9.8 | 35.2 | 2.92 | 12.6 | 50.9 | 40.5 |

The queue's high-water mark was 46 MiB at 64 MiB and 190 MiB at 256 MiB. At
1 GiB it was up to 766 MiB at 128 and 256 MiB/s, and 262 to 448 MiB faster,
where the publication never waited for room.

The median fault of a chain took 7.6 to 8.1 ms with nothing publishing.
Beside a publication it took 7.3 to 8.0 ms on the publisher at up to
192 MiB/s, and 8.0 to 9.9 ms faster. On the holder it took 6.7 to 7.2 ms at
every rate. A publication beside the chains took as long as one alone: the
median difference was nothing, and the others ran from 6 s sooner to 8 s
later.

## What it shows

**The rate sets a publication's pace up to about 256 MiB/s.** An 8 GiB guest
is 10.7 GB of keeps. At 128 MiB/s it committed in 84 s, at 192 MiB/s in 54 s,
and at 256 MiB/s in 42 s. Faster than that it took 30 to 48 s whatever the
rate, as the holders' pace allows. A 2 GiB guest took 20.5 s, 14 s and 11 s.

**Above 192 MiB/s the publisher's faults pay.** With nothing publishing a
fault's p99 was 12 ms. Beside a publication paced at 128 MiB/s it was 27 ms on
the publisher. Unpaced it was 41 to 51 ms, 45 ms in the median. At 192 MiB/s
it was 31 ms, and at 256 MiB/s already 50 ms, as unpaced. The holder's faults
paid much less: 11 to 12 ms up to 192 MiB/s, and 13 to 16 ms at 256 MiB/s.
Single runs faster than that reached up to 57 ms.

**The queue does not set the pace.** Once the rate binds, a larger queue only
waits less. At 256 MiB/s an 8 GiB guest committed in 41.6 s with 64 MiB,
40.9 s with 256 MiB and 38.2 s with 1 GiB. At 128 MiB/s a 1 GiB queue
committed sooner but settled no sooner: 73 s to commit, 81 s to settle.

**The queue costs memory.** Publishing the 8 GiB guest, the publisher's peak
was 1.6 to 1.9 GiB at 64 MiB, 2.3 to 2.6 GiB at 256 MiB and 2.9 to 4.6 GiB at
1 GiB. A queue held full by a slow rate costs most: 4.6 GiB at 1 GiB and
128 MiB/s.

**Runs vary.** A single run's p99 of the faults beside a 2 GiB publication
rests on 200 to 600 faults, and moved from 31 to 120 ms between runs of one
case. The pooled p99 moved less: 35 to 68 ms over four runs at 256 MiB/s, and
30 to 37 ms over three at 192 MiB/s.

## Decisions

- **The rate of keeps is 192 MiB/s.** The bound is the faults' p99 beside a
  publication: 27 ms paced at 128 MiB/s, 45 ms unpaced. A default should keep
  the publisher's p99 nearer the paced end, below the midpoint of 36 ms.
  192 MiB/s did, at 31 ms, and 256 MiB/s did not, at 50 ms. At 192 MiB/s an
  8 GiB guest commits in 54 s rather than 84 s, and a 2 GiB guest in 14 s
  rather than 20.5 s.
- **The fill queue stays at 64 MiB.** A larger one saves a few seconds at
  most once the rate binds. At 256 MiB/s it cost the publisher 0.6 GiB more
  at 256 MiB and 2.5 GiB more at 1 GiB.

## What it does not show

- Rates between 192 and 256 MiB/s. The faults' p99 rose between them.
- Why the publisher's faults pay. The chains ran beside the publication's
  encoders and its keeps on four processors; no run profiled the publisher.
- A host pod's limits. The bench's process has the whole host, and no
  Firecracker.
- Cascade Lake, or a publication from more than one host at once. Each host
  has its own rate, so a holder takes keeps from every host that publishes.

## Cleanup

Each set of hosts was created once, ran its cases and was deleted with
`scripts/bench-restore-gce.sh delete`, which verified that no
`sproutfs-restore-filldef-` host, disk or object remained. `gcloud compute
instances list`, `gcloud compute disks list` and `gcloud storage ls` show none.
