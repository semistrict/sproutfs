# FoundationDB's internal transport, and the peer server — 2026-10-03

Research for the host-to-host channel. Today it is the page server
([hosting](../hosting.md#transport), [migration](../migration.md)). It will be
renamed the peer server and will carry handoffs, fork points and the requests
of [the distributed disk cache plan](../../plans/disk-cache-2026-10-02.md):
read stripes, keep, drop, presence and probe. This note reads how FoundationDB
(FDB) moves messages between its processes, and asks what that teaches the
peer server.

## Sources

| Tag | Project | Commit | Date | License (from its LICENSE file) |
| --- | --- | --- | --- | --- |
| fdb | github.com/apple/foundationdb, `~/src/foundationdb` | `178c4e9148e5b40d25748447bff54bf402cef2f8` | 2026-09-03 | Apache-2.0 (`LICENSE` line 2) |

Apache-2.0 is OSI-approved. Nothing here is a dependency and no code is
copied. Citations are `path:line` at that commit. Short names:

| Short | Path |
| --- | --- |
| `FT.cpp` | `fdbrpc/FlowTransport.cpp` |
| `FT.h` | `fdbrpc/include/fdbrpc/FlowTransport.h` |
| `FM.cpp`, `FM.h` | `fdbrpc/FailureMonitor.cpp`, `fdbrpc/include/fdbrpc/FailureMonitor.h` |
| `LB.h`, `LB.cpp` | `fdbrpc/include/fdbrpc/LoadBalance.h`, `fdbrpc/LoadBalance.cpp` |
| `QM.h`, `QM.cpp` | `fdbrpc/include/fdbrpc/QueueModel.h`, `fdbrpc/QueueModel.cpp` |
| `rpc.h` | `fdbrpc/include/fdbrpc/fdbrpc.h` |
| `GA.h` | `fdbrpc/include/fdbrpc/genericactors.h` |
| `sim2.cpp` | `fdbrpc/sim2.cpp` |
| `knobs` | `flow/Knobs.cpp` (default values of `FLOW_KNOBS`) |

At this commit FDB's actors are C++ coroutines (`co_await`), not the older
actor compiler. The tree also has an optional gRPC path
(`fdbrpc/FlowGrpc.cpp`, and `fdbrpc/FileTransfer.cpp` behind
`FLOW_GRPC_ENABLED`). This note covers FlowTransport, which is what FDB's
roles use to talk to each other.

Our side is cited as repository paths.

## Today's peer channel, for comparison

- A destination dials the source's page server. `platform.Network` frames the
  stream and a `platform.Transport` carries the bytes (`docs/hosting.md:64-70`,
  `platform/network.go:13-35`).
- A frame is a 20-byte prefix with magic, a frame version and two sizes, then a
  protobuf header, then a raw payload (`platform/internal/real/network.go:18-23`,
  `:186-210`). The header is a protobuf `Envelope` with `wire_version`,
  `request_id`, `in_reply_to`, the message as an `Any`, and a payload
  descriptor (`vmmigrate/internal/wire/proto/sproutfs/wire/v1/*.proto:11-23`).
- Every frame must carry wire version 1 (`vmmigrate/internal/wire/codec.go:21`,
  `:132-135`). Page requests also carry `payload_format` 1
  (`vmmigrate/proto/sproutfs/migrate/v1/migrate.proto:31-32`).
- The payload has an optional CRC32C or SHA-256 (`codec.go:29-48`). The page
  source always sends CRC32C over the payload (`vmmigrate/pagesource.go:829-836`).
  The header is not covered by any checksum.
- Each connection carries one request at a time. The server answers each
  request before it reads the next (`pagesource.go:543-558`). The client
  numbers requests and checks `in_reply_to`, but never has two in flight on one
  connection (`vmmigrate/internal/peer/peer.go:310-317`, `:411-439`).
- Connections are pooled per memory region, four by default, with one kept for
  guest faults (`docs/migration.md:305-311`, `peer.go:120-137`,
  `vmmigrate/peer.go:216`).
- The source bounds each remote host to 8 connections and 8 MiB of pages in
  flight (`pagesource.go:42-47`). A connection over the budget is closed. A
  request over the byte budget is answered `BUSY` (`pagesource.go:536-541`,
  `:717-724`).
- The destination retries a run that only the source holds forever, with
  backoff from 1 ms to 100 ms, until the source says it no longer serves the VM
  or the backing closes (`docs/migration.md:78-101`, `vmmigrate/peer.go:196-197`,
  `:646-695`).
- A frame the server cannot decode, including one of another wire version,
  ends the connection with no reply (`pagesource.go:551-553`).
- The only liveness check on an idle connection is TCP keepalive, set to 30 s
  (`platform/internal/real/network.go:56`). The destination's requests carry no
  deadline of their own (`vmmigrate/peer.go:765-769`). The source gives each
  request 30 s for its local reads (`pagesource.go:48-53`, `:732`).

## FlowTransport

### Endpoints and tokens

- An endpoint is a list of addresses and a 128-bit token (`FT.h:50-60`). A
  message on the wire is addressed to a token, not to a connection.
- A few tokens are well known. Their first half is all ones and their second
  half is a small index: endpoint-not-found, ping and unauthorized
  (`FT.h:41-48`, `:67-71`). An application reserves more well-known indexes
  when it creates the transport (`FT.h:208`).
- Every other token is random (`FT.cpp:2020-2030`). Its low 32 bits index a
  slot in a table (`FT.cpp:145-154`). A lookup checks the rest of the token
  against the slot, so a stale token to a reused slot finds nothing
  (`FT.cpp:195-207`).
- The slot stores the receiver's task priority (`FT.cpp:106-114`,
  `:136-143`). Delivery runs at that priority (`FT.cpp:1332-1355`). The ping
  receiver runs at socket-read priority (`FT.cpp:326-329`), so heavy request
  work does not delay a ping's answer.
- An interface's request streams get adjacent slots under one base token, so a
  whole interface is serialized as one endpoint (`FT.cpp:156-193`,
  `FT.h:91-97`).
- A request carries its own reply endpoint (`rpc.h:760-777`). The server sends
  the reply to that token. So many requests can be in flight on one
  connection, and replies come back in any order.

### One connection per peer

- The transport keeps one `Peer` per remote address (`FT.cpp:407`,
  `:1842-1856`). The peer owns the unsent queue, the reliable packets, the
  connection state, ping latencies and counters (`FT.h:156-200`).
- One connection carries both directions. When two processes dial each other
  at once, the one with the larger address keeps its outgoing connection
  (`FT.cpp:1187-1223`).
- A connection is opened only when there is something to send
  (`FT.cpp:871-901`). A client closes an idle one after 180 s, and a peer
  nothing references is closed after 2 s (`FT.cpp:746-757`, `knobs:120-122`).

### The connect packet and versions

- The first bytes on every connection are a connect packet: its length, the
  sender's protocol version, its canonical address and a connection id
  (`FT.cpp:617-682`). The sender puts it in front of its unsent queue on every
  new connection (`FT.cpp:1138-1172`).
- Two versions are compatible when all but their lowest 16 bits match
  (`flow/protocolversion/ProtocolVersion.h.template:53`, `:61-63`).
- An incompatible peer is recorded with the time it appeared
  (`FT.cpp:1603-1636`). The process exposes that list and a trigger for it
  (`FT.h:240-244`). Very old peers are hung up on (`FT.cpp:1640-1651`).
- An incompatible connection stays open. The reader drops its packets
  (`FT.cpp:1706-1720`), and the sender refuses everything but pings
  (`FT.cpp:2080-2085`). A comment there says the reason was multi-version
  clients during upgrades (`FT.cpp:705-721`).
- A receiver can also state its own rule: exactly this version, or at least
  some version (`FT.h:136-152`, `FT.cpp:1231-1241`, `:1253-1255`). Ping
  accepts any version with stable interfaces (`FT.cpp:316-339`). So the
  liveness check works across an upgrade even when nothing else does.

### Reliable and unreliable sends

- `sendReliable` keeps a packet until it is cancelled. After a reconnect, the
  unreliable packets are dropped and the reliable ones are sent again
  (`FT.h:264-271`, `FT.cpp:1174-1185`, `:1026`).
- `getReply` is at-least-once: reliable send, wait for a reply, cancel the
  resend when it comes (`rpc.h:760-777`, `GA.h:411-440`). It gives up only if
  the endpoint is permanently failed (`GA.h:416-424`).
- `tryGetReply` is at-most-once: an unreliable send that returns
  `request_maybe_delivered` when the connection drops or the address is marked
  failed (`rpc.h:794-819`, `GA.h:361-409`). The caller must be able to retry.
- A reply is an unreliable send on the existing connection only
  (`fdbrpc/include/fdbrpc/networksender.h:42-66`). If that connection is gone,
  the reply is lost, and the client's wait ends on the disconnect.

### Unsent queues, coalescing and reconnect

- A send appends to the peer's unsent queue and wakes the writer
  (`FT.cpp:1130-1136`). The queue has no bound at this layer.
- The writer waits 10–20 µs to coalesce small packets, then writes at most
  128 KiB per call until the queue is empty (`FT.cpp:802-834`,
  `knobs:226-227`, `:240`).
- A connect attempt times out after `CONNECTION_MONITOR_TIMEOUT`, 2 s
  (`FT.cpp:913-914`, `knobs:119`). The delay between attempts starts at
  50 ms and grows by 1.2 up to 500 ms. It resets after 5 s of a stable
  connection (`FT.cpp:897-900`, `:995-1000`, `knobs:128-131`).

### Liveness: the connection monitor

- Every second, with jitter, the monitor sends a ping to the peer's well-known
  ping endpoint (`FT.cpp:759-763`, `knobs:118`).
- If no reply comes in 2 s, the connection fails only if no bytes at all
  arrived in that time. A connection busy with large replies is not failed for
  a late ping (`FT.cpp:767-787`).
- A server does not ping clients without a public address. It waits for their
  traffic instead, and pings only after a long silence (`FT.cpp:701-736`).
- Ping round trips are kept in a sketch per peer and logged with percentiles
  (`FT.cpp:543-604`, `:775`, `:789-791`).
- An incoming connection that sends no connect packet in 2 s is closed
  (`FT.cpp:1757-1771`).

### When an address is marked failed

- A new peer starts out available (`FT.cpp:1127`). The header calls this
  optimistic (`FM.h:55-58`).
- After a connection fails, the address is marked failed only once
  connecting has kept failing for `FAILURE_DETECTION_DELAY`, 4 s
  (`FT.cpp:1014-1024`, `knobs:329`). The comment says this mimics the older
  centralized detector.
- An address whose connections close more than 5 times in 30 s is marked
  failed at once, and stays failed while that lasts (`FT.cpp:1066-1079`,
  `fdbrpc/HealthMonitor.cpp:23-52`, `knobs:330-332`, `FT.cpp:836-855`).
- A successful connection marks the address available again
  (`FT.cpp:925-927`, `:971-973`).
- When a server's connections to a peer the monitor still thinks healthy keep
  closing for 20 s, it sets a process-wide `degraded` flag (`FT.cpp:1050-1064`,
  `FT.h:273-275`, `knobs:134-135`). That is FDB's sign that the fault may be
  local.

### Checksums and large packets

- A packet is its length, then an XXH3-64 checksum, then the destination
  token, then the body (`FT.cpp:2099-2172`). The checksum covers token and body.
- The checksum is skipped when the address is TLS (`FT.cpp:1372`, `:2077`).
  TLS already protects integrity.
- A bad checksum throws `checksum_failed` and drops the connection
  (`FT.cpp:1443-1455`). Reliable packets are then sent again on the next one.
- A packet over 100 MiB is a fatal protocol error on receive and an error log
  on send. One over 2 MiB logs a warning (`FT.cpp:1396-1401`, `:1476-1482`,
  `:2174-2186`, `knobs:237-238`). The read buffer grows to fit the next packet
  (`FT.cpp:1509-1526`).
- Bulk data does not travel as large packets. It travels as a reply stream:
  the server sends chunks and waits whenever more than a byte limit is
  unacknowledged (`rpc.h:843-847`, `:597-619`). Checkpoint files go this way,
  in 80 KB chunks under a 40 MB window
  (`fdbserver/storageserver/storageserver.cpp:2851-2870`,
  `fdbclient/ClientKnobs.cpp:172`, `fdbserver/core/ServerKnobs.cpp:1270`).
- Everything is serialized into user-space packet buffers
  (`FT.cpp:2089-2118`). There is no zero-copy send.

### TLS, trust and authorization tokens

- A connection is trusted when its IP is in the allow list and the TLS layer
  verified the peer (`FT.cpp:1545`).
- An untrusted peer may reach only endpoints marked public
  (`FT.cpp:1251-1255`). A message to a private stream endpoint is answered with
  the unauthorized well-known token (`FT.cpp:1282-1295`). The sender then fails
  that endpoint for good, and its requests return `unauthorized_attempt`
  (`FT.cpp:341-354`, `FM.cpp:139-146`, `:228-231`, `rpc.h:806-808`).
- A message to a missing stream endpoint is answered with endpoint-not-found
  (`FT.cpp:1296-1311`, `:300-314`).
- The transport loads and watches a JWKS public key file for authorization
  tokens (`FT.cpp:1864-1883`, `:2286-2327`,
  `fdbserver/fdbserver.cpp:2009-2020`). At this commit we found no caller of
  `getPublicKeyByName` outside FlowTransport itself, so token checks are not
  in this layer.

## FailureMonitor

### Address failure and endpoint failure

- There is one monitor per process (`FM.h:136-139`, `FT.cpp:2278`). Every
  component asks it. The transport is what sets it.
- Status is kept per address. An address the monitor has never heard of
  counts as failed (`FM.h:72`, `FM.cpp:194-212`).
- An endpoint can fail on its own while its address is fine: when the remote
  says endpoint-not-found, or unauthorized (`FM.cpp:115-146`). That failure is
  permanent (`FM.cpp:224-226`, `FM.h:108-115`). The map of such endpoints is
  never expired, only cleared at 100,000 entries (`FM.cpp:116`, `:129-134`).
- The header warns that an address may be reported failed while it works,
  and advises waiting before costly actions (`FM.h:45-49`). `onFailedFor`
  waits for a failure that lasts (`FM.cpp:32-70`).

### How a request learns of a failure

- A status change triggers every endpoint of that address at once
  (`FM.cpp:81-113`). A disconnect does the same (`FM.cpp:148-152`,
  `FT.cpp:1093-1096`).
- `tryGetReply` races the reply against that trigger and the peer's
  disconnect (`rpc.h:803-814`, `GA.h:369-391`). So every request to a dead
  peer ends together, within seconds, without a timeout of its own.
- The monitor is also what load balancing reads to skip failed replicas
  (`LB.h:361`, `:469`).

## Load balancing

### The queue model

- The client keeps, per server, a smoothed count of its own outstanding
  requests, the last latency it saw, a penalty the server sent, and a
  `failedUntil` time (`QM.h:40-72`).
- Starting a request adds the server's penalty to the count. Ending it takes
  the same amount off (`QM.cpp:57-61`, `:24-28`, `LB.h:43-68`). The smoother
  is exponential with a 2 s time constant (`fdbrpc/include/fdbrpc/Smoother.h:28-70`,
  `knobs:70`).
- A clean reply sets the latency. A failed one only raises it
  (`QM.cpp:30-34`).
- The model is local. Servers do not gossip load. The only server input is the
  penalty in each reply.

### Choosing a replica

- Replicas are shuffled, then sorted by distance, and the nearest group is
  "best" (`fdbrpc/include/fdbrpc/MultiInterface.h:186-203`).
- With a model, the client picks the healthy replica with the smallest
  smoothed count, and the second smallest as the hedge target
  (`LB.h:336-414`). A replica is skipped if the monitor says failed, or if it
  is inside `failedUntil` (`LB.h:361-392`).
- A replica whose penalty is above 1 counts as bad. Remote replicas are tried
  only when too many local ones are bad (`LB.h:343-371`, `knobs:310-311`).
- If every replica is failed, the client waits for any to recover, then
  backs off the next attempt (`LB.h:479-501`, `LB.cpp:29-53`). Retries over
  all replicas back off from 10 ms up to 5 s (`LB.h:564-568`, `:617-621`,
  `knobs:290-292`).

### Second requests and their budget

- The second request is sent after a delay: a multiplier times the second
  replica's last latency, plus 0.5 ms (`LB.h:416-423`, `knobs:295`). If the
  best replica's own latency is already more than twice that, the second
  request goes at once (`LB.h:418-420`, `knobs:294`).
- The budget starts at zero (`QM.h:103`). Each first request that answers
  before the delay adds 0.05, up to 100 (`LB.h:588-594`, `knobs:298-299`).
  Each second request costs 1 (`LB.h:609-612`). So at most about one request
  in twenty is hedged, and only fast answers refill the budget.
- When the delay fires with less than 1 in the budget, nothing is sent. The
  client keeps waiting on the first replica (`LB.h:607-614`).
- Each hedge raises the multiplier by 0.01. Each answer without one lowers it
  by 0.00025, never below 1 (`LB.h:590-591`, `:610`, `knobs:296-297`). So a
  client that hedges often waits longer before the next hedge.
- Without a model there is no second request (`LB.h:323`, `:336`).
- A simpler balancer, used where the list is always fresh, sends no second
  requests and shifts probabilities by each server's reported busyness every
  10 s (`LB.h:666-761`, `MultiInterface.h:96-157`).

### Overload and penalties

- A storage server's penalty grows with its write queue beyond a target, and
  with the inverse of its durable rate (`fdbserver/storageserver/storageserver.cpp:1656-1663`).
- An overloaded server answers `server_overloaded` with its penalty
  (`storageserver.cpp:1712`). The client records the penalty and tries the
  next replica, without counting it as a failure (`LB.h:212-218`).
- `future_version` and `process_behind` put the server in `failedUntil` with
  its own backoff, 1 s growing to 8 s (`QM.cpp:36-46`, `knobs:307-309`).

### Lagging requests

- When the other request wins, the loser is not forgotten. Its reply, when it
  comes, still updates the model (`LB.h:263-294`). Otherwise a slow replica's
  latency would never be recorded, because nobody waits for it.

### TimedRequest

- A request records the time it was decoded on a server (`fdbrpc/include/fdbrpc/TimedRequest.h:28-46`).
  Servers use it to measure queue wait apart from service time
  (`storageserver.cpp:2400`, `fdbserver/grvproxy/GrvProxyServer.cpp:490`).

## How the simulator models the network

FDB's simulator sits under FlowTransport, at the byte-stream level
(`sim2.cpp:292-417`). So the real packet framing, checksums, connect packets
and reconnects all run in simulation.

- **Latency.** Each one-way hop draws from 50–400 µs 99.9 % of the time, and
  up to 50 ms otherwise (`sim2.cpp:275-287`, `knobs:252-254`).
- **Slow pairs.** Each pair of machines gets a fixed extra latency when its
  first connection opens. In buggified runs it is up to 100 ms
  (`sim2.cpp:307-312`, `knobs:255`). So some links stay slow for the whole
  run.
- **Clogging delays, it does not refuse.** A clog holds bytes until it ends.
  It can apply to a pair, a process pair, or an interface's sends or receives
  (`sim2.cpp:195-214`, `:226-251`, `:2086-2117`). A clogged interface picks
  send, receive or both at random (`sim2.cpp:2087-2095`). The random clogging
  workload draws exponential durations and also swizzles
  (`fdbserver/workloads/RandomClogging.cpp:88-137`).
- **Disconnection.** A disconnected pair fails new connections and data in
  flight with `connection_failed` (`sim2.cpp:216-224`, `:322-329`,
  `:473-480`, `:2119-2127`).
- **Random closes.** Any read or write may close the connection on one side,
  the other or both, and may report it at once or later (`sim2.cpp:540-563`).
  A run turns these on or off, and tests can switch them off near the end
  (`sim2.cpp:2606-2612`, `:2758-2785`).
- **Bounded buffers.** Each connection gets a random send buffer, and a full
  one blocks the writer (`sim2.cpp:313`, `:444-446`, `:516-538`).
- **Fragmentation.** Writes are cut short at random, down to under 1000 bytes
  (`sim2.cpp:387-401`). Receives deliver a random prefix of what was sent
  (`sim2.cpp:482-485`).
- **Bit flips.** On rare packets, buggify flips bits before the checksum
  check, and the run asserts the checksum caught it (`FT.cpp:1418-1462`).
- **Connecting to the dead.** A connect to a dead process either fails or
  hangs forever, chosen per run (`sim2.cpp:1199-1211`, `knobs:258`). A connect
  to an address with no process waits until one appears
  (`sim2.cpp:1217-1226`). The accept side sees the connection after a random
  delay up to 0.5 s (`sim2.cpp:1122-1124`).
- **A closed peer stops receiving after 1 s**, not at once
  (`sim2.cpp:360-363`, `:448-455`).

## Lessons for sproutfs's peer server

### 1. One peer object per remote host, shared by every user

FDB keeps one `Peer` per address and one failure monitor per process
(`FT.cpp:1842-1856`, `FM.h:136-139`). Every request to that address, from any
component, shares its connection, its liveness and its failure state.

We pool connections per memory region (`peer.go:97-133`). Each region dials,
backs off and learns of failure alone. The source then has to count
connections per remote host to bound them (`pagesource.go:519-533`), and the
plan's down marks are kept "by each reader on its own"
(`plans/disk-cache-2026-10-02.md:212`).

Recommendation: the host keeps one peer table. Each entry is one remote host.
It holds that host's connections, its liveness state, its down mark and its
queue model. Post-copy faults, the post-copy stream, fork claims, cache reads,
keeps, drops, presence and probes all go through it. A memory region keeps
its own retry rule, but asks the table for a connection. FDB is better here.

### 2. Multiplex by request ID, in two lanes

FDB puts every request to a host on one connection and matches replies by
token (`rpc.h:760-777`). We already carry `request_id` and `in_reply_to` in
every envelope (`wire/v1 proto:12-15`) and check them (`peer.go:427-430`), but
use one connection per request in flight (`pagesource.go:543-545`).

FDB can share one stream because its messages are small: it warns past 2 MiB
and moves bulk data as 80 KB chunks (`knobs:238`, `ClientKnobs.cpp:172`). Ours
are not. One page reply is 2 MiB (`docs/migration.md:878`), and a host may
have 8 MiB in flight to one peer. On one TCP stream, a 4 KiB fault reply would
wait behind those. That is why each memory region keeps a connection for
faults (`docs/migration.md:305-311`).

Recommendation: per peer, keep two connections, multiplexed by request ID.

- A latency lane for guest faults, probes, presence, claims, keeps without
  data, and drops. Small requests and small replies.
- A bulk lane for the post-copy stream, stripe reads and fills with data.

The server answers requests on a connection in any order. It bounds work by
bytes in flight per peer, which it already does, not by connection count. The
reserved fault connection per memory region becomes one latency lane per host
pair. A host draining 40 VMs to one destination then uses two connections,
not 160. FDB does not need lanes. We do, because our replies are large.

Keep one connection per direction, not FDB's single merged connection. FDB
breaks dial races by address order (`FT.cpp:1187-1223`). Our roles are
asymmetric: the dialer asks and the listener serves. Two independent
connections, one each way, are simpler and lose nothing.

### 3. Negotiate the version once, at connect

FDB sends its protocol version first on every connection
(`FT.cpp:1138-1172`). An incompatible peer is recorded and listed
(`FT.cpp:1629-1631`, `FT.h:240-244`). Ping still works across versions
(`FT.cpp:316-339`). Receivers can accept a range of versions
(`FT.h:136-152`).

We check a wire version on every frame, and accept only 1
(`codec.go:132-135`). On a mismatch the server closes the connection without a
reply (`pagesource.go:551-553`). The destination treats a closed connection
as a stumbling source and retries pages only the source holds until the hold
ends (`vmmigrate/peer.go:646-695`). During a rolling upgrade, a version
mismatch would look like a flaky source for four checkpoint intervals, with
only the stalled-ask log line to show it (`vmmigrate/peer.go:681-688`).

Recommendation:

- The dialer's first frame is a hello: the lowest and highest peer protocol it
  speaks, and its host identity. The server answers with the version both will
  use, or with an explicit `INCOMPATIBLE` that names its own range.
- Per-frame `wire_version` then only guards framing. New request kinds (keep,
  drop, presence, probe) are gated by the negotiated version, not by a new
  wire version.
- `INCOMPATIBLE` is its own outcome: not down, not gone. For post-copy it is
  as fatal as `ErrPageSize` (`vmmigrate/peer.go:728-730`). For the cache it
  removes that host from the reader's list. The peer table reports it in
  `/status`, as FDB lists incompatible peers.
- A drain hands VMs from old hosts to new ones. So every release must speak
  the previous release's peer protocol. Test it with a two-version simulation.

FDB is better here. A per-frame version catches a mismatch, but tells nobody.

### 4. Liveness apart from request timeouts

FDB decides that a connection is dead from the connection, not from requests.
It pings each peer every second and fails the connection when 2 s pass with no
bytes at all (`FT.cpp:759-787`). A slow request never marks a host failed. A
slow server shows up in the queue model's latency and penalty instead
(`QM.cpp:30-34`, `LB.h:366-371`).

We have no liveness check of our own. A source that vanishes without a reset
holds a guest fault until TCP keepalive gives up (`platform/internal/real/network.go:56`).
With Go's defaults that is minutes (Go `net/dial.go:20-27`, nine probes). For
a run a checkpoint holds, the fault could have read the volume after a few
seconds.

The plan marks a host down after three timeouts in a row
(`plans/disk-cache-2026-10-02.md:212-219`). A request timeout mixes two causes:
a dead host, and a host whose disk is slow. The first should be marked down.
The second should get fewer requests, which is the queue model's job.

Recommendation:

- The peer table pings each peer with traffic on its latency lane, about
  once a second. A connection with no bytes received for a few pings is
  closed. Every request on it ends with a disconnect at once, as FDB's
  `tryGetReply` does (`GA.h:369-391`).
- The ping is answered on the connection's reader, never queued behind disk
  reads, as FDB runs ping at socket priority (`FT.cpp:326-329`).
- Down marks come from liveness: failed connects and dead connections. A
  request that is merely slow updates the queue model. Keep the plan's "at
  most a fifth" guard. FDB has only a `degraded` flag for that case
  (`FT.cpp:1050-1064`). Ours acts on it, which is better for a cache.
- Keep the plan's probe schedule (10 s growing to 60 s). FDB reconnects within
  500 ms (`knobs:129`) because its peers are not optional. Ours are a cache, and
  a slow return to a flapping host is the safer choice. FDB adds a similar
  damper for hosts that flap (`FT.cpp:1066-1079`).

### 5. Shared failure state is a hint; only evidence says "gone"

FDB separates two kinds of failure. An address marked failed is transient and
may be wrong (`FM.h:45-49`). An endpoint the remote says it does not have is
failed for good (`FM.cpp:115-137`, `:224-226`). `getReply` retries through
the first and stops only at the second (`GA.h:411-433`).

Our post-copy already draws the same line. A busy or broken source is retried.
Only `STATUS_UNKNOWN_VM` or this host's close ends the asking
(`docs/migration.md:78-101`). This is right, and FDB confirms it.

Recommendation: when the peer table marks a host down, post-copy for pages only
that host holds must ignore the mark. It may read the volume at once for runs a
checkpoint holds. Cache reads skip down hosts. Write this rule next to the
peer table, so a later change cannot turn a down mark into "the source is
gone".

### 6. Choosing ranks: a queue model, and a hedge budget earned by fast answers

FDB asks one replica, chosen by the smallest smoothed count of its own
outstanding requests, and hedges to a second only after a delay
(`LB.h:336-427`). The hedge budget grows by 0.05 per fast answer and each hedge
costs 1 (`LB.h:588-612`). The hedge delay grows when it hedges often
(`LB.h:590-591`, `:610`). Late replies still update the model (`LB.h:263-294`).

The plan reads all k+m ranks at once and takes the first k
(`plans/disk-cache-2026-10-02.md:231-233`). That hedges every read in full. At
4+2 each read moves 1.5 times the envelope's bytes and costs six disk reads
instead of four. At 1+1 it moves twice the bytes. It also makes every reader
add load to the slowest rank, which is the one that should get less.

The plan's store hedge is a token bucket at 5 % of reads
(`plans/disk-cache-2026-10-02.md:244-252`). FDB's budget is the same size, but
it is refilled only by answers that came back before the hedge delay. When
every server is slow, nothing refills it and hedging stops. That is the
property the plan wants: a slowdown that reaches every host must not double
the load on the store.

Recommendations:

- Refill the store-read bucket by reads that the cluster answered within the
  bound, not by time or by all reads. Then a cluster-wide slowdown drains it.
- Measure "ask k+1, hedge the rest" against "ask all k+m". Choose the k+1 by
  a per-peer queue model: smoothed outstanding requests, last latency, and a
  hint from `BUSY` (lesson 7). Send the remaining ranks after a delay set from
  the measured latency, under a budget like FDB's. If all-k+m wins on tail
  latency and the disks and links have room, keep it. It is a choice to
  measure, not to assume.
- Whichever is chosen, keep the replies that arrive after the first k. Record
  their latency in the queue model, as FDB's lagging requests do. Do not
  cancel them silently.
- Have the server record when it decoded each request, as `TimedRequest`
  does. Report queue wait and service time per request kind. A reader can then
  tell a slow disk from a slow link.

### 7. Back-pressure: answer `BUSY` in-band, and never close for a budget

FDB has no per-peer limit in the transport. Its unsent queue is unbounded
(`FT.cpp:1130-1136`). Back-pressure is in the application: a server answers
`server_overloaded` with a penalty, and the client moves to another replica
without counting a failure (`LB.h:212-218`). Bulk replies are paced by
acknowledged bytes (`rpc.h:597-619`).

Ours is a hard per-peer byte budget at the source (`pagesource.go:717-724`).
That fits our problem better. One destination can ask for gigabytes of pages,
and the source's memory and link are shared by every VM it hands over.

Two parts need care once cache requests share the channel:

- A connection over the per-peer connection budget is closed
  (`pagesource.go:536-541`). The plan marks a host down after one refused
  connection (`plans/disk-cache-2026-10-02.md:214-215`). A reader that saw an
  accept and then a close could read it as a refused or dead host, and mark a
  healthy, busy host down. With multiplexing (lesson 2) the connection budget
  can go. Every refusal is then a `BUSY` reply on an open connection, and a
  closed connection always means trouble.
- `BUSY` says nothing about how busy. FDB's penalty says how much
  (`storageserver.cpp:1656-1663`). Let `BUSY` carry the server's queued bytes
  for that peer, or a suggested wait. A cache reader treats a busy rank as
  FDB treats an overloaded replica: skip it this time, and weight it in the
  queue model. A post-copy destination, which has only one source, keeps
  waiting as today, but can wait as long as the hint says rather than
  doubling from 1 ms (`vmmigrate/peer.go:196-197`).

### 8. Checksums: cover the header; skip the payload CRC when something else covers it

FDB checksums every packet, header and body together, with XXH3-64, and skips
it only under TLS (`FT.cpp:2099-2172`, `:1372`). A mismatch drops the
connection (`FT.cpp:1443-1455`).

We checksum the payload only (`codec.go:95-102`). The protobuf header is not
covered. For a page reply the header holds the `present` and `dirty` bitmaps
(`migrate.proto:40-55`). A flipped `dirty` bit on plain TCP would make the
destination treat a page as the checkpoint's bytes when it is the guest's
own, or the reverse. Nothing end to end would catch that. TCP's own checksum
is 16 bits.

The plan drops the CRC on stripe replies because each stripe carries its own
checksum (`plans/disk-cache-2026-10-02.md:330-331`). That is right for the
stripe bytes, and the envelope's SHA-256 catches a stripe that was swapped for
another. But the header still says which stripe, which envelope and whether
it is present.

Recommendations:

- Checksum the header on every frame. It is small, so CRC32C costs little.
  Then a payload whose own format is self-checking can go without the frame
  CRC, as the plan wants for stripes, and `sendfile` stays possible.
- Like FDB, let a transport that has integrity (mutual TLS) turn the frame
  checksums off. Today `wire` always computes CRC32C for pages
  (`pagesource.go:835`), which is wasted work under TLS.
- On a header checksum failure, drop the connection, as FDB does. Do not
  answer it: a header that cannot be trusted may name the wrong request.
- Make the simulator flip header bits, not only payload bits (lesson 12).

### 9. Large payloads

FDB keeps messages small and streams bulk data with an acknowledged window
(`rpc.h:843-847`). It has no zero-copy path.

Ours fits our problem better: a raw payload beside a small header
(`platform/network.go:43-50`) is what makes `sendfile` of a stripe run
possible. Two things to take from FDB:

- A hard cap on one frame, well below today's 1 GiB
  (`platform/internal/real/network.go:43`). Our largest reply is a few MiB.
  A cap near 16 MiB bounds what one bad header can make a reader allocate,
  as FDB's 100 MiB cap does (`FT.cpp:1396-1401`).
- Large replies belong on the bulk lane only. The latency lane can then have
  a much smaller cap, which keeps a fault from ever waiting behind 2 MiB.

### 10. Name what a request is for, so a stale request cannot hit a new state

FDB's random token half means a request to a slot that was reused finds
nothing (`FT.cpp:195-207`). The sender then learns that endpoint is gone for
good.

Our page requests name a VM and a volume by string (`migrate.proto:26-33`). A
VM that left a host and later came back to it has the same name. We found no
path today where a stale destination asks the new incarnation, because a
destination closes its backing before it can hand the VM on. But the peer
server will carry more request kinds and more holders.

Recommendation: page and claim requests also carry the checkpoint sequence the
handoff names (`docs/migration.md:41-43`). The server answers
`STATUS_UNKNOWN_VM` when that sequence is not the one it serves. Cache
requests name content, so they need nothing.

### 11. Say which requests may be retried

FDB states the delivery rule of each call: `getReply` at least once,
`tryGetReply` at most once, with `request_maybe_delivered` when it cannot tell
(`rpc.h:760-819`).

Our page and listing requests are reads and safe to repeat. Keep and drop are
safe to repeat if a host drops a keep for a stripe it already holds, as the plan
says (`plans/disk-cache-2026-10-02.md:302-303`). A claim must be safe to repeat
too: a retried claim of a hold already claimed must answer OK.

Recommendation: each request kind declares itself idempotent or not, in the
proto. The peer table retries only idempotent ones after a disconnect. A
non-idempotent one returns a "maybe delivered" error to its caller.

### 12. What `platform/sim`'s network should also model

Our simulated network is framed (`platform/sim/network.go:461-640`). It has
latency with uniform jitter, a per-send bandwidth cost, partitions, clogs,
swizzles, and one-shot drop, duplicate, corrupt and delay
(`platform/sim/network.go:16-41`, `:183-323`). The campaign uses partitions and
swizzles (`internal/simtest/fault.go:68-69`).

FDB models more, and each item tests something the peer server will rely on.

1. **A clog that delays instead of refusing.** Our clog refuses a send with
   `ErrUnavailable` (`platform/sim/network.go:235-239`, `:532-534`). The
   sender learns at once. A real partition is silent: bytes sit in buffers and
   nothing comes back. FDB's clog holds the bytes until it ends
   (`sim2.cpp:195-214`). Only a silent clog tests liveness pings, hedge
   delays, down marks and the fault path's wait. Keep the refusing form as
   `Partition`.
2. **A heavy latency tail and slow pairs.** Ours is uniform jitter around a
   mean (`platform/sim/network.go:364-368`). FDB puts 0.1 % of hops up to
   50 ms, and gives some pairs a lasting extra delay (`sim2.cpp:275-287`,
   `:307-312`). The queue model and the hedge only matter in the tail, and a
   rank that is always slow is the case they exist for.
3. **Link bandwidth shared by all connections between two hosts.** Our
   bandwidth cost is per send and independent across connections
   (`platform/sim/util.go:27-32`). So eight connections get eight times the
   bandwidth, and per-region connections look free. A queue per link would
   show head-of-line waits, and would let the simulator compare one connection
   per region with lanes per host (lesson 2).
4. **Random connection closes.** FDB closes connections at random, on one
   side or both, noticed at once or later (`sim2.cpp:540-563`). Ours closes
   only on an explicit `Close` or a process crash. Reconnects, pool trimming
   and the "replies lost on a dead connection" path need this.
5. **A connect that hangs.** A connect to a dead process hangs forever in
   some FDB runs (`sim2.cpp:1199-1211`). Ours fails at once
   (`platform/sim/network.go:121-139`). A real dial to a host whose machine is
   gone waits for SYN retries. The connect timeout and the "one refused
   connection" rule are only tested by a hang.
6. **The real framer under fragmentation.** FDB's simulator is a byte stream
   under the real packet code, with short writes and partial reads
   (`sim2.cpp:387-401`, `:482-485`). Ours replaces the framer, so
   `platform/internal/real/network.go` never runs in simulation. Add a
   simulated `platform.Transport` (byte streams) and run the real framer over
   it in some campaigns. That also tests the hello exchange of lesson 3.
7. **Bit flips in headers, at random.** `CorruptNext` can flip a header bit
   (`platform/sim/network.go:563-570`), but only `codec_test.go:129` uses it.
   Turn it on at a low rate in campaigns, once headers are checksummed
   (lesson 8). FDB asserts that every flip it makes is caught
   (`FT.cpp:1457-1461`).
8. **Bounded send buffers.** FDB gives each connection a random buffer, and
   a full one blocks the writer (`sim2.cpp:313`, `:516-538`). A peer server
   that stops reading must stall its sender, not grow memory.
9. **A quiet end.** FDB can switch connection failures off for the end of a
   test, so the system must converge (`sim2.cpp:2758-2785`). A campaign that
   ends with faults off and then asserts that every drain finishes and every
   `Serving` set empties tests liveness, not only safety.

### Where our design fits our problem better

- **A hard per-peer byte budget at the server.** FDB's queues are unbounded at
  the transport (`FT.cpp:1130-1136`). Our pages are large and our sources are
  shared. Keep the budget. Make it the only budget (lesson 7).
- **Retry until evidence, for pages only one host holds.** FDB's `getReply`
  also retries until an endpoint is known gone (`GA.h:411-433`). Our rule is
  the same, stated more strictly, and should stay.
- **A raw payload beside a small header.** It allows `sendfile` and avoids
  serializing pages. FDB's design suits small RPCs, and it streams bulk data
  in 80 KB pieces instead.
- **A cap on how many hosts a reader marks down.** FDB only sets a
  `degraded` flag (`FT.cpp:1050-1064`). The plan's fifth is a better guard for
  a cache whose failures cost only a store read.
- **One connection per direction.** FDB merges both directions and breaks
  dial races (`FT.cpp:1187-1223`). Our roles are asymmetric, so we need
  neither.

### Summary

| # | Lesson | Where it lands |
| --- | --- | --- |
| 1 | One peer table per host, shared by every request kind | `vmmigrate/internal/peer`, `docs/hosting.md#transport` |
| 2 | Multiplex by request ID; a latency lane and a bulk lane per host pair | `docs/migration.md` page protocol; plan "In flight" |
| 3 | Version hello at connect; explicit `INCOMPATIBLE`; N and N−1 interoperate | `vmmigrate/internal/wire`, `docs/hosting.md#transport` |
| 4 | Liveness by pings and bytes received; down marks from liveness only | plan "Hosts marked down" |
| 5 | A down mark never means "source gone" | `docs/migration.md` failure during post-copy |
| 6 | Queue model per peer; hedge budget refilled by fast answers; measure k+1 against k+m; keep late replies | plan "Reading a page" |
| 7 | `BUSY` in-band with a hint; no connection budget | `vmmigrate/pagesource.go`; plan "Hosts marked down" |
| 8 | Checksum the header; skip payload CRC under TLS and for stripes | `vmmigrate/internal/wire`; plan "Serving stripes" |
| 9 | Frame cap near 16 MiB; large replies on the bulk lane only | `platform/internal/real/network.go` |
| 10 | Page and claim requests name the handoff's checkpoint sequence | `migrate.proto` |
| 11 | Each request kind declares whether it is idempotent | `migrate.proto`, cache protos |
| 12 | Simulate silent clogs, tails, shared link bandwidth, random closes, hanging connects, the real framer, header flips, bounded buffers, a quiet end | `platform/sim/network.go`, `internal/simtest` |
