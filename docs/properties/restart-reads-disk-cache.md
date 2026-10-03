---
title: Then from the cluster's disk cache
summary: A page evicted from memory comes from the hosts' disk caches, this host's among them, before the object store.
---

**Given** a VM stopped hours ago whose page was evicted from memory but is
still in the cluster's disk cache,
**when** the VM starts again, on this host or another,
**then** that page comes from the hosts' disks, this host's and its peers'
together, with no object-store read.

This replaced "local disk first, then another host's disk" on 2026-10-02. A
page read whole from the local disk saves about half a millisecond over one
read from the cluster, and holding it whole on every host that reads it costs
the cluster most of its capacity.
