# Open work

This page lists the open work, verified against the tree. The designs behind
these items, and the work already finished, are in
[plans/README.md](../plans/README.md). A search for `TODO`, `FIXME`, `XXX` and
`HACK` across the Go and Rust sources finds nothing. Everything open is listed
here.

## Correctness and unbounded growth

- **Fixed, and kept here for reference: a post-copy child was told that its own published pages had no object.** The defect killed a child's guest a second or so after it resumed. Usually the crash was in the kernel's timer wheel: `__run_timers` on a node whose `pprev` was `dead000000000122`, the poison value that `hlist_del` leaves. Otherwise the guest crashed in `rb_erase`, `profile_tick` or `process_one_work`, jumped to a wild address, or hung silently. All of these were one defect: the guest read an older version of a page it had written.

  A destination reports no identity for the pages its handoff named. So the pager loads those pages through the peer backing, where the source answers. It does not resolve them against a checkpoint, because no checkpoint holds them. That set was fixed for the backing's lifetime. So the backing kept reporting no identity for those pages after the child's own checkpoint had published them. The pager trusted the answer. A retire gives up a page when the volume holds no object for it, because a publication writes an all-zero page as a sparse hole, and the volume can reproduce such a page without an object. Here the answer was wrong. The retire revoked the guest's mapping and released the only copy of bytes the guest had written. The guest's next read of that page returned the fork point's version. The failure rate scaled with the page count: the fan-out fixture's handoff set is about 8300 pages at 4 KiB, against about 16 at 2 MiB. That is why the defect appeared with the page-geometry plan's fourth step, although that step did not cause it.

  `PeerBacking.Locate` now removes a page from that set only while the checkpoint the volume names for the page predates the handoff. That covers another VM's checkpoint, and this VM's own checkpoints up to the sequence the handoff selected. Both conditions are required, and each has a test that fails without it. Both are needed because a migration keeps the same VM, and its unpublished pages are written after its own last checkpoint.

  Three safeguards were deliberately left in place:
  - `vmmemory.ErrUndroppable` refuses the retire instead of trusting the answer. A page that is given up because the volume holds no object for it must be a page the volume can reproduce without an object, so it must be zeros. Any other page fails the retire. The checkpoint stays durable, the page stays sealed, and the guest keeps its memory.
  - `volume.ErrRetired` refuses a hold on a fork point that its last holder retired. It caught a second defect when it was added. Forking two children through the manager one after the other, with each child's hold released as the child closed, took the second child from a fork point whose seal had ended.
  - `TestFirecrackerForkChildrenSurviveTheirFirstSeconds` reproduces the whole failure in about a hundred seconds per run instead of ten minutes. It keeps the arms that isolated the defect (`SPROUTFS_FORK_ARM`: the whole checkpoint, the capture without the settle, the bare pause, one child, no interval). The sequence of runs that found the defect was 0/8 with no interval, 0/8 for a bare pause, 0/8 for a capture and seal, 5–8/8 for the whole checkpoint, and 0/16 after the fix.

