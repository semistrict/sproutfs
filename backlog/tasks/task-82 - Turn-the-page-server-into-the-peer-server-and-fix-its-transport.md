---
id: TASK-82
title: 'Turn the page server into the peer server, and fix its transport'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-03 04:52'
updated_date: '2026-10-03 13:13'
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
- [x] #1 The channel is called the peer server everywhere: docs/context.md defines it, the docs use it, and the code that serves handoffs, fork points and cache requests lives outside vmmigrate; existing metric names keep working
- [x] #2 Each host keeps one peer object per remote host, shared by every request kind, with a small connection pool per traffic class (guest faults, bulk reads, bulk writes) and a byte budget per class; a request over budget is answered BUSY with how busy, never by closing the connection
- [x] #3 A frame is sent in one vectored write, a payload is read into a pooled buffer of its known length, and a frame can carry a file range that the TCP adapter sends with sendfile; frames are capped near 16 MiB
- [x] #4 Every frame header is checksummed; a payload checksum is optional and is omitted for cache stripes, which carry their own
- [x] #5 A connection opens with a hello that states a version range; a host accepts the current and the previous version and answers INCOMPATIBLE otherwise, and a release is tested against the previous one
- [x] #6 Liveness comes from pings and a short no-bytes-received timeout with TCP_USER_TIMEOUT; a peer is marked down only on hard failures, never on a request its caller cancelled, is probed back from about 1 s up to 10 s, and every cache request names the cache it expects so a reused address answers not me
- [x] #7 One prioritised background budget per host paces bulk work: unpublished post-copy pages, then the rest of the post-copy stream, then cache fills, then repairs; fills and repairs over budget are dropped, not queued; each reader bounds its stripe bytes in flight
- [x] #8 platform/sim network models what FoundationDB simulates and ours does not: clogs that hold bytes, a heavy latency tail and links that stay slow, bandwidth shared per host pair, random connection closes, hanging connects, the real framer over fragmented streams, header bit flips and bounded send buffers
- [x] #9 Tests follow the repo practice: synctest bubbles over platform/sim, Buggify sites with probes a campaign asserts, sim.Bug guards in scripts/mutation/guards.json, and Gremlins on the new code; a GCE run reports fault latency under a concurrent post-copy stream and stripe reads before and after
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

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Commit 1 (5c8abac3): moved PageSource, the wire codec, the page protocol and the destination's client from vmmigrate into a new package peer (peer.Server). vmmigrate keeps Migrate, Fork, Receive, PeerBacking and the Pages adapters. peer/peertest reads and rewrites frames for wire-level tests. Metric names, env vars and JSON fields unchanged.
Commit 2 (3978f855): platform/internal/framer shared by the TCP adapter (and the simulation later): one vectored write per in-memory frame, file ranges by sendfile on Linux (copied elsewhere), 16 MiB cap on both networks, a failed mid-frame send closes the connection. Header checksum is a CRC32C trailer field (Envelope.header_checksum, field 6) so the frame prefix stays the previous release's; wire version 2 requires it. A header that fails to parse or match is ErrCorrupt (retried on a new connection), never ErrMalformed (which tears a receive). Payload checksum optional, mismatch sticky. Replies read into pooled buffers of known length. The Linux sendfile test has to run on Linux (GCE).
Commit 3 (91a4426a): hello with version range, INCOMPATIBLE, v1 compatibility both ways (fallback when a hello is closed unanswered), peer/internal/previous frozen from main 6af0d3de; tests at request level and a whole migration from a previous-release source.

Commit 4 (1e92a13e): peer.Table/Peer, one peer per remote host; pools per class (fault 2, bulk read 2, bulk write 1); multiplexed connections, replies in arrival order; server budgets per (host, class) 8/16/16 MiB answered with Busy (held/budget/asked); no connection refused; client never asks past the budget the hello gave. Receive takes the host table. Connection-budget tests converted to byte-budget tests; premortem_pool_test replaced by TestABurstHoldsNoMoreConnectionsThanItsClassMay. Crash campaign kill window 2 ms -> 4 ms (hello round trips lengthen a fork receive). Commit 5 (e3c580f3): sim network holds, heavy tail, slow pairs, shared host-pair bandwidth, bounded send buffers, hanging dials, random-close and header-bit-flip sites, Network.Framed (real framer over fragmented byte streams, stream bit-flip site). Commit 6 (67794339): liveness: ping after 1 s quiet, dead after 4 s without a byte, connect timeout 3 s, idle close 30 s; down marks on hard failures only; probes 1 s x1.5 to 10 s; incompatible state; keepalive and TCP_USER_TIMEOUT 10 s; /status peers and sproutfs_peers. Host table on the wall clock. Commit 7 (7f4fc678): cache request kinds dispatched to peer.Cache, NOT_ME, stripe replies without payload checksum (file range payloads). Commit 8 (486b0668): peer.Background prioritised budget, fills/repairs dropped, faults shrink it, per-reader stripe bytes bound.

