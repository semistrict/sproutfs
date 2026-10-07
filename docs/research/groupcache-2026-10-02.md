# groupcache and its descendants — 2026-10-02

Research for [the distributed disk cache plan](../../plans/disk-cache-2026-10-02.md).

## Sources

| Tag | Project | Commit | Date | License (from its LICENSE file) |
| --- | --- | --- | --- | --- |
| gc | github.com/golang/groupcache, `~/src/groupcache` | `2c02b8208cf8c02a3e358cb1d9b60950647543fc` | 2024-11-29 | Apache-2.0 |
| mg | github.com/mailgun/groupcache (v2), `~/src/mailgun-groupcache` | `90b5ebf376fe71aa1886fd48bce61c065dcf2a80` | 2024-12-30 | Apache-2.0 |
| gx | github.com/vimeo/galaxycache, `~/src/galaxycache` | `df1970c32c929f9a3b5c105a8ea9c6b0e145dd99` | 2026-02-10 | Apache-2.0 |
| xs | golang.org/x/sync, `~/src/x-sync` | `36f2d70ecde9857dc4e363242a1369c7645d6770` | 2026-09-23 | BSD-3-Clause |

`gc groupcache.go:272` means that file and line at the commit above. Nothing
here is a dependency.

## groupcache

### Who owns a key

- The ring is `consistenthash.Map`. Each peer adds `replicas` points. Point `i`
  of peer `p` is `hash(strconv.Itoa(i) + p)` (gc consistenthash/consistenthash.go:53-62).
- The default hash is CRC-32 IEEE (gc consistenthash/consistenthash.go:41-43),
  with 50 points per peer (gc http.go:36, 107-109).
- A key's owner is the first point at or after `hash(key)`, wrapping at the
  end, found by binary search (gc consistenthash/consistenthash.go:65-80).
- Two points with the same hash overwrite each other; the last peer added wins
  (gc consistenthash/consistenthash.go:58). Two processes that add the same
  peers in a different order can disagree on an owner.
- There are no weights.
- Discovery is not part of the library. The caller passes the full peer list
  to `HTTPPool.Set`, which builds a new ring (gc http.go:119-128).
- When the list changes, nothing moves. Keys whose owner changed miss at the
  new owner, which loads them from the origin.

### The read path

`Group.Get` (gc groupcache.go:212-238):

1. Look in `mainCache`, then `hotCache` (gc groupcache.go:334-344).
2. On a miss, enter `load`, under a `singleflight.Group` keyed by the key
   (gc groupcache.go:241-243).
3. Inside the flight, look in the caches again. Singleflight only merges
   overlapping calls, so a later call would otherwise load again and count its
   bytes twice (gc groupcache.go:244-268).
4. Ask `PickPeer` for the owner. If it is another process, fetch from it
   (gc groupcache.go:272-277).
5. If this process is the owner, or the peer fetch failed for any reason, call
   the local `Getter` and put the value in `mainCache`
   (gc groupcache.go:284-291).

The HTTP handler calls `group.Get` for the requested key (gc http.go:168-170).
On the owner, `PickPeer` returns "self" (gc http.go:136-139), so the owner
reads through to the origin on a miss. Its singleflight merges every
concurrent miss for that key, local or from any peer. A burst of N peers
asking for one cold key costs the origin one load if all N requests reach the
owner while the first load is in flight (gc README.md:23-29,
gc groupcache.go:75-82).

Limits of that bound:

- A request that cannot reach the owner loads from the origin itself
  (gc groupcache.go:278-284).
- Any peer error falls back to a local load. If the owner's origin load fails,
  every requester retries the origin.
- A non-owner that falls back puts the value in its own `mainCache`
  (gc groupcache.go:291). After an owner outage, keys sit in the wrong
  processes' main caches.
- If the receiving peer's list says another peer owns the key, it forwards
  again (gc http.go:170 into gc groupcache.go:272). There is no hop count, so
  two peers with different lists can bounce a request.

