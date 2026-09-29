---
id: TASK-67
title: Read only the data extents of an imported image
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-29 23:10'
updated_date: '2026-09-29 23:34'
labels:
  - embedder
dependencies: []
references:
  - host/template.go
  - host/template_linux.go
priority: high
type: enhancement
ordinal: 75000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An import reads the image's whole apparent size twice: once to hash it (`imageDigest`, host/template.go:203) and once to write it (`importImage`). It already skips writing zero runs, but it still reads them. An embedder's root images are 16 GiB sparse files on a FUSE filesystem, so most of a 156 s import is reading zeroes.

A hole can be read as data: a FUSE filesystem without lseek support reports the whole file as one data extent. Measure on the embedder's filesystem before relying on the gain. Hashing 16 GiB of generated zeroes still costs several seconds of CPU.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 An import from a file with holes reads only its data extents, in both the hash pass and the write pass
- [x] #2 A sparse image and the same image written densely get the same template identity
- [x] #3 A source that is not a file, or a filesystem that reports no holes, imports as before
- [ ] #4 Measured: import time of a 16 GiB sparse image on the embedder filesystem, before and after
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. scanImage finds the image's size and data extents once: a host.SparseSource reports its own, an *os.File asks SEEK_DATA/SEEK_HOLE (unix only; other GOOS and non-file sources are one whole extent). Extents are validated ascending, apart, within size.
2. digest hashes extents read from the source and holes as generated zeroes, so identities are unchanged.
3. importInto reads only extents, skips all-zero batches as before, checkpoints every importCheckpointBytes.
4. Tests: a SparseSource that fails on any hole read; a real sparse file on the local filesystem; impossible extents refused; fileExtents leaves out a 3 MiB hole.
5. Document in docs/hosting.md.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Verified on macOS APFS (TestFileExtentsLeaveOutTheHoles, TestASparseFileIsTheTemplateOfItsDenseImage). The fallback for a filesystem that reports no holes is the whole file; a FUSE filesystem without lseek reports the whole file as one data extent, which is the same fallback.
AC 4 is the embedder's measurement: it needs their FUSE filesystem and a 16 GiB root image. It stays open.
<!-- SECTION:NOTES:END -->
