---
id: TASK-119
title: 'Give the orchestrator a fault campaign over its hosts, pods and records'
status: To Do
assignee: []
created_date: '2026-10-08 15:23'
labels:
  - testing
dependencies: []
priority: high
ordinal: 154000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
The orchestrator (cmd/sproutfs-orchestrator) calls each host through hostClient (the host API), the Kubernetes API through pods, and the bucket through records. Its tests fake all three, and the fakes fail only where a test switches a failure on (down, wedged, refuse, receives). That is the pattern that hid the mapping refusal which killed an embedder's VM (docs/testing.md, "Every boundary error is simulated"). Nothing lists these interfaces under scripts/faults, and no campaign meets a host that acted and lost its reply.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 scripts/faults/orchestrator.json lists hostClient, pods and records, with an entry for each error each method can return
- [ ] #2 The fakes return each at random at a sim.Buggify site, including a reply lost after the host acted
- [ ] #3 A seeded campaign drives creates, migrations, forks, drains, stops and recoveries through them and requires, once the faults stop, every VM to run on exactly one host or be stopped, and no VM to be lost
- [ ] #4 python3 scripts/check-faults.py --file scripts/faults/orchestrator.json passes
<!-- AC:END -->
