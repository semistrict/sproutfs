---
id: TASK-13
title: Authenticate hosts with JWT or mTLS
status: To Do
assignee: []
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 14:53'
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
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. Its setup uses JWT or mTLS. This replaces the shared bearer token on the host API and the page server.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Host API, page server and peer dialer take pluggable credentials
- [ ] #2 Adversarial tests: forged and replayed credentials are refused, and a handoff whose source is not an authenticated host is refused
<!-- AC:END -->
