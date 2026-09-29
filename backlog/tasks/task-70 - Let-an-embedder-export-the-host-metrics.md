---
id: TASK-70
title: Let an embedder export the host metrics
status: Done
assignee:
  - '@claude'
created_date: '2026-09-29 23:10'
updated_date: '2026-09-29 23:20'
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
- [x] #1 An embedder can produce the same Prometheus text the host binary serves, from an importable package
- [x] #2 The metrics include object-store errors and pages spilled to disk, added to Status if they are not there
- [x] #3 sproutfs-host serves the same output it serves today
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Move metrics() from cmd/sproutfs-host (package main) to api/host as exported Metrics(Status).
2. sproutfs-host serves hostapi.Metrics.
3. Test the library function for loss window, spills, store failures; test /metrics equals the library output.
4. Document for embedders in docs/hosting.md.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Spills (sproutfs_pager_spills_total) and object-store failures (sproutfs_store_failures_total) were already in Status and the exposition; nothing had to be added. Status comes from the supervisor's Status method, which an embedder already has.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Moved the Prometheus exposition to hostapi.Metrics(Status) so an embedder can serve it. sproutfs-host serves the same function. Verified by TestMetricsAreWhatAnEmbedderServes (api/host), TestMetricsAreTheLibraryExposition (byte-for-byte equality with /metrics), the existing /metrics tests, and just check.
<!-- SECTION:FINAL_SUMMARY:END -->
