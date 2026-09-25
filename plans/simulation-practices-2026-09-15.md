# Simulation practices to adopt from FoundationDB — 2026-09-15

A read-only comparison of FoundationDB's deterministic simulation
(`fdbrpc/sim2.cpp`, `flow/Buggify.h`, `fdbserver/SimulatedCluster.cpp`,
`fdbserver/workloads/`, `contrib/TestHarness2`) against
[docs/testing.md](../docs/testing.md) and `internal/platform/sim`. Ranked by
value against sproutfs's real risks: two writers mixing, lost unpublished
pages, host loss, format drift. Nothing here is implemented.

All twelve are done on `main` as of 2026-09-15; each item's note says what it
built and what it found. Items 1, 2, 5, 8 and 11 have since been converged onto
one harness: `internal/simtest` is the one way a simulated deployment is built,
and each of those campaigns is now a schedule, a fault set and an invariant set
over its `World`. See
[plans/one-simulation-harness-2026-09-15.md](one-simulation-harness-2026-09-15.md).

Where sproutfs is already ahead: byte-exact cross-process trace comparison
(`internal/testrepro`), explicit logical-caller admission (`sim.WithTask`,
`Runtime.Admit`, `vmmigrate.WithAdmission`), and the audited mutation
catalogue. Do not spend effort there.

