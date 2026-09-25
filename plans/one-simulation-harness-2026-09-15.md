# One simulation harness — 2026-09-15

## The problem

Four campaigns build a simulated deployment four ways: the migration chaos
campaign over `vmmigrate`'s `cluster`, the scheduled scenarios in
`volume`, `vmmigrate` and `host` over their own
fixtures, the crash campaign over `host`'s `hostHarness` with a
`sim.Process` per host, and `internal/simtest`'s `World`, which already runs
real `host.Host`s from a seeded `Topology`. Each fixed the same things
separately — stores, networks, clocks, guests that remember what they wrote —
and each carries its own subset of the fault kit. A fault added to one is
missing from three.

## The design

`internal/simtest` is the one way a simulated deployment is built, and every
campaign is a schedule, a fault set and an invariant set over a `World`.

### World

- A `World` is what the crash harness and `hostHarness` build today: one
  `sim.Process` per host, one `sim.Disk` per host index with
  `PowerLossFaults` on, a per-host `sim.Clock` and `Entropy`, a gated store
  view per host, real `host.Host`s with pagers and page servers, guests that
  hold the bytes they stored. `World.Kill(host, mode)` and
  `World.Restart(host)` move in from the crash harness; the `LostHost` fault
  uses them, and the crash campaign's four scenarios (mid-checkpoint,
  holding a fork point, migration source, migration destination) become
  schedule constraints the driver can be asked for.
- The swizzle campaign's link faults (`DropNext`, `DuplicateNext`,
  `DelayNext`, `SetLink` on page-server links) join `simtest`'s fault kit.
- The hooks the campaigns grew — knobs per seed, buggify, probes, in-tree
  bug guards, fingerprints, `testsoak.Measure`, `CheckDeployment` at the end
  — are `World` options, on for every campaign, not per-harness code.

### Campaigns

- **The migration chaos campaign** becomes a `simtest` schedule: its ten
  faults are already ported; what moves is its assertion set, its soak and
  its buggify and fingerprint tests. `vmmigrate/chaos_test.go` and
  the campaign-only parts of its `harness_test.go` go; the `cluster` fixture
  stays only where package unit tests need it.
- **The crash campaign** and **the swizzle campaign** become `simtest`
  schedules; `host/crash_campaign_test.go`, `crash_test.go`,
  `swizzle_test.go`, `soak_test.go` and `newSimHostHarness` go.
- **The scheduled scenarios** are what recording, replay and byte-exact
  cross-process reproduction run over. One scenario over a `World` with a
  fixed seed replaces the three; `internal/testrepro` and
  `scripts/check-overlap-reproducibility.py` point at it, and
  `TestScheduled*Reproduces` becomes one test. The volume scenario's
  package-level fixture stays for volume unit tests only.
- **Soak** runs `simtest`'s campaigns: `internal/testsoak`'s blocks,
  `just soak`, `just soak-race`, `just test-knobs` and
  `.github/workflows/soak.yml` name them and nothing else.

### What must not be lost

Every assertion, fault, seed count, probe, buggify site and mutation guard
the four campaigns exercise today is carried over, and the report lists each
deleted test by name next to the `simtest` test that now makes its
assertion. Concretely, after the change:

- `scripts/mutate-simulation.py --seeds 2` kills every catalogue entry it
  kills today (`guards.json` names the new invocations).
- `unreachedProbes` does not grow.
- `TestMigrationChaosUnderBuggify`'s four sites still fire.
- The reproducibility script reports zero mismatches for the one scenario.
- `just check` passes and the soak matrix runs.

## Proof

The gates above, plus 200 seeds of each converged campaign green and a
40-seed soak block, and `-race` on `internal/simtest`, `host`,
`vmmigrate` and `volume`.

## Docs

`docs/testing.md` describes one harness and its campaigns;
`plans/simulation-practices-2026-09-15.md` items 1, 2, 5, 8 and 11 point at
`simtest`.
