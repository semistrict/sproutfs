# Defects found by the specs

Every real defect a spec under `spec/` has found in the code, or in a plan
before the code, with the counterexample that showed it and what became of
it. A counterexample that turned out to be a gap in a model is not listed
here; the spec's task records those.

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

## B3. A recovery could fence a guest its survey missed

Found by `spec/recovery` (TASK-77) on 2026-10-01. Fixed.

A recovery or a start reopens a VM once a survey shows no host running it,
serving its pages or receiving it, and the table row is not in flight. A survey
asks each host at its own moment, and the row is read after it, so the survey
can miss a host that opened the VM while it was asking. The open then took the
epoch from that host and fenced a guest that was running: its writes since its
last checkpoint were lost, and the VM came back from that checkpoint elsewhere.

The counterexample: a recovery begins while a migration from c to b is under
way. The survey asks a and b, which hold nothing yet. The migration stops the
guest on c, b opens it and runs it, the row says it is running on b, and c is
released. The survey asks c, which holds nothing now. The row is not in flight,
so the recovery opens the VM on a, and fences b. Two concurrent reopens of a
stopped VM, such as a start a client retried, race the same way.

The fix: a reopen reads the VM's epoch before it surveys, and the host's open
takes the next epoch only from that one (`control.Client.OpenAfter`,
`volume.Manager.OpenAfter`, `OpenRequest.Epoch`). An open made since is refused
with `control.ErrMoved` and fences nothing. Tests:
`TestOpenAfterRefusesARecordThatMoved` in `control`,
`TestAnOpenPastItsEpochIsAConflict` in `cmd/sproutfs-host`, and
`TestRecoverIsRefusedOnceTheVMWasOpenedBehindTheSurvey` in
`cmd/sproutfs-orchestrator`. Mutant: `spec/recovery/mutants/b3.cfg`.

## B4. Another writer takes the space a sparse spill file was promised

Found by `spec/disklimit` (TASK-81) on 2026-10-03, in
`plans/disk-cache-2026-10-02.md`, before any code. Open: the plan must change.

The plan keeps the spill files sparse, and has the limiter count each at its
promise, so the cache never takes the space a spill file may still need. But
that space is only free space on the filesystem, and another writer on the
same filesystem can take it. Nothing the host does gets it back. The cache
gives every region back, and the spill file is still refused. A store into
guest memory then has nowhere to put a dirty page. So `PromisesKept` fails,
whatever the limiter counts.

The counterexample: the cache is empty, another writer fills the
filesystem, and the spill file grows into no space.

The fix: allocate each spill file's whole extent when its pager starts
(`fallocate`), as a region's space is allocated when it opens. The dirty
budget already sets that extent, and the limiter already counts it whole, so
the cache gets no less. Another writer then finds the filesystem full, not
the guest. The ephemeral spill file needs the same. The model checks the
design with the fix (`Spill = "allocated"`). Mutant:
`spec/disklimit/mutants/b4.cfg`.

## B5. A join or a leave moves every later stripe off its rank

Found by `spec/diskcache` (TASK-81) on 2026-10-03, in
`plans/disk-cache-2026-10-02.md`, before any code. Open: the plan must change.

The plan puts stripe i on rank i, and says that a join or a leave costs a
window one stripe. Rendezvous keeps the order of the other caches, but not
their ranks: a cache that joins at rank r moves every holder below it down
by one, and a leave moves them up. So after one join at the top of a
window's ranks, no holder holds the stripe of its own rank. A reader that
asks rank i for stripe i decodes nothing, though K stripes are there. And
repair, which sends a rank that lacks the stripe of its own index that
stripe, writes to every rank from the change down, so each holder below it
keeps two stripes of the window. A drain or a rolling restart does this to
every window it touches.

The counterexample: three hosts hold a window with a 2+1 code. A fourth
joins at rank 1. Its join pushed one holder out, so two stripes are still
on the window's ranks, but neither is on the rank of its own index, and a
reader with the new list reads the store.

The fix: a read asks each rank for every stripe of the window it holds, of
any index, and decodes from any K, as the model does. Repair sends only an
index that no rank holds, to a rank that holds fewer of the window's
stripes than the code puts on it. The model checks reads of any index
(`SurvivesLosses`); it does not count the writes repair makes. Mutant:
`spec/diskcache/mutants/b5.cfg`.

## B6. A failed batch leaves its pages journaled

Found by `spec/journal` (TASK-104.1) on 2026-10-06, in
`plans/fsync-journal-2026-10-06.md`, before any code. Fixed in the plan.

A capture takes a page out of the unjournaled set, write-protects it, and
sets its digests to the blocks it read. The plan said what a failed write or
sync does to the journal: its flushes fail with EIO, and the next batch pads
the failed range. It did not say what it does to the pages. Left as they
are, they are not unjournaled, and their digests describe an entry that may
not be on the disk. The next capture takes nothing of them, or skips the
blocks whose digests match. So a flush that succeeds after a failed one does
not cover the stores the failed one was sent after. A guest that tries its
flush again after EIO, as a database may, is told its data is durable when
it is not.

The counterexample: the guest stores into a block and flushes; the capture
takes the page; the batch fails. The guest flushes again; the capture takes
no page, the empty entry syncs, and the flush is answered. A replay holds the
old bytes. With the pages given back but their digests kept, the second
capture takes the page but skips the block.

The fix: a failed batch gives every page it took back as unjournaled and
drops their digests, as an abandoned seal does. The model checks the design
with the fix (`NoLostFlush`). Mutants:
`spec/journal/mutants/failed-keeps-pages.cfg` and
`spec/journal/mutants/failed-keeps-digests.cfg`.
