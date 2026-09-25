---
id: TASK-27
title: Measure the attach populate bound and the batched span on a cluster
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
labels:
  - measurement
  - gce
dependencies: []
priority: medium
type: task
ordinal: 27000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**The attach populate's bound and the batched span are unmeasured on a
cluster.** The warm restore of 2026-09-22 spent 5.1 s of its 5.48 s in VMM
start. Before the guest ran, it mapped 2,930,747 sibling-resident pages in
21,698 runs, against a 0.5 s bound. Bounding the resident runs brought it to
4.77 s and 14,447 runs over 2,166,194 pages on 2026-09-23. Only 123,056 of
those pages were resident identities. The other two million pages were
scattered holes, and each paid one command. So holes and the runs a fork
point names now share one budget, and an attach installs at most 128 runs of
any kind (`vmmemory/population.go`). A batch's contiguous runs are
built in one reservation, so 64 scattered runs cost the VMM 136 kernel calls
instead of 320 (`rust/sproutfs-vm-memory/src/linux.rs`, `Staging`). Counts in
tests prove both changes: the Go suite, and the crate's own tests, which run
only on Linux. Neither change has been timed on GCE. On GCE the real cost is
the `mremap` per run and the REMAP event the pager reads back for it. That
cost cannot be batched. `mremap` moves a single mapping, and the runs of a
batch are separate mappings, so combining them would require a wire change.

After the bound, the walk remained. The walk went window by window over the
whole memory region, regardless of how much of the budget was left. Every window
asks the volume for the identity of every page in it. For a 16 GiB guest at a
4 KiB page, that is four million identities, decoded from the index's
segments before the guest runs. But a window reached with no budget left can
install no run. The walk now stops when the budget runs out
(`TestPopulationStopsWalkingWhenItsRunBudgetIsSpent`). The phases in the next
item are meant to show whether the walk is what a managed restore's seconds
were spent on.
<!-- SECTION:DESCRIPTION:END -->
