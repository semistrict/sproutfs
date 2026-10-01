----------------------------- MODULE Ownership -----------------------------
(***************************************************************************)
(* How one VM changes owner, as docs/metadata.md and docs/migration.md     *)
(* describe it and control/client.go, volume/publish.go, volume/kept.go,   *)
(* checkpoint/reclaim.go and vmmigrate implement it.                       *)
(*                                                                         *)
(* The store holds the VM's control record and its checkpoints. Every      *)
(* write to the record is conditional on the version its writer read, and  *)
(* any reply may be lost: the write may have landed or not, and the read   *)
(* that would settle it may fail too. The parties are:                     *)
(*                                                                         *)
(*  - handles, one per epoch. The creator holds the first. Each opener     *)
(*    takes the next epoch, which fences the handle before it. A handle    *)
(*    publishes checkpoints, selects them (keeping some), pins fork        *)
(*    points, reclaims what its last selection replaced, confirms its      *)
(*    epoch, hands the VM off to a migration destination, or closes;       *)
(*  - pinners, which pin a checkpoint without the epoch (Client.Pin);      *)
(*  - a releaser, which gives up a kept checkpoint without the epoch and   *)
(*    sweeps what only it held (Manager.Release);                          *)
(*  - a deleter, which removes the record once no writer holds it and      *)
(*    deletes every checkpoint no pin protects (Manager.Delete).           *)
(*                                                                         *)
(* A checkpoint is a sequence <<epoch, counter>>. It names itself and      *)
(* some of the older checkpoints the guest's state reads. A capture never  *)
(* reads one the capture before it did not: what the guest wrote since is *)
(* in this capture, and a capture whose selection failed gives its pages   *)
(* back to the guest as dirty. A checkpoint is stored whole or deleted     *)
(* whole; deleting its index object first is what makes that so for a     *)
(* reader.                                                                 *)
(*                                                                         *)
(* Left out: more than one VM, so the child a fork starts; the pager and   *)
(* the post-copy, which docs/migration.md argues and the simulation tests; *)
(* creation, which starts the model; and the orchestrator, which decides   *)
(* when an open, a migration or a delete may happen.                       *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
    Creator,    \* the handle that created the VM
    Nobody,     \* the nonce of no record
    Openers,    \* handles that may take the VM over, each once
    Pinners,    \* no-epoch pins, one attempt each
    MaxCkpts,   \* checkpoints each handle may allocate
    MaxFaults,  \* lost replies and failed reads across a whole run
    Releasing,  \* whether a releaser runs
    Deleting,   \* whether a deleter runs
    Migrating,  \* whether a handle may hand the VM off
    Compaction, \* "whole": a checkpoint names all its base named, or rewrites
                \* every page itself; "any": it names any subset of them
    Bugs        \* defects to put back, to show the invariants catch each one

Handles == {Creator} \cup Openers

None == <<0, 0>>
Epoch(s) == s[1]
Before(a, b) == a[1] < b[1] \/ (a[1] = b[1] /\ a[2] < b[2])

NoRecord == [exists |-> FALSE, epoch |-> 0, nonce |-> Nobody,
             selected |-> None, created |-> FALSE, pins |-> {}, kept |-> {}]

Protected(r) == r.pins \cup r.kept

\* The checkpoint a sweep deletes next. Sweeps delete in sequence order.
Next1(S) == CHOOSE d \in S : \A e \in S : d = e \/ Before(d, e)

\* States in which a handle believes it holds the epoch it took.
Holding == {"running", "upd_begin", "upd_put", "upd_readback"}

VARIABLES
    \* The store.
    rec,        \* the control record, or NoRecord
    ver,        \* the record's version tag: every landed write changes it
    names,      \* committed checkpoint -> the checkpoints its index names
    stored,     \* the checkpoints whose objects are in the store
    \* History, which the invariants read and no party does.
    guestOf,    \* committed checkpoint -> the guest it captured
    gstart,     \* guest -> the checkpoint it started from
    everPinned, \* every checkpoint any record ever pinned
    collision,  \* a checkpoint was committed twice
    mixed,      \* a destination ran a guest over another guest's checkpoint
    faults,     \* lost replies and failed reads so far
    handoff,    \* the migration in flight, if any
    \* Handles.
    hpc, hmode, hepoch, hrec, hver, hnext, hop, howned, hbase, hcount,
    hdead, hguest, hview,
    \* Pinners.
    ppc, pwanted, pver, prec, pnext,
    \* The releaser.
    rpc, rseq, rver, rrec, rnext, rdead,
    \* The deleter.
    dpc, dver, drec, ddead

storeVars == <<rec, ver, names, stored>>
ghostVars == <<guestOf, gstart, everPinned, collision, mixed, faults, handoff>>
handleVars == <<hpc, hmode, hepoch, hrec, hver, hnext, hop, howned, hbase,
                hcount, hdead, hguest, hview>>
pinVars == <<ppc, pwanted, pver, prec, pnext>>
relVars == <<rpc, rseq, rver, rrec, rnext, rdead>>
delVars == <<dpc, dver, drec, ddead>>
vars == <<storeVars, ghostVars, handleVars, pinVars, relVars, delVars>>

First == <<1, 1>>
NoOp == [kind |-> "none", seq |-> None, keep |-> FALSE, then |-> "none"]

Init ==
    /\ rec = [exists |-> TRUE, epoch |-> 1, nonce |-> Creator,
              selected |-> First, created |-> TRUE, pins |-> {}, kept |-> {}]
    /\ ver = 1
    /\ names = (First :> {First})
    /\ stored = {First}
    /\ guestOf = (First :> Creator)
    /\ gstart = [g \in Handles |-> None]
    /\ everPinned = {}
    /\ collision = FALSE
    /\ mixed = FALSE
    /\ faults = 0
    /\ handoff = [active |-> FALSE, seq |-> None, guest |-> Creator, taken |-> FALSE]
    /\ hpc = [h \in Handles |-> IF h = Creator THEN "running" ELSE "idle"]
    /\ hmode = [h \in Handles |-> "recover"]
    /\ hepoch = [h \in Handles |-> IF h = Creator THEN 1 ELSE 0]
    /\ hrec = [h \in Handles |-> IF h = Creator THEN rec ELSE NoRecord]
    /\ hver = [h \in Handles |-> IF h = Creator THEN 1 ELSE 0]
    /\ hnext = [h \in Handles |-> NoRecord]
    /\ hop = [h \in Handles |-> NoOp]
    /\ howned = [h \in Handles |-> IF h = Creator THEN First ELSE None]
    /\ hbase = [h \in Handles |-> IF h = Creator THEN First ELSE None]
    /\ hcount = [h \in Handles |-> IF h = Creator THEN 1 ELSE 0]
    /\ hdead = [h \in Handles |-> {}]
    /\ hguest = [h \in Handles |-> h]
    /\ hview = [h \in Handles |-> IF h = Creator THEN {First} ELSE {}]
    /\ ppc = [p \in Pinners |-> "idle"]
    /\ pwanted = [p \in Pinners |-> None]
    /\ pver = [p \in Pinners |-> 0]
    /\ prec = [p \in Pinners |-> NoRecord]
    /\ pnext = [p \in Pinners |-> NoRecord]
    /\ rpc = IF Releasing THEN "idle" ELSE "done"
    /\ rseq = None
    /\ rver = 0
    /\ rrec = NoRecord
    /\ rnext = NoRecord
    /\ rdead = {}
    /\ dpc = IF Deleting THEN "idle" ELSE "done"
    /\ dver = 0
    /\ drec = NoRecord
    /\ ddead = {}

(***************************************************************************)
(* The store's two operations on the record. A write lands only when the   *)
(* record is still at the version its writer read.                         *)
(***************************************************************************)
Land(r) ==
    /\ rec' = r
    /\ ver' = ver + 1
    /\ everPinned' = everPinned \cup r.pins

Fault == faults < MaxFaults /\ faults' = faults + 1

\* Everything one checkpoint keeps alive, if every index involved is readable.
\* An index a sweep cannot read stops the sweep, which then deletes nothing.
Readable(P) == P \subseteq stored
Closure(P) == UNION {names[p] : p \in P}

\* What a reclamation deletes: the replaced checkpoint and what it named, less
\* what the current one names and what the record protects.
Dead(replaced, current, r) ==
    LET spared == IF "sweep-forgets-kept" \in Bugs THEN r.pins ELSE Protected(r)
        live == IF "sweep-takes-live" \in Bugs THEN {current} ELSE names[current]
    IN (({replaced} \cup names[replaced]) \ live) \ Closure(spared)

(***************************************************************************)
(* A handle that holds its epoch.                                          *)
(***************************************************************************)
Fresh(h) == <<hepoch[h], hcount[h] + 1>>

\* Commit a checkpoint's objects: create-if-absent, so a sequence committed
\* twice is a collision. It names itself and some of what the guest's state
\* reads (hview): a capture reads no older checkpoint the capture before it
\* did not, because whatever the guest wrote since is in this one.
Commit(h, s) ==
    \E kept \in IF Compaction = "any" THEN SUBSET hview[h]
                ELSE {hview[h], {}} :
        /\ collision' = (collision \/ s \in DOMAIN names)
        /\ names' = (s :> ({s} \cup kept)) @@ names
        /\ guestOf' = (s :> hguest[h]) @@ guestOf
        /\ stored' = stored \cup {s}

Begin(h, op) ==
    /\ hop' = [hop EXCEPT ![h] = op]
    /\ hpc' = [hpc EXCEPT ![h] = "upd_begin"]

Publish(h) ==
    /\ hpc[h] = "running"
    /\ hcount[h] < MaxCkpts
    /\ LET s == Fresh(h) IN
       \E keep \in BOOLEAN :
          /\ Commit(h, s)
          /\ hcount' = [hcount EXCEPT ![h] = @ + 1]
          /\ Begin(h, [kind |-> "select", seq |-> s, keep |-> keep, then |-> "none"])
    /\ UNCHANGED <<rec, ver, gstart, everPinned, mixed, faults, handoff,
                   hmode, hepoch, hrec, hver, hnext, howned, hbase, hdead,
                   hguest, hview, pinVars, relVars, delVars>>

\* A fork point with pages of its own takes a new sequence, pins it, and
\* publishes it as an ordinary checkpoint behind the children.
ForkNew(h) ==
    /\ hpc[h] = "running"
    /\ hcount[h] < MaxCkpts
    /\ hcount' = [hcount EXCEPT ![h] = @ + 1]
    /\ Begin(h, [kind |-> "pin", seq |-> Fresh(h), keep |-> FALSE, then |-> "publish"])
    /\ UNCHANGED <<storeVars, ghostVars, hmode, hepoch, hrec, hver, hnext,
                   howned, hbase, hdead, hguest, hview, pinVars, relVars, delVars>>

\* A fork point with nothing unpublished inherits the checkpoint the handle
\* is on, and pins that. Nothing is unpublished only while the guest reads
\* what that checkpoint names: a capture whose selection failed gave its pages
\* back to the guest as dirty.
ForkBase(h) ==
    /\ hpc[h] = "running"
    /\ hview[h] = names[hbase[h]]
    /\ Begin(h, [kind |-> "pin", seq |-> hbase[h], keep |-> FALSE, then |-> "none"])
    /\ UNCHANGED <<storeVars, ghostVars, hmode, hepoch, hrec, hver, hnext,
                   howned, hbase, hcount, hdead, hguest, hview, pinVars, relVars, delVars>>

\* Handle.update: the change to the record this handle tracks. A selection
\* must be of this handle's epoch, or the one already selected, and must not go
\* backwards.
Admitted(h) ==
    LET op == hop[h] IN
    op.kind = "pin" \/
    ~((Epoch(op.seq) # hepoch[h] /\ op.seq # hrec[h].selected)
      \/ Before(op.seq, hrec[h].selected))

Mutated(h) ==
    LET op == hop[h]
        r == hrec[h]
    IN IF op.kind = "select"
       THEN [r EXCEPT !.selected = op.seq, !.created = TRUE,
                      !.kept = IF op.keep THEN @ \cup {op.seq} ELSE @]
       ELSE [r EXCEPT !.pins = @ \cup {op.seq}]

\* The handle is free for its next operation. One that failed keeps its epoch,
\* and the sequence it burnt stays burnt. A selection that failed, or whose
\* outcome the handle never learnt, gives the pages it sealed back to the
\* guest as dirty: the guest reads what that capture read, less the capture.
Ready(h) ==
    /\ hpc' = [hpc EXCEPT ![h] = "running"]
    /\ hop' = [hop EXCEPT ![h] = NoOp]
    /\ hview' = [hview EXCEPT ![h] =
           IF hop[h].kind = "select" THEN names[hop[h].seq] \ {hop[h].seq} ELSE @]

\* The operation's write is durable: install it, and start what follows.
Succeeded(h, r) ==
    LET op == hop[h] IN
    IF op.kind = "select"
    THEN /\ hpc' = [hpc EXCEPT ![h] = "running"]
         /\ hop' = [hop EXCEPT ![h] = NoOp]
         /\ howned' = [howned EXCEPT ![h] = op.seq]
         /\ hbase' = [hbase EXCEPT ![h] = op.seq]
         /\ hview' = [hview EXCEPT ![h] = names[op.seq]]
         \* vm.reclaim, with the record the selection returned. The first
         \* checkpoint an opened handle publishes replaces nothing it owns.
         /\ hdead' = [hdead EXCEPT ![h] =
                IF howned[h] = None \/ howned[h] = op.seq \/ ~Readable(Protected(r))
                THEN @
                ELSE @ \cup Dead(howned[h], op.seq, r)]
         /\ UNCHANGED <<names, stored, guestOf, collision>>
    ELSE IF op.then = "publish"
    THEN /\ Commit(h, op.seq)
         /\ hpc' = [hpc EXCEPT ![h] = "upd_begin"]
         /\ hop' = [hop EXCEPT ![h] = [kind |-> "select", seq |-> op.seq,
                                       keep |-> FALSE, then |-> "none"]]
         /\ UNCHANGED <<howned, hbase, hdead, hview>>
    ELSE /\ Ready(h)
         /\ UNCHANGED <<howned, hbase, hdead, names, stored, guestOf, collision>>

UpdBegin(h) ==
    /\ hpc[h] = "upd_begin"
    /\ LET next == Mutated(h) IN
       IF ~Admitted(h)
       THEN /\ Ready(h)
            /\ UNCHANGED <<hnext, howned, hbase, hdead, names, stored, guestOf, collision>>
       ELSE IF next = hrec[h]
       THEN /\ Succeeded(h, hrec[h])
            /\ UNCHANGED hnext
       ELSE /\ hnext' = [hnext EXCEPT ![h] = next]
            /\ hpc' = [hpc EXCEPT ![h] = "upd_put"]
            /\ UNCHANGED <<hop, howned, hbase, hdead, hview, names, stored, guestOf, collision>>
    /\ UNCHANGED <<rec, ver, gstart, everPinned, mixed, faults, handoff,
                   hmode, hepoch, hrec, hver, hcount, hguest,
                   pinVars, relVars, delVars>>

UpdPut(h) ==
    /\ hpc[h] = "upd_put"
    /\ \/ \* Landed, and the reply says so.
          /\ rec.exists /\ ver = hver[h]
          /\ Land(hnext[h])
          /\ hrec' = [hrec EXCEPT ![h] = hnext[h]]
          /\ hver' = [hver EXCEPT ![h] = ver + 1]
          /\ Succeeded(h, hnext[h])
          /\ UNCHANGED faults
       \/ \* Landed, and the reply was lost.
          /\ rec.exists /\ ver = hver[h]
          /\ Fault
          /\ Land(hnext[h])
          /\ hpc' = [hpc EXCEPT ![h] = "upd_readback"]
          /\ UNCHANGED <<hrec, hver, hop, howned, hbase, hdead, hview, names, stored,
                         guestOf, collision>>
       \/ \* Refused: the record moved.
          /\ ~(rec.exists /\ ver = hver[h])
          /\ hpc' = [hpc EXCEPT ![h] = "upd_readback"]
          /\ UNCHANGED <<rec, ver, everPinned, faults, hrec, hver, hop, howned,
                         hbase, hdead, hview, names, stored, guestOf, collision>>
       \/ \* Failed without landing.
          /\ Fault
          /\ hpc' = [hpc EXCEPT ![h] = "upd_readback"]
          /\ UNCHANGED <<rec, ver, everPinned, hrec, hver, hop, howned,
                         hbase, hdead, hview, names, stored, guestOf, collision>>
    /\ UNCHANGED <<gstart, mixed, handoff, hmode, hepoch, hnext, hcount, hguest,
                   pinVars, relVars, delVars>>

\* Handle.replace's read-back. It settles what happened: the record it meant
\* to write, the one it had, one of its own epoch and nonce to adopt, or a
\* takeover.
UpdReadback(h) ==
    /\ hpc[h] = "upd_readback"
    /\ \/ \* The read fails too, or the record is gone: an error, no fence.
          /\ \/ Fault
             \/ ~rec.exists /\ UNCHANGED faults
          /\ Ready(h)
          /\ UNCHANGED <<hrec, hver, howned, hbase, hdead, names, stored,
                         guestOf, collision>>
       \/ /\ rec.exists
          /\ UNCHANGED faults
          /\ IF rec = hnext[h]
             THEN /\ hrec' = [hrec EXCEPT ![h] = rec]
                  /\ hver' = [hver EXCEPT ![h] = ver]
                  /\ Succeeded(h, rec)
             ELSE IF rec = hrec[h]
             THEN /\ hver' = [hver EXCEPT ![h] = ver]
                  /\ Ready(h)
                  /\ UNCHANGED <<hrec, howned, hbase, hdead, names, stored,
                                 guestOf, collision>>
             ELSE IF (rec.epoch = hepoch[h] /\ rec.nonce = h) \/ "adopt-any" \in Bugs
             THEN /\ hrec' = [hrec EXCEPT ![h] = rec]
                  /\ hver' = [hver EXCEPT ![h] = ver]
                  /\ hpc' = [hpc EXCEPT ![h] = "upd_begin"]
                  /\ UNCHANGED <<hop, howned, hbase, hdead, hview, names, stored,
                                 guestOf, collision>>
             ELSE /\ hpc' = [hpc EXCEPT ![h] = "fenced"]
                  /\ UNCHANGED <<hrec, hver, hop, howned, hbase, hdead, hview, names,
                                 stored, guestOf, collision>>
    /\ UNCHANGED <<rec, ver, gstart, everPinned, mixed, handoff, hmode, hepoch,
                   hnext, hcount, hguest, pinVars, relVars, delVars>>

\* One delete of a sweep this handle started. It goes on whatever the handle
\* does next, and after the handle is gone.
ReclaimStep(h) ==
    /\ hdead[h] # {}
    /\ LET d == Next1(hdead[h]) IN
          /\ stored' = stored \ {d}
          /\ hdead' = [hdead EXCEPT ![h] = @ \ {d}]
    /\ UNCHANGED <<rec, ver, names, ghostVars, hpc, hmode, hepoch, hrec, hver,
                   hnext, hop, howned, hbase, hcount, hguest, hview,
                   pinVars, relVars, delVars>>

\* VM.Confirm: a changed epoch fences; a record that cannot be read, or is
\* gone, is evidence of nothing.
Confirm(h) ==
    /\ hpc[h] = "running"
    /\ rec.exists /\ rec.epoch # hepoch[h]
    /\ hpc' = [hpc EXCEPT ![h] = "fenced"]
    /\ UNCHANGED <<storeVars, ghostVars, hmode, hepoch, hrec, hver, hnext, hop,
                   howned, hbase, hcount, hdead, hguest, hview, pinVars, relVars, delVars>>

\* vmmigrate.Migrate: the handoff names the checkpoint this handle is on, the
\* handle confirms its epoch, and releases the VM without publishing.
Handoff(h) ==
    /\ Migrating
    /\ hpc[h] = "running"
    /\ ~handoff.active
    /\ rec.exists /\ rec.epoch = hepoch[h]
    /\ hpc' = [hpc EXCEPT ![h] = "handedoff"]
    /\ handoff' = [active |-> TRUE, seq |-> hbase[h], guest |-> hguest[h], taken |-> FALSE]
    /\ UNCHANGED <<storeVars, guestOf, gstart, everPinned, collision, mixed, faults,
                   hmode, hepoch, hrec, hver, hnext, hop, howned, hbase, hcount,
                   hdead, hguest, hview, pinVars, relVars, delVars>>

Close(h) ==
    /\ hpc[h] = "running"
    /\ hpc' = [hpc EXCEPT ![h] = "closed"]
    /\ UNCHANGED <<storeVars, ghostVars, hmode, hepoch, hrec, hver, hnext, hop,
                   howned, hbase, hcount, hdead, hguest, hview, pinVars, relVars, delVars>>

(***************************************************************************)
(* Client.Open: claim the next epoch. An opener either recovers the VM,    *)
(* starting a guest of its own from the selected checkpoint, or is the     *)
(* destination of the migration in flight.                                 *)
(***************************************************************************)
OpenRead(h) ==
    /\ hpc[h] = "idle"
    /\ \/ /\ rec.exists
          /\ \E mode \in IF handoff.active /\ ~handoff.taken
                         THEN {"recover", "dest"} ELSE {"recover"} :
                hmode' = [hmode EXCEPT ![h] = mode]
          /\ hrec' = [hrec EXCEPT ![h] = rec]
          /\ hver' = [hver EXCEPT ![h] = ver]
          /\ hnext' = [hnext EXCEPT ![h] = [rec EXCEPT !.epoch = @ + 1, !.nonce = h]]
          /\ hpc' = [hpc EXCEPT ![h] = "open_put"]
       \/ /\ ~rec.exists
          /\ hpc' = [hpc EXCEPT ![h] = "gone"]
          /\ UNCHANGED <<hmode, hrec, hver, hnext>>
    /\ UNCHANGED <<storeVars, ghostVars, hepoch, hop, howned, hbase, hcount,
                   hdead, hguest, hview, pinVars, relVars, delVars>>

\* The handle the open returns reads the selected checkpoint's root. A
\* destination refuses a record that has moved past the handoff, and releases
\* the VM without publishing.
Opened(h, r, v) ==
    /\ hepoch' = [hepoch EXCEPT ![h] = r.epoch]
    /\ hrec' = [hrec EXCEPT ![h] = r]
    /\ hver' = [hver EXCEPT ![h] = v]
    /\ hbase' = [hbase EXCEPT ![h] = r.selected]
    /\ hview' = [hview EXCEPT ![h] = names[r.selected]]
    /\ IF hmode[h] = "dest"
       THEN IF r.selected = handoff.seq \/ "no-stale-check" \in Bugs
            THEN /\ hpc' = [hpc EXCEPT ![h] = "running"]
                 /\ hguest' = [hguest EXCEPT ![h] = handoff.guest]
                 /\ handoff' = [handoff EXCEPT !.taken = TRUE]
                 /\ mixed' = (mixed \/
                      ~(r.selected = gstart[handoff.guest] \/
                        (r.selected \in DOMAIN guestOf /\ guestOf[r.selected] = handoff.guest)))
                 /\ UNCHANGED gstart
            ELSE /\ hpc' = [hpc EXCEPT ![h] = "stale"]
                 /\ UNCHANGED <<hguest, handoff, mixed, gstart>>
       ELSE /\ hpc' = [hpc EXCEPT ![h] = "running"]
            /\ gstart' = [gstart EXCEPT ![h] = r.selected]
            /\ UNCHANGED <<hguest, handoff, mixed>>

