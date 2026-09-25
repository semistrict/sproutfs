---
id: TASK-24
title: Build the collector that releases pins
status: To Do
assignee: []
created_date: '2026-09-25 18:18'
labels:
  - deferred
dependencies: []
priority: low
type: feature
ordinal: 24000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
**Nothing releases a pin. The collector is deferred indefinitely, and until it exists the store grows without bound.** On 2026-09-15 the owner decided to keep pins correct and permanent, and not to write a collector, background or otherwise, for now. Accumulating data is accepted. Every object a pin covers accumulates and is never released. A pin records that a checkpoint of a VM was forked, and the pin is permanent. No participant can tell that nothing reads through a pinned checkpoint any more, because a descendant sees neither its siblings nor the forks taken below it, and a grandchild's root names its grandparent's checkpoints directly. So these objects accumulate: every checkpoint at which any VM was ever forked, every checkpoint that checkpoint's root names, and everything a deleted VM leaves pinned (`control/record.go`, `volume/fork.go`, `volume/manager.go`, `checkpoint/reclaim.go`). Only a collector can release a pin, and it must handle:
- **Pins nothing reads through any more.** This is the common case: every child forked from that point has been deleted, or every child has published a root that names none of the checkpoints the pin protects. Establishing this requires reading every live record's selected root, including the roots of VMs on other hosts. So the answer holds only against a survey that also accounts for what is in flight.
- **Pins nothing ever read through**: a fork that failed after the pin, a fork point retired with no child taken from it, a child abandoned before it published its root, and a host lost between the pin and the child's record.
- **The objects of deleted VMs.** A delete removes the record and sweeps what no pin covers. So what is left under `vm/<id>/ckpt/` is exactly the pinned checkpoints of a VM that no longer has a record. Nothing names them. The collector must reach them from the roots of the VMs that still read them, or by listing the deployment's objects against its live set.
- **Checkpoints no handle will ever reclaim**: the checkpoint a handle opened on, which the handle cannot account for, and the objects of a writer that died mid-checkpoint or published after being fenced.
- **A delete interrupted between removing the record and sweeping**, and a VM whose record cannot be parsed. Such a VM cannot be deleted until the record is repaired.
- **In-flight publications and forks**, which the collector must not collect. A checkpoint's earlier parts exist before its last part does, and a pin exists before the child that reads through it.
- **`MaximumPins`**, 4096 per record. A VM forked at that many distinct checkpoints reaches this limit, because no collector releases any pins.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The owner has decided to build it
<!-- AC:END -->
