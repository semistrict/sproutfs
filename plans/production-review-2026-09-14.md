# Production review — 2026-09-14

Three read-only reviews of `main` at 89274a0 (storage and durability; memory,
VMM and migration; control plane and operations), ranked together. Items
marked *proven* have a reproducing scratch test. Line numbers are 89274a0's,
before the checkpoint/page rename.

## Status (2026-09-14, later the same day)

Blockers 1, 2, 3, 4, 6, 7, 8, 9 and 10 are fixed on `main`, each with a
test that failed before the fix: sequences are never reused; the dirty
budget waits host-wide, asks for an immediate checkpoint at three quarters
and stops a VM deliberately only when no checkpoint can help; an
unpublished page is never read from the volume and `Done` counts only what
the source served; pins are released when no child inherited the checkpoint
durably; fork holds expire after four intervals and a survey releases stale
handovers; `Recover` needs positive evidence and hosts re-read their epoch
every two seconds, with a sim invariant that two writers never mix; a dead
VMM is logged with its cause and forgotten; `Status` has a two-second
deadline and surveys are cached for a second; control records live under
`control/`. Blocker 5 (the collector) is deliberately open. A fork's
children also now share the parent's sealed pages, which the fork-by-handoff
change had left unimplemented.

## Blockers

1. **A failed publication wedges a pager-backed VM forever.** *(proven)*
   `internal/volume/checkpoint.go:349` reuses the sequence after a failed
   publication because `vm.applied` counts overlay writes only, which a guest
   never makes. The retry seals a different dirty set under the same sequence,
   hits the already-uploaded part and reports `ErrConflict` on every interval
   from then on. Not `ErrNeedsRecovery`, so nothing fences or closes it: silent,
   unbounded durability loss after one transient store error. Fix: never reuse a
   sequence.
2. **The dirty budget kills the guest instead of stalling it.**
   `internal/vmmemory/memory.go:606` waits only while the region's own
   checkpoint is draining; otherwise `ErrCapacity` fails the fault, the session
   closes and the VMM is SIGKILLed. The budget is host-wide, the wait
   region-local, and the default is the arena's page count for all VMs against a
   60 s interval. This is the VMM death the workload run saw. Fix: host-wide
   wait; a high-water mark that triggers an immediate checkpoint; a deliberate
   stop with a logged reason only as last resort.
3. **Post-copy substitutes stale bytes for unpublished pages and reports
   success.** `internal/vmmigrate/peer.go:325`: a BUSY reply returns an empty
   present set with no error, so the page is filled from the destination's
   volume, which holds the previous checkpoint. `Done` then succeeds and the
   source releases the only copy. BUSY is the normal state under a drain (8 MiB
   in flight per peer is four pages). Same on dial fallback. Fix: an unpublished
   page is never satisfiable from the volume; retry BUSY; `Done` counts only
   pages the source served.
4. **Fork pins are never released.** `internal/control/client.go:271`: no unpin
   exists. Every fork keeps its parent's pinned checkpoints forever, and
   `MaximumPins` of 4096 is a lifetime cap after which a parent cannot be
   forked. Fix: release on the child's root publication or on
   `ForkPoint.Retire`.
5. **No collector, and reclamation is not self-healing.** *(proven)* One skipped
   sweep leaks permanently (`internal/checkpoint/reclaim.go:32` computes the
   dead set from the previous index only); every takeover leaks the first
   publication's predecessors and the fenced writer's in-flight objects;
   `Delete` leaves every object. Fix: sweep by listing the checkpoint prefix
   against the selected index's packs, and build the collector.
6. **Recover fences a live-but-slow host.** `orchestrator.go:545` treats one
   failed `Status` call as absence, bumps the epoch and starts a second
   guest; the first runs on until its next interval checkpoint, up to 60 s,
   or forever if it is sealed. Fix: positive evidence of loss (pod gone or
   explicit force); hosts re-read their epoch on a short timer.
7. **A cross-host fork hold is released only by the orchestrator.**
   `internal/replica/migrate.go:366`: if the orchestrator restarts or the
   destination dies between fork and release, the parent stays sealed forever:
   never checkpointed (`:158` skips it), never fenced, never migratable, and its
   dirty set grows into blocker 2. Fix: deadline the hold; reconcile `Serving`
   against the table.
8. **A killed VMM leaves no diagnostic.** Nothing calls `Process.Wait`
   after boot and nothing logs a session failure; the machine stays
   registered and the only line is a connection-refused checkpoint failure
   up to a minute later. Fix: a watcher on `Wait` that logs the cause and
   console tail and forgets the machine; log in `Connection.fail`.
9. **`survey` has no per-host timeout and runs on every request.** One
   ten-minute client for `Status` and `Receive`; a wedged host blocks
   `list`, `exec`, `console` and the draining host's `Migrate` for ten
   minutes. Fix: seconds for `Status`, cached for a second.
