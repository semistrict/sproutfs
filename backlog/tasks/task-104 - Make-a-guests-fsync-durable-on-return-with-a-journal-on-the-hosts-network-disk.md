---
id: TASK-104
title: >-
  Make a guest's fsync durable on return with a journal on the host's network
  disk
status: To Do
assignee: []
created_date: '2026-10-07 00:26'
labels:
  - durability
  - disk-cache
dependencies: []
priority: high
ordinal: 124000
---

## Description

<!-- SECTION:DESCRIPTION:BEGIN -->
Today a guest's flush (fsync on its virtio-pmem disk) returns at once unless the VM holds an unpublished disk write older than SPROUTFS_FLUSH_BOUND (120 s by default), so fsynced data is only durable within the bound plus one checkpoint interval (docs/architecture.md, flush section). A database that fsyncs its log loses committed writes if the host dies in that time. Checkpoint per fsync is not an option: each checkpoint writes the control record by compare-and-set (~50 ms p50 on GCS) and GCS throttles changing one object more than about once a second. Owner decision 2026-10-06: a successful fsync must survive loss of the host, with an fsync latency of a few milliseconds, using a write-ahead journal on the cloud-replicated network disk the host already attaches for the cache (TASK-86). The object store stays the long-term copy; after a host loss the disk moves to another host, which replays the journal before the VM opens.
<!-- SECTION:DESCRIPTION:END -->

## Acceptance Criteria
<!-- AC:BEGIN -->
- [ ] #1 A plan in plans/ covers the journal format, what fsync writes and waits for, how a checkpoint trims the journal, recovery after host loss (disk reattach, replay, fencing against a host that is still alive), and the interaction with migration, forks and the loss window
- [ ] #2 A guest fsync returns only after its data is on the network disk, and survives the host's loss: shown by a simulation test that kills the host right after fsync and opens the VM elsewhere
- [ ] #3 A TLA+ spec checks that no fsynced write is lost and no fenced host's journal is replayed over a newer checkpoint, with TLC runs of a couple of minutes
- [ ] #4 A GCE measurement gives fsync p50/p99 on Hyperdisk Balanced and the throughput cost
<!-- AC:END -->