- **Fixed: the probe build's `TestSealTakingAReclaimingPagesReservationKeepsItsBytes` panicked under load. The cause was a refault that acted on a decision a checkpoint had already superseded.** The pager's audit reported `probe bind: page N of memoryRegion … was given slot -1 from outside its own store path while it owned generation G, and now takes slot S — a lost write`. The report came from the store's own `takePrivate`, in about one lane in eight under contention.

  No write was lost. At every step, the page the guest was bound to held the bytes the guest last stored. With the audit finding made non-fatal, thirty lanes ran to completion, and the test's own `reads %d, want the %d the guest stored` check never fired. The audit had caught something else: the pager granted a binding the right to store into memory after that binding's dirty epoch had already ended.

  A reclaim for a private page releases the memory region while it looks for an arena slot. So a seal and a retire can both run inside a fault that has already decided what the page it serves is. The store path re-checks its decision across its own reclaim: `fault` compares the checkpoint's copy before and after. The spill refault in `loadOnce` did not re-check. A checkpoint taken in that window retires the page: the volume holds its bytes, the reservation that spilled them is returned, and the binding is clean. The refault then bound a private page into the binding anyway. That page has neither a reservation nor a checkpoint. `evictBatch` punches out a page in that state without writing it anywhere. Nothing names the page, so nothing that inherits the identity the checkpoint gave it can map it. Every other memory region of that volume reads its own copy of bytes this host already holds. The audit's generation bookkeeping is correct. The binding that owed the audit a newer generation was one the pager should never have granted.

  `loadOnce` now reads the page's dirty state and the checkpoint's copy of the page together, before and after the reclaim. If either changed, it decides again from the start what the page is (`internal/vmmemory/fault.go`, `bindings.go`, `privateEpoch`). `TestARefaultWhoseCheckpointRetiresWhileItReclaimsGivesThePageToTheVolume` drives the interleaving through a reclaim seam. Without the fix it fails on every run in both builds. The ordinary build fails with the second memory region reading its own copy. The probe build fails with the same panic and the same stack. Measured on 2026-09-22 on a fifteen-core machine, with 50 lanes each and a detector on the grant: **10 of 50 lanes before, 0 of 50 after**. At that rate, the chance of a clean result by luck is about 1 in 70,000. The panic that the lanes produce is rarer than the grant that causes it: about 1 lane in 50 on this machine, against 1 in 8 on the eight-core machine the earlier counts came from. After the fix the panic count is 0 of 150 lanes, but the grant's count is what supports the result.

- **No simulation reaches the post-copy paths where that defect was.** Two separate things were wrong there, and neither simulated campaign caught either of them. First, the pager's test double stripped the identity permanently, as the product did, so the tests modelled the retire's mistake instead of catching it. Second, with an earlier fix to `readIn` disabled, a hundred soak seeds still passed, because in every campaign the source's unpublished set only shrinks. The missing piece is a scenario in which the source keeps storing and checkpointing while a destination post-copies from it, and the destination then publishes and retires what it received. Until that scenario exists, this class of defect can only be reached on a real kernel.

- **Nothing releases a pin. The collector is deferred indefinitely, and until it exists the store grows without bound.** On 2026-09-15 the owner decided to keep pins correct and permanent, and not to write a collector, background or otherwise, for now. Accumulating data is accepted. Every object a pin covers accumulates and is never released. A pin records that a checkpoint of a VM was forked, and the pin is permanent. No participant can tell that nothing reads through a pinned checkpoint any more, because a descendant sees neither its siblings nor the forks taken below it, and a grandchild's root names its grandparent's checkpoints directly. So these objects accumulate: every checkpoint at which any VM was ever forked, every checkpoint that checkpoint's root names, and everything a deleted VM leaves pinned (`internal/control/record.go`, `internal/volume/fork.go`, `internal/volume/manager.go`, `internal/checkpoint/reclaim.go`). Only a collector can release a pin, and it must handle:
  - **Pins nothing reads through any more.** This is the common case: every child forked from that point has been deleted, or every child has published a root that names none of the checkpoints the pin protects. Establishing this requires reading every live record's selected root, including the roots of VMs on other hosts. So the answer holds only against a survey that also accounts for what is in flight.
  - **Pins nothing ever read through**: a fork that failed after the pin, a fork point retired with no child taken from it, a child abandoned before it published its root, and a host lost between the pin and the child's record.
  - **The objects of deleted VMs.** A delete removes the record and sweeps what no pin covers. So what is left under `vm/<id>/ckpt/` is exactly the pinned checkpoints of a VM that no longer has a record. Nothing names them. The collector must reach them from the roots of the VMs that still read them, or by listing the deployment's objects against its live set.
  - **Checkpoints no handle will ever reclaim**: the checkpoint a handle opened on, which the handle cannot account for, and the objects of a writer that died mid-checkpoint or published after being fenced.
  - **A delete interrupted between removing the record and sweeping**, and a VM whose record cannot be parsed. Such a VM cannot be deleted until the record is repaired.
  - **In-flight publications and forks**, which the collector must not collect. A checkpoint's earlier parts exist before its last part does, and a pin exists before the child that reads through it.
  - **`MaximumPins`**, 4096 per record. A VM forked at that many distinct checkpoints reaches this limit, because no collector releases any pins.

