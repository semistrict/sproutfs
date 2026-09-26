---
id: TASK-18
title: Put a tenant prefix in object keys
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-25 23:31'
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
- [ ] #1 Every object of a VM is under its tenant's prefix
- [ ] #2 Deleting a tenant's prefix removes all its data and no other tenant's
- [ ] #3 No fork, inherit or page sharing crosses tenants
<!-- AC:END -->
