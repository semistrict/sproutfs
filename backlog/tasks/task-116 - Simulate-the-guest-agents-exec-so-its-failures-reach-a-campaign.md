---
id: TASK-116
title: 'Simulate the guest agent''s exec, so its failures reach a campaign'
status: To Do
assignee: []
created_date: '2026-10-08 15:23'
labels:
  - testing
dependencies: []
priority: medium
ordinal: 151000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The host runs a command in a guest through api/guest.Exec, over the vsock the VMM serves. Only the Linux supervisor calls it, and the deterministic simulation does not run the supervisor, so no campaign meets an exec that finds no guest, a handshake the VMM refuses, an answer past its bound or larger than MaxResultBytes, or nonsense from the guest. docs/testing.md ("Every boundary error is simulated") requires each to be returned at random by a simulated implementation and fired by a campaign; scripts/faults/host.json names this as missing.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The exec the host makes goes through an interface listed in a manifest under scripts/faults, with an entry for each error guest.Exec returns
- [ ] #2 A simulated guest agent returns each of those errors at random at a sim.Buggify site, and a campaign fires every one
- [ ] #3 python3 scripts/check-faults.py --file on that manifest passes
<!-- AC:END -->
