# Plans

One status line per plan: what is done, what is open. The open engineering work
across all of them is collected in [open-work.md](../docs/open-work.md).

## Designs under way

- [2026-09-19 RAM and PMEM page geometry](ram-pmem-page-geometry-2026-09-19.md)
  — separate arenas and volume geometry; 4 KiB RAM ownership and writes with
  2 MiB read-only mapping batches; PMEM stays 2 MiB on HugeTLB, unchanged. No
  stored data is converted: the formats' versions are bumped.
  **Steps 1 to 4 are done — the sharing gauges; a volume that carries its own
  page size, 4 KiB or 2 MiB, recorded in its checkpoints at index format 8; a
  pager whose page is an instance's, with one pager per kind of region on every
  host, its own arena and spill file each and the deployment's byte budgets
  divided between them; and, at step 4, RAM at 4 KiB on a real host. Mapping
  protocol version 7 carries each session's page and the kind of memory its
  arena is made of, the RAM arena is an ordinary memfd and PMEM's stays on the
  HugeTLB pool, and Firecracker takes no huge-page setting for managed RAM. Step
  4's own outstanding piece is the VMA-budget merge, and the fan-out suite does
  not yet pass at 4 KiB; steps 5 to 7 are planned.**
- [2026-09-19 a private page that did not change](unchanged-pages-2026-09-19.md)
  — a write fault is not always a store (KVM's asynchronous page fault worker on
  x86-64, cache maintenance on aarch64), so a sealed page whose bytes equal the
  page it was copied from is not dirty: the checkpoint does not publish it and
  the guest's page goes back to sharing its origin. **Done**, and measured on GCE and
  in Lima: a read-only fork's root pages are its own until its next checkpoint
  and nobody's after it.
- [2026-09-14 repository layout](layout-2026-09-14.md) — nothing in the module is
  a library, so every package but `cmd` moves under `internal/`, the large
  packages gain nested `internal` bodies, `image` becomes `checkpoint`, and
  nothing outside `platform` can name a platform adapter. **Step 1 done**:
  page identity moved to `control`, `image` renamed to `checkpoint`, and every
  non-`cmd` package moved under `internal/`, with the real adapters behind
  `internal/platform/adapters` and the API packages at `internal/api/{host,orch,guest}`.
  Open: the nested splits, the `internal/host` consolidation, and decoupling the
  API packages from the runtime.

## Designs carried out

- [2026-09-18 four words](words-2026-09-18.md) — *frame* (of memory) is
  *resident page* or *private page*; *instant* is *pause* or *fork point*; *two
  planes* is said plainly; the second decision is stated rather than contrasted
  with content addressing. **Done.**
- [2026-09-18 loss window](loss-window-2026-09-18.md) — a VM whose oldest
  unpublished write is older than `SPROUTFS_LOSS_WINDOW` (five minutes; zero
  disables) has its stores wait in the pager until a checkpoint lands, and the
  checkpoint is asked for out of turn and retried at an eighth of the interval.
  The age travels with a migration and a fork, so a destination inherits the
  window rather than restarting it. **Done.**
