---
title: Two hosts are enough
summary: The cluster's disk cache works in a deployment of two hosts, and survives losing either one.
---

**Given** a deployment of two hosts,
**when** their VMs read and publish pages,
**then** the two hosts' disks form one cache, as a larger cluster's do, and
either host alone still serves every page in it while the other is lost,
drained or restarted.

**Status, 2026-10-03.** Holds. Under 1+1 each host holds every window whole.
`TestAPageSurvivesLosingDrainingOrRestartingAnyOneHost` and
`TestAVMOpensFromTheClusterAfterAnyOneHostIsLostDrainedOrRestarted` lose,
drain or restart either host of two, and the other reads every page from its
own disk.
