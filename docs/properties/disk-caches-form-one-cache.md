---
title: The hosts' disk caches form one cache
summary: A page any host's disk cache holds is served to every other host before the object store is asked.
---

**Given** a cluster of hosts, each with a disk cache,
**when** a host needs a page that is not in its own memory or on its own disk,
**then** it reads the page from another host's disk cache if any host's cache
can serve it, and from the object store only if none can. Together the hosts'
caches behave as one cache whose size is the sum of their disks.
