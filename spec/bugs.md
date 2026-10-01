# Defects found by the specs

Every real defect a spec under `spec/` has found in the code, with the
counterexample that showed it and what became of it. A counterexample that
turned out to be a gap in a model is not listed here; the spec's task records
those.

## B1. A survey released a source under a receive still in flight

Found by `spec/postcopy` (TASK-75) on 2026-10-01. Fixed.

A migration's row is believed in flight for two minutes (`inFlightFor`). While
one receive ran, nothing wrote the row again, so a receive that outlasted two
minutes left an aged row. Every survey releases a handover whose row is not in
flight (`orchestrator.release`), and it did not ask whether a host still
reported a receive of the VM. The source accepts a release once its book of
pages owed is empty. The book strikes a page off when a reply carrying it has
left the host, so it is no evidence the destination has the page.

The counterexample: a reply carrying page 1 leaves the source and is lost; the
row ages; the source sends page 2 and its book is empty; a survey releases the
source. The receive still needs page 1, which now exists nowhere, and the VM
falls back to its last checkpoint. The source was alive and inside its hold
throughout. A receive discarded after the source sent every page, followed by
a survey during the retry's wait, loses the pages the same way.

The fix: a survey leaves a handover alone while any host reports a receive of
its VM in flight, and a migration writes its row again on each look at its
source. Tests: `TestAnAgedRowDoesNotReleaseASourceUnderAReceive` and
`TestAReceiveKeepsItsRowInFlight` in `cmd/sproutfs-orchestrator`. Mutant:
`spec/postcopy/mutants/b1.cfg`. Since B2's fix a survey releases only a
handover a host runs, which covers this too; the row is still written again so
that a survey never takes up a handover a live orchestrator is retrying.

## B2. An orchestrator crash during a migration loses the guest's writes

Found by `spec/postcopy` (TASK-75) on 2026-10-01. Fixed by TASK-80.

The handoff — the VMM state and the memory layout a destination needs — lived
only in the orchestrator's memory. If the orchestrator crashed while a
migration was handing over, a receive that failed afterwards could never be tried
again, though the source still held every page no checkpoint has. Once the
row aged, a survey released the source, and the VM was recovered from its last
checkpoint: every write since was lost, with no host lost and no hold run out.

The counterexample: a receive fetches every page and is then discarded; the
orchestrator crashes; it restarts; the row has aged; a survey releases the
source.

The fix: the source keeps the handoff for as long as it holds the pages and
hands it out again (`Host.Handed`, `GET /vms/{id}/handoff`). A survey
releases a handover only once a host runs the VM. One that no host runs or
receives it takes up again with the source's handoff, and carries it over as
a migration's own retries would. Tests: `TestASourceKeepsItsHandoffWhileItHoldsThePages`
in `host`, `TestAHostHandsOutTheHandoffItHolds` in `cmd/sproutfs-host`, and
`TestASurveyTakesUpAHandoverNothingDrives` in `cmd/sproutfs-orchestrator`.
Mutant: `spec/postcopy/mutants/b2.cfg`.
