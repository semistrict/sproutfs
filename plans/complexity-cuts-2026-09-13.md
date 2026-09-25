# Complexity cuts — 2026-09-13

Decisions from a requirements review of the working tree on `bfaca85`, aimed
at removing features and code paths the product does not need. Requirement
answers came from the project owner; the remaining determinations follow from
them. No source changes are included.

## Requirements confirmed

| Requirement | Consequence |
| --- | --- |
| A guest write or fsync must never block on object-store latency. Losing a few seconds of guest state on host loss is acceptable. | No log. The VM is checkpointed whole on an interval; host loss rewinds it to the last uploaded cut. |
| Flush is neither a fence nor a trigger. | virtio-pmem flush does nothing. Cuts run on the interval and on explicit request only. |
| Hosts run as a Kubernetes deployment on a trusted cluster network, plain TCP. | No membership, no host identity, no TLS. The orchestrator carries pod addresses. |
| Migration pause is at most 1 s. | One mechanism whose pause does not depend on the dirty set. |
| A VM is one RAM memory region plus a few PMEM disks. | Single RAM volume; PMEM devices stay per disk and are treated like RAM. |
| Forks of running VMs are required. | The cut under a vCPU pause stays. |
| A process restart is a host loss. | No local state survives a restart. |
| Recovery may give the VM a new identity. | No in-place recovery. |

## The model

A VM's durable state is one checkpoint: an index, the 2 MiB chunks it
references, and VMM state. Every interval, and on request, the host pauses the
vCPUs, saves device state, seals the dirty set of every memory region by write
protection, and resumes. The sealed pages upload straight from the arena as
chunk objects; a store into a sealed page during its upload copies that page.
The index and VMM state follow, and the control record selects the new
checkpoint. RAM and PMEM go through the same path; nothing distinguishes them.

A cut taken under the pause is one moment of the whole machine, so no
persisted state can hold a later write without an earlier one. That is the
ordering guarantee a flush would otherwise provide. A running guest is never
sealed.

Host loss loses the cuts not yet uploaded, a few seconds. Recovery is opening
the VM: read the control record and the selected index, resume from the VMM
state. A fork is a new VM whose root index inherits any cut. Migration stops
the source, saves state, advances the epoch in the control record, and the
destination resumes and pulls pages from the source page server; the
destination's next cut uploads them.

After selecting cut N, the VM deletes the objects of cut N-1 that index N no
longer references, unless a fork pinned that cut. Pins live in the parent's
control record and are written before the fork handle is returned.

## Cuts

### Decided by the owner

- **No log.** Remove the log package, `replica`, the replacement monitor,
  heartbeats, member probes, the replica network, connection pool, replica
  journals, journal compaction, placement, quorum configuration, zones,
  `Host.Logs()`, the record format, the overlay, replay on open, log
  positions, the pending-byte budgets and the write path that produced
  records. Writer fencing stays in the control record's epoch through
  conditional writes.
- **No flush.** Remove the durability worker per PMEM device in the
  Firecracker branch, the pager's flush path and its authority check, the
  flush and write-ahead budgets and the flush-size pairing at attach. The
  virtio-pmem flush command returns success.
- **One cut operation.** Capture, checkpoint publication and pre-copy's
  repeated cuts become the interval cut. Remove the checkpoint worker's byte
  trigger, idle ceiling and jitter, and the host-wide press to publish. Writes
  from outside a pager, which image building needs, use a direct chunk-publish
  call.
- **No membership.** With the log gone, membership's only consumer is the
  migration page server's peer authentication. On a trusted cluster network that
  is the network policy's job. Remove `membership/` entirely: member ID
  allocation by conditional write, keys and certificates, the authenticator,
  enrollment, retirement, roster snapshots and their root, the refresh watcher,
  revocation, the joining/active/draining/removed states, the 4,096-entry roster
  bound and its retirement backlog. Remove TLS from
  `platform/internal/real/network.go` and the identity-binding network
  wrapper. A host needs no admitted identity or local view to start; the handoff
  carries the source pod's page-server address and the destination dials it.
  Draining is `Host.Drain` from the preStop hook, as now, with the deployment
  routing the handoffs. The page server serves any peer that can reach it; the
  cluster's network policy must restrict that port to hosts.
- **Pager page.** Fix at 2 MiB in the pager, arena, Rust adapter and the
  simulation model. Delete the runtime `PageSize` configuration,
  `forEachPageSize` and the page-size tests; tests use few 2 MiB slots.
- **Deltas.** Publish whole compressed 2 MiB chunks only. Remove the delta
  object, base-plus-delta index lookup, 4 KiB publication diffing and the
  partial-identity private-load rule. A sealed pager page is a chunk.
- **Recovery.** Opening a VM reads the control record and the selected index.
  Remove `Manager.Recover`, `ErrQuorumLost` and the group-replacing recovery
  path.
- **Restart is host loss.** Remove disk-namespace reconciliation and restart
  scans, fresh-instance-identity reconciliation of lost conditional writes,
  and scratch/console cleanup under an old VMM's lock. A starting process
  wipes its local directories. The deployment reopens every VM a restarted
  host ran elsewhere.
- **Multi-RAM memory regions.** One RAM volume, `ram0`. Remove the memory region list and
  guest-address ordering for RAM.

### Determined from the requirements

