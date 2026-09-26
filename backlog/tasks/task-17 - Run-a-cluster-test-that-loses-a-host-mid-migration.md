---
id: TASK-17
title: Run a cluster test that loses a host mid-migration
status: Done
assignee: []
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 19:23'
labels:
  - embedder
  - gce
dependencies: []
priority: high
type: task
ordinal: 17000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs.

**The orchestrator's watch of a migration's source is unproven on a cluster.** The simulated deployment and the orchestrator's own tests over fakes cover it. The one soak run that passed killed a host that ran no VMs. So no GCE run has yet lost a host during a real migration (`cmd/sproutfs-orchestrator/lostsource_test.go`, `internal/simtest/lostmigrationsource_test.go`).
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A GCE run kills a host during a real migration and every VM ends running or reopenable
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
GCE run 2026-09-26, recorded in docs/measurements/gce-2026-09-26.md. New fix lost-migration in scripts/lib/demo-fixes.sh: a 3 GiB guest holds 2.5 GiB no checkpoint has; once the destination is receiving, a host is killed with sproutfsctl kill-host (POST /hosts/{name}/kill). Destination killed: the receive was retried on the replacement pod and the VM kept every byte (md5), pause 32.8 s, stream 22.6 s. Source killed: the orchestrator's watch saw the pod unlisted and ended the receive; the destination discarded the half-received guest; the VM and a bystander on the lost source ended running. Passed twice, shared arena. One defect remains: the orchestrator's own recoverLost failed both times with 'no host has 3221225472 bytes of memory free', because the destination's survey still counted the guest it had just discarded; the VM was left stopped and sproutfsctl recover reopened it 3 s later. So every VM ended running or reopenable, but not by the orchestrator alone.
<!-- SECTION:NOTES:END -->
