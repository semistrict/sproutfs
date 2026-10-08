---
id: TASK-113
title: Turn the object store's refusals on in every campaign
status: To Do
assignee: []
created_date: '2026-10-08 14:47'
updated_date: '2026-10-08 14:47'
labels:
  - store
  - simulation
dependencies:
  - TASK-112
priority: medium
ordinal: 152000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The simulated object store refuses a request, loses the reply to a write it applied, and resets a body partway (sim/object-store/unavailable/<operation>, reply-lost/<operation>, body-fails), but only on a store with ObjectStoreConfig.RequestChaos, which only the bounded campaign and the seeded topology campaigns under Buggify set. The rest meet none of it, so a caller that does not make a refused request again passes there and fails in a deployment. Two are known: a guest fault (TASK-112), and a VM create whose root publication the store refuses (the host-crash campaign, internal/simtest/crash_test.go, fails creating vm-1 with "publication: unavailable"); volume.Create refuses an identity with leftover objects, so a caller cannot simply create it again. Found while writing scripts/faults/platform.json.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The store refuses, loses replies and resets bodies under Buggify whatever its config, and RequestChaos holds only the request holds
- [ ] #2 Every campaign in go test ./... passes with that on, each refusal its callers meet handled as a deployment handles it
- [ ] #3 scripts/faults/platform.json names the checkpoint and host-crash campaigns for the store entries they fire
<!-- AC:END -->
