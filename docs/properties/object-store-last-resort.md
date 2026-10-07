---
title: The object store is the last resort
summary: A restarted VM reads a page from the object store only when no host holds it.
---

**Given** a VM stopped hours ago,
**when** it starts again,
**then** it reads a page from the object store only when no host in the
cluster holds that page in memory or on disk.

**Status, 2026-10-03.** Holds for the hosts' disks, inside the share, with two exceptions.
`TestTheStoreIsReadOnlyWhenFewerThanKStripesExist` reads the store exactly
when fewer than k stripes of a page remain. A read that waits past its bound
reads the store as well, within a token bucket of a twentieth of reads
(`TestStoreReadsPastTheBoundStayWithinTheirBucket`). Another host's memory is
never asked.

**With a hot tier, 2026-10-03.** A deployment that reads through a
[hot tier](../hosting.md#reading-through-a-hot-tier) instead of the cluster
cache reads the regional bucket only for an object the hot tier does not
hold, or when the hot tier fails or is slower than its bound. A miss fills
the hot tier behind the read, and a publication writes it once the regional
PUT has succeeded, so a VM opened on another host reads its checkpoint from
the hot tier alone. `TestAMissIsFilledBehindTheReadAndTheNextReadHits` sends
the regional bucket no GET on the second read, and
`TestAVMOpenedOnAnotherHostReadsItsCheckpointFromTheHotTier` opens a VM on a
second host with three hits and no miss. Here "last resort" means the
regional bucket.

**A pull, 2026-10-04.** Holds for pulls inside the share. A pull asks each
window's ranks what they hold and reads the store only for a page with fewer
than k distinct indices. `TestAPullOfACheckpointTheClusterHoldsReadsNothingFromTheStore`
pulls a checkpoint the cluster holds with no request but the open of its
index, under 1+1, 2+2 and 4+2, and
`TestAPullOfACheckpointTheClusterPartlyHoldsReadsOnlyWhatItLacks` reads only
the two ranges of pages the cluster lacks. A pull never reads the store as a
hedge (`TestAPullsReadsOfTheClusterAreBulkWorkThatNeverHedges`).
