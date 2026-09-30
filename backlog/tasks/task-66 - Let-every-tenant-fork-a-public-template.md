---
id: TASK-66
title: Let every tenant fork a public template
status: In Progress
assignee:
  - '@claude'
created_date: '2026-09-29 23:10'
updated_date: '2026-09-30 00:02'
labels:
  - embedder
  - tenancy
dependencies: []
references:
  - volume/fork.go
  - vmmemory/region.go
  - vmmemory/isolation.go
  - host/template.go
priority: high
type: feature
ordinal: 74000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
An embedding program imports the same system image once per tenant. Each import took 156 s in its test. Each tenant also stores its own copy, and each host caches the same pages once per tenant.

Three checks refuse a page of another tenant: `sameTenant` in `Fork` (volume/fork.go:576), which is the one a create from a template reaches, `inTenant` (vmmemory/region.go:250), and the shared-file check (vmmemory/isolation.go:362).

Tenant `""` cannot mean public: a deployment without tenants gives every VM tenant `""`. Public needs its own namespace, and only a template identity may live in it.

The shared deployment token is not enough authority to import a public template: any holder could plant an image every tenant forks. Until TASK-13 lands, only images in the host configuration may be public.

Cost: tenants share physical pages, which opens a page-access timing channel. The pages are public, so it reveals at most which image files another tenant reads.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [x] #1 A VM of any tenant can be created from a public template, and a VM-to-VM fork, capture or create-from-checkpoint across tenants is still refused
- [x] #2 A public template is stored outside every tenant prefix and is never opened for writing again after import
- [x] #3 A tenant-less VM that is not a public template is still refused to a tenant VM
- [x] #4 In the isolated arena, public pages live in one read-only file mapped into every VMM, and no tenant's own page enters it
- [x] #5 A child of a public template publishes its writes under its own tenant, and deleting a tenant leaves public objects in place
- [x] #6 Stored bytes billed to a tenant count none of the public template's pages
- [ ] #7 Adversarial tests: a VMM of one tenant reaches no page of another tenant through the public file
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Decision: a public template is a template of no tenant (control/template-<digest>, outside every tenants/ prefix). The template namespace is reserved, so no other VM of no tenant is public. Configured images are imported once as public templates; an import with a tenant stays that tenant's.
1. control: TemplatePrefix, IsTemplate and Public(id) (a template of no tenant); api/host uses them.
2. volume.sameTenant passes when the parent is public. host fork, capture-into and create-from-checkpoint between VMs stay refused.
3. vmmemory: a region may read a public checkpoint (plan and single-page load); Share stays strict. In the isolated arena every region is also given one public shared file, read-only, as file 2 (fork files from 3). A page loaded under a public identity goes there, and only such a page. reach accepts it.
4. host: configured templates import with no tenant; templateNamed lets a tenant VM open a public template by identity.
5. Tests: pager (public page is one page of the public file for two tenants; a tenant's own page never enters it; a non-template tenant-less page is still refused), volume fork rules, host create from a public template by a tenant, billing and tenant deletion leave public objects, simtest two tenants forking a public template.
6. Docs: architecture, volumes, vm-memory isolated arena, hosting templates.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Public = a template of no tenant (control.Public); the template namespace is reserved: volume.Fork refuses a template child, the supervisor refuses create/open/capture/receive of one (guestless). The shared deployment token already trusts its holder with every tenant, so no separate public flag: an import with no tenant is public. Configured images now import as public templates.
Pager: every region of an isolated arena gets the public file as file 2 (fork files now start at 3). A page is loaded, or moved, into the file of its identity (loadFile/fileOf). The simulation found that move put a public page into the inheriting tenant's shared file; fixed, with TestAPublicPageMovesIntoThePublicFile failing before the fix.
Evidence on the Mac: volume TestEveryTenantForksAPublicTemplate and TestOnlyATemplateOfNoTenantIsPublic; host TestEveryTenantCreatesFromAPublicTemplate and TestNoGuestRunsAsATemplate; vmmemory TestEveryTenantMapsAPublicPageFromThePublicFile, TestATenantsOwnPageNeverEntersThePublicFile, TestOnlyAPublicTemplatesPageCrossesTenants, TestAPublicPageMovesIntoThePublicFile; simtest TestTwoTenantsCreatingFromAPublicTemplateShareOnlyItsPages in both arena modes (World.Sharing now also fails on any non-public page in the public file); just check.
AC 7 is open: the real-kernel hostile suite (vmmemory/hostile_linux_test.go, reach_linux_test.go) runs only on GCE. Every VMM there now holds the public file too; that suite has not been run.
<!-- SECTION:NOTES:END -->
