---
title: Scaling compute moves no window
summary: Hosts the autoscaler adds and removes change no window's holders, because the cache is on shards that move with their stripes.
---

**Given** a deployment whose cluster cache is on shards, network disks the
membership assigns to its hosts,
**when** the autoscaler adds or removes hosts, one at a time or several in a
day,
**then** no window changes the disks it is ranked on, every shard serves again
within seconds on a host that remains, and a page the cache held before is
read from it after, never from the object store. No shard is ever attached to
or served by two hosts.

**Status, 2026-10-04.** Holds in the simulation and in its models.
`TestHostsScaleUpAndDownWithNoStoreReadForACachedWindow` scales six hosts down
to three and back: the membership ranks windows over the same six shards
throughout, and after each change a VM opened on a host reads every page from
the shards. `TestReadsDuringAShardsMoveHedgeAroundIt` reads while a shard is
moving, or after its host is lost, with no read of the store.
`TestShardsSurviveTheirFaultsAndReachTheirProbes` moves shards under the
cloud's and the hosts' faults. `spec/shards` checks `OneServer` with stale
controllers and hosts, and `spec/diskcache` checks `MovesKeepStripes`.
On GCE the moves are timed in [the shards
measurement](../measurements/gce-shards-2026-10-04.md). Not yet run on a GKE
cluster under a real autoscaler.
