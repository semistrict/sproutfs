# Fixed defects and notes

The open work is in the [backlog](../backlog/tasks): one Backlog.md task per
file. Run `backlog board` or `backlog task list --plain` to see it, and use the
`backlog` CLI to change it. The designs behind finished work are in
[plans/README.md](../plans/README.md).

This page keeps two things the backlog does not: defects that were fixed and
are kept for reference, and notes for anyone running the qualification.

## Fixed, and kept for reference

- **Fixed, and kept here for reference: a post-copy child was told that its own published pages had no object.** The defect killed a child's guest a second or so after it resumed. Usually the crash was in the kernel's timer wheel: `__run_timers` on a node whose `pprev` was `dead000000000122`, the poison value that `hlist_del` leaves. Otherwise the guest crashed in `rb_erase`, `profile_tick` or `process_one_work`, jumped to a wild address, or hung silently. All of these were one defect: the guest read an older version of a page it had written.

  A destination reports no identity for the pages its handoff named. So the pager loads those pages through the peer backing, where the source answers. It does not resolve them against a checkpoint, because no checkpoint holds them. That set was fixed for the backing's lifetime. So the backing kept reporting no identity for those pages after the child's own checkpoint had published them. The pager trusted the answer. A retire gives up a page when the volume holds no object for it, because a publication writes an all-zero page as a sparse hole, and the volume can reproduce such a page without an object. Here the answer was wrong. The retire revoked the guest's mapping and released the only copy of bytes the guest had written. The guest's next read of that page returned the fork point's version. The failure rate scaled with the page count: the fan-out fixture's handoff set is about 8300 pages at 4 KiB, against about 16 at 2 MiB. That is why the defect appeared with the page-geometry plan's fourth step, although that step did not cause it.

  `PeerBacking.Locate` now removes a page from that set only while the checkpoint the volume names for the page predates the handoff. That covers another VM's checkpoint, and this VM's own checkpoints up to the sequence the handoff selected. Both conditions are required, and each has a test that fails without it. Both are needed because a migration keeps the same VM, and its unpublished pages are written after its own last checkpoint.

  Three safeguards were deliberately left in place:
  - `vmmemory.ErrUndroppable` refuses the retire instead of trusting the answer. A page that is given up because the volume holds no object for it must be a page the volume can reproduce without an object, so it must be zeros. Any other page fails the retire. The checkpoint stays durable, the page stays sealed, and the guest keeps its memory.
  - `volume.ErrRetired` refuses a hold on a fork point that its last holder retired. It caught a second defect when it was added. Forking two children through the manager one after the other, with each child's hold released as the child closed, took the second child from a fork point whose seal had ended.
  - `TestFirecrackerForkChildrenSurviveTheirFirstSeconds` reproduces the whole failure in about a hundred seconds per run instead of ten minutes. It keeps the arms that isolated the defect (`SPROUTFS_FORK_ARM`: the whole checkpoint, the capture without the settle, the bare pause, one child, no interval). The sequence of runs that found the defect was 0/8 with no interval, 0/8 for a bare pause, 0/8 for a capture and seal, 5–8/8 for the whole checkpoint, and 0/16 after the fix.

- **Fixed: the probe build's `TestSealTakingAReclaimingPagesReservationKeepsItsBytes` panicked under load. The cause was a refault that acted on a decision a checkpoint had already superseded.** The pager's audit reported `probe bind: page N of memoryRegion … was given slot -1 from outside its own store path while it owned generation G, and now takes slot S — a lost write`. The report came from the store's own `takePrivate`, in about one lane in eight under contention.

  No write was lost. At every step, the page the guest was bound to held the bytes the guest last stored. With the audit finding made non-fatal, thirty lanes ran to completion, and the test's own `reads %d, want the %d the guest stored` check never fired. The audit had caught something else: the pager granted a binding the right to store into memory after that binding's dirty epoch had already ended.

  A reclaim for a private page releases the memory region while it looks for an arena slot. So a seal and a retire can both run inside a fault that has already decided what the page it serves is. The store path re-checks its decision across its own reclaim: `fault` compares the checkpoint's copy before and after. The spill refault in `loadOnce` did not re-check. A checkpoint taken in that window retires the page: the volume holds its bytes, the reservation that spilled them is returned, and the binding is clean. The refault then bound a private page into the binding anyway. That page has neither a reservation nor a checkpoint. `evictBatch` punches out a page in that state without writing it anywhere. Nothing names the page, so nothing that inherits the identity the checkpoint gave it can map it. Every other memory region of that volume reads its own copy of bytes this host already holds. The audit's generation bookkeeping is correct. The binding that owed the audit a newer generation was one the pager should never have granted.

  `loadOnce` now reads the page's dirty state and the checkpoint's copy of the page together, before and after the reclaim. If either changed, it decides again from the start what the page is (`vmmemory/fault.go`, `bindings.go`, `privateEpoch`). `TestARefaultWhoseCheckpointRetiresWhileItReclaimsGivesThePageToTheVolume` drives the interleaving through a reclaim seam. Without the fix it fails on every run in both builds. The ordinary build fails with the second memory region reading its own copy. The probe build fails with the same panic and the same stack. Measured on 2026-09-22 on a fifteen-core machine, with 50 lanes each and a detector on the grant: **10 of 50 lanes before, 0 of 50 after**. At that rate, the chance of a clean result by luck is about 1 in 70,000. The panic that the lanes produce is rarer than the grant that causes it: about 1 lane in 50 on this machine, against 1 in 8 on the eight-core machine the earlier counts came from. After the fix the panic count is 0 of 150 lanes, but the grant's count is what supports the result.

## Notes for the qualification

- **The Firecracker fork has to be rebuilt for mapping protocol version 9.**
  The crate is vendored into the VMM by path. So a cached qualification build
  keeps speaking an older version, and every session it opens fails with
  `invalid managed-memory hello` before a guest starts. That failure is the
  version check working as intended. It is also the first thing to check when a
  Lima or GCE run that used to pass stops attaching: rebuild the VMM, and do not
  reuse `~/.cache/sproutfs-fanout`. The VMM's snapshot format is also at
  version 14. So VMM state that an older build captured is refused on restore,
  instead of being read without its PMEM devices' waiting flushes.

  Two placements deliberately use an ordinary offset, and the code records both
  where they happen (`vmmemory/placement.go`):
  - A store that copies away from the copy a checkpoint froze cannot use its own
    offset, because that offset holds the bytes the upload is reading. The page
    stays outside its range's run until something releases it, and nothing
    moves it back.
  - A page that a migration destination loads privately from the source arrives
    in its own run, like any other load. So a post-copy destination's private
    pages are not placed at all until the guest stores into them.