- **A migration whose source host is unreachable but still listed waits for that host.** A destination asks its source for the pages no checkpoint holds until they arrive. The orchestrator ends the migration only on positive evidence that the source is gone: the Kubernetes API no longer lists the pod, or the pod answers and neither runs the VM nor serves its pages (`cmd/sproutfs-orchestrator/orchestrator.go`). If the source host's process is alive, the migration resolves either way. The source's own handover deadline of four checkpoint intervals gives the pages up, and the source's next answer stops the asking. But a host that is unreachable while its pod is still listed does neither. The migration then stays in flight for as long as the request driving it lives. The rule exists to avoid guessing, so the open item is better evidence, not a timeout. The orchestrator has no liveness signal for a pod it cannot reach, other than the Kubernetes API's own.
- **A migration tries the receive once. If the receive fails, the guest loses its writes since its last checkpoint.** The source stops the guest and gives up its volumes before the destination is asked to take the VM. The source keeps the pages no checkpoint holds until it is told that the destination has all of them. When the destination cannot take the VM, the orchestrator records the VM as stopped and reports the failure. Nothing retries the same destination, or another one, while the source still holds those pages (`cmd/sproutfs-orchestrator/orchestrator.go`). The VM reopens from its last checkpoint, and the source's own four-interval hold deadline retires the pages. A drain performs one such migration per VM. So a host that leaves while a destination is briefly unreachable takes the guest's unpublished writes with it. The simulated swizzle campaign shows that a retry is sound, because the source's handoff stays valid for as long as the source holds it. The campaign also shows that the first attempt does fail under a separated link (`internal/simtest/swizzle_test.go`). The nightly seed sweep found this on 2026-09-18. Its seeds 21 and 227 had been passing only because the world reported a VM as on a host that a failed takeover had only attempted.
- **A child forked onto its parent's own host holds the fork point without the orchestrator seeing the hold.** Such a child is served no pages, so it is not in the parent host's `Status().Serving`. The orchestrator's survey of stale handovers therefore cannot see the hold. Only the host's own four-interval deadline ends a hold whose child never publishes its root (`internal/host/fork.go`, `internal/host/migrate.go`). If hosts reported local holds alongside served ones, the survey could release them the same way.
- **The orchestrator's watch of a migration's source is unproven on a cluster.** The simulated deployment and the orchestrator's own tests over fakes cover it. The one soak run that passed killed a host that ran no VMs. So no GCE run has yet lost a host during a real migration (`cmd/sproutfs-orchestrator/lostsource_test.go`, `internal/simtest/lostmigrationsource_test.go`).

## Measurement

- **The two costs that the 2026-09-22 GCE run measured are fixed in the
  simulation and unmeasured on a host.**
  - A store now replaces the mapping it copied from instead of revoking it
    first. So a copy-on-write, and a store that closes a gap or fills a range,
    is one mapping command with no revocation. In that run, the three-fork
    `cargo test` fan-out spent 1,753 s on 5,450,465 revocations over 5,481,191
    pages, which is about one per page the guests wrote.
  - A seal's pause now consists only of its write-protect commands. The walk
    that moves each page into the checkpoint runs after the pause, while the
    guest is already running. In that run, the capture of 2,204,672 sealed
    pages paused for 2.14 s, of which 0.18 s was commands.

  Exact counts in `internal/vmmemory` and the campaigns prove both changes. No
  run has yet measured what they are worth in seconds on a real host. That
  requires re-running the same fan-out and the same capture (`revocations`,
  `revoked_pages`, `pause_ns`, `seal_ns`, the new `seal_walk_ns`).

  **The revocations that the 2026-09-23 fan-out then recorded did not come from
  stores.** Its 12,428 revocations against 12,826 copy-on-writes looked like one
  per page the forks wrote, but they came from the retire. A published
  checkpoint hands back every page the volume holds no object for. Here those
  were the write-ahead pages the guest never stored into, 15,477 of them, and
  the retire revoked each one separately. The hand-backs of one retire batch are
  now checked and revoked together, with one command per run
  (`MemoryRegion.revokeHandedBack`, `TestAForksFirstCheckpointRevokesItsHolesInRunsNotPages`).
  Two per-page revocations remain, and neither happens in a fork's first
  seconds:
  - a page whose identity another resident already holds, which arrives alone;
  - `Host.dropSharers`, which an abandoned checkpoint and a retired fork point
    use per sharer per page.

