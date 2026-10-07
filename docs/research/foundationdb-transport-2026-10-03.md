# FoundationDB's internal transport, and the peer server — 2026-10-03

Research for the host-to-host channel: the page server today
([hosting](../hosting.md#transport), [migration](../migration.md)), to be
renamed the peer server and to carry handoffs, fork points and the requests of
[the distributed disk cache plan](../../plans/disk-cache-2026-10-02.md): read
stripes, keep, drop, presence and probe.

## Sources

| Tag | Project | Commit | Date | License (from its LICENSE file) |
| --- | --- | --- | --- | --- |
| fdb | github.com/apple/foundationdb, `~/src/foundationdb` | `178c4e9148e5b40d25748447bff54bf402cef2f8` | 2026-09-03 | Apache-2.0 (`LICENSE` line 2) |

Nothing here is a dependency and no code is copied. Citations are `path:line`
at that commit. Short names:

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

At this commit FDB's actors are C++ coroutines (`co_await`). The tree also
has an optional gRPC path (`fdbrpc/FlowGrpc.cpp`, and `fdbrpc/FileTransfer.cpp`
behind `FLOW_GRPC_ENABLED`). This note covers FlowTransport, which FDB's roles
use to talk to each other. Our side is cited as repository paths.

## Today's peer channel

- A destination dials the source's page server. `platform.Network` frames the
  stream and a `platform.Transport` carries the bytes (`docs/hosting.md:64-70`,
  `platform/network.go:13-35`).
- A frame is a 20-byte prefix with magic, a frame version and two sizes, then
  a protobuf header, then a raw payload (`platform/internal/real/network.go:18-23`,
  `:186-210`). The header is a protobuf `Envelope` with `wire_version`,
  `request_id`, `in_reply_to`, the message as an `Any`, and a payload
  descriptor (`vmmigrate/internal/wire/proto/sproutfs/wire/v1/*.proto:11-23`).
- Every frame must carry wire version 1 (`vmmigrate/internal/wire/codec.go:21`,
  `:132-135`). Page requests also carry `payload_format` 1
  (`vmmigrate/proto/sproutfs/migrate/v1/migrate.proto:31-32`).
- The payload has an optional CRC32C or SHA-256 (`codec.go:29-48`). The page
  source always sends CRC32C over the payload (`vmmigrate/pagesource.go:829-836`).
  No checksum covers the header.
- Each connection carries one request at a time; the server answers each
  before reading the next (`pagesource.go:543-558`). The client numbers
  requests and checks `in_reply_to`, but never has two in flight on one
  connection (`vmmigrate/internal/peer/peer.go:310-317`, `:411-439`).
- Connections are pooled per memory region, four by default, with one kept for
  guest faults (`docs/migration.md:305-311`, `peer.go:120-137`,
  `vmmigrate/peer.go:216`).
- The source bounds each remote host to 8 connections and 8 MiB of pages in
  flight (`pagesource.go:42-47`). A connection over the budget is closed; a
  request over the byte budget is answered `BUSY` (`pagesource.go:536-541`,
  `:717-724`).
- The destination retries a run only the source holds indefinitely, with
  backoff from 1 ms to 100 ms, until the source says it no longer serves the VM
  or the backing closes (`docs/migration.md:78-101`, `vmmigrate/peer.go:196-197`,
  `:646-695`).
- A frame the server cannot decode, including one of another wire version,
  ends the connection with no reply (`pagesource.go:551-553`).
- The only liveness check on an idle connection is TCP keepalive at 30 s
  (`platform/internal/real/network.go:56`). The destination's requests carry no
  deadline (`vmmigrate/peer.go:765-769`). The source gives each request 30 s
  for its local reads (`pagesource.go:48-53`, `:732`).

## FlowTransport

### Endpoints and tokens

- An endpoint is a list of addresses and a 128-bit token (`FT.h:50-60`). A
  message is addressed to a token, not a connection.
- Well-known tokens have an all-ones first half and a small index second half:
  endpoint-not-found, ping and unauthorized (`FT.h:41-48`, `:67-71`). An
  application reserves more when it creates the transport (`FT.h:208`).
- Every other token is random (`FT.cpp:2020-2030`). Its low 32 bits index a
  table slot (`FT.cpp:145-154`). A lookup checks the rest of the token against
  the slot, so a stale token to a reused slot finds nothing
  (`FT.cpp:195-207`).
- The slot stores the receiver's task priority (`FT.cpp:106-114`,
  `:136-143`), and delivery runs at that priority (`FT.cpp:1332-1355`). The
  ping receiver runs at socket-read priority (`FT.cpp:326-329`), so request
  work does not delay a ping's answer.
- An interface's request streams get adjacent slots under one base token, so
  an interface serializes as one endpoint (`FT.cpp:156-193`, `FT.h:91-97`).
- A request carries its own reply endpoint (`rpc.h:760-777`), so many requests
  can be in flight on one connection and replies come back in any order.

### One connection per peer

- The transport keeps one `Peer` per remote address (`FT.cpp:407`,
  `:1842-1856`). It owns the unsent queue, the reliable packets, the
  connection state, ping latencies and counters (`FT.h:156-200`).
- One connection carries both directions. When two processes dial each other
  at once, the one with the larger address keeps its outgoing connection
  (`FT.cpp:1187-1223`).
- A connection opens only when there is something to send
  (`FT.cpp:871-901`). A client closes an idle one after 180 s, and a peer
  nothing references is closed after 2 s (`FT.cpp:746-757`, `knobs:120-122`).

### The connect packet and versions

- Every connection starts with a connect packet: length, the sender's protocol
  version, its canonical address and a connection id (`FT.cpp:617-682`). The
  sender puts it at the front of its unsent queue on every new connection
  (`FT.cpp:1138-1172`).
- Two versions are compatible when all but their lowest 16 bits match
  (`flow/protocolversion/ProtocolVersion.h.template:53`, `:61-63`).
- An incompatible peer is recorded with the time it appeared
  (`FT.cpp:1603-1636`). The process exposes that list and a trigger for it
  (`FT.h:240-244`). Very old peers are hung up on (`FT.cpp:1640-1651`).
- An incompatible connection stays open. The reader drops its packets
  (`FT.cpp:1706-1720`), and the sender refuses everything but pings
  (`FT.cpp:2080-2085`). A comment gives multi-version clients during upgrades
  as the reason (`FT.cpp:705-721`).
- A receiver can require exactly one version or at least some version
  (`FT.h:136-152`, `FT.cpp:1231-1241`, `:1253-1255`). Ping accepts any version
  with stable interfaces (`FT.cpp:316-339`), so liveness checks work across an
  upgrade.

### Reliable and unreliable sends

- `sendReliable` keeps a packet until cancelled. After a reconnect, unreliable
  packets are dropped and reliable ones resent (`FT.h:264-271`,
  `FT.cpp:1174-1185`, `:1026`).
- `getReply` is at-least-once: reliable send, wait for a reply, cancel the
  resend when it comes (`rpc.h:760-777`, `GA.h:411-440`). It gives up only if
  the endpoint is permanently failed (`GA.h:416-424`).
- `tryGetReply` is at-most-once: an unreliable send that returns
  `request_maybe_delivered` when the connection drops or the address is marked
  failed (`rpc.h:794-819`, `GA.h:361-409`). The caller must be able to retry.
- A reply is an unreliable send on the existing connection only
  (`fdbrpc/include/fdbrpc/networksender.h:42-66`). If that connection is gone
  the reply is lost, and the client's wait ends on the disconnect.

### Unsent queues, coalescing and reconnect

- A send appends to the peer's unsent queue and wakes the writer
  (`FT.cpp:1130-1136`). The queue is unbounded at this layer.
- The writer waits 10–20 µs to coalesce small packets, then writes at most
  128 KiB per call until the queue is empty (`FT.cpp:802-834`,
  `knobs:226-227`, `:240`).
- A connect attempt times out after `CONNECTION_MONITOR_TIMEOUT`, 2 s
  (`FT.cpp:913-914`, `knobs:119`). The delay between attempts starts at
  50 ms and grows by 1.2 up to 500 ms, resetting after 5 s of a stable
  connection (`FT.cpp:897-900`, `:995-1000`, `knobs:128-131`).

### Liveness: the connection monitor

- Every second, with jitter, the monitor pings the peer's well-known ping
  endpoint (`FT.cpp:759-763`, `knobs:118`).
- If no reply comes in 2 s, the connection fails only if no bytes at all
  arrived in that time, so a connection busy with large replies is not failed
  for a late ping (`FT.cpp:767-787`).
- A server does not ping clients without a public address; it pings only
  after a long silence (`FT.cpp:701-736`).
- Ping round trips are kept in a sketch per peer and logged with percentiles
  (`FT.cpp:543-604`, `:775`, `:789-791`).
- An incoming connection that sends no connect packet in 2 s is closed
  (`FT.cpp:1757-1771`).

### When an address is marked failed

- A new peer starts out available (`FT.cpp:1127`); the header calls this
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
- When a server's connections to a peer the monitor thinks healthy keep
  closing for 20 s, it sets a process-wide `degraded` flag (`FT.cpp:1050-1064`,
  `FT.h:273-275`, `knobs:134-135`), FDB's sign that the fault may be local.

### Checksums and large packets

- A packet is its length, an XXH3-64 checksum, the destination token, then the
  body (`FT.cpp:2099-2172`). The checksum covers token and body.
- The checksum is skipped when the address is TLS (`FT.cpp:1372`, `:2077`).
- A bad checksum throws `checksum_failed` and drops the connection
  (`FT.cpp:1443-1455`); reliable packets are resent on the next one.
- A packet over 100 MiB is a fatal protocol error on receive and an error log
  on send. One over 2 MiB logs a warning (`FT.cpp:1396-1401`, `:1476-1482`,
  `:2174-2186`, `knobs:237-238`). The read buffer grows to fit the next packet
  (`FT.cpp:1509-1526`).
- Bulk data travels as a reply stream: the server sends pieces and waits
  whenever more than a byte limit is unacknowledged (`rpc.h:843-847`,
  `:597-619`). Checkpoint files go this way, in 80 KB pieces under a 40 MB
  window (`fdbserver/storageserver/storageserver.cpp:2851-2870`,
  `fdbclient/ClientKnobs.cpp:172`, `fdbserver/core/ServerKnobs.cpp:1270`).
- Everything is serialized into user-space packet buffers
  (`FT.cpp:2089-2118`). There is no zero-copy send.

### TLS, trust and authorization tokens

- A connection is trusted when its IP is in the allow list and TLS verified
  the peer (`FT.cpp:1545`).
- An untrusted peer may reach only endpoints marked public
  (`FT.cpp:1251-1255`). A message to a private stream endpoint is answered with
  the unauthorized token (`FT.cpp:1282-1295`). The sender then fails that
  endpoint permanently, and its requests return `unauthorized_attempt`
  (`FT.cpp:341-354`, `FM.cpp:139-146`, `:228-231`, `rpc.h:806-808`).
- A message to a missing stream endpoint is answered with endpoint-not-found
  (`FT.cpp:1296-1311`, `:300-314`).
- The transport loads and watches a JWKS public key file for authorization
  tokens (`FT.cpp:1864-1883`, `:2286-2327`,
  `fdbserver/fdbserver.cpp:2009-2020`). We found no caller of
  `getPublicKeyByName` outside FlowTransport, so token checks are not in this
  layer.

## FailureMonitor

### Address failure and endpoint failure

- There is one monitor per process (`FM.h:136-139`, `FT.cpp:2278`). Every
  component reads it; the transport sets it.
- Status is kept per address. An unknown address counts as failed (`FM.h:72`,
  `FM.cpp:194-212`).
- An endpoint can fail while its address is fine, when the remote says
  endpoint-not-found or unauthorized (`FM.cpp:115-146`). That failure is
  permanent (`FM.cpp:224-226`, `FM.h:108-115`). The map of such endpoints is
  never expired, only cleared at 100,000 entries (`FM.cpp:116`, `:129-134`).
- The header warns that an address may be reported failed while it works,
  and advises waiting before costly actions (`FM.h:45-49`). `onFailedFor`
  waits for a lasting failure (`FM.cpp:32-70`).

### How a request learns of a failure

- A status change triggers every endpoint of that address at once
  (`FM.cpp:81-113`), and so does a disconnect (`FM.cpp:148-152`,
  `FT.cpp:1093-1096`).
- `tryGetReply` races the reply against that trigger and the peer's
  disconnect (`rpc.h:803-814`, `GA.h:369-391`), so every request to a dead
  peer ends together, within seconds, without its own timeout.
- Load balancing reads the monitor to skip failed replicas (`LB.h:361`,
  `:469`).

## Load balancing

### The queue model

- The client keeps, per server, a smoothed count of its own outstanding
  requests, the last latency it saw, a penalty the server sent, and a
  `failedUntil` time (`QM.h:40-72`).
- Starting a request adds the server's penalty to the count; ending it takes
  the same amount off (`QM.cpp:57-61`, `:24-28`, `LB.h:43-68`). The smoother
  is exponential with a 2 s time constant (`fdbrpc/include/fdbrpc/Smoother.h:28-70`,
  `knobs:70`).
- A clean reply sets the latency; a failed one only raises it
  (`QM.cpp:30-34`).
- The model is local. Servers do not gossip load; the only server input is
  the penalty in each reply.

### Choosing a replica

- Replicas are shuffled, then sorted by distance, and the nearest group is
  "best" (`fdbrpc/include/fdbrpc/MultiInterface.h:186-203`).
- With a model, the client picks the healthy replica with the smallest
  smoothed count, and the second smallest as the hedge target
  (`LB.h:336-414`). A replica is skipped if the monitor says failed or it is
  inside `failedUntil` (`LB.h:361-392`).
- A replica whose penalty is above 1 is bad. Remote replicas are tried only
  when too many local ones are bad (`LB.h:343-371`, `knobs:310-311`).
- If every replica is failed, the client waits for any to recover, then backs
  off the next attempt (`LB.h:479-501`, `LB.cpp:29-53`). Retries over all
  replicas back off from 10 ms up to 5 s (`LB.h:564-568`, `:617-621`,
  `knobs:290-292`).

### Second requests and their budget

- The second request is sent after a multiplier times the second replica's
  last latency, plus 0.5 ms (`LB.h:416-423`, `knobs:295`). If the best
  replica's latency is already more than twice that, it goes at once
  (`LB.h:418-420`, `knobs:294`).
- The budget starts at zero (`QM.h:103`). Each first request that answers
  before the delay adds 0.05, up to 100 (`LB.h:588-594`, `knobs:298-299`).
  Each second request costs 1 (`LB.h:609-612`). So at most about one request
  in twenty is hedged, and only fast answers refill the budget.
- When the delay fires with less than 1 in the budget, nothing is sent and the
  client keeps waiting on the first replica (`LB.h:607-614`).
- Each hedge raises the multiplier by 0.01; each answer without one lowers it
  by 0.00025, never below 1 (`LB.h:590-591`, `:610`, `knobs:296-297`).
- Without a model there is no second request (`LB.h:323`, `:336`).
- A simpler balancer, used where the list is always fresh, sends no second
  requests and shifts probabilities by each server's reported busyness every
  10 s (`LB.h:666-761`, `MultiInterface.h:96-157`).

### Overload and penalties

- A storage server's penalty grows with its write queue beyond a target, and
  with the inverse of its durable rate (`fdbserver/storageserver/storageserver.cpp:1656-1663`).
- An overloaded server answers `server_overloaded` with its penalty
  (`storageserver.cpp:1712`). The client records the penalty and tries the
  next replica without counting a failure (`LB.h:212-218`).
- `future_version` and `process_behind` put the server in `failedUntil` with
  its own backoff, 1 s growing to 8 s (`QM.cpp:36-46`, `knobs:307-309`).

### Lagging requests

- When the other request wins, the loser's reply still updates the model when
  it comes (`LB.h:263-294`). Otherwise a slow replica's latency would never be
  recorded.

### TimedRequest

- A request records when it was decoded on a server (`fdbrpc/include/fdbrpc/TimedRequest.h:28-46`).
  Servers use it to separate queue wait from service time
  (`storageserver.cpp:2400`, `fdbserver/grvproxy/GrvProxyServer.cpp:490`).

## How the simulator models the network

FDB's simulator sits under FlowTransport, at the byte-stream level
(`sim2.cpp:292-417`), so the real framing, checksums, connect packets and
reconnects run in simulation.

- **Latency.** Each one-way hop draws from 50–400 µs 99.9 % of the time, and
  up to 50 ms otherwise (`sim2.cpp:275-287`, `knobs:252-254`).
- **Slow pairs.** Each pair of machines gets a fixed extra latency when its
  first connection opens, up to 100 ms in buggified runs
  (`sim2.cpp:307-312`, `knobs:255`).
- **Clogs delay, they do not refuse.** A clog holds bytes until it ends. It can
  apply to a pair, a process pair, or an interface's sends or receives
  (`sim2.cpp:195-214`, `:226-251`, `:2086-2117`). A clogged interface picks
  send, receive or both at random (`sim2.cpp:2087-2095`). The random clogging
  workload draws exponential durations and also swizzles
  (`fdbserver/workloads/RandomClogging.cpp:88-137`).
- **Disconnection.** A disconnected pair fails new connections and data in
  flight with `connection_failed` (`sim2.cpp:216-224`, `:322-329`,
  `:473-480`, `:2119-2127`).
- **Random closes.** Any read or write may close the connection on one side
  or both, reported at once or later (`sim2.cpp:540-563`). A run turns these
  on or off, and tests can switch them off near the end
  (`sim2.cpp:2606-2612`, `:2758-2785`).
- **Bounded buffers.** Each connection gets a random send buffer; a full one
  blocks the writer (`sim2.cpp:313`, `:444-446`, `:516-538`).
- **Fragmentation.** Writes are shortened at random, down to under 1000 bytes
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
(`FT.cpp:1842-1856`, `FM.h:136-139`). We pool connections per memory region
(`peer.go:97-133`), and each region dials, backs off and learns of failure
alone. The source counts connections per remote host to bound them
(`pagesource.go:519-533`), and the plan's down marks are kept "by each reader
on its own" (`plans/disk-cache-2026-10-02.md:212`).

Recommendation: one peer table per host, one entry per remote host, holding
its connections, liveness state, down mark and queue model. Post-copy faults,
the post-copy stream, fork `claim` requests, cache reads, keeps, drops,
presence and probes all go through it. A memory region keeps its own retry
rule but asks the table for a connection.

### 2. Multiplex by request ID, in two lanes

We carry `request_id` and `in_reply_to` in every envelope
(`wire/v1 proto:12-15`) and check them (`peer.go:427-430`), but use one
connection per request in flight (`pagesource.go:543-545`). FDB can share one
connection per peer because its messages are small: it warns past 2 MiB and
streams bulk data in 80 KB pieces (`knobs:238`, `ClientKnobs.cpp:172`). Our
page replies are 2 MiB (`docs/migration.md:878`) with up to 8 MiB in flight
per peer, so a 4 KiB fault reply would wait behind them on one stream. That is
why each memory region keeps a fault connection (`docs/migration.md:305-311`).

Recommendation: per peer, two connections, multiplexed by request ID:

- A latency lane for guest faults, probes, presence, `claim`, keeps without
  data, and drops.
- A bulk lane for the post-copy stream, stripe reads and fills with data.

The server answers in any order and bounds work by bytes in flight per peer,
not by connection count. A host draining 40 VMs to one destination then uses
two connections, not 160.

Keep one connection per direction. FDB merges directions and breaks dial
races by address order (`FT.cpp:1187-1223`); our dialer asks and our listener
serves, so two independent connections are simpler.

### 3. Negotiate the version once, at connect

We check a wire version on every frame and accept only 1
(`codec.go:132-135`). On a mismatch the server closes without a reply
(`pagesource.go:551-553`), and the destination retries pages only the source
holds until the hold ends (`vmmigrate/peer.go:646-695`). In a rolling upgrade
a mismatch would look like a flaky source for four checkpoint intervals,
visible only in the stalled-ask log line (`vmmigrate/peer.go:681-688`).

Recommendation, after FDB's connect packet and version range
(`FT.cpp:1138-1172`, `FT.h:136-152`):

- The dialer's first frame is a hello: the lowest and highest peer protocol it
  speaks, and its host identity. The server answers with the version both will
  use, or `INCOMPATIBLE` naming its own range.
- Per-frame `wire_version` then only guards framing. New request kinds (keep,
  drop, presence, probe) are gated by the negotiated version.
- `INCOMPATIBLE` is its own outcome, neither down nor gone. For post-copy it is
  as fatal as `ErrPageSize` (`vmmigrate/peer.go:728-730`). For the cache it
  removes that host from the reader's list. The peer table reports it in
  `/status`, as FDB lists incompatible peers (`FT.h:240-244`).
- A drain hands VMs from old hosts to new ones, so every release must speak
  the previous release's peer protocol. Test it with a two-version simulation.

### 4. Liveness apart from request timeouts

FDB judges a connection by bytes received, never by a slow request; a slow
server shows up in the queue model (`FT.cpp:759-787`, `QM.cpp:30-34`,
`LB.h:366-371`). We have no liveness check of our own: a source that vanishes
without a reset holds a guest fault until TCP keepalive gives up
(`platform/internal/real/network.go:56`), minutes with Go's defaults (Go
`net/dial.go:20-27`, nine probes), when a fault for a run a checkpoint holds
could read the volume after a few seconds.

