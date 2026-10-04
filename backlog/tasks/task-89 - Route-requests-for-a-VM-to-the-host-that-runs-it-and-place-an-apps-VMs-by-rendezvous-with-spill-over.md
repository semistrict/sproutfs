---
id: TASK-89
title: >-
  Route requests for a VM to the host that runs it, and place an app's VMs by
  rendezvous with spill-over
status: To Do
assignee: []
created_date: '2026-10-04 19:22'
labels:
  - orchestrator
dependencies: []
references:
  - 'https://pinggy.io/amp/blog/edge_functions_isolates_to_microvms/'
  - docs/hosting.md
priority: medium
type: feature
ordinal: 96000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Nothing sends a client's request to the host that holds a VM. The orchestrator places VMs through its API and a caller must find the host itself. Netlify keeps an app on the same compute node by rendezvous hashing so its image and snapshot stay cached there, and spreads it to the next ranked nodes above a load threshold. We already rank cache windows by rendezvous over the membership; an app's VMs could be placed the same way, so its template and checkpoints stay hot in one host's pager.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A request addressed to a VM reaches the host that runs it without the caller knowing the host, and reaches the new host after a migration
- [ ] #2 New VMs of one app (one template) are placed on its top-ranked host by rendezvous over the membership, and on the next ranked hosts once that host is over a stated load
- [ ] #3 A simulation test shows a join or leave moves only the apps whose top host changed
- [ ] #4 A GCE measurement compares the first faults of a VM placed by rank against one placed anywhere
<!-- AC:END -->
