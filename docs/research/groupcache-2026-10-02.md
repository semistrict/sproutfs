# groupcache and its descendants — 2026-10-02

Research for [the distributed disk cache plan](../../plans/disk-cache-2026-10-02.md).

## Sources

| Tag | Project | Commit | Date | License (from its LICENSE file) |
| --- | --- | --- | --- | --- |
| gc | github.com/golang/groupcache, `~/src/groupcache` | `2c02b8208cf8c02a3e358cb1d9b60950647543fc` | 2024-11-29 | Apache-2.0 |
| mg | github.com/mailgun/groupcache (v2), `~/src/mailgun-groupcache` | `90b5ebf376fe71aa1886fd48bce61c065dcf2a80` | 2024-12-30 | Apache-2.0 |
| gx | github.com/vimeo/galaxycache, `~/src/galaxycache` | `df1970c32c929f9a3b5c105a8ea9c6b0e145dd99` | 2026-02-10 | Apache-2.0 |
| xs | golang.org/x/sync, `~/src/x-sync` | `36f2d70ecde9857dc4e363242a1369c7645d6770` | 2026-09-23 | BSD-3-Clause |

A citation such as `gc groupcache.go:272` means that file and line at the
commit above. Nothing here is a dependency. These are designs to learn from.

## groupcache

### Who owns a key

- The ring is `consistenthash.Map`. Each peer adds `replicas` points. Point `i`
  of peer `p` is `hash(strconv.Itoa(i) + p)` (gc consistenthash/consistenthash.go:53-62).
- The default hash is CRC-32 IEEE (gc consistenthash/consistenthash.go:41-43).
  The default is 50 points per peer (gc http.go:36, 107-109).
- A key's owner is the first point at or after `hash(key)`, wrapping at the end
  (gc consistenthash/consistenthash.go:65-80). Lookup is a binary search.
- Two points with the same hash overwrite each other. The last peer added wins
  (gc consistenthash/consistenthash.go:58). So two processes that add the same
  peers in a different order can disagree on an owner.
- There are no weights. Every peer gets the same number of points.
- Discovery is not part of the library. The caller calls `HTTPPool.Set` with
  the full peer list. `Set` builds a new ring from scratch
  (gc http.go:119-128).
- When the list changes, nothing moves. Keys whose owner changed simply miss
  at the new owner, and the new owner loads them from the origin.

### The read path

`Group.Get` (gc groupcache.go:212-238):

1. Look in `mainCache`, then `hotCache` (gc groupcache.go:334-344).
2. On a miss, enter `load`, which runs under a `singleflight.Group` keyed by the
   key (gc groupcache.go:241-243).
3. Inside the flight, look in the caches again. Singleflight only merges calls
   that overlap, so a second, later call would otherwise load again and count
   its bytes twice (gc groupcache.go:244-268).
4. Ask `PickPeer` for the owner. If the owner is another process, fetch from it
   (gc groupcache.go:272-277).
5. If this process is the owner, or the peer fetch failed for any reason, call
   the local `Getter` and put the value in `mainCache`
   (gc groupcache.go:284-291).

The owner side is the same function. The HTTP handler calls `group.Get` for
the requested key (gc http.go:168-170). On the owner, `PickPeer` returns
"self" (gc http.go:136-139), so the owner calls its own `Getter`: the owner
reads through to the origin on a miss. Its singleflight merges every
concurrent miss for that key, whether it came from a local caller or from any
peer. So a burst of N peers asking for one cold key costs the origin one load,
if all N requests reach the owner while the first load is in flight
(gc README.md:23-29, gc groupcache.go:75-82).

That bound has holes:

- It holds only while the requests overlap. A request that lands just after
  the flight ends finds the value in `mainCache`, so that is fine. But a
  request that cannot reach the owner loads from the origin itself
  (gc groupcache.go:278-284).
- Any peer error falls back to a local load. So if the owner's origin load
  fails, every requester retries the origin on its own. The burst the owner
  merged is undone.
