# One post-copy rule — 2026-09-16

## The problem

A migration's destination resumes the guest before it holds every page.
Pages a checkpoint holds come from the destination's own volume; pages only
the source's RAM holds — written since the last checkpoint — are fetched
from the source's page server. Around that core `PeerBacking` grew a policy
layer, each rule fixing a real bug: five attempts per request with backoff,
four consecutive exhausted loads before the source is given up for good, a
classification of which errors are final, a fallback that sends a memory region to
its volume after one line of log, and a `failures` counter reset by any
reply. The numbers are guesses. Give up too early and a live source's page
is lost for good; the guest then reads a page no checkpoint holds and the
fault fails. Give up too late and a dead source stalls the guest. Nothing in
the destination can tell the two apart, so no number is right.

## The rule

A page only the source holds is asked for until it arrives or until
something that knows says the source is gone. Two things know:

1. **The source itself**, answering that it no longer serves this VM
   (`ErrNotServed`), which it does only after a release it agreed to.
2. **The orchestrator**, ending the migration: it discards the received VM,
   which cancels the backing's context. It does so when it has lost the
   source host, at which point the pages are lost with it and the VM is
   recovered from its checkpoint.

Everything else — a reset connection, a timeout, a listener restarting, a
connection dropped by a budget, a BUSY reply — is retried with the existing
backoff for as long as the backing's context lives. There is no attempt
count, no failure threshold, and no volume fallback for a page only the
source holds.

## What changes

- `fetchAttempts`, `persistentFailures`, `failures`, `giveUp` and the
  fallback classification go. `ask` loops until a reply, `ErrNotServed`,
  or cancellation.
- A source serving pages of another size, or a reply this host cannot read,
  is a source this destination cannot use, not a source that is gone: the
  fault fails with that cause and the received VM is torn, exactly as a
  torn post-copy is handled today, rather than reading the volume.
- `fallBack` remains only as the transition to "the source is gone" on
  `ErrNotServed`: from then, a page a checkpoint holds reads from the volume
  and a page only the source held fails with `ErrUnpublishedLost`.
- BUSY handling is unchanged: it is flow control. Per-peer budgets are
  unchanged: they bound a source's work, not a destination's patience.
  Install-not-load accounting and cancellation from the context are
  unchanged.
- The orchestrator's part is made explicit and tested: when its survey loses
  a source host while a migration of a VM from it is in flight, it discards
  the destination's received VM and recovers the VM from its checkpoint,
  and the in-flight row goes with it. If that already holds, the test
  proves it; if not, it is built.
- `docs/migration.md` states the rule once, in place of the paragraph that
  explains the numbers.

## Proof

Red first, in `vmmigrate` under the simulated clock and network:

1. A source that resets the connection twenty times in a row still serves
   the page afterwards, and the fault completes.
2. A source silent for ten simulated minutes leaves the fault waiting, not
   failed; when the source answers, the fault completes with the right bytes.
3. A source answering `ErrNotServed` fails a fault for a page only it held
   at once, with `ErrUnpublishedLost`, and serves published pages from the
   volume.
4. Discarding the received VM ends a waiting fault with the cancellation's
   cause and nothing else.
5. In `internal/simtest`: the orchestrator-side rule above, through the
   `LostHost` fault landing on a migration source.

Then the whole suite, `-race` on `vmmigrate` and `internal/simtest`,
200 seeds of the topology campaign, the Lima Firecracker suite's live
migration test, and the reproducibility scenario.

## Docs

`docs/migration.md`, `docs/architecture.md`'s loss model if it names the
retry policy, `docs/open-work.md` if anything is left.