OpenPut(h) ==
    /\ hpc[h] = "open_put"
    /\ \/ /\ rec.exists /\ ver = hver[h]
          /\ Land(hnext[h])
          /\ Opened(h, hnext[h], ver + 1)
          /\ UNCHANGED faults
       \/ /\ rec.exists /\ ver = hver[h]
          /\ Fault
          /\ Land(hnext[h])
          /\ hpc' = [hpc EXCEPT ![h] = "open_readback"]
          /\ UNCHANGED <<hepoch, hrec, hver, hbase, hview, hguest, handoff, mixed, gstart>>
       \/ \* Another open claimed this epoch first: read again.
          /\ ~(rec.exists /\ ver = hver[h])
          /\ hpc' = [hpc EXCEPT ![h] = "idle"]
          /\ UNCHANGED <<rec, ver, everPinned, faults, hepoch, hrec, hver,
                         hbase, hview, hguest, handoff, mixed, gstart>>
       \/ /\ Fault
          /\ hpc' = [hpc EXCEPT ![h] = "open_readback"]
          /\ UNCHANGED <<rec, ver, everPinned, hepoch, hrec, hver, hbase,
                         hview, hguest, handoff, mixed, gstart>>
    /\ UNCHANGED <<names, stored, guestOf, collision, hmode, hnext, hop, howned,
                   hcount, hdead, pinVars, relVars, delVars>>

