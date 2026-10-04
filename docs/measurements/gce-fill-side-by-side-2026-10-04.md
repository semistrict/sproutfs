# A publication's keeps side by side on GCE, 2026-10-04

[The paced fills](gce-fill-backpressure-2026-10-04.md) stopped a publication
from dropping its windows, and slowed it to the pace of its keeps: one worker
sent one keep at a time, 155 MB/s of stripes, and an 8 GiB guest took 69 s to
commit instead of 33 s. Now the worker decides each keep in fill order and
hands it to the lane of its holder, and the lanes run beside each other
([filling the cluster](../hosting.md#filling-the-cluster)). A window taken
into the queue also holds a copy of its envelopes rather than its part. This
report measures both.

## What ran

The same bench as before, on six disposable `n2-standard-4` hosts in
us-east4-a with `--min-cpu-platform="Intel Ice Lake"`, each with one local
NVMe SSD, under 4+2, against the bench's bucket in the same region. Host 0
published a guest of 8 GiB, 4,096 pages of 2 MiB of noise, twice per case,
each time as a VM of its own: 4,112 windows and 24,672 stripes, 10.7 GB of
keeps. The rate of keeps was 4 GiB/s, so the rate bound nothing.

`scripts/bench-restore-gce.sh run` ran every case on the same six hosts, on
fresh node processes and fresh cache files, with
`SPROUTFS_RESTORE_PUBLISHES=2`, `SPROUTFS_RESTORE_CASES=2MiB/chain/page/1` and
a prebuilt binary:

- **before**: main at 607f9ef1. One keep at a time.
- **after**: ccc773a1, this change.

Each ran with the fill queue at 64 MiB, the default, and at 1 GiB. The raw
results and logs are kept outside the tree with the run.

## Results

Each row is one publication. Memory is the publisher's peak RSS.

| Case | Queue | Commit s | Fills settled s | Stripes dropped | Waits | Waited s | Queue peak MiB | Peak memory GiB |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| before | 64 MiB | 70.3 | 70.4 | 0 | 4,061 | 68.5 | 46 | 1.84 |
| before | 64 MiB | 68.6 | 68.7 | 0 | 4,073 | 66.8 | 46 | 1.84 |
| after | 64 MiB | 38.3 | 38.3 | 0 | 2,814 | 19.8 | 46 | 1.80 |
| after | 64 MiB | 39.0 | 39.0 | 0 | 2,609 | 17.5 | 46 | 1.82 |
| before | 1 GiB | 65.5 | 70.3 | 0 | 3,459 | 57.8 | 766 | 5.60 |
| before | 1 GiB | 63.1 | 68.3 | 0 | 3,444 | 56.2 | 766 | 5.60 |
| after | 1 GiB | 27.9 | 28.6 | 0 | 0 | 0 | 374 | 3.06 |
| after | 1 GiB | 27.7 | 28.0 | 0 | 0 | 0 | 412 | 3.06 |

Every stripe of every publication landed on its holder. No keep was dropped
and no publication waited out the bound. The holders' memory stayed at
50 MB.

## What it shows

**At the default queue the commit takes 38 s instead of 69 s.** The
publisher's keeps carried 10.7 GB in 38 s, 280 MB/s, against 153 MB/s one at
a time: 224 MB/s of guest. The old fills that dropped two thirds of the guest
took 33 to 35 s.

**A 1 GiB queue now buys speed.** With room for the keeps to run ahead, the
publication never waited, and committed in 28 s: faster than the old fills
that dropped. One keep at a time, the larger queue bought nothing. At 64 MiB
the publication still waits for room 2,600 to 2,800 times, 18 to 20 s in all.
The worker decides the keeps in fill order, so a holder that answers late
holds back the keeps behind it to the other holders too. A queue of 48 MiB
below its mark holds 24 windows, which is not enough to ride out those
moments.

**The queue costs less memory, but more than its bytes.** At the default
queue the publisher's peak is the same, 1.8 GiB: the queue is small beside
the parts under the eight upload slots and the encoders. At 1 GiB, the
publisher held 3.06 GiB with at most 412 MiB queued, against 5.6 GiB with
766 MiB queued before. Before, each queued window held its whole 64 MiB part.
Above the 1.8 GiB a 64 MiB queue costs, memory is now about three times the
queued bytes rather than five. The rest is most likely the Go heap's headroom
and each window's stripes and keeps while they are on their way, which the
queue does not count. The run did not profile the heap.

## Decisions

- **The default queue stays at 64 MiB.** The commit is 31 s faster there. A
  larger queue would be faster still, for memory; that trade is the owner's.
- **A lane holds two of a publication's keeps, and the host 16.** At 512 KiB
  a keep under 4+2, the five remote lanes hold 5 MiB of the background budget
  at most, within the three quarters a publication's keeps may take.

## What it does not show

- A publication beside reads. The tests in virtual time show a read's keep
  waiting behind the keep on the wire at most.
- The default rate of keeps, 128 MiB/s. Under it, a publication's keeps go no
  faster than 134 MB/s, as one keep at a time went: the rate, not the worker,
  sets the pace.
- Where the remaining memory goes.

## What the plan should change

1. **Measure a queue between 64 MiB and 1 GiB**, for the commit's time
   against the publisher's memory, and choose the default from it.
2. **Profile the publisher's heap** at a large queue, and count a window's
   stripes and keeps against the queue if they are what the memory is.
3. **Weigh the default rate of keeps** against a publication's pace.

## Cleanup

The script created the six hosts once, ran the four cases on them, and
deleted them. It verified that no `sproutfs-restore-sidebyside-` host, disk
or object remained, and `gcloud compute instances list`, `gcloud compute
disks list` and `gcloud storage ls` show none.
