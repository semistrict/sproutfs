# Open work

What is genuinely open, verified against the tree. The designs behind these and
what is already finished are in [plans/README.md](../plans/README.md). A search for
`TODO`, `FIXME`, `XXX` and `HACK` across the Go and Rust sources finds nothing;
everything open is listed here.

## Correctness and unbounded growth

- **Fixed, and worth keeping: a post-copy child was told its own published pages had no object.** It killed a child's guest a second or so after it resumed, usually in the kernel's timer wheel — `__run_timers` on a node whose `pprev` was `dead000000000122`, the poison `hlist_del` leaves — and otherwise in `rb_erase`, `profile_tick`, `process_one_work`, a wild jump, or a silent hang. It was one defect: a guest reading an older version of a page it had written.

  A destination reports no identity for the pages its handoff named, so the pager loads them through the peer backing — where the source answers — rather than resolving them against a checkpoint that does not hold them. That set was fixed for the backing's life, so it went on saying so after the child's own checkpoint had published those very pages. The pager believed it: a retire gives up a page the volume holds no object for, because a publication writes an all-zero page as a sparse hole and the volume reproduces such a page without one. Here the answer was wrong, so the retire revoked the guest's mapping and released the only copy of bytes the guest had written, and its next read of that page came back from the fork point. The rate scaled with the page count — the fan-out fixture's handoff set is about 8300 pages at 4 KiB against about 16 at 2 MiB — which is why it arrived with the page-geometry plan's fourth step rather than being caused by it.

  `PeerBacking.Locate` now strips a page of that set for exactly as long as the checkpoint the volume names for it predates the handoff: another VM's, or this VM's own up to the sequence the handoff selected. Both halves are load-bearing and each has a test that is red without it, because a migration keeps the same VM and its unpublished pages are written past its own last checkpoint.

  Three things are left behind deliberately. `vmmemory.ErrUndroppable` refuses the retire rather than trusting the answer: a page given up because the volume holds no object for it must be one the volume can reproduce without one, so it must be zeros, and anything else fails the retire with the checkpoint still durable, the page still sealed and the guest still holding its memory. `volume.ErrRetired` refuses a hold on a fork point its last holder retired, which caught a second defect on the way in — forking two children through the manager one after the other, each child's hold going as it closes, took the second from a point whose seal had ended. And `TestFirecrackerForkChildrenSurviveTheirFirstSeconds` reproduces the whole shape in about a hundred seconds a run rather than ten minutes, with the arms that isolated it (`SPROUTFS_FORK_ARM`: the whole checkpoint, the capture without the settle, the bare pause, one child, no interval) — the ladder that found it was 0/8 with no interval, 0/8 for a bare pause, 0/8 for a capture and seal, 5–8/8 for the whole checkpoint, and 0/16 fixed.

- **The probe build's `TestSealTakingAReclaimingPagesReservationKeepsItsBytes` panics under load, and it predates the mapping work.** The pager's audit reports `probe bind: page N of region … was given slot -1 from outside its own store path while it owned generation G, and now takes slot S — a lost write`, from the store's own `takePrivate`. It needs contention to appear: `go test -tags sproutfsprobe ./internal/vmmemory` alone is green over a dozen runs, and ten copies of that binary at once on an eight-core machine reach it in about one lane in eight. Measured on 2026-09-22 with the same binary built from each side: **8 of 50 lanes on `5d7029e`, 6 of 50 after the mapping rules**, every one of them that test and that assertion, so it is the same defect at the same rate and not this work's. What it is saying is that the region the test drives — a seal taking the reservation of a page a reclaim is holding, with a store racing both — hands the store a page while the probe believes the binding still owns a newer generation. Either the audit's generation bookkeeping does not cover this three-way race or the store really does take a page the reclaim has moved; only the second is a defect, and nothing has told them apart yet.

- **No simulation reaches the post-copy paths that defect lived in.** Two separate things were wrong there and neither simulated campaign saw either. The pager's own test double stripped the identity for ever exactly as the product did, so the retire's mistake was modelled rather than caught; and with an earlier fix to `readIn` disabled, a hundred soak seeds still passed, because in every campaign the source's unpublished set only ever shrinks. What is missing is a scenario whose source keeps storing and checkpointing while a destination post-copies from it, and whose destination then publishes and retires what it received. Until there is one, this class is only reachable on a real kernel.