OpenReadback(h) ==
    /\ hpc[h] = "open_readback"
    /\ \/ /\ \/ Fault
             \/ ~rec.exists /\ UNCHANGED faults
          /\ hpc' = [hpc EXCEPT ![h] = "gone"]
          /\ UNCHANGED <<hepoch, hrec, hver, hbase, hview, hguest, handoff, mixed, gstart>>
       \/ /\ rec.exists
          /\ UNCHANGED faults
          /\ IF rec.epoch = hnext[h].epoch /\ rec.nonce = h
             THEN Opened(h, rec, ver)
             ELSE /\ hpc' = [hpc EXCEPT ![h] =
                        IF rec.epoch >= hnext[h].epoch THEN "idle" ELSE "gone"]
                  /\ UNCHANGED <<hepoch, hrec, hver, hbase, hview, hguest, handoff,
                                 mixed, gstart>>
    /\ UNCHANGED <<storeVars, guestOf, everPinned, collision, hmode, hnext, hop,
                   howned, hcount, hdead, pinVars, relVars, delVars>>

(***************************************************************************)
(* Client.Pin: a pin without the epoch. Only the published selection, a    *)
(* kept checkpoint, or one already pinned may be named.                    *)
(***************************************************************************)
PinRead(p) ==
    /\ ppc[p] = "idle"
    /\ \/ /\ rec.exists
          /\ \E wanted \in {rec.selected} \cup DOMAIN names :
                /\ pwanted' = [pwanted EXCEPT ![p] = wanted]
                /\ IF wanted \in rec.pins
                      \/ ((wanted # rec.selected \/ ~rec.created) /\ wanted \notin rec.kept
                          /\ "pin-any" \notin Bugs)
                   THEN /\ ppc' = [ppc EXCEPT ![p] = "done"]
                        /\ UNCHANGED <<pver, prec, pnext>>
                   ELSE /\ ppc' = [ppc EXCEPT ![p] = "put"]
                        /\ pver' = [pver EXCEPT ![p] = ver]
                        /\ prec' = [prec EXCEPT ![p] = rec]
                        /\ pnext' = [pnext EXCEPT ![p] = [rec EXCEPT !.pins = @ \cup {wanted}]]
       \/ /\ ~rec.exists
          /\ ppc' = [ppc EXCEPT ![p] = "done"]
          /\ UNCHANGED <<pwanted, pver, prec, pnext>>
    /\ UNCHANGED <<storeVars, ghostVars, handleVars, relVars, delVars>>

PinPut(p) ==
    /\ ppc[p] = "put"
    /\ \/ /\ rec.exists /\ ver = pver[p]
          /\ Land(pnext[p])
          /\ ppc' = [ppc EXCEPT ![p] = "done"]
          /\ UNCHANGED faults
       \/ /\ rec.exists /\ ver = pver[p]
          /\ Fault
          /\ Land(pnext[p])
          /\ ppc' = [ppc EXCEPT ![p] = "readback"]
       \/ /\ ~(rec.exists /\ ver = pver[p])
          /\ ppc' = [ppc EXCEPT ![p] = "idle"]
          /\ UNCHANGED <<rec, ver, everPinned, faults>>
       \/ /\ Fault
          /\ ppc' = [ppc EXCEPT ![p] = "readback"]
          /\ UNCHANGED <<rec, ver, everPinned>>
    /\ UNCHANGED <<names, stored, guestOf, gstart, collision, mixed, handoff,
                   handleVars, pwanted, pver, prec, pnext, relVars, delVars>>

PinReadback(p) ==
    /\ ppc[p] = "readback"
    /\ \/ /\ \/ Fault
             \/ ~rec.exists /\ UNCHANGED faults
          /\ ppc' = [ppc EXCEPT ![p] = "done"]
       \/ /\ rec.exists
          /\ UNCHANGED faults
          /\ ppc' = [ppc EXCEPT ![p] =
                IF pwanted[p] \in rec.pins \/ rec = prec[p] THEN "done" ELSE "idle"]
    /\ UNCHANGED <<storeVars, guestOf, gstart, everPinned, collision, mixed,
                   handoff, handleVars, pwanted, pver, prec, pnext, relVars, delVars>>

