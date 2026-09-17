# Production review, second pass — 2026-09-14

Four read-only reviews of `main` at 4722322, after the blocker fixes and the
layout refactor: storage and durability; memory, VMM and migration; control
plane and operations; structure and code quality. Ranked together. Items
marked *proven* have a reproducing scratch test. The first pass is
[production-review-2026-09-14.md](production-review-2026-09-14.md).

## Status (2026-09-14, later)

Every finding below is fixed on `main`, each behavioural one with a test that
failed first, except: blocker 5 of the first pass (the collector), which the
owner deliberately leaves open; the `TestScheduledHostReproduces` flake,
whose root cause is a cancelled post-copy fetch racing the simulator's
ordering and is recorded in docs/open-work.md rather than papered over; and
the reclamation-guard item, which turned out to be a no-op rather than a
leak (the guard was removed as cleanup). The control record is format 2
(pins with holders, a child's parent), the index format 5 (origin refs,
compaction grace) and the pack format 1 (versioned trailer and table).

## Blockers

1. **A handoff never re-reads the control record.** `Host.Migrate` and
   `Host.seal` check only local handle state; `Confirm` is called from the
   epoch watcher alone, and a read error there is treated as no evidence. A
   host that has lost the object store but not the pod network keeps running
   and can hand its stale frames to a third host, which then post-copies them
   over the checkpoint the new writer published: one VM's memory made of two
   writers' pages. The store itself never mixes (epoch-major sequences plus
   the compare-and-set); the frame path is the hole, and `runner` picks the
   first of two hosts claiming a VM. Fix: `Confirm` at the top of `Migrate`
   and `seal`; `runner` refuses when two hosts claim one VM.
2. **A fork's pin is kept forever once the child publishes.** `publishedBy`
   marks the point needed as soon as the child's root index names any parent
   pack, which every real fork's does, so `Retire` never unpins; deleting the
   child never touches the parent's pins. The parent's footprint grows
   monotonically and at 4096 pins it can never be forked again. Fix: a
   refcounted pin owned by the child's record, released when the child is
   deleted or its index stops naming the parent's packs; the collector
   reconciles pins against live descendants.
3. **Fork of a fork dies at restore, silently.** Root cause: the host's
   logical-page cap (`LogicalPages`, 8 × arena) refuses the sixth workload
   VM's PMEM region after admitting its RAM; the pager closes the socket
   before ATTACH and logs nothing; Firecracker reports an orderly close as
   "expected exactly one backing descriptor"; `vmmachine.Start` returns the
   HTTP error before reading the connect errors. Fix: distinguish EOF in
   `wire.rs`, drain and log connect errors on a failed load, report the cap in
   status and refuse the create or fork in the control plane; size the cap.
4. **A post-copy read fault dies on a full dirty budget.** An unpublished
   page loaded from the source takes its reservation through the non-waiting
   `tryTakeSpill`; on a full budget the fault fails the session and the
   received guest is killed. The host-wide wait added for stores does not
   cover the migration destination's read path. Fix: reserve through the
   waiting path before the plan holds locks, or retry the fault.

## Serious

Storage:
- A lost control write whose read-back also fails fences the handle against
  its own work *(proven)*; adopt the observed record when epoch and nonce are
  ours.
- Compaction still changes lineage identity *(proven)*, and the comments now
  assert the opposite; `Protect` covers pinned sequences, not their packs.
- `Rebuild` truncates at a part gap and resurrects zeroed pages *(proven)*,
  and has no caller.
- A state-less checkpoint clears the parent's VMM state *(proven)*; a dead
  assignment shows the intent to inherit.
- `publish.go` skips the whole reclamation sweep when the replaced checkpoint
  is pinned: a permanent leak on every same-host fork. Delete the guard.
- Reclamation deletes compaction-emptied packs under readers that still hold
  the replaced view: an I/O error on a healthy VM.
- Publication memory is bounded per checkpoint, not per host.
- `Create` maps a precondition failure to `ErrExists` without the nonce
  read-back; an SDK retry of a landed PUT leaves a fork's child record
  unopenable.
