---
title: Tiers evict independently
summary: Eviction from memory does not evict from disk, and one host's eviction does not evict from another.
---

**Given** a stopped VM's pages held in memory and in disk caches,
**when** pressure evicts a page from one tier,
**then** the other tiers keep their copy until their own pressure evicts it.

**Status, 2026-10-03.** Partly. The pager's arena and each host's disk evict
by their own pressure alone.
`TestDiskEvictsTheOldestRegionFirst` and `TestPullsShareOneCopyAndClosingFreesNothing`
hold the disk to it. No test yet fills the arena until a stopped VM's pages
leave it and then reads them from the disks.
