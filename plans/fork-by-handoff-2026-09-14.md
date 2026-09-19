# Fork by handoff — 2026-09-14

Owner's direction: a fork should reuse the live-migration mechanism rather
than require a checkpoint of the parent.

The two paths this left — a fork beside the parent and a fork handed to
another host — are one in
[one fork path](one-fork-path-2026-09-16.md), which is how a fork works now.

## Today

`vmcapture.Fork` takes a full checkpoint of the parent (pause, state, seal,
resume, upload), pins its sequence, creates the child's control record with a
root index copied from that checkpoint, and boots the child sharing the
sealed pages through the pager. Every fork therefore publishes a checkpoint
of the parent: an index, a control write and the parent's dirty pages, even
for a fork that lives seconds. Forks are same-host only.

## The model

A checkpoint has two halves: the pause (stop, save VMM state, seal the
dirty set, resume) and the upload. A fork, like a migration, needs only the
pause. The parent is treated as a migration source that keeps running:

1. Pause, save state, seal, resume. The parent's pause is unchanged.
2. Build a handoff for the child as `vmmigrate` builds one for a migration:
   the VMM state, the parent's last published checkpoint (index sequence),
   and the runs of unpublished pages — everything dirty since that
   checkpoint, now sealed. The parent's handle is not given up; the sealed
   pages stay the parent's and the child reads them by page identity.
3. Pin the parent's last published sequence in its control record, as now,
   and release that pin when the point retires with nothing having
   inherited the checkpoint. Create the child's control record selecting a
   root over that sequence.
4. Start the child. On the parent's host the child shares the sealed pages
   through the pager, as now. On another host the child's pager pulls the
   unpublished pages from the parent's page server post-copy, as a migration
   destination does, and the parent serves them until the child reports
   every unpublished page received or published. `PeerBacking` already marks
   received pages dirty.
5. The child's first checkpoint uploads the inherited unpublished pages as
   its own. The parent's next checkpoint uploads them too. A fork that ends
   before its first checkpoint never touches the store.

The parent's sealed pages are released to it page by page as it writes
(copy on write, as during any upload) and wholesale once every child sharing
them has either copied, published or pulled them, which is the existing
retire path.

## Consequences

- No checkpoint per fork: no index and no upload on the parent's side. The
  one control write left is the pin of step 3, which the point gives back
  when it retires without a child having inherited the checkpoint. Fork
  latency is the pause plus the child's boot.
- Forks may be placed on any host; the orchestrator chooses as it does for a
  migration destination.
- Unpublished pages upload twice when parent and child both live past their
  next checkpoint. Bounded by one interval's dirty set.
- The pin is on the last published sequence, which may be older than the
  fork point; nothing between it and the point is in the store until one
  side publishes, which is the same exposure a migration has.

## Interfaces

`vmmigrate` grows a source mode in which the VM keeps running: the handoff
is built from a seal rather than a stop, and the source's regions are not
released on handoff. `vmcapture.Fork` becomes: seal, handoff, child open from
handoff. `replica.Host` gains `ForkOut`/`ForkIn` beside `Migrate`/`Receive`,
or `Migrate` takes a `KeepRunning` flag. The orchestrator's fork takes a
destination host and defaults to the parent's own host, so the common case
shares pages and the cross-host case is one flag: `sproutfsctl fork
[--to <host>]`. Every fork goes through the orchestrator, which allocates the
child's identity, records it in its table (host, state creating then
running, parent) before the handoff starts, and carries the handoff between
hosts in the cross-host case exactly as it carries a migration's. A host
never forks on its own.

## Proof

Sim tests: same-host fork shares pages and publishes nothing on the parent;
cross-host fork pulls exactly the unpublished pages; the child's first
checkpoint uploads them and reads back correctly; parent writes after the
fork do not reach the child and vice versa; a fork that closes before its
first checkpoint leaves no objects; a parent host loss after a cross-host
fork leaves the child recoverable once it has published. Firecracker Lima
suite. GCE demo flow 2 with `--to` the other host, fork latency and store
counters printed before and after.
