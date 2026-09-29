---
id: TASK-67
title: Read only the data extents of an imported image
status: To Do
assignee: []
created_date: '2026-09-29 23:10'
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
- [ ] #1 An import from a file with holes reads only its data extents, in both the hash pass and the write pass
- [ ] #2 A sparse image and the same image written densely get the same template identity
- [ ] #3 A source that is not a file, or a filesystem that reports no holes, imports as before
- [ ] #4 Measured: import time of a 16 GiB sparse image on the embedder filesystem, before and after
<!-- AC:END -->