- A non-owner that falls back puts the value in its own `mainCache`
  (gc groupcache.go:291). After an owner outage, keys sit in the wrong
  processes' main caches.
- The handler does not stop forwarding. If the receiving peer's list says
  another peer owns the key, it forwards again (gc http.go:170 into
  gc groupcache.go:272). There is no hop count. Two peers with different
  lists can bounce a request.

### singleflight in groupcache

- `Do` stores one in-flight call per key. Later callers wait on its
  `WaitGroup` and get the same value and error (gc singleflight/singleflight.go:41-63).
- A waiter cannot give up. It has no context (gc singleflight/singleflight.go:46-49).
- The leader runs the fetch with its own context (gc groupcache.go:243, 273,
  284). If the leader's context is cancelled, every waiter gets that error.

### hotCache

- `hotCache` holds keys this process does not own but that seem popular. The
  comment gives the reason: a popular key would otherwise make the owner's
  "network card could become the bottleneck" (gc groupcache.go:155-162). The
  same comment says it is used sparingly, to keep the number of distinct keys
  in the cluster high.
- A value fetched from a peer is put in `hotCache` with probability 1 in 10
  (gc groupcache.go:322-330). A TODO says it should use the owner's reported
  QPS instead (gc groupcache.go:319-321). The response has a `minute_qps`
  field for this (gc groupcachepb/groupcache.proto:28), but the server never
  sets it (gc http.go:177).
- Both caches share one byte budget (gc groupcache.go:147). On overflow, the
  victim is `hotCache` when it holds more than one eighth of `mainCache`'s
  bytes, and `mainCache` otherwise (gc groupcache.go:353-367). Under pressure,
  the hot cache settles at about one ninth of the total.
- Each cache is a plain LRU. Nothing expires. Values must be immutable
  (gc groupcache.go:43-47, gc README.md:31-34).
- The decision is made by the reader alone, from its own random draw. The
  owner has no say, and the reader does not count how often it reads the key.

### Failure handling

- There is no timeout in groupcache itself. The peer request uses the
  caller's context and an `http.RoundTripper` (gc http.go:195-211).
- A dead owner costs each miss whatever the transport takes to fail, then a
  local origin load. There is no memory of failed peers. A TODO wonders
  whether to log peer errors at all (gc groupcache.go:278-282).
- The only fallback is the local origin load (gc README.md:54-60).

## mailgun/groupcache

It keeps groupcache's structure and adds mutation and expiry.

- Ring: same scheme, but each point is the FNV-1 64-bit hash of the hex MD5 of
  `i + peer` (mg consistenthash/consistenthash.go:44-46, 59). The changelog
  says MD5 was added to spread hosts more evenly (mg CHANGELOG:21-23).
- Hot cache: every value fetched from a peer goes into `hotCache`
  (mg groupcache.go:483-484). The changelog argues the LRU and the one-eighth
  eviction rule are enough (mg CHANGELOG:42-44).
- Errors: the reader no longer falls back to a local load in three cases. The
  context is done (mg groupcache.go:409-411, 431-435). The owner said not
  found (mg groupcache.go:413-415). The owner's own getter failed, reported as
  HTTP 503 (mg groupcache.go:417-419, mg http.go:233, 314-316,
  mg errors.go:20-22). This last case keeps an origin failure from turning
  into N origin loads.
- TTL: a value may carry an expiry. The LRU drops an expired entry on read
  (mg lru/lru.go:97-98). A peer reply that is already expired is an error
  (mg groupcache.go:473-479).
- `Set` writes to the owner, and optionally to the local hot cache
  (mg groupcache.go:279-305).
- `Remove` removes from the owner, then from every peer in parallel
  (mg groupcache.go:309-354). It is best effort and costs one request per peer
  per key. It holds the singleflight lock so no load is in flight while the
  local entry goes (mg groupcache.go:538-549, mg singleflight/singleflight.go:74-81).