The plan marks a host down after three timeouts in a row
(`plans/disk-cache-2026-10-02.md:212-219`). A timeout mixes a dead host, which
should be marked down, with a slow disk, which should get fewer requests.

Recommendation:

- The peer table pings each peer on its latency lane about once a second. A
  connection with no bytes received for a few pings is closed, and every
  request on it ends at once, as with FDB's `tryGetReply` (`GA.h:369-391`).
- The ping is answered on the connection's reader, never behind disk reads,
  as FDB runs ping at socket priority (`FT.cpp:326-329`).
- Down marks come from failed connects and dead connections; a slow request
  updates the queue model. Keep the plan's "at most a fifth" guard; FDB has
  only a `degraded` flag (`FT.cpp:1050-1064`).
- Keep the plan's probe schedule (10 s growing to 60 s). FDB reconnects within
  500 ms (`knobs:129`) because its peers are required; ours are a cache. FDB
  also damps hosts that flap (`FT.cpp:1066-1079`).

### 5. A down mark is a hint; only evidence means "gone"

FDB's `getReply` retries through a failed address, which may be wrong
(`FM.h:45-49`), and stops only when the remote says the endpoint is gone
(`FM.cpp:115-137`, `GA.h:411-433`). Our post-copy draws the same line: only
`STATUS_UNKNOWN_VM` or this host's close ends the asking
(`docs/migration.md:78-101`).