- **Migration is post-copy only.** Stop saves VMM state and hands off the
  memory regions and the control record without uploading anything; the destination
  resumes and pulls pages from the source page server, with the bulk stream
  behind it. Remove pre-copy rounds, their residue threshold and round count.
  Exposure: source death during post-copy loses the cuts since the last
  upload, the same as any host loss.
- **Per-cut reclamation.** The first deletion of checkpoint objects in the
  system, described under the model. Checkpoints shared with forks stay pinned
  and still need a collector later.
- **Disk is capped per concern.** The chunk cache and pager spill each get a
  fixed cap in their own directory or partition. Remove the shared byte
  ledger, cross-concern reclaim ordering, reclaim permits, protected-file
  rewrite headroom and the disk-full retry paths. RAM and HugeTLB accounting
  stays.
- **Console output** is a 1 MiB in-memory ring buffer per process. Remove
  disk retention and its budget interplay.
- **Read-ahead** is one host default. Remove per-PMEM-device overrides.
- **Empty directories** `control/`, `coordinator/` and `tmp/` are deleted.
- **TODO.md** described removed code (`RetireSnapshotNames`, the capture
  catalog) and had to be rewritten after the cuts. *(done: [open-work.md](../docs/open-work.md)
  now lists only what is genuinely open)*

## Done

- **Checkpoint interval.** Settled at 60 s, each wait jittered shorter by up to
  a quarter so VMs do not checkpoint in lockstep
  (`replica.DefaultCheckpointInterval`, `HostConfig.CheckpointInterval`,
  `SPROUTFS_CHECKPOINT_INTERVAL`). One workload was measured before fixing it —
  [what a checkpoint costs under a workload](../docs/measurements-2026-09-14-workload.md):
  the pause is 6–9 ms and does not scale with the dirty set, so nothing about
  the interval needs to defend it; the upload is the cost, and a shorter
  interval multiplies the per-checkpoint fixed traffic without changing the
  dirty bytes. It does not adapt to the dirty rate. That run predates packs and
  measured only one fork setting, and never produced the 4 KiB dirty ratio, so
  the numbers behind this want re-taking.
- **Stopping a fenced VMM.** Writeback used to be where a fenced host found
  out and stopped its guest. The interval checkpoint is where it finds out
  now, within one interval, and the host closes the machine there: it stops
  the VMM process, releases the VM's volumes and reports the VM to its
  supervisor through `HostConfig.MachineClosed`, so no stale guest keeps
  running with writes that can never be published.

## Rejected

Recorded so they are not proposed again.

- **Keep peer replication so fsync-confirmed writes survive host loss.** An
  earlier note in this directory recorded that requirement. The owner
  withdrew it on 2026-09-13: losing a few seconds of guest state on permanent
  host loss is acceptable. Only blocking a write on object-store latency is
  unacceptable.
- **The object store as an append log.** Records staged locally and shipped
  as batches. Superseded the same day by whole-VM cuts: a sealed 2 MiB page
  is already a chunk, so batches, records, replay and the overlay are
  unnecessary.
- **Flush as a cut trigger.** Proposed to shrink the loss window after an
  fsync. Rejected by the owner: flush does nothing.
- **Load migrated memory from storage instead of the source.** Proposed as a
  way to remove the page server while keeping pre-copy and a final cut into
  the log. Uploading the residue inside the pause would spend the 1 s budget,
  so the page server is the mechanism that bounds the pause and pre-copy is
  the one removed.
- **A Kubernetes discovery adapter replacing membership.** Unnecessary once
  peer replication is gone: the only address a host needs is the source
  page-server address in a handoff, which the orchestrator already carries.
  No deployment-target abstraction is needed.

## Open

- **Index per checkpoint.** Still open. Every checkpoint publishes a full
  index: `Index.encode` emits one entry per 2 MiB page of every volume, so the
  index a 1 TiB volume publishes is tens of megabytes whether one page changed
  or all of them. An idle VM publishes nothing, so this only bites at fleet
  scale or on very large volumes. The alternative is an index listing only
  changed pages and naming its parent, at the price of open following a chain,
  or a segmented index read by range.

## Order

1. One cut operation and no log: seal-and-upload from the arena, index and
   control-record selection per cut, open from the selected index, forks over
   cuts, per-cut reclamation with pins. Remove the log package, `replica` and
   everything listed under the no-log and no-flush cuts, including the PMEM
   durability worker in the Firecracker branch. This is most of the removal
   and every later step is simpler after it. *(done)*
2. Remove membership and TLS; the page server dials by address. *(done)*
3. Restart is host loss, per-concern disk caps, console. *(done)*
4. Deltas, then fixed pager page. *(done)*
5. Single RAM memory region, read-ahead overrides. *(done)*
6. Post-copy-only migration. *(done in step 1: no pre-copy code remains)*
7. TODO.md rewrite and documentation updates in `docs/` for every cut above;
   `docs/replication.md` and `docs/membership.md` go, `docs/hosting.md` loses
   its replica and assembly-identity sections, `docs/volumes.md` and
   `docs/vm-memory.md` describe the checkpoint model. *(done; `docs/metadata.md`
   stayed — the control record it describes is the authority that survived the
   cuts)*

Each step runs `go vet ./...` and `go test ./...`; steps touching `vmmemory`,
`vmmachine`, `vmmigrate` or the Firecracker branch also run the Linux
integration suite in the documented Lima environment.
