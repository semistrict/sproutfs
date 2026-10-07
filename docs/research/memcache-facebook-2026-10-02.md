# Memcache at Facebook and mcrouter: lessons for the disk cache — 2026-10-02

"Scaling Memcache at Facebook" and the source of mcrouter, read for the
[distributed disk cache plan](../../plans/disk-cache-2026-10-02.md).

## Sources

- **[P]** R. Nishtala et al., "Scaling Memcache at Facebook", NSDI 2013.
  <https://www.usenix.org/system/files/conference/nsdi13/nsdi13-final170_update.pdf>.
  Cited by section and proceedings page (385–398).
- **[M]** mcrouter, <https://github.com/facebook/mcrouter>, cloned to
  `~/src/mcrouter` at commit `71319f0e01b351b8507e78988f174640f4f0555c`
  (2026-10-02). Cited as `path:line` under `mcrouter/`. A permalink is
  `https://github.com/facebook/mcrouter/blob/71319f0e01b351b8507e78988f174640f4f0555c/mcrouter/<path>#L<line>`.
- **[W]** The mcrouter wiki, <https://github.com/facebook/mcrouter/wiki>, at
  wiki commit `855a79c` (2022-02-17). Cited by page name. The wiki is older than
  the code; where they disagree, this note says so and follows the code.

**License.** mcrouter is MIT licensed (`LICENSE` line 1 at the commit above;
the README badge says the same). Nothing here copies its code. The server side
of leases lives in Facebook's memcached fork, not in mcrouter, so lease
details come from the paper.

## What the paper says

### The setting

Facebook's memcache is a look-aside cache in front of databases. A web server
reads memcache, and on a miss reads the database and sets the value back
[P §3.2, p.388]. Values change, so most of the paper is about stale data. Ours
never changes, so only the load and failure parts apply.

Clients hold the map of servers, which a configuration system updates. Servers
never talk to each other [P §3.1, p.387]. The paper's lessons include keeping
logic in a stateless client, and supporting gradual rollout and rollback
[P §9, p.396].

### Leases

A lease solves stale sets and thundering herds [P §3.2.1, p.388].

- On a miss, the server gives the client a 64-bit token bound to the key,
  which the client presents when it sets the value. A delete of the key voids
  the token, so a late set of an old value is refused.
- The server hands out at most one token per key every 10 seconds by default.
  Any other client that misses in that time is told to wait briefly and retry.
  The holder usually sets the value within a few milliseconds, so the retry
  usually hits.
- For keys prone to herds, leases cut the peak database query rate from 17K/s
  to 1.3K/s over a week [P §3.2.1, p.388].
- A get may also return a recently deleted value marked stale, for clients that
  can use it [P §3.2.1, p.388–389].

In mcrouter, a lease get that returns token 1 is a "hot miss", and the client
retries with exponential backoff: 2 ms first, at most 500 ms, at most 10
retries (`routes/CarbonLookasideRoute.h:35-40`,
`routes/CarbonLookasideRoute.h:271-279`). After a failover, a lease set is
sent back to the server that issued the token (`routes/FailoverRoute.h:131-146`).

For immutable data only the herd part applies: one filler per key per
interval, the rest wait. No set can be stale.

### Pools

Keys are split into pools by access pattern, because high-churn keys nobody
read again evicted low-churn keys that were still useful [P §3.2.2, p.389].

### Replication within a pool

Some pools replicate every key to several servers [P §3.2.3, p.389]: when
clients fetch many keys at once, the whole set fits on one or two servers, and
the request rate exceeds one server. When every request asks for many keys,
splitting the keys does not lower the request rate per server, but a replica
does. Each client picks its replica by its own IP address.

### Gutter

When a few servers fail, automated repair takes up to a few minutes, and the
failed servers' keys fall on the database, which can cascade [P §3.3, p.389].

- Gutter is a small set of spare servers, about 1 % of a cluster's memcached
  servers [P §3.3, p.389].
- A client that gets no reply to a get asks Gutter. On a Gutter miss it reads
  the database and sets the value into Gutter [P §3.3, p.389].
- Gutter entries expire quickly, so Gutter needs no invalidations
  [P §3.3, p.389–390]. The paper gives no number. mcrouter's
  `FailoverWithExptimeRoute` rewrites the TTL of sets that fail over, 60 s by
  default (`routes/FailoverWithExptimeRouteFactory.h:74`; [W List-of-Route-Handles]).