1. **Kill a host and restart it in-process from the same disk.** FDB's
   `simulatedMachine` loop kills processes with a kill type, drops or garbles
   unsynced writes, and reboots from the same folders. `sim.Process` already
   has this shape (`Crash`, `PowerLoss`, `DestroyMachine`, `FailDisk`) and is
   used by nothing outside its own test; host restarts in
   `internal/host/scheduled_test.go` are orderly closes onto a fresh disk. No
   host is ever lost mid-checkpoint, while holding a fork point, or while
   serving a migration. First step: the host harness owns a `sim.Process` per
   host and one `sim.Disk` per host index; a `crash_test.go` replaces one
   orderly close with `Crash(PowerLoss)` and a restart on the same disk.
   **Done.** Every harness host runs inside a `sim.Process` with a disk of its
   own that outlives each incarnation and its own view of the object store,
   which a kill takes away first so a dying host publishes nothing.
   `sim.Process` gained `Stop`, the orderly end that leaves the disk alone and
   is traced as a stop, so a campaign can ask the trace which hosts it *lost*
   rather than which ended. `crash_test.go` runs one script under all three
   endings and requires the restart to come back on its own disk;
   `crash_campaign_test.go` is the seeded campaign, killing a host
   mid-checkpoint, while it holds a fork point, while it serves a migration
   and while it takes one in, at a moment and in a mode the seed draws, with
   the source's four-checkpoint-interval hold reached on the injected
   `sim.Clock`. `TestHostCrashSoak` runs it over a seed block. See
   [docs/testing.md](../docs/testing.md#losing-a-host).

   **Now in `internal/simtest`.** A `World` host runs inside a `sim.Process` on
   a `sim.Disk` of its own, and `World.Kill`, `World.Restart`, `World.Shutdown`
   and `World.KillDuring` are what a campaign takes one away with; the kill
   campaign and the two endings test are
   `internal/simtest/crash_test.go` and `internal/simtest/lost_host_test.go`.
   What a recovered VM is required to read is now the set of checkpoints it may
   have come back at rather than a range, so an interrupted publication is
   answered for exactly.

   What it found, over 500 seeds: no product defect in what a host loss costs.
   Every recovered VM read as one whole generation between the last
   acknowledged checkpoint and the last one written, through its volume and
   through a guest's own fault path; every one republished; every scenario left
   a deployment behind. Three other things. The shared `machine` double emptied
   its page mapping before detaching the memory region, so the pager was still mapping
   and protecting through a map the close was clearing — nothing before this
   had closed a machine with a capture in flight, so the race had never been
   reached. A fork's child handed to another host has no root index until
   something checkpoints it, and with the interval loop off nothing did, so the
   child opened as `volume: fork's root checkpoint is not published`; the
   destination's own checkpoint is what publishes it, and the campaign takes it
   where a deployment's interval loop would. And a campaign without
   `sim.WithRuntime` on its context injects nothing at all — every probe,
   buggified site and in-tree bug guard is a no-op — which is how the first
   version of this campaign killed none of the thirteen guards; with the
   runtime threaded through it is killed by `migration-corrupt-peer-page`,
   `pager-zero-new-page` and `checkpoint-pack-member-offset`, and it runs with
   `Buggify` on.
2. **Seed-generated topology and failure schedule.** FDB generates the
   cluster and its concurrent failures from the seed
   (`CompoundWorkload::addFailureInjection`); sproutfs permutes a fixed list
   of ten faults over a fixed five-host topology, one fault at a time.
   "Source partitioned while the store is unavailable while a second host
   takes over" is unreachable. First step: an `internal/simtest` package with
   a seeded `Topology` and a `Fault` interface whose driver starts one to
   three faults concurrently at seeded offsets; port the chaos test's faults.
   **Done.** `internal/simtest` draws the deployment from the seed — two to
   four hosts, two to four VMs of one or two volumes, which of them are forks
   of which, and the host each starts on — and `Fault` is `Begin`, `End` and
   `Holds`, the last being what the fault must leave true once it has ended.
   The driver places every fault of the set in a 26-step schedule at seeded
   offsets, one to three on at a time for a seeded run of steps, over
   operations the seed picks: checkpoints, forks, migrations, deletes and a
   host lost and started again. Every start and end is traced through
   `sim.Trace.Record`. The chaos campaign's ten faults are all here as
   conditions of the world rather than phases of one migration, plus three only
   a generated topology can express — a whole host lost, a store one host
   cannot reach while another can, and a swizzle across every link — and that
   campaign itself is untouched.
   `TestASourcePartitionedWhileTheStoreIsAwayAndASecondHostTakesOver` names the
   combination this item calls unreachable, in both its halves: a destination
   that cannot reach the store at all, and one that does take the VM over and
   only then finds it cannot fetch the pages the source still holds. The
   invariants are the three: no guest reads bytes it never wrote, every VM's
   selected checkpoint is one a writer of it published, and `CheckDeployment`
   passes at the end with only the allowances this campaign's own faults earn:
   what a lost host leaves, what a deleted fork parent leaves, and the
   checkpoint a sweep the store refused could not take. A VM nobody is running
   is opened again by a host that can, and the model rewinds to exactly what
   the checkpoint its record selects published; see
   [docs/testing.md](../docs/testing.md#seeded-topologies-and-failure-schedules).

   **And it is now the only harness.** The migration chaos campaign, the kill
   campaign, the swizzle campaign and the three scheduled scenarios are
   schedules over the same `World`, whose hosts are real `host.Host`s: what each
   of them used to build for itself — the stores, the networks, the clocks, the
   processes, the disks and the guests that remember what they wrote — is built
   once, and a fault added to the kit is a fault every campaign can draw.

   Running real hosts is what found the next defect, fixed in `8ccfb15`: a host
   owns a page cache, `checkpoint.Cache` names a cached page by its page
   identity, and a delete freed that identity — so a VM created again under a
   deleted VM's name read the deleted VM's pages out of the cache. Seventeen of
   the first two hundred seeds reached it; the smallest is seed 45. No host-local
   fix was enough, because every host that ever read the old VM's pages held
   them under the same name, so a creation now draws its first epoch from the
   host's entropy and an identity whose namespace still holds objects is
   refused. See [docs/testing.md](../docs/testing.md#seeded-topologies-and-failure-schedules).

   What it found, once the harness itself stopped being the problem, is a
   defect in reclamation, on ten of the first two hundred seeds. A sweep spares
   a pinned checkpoint and, its own documentation says, every pack that
   checkpoint's index names; the code asked the index for the packs it *reads*,
   which leaves out the ones its compaction emptied and it
   still names. A fork taken on a checkpoint that had just compacted a pack
   empty therefore lost that pack at the next sweep, and the pinned index went
   on naming an object nothing could fetch — a hole in what a fork inherits, which
   is what `CheckDeployment` reports as a pack part that does not read. It needs
   a fork's point, a compaction and a later sweep to line up, which no
   campaign over a fixed topology with one parent and its forks had put together.
   `TestReclamationSparesThePacksAPinnedIndexOnlyNames` is the case, and the
   sweep now spares what the pinned index names.

   It also found two things about the simulated world, both of which a campaign
   running one fault at a time could not reach. A frame held by the
   stalled-stream fault deadlocked the whole bubble: the first frame a host
   receives may be a guest's own demand page fault rather than a post-copy's
   request, and a page fault has no deadline of its own, so the guest waited on
   a frame the fault was holding and the step that would have ended the fault
   was the step the guest was blocking. The hold now ends on a bounded
   simulated wait as well as on the cancel and the end of the fault. And a
   frame dropped with `Network.DropNext` on a page-server link hangs the guest
   for ever with no other outcome available: the connection stays open so the
   reply never comes, and the demand fault waits rather than giving up — which
   is the right answer for a page whose only copy is on that peer, since giving
   up on it loses the guest's memory. A reliable message-framed connection
   cannot lose a frame while staying open, so the degraded-links fault does not
   ask it to; what a lost reply really costs is the connection, which is what
   the lost-page-replies fault takes. A third finding is the harness's own: a
   checkpoint's sweep runs behind its publication, so a world closed the
   moment after a checkpoint lands cancels it and leaves the checkpoint it
   replaced behind. The campaign gives its sweeps a moment before it closes, as
   a host draining itself would, rather than spending an allowance on it.
   `Network.ClearFaults` is new beside them: a drop or a delay still armed when
   a fault ends is a fault that outlived its window.
3. **BUGGIFY: per-site fault injection inside production code.** FDB's
   `buggify(p)` keys on source location, activates ~25% of sites per seed,
   traces activations, and randomises tunables. sproutfs injects only at
   adapter boundaries; tunables are constants. First step:
   `sim.Buggify(ctx, id, p)` two-level as FDB's, recording activations in the
   trace; an `internal/knobs` struct for the tunables with
   `Randomize(sim.Random)` used by the campaigns.
   *Tunables done*: `internal/knobs` holds them with production defaults,
   `Validate` and a seeded `Randomize`; the migration chaos and soak campaigns
   and the volume scheduled scenario draw one set per seed under
   `SPROUTFS_TEST_KNOBS`.
   *Sites done*: `sim.Buggify(ctx, id, p)` and `sim.BuggifyDelay` activate a
   site once per run at 0.25 from the seed and the site's id, then draw each
   call's firing, and trace both. Every site is off unless a campaign sets the
   runtime's switch, so the recordings keep their bytes. Five sites: a pack
   part that fills at one page, a publication that gives up on a part its own
   retry found, a control-record write that takes seconds, a pager that takes a
   victim past a free slot, and a page source that answers BUSY.
   `TestMigrationChaosUnderBuggify` drives four of them and requires the
   campaign to reach each. The BUSY site — a fault one migration at a time
   could never produce — fires around a hundred times a run, and the eviction
   site is what reaches the spill paths an arena sized for its guest never
   touched.
4. **[done] Unsynced writes come back lost, partial or garbled.** FDB's
   `AsyncFileNonDurable` resolves each pending write at a kill into applied,
   dropped or sector-garbled; sproutfs's `Disk.PowerLoss` restores the last
   sync exactly, and nothing calls it. First step: pending writes since the
   last `Sync` resolved through the seeded random at `PowerLoss`.
   Done as `DiskConfig.PowerLossFaults`, opt-in so the default device and
   every recording built on it are unchanged: a kill mode per file per open,
   a mode per 4 KiB page, and each 512 B sector applied, dropped, torn or
   garbled, traced per modification. `SyncDurableProbability` models a lying
   flush and is driven by no consumer, since nothing here treats a local file
   as durable. The pager's spill gained a per-reservation checksum, which was
   handing the guest bytes it never wrote; the VMM state file can carry no
   checksum of ours and is refused by its invalidated handle; pack parts are
   refused by their own trailer, table and member envelopes.
5. **[done] Clogging and swizzling, generated.** FDB's `RandomClogging` and
   swizzle (block half the links at random offsets, heal in a different
   order) is its champion bug finder. sproutfs's network has the whole kit
   (`Partition`, `DropNext`, `DuplicateNext`, `DelayNext`, `SetLink`) and
   uses almost none of it; there is no timed clog. Two writers mixing *is* a
   swizzle. First step: `Network.Clog(from, to, until)` and
   `Swizzle(addrs, window, r)`; a host test swizzling two hosts and the
   store while both hold one VM.
   Done as a schedule read through `Runtime.Now` rather than goroutines that
   heal themselves, with `Network.Clogged` so the object store can be taken
   away by naming it as an endpoint.
   `internal/simtest/swizzle_test.go` is the campaign, and it drives
   `DropNext`, `DuplicateNext`, `DelayNext` and `SetLink` on the page-server
   links a handoff runs over, through the `simtest.DroppedPageServerFrames` fault.
   That fault is the one the generated schedule does not draw: a dropped frame
   on an open connection leaves a guest's demand fault waiting for ever, which
   is the right answer for a page whose only copy is on that peer, so the drop
   belongs to a campaign whose every fetch is a bounded attempt that is
   retried.
6. **An invariant check after every test.** FDB runs `ConsistencyCheck`
   after every spec regardless of workload. First step:
   `volume.CheckDeployment(ctx, store, prefix)` listing the whole simulated
   store: every record, index and pack parses at the current version; every
   pack a selected index names exists; every pin names a live holder; every
   object is reachable from a record. Call it from every scenario's cleanup.
   **Done**: `volume.CheckDeployment`, composing
   `(*checkpoint.Store).CheckIndex`, called from the migration chaos campaign
   and its soak, the scheduled volume, migration and host scenarios and the
   host harness's cleanup, with an explicit allowance per class of leftover a
   lost host leaves; see
   [docs/testing.md](../docs/testing.md#the-deployment-check). It found two
   leaks, both fixed: a create never reclaimed its own root index, and a fork
   closed before publishing left its record behind.
7. **Restart across versions for format drift.** FDB's `tests/restarting`
   pairs run phase one on an old binary and phase two on the new one over
   the same folders. sproutfs refuses every version but the current and has
   no fixtures. First step: byte-exact dumps of a small simulated store under
   `testdata/` for record 3, index 5 and pack 1, written by an `-update`
   flag and read back by a test; keep the old fixture at every bump. **Done**:
   a whole deployment under `internal/volume/testdata`, opened and read back
   byte for byte and checked with `CheckDeployment`, plus per-format fixtures
   and superseded twins under `internal/control`, `internal/checkpoint` and
   `internal/checkpoint/internal/pack`. The contract is refusal with the
   version named, and every refusal's text is asserted; see
   [docs/testing.md](../docs/testing.md#format-fixtures).
8. **Determinism validation and a no-cheating rule.** FDB prints an unseed
   after every run and re-runs 5% to compare; its `IRandom` split forbids
   nondeterministic draws from influencing simulated state. First step:
   `Runtime.Fingerprint()` hashing every trace event, asserted equal across
   two runs per seed in the chaos campaign; a `just check` rule forbidding
   `math/rand`, `crypto/rand` and bare `time.Now` in volume, checkpoint,
   control and vmmigrate.
   *No-cheating rule done*: `internal/testdeterminism` is a `just check` step
   over volume, checkpoint, control, vmmigrate, host and vmmemory, with one
   allowed entry — the pager client's two kernel socket deadlines — and a test
   of its own that it rejects each thing it forbids.
   *Fingerprint done*, and it found that the migration chaos campaign is not
   reproducible at the dependency level: how many of a destination's memory regions
   dial a source that is being taken away before the first failure marks it
   fallen is a race between goroutines, and one extra connection attempt moves
   the simulated clock and the network's operation numbering for the rest of
   the run. Nothing else about the campaign varies. So the strict fingerprint
   is asserted where the harness chooses completion order
   (`TestScheduledHostFingerprintIsStable`) and `Runtime.WorkFingerprint` —
   the same digest without order, moment or adapter numbering — where it does
   not (`TestMigrationChaosFingerprintIsStable`, which excludes connection
   attempts and bounds how many it excludes). A trace event carries no offset,
   so neither digest can tell two equal-sized writes to one file apart; the
   models compare the bytes.

   **Now in `internal/simtest`**, unchanged in substance:
   `TestScheduledWorldFingerprintIsStable` asserts the strict digest over the
   one recorded scenario, and `TestSeededTopologyFingerprintIsStable` the work
   digest over the generated schedule, bounding the connection attempts it
   excludes by the number of memory regions the topology has.
9. **One clock, injected.** *Done.* `platform.Clock` and its sibling
   `platform.Entropy` sit beside `platform.Disk`, with wall implementations and
   a simulated clock the test advances itself. They are threaded through
   `host.Config`, `SupervisorConfig`, `control.Config`, `vmmemory.Config` and
   vmmigrate's `Options` and `PeerConfig`; every `time.Now`, `time.Since`,
   `time.AfterFunc`, `time.NewTimer`, `time.NewTicker` and `ctxsync.Sleep` on a
   production path in those packages is the injected clock, and the writer nonce
   and the interval jitter are the injected entropy. Both hold timers, the
   checkpoint interval and the epoch watch went first;
   `TestForkHoldExpiresOnTheSimulatedClock` retires a fork hold at the
   deployment's own four-interval bound with no wall-clock wait.
10. **Know which faults never fired.** FDB's `CODE_PROBE` traces every
    probe a run missed. First step: `sim.Probe(ctx, name)` and
    `Runtime.MissedProbes`, with a soak-only test failing on a registered
    probe never hit: fenced publication, reconciled lost reply, compaction
    rewrite, eviction during publication, volume fallback.
    **Done**, and the answer is that three of the five registered probes are
    reached by no campaign here: a fenced publication, a reconciled lost reply
    and an eviction during a publication. Only a compaction rewrite and a
    volume fallback fire. The two the plan's list does not name — a pin
    released and a tombstone finished — have no site at all now that a pin is
    permanent, and were dropped. `unreachedProbes` in
    `internal/vmmigrate/probe_test.go` names the three and is asserted in both
    directions, so the list can only shrink. Probes are counted on the runtime
    rather than traced, so registering one changes no recording.
11. **Seed sweeps on a schedule.** Joshua runs tens of thousands of seeds
    nightly; sproutfs has no CI. First step: a workflow running `just check`
    on push and a nightly soak with a seed-range matrix, logging simulated
    versus wall time per seed. **Done.** `.github/workflows/check.yml` runs
    `just check` — now gofmt, build and vet for both `GOOS`, `go test ./...`,
    `buf lint`, `shellcheck` and the Rust crate's fmt/clippy/tests — on push
    and pull request; `.github/workflows/soak.yml` runs eight blocks of 100
    seeds nightly and on dispatch. `internal/testsoak` is the seed-block
    plumbing (`SPROUTFS_SOAK_SEED_BASE`/`_COUNT`) and the per-seed record:
    simulated time, wall time, ratio and a trace fingerprint, uploaded as the
    sweep's artifact. The Lima/KVM suites stay out; hosted runners have no
    nested virtualization. The sweep is now one package: `just soak`,
    `just soak-race`, `just test-knobs` and the nightly workflow run
    `internal/simtest`'s campaigns and nothing else. First finding, now fixed: half of the migration
    campaign's first sixteen seeds deadlocked at `stream-canceled` in a later
    cycle, waiting on a stall that never happens when the destination needs no
    page from the source (`just soak 13 1`). The fault stalls the first frame
    of any kind rather than the first page reply, so it lands on the resident
    listing when there is no page to fetch, and the hop asserts the cancel is
    what ended the stream.
12. **Negative tests in-tree.** FDB's `SimBugInjector` enables a named bug
    for one run. sproutfs patches source strings out of tree. First step:
    `sim.Bug(ctx, id)` guards at the sites the catalogue already names,
    enabled by an environment list, so a catalogue entry is one `go test`.
    **Done**: thirteen of the fifteen catalogue entries are guards named by
    `SPROUTFS_SIM_BUG`, which a runtime reads once when it is built, and each
    is killed by the invocation `scripts/mutation/guards.json` gives. Three of
    those fifteen had already stopped matching the source they were written
    against, which is what the guards are for. `pager-forget-spill` is killed
    only by the buggified campaign, because nothing else evicts enough to
    spill. The two that remain source edits are wrong behaviours in the
    simulated dependencies themselves, which no guard in production code can
    express.

Two corrections: `internal/platform/sim/process.go` is complete and
unreachable; wire it in (item 1) or delete it. `docs/testing.md`'s failure
coverage section implies a fault surface the tests do not drive. *The first is
settled: `sim.Process` is what every harness host runs inside, and
`docs/testing.md` now has a section on losing one.*

A third, found while wiring the knobs in and reproduced on this plan's base
commit with no knobs and no clock injected: the `stream-canceled` fault may
cancel a stream that was never going to fetch anything. In the default seeds 1,
7 and 23 that hop's handoff carries no unpublished page, so forcing pages to
exist is what exercises the fault, and doing so makes seeds 1 and 7 fail at the
destination's first read with `the source has not served a page no checkpoint
holds: disk page 0`. **Settled, and it was both.** The campaign listed
`stream-canceled` among the faults that take the source's pages away, so that
hop checkpointed before its migration and handed off nothing to fetch; a
cancelled stream takes nothing away — the source keeps every page and goes on
serving — so the fault now runs over a handoff that carries pages. With one, it
found a real post-copy defect: `PeerBacking.Resident` gave the source up for
good on any listing error, and the caller of that listing is the stream the
destination itself stops and cancels with a cause of its own, so stopping a
healthy stream sent the memory region to a volume that does not hold the pages no
checkpoint has. A cancellation is now read from the context there, as it
already is on the load path.
