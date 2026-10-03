---
title: One disk limiter
summary: One limiter bounds everything the host writes to its disk, together.
---

**Given** a host whose disk holds spill files, ephemeral disks, VMM staging
and the disk cache,
**when** any of them grows,
**then** one limiter decides whether the host as a whole may use that space.
No part of the host has a disk cap of its own outside the limiter.