- Replication: none.

## galaxycache

It is a rewrite of groupcache with no global state, and adds hot-key
promotion by QPS, peeking at the previous owner, and replica lookups in the
ring.

### Ring and membership

- Same ring scheme and defaults as groupcache: CRC-32, 50 points per peer
  (gx peers.go:39, gx consistenthash/consistenthash.go:66-68). A comment says
  FNV-32a is better but the default cannot change without breaking running
  systems (gx consistenthash/consistenthash.go:28-31). Another says the point
  naming is weak but fixed for the same reason
  (gx consistenthash/map_rangefunc.go:31-34).
- Point collisions are settled by name order, not insertion order, so every
  process builds the same ring (gx consistenthash/map_rangefunc.go:56-66).
- Peers have an ID separate from their address (gx peers.go:173-183). The
  ring is built from IDs. The ring is rebuilt on every change
  (gx peers.go:423-448).
- A Kubernetes watcher adds a pod when it is ready and has an IP, and removes
  it otherwise (gx k8swatch/k8swatch.go:21-31). A process takes itself out of
  the ring once its pod has a deletion time (gx k8swatch/k8swatch.go:13-19).

### Replica lookups in the ring

- `GetReplicated(key, n)` returns n distinct owners. Replica `i` hashes the
  key with a suffix `i`, finds its point, and walks forward past owners already
  chosen (gx consistenthash/map_rangefunc.go:79-111,
  gx consistenthash/consistenthash.go:116-134). The doc comment says it walks
  backwards; the code walks forward (gx consistenthash/map_rangefunc.go:99-101
  against gx consistenthash/consistenthash.go:101-114).
- `OwnerRangeSummary` answers "might this peer own this key, if up to s peers
  vanish" with no false negatives (gx consistenthash/rangeset.go:74-171).
- The commit that added `GetReplicated` says only that some uses need several
  owners (gx commit 2e38d0b). The `Galaxy` itself never calls it. It uses one
  owner per key (gx peers.go:154-162).
- Because replica `i` hashes a different string, the second replica of a key
  is not the peer that would own the key if the first left. On a ring, that
  successor is the next point after `hash(key)`.

### Owner read-through, without forwarding

- A `Galaxy.Get` miss runs under singleflight. If another peer owns the key,
  it fetches from it. Otherwise it calls the backend and fills `mainCache`
  (gx galaxycache.go:904-1001).
- A peer's not-found is final. Any other peer error falls back to the backend
  (gx galaxycache.go:963-966, 816).
- The servers call `GetWithOptions` with `FetchModeNoPeerBackend`
  (gx http/http.go:219-225, gx grpc/grpcserver.go:59). So the receiving peer
  never forwards. It answers from its cache or its backend. This removes
  groupcache's unbounded forwarding.

### Peeking at the previous owner

- A new process starts with an empty cache, and takes over a share of keys.
  For `WarmTime` after start, when it owns a key and misses, it first asks the
  peer that would own the key if it were not in the ring
  (gx galaxycache.go:967-978, gx peers.go:137-150).
- That "peek map" is the ring without self (gx peers.go:442-447). A peek
  answers from memory only, never from the backend
  (gx galaxycache.go:861-867, gx grpc/grpcserver.go:86).
- A peek has its own short deadline. The config suggests 2 to 10 ms on a
  local network (gx galaxycache.go:433-436, 1047-1051).
- A peek hit goes into the new owner's `mainCache`
  (gx galaxycache.go:1073-1078). So the value moves to its new owner on first
  use.
- Peeking at peers that are leaving is a TODO (gx peers.go:62-70,
  gx galaxycache.go:439-441). The commit says it targets startup only
  (gx commit bfde671).

### Hot-key promotion

- Every value carries a `keyStats` with a count and a start time
  (gx hotcache.go:55-97). QPS is count over age, with a 100 ms floor on age
  (gx hotcache.go:123-135).