- [2026-09-16 a template named by its image](template-by-digest-2026-09-16.md) —
  a template is `template-<sha256 of the image file>`: hosts import an image
  once between them and only if absent, a restart imports nothing, a changed
  image is a new template, and the hosts go back to a Deployment. Supersedes the
  per-pod generations of the same day, which never reached a cluster. **Done**
  (`31b8ec3`, `cc8daa9`, `026adeb`, `3919282`, `c52eed1`). `Host.TemplateOf` is the one entry point: a published template
  — its record pins the checkpoint it selects — is opened by nobody and rebuilt
  with `Manager.Inherit`, which is what the importing host uses too, so no
  host's epoch sits on an identity every host names; an absent one is imported
  as before; a record with no pin is waited for and then recovered by taking the
  epoch. `PrepareTemplate`, `TemplateGeneration`, the lookup of a template's
  earlier generations and the three `Identities` listings that served them are gone, and
  `deploy/10-host.yaml` is a Deployment at `maxSurge: 0`, `maxUnavailable: 1`.

  Three departures, each in the plan's own terms. `Manager.CreateIfAbsent` is
  new, because the plan's race — "two hosts race on the record's create-if-absent
  and the loser reads the winner's record" — cannot be had from `Manager.Create`,
  which opens an identity it finds recorded and would fence the import in
  flight. It is also not refused by objects under the identity that no record
  accounts for, which `Create` is: a create that ended between its root and its
  record leaves exactly that, nothing can have forked a VM whose record never
  existed, and a template's identity is met again by design, so refusing them
  would wedge that guest image for ever. And `TemplateImport.Wait` is where the
  plan's "bounded by the import's own timeout" lives, because there was no
  import timeout to bound it by — ten minutes by default, and negative for a
  test that has already established the writer is gone.

  One thing the plan did not see, recorded in
  [open-work.md](../docs/open-work.md): a guest image's RAM is fixed at the
  moment it is first imported, because the create forks the template and a fork
  inherits what it forked. Raising `SPROUTFS_VM_MEMORY_BYTES` and rolling gives
  no new memory to a VM of an image the deployment already holds, which is what
  `scripts/lib/demo-bigguest.sh` used to do; it takes its 4 GiB from a cold
  start now, which is the one moment a VM's shape can change and exercises the
  same mapping above the MMIO gap.

  Proof: `just check`, including the shell harness, whose new
  `scripts/test/fixes-test.sh` runs the fixes flow's rollout step against a
  model of the deployment that replaces a pod with a pod of another name;
  `go test -race` over `internal/host`, `internal/volume`, `internal/control`,
  `internal/simtest` and `cmd/...`; 200 seeds of `TestSeededTopologySoak`;
  `TestScheduledWorldReproduces`; and the Lima Firecracker suite. Open: the
  whole of it is unproven on a cluster — `scripts/demo-gce.sh redeploy` and
  `fixes` are what prove it, and the bucket should then hold exactly one
  template per distinct guest image.
- [2026-09-16 cold boot](cold-boot-2026-09-16.md) — `start --cold` discards a
  stopped VM's memory and VMM state in one publication and boots its kernel from
  the root volume, as a template's children already do; the soak cold-starts half
  of what it restarts. **Done** (`34b3c43`, `248b609`, `8e3acbd`, `14750b9`,
  `41f8bdb`, `77c70c5`). `volume.VM.DiscardMemory` is the one operation, under
  the publication lock, and `checkpoint.Publication.DropState` is what lets a
  checkpoint name no VMM state rather than going on naming its parent's;
  `host.Host.OpenCold` opens and discards, and `vmmachine` boots the kernel
  because there is no state to restore. The shape goes with it: any memory the
  host admits, up or down, and a root volume that may only grow, with the
  guest growing its filesystem over the new pages after the boot.

  Five departures, each in the plan's own terms. `DiscardMemory` takes the name
  of the memory volume and the sizes, rather than the plan's `DiscardMemory(ctx)`
  — this package does not decide which of a VM's volumes is its memory, and the
  resize is part of the same publication by the plan's own requirement. The
  host's cold open is `Host.OpenCold` rather than a flag on the supervisor's
  `Open` alone, so that the operation is provable off Linux, where the
  supervisor is not built; the supervisor's `Open` gains the flag and calls it.
  The discard covers the pages the new shape still has rather than the whole old
  volume: a page past the end of a volume that shrank is one the publication
  trims out of the index rather than one it republishes as zeroes. The
  orchestrator's own record of a VM's memory keeps the template lookup as its
  fallback, for a row written before it and for a VM the table has never heard
  of, which is the placement this had before it measured memory at all. And the
  witness's `--disk-only` is a standalone check that opens the file and takes
  its size from it rather than a request to a resident witness: a cold-started
  guest has none, which is the whole reason it is asked for its disk alone.

  Seed 91 of the topology campaign found a defect the moment the schedule could
  draw a cold start, and it was the model's: `RestartHost` gave a host back a VM
  the deployment had stopped, leaving it marked stopped while it ran, so a later
  start of it was a second writer. A settle already left one alone; a host
  restart does now too.

  Proof: `just check`; `go test -race` over `internal/volume`,
  `internal/checkpoint`, `internal/host`, `internal/simtest` and `cmd/...`; 200
  seeds of `TestSeededTopologySoak`, which drew 89 cold starts and 84 warm ones
  over 307 stops; `TestScheduledWorldReproduces` and
  `TestSeededTopologyFingerprintIsStable`; `shellcheck` and the shell harness,
  which runs the soak's cold half, its resize share and its grow against the
  model; and the Lima Firecracker suite. Open: the host's cold start is
  proven on the simulated deployment and in `internal/host`, and the real
  Firecracker suite proves the cold boot it ends in but not that whole path —
  the GCE soak run is where the two meet, and it is the owner's.
