# Memcache at Facebook and mcrouter: lessons for the disk cache — 2026-10-02

This note reads "Scaling Memcache at Facebook" and the source of mcrouter, and
asks what they teach the [distributed disk cache plan](../../plans/disk-cache-2026-10-02.md).
The last section maps each lesson to a section of that plan.

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
  the code. Where they disagree, this note says so and trusts the code.

**License.** mcrouter is MIT licensed (`LICENSE` line 1 at the commit above;
the README badge says the same). MIT is OSI-approved. Nothing here copies its
code. The server side of leases lives in Facebook's memcached fork, not in
mcrouter, so lease details below come from the paper.

## What the paper says

### The setting

Facebook's memcache is a look-aside cache in front of databases. A web server
reads memcache, and on a miss reads the database and sets the value back
[P §3.2, p.388]. Values change, so most of the paper's machinery is about
stale data. Ours never changes, so only the load and failure parts carry over.

Clients hold the map of servers, which a configuration system updates. Servers
never talk to each other [P §3.1, p.387]. The lessons list includes keeping
logic in a stateless client, and supporting gradual rollout and rollback
[P §9, p.396].

### Leases

A lease solves two problems: stale sets and thundering herds
[P §3.2.1, p.388].

- On a miss, the server gives the client a 64-bit token bound to the key. The
  client must present it when it sets the value. A delete of the key voids the
  token, so a late set of an old value is refused. This is the stale-set part.
- The server hands out at most one token per key every 10 seconds by default.
  Any other client that misses within that time is told to wait a short time
  and retry. The holder usually sets the value within a few milliseconds, so the
  retry usually hits. This is the herd part.
- For keys prone to herds, leases cut the peak database query rate from 17K/s
  to 1.3K/s over a week [P §3.2.1, p.388].
- A get may also return a recently deleted value marked stale, for clients that
  can use it [P §3.2.1, p.388–389].

mcrouter shows how a waiting client behaves. A lease get that returns token 1
is a "hot miss", and the client retries with exponential backoff: 2 ms first,
at most 500 ms, at most 10 retries (`routes/CarbonLookasideRoute.h:35-40`,
`routes/CarbonLookasideRoute.h:271-279`). After a failover, a lease set is
sent back to the server that issued the token (`routes/FailoverRoute.h:131-146`).

**What applies to immutable data.** Only the herd part. The token's job of
refusing a stale set has nothing to do in our cache, because no set can be
stale. What is left is "one filler per key per interval, the rest wait".

### Pools

Keys are split into pools by access pattern, because low-churn keys that are
still useful were evicted by high-churn keys nobody read again [P §3.2.2,
p.389].

### Replication within a pool

Some pools replicate every key to several servers [P §3.2.3, p.389]. They do
so when clients fetch many keys at once, the whole set fits on one or two
servers, and the request rate is more than one server can take. The argument is
that splitting the keys does not lower the request rate per server when every
request asks for many keys, but a replica does. Each client picks its replica
by its own IP address.

### Gutter

When a few servers fail, automated repair takes up to a few minutes. In that
time the failed servers' keys all fall on the database, which can cascade
[P §3.3, p.389].

- Gutter is a small set of spare servers, about 1 % of a cluster's memcached
  servers [P §3.3, p.389].
- A client that gets no reply to a get assumes the server failed and asks
  Gutter. On a Gutter miss it reads the database and sets the value into Gutter
  [P §3.3, p.389].
- Gutter entries expire quickly, so Gutter needs no invalidations
  [P §3.3, p.389–390]. The paper gives no number. mcrouter's
  `FailoverWithExptimeRoute` rewrites the TTL of sets that fail over, 60 s by
  default (`routes/FailoverWithExptimeRouteFactory.h:74`; [W List-of-Route-Handles]).
- They chose Gutter over rehashing the keys onto the remaining servers. A single
  key can be 20 % of one server's requests, and the server that inherits it can
  be overloaded in turn. Idle spares absorb that risk [P §3.3, p.390].
- Results: client-visible failures drop by 99 %, 10–25 % of failures become
  hits each day, and after a server dies Gutter's hit rate passes 35 % within 4
  minutes and often nears 50 % [P §3.3, p.390].

