---
id: TASK-40
title: Stop a migration receive that outlives its caller on another host
status: To Do
assignee: []
created_date: '2026-09-26 15:52'
labels:
  - embedder
dependencies: []
priority: low
ordinal: 47000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A migration now retries a failed receive (TASK-14). A receive whose caller gave up can still be running on the destination it was sent to while the retry goes to another host. The control record's epoch fences whichever of the two loses, so no data mixes, but the loser has started a guest for nothing and holds its pages until it is fenced. Preventing it needs each host to report the receives it has in flight, so that the orchestrator's evidence rules can wait for them or end them.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A host's Status lists the receives it has in flight
- [ ] #2 The orchestrator never retries a receive on another host while an earlier destination reports that receive in flight
- [ ] #3 A simulation scenario where a receive outlives its caller starts no second guest
<!-- AC:END -->
