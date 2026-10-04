# Dependent reads from a hot tier on S3 Express One Zone, AWS, 2026-10-04

The [GCE run](gce-hot-tier-2026-10-03.md) could not try a zonal hot tier: no
zonal bucket on Google Cloud takes our writes. AWS has one, S3 Express One
Zone. This run puts the [hot tier](../hosting.md#reading-through-a-hot-tier)
on a directory bucket in the hosts' availability zone and compares it with
the regional bucket and with the cluster cache on network disks, as the
[shards](gce-shards-2026-10-04.md) keep it. Each case is a chain of dependent
single reads: one page at a time, each next page chosen from the bytes of the
one before. It also measures how a cold hot tier fills, and what each design
costs.

## S3 Express through the S3 adapter

A directory bucket is named `bucket--<zone id>--x-s3`, for example
`s3://sproutfs-hot--use1-az4--x-s3/prefix`. The AWS SDK for Go v2 sees the
suffix, sends each request to the zone's endpoint
(`s3express-use1-az4.us-east-1.amazonaws.com`), and signs it with a session
it makes and renews with `CreateSession`. So the S3 adapter needs nothing for
it but the bucket's name.

The adapter's conformance test ran against a real directory bucket in
use1-az4 (`TestS3ObjectStoreConformanceOnABucket` with
`SPROUTFS_TEST_S3_BUCKET` set). Every case passed but one:

- **Create-if-absent works.** A PUT with `If-None-Match: *` of an object that
  is there fails with 412. So a fill is the same create-if-absent PUT as on
  any bucket, and two hosts filling one object at once leave one copy.
- `If-Match` on a PUT and a DELETE, ranged and suffix GETs, HEAD, and 404 on
  a missing object behave as on a general purpose bucket.
- **The ETag is opaque.** Two PUTs of the same bytes got two ETags, neither
  the MD5 of the body. A compare-and-set on it still holds. The hot tier
  never compares ETags.
- **A listing comes back in no order**, and only under a prefix that ends in
  a slash. The store's interface promises keys in order, so the adapter now
  refuses `List` on a directory bucket (`ErrUnorderedListing`, an
  `errors.ErrUnsupported`). A hot tier never lists. A directory bucket is
  therefore a hot tier only, never a deployment's own bucket.

`TestADirectoryBucketIsWrittenThroughASessionAtItsZone` checks the same
offline: the store's create-if-absent PUT goes to the zonal endpoint with the
session's token and `If-None-Match: *`, after one `CreateSession`.

## What ran

`scripts/bench-hot-tier-aws.sh` ran `sproutfs-restorebench walk`, built from
163f35ca (sha256 `c73bb7e8…bc062`), on six disposable hosts in use1-az4
(us-east-1d). The script made everything the run used and tagged it
`sproutfs-bench=sproutfs-aws-20261004-133556`: a VPC with one subnet in the
zone, gateway endpoints for S3 and for S3 Express, a security group, a key
pair, an IAM role that could reach only the run's two buckets, the two
buckets and the hosts. Afterwards it deleted all of it.

- **Hosts.** Six `m7i.xlarge`: 4 vCPUs of a Xeon Platinum 8488C (Sapphire
  Rapids, with SHA instructions), 16 GiB, up to 12.5 Gbps, Amazon Linux 2023.
  It is the same core and the same network and EBS limits as `c7i.xlarge`,
  with twice the memory, so the publisher's fill queue (4 GiB) holds a whole
  publication beside a 2 GiB guest; the nodes ran with `GOMEMLIMIT=12GiB`. It
  is the nearest type to the `c3-standard-4` of the shards run. Its EBS
  limits: 156 MB/s and 6,000 IOPS sustained, 1,250 MB/s and 40,000 IOPS for
  30 minutes a day.
- **Regional bucket.** A general purpose bucket in us-east-1, S3 Standard.
- **Hot tier.** A directory bucket in use1-az4, the hosts' zone.
- **Cluster cache.** One gp3 volume a host, 64 GiB, provisioned 16,000 IOPS
  and 500 MiB/s, under 4+2 over the six hosts. Each node kept its cache on
  the volume's raw block device
  (`/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol<id>`), opened
  exclusively with buffered I/O, as a shard keeps its disk. 500 MiB/s is a
  node's serving budget, and 16,000 IOPS covers a shard's 512 KiB stripe
  reads at that rate (two IOPS each) with room for small ones. The volumes
  were attached at launch by the script. The AWS network-disk adapter that
  TASK-86 needs, to attach them as the membership says, is not written.

Host 0 published four guests of noise, as in the GCE run: `guest`, 1,024
pages of 2 MiB, and `small`, 131,072 pages of 4 KiB, each once filling the
cluster and once (`guest-hot`, `small-hot`) writing the hot tier. Host 1 then
walked 500 reads at a time from each source, three rounds of each, each round
in an order it drew and from a page of its own:

- **regional**: every read a GET of the regional bucket;
- **hot**: through the hot tier, warm, holding what the publication wrote;
- **cluster**: through the cluster cache, four stripes from the other hosts'
  volumes for each page.

Before those, three **cold** rounds walked `guest` and `small` through the hot
tier, which held nothing of them, with the fills settled between rounds. The
hot tier had its defaults: a bound of 500 ms, a queue of 256 MiB and
128 MiB/s. Before each walk the reader emptied its memory tiers and dropped
the kernel's page cache. Then every node stopped and fio measured each gp3
volume raw (`scripts/lib/shard-fio.sh`). Raw results were kept outside the
repository; `scripts/lib/hot-tier-summary.py` makes these tables from them.

## Results

Each read, in milliseconds, and the walk's reads a second (hops), the median
over the three rounds of each round's figure; the GETs and hot-tier hits are
over all three rounds:

| Source | page | p50 | p90 | p99 | max | hops/s | regional GETs | hot GETs | hot hits |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| regional S3 Standard | 2 MiB | 102 | 157 | 241 | 293 | 9.4 | 1,515 | 0 | |
| hot tier on S3 Express, warm | 2 MiB | 16.9 | 19.0 | 21.0 | 26.0 | 61.1 | 0 | 1,515 | 1,515 |
| cluster on gp3, 4+2 | 2 MiB | 6.23 | 6.80 | 7.88 | 11.2 | 161 | 3 | 0 | |
| regional S3 Standard | 4 KiB | 26.3 | 55.9 | 123 | 138 | 30.8 | 1,527 | 0 | |
| hot tier on S3 Express, warm | 4 KiB | 4.20 | 5.10 | 10.9 | 16.0 | 227 | 0 | 1,527 | 1,527 |
| cluster on gp3, 4+2 | 4 KiB | 1.24 | 1.42 | 4.89 | 5.69 | 760 | 3 | 0 | |

Each round:

| Source | page | p50 by round | p99 by round | max by round | hops/s by round |
| --- | --- | --- | --- | --- | --- |
| regional | 2 MiB | 107, 102, 101 | 241, 244, 233 | 277, 315, 293 | 9.0, 9.4, 9.5 |
| hot, warm | 2 MiB | 14.4, 16.9, 17.5 | 20.4, 21.0, 21.0 | 27.5, 22.3, 26.0 | 66.5, 61.1, 58.9 |
| cluster | 2 MiB | 6.18, 6.23, 6.29 | 8.64, 7.88, 7.72 | 11.2, 9.91, 11.8 | 162, 161, 161 |
| regional | 4 KiB | 26.7, 25.3, 26.3 | 134, 61.7, 123 | 155, 125, 138 | 28.9, 37.6, 30.8 |
| hot, warm | 4 KiB | 4.97, 4.20, 4.05 | 13.8, 10.6, 10.9 | 16.0, 11.7, 18.1 | 195, 227, 232 |
| cluster | 4 KiB | 1.22, 1.28, 1.24 | 4.70, 4.89, 4.98 | 8.48, 5.55, 5.69 | 769, 740, 760 |

Every page read back right and no read failed. Every warm hot walk was all
hits, with no miss, no failure and no GET of the regional bucket. A cluster
page took four stripe requests, one to each of four holders; 8 of about
3,000 pages sent a second request and none read the store. The cluster walks' only GETs of
the regional bucket were the checkpoint's index, once a guest.

The cold hot tier, walking `guest` and `small`. A fill copies the whole part,
64 MiB, which holds the page:

| page | round | hops/s | p50 | p99 | hits | misses | fills sent | MiB sent | already there | dropped (queue, rate) | regional GETs |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2 MiB | 0 | 14.9 | 46.1 | 232 | 217 | 288 | 27 | 1,664 | 3 | 214, 1 | 317 |
| 2 MiB | 1 | 46.8 | 14.8 | 131 | 462 | 43 | 6 | 384 | 0 | 7, 2 | 49 |
| 2 MiB | 2 | 59.6 | 17.4 | 21.5 | 505 | 0 | 0 | 0 | 0 | 0, 0 | 0 |
| 4 KiB | 0 | 50.5 | 5.79 | 160 | 307 | 202 | 10 | 522 | 0 | 41, 2 | 212 |
| 4 KiB | 1 | 206 | 4.55 | 11.2 | 509 | 0 | 0 | 0 | 0 | 0, 0 | 0 |
| 4 KiB | 2 | 235 | 4.03 | 11.1 | 509 | 0 | 0 | 0 | 0 | 0, 0 | 0 |

No read of the hot tier failed, and no fill failed. "Already there" is a
create-if-absent PUT that S3 Express refused with 412 because the object was
there: a read missed a part just before its first fill landed, and the
second fill found it written.

The publications, from host 0:

| Guest | commit, s | settled, s |
| --- | --- | --- |
| `guest`, 2 GiB, filling the cluster | 3.6 | 4.5 |
| `guest-hot`, 2 GiB, writing the hot tier | 3.4 | 19.5 |
| `small`, 512 MiB, filling the cluster | 1.6 | 23.2 |
| `small-hot`, 512 MiB, writing the hot tier | 1.3 | 5.5 |

The hot tier's one fill worker wrote 2 GiB to S3 Express in 16 s, about
130 MiB/s. No fill of either kind was dropped.

### Each gp3 volume raw

fio on the raw device, O_DIRECT, libaio, the median over the six hosts:

| Job | MiB/s | IOPS | p50 µs | p99 µs |
| --- | --- | --- | --- | --- |
| read, random, 4 KiB, 1 deep | 6.5 | 1,658 | 586 | 901 |
| read, random, 4 KiB, 16 deep | 63 | 16,000 | 987 | 1,327 |
| read, random, 4 KiB, 64 deep | 63 | 15,998 | 3,949 | 4,424 |
| read, random, 512 KiB, 1 deep | 307 | 614 | 1,573 | 2,359 |
| read, random, 512 KiB, 4 deep | 500 | 1,000 | 3,981 | 4,948 |
| read, random, 512 KiB, 16 deep | 500 | 1,000 | 15,925 | 16,941 |
| read, in order, 1 MiB, 8 deep | 500 | 500 | 15,925 | 16,908 |
| write, in order, 1 MiB, 8 deep | 533 | 533 | 15,925 | 16,318 |
| write, random, 4 KiB, 16 deep | 63 | 16,187 | 979 | 1,253 |
| write, random, 512 KiB, 4 deep | 513 | 1,026 | 3,949 | 4,358 |

Each volume gave what it was provisioned: 16,000 IOPS and 500 MiB/s, within
the host's burst limits. A 4 KiB read at one in flight took 586 µs, against
about 300 µs for a GCE persistent disk and 467 µs for Hyperdisk Balanced in
the shards run.

## Cost

Prices for us-east-1 from the AWS Price List API
(`aws pricing get-products`, read 2026-10-04), which is what
aws.amazon.com/s3/pricing and aws.amazon.com/ebs/pricing show:

- **S3 Express One Zone**: storage $0.11 a GB-month; PUT, COPY, POST and LIST
  $0.00113 per 1,000; GET and all other requests $0.00003 per 1,000; upload
  $0.0032 a GB and retrieval $0.0006 a GB. Since April 2025 the upload and
  retrieval charges apply to every byte, not only to the part of a request
  past 512 KB (aws.amazon.com/about-aws/whats-new/2025/04/
  amazon-s3-express-one-zone-reduces-storage-request-prices/).
- **S3 Standard**: storage $0.023 a GB-month for the first 50 TB; PUT
  $0.005 per 1,000; GET $0.0004 per 1,000.
- **EBS gp3**: $0.08 a GB-month, with 3,000 IOPS and 125 MiB/s included;
  $0.005 per provisioned IOPS-month and $0.04 per provisioned MiB/s-month
  above those. (The worked example on aws.amazon.com/ebs/pricing uses
  $0.06 per MB/s-month; the price list says $0.04 for us-east-1.)
- **m7i.xlarge**: $0.2016 an hour on demand.

A month for a hot set of 10 TiB read at 1,000 pages a second, 2.63 billion
page reads in 730 hours, with every read a hit. The regional bucket is the
same under both designs and is left out; read from it instead, 1,000 GETs a
second would cost $1,051 a month in requests alone.

**Hot tier on S3 Express**, holding each object once (10,240 GB):

| Item | 2 MiB pages | 4 KiB pages |
| --- | --- | --- |
| storage, 10,240 GB × $0.11 | $1,126 | $1,126 |
| GETs, 2.63 billion × $0.00003 per 1,000 | $79 | $79 |
| retrieval, 5.13 million GB or 10,025 GB × $0.0006 | $3,080 | $6 |
| **a month** | **$4,285** | **$1,211** |
| filling all 10 TiB once (upload and 163,840 PUTs) | $33 | $33 |

**Shards on gp3 under 4+2**, holding 1.5 times the bytes (15,360 GB) on 12
volumes of 1,280 GB. A 2 MiB page is four 512 KiB stripe reads, so 1,000
pages a second is 2,000 MiB/s and 8,000 IOPS over the volumes; a 4 KiB page
is four reads of 1 KiB, 4,000 IOPS:

| Item | 2 MiB pages | 4 KiB pages |
| --- | --- | --- |
| storage, 15,360 GB × $0.08 | $1,229 | $1,229 |
| IOPS: 667 or 333 a volume, within the 3,000 included | $0 | $0 |
| throughput: 167 MiB/s a volume, provisioned 210 (85 above 125) × 12 × $0.04 | $41 | $0 |
| **a month** | **$1,270** | **$1,229** |

The shards also need hosts to serve them, and a host's EBS limit binds before
the volume's: an `m7i.xlarge` sustains 156 MB/s. Twelve shards at 2 MiB pages
need hosts that sustain about 220 MB/s each, such as `m7i.2xlarge`
(312.5 MB/s), or more shards. A deployment already runs hosts; if its hosts
are too small for that, the difference is compute cost this table leaves out.
Traffic within one zone, to S3 Express or between hosts, is not charged.

So at 4 KiB pages the two cost about the same, $1,211 and $1,229 a month, both
mostly storage: S3 Express's $0.11 a GB at 1× is near gp3's $0.08 a GB at
1.5×. At 2 MiB pages the hot tier's retrieval charge, $0.0006 a GB on every
byte read, is $3,080 of its $4,285, and the shards cost $1,270.

## What it shows

**The cluster cache is faster than a hot tier on S3 Express, for dependent
reads of both sizes.** A 2 MiB hop took 6.2 ms from the cluster and 16.9 ms
from S3 Express, 161 against 61 hops a second. A 4 KiB hop took 1.24 ms
against 4.20 ms, 760 against 227 hops a second. The cluster's tail was
tighter too: its p99 was 7.9 and 4.9 ms, the hot tier's 21 and 10.9 ms.

**A hot tier on S3 Express is far faster than the regional bucket.** It took
a sixth of the time of S3 Standard: 16.9 against 102 ms at 2 MiB and 4.2
against 26.3 ms at 4 KiB, with a much tighter tail (21 against 241 ms at p99
for 2 MiB). It is "single-digit milliseconds" for a small read, as AWS says,
and about 13 ms more to move 2 MiB. Unlike the hot tier on a second regional
bucket in the GCE run, this one pays for itself in latency.

**The regional bucket was slow at 2 MiB.** S3 Standard took 102 ms for a
2 MiB page, against 46 ms for a regional GCS bucket in the GCE run, while
4 KiB took 26 ms on both. Both buckets of this run were minutes old. This run
does not separate a new bucket from the service's speed for one stream.

**The cluster on gp3 is as fast as on GCE's network disks.** A 2 MiB hop took
6.2 ms here and 7.3 ms on C3 hosts with pd-balanced; a 4 KiB hop 1.24 ms
against 1.0 ms. The hosts here have SHA instructions, which the C3 hosts also
have.

**A cold hot tier fills in about two walks, and its queue drops most misses
of 2 MiB pages.** The first cold walk of the 2 MiB guest missed 288 times.
Each fill of a new part is 64 MiB, so the default queue of 256 MiB held four
and dropped 214 misses. The 27 fills it kept were most of the guest's 34
parts, the next walk missed 43 times, and the third walk was all hits at the
warm speed. The 4 KiB guest's parts filled in its first walk. A miss costs
the hot tier's 404 and then the regional read, but fills land during the
walk, so even the first cold walk of 2 MiB pages was faster at the median
than the regional bucket, 46 against 102 ms, with the same p99.

**The hot tier costs more than the shards for large pages, the same for small
ones.** See the cost section: S3 Express charges for every byte it returns,
which at 1,000 2 MiB pages a second is $3,080 a month. The shards' gp3 needs
only throughput above the included 125 MiB/s, and hosts big enough to serve
it.

So the owner's question has a plain answer for dependent faults: the
read-through hot tier on S3 Express does not match the cluster cache on
network-disk shards. It is 2.7 times slower at 2 MiB and 3.4 times at 4 KiB,
and costs about the same at 4 KiB and three times as much at 2 MiB. It is
simpler: no disks to attach, no membership of shards, and a lost zone costs
only misses. It is six times faster than the regional bucket.

## What it does not show

- Many readers. One host read one page at a time.
- A guest. A walk is one process reading through the store, as a pager's
  faults would.
- Run reads. A fault today reads a run of 8 MiB; this run read single pages.
- Shards that move. The volumes were attached at launch and stayed; the AWS
  network-disk adapter for TASK-86 is not written.
- A bucket of S3 Standard that has served for a while; both buckets were new.
- Sustained load on the hosts' EBS limits. The run stayed within their burst.

## Cleanup

The script's `delete` terminated the six hosts, which deleted their root and
gp3 volumes, emptied and deleted both buckets, and deleted the key pair, the
security group, both gateway endpoints, the route table, the internet
gateway, the subnet, the VPC, the role and the instance profile. It then
listed each kind by the run's tag and found none. Listed again by hand
afterwards: the six instances were `terminated`, no volume, key pair,
security group or endpoint carried the tag, the account's VPCs were the four
it had before, `list-directory-buckets` was empty, no general purpose bucket,
role or instance profile was named for the run. A probe directory bucket used
for the conformance test before the run was emptied and deleted too.
