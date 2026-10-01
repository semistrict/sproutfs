---
id: TASK-13
title: Let an embedder carry host-to-host traffic over its own transport
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-10-01 00:07'
labels:
  - embedder
  - security
dependencies: []
priority: high
type: feature
ordinal: 13000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedder runs sproutfs hosts inside its own sandbox fleet. Its hosts authenticate one another with JWT or mTLS on a transport fabric it already runs.

Authentication is the embedder's job, not sproutfs's. Sproutfs gives it a hook. Hosts reach one another over one channel: the page server, and the dialer a destination uses to reach it (vmmigrate/pagesource.go, host/migrate.go dialPages). Today that channel is plain TCP, and it must rely on a network policy.

The hook is a transport: a stream listener and a stream dialer that the host's network frames over. Plain unauthenticated TCP is the default. An embedder supplies its own, for example one that does mutual TLS, and refuses a peer it cannot authenticate.

The host API needs no new hook. Its client already takes an *http.Client, and an embedder serves the host library behind its own API.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A real network frames over a transport a caller supplies; plain TCP without authentication is the default
- [x] #2 A host's page server and its peer dialer use the transport of the host's network
- [x] #3 Adversarial tests with a mutual TLS transport: a migration between two hosts that trust each other succeeds; a peer without a trusted certificate is not served; a destination refuses a source it cannot authenticate
- [x] #4 Docs say where the hook is and that hosts trust the network only under the default transport
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Deferred by the owner on 2026-09-27. Rescoped on 2026-09-30: the owner said authentication is the embedder's job; sproutfs adds a transport hook and keeps plain TCP as the default.

platform.Transport (stream Listen and Dial) under the real network; adapters.NewNetworkOver and adapters.TCP. The real listener accepts on its own goroutine so a transport's listener needs no deadline. A TLS server handshakes on its first read, so a test must be reading before a dial completes.

A destination that cannot authenticate its source keeps asking, as for an unreachable source, until its caller cancels the receive; the VM is then discarded.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added platform.Transport, the stream fabric a real network frames over. Plain TCP (adapters.TCP) is the default; adapters.NewNetworkOver takes an embedder's own. The page server and peer dialer use the host network, so they use its transport. internal/testnet.MutualTLS is an example transport. Verified with tests in platform/internal/real/network_test.go (frames over mTLS; a refused peer's frames never arrive; a dial refuses an impostor) and host/transport_test.go (migration over mTLS serves 4 pages; a stranger destination gets no requests served; a destination refuses an untrusted source). Both suites were repeated 10 times; just check passes. Docs: hosting.md#transport, migration.md, deploy/README.md.
<!-- SECTION:FINAL_SUMMARY:END -->
