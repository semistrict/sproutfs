---
id: TASK-8
title: Create a VM from a stopped VM's published checkpoint
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 00:32'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 8000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. The host API needs to expose volume.Manager.Inherit, and the pin has to work when no host runs the parent.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 An API call creates a VM from another VM's published checkpoint
- [x] #2 It works when no host runs the parent, and the checkpoint is pinned
- [x] #3 It refuses a checkpoint of another tenant
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. control: Client.Pin pins a published checkpoint of a VM without taking its epoch. It is a conditional write (IfMatch) of the record as read, plus the pin, keeping the holder's epoch and nonce. It pins only the published checkpoint the record selects; a sequence already pinned needs no write; anything else is refused, because a writer may be reclaiming it.
2. control: Handle.update repeats its change on the record it adopts after a refused write, so a holder whose record gained a pin underneath it settles in one call and its next selection reports the pin to reclamation.
3. volume: Manager.InheritPublished pins through the client and then inherits the checkpoint, the fork point a create starts from.
4. host: Host.Published and Host.CreateAt (fork + root at a shape through Reshape), used by the supervisor for both a template and a checkpoint.
5. api: CreateRequest.From {vm, checkpoint}; server refuses From with Template; fake and tests.
6. orchestrator and sproutfsctl: create --from <vm>[@seq].
7. Docs: metadata.md (who writes a pin), volumes.md, hosting.md, deploy/README.md.
Tests first: control (pin without the writer, refusals, holder adopts), volume (stopped parent pinned, reopened parent reclaims around it, delete spares it), host (create from a stopped VM, refusals).
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Pin design: control.Client.Pin adds a pin without the epoch. It reads the record and writes it back with the pin under IfMatch on the version read, keeping the epoch and nonce; a moved record is read again. It names only the published checkpoint the record selects (a writer's sweep never deletes its own selection or what that names) or one a pin already keeps (no write). Anything else is refused with control.ErrNotPublished, because a sweep may have read the pins before the pin landed. A writer that holds the epoch has its next write refused, adopts the record (same epoch and nonce) and Handle.update makes its change again, so its selection reports the pin to reclamation. A refused write whose record cannot be read no longer fences, since a pin refuses a write as a takeover does. volume.Manager.Delete now removes the record conditionally (control.Client.Remove), so a pin added during a delete is spared by its sweep. Taking the epoch instead was rejected: it would fence a host that turns out to run the VM, or a concurrent restart of it.
API: host CreateRequest.From {vm, checkpoint?}; orchestrator CreateRequest.From; sproutfsctl create --from VM[@CHECKPOINT]. Same create path as a template: fork, root through Host.Reshape at the shape, cold boot. 409 for ErrNotPublished.
Tests: control/pin_test.go (pin keeps epoch, refusals, writer adopts, race with a selection, lost reply, unreadable refusal does not fence, conditional removal), volume/inherit_test.go (stopped parent pinned and survives its delete; running parent spares the pin in its own sweeps; refusals), host/createfrom_test.go (create from a stopped VM at a shape on another host; ErrExists; ErrNotPublished), server, orchestrator and CLI tests. Each new test was seen failing without its change. go build, GOOS=linux go vet and go test ./... pass. No Lima run: the supervisor path is a thin caller of tested Host and volume methods.
AC #3 (refuse another tenant's checkpoint) is not implementable yet: there is no tenant in the system. TASK-18 AC #3 (no fork, inherit or page sharing crosses tenants) covers it.

AC3 done with TASK-18: InheritPublished refuses a checkpoint of another tenant before pinning (volume/tenant_test.go).
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
A create can start from another VM's published checkpoint, including a VM no host runs. The checkpoint is pinned in that VM's record without its epoch by a conditional write that only names the selected published checkpoint or an already pinned one; a live writer adopts the pin. Wired through the host API (CreateRequest.From), the orchestrator and sproutfsctl create --from. Verified by control, volume, host, server, orchestrator and CLI tests. The tenant refusal (AC #3) waits on TASK-18.
<!-- SECTION:FINAL_SUMMARY:END -->
