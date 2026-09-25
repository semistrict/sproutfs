# The loss window — 2026-09-18

**Status: done.** The bound is proved where it acts: the pager holds a guest's
stores back past the window and lets them through when a checkpoint of its VM
lands, the window survives an abandoned checkpoint and travels with a migration
and a fork, and a simulated host lost after an outage rewinds its VM by at most
the window plus one checkpoint attempt — asserted at every recovery the
campaigns make and in four scenarios of their own.

## The decision

A VM's unpublished writes are bounded in time, not only in bytes. Each host
carries a **loss window**, `SPROUTFS_LOSS_WINDOW`, five minutes by default and
zero to disable. When a VM has held an unpublished write for longer than the
window, the pager admits no further dirty page for that VM — every store that
needs a dirty reservation waits — and asks for a checkpoint out of turn; the
stores land when a checkpoint of that VM lands. The talk claimed this on
2026-09-18 and the code did not do it: the interval was a target, a failed
upload was retried an interval later, and a parent held sealed by a fork was
not checkpointed at all until the hold's deadline.

## What is guaranteed, exactly

The **loss window of a VM** is the age of its oldest unpublished write: the
time since the first store, into any of its memory regions, that no landed checkpoint
covers. It is measured on the host's clock — the pager's `platform.Clock`, so
a simulated deployment measures it in simulated time.

While a VM's loss window exceeds `LossWindow`:

- no store of that VM that needs a dirty reservation is admitted: a store into
  a page that is not yet privately dirty, and a store into a sealed page, both
  wait in the pager exactly as a store past the dirty budget waits today;
- the pager asks the host for an immediate checkpoint of the VM, through the
  same `Pressure.Checkpoint` the dirty budget uses, and the host's loop takes
  it out of the interval's turn;
- a publication that fails while the window is exceeded is retried promptly,
  with the loop's backoff — an eighth of the interval, doubling to the
  interval — rather than at the next interval;
- the wait ends when a checkpoint of the VM lands and the pages it published
  are retired, or when the VM is stopped.

A store into a page the guest already dirtied and that no seal covers does not
fault and is not blocked. The checkpoint the pager asks for seals every dirty
page in its pause, so from that pause every store of the VM waits; the gap
is between the window expiring and that seal, and it is one pause away.

So what a host loss can cost a VM is bounded: **the lost writes of one VM span
at most the loss window plus one checkpoint attempt's pause**, from the first of
them to the last. Losing the host after an outage longer than the window still
loses writes older than the window — nothing can publish through an outage —
but the guest was stopped from building on them from the window on.

A migration or a fork moves unpublished pages to another host; their age moves
with them. The handoff carries, per memory region, how old that memory region's oldest
unpublished write is at the handoff, and the destination dates the pages it
receives from that, on its own clock. A destination therefore inherits the
source's window rather than restarting it.

Where a VM's checkpoint can never be taken — its loop is off, or its memory region
belongs to no VM the host runs — a store waiting on the window is a store
waiting for a checkpoint nothing will take, and it ends the way a budget stall
ends today: the host stops the VM deliberately, with a last checkpoint of what
it can still capture. A fork hold is not that: the hold ends at its deadline
and the parent is checkpointed then, so a parent's stores wait.

The overlay path — writes through `volume.VM.Volume().Write`, which image
building and tests use — is outside the window: nothing runs a guest through
it, and its callers publish a checkpoint of their own when they are done.

## The changes

### `internal/vmmemory`

- `Config.LossWindow time.Duration`. Zero disables.
- Each `MemoryRegion` records `dirtySince`: the clock time its first unpublished
  store landed, zero while it holds no unpublished page. Set under the memory region
  lock where a page first becomes privately dirty (the store path and the
  peer-served unpublished load path in `fault.go`), when it is zero. A
  `MemoryRegionCheckpoint` records the memory region's `dirtySince` at the seal and the
  memory region's own is cleared; a retire that published drops the checkpoint's; a
  retire that did not, and an `Unseal`, hand it back — the memory region's becomes the
  older of the two. `MemoryRegion.oldestUnpublished()` is the older of the memory region's
  and its draining checkpoint's.