(***************************************************************************)
(* Manager.Release: give up a kept checkpoint without the epoch, then      *)
(* sweep what only it held, against the record the release left.          *)
(***************************************************************************)
\* sweepReleased. An index it cannot open ends the sweep with nothing deleted.
Sweep(r) ==
    IF rseq = r.selected \/ ~r.created
       \/ ~Readable({rseq, r.selected} \cup Protected(r))
    THEN /\ rpc' = "done"
         /\ UNCHANGED rdead
    ELSE /\ rpc' = "sweeping"
         /\ rdead' = Dead(rseq, r.selected, r)

RelRead ==
    /\ rpc = "idle"
    /\ \/ /\ rec.exists
          /\ \E k \in rec.kept :
                /\ rseq' = k
                /\ IF k \in rec.pins
                   THEN /\ rpc' = "done"
                        /\ UNCHANGED <<rver, rrec, rnext>>
                   ELSE /\ rpc' = "put"
                        /\ rver' = ver
                        /\ rrec' = rec
                        /\ rnext' = [rec EXCEPT !.kept = @ \ {k}]
       \/ /\ ~rec.exists
          /\ rpc' = "done"
          /\ UNCHANGED <<rseq, rver, rrec, rnext>>
    /\ UNCHANGED <<storeVars, ghostVars, handleVars, pinVars, rdead, delVars>>

