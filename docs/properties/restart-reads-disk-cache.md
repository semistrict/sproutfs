---
title: Then from the cluster's disk cache
summary: A page evicted from memory comes from the hosts' disk caches, this host's among them, before the object store.
---

**Given** a VM stopped hours ago whose page was evicted from memory but is
still in the cluster's disk cache,
**when** the VM starts again, on this host or another,
**then** that page comes from the hosts' disks, this host's and its peers'
together, with no object-store read.

This replaced "local disk first, then another host's disk" on 2026-10-02: a
whole local copy saves about half a millisecond a read, and holding one on
every host that reads a page costs the cluster most of its capacity.

**Status, 2026-10-03.** Holds for the windows inside the share the cluster
cache is on for (`SPROUTFS_CACHE_CLUSTER_PERCENT`, which the manifest still sets to 0).
`TestAVMOpensFromTheClusterAfterAnyOneHostIsLostDrainedOrRestarted` opens a
suspended VM on another host of six under 4+2, or of two under 1+1, which
reads no part of the store but the VMM state, and
`TestAPageInTheClusterIsReadWithNoStoreRead` reads every page from the
cluster on every host.
