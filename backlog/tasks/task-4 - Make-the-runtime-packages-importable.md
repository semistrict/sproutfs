---
id: TASK-4
title: Make the runtime packages importable
status: Done
assignee: []
created_date: '2026-09-25 18:17'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 4000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. It has to import host, volume, vmmemory, vmmigrate and api/host.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 host, volume, vmmemory, vmmigrate, vmmachine and api/host are outside internal/
<!-- AC:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Done in 8657fc0: the runtime packages and what their exported types name moved to the module root.
<!-- SECTION:FINAL_SUMMARY:END -->
