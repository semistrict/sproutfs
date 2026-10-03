# Dependent reads from a hot tier on GCE, 2026-10-03

TASK-84 adds a [hot tier](../hosting.md#reading-through-a-hot-tier): a second
bucket that every read of a checkpoint object tries first, filled behind the
reads and the publications. It is an alternative to the hosts' cluster
cache. This report measures dependent single reads, one page at a time, each
next page chosen from the bytes of the one before, from the regional bucket,
from a warm hot tier and from the hosts' cluster cache. It also measures how
a cold hot tier fills.

## Rapid Bucket, the zonal bucket on GCS

The hot tier is meant to be a bucket close to the hosts. On Google Cloud the
zonal bucket is Rapid Bucket. Google's documentation
(cloud.google.com/storage/docs/rapid/rapid-bucket) says that a Rapid bucket
is read through the JSON, XML and gRPC APIs, but written only through gRPC's
`BidiWriteObject`, as appendable objects. Writes through the JSON API and the
XML API are not supported. The bucket also needs hierarchical namespace and
uniform bucket-level access.

Our GCS adapter writes through the client library's JSON upload, with a
`DoesNotExist` precondition. That is the standard API, and a Rapid bucket
refuses it. So a Rapid bucket cannot be a hot tier without an API of one
cloud, and this change adds none.

Trying it confirmed nothing further. `gcloud storage buckets create` of a
Rapid bucket in us-east4-a, and again in us-central1-a, failed with HTTP 400:
the project does not have the quota (`rapid_zonal_bytes`) for it. The
documentation says the default is 1 TB a zone. Raising it needs the Cloud
Quotas API, which is not enabled on the project, so this run did not change
it. No reads or writes against a Rapid bucket were tried.

So the hot tier here is a second **regional** bucket in the hosts' region,
us-east4, the same kind as the regional bucket. This shows what the hot tier
costs and does: its reads, its misses, its fills and its counters, through
the same adapter, on the real service. It does not show what a bucket closer
than the region would give. A zonal bucket reachable through the standard
API, or another cloud's, could. On AWS that would be S3 Express One Zone,
whose standard S3 calls our S3 adapter makes, but this run had no AWS
account.

## What ran

Two disposable `n2-standard-4` hosts in us-east4-a (4 vCPUs, Intel Cascade
Lake), each with one local NVMe SSD for its cache. Six other hosts of the
project held 24 of its 32 vCPUs and 6 of its 8 external addresses, so two
was what fit. The regional bucket was `echophase-sproutfs-bench` and the hot
tier a bucket made for the run, both STANDARD in US-EAST4.
`scripts/bench-hot-tier-gce.sh` ran it with `sproutfs-restorebench walk`,
built from 13b9916b. It deleted both hosts, their disks, the run's objects
and the hot tier's bucket afterwards, and checked that none remained. Raw
results are in `gce-hot-tier-2026-10-03/two-hosts/`.

Each host ran one process with the real checkpoint store, page cache, peer
server and table of peers, over TCP and the real buckets. The two caches
were one list under 1+1, the code for two hosts, with every window in the
cluster share. Under 1+1 each host holds every window whole, so the reader
read the cluster from its own SSD. There is no Firecracker and no guest. The
memory tier holds 64 MiB and is cleared before each walk, and the reader's
kernel page cache is dropped.

Host 0 published four guests of noise, so nothing compresses:

- `guest`, 1,024 pages of 2 MiB (2 GiB), through a store whose publication
  fills the cluster;
- `guest-hot`, the same bytes, through a store whose publication writes the
  hot tier;
- `small`, 131,072 pages of 4 KiB (512 MiB), filling the cluster;
- `small-hot`, the same bytes, writing the hot tier.

The hot tier that read `guest-hot` and `small-hot` had the defaults: a bound
of 500 ms, a queue of 256 MiB and a rate of 128 MiB/s. Their publications
went through a second hot tier over the same bucket with room for the whole
guest, so the warm case reads what a publication wrote rather than what the
default rate dropped of it.

Host 1 then walked 500 reads at a time. A walk opens the checkpoint, reads
one page, chooses the next page from that page's first 8 bytes and its step,
and reads it, so no read begins before the one before it ends. Each round
started at its own page and read the same chain from every source:

- **regional**: `guest` or `small` through a store with no disk cache, so
  every read is a GET of the regional bucket;
- **hot**: `guest-hot` or `small-hot` through the hot tier;
- **cluster**: `guest` or `small` through the cluster cache.

Three rounds ran each case, in an order each round drew. Before them, three
**cold** rounds walked `guest` and `small` through the hot tier, which held
nothing of them, each round from its own page, with the fills settled between
rounds.

## Results

Each read, in milliseconds, and the walk's reads a second (hops), the median
over the three rounds of each round's figure:

| Source | page | p50 | p90 | p99 | max | hops/s |
| --- | --- | --- | --- | --- | --- | --- |
| regional | 2 MiB | 46.1 | 60.7 | 113.9 | 183.0 | 20.2 |
| hot tier, warm | 2 MiB | 54.7 | 81.0 | 134.6 | 176.5 | 17.3 |
| cluster (own SSD) | 2 MiB | 9.4 | 10.6 | 13.0 | 14.0 | 102.9 |
| regional | 4 KiB | 27.3 | 38.1 | 69.0 | 150.9 | 34.4 |
| hot tier, warm | 4 KiB | 28.6 | 51.0 | 101.1 | 189.0 | 30.6 |
| cluster (own SSD) | 4 KiB | 0.20 | 0.24 | 9.5 | 10.3 | 2,875 |

Each round:

| Source | page | p99 by round | max by round | hops/s by round |
| --- | --- | --- | --- | --- |
| regional | 2 MiB | 117.4, 113.9, 83.3 | 183.0, 416.6, 115.8 | 19.4, 20.2, 22.2 |
| hot tier, warm | 2 MiB | 134.6, 166.4, 123.0 | 158.8, 182.2, 176.5 | 17.9, 16.5, 17.3 |
| cluster | 2 MiB | 13.0, 12.9, 13.4 | 13.6, 14.0, 14.6 | 102.9, 102.5, 103.2 |
| regional | 4 KiB | 139.1, 69.0, 64.1 | 201.5, 110.0, 150.9 | 30.3, 34.4, 35.0 |
| hot tier, warm | 4 KiB | 101.1, 103.2, 79.7 | 189.2, 189.0, 110.1 | 30.6, 28.8, 34.0 |
| cluster | 4 KiB | 9.5, 8.0, 9.5 | 10.3, 10.3, 11.0 | 2,875, 2,983, 2,828 |

Every page read back right. No read failed. Every warm hot walk was all hits,
492 to 508 of them, with no miss, no failure and no GET of the regional
bucket. A walk's reads number a little under 500 where the memory tier held
a page the walk had read before. The cluster walks sent the regional bucket
one GET, the checkpoint's index, or six in the first round.

The cold hot tier, walking `guest` and `small`:

| page | round | hops/s | p50 | p99 | hits | misses | fills sent | MiB sent | dropped (queue, rate) | regional GETs |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | 0 | 14.0 | 69.9 | 144.3 | 226 | 267 | 30 | 1,856 | 195, 3 | 296 |
| 2 MiB | 1 | 17.9 | 52.1 | 142.3 | 487 | 7 | 3 | 192 | 0, 0 | 10 |
| 2 MiB | 2 | 20.1 | 47.5 | 105.8 | 492 | 0 | 0 | 0 | 0, 0 | 0 |
| 4 KiB | 0 | 26.6 | 33.5 | 103.4 | 376 | 131 | 11 | 521 | 48, 3 | 142 |
| 4 KiB | 1 | 35.4 | 26.8 | 66.6 | 508 | 0 | 0 | 0 | 0, 0 | 0 |
| 4 KiB | 2 | 37.4 | 25.2 | 59.8 | 508 | 0 | 0 | 0 | 0, 0 | 0 |

The publications took 12.4 s for each 2 GiB guest and 4.3 s for each
512 MiB guest. The fills that followed settled 10 s later for the cluster and
19 s later for the hot tier, whose one worker wrote 2 GiB at about 110 MB/s.

## What it shows

**A hot tier in the same region as the regional bucket is no faster.** A
warm hot tier answered every read, and sent the regional bucket nothing, but
each read took as long as a GET of the regional bucket, or longer: 55 ms
against 46 ms at the median for a 2 MiB page, and 29 against 27 ms for a
4 KiB page. Both buckets are regional STANDARD buckets in us-east4, so this is
what to expect. The extra 9 ms at 2 MiB is in every round. The hot tier's own
work on a hit is a timer and a counter. The bucket was minutes old, and a new
bucket may not yet be spread over the service's capacity, but this run does
not separate that from anything else.

**The cluster cache is far faster for dependent reads.** Read from the
reader's own SSD, a 2 MiB page took 9.4 ms and a 4 KiB page 0.2 ms at the
median: five times the hops of the regional bucket at 2 MiB, and more than
eighty times at 4 KiB. Its tail was tight too: the worst read was 14 ms. Under 1+1 on two
hosts every read is local. On more hosts a read asks its peers too, which
adds round trips this run does not have.

**A cold hot tier fills in about one walk.** The first cold walk of the
2 MiB guest missed 267 times. Each miss of a new part handed over a fill of
the whole part, 64 MiB, so the queue of 256 MiB held four at a time and
dropped 195 misses, and the rate dropped 3. The fills it kept, 30 parts, were
most of the guest's 32, and the second walk missed 7 times. The 4 KiB guest
has 8 parts, and its first walk filled all of them, so the next walks read
only hits. A miss costs the hot tier's 404 and then the regional read: the
first cold walk was 24 ms slower at the median than the regional walks at
2 MiB, and 6 ms at 4 KiB. The fills cost nothing a read waited for.

**A miss of a 4 KiB page copies 64 MiB.** The fill is the same object under
the same name, and a page lives in a part. Here the first walk of the small
guest copied 521 MiB, all of it, to serve 131 misses.

## What it does not show

- A zonal hot tier. No zonal bucket on GCS takes our writes; see above.
- The cluster over the network. Two hosts under 1+1 read their own SSD. The
  [cluster read measurement](gce-cluster-reads-2026-10-03.md) reads a guest
  from six hosts under 4+2, 16 pages at a time, not one.
- A guest. A walk is one process reading through the store, as a pager's
  faults would.
- Many readers. One host read, one page at a time.
- Expiry of the hot tier, which is not written.

## Cleanup

Both hosts, their disks, the run's objects and the hot tier's bucket were
deleted, and the script checked that none remained. Three smoke runs before
it were deleted the same way. The first stopped at a flag `gcloud storage
buckets create` does not take, and made nothing. The second failed to start
its third host on the project's quota of 8 external addresses. The third ran
on two hosts and small guests.
