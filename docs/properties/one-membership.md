---
title: One membership
summary: Which hosts are in the cluster, with their weights, is one object in the object store, changed only by compare-and-set, and every request that routes by it names the generation it used.
---

**Given** a cluster whose hosts route requests by its membership, such as the
disk cache placing a window's stripes,
**when** a host joins, drains or leaves, or a weight or the code changes,
**then** the change is a compare-and-set of one object in the object store
that raises its generation. Correctness rests on that primitive alone: any
process may make the change, by reading the object, changing it and writing it
back conditional on the generation it read. Usually the orchestrator does, but
nothing depends on there being one writer.

Every request between hosts that depends on the membership names the
generation its sender holds. A host that finds the other side's generation
newer than its own reads the object before it acts. So no two hosts act on
different memberships without finding out.

A host keeps a copy of the membership, never an authority of its own.

**Status, 2026-10-03.** Does not hold. The orchestrator builds a list of
caches from its survey and serves it at `GET /caches`; each host reads it every
10 s, and two hosts may hold different lists until then. Decided on 2026-10-03
to replace it with one membership object in the object store.
