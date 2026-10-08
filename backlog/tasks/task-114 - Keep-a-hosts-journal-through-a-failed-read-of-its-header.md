---
id: TASK-114
title: Keep a host's journal through a failed read of its header
status: To Do
assignee: []
created_date: '2026-10-08 15:12'
labels:
  - journal
  - host
dependencies: []
priority: high
type: bug
ordinal: 153000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The journal disk pass in host/journaldisks.go closes a disk whenever journal.CheckLease fails, but CheckLease fails for any error reading the header, not only for a lease another member took (journal.ErrLeased). One I/O error from the device, which the simulated disk now returns at random under Buggify (sim/disk/io-error/read), closes the host own journal disk: SetJournal(nil), then a reopen on a later pass a minute on. A flush that was waiting on the closed journal is then never answered: in the journals campaign (internal/simtest, seed 8, about one run in five with only the disk sites on) a flush of vm-1 went unanswered for ten minutes, and the host logged "a journal disk is closed ... why=journal: reading the header: injected fault: input/output error". Until this is fixed, network disk devices are made with sim.DiskConfig.ReadsNeverFail, so the journal campaigns meet no read error. Found while writing scripts/faults/platform.json.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A read error in the lease check leaves the journal disk open, and only ErrLeased closes it
- [ ] #2 A flush in flight when the host own journal disk closes is answered, failed or journaled, and never waits for ever
- [ ] #3 Network disk devices fail reads at random again (ReadsNeverFail removed), and the journals campaign passes on repeated runs
- [ ] #4 Each bug has a regression test and a sim.Bug guard in scripts/mutation/guards.json
<!-- AC:END -->
