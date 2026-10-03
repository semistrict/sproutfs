---
title: The object store is the last resort
summary: A restarted VM reads a page from the object store only when no host holds it.
---

**Given** a VM stopped hours ago,
**when** it starts again,
**then** it reads a page from the object store only when no host in the
cluster holds that page in memory or on disk.

**Status, 2026-10-03.** Holds for the hosts' disks, inside the share, with two exceptions by design.
`TestTheStoreIsReadOnlyWhenFewerThanKStripesExist` reads the store exactly
when fewer than k stripes of a page remain. A read that waits past its bound
reads the store as well, within a token bucket of a twentieth of reads
(`TestStoreReadsPastTheBoundStayWithinTheirBucket`). Another host's memory is
never asked: memory is each host's own tier.