- They chose Gutter over rehashing keys onto the remaining servers because a
  single key can be 20 % of one server's requests and could overload the
  server that inherits it [P §3.3, p.390].
- Results: client-visible failures drop by 99 %, 10–25 % of failures become
  hits each day, and after a server dies Gutter's hit rate passes 35 % within 4
  minutes and often nears 50 % [P §3.3, p.390].

Also:

- A get error counts as a miss, and the web server then does not set the value
  back, to add no load to a possibly overloaded server or network
  [P §3.1, p.387].
- mcrouter's `hash_salt` spreads load failed over from one host over a backup
  pool instead of one backup host [W Pools].

### Regional pools

Several frontend clusters in a region can share one pool instead of each
keeping a replica [P §4.2, p.391], for keys that are large and rarely read.
The decision is manual, by access rate, data set size and the number of
distinct users of an item [P §4.2, p.391]. Table 1 contrasts a family left
replicated (median 30 users, 3.26 M gets/s) with one moved to a regional pool
(median 1 user, 458 K gets/s).

### Cold cluster warmup

A new or repaired cluster starts with empty caches. Clients there read a miss
from a warm cluster instead of the database, and add the value to the cold
cluster [P §4.3, p.391]. This brings a cluster to full capacity in a few hours
instead of a few days. Most of the section handles a race with invalidations,
using a two-second hold-off on deletes, which cannot happen to immutable
pages. Warmup is turned off once the hit rate settles.

### Restarts

A memcached server reaches 90 % of its peak hit rate within a few hours, so
upgrading a set of them took over 12 hours to keep database load in check.
They moved the cache into System V shared memory so it survives a software
upgrade [P §6.4, p.394].

### Incast and batching

- A client sends independent fetches together, in batches of 24 keys on
  average [P §3.1, p.387].
- mcrouter shares one TCP connection per destination per thread among all
  requests [P §3.1, p.387; W Features].
- Each client limits its outstanding requests with a sliding window across all
  destinations, grown slowly on success and shrunk when a request goes
  unanswered [P §3.1, p.387–388]. Too small a window serializes work; too
  large causes incast and errors, which push load onto the database
  (Figure 4).
- mcrouter also has a per-destination cap on requests in flight, off by default
  (`mcrouter_options_list.h:286-302`).

## What mcrouter does

### Hashing

- **Ch3** is the default: `furc_hash(key, n)` returns an index into the pool's
  ordered server list (`lib/Ch3HashFunc.h:21-32`; [W Pools]). It is consistent
  in n: growing a pool from 11 to 12 servers moves 1/12 of the keys
  (`lib/fbi/hash.h:19-30`). It is not consistent in a server's identity:
  removing a server from the middle of the list shifts the index of every
  server after it.
- **WeightedCh3** gives each server a weight in [0, 1]. It hashes the key to an
  index and accepts it with probability equal to its weight, or retries with a
  salt (`lib/WeightedCh3HashFunc.h:16-54`). The header says it is only mostly
  consistent when n changes and can give up when weights are skewed. At an
  average weight of 0.25 it needs about 16 tries for a 1 % failure rate.
- **Rendezvous** and **WeightedRendezvous** score each server by a hash of its
  tag and the key. Weighted scores are `w / -ln(U)` with U the hash mapped to
  (0, 1) (`lib/RendezvousHashFunc.cpp:30-66`,
  `lib/WeightedRendezvousHashFunc.cpp:99-101`). A server's tag is a string in
  the configuration, not its position (`routes/RendezvousRouteHelpers.cpp:16-40`).
- **FailoverRendezvousPolicy** walks the servers in rendezvous rank order on
  failover, skipping the primary (`routes/FailoverPolicy.h:603-720`). This is
  the plan's "rank 2 on failure".

### Failover and its variants

- **FailoverRoute** sends to the first child, and on an error to the next. A
  miss is not an error ([W List-of-Route-Handles];
  `routes/FailoverRoute.h:55-59`). Which errors fail over is configurable per
  operation. A token bucket can cap failovers to a fraction of requests. A TKO
  reply skips the bucket and does not count as a try, because no work was done
  (`routes/FailoverRoute.h:221-230`).
- **MissFailoverRoute** tries children in order until the first hit
  (`routes/MissFailoverRoute.h:29-34`).
