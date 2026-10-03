---
id: TASK-83
title: >-
  Make membership one object in the object store, changed only by
  compare-and-set
status: To Do
assignee: []
created_date: '2026-10-03 17:54'
updated_date: '2026-10-03 17:54'
labels:
  - cluster
  - correctness
dependencies: []
references:
  - docs/properties/one-membership.md
  - plans/disk-cache-2026-10-02.md
priority: high
type: feature
ordinal: 90000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Which hosts are in the cluster, with their identity, peer-server address, weight and state (joining, active, draining), and the deployment's code, becomes one object in the object store: the membership. It is not specific to the disk cache; anything that routes between hosts reads it, and the cache's stripe placement is the first user. Today (TASK-81 step 4) the orchestrator assembles a 'list of caches' from its survey of pods and serves it at GET /caches; each host reads it every 10 s, so two hosts may route by different lists for that long and the list has no source of truth. Decided 2026-10-03 by the owner: correctness rests only on the object store's compare-and-set. A change reads the object, changes it, and writes it back conditional on the generation it read, raising the generation by one; a lost write retries from a fresh read. Any process may change it; usually the orchestrator does, as a controller moving the membership one step at a time (drain before leave; one join, leave or weight change per step) so the data each change moves is bounded, but nothing depends on a single writer. Every routing-dependent request between hosts (stripe read, keep, drop, fill right) names the generation its sender holds; a holder behind reads the object first, a holder ahead answers stale with its own generation and the sender re-reads and retries. A host's identity is written in its disk, so a pod replaced on the same node keeps its identity and place. This replaces the 'list of caches', the separate cache identity and GET /caches. See docs/properties/one-membership.md and the plan section 'The membership'.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The membership is one object in the object store holding a generation, the code, and each host's identity, address, weight and state; nothing else is an authority for it, and the orchestrator's and hosts' copies are caches of it
- [ ] #2 It changes only by compare-and-set on the generation; concurrent changers (for example two orchestrators, or a host and the orchestrator) never lose an update and never write an older generation, shown by a simulation test with several writers and lost replies
- [ ] #3 The orchestrator reconciles the membership from the pods and host reports one step at a time: a host drains before it leaves, and a join, leave or weight change is one generation
- [ ] #4 A host's identity is written in its disk; a pod replaced on the same node keeps it, and a pod on another node takes that node's
- [ ] #5 Every stripe read, keep, drop and fill right names the sender's generation; a holder behind reads the object before answering, a holder ahead answers stale and the sender re-reads and retries; no stripe is placed, served or repaired under a membership the two sides do not both hold
- [ ] #6 The step-4 list of caches, the separate cache identity in /status and GET /caches are removed or folded into the membership; docs/context.md defines membership and generation, and docs/hosting.md, docs/volumes.md and the talk use the new terms
- [ ] #7 spec/diskcache models the membership as one CAS-updated object with generations and checks that no stripe is placed or served under different memberships, with a mutant that drops the generation check; every TLC run ends within a couple of minutes
- [ ] #8 Tests follow repo practice: synctest over platform/sim, Buggify sites with probes a campaign asserts (lost CAS replies, stale holders, store outages), sim.Bug guards in scripts/mutation/guards.json, Gremlins on the new code, and the fingerprint test stays stable under shake
<!-- AC:END -->
