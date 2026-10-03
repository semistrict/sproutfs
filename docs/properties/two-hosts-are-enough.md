---
title: Two hosts are enough
summary: The cluster's disk cache works in a deployment of two hosts, and survives losing either one.
---

**Given** a deployment of two hosts,
**when** their VMs read and publish pages,
**then** the two hosts' disks form one cache, as a larger cluster's do, and
either host alone still serves every page in it while the other is lost,
drained or restarted.
