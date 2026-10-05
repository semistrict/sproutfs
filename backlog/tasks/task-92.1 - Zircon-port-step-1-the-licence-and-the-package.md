---
id: TASK-92.1
title: 'Zircon port step 1: the licence and the package'
status: Done
assignee:
  - '@claude'
created_date: '2026-10-05 05:09'
updated_date: '2026-10-05 05:47'
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
- [x] #1 vmmemory/internal/zirconvm/LICENSE is a verbatim copy of zircon/kernel/LICENSE at fuchsia 90e54e09, with both copyright lines
- [x] #2 A test in the package fails on a Go file that lacks the source copyright line and the Ported from line naming its Zircon files and revision, and on a Ported from line naming a file that does not exist at that path; the test is shown to fail on a file written to break each rule
- [x] #3 internal/testdeterminism covers the new package, shown by a source in it that reads time.Now failing the rule
- [x] #4 docs/vm-memory.md names the package and its licence where it lists the nested packages of vmmemory
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Copy zircon/kernel/LICENSE verbatim to vmmemory/internal/zirconvm/LICENSE.
2. Record every file under zircon/kernel at fuchsia 90e54e09 with its copyright lines in the package's testdata, written by a script from a checkout, so the header rule can check paths and copyright lines without the checkout (CI has none).
3. A test in the package holds every .go file in it, except the rule's own test file, to the header: the source's copyright lines, then 'Ported from zircon/kernel/<path> [and <path>] at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.' It is run over sources written to break each rule.
4. internal/testdeterminism already walks vmmemory with its subdirectories; add a test that runs the rule over a tree holding a time.Now in vmmemory/internal/zirconvm and requires the finding.
5. Name the package and its licence in docs/vm-memory.md and docs/testing.md.
6. just check.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
LICENSE is cmp-identical to zircon/kernel/LICENSE at 90e54e09. The ~/src/fuchsia clone is partial (blob:none) with a sparse tree, so scripts/zircon-sources.py lists every path under zircon/kernel from the tree objects at the revision and reads copyright lines with GIT_NO_LAZY_FETCH=1: 2640 files, 796 of them without a local blob and so without copyright lines (none under the vm files the port draws from). The header test fails a file naming one of those, telling the porter to regenerate the list from a clone that holds it. header_test.go is the one Go file exempt from the rule, as it is not ported. TestTheHeaderRuleRejectsEachBrokenHeader covers a missing copyright line, a missing Ported from line, another source's copyright, one of two copyright lines, another revision, a path not at the revision, a path with no recorded copyright, a missing licence pointer and a header after the package clause; a real file naming vm/vm_page_listt.cc failed TestEveryFileNamesTheZirconSourcesItIsPortedFrom. internal/testdeterminism already walked vmmemory's subdirectories; the walk is now one function, check, and TestTheRuleReachesThePortOfZircon runs it over a temp tree with a time.Now in vmmemory/internal/zirconvm and gets exactly that finding. docs/vm-memory.md's list of nested packages named internal/latency, which is now top-level internal/latency; it now names zirconvm instead and says where latency is. just check exited 0.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Added vmmemory/internal/zirconvm with a verbatim copy of zircon/kernel/LICENSE, a list of the files under zircon/kernel at fuchsia 90e54e09 with their copyright lines (written by scripts/zircon-sources.py), and a header test that holds every ported Go file to its sources' copyright lines and a Ported from line naming existing files at that revision, shown failing on headers broken each way. The determinism rule's walk is shown to reach the package. docs/vm-memory.md and docs/testing.md name the package and its licence. Verified with go test ./vmmemory/internal/zirconvm ./internal/testdeterminism and just check (exit 0).
<!-- SECTION:FINAL_SUMMARY:END -->
