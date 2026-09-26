---
id: TASK-38
title: Fork from an older checkpoint that was marked kept
status: To Do
assignee: []
created_date: '2026-09-26 01:40'
labels:
  - embedder
dependencies: []
documentation:
  - docs/hosting.md
  - docs/metadata.md
priority: high
ordinal: 44000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A fork can start only from a VM's newest checkpoint or from one a pin already keeps. The next checkpoint's reclamation deletes every older one, so a user cannot go back to an earlier point in a VM's life and start a new VM there. The owner decided on 2026-09-25 that older checkpoints stay forkable only when someone asks for it: a checkpoint request can mark its checkpoint kept, and only kept checkpoints cost storage beyond the newest. The owner also decided that a fork from a checkpoint that holds memory and VMM state resumes the guest where it was, as a fork of a running VM does, and that a fork from a checkpoint without state boots cold over its disks. Today a create from a published checkpoint (TASK-8, CreateRequest.From) always boots cold. A pin is permanent until a collector exists (TASK-24), so a kept checkpoint that has been forked cannot be released safely without one.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A checkpoint request (suspend, capture and the host's checkpoint call, through the host API and the CLI) can mark the checkpoint it publishes kept; a kept checkpoint survives every later reclamation
- [ ] #2 A VM's kept checkpoints can be listed, with each one's sequence, time and whether it holds VMM state
- [ ] #3 A create from a kept checkpoint of any VM of the same tenant succeeds whether or not that VM runs, and whatever checkpoints it has published since
- [ ] #4 A fork from a kept checkpoint that holds VMM state resumes the guest from that state with its memory; one without state boots cold over its disks, and the result says which
- [ ] #5 A kept checkpoint that no fork was taken from can be released, and reclamation then deletes what only it held; releasing one that was forked is refused
- [ ] #6 Simulation invariants: a kept checkpoint's pages are never reclaimed while it is kept; a child started from it reads exactly that checkpoint's bytes, never a later one's
- [ ] #7 docs/hosting.md and docs/metadata.md describe kept checkpoints
<!-- AC:END -->