Campaign, sites, guards (297995fb): TestThePeerServerCampaignNeverAnswersWrong (peer) runs seeds 1, 6 and 8 (64 under SPROUTFS_TEST_SOAK) over framed and message networks with every new sim fault; it requires every answer right, every peer back after the faults, every peer.Probes probe reached and every peer and sim network site activated. It found three bugs, fixed: a stream write spinning after its connection closed, version 1 connections never found dead (now dead after requestTimeout + DeadAfter of owed silence), and a probe answered INCOMPATIBLE leaving its peer down. New sites peer/slow-answer and peer/stall. Ten sim.Bug guards in scripts/mutation/guards.json and docs/testing.md, each shown to fail its test. TestAGuestFaultIsAnsweredWhileTheStreamSaturatesTheLink: slowest fault 4.9 ms against a 16.6 ms bound at 256 MiB/s with a 4 MiB background budget; 102 ms with peer-unbounded-background. Gremlins on peer (13 files): before 370 killed / 90 lived / 12 timeouts / 4 build errors of 476; after new tests 401 / 62 / 12 / 4 of 479. Also fixed: a hello settling version 1 crashed the server (nil first request).

Docs (61879c19): context.md defines peer server, peer, class, BUSY, background budget, down; migration.md has a peer server section; hosting.md the transport; testing.md the sim network faults, the campaign, sites, probes, guards. Kept SPROUTFS_PAGE_SERVER_PORT, page_address and every metric name for compatibility; docs say so. Plan step 11 changed: the release before has no stripe requests, so stripe reads have no before; the GCE run measures them over the peer server alone and beside the stream, and the report points to gce-stripes-2026-10-03 for the bench protocol's numbers.

Mutation tests (12963688): new tests for 40-odd survivors; found and fixed a server crash when a hello settled version 1. Gremlins final: 404 killed by tests / 61 lived / 12 timeouts / 4 build errors of 481 (before: 370/90/12/4 of 476); handoffs.go+session.go with --integration against vmmigrate tests: 95 of 119 killed. Survivors are equivalent or speed-only (pool size classes, buggify delay lengths, bitmap length, map cleanup, least-loaded choice, zero-means-default bounds, v2 reply checksum branch); internal/wire survivors include mutants peer tests kill (non-integration runs only the mutated package's tests). GCE (60b9fe4f, docs/measurements/gce-peer-server-2026-10-03.md, two n2-standard-4, 3 rounds, deleted and verified): beside the stream fault p99 612 -> 97 ms, p99.9 1640 -> 106 ms, max 2158 -> 118 ms, busy 214 -> 0, refused connections ~25k -> 0; median 48 -> 56 ms, stream 429 -> 300 MB/s (budget shrinks to a quarter while faults wait). Idle fault 31 -> 28 ms p50; both builds bound by the page codec's CPU (~14 ms source + 10 ms destination per 2 MiB page), not the link. Stripe reads over the peer server: p50 0.4 ms but p99 27 ms alone, p99 63 ms beside the stream, because they share fault connections with 2 MiB page faults (follow-up). Linux-only sendfile and TCP_USER_TIMEOUT tests and the peer suite passed on GCE.

Kill campaign (2cc6eb8a, 323c65ed): under SPROUTFS_ARENA=shared the fork-destination scenario reached its span on no seed, because the hellos made the fork ~4 ms and the span (receive returned, root not landed) is a fraction of a millisecond. World.ChildReceived now signals the receive's return and the kill is drawn over 2 ms from it: 4 of 8 seeds cut the root in both arenas. just check passes at 323c65ed.

Merged into main as 9d084d00 with doc conflicts resolved (peer server naming combined with step 10's cache-disk status). just check on main passed, exit 0, including TLC and the shared-arena run.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
The page server is now the peer server (package peer): framed protocol with one vectored write per frame, sendfile for file ranges, 16 MiB cap, CRC32C on every v2 header, a hello stating a version range (v1 and v2 spoken, INCOMPATIBLE otherwise, tested against a frozen copy of the previous release), one peer per remote host with pools and budgets per class answered BUSY, pings and silence for liveness with down marks only on hard failures, cache request kinds dispatched to peer.Cache with NOT_ME, and a prioritised background budget. platform/sim gained FoundationDB's network faults. Verified by synctest suites, a peer campaign over every fault (three bugs found and fixed), ten sim.Bug guards each failing its test, Gremlins (61 of 481 alive, survivors justified), just check, and a GCE run: beside a post-copy stream a fault's p99 fell from 612 to 97 ms and p99.9 from 1.6 s to 106 ms (docs/measurements/gce-peer-server-2026-10-03.md).
<!-- SECTION:FINAL_SUMMARY:END -->
