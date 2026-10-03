# A real application restored from the cluster and from the store on GCE, 2026-10-03

[The cluster reads](gce-cluster-reads-2026-10-03.md) read a guest's memory
in order, 16 pages at a time, and the cluster was 1.7 times as fast as the
store. This run asks the other question: a real application in a real guest,
restored on another host, whose first requests each wait for a page the one
before it named. Every fault pays its whole latency in sequence.

## The application

Valkey 9.0.4, Alpine's package (BSD-3-Clause). Valkey is the open fork of
Redis and is widely deployed. Its data is a heap of linked objects: a GET
hashes the key into the keyspace's table, follows the bucket to the object,
and from the object to its value. A sorted set is a skiplist, whose level-0
links are followed one node at a time. Alpine builds it with musl's malloc,
so the objects lie where that allocator put them. It was easy to add: the
image needs one package, and the repository's storage bench already runs it.

`cmd/sproutfs-guest-chase` loads and walks it over a unix socket, because the
guest has no network:

- **The chain.** 24,000,000 string keys of 100-byte values. Each value starts
  with the name of the next key in one cycle through every key, in an order a
  keyed permutation draws. The keys are written in numeric order, so the
  server allocates them in that order and each step of the chain lands
  anywhere in the heap. The walk does 20,000 GETs, one at a time. Each GET
  asks for the key the previous value named, and the walk checks every byte
  it gets back.
- **The scan.** A sorted set of 10,000,000 members, added in numeric order,
  each scored by its place in a second permutation. The walk reads the first
  4,000,000 members in rank order, 100 at a time with ZRANGE, and checks each.
  Every link it follows jumps through the set's part of the heap.

Loaded, the server held `used_memory` 4.02 GiB and a resident set of
5.12 GiB. The load took 89 to 92 s.

## What ran

Six disposable `n2-highmem-4` nodes in us-east4-a (4 vCPUs, 32 GB, Intel
Cascade Lake, no SHA instructions), each with nested virtualization and one
local NVMe SSD. They formed one k3s cluster and ran `deploy/` with one host
pod per node, as `scripts/lib/app-restore-manifest.py` adapts it: the host
pods on their nodes' network, so peers talk over the VPC and not the overlay;
a 10 GiB HugeTLB arena, nine tenths of it RAM's, so the guest fits whole; the
page cache's file and the scratch on the SSD; and the code 4+2. The store was
a GCS bucket in the same region. The guest had 8 GiB of RAM and 2 vCPUs, and
the deployment's default 2 MiB RAM pages with an 8 MiB read-ahead.
`scripts/bench-app-restore-gce.sh` ran it from 0fd3d9f0 with this branch's
changes. Raw results are in `gce-real-app-restore-2026-10-03/`.

Each case of each round created a VM from the valkey template, started Valkey,
loaded it, waited 30 s, and suspended it with `sproutfs stop --suspend`, which
publishes its memory. Once the publication's fills had settled, the run
dropped the kernel's page cache on every node and started the VM on the
case's host. The restore is lazy: the guest resumes at once, and each page it
touches faults in. The walk began as the start returned.

- **cluster**: `SPROUTFS_CACHE_CLUSTER_PERCENT=100`, restored on another
  host. The suspend's publication filled the cluster, and the faults read it.
- **store**: the share at 0, restored on another host. The faults read GCS.
- **memory**: the share at 100, restored on the host that suspended it.

Three rounds ran each case: rounds 0 and 2 in the order cluster, memory,
store; round 1 store, memory, cluster. A change of share restarts the host
pods. The run read every host's `/status` and `/metrics` before and after
each step.

## Results

### The walk

The chase, 20,000 dependent GETs. Times in milliseconds, the median over the
three rounds of each round's percentile:

| Case | seconds | p50 | p90 | p99 | max | requests ≥ 1 ms | seconds in them |
| --- | --- | --- | --- | --- | --- | --- | --- |
| cluster | 22.35 | 0.07 | 1.28 | 29.8 | 258 | 2,071 | 21.0 |
| store | 55.57 | 0.07 | 1.24 | 97.0 | 816 | 2,061 | 54.2 |
| memory | 4.24 | 0.07 | 1.25 | 1.5 | 14 | 2,052 | 3.0 |

| Case | round 0 | round 1 | round 2 |
| --- | --- | --- | --- |
| cluster | 22.35 | 23.94 | 21.92 |
| store | 53.71 | 58.61 | 55.57 |
| memory | 4.16 | 4.24 | 4.27 |

The scan, 40,000 ZRANGEs of 100 members:

