---
title: Stopping does not evict
summary: A stopped VM's pages stay in every tier until other work needs the space.
---

**Given** a VM with 8 GiB of resident memory that goes idle and is stopped,
**when** nothing else on the host needs that memory or disk,
**then** its pages stay where they were: in memory, and in the cluster's disk
cache.
