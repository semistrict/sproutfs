---
id: TASK-7
title: Register templates at runtime
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-25 22:21'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 7000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. A builder produces an ext4 image. The embedder imports it on request, and any host can then create VMs from it. This replaces the static SPROUTFS_TEMPLATES file list.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 An API call imports an image into a template named by its digest
- [x] #2 Any host creates VMs from a template another host imported
- [x] #3 Two hosts importing the same image at once end with one template
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Host-level: TestAnotherHostOpensATemplateByItsIdentity; the concurrent import is TestTwoHostsRacingToImportOneImageImportItOnce. API, relay, CLI and config have tests. The supervisor's staging and create-by-identity are Linux-only and are proven only by the GCE run at the end.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Committed ea6f963. Host.Template opens a template by identity from its control record; the supervisor imports images on request (POST /templates, staged when not seekable); creates take a template's identity on any host; SPROUTFS_TEMPLATES=none.
<!-- SECTION:FINAL_SUMMARY:END -->