RelPut ==
    /\ rpc = "put"
    /\ \/ /\ rec.exists /\ ver = rver
          /\ Land(rnext)
          /\ Sweep(rnext)
          /\ UNCHANGED faults
       \/ /\ rec.exists /\ ver = rver
          /\ Fault
          /\ Land(rnext)
          /\ rpc' = "readback"
          /\ UNCHANGED rdead
       \/ /\ ~(rec.exists /\ ver = rver)
          /\ rpc' = "idle"
          /\ UNCHANGED <<rec, ver, everPinned, faults, rdead>>
       \/ /\ Fault
          /\ rpc' = "readback"
          /\ UNCHANGED <<rec, ver, everPinned, rdead>>
    /\ UNCHANGED <<names, stored, guestOf, gstart, collision, mixed, handoff,
                   handleVars, pinVars, rseq, rver, rrec, rnext, delVars>>

RelReadback ==
    /\ rpc = "readback"
    /\ \/ /\ \/ Fault
             \/ ~rec.exists /\ UNCHANGED faults
          /\ rpc' = "done"
          /\ UNCHANGED rdead
       \/ /\ rec.exists
          /\ UNCHANGED faults
          /\ IF rseq \notin rec.kept
             THEN Sweep(rec)
             ELSE /\ rpc' = IF rec = rrec THEN "done" ELSE "idle"
                  /\ UNCHANGED rdead
    /\ UNCHANGED <<storeVars, guestOf, gstart, everPinned, collision, mixed,
                   handoff, handleVars, pinVars, rseq, rver, rrec, rnext, delVars>>

