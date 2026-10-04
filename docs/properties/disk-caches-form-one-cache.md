---
title: The hosts' disk caches form one cache
summary: A page any host's disk cache holds is served to every other host before the object store is asked.
---

**Given** a cluster of hosts, each with a disk cache,
**when** a host needs a page that is not in its own memory or on its own disk,
**then** it reads the page from another host's disk cache if any host's cache
can serve it, and from the object store only if none can. Together the hosts'
caches behave as one cache whose size is the sum of their disks.

**Status, 2026-10-03.** Holds inside the share. `TestAPageInTheClusterIsReadWithNoStoreRead` reads
every page any host's publication filled from the cluster on every host, and
`TestAColdBurstFillsAWindowOnce` fills a window once for a burst of readers.

**A pull, 2026-10-04.** Unchanged. Inside the share a pull keeps no whole
copy on its own host's disk: it fills only what the cluster lacks, and the
pulling host keeps only the stripes the membership ranks it for. `TestAPullOfACheckpointTheClusterHoldsReadsNothingFromTheStore`
leaves the pulling host's disk as it was, and the pull campaign
(`TestPullsSurviveTheirFaultsAndReachTheirProbes`) finds no host holding a
window whole or a stripe no list ranked it for.