- **RAM runs 2 MiB by default again; 4 KiB is measured and optional.** The
  measurements of 2026-09-23 are in docs/measurements. 2 MiB is faster at every
  timing, and it costs about the same memory once forks do real work. 4 KiB
  holds a tenth of the memory, but only for sparse writers. The rest of this
  item is the history of the 4 KiB work. The store, the pager, the wire and the
  VMM all carry each memory region's own page size now. The
  [page-geometry plan](../plans/ram-pmem-page-geometry-2026-09-19.md) still has
  steps 5 and 7 left (handoff and migration geometry, and the qualification).
  It also still lacks the realistic workload measurements of retained sharing
  and runtime that the whole change is meant to justify. Nothing here has been
  run against the recorded workload at 4 KiB. Step 6's request counts are
  measured, but only in the simulation: a cold 2 MiB read-ahead run of 512 RAM
  pages takes two object-store requests where it took 513. What that saves
  against the 169 s that a GCE restore of a 16 GiB guest took on 2026-09-21 is
  unmeasured on a real store. The fan-out on 2026-09-22 showed why a restore's
  numbers did not apply to forks. A restore's windows are whole, and a fork's
  are not: 8,660 loads brought 31,867 pages, 3.7 pages per load, against 16 on
  the same run's cold restore. The cause was that the pager split its window at
  every page it already held. That is fixed, and a unit test counts it (a
  512-page window over three checkpoints with 64 pages resident: 65 loads and
  195 GETs, against one load and three GETs). The GCE re-measurement on
  2026-09-22 took a cold restore from 7.5 s to 4.6 s, and its loads from 1,323
  to 293. The fan-out did not change, because a fork does not fault the way a
  restore does: 20,016 of its 21,130 faults were stores, and a store read only
  its own page. A store now reads its window ahead too, counted the same way.
  The fan-out has not been re-run, so as far as this repository knows, first
  output within 2 s is still unmet.
- **The attach populate's bound and the batched span are unmeasured on a
  cluster.** The warm restore of 2026-09-22 spent 5.1 s of its 5.48 s in VMM
  start. Before the guest ran, it mapped 2,930,747 sibling-resident pages in
  21,698 runs, against a 0.5 s bound. Bounding the resident runs brought it to
  4.77 s and 14,447 runs over 2,166,194 pages on 2026-09-23. Only 123,056 of
  those pages were resident identities. The other two million pages were
  scattered holes, and each paid one command. So holes and the runs a fork
  point names now share one budget, and an attach installs at most 128 runs of
  any kind (`internal/vmmemory/population.go`). A batch's contiguous runs are
  built in one reservation, so 64 scattered runs cost the VMM 136 kernel calls
  instead of 320 (`rust/sproutfs-vm-memory/src/linux.rs`, `Staging`). Counts in
  tests prove both changes: the Go suite, and the crate's own tests, which run
  only on Linux. Neither change has been timed on GCE. On GCE the real cost is
  the `mremap` per run and the REMAP event the pager reads back for it. That
  cost cannot be batched. `mremap` moves a single mapping, and the runs of a
  batch are separate mappings, so combining them would require a wire change.

  After the bound, the walk remained. The walk went window by window over the
  whole memory region, regardless of how much of the budget was left. Every window
  asks the volume for the identity of every page in it. For a 16 GiB guest at a
  4 KiB page, that is four million identities, decoded from the index's
  segments before the guest runs. But a window reached with no budget left can
  install no run. The walk now stops when the budget runs out
  (`TestPopulationStopsWalkingWhenItsRunBudgetIsSpent`). The phases in the next
  item are meant to show whether the walk is what a managed restore's seconds
  were spent on.

- **The breakdown of a managed restore's 1.7–3.3 s is now recorded, but still
  untimed on a cluster.** A fork's restore used to be recorded as one duration.
  So the fan-out's `restore_each_ns` (1.68 s and 3.28 s, against plain
  Firecracker's 0.22 s cold restore) did not show which part was the VMM's own
  start, the snapshot load, or the pager attaching and populating each memory region.
  `vmmachine.StartPhases` records those four phases. `vmmemory.AttachStats`
  records what one session's `Connect` cost, including the populate's
  commands, runs, pages and duration. The fan-out records all of this per fork
  as `restore_phases` (`internal/vmmachine/process_linux.go`,
  `internal/vmmemory/connection_linux.go`,
  `internal/vmmemory/population.go`). Nothing has run with them yet. The
  2026-09-23 record already rules out the mapping commands. 482 of them carried
  12,000 runs over 4,089,383 pages for the whole scenario, at about a fifth of a
  millisecond each. So the round trips total a tenth of a second, not seconds.
  With the populate bounded at 128 runs per memory region, those 12,000 runs are the
  faults' windows, not the populate. The pages are the populate's holes, and
  one command covers any number of holes.
