# Open work

What is genuinely open, verified against the tree. The designs behind these and
what is already finished are in [plans/README.md](../plans/README.md). A search for
`TODO`, `FIXME`, `XXX` and `HACK` across the Go and Rust sources finds nothing;
everything open is listed here.

## Correctness and unbounded growth

- **A fan-out of two children panics a child's guest kernel at a 4 KiB RAM page, and `main` carries it.** The guest dies in `__run_timers`, dereferencing a list node whose `pprev` is `dead000000000122` — `LIST_POISON2`, the value `hlist_del` leaves behind — about twelve seconds into the child's life. It is a guest reading an *older* version of a page it wrote itself, not random bytes: the timer the kernel had removed is still on the list it is walking. The shape is `TestFirecrackerForkFanOutServesBothChildrenAtOnce`: two children of one fork point, received onto one destination pager, reading everything they inherited while each is checkpointed every 250 ms, with an arena a quarter of the memory the two of them map.

  Rates, on the qualification instance with the fixture unchanged:

  | build | runs | panics |
  | --- | --- | --- |
  | before the settle changed | 5 | 1 |
  | before the settle changed, accelerated | 16 | 8 |
  | settle revoking and then installing, accelerated | 8 | 2 |
  | **settle revoking only — what `main` has** | **31** | **1** |

  The settle's in-place re-share of an unchanged page — installing the origin over the page the guest still mapped — was a large part of it, which is why `main` revokes instead. **That change is not a fix and must not be described as one:** the defect survives it at about one run in thirty.

  What has been ruled out, and with what power. Eviction, spill and refault: the panic reproduces with an arena large enough that `evictions=0`. Slot reuse: it reproduces with the settle's private page never released. A shared page being written, and a private page reachable from two regions: the pager's own audit (`-tags sproutfsprobe`) checked both at every release of a page's lock across sixteen runs that panicked eight times, and neither ever fired. Arms that saw no panic in ten runs — interval checkpoints off, the settle disabled — prove **nothing** and are not ruled out: at a three-percent rate ten clean runs happen three times in four by chance, which is the mistake that produced the retracted claim above.

  `scripts/fanout-reduce-lima.sh` runs an arm and classifies every run, and writes the arm's own diff beside the results; `internal/vmmemory/probe_on.go` is the accelerator, which roughly doubles the rate by slowing the pager and is not itself a cause. It now also dates every page and refuses to install into a guest anything older than the newest it was last given writable, which is what the signature says is happening; it has not yet fired.

  **The geometry arm is not yet the answer it was meant to be.** The same fixture with the RAM pager back at 2 MiB over HugeTLB — the configuration before the page-geometry plan's fourth step — ran twenty times on an idle instance with no panic, no wrong bytes and no liveness failure. At the accelerated rate of one panic in two that is a probability of 2<sup>-20</sup> by chance, and at three percent it is one in two; but neither number applies, because a run at 2 MiB is not the same trial. It finished in six to eight seconds against ten minutes at 4 KiB and moved five hundred and twelve times fewer pages, so what it shows is a workload that does far less of everything the pager does, not a geometry that does not fail. Making it decisive means matching the page work — the same number of page transitions on both sides — rather than the same number of runs.

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
  to be re-scaled. Letting read-ahead evict, or reserving a run's worth of slots
  before a scan, is performance work this plan did not do
  (`internal/vmmemory/window.go`, `reserveRuns`).
- **The mapping budget is not enforced at 4 KiB.** `ConnectionConfig.MaxVMAs` is
  still only the client's admission limit, answered with `ENOSPC` and a deferred
  fault. The plan's step 4 also asks the pager to count the mappings a region
  holds and, when a store would exceed the budget, to merge — to copy the
  remaining shared 4 KiB pages of the densest 2 MiB-aligned range into private
  pages so the whole range becomes one private run — rather than leaving the
  guest waiting on a budget only revocation can free. A guest whose writes
  scatter widely enough can still refuse its way into a stall.
- **A page a guest only reads is copied, and the copy is given back at the next checkpoint.** A cold read that has to wait for the pager reaches it as a write fault — on x86-64 because KVM's asynchronous page fault worker always asks for the page writable, on aarch64 when the guest first executes a page — and the pager answers a write fault with a private page. What the [unchanged-page rule](../plans/unchanged-pages-2026-09-19.md) recovers is done: the copy remembers the page it was made from, the settle behind each checkpoint's pause compares the two, and a page that did not change is published nowhere and goes straight back to sharing its origin. What remains is the copy itself. Between the fault and the next checkpoint the host holds the page twice, and with 4 KiB RAM pages under a 2 MiB read-ahead run that is one page in 512, while for PMEM at 2 MiB it is a whole page per cold fault until the interval passes. Preventing it needs a host kernel that passes the guest's access through, or KVM userfault once it exists, and neither is ours to start. Fork points are not settled either: a child inherits an unchanged page as an unpublished one, which its own next checkpoint settles.
- **The workload measurement predates the multi-page parts and wants re-taking.** It was measured against one object per dirty page, before `39bfe37`, so its object counts describe a store layout that no longer exists, and only one fork setting (`FORKS_BASE=2 FORKS_PER_REPO=1`) was run; the commands to re-take it on current `main` are in the document (`docs/measurements-2026-09-14-workload.md`).

## Found by the 2026-09-14 GCE validation

- **Ten workload guests on one boot disk time out.** `FORKS_BASE=4 FORKS_PER_REPO=2` had two guests exceed the orchestrator's ten-minute exec ceiling with nothing in the host logs; both host pods spill to the node's one pd-balanced disk. Three-and-two completed.
- **`sproutfsctl hosts` shows RESIDENT, not COMMITTED.** Placement now turns on the guest RAM a host has promised (`Pager.CommittedBytes` on `/status` and `/metrics`), so an operator cannot see from the table why a placement was refused. The column was left alone because `scripts/lib/demo-run.sh` and `demo-workload.sh` parse the table positionally.
- **Two control-plane changes are unproven on a cluster** (templates named by their image's bytes are proven: the soak's redeploys replaced every pod and the bucket kept one template per image): a same-host fan-out that fails part way takes back the children it started; and the termination budget is exactly 90 s of preStop plus 60 s of shutdown in the 150 s grace period, with no slack for the kubelet's own round trips. Both live in `lifecycle_linux.go`, which need Linux, hugetlbfs and Firecracker; `scripts/demo-gce.sh redeploy` is what proves them (across restarts the bucket should hold exactly one template per distinct guest image, whatever the pods are called, and `scripts/demo-gce.sh fixes` runs the rollout end to end).
- **A guest image's RAM is fixed at the moment it is first imported.** `SPROUTFS_VM_MEMORY_BYTES`, and a template's own `name=path:bytes`, are read when a host imports the image into a template — which now happens once for the whole deployment, because the template is named by the image's bytes. Raising either and rolling therefore gives no new memory to a VM created from an image the deployment already holds: the create forks the template, and a fork inherits what it forked. A cold start with `--memory` changes one VM's shape, which is what `scripts/lib/demo-bigguest.sh` uses, but there is no way to say "every VM of this image is larger from now on" short of changing the image. Either a create should publish its root at the shape the host's configuration gives that image, or the configured memory should be part of what names the template.
