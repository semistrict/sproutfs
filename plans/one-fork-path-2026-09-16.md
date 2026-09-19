# One fork path — 2026-09-16

## The problem

A fork is a handoff from a running parent, but there are two of them.
`Host.Fork` creates the children beside the parent: it seals, records a
hold per child, creates each child on the fork point, publishes the child's
root before the child's guest exists (the child's pages are the parent's
sealed pages, shared through the pager by page identity, and publishing
is what lets the parent's seal end), and gives the hold back. `Host.ForkOut`
hands each child to another host as a migration: the destination boots the
child first and pulls the pages no checkpoint holds from the parent's page
server; the child's root is published by its first checkpoint, so it reports
`ErrForkPending` until then; the parent's seal ends when every child has
released. Each path has its own admission, hold registration, deadline,
partial-failure cleanup and give-up ordering, and the third review found its
bugs in the gaps between them: local children with no hold, a partial
fan-out orphaning guests, a parent deleted under a local child, forks placed
without admission.

## The design

One path. A fork is always a handoff; where the child lands only changes
how its inherited pages reach it.

- **Take the pause once.** Seal the parent, pin the checkpoint, and produce
  one handoff per child, whatever the destination. This is `ForkOut`'s
  first half, and `Fork` is gone.
- **Receive the child the same way everywhere.** The destination — this host
  or another — receives the handoff, admits the child against its own
  budget, boots it, and binds its unpublished pages to a backing. On another
  host that backing is the peer backing over the page server, as today. On
  the parent's host it is a local backing over the fork point: the pager
  shares the parent's sealed pages with the child by identity, so every
  inherited page is present the moment the region attaches and no byte is
  copied. Both satisfy the same interface; `Received.Done` means the same
  thing for both: the child holds every page only the parent had.
- **Publish the root when the child has its pages.** The child's root is
  published as soon as `Done` reports, on the destination, by the
  destination — locally that is right after the child attaches, exactly when
  `Fork` published it today; remotely it is the end of the post-copy, rather
  than the child's first interval checkpoint. `ErrForkPending` still names
  the window between boot and that publication.
- **One hold table.** The parent's host records one hold per child with one
  deadline, whatever the destination, and retires the fork point when the
  last child has released — after its root is published — or when the
  deadline passes; the fan-out's own hold covers the window before the
  children exist. `Abandon` is the one give-up path for a child that will
  never release.
- **One cleanup.** A fan-out that fails part way discards every child it
  started, on whichever host, and retires the point; the orchestrator's
  `discardChildren` and the host's own rollback are the same operation.
- **One admission.** Every child is admitted against the host taking it
  before the parent is paused, locally as remotely.

## What goes

`Host.Fork`, `CreateFork`'s eager root as a separate mechanism, the local
hold registration in `fork.go`, the local partial-fan-out rollback, and the
orchestrator's two fork branches. `ForkPoint.share` becomes the local
backing's attach. The fork API (`POST /vms/{id}/fork` with `to` defaulting
to the parent's host) is unchanged.

## Proof

Red first:

1. A local fork's child reads its inherited pages with no copy and no page
   server dial (count pager loads and connections), and its root is
   published before the parent's seal ends.
2. A remote fork's child has its root published when its post-copy is done,
   not at its first interval checkpoint: with the interval loop off, the
   child opens on another host once `Done` has reported.
3. The parent deleted, lost or stopped while a local child still reads the
   point is handled exactly as for a remote child: refused while sealed by
   a hold this host has none for, given up by deadline otherwise.
4. A partial fan-out, local and remote, leaves no child running, no child
   record, and the parent checkpointable.

Then the whole suite, `-race` on `internal/host`, `internal/vmmigrate`,
`internal/volume` and `internal/simtest`, both Lima suites (the same-host
fork test in the Firecracker suite is the real-guest proof), 200 seeds of
`internal/simtest`'s topology campaign, whose faults fork on both kinds of
host, and the reproducibility scenario.

## Docs

`docs/migration.md` (the fork sections), `docs/hosting.md`,
`docs/architecture.md`, `deploy/README.md`'s fork rows, and
`plans/fork-by-handoff-2026-09-14.md` gets a one-line pointer here.