Recommendation: post-copy for pages only a down host holds ignores the down
mark; it may read the volume at once for runs a checkpoint holds. Cache reads
skip down hosts. Document this rule next to the peer table.

### 6. Choosing ranks: a queue model, and a hedge budget earned by fast answers

The plan reads all k+m ranks at once and takes the first k
(`plans/disk-cache-2026-10-02.md:231-233`), hedging every read in full: at
4+2 each read moves 1.5 times the envelope's bytes and costs six disk reads
instead of four, at 1+1 twice the bytes, and every reader adds load to the
slowest rank. FDB asks one replica and hedges after a delay (`LB.h:336-427`).

The plan's store hedge is a token bucket at 5 % of reads
(`plans/disk-cache-2026-10-02.md:244-252`). FDB's budget is the same size but
refilled only by answers before the hedge delay (`LB.h:588-612`), so hedging
stops when every server is slow. The plan needs that: a slowdown on every host
must not double the load on the store.

Recommendations:

- Refill the store-read bucket only by reads the cluster answered within the
  bound.
- Measure "ask k+1, hedge the rest" against "ask all k+m". Choose the k+1 by a
  per-peer queue model (smoothed outstanding requests, last latency, a hint
  from `BUSY`), and send the rest after a delay from measured latency, under a
  budget like FDB's. Keep all-k+m if it wins on tail latency and the disks and
  links have room.
