# A guest fault beside a post-copy stream on GCE, 2026-10-03

The measurement TASK-82 asks for: how long a guest fault waits while a
post-copy stream runs to the same host, over the release before's page server
and over the [peer server](../migration.md#the-peer-server), and how stripe
reads fare over the peer server beside the stream.

## What ran

Two disposable `n2-standard-4` hosts in us-east4-a (4 vCPUs, Intel Cascade
Lake, 10 Gbps). The project's CPU quota is 32 vCPUs with 8 used elsewhere.
Every host was deleted afterwards, with its disks, and the script checked
that none remained. `scripts/bench-peer-gce.sh` ran it. Raw results are in
`gce-peer-server-2026-10-03/run-2/`.

One host served a VM of four memory regions of 1024 pages of 2 MiB each. The
pages are noise, so no compression shrinks them. Every page is one no
checkpoint holds, so a fault on it is asked again while the source is busy, as
a fault on such a page is. The other host ran the destination:

- a fault on one page of each region every 100 ms, 40 a second in all, each
  asked again after a busy answer or a failure with the backoff a destination
  uses (1 ms doubling to 100 ms), and timed from the first ask to the answer;
- in the stream cases, every page of each region fetched over and over, as
  the post-copy stream does: three requests in flight a region before (all
  but one of its four connections), four a region after;
- in the stripe cases, 500 reads a second of one 90 KiB stripe from the peer
  server's cache, which answers from a file with `sendfile`.

Before is main at 6af0d3de: one connection pool a memory region, a per-peer
budget of eight connections and 8 MiB in flight, and connections over it
closed. After is this branch at 61879c19 with its defaults: one table of
peers, a fault class and a bulk-read class, a background budget of 16 MiB
that shrinks to a quarter while a fault waits. Both built the same workload
(`cmd/sproutfs-peerbench/workload.go`); before ran it as vmmigrate's test
binary, because its page server and client are internal there.

Each case timed faults for 30 s after two seconds of warm-up. Three rounds
ran every case of both builds, alternating which went first. Latencies are
the median over the rounds of each round's percentile, in milliseconds.
CPUs are the CPU time each process used over the case divided by its length.

A first run, with a fault of each region every 20 ms, was stopped after two
cases (`run-1/`): at that rate the faults alone kept the hosts busy, so it
measured CPU rather than waiting.

## Results

Faults, in milliseconds:

| Build | Case | p50 | p90 | p99 | p99.9 | max | busy answers | stream MB/s | server CPUs | client CPUs | failed asks |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| before | idle | 31.1 | 37.7 | 51.6 | 54.7 | 55.5 | 0 | — | 0.57 | 0.52 | 0 |
| after | idle | 28.1 | 31.0 | 35.9 | 42.6 | 44.6 | 0 | — | 0.55 | 0.42 | 0 |
| before | stream | 47.7 | 118 | 612 | 1,640 | 2,158 | 214 | 429 | 3.06 | 3.06 | 25,046 |
| after | stream | 55.9 | 80.5 | 97.3 | 106 | 118 | 0 | 300 | 2.33 | 1.69 | 0 |
| after | stripes | 27.6 | 35.2 | 44.9 | 48.4 | 64.8 | 0 | — | 0.60 | 0.49 | 0 |
| after | stream and stripes | 45.4 | 72.1 | 99.9 | 114 | 139 | 0 | 267 | 2.20 | 1.61 | 0 |

The rounds agree: before's stream p99 was 770, 563 and 612 ms; after's was
97.0, 97.7 and 97.3 ms. Busy answers and failed asks are totals over the three
rounds. Each of before's failed asks was a stream request on a connection the
source closed for being over its per-peer budget of eight.

Stripe reads over the peer server, 500 a second, in milliseconds:

| Case | p50 | p90 | p99 | p99.9 | max |
| --- | --- | --- | --- | --- | --- |
| stripes beside the faults | 0.4 | 13.1 | 27.0 | 35.3 | 58.2 |
| stripes beside the faults and the stream | 9.1 | 34.3 | 63.3 | 86.0 | 107 |

The release before has no stripe requests, so they have no before here. Over
the stripe bench's own protocol, a 90 KB stripe read from memory took about
0.9 ms at the median ([stripes on GCE](gce-stripes-2026-10-03.md)).

The transport's Linux-only tests passed on the serving host: a file range over
TCP is sent with `sendfile` without the file being read into the process, and
host sockets carry a `TCP_USER_TIMEOUT` of ten seconds. The peer package's
whole suite passed there too.

## What it shows

**The tail is gone.** Beside the stream, before's faults reached 612 ms at
p99 and 1.6 s at p99.9. The source answered 214 of them busy, because the
stream held the per-peer budget faults shared, and closed over 8,000 stream
connections a round for being past the connection budget. After, a fault
never waits past 118 ms, no answer is busy and nothing is refused: faults
have their own class, budget and connections, and the stream waits here
before it sends.

**The median did not improve, and the stream gave up 30 %.** A fault beside
the stream took 56 ms at the median after, 48 ms before. The stream ran at
300 MB/s after, 429 MB/s before. The background budget shrinks to a quarter,
two pages, while any fault waits, and at 40 faults a second of about 50 ms a
fault is waiting most of the time. So the stream has two pages in flight
instead of eight. That is the trade the budget is meant to make. Whether a
quarter is the right fraction is a tuning question this run does not answer.

**The link was never the limit; the page codec was.** An idle fault took
28 ms for 2 MiB that a 10 Gbps link carries in under 2 ms. At 40 faults a
second the source used 0.55 CPUs and the destination 0.42, so one page costs
about 14 ms of the source's CPU and 10 ms of the destination's. The stream
before kept three of the source's four CPUs busy at 429 MB/s, a third of the
link. Every page goes through `internal/blob`, which hashes it with SHA-256
at both ends and tries zstd at the source. These hosts have no SHA
instructions. This run did not profile the codec, so how the 24 ms divides is
not measured. It is the next thing to look at for fault latency, and it is
not the transport's.

**A stripe read waits behind a page fault.** Alone, a stripe read takes
0.4 ms at the median, but 13 ms at p90 and 27 ms at p99. Stripe reads are in
the fault class, so they share the two fault connections with 2 MiB page
faults, and replies on a connection leave in the order their requests came.
A stripe behind a page waits for that page's codec. This is the
small-behind-large problem again, inside one class. Stripe reads need
connections of their own, or a class of their own, before the cache uses
them for restores.

## Cleanup

Both hosts and their disks were deleted. `gcloud compute instances list` and
`gcloud compute disks list` show no `sproutfs-peer-` host or disk.