RelStep ==
    /\ rpc = "sweeping"
    /\ IF rdead = {}
       THEN /\ rpc' = "done"
            /\ UNCHANGED <<stored, rdead>>
       ELSE LET d == Next1(rdead) IN
               /\ stored' = stored \ {d}
               /\ rdead' = rdead \ {d}
               /\ UNCHANGED rpc
    /\ UNCHANGED <<rec, ver, names, ghostVars, handleVars, pinVars,
                   rseq, rver, rrec, rnext, delVars>>

(***************************************************************************)
(* Manager.Delete: once no writer holds the record, remove it              *)
(* conditionally (Client.Remove), then delete every checkpoint no pin of   *)
(* the removed record protects (Store.DeleteVM).                           *)
(***************************************************************************)
NoWriter == \A h \in Handles : ~(hpc[h] \in Holding /\ hepoch[h] = rec.epoch)

DeleteVM(pins) ==
    IF ~Readable(pins)
    THEN /\ dpc' = "done"
         /\ UNCHANGED ddead
    ELSE /\ dpc' = "sweeping"
         /\ ddead' = stored \ (IF "delete-forgets-pins" \in Bugs THEN {} ELSE Closure(pins))

DelRead ==
    /\ dpc = "idle"
    /\ \/ /\ rec.exists /\ NoWriter
          /\ dpc' = "put"
          /\ dver' = ver
          /\ drec' = rec
       \/ /\ ~rec.exists
          /\ dpc' = "done"
          /\ UNCHANGED <<dver, drec>>
    /\ UNCHANGED <<storeVars, ghostVars, handleVars, pinVars, relVars, ddead>>