- Record the latency of replies after the first k in the queue model, as FDB
  does for lagging requests (`LB.h:263-294`).
- Have the server record when it decoded each request, as `TimedRequest`
  does, and report queue wait and service time per request kind, to tell a
  slow disk from a slow link.

### 7. Back-pressure: answer `BUSY` in-band, and never close for a budget

FDB's transport queue is unbounded (`FT.cpp:1130-1136`); back-pressure is the
server's `server_overloaded` penalty, which the client does not count as a
failure (`LB.h:212-218`). Keep our hard per-peer byte budget at the source
(`pagesource.go:717-724`): one destination can ask for gigabytes, and the
source's memory and link are shared by every VM it hands over. Two changes:

- A connection over the per-peer connection budget is closed
  (`pagesource.go:536-541`), and the plan marks a host down after one refused
  connection (`plans/disk-cache-2026-10-02.md:214-215`), so a reader could mark
  a busy host down. With multiplexing the connection budget can go: every
  refusal is a `BUSY` reply, and a closed connection always means trouble.
- Let `BUSY` carry the server's queued bytes for that peer, or a suggested
  wait, as FDB's penalty does (`storageserver.cpp:1656-1663`). A cache reader
  skips a busy rank and weights it in the queue model. A post-copy
  destination keeps waiting, for as long as the hint says instead of doubling
  from 1 ms (`vmmigrate/peer.go:196-197`).