- **LatestRoute** builds a FailoverRoute over at most `failover_count` children
  (default 5), chosen by hashing the client's host ID, not the key
  (`routes/LatestRoute.h:27-66`), so each client talks to few servers. With
  AllSyncRoute for writes, this is the documented replicated pool
  [W Replicated-pools-setup].
- **AllFastestRoute** sends to every child and returns the first reply that is
  not an error (`lib/routes/AllFastestRoute.h:23-63`). A miss is not an error,
  so a fast miss wins over a slower hit.
- **AllSyncRoute** sends to every child and returns the worst reply
  (`lib/routes/AllSyncRoute.h:25-28`). **AllInitialRoute** returns the first
  child's reply and lets the rest finish in the background
  (`lib/routes/AllInitialRoute.h:23-27`).
- **LoadBalancerRoute** can pick two children at random and send to the less
  loaded one, using load figures the servers piggyback on replies, which
  expire after 100 ms by default ([W List-of-Route-Handles];
  `routes/LoadBalancerRoute.h:33-53`).

### Warm-up

- **WarmUpRoute** reads the cold route first. On a miss it reads the warm
  route, returns that reply, and adds the value to the cold route in the
  background (`routes/WarmUpRoute.h:29-61`, `routes/WarmUpRoute.h:90-108`). A
  hot miss on the cold route is returned as is, because another client holds
  the lease (`routes/WarmUpRoute.h:120-126`). The wiki uses it for new boxes and
  for a two-level cache [W Cold-cache-warm-up-setup; W Two-level-caching].
- **SlowWarmUpRoute** sends only a fraction of requests to a box whose hit rate
  is below a threshold, `start + step × hitRate`, and fails the rest over at
  once (`routes/SlowWarmUpRoute.h:32-63`).

### Shadowing

A pool can copy requests to a shadow pool for a range of server indices and a
fraction of the key space, chosen by a key hash
([W Shadowing-setup; W Pools]; `routes/ShadowRoute.h:41-49`), to test new
servers and code on production traffic without affecting replies.

### Marking hosts down (TKO)

- A destination is marked soft TKO after `failures_until_tko` consecutive
  timeouts, 3 by default (`mcrouter_options_list.h:622-628`;
  `TkoTracker.h:57-68`). A connect error, connect timeout or shutdown marks it
  hard TKO at once (`lib/McResUtil.h:105-126`).
- While TKO, requests to it fail at once with no network trip
  (`ProxyDestinationBase.cpp:119-125`). With failover set up, they go to the
  next child [W Features].
- One proxy thread owns the TKO state and sends `version` probes. The delay
  starts at 10 s, grows by 1.5× per probe up to 60 s, with 5–50 % random jitter
  (`ProxyDestinationBase.cpp:27-30`, `ProxyDestinationBase.cpp:197-262`;
  `mcrouter_options_list.h:606-620`). Only a successful probe clears TKO
  (`ProxyDestinationBase.cpp:187-195`; `TkoTracker.h:57-62`).
- **Fail-open.** A pool can set an upper and a lower count or percentage of TKO
  hosts. Past the upper one, the pool stops marking hosts TKO until the count
  falls to the lower one (`TkoTracker.cpp:22-49`;
  `routes/McRouteHandleProvider-inl.h:282-310`). The wiki still describes the
  older global `--maximum-soft-tkos` [W Command-line-options], which is not in
  the options at this commit. When many hosts look dead at once, the fault is
  more likely the client's, and marking them all down would cascade.
- The default server timeout is 1 s (`mcrouter_options_list.h:640-647`), far
  above our bound.


## Lessons for the sproutfs plan

### Reading a page

1. **Step 3 doubles the bytes of a hot window instead of spreading them.** The
   reader asks rank 1 and a random rank from 2 to k at once; if both hold the
   window, both send it, and the owner still serves every reader. A burst of
   100 readers costs the owner the same 35 MB as with no copies, and the
   cluster about 70 MB. Cancelling the slower reply saves little, since a
   ~300 KB envelope run leaves a zone NIC in a fraction of a millisecond.
   Memcache has each client ask one replica, picked by its own address
   [P §3.2.3, p.389], as does mcrouter's LatestRoute by host ID
   (`routes/LatestRoute.h:27-59`).
   **Recommendation:** ask one cache per read; for a cold window, rank 1. The
   owner marks its reply when the window has copies. For a short time after
   seeing the mark, a reader picks one of ranks 1 to k by a hash of its own
   identity and the window, and falls back to rank 1 on a miss. If a parallel
   request is kept, make it a small "do you hold it" probe, not a read.
