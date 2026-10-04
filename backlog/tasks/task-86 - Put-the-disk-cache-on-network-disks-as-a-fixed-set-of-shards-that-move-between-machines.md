---
id: TASK-86
title: >-
  Put the disk cache on network disks as a fixed set of shards that move between
  machines
status: In Progress
assignee:
  - '@claude'
created_date: '2026-10-03 23:10'
updated_date: '2026-10-04 04:10'
labels:
  - cluster
  - storage
dependencies:
  - TASK-83
references:
  - plans/disk-cache-2026-10-02.md
  - docs/measurements/gce-dependent-reads-2026-10-03.md
priority: high
type: feature
ordinal: 93000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Decided 2026-10-03 by the owner. Today the cluster's disk cache (TASK-81) lives on each host's local SSD, so a host the autoscaler removes takes its part of the cache with it, and every join or leave changes which hosts hold each window. Instead, the cache becomes a fixed number of shards. Each shard is one network disk (Hyperdisk Balanced on GCP, gp3 on AWS; chosen because every cloud supports it and nothing needs allowlisting) and a small process that runs the existing disk log and serves stripes over the peer server. Shards run as a Kubernetes StatefulSet with one PersistentVolumeClaim each, so when the autoscaler adds or removes machines, Kubernetes reschedules shard pods onto the machines that remain and detaches and reattaches their disks; that takes seconds, which autoscaling can afford, and loses nothing on the disk. Windows are ranked over shards, never over hosts, so the ranking changes only when the shard count is changed on purpose, never when compute scales. A shard moving is briefly down, which 4+2 reads hedge around. The membership (TASK-83) maps each shard's identity to its current peer-server address and is updated by compare-and-set when a shard moves. The shard's identity is in its disk's cache file header, as today. Hosts read stripes from shards and send fills to them; whether a host also keeps local-SSD caching is out of scope.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 The cache is a configured, fixed number of shards, each a StatefulSet pod with its own PersistentVolumeClaim on a network disk (Hyperdisk Balanced on GCP), running the existing disk log and serving stripes over the peer server
- [x] #2 Windows are ranked over shard identities; adding or removing compute hosts changes no window's holders, shown by a simulation that scales hosts up and down with no store reads for windows already cached
- [ ] #3 When a shard's machine goes away, Kubernetes reschedules the shard and reattaches its disk; the shard reads its regions back from their tables (step 3) and serves again, and reads during the move hedge around it with no wrong bytes and no store read under 4+2
- [x] #4 The membership (TASK-83) maps each shard to its current address, updated by compare-and-set when a shard starts somewhere new
- [x] #5 Hosts read from and fill shards exactly as they read from and fill each other today; the disk limiter accounts for a shard's disk on the shard, not on the host
- [ ] #6 deploy/ runs the shards on GKE and k3s with a StorageClass for Hyperdisk Balanced; docs/hosting.md and deploy/README.md describe sizing (shard count, disk size, provisioned IOPS and throughput)
- [x] #7 A GCE measurement compares dependent single reads of 2 MiB and 4 KiB pages from shards on Hyperdisk Balanced, pd-balanced and pd-ssd against local NVMe, reports each disk's throughput against the serving budget, and times a shard's move when its node is removed
- [x] #8 Tests follow repo practice: synctest over platform/sim, Buggify sites with probes a campaign asserts (shard moving, disk reattach slow, shard restarting), sim.Bug guards in scripts/mutation/guards.json, Gremlins on the new code, the fingerprint test stable under shake; spec/diskcache models shards that move with their stripes, every TLC run within a couple of minutes
- [x] #9 Disk attachment follows the membership's assignments: a release detaches, an assignment attaches to the named member, and no disk is ever attached to or served by two members; the move of a disk off a member being removed is timed on GCE
<!-- AC:END -->

## Implementation Plan

