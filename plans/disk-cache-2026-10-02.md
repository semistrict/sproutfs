# The distributed disk cache — 2026-10-02

**Status: proposed.**

## The goal

A host's local SSD is large and fast, and today it is mostly unused. The page
cache's disk holds only the pages of VMs marked to pull, only while those VMs
run, and only up to a fixed cap that is off by default. Every other cold fault,
and every refault of a page the pager evicted, reads the object store.

A cold fault on GCS costs 40 to 58 ms on average for a 2 MiB page, measured on
GCE on 2026-09-23. Lambda's zone cache, which is the closest system to this
one, serves a hit in 550 µs at the median and 3.7 ms at p99.9, against 36 ms
and 175 ms from S3 ([research](../docs/research/lambda-container-loading-2026-10-02.md)).
Our estimate for a page from the cluster's SSDs is 1.5 to 3 ms, most of it the
decode that a store read pays too. So the cluster's disks are worth about
twenty times the object store.

This plan makes the hosts' SSDs one cache for the cluster. It serves these
[desired properties](../docs/properties/README.md):

- [Stopping does not evict](../docs/properties/stop-does-not-evict.md)
- [Tiers evict independently](../docs/properties/tiers-evict-independently.md)
- [Then from the cluster's disk cache](../docs/properties/restart-reads-disk-cache.md)
- [The object store is the last resort](../docs/properties/object-store-last-resort.md)
- [One disk cache for the whole host](../docs/properties/disk-cache-shared-by-every-vm.md)
- [The disk cache uses the disk it is given](../docs/properties/disk-cache-uses-the-disk.md)
- [The hosts' disk caches form one cache](../docs/properties/disk-caches-form-one-cache.md)
- [A cached page survives losing a host](../docs/properties/a-page-survives-losing-a-host.md)
- [A hot page spreads its load](../docs/properties/a-hot-page-spreads-its-load.md)
- [A slow host does not slow reads](../docs/properties/a-slow-host-does-not-slow-reads.md)
- [Two hosts are enough](../docs/properties/two-hosts-are-enough.md)
- [Serving a peer copies nothing into memory](../docs/properties/serving-a-peer-copies-nothing.md)
- [One disk limiter](../docs/properties/one-disk-limiter.md)
- [The disk limiter follows its goals](../docs/properties/disk-limiter-goals.md)

One property is left to a later plan: placing a restarted VM on the host that
still holds its pages in memory.

The design draws on four systems, each written up in `docs/research/`:
[Lambda's container loading](../docs/research/lambda-container-loading-2026-10-02.md),
[CacheLib's Navy](../docs/research/cachelib-navy-2026-10-02.md),
[groupcache](../docs/research/groupcache-2026-10-02.md) and
[Memcache at Facebook with mcrouter](../docs/research/memcache-facebook-2026-10-02.md).
Where this plan takes a choice from one of them, it says so.

## What stays the same

The design rests on one fact the system already guarantees: **a page's bytes
never change under its identity.** A page is named by the checkpoint that
published it. Compaction moves its bytes and keeps its name. A drawn epoch and
a refused create keep a name from ever naming other bytes. So a cached copy is
never stale. It can only be absent, damaged or no longer wanted.

What stays as it is:

- The cache is keyed by page identity and segment identity.
- What is cached is each member's and segment's encoded envelope, as the store
  holds it. A page read from the cache is checked by the same envelope as a
  page read from the store.
- Nothing is published from the cache. A cached copy is never evidence that a
  publication landed.
- Each host's memory tier, its cap and its LRU are unchanged, and so is the
  pager's arena. Memory is the host's own tier. The disks are the cluster's.
- Concurrent misses on one host still share one fetch, through the cache's
  flights, under the fetch's own context. A caller that gives up does not fail
  the others.

## The cluster's disk cache

Every envelope in the cluster cache is stored **erasure-coded** across the
disks of several hosts. The deployment sets the code: k data stripes and m
parity stripes, by Reed-Solomon. An envelope is split into k stripes of equal
length, padded at the end, and m parity stripes are computed from them. Any k
of the k+m stripes rebuild the envelope. A reader asks every holder and
decodes from the first k that arrive. The code is systematic: the first k
stripes are the envelope itself, so a read that gets those first does no
decoding. The library is `github.com/klauspost/reedsolomon` (MIT), which uses
SIMD and runs at several GB/s per core.

The default is 4+2. Two parity stripes let a reader ignore any two holders.
That matters because drains and rolling restarts are routine: with one parity
stripe, a drained host uses up the spare, and until repair refills it one slow
host puts reads back in the tail. With two, one host can be gone and another
slow, and no read waits. 4+2 costs half again the disk of one copy, about a
sixth less capacity than 4+1. The hosts' SSDs are large, and tail latency is
what a restore feels. 6+2 would cost less disk, but its stripes are a few
hundred bytes at a 4 KiB page and it needs eight hosts.

This is the choice Lambda's zone cache made, and its reasons are ours
([research](../docs/research/lambda-container-loading-2026-10-02.md#erasure-coding-in-the-az-cache)):

- **Tail latency.** A restore of an 8 GiB guest reads about 4,000 windows. With
  one copy per window, one slow host is in almost every restore's tail. With
  stripes to spare, a reader never waits for the slowest holders.
- **Losing a host costs no hit.** A host lost, drained, restarted or added
  takes at most one stripe of any envelope with it, while the list holds at
  least k+m hosts. The rest still decode. So a deployment rolls through its
  hosts without emptying the cache.
- **A hot envelope spreads its load.** Every reader takes a quarter of the
  bytes from each of four holders. A burst of a hundred hosts reading one
  window costs each holder about 9 MB, not one host 35 MB. No host has to
  notice that the window is hot.
- **Constant work.** A read sends the same requests whether hosts are well or
  not. There is no retry, no hedge to another copy, and no fallback rank to
  ask. Lambda chose this because retries are a known cause of metastable
  failure.

The cost, at 4+2, is half again the disk of one copy, and six requests of
about 90 KB where one copy would take one of about 350 KB. And a drained host's
share of the serving moves onto the others, a fifth more each on six hosts.
Under 4+1 it does not: the host that stands in holds nothing. On GCE that move
explained 4+2's extra tail at full load
([measurement](../docs/measurements/gce-stripes-tail-2026-10-03.md)).

There is no tier of whole local copies. A page read whole from the local disk
would save about half a millisecond over a read from the cluster, and keeping
whole pages on every host that reads them would cost the cluster most of its
capacity. Lambda's small worker-local cache serves two thirds of its chunk
loads, but a worker there has no pager. Here the pager's arena and the memory
tier hold what a host reads again and again. The host reports how often it
reads the same window from the cluster within an hour, so the question can be
asked again with numbers.

### Small clusters

A deployment may have as few as two hosts. The code is fixed per deployment,
and the operator sets it to fit the cluster's usual size:

| Hosts | Code | Extra disk | Survives |
| --- | --- | --- | --- |
| 1 | 1+0 | none | nothing: a single host has no peer |
| 2 | 1+1 | 100 % | one host lost or slow |
| 3 | 2+1 | 50 % | one host lost or slow |
| 4 or 5 | 2+2 | 100 % | two hosts lost or slow |
| 6 or more | 4+2 | 50 % | two hosts lost or slow |

A code with k = 1 is whole copies: 1+1 keeps each envelope whole on two hosts,
and a reader takes the first to answer. So replication is not a second
mechanism. It is the code at k = 1, with the same ranks, fills, reads, repair
and checks.

The code is not derived from the live list. A drain takes a six-host cluster to
five for a while, and a code that followed the list would change, and every
stripe in the cluster would become a miss. An operator who sets no code gets
the table's code for the most caches the orchestrator has listed since it
started, which a drain does not lower. Instead, while the list holds fewer
hosts than k+m, a window's stripes go round the hosts it has: stripe i on rank
((i − 1) mod n) + 1. A host then holds two stripes of some windows, and losing
it costs both. With 4+2 on five hosts, any one host can still be lost. With
4+2 on two hosts, no host can, so the operator sets the code for the size the
cluster normally runs at, not the size it may briefly fall to.

Every stored stripe states its code. A reader that finds a stripe of another
code treats it as a miss, so changing a deployment's code costs a refill from
the store, as the cluster reads, and nothing worse.

### Tenants

A host serves any stripe to any host that asks. That is no wider than today:
any host may already read any tenant's objects in the store. No VMM is given a
cache file or its bytes. A page reaches a guest only through the pager, which
loads it into the arena file the isolated arena allows and refuses a page of
another tenant (`ErrOtherTenant`). So the cache adds no path from one tenant to
another.

## Where a window's stripes live

### Windows

The unit of placement is the **window**: the pages of one volume, in one
aligned 2 MiB span, that one checkpoint published. A window at a 2 MiB page is
one envelope. At a 4 KiB page it is up to 512 envelopes. Every envelope of a
window is striped on the same hosts, so a read-ahead run asks the same five
hosts for all its pages, in one request each. A segment is its own window.

### Ranks

Placement is computed, not recorded. Every host holds the list of caches in
the cluster. For each window, weighted rendezvous hashing ranks every cache:
each cache scores the window by `w / -ln(u)`, where `u` is a 64-bit hash of the
cache's identity and the window, mapped into (0, 1), and `w` is the cache's
weight. Ties go to the lower cache identity. The caches ranked 1 to k+m hold
the window's stripes. A fill puts stripe i on rank i, but a reader never relies
on that: once ranks shift, a holder may hold any index (see
[reading a page](#reading-a-page)).

A cache's weight comes from the size of its configured disk, rounded to a
coarse step, so a host with twice the disk holds about twice the windows. It
never comes from the limiter's moving share. Every change of a weight moves
windows, and the share moves all the time. Rendezvous is kept over a ring of
virtual nodes because it ranks the next cache exactly, which repair depends on
([research](../docs/research/groupcache-2026-10-02.md#who-owns-a-page)).

When a cache joins or leaves, the ranks of a window change only if that cache
is among its first k+m. A join pushes one holder out of the first k+m, and a
leave pulls a cache in that holds nothing yet. Either way the window's first
k+m ranks lose at most one of its stripes, and a reader that takes any index
still decodes.

A cache's identity is not its pod's. It is a random value written in the cache
file's header when the file is made. A pod that restarts on the same node, over
the same persistent volume, keeps its identity and its windows.

### The list of caches

Today a host dials only the page-server address a handoff carries, and has no
identity of its own. The cluster cache needs every host to know every cache.
Each host reports its cache identity, its weight and its page-server address
in `/status`. The orchestrator already surveys every host, and it serves the
list it found (`GET /caches`), with the deployment's code. A host that did not
answer one survey keeps its place in the list, as a crashed host keeps its
place in the model; only a host the cluster no longer has leaves it. Each host
reads that list on a timer and keeps the last one it got. An orchestrator that
is down leaves the list as it was.

Two hosts that hold different lists disagree only about whom to ask. The worst
a stale list costs is a miss, and a miss is a read of the object store.

### Hosts marked down

A host that does not answer is marked down by each reader on its own, as
mcrouter does
([research](../docs/research/memcache-facebook-2026-10-02.md#marking-hosts-down-tko)).
Three timeouts in a row, or one refused connection, mark it down. While it is
down, readers do not ask it and do not send it fills, and they decode from the
other holders. A probe is sent after 10 s, then at intervals growing by half
each time up to 60 s, with jitter. Only a probe that succeeds clears the mark.
A miss or a bad stripe is not a failure of the host.

A reader marks down at most a fifth of the list, and always at least one host,
so a small cluster can still mark one down. Past that, it stops marking
hosts down, because so many failing at once more likely means its own network
has failed. A host's down set is only another list, so it changes no
correctness argument.

## Reading a page

A read looks in this order:

1. this host's memory tier, then the pager's arena, as today;
2. k+1 of the window's first k+m ranks, one request each, for every stripe of
   the window they hold. A rank this host holds is read from its own disk. A
   rank marked down is not asked;
3. the rest of those ranks, only if k stripes have not arrived after a short
   delay, under a budget;
4. the object store.

A reader asks k+1 holders, not all k+m. The stripe benchmark showed why
([measurement](../docs/measurements/gce-stripes-2026-10-03.md)): with every
holder asked, every holder sends its stripe whether or not the reader still
needs it, and at 9,000 reads a second across six hosts 4+2's tail grew to
110 ms at p99, worse than whole reads. One spare request already covers one
slow or lost holder. The second spare is asked only when the first k+1 have
not answered, which is FoundationDB's second request
([research](../docs/research/foundationdb-transport-2026-10-03.md)). So 4+2
keeps its protection against a drained host and a slow one together, at
about the bytes of 4+1.

Which k+1 of the ranks a reader asks is chosen by a hash of the reader's own
identity and the window. So the readers of one window spread their requests
over all of its holders, and one reader always asks the same ones. A holder
that answers that it holds nothing of the window is replaced at once by the
next rank not yet asked; that is a miss, not a hedge.

The delay before the rest are asked follows the reader's own recent stripe
latencies: about their 95th percentile, so about one read in twenty sends a
second request. The second requests draw on a budget, as FoundationDB's do.
Each read that completes within the delay adds a twentieth of a request to it,
and each second request takes one away. So when every holder is slow at once,
the budget runs out and the reader waits, rather than doubling the load on
every holder.

A holder answers with every stripe of the window it holds, of any index.
Ranks shift when a cache joins or leaves: a join near the top of a window's
ranks moves every holder below it down by one, so a holder seldom holds the
stripe whose index matches its rank. A reader that asked rank i for stripe i
would decode nothing though k stripes are there. So a reader decodes from any
k distinct indices it receives. `spec/diskcache` found this (B5 in
[spec/bugs.md](../spec/bugs.md)).

A reader takes the first k stripes of each envelope. It checks each stripe's
own checksum, decodes, and then checks the envelope's SHA-256 and the key
beside it. A stripe that fails its checksum is not used. If the envelope fails
its SHA-256 with more than k stripes in hand, the reader decodes from other
sets of k to find the stripe that is wrong. Either way it tells that stripe's host to drop it, and a
host drops a stripe it is told is wrong. Without that, a bad stripe would be
served until it aged out.

A read waits for stripes up to a bound. If k stripes of an envelope have not
arrived by then, the reader reads the store as well and takes whichever
answers first. The bound is set from the measured latency of the cluster's
reads, a few milliseconds. Reads of the store that start this way are limited
by a token bucket to a small share of reads, 5 % by default, as Brooker
recommends ([research](../docs/research/lambda-container-loading-2026-10-02.md#hedging)).
Past the bucket, the reader waits for the stripes instead. So a slowdown that
reaches every host at once does not double the load on the store.

A cache never forwards a read. It answers from its own disk or says it does
not hold the stripe. A cache never reads the store on a reader's behalf.

A reader keeps nothing on its own disk except the stripes it is ranked for, as
any filler does.

### Repair

When ranks change, a window's stripes no longer sit one to each of its first
k+m ranks. A reader that decodes an envelope rebuilds an index that no rank
holds, and sends it to a rank that holds fewer of the window's stripes than the
code puts on it. It never sends an index another rank already holds, so a
change of ranks never leaves a holder with two stripes of one window. Repair is bounded by the same rate as fills, and it is the
lowest priority of all writes. So a window that is read heals itself, and a
window that is not read ages out.

This is the write half of mcrouter's warm-up route. Without it, a cache that
joins would never warm for the windows it took
([research](../docs/research/memcache-facebook-2026-10-02.md#warm-up)).

### In flight

Each reader bounds the stripe requests it has in flight to each peer, and uses
one pooled connection per peer, as the page server's per-peer budgets already
do. A pull or a restore that faults thousands of windows then cannot flood one
peer's link.

## Filling the cluster

Three things bring a window to the cluster:

1. **A read of the store.** A run the store served has its members' and
   segments' envelopes in hand. The reader stripes them and sends each stripe
   to its rank behind the read.
2. **A publication.** Each part's envelopes are striped and sent once the part
   is durable, and each segment once the index object is. This primes the
   cache. A suspending stop's RAM, a fork point and a template import all
   publish, so their windows are in the cluster before the restore or the
   fan-out that reads them. Lambda found its lowest hit rates in bursts on new
   functions, and planned to prime its cache the same way.
3. **A pull** (see [pull](#pull)).

The fill request is **keep**, a new page-server request. A host ranked for a
window writes its own stripe to its own disk.

A cold burst would fill one window many times: a hundred hosts miss it at
once, each reads the store, and each sends its stripes. So rank 1 hands out
one **fill right** per window per interval, as Memcache's leases give one
client per key the right to fill
([research](../docs/research/memcache-facebook-2026-10-02.md#leases)). A miss
reply from rank 1 carries the right to the first reader that asks. The others
read the store as they would anyway, and send nothing. A host also drops a
keep for a stripe it already holds or is already writing. No reader waits for
another reader's fill: a waiting reader is no faster than one that reads the
store itself.

Nothing waits on a fill. Local writes go through one bounded queue per host,
and keeps through a bounded rate per host. A fill that finds either full is
dropped, and the window is read from the store next time. A fault, a
publication and a pull never wait for one.

## Serving stripes

A host serves stripes without reading them into its own memory. Stripes are
stored as they go on the wire, so there is nothing to decode or encode, and
the stripes of a run lie next to each other in one disk region. A read request
can be served by one `sendfile` from the cache file to the socket per run,
after a small header.

That needs three things:

- **Buffered I/O on the cache file, not `O_DIRECT`.** `sendfile` reads through
  the kernel's page cache, which also keeps the stripes many hosts ask for in
  memory. That memory counts against the pod's limit, and the kernel reclaims
  it before anything fails. Navy uses `O_DIRECT`, but it keeps its own DRAM
  cache in front of the disk and copies every hit anyway
  ([research](../docs/research/cachelib-navy-2026-10-02.md)). A host drops what
  it read for itself or for eviction from the page cache
  (`POSIX_FADV_DONTNEED`), so only what peers ask for stays.
- **No CRC32C on stripe replies.** Each stripe carries its own checksum, which
  was written with it.
- **A file-range send on the transport.** The TCP adapter does it with
  `sendfile`. The simulated transport copies. A transport under TLS copies
  through a bounded buffer per peer, as the page server does today.

Lambda's cache servers fill 50 Gb/s links from user space, with no
`sendfile`. So the cost of the copy is measured before this is built (see
[the steps](#the-steps)). If a plain copy costs too little to matter, the
property is met by the bounded buffer and step 8 is dropped.

A host's serving bandwidth is a budget, and the tail meets it well before the
NIC's rate. On six 10 Gb/s GCE hosts that each read as much as they served,
p99 stayed under 2 ms in 14 of 15 rounds at 4.4 Gb/s served per host and rose
past 89 ms at 6.3,
with no host's CPU over half used
([measurement](../docs/measurements/gce-stripes-tail-2026-10-03.md)). A
deployment's expected read rate, after one drain has moved its share, keeps
each host's serving under about 40 % of its NIC's rate until the deployment's
own machine type is measured.

## On one host's disk

### The layout

The disk is a log of fixed-size **disk regions**, 64 MiB each. One region at a
time is open for writes. Its space is allocated (`fallocate`) when it opens,
so a write never fails half way through a region on a shared filesystem.
Stripes are appended in the order they arrive. A run arrives in page order, so
its stripes lie next to each other.

Each stripe on disk has a header: its key, its index in the code, the code,
its length and a checksum of the header and the bytes. A read checks the key
it finds against the key it asked for. So a read never returns another
stripe's bytes, whatever happened to the index, to a region reused under a
read in flight, or to a write torn by a crash. The checksum makes every damaged
stripe a miss.

When the open region is full it is **closed**. Closing syncs the region's
stripes, then writes the region's table at its end: its sequence number, and
for each stripe its key and offset. Then it syncs again. Buffered writes can
reach the disk in any order, and the first sync keeps a table from ever naming
a stripe that is not there. A region whose table was lost can still be read
back by scanning its headers.

Regions are written whole and given back whole. That is the pattern an SSD
wears least under, and it lets the limiter give space back a region at a time,
one punch each. Navy chose regions for the same reasons. Its regions grow to a
gigabyte only because its eviction reads the whole region into memory, which
this design does not ([research](../docs/research/cachelib-navy-2026-10-02.md)).

### The index in memory

The index maps a key to where its stripe is. At a 2 MiB page a terabyte of disk
holds about 3.5 million stripes of 300 KB, and an entry each costs nothing worth
counting. At a 4 KiB page a terabyte holds about two billion stripes of 512
bytes, and an entry each would cost far more memory than the host has.

So the index is kept per window, as Kangaroo shares the key bits its buckets
imply ([research](../docs/research/cachelib-navy-2026-10-02.md)). A window is
keyed by an 8-byte hash of its identity. The key check on every read catches a
collision. An entry holds the region, the window's first offset, which pages
are present and each present stripe's length, about 4 bytes a page. A window
with few pages present holds a short list of (page, length) instead, so a
guest that faults scattered pages does not pay a whole entry per page. A host
that holds several indices of a window, round a list shorter than k+m, writes
them next to each other in index order, so the window still costs one entry.
Each
stripe also has a small read counter (see below), which the index's budget
counts. A window filled at two different times has an entry for each region it
is in. The index is charged to the host's memory budget, and the cache refuses
writes rather than grow past its share.

### What comes out

Eviction is by pressure only. Nothing about a VM evicts anything: not a stop,
not a migration away, not a delete, not a host restart. A stripe stays on its
host's disk until the limiter needs the space.

The victim is the oldest closed region. Before it is given back, the stripes in
it that were read most since they were written are written again into the open
region. Every other stripe goes. This is FIFO with a second chance, at the
scale of a region. Each stripe keeps a small read counter, and the threshold
for a second chance is set from traces of real reads. Lambda chose a policy
that counts several reads, because a single read bit lets periodic scans keep
pages that nobody else wants. A read by a peer counts like a read by this host.

The second chance is bounded so that eviction always gives space back. At most
half of a victim region is written again. When the cache is over its share, or
the [write budget](#endurance) is spent, nothing is. So eviction has two
causes: making room at the share, where the second chance applies, and
shrinking to a share that fell, where nothing is written again. Navy has no such bound,
and an eviction that rewrites the whole victim frees nothing.

One region is always kept free, for the second chance alone. It counts
inside the share. A second chance
that would need more stops, and the rest of the victim goes. The model checks
that this keeps eviction moving.

A read in flight from a region holds it, as the readers count in
`checkpoint/disk.go` does today, and a region is given back only when its last
reader has finished. A peer's `sendfile` is a reader, with a deadline. The key
check is the backstop, not the guard.

## Pull

A pull no longer holds anything. It becomes a prefetch into the cluster cache.
A VM marked to pull has every window of its checkpoint fetched, in the
background, behind every fault, as today. For each window the pull asks the
window's ranks whether they hold it, and reads from the store and fills only
the windows the cluster lacks. Hosts evict pulled windows like any other.

So the promise changes. Today, once a pull is complete, a fault on a page that
is not resident makes no request of the store while the VM runs. After this
plan, it makes no request of the store while the cluster cache holds the page.
The mark stays with the VM as today, and so does the progress a host reports.

## The disk limiter

One limiter decides how much of the node's disk the host may use, for
everything the host writes:

| User | What it needs | Can it give space back? |
| --- | --- | --- |
| RAM and PMEM spill files | their dirty budget, promised when the pager starts | no: a spilled dirty page is the only copy |
| the ephemeral spill file | every admitted ephemeral disk | no: it is the disk's only copy |
| VMM staging and imported images | what each start and import writes | no, until the process or import ends |
| the disk cache | whatever is left | yes, a region at a time |

A store must never fail for want of disk, so each spill file's whole extent
is allocated (`fallocate`) when its pager starts, as a disk region's space is
allocated when it opens. The limiter counts each spill file at that promise.
A sparse spill file would not be enough, however the limiter counted it: the
space it had not yet used would be only free space on the filesystem, which
another writer on the node can take, and nothing the host does gets it back.
Emptying the cache would not help either, and a guest's dirty page would then
have nowhere to go. `spec/diskcache` found this (B4 in
[spec/bugs.md](../spec/bugs.md)). With the extent allocated, another writer
finds the filesystem full, not the guest. The ephemeral spill file is
allocated the same way.

### Goals

The limiter is given any combination of three goals, as FoundationDB's
Ratekeeper takes its free-space floor:

- a minimum percentage of the filesystem left free;
- a minimum number of bytes left free;
- a maximum number of bytes the host may use.

At least one is set. The strictest wins. The two free-space goals give one
floor, the larger of the bytes and the percentage of the total. The
maximum-used goal caps what the host may hold. `/status` and `/metrics` report
which goal binds, as Ratekeeper reports its limit reason.

The free-space goals measure the whole filesystem, so they account for
anything else on the node that writes to it. The limiter reads the
filesystem's free space (`platform.DiskSpace`) on a timer and before each
region it opens, and smooths the readings over time before it acts on them, so
one odd reading cannot make the cache give space back. From the goals it
computes what the host may hold:

```
floor       = max(goal_free_bytes, goal_free_ratio × smoothed total)
room        = smoothed available + what the host holds now
promises    = spill promises + ephemeral promises + staging
cache       = min(room - floor - reserve, goal_used) - promises
```

The reserve (1 GiB by default) is free space the cache leaves above the floor
for promises not yet made. Two hosts on one node see each other only as free
space the filesystem does not have. Without it, a cache that filled the disk to
the floor would leave a host beside it, whose own cache is empty, no room to
promise a VM's staging, and nothing would tell the full cache to give back.

A promise is counted once, whole, whatever its file has allocated so far, and
"what the host holds" counts each spill file at its promise. Since spill files
are allocated whole, the two agree.

The cache's share does not drop in one step at a mark. It falls gradually as
free space nears the floor, across a band above it: by default a fifth of the
headroom between the floor and the current free space, capped at a configured
number of bytes. This is Ratekeeper's "spring". So the cache gives back a few
regions at a time as the disk fills, not a burst at one moment. One region of
hysteresis keeps a share that hovers at a region's boundary from evicting and
refilling.

When the cache is empty and the rest still does not fit, the host has promised
more than the disk can keep. It then reports itself unready, admits no VM that
would promise more, and says why. It never takes space back from a spill file.

The kernel's page cache that `sendfile` uses is memory, not disk, so the
limiter does not count it.

### Endurance

The limiter also keeps a **write budget**: an average number of bytes per day
the cache may write to the device, with a cap on bursts. CacheLib found that a
cache that admits everything wrote half again its endurance target, and Navy's
main control is a write rate measured from the device's own counters
([research](../docs/research/cachelib-navy-2026-10-02.md)). The limiter reads
the bytes the device has written, and the default budget is a configured
share of the device's rated endurance.

When writes run over budget, the cache drops writes in this order: repairs,
second chances, fills from reads of the store, and last fills from
publications. A dropped write costs a store read later, never a wrong byte.

`SPROUTFS_CACHE_DISK_BYTES` goes. The spill and ephemeral settings stay,
because they are promises the pager makes to its guests, but the limiter checks
them at startup and refuses a configuration that cannot keep them.

## Restarts

Today the cache file is truncated when the host process starts, because a
process restart is a host loss. It is a loss for the VMs, whose unpublished
writes are gone. It is not a loss for the cache. Every stripe on the disk is
named by an identity that will never name other bytes. So the cache survives a
restart, including one after a crash:

- Each closed region's table is read back, in sequence order, and the index
  and the FIFO order are rebuilt from them. Navy lost hit rate when a restart
  reset its order.
- The region that was open at the crash has no table, and is given back.
- A region whose table fails its checksum is scanned by its headers, and given
  back if the scan fails too.
- Each read still checks the key and the checksum, so a region damaged after
  its table was written costs a store read and nothing else.

Navy recovers only after a clean shutdown, because its keys can be overwritten
and deleted. Ours cannot, which is why a crash need not cost the cache.

The cache file's header holds a format version, the region size, the cache's
identity and the deployment it belongs to (the object store, its bucket and its
prefix). A host started against another deployment, or with another format or
region size, gives the whole file back and makes a new identity, because page
identities are unique only within one deployment.

The cache must survive a pod's restart too, so it lives on a disk that outlives
the pod: a `hostPath` directory on the node's SSD (`SPROUTFS_CACHE_DIR`), on
the filesystem the scratch is on, so one limiter measures both. Not a local
`PersistentVolume`: a claim pins the pod to its node, and a pod that moves
should start on the new node with the cache there. Each host takes the first
file in the directory, `cache-0`, `cache-1` and so on, whose lock (`flock`) no
other process holds, so two hosts on a node never share one and a replaced
pod takes its predecessor's back. The spill files and the VMM staging stay in
the `emptyDir`, because a restart is a host loss for them. Hosts that share a
node each set a used goal, so the first cache to fill does not take the whole
disk.

## Correctness

The cache is safe because of four checks on every read, whether the stripe
came from this host's disk or from a peer's:

1. The key stored with each stripe matches the key asked for.
2. Each stripe's checksum matches its bytes.
3. The decoded envelope's own SHA-256 matches its bytes.
4. The cache file names this deployment. A host joins the list of caches only
   for its own deployment.

The first three make every failure of a disk, and every bad answer of a peer, a
miss. The fourth makes a cache from another deployment empty. None of them
depends on how the index was built, which hosts were asked or what list a host
held. So the index, eviction, ranks, repair and restarts can all be wrong
without a guest ever reading wrong bytes. A wrong one costs a store read.

The checks rest on one assumption: an identity never names two contents. The
ownership spec checks `NoCollision`, but only for committed checkpoints. The
cache keeps more than that. It keeps the parts of a publication that never
committed, and it keeps pages after their VM is deleted and across restarts. So
the assumption has to hold for every part ever uploaded, for as long as a disk
keeps it. Two cases need care:

- A publication retried under the same reference must upload the same bytes.
  The store refuses different bytes under an existing part key, and a part is
  filled only once its PUT has succeeded, so no cache sees the bytes of a
  refused attempt.
- A name reused after a delete is a new VM only because its epoch is drawn at
  random. Two draws collide once in 2³¹ creates of one name. The orchestrator
  never reuses a name, so this needs an embedder that reuses names and an
  unlucky draw. A cache that outlives the VM makes that window longer, but not
  more likely.

### A warm cache can hide a reclamation bug

A page that reclamation wrongly deleted is still read from the cache, so a
reclamation bug would show only when the cache turns over, far from its cause.
Lambda alarms on a read of a root it has expired for the same reason. So a host
samples one cache hit in ten thousand and checks with a HEAD that the part
holding the page still exists. A missing part is logged as an error and counted
in `/metrics`, with the page's identity.

### The model

`spec/diskcache/DiskCache.tla` models the hosts' caches, their limiters and the
requests between them. One configuration has four hosts and a 2+1 code, which
keeps the state space small. Another has two hosts and a 1+1 code, the
smallest deployment. A third has three hosts and a 2+2 code, so that stripes
go round the hosts:

- disk regions, opened, closed, evicted and given back, the region kept free,
  and the bounded second chance;
- reads in flight from a region that eviction gives back;
- fills from reads and from publications, including publications that never
  commit and are retried under the same reference;
- ranks, stripes on ranks 1 to k+m and round the hosts when there are fewer,
  fill rights, repair, and reads of k+1 ranks with a hedge to the rest;
- hosts that hold different lists of caches, hosts marked down, and a host that
  restarts, joins or leaves;
- a peer that answers with a wrong stripe;
- a VM deleted and its name created again, with a small epoch space so that a
  collision is reachable;
- the limiter's goal, the spill promises allocated whole (and, as a mutant,
  sparse), the write budget, and another writer on the same filesystem;
- a host restart with torn writes and regions without a table.

Its invariants:

- `NoWrongBytes`: every read returns the bytes the store holds, or held, under
  the identity it asked for, or misses.
- `StripesRanked`: a stripe is only ever on ranks 1 to k+m of its window,
  under some list a host held.
- `SurvivesLosses`: a window whose k+m stripes were all written decodes for a
  reader whose list agrees with the cluster's, after any changes to its ranks
  that took no more than m of its stripes.
- `PromisesKept`: a spill file is never refused space it was promised.
- `GoalKept`: the host's disk use meets the goal, or the host reports itself
  unready.

And one liveness property, `EvictionProgresses`: while the cache is over its
share, the bytes it holds fall. TLC also checks for deadlock.

The mutants put back a read without the key check, a stripe used without its
checksum, a part filled before its PUT succeeded, a keep accepted from a cache
not ranked for the window, a limiter that counts spill files by their
allocation, eviction without its free region, and an unbounded second chance.
Each must fail with the property it names, or deadlock. The epoch collision is
a configuration of its own, which must fail `NoWrongBytes` to show the model
reaches it.

The existing specs need no change. The cache changes no control record, no
selection, no reclamation, no fork and no post-copy. The new page-server
requests serve only published pages, by identity, and never a page no
checkpoint holds, so `NoSilentLoss` in `spec/postcopy` is untouched. A cached
copy is never evidence that a checkpoint exists, so `SelectedReadable` and
`NoDanglingRead` still describe the store.

## The steps

Each step is its own commit, with its tests and its docs.

0. **The model.** `spec/diskcache` and its mutants, and an entry in
   `spec/bugs.md` for anything it finds in the design.
1. **The limiter.** `platform.DiskSpace` for the simulated disk. Space goals,
   spill promises and the cache's share. Each spill file is allocated whole
   when its pager starts (B4). The write budget, with the device's
   byte counter behind a port the simulation can drive. The host builds it,
   logs what it chose, reports it in `/status` and `/metrics`, and refuses a
   configuration that cannot keep its promises.
2. **The log.** Disk regions with allocation at open, stripe headers, tables
   with sequence numbers and the two syncs, the window index, and FIFO
   eviction with a bounded second chance and a free region. A pull stops
   holding regions. The disk is still filled only by pulls and their
   publications, and still holds whole envelopes, so this step changes how the
   disk is laid out and when it is freed, and nothing else.
3. **Restarts.** The header, reading the tables back in order, scanning a
   region whose table is lost, and the deployment check. The host stops
   truncating the cache file.
4. **Ranks.** The cache identity and weight in `/status`, the orchestrator's
   `GET /caches` with the code, each host's copy of the list, and weighted
   rendezvous ranking of windows. A host alone in its list ranks first for
   everything and stores envelopes whole.
5. **The code.** Reed-Solomon striping by `klauspost/reedsolomon`, decoding
   from any k, finding a wrong stripe, stripes going round a list shorter than
   k+m, and the code as a deployment setting, with the table of codes for small
   clusters in `docs/hosting.md`.
6. **Filling the cluster.** Every store read and every publication sends its
   stripes to their ranks, with fill rights, the dropping of duplicate keeps,
   and the bounded rate.
7. **Reading from the cluster.** The stripe read on the peer server (TASK-82),
   asking k+1 ranks chosen by the reader's hash and the rest after the
   adaptive delay under its budget, taking any k indices, the bound and its
   token bucket, marking hosts down, telling a host to drop a wrong stripe,
   repair of indices no rank holds, the per-peer bounds, and the sampled HEAD
   check. Before it is built, the stripe benchmark gains this read pattern and
   its full-load pass runs again on GCE.
8. **Serving without a copy.** First, measure on GCE what a plain copy through
   a bounded buffer costs a host serving stripes at full rate. If it matters:
   buffered I/O with dropped reads, the transport's file-range send,
   `sendfile` in the TCP adapter, and no CRC32C on stripe replies.
9. **Pull as a prefetch.** The pull asks the ranks and fills what the cluster
   lacks.
10. **The deployment.** The host manifest puts the cache in a `hostPath`
    directory on the node's SSD, and sets space goals and a write budget
    instead of a cap. A setting turns the cluster cache on for a share of windows, by
    a hash of the window, so it can be rolled out gradually, as mcrouter's
    shadowing does. The setting was built with step 5
    (`SPROUTFS_CACHE_CLUSTER_PERCENT`), off by default, so that no deployment
    reads less from its own disk before step 7; this step raises it. `docs/architecture.md`, `docs/hosting.md`,
    `docs/volumes.md`, `docs/vm-memory.md`, `docs/migration.md` and
    `docs/context.md` describe the cache as it is.

## How it is proved

Each property has a test that states it in its own words.

- **Stopping does not evict.** In the simulation, a VM is suspended, stopped
  and opened again on the same host after its arena pages have been taken by
  other VMs. Its faults make no request of the store.
- **Tiers evict independently.** The arena is filled until the stopped VM's
  idle pages are evicted. The disks still hold them. Then the limiter's goal is
  tightened until the disks give them back, and the VM reads the store.
- **One disk cache for the whole host.** Two VMs of two tenants fork the same
  public template on one host. The second VM's faults make no request of the
  store, and the host's disk holds each of its stripes once.
- **Then from the cluster's disk cache.** In a simulated cluster of six hosts
  with 4+2, and again in one of two hosts with 1+1, a VM is suspended on one
  host and opened on another. Its faults make no request of the store.
- **The hosts' disk caches form one cache.** Six hosts read the same
  checkpoint one after another. The store serves each window once, and each
  window is filled once.
- **A cached page survives losing a host.** In the six-host cluster, two hosts
  are lost, then one is added. Every window read before still decodes, and
  repair restores its missing stripes on the next read. In the two-host
  cluster, one host is lost and every window still reads from the other.
- **A drain does not change the code.** The six-host cluster drains to five.
  Its code stays 4+2, its stripes go round five hosts, and every window still
  decodes with one more host lost.
- **A hot page spreads its load.** All six hosts read one window at once. Each
  holder sends at most one stripe to each reader, no holder sends a whole
  window, and the requests spread over all six holders.
- **A slow host does not slow reads.** One holder stops answering. Reads take
  the time of the other holders, or the hedge delay where the stalled holder
  was among the first k+1 asked. Second requests stay within their budget. After three timeouts every reader marks it
  down and stops asking it. A probe clears the mark when it answers again.
- **Serving a peer copies nothing into memory.** On Linux, a host serves a run
  of stripes over TCP. The cache file is read by `sendfile` alone, and no read
  of it reaches user space. Under TLS, the same run is served through the
  bounded buffer.
- **The disk limiter follows its goals.** A simulated filesystem whose free
  space drifts on its own, as FoundationDB's simulator drifts it, is filled
  from outside the host. Under each combination of goals, the strictest binds
  and is reported, and the cache gives regions back gradually across the band
  until the floor holds. A spill
  file's promise is never taken back. A write budget that runs out drops
  repairs and second chances first.
- **Restarts.** A host is restarted. Its cache answers reads from before the
  restart, its own and its peers', without a request of the store. A region
  torn by the crash is given back. A region whose table is lost is read back
  by scanning. A cache file of another deployment is emptied.
- **An empty cluster at full load.** Every host's cache is emptied and every
  VM restored at once, at the host's maximum concurrency. The store's load
  rises and falls back, and the cache fills, as Lambda tests for. The token
  bucket keeps the reads of the store that the bound starts within its share.
- **No wrong bytes.** A fault-injection campaign corrupts the index, reuses a
  region under a read in flight, tears writes and has peers answer with the
  wrong stripe. Every read returns the right bytes or reads the store. A
  negative test removes the key check, and the campaign must fail.

Then the measurements on GCE, against a real bucket, on six hosts in one zone.
Each case runs in at least three rounds, each round in its own order, and the
report gives the bytes each host served and the spread over the rounds: one
slow host for a few seconds moves one window's p99 thirty times
([measurement](../docs/measurements/gce-stripes-tail-2026-10-03.md)).

1. First, before step 5 is built: one 350 KB read from one host against 4+1
   and 4+2 reads of 90 KB stripes, at the median and the tail. Each runs with
   the hosts idle, serving at full rate, with one host drained, and with one
   host drained and another slow. This checks Lambda's result on our hosts,
   which also run guests. If 4+2 does not cut the tail of the drained-and-slow
   case against 4+1, the default becomes 4+1.
   `scripts/bench-stripes-gce.sh` runs it with `cmd/sproutfs-stripebench`.
2. Before step 8: the cost of serving stripes by a plain copy, in CPU and
   memory bandwidth.
3. After step 7: an 8 GiB guest suspended and restored with its pages in
   memory, in the cluster cache only, and in the store only, then again with
   one host lost during the restore. The time to read its memory back in each
   case goes in `docs/measurements/`, with the serving hosts' CPU and memory.
   Results are reported as the shape of the distribution, not only
   percentiles, because a cache's latency has a mode for each tier.

Those numbers replace the estimates at the top of this plan.

## Not in this plan

- Placing a restarted VM on the host that still holds its pages in memory.
- A tier of whole local copies. The host reports how often it reads the same
  window from the cluster, and that number decides whether one is worth
  adding.
- Having a host read the store for a reader on a miss. Fill rights already
  keep a cold burst to one fill per window, and the reads of the store that a
  burst makes are no slower than the miss itself.
- Purging a deleted tenant's pages from the disks at once. They are never read
  again, because nothing names them, and they leave as the disks turn over.
  The owner accepted this on 2026-10-02.
