---
id: TASK-13
title: Authenticate hosts with JWT or mTLS
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
updated_date: '2026-09-29 23:10'
labels:
  - embedder
  - security
  - deferred
dependencies: []
priority: high
type: feature
ordinal: 13000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. Its setup uses JWT or mTLS.

The host API is protected only by a shared bearer token, which says a caller is part of the deployment and nothing more. The page server has no authentication at all: it serves every peer that reaches its listener (vmmigrate/pagesource.go:56) and relies on a network policy. It serves a migrating VM's memory, so an embedder that cannot trust its pod network binds it to loopback, and that rules out migration.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Host API, page server and peer dialer take pluggable credentials
- [ ] #2 Adversarial tests: forged and replayed credentials are refused, and a handoff whose source is not an authenticated host is refused
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Deferred by the owner on 2026-09-27.
<!-- SECTION:NOTES:END -->
