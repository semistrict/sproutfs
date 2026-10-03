---
title: One disk cache for the whole host
summary: Every VM on a host reads through one disk cache, whatever its tenant.
---

**Given** a host that runs VMs of several tenants,
**when** any of them reads a page,
**then** it reads through the one disk cache that host keeps. A page is cached
once, and every VM that names it is served from that copy. There is no cache
per VM and no cache per tenant.
