# CockroachDB's node-to-node RPC: lessons for the peer server — 2026-10-03

**License rule for this note.** CockroachDB's code is under the Business Source
License 1.1, which is not OSI-approved. The owner allows reading it to learn
design ideas. Nothing here is copied from it: no code, no snippets, no
near-verbatim paraphrase of code, and no pasted comments. Every idea is in this
note's own words, with `file:line` so a reader can look.

This note reads how CockroachDB moves bytes between nodes, and asks what that
teaches the host-to-host protocol of sproutfs: the page server of
[migration](../migration.md) and the stripe reads, keeps and repairs of
[the distributed disk cache plan](../../plans/disk-cache-2026-10-02.md). The
last section answers four questions: which protocol for stripe reads,
connection classes, per-peer breakers, and rate limits for bulk work.

## Sources

| Tag | Project | Commit | License |
| --- | --- | --- | --- |
| cr | CockroachDB, `~/src/cockroach` | `f39597ba133` (a fork of release 23.1; `git describe` gives `v23.1.28-61`) | BSL 1.1 (each file's header; `licenses/BSL.txt`) |
| sp | sproutfs, this repo | `a79a77d7` | the repo's own |
| go | Go standard library | go1.26.6 | BSD-3-Clause |
| grpc | grpc-go in the module cache | `v1.85.0-dev` | Apache-2.0 |

A citation such as `cr pkg/rpc/context.go:80` means that file and line at the
commit above. A bare hash such as `c65b38b5be7` is a CockroachDB commit whose
message is cited. CockroachDB's own `go.mod` pins grpc-go v1.80.0
(`cr go.mod:32`).

The checkout is a 23.1 fork. Later releases rewrote parts of `pkg/rpc`. This
note covers only what the checkout holds. A fork of Storj's DRPC exists under
the `cockroachdb` organisation on the Go module proxy; what later CockroachDB
releases do with it is not covered here. DRPC itself is MIT licensed, and its
README points to a separate package for running several RPCs at once over one
connection ([storj/drpc](https://github.com/storj/drpc)).

## gRPC between nodes

### One connection per peer and class

- A connection is keyed by target address, remote node ID and class
  (`cr pkg/rpc/context.go:471-490`). Two classes to one node are two gRPC
  client connections, so two TCP connections.
- The node ID is in the key so that a reused address does not route requests
  to the wrong node. The first heartbeat checks that the remote really is that
  node, and that its cluster ID matches (`cr pkg/rpc/heartbeat.go:118-153`).
  A connection is not handed to callers until that heartbeat returns
  (`cr pkg/rpc/context.go:343-357`).
- Every request of a class to a node shares that one connection. gRPC
  multiplexes them as HTTP/2 streams. There is no pool of connections per
  class.
- The dialer for the local node skips gRPC and calls the server in process,
  but still runs the interceptors (`cr pkg/rpc/context.go:769-790`,
  `cr pkg/rpc/nodedialer/nodedialer.go:153-160`).

### Why classes exist

There are three classes: default, system and rangefeed
(`cr pkg/rpc/connection_class.go:31-44`). The type's comment says why: gRPC
does not prioritise streams on one connection, so traffic of different
priority must go on different TCP connections
(`cr pkg/rpc/connection_class.go:20-30`).

- **System.** Traffic for the first meta range and for node liveness goes on
  its own connection (`cr pkg/rpc/connection_class.go:61-79`). Raft messages
  for those ranges get their own send queues per class
  (`cr pkg/kv/kvserver/raft_transport.go:152`). The PR that added it
  (merge `d9060dd2cc5`, PR 39172) says why. Large analytic scans pushed node
  liveness latency up until liveness failed, and with it CPU use dropped. A
  roachtest that failed every time on master passed ten runs in a row with the
  change. The same PR notes that it doubles the TCP connections between nodes.
- **Rangefeed.** Each rangefeed stream reserved the default 2 MB window. A
  feed over a large table opens a stream per range, and the memory HTTP/2
  holds for them is not accounted, so nodes ran out of memory (`89a1139b1c3`).
  The class gives those streams a 128 KiB window
  (`cr pkg/rpc/context.go:111-114`). The later fix multiplexed all ranges of a
  feed onto one stream per node, so the worst-case memory is nodes times one
  window (merge `2183af9d671`).
- **Snapshots are not separated.** A Raft snapshot is dialed on the default
  class (`cr pkg/kv/kvserver/raft_transport.go:632`). The bulk transfer shares
  a connection with foreground KV traffic. What protects foreground traffic is
  the snapshot rate limit, not the connection.

### Head-of-line blocking they hit anyway

HTTP/2 multiplexing did not keep small RPCs from waiting behind large ones.

- Heartbeats were ordinary RPCs and so subject to gRPC flow control. Large
  DistSQL streams blocked them, and healthy connections were closed
  (`c65b38b5be7`). The fix moved liveness of the connection to HTTP/2 PING
  frames, which flow control does not hold back.
- Later the heartbeat timeout was cut to 2 s and clusters under TPC-C import
  at 400 ms RTT became unstable, because Ping RPCs waited behind other RPC
  traffic. The timeout went back to 6 s (`e97fe41e72f`). The comment on the
  timeout still lists head-of-line blocking under load as a reason it is so
  high, and links an open issue to remove that blocking
  (`cr pkg/base/config.go:196-217`).

### Windows, streams and message sizes

- The stream window is 2 MB and the connection window 16 times that, capped
  at 64 MB (`cr pkg/rpc/context.go:79-110`). Both are set on server and
  client (`cr pkg/rpc/context.go:218-221`, `cr pkg/rpc/context.go:1902-1908`).
  The server comment says gRPC's defaults are too low for high-latency links.
  A value at or below 64 KiB makes gRPC size windows dynamically
  (`cr pkg/rpc/context.go:91-94`). The cap was raised to 64 MB so that
  multi-region clusters can push writes over long links (`63af0dd9f79`).
- gRPC's default stream window is 65,535 bytes (`grpc
  internal/transport/defaults.go:28-30` in older modules). Its data frames are
  16 KiB (`grpc internal/transport/http_util.go:47`).
- The server allows unlimited concurrent streams. The client default of 100
  was thought to add latency (`636448dc152`, `cr pkg/rpc/context.go:222-227`).
- Message size limits are set to the largest int32 on both sides, because a
  single key-value has no size limit. A TODO says to lower it once tested
  (`cr pkg/rpc/context.go:210-217`, `cr pkg/rpc/context.go:1881-1891`).

### Compression and codec

- The client asks for Snappy on every call; the server accepts either
  (`cr pkg/rpc/context.go:1780-1796`). It is on by default
  (`cr pkg/rpc/context.go:138`). When it was added, throughput did not change
  and network bytes dropped about 10 % (`56d6ed49dd8`). Turning it on was safe
  in a rolling upgrade only because the decompressor had shipped one release
  earlier (`807ee60619a`).
- Readers and writers are pooled (`cr pkg/rpc/snappy.go:49-72`). The writer
  is unbuffered, which saves a memcpy and a 64 KB buffer per message
  (`e44f70efa2f`). Compressed bytes carry their decompressed length, so the
  receiver allocates once instead of growing a buffer (`47379694f42`).
- They replaced gRPC's proto codec with one that calls the generated marshal
  methods directly (`cr pkg/rpc/codec.go:28-44`, `5ba160fe1e3`).
- gRPC's own request tracing is off, because it kept message copies for a
  debug page and that was very expensive for snapshots
  (`cr pkg/rpc/context.go:64-71`).
- A tracing interceptor once raised packets per operation even with tracing
  off (`b83bd4c5f8b`).

## Failure detection

### Heartbeats

- Each connection runs a loop that sends a Ping at once, then every
  `PingInterval`, 1 s (`cr pkg/rpc/context.go:2463-2617`,
  `cr pkg/base/config.go:152-156`). The timeout is 3 × `NetworkTimeout`, 6 s
  (`cr pkg/base/config.go:196-217`). `NetworkTimeout` is 2 s: a worst
  inter-region RTT plus one TCP retransmit plus a margin
  (`cr pkg/base/config.go:126-143`). Dialing gets twice that, for the TCP and
  TLS round trips (`cr pkg/base/config.go:145-150`).
- Any error ends the connection for good. gRPC is not allowed to reconnect
  underneath: the dialer dials once and reports every later dial as a
  permanent error (`cr pkg/rpc/context.go:1960-2012`). A new connection is
  made by the pool when someone next asks, and it is validated by a new first
  heartbeat (`cr pkg/rpc/context.go:2466-2472`, `00f9680e60a`).
- A goroutine also watches gRPC's connection state and wakes the loop as soon
  as the connection leaves Ready (`cr pkg/rpc/context.go:2595-2614`).
- Each Ping measures RTT and clock offset. The RTT feeds a moving average per
  node (`cr pkg/rpc/clock_offset.go:35-40`, `cr pkg/rpc/context.go:2552-2575`).
- A one-way partition is caught by dialback. The node that receives the first
  Ping dials back before it answers, and later Pings check that the reverse
  connection is healthy (`cr pkg/rpc/context.go:2637-2712`).

### Keepalives and TCP_USER_TIMEOUT

- The client sends HTTP/2 pings every 10 s, gRPC's minimum, and drops the
  connection if one is not answered in 10 s (`cr pkg/rpc/keepalive.go:55-73`).
- The server pings after 2 s idle and closes after 4 s. gRPC also sets
  `TCP_USER_TIMEOUT` from that, so the kernel drops a connection whose sends
  stay unacknowledged (`cr pkg/rpc/keepalive.go:21-53`, `74-81`). Both were
  raised from 1× to 2× after spurious closes under node load
  (`71541c33b1b`, `459ea46244f`). The comment's reasoning: the server side
  gains little from being aggressive, since the client's heartbeat decides
  recovery time.
- The server accepts pings at any rate (`cr pkg/rpc/keepalive.go:83-89`).

### Circuit breakers

- The node dialer keeps one breaker per node and class
  (`cr pkg/rpc/nodedialer/nodedialer.go:47-53`, `282-291`). One failure trips
  it (`cr pkg/rpc/breaker.go:94-102`).
- While it is open, a dial fails at once with a breaker error
  (`cr pkg/rpc/nodedialer/nodedialer.go:189-192`). Raft's sender drops the
  message instead of queuing it (`cr pkg/kv/kvserver/raft_transport.go:520`).
- After a backoff the breaker lets one attempt through. The backoff starts at
  0.5 s, grows by 1.5×, has 50 % jitter, and stops at 1 s
  (`cr pkg/rpc/breaker.go:70-92`). The cap is below Raft's election timeout,
  so that a restarted node hears from its leader before it campaigns
  (`cr pkg/rpc/breaker.go:75-81`).
- A cancelled caller never trips the breaker. The check is made before the
  dial and again after a failed one (`cr pkg/rpc/nodedialer/nodedialer.go:107-110`,
  `185-187`, `200-204`; `3c72ce9b3b7`).
- Any successful dial resets the breaker
  (`cr pkg/rpc/nodedialer/nodedialer.go:212-220`). A TODO there says the
  heartbeat health and the breakers disagree, and that DistSQL schedules on a
  node it thinks healthy and then fails on an open breaker.
- DistSQL dials around the breaker and retries, because a false open breaker
  fails a whole query (`cr pkg/sql/execinfra/outboxbase.go:32-51`,
  `66683d5c3fb`). The heartbeat loop's own comment admits there is no breaker
  at the connection level in this version (`cr pkg/rpc/context.go:2400-2410`).

## Bulk transfers: Raft snapshots

- **Handshake first.** The sender sends a header and waits. The receiver
  queues it for a reservation or rejects it before any data moves
  (`cr pkg/kv/kvserver/store_snapshot.go:1503-1530`).
- **Few at a time.** A store applies one snapshot at a time and sends two
  (`cr pkg/kv/kvserver/store_snapshot.go:60-67`). Waiters are ordered by
  priority (`cr pkg/kv/kvserver/store_snapshot.go:69-77`). Empty snapshots skip
  the queue, so they do not wait behind large ones
  (`cr pkg/kv/kvserver/store_snapshot.go:729-733`).
- **No starvation from FIFO plus timeouts.** A snapshot may spend at most
  40 % of its deadline waiting for a reservation. Otherwise each queued
  snapshot waits so long that it times out while sending, and the next one
  does the same, forever. The comment works the example through
  (`cr pkg/kv/kvserver/store_snapshot.go:1202-1305`).
- **Chunks.** Key-values are sent in batches of 256 KB, which bounds the
  sender's memory and is the unit of rate limiting
  (`cr pkg/kv/kvserver/store_snapshot.go:1190-1200`).
- **Rate.** Rebalance and recovery snapshots are limited to 32 MiB/s each by
  default, and never below 1 MiB/s, so a 512 MB range still finishes in under
  ten minutes (`cr pkg/kv/kvserver/store_snapshot.go:1140-1188`). The limiter
  counts batches per second with a burst of one batch. A byte limiter needed
  either a huge burst, which disabled it, or small waits, which were slow
  (`cr pkg/kv/kvserver/store_snapshot.go:1540-1549`). A wait happens before
  each batch (`cr pkg/kv/kvserver/store_snapshot.go:643`).
- **Static rates are awkward.** A TODO notes that the two rates make little
  sense, since both kinds compete for the receiver's semaphore and the slower
  decides (`cr pkg/kv/kvserver/store_snapshot.go:1172-1175`).
- **Flow control** is gRPC's: the stream window, 2 MB. The rate limit, not
  the window, keeps snapshots from crowding foreground traffic on the shared
  default connection.

### Other bulk paths

- Raft messages are queued per node and class, up to 10,000 each, and sent in
  batches of up to 64 MB on one long-lived stream
  (`cr pkg/kv/kvserver/raft_transport.go:41-67`, `445-466`). A full queue
  drops the message (`cr pkg/kv/kvserver/raft_transport.go:536-543`). An idle
  stream closes after a minute (`cr pkg/kv/kvserver/raft_transport.go:57`).
- A DistSQL outbox sends after 16 rows or 100 µs, whichever comes first
  (`cr pkg/sql/flowinfra/outbox.go:36-47`). Small writes are coalesced on
  purpose.

## Versions, dialing and TLS

- Each Ping carries the binary version. A peer older than the active cluster
  version is refused (`cr pkg/rpc/heartbeat.go:82-112`,
  `cr pkg/rpc/context.go:2547-2551`). The cluster version moves forward only
  once every node runs a binary that supports it, so mixed versions talk
  during a roll.
- DistSQL carries its own flow version and an oldest accepted version
  (`cr pkg/sql/execinfra/version.go:67-71`).
- Dialing ignores `HTTPS_PROXY`, because a proxy would become a bottleneck and
  a failure point (`cr pkg/rpc/context.go:1797-1804`). gRPC's retries of
  sent RPCs are off, but its comment admits that gRPC still retries an RPC
  that never reached the wire (`cr pkg/rpc/context.go:1893-1900`). Tests swap the dialer for an in-memory
  one through a knob (`cr pkg/rpc/context.go:1851-1856`).
- TLS is 1.2 or later with a fixed cipher list, and the server verifies a
  client certificate if one is given (`cr pkg/security/tls.go:32-39`,
  `125-138`).

## Lessons for sproutfs's peer server

### What sproutfs has today

- `platform.Network` frames messages over a `platform.Transport` byte stream,
  and the simulator injects faults beneath it (`sp platform/network.go:13-35`).
- A frame is a 20-byte prefix, a protobuf header and a raw payload. `Send`
  writes them in three writes, without buffering
  (`sp platform/internal/real/network.go:172-210`). `Receive` allocates the
  prefix and the header for every frame
  (`sp platform/internal/real/network.go:224`, `245`).
- A connection carries one request at a time, and the server answers each
  before reading the next (`sp vmmigrate/pagesource.go:534-558`). A destination
  memory region has four connections and keeps one for guest faults
  (`sp vmmigrate/internal/peer/peer.go:120-137`).
- The source bounds each peer host to 8 connections and 8 MiB of pages in
  flight (`sp vmmigrate/pagesource.go:42-47`). A connection over the budget is
  closed after accept (`sp vmmigrate/pagesource.go:534-541`). A request over
  the bytes budget gets `BUSY`.
- The reader reads a reply's payload with `io.ReadAll`, though the size is in
  the frame (`sp vmmigrate/internal/peer/peer.go:441-453`). The server copies
  each page into a buffer, appends it to the payload, then encodes the blob
  (`sp vmmigrate/pagesource.go:737-765`).
- The wire version must equal 1 exactly (`sp vmmigrate/internal/wire/codec.go:21`,
  `131-134`).

### (a) Stripe reads: keep the framed protocol, and fix four things in it

A stripe reply is about 90 KB. On a 25 Gb/s link that is about 30 µs of wire
time; on 10 Gb/s, about 70 µs. The plan's budget for a whole page read is 1.5 to
3 ms. Per-request overhead of a few µs does not decide the median. What decides
the tail is queueing: how many requests can be in flight to each holder, and
what a small request waits behind.

Against that, CockroachDB's experience argues against gRPC here:

- **It brings head-of-line blocking back.** CockroachDB put everything for a
  class on one HTTP/2 connection and spent years working around small RPCs
  waiting behind large ones (`c65b38b5be7`, `e97fe41e72f`, merge
  `d9060dd2cc5`, `cr pkg/base/config.go:212-215`). Its fix was more TCP
  connections. The page protocol already has the end state: one request per
  connection, several connections per peer.
- **It needs tuning before it fits.** A 90 KB reply is larger than gRPC's
  default 64 KiB stream window, so on a fresh stream the sender stops for a
  window update. CockroachDB set 2 MB windows, unlimited streams, unlimited
  message sizes, a custom codec, pooled compressors, and turned tracing off.
  Each was a separate fix.
- **It copies, and cannot `sendfile`.** A message is marshalled into a buffer,
  cut into 16 KiB frames, and gathered again on the other side. The plan's
  [serving copies nothing](../properties/serving-a-peer-copies-nothing.md)
  property cannot hold over gRPC. Over the framed protocol it can.
- **It hides memory.** HTTP/2 windows are memory nobody accounts. CockroachDB
  ran out of it with many streams (`89a1139b1c3`). sproutfs bounds bytes in
  flight per peer, which is the better design.
- **It goes around the simulator.** gRPC owns framing, dialing and retries.
  CockroachDB needed a knob to give it an in-memory dialer
  (`cr pkg/rpc/context.go:1851-1856`). sproutfs's simulator works at the frame
  level, beneath `platform.Network`. gRPC over `platform.Transport` would skip
  it.

Go's `net/http` with HTTP/1.1 is the same model as the framed protocol: one
request per connection and a pool. It can `sendfile` a response on plain TCP
(`go src/net/http/server.go:589`). It adds text header parsing and gains
nothing else. Its client keeps only 2 idle connections per host by default
(`go src/net/http/transport.go:61`), so a burst of six-way fan-out churns
connections unless that is raised. HTTP/2 through `net/http` brings back the
shared windows.

So extend the framed protocol. Four changes matter more than the choice:

1. **More than one request in flight per peer.** The plan says each reader
   "uses one pooled connection per peer". With one request per connection,
   that serialises every stripe a host asks of one holder: a read-ahead run
   and a concurrent fault queue behind each other at all six holders, and the
   slowest decides. Keep a small pool per peer per class, as `peer.Source`
   does, and bound requests in flight per peer by count and bytes. Do not
   multiplex requests on one connection: replies in order bring head-of-line
   blocking back, and replies out of order are HTTP/2 again.
2. **One write per frame.** Write the prefix, the header and a small payload
   with one vectored write (`net.Buffers`). Today a request costs two writes
   and may cost two packets, since Go sets `TCP_NODELAY`. CockroachDB treated
   packets per operation as a regression to fix (`b83bd4c5f8b`), and its
   DistSQL outbox coalesces on purpose (`cr pkg/sql/flowinfra/outbox.go:36-37`).
3. **No needless allocation on receive.** Pool the prefix and header buffers.
   Read a payload into a buffer of its known size, as CockroachDB did for
   compressed messages (`47379694f42`). `io.ReadAll` grows by doubling and
   copies each time.
4. **A file-range payload.** Let `platform.Frame` carry a file range, so that
   `frameConn.Send` hands the TCP connection a reader it can `sendfile`. Today
   `Send` wraps the payload in a section reader, which never takes that path
   (`sp platform/internal/real/network.go:203`). This is step 8 of the plan.

Also bound the cache listener's payload to the largest stripe-run reply. The
network's default allows 1 GiB (`sp platform/internal/real/network.go:43`).

### (b) Connection classes: separate by connection and by queue

The page protocol already separates two classes for one memory region: guest
faults keep one connection the stream cannot take
(`sp vmmigrate/internal/peer/admission.go:46-58`, `peer.go:120-137`). That is
CockroachDB's lesson applied at the right size. Three gaps remain.

1. **The source's budget does not know classes.** It counts 8 connections per
   peer host across all memory regions and kinds of request
   (`sp vmmigrate/pagesource.go:561-574`). Three memory regions streaming at
   once hold 9 connections' worth of demand, and a fault's new dial can be
   closed. Put the class in each request, and budget per peer and class, so
   bulk work can never refuse a fault. The cluster cache adds stripe reads,
   keeps and repairs to the same server; give them classes too.
2. **Classes for the cluster cache.** Use three: *fault* (a guest waits:
   post-copy faults, stripe reads for a fault, claims), *bulk read* (post-copy
   stream, resident listings, pull, read-ahead), and *bulk write* (keeps and
   repairs). This mirrors system, default and a snapshot class that
   CockroachDB lacks. CockroachDB runs snapshots on the default class
   (`cr pkg/kv/kvserver/raft_transport.go:632`) and relies only on a rate
   limit. sproutfs should do both.
3. **Separate connections do not separate the server's queues or the NIC.**
   CockroachDB's heartbeats waited under load even on their own class
   (`cr pkg/base/config.go:196-217`). A fault's stripe read that lands in a FIFO
   disk queue behind repairs waits just the same. The disk limiter must serve
   the fault class first. On the wire, TCP shares a congested link per flow,
   so three bulk flows take three quarters of it from one fault flow. Pace bulk
   work (section (d)), do not count on flows.

Do not grow connection counts without bound. CockroachDB's classes doubled its
connections (merge `d9060dd2cc5`). With six holders, three classes and a few
connections each, a host keeps tens of connections per peer. Close idle bulk
connections; keep the fault pool warm. Today a memory region trims to one idle
connection (`sp vmmigrate/internal/peer/peer.go:369-387`). For the cache, a
peer that just served a fault will likely serve the next, so keep a small warm
pool instead.

### (c) Marking down versus breakers

The plan's marking-down is a breaker per peer with a slow probe. CockroachDB
suggests five changes.

1. **A cancelled request is not a timeout.** A reader cancels the stripe
   requests it no longer needs once k have arrived, and cancels again when it
   hedges to the store. CockroachDB never trips on the caller's own
   cancellation (`cr pkg/rpc/nodedialer/nodedialer.go:107-110`, `200-204`).
   Count only a request that reached its own deadline while the reader still
   wanted it. Otherwise every healthy sixth holder accrues "timeouts" just by
   being slowest.
2. **Mark down for hard failures, not for being slow.** Erasure coding
   already hides a slow holder. Marking it down removes a stripe the reader
   could have used. Use a per-request deadline well above the hedge bound
   (hundreds of ms), and keep refused and reset connections as instant marks.
3. **Probe faster.** CockroachDB retries a dead peer every 0.5 to 1 s
   (`cr pkg/rpc/breaker.go:70-92`). The plan starts at 10 s and grows to 60 s.
   In a rolling restart, a host that is back stays out of every reader's
   reads for up to a minute while the next host goes down, so the code's two
   spares are spent on hosts that are fine. A probe is one dial and one tiny
   request. Start at 1 s and cap near 10 s, with jitter.
4. **Validate identity, not just liveness.** CockroachDB checks the node ID on
   the first heartbeat, so a reused address cannot answer for the wrong node
   (`cr pkg/rpc/heartbeat.go:137-153`). A cache's identity is random and a new
   pod can take an old address. Put the expected cache identity in every
   stripe request and in the probe. A mismatch means the list is stale. It is
   a miss, not a down host.
5. **The server's own refusals are not failures.** Today a connection over the
   per-peer budget is closed after accept
   (`sp vmmigrate/pagesource.go:534-541`). A reader would see a reset and mark
   a busy host down. Answer `BUSY` instead, and never count `BUSY` as a
   failure. The same holds for a wire version the server does not speak.

Keep two things the plan has that CockroachDB lacks: the cap of a fifth of the
list, and one down state per host rather than one per class. CockroachDB keeps
a breaker per class (`cr pkg/rpc/nodedialer/nodedialer.go:52`) and has a TODO
about them disagreeing with its heartbeats
(`cr pkg/rpc/nodedialer/nodedialer.go:212-217`). A down host is down for every
class.

The post-copy source must stay outside marking-down. It holds pages that exist
nowhere else, and migration's rule is to ask until it answers or says it is
gone ([migration](../migration.md)).

Two smaller points:

- **Dead pooled connections.** Host sockets set only a 30 s TCP keepalive
  (`sp platform/internal/real/network.go:56`). A pooled connection to a host
  that vanished costs one request deadline before anyone notices. Set
  `TCP_USER_TIMEOUT` on host-to-host sockets, as gRPC does for CockroachDB's
  server (`cr pkg/rpc/keepalive.go:21-41`), so the kernel drops it.
- **Versions in a roll.** The wire version must match exactly. A rolling
  upgrade that bumps it makes old and new hosts unable to read each other's
  stripes. Every read becomes a store read for the length of the roll.
  CockroachDB lets mixed binaries talk, and shipped its decompressor a release
  before turning compression on (`807ee60619a`). Accept the current and the
  previous version, publish each host's versions in the list of caches, and
  treat a version the peer does not speak as a miss.

### (d) Rate limits for bulk work

1. **Limit in requests, sized in bytes, burst of one.** CockroachDB found a
   byte bucket either useless (big burst) or slow (small waits), and counts
   256 KB batches instead (`cr pkg/kv/kvserver/store_snapshot.go:1540-1549`).
   sproutfs's unit is already a bounded request (one 2 MiB page, or a run of
   stripes). Take one token per request, weighted by its size, with a burst of
   one request.
2. **Drop, do not queue, what can be dropped.** CockroachDB showed that a FIFO
   with timeouts starves everyone (`cr pkg/kv/kvserver/store_snapshot.go:1202-1305`),
   and its Raft queue drops when full (`cr pkg/kv/kvserver/raft_transport.go:536-543`).
   The plan already drops keeps and repairs when the queue or rate is full.
   Keep it so. Never let a keep wait.
3. **One budget per host for background bytes, with priorities.** Post-copy
   pages no checkpoint has, then the rest of the post-copy stream, then keeps
   from publications, then repairs. The first cannot be dropped; it waits. The
   last two are dropped. CockroachDB's two separate static rates turned out to
   make little sense together (`cr pkg/kv/kvserver/store_snapshot.go:1172-1175`).
   One budget with an order avoids that.
4. **Do not give the post-copy stream a fixed rate.** How fast it runs decides
   how long the source must stay up and how long a drain takes. CockroachDB
   needed a floor on its rate so a snapshot cannot run for too long
   (`cr pkg/kv/kvserver/store_snapshot.go:1140-1148`). Let the stream use
   what faults leave, and slow it when the fault class has requests waiting.
5. **Bound the reader too.** The source bounds bytes in flight per peer. A
   restore that faults thousands of windows asks six holders for each, so the
   reader also needs one global bound on stripe bytes in flight, or its memory
   grows with the restore. This is the lesson of CockroachDB's rangefeed
   memory (`89a1139b1c3`).

### Things sproutfs already does better

- **Liveness from real requests.** sproutfs has no separate heartbeat to be
  blocked. A request's own outcome is the evidence. CockroachDB's heartbeat
  timeout is 6 s because its Pings wait behind data.
- **Bounded messages.** Every page reply is bounded and checked
  (`sp vmmigrate/internal/peer/peer.go:441-453`). CockroachDB runs with no
  message size limit.
- **Bytes in flight per peer.** CockroachDB's HTTP/2 window memory is not
  accounted. sproutfs counts what each peer holds.
- **A checked payload.** CRC32C on page replies and SHA-256 on envelopes.
  CockroachDB relies on TLS or TCP for integrity on the wire.
