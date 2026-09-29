---
id: TASK-66
title: Let every tenant fork a public template
status: To Do
assignee: []
created_date: '2026-09-29 23:10'
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
- [ ] #1 A VM of any tenant can be created from a public template, and a VM-to-VM fork, capture or create-from-checkpoint across tenants is still refused
- [ ] #2 A public template is stored outside every tenant prefix and is never opened for writing again after import
- [ ] #3 A tenant-less VM that is not a public template is still refused to a tenant VM
- [ ] #4 In the isolated arena, public pages live in one read-only file mapped into every VMM, and no tenant's own page enters it
- [ ] #5 A child of a public template publishes its writes under its own tenant, and deleting a tenant leaves public objects in place
- [ ] #6 Stored bytes billed to a tenant count none of the public template's pages
- [ ] #7 Adversarial tests: a VMM of one tenant reaches no page of another tenant through the public file
<!-- AC:END -->
