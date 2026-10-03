---
title: A slow host does not slow reads
summary: One slow or unresponsive host does not raise the latency of reads from the cluster's disk cache.
---

**Given** one host whose disk cache answers slowly or not at all,
**when** other hosts read pages it holds part of,
**then** they read at the speed of the other hosts, and do not wait for it or
fall back to the object store because of it.

**Status, 2026-10-03.** Holds. `TestAStalledOrSlowHolderSlowsAReadByTheHedgeDelayAtMost` reads exactly
as fast with one stalled holder among those asked first, and exactly the hedge
delay slower where a stalled and a slow holder were both among them. Second
requests stay within their budget (`TestSecondRequestsStayWithinTheirBudget`),
and three timeouts mark a host down until a probe answers
(`TestThreeTimeoutsMarkAHostDownAndOnlyAProbeClearsIt`).
