----------------------------- MODULE Writeback -----------------------------
(***************************************************************************)
(* A page's dirty life across a checkpoint, as the Zircon port keeps it    *)
(* (plans/zircon-pager-port-2026-10-05.md, departures D1 to D5) and as     *)
(* vmmemory/checkpoint.go, fault.go and bindings.go do it today.           *)
(*                                                                         *)
(* A few pages of one region. Each page has the guest's binding and, while *)
(* a checkpoint holds it, the checkpoint's copy. In Zircon's words the     *)
(* guest's page is Clean, Dirty, or AwaitingClean while it shares the      *)
(* checkpoint's copy; the copy is AwaitingClean until the retire or the    *)
(* abandon ends it. The guest's page is:                                   *)
(*                                                                         *)
(*  - "vol":    clean and not resident; it reads what the volume holds;    *)
(*  - "shared": clean and resident under the volume's identity;            *)
(*  - "frame":  dirty, a private resident page with its own reservation;   *)
(*  - "spill":  dirty, spilled to its own reservation's slot (D2);         *)
(*  - "ck":     sharing the checkpoint's copy, with no reservation.        *)
(*                                                                         *)
(* The copy is "none", "frame" or "spill", and owns its own reservation.   *)
(* A reservation is a slot of the spill file, and Slots is the dirty       *)
(* budget. A store takes its slot before the page is dirty (D5).           *)
(*                                                                         *)
(* The guest's bytes are a count of its stores to the page, so a store     *)
(* that copied stale bytes shows as a wrong count.                         *)
(*                                                                         *)
(* The parties:                                                            *)
(*                                                                         *)
(*  - the guest, which stores in place into a page it maps writable;       *)
(*  - a store fault, which copies the page it traps on, gives the region   *)
(*    up for the reclaim its new page needs, and decides again after it    *)
(*    (fault.go). A store into a page the checkpoint holds gets a copy of  *)
(*    its own, and the checkpoint keeps the page (D1). A store trap that   *)
(*    stores nothing copies too;                                           *)
(*  - a refault of a spilled page, which reads the slot, gives the region  *)
(*    up for its reclaim, and decides again after it (loadOnce);           *)
(*  - a fault on the guest's own resident page that it does not map, as an *)
(*    abandon leaves it: it looks at the page under its lock, lets the     *)
(*    lock go, and looks the page up. A lookup that finds the page spilled *)
(*    meanwhile decides again, and refaults it (errOwnPageSpilled);        *)
(*  - eviction, in two halves under the page's lock alone: it takes the    *)
(*    page, then writes it to the slot its owner's reservation names, or   *)
(*    drops a clean page;                                                  *)
(*  - the pause, which write-protects the dirty set and takes it whole     *)
(*    (D3);                                                                *)
(*  - the walk behind the pause, which holds the region and moves each     *)
(*    page into the checkpoint. A page a reclaim holds is joined to the    *)
(*    reclaim, which writes it to the reservation the copy takes over      *)
(*    (takePages, joinReclaiming);                                         *)
(*  - the settle, which drops a copy whose bytes are the volume's;         *)
(*  - readers of the checkpoint: its upload, and a fork point's children,  *)
(*    which map its pages for as long as the fork holds the seal. A fork   *)
(*    point is not settled;                                                *)
(*  - the publication, which lands what the upload read, or fails;         *)
(*  - the retire of a landed checkpoint, a page at a time, which makes a   *)
(*    page the guest still shares clean; and the abandon of a failed one,  *)
(*    which gives each such page back as dirty with the copy's reservation *)
(*    (D4).                                                                *)
(*                                                                         *)
(* The walk holds the region exclusively, so no fault decides while it     *)
(* runs. A retire or abandon step is one batch under the region. Every     *)
(* fault holds its page's window from start to end, so one fault per page  *)
(* at a time.                                                              *)
(*                                                                         *)
(* Left out: the mapping protocol and replacement, which order commands    *)
(* and change no bytes; cold copies and the give-back, which the settle    *)
(* stands for; write-ahead and zero pages, which start dirty like a store; *)
(* the sharing index across regions and the isolated arena (spec/arena);   *)
(* the loss window, pressure and waiting stores, which decide when, not    *)
(* what; a failed seal, which takes nothing; detach.                       *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
    Pages,      \* the pages of the region
    Slots,      \* the spill file's slots: the dirty budget
    None,       \* no slot, and an empty slot's bytes
    MaxStores,  \* stores and store traps across a run
    MaxSeals,   \* checkpoints across a run
    Forks,      \* whether a seal may be a fork point's
    Bugs        \* defects to put back, to show the invariants catch each one

ASSUME None \notin Slots

Values == 0..MaxStores

VARIABLES
    guest,      \* the bytes the guest last stored: what it must read
    vol,        \* the bytes the volume holds
    gloc,       \* where the guest's page is, see above
    gbytes,     \* the bytes of the guest's private resident page
    gslot,      \* the reservation the guest's page owns, or None
    cloc,       \* where the checkpoint's copy is: "none", "frame" or "spill"
    cbytes,     \* the bytes of the copy's resident page
    cslot,      \* the reservation the copy owns, or None
    slotBytes,  \* what each slot of the spill file holds, or None
    busy,       \* a reclaim holds the guest's page ("guest") or the copy ("copy")
    dset,       \* the dirty set the next pause takes
    pending,    \* the pages the pause took that the walk has not moved
    pause,      \* each page's bytes at the last pause
    reads,      \* <<page, bytes>> read from the checkpoint since the pause
    phase,      \* the checkpoint, see Phases
    gen,        \* seals taken; it names the checkpoint's copy
    forked,     \* the seal is a fork point's
    fstate,     \* the fault on each page: "idle", "copying", "refaulting"
                \* or "looking"
    fslot,      \* the reservation a store fault took
    fdata,      \* the bytes the fault read before it gave the region up
    fheld,      \* what the fault decided against: <<dirty, copy>>
    fstore,     \* whether the store fault stores, or is a trap that does not
    stores

vars == <<guest, vol, gloc, gbytes, gslot, cloc, cbytes, cslot, slotBytes,
          busy, dset, pending, pause, reads, phase, gen, forked,
          fstate, fslot, fdata, fheld, fstore, stores>>
guestVars == <<guest, gloc, gbytes, gslot, dset>>
copyVars == <<cloc, cbytes, cslot>>
faultVars == <<fstate, fslot, fdata, fheld, fstore>>
sealVars == <<pending, pause, reads, phase, gen, forked>>

Phases == {"none", "walking", "settling", "reading", "landed", "abandoning"}
Dirty == {"frame", "spill"}

Init ==
    /\ guest = [p \in Pages |-> 0]
    /\ vol = [p \in Pages |-> 0]
    /\ gloc = [p \in Pages |-> "vol"]
    /\ gbytes = [p \in Pages |-> 0]
    /\ gslot = [p \in Pages |-> None]
    /\ cloc = [p \in Pages |-> "none"]
    /\ cbytes = [p \in Pages |-> 0]
    /\ cslot = [p \in Pages |-> None]
    /\ slotBytes = [s \in Slots |-> None]
    /\ busy = [p \in Pages |-> "none"]
    /\ dset = {}
    /\ pending = {}
    /\ pause = [p \in Pages |-> 0]
    /\ reads = {}
    /\ phase = "none"
    /\ gen = 0
    /\ forked = FALSE
    /\ fstate = [p \in Pages |-> "idle"]
    /\ fslot = [p \in Pages |-> None]
    /\ fdata = [p \in Pages |-> 0]
    /\ fheld = [p \in Pages |-> <<FALSE, 0>>]
    /\ fstore = [p \in Pages |-> FALSE]
    /\ stores = 0

(***************************************************************************)
(* What things read.                                                       *)
(***************************************************************************)
CopyBytes(p) == IF cloc[p] = "frame" THEN cbytes[p] ELSE slotBytes[cslot[p]]

\* What the guest reads of a page.
GuestBytes(p) ==
    CASE gloc[p] \in {"vol", "shared"} -> vol[p]
      [] gloc[p] = "frame" -> gbytes[p]
      [] gloc[p] = "spill" -> slotBytes[gslot[p]]
      [] gloc[p] = "ck" -> CopyBytes(p)

\* The checkpoint's copy the guest shares, by the seal that made it, or 0:
\* the binding's checkpoint pointer, which checkpointCopy reads.
CopyOf(p) == IF gloc[p] = "ck" THEN gen ELSE 0

\* The binding's dirty flag and checkpoint pointer, which privateEpoch reads.
Epoch(p) == <<gloc[p] \in Dirty \cup {"ck"}, CopyOf(p)>>

Free == Slots \ ({gslot[p] : p \in Pages} \cup {cslot[p] : p \in Pages}
                 \cup {fslot[p] : p \in Pages})

\* A fault may hold the region shared: the walk is not holding it.
Region == phase # "walking"

Idle(p) == fstate[p] = "idle"

\* A fault that has finished, holding nothing.
FaultEnds(p) ==
    /\ fstate' = [fstate EXCEPT ![p] = "idle"]
    /\ fslot' = [fslot EXCEPT ![p] = None]
    /\ fdata' = [fdata EXCEPT ![p] = 0]
    /\ fheld' = [fheld EXCEPT ![p] = <<FALSE, 0>>]
    /\ fstore' = [fstore EXCEPT ![p] = FALSE]

(***************************************************************************)
(* The guest and its faults.                                               *)
(***************************************************************************)
\* A store into a page the guest maps writable: its own dirty page, not
\* write-protected by a pause, and not being taken by a reclaim, which
\* revokes the mapping before it reads the page.
StoreInPlace(p) ==
    /\ stores < MaxStores
    /\ Idle(p)
    /\ gloc[p] = "frame" /\ gslot[p] # None
    /\ p \notin pending
    /\ busy[p] = "none"
    /\ gbytes' = [gbytes EXCEPT ![p] = @ + 1]
    /\ guest' = [guest EXCEPT ![p] = @ + 1]
    /\ stores' = stores + 1
    /\ UNCHANGED <<vol, gloc, gslot, copyVars, slotBytes, busy, dset,
                   sealVars, faultVars>>

\* A store fault into a clean page or one the checkpoint holds. It takes its
\* reservation first (D5), reads the bytes to copy under the page's lock,
\* and gives the region up for the reclaim its new page needs.
CopyBegin(p, store) ==
    /\ stores < MaxStores
    /\ Idle(p) /\ Region
    /\ gloc[p] \in {"vol", "shared", "ck"}
    /\ busy[p] = "none"
    \* Zircon makes a resident AwaitingClean page Dirty in place instead.
    /\ ~("zircon-in-place" \in Bugs /\ gloc[p] = "ck" /\ cloc[p] = "frame")
    /\ \E s \in Free : fslot' = [fslot EXCEPT ![p] = s]
    /\ fstate' = [fstate EXCEPT ![p] = "copying"]
    /\ fdata' = [fdata EXCEPT ![p] = GuestBytes(p)]
    /\ fheld' = [fheld EXCEPT ![p] = Epoch(p)]
    /\ fstore' = [fstore EXCEPT ![p] = store]
    /\ stores' = stores + 1
    /\ UNCHANGED <<guestVars, vol, copyVars, slotBytes, busy, sealVars>>

\* Back under the region with its new page. If the checkpoint's copy it
\* decided against has changed, a seal ended while it reclaimed: it gives
\* the page and the reservation back, and the guest faults again. Otherwise
\* the guest's page becomes its own dirty copy, and the checkpoint keeps its
\* copy (D1).
CopyEnd(p) ==
    /\ fstate[p] = "copying" /\ Region
    /\ busy[p] = "none"
    /\ FaultEnds(p)
    /\ IF CopyOf(p) # fheld[p][2]
       THEN UNCHANGED guestVars
       ELSE LET bytes == fdata[p] + (IF fstore[p] THEN 1 ELSE 0) IN
            /\ gloc' = [gloc EXCEPT ![p] = "frame"]
            /\ gbytes' = [gbytes EXCEPT ![p] = bytes]
            /\ gslot' = [gslot EXCEPT ![p] = fslot[p]]
            /\ dset' = dset \cup {p}
            /\ guest' = [guest EXCEPT ![p] = IF fstore[p] THEN @ + 1 ELSE @]
    /\ UNCHANGED <<vol, copyVars, slotBytes, busy, sealVars, stores>>

\* Zircon's rule, put back: a store into an AwaitingClean page makes it
\* Dirty in place, in the page the checkpoint holds.
ZirconStore(p) ==
    /\ "zircon-in-place" \in Bugs
    /\ stores < MaxStores
    /\ Idle(p) /\ Region
    /\ gloc[p] = "ck" /\ cloc[p] = "frame" /\ busy[p] = "none"
    /\ cbytes' = [cbytes EXCEPT ![p] = @ + 1]
    /\ guest' = [guest EXCEPT ![p] = @ + 1]
    /\ dset' = dset \cup {p}
    /\ stores' = stores + 1
    /\ UNCHANGED <<vol, gloc, gbytes, gslot, cloc, cslot, slotBytes, busy,
                   sealVars, faultVars>>

\* A fault on a spilled page, the guest's own or the copy it shares. It
\* reads the slot, and gives the region up for the reclaim its page needs.
RefaultBegin(p) ==
    /\ Idle(p) /\ Region
    /\ \/ gloc[p] = "spill"
       \/ gloc[p] = "ck" /\ cloc[p] = "spill"
    /\ fstate' = [fstate EXCEPT ![p] = "refaulting"]
    /\ fdata' = [fdata EXCEPT ![p] = GuestBytes(p)]
    /\ fheld' = [fheld EXCEPT ![p] = Epoch(p)]
    /\ UNCHANGED <<guestVars, vol, copyVars, slotBytes, busy, sealVars,
                   fslot, fstore, stores>>

\* Back under the region with its new page. If the page's dirty flag or its
\* checkpoint changed, a seal or a retire ran while it reclaimed, and it
\* decides again from the top. Before 2026-09-22 it did not look, and a
\* retire left it binding a private page to a clean binding.
RefaultEnd(p) ==
    /\ fstate[p] = "refaulting" /\ Region
    /\ FaultEnds(p)
    /\ IF Epoch(p) # fheld[p] /\ "refault-ignores-retire" \notin Bugs
       THEN UNCHANGED <<gloc, gbytes, copyVars, slotBytes>>
       ELSE CASE gloc[p] = "ck" /\ cloc[p] = "spill" ->
                   /\ cloc' = [cloc EXCEPT ![p] = "frame"]
                   /\ cbytes' = [cbytes EXCEPT ![p] = fdata[p]]
                   /\ slotBytes' = [slotBytes EXCEPT ![cslot[p]] = None]
                   /\ UNCHANGED <<gloc, gbytes, cslot>>
              [] gloc[p] = "spill" ->
                   /\ gloc' = [gloc EXCEPT ![p] = "frame"]
                   /\ gbytes' = [gbytes EXCEPT ![p] = fdata[p]]
                   /\ slotBytes' = [slotBytes EXCEPT ![gslot[p]] = None]
                   /\ UNCHANGED copyVars
              [] OTHER ->
                   \* Only the mutant gets here: the page is clean now, and
                   \* gets a private page no reservation covers.
                   /\ gloc' = [gloc EXCEPT ![p] = "frame"]
                   /\ gbytes' = [gbytes EXCEPT ![p] = fdata[p]]
                   /\ UNCHANGED <<copyVars, slotBytes>>
    /\ UNCHANGED <<guest, vol, gslot, dset, busy, sealVars, stores>>

\* The guest's own page, resident: its dirty page, or the copy it shares.
OwnResident(p) == gloc[p] = "frame" \/ (gloc[p] = "ck" /\ cloc[p] = "frame")

\* A fault on its own resident page, which the guest does not map: it looks
\* at the page under its lock, finds nothing to complete, and lets the lock
\* go before its lookup (loadOnce).
LookBegin(p) ==
    /\ Idle(p) /\ Region
    /\ OwnResident(p) /\ busy[p] = "none"
    /\ fstate' = [fstate EXCEPT ![p] = "looking"]
    /\ UNCHANGED <<guestVars, vol, copyVars, slotBytes, busy, sealVars,
                   fslot, fdata, fheld, fstore, stores>>

\* The lookup, under the layer's lock, which an eviction holds the page
\* across. It finds the page resident, and maps it; or an eviction spilled
\* it meanwhile, and the fault decides again from the top, which refaults
\* it. Before 2026-10-08 the lookup went on past the layer, and bound the
\* volume's page over the guest's own.
LookEnd(p) ==
    /\ fstate[p] = "looking" /\ Region /\ busy[p] = "none"
    /\ FaultEnds(p)
    /\ IF OwnResident(p) \/ "lookup-past-the-layer" \notin Bugs
       THEN UNCHANGED gloc
       ELSE gloc' = [gloc EXCEPT ![p] = "shared"]
    /\ UNCHANGED <<guest, vol, gbytes, gslot, dset, copyVars, slotBytes, busy,
                   sealVars, stores>>

(***************************************************************************)
(* Eviction, under the page's lock alone, at any time.                     *)
(***************************************************************************)
EvictBegin(p, who) ==
    /\ busy[p] = "none"
    /\ \/ who = "guest" /\ gloc[p] = "frame"
       \/ who = "copy" /\ cloc[p] = "frame"
    /\ busy' = [busy EXCEPT ![p] = who]
    /\ UNCHANGED <<guestVars, vol, copyVars, slotBytes, sealVars, faultVars,
                   stores>>

\* The bytes go to the slot of whichever reservation owns the page now: the
\* walk may have handed it to the checkpoint's copy meanwhile (D2). A page
\* that owns none is dropped, with nowhere to put its bytes.
EvictEnd(p) ==
    /\ busy[p] # "none"
    /\ busy' = [busy EXCEPT ![p] = "none"]
    /\ IF busy[p] = "copy"
       THEN /\ cloc' = [cloc EXCEPT ![p] = "spill"]
            /\ cbytes' = [cbytes EXCEPT ![p] = 0]
            /\ slotBytes' = [slotBytes EXCEPT ![cslot[p]] = cbytes[p]]
            /\ UNCHANGED <<gloc, gbytes>>
       ELSE /\ gloc' = [gloc EXCEPT ![p] = IF gslot[p] # None THEN "spill" ELSE "vol"]
            /\ gbytes' = [gbytes EXCEPT ![p] = 0]
            /\ slotBytes' = IF gslot[p] # None
                            THEN [slotBytes EXCEPT ![gslot[p]] = gbytes[p]]
                            ELSE slotBytes
            /\ UNCHANGED <<cloc, cbytes>>
    /\ UNCHANGED <<guest, vol, gslot, dset, cslot, sealVars, faultVars, stores>>

\* A clean page is dropped and read again from the volume.
Drop(p) ==
    /\ gloc[p] = "shared"
    /\ gloc' = [gloc EXCEPT ![p] = "vol"]
    /\ UNCHANGED <<guest, vol, gbytes, gslot, dset, copyVars, slotBytes, busy,
                   sealVars, faultVars, stores>>

(***************************************************************************)
(* The checkpoint.                                                         *)
(***************************************************************************)
\* The pause write-protects the dirty set and takes it whole, marking
\* nothing (D3). A fork point's pause holds the seal for its children.
Pause(fork) ==
    /\ phase = "none" /\ gen < MaxSeals
    /\ pending' = dset
    /\ dset' = {}
    /\ pause' = [p \in Pages |-> IF p \in dset THEN guest[p] ELSE 0]
    /\ reads' = {}
    /\ phase' = "walking"
    /\ gen' = gen + 1
    /\ forked' = fork
    /\ UNCHANGED <<guest, vol, gloc, gbytes, gslot, copyVars, slotBytes, busy,
                   faultVars, stores>>

\* The walk moves one page into the checkpoint: the copy takes the guest's
\* page and its reservation, and the guest shares the copy, AwaitingClean.
\* A page a reclaim holds is joined to that reclaim, which then writes it to
\* the reservation the copy has taken over.
Walk(p) ==
    /\ phase = "walking" /\ p \in pending
    /\ pending' = pending \ {p}
    /\ IF busy[p] = "guest" /\ "seal-skips-reclaim" \in Bugs
       THEN UNCHANGED <<gloc, gbytes, gslot, copyVars, busy>>
       ELSE /\ cloc' = [cloc EXCEPT ![p] = gloc[p]]
            /\ cbytes' = [cbytes EXCEPT ![p] = gbytes[p]]
            /\ cslot' = [cslot EXCEPT ![p] = gslot[p]]
            /\ gloc' = [gloc EXCEPT ![p] = "ck"]
            /\ gbytes' = [gbytes EXCEPT ![p] = 0]
            /\ gslot' = [gslot EXCEPT ![p] = None]
            /\ busy' = [busy EXCEPT ![p] = IF @ = "guest" THEN "copy" ELSE @]
    /\ UNCHANGED <<guest, vol, dset, slotBytes, pause, reads, phase, gen,
                   forked, faultVars, stores>>

\* The walk gives the region back. A fork point is not settled.
WalkDone ==
    /\ phase = "walking" /\ pending = {}
    /\ phase' = IF forked THEN "reading" ELSE "settling"
    /\ UNCHANGED <<guestVars, vol, copyVars, slotBytes, busy, pending, pause,
                   reads, gen, forked, faultVars, stores>>

\* A resident copy whose bytes are the volume's leaves the checkpoint, and a
\* guest that still shares it takes the volume's page and is clean.
Settle(p) ==
    /\ phase = "settling"
    /\ cloc[p] = "frame" /\ busy[p] = "none"
    /\ cbytes[p] = vol[p]
    /\ gloc' = [gloc EXCEPT ![p] = IF @ = "ck" THEN "shared" ELSE @]
    /\ cloc' = [cloc EXCEPT ![p] = "none"]
    /\ cbytes' = [cbytes EXCEPT ![p] = 0]
    /\ cslot' = [cslot EXCEPT ![p] = None]
    /\ UNCHANGED <<guest, vol, gbytes, gslot, dset, slotBytes, busy,
                   sealVars, faultVars, stores>>

SettleDone ==
    /\ phase = "settling"
    /\ phase' = "reading"
    /\ UNCHANGED <<guestVars, vol, copyVars, slotBytes, busy, pending, pause,
                   reads, gen, forked, faultVars, stores>>

\* The upload reads a copy, under its page's lock; so does a fork point's
\* child, for as long as the fork holds the seal.
Read(p) ==
    /\ \/ phase = "reading"
       \/ forked /\ phase = "landed"
    /\ cloc[p] # "none" /\ busy[p] # "copy"
    /\ reads' = reads \cup {<<p, CopyBytes(p)>>}
    /\ UNCHANGED <<guestVars, vol, copyVars, slotBytes, busy, pending, pause,
                   phase, gen, forked, faultVars, stores>>

Read1(p) == CHOOSE v \in {r[2] : r \in {x \in reads : x[1] = p}} : TRUE

\* The publication lands what the upload read of every page.
Land ==
    /\ phase = "reading"
    /\ \A p \in Pages : cloc[p] # "none" => \E r \in reads : r[1] = p
    /\ vol' = [p \in Pages |-> IF cloc[p] # "none" THEN Read1(p) ELSE vol[p]]
    /\ phase' = "landed"
    /\ UNCHANGED <<guestVars, copyVars, slotBytes, busy, pending, pause, reads,
                   gen, forked, faultVars, stores>>

\* The publication fails, or the region is unsealed.
Fail ==
    /\ phase \in {"settling", "reading"}
    /\ phase' = "abandoning"
    /\ UNCHANGED <<guestVars, vol, copyVars, slotBytes, busy, pending, pause,
                   reads, gen, forked, faultVars, stores>>

\* Retire, a page at a time: a page the guest still shares is clean under
\* the volume's identity; the copy and its reservation go.
Retire(p) ==
    /\ phase = "landed"
    /\ cloc[p] # "none" /\ busy[p] # "copy"
    /\ gloc' = [gloc EXCEPT ![p] =
                  IF @ = "ck" THEN (IF cloc[p] = "frame" THEN "shared" ELSE "vol")
                  ELSE @]
    /\ cloc' = [cloc EXCEPT ![p] = "none"]
    /\ cbytes' = [cbytes EXCEPT ![p] = 0]
    /\ cslot' = [cslot EXCEPT ![p] = None]
    /\ slotBytes' = [slotBytes EXCEPT ![cslot[p]] = None]
    /\ UNCHANGED <<guest, vol, gbytes, gslot, dset, busy, sealVars, faultVars,
                   stores>>

\* Abandon, a page at a time (D4): a page the guest still shares is dirty
\* again with the copy's page and reservation; otherwise the copy goes.
Abandon(p) ==
    /\ phase = "abandoning"
    /\ cloc[p] # "none" /\ busy[p] # "copy"
    /\ IF gloc[p] = "ck"
       THEN /\ gloc' = [gloc EXCEPT ![p] = cloc[p]]
            /\ gbytes' = [gbytes EXCEPT ![p] = cbytes[p]]
            /\ gslot' = [gslot EXCEPT ![p] = cslot[p]]
            /\ dset' = dset \cup {p}
            /\ UNCHANGED slotBytes
       ELSE /\ slotBytes' = [slotBytes EXCEPT ![cslot[p]] = None]
            /\ UNCHANGED <<gloc, gbytes, gslot, dset>>
    /\ cloc' = [cloc EXCEPT ![p] = "none"]
    /\ cbytes' = [cbytes EXCEPT ![p] = 0]
    /\ cslot' = [cslot EXCEPT ![p] = None]
    /\ UNCHANGED <<guest, vol, busy, sealVars, faultVars, stores>>

\* The seal ends once every copy is gone.
End ==
    /\ phase \in {"landed", "abandoning"}
    /\ \A p \in Pages : cloc[p] = "none"
    /\ phase' = "none"
    /\ forked' = FALSE
    /\ pause' = [p \in Pages |-> 0]
    /\ reads' = {}
    /\ UNCHANGED <<guestVars, vol, copyVars, slotBytes, busy, pending, gen,
                   faultVars, stores>>

Next ==
    \/ \E p \in Pages :
          \/ StoreInPlace(p) \/ CopyEnd(p) \/ ZirconStore(p)
          \/ \E store \in BOOLEAN : CopyBegin(p, store)
          \/ RefaultBegin(p) \/ RefaultEnd(p) \/ LookBegin(p) \/ LookEnd(p)
          \/ \E who \in {"guest", "copy"} : EvictBegin(p, who)
          \/ EvictEnd(p) \/ Drop(p)
          \/ Walk(p) \/ Settle(p) \/ Read(p) \/ Retire(p) \/ Abandon(p)
    \/ \E fork \in IF Forks THEN BOOLEAN ELSE {FALSE} : Pause(fork)
    \/ WalkDone \/ SettleDone \/ Land \/ Fail \/ End

Spec == Init /\ [][Next]_vars

Symmetry == Permutations(Pages) \cup Permutations(Slots)

(***************************************************************************)
(* Invariants.                                                             *)
(***************************************************************************)
TypeOK ==
    /\ gloc \in [Pages -> {"vol", "shared", "frame", "spill", "ck"}]
    /\ cloc \in [Pages -> {"none", "frame", "spill"}]
    /\ busy \in [Pages -> {"none", "guest", "copy"}]
    /\ fstate \in [Pages -> {"idle", "copying", "refaulting", "looking"}]
    /\ phase \in Phases
    /\ dset \subseteq Pages /\ pending \subseteq Pages

\* Whatever reads the checkpoint, its upload or a fork point's child, reads
\* the bytes the page held at the pause.
SealedBytes == \A r \in reads : r[2] = pause[r[1]]

\* The guest reads what it last stored, and that write is published, or in
\* the dirty set the next pause takes, or in the set this one took, or in
\* the checkpoint's copy the guest still shares.
NoLostWrite ==
    \A p \in Pages :
        /\ GuestBytes(p) = guest[p]
        /\ \/ vol[p] = guest[p]
           \/ p \in dset \cup pending
           \/ gloc[p] = "ck"

\* Every private page the guest may store into, resident or spilled, owns a
\* reservation; so does every copy a checkpoint holds; a page that shares
\* the copy owns none of its own; and a store fault holds the one it took.
Reserved ==
    \A p \in Pages :
        /\ (gloc[p] \in Dirty) = (gslot[p] # None)
        /\ (cloc[p] # "none") = (cslot[p] # None)
        /\ gloc[p] = "ck" => cloc[p] # "none"
        /\ (fstate[p] = "copying") = (fslot[p] # None)

\* No reservation is held twice, so the reservations taken never exceed the
\* dirty budget, which is the slots of the spill file.
Owners(s) ==
    {<<"guest", p>> : p \in {q \in Pages : gslot[q] = s}}
    \cup {<<"copy", p>> : p \in {q \in Pages : cslot[q] = s}}
    \cup {<<"fault", p>> : p \in {q \in Pages : fslot[q] = s}}

Budget == \A s \in Slots : Cardinality(Owners(s)) <= 1
=============================================================================