- **Compaction reads the pages it rescues one at a time.** A read of a range of
  a volume now fetches a run of members as one ranged read per extent. But
  compaction walks a segment's pages and calls `Store.loadPage` for each page it
  moves. So rewriting a mostly dead checkpoint of 4 KiB pages costs one request
  per page, up to `compactionBudget`. That budget is 64 MiB, which is 16,384
  requests (`internal/checkpoint/publication.go`, `compact`). The pages it moves
  are consecutive within a segment, and their members are adjacent in the part
  they came from, so the same grouping would apply. It was left as it is
  because compaction runs after the guest has resumed and off the fault path.
- **A 4 KiB read-ahead run is one page under pressure.** Read-ahead takes only
  free arena slots and never evicts. So a guest that scans more memory than the
  arena holds takes one fault per page instead of one per run. At 2 MiB that
  was one fault per 2 MiB. At 4 KiB it is 512 times as many faults. This is why
  the fan-out suite's read phase went from seconds to minutes, and why its
  bound had to be rescaled. `forkFanOutRead` is six minutes. That is three
  times the slowest reading of the phase on an idle qualification instance
  under the probe build, and a third above the slowest reading behind the whole
  suite. Letting read-ahead evict, or reserving a run's worth of slots before a
  scan, is performance work that this plan did not do
  (`internal/vmmemory/window.go`, `reserveRuns`).
- **RAM's mappings are kept whole, and the benefit is unmeasured on a
  cluster.** The step of the
  [page-geometry plan](../plans/ram-pmem-page-geometry-2026-09-19.md) for this
  is done:
  - The arena's offsets are not its pages, and its memfd is sized to the
    offsets.
  - Every 2 MiB-aligned range that holds a private page owns an extent, and a
    private page sits at its own offset within its range.
  - A store closes a gap of at most sixteen pages.
  - A range that reaches half its pages is filled.
  - The mapping budget is the backstop, and `Stats.MappingMerges` reports when
    it acted.

  The mapping protocol went to version 8 for this change, because ATTACH's
  length is now the offset space instead of the capacity. What remains open is
  the benefit on a real workload. The counts are proved in `internal/vmmemory`
  and in the simulation. One Lima reading of the fork fan-out at 4 KiB shows a
  child's VMM holding 3,299 and 3,288 mappings, down from 4,485 and 4,631. It
  also shows 28 private extents, 2,723 pages copied by the rules, and the
  backstop never acting. But that is one run of each, on an instance whose load
  differed between the two runs. Nothing has been run against the recorded
  workload, or on GCE with the rules on and off.

- **The Firecracker fork has to be rebuilt for mapping protocol version 9.**
  The crate is vendored into the VMM by path. So a cached qualification build
  keeps speaking an older version, and every session it opens fails with
  `invalid managed-memory hello` before a guest starts. That failure is the
  version check working as intended. It is also the first thing to check when a
  Lima or GCE run that used to pass stops attaching: rebuild the VMM, and do not
  reuse `~/.cache/sproutfs-fanout`. The VMM's snapshot format is also at
  version 14. So VMM state that an older build captured is refused on restore,
  instead of being read without its PMEM devices' waiting flushes.

  Two placements deliberately use an ordinary offset, and the code records both
  where they happen (`internal/vmmemory/placement.go`):
  - A store that copies away from the copy a checkpoint froze cannot use its own
    offset, because that offset holds the bytes the upload is reading. The page
    stays outside its range's run until something releases it, and nothing
    moves it back.
  - A page that a migration destination loads privately from the source arrives
    in its own run, like any other load. So a post-copy destination's private
    pages are not placed at all until the guest stores into them.
