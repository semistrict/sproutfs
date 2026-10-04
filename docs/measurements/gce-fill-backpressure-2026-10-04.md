# A publication's fills paced by their keeps on GCE, 2026-10-04

[The real application's restore](gce-real-app-restore-2026-10-03.md) found
that a suspend's fills dropped 2.5 to 7 % of its stripes. The windows they
held were then read from the store, and they made the restore's tail.
[The dependent reads](gce-dependent-reads-2026-10-03.md) found worse on Ice
Lake: the publisher overflowed a 4 GiB fill queue, dropped 5,436 stripes and
was killed for want of memory. Since
[the publication speedup](gce-publication-throughput-2026-10-04.md) a
publication encodes its parts side by side and goes faster still.

A fill used to be dropped whenever the queue was full. Now a publication's
fills wait for room in the queue, and so the publication goes at the pace its
keeps go ([filling the cluster](../hosting.md#filling-the-cluster)). This
report measures what that costs and what it saves.

## What ran

Six disposable `n2-standard-4` hosts in us-east4-a with
`--min-cpu-platform="Intel Ice Lake"` (4 vCPUs, 16 GB, SHA instructions), each
with one local NVMe SSD, under 4+2, against the bench's GCS bucket in the
same region. Each host ran `sproutfs-restorebench node`: the real checkpoint
store, page cache, peer server and table of peers, with its cache on the SSD,
the whole share in the cluster and the rate of keeps at 4 GiB/s, so the rate
bound nothing.

Host 0 published a guest of 8 GiB, 4,096 pages of 2 MiB of noise, through
the real store with eight upload slots. The drive ran with `-publishes 2`:
each case published the guest twice, each time as a VM of its own, and read
nothing back. Every window is new to the cluster each time: 4,096 pages and
16 page-table segments, 4,112 windows, 24,672 stripes. The bench reports, for
each publication, the commit's time, the time until the publisher's fills had
settled, what every host's fills did, and the most memory the publisher's
process held.

`scripts/bench-restore-gce.sh` ran each case on fresh node processes and
fresh cache files, on the same six hosts, with `SPROUTFS_RESTORE_PUBLISHES=2`
and a prebuilt binary (`SPROUTFS_RESTORE_BINARY`):

- **before**: 289cef6e, main at 381ac96b with the bench's publishing mode. A
  fill that finds the queue full is dropped.
- **after**: 341b4b06, this change. A publication's window waits for room
  below three quarters of the queue.

Each ran with the fill queue at 64 MiB, the default, at 1 GiB, and at 4 GiB,
the queue the dependent reads ran with. The raw results and logs are kept
outside the tree with the run.

## Results

Each row is one publication. A stripe dropped is one the cluster does not
hold, and its window is read from the store.

| Case | Queue | Commit s | Fills settled s | Windows filled | Stripes dropped | Dropped % | Peak memory GiB |
| --- | --- | --- | --- | --- | --- | --- | --- |
| before | 64 MiB | 35.3 | 35.5 | 1,407 | 16,230 | 65.8 | 2.19 |
| before | 64 MiB | 33.3 | 34.2 | 1,323 | 16,734 | 67.8 | 2.43 |
| after | 64 MiB | 69.4 | 69.5 | 4,112 | 0 | 0 | 1.75 |
| after | 64 MiB | 69.7 | 69.8 | 4,112 | 0 | 0 | 1.75 |
| before | 1 GiB | 30.3 | 37.1 | 1,670 | 14,652 | 59.4 | 8.00 |
| before | 1 GiB | 32.6 | 40.2 | 1,822 | 13,740 | 55.7 | 8.23 |
| after | 1 GiB | 68.4 | 74.0 | 4,112 | 0 | 0 | 5.76 |
| after | 1 GiB | 65.2 | 70.8 | 4,112 | 0 | 0 | 6.51 |
| before | 4 GiB | 31.9 | 61.5 | 3,324 | 4,728 | 19.2 | 11.80 |
| before | 4 GiB | 32.1 | 60.3 | 3,327 | 4,710 | 19.1 | 12.95 |
| after | 4 GiB | killed | | | | | |

Every stripe dropped was dropped for the queue. No keep was dropped by a
holder, the budget or the rate, and no publication waited out the bound.

After, at 64 MiB, each publication waited for room 4,073 and 4,067 times,
67 s in all, and the queue never held more than 46 MiB, its high-water mark.
The publisher's keeps carried 10.7 GB to its five peers in 69 s, 155 MB/s.
At 1 GiB the queue held at most 766 MiB, and each publication waited about
3,600 times.

## What it shows

**A publication no longer drops its windows.** At the default queue, before,
two thirds of the guest never reached the cluster: a restore elsewhere would
have read 2,700 of its 4,112 windows from the store. After, every stripe of
both publications landed on its holder. A 1 GiB queue did little for the old
fills, which still dropped more than half. A 4 GiB queue dropped a fifth.

**It costs the publication its speed.** The commit took 69 s instead of
33 s: 124 MB/s of guest instead of 258. That is the pace of the keeps. The
host does its fills one at a time, and each keep waits for its holder to
write it, so the publisher sent 155 MB/s of stripes, parity included. Before,
the fills that were kept took as long: at 4 GiB the old fills settled 61 s
after the commit began, and still lost a fifth of the guest. So a suspend
followed by a restore elsewhere now has the whole guest in the cluster about
as soon as it had four fifths of it before.

**A larger queue buys nothing now.** At 1 GiB the paced publication took
65 to 68 s, as at 64 MiB: the keeps set the pace, not the queue. The queue
only costs memory.

**The queue costs several times its bytes in memory.** The publisher held
1.75 GiB at 64 MiB, 6.1 GiB at 1 GiB, and with the old fills 12.9 GiB at
4 GiB. After at 4 GiB, the queue held its 3 GiB high-water mark for the whole
publication, and the kernel killed the publisher for want of memory, as it
killed the old one on 2026-10-03; systemd put its peak at 15.2 GB with the
page cache. A window in the queue holds its part's buffer,
which the part's other windows share, and a part waiting for room keeps its
slot, so the publication holds the parts under its slots and the queue's
windows. The rest is most likely the Go heap's headroom over what it holds,
and the garbage of splitting each window and building its keeps. Before, a
window kept when its part's other windows were dropped held the whole part
alone, which would be why the old fills held more for the same queue. The
run did not profile the heap.

## Decisions

- **The fill queue stays at 64 MiB.** With paced fills it drops nothing and
  is as fast as a larger one. The bench's default of 4 GiB was there only to
  keep the old fills from dropping, and it now kills the publisher of an
  8 GiB guest on a 16 GB host. `scripts/bench-restore-gce.sh` still defaults
  to it, so that runs of the restore bench stay comparable with the reports
  before this one; a run on a 16 GB host should set
  `SPROUTFS_RESTORE_FILL_QUEUE_BYTES` lower.
- **The bound stays at 10 s.** No wait came near it. A wait of a healthy
  publication lasted about 16 ms, one window's keeps.

## What it does not show

- The restore was not run. With the guest in the cluster, every window of it
  is read from the cluster rather than two thirds from the store, which
  [the dependent reads](gce-dependent-reads-2026-10-03.md) measured window by
  window.
- A publication beside reads. The tests in virtual time
  (`TestAReadsFillGoesAheadOfAPublications`) show a read's fill going ahead
  of a publication's; no run here read while it published.
- Cascade Lake, whose publisher is slower: there the keeps' pace costs a
  publication less.

## What the plan should change

1. **Send a fill's keeps to its holders side by side.** The keeps now set a
   publication's pace, and one worker sends one keep at a time, each waiting
   for its holder's write: 155 MB/s across five holders. The keeps of one
   window go to different holders, so they can go together if the order they
   reach each holder in stays the order the fills were handed over in.
2. **Count a window's part, not its envelopes, against the queue, and give
   the host's process a memory limit.** Memory is a few times the queue's
   bytes, so the queue alone does not say what a host can afford.
3. **Run the real application's suspend and restore again** with this change.

## Cleanup

Each run created its six hosts and deleted them at its end. The script
verified that no `sproutfs-restore-fillpace-` host, disk or object remained
after each, and `gcloud compute instances list`, `gcloud compute disks list`
and `gcloud storage ls` show none.
