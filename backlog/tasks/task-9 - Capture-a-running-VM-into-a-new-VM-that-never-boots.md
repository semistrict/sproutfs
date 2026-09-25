---
id: TASK-9
title: Capture a running VM into a new VM that never boots
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-25 22:37'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 9000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. The new VM publishes its root and is never started, so it can be forked or opened later.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 An API call captures a running VM into a new VM that publishes its root and does not boot
- [x] #2 The source VM keeps running
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. host: Host.CaptureInto(source, child) takes a fork point on a running VM (Seal), forks the child with Manager.Fork, publishes the child's root with the point's VMM state (Snapshot with Prepared state, no sources) reading the inherited pages through the point, closes the child, and retires the point. The source keeps running and is unsealed at the end on every path.
2. api: CaptureRequest.Into; VMs.Capture takes the request; supervisor calls Host.CaptureInto; server and fake.
3. orchestrator: capture --into allocates a new identity and records it stopped; sproutfsctl capture --new.
4. Docs: hosting.md, volumes.md, deploy/README.md.
Tests first in host with the capture harness: child record created and root published with state, source running and unsealed, child openable at the point's bytes, existing child identity refused with the source unsealed.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Host.CaptureInto(source, child): h.seal takes the fork point (pause, state, seal, parent writer pins its checkpoint); the capture takes its own hold on the point; Manager.Fork creates the child; vm.Snapshot(Prepared(point.State(), nil)) publishes the child's root, reading the sealed pages through the point and carrying the point's VMM state; the child is closed; the point is retired, which unseals the source on every path. No VMM starts and nothing is served. A plain vm.Checkpoint of the child would have named the parent's older published VMM state over the point's pages, so the root must carry the point's state.
API: host CaptureRequest{into}; VMs.Capture now takes the request; orchestrator CaptureRequest{new}, which allocates the identity and records the new VM stopped with the source as parent; sproutfsctl capture --new.
Tests: host/captureinto_test.go (root published with the saved state, source unsealed and checkpointing, new VM opens on another host with the source's pages; existing identity refused and source unsealed; source not running refused). Both main tests failed with the child's root published by Checkpoint and without the retire on failure. Server, orchestrator and CLI tests. go build, GOOS=linux go vet and go test ./... pass.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A running VM can be captured into a new VM that publishes its root and never boots. Host.CaptureInto forks a child over one pause, publishes its root from the sealed pages with the pause's VMM state, closes it, and retires the point so the source is unsealed and keeps running. Served as capture {into} on the host, {new} on the orchestrator and capture --new in sproutfsctl. Verified by host tests on fake machines plus server, orchestrator and CLI tests.
<!-- SECTION:FINAL_SUMMARY:END -->