- **A page that a guest only reads is copied, and the copy is released at the next checkpoint.** A cold read that has to wait for the pager arrives as a write fault. On x86-64 this happens because KVM's asynchronous page fault worker always requests the page as writable. On aarch64 it happens when the guest first executes a page. The pager answers a write fault with a private page. The part that the [unchanged-page rule](../plans/unchanged-pages-2026-09-19.md) recovers is done. The copy records the page it was made from, and the settle after each checkpoint's pause compares the two. A page that did not change is published nowhere and goes straight back to sharing its origin. The copy itself remains. Between the fault and the next checkpoint, the host holds the page twice. With 4 KiB RAM pages under a 2 MiB read-ahead run, that is one page in 512. For PMEM at 2 MiB, it is a whole page per cold fault until the interval passes. Preventing the copy requires a host kernel that passes the guest's access through, or KVM userfault once it exists. Neither is this project's to start. Fork points are not settled either. A child inherits an unchanged page as an unpublished page, and the child's own next checkpoint settles it.

  **The copy is one page per fault and no more. The 2026-09-23 fan-out's counts show this, and the suite now asserts it.** 12,826 copy-on-writes over 13,226 faults is one copy per store-served fault. The pages a store copies beyond the one it faulted on come from the two rules. They are counted separately as `Stats.RuleCopies`, which was missing from the record until now. `TestAForksFirstStoresRevokeNothing` checks this at 4 KiB. It attaches a memory region over a sibling's resident pages. It then stores into a page the populate mapped, into a page the guest has never touched, and into a page inside the window a read brought in. Each store makes exactly one page private and copies nothing for the rules. So what remains of a fork's first pass is the number of faults, not what each fault copies: 13,226 faults at a mean of 1.01 ms. The levers for the fault count are a window larger than the free arena slots a fault can reserve, and a populate of the fork point's hot set instead of whatever pages a sibling happens to hold. Neither is done.
- **The workload measurement predates multi-page parts and needs to be re-taken.** It was measured with one object per dirty page, before `39bfe37`. So its object counts describe a store layout that no longer exists. Only one fork setting (`FORKS_BASE=2 FORKS_PER_REPO=1`) was run. The commands to re-take it on current `main` are in the document (`docs/measurements-2026-09-14-workload.md`).

## Found by the 2026-09-14 GCE validation

- **Ten workload guests on one boot disk time out.** With `FORKS_BASE=4 FORKS_PER_REPO=2`, two guests exceeded the orchestrator's ten-minute exec limit, and the host logs showed nothing. Both host pods spill to the node's single pd-balanced disk. Three-and-two completed.
- **`sproutfsctl hosts` shows RESIDENT, not COMMITTED.** Placement now depends on the guest RAM a host has promised (`Pager.CommittedBytes` on `/status` and `/metrics`). So an operator cannot see from the table why a placement was refused. The column was not changed, because `scripts/lib/demo-run.sh` and `demo-workload.sh` parse the table by position.
- **Two control-plane changes are unproven on a cluster.** (Templates named by their image's bytes are proven: the soak's redeploys replaced every pod, and the bucket kept one template per image.) The two unproven changes are:
  - A same-host fan-out that fails part way takes back the children it started.
  - The termination budget is exactly 31 min of preStop plus 60 s of shutdown within the 1920 s grace period, with no slack for the kubelet's own round trips.

  Both are in `lifecycle_linux.go`, which needs Linux, hugetlbfs and Firecracker. `scripts/demo-gce.sh redeploy` proves them. Across restarts, the bucket should hold exactly one template per distinct guest image, regardless of pod names. `scripts/demo-gce.sh fixes` runs the rollout end to end.
- **A guest image's RAM is fixed when the image is first imported.** A host reads `SPROUTFS_VM_MEMORY_BYTES`, and a template's own `name=path:bytes`, when it imports the image into a template. That import now happens once for the whole deployment, because the template is named by the image's bytes. So raising either value and rolling out gives no new memory to a VM created from an image the deployment already holds. The create forks the template, and a fork inherits the template's memory. A cold start with `--memory` changes one VM's shape, and `scripts/lib/demo-bigguest.sh` uses that. But the only way to make every VM of an image larger from now on is to change the image. Either a create should publish its root at the shape the host's configuration gives that image, or the configured memory should be part of the template's name.