2. **Wait for a hit, not the first reply.** If two caches are asked at once,
   the reader must take the first hit, or two misses; mcrouter's
   AllFastestRoute lets a fast miss beat a slow hit
   (`lib/routes/AllFastestRoute.h:58-62`). The plan should state this and the
   model check it.
3. **Mark a dead or slow peer down**, as mcrouter's TKO does
   (`TkoTracker.h:57-68`; `ProxyDestinationBase.cpp:197-262`): after a few
   consecutive timeouts, or at once on a refused connection; then probe with
   backoff and jitter and clear only on a successful probe. Today a dead owner
   costs every read of its windows the bound, plus piling connection attempts,
   until the orchestrator drops it. While a peer is down, the reader asks the
   next rank and starts the store read at once.
   - Count only timeouts and connect errors, not misses or bad envelopes.
   - **Fail open.** Cap the share of caches one host may mark down
     (`TkoTracker.cpp:22-49`). If most peers time out, the host's own network
     is the likely fault.
   A host's down set is another list it holds, and the plan allows any list,
   so no correctness argument changes. The model's lists should include "the
   list minus the caches this host marked down".
4. **Rate-limit what falls back.** mcrouter can cap failovers to a fraction of
   requests (`routes/FailoverRoute.h:221-225`). The plan does not bound the
   store reads a host starts when its peers fail. GCS can take them, so add a
   metric and an alert, and a limit only if a measurement needs one.

### Filling the owner

5. **A cold burst needs a fill token, not a lease wait or read-through.** When
   N hosts miss one window at once, each reads the store and sends a keep, and
   the append-only owner may write the window N times. Take the herd half of
   memcache's leases [P §3.2.1, p.388]:
   - The owner's miss reply carries a "you fill this" flag, given to one
     reader per window per interval. Only that reader keeps.
   - The owner drops a keep for a window it holds or is filling. This alone
     fixes duplicate writes.
   Do not make the others wait: the filler's store read plus its keep takes
   longer than a waiter's own store read, and waiting only saves store
   requests, which the plan defers until GCS needs it. Owner read-through
   would also merge the burst, but puts a store read inside the owner, turns a
   dead owner into a stall, and is the stateful-server design the paper
   advises against [P §9, p.396].
   Herds are rarer for us: memcache's came from writes invalidating hot keys,
   while publications fill our owners before anyone reads. Ours come only
   after every cache has evicted a window, or after a mass restart of the
   cache tier.
6. **Do not keep to a peer that just timed out**, as memcache does not set a
   value back after a get error [P §3.1, p.387]. Drop the keep or send it to
   the next rank (item 7).
7. **Keep to rank 2 while the owner is down.** If the owner is down but still
   listed, every keep fails and every read pays the bound and the store for
   the length of the outage. A host that has marked the owner down should keep
   to the next rank that is up. The plan accepts a keep from any cache within
   k, so this is a copy on rank 2 that ages out once the owner is back:
   Gutter's idea without a separate pool.

### Copies of hot windows

8. **Next rank versus Gutter.** Facebook rejected rehashing because one hot
   key could overload its new home [P §3.3, p.390]. Our next rank is a rehash,
   but:
   - Rendezvous spreads a lost owner's windows over all other caches, one
     window at a time, unlike a ring or Ch3's index shift
     (`lib/fbi/hash.h:19-30`).
   - A window hot enough to overload one cache has copies, and rank 2 serves
     it.
   - A miss falls through to the store, which takes far more load than
     Facebook's databases.
   Gutter is 1 % of capacity while our caches are terabytes, its entries
   expire in about a minute while a new owner's fills are kept, and it starts
   empty, as rank 2 does for cold windows.
   The gap: a window that became hot just before its owner died has no copy,
   because the owner's distinct-host count died with it, so rank 2 sees the
   whole burst. With item 1 a reader that holds the hot mark can go to ranks 2
   to k; with item 5 the new owner writes the window once. Also count distinct
   hosts on the copies, so a copy that sees a burst while the owner is down can
   push to the next rank within k.
9. **The threshold matches Facebook's criterion**, distinct users and access
   rate [P §4.2, p.391, Table 1], applied automatically per window instead of
   by hand per key family. It reacts after the fact; see item 8.