- A touch after more than `resetIdleStatsAge` (default one minute) restarts
  the count (gx hotcache.go:99-112, gx galaxycache.go:247). The comment says
  this stops a key that is hot in bursts from looking cold.
- Keys this process does not own keep their stats, without data, in a
  candidate LRU of 1024 entries (gx galaxycache.go:245, 268,
  gx typed_caches.go:28-49). A key evicted from the hot cache goes back to the
  candidates with its stats (gx galaxycache.go:280).
- After each peer fetch, the promoter decides whether the value enters the hot
  cache (gx galaxycache.go:1083-1111). The galaxy-wide stats it sees are
  refreshed at most once a second (gx hotcache.go:26-53).
- The default promoter promotes when the key's QPS is at least the QPS of the
  least recently used hot entry (gx promoter/promoter.go:68-77). While the hot
  cache is empty, that QPS is zero (gx hotcache.go:36-43), so every fetched
  key is promoted until it fills.
- Other promoters: 1 in N at random (gx promoter/promoter.go:57-66), and "seen
  before" (gx promoter/promoter.go:79-89).
- Hot is one eighth of the budget by default (gx galaxycache.go:244), with the
  same eviction rule as groupcache (gx galaxycache.go:1141-1157).
- All counting is local. A process counts its own reads of a key. It never
  learns how many other processes read it.

### TTL

- A backend may return an expiry. `WithGetTTL` and `WithPeekTTL` cap it, with
  random jitter so values filled together do not expire together
  (gx galaxycache.go:583-608, 1003-1024).

## x/sync singleflight

- `Do` merges concurrent calls for one key and reports whether the result was
  shared (xs singleflight/singleflight.go:86-115).
- `DoChan` returns a channel, so a waiter can select on its own context and
  leave (xs singleflight/singleflight.go:117-141). The call itself keeps
  running.
- A panic in the function is re-raised in every waiter
  (xs singleflight/singleflight.go:163-171).
- `Forget` drops the in-flight entry, so the next call starts fresh
  (xs singleflight/singleflight.go:207-214).
- It does not cache. It only merges calls that overlap. groupcache re-checks
  its cache inside the flight for that reason (gc groupcache.go:244-268).

## Lessons for the sproutfs plan

### Who owns a page

1. **Keep rendezvous.** The plan needs one property above all: when the owner
   leaves, the old rank 2 becomes the owner. Rendezvous gives that exactly.
   A ring with replica lookups does not. galaxycache's second replica hashes a
   different string (gx consistenthash/consistenthash.go:116-134), so it is
   not the ring successor that takes over. Copies on "ranks 2 to k" only make
   sense with rendezvous.
2. **Balance favours rendezvous too.** With 50 points per peer, each peer's
   share is a sum of 50 random arcs. Its spread is about 1/√50, roughly 14 %
   of the mean. With many peers the largest share runs about a third over the
   mean. This is our own estimate, not measured here. Rendezvous gives each
   window an independent draw, so with millions of windows the shares are
   even.
3. **Lookup cost is not a concern.** Rendezvous costs one hash per cache per
   window. A few hundred hashes is microseconds against a 1 ms read. Cache the
   ranks per window if profiles say so.
4. **Use a strong 64-bit hash of (cache identity, window key) together.** Both
   descendants are stuck with CRC-32 and weak point names
   (gx consistenthash/consistenthash.go:28-31,
   gx consistenthash/map_rangefunc.go:31-34). Pick the hash once and write it
   in the spec. Changing it later moves every window.
5. **Break ties by cache identity.** groupcache's ring let insertion order
   decide collisions (gc consistenthash/consistenthash.go:58). galaxycache had
   to fix this (gx consistenthash/map_rangefunc.go:56-66). Two hosts with the
   same list must rank the same, whatever order the orchestrator lists them.
