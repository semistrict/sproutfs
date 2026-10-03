---
title: One disk limiter
summary: One limiter bounds everything the host writes to its disk, together.
---

**Given** a host whose disk holds spill files, ephemeral disks, VMM staging
and the disk cache,
**when** any of them grows,
**then** one limiter decides whether the host as a whole may use that space.
No part of the host has a disk cap of its own outside the limiter.

**Status, 2026-10-03.** Holds. One `resource.DiskLimiter` counts the spill files at their promises,
VMM staging and the cache, and nothing has a disk cap of its own.
`TestTheDiskLimiterStaysSafeUnderFaults` and
`TestASpillFileCountsAtItsPromiseWhileSparse` hold it.