### The list of caches

10. **How fast a dead cache leaves the list sets the cost of an outage.** The
    plan does not say how fast the orchestrator drops a cache that stops
    answering. Facebook's remediation took minutes, which is why Gutter exists
    [P §3.3, p.389]. With TKO (item 3) a stale list costs one bound per host
    per dead peer; without it, the bound on every read until the list changes.
11. **Gradual rollout** [P §9, p.396; W Shadowing-setup]. A setting for the
    fraction of windows, by key hash, for which hosts use peers would let the
    GCE measurement compare peer and store reads on the same traffic, and let
    a deployment back off without a restart.

### Who owns a page

12. **Rendezvous over a stable random identity.** Ch3 is index-based, so
    removing a server from the middle of the list moves many keys
    (`lib/fbi/hash.h:19-30`; [W Pools]). mcrouter's rendezvous keys servers by
    a configured tag (`routes/RendezvousRouteHelpers.cpp:16-40`), as the plan
    does with the identity in the cache file's header.
13. **A joining cache should pull from its old rank, once per window.** A
    joining cache wins about 1/N of the windows, and each one's old owner
    becomes its rank 2. Step 4 finds them, which is WarmUpRoute's read half
    (`routes/WarmUpRoute.h:90-108`). The plan lacks the write half: the window
    stays cold on rank 1, every read pays the extra round trip, and the window
    lives on rank 2 as if it were a copy, which `OneColdCopy` does not
    describe.
    **Recommendation:** a cache that serves a window for which it is not
    rank 1, and holds it as owner data rather than a copy, pushes it to rank 1
    with keep, once, by `sendfile`, and may then drop or demote it. That is one
    transfer per window instead of one per reader. No ramp like
    SlowWarmUpRoute (`routes/SlowWarmUpRoute.h:32-63`) is needed: there a cold
    miss costs a backend read [P §4.3, p.391], ours a trip to rank 2.
    When the whole tier starts cold, another zone's caches could serve as a
    warm tier ahead of the store, as in Facebook's cold cluster warmup
    [P §4.3, p.391]. That costs cross-zone transfer and belongs in a later
    plan.
14. **Restarts on a persistent volume.** Facebook kept memcached's data in
    shared memory across upgrades [P §6.4, p.394]. The plan's reading back of
    region tables does the same and also survives a pod restart.
15. **Use weighted rendezvous with coarse, stable weights**
    (`lib/WeightedRendezvousHashFunc.cpp:99-101`), not WeightedCh3, which is
    only mostly consistent and can fail at skewed weights
    (`lib/WeightedCh3HashFunc.h:37-54`).
    - **Weight is load, not only space.** Twice the weight serves twice the
      reads and keeps [W Pools]. On hosts of one machine type, a large weight
      can make the big disk the hot spot.
    - **Weight must not follow the limiter**, whose share moves with spill
      promises and other writers; every move shifts windows. Take it from the
      disk's size, or the limiter's goal, rounded to a few steps, and publish
      it with the identity in `/status` and `/caches`.
    - **Equal weights by default**, until a cluster mixes disk sizes. mcrouter
      keeps a fast path for equal weights that matches the unweighted hash
      (`lib/WeightedRendezvousHashFunc.cpp:25-37`).

### Incast and batching

16. **Bound a reader's requests in flight.** A pull or a large restore asks
    many owners at once, and answers of a few hundred kilobytes can arrive
    together and overflow the reader's NIC or switch queue: the incast
    Facebook bounded with a sliding window over all destinations
    [P §3.1, p.387–388]. The plan bounds keeps but not reads. Add a window of
    bytes in flight per host for peer reads, with faults ahead of pull reads.
17. **Pool long-lived connections per peer** and close them when idle
    [W Features; P §3.1, p.387].
18. **Ask by run.** Ranking by 2 MiB window so a read-ahead run asks one owner
    or a few is Facebook's batching [P §3.1, p.387].

## What does not apply

- Invalidation, mcsqueal, remote markers, delete hold-offs and stale values
  [P §3.2.1, §4.1, §4.3, §5]. Our pages never change.
- UDP gets [P §3.1, p.387]. A miss on a dropped packet would cost a store read.
- Separate pools by churn [P §3.2.2, p.389]. FIFO with a second chance lets a
  one-time scan pass through. If pull fills push out windows read often, mark
  pulled fills as not yet read, so they go first.
