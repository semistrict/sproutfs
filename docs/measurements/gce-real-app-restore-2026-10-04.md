# A real application restored from the cluster and from the store on GCE, 2026-10-04

This runs [the real application's restore](gce-real-app-restore-2026-10-03.md)
again, the same way, with everything merged since:

- a publication encodes its pages side by side
  ([publication throughput](gce-publication-throughput-2026-10-04.md));
- a publication's fills wait for room instead of dropping, and their keeps go
  to the holders side by side
  ([back-pressure](gce-fill-backpressure-2026-10-04.md),
  [side by side](gce-fill-side-by-side-2026-10-04.md));
- a fault reads its own page first and prefetches the rest of its run only
  when it follows a recent fault, and plans its page alone at random
  ([fault first](gce-fault-first-2026-10-04.md),
  [fault planning](gce-fault-planning-2026-10-04.md),
  [random fault planning](gce-random-fault-planning-2026-10-04.md));
- the page cache keeps decoded page tables, a page read from the cluster is
  copied twice instead of seven times, the hedge delay is kept per size of
  read, and every store request is bounded.

It found two things wrong. A rolling restart of the hosts left a host's disk
releasing for good, and the run could not go on until that was fixed. And a
Valkey guest restored from the store was 1.8 times as slow as on 2026-10-03,
because its faults at random read one page each. A third build, which
prefetches behind every fault in a pager of 2 MiB pages, ran the same GETs in
11.7 s from the cluster and 32.5 s from the store.

## What ran

As before: six disposable `n2-highmem-4` nodes in us-east4-a (4 vCPUs,
32 GB), each with nested virtualization and one local NVMe SSD, as one k3s
cluster running `deploy/` with one host pod per node under 4+2, as
`scripts/lib/app-restore-manifest.py` adapts it. The store was the bench's
GCS bucket in the same region. The guest had 8 GiB of RAM in 2 MiB pages and
2 vCPUs, with an 8 MiB read-ahead. Valkey 9.0.4 held 24,000,000 chained keys
of 100 bytes and a sorted set of 10,000,000 members: `used_memory` 4.02 GiB,
a resident set of 5.12 GiB, as before. The load took 87 to 95 s. Each case
loaded, waited 30 s, suspended with `stop --suspend`, waited for the fills
to settle, dropped every node's page cache, started the VM lazily on the
case's host, and walked 20,000 dependent GETs and 40,000 ZRANGEs of 100
members. Three rounds, in the orders of last time. Every byte of every
request checked.

**The hosts were Ice Lake.** The nodes were made with
`--min-cpu-platform="Intel Ice Lake"`, now the script's default
(`SPROUTFS_APP_PLATFORM`). Every node reported `Intel(R) Xeon(R) CPU @
2.60GHz` with `sha_ni` in its flags (`cpus.txt`). The run of 2026-10-03 was
on Cascade Lake, which has no SHA instructions, and SHA-256 was half of a
cluster fault's processor time there. So the comparison with that run mixes
the code's changes with the processor's. On these processors SHA-256 of a
2 MiB page takes 1.7 ms against 5.7 ms
([dependent reads](gce-dependent-reads-2026-10-03.md)).

`scripts/bench-app-restore-gce.sh` ran three builds on the same six nodes:

- **as merged**: main at 44bc9f0d with the fix to the membership below
  (a8dafc50). This is the run asked for.
- **every fault prefetches**: the same, with every fault prefetching its run,
  built apart for the experiment; cluster and store only.
- **after**: de7fc948, which prefetches behind a fault at random in a pager
  of 2 MiB pages. On this deployment both pagers are of 2 MiB pages, so every
  fault prefetches its run, as in the experiment. Its own run was cut short
  (see [cleanup](#cleanup)), so the experiment's numbers stand for it.

Raw results of the run as merged are in `gce-real-app-restore-2026-10-04/`.
The first attempt's snapshots of round 0 are also in its `r0-cluster` and
`r0-memory` and are counted in its `summary.md`; the tables here are from a
clean copy. The experiment's raw results were kept outside the tree.

## Results

### The suspend and its fills

As merged, each round. The experiment's suspends were the same within a
second.

| Case | suspend s | fills settle s | uploaded MB | uploaded MB/s | source CPUs | windows filled | stripes kept | stripes dropped | publication waits | waited s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cluster | 27.5–28.0 | 11.3 | 2,805–2,813 | 100.3–101.9 | 1.83–2.06 | 2,910–2,928 | 17,460–17,568 | none | 2,942–3,695 | 27.9–36.4 |
| memory | 27.5–28.0 | 11.3 | 2,797–2,818 | 100.7–101.8 | 1.84–2.08 | 2,913–2,919 | 17,478–17,514 | none | 2,907–3,635 | 27.3–34.9 |
| store | 25.1–25.5 | 6.1–6.2 | 2,819–2,833 | 110.6–112.6 | 2.12–2.28 | 0 | 0 | none | 0 | 0 |

On 2026-10-03 the suspend took 56.0 to 57.0 s at about 50 MB/s, the source
used 1.3 processors, and the fills dropped 438 to 1,182 stripes a round for
the queue. The fill queue never held more than 48 MiB. The waits for room
overlap the uploads: a suspend that waited 36 s in all took 28 s.

### The walk

The chase, 20,000 dependent GETs, and the scan, 40,000 ZRANGEs. Seconds are
the median of the three rounds, with the range; per-GET times in
milliseconds, the median over the rounds of each round's percentile.

| Case | build | chase s | p50 | p90 | p99 | max | GETs ≥ 1 ms | s in them | scan s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cluster | 2026-10-03, Cascade Lake | 22.35 (21.92–23.94) | 0.07 | 1.28 | 29.8 | 258 | 2,071 | 21.0 | 9.48 |
| cluster | as merged | 22.52 (21.87–22.75) | 0.08 | 1.31 | 19.3 | 90 | 2,179 | 21.1 | 9.63 |
| cluster | every fault prefetches | 11.70 (11.47–12.00) | 0.07 | 1.18 | 15.2 | 114 | 2,054 | 10.4 | 7.09 |
| store | 2026-10-03, Cascade Lake | 55.57 (53.71–58.61) | 0.07 | 1.24 | 97.0 | 816 | 2,061 | 54.2 | 16.09 |
| store | as merged | 99.81 (97.59–100.01) | 0.08 | 1.32 | 93.7 | 481 | 2,211 | 98.3 | 22.76 |
| store | every fault prefetches | 32.53 (29.56–32.97) | 0.06 | 1.18 | 52.8 | 558 | 2,062 | 31.2 | 10.77 |
| memory | 2026-10-03, Cascade Lake | 4.24 (4.16–4.27) | 0.07 | 1.25 | 1.5 | 14 | 2,052 | 3.0 | 5.81 |
| memory | as merged | 4.05 (4.04–4.06) | 0.06 | 1.20 | 1.5 | 10 | 2,036 | 2.9 | 5.35 |

The chase's GETs in each band of milliseconds, over the three rounds (60,000
a case):

| Case | build | 1–2 | 2–4 | 4–8 | 8–16 | 16–32 | 32–64 | 64–128 | 128–256 | 256+ |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cluster | as merged | 1,506 | 31 | 5 | 4,132 | 789 | 80 | 4 | 0 | 0 |
| cluster | every fault prefetches | 4,669 | 147 | 7 | 907 | 325 | 106 | 8 | 0 | 0 |
| store | as merged | 1,563 | 21 | 0 | 0 | 265 | 3,585 | 933 | 218 | 8 |
| store | every fault prefetches | 4,682 | 162 | 15 | 1 | 47 | 925 | 257 | 74 | 6 |
| memory | as merged | 5,798 | 278 | 33 | 7 | 0 | 0 | 0 | 0 | 0 |

The rest of each case's GETs took under 1 ms. 90 % of the chase's time had
passed by step 4,462 to 4,869 from the cluster as merged and 5,301 to 6,375
prefetching; from the store by step 3,622 to 3,749 as merged and 1,445 to
1,612 prefetching.

The start took 0.69 to 1.61 s on another host and 5.3 to 5.4 s on the same
host.

### Where the destination's pages came from

From the snapshot before the start to the one after the walk, each round. A
load is one read a fault waits for; a prefetch's read is not one.

| Case | build | RAM faults | faults a request | loads | mean load ms | pages loaded | pages prefetched | copies on write | from the cluster | cluster misses | cluster MB served | store GETs | store MB |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cluster | 2026-10-03 | 2,437–2,558 | 0.041–0.043 | 707–719 | 29.5–32.1 | 2,821–2,869 | | 2,425–2,541 | 2,673–2,820 | 76–177 | 2,640–2,750 | 84–126 | 64–149 |
| cluster | as merged | 3,144–3,255 | 0.052–0.054 | 2,517–2,556 | 8.0–8.3 | 2,785–2,840 | 250–323 | 3,114–3,216 | 2,817–2,871 | 0 | 2,763–2,803 | 28–30 | 0 |
| cluster | every fault prefetches | 2,492–2,601 | 0.042–0.043 | 704–729 | 12.0–12.5 | 2,807–2,908 | 2,103–2,179 | 2,460–2,567 | 2,841–2,940 | 0 | 2,759–2,846 | 21–23 | 0 |
| store | 2026-10-03 | 2,524–2,564 | 0.042–0.043 | 716–719 | 88.0–96.0 | 2,856–2,869 | | 2,503–2,545 | 0 | 0 | 0 | 1,233–1,266 | 2,814–2,828 |
| store | as merged | 3,180–3,268 | 0.053–0.054 | 2,498–2,584 | 42.5–46.2 | 2,801–2,829 | 242–303 | 3,147–3,239 | 0 | 0 | 0 | 2,811–2,853 | 2,771–2,815 |
| store | every fault prefetches | 2,479–2,576 | 0.041–0.043 | 714–718 | 45.6–50.1 | 2,849–2,864 | 2,135–2,146 | 2,457–2,550 | 0 | 0 | 0 | 1,830–1,877 | 2,776–2,819 |
| memory | as merged | 2,264–2,288 | 0.038 | 17–32 | 7.2–8.8 | 25–43 | 8–11 | 2,248–2,266 | 25–44 | 0 | 10–23 | 12–17 | 0 |

No window was missing from the cluster in any round, and no page came from
the store in the cluster cases but the index. No store request timed out or
was tried again. The cluster read asked a second request 5 times a round and
hedged to the store 6 times, and the store never won. In the memory case the
2,862 to 2,882 pages moved are the isolated arena's copies of the published
pages into the tenant's file, as on 2026-10-03.

### CPU

Over the start and the walk, in processors:

| Case | build | destination | busiest other host |
| --- | --- | --- | --- |
| cluster | 2026-10-03 | 2.2–2.3 | 0.19 |
| cluster | as merged | 1.45–1.59 | 0.09–0.18 |
| cluster | every fault prefetches | 2.53–2.63 | 0.14–0.25 |
| store | 2026-10-03 | 0.9 | |
| store | as merged | 0.47–0.58 | 0.02–0.08 |
| store | every fault prefetches | 1.14–1.18 | 0.10 |
| memory | as merged | 1.99–2.15 | 0.03–0.15 |

## What it shows

**The suspend is twice as fast and drops nothing.** It took 28 s instead of
57 s, at 100 MB/s instead of 50, and every stripe of every round reached its
holder. Encoding side by side and keeps side by side both show here. The
source used about 1.9 of its 4 processors. Without fills, to the store alone,
it took 25 s at 112 MB/s.

**As merged, the restore from the store got 1.8 times slower.** The chase
took 99.8 s against 55.6 s. Valkey's faults follow pointers to anywhere in
its heap, so almost none follows a recent fault, and since fault first such
a fault reads its page alone. The destination loaded 2,500 pages one at a
time, each a GET of about 45 ms, one after another. On 2026-10-03 it loaded
the same 2,860 pages as 716 runs of four, 92 ms each. Reading a page alone
halves a fault's wait and quarters what it brings in. From the cluster the
same trade came out even: 2,540 loads of 8 ms against 710 of 30 ms, 22.5 s
against 22.4 s. Ice Lake's SHA instructions are in that 8 ms, and the
destination used 1.5 processors instead of 2.2.

**Prefetching behind every fault halves the chase from the cluster and
thirds it from the store.** Each fault still waits only for its page, and the
other three pages of its run come in behind it. About 715 faults loaded,
as on 2026-10-03, and each waited 12 ms from the cluster and 48 ms from the
store. The chase took 11.7 s from the cluster and 32.5 s from the store: 1.9
and 1.7 times as fast as on 2026-10-03, and 1.9 and 3.1 times as fast as
merged. The scan took 7.1 s and 10.8 s. It costs processors: the destination
used 2.6 instead of 1.5 from the cluster.

The fault-first change chose to read a page alone at random from chains that
touch a few hundred of a guest's pages. Valkey touches 2,800 of its 4,096
over the walk, most of every run, at different times. A run of 2 MiB pages
is three pages to prefetch; a run of 4 KiB pages is 2,047, about 100 ms of
processor, which is why the chain of 4 KiB faults took 24 ms a hop
prefetching against 3.2 ms alone. So a pager of 2 MiB pages now prefetches
behind a fault at random too, and one of 4 KiB pages does not (below).

**The cluster is 2.8 times as fast as the store for this application.** With
every fault prefetching, 11.7 s against 32.5 s. A load from the cluster took
12 ms against 48 ms from the store.

**The copies on write are still the floor.** Valkey stores into what it
reads, so every page it touches is copied once: 2,250 to 3,240 copies a
round, in every case and build. On the same host that is the whole cost:
4.05 s, of which the 2,036 GETs of 1 ms or more hold 2.9 s. The scan took 5.35 s
against 5.81 s.

**The slowest GETs came down.** The chase's p99 from the cluster went from
29.8 to 15.2 ms and its slowest from 258 to 114 ms. No window was missing
from the cluster, because no fill dropped one: on 2026-10-03, 76 to 177
windows a round were read from the store and made the tail.

## Two things it found, and fixed

**A rolling restart left a host's disk releasing for good.** The first
attempt failed at round 0's change of share, which restarts every host pod:
after fifteen minutes one host's disk was still `releasing`, and the bench
waits for every disk to serve. The orchestrator had seen the old pod
terminating and drained its member. The new pod came back over the same disk,
under the same identity, so it was wanted and never gone, and listed and
never joined; nothing let its disk go. The rule let a host's own releasing
disk go only once its pod was gone. Now a host reports the generation of the
membership it holds, and the controller also lets the disk go once the
host's copy, at or after the generation that assigned the disk, has it
releasing: the host has read the release and serves the disk no more. The
member then leaves and joins again with its disk
([hosting](../hosting.md#the-membership)). On the cluster, the orchestrator
built with the fix brought three such disks back in 55 s, and every later
change of share passed. `TestNextBringsBackAHostThatReturnsOverItsDisk` and
`TestAPodReplacedOverItsDiskServesItAgain` hold it; the guards
`membership-let-only-a-gone-hosts-disk` and
`membership-let-on-an-earlier-release` fail the first.

**Faults at random read one page each in a pager of 2 MiB pages.** The
pager now has `Config.PrefetchAtRandom`, which the host, the simulated world
and the restore bench set from `vmmemory.PrefetchesAtRandom`: on for pages of
2 MiB or more ([reading at random](../vm-memory.md#reading-at-random)).
`TestAPagerThatPrefetchesAtRandomFaultsOnceARun` holds a guest touching every
page at random to one fault a run; the guard `pager-read-alone-at-random`
fails it. It costs the chain of 2 MiB faults of
[fault first](gce-fault-first-2026-10-04.md): 10.4 ms a hop from the cluster
instead of 7.1 ms.

## What the plan should change

1. **Read the rest of a restored guest behind its faults.** A guest that
   touches its whole heap pays a fault per run, about 715 here, one after
   another. A background read of the checkpoint at the bulk class, with
   faults going first, would take the heap in at the cluster's bulk pace,
   about 16 s for 8 GiB, while the guest runs.
2. **Measure 4 KiB RAM pages with this application.** At 4 KiB the heap is
   1.3 million pages and the walk touches few of each run, so a fault at
   random reading its page alone is likely right there; that is not measured.
3. **The copy on write is now the largest cost on the same host and a
   quarter of it from the cluster.** It is unchanged since 2026-10-03.
4. **The rest of the earlier plan stands**: more than one restore at once,
   and the holders under load.

## Also seen

- `TestEveryCaseReadsTheGuestBack` in `cmd/sproutfs-restorebench` fails
  about 3 to 6 runs in 100, at main 44bc9f0d as well: its store hedges and
  GETs vary with goroutine order inside the bubble. It does not touch the
  pager.
- A VM's delete logs `vmmemory: the memory session failed` at error level, a
  read of a closed userfaultfd, in 4 of the 9 cases.

## Cleanup

The six nodes were deleted at 14:04 UTC from the Cloud Console, while the run
of the build after the fix had begun its first round; that run has no
results. `scripts/bench-app-restore-gce.sh delete` then removed the run's
objects and verified that no node, disk or object remained, and
`gcloud compute instances list`, `gcloud compute disks list` and
`gcloud storage ls` show no `sproutfs-apprestore-` node, disk or object. The
script now fails a run whose node is deleted under it, where it used to poll
for ever.
