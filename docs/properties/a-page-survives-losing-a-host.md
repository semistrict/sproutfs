---
title: A cached page survives losing a host
summary: Losing, draining or restarting any one host costs the cluster's disk cache no page.
---

**Given** a page in the cluster's disk cache,
**when** any one host is lost, drained, restarted or added,
**then** every host still reads that page from the cluster's disk cache, and
none reads it from the object store.
