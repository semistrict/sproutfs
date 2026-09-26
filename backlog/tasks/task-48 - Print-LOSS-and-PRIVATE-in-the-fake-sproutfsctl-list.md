---
id: TASK-48
title: Print LOSS and PRIVATE in the fake sproutfsctl list
status: To Do
assignee: []
created_date: '2026-09-26 19:23'
labels:
  - chore
dependencies: []
priority: low
ordinal: 55000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
scripts/test/fake-sproutfsctl.sh still prints list without the LOSS and PRIVATE columns the real CLI prints. Found in the GCE run of 2026-09-26.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The fake prints the same columns as sproutfsctl list, and the shell tests that read it pass
<!-- AC:END -->
