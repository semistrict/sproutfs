# The cluster's disk cache on network disks, GCE, 2026-10-04

The plan keeps the cache's disks on **shards**: network disks that outlive the
hosts that serve them ([Shards on network
disks](../../plans/disk-cache-2026-10-02.md#shards-on-network-disks)). The
earlier runs kept the cache on each host's local NVMe SSD
([dependent reads](gce-dependent-reads-2026-10-03.md)). This run measures what
a network disk costs a read. It repeats the chains of that report with every
host's cache on Hyperdisk Balanced, pd-balanced, pd-ssd and local NVMe in
turn, and measures each disk raw with fio against a host's serving budget of
500 MiB/s. A last run times a shard moving from a host that is removed to
another host.

## What ran

`scripts/bench-shards-gce.sh` ran `cmd/sproutfs-restorebench` built from
080c3d7c (sha256 `dd385b37…3d605a`). Every node opened its cache's disk with
`-device <disk>`: the raw block device `/dev/disk/by-id/google-<disk>`,
opened exclusively, with buffered I/O, as a shard keeps its disk. The local
SSD was opened the same way, as `-device local-nvme-ssd-0`; no file system
was on it. Raw results are in `gce-shards-2026-10-04/`.

Each run used six hosts in us-east4-a under 4+2 and the bucket in us-east4,
as before. The hosts kept one set of disks of one type at a time. For each
type, every node restarted on the new disk with a new prefix in the bucket,
host 0 published both guests again, and host 1 read the chains back three
rounds. Before each case every host emptied its memory tiers and dropped the
kernel's page cache, which also drops the block device's cache. Then every
node stopped and fio ran on the disk on all six hosts at once. The disks of
that type were deleted before the next type's were made.

Two series of host:

- **C3**: six `c3-standard-4-lssd` (Sapphire Rapids, 4 vCPUs, 16 GB, up to
  23 Gbps, one 375 GB local SSD). All four disk types, on the same six
  hosts. The 2 MiB guest was 4 GiB (2,048 pages), not 8 GiB, so that a fill
  queue of 6 GiB held its whole publication in 16 GB of memory; the nodes ran
  with `GOMEMLIMIT=12GiB`. The 4 KiB guest was 4 GiB, 1,048,576 pages, as
  before.
- **n2, Ice Lake**: six `n2-highmem-4` with `--min-cpu-platform="Intel Ice
  Lake"` (4 vCPUs, 32 GB, 10 Gbps, one local SSD), as the earlier Ice Lake
  run. Guests of 8 GiB and 4 GiB, a fill queue of 12 GiB. Two sets of six
  hosts. The first read local NVMe and the store. Its other passes made no
  reads: Hyperdisk Balanced would not attach (below), and the persistent
  disks failed on a fault in the script, since fixed. The second set read
  local NVMe again, as a control, then pd-balanced and pd-ssd.

Hyperdisk Balanced does not attach to an `n2-highmem-4`. The API refused the
attachment: "hyperdisk-balanced disk type cannot be used by n2-highmem-4
machine type". So Hyperdisk Balanced ran only on C3, and C3 ran every type
so that the comparison is on one series.

The disks were what the project's quota allowed. us-east4 holds 500 GB of
Hyperdisk Balanced and 500 GB of SSD-backed persistent disk (pd-balanced and
pd-ssd together), of which 100 GB was in use elsewhere. Six shards of 60 GB
fit beside six boot disks of 20 GB (Hyperdisk Balanced on C3, which cannot
boot from a standard disk; pd-standard on n2). Each disk's limits, and the
host's:

| Disk, 60 GB | Disk's read IOPS | Disk's MiB/s | Host's limit, 4 vCPUs | What binds |
| --- | --- | --- | --- | --- |
| Hyperdisk Balanced, provisioned 25,000 IOPS and 500 MiB/s | 25,000 | 500 | C3: 25,000 IOPS, 400 MiB/s | the host's throughput |
| pd-balanced | 3,000 + 6 a GB = 3,360 | 140 + 0.28 a GB = 157 | 15,000 IOPS, 240 MiB/s | the disk |
| pd-ssd | 6,000 + 30 a GB = 7,800 | 240 + 0.48 a GB = 269 | 15,000 IOPS, 240 MiB/s | the disk's IOPS, the host's throughput |
| local NVMe, 375 GB | | | | the device: 703 MiB/s read, 391 MiB/s written |

Hyperdisk Balanced was provisioned with 500 MiB/s so that the disk alone
could carry the budget, and 25,000 IOPS, the most a 4-vCPU C3 host takes; a
60 GB disk may have up to 30,000. A host of 4 vCPUs cannot reach 500 MiB/s
on any network disk. Google's limits for 8 vCPUs are 800 MiB/s and 50,000
IOPS for Hyperdisk Balanced on C3, and for pd-balanced and pd-ssd 800 MiB/s
on N2 but still 240 MiB/s on C3. pd-balanced reaches the
host's 240 MiB/s at 358 GB, pd-ssd at any size.

Every page of every case read back right, no read failed, and no publication
dropped a fill.

## Results

### A chain of reads on C3

Each hop's read, in milliseconds, the median over three rounds of each
round's percentile, and hops a second. "store" reads the bucket with no
cache, as the baseline:

| Guest | Read | Disk | p50 | p90 | p99 | p99.9 | max | hops/s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | page | local NVMe | 5.47 | 6.01 | 9.45 | 12.9 | 13.0 | 173 |
| 2 MiB | page | Hyperdisk Balanced | 8.64 | 10.3 | 13.1 | 14.5 | 14.7 | 124 |
| 2 MiB | page | pd-balanced | 7.25 | 8.37 | 11.4 | 12.5 | 13.3 | 144 |
| 2 MiB | page | pd-ssd | 7.26 | 8.45 | 11.4 | 12.9 | 14.0 | 144 |
| 2 MiB | page | store | 39.7 | 52.7 | 93.1 | 349 | 497 | 23.6 |
| 2 MiB | run | local NVMe | 11.1 | 13.2 | 20.3 | 31.6 | 31.6 | 82.8 |
| 2 MiB | run | Hyperdisk Balanced | 15.0 | 17.0 | 25.1 | 43.9 | 43.9 | 62.6 |
| 2 MiB | run | pd-balanced | 13.2 | 15.0 | 22.1 | 59.9 | 59.9 | 70.5 |
| 2 MiB | run | pd-ssd | 13.3 | 16.3 | 21.0 | 28.2 | 28.2 | 69.7 |
| 2 MiB | run | store | 60.2 | 75.1 | 121 | 272 | 272 | 15.7 |
| 4 KiB | page | local NVMe | 0.56 | 0.62 | 7.20 | 8.60 | 8.89 | 1,287 |
| 4 KiB | page | Hyperdisk Balanced | 1.39 | 1.71 | 9.17 | 13.6 | 14.6 | 596 |
| 4 KiB | page | pd-balanced | 1.02 | 1.22 | 8.24 | 9.93 | 15.2 | 786 |
| 4 KiB | page | pd-ssd | 1.00 | 1.18 | 8.01 | 9.77 | 9.89 | 808 |
| 4 KiB | page | store | 28.6 | 48.9 | 116 | 337 | 577 | 30.1 |
| 4 KiB | run | local NVMe | 29.9 | 42.7 | 56.1 | 68.0 | 68.0 | 29.9 |
| 4 KiB | run | Hyperdisk Balanced | 34.4 | 47.6 | 62.7 | 71.6 | 71.6 | 26.1 |
| 4 KiB | run | pd-balanced | 31.9 | 43.9 | 62.1 | 68.0 | 68.0 | 28.2 |
| 4 KiB | run | pd-ssd | 32.5 | 45.3 | 60.4 | 68.8 | 68.8 | 27.7 |
| 4 KiB | run | store | 75.9 | 114 | 250 | 304 | 304 | 11.1 |

Hops in each band of milliseconds, over the three rounds:

| Guest | Read | Disk | <0.5 | 0.5–1 | 1–2 | 2–4 | 4–8 | 8–16 | 16–32 | 32–64 | 64–128 | 128+ |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | page | local NVMe | 0 | 0 | 0 | 0 | 5,750 | 246 | 4 | 0 | 0 | 0 |
| 2 MiB | page | Hyperdisk Balanced | 0 | 0 | 0 | 0 | 2,817 | 3,183 | 0 | 0 | 0 | 0 |
| 2 MiB | page | pd-balanced | 0 | 0 | 0 | 0 | 4,932 | 1,068 | 0 | 0 | 0 | 0 |
| 2 MiB | page | pd-ssd | 0 | 0 | 0 | 2 | 4,985 | 1,013 | 0 | 0 | 0 | 0 |
| 2 MiB | page | store | 0 | 0 | 0 | 0 | 0 | 0 | 465 | 4,852 | 596 | 87 |
| 2 MiB | run | local NVMe | 0 | 0 | 0 | 0 | 0 | 1,104 | 95 | 1 | 0 | 0 |
| 2 MiB | run | Hyperdisk Balanced | 0 | 0 | 0 | 0 | 0 | 952 | 245 | 3 | 0 | 0 |
| 2 MiB | run | pd-balanced | 0 | 0 | 0 | 0 | 0 | 1,067 | 127 | 5 | 1 | 0 |
| 2 MiB | run | pd-ssd | 0 | 0 | 0 | 0 | 0 | 1,056 | 141 | 3 | 0 | 0 |
| 2 MiB | run | store | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 800 | 382 | 18 |
| 4 KiB | page | local NVMe | 283 | 5,514 | 10 | 1 | 173 | 19 | 0 | 0 | 0 | 0 |
| 4 KiB | page | Hyperdisk Balanced | 13 | 0 | 5,677 | 112 | 28 | 169 | 1 | 0 | 0 | 0 |
| 4 KiB | page | pd-balanced | 13 | 2,684 | 3,010 | 99 | 98 | 95 | 1 | 0 | 0 | 0 |
| 4 KiB | page | pd-ssd | 13 | 2,962 | 2,808 | 25 | 112 | 78 | 2 | 0 | 0 | 0 |
| 4 KiB | page | store | 0 | 0 | 0 | 0 | 0 | 95 | 3,637 | 1,888 | 335 | 45 |
| 4 KiB | run | local NVMe | 0 | 0 | 0 | 0 | 0 | 0 | 773 | 423 | 4 | 0 |
| 4 KiB | run | Hyperdisk Balanced | 0 | 0 | 0 | 0 | 0 | 0 | 108 | 1,083 | 9 | 0 |
| 4 KiB | run | pd-balanced | 0 | 0 | 0 | 0 | 0 | 0 | 633 | 560 | 7 | 0 |
| 4 KiB | run | pd-ssd | 0 | 0 | 0 | 0 | 0 | 0 | 543 | 650 | 7 | 0 |
| 4 KiB | run | store | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 88 | 978 | 134 |

The reader's CPU did not depend on the disk. Profiled once more after the
rounds, a 2 MiB hop took 5.9 ms of the reader's CPU on local NVMe, 6.0 ms on
pd-balanced and 6.2 ms on Hyperdisk Balanced, while the hops took 5.6, 7.0
and 8.0 ms. What the network disks add is waiting for the holders' disks.

In every case the holders served within 15 % of one another. No server
answered BUSY, no stripe was wrong and no request timed out. A holder serving
the 2 MiB chain served 51 to 75 MB/s and used about 0.1 CPU.

### A chain of reads on n2, Ice Lake

The same tables for the n2 hosts. The first local NVMe row and the store are
from the first set of hosts, the rest from the second:

| Guest | Read | Disk | p50 | p90 | p99 | p99.9 | max | hops/s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | page | local NVMe, first hosts | 6.07 | 6.76 | 9.53 | 12.2 | 13.0 | 156 |
| 2 MiB | page | local NVMe, second hosts | 6.56 | 7.13 | 9.90 | 12.0 | 12.1 | 146 |
| 2 MiB | page | pd-balanced | 8.89 | 9.99 | 12.8 | 14.6 | 14.7 | 113 |
| 2 MiB | page | pd-ssd | 8.84 | 9.73 | 12.5 | 14.0 | 14.5 | 114 |
| 2 MiB | page | store | 47.0 | 77.4 | 136 | 216 | 227 | 19.0 |
| 2 MiB | run | local NVMe, first hosts | 12.0 | 14.4 | 20.7 | 25.2 | 25.2 | 76.3 |
| 2 MiB | run | local NVMe, second hosts | 13.1 | 16.1 | 23.3 | 31.0 | 31.0 | 68.6 |
| 2 MiB | run | pd-balanced | 14.9 | 17.9 | 22.9 | 23.8 | 23.8 | 62.4 |
| 2 MiB | run | pd-ssd | 14.7 | 17.4 | 22.6 | 24.9 | 24.9 | 63.1 |
| 2 MiB | run | store | 58.6 | 88.3 | 173 | 251 | 251 | 15.1 |
| 4 KiB | page | local NVMe, first hosts | 0.65 | 0.71 | 8.12 | 13.6 | 18.4 | 1,094 |
| 4 KiB | page | local NVMe, second hosts | 0.70 | 0.79 | 8.48 | 10.4 | 10.9 | 1,051 |
| 4 KiB | page | pd-balanced | 1.20 | 1.39 | 9.79 | 15.4 | 15.9 | 665 |
| 4 KiB | page | pd-ssd | 1.15 | 1.33 | 9.73 | 18.5 | 20.1 | 688 |
| 4 KiB | page | store | 28.9 | 45.0 | 83.5 | 137 | 376 | 31.2 |
| 4 KiB | run | local NVMe, first hosts | 31.4 | 45.1 | 62.4 | 71.8 | 71.8 | 28.2 |
| 4 KiB | run | local NVMe, second hosts | 33.2 | 46.2 | 69.1 | 76.3 | 76.3 | 26.8 |
| 4 KiB | run | pd-balanced | 35.5 | 48.3 | 65.4 | 83.0 | 83.0 | 25.2 |
| 4 KiB | run | pd-ssd | 35.5 | 48.3 | 70.1 | 78.9 | 78.9 | 25.1 |
| 4 KiB | run | store | 69.6 | 106 | 188 | 511 | 511 | 12.4 |

| Guest | Read | Disk | <0.5 | 0.5–1 | 1–2 | 2–4 | 4–8 | 8–16 | 16–32 | 32–64 | 64–128 | 128+ |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | page | local NVMe, first hosts | 0 | 0 | 0 | 0 | 5,687 | 313 | 0 | 0 | 0 | 0 |
| 2 MiB | page | local NVMe, second hosts | 0 | 0 | 0 | 0 | 5,620 | 379 | 1 | 0 | 0 | 0 |
| 2 MiB | page | pd-balanced | 0 | 0 | 0 | 0 | 1,361 | 4,639 | 0 | 0 | 0 | 0 |
| 2 MiB | page | pd-ssd | 0 | 0 | 0 | 0 | 1,378 | 4,622 | 0 | 0 | 0 | 0 |
| 2 MiB | page | store | 0 | 0 | 0 | 0 | 0 | 0 | 398 | 4,750 | 794 | 58 |
| 2 MiB | run | local NVMe, first hosts | 0 | 0 | 0 | 0 | 0 | 1,091 | 109 | 0 | 0 | 0 |
| 2 MiB | run | local NVMe, second hosts | 0 | 0 | 0 | 0 | 0 | 1,072 | 127 | 1 | 0 | 0 |
| 2 MiB | run | pd-balanced | 0 | 0 | 0 | 0 | 0 | 930 | 267 | 3 | 0 | 0 |
| 2 MiB | run | pd-ssd | 0 | 0 | 0 | 0 | 0 | 948 | 250 | 2 | 0 | 0 |
| 2 MiB | run | store | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 770 | 378 | 52 |
| 4 KiB | page | local NVMe, first hosts | 17 | 5,786 | 3 | 2 | 115 | 73 | 4 | 0 | 0 | 0 |
| 4 KiB | page | local NVMe, second hosts | 14 | 5,786 | 5 | 3 | 12 | 180 | 0 | 0 | 0 | 0 |
| 4 KiB | page | pd-balanced | 14 | 19 | 5,713 | 60 | 2 | 190 | 2 | 0 | 0 | 0 |
| 4 KiB | page | pd-ssd | 14 | 90 | 5,687 | 15 | 2 | 188 | 4 | 0 | 0 | 0 |
| 4 KiB | page | store | 0 | 0 | 0 | 0 | 0 | 96 | 3,785 | 1,851 | 248 | 20 |
| 4 KiB | run | local NVMe, first hosts | 0 | 0 | 0 | 0 | 0 | 0 | 675 | 515 | 10 | 0 |
| 4 KiB | run | local NVMe, second hosts | 0 | 0 | 0 | 0 | 0 | 0 | 333 | 855 | 11 | 1 |
| 4 KiB | run | pd-balanced | 0 | 0 | 0 | 0 | 0 | 0 | 24 | 1,165 | 10 | 1 |
| 4 KiB | run | pd-ssd | 0 | 0 | 0 | 0 | 0 | 0 | 24 | 1,158 | 18 | 0 |
| 4 KiB | run | store | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 287 | 822 | 91 |

The two sets of hosts read local NVMe 6.07 and 6.56 ms a 2 MiB hop: about
8 % apart, which is what a fresh set of hosts moves a figure here. On n2 a
persistent disk added 2.3 ms to a 2 MiB hop and 0.5 ms to a 4 KiB hop, over
the second set's local NVMe; on C3 it added 1.8 and 0.45 ms.

### Each disk raw

fio on the raw device, O_DIRECT, libaio, over the first 8 GiB, which the
first job wrote in order. Each job ran 20 s after 2 s of warm-up. The median
over the six hosts; no host was more than 13 % below it. The last column is
the median's share of the 500 MiB/s a node may serve
(`-serve-bytes-per-second`).

On C3:

| Disk | Job | MiB/s | IOPS | p50 µs | p99 µs | of 500 MiB/s |
| --- | --- | --- | --- | --- | --- | --- |
| local NVMe | read, in order, 1 MiB, 8 deep | 703 | 703 | 11,141 | 12,452 | 141 % |
| local NVMe | read, random, 512 KiB, 1 deep | 703 | 1,406 | 741 | 1,065 | 141 % |
| local NVMe | read, random, 512 KiB, 16 deep | 703 | 1,406 | 11,207 | 12,452 | 141 % |
| local NVMe | read, random, 4 KiB, 1 deep | 30.6 | 7,825 | 125 | 163 | 6 % |
| local NVMe | read, random, 4 KiB, 64 deep | 703 | 179,946 | 209 | 791 | 141 % |
| local NVMe | write, in order, 1 MiB, 8 deep | 391 | 391 | 20,054 | 21,627 | 78 % |
| local NVMe | write, random, 4 KiB, 16 deep | 390 | 99,946 | 98 | 553 | 78 % |
| Hyperdisk Balanced | read, in order, 1 MiB, 8 deep | 400 | 400 | 20,054 | 21,365 | 80 % |
| Hyperdisk Balanced | read, random, 512 KiB, 1 deep | 400 | 800 | 1,106 | 2,490 | 80 % |
| Hyperdisk Balanced | read, random, 512 KiB, 16 deep | 400 | 799 | 20,054 | 21,365 | 80 % |
| Hyperdisk Balanced | read, random, 4 KiB, 1 deep | 7.8 | 1,989 | 467 | 1,042 | 2 % |
| Hyperdisk Balanced | read, random, 4 KiB, 64 deep | 97.6 | 24,994 | 2,540 | 3,310 | 20 % |
| Hyperdisk Balanced | write, in order, 1 MiB, 8 deep | 401 | 401 | 19,792 | 22,413 | 80 % |
| Hyperdisk Balanced | write, random, 4 KiB, 16 deep | 97.6 | 24,996 | 614 | 1,245 | 20 % |
| pd-balanced | read, in order, 1 MiB, 8 deep | 157 | 156 | 51,118 | 52,167 | 31 % |
| pd-balanced | read, random, 512 KiB, 1 deep | 157 | 314 | 3,162 | 3,850 | 31 % |
| pd-balanced | read, random, 512 KiB, 16 deep | 157 | 313 | 51,118 | 51,642 | 31 % |
| pd-balanced | read, random, 4 KiB, 1 deep | 11.9 | 3,051 | 303 | 631 | 2 % |
| pd-balanced | read, random, 4 KiB, 64 deep | 13.1 | 3,357 | 19,005 | 19,792 | 3 % |
| pd-balanced | write, in order, 1 MiB, 8 deep | 157 | 157 | 51,118 | 51,642 | 31 % |
| pd-balanced | write, random, 4 KiB, 16 deep | 13.1 | 3,359 | 4,751 | 5,734 | 3 % |
| pd-ssd | read, in order, 1 MiB, 8 deep | 240 | 240 | 33,423 | 34,341 | 48 % |
| pd-ssd | read, random, 512 KiB, 1 deep | 240 | 480 | 2,073 | 3,162 | 48 % |
| pd-ssd | read, random, 512 KiB, 16 deep | 240 | 479 | 33,423 | 34,341 | 48 % |
| pd-ssd | read, random, 4 KiB, 1 deep | 12.0 | 3,067 | 299 | 631 | 2 % |
| pd-ssd | read, random, 4 KiB, 64 deep | 30.5 | 7,797 | 8,225 | 8,585 | 6 % |
| pd-ssd | write, in order, 1 MiB, 8 deep | 241 | 241 | 33,423 | 37,749 | 48 % |
| pd-ssd | write, random, 4 KiB, 16 deep | 30.5 | 7,799 | 2,040 | 2,425 | 6 % |

The 4-deep reads of 512 KiB and the 16-deep reads of 4 KiB are in the raw
results; they reach the same limits. Every network disk ran at its limit from
one request in flight on: a read of 512 KiB at a time already took the whole
400, 240 or 157 MiB/s, and the latency of a deeper queue is only the wait in
the queue. Each got exactly the IOPS and MiB/s its limits give:
Hyperdisk Balanced 25,000 IOPS and the host's 400 MiB/s, not the 500
provisioned; pd-balanced 3,360 IOPS and 157 MiB/s; pd-ssd 7,800 IOPS and the
host's 240 MiB/s.

On n2 every figure came within 10 % of C3's, and most were the same: local
NVMe 703 MiB/s read and 391 MiB/s written, 8,372 reads of 4 KiB a second at
one in flight (111 µs); pd-balanced 157 MiB/s and 3,359 IOPS, 301 µs for a
4 KiB read; pd-ssd 240 MiB/s and 7,799 IOPS, 301 µs. The n2 limits for 4
vCPUs, 15,000 IOPS and 240 MiB/s, are the same as C3's for persistent disks.

### Publishing onto each disk

How long host 0 took to publish each guest, and until every fill was on its
holder's disk (C3):

| Disk | 2 MiB guest (4 GiB): commit, settled s | 4 KiB guest (4 GiB): commit, settled s |
| --- | --- | --- |
| local NVMe | 13.5, 31.8 | 16.6, 135.5 |
| Hyperdisk Balanced | 13.7, 31.4 | 16.9, 343.3 |
| pd-balanced | 14.2, 43.6 | 16.8, 185.4 |
| pd-ssd | 16.7, 33.8 | 17.0, 170.6 |

On n2, with the 8 GiB guest, the 2 MiB guest settled in 73.8 s on local NVMe,
95.8 s on pd-balanced and 80.6 s on pd-ssd, and the 4 KiB guest in 143.0,
174.7 and 174.3 s.

## What it shows

**A network disk adds a little to a hop from the cluster, and the cluster
stays far ahead of the store.** On C3 a chain of 2 MiB pages took 5.5 ms a
hop on local NVMe, 7.3 ms on pd-balanced or pd-ssd and 8.6 ms on Hyperdisk
Balanced, against 39.7 ms from the store. A chain of 4 KiB pages took
0.56 ms on local NVMe, 1.0 ms on a persistent disk and 1.4 ms on Hyperdisk
Balanced, against 28.6 ms from the store. So a shard is 4.6 to 5.5 times as
fast as the store at 2 MiB, and 21 to 29 times at 4 KiB. On n2 a persistent
disk took 8.9 ms a 2 MiB hop and 1.2 ms a 4 KiB hop, against 6.6 and 0.70 ms
on local NVMe and 47 and 29 ms from the store. The runs a fault
reads today moved less: 11 to 15 ms at 2 MiB and 30 to 34 ms at 4 KiB, since
those are mostly the reader's CPU.

**The extra is the disk's own latency, paid once a hop.** A 4 KiB read at
one in flight took 125 µs on the local SSD, 300 µs on a persistent disk and
467 µs on Hyperdisk Balanced. A 4 KiB hop grew by about that: 0.45 ms on a
persistent disk and 0.83 ms on Hyperdisk Balanced. A 2 MiB hop waits for the
slowest of four 512 KiB reads on four holders, and grew by 1.8 and 3.2 ms.

**Hyperdisk Balanced was the slowest for single reads, not the fastest.**
Its 4 KiB read at one in flight took 467 µs against 300 µs for pd-balanced,
and its 2 MiB chain was 19 % slower. It has more throughput and IOPS, but a
chain never uses them. A 512 KiB read on it took 1.1 ms in fio, yet the
2 MiB hop grew by 3.2 ms. fio read with O_DIRECT; the node reads through the
page cache, which reads ahead 128 KiB, and these disks take requests of at
most 256 KiB (`max_sectors_kb`), against 2 MiB on the local SSD. Where the
rest goes is not measured here.

**No network disk on a 4-vCPU host can serve 500 MiB/s.** The host's limit
binds first: 400 MiB/s for Hyperdisk Balanced on C3, 240 MiB/s for pd-ssd,
and 157 MiB/s for pd-balanced of 60 GB. The local SSD read 703 MiB/s. Reads of
4 KiB are worse: Hyperdisk Balanced at its 25,000 IOPS is 98 MiB/s, a pd-ssd
of 60 GB 30 MiB/s, a pd-balanced 13 MiB/s. Writes are capped the same way.
A shard also filled more slowly: on C3 the 4 KiB guest settled in 171 to
185 s on a persistent disk and 343 s on Hyperdisk Balanced, against 136 s on
local NVMe. Hyperdisk Balanced was the slowest to fill although it has the
most IOPS; this run does not show why.

**Hyperdisk Balanced does not attach to an n2-highmem-4.** The API refused
it. Ice Lake hosts would keep shards on pd-balanced or pd-ssd. Shards on
Hyperdisk Balanced need C3, or another series that takes it; C3 also has SHA
instructions.

## What the plan should change

- **Size a shard's host for its disk's throughput, not its CPUs.** At 4 vCPUs
  no network disk gives 500 MiB/s. The serving budget should be the least of
  the node's setting and what its disk and host allow, or a shard will answer
  slower than the hedge expects under load. To serve 500 MiB/s from one shard
  a host needs 8 vCPUs, by Google's limits: Hyperdisk Balanced on a C3 of 8
  vCPUs (800 MiB/s), or a persistent disk on an N2 of 8 vCPUs (800 MiB/s).
  C3 caps persistent disks at 240 MiB/s up to 8 vCPUs. Neither was measured
  here.
- **Prefer a persistent disk for latency, and provision for IOPS.** For
  dependent reads, pd-balanced and pd-ssd were faster than Hyperdisk
  Balanced. Their IOPS follow the size, though: a pd-balanced of 60 GB does
  3,360 reads a second and 157 MiB/s; it needs 358 GB to reach a host's
  240 MiB/s. A shard's size should be chosen for its IOPS and throughput as
  well as its bytes.
- **Try reading a shard's stripes with direct I/O.** On Hyperdisk Balanced a
  512 KiB read took 1.1 ms in fio with O_DIRECT, yet a 2 MiB hop grew by
  3.2 ms through the page cache, which also reads ahead. A shard's reads are
  random and of known size. Reading them with O_DIRECT, aligned, may take
  part of the extra off a 2 MiB hop. That needs a measurement of its own.
- **Expect the 4 KiB chain to double.** The plan quotes Google's "below a
  millisecond" for a network disk. That held for a 4 KiB read (0.3 to
  0.47 ms), but a hop is about twice the local SSD's: 1.0 to 1.4 ms against
  0.56 ms. It is still about 25 times faster than the store.

## Moving a shard

`scripts/bench-shard-move-gce.sh` ran `cmd/sproutfs-shardbench` built from
68562305 on two `c3-standard-4` hosts in us-east4-a, with one shard: a
Hyperdisk Balanced disk of 256 GB, provisioned 6,000 IOPS and 500 MiB/s. Each
host ran the real host as a member, with no VMM, as a systemd unit that starts
again when it ends, as a kubelet starts a pod again. Host 0 also ran the
controller: a pass of the membership and of Compute Engine's attach API every
second, with the hosts' own credentials. The orchestrator passes every five
seconds. Raw results are in `gce-shards-2026-10-04/move-hyperdisk-balanced/`.

The controller first filled the shard: a 32 GiB publication through the
member that served it, which took 283 s and left 16,512 entries in 529
regions. Then it took the serving member away six times, three times by
draining it, as the orchestrator does when a pod is terminating, and three
times by ending its process at once, as a machine that dies does. It timed
each step as its own passes saw it, until the shard served on the other host.

Seconds from the member's removal:

| How | Releasing | Closed and detached | Let go | Assigned and attached | Serving |
| --- | --- | --- | --- | --- | --- |
| drained | 0.25 | 5.97 | 7.27 | 13.12 | 14.36 |
| drained | 0.28 | 5.67 | 6.95 | 13.34 | 14.59 |
| drained | 0.28 | 4.71 | 6.00 | 11.96 | 13.16 |
| died | 3.63 | 3.63 | 4.86 | 12.29 | 13.54 |
| died | 3.61 | 3.61 | 4.82 | 11.94 | 13.23 |
| died | 3.49 | 3.49 | 4.73 | 11.70 | 12.98 |

A shard moved in 13 to 14.6 s, and in about the same time whether its member
drained or died. Every move read the shard back from its tables: all 529
regions, none scanned, in 0.30 s. No move lost an entry.

Each controller pass makes its calls to Compute Engine before it looks, so a
column with two steps holds the call between them. The detach call is in
"closed and detached", and the attach call in "assigned and attached":

- **Drained.** The release is one pass. The host sees it at its next read of
  the membership, within a second, and closes the shard. The next pass
  detaches the disk: 4.4 to 5.7 s for both. Letting the shard go waits for
  the pass after, 1.3 s later.
- **Died.** The member stops answering, so the next pass drains it, takes it
  as closed, and detaches the disk in the same pass: 3.5 to 3.6 s. The
  process came back within a second as a new member, which holds nothing.
- **Both.** Assigning the shard and attaching it took 5.9 to 7.4 s, almost
  all of it the attach call. The new host then read the membership again,
  opened the device, took the lease, read the regions back and reported the
  shard open, and the next pass marked it serving: 1.2 to 1.3 s.

So about 10 s of a move is Compute Engine's attach and detach calls, and
about 3 s is the controller's and the host's one-second passes. At the
orchestrator's five-second pass, those three waits would add about 12 s.
While a shard moves, each of its windows has one holder down under 4+2, which
the reads hedge around, as the simulation shows.

## Cleanup

Every host, disk and object of the three runs of `bench-shards-gce.sh` (one
on C3, two on n2) was deleted, and the script
checked each time that none remained. Afterwards `gcloud compute instances
list --filter="name ~ ^sproutfs-shards"` and `gcloud compute disks list
--filter="name ~ ^sproutfs-shards"` listed 0 items, and `gcloud storage ls
gs://echophase-sproutfs-bench/sproutfs-bench/sproutfs-shards-*` matched no
objects.

The move run's two hosts, its shard and its objects were deleted the same
way, and its script checked that none remained. Afterwards no instance or
disk named `sproutfs-move-*` was listed, and `gcloud storage ls
gs://echophase-sproutfs-bench/sproutfs-bench/sproutfs-move-*` matched no
objects.
