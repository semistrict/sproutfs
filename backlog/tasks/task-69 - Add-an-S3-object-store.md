---
id: TASK-69
title: Add an S3 object store
status: To Do
assignee: []
created_date: '2026-09-29 23:10'
updated_date: '2026-09-29 23:15'
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
- [ ] #3 A host can be configured to use it
<!-- AC:END -->
