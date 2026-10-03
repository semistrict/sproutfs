---
title: The disk limiter follows its goals
summary: The limiter keeps a minimum percentage or number of bytes free and a maximum number of bytes used, in any combination, the strictest winning.
---

**Given** a host configured with any combination of these goals:

- a minimum percentage of the disk left free,
- a minimum number of bytes left free,
- a maximum number of bytes the host may use,

**when** the disk fills, from this host or from anything else on the node,
**then** the limiter keeps the strictest of them, by having the disk cache give
back space gradually as the disk nears the goal, before any part of the host
is refused space it needs.

**Status, 2026-10-03.** Holds. `TestTheShareFollowsTheDiskFilledFromOutside` and
`TestTheDiskLimiterStaysSafeUnderFaults` fill a simulated filesystem from
outside under each combination of goals; the strictest binds, and the cache
gives regions back across the band.
