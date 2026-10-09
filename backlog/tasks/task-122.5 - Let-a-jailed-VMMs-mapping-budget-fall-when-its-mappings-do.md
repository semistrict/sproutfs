---
id: TASK-122.5
title: Let a jailed VMM's mapping budget fall when its mappings do
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-09 00:48'
updated_date: '2026-10-09 00:56'
labels: []
dependencies: []
parent_task_id: TASK-122
priority: high
type: bug
ordinal: 161000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A Firecracker the host runs jailed has no /proc, so VmaBudget (rust/sproutfs-vm-memory/src/vma_budget.rs) opens no /proc/self/maps and its estimate is cumulative: every MAP or MAP_ZERO command adds 6, and nothing subtracts. After limit/6 commands (87,381 at the host's 524,288) it refuses every map with ENOSPC for the rest of the session, whatever the process holds: on GCE 2026-10-09 the jailed VMM had 326 mappings when every map was refused, and the guest froze (TASK-122.4). The jail is how every deployment runs, so a busy guest freezes after enough faults. The budget has to know what the process maps without /proc: the client applies every MAP, MAP_ZERO and REVOKE itself, so it can keep the count of mappings its own memory regions make (or an fd of /proc/self/maps opened before the jail), and a revocation must lower the estimate.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A jailed VMM's VMA estimate follows its real mappings: it falls when revocations merge them back into traps
- [ ] #2 A test in the crate drives more than limit/6 map and revoke commands with no /proc/self/maps and none is refused while the real count is far under the limit
- [ ] #3 The PostgreSQL benchmark runs with no refused mapping
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Count the region's mappings in the client from the commands it applied (a run map, Mappings over a generic Runs that Generations shares), and admit each command against that count plus six per installing replacement; no /proc.
2. Unit tests: the count matches an independent per-page model; 100,000 map/zero/revoke rounds are never refused while the region holds one mapping; a region holding many is refused and admitted again after revocation.
3. A session test drives a real UFFD session and checks the count against the kernel's mappings of the region after every command.
4. Run the crate's tests on Linux (GCE node, privileged container), rebuild the demo image, and rerun the PostgreSQL benchmark that froze.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Implemented: rust/sproutfs-vm-memory/src/runs.rs (generic run map, Generations now wraps it), src/mappings.rs (Mapped::Trap / Zero / File{file, delta}), vma_budget.rs (limit only, admit(mapped, replacements)), lib.rs records each applied command. The first kernel run showed a private-file page mapped immutable merges with the next mapped writable (the private file is shared either way; immutability is page-table write protection), so a file run is file and continuing offset only. All 63 crate tests pass on Linux with --include-ignored --test-threads=1, including the new kernel comparison. Go comments (host/pager_linux.go, vmmemory/connection_linux.go) and docs/vm-memory.md no longer say a budget without /proc is disabled.
<!-- SECTION:NOTES:END -->