- [2026-09-16 GCE soak](gce-soak-2026-09-16.md) — `scripts/demo-gce.sh soak`:
  rounds of forks on the same and other hosts, migrations, stop and start and a
  host kill, with a witness in every guest checking memory and disk after every
  step and a deployment check at the end; adds `stop`, `start` and `check` to
  the control plane. **Built** (`525a304`, `7c5a632`, `821009b`, `3371c14`,
  `f967d59`, `ab55012`, `6687fa3`); **run on GCE on 2026-09-17**, passing on
  the ninth attempt after eight real defects, recorded in
  [docs/measurements-2026-09-17-soak.md](../docs/measurements-2026-09-17-soak.md). `Host.Stop` publishes what a guest holds and then
  gives up the process, the pages and the handle, refusing a VM a fork point
  holds sealed as a delete is; `Start` is `Recover` without the evidence of a
  loss, so the two are one path with the terms that differ passed in; `GET
  /check` reports every violation as a body rather than a status line. The
  seeded campaign draws stop and start, `sproutfs-guest-witness` is in both
  guest images, and `scripts/lib/demo-soak.sh` runs the rounds.

  Six departures, each in the plan's own terms. The check's allowances are four
  classes and not the three the plan names: "the templates" is two of them — a
  guest image's import publishes intermediate checkpoints nothing selects, and
  a replacement host pod re-importing at its predecessor's identity leaves that
  epoch superseded. The soak's object-store counters come from `sproutfsctl
  store` rather than from `/metrics`: they are the same counters, and neither
  pod image carries an HTTP client to read the exposition with. The host is
  killed at the top of its round, straight after the capture and before
  anything in that round has mutated a guest, rather than after the round's
  work: every host checkpoints on its own interval, so a kill taken after a
  mutation comes back at a state the script cannot name — or at one published
  in the middle of a mutation — and a guest checked against a state it was
  never in is a failure that says nothing. A fork's child takes a seed of its
  own by refilling rather than by mutating under a new one, because `mutate`
  rewrites under the resident witness's own seed and a check's expectation is a
  pure function of one `(seed, step)`; a child that mutated under a second seed
  would need a chain of seeds in the pattern. One parent fans out per round
  rather than every running VM, because four children per VM per round is fifty
  VMs by the second one. And `stop` reports the checkpoint it published, which
  the plan did not ask for: it is the pause a start brings the VM back at,
  and nothing else records it — the handle that knew is released by the time
  the stop answers.

  Proof: `just check`; `go test -race` over `internal/simtest`, `internal/host`
  and `cmd/...`; 200 seeds of `TestSeededTopologySoak`, which drew 329 stops and
  177 starts; `TestScheduledWorldReproduces` and
  `TestSeededTopologyFingerprintIsStable`; `shellcheck`; the Lima Firecracker
  suite. The witness was also exercised as a binary: filled, checked, mutated,
  checked again, and refused with the offset named against the step before it
  and against another seed.
- [2026-09-16 one fork path](one-fork-path-2026-09-16.md) — a fork is always a
  handoff; a child on the parent's host receives it over a local backing that
  shares the parent's sealed pages, every child's root is published when it
  holds its pages, and one hold table, one cleanup and one admission replace
  the local and remote paths. **Done** (`2350921`, `0f5738b`, `3e82df0`,
  `1e5b90b`, `9766c99`). `Host.Fork` takes
  the point once and returns one handoff per child whatever the destination;
  `Host.ForkOut`, the local branch, its hold registration, its rollback and its
  eager root are gone, and so is the orchestrator's second fork branch.
  `ForkPoint.Share` is the local backing's attach. Five departures, each in the
  plan's own terms: `vmmigrate.Fork` takes no page source for a child the
  parent's host takes in, rather than serving it and releasing it, because a
  page server that answered no fetch cannot say the child has the pages and
  would refuse the release with `ErrOutstanding`; the root the destination
  publishes is a capture rather than the overlay-only publication the local
  path used, because a child on another host holds what it inherited as its
  pager's dirty state and only a seal publishes that; `DirtySource.Hold` is new,
  because `Share` used to mark the seal as a fork point's as well as name its
  pages and a child on another host never names them; a local child's hold is
  not in `Status().Serving`, which is what a drain waits on and what the
  orchestrator's stale-handover survey reads, so a local hold an orchestrator
  never releases is bounded by its deadline alone; and a forked child no longer
  carries its parent's template name in the host's own VM record, which only
  the local path ever set. Nothing left open.
- [2026-09-16 one post-copy rule](one-post-copy-rule-2026-09-16.md) — a page
  only the source holds is asked for until it arrives or the source is declared
  gone by the source itself or by the orchestrator; the retry caps, failure
  counters and error classification go. **Done** (`1da0484`, `c6cb79f`,
  `53bc04f`, `610c873`). Two departures, each in the plan's own terms. `ask`
  loops for a run that holds a page only the source has; a run every checkpoint
  holds is read from the volume when its request fails and decides nothing for
  the next load, which is what the plan's "no volume fallback for a page only
  the source holds" leaves open and what keeps the bulk resident stream — every
  page of which is published — from asking a dead host for ever. And the
  backing's context is its own rather than the received VM's: `Close` is what
  ends it, which is what the destination's own stop and the orchestrator's
  discard both come to. The orchestrator did not end a migration from a lost
  source before this — its `Receive` call had nothing watching it — so that half
  was built: a survey on `SourceWatchInterval` while the destination receives,
  acting only on a pod the Kubernetes API no longer lists or a host that answers
  and neither runs the VM nor serves its pages. Open: that watch is unproven on
  a cluster, and a source that is unreachable while its pod is still listed
  produces no evidence either way, both recorded in
  [open-work.md](../docs/open-work.md).
- [2026-09-16 two planes](two-planes-2026-09-16.md) — the store from first
  principles: a checkpoint is one index object (header, the segments it changed,
  root) and its parts (VMM state and pages only); a segment lives in the index
  object of the checkpoint that wrote it, compaction touches the data plane
  only, and "pack" is gone from identifiers, keys, docs and tests. Index format
  7, part layout 4. **Done** (`e9e13a4`, `2f37094`, `d1a0559`, `e7daac9`,
  `a216491`). Three
  departures, each in the plan's own terms: `Open` is one whole GET of the index
  object rather than a ranged read, because the header at its front is what
  locates the root and the object is bounded at 64 MiB for exactly that; a
  segment left with no pages loses its root entry rather than being written
  empty, which is what "a segment whose last page is zeroed loses its entry"
  requires and what makes an emptied segment cost nothing; and the proof that
  "compaction opens no index object it did not write" is taken as "it reads no
  other root and moves no segment", since rewriting a page's location requires
  range-reading the segment that locates it, wherever that segment was written.
  `Root` for a new VM writes an index object and no parts at all, so a
  checkpoint entry of zero parts is now a legal thing for a root to name.
  Nothing left open.
- [2026-09-15 one simulation harness](one-simulation-harness-2026-09-15.md) —
  `internal/simtest` is the one way a simulated deployment is built; the
  migration chaos, crash, swizzle and scheduled campaigns are schedules, faults
  and invariants over its `World`, with nothing they assert lost. **Done**
  (`61561d6`, `495a28a`, `8cfc20c`, `1fede03`, `fda1477`, `866e11a`). A `World`
  host is a real `host.Host` inside a
  `sim.Process`, on a `sim.Disk` with power-loss faults, keeping its deadlines
  against a `sim.Clock`; `Kill`, `Restart`, `Shutdown`, `Takeover` and
  `KillDuring` moved in from the harnesses that are gone. Three departures from
  the plan: the swizzle campaign's `DropNext` is a fault the generated schedule
  deliberately does not draw, because a dropped frame on an open connection has
  only one outcome for a guest's demand fault and it is to wait for ever; the
  four crash scenarios are a fixed schedule asking the driver for a kill inside
  one operation rather than constraints on the generated schedule; and
  `sim-power-loss-keeps-volatile` is killed by a full-suite run rather than a
  scheduled one, which is what it was before this change too. Open: the defect
  running real hosts found — a deleted VM's identity is handed out again, and a
  host's page cache still holds the pages that name it, so the VM created under
  that name reads the dead one's bytes. Seventeen of the first two hundred
  seeds of the generated schedule reach it and stay red until a page identity
  carries which creation it belongs to.
- [2026-09-15 the root in the pack](root-in-the-pack-2026-09-15.md) — the root
  is the last member of a checkpoint's last part, so a checkpoint is its parts
  only, one suffix read of `pack/last` opens it, and `Rebuild`, `Recover` and
  tombstones are gone. Pack format 3. *Superseded by the two planes above.*
  **Done** (`ae04c34`, `f0dcc1d`,
  `2f118ab`). Three departures, each in the plan's own terms: a pack's
  recorded bytes exclude the root, because the root states that total and cannot
  be inside it, so the consistency check subtracts the root's length from the
  last part's members; the part count the root states is what the writer says
  the root itself will make, asserted against the part it lands in; and the
  trailer keeps the layout version sixteen bytes from the end and the magic in
  the last eight, where the 32-byte trailers put them, so a superseded part is
  still refused by the version it names. Opening a deployment that has `index`
  objects reads one of them to name the index format version it was written
  under, which is what the refusal owes. `Open` decodes the trailer and the root
  and refuses a root the trailer places outside the bytes in hand; it does not
  also decode the table, which no reader of a root needs. Nothing left open.
- [2026-09-15 a part's table in one read](one-read-part-table-2026-09-15.md) —
  a part's tail is bounded at 256 KiB of table plus the trailer and read as
  one suffix range, so rebuild, recovery and the consistency check cost one
  round trip per part instead of three. *Still how a part's table is read; the
  checkpoint layout around it is the two planes above.* **Done** (`399797c`, `c05d1d3`,
  `f90ccd9`). `platform.ByteRange` carries the suffix form, the writer seals a
  part whose table is at the bound, and the pack layout is untouched: the format
  stays 2 and the fixtures are byte-identical. One departure: the size the
  trailer's offsets are checked against is the object size the ranged Get
  reports, not the suffix's length plus the table's offset — that offset is what
  the trailer decode returns, so it cannot be an input to it. The reader checks
  the table lies inside the bytes it holds separately, which is what the plan
  asked that arithmetic for. Nothing left open.
- [2026-09-15 segmented index](segmented-index-2026-09-15.md) — the page table
  is split into 512 MiB segments stored as pack members, and the index object
  becomes a root naming segments, so a checkpoint writes O(changed segments)
  rather than O(volume) of index and a reader fetches segments on demand.
  Index format 6, pack format 2. *Superseded by the two planes above, which
  keeps the segments and moves them out of the data plane.* **Done** (`0174c37`, `931744a`, `42027fe`, `e79e008`, `0505759`).
  Three departures from the design, each in the plan's own terms: a segment a
  checkpoint emptied is written and named rather than dropped, so that a rebuild
  reads what a tombstone means out of the same pack; the segments go into the
  pack after compaction rather than after each volume's pages, since a page
  compaction moves is one the segment naming it has to be written for; and a
  segment entry's reads carry the bytes as well as the pack, so that measuring a
  pack's liveness is a sum over the root rather than a read of every segment.
  Nothing left open.
- [2026-09-13 complexity cuts](complexity-cuts-2026-09-13.md) — the requirement
  answers and the features removed. **Done.** All seven steps are complete: the
  log, replication, membership, TLS, deltas, configurable page size, multi-RAM
  regions, pre-copy and the shared disk ledger are gone, and the documentation
  and `TODO.md` rewrite that was step 7 has landed. The index per checkpoint it
  left open is closed by the segmented index above. Its checkpoint-interval
  question is answered for one workload at 60 s with jitter; the measurement
  wants re-taking.
- [2026-09-14 pack per checkpoint](pack-per-checkpoint-2026-09-14.md) — a few
  objects per checkpoint instead of one object per dirty page, with checkpoint
  position, part and offset in the index, set-difference reclamation and bounded
  compaction. *Superseded by the two planes above.* **Done** (`39bfe37`, refined
  by `5cd5d01`). `checkpoint.Config.PartBytes` defaults to 64 MiB;
  `internal/checkpoint/internal/part` writes the self-describing parts. Open: the
  host-wide bound on concurrent publications' part buffers.
- [2026-09-14 fork by handoff](fork-by-handoff-2026-09-14.md) — a fork is a
  migration handoff from a parent that keeps running, so no checkpoint is
  published per fork and a fork can be placed on any host. **Done**
  (`6ad0e05`, `9aed0a6`, `861a60b`). `volume.ForkPoint` is one pause for N
  children and `sproutfsctl fork --to` places them. A pin is released when
  no child inherited the checkpoint durably; bounding pins further needs the
  collector.
- [2026-09-13 GCE demo](demo-gce-2026-09-13.md) — a single-node k3s cluster on
  one disposable GCE VM, two host pods, a GCS backend, and the boot, fork,
  migration and recovery flows. **Done and in use**: `scripts/demo-gce.sh`
  drives it and the workload measurement was taken on it. Open: the demo node
  builds the Firecracker branch from a submodule commit that is not pushed.

## Audits

- [Host resource completion audit](host-resource-audit.md) — the
  requirement-by-requirement evidence and the stated limits for the host
  resource work. **Closed**, with one caveat: the shared disk ledger it
  vouches for was replaced by per-concern caps.
- [2026-09-14 production review](production-review-2026-09-14.md) — three reviews of the new system ranked together: ten blockers, the serious and minor findings, and the documentation drift the per-doc pass fixed.
- [2026-09-14 production review, second pass](production-review-2026-09-14b.md) — four reviews after the fixes and the layout refactor: four blockers, the serious and minor findings, and what the first pass fixed.
- [2026-09-15 simulation practices](simulation-practices-2026-09-15.md) — twelve practices from FoundationDB's deterministic simulation to adopt, ranked by the risks they cover, each with a first step. **Done**, all twelve; each item's note records what it found, the last two being a pinned index losing a checkpoint its compaction had emptied and the harness races the crash campaign shook out.
- [2026-09-15 production review, third pass](production-review-2026-09-15.md) — four reviews after the second pass: ten blockers led by one-hop pins losing a grandchild's data, the serious findings, and what is sound. **Fixed** on `main`; see its status section. One of those fixes is superseded: a template's identity was made the pod name, then the pod name and a generation, and is now the sha256 of the guest image — see [a template named by its image](template-by-digest-2026-09-16.md) — so the hosts need no stable names and are a Deployment again.
