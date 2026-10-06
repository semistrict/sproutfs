---
id: TASK-96
title: 'Require hosts with SHA instructions, or check pages with a cheaper digest'
status: Done
assignee: []
created_date: '2026-10-05 17:20'
updated_date: '2026-10-06 16:18'
labels:
  - decision
  - performance
dependencies: []
priority: medium
ordinal: 116000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
SHA-256 is about 6 ms of a 2 MiB fault on Cascade Lake, which lacks SHA instructions, and 47% of a publication's CPU. Ice Lake and newer have them. Either the deployment requires such hosts, or pages are checked with a cheaper digest, which changes the stored format.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 The owner picks one
- [x] #2 The choice is enforced (deploy manifests or host start refuses) or implemented with a format change assessed for compatibility
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
2026-10-06 owner decision: use a cheaper page digest than SHA-256; no compatibility plan, nothing deployed depends on the current format.

2026-10-06: the owner left the digest to the coordinator: XXH3-128 (github.com/zeebo/xxh3, BSD-2; licence to be verified when added). It checks for corruption, which is the digest's job; nothing depends on the current format.

2026-10-06: implemented in cc29c1e6. Envelope format 2: magic SPB2, 32-byte header, XXH3-128 of the decoded bytes (github.com/zeebo/xxh3 v1.1.0, BSD-2-Clause, verified from its LICENSE). Format 1 envelopes are refused with the version named. The object digest attribute (checkpoint/store.go digestOf, used by putObject and the hot tier) is XXH3-128 hex. Formats holding envelopes bumped: index 9, part layout 5, disk cache 3; old fixtures kept as superseded and refused by version. Left on SHA-256: template identity (host/image.go, api/host), shard identity (membership/controller.go) and rank seeds (rank/) are identity and placement, not integrity; the sim object store's ETag (platform/sim/util.go) is simulation only; the peer wire ChecksumSHA256 option is unused in production (peers send CRC32C). The pager's isolated-arena digest is BLAKE3, untouched. Apple M5 Pro, under load 12-26: XXH3-128 of 2 MiB 0.10 ms vs SHA-256 0.8 ms (BenchmarkDigest); raw 2 MiB envelope decode 0.75 -> 0.10 ms; BenchmarkAReaderRebuildsA2MiBPage data 1.16 -> 0.53 ms, parity ~4.3 (load spike) -> ~1.05 ms. Not measured on Cascade Lake. just check exits 0; every run guard killed.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Envelopes and object digests use XXH3-128 instead of SHA-256 (envelope format 2, index 9, part 5, disk cache 3), with fixtures and docs updated; verified by just check (exit 0, guards killed) and benchmarks on an M5 Pro.
<!-- SECTION:FINAL_SUMMARY:END -->
