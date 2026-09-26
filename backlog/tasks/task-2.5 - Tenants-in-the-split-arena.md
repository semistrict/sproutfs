---
id: TASK-2.5
title: Tenants in the split arena
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-25 23:02'
updated_date: '2026-09-26 15:14'
labels:
  - security
dependencies:
  - TASK-18
parent_task_id: TASK-2
priority: medium
type: feature
ordinal: 41000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Step 5 of plans/isolated-arena-2026-09-25.md: MemoryRegionBacking.Tenant from TASK-18, one shared file per tenant.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 No resident page is shared across tenants in isolated mode
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. MemoryRegionBacking.Tenant, an opaque string the host states: vmmachine from the VM's identity (control.TenantOf), simtest guests, host and migration test harnesses.
2. The check: every identity a region's backing reports, and every name a fork point gives, must name the region's tenant (control.TenantOf of its Ref.VM); otherwise the fault fails with ErrOtherTenant. The sharing index stays keyed by identity, which then names the tenant. Applies in both modes.
3. Isolated arena: one shared file per tenant (Host.shared map), made when the tenant's first region attaches, given to the tenant's regions as file 1, kept while a region of the tenant is attached or it holds a page, given back with its last page. New makes no file in isolated mode; Connect checks the arena, not file 0. Loads by identity, moves and the allocated-blocks check use the region's own tenant's file.
4. testpager: a mapping states its tenant; in an isolated arena a read-only file is given to one tenant's mappings only. So every campaign checks that no file, and so no resident page, is readable across tenants.
5. Tests: simulated pager test (two tenants inheriting one image's bytes keep separate shared files; an identity of another tenant fails the fault); a simtest campaign where two tenants fork their own templates of one image on one host; the reach test gains an other-tenant neighbour (identity-loaded, dirty and inherited pages unreachable) and the same-tenant concession (a published page its tenant inherits is reachable), and the shared-arena contrast reads all of them.
6. Docs (vm-memory isolated arena, plan status), then go test ./... in both modes, just check, the Lima pager suite and the Firecracker suite in both modes.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Built: MemoryRegionBacking.Tenant (vmmachine states control.TenantOf of the VM id); ErrOtherTenant for an identity or fork-point name of another tenant (both modes); per-tenant shared file in isolated mode (Host.shared map, made at the tenant's first attach, kept while a region is attached or it holds a page); New makes no file in isolated mode and Connect checks the arena. Test fixtures name VMs without a slash (vmName), since a subtest name's slash would name a tenant. testpager and the vmmemory fixture give a read-only file to one tenant only. Tests: vmmemory/tenant_test.go, internal/simtest/tenant_test.go (World.Sharing), reach test with an other-tenant process and the same-tenant concession. Mutation (one shared file for all tenants) is killed by the pager test, the campaign and the Lima reach test (hostile reads 33 other-tenant pages).
<!-- SECTION:NOTES:END -->
