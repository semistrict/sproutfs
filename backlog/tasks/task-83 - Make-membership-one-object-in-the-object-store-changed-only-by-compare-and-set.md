---
id: TASK-83
title: >-
  Make membership one object in the object store, changed only by
  compare-and-set
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-03 17:54'
updated_date: '2026-10-04 00:26'
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
- [ ] #9 The membership also holds which network disk (cache shard) each member serves, with a state per disk (attaching, serving, releasing), changed only by compare-and-set on the generation: a disk is released before it is assigned again, a member serves a disk only under the generation that assigns it to that member, and replies name that generation so a member that lost a disk can never serve it again; tests cover two members both believing they hold a disk, lost replies and a stale server
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
1. New package membership: the object (generation, code, writer nonce, members with identity/address/state, disks with identity/volume/weight/member/state/assigned generation), its protobuf format at <prefix>membership, pure transitions (join, attach, activate, drain, release, released, assign, remove, weight, address, code) that each raise the generation by one, a Store that changes it only by read-modify-conditional-write with lost-reply reconciliation by nonce, and a View (a process's copy: current, refresh, catch up to a generation, slow timer) that replaces rank.Follower. Buggify sites for lost replies, failed writes and failed reads; probes; guards membership-write-unconditional and membership-assign-without-release.
2. rank keeps ranking; ranks are over disk identities, a disk's address is its member's while the disk is served. Remove rank/follower.go.
3. peer: every stripe read, keep, drop and presence names the sender's generation; replies name the holder's generation and the disk's assignment generation; a holder behind catches up before answering, a holder ahead or unable to catch up answers STALE with its generation; a disk not served by this member under that generation is NOT_ME. Client returns a StaleError; a reply whose assignment generation differs is refused. Guards membership-serve-stale-generation and membership-serve-stale-assignment.
4. checkpoint: the disk follows the host's view instead of a list func; each read, fill, fill right, keep check and repair places by one membership snapshot and sends its generation; a stale answer from a holder ahead makes the reader catch up and read again (guard membership-ignore-stale-answer); a fill is placed again under the newer membership.
5. host: open the view over the object store, the host's member identity is its cache file's identity; mark its own disk serving once attached; /status reports member, disk and the membership held instead of cache and caches. supervisor and cmd/sproutfs-host lose the orchestrator list reader.
6. orchestrator: replace caches.go and GET /caches with a controller that reconciles the membership from pods and host reports one step per pass (join with its disk, address change, weight change, drain, release, remove disk, remove member, code), on its own timer; tests incl. two controllers at once.
7. simtest world writes the membership itself as a controller; fingerprint test under shake; volume.CheckDeployment accepts the membership object; restorebench builds a membership.
8. spec/membership/Membership.tla: CAS object with generations, hosts' copies, requests naming generations, disk moves; invariants no stripe placed/served under different memberships and one server per disk; mutants without the generation check, without release, and an unconditional write; runs in under two minutes.
9. Tests: multi-writer CAS campaign with lost replies and outages (synctest over platform/sim), protocol tests (holder behind/ahead, store outage, two members believing they hold a disk), campaign probes, guards in scripts/mutation/guards.json and docs/testing.md each shown to fail, Gremlins before/after on membership and the orchestrator controller.
10. Docs: context.md (membership, generation, disk assignment), hosting.md, volumes.md, deploy/README.md, talk, one-membership.md status, testing.md. just check; merge main; commit in steps.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Owner, 2026-10-03: membership must include the assignment of remote disks to members, evolved linearizably by the same compare-and-set. Ranking stays over disk identities. The attach and detach themselves are TASK-86.

Built (2026-10-03, worktree branch): package membership holds the object (protobuf at <prefix>membership: generation, code and earlier codes, writer nonce, members {id, address, joining/active/draining}, disks {id, volume, weight, member, attaching/serving/releasing/released, assigned generation}); Store.Update is the only writer (read, build, write IfMatch/IfNoneMatch, lost reply reconciled by nonce, Step refuses illegal next generations); View is each process's copy (catch up when a request names a newer generation, 30 s timer, never goes back). Ranks are over disks; a disk is routed only while served. peer: every cache request names generation and disk, replies name generation and assignment generation; holder behind catches up, holder on another generation answers STALE, holder not serving answers NOT_ME. checkpoint: reads retry a window under a newer generation; keeps, drops and fill rights resend under it. Host identity = cache file identity; /status reports member and membership. Orchestrator steps the membership with membership.Next every 5 s; GET /caches, rank.Follower and the host's list reader removed. Merged TASK-85: the code and earlier codes are membership fields written in one step before any join. spec/membership with 4 mutants. Guards: membership-write-unconditional, membership-assign-without-release, membership-serve-stale-generation, membership-serve-stale-assignment, membership-ignore-stale-answer, orchestrator-drop-quiet-member.
<!-- SECTION:NOTES:END -->
