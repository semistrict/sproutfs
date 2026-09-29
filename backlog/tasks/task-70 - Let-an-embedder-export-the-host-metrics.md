---
id: TASK-70
title: Let an embedder export the host metrics
status: To Do
assignee: []
created_date: '2026-09-29 23:10'
labels:
  - embedder
dependencies: []
references:
  - cmd/sproutfs-host/metrics.go
  - api/host/host.go
priority: high
type: enhancement
ordinal: 78000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedder runs the host as a library, so it cannot scrape its metrics. The exporter is in cmd/sproutfs-host/metrics.go, which is package main. The embedder cannot see the loss window, spilled pages, VMs held back past their window, or object-store errors.

`metrics()` reads only `hostapi.Status`, so it can move to an importable package.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 An embedder can produce the same Prometheus text the host binary serves, from an importable package
- [ ] #2 The metrics include object-store errors and pages spilled to disk, added to Status if they are not there
- [ ] #3 sproutfs-host serves the same output it serves today
<!-- AC:END -->
