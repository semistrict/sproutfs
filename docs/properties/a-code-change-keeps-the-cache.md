---
title: A change of the code keeps the cache
summary: Changing the cache's erasure code on purpose reads no page the cluster held from the object store, and nothing else changes the code.
---

**Given** pages in the cluster's disk cache under the deployment's code,
**when** the operator changes the code and names the old one as earlier, or
when hosts join, leave or are lost,
**then** every host still reads those pages from the cluster's disk cache, and
none reads them from the object store. The code changes only when the
operator changes it.

**Status, 2026-10-03.** Holds inside the share.
`TestAChangedCodeReadsEveryEarlierWindowWithNoStoreRead` changes six hosts
from 4+2 to 2+1, with and without a host lost, and three from 2+1 to 4+2, and
every host reads every earlier page from the cluster.
`TestFillsAfterACodeChangeAreUnderTheNewCode` shows that a page read under
the earlier code is filled under the new one. `TestTheCodeNeverFollowsTheHosts`
drains six hosts to two and the code stays 4+2. `spec/diskcache` checks a
change of the code in `MCChange` and `MCWiden`.
