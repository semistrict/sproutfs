---
title: The disk limiter follows a goal
summary: The limiter keeps a minimum percentage or number of bytes free, or a maximum number of bytes used.
---

**Given** a host configured with one of these goals:

- a minimum percentage of the disk left free,
- a minimum number of bytes left free,
- a maximum number of bytes the host may use,

**when** the disk fills, from this host or from anything else on the node,
**then** the limiter keeps the goal, by having the disk cache give back space,
before any part of the host is refused space it needs.