6. **Weighted ranks for unequal disks.** Neither library has weights. The
   standard weighted rendezvous score is `-w / ln(u)`, with `u` the hash mapped
   into (0, 1). It gives each cache a share proportional to `w` and still moves
   only the windows a change touches. This comes from the weighted rendezvous
   literature, not from these sources. The plan should:
   - take `w` from the cache's configured disk size, rounded to a coarse step;
   - never take it from the limiter's live share, which moves with spill
     promises, because every change of `w` moves windows;
   - report `w` in `/status` and serve it in `GET /caches`.
7. **Membership from readiness.** galaxycache drops a pod from the ring once
   it is not ready, and a pod drops itself once it is terminating
   (gx k8swatch/k8swatch.go:13-31). The orchestrator's list should do the same,
   so a draining host stops being rank 1 before it stops answering.

### Reading a page

8. **The parallel second request defeats its purpose for hot windows.** Step 3
   asks rank 1 and a random rank from 2 to k at once. For a hot window both
   hold the data, so both send it. The owner still sends every byte to every
   reader, and the reader receives the window twice. For a cold window the
   second request always misses. groupcache never asks two peers; its hot
   copies are local. Change step 3:
   - ask rank 1 alone;
   - when the window has copies, the owner answers with a short redirect to
     one of ranks 2 to k, chosen by the owner, instead of the bytes;
   - the reader then reads from that rank.
   A cold window then costs one request. A hot window costs one small extra
   round trip and its bytes travel once. The owner already counts hosts per
   window, so it knows which windows have copies. It can also balance the
   redirects.
9. **Ask rank 2 only when rank 1 may be new.** Step 4 adds a sequential round
   trip to every cold miss, forever. galaxycache peeks at the previous owner
   only during a warm period after start (gx galaxycache.go:967-978,
   gx peers.go:137-150). Do the same. The list from the orchestrator should
   carry when each cache joined. A reader asks rank 2 only while rank 1 joined
   within the warm period, and then asks it in parallel with rank 1. In steady
   state a cold miss goes from rank 1 straight to the store.
10. **Move the window to the new owner.** A galaxycache peek hit fills the new
    owner (gx galaxycache.go:1073-1078). In the plan, a window served by rank 2
    after a join stays there, off its owner, until it is evicted. When rank 2
    serves a read that rank 1 missed, the reader should send a keep to rank 1,
    under the normal keep rate.
11. **Never forward a read.** groupcache's handler forwards to its own idea of
    the owner, with no hop limit (gc http.go:170, gc groupcache.go:272).
    galaxycache fixed this (gx http/http.go:219-225). The plan implies that an
    owner answers only from its disk. Say so in the plan and in the model:
    a cache that is asked answers from its disk or misses.
12. **Remember dead peers.** groupcache keeps no peer health, so a dead owner
    costs every miss its full failure time (gc http.go:195-215). The plan's
    bound caps each read, but a dead owner still costs the bound on each read
    until the next list refresh. Add a short per-peer suspicion: after a few
    timeouts in a row, skip that cache as rank 1 for some seconds and go to
    rank 2 or the store at once.
13. **A peer's "I could not" must not fan out.** When mailgun's owner fails at
    its own origin load, readers do not retry the origin themselves
    (mg groupcache.go:417-419). sproutfs owners never read the store, so this
    does not arise. Keep it that way. If owner read-through is ever added, copy
    this rule.
14. **Collapse faults on one host with `DoChan`.** Concurrent faults for one
    window on one host should make one owner request and at most one store
    read. Use a singleflight keyed by window. Use `DoChan` so a fault whose
    context ends can leave (xs singleflight/singleflight.go:117-141). Run the
    shared fetch under its own context, not the first caller's. In groupcache
    the leader's cancelled context fails every waiter
    (gc groupcache.go:243, 273).

### Filling the owner

