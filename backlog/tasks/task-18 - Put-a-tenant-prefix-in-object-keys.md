---
id: TASK-18
title: Put a tenant prefix in object keys
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 00:32'
labels:
  - embedder
  - security
dependencies: []
priority: high
type: feature
ordinal: 18000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. It needs per-tenant deletion and data residency.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Every object of a VM is under its tenant's prefix
- [x] #2 Deleting a tenant's prefix removes all its data and no other tenant's
- [x] #3 No fork, inherit or page sharing crosses tenants
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Resident pages are never shared across tenants because page identity is the publishing checkpoint's ref, which names the tenant; the isolated arena's per-tenant shared file (TASK-2.5) adds the file boundary. The demo orchestrator lists only the root namespace.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Committed 3479246. Tenant is part of the VM identity (<tenant>/<name>) and of every key (tenants/<tenant>/...). Forks, inherits, captures and templates never cross tenants. Verified by control, volume and host tests.
<!-- SECTION:FINAL_SUMMARY:END -->
