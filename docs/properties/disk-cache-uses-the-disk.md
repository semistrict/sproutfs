---
title: The disk cache uses the disk it is given
summary: A host with a large, fast local SSD keeps as much of what it reads as the disk limiter allows.
---

**Given** a host with a large, fast local SSD,
**when** its VMs read and publish pages,
**then** the host keeps those pages on the SSD, up to what the disk limiter
allows, so that later reads come from the SSD and not from the object store.

**Status, 2026-10-03.** Holds inside the share the cluster cache is on for, within the disk limiter's
share. Reads of the store and publications fill the cluster
(`TestAStoreReadFillsExactlyTheRankedCaches`,
`TestAPublicationFillsNothingBeforeItsPartIsDurable`), and later reads come
from the disks (`TestAPageInTheClusterIsReadWithNoStoreRead`). Outside the
share a host keeps only what pulls copy.