- **Nothing releases a pin: the collector is deferred indefinitely, and until it exists the store grows without bound.** The owner's decision on 2026-09-15 was to keep pins correct and permanent and to write no collector, background or otherwise, for now; accumulating data is accepted. What accumulates is every object a pin covers, and it is never given back. A pin says a checkpoint of a VM was forked, and it is permanent — no participant can tell that nothing reads through it any more, because a descendant sees neither its siblings nor the forks taken below it, and a grandchild's root names its grandparent's checkpoints directly. So the objects a pin covers accumulate: every checkpoint any VM was ever forked at, with every checkpoint its root names, and everything a deleted VM leaves pinned (`internal/control/record.go`, `internal/volume/fork.go`, `internal/volume/manager.go`, `internal/checkpoint/reclaim.go`). A collector is the only thing that can give one back, and it must handle:
  - **Pins nothing reads through any more**, which is the common case: every child forked from that point has been deleted, or every one of them has published a root naming none of the checkpoints that one protects. Establishing it means reading every live record's selected root — including the roots of VMs on other hosts — so the answer holds only against a survey that also accounts for what is in flight.
  - **Pins nothing ever read through**: a fork that failed after the pin, a fork point retired with no child taken from it, a child abandoned before it published its root, and a host lost between the pin and the child's record.
  - **The objects of deleted VMs**: a delete removes the record and sweeps what no pin covers, so what is left under `vm/<id>/ckpt/` is exactly the pinned checkpoints of a VM that no longer has a record. Nothing names them, so the collector has to reach them from the roots of the VMs that still read them, or by listing the deployment's objects against its live set.
  - **Checkpoints no handle will ever reclaim**: the one a handle opened on, which it cannot account for, and the objects of a writer that died mid-checkpoint or published after being fenced.
  - **A delete interrupted between the record's removal and its sweep**, and a VM whose record cannot be parsed, which refuses to be deleted at all until the record is repaired.
  - **In-flight publications and forks**, which it must not collect: a checkpoint's earlier parts exist before the last one does, and a pin exists before the child that reads it.
  - **`MaximumPins`**, 4096 per record, which is what a VM forked at that many distinct checkpoints reaches with no collector to release any.

- **A migration whose source host is unreachable but still listed waits for it.** A destination asks its source for the pages no checkpoint holds until they arrive, and the orchestrator ends the migration only on positive evidence that the source is gone: a pod the Kubernetes API no longer lists, or one that answers and neither runs the VM nor serves its pages (`cmd/sproutfs-orchestrator/orchestrator.go`). A source host whose process is alive resolves it either way — its own handover deadline of four checkpoint intervals gives the pages up, and its next answer ends the asking — but a host that is unreachable while its pod is still listed produces neither, and the migration stays in flight for as long as the request driving it lives. Guessing is the thing the rule exists to avoid, so what is open is evidence rather than a timeout: the orchestrator has no liveness signal for a pod it cannot reach beyond the Kubernetes API's own.
- **A migration tries the receive once, and a receive that fails costs the guest its writes since its last checkpoint.** The source stops the guest and gives its volumes up before the destination is asked to take the VM in, and it keeps the pages no checkpoint holds until told the destination has them all. When the destination cannot take it in, the orchestrator records the VM stopped and reports the failure; nothing asks the same destination again, or another, while the source still holds those pages (`cmd/sproutfs-orchestrator/orchestrator.go`). What reopens the VM is its last checkpoint, and the source's own four-interval hold deadline retires the pages. A drain is such a migration per VM, so a host that leaves while a destination is briefly unreachable leaves with the guest's unpublished writes. The simulated swizzle campaign shows the retry is sound — the source's handoff is good for as long as it holds — and that the first attempt does fail under a separated link (`internal/simtest/swizzle_test.go`); found on 2026-09-18 by the nightly seed sweep, whose seeds 21 and 227 had been passing only because the world reported a VM as on a host a failed takeover had merely tried.
- **A child forked onto its parent's own host holds the point invisibly to the orchestrator.** Such a child is served no pages, so it is not in the parent host's `Status().Serving`, and the orchestrator's survey of stale handovers cannot see the hold; only the host's own four-interval deadline ends one whose child never publishes its root (`internal/host/fork.go`, `internal/host/migrate.go`). Reporting local holds beside served ones would let the survey release them the same way.
- **The orchestrator's watch of a migration's source is unproven on a cluster.** It is covered by the simulated deployment and by the orchestrator's own tests over fakes; the one soak run that passed killed a host running no VMs, so no GCE run has yet lost a host under a real migration (`cmd/sproutfs-orchestrator/lostsource_test.go`, `internal/simtest/lostmigrationsource_test.go`).

