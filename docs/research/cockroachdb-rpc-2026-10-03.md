# CockroachDB's node-to-node RPC: lessons for the peer server — 2026-10-03

**License.** CockroachDB's code is under the Business Source License 1.1,
which is not OSI-approved. The owner allows reading it for design ideas.
Nothing here is copied from it: no code, no near-verbatim paraphrase, no
pasted comments. Each idea has a `file:line` to look at.

This note covers how CockroachDB moves bytes between nodes, for the page
server of [migration](../migration.md) and the stripe reads, keeps and repairs
of [the distributed disk cache plan](../../plans/disk-cache-2026-10-02.md).
The last section covers the protocol for stripe reads, connection classes,
per-peer breakers, and rate limits for bulk work.

## Sources

| Tag | Project | Commit | License |
| --- | --- | --- | --- |
| cr | CockroachDB, `~/src/cockroach` | `f39597ba133` (a fork of release 23.1; `git describe` gives `v23.1.28-61`) | BSL 1.1 (each file's header; `licenses/BSL.txt`) |
| sp | sproutfs, this repo | `a79a77d7` | the repo's own |
| go | Go standard library | go1.26.6 | BSD-3-Clause |
| grpc | grpc-go in the module cache | `v1.85.0-dev` | Apache-2.0 |

`cr pkg/rpc/context.go:80` means that file and line at the commit above. A
bare hash such as `c65b38b5be7` is a CockroachDB commit whose message is
cited. CockroachDB's `go.mod` pins grpc-go v1.80.0 (`cr go.mod:32`).

Later releases rewrote parts of `pkg/rpc`; this note covers only the 23.1
checkout. A fork of Storj's DRPC exists under the `cockroachdb` organisation
on the Go module proxy; its use in later releases is not covered. DRPC is MIT
licensed, and its README points to a separate package for running several
RPCs at once over one connection ([storj/drpc](https://github.com/storj/drpc)).

## gRPC between nodes

### One connection per peer and class

- A connection is keyed by target address, remote node ID and class
  (`cr pkg/rpc/context.go:471-490`). Two classes to one node are two gRPC
  client connections, so two TCP connections.
- The node ID is in the key so a reused address does not route requests to
  the wrong node. The first heartbeat checks the remote's node ID and cluster
  ID (`cr pkg/rpc/heartbeat.go:118-153`). A connection is not handed to
  callers until that heartbeat returns (`cr pkg/rpc/context.go:343-357`).
- Every request of a class to a node shares that connection as HTTP/2
  streams. There is no pool per class.
- The dialer for the local node calls the server in process, skipping gRPC
  but running the interceptors (`cr pkg/rpc/context.go:769-790`,
  `cr pkg/rpc/nodedialer/nodedialer.go:153-160`).

### Why classes exist

There are three classes: default, system and rangefeed
(`cr pkg/rpc/connection_class.go:31-44`). gRPC does not prioritise streams on
one connection, so traffic of different priority goes on different TCP
connections (`cr pkg/rpc/connection_class.go:20-30`).

- **System.** Traffic for the first meta range and for node liveness has its
  own connection (`cr pkg/rpc/connection_class.go:61-79`), and Raft messages
  for those ranges have their own send queues per class
  (`cr pkg/kv/kvserver/raft_transport.go:152`). Per the PR that added it
  (merge `d9060dd2cc5`, PR 39172), large analytic scans pushed node liveness
  latency up until liveness failed; a roachtest that always failed on master
  passed ten runs in a row with the change. The PR notes it doubles the TCP
  connections between nodes.
- **Rangefeed.** Each rangefeed stream reserved the default 2 MB window. A
  feed over a large table opens a stream per range, HTTP/2 does not account
  that memory, and nodes ran out of memory (`89a1139b1c3`). The class gives
  those streams a 128 KiB window (`cr pkg/rpc/context.go:111-114`). A later
  fix multiplexed all ranges of a feed onto one stream per node, so the
  worst case is nodes times one window (merge `2183af9d671`).
- **Snapshots share the default class.** A Raft snapshot is dialed on the
  default class (`cr pkg/kv/kvserver/raft_transport.go:632`), sharing a
  connection with foreground KV traffic. The snapshot rate limit protects
  foreground traffic.

### Head-of-line blocking

HTTP/2 multiplexing did not keep small RPCs from waiting behind large ones.

- Heartbeats were ordinary RPCs under gRPC flow control. Large DistSQL
  streams blocked them, and healthy connections were closed (`c65b38b5be7`).
  The fix moved connection liveness to HTTP/2 PING frames, which flow control
  does not hold back.
- When the heartbeat timeout was cut to 2 s, clusters under TPC-C import at
  400 ms RTT became unstable because Ping RPCs waited behind other traffic.
  It went back to 6 s (`e97fe41e72f`). The timeout's comment still lists
  head-of-line blocking under load as a reason and links an open issue
  (`cr pkg/base/config.go:196-217`).

### Windows, streams and message sizes

- The stream window is 2 MB and the connection window 16 times that, capped
  at 64 MB (`cr pkg/rpc/context.go:79-110`), set on server and client
  (`cr pkg/rpc/context.go:218-221`, `cr pkg/rpc/context.go:1902-1908`). The
  server comment says gRPC's defaults are too low for high-latency links. A
  value at or below 64 KiB makes gRPC size windows dynamically
  (`cr pkg/rpc/context.go:91-94`). The cap was raised to 64 MB for
  multi-region writes over long links (`63af0dd9f79`).
- gRPC's default stream window is 65,535 bytes (`grpc
  internal/transport/defaults.go:28-30` in older modules). Its data frames are
  16 KiB (`grpc internal/transport/http_util.go:47`).
- The server allows unlimited concurrent streams; the client default of 100
  was thought to add latency (`636448dc152`, `cr pkg/rpc/context.go:222-227`).
- Message size limits are the largest int32 on both sides, because a single
  key-value has no size limit. A TODO says to lower it once tested
  (`cr pkg/rpc/context.go:210-217`, `cr pkg/rpc/context.go:1881-1891`).

### Compression and codec

- The client asks for Snappy on every call; the server accepts either
  (`cr pkg/rpc/context.go:1780-1796`). It is on by default
  (`cr pkg/rpc/context.go:138`). When added, throughput did not change and
  network bytes dropped about 10 % (`56d6ed49dd8`). Turning it on was safe in
  a rolling upgrade because the decompressor had shipped one release earlier
  (`807ee60619a`).
- Readers and writers are pooled (`cr pkg/rpc/snappy.go:49-72`). The writer
  is unbuffered, saving a memcpy and a 64 KB buffer per message
  (`e44f70efa2f`). Compressed bytes carry their decompressed length, so the
  receiver allocates once (`47379694f42`).
- A custom codec calls the generated marshal methods directly
  (`cr pkg/rpc/codec.go:28-44`, `5ba160fe1e3`).
- gRPC's request tracing is off, because it kept message copies for a debug
  page, which was very expensive for snapshots
  (`cr pkg/rpc/context.go:64-71`).
- A tracing interceptor once raised packets per operation even with tracing
  off (`b83bd4c5f8b`).

## Failure detection

### Heartbeats

- Each connection sends a Ping at once, then every `PingInterval`, 1 s
  (`cr pkg/rpc/context.go:2463-2617`, `cr pkg/base/config.go:152-156`). The
  timeout is 3 × `NetworkTimeout`, 6 s (`cr pkg/base/config.go:196-217`).
  `NetworkTimeout` is 2 s: a worst inter-region RTT plus one TCP retransmit
  plus a margin (`cr pkg/base/config.go:126-143`). Dialing gets twice that,
  for the TCP and TLS round trips (`cr pkg/base/config.go:145-150`).
- Any error ends the connection. gRPC may not reconnect underneath: the
  dialer dials once and reports every later dial as a permanent error
  (`cr pkg/rpc/context.go:1960-2012`). The pool makes a new connection on the
  next request, validated by a new first heartbeat
  (`cr pkg/rpc/context.go:2466-2472`, `00f9680e60a`).
- A goroutine watches gRPC's connection state and wakes the loop when the
  connection leaves Ready (`cr pkg/rpc/context.go:2595-2614`).
- Each Ping measures RTT and clock offset; the RTT feeds a moving average per
  node (`cr pkg/rpc/clock_offset.go:35-40`, `cr pkg/rpc/context.go:2552-2575`).
- Dialback catches a one-way partition. The node receiving the first Ping
  dials back before answering, and later Pings check the reverse connection
  (`cr pkg/rpc/context.go:2637-2712`).

### Keepalives and TCP_USER_TIMEOUT

- The client sends HTTP/2 pings every 10 s, gRPC's minimum, and drops the
  connection if one is not answered in 10 s (`cr pkg/rpc/keepalive.go:55-73`).
- The server pings after 2 s idle and closes after 4 s. gRPC sets
  `TCP_USER_TIMEOUT` from that, so the kernel drops a connection whose sends
  stay unacknowledged (`cr pkg/rpc/keepalive.go:21-53`, `74-81`). Both were
  raised from 1× to 2× after spurious closes under load (`71541c33b1b`,
  `459ea46244f`). The comment says the server side gains little from being
  aggressive, since the client's heartbeat decides recovery time.
- The server accepts pings at any rate (`cr pkg/rpc/keepalive.go:83-89`).

### Circuit breakers

- The node dialer keeps one breaker per node and class
  (`cr pkg/rpc/nodedialer/nodedialer.go:47-53`, `282-291`). One failure trips
  it (`cr pkg/rpc/breaker.go:94-102`).
- While open, a dial fails at once with a breaker error
  (`cr pkg/rpc/nodedialer/nodedialer.go:189-192`). Raft's sender drops the
  message (`cr pkg/kv/kvserver/raft_transport.go:520`).
- After a backoff the breaker lets one attempt through. The backoff starts at
  0.5 s, grows by 1.5×, has 50 % jitter, and stops at 1 s
  (`cr pkg/rpc/breaker.go:70-92`). The cap is below Raft's election timeout,
  so a restarted node hears from its leader before it campaigns
  (`cr pkg/rpc/breaker.go:75-81`).
- A cancelled caller never trips the breaker; the check is made before the
  dial and after a failed one (`cr pkg/rpc/nodedialer/nodedialer.go:107-110`,
  `185-187`, `200-204`; `3c72ce9b3b7`).
- Any successful dial resets the breaker
  (`cr pkg/rpc/nodedialer/nodedialer.go:212-220`). A TODO there says the
  heartbeat health and the breakers disagree: DistSQL schedules on a node it
  thinks healthy and then fails on an open breaker.
- DistSQL dials around the breaker and retries, because a false open breaker
  fails a whole query (`cr pkg/sql/execinfra/outboxbase.go:32-51`,
  `66683d5c3fb`). The heartbeat loop's comment says this version has no
  connection-level breaker (`cr pkg/rpc/context.go:2400-2410`).

## Bulk transfers: Raft snapshots

- **Handshake first.** The sender sends a header and waits. The receiver
  queues it for a reservation or rejects it before any data moves
  (`cr pkg/kv/kvserver/store_snapshot.go:1503-1530`).
- **Few at a time.** A store applies one snapshot at a time and sends two
  (`cr pkg/kv/kvserver/store_snapshot.go:60-67`). Waiters are ordered by
  priority (`cr pkg/kv/kvserver/store_snapshot.go:69-77`). Empty snapshots
  skip the queue (`cr pkg/kv/kvserver/store_snapshot.go:729-733`).
- **Queue wait limit.** A snapshot may spend at most 40 % of its deadline
  waiting for a reservation. Without that, each queued snapshot waits so long
  that it times out while sending, and so does the next, indefinitely
  (`cr pkg/kv/kvserver/store_snapshot.go:1202-1305`).
- **Batches.** Key-values are sent in 256 KB batches, which bounds the
  sender's memory and is the unit of rate limiting
  (`cr pkg/kv/kvserver/store_snapshot.go:1190-1200`).
- **Rate.** Rebalance and recovery snapshots are limited to 32 MiB/s each by
  default, never below 1 MiB/s, so a 512 MB range finishes in under ten
  minutes (`cr pkg/kv/kvserver/store_snapshot.go:1140-1188`). The limiter
  counts batches per second with a burst of one batch. A byte limiter needed
  either a huge burst, which disabled it, or small waits, which were slow
  (`cr pkg/kv/kvserver/store_snapshot.go:1540-1549`). A wait happens before
  each batch (`cr pkg/kv/kvserver/store_snapshot.go:643`).
- **Two static rates.** A TODO notes the two rates make little sense, since
  both kinds compete for the receiver's semaphore and the slower decides
  (`cr pkg/kv/kvserver/store_snapshot.go:1172-1175`).
- **Flow control** is gRPC's 2 MB stream window. The rate limit keeps
  snapshots from crowding foreground traffic on the shared connection.

### Other bulk paths

- Raft messages are queued per node and class, up to 10,000 each, and sent in
  batches of up to 64 MB on one long-lived stream
  (`cr pkg/kv/kvserver/raft_transport.go:41-67`, `445-466`). A full queue
  drops the message (`cr pkg/kv/kvserver/raft_transport.go:536-543`). An idle
  stream closes after a minute (`cr pkg/kv/kvserver/raft_transport.go:57`).
- A DistSQL outbox sends after 16 rows or 100 µs, whichever is first, to
  coalesce small writes (`cr pkg/sql/flowinfra/outbox.go:36-47`).

## Versions, dialing and TLS

- Each Ping carries the binary version. A peer older than the active cluster
  version is refused (`cr pkg/rpc/heartbeat.go:82-112`,
  `cr pkg/rpc/context.go:2547-2551`). The cluster version moves forward only
  once every node runs a binary that supports it, so mixed versions talk
  during a roll.
- DistSQL carries its own flow version and an oldest accepted version
  (`cr pkg/sql/execinfra/version.go:67-71`).
- Dialing ignores `HTTPS_PROXY`, because a proxy would be a bottleneck and a
  failure point (`cr pkg/rpc/context.go:1797-1804`). gRPC's retries of sent
  RPCs are off, but the comment notes gRPC still retries an RPC that never
  reached the wire (`cr pkg/rpc/context.go:1893-1900`). Tests swap the dialer
  for an in-memory one through a knob (`cr pkg/rpc/context.go:1851-1856`).
- TLS is 1.2 or later with a fixed cipher list, and the server verifies a
  client certificate if one is given (`cr pkg/security/tls.go:32-39`,
  `125-138`).

## Lessons for sproutfs's peer server

### What sproutfs has today

- `platform.Network` frames messages over a `platform.Transport` byte stream,
  and the simulator injects faults beneath it (`sp platform/network.go:13-35`).
- A frame is a 20-byte prefix, a protobuf header and a raw payload. `Send`
  writes them in three unbuffered writes
  (`sp platform/internal/real/network.go:172-210`). `Receive` allocates the
  prefix and the header for every frame
  (`sp platform/internal/real/network.go:224`, `245`).
- A connection carries one request at a time, and the server answers each
  before reading the next (`sp vmmigrate/pagesource.go:534-558`). A
  destination memory region has four connections and keeps one for guest
  faults (`sp vmmigrate/internal/peer/peer.go:120-137`).
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

### (a) Stripe reads: keep the framed protocol, with four changes

A stripe reply is about 90 KB: about 30 µs of wire time at 25 Gb/s, 70 µs at
10 Gb/s, against the plan's 1.5 to 3 ms budget for a page read. Per-request
overhead of a few µs does not decide the median. The tail depends on how many
requests can be in flight to each holder and what a small request waits
behind.

Reasons not to use gRPC here:

- **Head-of-line blocking.** CockroachDB spent years working around small RPCs
  waiting behind large ones on one HTTP/2 connection (`c65b38b5be7`,
  `e97fe41e72f`, merge `d9060dd2cc5`, `cr pkg/base/config.go:212-215`), and
  fixed it with more TCP connections. The page protocol already has one
  request per connection and several connections per peer.
- **Tuning.** A 90 KB reply exceeds gRPC's default 64 KiB stream window, so a
  fresh stream stops for a window update. CockroachDB needed separate fixes
  for windows, stream limits, message sizes, codec, compressor pooling and
  tracing.
- **No `sendfile`.** A message is marshalled into a buffer and split into
  16 KiB frames, so the
  [serving copies nothing](../properties/serving-a-peer-copies-nothing.md)
  property cannot hold over gRPC.
- **Unaccounted memory.** HTTP/2 windows are memory nobody accounts
  (`89a1139b1c3`). sproutfs bounds bytes in flight per peer.
- **The simulator.** gRPC owns framing, dialing and retries; CockroachDB
  needed a knob for an in-memory dialer (`cr pkg/rpc/context.go:1851-1856`).
  gRPC over `platform.Transport` would bypass sproutfs's frame-level
  simulator.

Go's `net/http` with HTTP/1.1 is the framed protocol's model (one request per
connection, a pool) and can `sendfile` on plain TCP
(`go src/net/http/server.go:589`), but adds only text header parsing. Its
client keeps 2 idle connections per host by default
(`go src/net/http/transport.go:61`), so six-way fan-out churns connections
unless that is raised. HTTP/2 through `net/http` brings back shared windows.

Changes to the framed protocol:

1. **More than one request in flight per peer.** The plan's "one pooled
   connection per peer", with one request per connection, queues a read-ahead
   run and a concurrent fault behind each other at all six holders. Keep a
   small pool per peer per class, as `peer.Source` does, bounded by count and
   bytes. Do not multiplex on one connection: in-order replies bring back
   head-of-line blocking, and out-of-order replies are HTTP/2 again.
2. **One write per frame.** Write the prefix, header and a small payload with
   one vectored write (`net.Buffers`). Today a request costs two writes and
   may cost two packets, since Go sets `TCP_NODELAY`. CockroachDB treated
   packets per operation as a regression (`b83bd4c5f8b`) and coalesces DistSQL
   writes (`cr pkg/sql/flowinfra/outbox.go:36-37`).
3. **No needless allocation on receive.** Pool the prefix and header buffers,
   and read a payload into a buffer of its known size, as CockroachDB does
   (`47379694f42`), instead of `io.ReadAll`, which grows by doubling.
4. **A file-range payload.** Let `platform.Frame` carry a file range that
   `frameConn.Send` can `sendfile`. Today `Send` wraps the payload in a section
   reader, which never takes that path
   (`sp platform/internal/real/network.go:203`). This is step 8 of the plan.

Also bound the cache listener's payload to the largest stripe-run reply,
instead of the network's default 1 GiB
(`sp platform/internal/real/network.go:43`).

### (b) Connection classes: separate by connection and by queue

The page protocol already reserves a connection per memory region for guest
faults (`sp vmmigrate/internal/peer/admission.go:46-58`, `peer.go:120-137`).
Three gaps remain.

1. **The source's budget does not know classes.** It counts 8 connections per
   peer host across all memory regions and request kinds
   (`sp vmmigrate/pagesource.go:561-574`), so three memory regions streaming
   at once want 9, and a fault's new dial can be closed. Put the class in each
   request and budget per peer and class, so bulk work never refuses a fault.
2. **Three classes for the cluster cache:** *fault* (post-copy faults, stripe
   reads for a fault), *bulk read* (post-copy stream, resident listings, pull,
   read-ahead), and *bulk write* (keeps and repairs). CockroachDB has no
   snapshot class and protects foreground traffic with a rate limit only
   (`cr pkg/kv/kvserver/raft_transport.go:632`). Use both a class and a rate
   limit.
3. **Separate connections do not separate the server's queues or the NIC.**
   CockroachDB's heartbeats waited under load on their own class
   (`cr pkg/base/config.go:196-217`). The disk limiter must serve the fault
   class first. TCP shares a congested link per flow, so three bulk flows take
   three quarters of it from one fault flow; pace bulk work (section (d)).

Bound connection counts. CockroachDB's classes doubled its connections (merge
`d9060dd2cc5`); six holders, three classes and a few connections each make
tens per peer. Close idle bulk connections. Today a memory region trims to one
idle connection (`sp vmmigrate/internal/peer/peer.go:369-387`); keep a small
warm fault pool instead, since a peer that served a fault will likely serve
the next.

### (c) Marking down versus breakers

The plan's marking-down is a breaker per peer with a slow probe. Five changes:

1. **A cancelled request is not a timeout.** A reader cancels stripe requests
   once k have arrived, and again when it hedges to the store. Like
   CockroachDB (`cr pkg/rpc/nodedialer/nodedialer.go:107-110`, `200-204`),
   count only a request that reached its own deadline while still wanted, or
   every healthy sixth holder accrues timeouts by being slowest.
2. **Mark down for hard failures, not slowness.** Erasure coding already hides
   a slow holder. Use a per-request deadline well above the hedge bound
   (hundreds of ms), and keep refused and reset connections as instant marks.
3. **Probe faster.** CockroachDB retries every 0.5 to 1 s
   (`cr pkg/rpc/breaker.go:70-92`); the plan starts at 10 s and grows to 60 s.
   In a rolling restart, a host that is back stays out of reads for up to a
   minute while the next goes down, spending the two spares on healthy hosts.
   Start at 1 s and cap near 10 s, with jitter.
4. **Validate identity.** CockroachDB checks the node ID on the first
   heartbeat (`cr pkg/rpc/heartbeat.go:137-153`). A new pod can take an old
   address, so put the expected cache identity in every stripe request and
   probe. A mismatch means the list is stale: a miss, not a down host.
5. **The server's refusals are not failures.** A connection over the per-peer
   budget is closed after accept (`sp vmmigrate/pagesource.go:534-541`), so a
   reader would mark a busy host down. Answer `BUSY`, never count it as a
   failure, and do the same for an unsupported wire version.

Keep the plan's cap of a fifth of the list and one down state per host.
CockroachDB keeps a breaker per class (`cr pkg/rpc/nodedialer/nodedialer.go:52`)
with a TODO about them disagreeing with its heartbeats
(`cr pkg/rpc/nodedialer/nodedialer.go:212-217`).

The post-copy source stays outside marking-down: it holds pages that exist
nowhere else, and is asked until it answers or says it is gone
([migration](../migration.md)).

- **Dead pooled connections.** Host sockets set only a 30 s TCP keepalive
  (`sp platform/internal/real/network.go:56`). Set `TCP_USER_TIMEOUT` on
  host-to-host sockets, as gRPC does for CockroachDB's server
  (`cr pkg/rpc/keepalive.go:21-41`).
- **Versions in a roll.** An exact wire version match turns every stripe read
  between old and new hosts into a store read for the length of a roll that
  bumps it. CockroachDB lets mixed binaries talk, and shipped its decompressor
  a release before enabling compression (`807ee60619a`). Accept the current
  and previous version, publish each host's versions in the list of caches,
  and treat an unsupported version as a miss.

### (d) Rate limits for bulk work

1. **Limit in requests weighted by bytes, burst of one.** CockroachDB found a
   byte bucket either useless or slow and counts 256 KB batches
   (`cr pkg/kv/kvserver/store_snapshot.go:1540-1549`). sproutfs's unit is a
   bounded request (one 2 MiB page, or a run of stripes).
2. **Drop what can be dropped.** A FIFO with timeouts starves everyone
   (`cr pkg/kv/kvserver/store_snapshot.go:1202-1305`); CockroachDB's Raft
   queue drops when full (`cr pkg/kv/kvserver/raft_transport.go:536-543`).
   The plan drops keeps and repairs when the queue or rate is full; never let
   a keep wait.
3. **One budget per host for background bytes, in priority order:** post-copy
   pages no checkpoint has (these wait), the rest of the post-copy stream,
   keeps from publications, repairs (these are dropped). CockroachDB's two
   static rates make little sense together
   (`cr pkg/kv/kvserver/store_snapshot.go:1172-1175`).
4. **No fixed rate for the post-copy stream.** Its speed sets how long the
   source must stay up and how long a drain takes. CockroachDB needed a floor
   for snapshots (`cr pkg/kv/kvserver/store_snapshot.go:1140-1148`). Let the
   stream use what faults leave, and slow it when faults are waiting.
5. **Bound the reader too.** A restore that faults thousands of windows asks
   six holders for each, so the reader needs a global bound on stripe bytes in
   flight (compare CockroachDB's rangefeed memory, `89a1139b1c3`).
