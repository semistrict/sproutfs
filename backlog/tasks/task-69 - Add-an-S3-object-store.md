---
id: TASK-69
title: Add an S3 object store
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-29 23:10'
updated_date: '2026-09-29 23:45'
labels:
  - embedder
  - platform
dependencies: []
references:
  - platform/object_store.go
  - platform/internal/real/gcs.go
  - platform/internal/real/object_store_conformance_test.go
priority: high
type: feature
ordinal: 77000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Sproutfs ships only a GCS adapter, and an embedder runs hosts on AWS. The object store needs conditional put (`IfNoneMatch`, and `IfMatch` on the ETag) and conditional delete (`DeleteRequest.IfMatch`, platform/object_store.go:146). Confirm S3 general-purpose buckets support each of these before building.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 An S3 adapter in platform/internal/real passes the object-store conformance suite against a real bucket
- [ ] #2 A put conditioned on a stale ETag, a create of an existing key, and a delete conditioned on a stale ETag are each refused with platform.ErrPrecondition
- [x] #3 A host can be configured to use it
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. real.S3ObjectStore over aws-sdk-go-v2 (Apache-2.0): Head, ranged/suffix Get with size from Content-Range, Put with IfNoneMatch */IfMatch, Delete with IfMatch, ListObjectsV2; errors mapped by HTTP status (409 conditional conflict is ErrUnavailable).
2. adapters.NewS3, adapters.NewObjectStore and ObjectStoreFromEnvironment (SPROUTFS_OBJECT_STORE gcs|s3, SPROUTFS_S3_ENDPOINT); both binaries use them.
3. Conformance: new subtest for If-Match delete; S3 suite on gofakes3 (MIT) with a shim for the S3 behaviours it lacks; real-bucket suite gated on SPROUTFS_TEST_S3_BUCKET. GCS suite moved behind a shim for the generation-match delete fake-gcs-server ignores.
4. Docs: hosting.md, deploy/README.md.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Verified S3 conditional deletes on general purpose buckets exist (AWS, 2025-09). Emulator gaps shimmed in tests, each an S3/GCS behaviour: conditional DELETE (gofakes3, fake-gcs-server), no whole-object checksum on a ranged GET, a suffix longer than the object returns the whole object (gofakes3).
The conditional-delete conformance subtest is new, so it also covers the GCS adapter.
AC 1 and 2 need a real bucket: the AWS SSO session here has expired, so TestS3ObjectStoreConformanceOnABucket has not run. Run: aws sso login, then SPROUTFS_TEST_S3_BUCKET=<bucket> go test ./platform/internal/real -run OnABucket.
<!-- SECTION:NOTES:END -->