| Case | seconds | p50 | p99 | max | requests ≥ 1 ms | seconds in them |
| --- | --- | --- | --- | --- | --- | --- |
| cluster | 9.48 | 0.14 | 0.21 | 1,922 | 23 | 3.7 |
| store | 16.09 | 0.14 | 0.20 | 5,108 | 17 | 10.3 |
| memory | 5.81 | 0.14 | 0.20 | 11 | 10 | 0.04 |

The chase's requests in each band of milliseconds, over the three rounds
(60,000 a case):

| Case | < 0.5 | 1–2 | 2–4 | 16–32 | 32–64 | 64–128 | 128–256 | 256–512 | 512+ |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cluster | 53,795 | 4,770 | 84 | 956 | 188 | 174 | 22 | 2 | 0 |
| store | 53,833 | 4,725 | 120 | 0 | 0 | 1,025 | 227 | 62 | 4 |
| memory | 53,849 | 5,892 | 214 | 2 | 0 | 0 | 0 | 0 | 0 |

The few requests in the bands left out are under 0.1 % of each case. Every
step checked: no byte was wrong in any case.

The start took 0.86 to 1.16 s on another host and 5.5 s on the same host.

### Where the destination's pages came from

From the snapshot before the start to the one after the walk, each round:

| Case | RAM faults | pages loaded | loads | mean load ms | stores into restored pages | copies on write | from the cluster | cluster misses | store GETs | store MB |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cluster | 2,437–2,558 | 2,821–2,869 | 707–719 | 29.5–32.1 | 1,727–1,835 | 2,425–2,541 | 2,673–2,820 | 76–177 | 84–126 | 64–149 |
| store | 2,524–2,564 | 2,856–2,869 | 716–719 | 88.0–96.0 | 1,803–1,843 | 2,503–2,545 | 0 | 0 | 1,233–1,266 | 2,814–2,828 |
| memory | 2,215–2,292 | 24–39 | 14–23 | 16.4–18.0 | 2,197–2,268 | 2,208–2,276 | 38–53 | 0–1 | 13–15 | 0 |

A load is one read of the pager's backing: an 8 MiB read-ahead run, four
2 MiB pages. The pager's histogram puts the cluster's loads at p50 ≤ 33 ms and
p99 ≤ 131 ms, and the store's at p50 ≤ 131 ms and p99 ≤ 262 ms.

The memory tier of the page cache served nothing (0 or 1 hit), and no page
came from the destination's own disk. In the cluster case the destination
asked its peers 11,582 to 11,675 stripe requests, about 16 a load, and they
served it 2.64 to 2.75 GB. It sent 5 second requests a round and the budget
refused 26 to 28; it read the store past the bound 6 times, and the store
never won. The reader's delay settled at 22 ms.

### The suspend and its fills

| Case | suspend s | fills settle s | uploaded MB | windows filled | stripes kept | stripes dropped |
| --- | --- | --- | --- | --- | --- | --- |
| cluster | 56.0–56.9 | 11.2–11.3 | 2,782–2,806 | 2,688–2,841 | 16,128–17,046 | 438–1,182, queue |
| memory | 56.5–56.9 | 11.2–11.3 | 2,758–2,815 | 2,805–2,823 | 16,830–16,938 | 378–630, queue |
| store | 56.6–57.0 | 6.1 | 2,801–2,815 | 0 | 0 | none |

The suspend published 2,904 dirty pages, 6.09 GB, compressed to about
2.8 GB, at about 50 MB/s. The source used 1.3 of its 4 processors while it
did.

### CPU

Over the start and the walk, the destination's pod, which holds the guest's
VMM, used 2.2 to 2.3 processors in the cluster case, 0.9 in the store case
and 2.0 in the memory case. The busiest holder used 0.19.

One more cluster case ran under `perf record -a -g` on every node
(`profile/perf-destination.txt`). Over 75 s the destination's four CPUs were
73 % idle and the host process used 21 % of them. Of that, SHA-256
(`sha256.blockAVX2`) took 9.1 % of the node and zstd 5.4 %, all of it under
`stripe.Join` decoding each rebuilt envelope (`blob.Codecs.Decode`). That is
about 9.7 ms of SHA-256 and 5.8 ms of zstd for each 2 MiB window. Reed-Solomon
took under 0.2 %.

## What it shows

**The cluster restores this application 2.5 times as fast as the store.** The
chase took 22 s from the cluster and 56 s from the store; the scan 9.5 s and
16.1 s. A load, the unit a fault waits for, took 30 ms from the cluster and
92 ms from the store: 3.1 times. The bulk read measured 1.7 times. So
dependent access does gain more from the cluster, but not far more: both
cases pay the same CPU for each window, and both pay the same copies.

