---
id: TASK-82
title: 'Turn the page server into the peer server, and fix its transport'
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-03 04:52'
updated_date: '2026-10-03 07:21'
labels:
  - performance
  - network
dependencies: []
references:
  - plans/disk-cache-2026-10-02.md
  - docs/migration.md
  - docs/hosting.md
documentation:
  - docs/research/foundationdb-transport-2026-10-03.md
  - docs/research/cockroachdb-rpc-2026-10-03.md
priority: high
type: feature
ordinal: 89000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Hosts talk to each other over one channel, today called the page server (vmmigrate.PageSource, the framed protocol in vmmigrate/internal/wire and vmmigrate/proto/sproutfs/migrate/v1). The distributed disk cache (TASK-81) puts stripe reads, keeps, drops, presence checks and probes on the same channel, so it is no longer only a page server, and the cache is not migration. The owner was worried a custom protocol would bring needless performance problems. Research on FoundationDB (docs/research/foundationdb-transport-2026-10-03.md) and CockroachDB (docs/research/cockroachdb-rpc-2026-10-03.md, read for ideas only, BSL) both say to keep our framed protocol rather than move to gRPC or net/http: CockroachDB still hit small requests stuck behind large ones under HTTP/2 and fixed it with more connections; gRPC copies every message, cannot use sendfile, and its default window is smaller than one stripe. Decided on 2026-10-03: keep and extend the framed protocol, renamed the peer server. The research found these defects in what exists: one request at a time per connection; per-region connections, so each memory region detects a dead peer on its own; Send makes three writes per frame; payloads are read with io.ReadAll; the frame header carries the page bitmaps with no checksum; an exact wire-version match, so a mismatched frame closes the connection silently (vmmigrate/pagesource.go:551-553) and a rolling upgrade looks like a flaky source until the hold ends; connections over the per-peer budget are closed rather than answered BUSY, so a guest fault can be refused behind a stream; only TCP keepalive at 30 s for liveness; frames may be up to 1 GiB.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The channel is called the peer server everywhere: docs/context.md defines it, the docs use it, and the code that serves handoffs, fork points and cache requests lives outside vmmigrate; existing metric names keep working
- [ ] #2 Each host keeps one peer object per remote host, shared by every request kind, with a small connection pool per traffic class (guest faults, bulk reads, bulk writes) and a byte budget per class; a request over budget is answered BUSY with how busy, never by closing the connection
- [ ] #3 A frame is sent in one vectored write, a payload is read into a pooled buffer of its known length, and a frame can carry a file range that the TCP adapter sends with sendfile; frames are capped near 16 MiB
- [ ] #4 Every frame header is checksummed; a payload checksum is optional and is omitted for cache stripes, which carry their own
- [ ] #5 A connection opens with a hello that states a version range; a host accepts the current and the previous version and answers INCOMPATIBLE otherwise, and a release is tested against the previous one
- [ ] #6 Liveness comes from pings and a short no-bytes-received timeout with TCP_USER_TIMEOUT; a peer is marked down only on hard failures, never on a request its caller cancelled, is probed back from about 1 s up to 10 s, and every cache request names the cache it expects so a reused address answers not me
- [ ] #7 One prioritised background budget per host paces bulk work: unpublished post-copy pages, then the rest of the post-copy stream, then cache fills, then repairs; fills and repairs over budget are dropped, not queued; each reader bounds its stripe bytes in flight
- [ ] #8 platform/sim network models what FoundationDB simulates and ours does not: clogs that hold bytes, a heavy latency tail and links that stay slow, bandwidth shared per host pair, random connection closes, hanging connects, the real framer over fragmented streams, header bit flips and bounded send buffers
- [ ] #9 Tests follow the repo practice: synctest bubbles over platform/sim, Buggify sites with probes a campaign asserts, sim.Bug guards in scripts/mutation/guards.json, and Gremlins on the new code; a GCE run reports fault latency under a concurrent post-copy stream and stripe reads before and after
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Layout. A new top-level package peer holds the peer server and the peer table; vmmigrate keeps Migrate, Fork, Receive and PeerBacking and calls into it.
- peer: Server (was vmmigrate.PageSource) with the handoff book (Serve, Release, Discard, Claim, Serving, Outstanding), per-(remote host, class) byte budgets answered BUSY with held and budget bytes, hello, pings, and dispatch to the handoff book and to a Cache interface. Table: one Peer per remote address, shared by every request kind; per Peer a small pool per class (fault, bulk read, bulk write), multiplexed connections, liveness, down marks and probes. Background: the prioritised background budget. Cache: the interface the checkpoint cache implements in steps 6-7 (read stripes, keep, drop, presence; probe answered by the server from its identity).
- peer/internal/wire: the envelope codec, versions 1 and 2, the header checksum, hello. peer/proto: migrate.proto (moved, same proto package, so v1 bytes do not change) and peer.proto (hello, ping, busy, cache requests).
- platform/internal/framer: the framer shared by the TCP adapter and a new simulated byte-stream transport: one vectored write per frame, payload read into a buffer of known length, a file-range payload the TCP adapter sends with sendfile on Linux, a 16 MiB cap.

Versions. Protocol 1 is what main speaks: no hello, one request at a time. Protocol 2 opens with a hello stating a range, checksums every header (CRC32C trailer field in the envelope, so v1 decoders still parse it), multiplexes requests with replies in arrival order, and answers pings at once. A host speaks 1 and 2: a v1 first frame is served the v1 way; a client whose hello is closed unanswered falls back to v1 on a fresh connection; a range outside {1,2} is answered INCOMPATIBLE. A frozen copy of main's client and server (peer/internal/previous) tests the release against the previous one.

Commits, each with tests and docs:
1. Rename and move: PageSource -> peer.Server, wire and the client into peer; callers in host, simtest, cmd and vmmachine follow. No behaviour change; metric names unchanged.
2. Framer: shared framer package, vectored writes, buffers of known length, file-range payloads with sendfile in the TCP adapter (Linux), 16 MiB cap; header checksum in the codec.
3. Hello: version range, INCOMPATIBLE, v1 compatibility both ways, the previous-release test.
4. Peer object: Table/Peer, pools per class, multiplexing, server budgets per class answering BUSY with how busy; Receive takes the host's table.
5. Liveness: pings, no-bytes timeout, TCP_USER_TIMEOUT and keepalive in the TCP adapter, down marks on hard failures only, probes 1 s growing to 10 s, cache requests naming the expected cache (NOT_ME).
6. Cache request kinds as typed frames dispatched to peer.Cache, with tests against a test cache.
7. Background budget: unpublished post-copy, rest of the stream, fills, repairs; fills and repairs dropped over budget; stripe bytes in flight bounded per reader; faults in flight shrink it.
8. platform/sim network: holding clogs, heavy tail and slow pairs, bandwidth shared per host pair, random closes, hanging connects, byte-stream transport for the real framer with fragmentation, header bit flips, bounded send buffers.
9. Tests: synctest over platform/sim, a peer campaign with Buggify sites and probes it asserts, small-behind-large, sim.Bug guards in guards.json and docs/testing.md, Gremlins on the new files.
10. Docs: context.md defines the peer server, page server renamed across docs, migration.md and hosting.md describe the transport as built.
11. GCE: fault latency under a concurrent post-copy stream, before (main) and after, and stripe reads through the peer server against the bench's own protocol; docs/measurements/gce-peer-server-2026-10-03.md.
<!-- SECTION:PLAN:END -->
