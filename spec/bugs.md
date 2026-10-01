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
`TestAReceiveKeepsItsRowInFlight` in `cmd/sproutfs-orchestrator`. Mutants:
`spec/postcopy/mutants/release-under-receive.cfg` and
`rows-age-while-driven.cfg`.

## B2. An orchestrator crash during a migration loses the guest's writes

Found by `spec/postcopy` (TASK-75) on 2026-10-01. Open.

The handoff — the VMM state and the memory layout a destination needs — lives
only in the orchestrator's memory. If the orchestrator crashes while a
migration is handing over, a receive that fails afterwards can never be tried
again, though the source still holds every page no checkpoint has. Once the
row ages, a survey releases the source, and the VM is recovered from its last
checkpoint: every write since is lost, with no host lost and no hold run out.

The counterexample: a receive fetches every page and is then discarded; the
orchestrator crashes; it restarts; the row has aged; a survey releases the
source.

`spec/postcopy/MCPostCopy.cfg` tolerates this loss (`Tolerated = {"B2"}`), and
`spec/postcopy/mutants/b2-open.cfg` shows it is still there. A fix keeps the
handoff where a restarted orchestrator can find it: in the table, or on the
source, which could hand it out again while it holds the pages.
