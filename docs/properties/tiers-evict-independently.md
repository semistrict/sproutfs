---
title: Tiers evict independently
summary: Eviction from memory does not evict from disk, and one host's eviction does not evict from another.
---

**Given** a stopped VM's pages held in memory and in disk caches,
**when** pressure evicts a page from one tier,
**then** the other tiers keep their copy until their own pressure evicts it.