- `Host.takeSpill` waits, before it takes a slot, while the memory region's VM is
  over the window: `h.clock.Since(oldest) > LossWindow`. The wait is the
  existing one — `relief()` to ask for a checkpoint, `h.changed` to be woken
  when one ends, `stall` when none will ever be taken. A `WindowWaits` counter
  beside `DirtyWaits`, and a `WindowStalls` beside `DirtyStalls`.
- The window is a VM's, so the pager needs the VM's memory regions together: the
  memory region's owner, which `Pressure` already identifies per memory region, is asked for
  the oldest across the VM. Add `Pressure.Oldest func(*MemoryRegion) time.Time`, or
  key memory regions by an owner the host sets at attach — whichever leaves the pager
  with no knowledge of VMs. The implementer picks; the plan needs the window to
  be per VM, not per memory region, because the checkpoint is.
- `MemoryRegion.Status()` (or the host's per-memory-region report) exposes `DirtySince`, so
  the host can report a VM's loss window and whether its stores are waiting.
- The handoff: `MemoryRegion.Handoff()` reports `UnpublishedAge time.Duration`; the
  receive path sets `dirtySince = now - age` on the memory region when it binds the
  peer backing, and a peer-served unpublished load keeps the older of that and
  its own arrival.

### `internal/vmmigrate` and `internal/host`

- `Handoff` memory regions carry `UnpublishedAge`; the wire encodes it; the Lima
  fixture asserts it survives the round trip.
- `host.Config.LossWindow`, passed to the pager; `cmd/sproutfs-host` reads
  `SPROUTFS_LOSS_WINDOW`, logs it at start beside the interval.
- The checkpoint loop, `internal/host/interval.go`: a failed publication of a
  VM whose window is exceeded schedules the next attempt at an eighth of the
  interval, doubling to the interval, instead of the full jittered wait.
- `host.VM` status: `LossWindow time.Duration` (the current age of the oldest
  unpublished write, zero when nothing is unpublished) and `Waiting bool`
  (stores are waiting on the window). `/status` and `/metrics` carry both;
  `sproutfsctl vms` shows the age.

### `internal/simtest`

- `Knobs.LossWindow`, passed to every host.
- The world's model dates every store it makes. At every recovery — a VM
  coming back after a lost host, at the checkpoint its record selects — the
  world requires that the writes it rewound span at most the loss window plus
  one pause: `VerifyLossWindow`, called wherever `VerifyDurable` is.
- A scenario, `TestAGuestPastTheLossWindowWaitsForItsCheckpoint`: a VM stores
  on a host whose link to the store is blocked; its stores succeed until the
  window, then a store waits (a `Store` with a deadline reports it waiting
  rather than hanging the test); the link heals; the checkpoint lands; the
  store completes; the bytes are the guest's. A second scenario runs the same
  with the window disabled and requires that the store never waits.
- A scenario for the handoff: a VM with unpublished pages older than half the
  window migrates; on the destination, with the store blocked, stores wait at
  the window measured from the original writes, not from the arrival.
- The campaigns run with a window drawn from the seed — including zero — so
  the invariant is exercised under kills and swizzles.

### Documentation

- `docs/architecture.md` loss model and `docs/hosting.md` checkpoint loop:
  the window, what it bounds and what it does not, and the default.
- `docs/context.md`: **Loss window**.
- `deploy/README.md`: `SPROUTFS_LOSS_WINDOW`.
- `talk/slides.md`: the requirement slide may say "bounded" again, with the
  exact statement above.

## How it is proved

Every test above is written red first. `just check` green. The Lima
full-guest suite carries the handoff field. The simulated campaigns run
`just soak 1 100` with the window in the seed. GCE is not needed for this:
nothing in it is Linux-only except the wire field, which Lima covers.
