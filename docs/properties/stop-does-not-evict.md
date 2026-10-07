---
title: Stopping does not evict
summary: A stopped VM's pages stay in every tier until other work needs the space.
---

**Given** a VM with 8 GiB of resident memory that goes idle and is stopped,
**when** nothing else on the host needs that memory or disk,
**then** its pages stay where they were: in memory, and in the cluster's disk
cache.

**Status, 2026-10-03.** Partly. A stopped VM's pages leave a disk only when
its limiter needs the space.
`TestAVMOpensFromTheClusterAfterAnyOneHostIsLostDrainedOrRestarted` suspends a
VM and reads all of it back from the hosts' disks, and
`TestAPulledVMKeepsItsStopsCheckpointOnTheDisk` keeps a stopped pulled VM's
pages on its host's disk. No test yet shows the pager's arena keeping a
stopped VM's pages.