- Unchanged from the first pass: sealed frames held across the serial
  reclamation sweep; retry conflict-compare downloads whole parts; the four
  zstd workers and sixteen cache loads host-wide with production passing zero
  configs; VM id reuse after delete.

Memory and migration:
- The vCPU pause waits for every in-flight fault's backing I/O, including a
  BUSY-retry loop against a migration source with no bound.
- Seal blocks on frame locks held across eviction I/O (unchanged).
- The 30 s seal timeout on both sides, the VMM's timer starting first, kills
  the guest instead of failing the checkpoint (unchanged).
- One sealed region disables pressure-driven checkpoints host-wide: `relief`
  returns at the first draining region, and a fork hold counts.
- A migration failing after the first region's handoff restarts the
  checkpoint loop on a half-released VM; nothing inspects `ErrStopped`
  (unchanged).
- The retire holds the region exclusively for the whole dirty set.

Control plane:
- In-flight table rows are never aged; an orchestrator restart mid-migration
  pins the source's frames forever (`migrated` has no deadline).
- A failed post-copy leaves the guest running on a torn image and the next
  checkpoint publishes it.
- The drain is serial and unbounded, the orchestrator client has no timeout,
  and the 120 s grace period is shared with the 30 s shutdown; the bounded
  `Host.Drain` is unused in production.
- `Recreate` with two replicas and a preStop drain can never succeed: every
  rollout is a host loss.
- Deleting a fork parent skips retiring its fork points; remote children read
  stale bytes.
- No authentication; no NetworkPolicy shipped; `GET /drain` drains on a GET.
- Serving release rests on the non-authoritative table; a swallowed `note`
  failure plus a survey releases frames mid-fetch. The source should refuse
  release while unpublished pages are outstanding.
- Guest agent buffers unbounded output and kills only the shell.
- Placement counts VMs; nothing admits a create against memory.

Structure:
- `internal/objectref` is a dead package; the S3 adapter and three AWS
  modules exist for one benchmark line; the orchestrator's `hosts` table is
  write-only; two test harnesses duplicate the admitting backing wrapper;
  `host/handoff.go` is dead on non-Linux builds.
- The Rust crate cannot be tested or linted off Linux: kvm dev-dependencies
  are unconditional.

## Minor

Templates listed as VMs; epoch watch is one serial GET per VM per 2 s;
`GET /vms` rewrites the table; a partial cross-host fork orphans its earlier
children; the Dockerfile builds the dev Firecracker profile; jitter only
shortens and panics under 4 ns; no version, metrics or liveness; template
import does not gate readiness; the orchestrator rolls over a hostPath SQLite
file; `putFree` panics the host; `h.signal` per page unlock; no VMA budget,
`ConcurrentIO` 16, read-ahead and write-ahead off in production; a dead
`defaultWriteAheadBytes` with a false comment; `pages()` serves with no
per-request deadline; the cancelled-context dial; `ioctlRetrying` spins; no
pack format version; the unreachable compaction state branch; the protected
memo never evicts; cache reclaim drops one entry; `Cache.Close` double
unregisters; `contentRangeSize` rejects `bytes a-b/*`; the GCS writer chunk
size; `maximumPartSize` at 322 MiB; dead code (`OperationAdmitter`,
`WithOperation` in the ports package, `source.size`, `checkpointPages`,
`LatencyBucketOf`, `Memfd`, `Manager.handle`, `benchCheckpointBytes`,
`wire.ComputeChecksum`); four host tests proving negatives by sleep; a
202-line `Start`; stale line references in `docs/open-work.md`; a
`vmcapture.Seal` mention in `vmmigrate.go`; "replica" wording in `host.go`
and a Linux test.

## Confirmed fixed since the first pass

Sequence burning; the `control/` prefix; pins released for a point no child
inherited; `Confirm`; set-difference reclamation with transitive protection;
`Done` returning after the unpublished set; BUSY retried and never
substituted; the host-wide dirty wait, high-water request and deliberate
stop; the VMM exit watcher; request body limits; pre-stream `Receive`
failure; persistent SQLite in WAL mode; readiness probes; survey deadlines and
cache. The layout matches plans/layout-2026-09-14.md on every boundary,
verified on both operating systems.