### 8. Checksums: cover the header; skip the payload CRC when something else covers it

We checksum the payload only (`codec.go:95-102`). For a page reply the
unchecked header holds the `present` and `dirty` bitmaps
(`migrate.proto:40-55`). A flipped `dirty` bit on plain TCP would make the
destination take a page as the checkpoint's bytes when it is the guest's own,
or the reverse, and nothing would catch it; TCP's checksum is 16 bits. The
plan drops the CRC on stripe replies because each stripe carries its own
checksum (`plans/disk-cache-2026-10-02.md:330-331`), but the header still
says which stripe, which envelope and whether it is present.

Recommendations, after FDB (`FT.cpp:2099-2172`, `:1372`, `:1443-1455`):

- Checksum the header on every frame with CRC32C. A self-checking payload can
  then skip the frame CRC, and `sendfile` stays possible.
- Let mutual TLS turn frame checksums off. Today `wire` always computes CRC32C
  for pages (`pagesource.go:835`).
- On a header checksum failure, drop the connection without answering: the
  header may name the wrong request.

### 9. Large payloads

Our raw payload beside a small header (`platform/network.go:43-50`) makes
`sendfile` of a stripe run possible, which FDB cannot do. Two changes:

- Cap one frame near 16 MiB instead of 1 GiB
  (`platform/internal/real/network.go:43`); our largest reply is a few MiB.
  This bounds what one bad header can make a reader allocate, as FDB's
  100 MiB cap does (`FT.cpp:1396-1401`).