15. **(a) Owner read-through versus reader fills then keeps.** groupcache's
    owner reads the origin under singleflight, so a cold burst costs one
    origin load per key (gc groupcache.go:241-292). The plan has each reader
    read the store, then keep to the owner: a burst of N hosts costs N store
    reads and N keeps.
    The plan's choice is still right for sproutfs:
    - Read-through puts the store's 40 to 58 ms inside the owner request. The
      reader's bound would have to exceed the store's latency. The plan's
      "few ms, then race the store" would be gone, and a slow owner would
      again cost a stall.
    - Read-through makes the owner hold bytes in memory to serve them. That
      breaks "serving a peer copies nothing into memory".
    - groupcache's merge only works while requests overlap and the owner is
      healthy (gc groupcache.go:244-268, 278-291). Its worst case is the same
      N origin loads.
16. **But dedupe keeps at the owner.** In a burst the owner receives N keeps of
    the same window. The plan admits a window may sit in two regions. groupcache
    re-checks its cache inside the flight to avoid exactly this double fill
    (gc groupcache.go:244-268). The owner should drop a keep for a window it
    already holds, or has a keep in flight for, before it appends anything.
    The reader should send a keep only for a window its read found missing at
    the owner.
17. **A middle path, if the store bill or rate ever matters.** The owner can
    lend the fill. The first reader to miss is told to fill. Readers that miss
    while that fill is in flight wait at the owner for a bounded time, then go
    to the store. The owner still never reads the store. This is the lease idea
    from memcache at Facebook (Nishtala et al., NSDI 2013), not from these
    sources. It belongs with the item the plan already defers.
18. **Bound the counter's memory.** galaxycache keeps stats for at most 1024
    candidate keys (gx galaxycache.go:245). The owner's count of distinct hosts
    per window needs the same kind of bound: a fixed-size table of recent
    windows, each with a small fixed host set. Windows past the bound are
    simply not counted.

### Copies of hot windows

19. **(b) Owner-pushed copies on disk versus reader-held copies in memory.**
    All three libraries let the reader decide alone, from its own reads
    (gc groupcache.go:322-330, mg groupcache.go:483-484,
    gx galaxycache.go:1083-1111). That fits a key one process reads many times.
    In sproutfs the host's memory tier and the pager's arena already cover that
    case. What remains is a window that many hosts each read once. No reader
    can see that. Only the owner can, by counting hosts. So the plan's
    owner-side count is right, and groupcache's hot cache is the wrong model
    for this layer.
20. **groupcache's real lesson is the cap.** Its hot copies may take at most
    about one ninth of the cache (gc groupcache.go:353-367), and galaxycache's
    one eighth (gx galaxycache.go:244, 1141-1157). The plan puts no limit on
    the disk taken by copies. A storm of hot windows could push out owned cold
    windows, which costs the cluster capacity. Cap copies at a fixed share of
    each cache's disk, and refuse a keep for a copy past the cap.
21. **Copies help mainly with the owner's loss.** During a single burst the
    copies land after most hosts have read, and the owner's kernel page cache
    serves the burst anyway, as the plan says. Copies pay off for windows that
    stay hot, and when the owner leaves. The plan's test of "a hot page has
    copies" should check both: load spread while the window stays hot, and no
    store reads after the owner leaves.
22. **Reset on idle, not on a fixed clock.** galaxycache restarts a key's
    count after a minute idle (gx hotcache.go:99-112). That matches the plan's
    "four hosts in a minute". Count over a sliding interval, so a window that
    goes quiet loses its count and its copies age out, as the plan intends.

### Things the plan already does better

- **No removal, no TTL.** mailgun's `Remove` costs one request per peer per key
  (mg groupcache.go:309-354). sproutfs pages are immutable under their
  identity, as groupcache requires (gc groupcache.go:43-47). The plan is right
  to leave purging to eviction. If purging ever comes, do it per checkpoint,
  not per page.
- **Checked answers.** No library checks what a peer sends. The plan's key
  check and SHA-256 make a wrong answer a miss.
- **A reader keeps nothing on disk.** groupcache's fallback writes a non-owned
  key into the reader's main cache (gc groupcache.go:291). The plan's "one copy
  of a cold page" avoids that.
