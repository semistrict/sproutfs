---
id: TASK-87
title: >-
  Give every fork child and restored guest a new generation ID and a corrected
  clock
status: To Do
assignee: []
created_date: '2026-10-04 19:21'
labels:
  - vm-memory
  - firecracker
dependencies: []
references:
  - 'https://pinggy.io/amp/blog/edge_functions_isolates_to_microvms/'
  - third_party/firecracker/CHANGELOG.md
priority: high
type: bug
ordinal: 94000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Every child of one fork point resumes from the same VMM state, so its kernel random pool, its userspace RNG seeds and anything it derived from them start identical in every child. A restore from a checkpoint repeats them too. Firecracker in third_party has a VMGenID device and a VMClock device for exactly this, but nothing in the host, vmmachine or the docs makes sure they are enabled, that the guest kernel acts on them, or that the clock jump after a long stop is told to the guest. Netlify names VMGenID and VMClock as the fix for repeated snapshot restores.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 Two children of one fork point read different bytes from /dev/urandom right after they resume, shown by a Firecracker test on GCE
- [ ] #2 A guest restored after a stop sees the VMGenID change and a wall clock that is right within a stated bound
- [ ] #3 docs/vm-memory.md states which devices a VM gets, what the guest kernel must support, and what a guest userspace must do itself
- [ ] #4 The host refuses, or warns once per VM, when a VM is started without the device the docs require
<!-- AC:END -->
