---
id: TASK-12
title: Report stored bytes per VM for billing
status: Done
assignee:
  - '@claude'
created_date: '2026-09-25 18:17'
updated_date: '2026-09-26 01:43'
labels:
  - embedder
dependencies: []
priority: high
type: feature
ordinal: 12000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program replaces JuiceFS with sproutfs in its sandbox host. This is one of the changes it needs. Each page is billed to the VM that published it.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 Each VM reports the bytes it published that the store still holds
- [x] #2 A simulation invariant: billed bytes summed over VMs equal the bytes in the store; compaction moves bytes but not the bill; reclamation reduces it
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. Refactor volume/consistency.go: one function takes a deployment key apart into the VM it is stored under; the audit keeps object sizes.
2. volume.StoredBytes(ctx, store, prefix, tenant): list <ns>control/ and <ns>vm/, sum object sizes per VM identity. Exact (it is what the store holds), costs one LIST per 1000 keys, no GET or HEAD. A deleted VM's pinned checkpoints stay billed to it.
3. checkpoint.CheckIndex: every member of a part was published by the part's own VM (origin VM == holder VM). This is why compaction moves bytes but not the bill.
4. volume.CheckDeployment: per-namespace StoredBytes equals the audit's per-VM tally, and no stored byte belongs to no VM. Every sim campaign runs it.
5. Guard checkpoint-compact-another-vm (compaction of a parent's checkpoint into a child) to prove the check catches a moved bill.
6. Host API: VMs.Stored(ctx, tenant), GET /stored?tenant=, api/host client.
7. Tests: volume test with exact bills over compaction, reclamation, fork, deleted parent and tenants; server handler test.
8. Docs: volumes.md billing section, hosting.md endpoint, testing.md deployment check and guard.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
WIP afd80f4: platform.ListAll, control.TenantNamespace, volume.StoredBytes (stored.go), audit bill check in CheckDeployment, CheckIndex origin check, guard checkpoint-compact-another-vm. Next: tests, host API GET /stored, docs.

WIP d00c629+: volume test TestEachPageIsBilledToTheVMThatPublishedIt passes and fails under SPROUTFS_SIM_BUG=checkpoint-compact-another-vm; host API GET /stored + client + handler test. Next: docs, full go test, just check.

Design: the bill is computed from what the store lists (exact), not counters. volume.StoredBytes lists <ns>control/ and <ns>vm/ for one tenant: one LIST per 1000 keys, no GET/HEAD. Every page's bytes sit under its publisher's key because compaction only rewrites a VM's own checkpoints (now enforced by CheckIndex: part members must have the part's VM as origin). Exposed as volume.StoredBytes and host API GET /stored?tenant= (not in /status: it lists the store). go test ./... passes.

Validation: go test ./... and just check pass. TestEachPageIsBilledToTheVMThatPublishedIt asserts exact bills through a fork, a compaction (+ the rescued copy, same VM), the sweep after it (- exactly the emptied checkpoint), a deleted parent (pinned bytes stay billed, no record), a VM of no tenant, and sum of bills == store bytes. SPROUTFS_SIM_BUG=checkpoint-compact-another-vm makes it fail through the new CheckIndex origin check. CheckDeployment (run by every simtest campaign) now requires the per-tenant bills to match the store listing.
<!-- SECTION:NOTES:END -->

## Final Summary

<!-- SECTION:FINAL_SUMMARY:BEGIN -->
Stored bytes per VM come from what the store lists, so they are exact. volume.StoredBytes(ctx, store, prefix, tenant) lists the tenant's control/ and vm/ keys (one LIST per 1000 keys, no GET/HEAD) and sums sizes per VM identity, including deleted VMs whose pins remain. Served at host API GET /stored?tenant= and api/host Client.Stored. A VM's bill holds because every page sits under its publisher's key: compaction only rewrites a VM's own checkpoints, and CheckIndex now rejects a part member another VM published. CheckDeployment requires the bills to add up to the store. Refactors: platform.ListAll replaces five hand-written listing loops, control.TenantNamespace, one key parser (ownerOf/checkpointKey) shared by the audit and the report. New guard checkpoint-compact-another-vm. Docs: volumes.md#billing, hosting.md, testing.md. Verified by go test ./..., just check, and the guard run.
<!-- SECTION:FINAL_SUMMARY:END -->