Two smaller points from the same part of the paper:

- A get error counts as a miss. After one, the web server does not set the
  value back, so it adds no load to a server or network that may be overloaded
  [P §3.1, p.387].
- mcrouter's `hash_salt` exists so that load failed over from one host spreads
  over a backup pool instead of landing on one backup host [W Pools].

### Regional pools

Several frontend clusters in a region can share one pool instead of each
keeping a replica [P §4.2, p.391]. Keys go there when they are large and rarely
read. The decision is manual, by heuristics on access rate, data set size and
the number of distinct users of an item [P §4.2, p.391]. Table 1 contrasts a
family left replicated (median 30 users, 3.26 M gets/s) with one moved to a
regional pool (median 1 user, 458 K gets/s).

### Cold cluster warmup

A new or repaired cluster starts with empty caches. Clients there read a miss
from a warm cluster instead of the database, and add the value to the cold
cluster [P §4.3, p.391]. This brings a cluster to full capacity in a few hours
instead of a few days. Most of the section handles a race with invalidations,
using a two-second hold-off on deletes. That race cannot happen to immutable
pages. They turn warmup off once the hit rate settles.

### Restarts

A memcached server reaches 90 % of its peak hit rate within a few hours, so
upgrading a set of them took over 12 hours to keep database load in check.
They moved the cache into System V shared memory so it survives a software
upgrade [P §6.4, p.394].

### Incast and batching

- A client sends independent fetches together, in batches of 24 keys on
  average [P §3.1, p.387].
- mcrouter coalesces connections. One TCP connection per destination per
  mcrouter thread is shared by all requests [P §3.1, p.387; W Features].
- Each client limits its outstanding requests with a sliding window across all
  destinations. It grows slowly on success and shrinks when a request goes
  unanswered [P §3.1, p.387–388]. Too small a window serializes work. Too large
  causes incast and errors, which push load onto the database (Figure 4).
- mcrouter also has a per-destination cap on requests in flight, off by default
  (`mcrouter_options_list.h:286-302`).

## What mcrouter does

### Hashing

- **Ch3** is the default. It is `furc_hash(key, n)`, which returns an index
  into the pool's ordered server list (`lib/Ch3HashFunc.h:21-32`;
  [W Pools]). It is consistent in n: growing a pool from 11 to 12 servers moves
  1/12 of the keys (`lib/fbi/hash.h:19-30`). It is not consistent in a
  server's identity. Removing a server from the middle of the list shifts the
  index of every server after it.
- **WeightedCh3** gives each server a weight in [0, 1]. It hashes the key to an
  index, then accepts that index with probability equal to its weight, or
  retries with a salt (`lib/WeightedCh3HashFunc.h:16-54`). The header admits it
  is only mostly consistent when n changes, and can give up when weights are
  skewed. At an average weight of 0.25 it needs about 16 tries for a 1 %
  failure rate.
- **Rendezvous** and **WeightedRendezvous** score each server by a hash of its
  tag and the key. Weighted scores are `w / -ln(U)` with U the hash mapped to
  (0, 1) (`lib/RendezvousHashFunc.cpp:30-66`,
  `lib/WeightedRendezvousHashFunc.cpp:99-101`). A server's tag is a string in
  the configuration, not its position (`routes/RendezvousRouteHelpers.cpp:16-40`).
- **FailoverRendezvousPolicy** walks the servers in rendezvous rank order on
  failover, skipping the primary (`routes/FailoverPolicy.h:603-720`). This is
  the plan's "rank 2 on failure", already in production code.

### Failover and its variants

- **FailoverRoute** sends to the first child, and on an error to the next. A
  miss is not an error ([W List-of-Route-Handles];
  `routes/FailoverRoute.h:55-59`). Which errors fail over is configurable per
  operation. A token bucket can cap failovers to a fraction of all requests.
  A reply of TKO skips the bucket and does not count as a try, because no work
  was done (`routes/FailoverRoute.h:221-230`).
- **MissFailoverRoute** tries children in order until the first hit
  (`routes/MissFailoverRoute.h:29-34`).
