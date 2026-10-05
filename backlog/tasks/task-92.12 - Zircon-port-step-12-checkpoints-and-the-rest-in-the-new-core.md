---
id: TASK-92.12
title: 'Zircon port step 12: checkpoints and the rest in the new core'
status: To Do
assignee: []
created_date: '2026-10-05 05:10'
labels:
  - pager
  - zircon-port
dependencies:
  - TASK-92.11
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 110000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 12 of the plan. Everything else of the old core runs on the new one: the seal as WritebackBegin with D3 (protect in the pause, AwaitingClean in the walk behind it), the settle, the retire as WritebackEnd and a move into the identity root of the checkpoint that published the page, the abandon (D4), a fork point sharing its sealed pages as a temporary identity root, read dirty, the loss window and pressure, eviction and spill, cold copies and the give-back as the zero-fork scan widened to the origin, the isolated arena with tenants, moves and fork files, serving a peer and handoff. If it does not fit one reviewable change, it is divided between the seal and eviction.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The whole vmmemory suite, and the host, vmmigrate and internal/simtest suites, pass under SPROUTFS_PAGER_CORE=zircon in both arena modes; the named list is gone and just check runs them all under both cores
- [ ] #2 Every guard in scripts/mutation/guards.json is killed under both cores, every Buggify site fires and every probe is reached in the campaigns that require them, under both cores
- [ ] #3 The probe build audit (stable, bind, granted, retired, reshared) runs on the new core, and probe_internal_test.go passes against it
- [ ] #4 The seal pause issues only range protections, shown by the seal pause tests, and the walk runs after the vCPUs resume
- [ ] #5 The hostile, race and isolation Linux suites pass under the new core on GCE
<!-- AC:END -->
