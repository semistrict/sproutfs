---
id: TASK-92.1
title: 'Zircon port step 1: the licence and the package'
status: To Do
assignee: []
created_date: '2026-10-05 05:09'
labels:
  - pager
  - zircon-port
dependencies: []
references:
  - plans/zircon-pager-port-2026-10-05.md
parent_task_id: TASK-92
priority: high
type: task
ordinal: 99000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 1 of the plan. The port copies MIT code from fuchsia zircon/kernel into an Apache-2.0 repository, which is allowed only with the copyright and permission notice kept beside it. This step makes the package the port lands in, vmmemory/internal/zirconvm, and a check that keeps every ported file honest about where it came from, before any code is copied. See the Licence section of the plan.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 vmmemory/internal/zirconvm/LICENSE is a verbatim copy of zircon/kernel/LICENSE at fuchsia 90e54e09, with both copyright lines
- [ ] #2 A test in the package fails on a Go file that lacks the source copyright line and the Ported from line naming its Zircon files and revision, and on a Ported from line naming a file that does not exist at that path; the test is shown to fail on a file written to break each rule
- [ ] #3 internal/testdeterminism covers the new package, shown by a source in it that reads time.Now failing the rule
- [ ] #4 docs/vm-memory.md names the package and its licence where it lists the nested packages of vmmemory
<!-- AC:END -->