- **LatestRoute** builds a FailoverRoute over at most `failover_count` children
  (default 5), chosen by hashing the client's host ID, not the key
  (`routes/LatestRoute.h:27-66`). Each client therefore talks to few servers.
  With AllSyncRoute for writes, this is the documented replicated pool
  [W Replicated-pools-setup].
- **AllFastestRoute** sends to every child and returns the first reply that is
  not an error (`lib/routes/AllFastestRoute.h:23-63`). A miss is not an error,
  so a fast miss wins over a slower hit.
- **AllSyncRoute** sends to every child and returns the worst reply
  (`lib/routes/AllSyncRoute.h:25-28`). **AllInitialRoute** returns the first
  child's reply and lets the rest finish in the background
  (`lib/routes/AllInitialRoute.h:23-27`).
- **LoadBalancerRoute** can pick two children at random and send to the less
  loaded one, using load figures the servers piggyback on replies and that
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
  is below a threshold, `start + step × hitRate`, and fails the rest over
  straight away (`routes/SlowWarmUpRoute.h:32-63`).

### Shadowing

A pool can copy requests to a shadow pool for a range of server indices and a
fraction of the key space, chosen by a key hash
([W Shadowing-setup; W Pools]; `routes/ShadowRoute.h:41-49`). It is how they test
new servers and code on production traffic without affecting replies.

### Marking hosts down (TKO)

- A destination is marked soft TKO after `failures_until_tko` consecutive
  timeouts, 3 by default (`mcrouter_options_list.h:622-628`;
  `TkoTracker.h:57-68`). A connect error, connect timeout or shutdown marks it
  hard TKO at once (`lib/McResUtil.h:105-126`).
- While TKO, requests to it fail at once with no network trip
  (`ProxyDestinationBase.cpp:119-125`). With failover set up, they go straight
  to the next child [W Features].
- One proxy thread owns the TKO state and sends `version` probes. The delay
  starts at 10 s, grows by 1.5× per probe up to 60 s, with 5–50 % random jitter
  (`ProxyDestinationBase.cpp:27-30`, `ProxyDestinationBase.cpp:197-262`;
  `mcrouter_options_list.h:606-620`). Only a probe that succeeds clears TKO
  (`ProxyDestinationBase.cpp:187-195`; `TkoTracker.h:57-62`).
- **Fail-open.** A pool can set an upper and a lower count or percentage of TKO
  hosts. Past the upper one, the pool stops marking hosts TKO until the count
  falls to the lower one (`TkoTracker.cpp:22-49`;
  `routes/McRouteHandleProvider-inl.h:282-310`). The wiki still describes the
  older global `--maximum-soft-tkos` [W Command-line-options], which is not in
  the options at this commit. The aim is the same: when many hosts look dead at
  once, the fault is more likely the client's, and marking them all down would
  cascade.
- The default server timeout is 1 s (`mcrouter_options_list.h:640-647`), far
  above what our bound needs.

## Lessons for the sproutfs plan

### Reading a page

1. **Step 3 doubles the bytes of a hot window; it does not spread them.** The
   reader asks rank 1 and a random rank from 2 to k at once. If both hold the
   window, both send it. The owner still serves every reader, and the copies add
   their own sends on top. A burst of 100 readers costs the owner the same
   35 MB as with no copies, and the cluster about 70 MB. Cancelling the slower
   reply saves little, since a ~300 KB envelope run leaves a zone NIC in a
   fraction of a millisecond. Memcache spreads load the other way: each client
   asks one replica, picked by its own address [P §3.2.3, p.389]; mcrouter's
   LatestRoute does the same by host ID (`routes/LatestRoute.h:27-59`).
   **Recommendation:** ask one cache per read. For a cold window that is rank 1.
   The owner marks its reply when the window has copies. A reader that has seen
   the mark, for a short time, picks one of ranks 1 to k by a hash of its own
   identity and the window, and falls back to rank 1 on a miss. Cold windows
   then cost one request, not two, and a hot window's readers really divide
   over k caches. If a parallel request is kept, make it a small "do you hold
   it" probe, not a read.