## Measurement

- **RAM runs 4 KiB and PMEM 2 MiB, and what that costs is unmeasured.** The
  store, the pager, the wire and the VMM all carry each region's own page now.
  What the [page-geometry plan](../plans/ram-pmem-page-geometry-2026-09-19.md)
  has left is its steps 5 and 7 — handoff and migration geometry, and the
  qualification — and the realistic workload measurements of retained sharing
  and runtime that the whole change exists to justify. Nothing here has been run
  against the recorded workload at 4 KiB. Step 6's request counts are measured,
  but in the simulation: a cold 2 MiB read-ahead run of 512 RAM pages is two
  object-store requests where it was 513, and what that is worth against the
  169 s a GCE restore of a 16 GiB guest took on 2026-09-21 is unmeasured on a
  real store.
- **Compaction reads the pages it rescues one at a time.** A read of a range of
  a volume now fetches a run of members as one ranged read per extent, but
  compaction walks a segment's pages and calls `Store.loadPage` for each one it
  is moving, so rewriting a mostly dead checkpoint of 4 KiB pages costs a
  request per page — up to `compactionBudget`, 64 MiB, which is 16,384 of them
  (`internal/checkpoint/publication.go`, `compact`). The pages it moves are
  consecutive within a segment and their members are adjacent in the part they
  came from, so the same grouping applies; it runs after the guest has resumed
  and off the fault path, which is why it was left.
- **A 4 KiB read-ahead run is one page under pressure.** Read-ahead takes only
  free arena slots and never evicts, so a guest scanning more memory than the
  arena holds takes one fault per page rather than one per run: at 2 MiB that
  was one fault per 2 MiB, and at 4 KiB it is 512 times as many. It is why the
  fan-out suite's read phase went from seconds to minutes and why its bound had
  to be re-scaled: `forkFanOutRead` is six minutes, three times the slowest
  reading of the phase on an idle qualification instance under the probe build
  and a third above the slowest behind the whole suite. Letting read-ahead evict,
  or reserving a run's worth of slots before a scan, is performance work this
  plan did not do (`internal/vmmemory/window.go`, `reserveRuns`).
- **RAM's mappings are kept whole, and what that is worth is unmeasured on a
  cluster.** The [page-geometry plan](../plans/ram-pmem-page-geometry-2026-09-19.md)'s
  step for this is done: the arena's offsets are not its pages and its memfd is
  sized to the first, every 2 MiB-aligned range that holds a private page owns
  an extent and a private page sits at the offset it has within its range, a
  store closes a gap of at most sixteen pages, a range that reaches half its
  pages is filled, and the mapping budget is the backstop with
  `Stats.MappingMerges` to say when it acted. The mapping protocol is at
  version 8, because ATTACH's length is the offset space now rather than the
  capacity. What is open is what it is worth on a real workload: the counts are
  proved in `internal/vmmemory` and in the simulation, and one Lima reading of
  the fork fan-out at 4 KiB says a child's VMM holds 3,299 and 3,288 mappings
  where it held 4,485 and 4,631 before, with 28 private extents, 2,723 pages
  copied by the rules and the backstop never acting — but that is one run of
  each on an instance whose load differed between them, and nothing has been run
  against the recorded workload or on GCE with the rules on and off.

