---
title: One list of caches
summary: The list of caches has one source of truth in the object store, and no two hosts act on different lists without finding out.
---

**Given** a cluster whose hosts' disk caches form one cache,
**when** a cache joins or leaves, or the deployment's code changes,
**then** the change is one conditional write to one object in the object
store, which raises the list's generation. Every request between hosts about
the cache names the generation its sender holds, and a host that finds the
other side's generation newer than its own reads the object before it acts.
So no stripe is placed, served or repaired under a list the other side does
not also hold.

The orchestrator and each host keep a copy of the list, never an authority of
their own.

**Status, 2026-10-03.** Does not hold. The orchestrator builds the list from
its survey and serves it at `GET /caches`; each host reads it every 10 s, and
two hosts may hold different lists until then. Decided on 2026-10-03 to move
the list into the object store with a generation.