2. **Wait for a hit, not for the first reply.** If two caches are asked at
   once, the reader must take the first hit, or two misses. mcrouter's
   AllFastestRoute returns the first non-error reply, and a miss counts, so a
   fast miss beats a slow hit (`lib/routes/AllFastestRoute.h:58-62`). The plan
   should say this, and the model should check it.
3. **(c) Mark a dead or slow peer down instead of paying the bound on every
   read.** Today a dead owner costs every read of its windows the bound, plus
   connection attempts that pile up, until the orchestrator drops it from the
   list. mcrouter's TKO is the proven shape: count consecutive timeouts per
   peer, mark down after a few (3 by default), mark down at once on a refused
   connection, then probe with backoff and jitter and clear only on a probe
   that succeeds (`TkoTracker.h:57-68`; `ProxyDestinationBase.cpp:197-262`).
   While a peer is down, the reader skips it in its ranks: it asks the next
   rank and starts the store read at once, with no bound. Two details matter:
   - Count only timeouts and connect errors. A miss, or a bad envelope, is not
     a failure of the peer.
   - **Fail open.** Cap the share of caches one host may mark down, as
     mcrouter's pool thresholds do (`TkoTracker.cpp:22-49`). If a host sees
     most peers time out, its own network is the likely fault.
   This changes no correctness argument. A host's down set is just another list
   it holds, and the plan already allows any list. The model's lists should
   include "the list minus the caches this host marked down".
4. **Rate-limit what falls back.** mcrouter can cap failovers to a fraction of
   requests (`routes/FailoverRoute.h:221-225`). The plan bounds keeps but not
   the store reads a host starts when its peers fail. GCS can take them, so
   this is a metric and an alert first, a limit only if a measurement asks.

### Filling the owner

5. **(a) A cold burst needs a fill token, not a lease wait and not
   read-through.** When N hosts miss one window at once, each reads the store
   and each sends a keep. The owner's log is append-only, and the plan allows
   an entry per region a window was filled in. So the owner may write the same
   window N times, and burn N times its network on keeps. Memcache's herd fix
   gives one client per key per 10 s the right to fill [P §3.2.1, p.388]. Take
   that part only:
   - The owner tracks windows being filled. Its miss reply carries a flag
     saying "you fill this", given to one reader per window per interval.
   - Only that reader sends the keep. The others read the store and keep
     nothing.
   - The owner also drops a keep for a window it already holds or is filling.
     This alone fixes duplicate writes, even without the flag.
   Do not make the others wait, as memcache does. A waiter gains nothing in
   latency: the filler's store read plus its keep takes longer than the
   waiter's own store read. Waiting only saves store requests, and the plan
   already defers that until a measurement says GCS cares. Owner read-through
   would also merge the burst, but it puts a store read inside the owner, turns
   a dead owner into a stall, and is the stateful-server design the paper's
   lessons warn against [P §9, p.396]. Keep the plan's choice.
   Note too that a herd is rarer for us than for memcache. Memcache herds came
   from writes invalidating hot keys. Our publications fill owners before
   anyone reads, so a burst of forks of a fresh template finds the owners warm.
   Herds come only after every cache has evicted a window, or after a mass
   restart of the cache tier.
6. **Do not keep to a peer that just timed out.** Memcache skips setting a
   value back after a get error, to spare a possibly overloaded server
   [P §3.1, p.387]. A keep to a peer that is down, or that just passed the
   bound, should be dropped, or sent to the next rank (see 7).
7. **(b) Keep to rank 2 while the owner is down.** With the plan as written,
   readers keep to the owner they compute. If that owner is down but still in
   the list, every keep fails and the window stays uncached. Every read of it
   pays the bound and the store for as long as the outage lasts. A host that
   has marked the owner down should keep to the next rank that is up. The plan
   already accepts a keep from any cache within k, so this is a copy on rank 2
   and needs no new rule. It ages out like any copy once the owner is back.
   This is Gutter's idea, a place to fill while the server is away, without a
   separate pool.

### Copies of hot windows