- Put large replies on the bulk lane only, and give the latency lane a much
  smaller cap.

### 10. Name what a request is for, so a stale request cannot hit a new state

FDB's random token half makes a request to a reused slot find nothing
(`FT.cpp:195-207`). Our page requests name a VM and a volume by string
(`migrate.proto:26-33`), and a VM that left a host and came back has the same
name. We found no path today where a stale destination asks the new
incarnation, because a destination closes its backing before it can hand the
VM on, but the peer server will carry more request kinds and holders.

Recommendation: page and `claim` requests also carry the checkpoint sequence
the handoff names (`docs/migration.md:41-43`), and the server answers
`STATUS_UNKNOWN_VM` when it is not the one it serves. Cache requests name
content and need nothing.

### 11. Say which requests may be retried

FDB states each call's delivery rule (`rpc.h:760-819`). Our page and listing
requests are reads. Keep and drop are safe to repeat if a host drops a keep
for a stripe it holds (`plans/disk-cache-2026-10-02.md:302-303`). A retried
`claim` of a hold already claimed must answer OK.

Recommendation: each request kind declares in the proto whether it is
idempotent. The peer table retries only idempotent ones after a disconnect; a
non-idempotent one returns a "maybe delivered" error.

### 12. What `platform/sim`'s network should also model