Removed == rec' = NoRecord /\ ver' = ver + 1

DelPut ==
    /\ dpc = "put"
    /\ \/ /\ rec.exists /\ ver = dver
          /\ Removed
          /\ DeleteVM(drec.pins)
          /\ UNCHANGED faults
       \/ /\ rec.exists /\ ver = dver
          /\ Fault
          /\ Removed
          /\ dpc' = "readback"
          /\ UNCHANGED ddead
       \/ /\ ~(rec.exists /\ ver = dver)
          /\ dpc' = "idle"
          /\ UNCHANGED <<rec, ver, faults, ddead>>
       \/ /\ Fault
          /\ dpc' = "readback"
          /\ UNCHANGED <<rec, ver, ddead>>
    /\ UNCHANGED <<names, stored, guestOf, gstart, everPinned, collision, mixed,
                   handoff, handleVars, pinVars, relVars, dver, drec>>

DelReadback ==
    /\ dpc = "readback"
    /\ \/ /\ Fault
          /\ dpc' = "done"
          /\ UNCHANGED ddead
       \/ /\ UNCHANGED faults
          /\ IF ~rec.exists
             THEN DeleteVM(drec.pins)
             ELSE /\ dpc' = IF ver = dver THEN "done" ELSE "idle"
                  /\ UNCHANGED ddead
    /\ UNCHANGED <<storeVars, guestOf, gstart, everPinned, collision, mixed,
                   handoff, handleVars, pinVars, relVars, dver, drec>>

