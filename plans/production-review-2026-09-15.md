# Production review, third pass — 2026-09-15

Four read-only reviews of `main` at f44cefd, after the second pass's fixes:
storage and durability; memory, VMM and migration; control plane and
operations; whole-system consistency. Ranked together. Items marked *proven*
have a reproducing scratch test. The collector is out of scope by the owner's
decision: pins are permanent and nothing unpins until a collector exists.

## Status (2026-09-15, later)

Every blocker and every serious finding below is fixed on `main`, each
behavioural one with a test that failed first, in six branches merged in
turn: pins permanent (record format 4; `Unpin`, `Release`, `Tombstone`,
holders and the parent field gone), the control plane (same-host holds,
handoff checkpoint identity, `Recover` refusals, placement by committed RAM,
delete of an unrun VM, per-VM migrate exclusion, `/livez`, the exit budget, a
StatefulSet for template identity — *superseded: a template is named by its
guest image's bytes now and the hosts are a Deployment again, see
[a template named by its image](template-by-digest-2026-09-16.md)*), the pager
(the seal-versus-reclaim race,
the orphaned copy, `mapped` under its lock, retired `ReadDirty`, error-returning
`Resident`/`Unpublished`), and the VMM and migration (detached control
contexts, fetched-once-sent accounting, install-not-load, bounded retries,
cancellation read from the context, Firecracker waiting out in-flight seals,
the wire naming its failure). Merging them exposed two more, fixed the same
way: a store woken during a seal was stalled for a checkpoint an instruction
away (`c1f66ba`), and a released handover's test asserted the process close
before it landed (`805923b`).

The VMA-budget refusal landed last (`de6416a`, `5cc437d`): a refused map is a
deferred fault, and a revocation is admitted against the headroom a refusal
leaves. Still open from this pass, recorded in
[docs/open-work.md](../docs/open-work.md): the `sproutfsctl hosts` column and
the three control-plane changes only a cluster can prove.

## Blockers

1. **Pins protect one hop, so a grandchild's data is reclaimed.** *(proven
   twice)* A record pinned only its immediate parent and released the pin
   when its own index stopped naming that parent's packs, but a fork's index
   names its grandparent's packs too. Every VM is a fork of a template, so
   any fork of a VM is a grandchild. Fix: pins are permanent; a pin protects
   every pack its checkpoint's index names, transitively, and nothing
   releases it. Holders, tombstones, the parent field and the cascade go.
2. **Deleting or losing the parent of a same-host fork destroys the child.**
   *(proven)* Only cross-host forks registered a hold; `Delete` and
   `discard` closed the parent's process while a local child still read the
   point. Fix: register local children with the same deadline; refuse
   `Delete` on a sealed VM.
3. **A handoff carries no checkpoint identity.** A writer that publishes
   between the source's handoff and the destination's open is post-copied
   over; `Recover` ignores `Serving` and in-flight rows, which is the window.
   Fix: the handoff names the source's selected checkpoint and `Receive`
   refuses a mismatch; `Recover` refuses while any host serves the VM.
4. **A seal racing a reclaim loses a dirty page's only copy.** *(proven)*
   `evictBatch` snapshots the alias set before reading reservations; a seal
   in the gap moves the reservation, the walk writes nothing, and the page
   is punched.
5. **One failed page allocation while copying away from a checkpoint
   orphans the page for good.** *(proven)* `b.checkpoint` is cleared before
   the replacement page is bound and the region is not made terminal.
6. **A caller's cancelled context reaches the VMM's control requests and
   kills the guest**, from an HTTP disconnect on capture, fork, migrate or
   drain; the recovery path uses the same dead context, leaving the guest
   paused and sealed.
7. **Post-copy accounting order.** The source records a page fetched before
   the reply is sent; the destination clears it unfetched before the pager
   has installed it and can drop it silently; one transient error becomes a
   permanent fallback; caller cancellation discards a running VM.
8. **A fork point's own pin leaks once the parent's handle is gone**, and a
   tombstone never finishes. Moot under permanent pins.
9. **`stopStalled` skips the give-up ordering** every other path has.
10. **`/livez` cannot fail**, and a stuck in-flight table row disarms the
    orchestrator's release path for good.

## Serious

Storage: `Handoff` of a fork with an unpublished root seals the parent for
good; a tombstoned VM's writer uploads forever (moot); `Fork` leaks pins on
four error paths (harmless once pins are permanent, but the record it
orphans is not); the emptied-pack grace is dropped by `encode`; a corrupt
record lets `Delete` wipe a lineage; reclamation removes the index when part
deletes failed; GCS 416 unnormalised; publication memory is twice the
documented bound; `dirtySectors` materialises a slice.

Memory and migration: a data race on `binding.mapped` between seal and
reclaim; a retired checkpoint's `ReadDirty` answers with current bytes;
`Unpublished` and `Resident` return nil for both nothing and failure; three
fsyncs inside the pause; a failed `openScratch` wedges scratch; `Start` can
hang; per-peer budgets key on the ephemeral port; `fetch` has no retry;
Firecracker drops pending seals and exits when starting a seal fails; VMA
exhaustion is a terminal session; the wire collapses three causes into one
message.

Control plane: placement by arena residency refuses drains on warm hosts
*(proven)*; a VM no host runs can never be deleted *(proven)*; template
identity is the pod name, so every restart re-imports and orphans an image;
same-host forks have no eager root and a partial fan-out leaves the parent
sealed; partial same-host forks orphan guests; `Migrate` has no per-VM
exclusion; the termination budget is 60 s of shutdown in a 150 s grace
period with a 120 s preStop client against a 90 s drain; token trimming is
asymmetric; `maxSurge: 0` with residency placement loses VMs on a rollout;
the demo script rotates the token without rolling pods and restarts hosts
and orchestrator together; no automatic recovery after a node reboot.

Docs: eighteen mismatches, listed in the whole-system report and fixed with
the code.

## Sound

The format bumps reject every other version at their readers; key prefixing
is consistent; `Handle.replace`'s reconciliation and the writer-state split;
`Reclaim`'s transitive protection and the grace; the tombstone and delete
orderings; the NetworkPolicy port rules; the destination refusing to
substitute checkpoint bytes for a page only the source holds; batched retire
and unseal; the post-copy reservation retry; the host-wide dirty wait;
logical-page admission; write-ahead arithmetic; fork-point page sharing;
the production pager config; the wire, opcodes and seccomp filter across all
three implementations; the x86 gap arithmetic; `PageSource.Release` refusing
with pages outstanding; `Done` gating on the unpublished set; contested
claims refused; symmetric jitter; `AdmitRegions` before every VMM start;
`ForkOut`'s rollback; the guest agent's limits; constant-time token checks.