### singleflight in groupcache

- `Do` stores one in-flight call per key. Later callers wait on its
  `WaitGroup` and get the same value and error (gc singleflight/singleflight.go:41-63).
- A waiter has no context and cannot give up (gc singleflight/singleflight.go:46-49).
- The leader runs the fetch with its own context (gc groupcache.go:243, 273,
  284). If the leader's context is cancelled, every waiter gets that error.

### hotCache

- `hotCache` holds keys this process does not own but that seem popular.
  The comment's reason is that for a popular key the owner's "network card
  could become the bottleneck" (gc groupcache.go:155-162). The comment says it is used sparingly, to keep
  the number of distinct keys in the cluster high.
- A value fetched from a peer is put in `hotCache` with probability 1 in 10
  (gc groupcache.go:322-330). A TODO says it should use the owner's reported
  QPS (gc groupcache.go:319-321). The response has a `minute_qps` field for
  this (gc groupcachepb/groupcache.proto:28), but the server never sets it
  (gc http.go:177).
- Both caches share one byte budget (gc groupcache.go:147). On overflow, the
  victim is `hotCache` when it holds more than one eighth of `mainCache`'s
  bytes, and `mainCache` otherwise (gc groupcache.go:353-367). Under pressure
  the hot cache settles at about one ninth of the total.
- Each cache is a plain LRU. Nothing expires; values must be immutable
  (gc groupcache.go:43-47, gc README.md:31-34).
- The reader decides alone, from its own random draw. The owner has no say,
  and the reader does not count its reads of the key.

### Failure handling

- groupcache has no timeout. The peer request uses the caller's context and an
  `http.RoundTripper` (gc http.go:195-211).
- A dead owner costs each miss the transport's failure time, then a local
  origin load. Failed peers are not remembered. A TODO asks whether to log peer
  errors at all (gc groupcache.go:278-282).
- The only fallback is the local origin load (gc README.md:54-60).

## mailgun/groupcache

It keeps groupcache's structure and adds mutation and expiry.

- Ring: same scheme, but each point is the FNV-1 64-bit hash of the hex MD5 of
  `i + peer` (mg consistenthash/consistenthash.go:44-46, 59). The changelog
  says MD5 spreads hosts more evenly (mg CHANGELOG:21-23).
- Hot cache: every value fetched from a peer goes into `hotCache`
  (mg groupcache.go:483-484). The changelog argues the LRU and the one-eighth
  eviction rule are enough (mg CHANGELOG:42-44).
- Errors: the reader does not fall back to a local load when the context is
  done (mg groupcache.go:409-411, 431-435), the owner said not found
  (mg groupcache.go:413-415), or the owner's getter failed, reported as HTTP
  503 (mg groupcache.go:417-419, mg http.go:233, 314-316, mg errors.go:20-22).
  The last case keeps an origin failure from turning into N origin loads.
- TTL: a value may carry an expiry. The LRU drops an expired entry on read
  (mg lru/lru.go:97-98). An already expired peer reply is an error
  (mg groupcache.go:473-479).
- `Set` writes to the owner, and optionally to the local hot cache
  (mg groupcache.go:279-305).
- `Remove` removes from the owner, then from every peer in parallel
  (mg groupcache.go:309-354): best effort, one request per peer per key. It
  holds the singleflight lock so no load is in flight while the local entry
  goes (mg groupcache.go:538-549, mg singleflight/singleflight.go:74-81).
- No replication.

## galaxycache

A rewrite of groupcache with no global state. It adds hot-key promotion by
QPS, peeking at the previous owner, and replica lookups in the ring.

### Ring and membership

- Same ring scheme and defaults as groupcache: CRC-32, 50 points per peer
  (gx peers.go:39, gx consistenthash/consistenthash.go:66-68). A comment says
  FNV-32a is better but the default cannot change without breaking running
  systems (gx consistenthash/consistenthash.go:28-31). Another says the point
  naming is weak but fixed for the same reason
  (gx consistenthash/map_rangefunc.go:31-34).
