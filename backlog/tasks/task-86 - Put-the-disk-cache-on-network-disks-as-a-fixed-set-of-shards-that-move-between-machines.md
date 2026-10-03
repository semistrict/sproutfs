---
id: TASK-86
title: >-
  Put the disk cache on network disks as a fixed set of shards that move between
  machines
status: To Do
assignee: []
created_date: '2026-10-03 23:10'
updated_date: '2026-10-03 23:15'
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
- [ ] #2 Windows are ranked over shard identities; adding or removing compute hosts changes no window's holders, shown by a simulation that scales hosts up and down with no store reads for windows already cached
- [ ] #3 When a shard's machine goes away, Kubernetes reschedules the shard and reattaches its disk; the shard reads its regions back from their tables (step 3) and serves again, and reads during the move hedge around it with no wrong bytes and no store read under 4+2
- [ ] #4 The membership (TASK-83) maps each shard to its current address, updated by compare-and-set when a shard starts somewhere new
- [ ] #5 Hosts read from and fill shards exactly as they read from and fill each other today; the disk limiter accounts for a shard's disk on the shard, not on the host
- [ ] #6 deploy/ runs the shards on GKE and k3s with a StorageClass for Hyperdisk Balanced; docs/hosting.md and deploy/README.md describe sizing (shard count, disk size, provisioned IOPS and throughput)
- [ ] #7 A GCE measurement compares dependent single reads of 2 MiB and 4 KiB pages from shards on Hyperdisk Balanced, pd-balanced and pd-ssd against local NVMe, reports each disk's throughput against the serving budget, and times a shard's move when its node is removed
- [ ] #8 Tests follow repo practice: synctest over platform/sim, Buggify sites with probes a campaign asserts (shard moving, disk reattach slow, shard restarting), sim.Bug guards in scripts/mutation/guards.json, Gremlins on the new code, the fingerprint test stable under shake; spec/diskcache models shards that move with their stripes, every TLC run within a couple of minutes
- [ ] #9 Disk attachment follows the membership's assignments: a release detaches, an assignment attaches to the named member, and no disk is ever attached to or served by two members; the move of a disk off a member being removed is timed on GCE
<!-- AC:END -->

## Implementation Notes

<!-- SECTION:NOTES:BEGIN -->
Owner, 2026-10-03: which member serves which disk is decided by the membership object (TASK-83), by compare-and-set, not by Kubernetes scheduling alone. TASK-86 carries out those assignments: when the membership releases a disk from a member, that member stops serving and the volume is detached; when it assigns the disk to another member, the volume is attached there (Kubernetes or the cloud's attach API) and the member serves under the assigning generation. A controller, usually the orchestrator, moves disks off members the autoscaler is removing, one step at a time.
<!-- SECTION:NOTES:END -->