- **The Firecracker fork has to be rebuilt for mapping protocol version 8.** The
  crate is vendored into the VMM by path, so a cached qualification build keeps
  speaking version 7 and every session it opens fails with `invalid
  managed-memory hello` before a guest starts. That is the refusal working, and
  it is also the first thing to check when a Lima or GCE run that used to pass
  stops attaching: rebuild the VMM, do not reuse
  `~/.cache/sproutfs-fanout`.

  Two placements are deliberately left to an ordinary offset, and both are
  recorded where they happen (`internal/vmmemory/placement.go`). A store that
  copies away from the copy a checkpoint froze cannot have its own offset,
  because that offset is holding the bytes the upload is reading; the page stays
  outside its range's run until something releases it, and nothing moves it
  back. And a page a migration destination loads privately from the source
  arrives in a run of its own, like any other load, so a post-copy destination's
  private pages are not placed at all until the guest stores into them.
- **A page a guest only reads is copied, and the copy is given back at the next checkpoint.** A cold read that has to wait for the pager reaches it as a write fault — on x86-64 because KVM's asynchronous page fault worker always asks for the page writable, on aarch64 when the guest first executes a page — and the pager answers a write fault with a private page. What the [unchanged-page rule](../plans/unchanged-pages-2026-09-19.md) recovers is done: the copy remembers the page it was made from, the settle behind each checkpoint's pause compares the two, and a page that did not change is published nowhere and goes straight back to sharing its origin. What remains is the copy itself. Between the fault and the next checkpoint the host holds the page twice, and with 4 KiB RAM pages under a 2 MiB read-ahead run that is one page in 512, while for PMEM at 2 MiB it is a whole page per cold fault until the interval passes. Preventing it needs a host kernel that passes the guest's access through, or KVM userfault once it exists, and neither is ours to start. Fork points are not settled either: a child inherits an unchanged page as an unpublished one, which its own next checkpoint settles.
- **The workload measurement predates the multi-page parts and wants re-taking.** It was measured against one object per dirty page, before `39bfe37`, so its object counts describe a store layout that no longer exists, and only one fork setting (`FORKS_BASE=2 FORKS_PER_REPO=1`) was run; the commands to re-take it on current `main` are in the document (`docs/measurements-2026-09-14-workload.md`).

## Found by the 2026-09-14 GCE validation

- **Ten workload guests on one boot disk time out.** `FORKS_BASE=4 FORKS_PER_REPO=2` had two guests exceed the orchestrator's ten-minute exec ceiling with nothing in the host logs; both host pods spill to the node's one pd-balanced disk. Three-and-two completed.
- **`sproutfsctl hosts` shows RESIDENT, not COMMITTED.** Placement now turns on the guest RAM a host has promised (`Pager.CommittedBytes` on `/status` and `/metrics`), so an operator cannot see from the table why a placement was refused. The column was left alone because `scripts/lib/demo-run.sh` and `demo-workload.sh` parse the table positionally.
- **Two control-plane changes are unproven on a cluster** (templates named by their image's bytes are proven: the soak's redeploys replaced every pod and the bucket kept one template per image): a same-host fan-out that fails part way takes back the children it started; and the termination budget is exactly 90 s of preStop plus 60 s of shutdown in the 150 s grace period, with no slack for the kubelet's own round trips. Both live in `lifecycle_linux.go`, which need Linux, hugetlbfs and Firecracker; `scripts/demo-gce.sh redeploy` is what proves them (across restarts the bucket should hold exactly one template per distinct guest image, whatever the pods are called, and `scripts/demo-gce.sh fixes` runs the rollout end to end).
- **A guest image's RAM is fixed at the moment it is first imported.** `SPROUTFS_VM_MEMORY_BYTES`, and a template's own `name=path:bytes`, are read when a host imports the image into a template — which now happens once for the whole deployment, because the template is named by the image's bytes. Raising either and rolling therefore gives no new memory to a VM created from an image the deployment already holds: the create forks the template, and a fork inherits what it forked. A cold start with `--memory` changes one VM's shape, which is what `scripts/lib/demo-bigguest.sh` uses, but there is no way to say "every VM of this image is larger from now on" short of changing the image. Either a create should publish its root at the shape the host's configuration gives that image, or the configured memory should be part of what names the template.