- Point collisions are settled by name order, so every process builds the same
  ring (gx consistenthash/map_rangefunc.go:56-66).
- Peers have an ID separate from their address (gx peers.go:173-183). The
  ring is built from IDs and rebuilt on every change (gx peers.go:423-448).
- A Kubernetes watcher adds a pod when it is ready and has an IP, and removes
  it otherwise (gx k8swatch/k8swatch.go:21-31). A process removes itself from
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
- The commit that added `GetReplicated` says some uses need several owners
  (gx commit 2e38d0b). The `Galaxy` never calls it; it uses one owner per key
  (gx peers.go:154-162).
- Because replica `i` hashes a different string, a key's second replica is
  not the peer that would own the key if the first left. That successor is the
  next point after `hash(key)`.

### Owner read-through, without forwarding

- A `Galaxy.Get` miss runs under singleflight. If another peer owns the key,
  it fetches from it; otherwise it calls the backend and fills `mainCache`
  (gx galaxycache.go:904-1001).
- A peer's not-found is final. Any other peer error falls back to the backend
  (gx galaxycache.go:963-966, 816).
- The servers call `GetWithOptions` with `FetchModeNoPeerBackend`
  (gx http/http.go:219-225, gx grpc/grpcserver.go:59), so the receiving peer
  answers from its cache or its backend and never forwards.

### Peeking at the previous owner

- For `WarmTime` after start, when a process owns a key and misses, it first
  asks the peer that would own the key without it
  (gx galaxycache.go:967-978, gx peers.go:137-150).
- That "peek map" is the ring without self (gx peers.go:442-447). A peek
  answers from memory only, never from the backend
  (gx galaxycache.go:861-867, gx grpc/grpcserver.go:86).
- A peek has its own short deadline; the config suggests 2 to 10 ms on a
  local network (gx galaxycache.go:433-436, 1047-1051).
- A peek hit goes into the new owner's `mainCache`
  (gx galaxycache.go:1073-1078), so the value moves to its new owner on first
  use.
- Peeking at peers that are leaving is a TODO (gx peers.go:62-70,
  gx galaxycache.go:439-441). The commit says it targets startup only
  (gx commit bfde671).

### Hot-key promotion

- Every value carries a `keyStats` with a count and a start time
  (gx hotcache.go:55-97). QPS is count over age, with a 100 ms floor on age
  (gx hotcache.go:123-135).
- A touch after more than `resetIdleStatsAge` (default one minute) restarts
  the count (gx hotcache.go:99-112, gx galaxycache.go:247), so a key hot in
  bursts does not look cold.
- Keys this process does not own keep their stats, without data, in a
  candidate LRU of 1024 entries (gx galaxycache.go:245, 268,
  gx typed_caches.go:28-49). A key evicted from the hot cache goes back to the
  candidates with its stats (gx galaxycache.go:280).
- After each peer fetch, the promoter decides whether the value enters the hot
  cache (gx galaxycache.go:1083-1111). The galaxy-wide stats it sees are
  refreshed at most once a second (gx hotcache.go:26-53).
- The default promoter promotes when the key's QPS is at least that of the
  least recently used hot entry (gx promoter/promoter.go:68-77). While the hot
  cache is empty that QPS is zero (gx hotcache.go:36-43), so every fetched
  key is promoted until it fills.
- Other promoters: 1 in N at random (gx promoter/promoter.go:57-66), and "seen
  before" (gx promoter/promoter.go:79-89).
- Hot is one eighth of the budget by default (gx galaxycache.go:244), with
  groupcache's eviction rule (gx galaxycache.go:1141-1157).
- All counting is local; a process never learns how many others read a key.

### TTL

- A backend may return an expiry. `WithGetTTL` and `WithPeekTTL` cap it, with
  random jitter so values filled together do not expire together
  (gx galaxycache.go:583-608, 1003-1024).

## x/sync singleflight

