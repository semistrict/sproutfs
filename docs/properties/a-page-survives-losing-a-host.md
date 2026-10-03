---
title: A cached page survives losing a host
summary: Losing, draining or restarting any one host costs the cluster's disk cache no page.
---

**Given** a page in the cluster's disk cache,
**when** any one host is lost, drained, restarted or added,
**then** every host still reads that page from the cluster's disk cache, and
none reads it from the object store.

**Status, 2026-10-03.** Holds inside the share.
`TestAPageSurvivesLosingDrainingOrRestartingAnyOneHost` loses, drains or
restarts any one host of six under 4+2, or of two under 1+1, and every host
still reads every page from the cluster.
`TestAVMOpensFromTheClusterAfterAnyOneHostIsLostDrainedOrRestarted` does the
same to a whole VM in a simulated deployment, and
`TestAReaderRebuildsFromAnyIndicesAfterTheRanksShift` adds a host.