8. **(b) Next rank versus Gutter.** Facebook rejected rehashing to the
   remaining servers because one hot key could overload its new home
   [P §3.3, p.390]. Our next rank is a rehash, so the objection needs an
   answer. Three facts give one:
   - Rendezvous spreads a lost owner's windows over all the other caches, one
     window at a time. No single neighbour inherits a server's whole load, as
     with a ring or with Ch3's index shift (`lib/fbi/hash.h:19-30`).
   - A window hot enough to overload one cache is exactly a window with copies,
     and rank 2 already serves it.
   - A miss at the new owner falls through to the store, which takes far more
     load than Facebook's databases. Gutter existed to shield the database.
   Gutter would be worse here. It is 1 % of capacity, while our caches are
   terabytes. Its entries expire in about a minute, while a new owner's fills
   are permanent and useful. And it starts empty, as rank 2 does for cold
   windows.
   The weak spot is real, though. A window that became hot just before its
   owner died has no copy, because the owner's distinct-host count died with
   it. Rank 2 then sees the whole burst, and every reader goes to the store at
   once. Two cheap mitigations:
   - With item 1, readers know a window is hot from the owner's mark. A reader
     that holds the mark and finds the owner down can still go to ranks 2 to k.
   - With item 5, the new owner hands out one fill token, so the burst writes
     the window once.
   Also count distinct hosts on the copies, not only on the owner. A copy that
   sees a burst while the owner is down can push to the next rank within k.
9. **The threshold matches Facebook's criterion.** Facebook decided what to
   replicate by the number of distinct users of an item and its access rate
   [P §4.2, p.391, Table 1]. Counting distinct hosts is the same idea, done
   automatically per window instead of by hand per key family. That is an
   improvement, but it reacts after the fact; see item 8.

### The list of caches

10. **How fast a dead cache leaves the list sets the cost of an outage.** The
    plan has each host read the list on a timer but does not say how fast the
    orchestrator drops a cache that stops answering. Facebook's remediation
    took minutes, which is why Gutter exists [P §3.3, p.389]. With TKO
    (item 3) a stale list costs one bound per host per dead peer, then nothing.
    Without it, it costs the bound on every read until the list changes. TKO
    lets the list stay slow and simple.
11. **Gradual rollout.** Facebook names gradual rollout and rollback as a
    lesson [P §9, p.396], and mcrouter shadows a fraction of keys by hash
    [W Shadowing-setup]. A setting for the fraction of windows, by key hash,
    for which hosts use peers would let the GCE measurement compare peer and
    store reads on the same traffic, and would let a deployment back off
    without a restart.

### Who owns a page

12. **Rendezvous over a stable random identity is the right choice.**
    mcrouter's default Ch3 is index-based, so a removed server in the middle of
    the list moves many keys (`lib/fbi/hash.h:19-30`; [W Pools]). Its newer
    rendezvous hash keys servers by a configured tag
    (`routes/RendezvousRouteHelpers.cpp:16-40`), which is what the plan does
    with the identity in the cache file's header.
13. **(d) A joining cache should pull from its old rank, once per window.**
    When a cache joins, it wins about 1/N of the windows, and each one's old
    owner becomes its rank 2. The plan's step 4 then finds them, at half a
    millisecond. That is WarmUpRoute's read half
    (`routes/WarmUpRoute.h:90-108`). The plan lacks its write half. Nothing
    moves the window to the new owner, so it stays cold on rank 1 for good, and
    every read pays the extra round trip. Meanwhile the window lives on rank 2
    as if it were a copy, which `OneColdCopy` does not describe.
    **Recommendation:** a cache that serves a window for which it is not rank 1,
    and that it holds as owner data rather than as a copy, pushes it to rank 1
    with keep, once, and may then drop or demote it. The push comes from the
    holder, by `sendfile`, not from each reader. That is one transfer per
    window instead of one per reader.
    mcrouter ramps traffic to a cold box with SlowWarmUpRoute
    (`routes/SlowWarmUpRoute.h:32-63`), because there a cold miss costs a
    backend read [P §4.3, p.391]. Ours costs a trip to rank 2, so we do not need
    the ramp.
    The same pull serves the case where the whole tier starts cold: a cold
    cluster could treat another zone's caches as a warm tier ahead of the
    store, as Facebook's cold cluster warmup did [P §4.3, p.391]. That costs
    cross-zone transfer, so it is a later plan, not this one.