Our simulated network is framed (`platform/sim/network.go:461-640`), with
latency and uniform jitter, a per-send bandwidth cost, partitions, clogs,
swizzles, and one-shot drop, duplicate, corrupt and delay
(`platform/sim/network.go:16-41`, `:183-323`). The campaign uses partitions and
swizzles (`internal/simtest/fault.go:68-69`). Add:

1. **A clog that delays.** Ours refuses a send with `ErrUnavailable`
   (`platform/sim/network.go:235-239`, `:532-534`), so the sender learns at
   once; a real partition is silent. Only a silent clog
   (`sim2.cpp:195-214`) tests liveness pings, hedge delays, down marks and the
   fault path's wait. Keep the refusing form as `Partition`.
2. **A heavy latency tail and slow pairs** (`sim2.cpp:275-287`, `:307-312`).
   Ours is uniform jitter (`platform/sim/network.go:364-368`). The queue model
   and the hedge matter only in the tail and for a rank that is always slow.
3. **Link bandwidth shared by all connections between two hosts.** Ours is
   per send and independent across connections (`platform/sim/util.go:27-32`),
   so eight connections get eight times the bandwidth. A queue per link would
   show head-of-line waits and compare per-region connections with lanes per
   host.
4. **Random connection closes** (`sim2.cpp:540-563`). Ours closes only on
   `Close` or a crash. Reconnects, pool trimming and replies lost on a dead
   connection need this.
5. **A connect that hangs** (`sim2.cpp:1199-1211`). Ours fails at once
   (`platform/sim/network.go:121-139`); a real dial to a vanished machine waits
   for SYN retries. Only a hang tests the connect timeout and the "one refused
   connection" rule.
6. **The real framer under fragmentation** (`sim2.cpp:387-401`, `:482-485`).
   Ours replaces the framer, so `platform/internal/real/network.go` never runs
   in simulation. Run the real framer over a simulated `platform.Transport` in
   some campaigns, which also tests the hello of lesson 3.
7. **Random header bit flips.** `CorruptNext` can flip a header bit
   (`platform/sim/network.go:563-570`), but only `codec_test.go:129` uses it.
   Turn it on at a low rate once headers are checksummed, and assert every
   flip is caught, as FDB does (`FT.cpp:1457-1461`).
8. **Bounded send buffers** (`sim2.cpp:313`, `:516-538`). A peer server that
   stops reading must stall its sender, not grow memory.
9. **A quiet end** (`sim2.cpp:2758-2785`). End a campaign with faults off and
   assert every drain finishes and every `Serving` set empties.