DelStep ==
    /\ dpc = "sweeping"
    /\ IF ddead = {}
       THEN /\ dpc' = "done"
            /\ UNCHANGED <<stored, ddead>>
       ELSE LET d == Next1(ddead) IN
               /\ stored' = stored \ {d}
               /\ ddead' = ddead \ {d}
               /\ UNCHANGED dpc
    /\ UNCHANGED <<rec, ver, names, ghostVars, handleVars, pinVars, relVars,
                   dver, drec>>

(***************************************************************************)
Next ==
    \/ \E h \in Handles :
          \/ Publish(h) \/ ForkNew(h) \/ ForkBase(h)
          \/ UpdBegin(h) \/ UpdPut(h) \/ UpdReadback(h)
          \/ ReclaimStep(h) \/ Confirm(h) \/ Handoff(h) \/ Close(h)
          \/ OpenRead(h) \/ OpenPut(h) \/ OpenReadback(h)
    \/ \E p \in Pinners : PinRead(p) \/ PinPut(p) \/ PinReadback(p)
    \/ RelRead \/ RelPut \/ RelReadback \/ RelStep
    \/ DelRead \/ DelPut \/ DelReadback \/ DelStep

Spec == Init /\ [][Next]_vars

(***************************************************************************)
(* What must hold.                                                         *)
(***************************************************************************)

\* An open reads the selected checkpoint, and everything it names.
SelectedReadable ==
    rec.exists /\ rec.created => names[rec.selected] \subseteq stored

\* A pin is permanent, and outlives the record that took it: a fork, or a
\* fork's descendant, reads through the pinned checkpoint.
PinnedReadable ==
    \A p \in everPinned \cap DOMAIN names : names[p] \subseteq stored

\* A kept checkpoint can be forked for as long as it is kept.
KeptReadable ==
    rec.exists => \A k \in rec.kept : names[k] \subseteq stored

\* Every checkpoint object is written create-if-absent, and no two writers
\* ever allocate one sequence.
NoCollision == ~collision

\* A destination runs a guest only over that guest's own checkpoint.
NoMixedGuest == ~mixed

\* No checkpoint names one that was never committed.
NamesCommitted ==
    \A s \in DOMAIN names : names[s] \subseteq DOMAIN names

\* Openers are interchangeable, which TLC may use to check fewer states.
OpenerSymmetry == Permutations(Openers)

Invariants ==
    /\ SelectedReadable /\ PinnedReadable /\ KeptReadable
    /\ NoCollision /\ NoMixedGuest /\ NamesCommitted

\* Only the epoch holder moves the selection, only forward, and only to a
\* checkpoint of its own epoch. Pins are only ever added. Epochs only grow.
SelectionMoves ==
    [][rec.exists /\ rec'.exists =>
         /\ rec'.epoch >= rec.epoch
         /\ rec.pins \subseteq rec'.pins
         /\ (rec'.selected # rec.selected =>
                /\ Before(rec.selected, rec'.selected)
                /\ Epoch(rec'.selected) = rec'.epoch
                /\ rec'.epoch = rec.epoch)]_vars
=============================================================================
