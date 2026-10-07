---
title: One disk cache for the whole host
summary: Every VM on a host reads through one disk cache, whatever its tenant.
---

**Given** a host that runs VMs of several tenants,
**when** any of them reads a page,
**then** it reads through the host's one disk cache. A page is cached once,
and every VM that names it is served from that copy.

**Status, 2026-10-03.** Holds. The host's page cache is keyed by page
identity. `TestPullsShareOneCopyAndClosingFreesNothing` shares one copy
between two pulls, and `TestTwoTenantsForkingOneImageShareNoPage` and
`TestEveryTenantCreatesFromAPublicTemplate` keep tenants apart where they
must be.