**Every fault is serial, and about 2,060 of them make up the walk.** In every
case about 2,060 of the 20,000 GETs waited 1 ms or more, and those hold 70
to 97 % of the chase's time. The rest took 70 µs. The guest's 5.1 GiB of
resident heap is about 2,600 pages of 2 MiB, and the restore loaded 2,860
pages in all. Most of the waiting is early: 90 % of the chase's time had
passed by step 2,300 to 3,700 from the cluster and by step 1,130 to 1,170
from the store. The scan's slowest ZRANGE, which
touched about 100 new pages one after another, took 1.9 s from the cluster
and 5.1 s from the store.

**Each fault is either a load or a copy.** About 720 faults loaded a
read-ahead run. The other 1,800 found their page loaded and still waited
1–2 ms: the guest stored into it. Valkey writes into every object it reads,
for the eviction clock, so a read of the heap is a store into a 2 MiB page
that the restore mapped read-only, and the pager copies it. That is the whole
cost of the memory case: 2,250 copies at 1.2 ms, 3.0 of its 4.2 s. It is a
floor under every case.

**Half of a cluster fault is decoding.** The destination spends about 15 ms
of CPU on each window it rebuilds, mostly SHA-256 over the decoded page, on
CPUs without SHA instructions. The store's path decodes the same envelope.
If a load's four windows decode side by side, that is 15 ms of its 30 ms
from the cluster. The network and the holders' SSDs are the other half at
most.

**The suspend's fills lose some windows, and those make the cluster's tail.**
The fill queue dropped 2.5 to 7 % of the publication's stripes. The windows
that lost a stripe were misses, 76 to 177 a round, read from the store. The
cluster case's 64–128 ms band, 174 requests, is the store latency of those
windows.

**Restoring on the same host is the best case, and still pays.** Its start
took 5.5 s, against about 1 s elsewhere, because it maps what the arena holds.
Its walk took 4.2 s, nearly all copies on write.

## What the plan should change

1. **Make a cluster read cheaper to check.** Half of a cluster fault is SHA-256
   and zstd. Hosts with SHA instructions would cut the hash several times
   over; otherwise the check of a page rebuilt from stripes whose own
   checksums all passed is worth reconsidering, or a faster digest in the
   next envelope version. Measure on a machine type with SHA-NI before
   choosing.
2. **Fill a suspend's memory without dropping it.** A suspend publishes
   gigabytes at once and the 64 MiB fill queue drops part of it. The windows
   it drops are read from the store, and they are the restore's tail. A
   suspend, which is followed by a restore elsewhere, should fill at the
   publication's pace or keep its queue for longer.
3. **Count the copy on write as part of a restore.** A restored page that
   the guest stores into is copied whole, 2 MiB at a time, at about 1.2 ms.
   For an application that writes as it reads, that is about 3 s of a 5 GiB
   heap, wherever the pages came from. A restore could map pages that will
   be private writable from the start, or the measurement of 4 KiB RAM pages
   should include this case.
4. **The suspend itself is slow.** 2.8 GB at 50 MB/s took 57 s with the
   source mostly idle. That is longer than the restore and the walk together.
5. **The plan's measurement 3 is answered for one application.** Its numbers
   should replace the estimate for random access: 2.5 times the store end to
   end, 3.1 times a fault, on these hosts.

## A bug it found

The first run lost the walk of round 1's store case: the command sent as the
start returned was told that no host ran the VM, and the walk never began.
The orchestrator answers a read routed to a VM's host (a command, a console)
from a survey up to a second old. A move dropped that survey when it began,
but every host reads the list of caches every ten seconds, and each read
surveys and remembers. One that ran while the start was opening the VM saw
no host running it and was remembered. Every move now drops the survey again
when it ends, and a survey that overlapped a move is not remembered.
`TestACommandRightAfterAStartFindsTheVM` reproduces it with a read of the
list of caches during the open, and the guard
`orchestrator-remember-a-survey-across-a-move` brings the bug back and fails
it.

The VM that failed was left behind and recovered onto a host, where it held
8 GiB for the rest of that run. The rounds above are a later, clean run; the
run now deletes a VM it fails on and starts only on an empty deployment.

## What it does not show

- More than one restore at once. Every host restoring at the same time
  would load the holders, which served at 0.2 processors here.
- Other machine types. The decoding cost depends on the CPU.
- 4 KiB RAM pages, or a read-ahead other than 8 MiB.
- An application that does not store into what it reads.

## Cleanup

Every node, its disks and the run's objects were deleted.
`gcloud compute instances list`, `gcloud compute disks list` and
`gcloud storage ls` show no `sproutfs-apprestore-` node, disk or object.
