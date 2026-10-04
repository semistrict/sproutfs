---
id: TASK-90
title: Suspend an idle VM and wake it when a request arrives
status: To Do
assignee: []
created_date: '2026-10-04 19:22'
updated_date: '2026-10-04 19:22'
labels:
  - orchestrator
  - host
dependencies:
  - TASK-89
  - TASK-87
references:
  - 'https://pinggy.io/amp/blog/edge_functions_isolates_to_microvms/'
priority: medium
type: feature
ordinal: 97000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
A VM that serves nothing still holds its host's memory and CPU. Stop and start exist, but nothing stops a VM that has been idle or starts it again when a request for it arrives. Netlify scales idle VMs to zero and restores them from a snapshot on the next request, paying about 9 ms on 1.2% of requests.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A VM with an idle timeout set is checkpointed and stopped after that much time with no request
- [ ] #2 A request for a stopped VM starts it, on the host routing chooses, and is answered once the guest runs; requests that arrive meanwhile wait and are all answered
- [ ] #3 Docs state what idle means, what a woken guest sees (clock, connections) and the wake latency measured on GCE
<!-- AC:END -->