<!-- SECTION:PLAN:BEGIN -->
Mechanism: the membership is the authority; the controller (the orchestrator) carries its assignments out through the cloud's attach API, and the host opens the attached block device and serves it.
- Shards are network disks provisioned once (GKE: one PVC per shard from a Hyperdisk Balanced StorageClass, Immediate binding, Retain; k3s: the same or disks made by the demo script). The orchestrator finds them from labelled PVCs and their PVs' volume handles. A shard's identity is derived from its volume name, its weight from its size; windows are ranked over shard identities only.
- Members are host processes. In shard mode a host keeps no cache disk of its own; its member identity is drawn per process, so a restarted process never inherits an assignment.
- Steps, one per controller pass, each a compare-and-set: add a shard (released); assign a released shard to the least-loaded active member (attaching, at generation g); the controller attaches the volume to that member's machine (GCE instances.attachDisk; single-writer, so one machine at a time); the host reads the membership again, opens the device exclusively (O_EXCL), checks the header's lease (an assignment at least as new as its own refuses it), writes its own lease (generation g, member) and syncs, reads its regions back from their tables, and reports the disk held; the controller marks it serving; the host serves it only while its copy says serving at g. Removing a host (pod terminating or gone) drains the member: releasing; the host stops serving, closes the disk and the device, stops reporting it; the controller detaches the volume from every machine the cloud lists, and only once the cloud reports it attached nowhere lets it go (released); then it is assigned again. A rebalance releases one disk at a time from the most loaded member when the spread is above one and nothing moves.
- Crash points: every controller action is derived from the membership and from the cloud's own list of attachments each pass, never from memory, so attached-but-not-recorded and recorded-but-not-attached converge (detach stale attachments; attach again). A host dying mid-serve is a pod gone: drain, detach, let go, assign, attach, read back. A stale member that still has the device: the cloud attaches it to one machine; on one machine O_EXCL admits one opener; the lease in the header refuses a member whose assignment is older than the last one written, and a host re-reads it before every region it opens and every pass; and peers refuse answers naming another assignment.
- Disk limiter: each shard has its own budget, its device's size less its header region; the host's limiter counts only its own disk (spill, staging).
Steps:
1. platform: network disk ports (Volumes: describe, attach, detach; Devices: open exclusive); platform/sim attachable disks with Buggify sites (attach slow, attach fails, detach slow); real GCE compute adapter and Linux block device opener; tests.
2. checkpoint: a cache with an optional own disk and a changing set of shard disks (AddShard/RemoveShard), fills and reads per disk, peer.Cache per disk; disk header lease (fence generation, member, slots in use) so a device is read back without scanning every slot; tests including a stale member refused by the lease.
3. membership: Want with shards and machines; Next steps for shards; Carry: the attach/detach actions the membership calls for; tests; spec/membership extended with attachments, leases and crash points, OneServer and OneOpener invariants, mutants.
4. host: shard agent (open on assignment after a fresh read, close on release, header check each pass), /status reporting, config; tests.
5. orchestrator: shards from PVCs, machines from pods' nodes, terminating pods drain, attachments carried out each pass; tests over sim volumes.
6. internal/simtest: worlds whose hosts serve shards; a simulation that scales hosts up and down with no store reads for cached windows; a campaign with sites and probes (shard moving, attach slow or failing, member dying mid-move, stale member with device present); fingerprint arm stable under shake.
7. sim.Bug guards in scripts/mutation/guards.json and docs/testing.md, each shown failing its test; Gremlins on new code before/after.
8. deploy: GKE and k3s manifests, Hyperdisk Balanced StorageClass, RBAC for PVCs/PVs; docs/hosting.md, deploy/README.md sizing; architecture, context, plan, property updated.
9. GCE: dependent single reads (2 MiB and 4 KiB) from shards on Hyperdisk Balanced, pd-balanced, pd-ssd vs local NVMe; each disk's throughput against the 500 MiB/s serving budget; time to move a shard when its host is removed; docs/measurements/gce-shards-2026-10-04.md. Every VM, disk and object deleted and verified.
<!-- SECTION:PLAN:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Owner, 2026-10-03: which member serves which disk is decided by the membership object (TASK-83), by compare-and-set, not by Kubernetes scheduling alone. TASK-86 carries out those assignments: when the membership releases a disk from a member, that member stops serving and the volume is detached; when it assigns the disk to another member, the volume is attached there (Kubernetes or the cloud's attach API) and the member serves under the assigning generation. A controller, usually the orchestrator, moves disks off members the autoscaler is removing, one step at a time.

Progress 2026-10-04 (@claude). Mechanism chosen: the membership is the authority; the orchestrator carries it out through Compute Engine's attach API (platform.NetworkDisks), and the host opens /dev/disk/by-id/google-<name> with O_EXCL (platform.Devices) and serves it. Kubernetes provisions the disks (StorageClass + one claim per shard, Immediate binding, Retain) and never attaches them: a running pod cannot gain a volume, and pods scheduled by Kubernetes would follow the scheduler, not the membership. Members are hosts with an identity drawn per process; shard identities derive from the volume handle. Fencing: single-writer cloud + detach yanks the device; Let only once closed and detached; O_EXCL per machine; the lease (assignment generation, member, slots opened) at the end of the header region, checked at open, every region open/close and every pass. Built: platform sim and GCE adapters (fb862857), multi-disk cache + lease (df8e037e), membership Next/Carry/ShardControl (97c9d8c6), host shard server (06852770), orchestrator (2d0992ff), simtest scaling + spec/shards + diskcache MovesKeepStripes (77573005), campaign + fingerprint arm (afe86a2c), deploy (4216f1a2), move bench (d9626dbc). Guards: shard-ignore-lease, membership-let-attached-shard, membership-detach-held-shard, host-open-shard-without-reading-again, orchestrator-keep-terminating-host, all killed 3/3 by check-guards.py. TLC: spec/shards MCShards 30s, MCMultiAttach 1s, deep/Nine 1m58s; diskcache MCShards 6s; mutants caught. Gremlins before/after recorded in docs/testing.md. Remaining: GCE measurements and the report; deploy README sizing.

Merged main (d3377d88, guards.json kept both sides); just check exit 0 with check-guards.py killing all five TASK-86 guards. GCE move (scripts/bench-shard-move-gce.sh, 2 x c3-standard-4, Hyperdisk Balanced 256 GB, 32 GiB filled, controller pass 1 s): six moves, three drained and three died, 13.0 to 14.6 s each to serving on the other host; about 10 s is Compute Engine's detach and attach calls; read back 529 regions from tables in 0.30 s, none scanned, all 16,512 entries kept. Hyperdisk Balanced does not attach to N2 (API refuses), so its shards need C3/C4/N4; a 4-vCPU C3 reads 400 MiB/s from it, below the 500 MiB/s budget. Every VM and disk deleted and verified.

Criteria checked on evidence: #2 internal/simtest TestHostsScaleUpAndDownWithNoStoreReadForACachedWindow; #4 membership Next/ShardControl compare-and-set (membership/shards_test.go); #5 checkpoint per-disk fills/reads and per-shard budget (checkpoint/shards_test.go, host/shards_test.go); #7 docs/measurements/gce-shards-2026-10-04.md; #8 campaign TestShardsSurviveTheirFaultsAndReachTheirProbes, five guards killed by check-guards.py, Gremlins in docs/testing.md, fingerprint shards arm stable, spec/shards and spec/diskcache MCShards within 70 s, deep Nine 1m58s; #9 Carry/ShardControl tests, spec/shards OneServer, GCE move timed. Left unchecked for the owner: #1 and #3 name a StatefulSet that Kubernetes reschedules, which the owner's later note replaced with membership-driven attachment (no StatefulSet; the orchestrator attaches through Compute Engine's API; read back, hedging and no store read are shown by TestReadsDuringAShardsMoveHedgeAroundIt); #6 manifests and sizing are written and pass the manifest tests, but were not applied to a real GKE or k3s cluster.
<!-- SECTION:NOTES:END -->
