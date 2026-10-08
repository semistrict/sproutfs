---
id: TASK-117
title: Format a journal disk again in the journal campaign
status: To Do
assignee: []
created_date: '2026-10-08 15:23'
labels:
  - durability
  - testing
dependencies: []
priority: medium
ordinal: 152000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A recovery replays the journals a VM's record names. Where a journal disk was formatted again, Replayer.Replay returns journal.ErrGeneration, the open is refused with volume.ErrJournalLost, and only an operator's discard opens the VM, without the flushes that disk held. No campaign formats a journal disk again, and none has an oracle that accepts the loss of exactly those flushes, so that path runs nowhere. scripts/faults/volume.json defers its entry here.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A seeded site formats a journal disk again at random in the journal campaign
- [ ] #2 A VM whose record names such a disk is refused with ErrJournalLost, opens only by an operator's discard, and loses exactly the flushes that disk held, which the campaign checks
- [ ] #3 The deferred volume.Replayer entry in scripts/faults/volume.json names that site and campaign, and check-faults passes
<!-- AC:END -->