10. **Listing VMs walks every object in the bucket.**
    `discovery.go:93` lists `vm/` with no delimiter, which includes every
    pack part ever written. Fix: control records under their own prefix, or
    a delimiter in `ListRequest`.

## Serious

- **Compaction changes page identity** *(proven)*:
  `internal/checkpoint/index.go:100` keys identity by the pack a page currently
  lives in, so a compacted page stops sharing memory with forks and is cached
  twice. Carry an origin ref.
- **`Rebuild` truncates silently at a gap in the parts** *(proven)*; parts
  upload concurrently so gaps are the normal interrupted state. No CLI
  reaches `Rebuild` anyway.
- **A state-less checkpoint destroys the VMM state** *(proven)*:
  `publication.go:134` clears the state location instead of inheriting it;
  latent today, one caller away from turning a shutdown into a cold boot.
- **Sealed pages are held across the reclamation sweep**: `retire` runs
  after serial deletes under the publication lock, so copy-on-write and the
  doubled pages last longer than durability needs.
- **Retry conflict-compare downloads whole parts** (up to 322 MiB each).
- **Two host-wide chokepoints on the fault path**: a global pool of four
  zstd workers shared by publication and faults; sixteen concurrent cache
  misses per host.
- **VM id reuse after `Delete` is poisoned** by the old objects.
- **Pack parts carry no format version.**
- **The seal can block on page locks held across eviction I/O and
  population windows**, so the pause is unbounded under memory pressure.
- **A 30 s seal timeout on both sides with no margin kills the VM.**
- **`Received.Done` waits for the whole resident stream**, serially, holding
  the source's pages and the parent's seal for the full transfer.
- **The VMA budget is never set in production**; a fragmented arena around
  128 GiB hits `max_map_count` and the VMM exits.
- **`ConcurrentIO` of 16 host-wide** caps cold faults at 32 MiB in flight.
- **A migration failing after the first region's handoff leaves a zombie**
  with the checkpoint loop restarted against handed-off regions.
- **`h.signal()` per page unlock inside the pause.**
- **`GET /drain` is unauthenticated, unguarded and on the API port**; no
  auth anywhere and no NetworkPolicy shipped.
- **A failed `Receive` leaves a registered guest on the destination** while
  the table says stopped and the source serves forever.
- **In-flight table rows are never reconciled.**
- **The drain is serial with no deadline** inside a 120 s grace period that
  also has to cover shutdown; the bounded `Host.Drain` is used only by tests.
- **Placement counts VMs, not bytes.**
- **The guest agent buffers unbounded output**; no exec admission limit.
- **SQLite on one connection with a full table rewrite per request**, and
  the console polls five times a second.

## Minor

Per-fault clear plus double copy; serial per-page compress on one goroutine;
deletes take upload slots; GCS writer chunk size; `ForkPoint.Retire` data
race on `f.err`; a fork whose attach fails leaves an unopenable record;
unconditional `Delete` leaves a live writer retrying on 404; the protected
memo never evicts; serial one-GET-per-page `Read`; cache reclaim drops one
entry per call; cancelled fetches discard paid-for bytes; `putFree` panics
the node; peers dial with a cancelled context; `Verify` is local-only so a
fenced host is detected only at its next checkpoint; no readiness gating on
templates; no version or metrics endpoint; `imagePullPolicy: Never` with
`privileged: true`; tight budgets; no API timeouts or body limits below
64 MiB; jitter only shortens; a failed drain marks a running VM stopped.

## Sound

Write ordering (parts, index, control, then deletes); epoch-major
sequences; lost-reply reconciliation; index validation; the `packs` set
difference; pinned checkpoints spared; bounded publication memory; cache
coalescing and accounting; the blob envelope; the seal mechanism and
copy-on-write out of a sealed page; page lifetime; identity across
takeovers; the x86 gap mapping; seal/population lock ordering (undocumented,
load-bearing); the fork's seccomp filter; snapshot ordering; the
fork/migrate interlock; the fencing primitive; same-host fork lifecycle;
`Migrate` ordering; `validateMachine`; `loadConfig`; the vsock channel; the
metered store; `podReady` excluding terminating pods.

## Documentation drift found by the per-doc pass

Every doc described at least one removed feature as existing (the log,
replication, quorum, membership, deltas, per-page objects, pre-copy, the
fork over a published checkpoint, scaled page sizes, multi-RAM regions). The
recurring factual errors, now fixed: the page cache has its own budget, not
the pager's; the index has no parent or ref; the VMM state is a pack member;
the interval is 60 s jittered with the timer restarting after upload; the
seal copies nothing until a store; spill is not synced; the adapter rejects
balloon, hotplug, vhost-user and async block, not THP; a second x86 hole at
256 GiB; the drain is driven by the orchestrator; drain and page-server
budgets; `DirtyBytes` counts the overlay only; a pending fork is unopenable
anywhere; `Handle` serialises control writes, not the VM; deployment numbers
(6Gi hugepages, 8Gi memory, 12 GiB pool, 5 GiB arena). Stale scripts still
named the removed log scenario.
