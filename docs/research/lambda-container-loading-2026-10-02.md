# Lambda's container loading, and the disk cache plan — 2026-10-02

One paper and its author's blog posts, checked against
[the disk cache plan](../../plans/disk-cache-2026-10-02.md).

## Sources

- **[P]** Marc Brooker, Mike Danilov, Chris Greenwood, Phil Piwonka.
  *On-demand Container Loading in AWS Lambda.* USENIX ATC 2023, pages
  315–328. <https://www.usenix.org/system/files/atc23-brooker.pdf>. The arXiv
  copy (<https://arxiv.org/abs/2305.13162>) has the same text and numbers.
  Citations give the section and the proceedings page.
- **[B-load]** Brooker, *Container Loading in AWS Lambda*, 2023-05-23.
  <https://brooker.co.za/blog/2023/05/23/snapshot-loading.html>
- **[B-ec]** Brooker, *Erasure Coding versus Tail Latency*, 2023-01-06.
  <https://brooker.co.za/blog/2023/01/06/erasure.html>
- **[B-modes]** Brooker, *Caches, Modes, and Unstable Systems*, 2021-08-27.
  <https://brooker.co.za/blog/2021/08/27/caches.html>
- **[B-snap]** Brooker, *Lambda Snapstart, and snapshots as a tool for system
  builders*, 2022-11-29. <https://brooker.co.za/blog/2022/11/29/snapstart.html>
- **[B-sieve]** Brooker, *Why Aren't We SIEVE-ing?*, 2023-12-15.
  <https://brooker.co.za/blog/2023/12/15/sieve.html>

"What the paper says" comes from these sources. "Lessons" is our reasoning,
marked where it goes past them. Lambda calls its 512 KiB unit a chunk; the
word is used here only for Lambda.

## What the paper says

### The system

Lambda flattens a container image into one ext4 image, splits it into fixed
512 KiB chunks, encrypts each chunk, and stores it in S3 under a name derived
from its content [P §2, p. 317; §3.1, p. 319]. A Firecracker guest reads a
block device. A per-function agent serves it from a per-worker cache, then an
AZ-level cache, then S3 [P §2.1, pp. 317–318; §4, p. 320]. Guest writes go to
a local copy-on-write overlay, so every cached chunk stays immutable and
shareable [P §2.1, p. 318]. The same system stores and loads SnapStart's
memory snapshots [P §7.1, p. 325].

### The three tiers

- **Worker-local cache.** One per worker, in memory for very recent chunks and
  on local disk for older ones, several orders of magnitude smaller than the
  AZ cache [B-load].
- **AZ-level cache.** A fleet of dedicated cache servers speaking HTTP/2. Each
  has a memory tier for hot chunks and a flash tier for colder ones
  [P §4, p. 320]. The memory tier is about 10 % of the cache's size
  [P §5.1, p. 323].
- **Origin.** S3 gives durability, so the caches need none
  [P §4.1, p. 320; B-load].

**Finding data.** A worker maps a chunk name to cache servers with a variant of
consistent hashing that adds load spreading in the style of consistent hashing
with bounded loads (Chen et al.) [P §4, p. 320]. Content-derived names remove
the need for a central index [P §2, p. 317].

**Filling.** On a miss the worker reads S3, then uploads the chunk's
erasure-coded stripes to the cache [P §4, p. 320; §4.1, p. 321]. The cache
does not fetch from the origin for a reader.

**Hit rates.** Over one week in one large region [P §4, p. 320, Fig. 7–8]:

| Tier | Share of chunks loaded | Hit rate |
| --- | --- | --- |
| Worker-local | 67 % (median) | median 67 %, 10th percentile 65 % |
| AZ cache | 32 % | median 99.9 %, 10th percentile 99.4 % |
| S3 | 0.06 % | — |

The low tail of the AZ hit rate comes from traffic spikes to newly created
functions. The authors were evaluating priming the AZ cache when chunks are
created [P §4, p. 320].

**Latency.**

| Measurement | Median | Tail | Source |
| --- | --- | --- | --- |
| AZ cache hit, seen by worker | 550 µs | p99.9 3.7 ms | P §4, p. 320 |
| S3 fetch, seen by worker | 36 ms | p99.9 175 ms | P §4, p. 320 |
| AZ cache hit, another deployment | 500 µs | p99.9 4 ms | P §4.1, p. 321 |
| Cache server GET, server side | under 50 µs | — | P §5.1, p. 323, Fig. 10 |
| Cache server PUT, server side | 125 µs | p99 under 300 µs, p99.99 413 µs | P §5.1, p. 323 |
| Read at the agent, local hit | under 100 µs (mode) | — | P §5.1, p. 324, Fig. 11 |
| Read at the agent, AZ hit incl. decryption | about 2.75 ms (mode) | — | P §5.1, p. 324, Fig. 11 |

Every request and response is one 512 KiB chunk [P §5.1, p. 323]. The cache
servers saturate 50 Gb/s NICs with user-space HTTP/2, so the team kept HTTP
[B-load, footnote 2; P §5.1, p. 323].

### Erasure coding in the AZ cache

**Why.** A cache with one copy of each chunk failed three needs
[P §4.1, pp. 320–321]:

1. *Tail latency.* A start fetches many chunks, so one slow server hurts many
   starts. At 1000 chunks, 63 % of starts see the cache's p99.9 latency.
2. *Hit rate.* A server that fails or is being deployed takes its chunks' hits
   with it.
3. *Throughput.* One server's bandwidth bounds one chunk.

Tail latency was the largest concern; slowness and partial failure are harder
to debug than outright failure [P §4.1, p. 321].

**Why not replication.** Replication with redundant requests solves all three
but multiplies cost by the replication factor, which matters in a cache that
is mostly memory [P §4.1, p. 321].

**The code.** Production uses a 4-of-5 code, following EC-Cache. A reader asks
for more stripes than it needs and rebuilds the chunk from the first four
[P §4.1, p. 321]. The paper's parity routine is a byte-wise XOR
[P §5, p. 323, Listing 2], which fits a single-parity code, though the paper
does not name the code. Vectorized parity is about 5x faster than
8-bytes-at-a-time and 10x faster than byte-at-a-time [P §5, p. 322].

**Costs.** The paper gives 25 % storage overhead and 25 % more requests,
relative to splitting the chunk four ways [P §4.1, p. 321]. The blog counts
the same scheme as five requests per fetch and 20 % overhead in storage and
bandwidth [B-ec].

**Effect.** Figure 9 compares 4-of-5 with a hypothetical 4-of-4 split, using
production latencies [P §4.1, p. 321; Fig. 9, p. 322]. The tail falls sharply,
and the median falls by about 20 % [B-ec]. The baseline, 4-of-4, waits for the
slowest of four servers; the paper does not compare against one whole chunk
from one server.

**Node loss.** With M − k ≥ 1, one lost or deploying node costs no hit rate
[P §4.1, p. 321; B-load]. The blog's example: a 20-node cache with one copy
loses 5 % of its data when a node fails, which at a 99 % hit rate is more than
five times the misses [B-load]. Deployments can go one box at a time without
moving data first [B-load].

**Retries and constant work.** The paper says retries, the usual fix for
failed nodes, are known to cause metastable failures. Erasure coding does the
same work whether nodes succeed or fail, which the authors call constant work
[P §4.1, p. 321].

### Hedging

The paper does not hedge; it uses the erasure code [P §4.1, p. 321]. The blog
compares the two [B-ec]:

- A hedge is one request or two. It mostly helps the far tail.
- Dean and Barroso's deferred hedge waits until the p95 latency, hedging about
  5 % of requests in normal operation.
- If latency rises everywhere at once (load, failures, an empty cache), every
  request passes the p95 and the hedge doubles traffic. So a hedge must be
  limited by a token bucket to the expected share, such as 5 % [B-ec,
  footnote 1].

### Herds and metastability

The end-to-end hit rate usually exceeds 99.8 %. If the cache empties or the
hit rate drops, S3 may see up to 500 times its normal load. S3 copes, but the
higher latency raises concurrency (Little's law), which demands more sandboxes
and changes the working set, and the system may be unable to refill its cache
[P §4.2, pp. 321–322].

Mitigations [P §4.2, p. 322]:

- Lambda limits concurrency. When starts slow down past the limit, new starts
  are refused until running ones finish.
- The team tests cold starts from an empty cache at maximum concurrency.

The blog adds that a cache needs a feedback loop, such as back pressure or a
concurrency limit, or it can stay empty. Load tests rarely find this, because
the dangerous load is a different key distribution, not more load [B-modes].

### Deduplication and convergent encryption

- **Chunk size.** 512 KiB. Smaller chunks dedupe better and suit random reads.
  Larger chunks shrink metadata, need fewer requests, and give read-ahead. The
  authors expect the size to change [P §2, p. 317].
- **Deterministic flattening.** A serial ext4 writer with fixed timestamps makes
  identical files give identical blocks [P §2, p. 317].
- **Dedup rates.** About 80 % of new functions add no unique chunk. Of the rest,
  the mean upload is 4.3 % unique and the median 2.5 %. All-zero chunks are
  dropped at creation. Storage falls by up to 23x, and another 5x from exact
  re-uploads [P §3, p. 318].
- **Convergent encryption.** Each chunk's key is derived from the SHA-256 of
  its plaintext plus extra metadata. The chunk is encrypted with AES-CTR and a
  zero IV, named by the hash of its ciphertext, and uploaded only if no chunk
  of that name exists [P §3.1, p. 319].
- **Manifest.** Lists each chunk's offset, key and hash. Only the key table is
  encrypted, with a per-customer KMS key; the whole manifest is authenticated
  with AES-GCM, so garbage collection can read chunk lists without keys. A
  manifest is under 3 MiB for a 16 GiB image [P §3.1, p. 319].
- **Integrity.** Workers check each chunk against the manifest
  [P §3.1, p. 319]. SHA-256 is used instead of an AEAD tag because AEAD modes
  do not resist collisions by someone who knows the key [P §3.1, footnote 2].
- **No compression.** Decompression adds latency and hurts random access for
  little gain at their bandwidth, and compressing before encrypting would leak
  plaintext through sizes [P §3.2, p. 319].

### Blast radius

A popular chunk shared by many functions is a single point of wide failure:
gray cache failures, operational mistakes, GC bugs, corruption, and hot spots.
The cryptography detects corruption but does not correct it, so a corrupt
chunk becomes unavailable [P §3.3, p. 319].

The fix is a **salt** in the key derivation, varied over time, with
popularity, and by placement, such as per AZ. Chunks with different salts do
not dedupe, so the most popular content exists in several copies. Rotating the
salt trades dedup for blast radius, and only the chunk creator knows about it
[P §3.3, p. 319; B-load].

### Garbage collection

There is no central reference count. Data lives in **roots**. A new root
becomes active. The old one is retired and serves only reads while live
manifests and their chunks are copied forward. A retired root then becomes
**expired**: reads still work, but any read raises an alarm and stops further
deletion. Only then is it deleted [P §3.4, pp. 319–320, Fig. 6]. The active
root's identifier is part of the salt [P §3.4, p. 320].

### Eviction and sizing

- **LRU-k**, because LRU and FIFO are not scan resistant [P §4.3, p. 322]. The
  scans are periodic cron functions: many functions, each in one sandbox,
  starting together on weekly, daily and hourly spikes, whose chunks would
  evict the hot set [P §4.3, p. 322].
- **Sizing.** At least the five-minute-rule size, where holding a chunk costs
  what fetching it from S3 costs, and larger if needed for a hit-rate goal
  [P §4.3, p. 322].
- In a later post, Brooker notes that FIFO-reinsertion and SIEVE resist scans
  poorly on block workloads, and that a saturating counter (his SIEVE-k) helps
  only on some traces [B-sieve].

### Tenants

- A worker should reach only the data of functions placed on it. Manifest keys
  are released by KMS only to those workers [P §3, p. 319; §3.1, p. 319].
- Chunks dedupe across customers with no shared key [P §3.1, p. 319]. Cache
  servers hold only ciphertext.
- Shared worker components talk to the guest only over virtio-blk and
  virtio-net. Filesystem work stays in the guest, to keep the shared attack
  surface small [P §1.1, p. 316; §2, p. 316].

### Other details

- **Multimodal latency.** Agent reads have three modes: local, AZ, S3. A small
  change in mode frequencies moves the mean a lot. Percentiles hide the modes;
  empirical CDFs show them [P §5.1, p. 324].
- **FUSE.** FUSE in front of virtio-blk needs four threads per read and adds
  jitter under load. Lambda is moving to userfaultfd and mmap
  [P §5.2, p. 324].
- **Provenance sharing.** For SnapStart, Brooker describes sharing pages by
  where they came from in a tree of snapshots, not by scanning content
  [B-snap].

## Our numbers beside theirs

| | Lambda | sproutfs (plan) |
| --- | --- | --- |
| Unit on the wire | 512 KiB chunk, uncompressed | 2 MiB window, about 300 KiB compressed |
| Origin read | 36 ms median, 175 ms p99.9 (512 KiB, S3) | 40–58 ms mean (2 MiB page, GCS, measured) |
| Peer cache read | 550 µs median, 3.7 ms p99.9 at the worker | 1.5–3 ms (estimate, incl. decode) |
| Peer read incl. decode | about 2.75 ms (incl. decryption) | 1.5–3 ms (estimate) |
| Local read | under 100 µs | 1–2.5 ms (estimate, incl. decode) |
| Peer vs origin | about 65x at the median, 47x at p99.9 | about 20x (incl. decode) |

Our origin cost matches theirs. Our peer estimate matches their AZ hit once
decryption is counted, which supports the plan's view that decode dominates.
Their server answers in under 50 µs, so the network and server add about half a
millisecond, as the plan assumes. Our local estimate is far above theirs; their
local mode is likely served from memory, ours is a disk read plus a decode.


## Lessons for the plan

Section names refer to [the plan](../../plans/disk-cache-2026-10-02.md).

### Where the plan already matches

- Immutable names: Lambda's by content, ours by the publishing checkpoint
  ([What stays the same]; P §2, p. 317). Our sharing by provenance is what
  Brooker describes for SnapStart [B-snap].
- End-to-end checks against the manifest [P §3.1, p. 319]; ours are the key
  and the envelope's SHA-256 ([Correctness]).
- No directory: hashing maps a name to its cache [P §4, p. 320]
  ([Who owns a page]).
- Readers fill the cache, and Lambda does not coalesce misses at the cache
  either [P §4, p. 320] ([Filling the owner]). This supports deferring "owner
  reads the store for a reader".
- Priming at creation, which Lambda was evaluating [P §4, p. 320]; we fill
  owners at publication ([Filling the owner], item 2).
- Memory in front of flash [P §5.1, p. 323]; ours is the page cache
  ([Serving without a copy]).
- userfaultfd instead of FUSE [P §5.2, p. 324].
- A cache that survives a pod restart ([Restarts]), where Lambda's AZ cache
  can empty after power loss [P §4.2, p. 321].

### 1. A dead or slow owner costs more than the plan says

[Reading a page] says a slow owner "costs the bound, not a stall". It costs the
bound **plus a store read**, about 40–58 ms, because a cold window has no
other copy: step 4's rank 2 holds nothing, so the hedge target is twenty times
slower than what it hedges.

A full restore of an 8 GiB guest reads 4096 windows, so 98 % of restores hit
the peer tier's p99.9 at least once, about four times on average
(1 − 0.999⁴⁰⁹⁶; P §4.1, p. 321 makes the same argument for 1000 chunks).

**Change:** fix the sentence, and adopt lesson 2 or 3, since a hedge needs a
fast target.

### 2. Measure erasure coding against owner plus hot copies

Lambda chose a 4-of-5 code for our three problems: tail latency, hit rate on
node loss, and a hot chunk's bandwidth [P §4.1, pp. 320–321]. The plan answers
them separately with a store hedge, a miss on owner loss, and reactive copies.

For erasure coding:

- **Our cache servers run guests**, so gray slowness of an owner, Lambda's
  largest concern [P §4.1, p. 321], is more likely than in a dedicated fleet.
- **Our hosts churn** with the VM fleet. One lost host of N turns 1/N of cold
  hits into store reads; with N = 10 and a 99 % hit rate, misses rise about
  elevenfold (B-load's arithmetic).
- **Bursts need no detection.** Every window is on five hosts from its first
  read; the hot threshold, k and the per-window host count go away.
- **Capacity:** 1.25x for everything, against 1x for cold and 3x for hot
  windows in the plan.

Against:

- **The paper's gain is against 4-of-4** [P Fig. 9]. One read from one owner
  has one server's tail, not the slowest of four. Our gain must be measured.
- **Five requests per window** against our two, on CPU shared with guests.
- **The unit must be fixed bytes.** A 2 MiB-page window or a segment is one
  envelope and stripes cleanly. A 4 KiB-page window is filled member by member
  at different times and does not.
- **It needs five caches.** The test cluster has three; 2-of-3 costs 1.5x.
- Sendfile still works: each host sends its stripe from disk, and the reader
  rebuilds, then checks the key and the SHA-256.

**Change:** add to the GCE measurement a 4-of-5 (or 2-of-3) layout for
2 MiB-page windows and segments. Compare per-window read latency (p50, p99,
p99.9) and restore time against owner-plus-copies, with one host loaded by
guests and one killed mid-restore. Until then, make a window's placement in
the read path a set of ranks and a coding, so 1-of-1, copies and k-of-M are
configurations.

### 3. If the plan keeps a single owner, budget the hedge

**Change:** in [Reading a page], the "ask the store too" hedge draws from a
per-host token bucket sized to the expected hedge rate [B-ec, footnote 1];
past the budget, the reader waits for the peer. Set the bound from the
measured peer p99, not the round trip. Hedge to a copy before the store.

### 4. Copies come too late for a fork burst

The threshold is four hosts in a minute ([Copies of hot windows]). A fork
fan-out of a hundred VMs arrives within seconds, so the owner serves the first
wave alone. Lambda's worst hit rates come from such spikes [P §4, p. 320].

**Change:** copy proactively where fan-out is known: a template's checkpoints
and a fork point with children, when published or forked. The orchestrator
knows how many children it places. Erasure coding would make this
unnecessary.

### 5. Plan for an empty cluster cache

Our triggers for an empty or cold cache [P §4.2, pp. 321–322; B-modes]: a new
deployment, a cache header mismatch, a mass node replacement, or a shift in
which checkpoints are read.

**Change:** add to [How it is proved] a restore at maximum concurrency from an
empty cluster cache, as Lambda tests [P §4.2, p. 322]. The orchestrator should
limit concurrent restores per host and refuse past it, the feedback loop
Lambda relies on [P §4.2, p. 322; B-modes]. Fills must stay droppable under
load ([Filling the owner]).

### 6. A cold burst sends many keeps of one window

When a hundred hosts miss one window together, each sends a keep to the
owner; the per-host keep rate does not bound what one owner receives. Lambda
has the same pattern [P §4, p. 320] and does not discuss it; this is our
inference.

**Change:** a keep names its keys before its bytes, and the owner refuses a
keep for a key it holds or is already writing.

### 7. A bad copy at the owner must not keep serving

A bad answer is a miss for the reader ([Reading a page]), but the owner sends
envelopes without decoding them and never learns, so every later reader pays
the bound and a store read; for a public template's page, every fork. Lambda's
salt exists to contain this kind of wide failure [P §3.3, p. 319].

**Change:** a reader that rejects an answer tells the owner, which checks that
envelope and forgets it if it fails. The owner also checks an envelope's
SHA-256 when the second chance rewrites it. Copies or erasure coding remove
the single point for hot windows; no salt is needed, since copies give the
same spread.

### 8. The cache can hide a reclamation bug

A warm cache keeps serving a page after the store lost it, so a reclamation
bug that deletes a live part shows up only on eviction, long after the cause.
Lambda's expired state, where any read alarms and stops deletion, catches such
bugs early [P §3.4, p. 320]. The connection to our cache is our inference.

**Change:** at a low sampled rate, a cache hit also checks that the part still
exists in the store (a HEAD) and alarms if not; or `sproutfsctl check` covers
what the caches hold. [Correctness] should say a cached copy is not evidence
that an object still exists.

### 9. Reconsider "no local tier" with data

The plan rejects local copies because they save about half a millisecond
([One cache for the cluster]). Lambda's much smaller local cache serves 67 %
of chunk loads [P §4, p. 320; B-load], taking two thirds of the load off the
network, the cache servers and every remote tail. Our memory tier and the
pager's arena play part of that role; Lambda's local tier also has a disk part
[B-load].

**Change:** in the GCE measurement, count reads at the owner by whether the
reader read that window before. If rereads are a large share, give the local
disk a small slice, admitted on a second local read.

### 10. Choose the second-chance rule from traces

The plan's eviction rewrites any envelope read once since it was written
([What comes out]). Our version of Lambda's cron herd is many idle sandboxes
resumed once each; Lambda chose LRU-k for that pattern [P §4.3, p. 322], and
Brooker found one-bit FIFO variants weak on block traces [B-sieve].

**Change:** keep a small saturating counter per window, and pick the threshold
by replaying a (window, host, time) trace from the GCE run. Report the rewrite
rate, since every second chance is an SSD write.

### 11. Is zero-copy serving worth step 8?

Lambda's servers saturate 50 Gb/s NICs from user space [B-load, footnote 2;
P §5.1, p. 323]. Step 8 gives up the reply CRC32C, forces buffered I/O and
adds a file-range send to every transport. Our owners share CPU with guests,
so the copy may matter more, but that is unproven.

**Change:** measure owner CPU per GB served with an ordinary bounded-buffer
copy first, and do step 8 only if that cost is material.

### 12. Measure modes, not percentiles

**Change:** following [P §5.1, p. 324] and Lambda's per-tier hit rates in
one-minute buckets [P §4, p. 320], `/metrics` reports per tier (memory, arena,
local disk, owner, copy, store) the share of reads and a latency histogram,
and the GCE report shows eCDFs and hit rate per tier over time.

### 13. Ownership load

Lambda adds bounded-load spreading to consistent hashing [P §4, p. 320].
Rendezvous balances on average, but with few hosts one may own much more than
its share.

**Change:** report each cache's owned bytes and requests served, and consider
bounded loads only if the spread is wide.

### 14. Tenancy

Under the plan, every host's disk holds plaintext envelopes of tenants that
never ran there, which equals today's store access ([Tenants]). Lambda gives a
worker keys only for functions placed on it, and cache servers hold only
ciphertext [P §3.1, p. 319]. If hosts ever get credentials scoped to their
own VMs, the cache protocol becomes the gap; [Tenants] should state this
limit.

Our cross-tenant sharing is through public templates, by provenance, so it
needs no convergent encryption [P §3.1]. We compress but do not encrypt, so
the size side channel Lambda avoids [P §3.2, p. 319] does not apply today.