- `Do` merges concurrent calls for one key and reports whether the result was
  shared (xs singleflight/singleflight.go:86-115).
- `DoChan` returns a channel, so a waiter can select on its own context and
  leave while the call keeps running (xs singleflight/singleflight.go:117-141).
- A panic in the function is re-raised in every waiter
  (xs singleflight/singleflight.go:163-171).
- `Forget` drops the in-flight entry, so the next call starts fresh
  (xs singleflight/singleflight.go:207-214).
- It does not cache, which is why groupcache re-checks its cache inside the
  flight (gc groupcache.go:244-268).

## Lessons for the sproutfs plan

### Who owns a page

1. **Keep rendezvous.** When the owner leaves, the old rank 2 must become the
   owner, so copies on "ranks 2 to k" survive. Rendezvous gives that. A ring
   with replica lookups does not: galaxycache's second replica hashes a
   different string (gx consistenthash/consistenthash.go:116-134).
2. **Balance.** With 50 points per peer, each peer's share is a sum of 50
   random arcs, with a spread of about 1/√50, roughly 14 % of the mean; with
   many peers the largest share is about a third over the mean (our estimate,
   not measured). Rendezvous draws independently per window, so with millions
   of windows the shares are even. Its cost, one hash per cache per window, is
   microseconds against a 1 ms read; cache the ranks per window only if
   profiles show a need.
3. **Use a strong 64-bit hash of (cache identity, window key)**, fixed in the
   spec, since changing it moves every window. Both descendants are stuck with
   CRC-32 and weak point names (gx consistenthash/consistenthash.go:28-31,
   gx consistenthash/map_rangefunc.go:31-34).
4. **Break ties by cache identity**, so two hosts with the same list rank the
   same in any list order (gc consistenthash/consistenthash.go:58,
   gx consistenthash/map_rangefunc.go:56-66).
5. **Weighted ranks for unequal disks.** Neither library has weights. The
   standard weighted rendezvous score is `-w / ln(u)`, with `u` the hash mapped
   into (0, 1); it gives each cache a share proportional to `w` and moves only
   the windows a change touches (from the weighted rendezvous literature, not
   these sources). Take `w` from the configured disk size rounded to a coarse
   step, never from the limiter's live share, which moves with spill promises;
   every change of `w` moves windows. Report `w` in `/status` and
   `GET /caches`.
6. **Membership from readiness**, as galaxycache's watcher does
   (gx k8swatch/k8swatch.go:13-31), so a draining host stops being rank 1
   before it stops answering.

### Reading a page

7. **The parallel second request does not help hot windows.** Step 3 asks
   rank 1 and a random rank from 2 to k at once. For a hot window both send
   it, so the owner still sends every byte to every reader and the reader
   receives it twice. For a cold window the second request always misses.
   Change step 3:
   - ask rank 1 alone;
   - when the window has copies, the owner answers with a short redirect to
     one of ranks 2 to k, chosen by the owner, instead of the bytes;
   - the reader reads from that rank.
   A cold window costs one request; a hot window costs one small extra round
   trip and its bytes travel once. The owner already counts hosts per window,
   so it knows which windows have copies and can balance the redirects.
8. **Ask rank 2 only when rank 1 may be new.** Step 4 adds a sequential round
   trip to every cold miss. Like galaxycache's warm period
   (gx galaxycache.go:967-978, gx peers.go:137-150), the orchestrator's list
   should carry when each cache joined, and a reader asks rank 2, in parallel
   with rank 1, only while rank 1 is within its warm period.
9. **Move the window to the new owner.** A galaxycache peek hit fills the new
   owner (gx galaxycache.go:1073-1078). In the plan, a window served by rank 2
   after a join stays off its owner until evicted. When rank 2 serves a read
   rank 1 missed, the reader should send a keep to rank 1, under the normal
   keep rate.
10. **Never forward a read** (gc http.go:170, gc groupcache.go:272;
    gx http/http.go:219-225). State in the plan and the model that a cache
    answers from its disk or misses.