14. **Restarts on a persistent volume are worth it.** Facebook kept memcached's
    data in shared memory across upgrades because a cold server took hours to
    warm and upgrades took over 12 hours [P §6.4, p.394]. The plan's reading
    back of region tables is the same lesson, and stronger, since it also
    survives a pod restart.
15. **(e) Use weighted rendezvous, with coarse and stable weights.** mcrouter's
    weighted rendezvous scores `w / -ln(U)`
    (`lib/WeightedRendezvousHashFunc.cpp:99-101`). A change to one weight moves
    only the windows that cache wins or loses. Do not copy WeightedCh3: it is
    only mostly consistent and can fail at skewed weights
    (`lib/WeightedCh3HashFunc.h:37-54`). Three cautions:
    - **Weight is load, not only space.** A cache with twice the weight serves
      twice the reads and twice the keeps. Facebook uses weights to move
      traffic as much as to fit space [W Pools]. On hosts of one machine type,
      the network and decode are equal, so a large weight can make the big
      disk the hot spot.
    - **Weight must not follow the limiter.** The limiter's share of the disk
      moves with spill promises and other writers. A weight that follows it
      moves windows every time a VM starts. Take the weight from the disk's
      size, or the limiter's goal, rounded to a few steps, and publish it with
      the identity in `/status` and `/caches`.
    - **All weights equal is the default.** Add weights only when a cluster
      really mixes disk sizes. mcrouter keeps a fast path for equal weights
      that gives the same answer as the unweighted hash
      (`lib/WeightedRendezvousHashFunc.cpp:25-37`).

### Incast and batching

16. **Bound a reader's requests in flight.** A pull or a large restore asks
    many owners for many windows at once. Each answer is a few hundred
    kilobytes, so they can arrive together and overflow the reader's NIC or
    switch queue. That is the incast Facebook bounded with a sliding window
    over all destinations, grown on success and shrunk on timeouts
    [P §3.1, p.387–388]. The plan bounds keeps but not reads. Add one window
    of bytes in flight per host for peer reads, with faults ahead of pull
    reads.
17. **One connection per peer, reused.** mcrouter shares one connection per
    destination per thread [W Features], and Facebook calls connection
    coalescing necessary at scale [P §3.1, p.387]. Every host now talks to
    every cache, so connections should be pooled and long-lived, and closed
    when idle.
18. **Ask by run.** The plan already ranks by 2 MiB window so a read-ahead run
    asks one owner, or a few. That is Facebook's batching [P §3.1, p.387];
    keep it.

## What does not carry over

- Invalidation, mcsqueal, remote markers, delete hold-offs and stale values
  [P §3.2.1, §4.1, §4.3, §5]. Our pages never change.
- UDP gets [P §3.1, p.387]. A miss on a dropped packet would cost a store read.
- Separate pools by churn [P §3.2.2, p.389]. FIFO with a second chance already
  lets a one-time scan pass through. One caution remains: pull fills are a
  high-churn family. If pulls push out windows that are read often, mark
  pulled fills as not yet read, so they are the first to go.

## Summary of the five questions

- **(a)** No owner read-through and no lease wait. Add a fill token so one
  reader per window keeps, and have the owner drop duplicate keeps. (Item 5.)
- **(b)** The next rank is better than a Gutter pool for us. Its weak spot is
  a window that turned hot just before its owner died. Cover it with the hot
  mark and with keeps to the next rank while the owner is down. (Items 7, 8.)
- **(c)** Yes, add TKO with probes, backoff, jitter and fail-open. It turns a
  dead peer from a bound on every read into a bound once. (Item 3.)
- **(d)** Yes, add the write half of WarmUpRoute, done by the holder. Without
  it a joining cache never warms for windows that moved to it. (Item 13.)
- **(e)** Weighted rendezvous, with weights from disk size in coarse steps,
  equal by default. (Item 15.)

Item 1 is the most important finding outside the five: as written, asking
rank 1 and a random rank together does not take load off a hot window's owner.