11. **Remember dead peers.** The plan's bound caps each read, but a dead owner
    costs the bound on each read until the next list refresh
    (compare gc http.go:195-215). After a few timeouts in a row, skip that
    cache as rank 1 for some seconds and go to rank 2 or the store.
12. **An owner's failure must not fan out.** sproutfs owners never read the
    store, so this does not arise. If owner read-through is added, copy
    mailgun's rule (mg groupcache.go:417-419).
13. **Collapse faults on one host with `DoChan`.** Concurrent faults for one
    window on one host should make one owner request and at most one store
    read: a singleflight keyed by window, with `DoChan` so a fault whose
    context ends can leave (xs singleflight/singleflight.go:117-141). Run the
    shared fetch under its own context, not the first caller's
    (gc groupcache.go:243, 273).

### Filling the owner

14. **Keep reader fills over owner read-through.** groupcache's owner merges
    a cold burst into one origin load (gc groupcache.go:241-292); the plan's
    readers each read the store and keep: N store reads and N keeps. Still:
    - Read-through puts the store's 40 to 58 ms inside the owner request, so
      the reader's bound would have to exceed it, losing "few ms, then race
      the store".
    - The owner would hold bytes in memory to serve them, breaking "serving a
      peer copies nothing into memory".
    - groupcache's merge works only while requests overlap and the owner is
      healthy (gc groupcache.go:244-268, 278-291); its worst case is the same
      N origin loads.
15. **Dedupe keeps at the owner.** In a burst the owner receives N keeps of
    one window. As groupcache re-checks inside the flight
    (gc groupcache.go:244-268), the owner should drop a keep for a window it
    holds or has a keep in flight for, before appending. The reader should
    keep only a window its read found missing at the owner.
16. **If store cost or rate matters, the owner can lend the fill.** The first
    reader to miss is told to fill; readers that miss during that fill wait at
    the owner for a bounded time, then go to the store. The owner never reads
    the store. This is the lease idea from memcache at Facebook (Nishtala et
    al., NSDI 2013), not these sources, and belongs with the item the plan
    defers.
17. **Bound the counter's memory**, as galaxycache bounds candidates at 1024
    (gx galaxycache.go:245): a fixed-size table of recent windows, each with a
    small fixed host set. Windows past the bound are not counted.

### Copies of hot windows

18. **Keep the owner-side count.** All three libraries let the reader decide
    from its own reads (gc groupcache.go:322-330, mg groupcache.go:483-484,
    gx galaxycache.go:1083-1111). That fits a key one process reads many
    times, which the memory tier and the pager's arena already cover. A window
    that many hosts each read once is visible only to the owner.
19. **Cap copies** at a fixed share of each cache's disk, and refuse a keep
    for a copy past it, as groupcache caps hot copies at about one ninth
    (gc groupcache.go:353-367) and galaxycache at one eighth
    (gx galaxycache.go:244, 1141-1157). Otherwise many hot windows can push
    out owned cold windows.
20. **Copies help mainly with the owner's loss.** During a single burst they
    land after most hosts have read, and the owner's page cache serves the
    burst. They pay off for windows that stay hot and when the owner leaves.
    The test of "a hot page has copies" should check both: load spread while
    hot, and no store reads after the owner leaves.
21. **Count over a sliding interval**, so a window that goes quiet loses its
    count and its copies age out; galaxycache resets after a minute idle
    (gx hotcache.go:99-112), matching the plan's "four hosts in a minute".
22. **No removal, no TTL.** sproutfs pages are immutable under their
    identity, as groupcache requires (gc groupcache.go:43-47), so purging is
    left to eviction. mailgun's `Remove` costs one request per peer per key
    (mg groupcache.go:309-354); if purging is added, do it per checkpoint.
23. No library checks what a peer sends; the plan's key check and SHA-256
    make a wrong answer a miss. groupcache's fallback writes a non-owned key
    into the reader's cache (gc groupcache.go:291); the plan's "one copy of a
    cold page" avoids that.
