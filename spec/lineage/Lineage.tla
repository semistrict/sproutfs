----------------------------- MODULE Lineage -------------------------------
(***************************************************************************)
(* Fork lineage across VMs, as docs/volumes.md (The fork point) and        *)
(* docs/metadata.md describe it and volume/fork.go, checkpoint/reclaim.go  *)
(* implement it, and a candidate design for the pin collector of TASK-24.  *)
(*                                                                         *)
(* A fork pins the parent's checkpoint, then creates the child, which      *)
(* reads through the pinned checkpoint until its own root lands. The root  *)
(* names the parent's checkpoints the child still reads, and a later       *)
(* checkpoint of the child names some of what its last one did. So a      *)
(* grandchild names its grandparent's checkpoints directly, and no record  *)
(* says so. A sweep deletes only its own VM's checkpoints, and spares the  *)
(* pinned ones and everything they name. A delete removes the record and   *)
(* spares the pins. Nothing in the code releases a pin.                    *)
(*                                                                         *)
(* Collector is the candidate collector, or "none", which is the code      *)
(* today:                                                                  *)
(*  - "naive" scans the records, one at a time, and releases a pin no      *)
(*    record's selected checkpoint reads, conditional on the record it     *)
(*    read;                                                                *)
(*  - "holders" also keeps a pin while a fork that took it may still      *)
(*    create its child or publish the child's root: each fork names its    *)
(*    child in the pin, and a holder counts until the scan saw its root    *)
(*    landed, or its fork is over.                                         *)
(*                                                                         *)
(* The invariant: no VM, and no fork in flight, reads a checkpoint that is *)
(* not in the store.                                                       *)
(*                                                                         *)
(* Left out: lost replies and takeovers, which spec/ownership checks for   *)
(* one VM; kept checkpoints; compaction is any subset, as overwriting.     *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
    Root,       \* the VM that exists at the start
    Child,      \* forked from Root
    Grandchild, \* forked from Child
    MaxCkpts,   \* checkpoints each VM may publish after its first
    Collector,  \* "none", "naive", "holders" or "holders-late"
    Deleting,   \* whether VMs may be deleted
    Bugs        \* defects to put back, to show the invariant catches each one

VMs == {Root, Child, Grandchild}
Forks == {<<Root, Child>>, <<Child, Grandchild>>}

None == <<"none", 0>>
Ckpt(v, n) == <<v, n>>
VMOf(c) == c[1]

VARIABLES
    exists,   \* VMs with a record
    sel,      \* each VM's selected checkpoint, None while its root is pending
    pins,     \* each VM's pinned checkpoints
    holders,  \* pinned checkpoint -> the children forks named in it
    ver,      \* each VM's record version
    count,    \* checkpoints each VM has allocated
    names,    \* committed checkpoint -> what its index names
    stored,   \* checkpoints in the store
    sweep,    \* checkpoints a sweep has decided to delete, not yet deleted
    fork,     \* child -> "idle", "pinned", "created", "rooted", "failed"
    point,    \* child -> the checkpoint its fork pinned
    cpc,      \* the collector: "idle", "scanning", "releasing", "done"
    seenSel,  \* what the collector read of each record: its selection,
    seenPins, \*   its pins and their holders, and its version
    seenHold,
    seenVer,
    scanned

vars == <<exists, sel, pins, holders, ver, count, names, stored, sweep, fork, point,
          cpc, seenSel, seenPins, seenHold, seenVer, scanned>>
colVars == <<cpc, seenSel, seenPins, seenHold, seenVer, scanned>>

Children == {f[2] : f \in Forks}
ParentOf(v) == CHOOSE p \in VMs : <<p, v>> \in Forks
Closure(S) == UNION {names[s] : s \in S}

Init ==
    /\ exists = {Root}
    /\ sel = [v \in VMs |-> IF v = Root THEN Ckpt(Root, 1) ELSE None]
    /\ pins = [v \in VMs |-> {}]
    /\ holders = [c \in {} |-> {}]
    /\ ver = [v \in VMs |-> 0]
    /\ count = [v \in VMs |-> IF v = Root THEN 1 ELSE 0]
    /\ names = (Ckpt(Root, 1) :> {Ckpt(Root, 1)})
    /\ stored = {Ckpt(Root, 1)}
    /\ sweep = [v \in VMs |-> {}]
    /\ fork = [v \in VMs |-> "idle"]
    /\ point = [v \in VMs |-> None]
    /\ cpc = IF Collector = "none" THEN "done" ELSE "idle"
    /\ seenSel = [v \in VMs |-> None]
    /\ seenPins = [v \in VMs |-> {}]
    /\ seenHold = [c \in {} |-> {}]
    /\ seenVer = [v \in VMs |-> 0]
    /\ scanned = {}

\* What a VM reads: its selected checkpoint's names, or, while its root is
\* pending, the names of the point it was forked at.
Reads(v) ==
    IF sel[v] # None THEN names[sel[v]]
    ELSE IF point[v] # None THEN names[point[v]] ELSE {}

Holders(c) == IF c \in DOMAIN holders THEN holders[c] ELSE {}

(***************************************************************************)
(* A VM publishes a checkpoint and selects it; its sweep is decided then   *)
(* and runs later. It reads no older checkpoint the last one did not.      *)
(***************************************************************************)
Publish(v) ==
    /\ v \in exists /\ sel[v] # None /\ count[v] < MaxCkpts + 1
    /\ LET s == Ckpt(v, count[v] + 1)
           replaced == sel[v]
       IN \E kept \in SUBSET names[replaced] :
          /\ names' = (s :> ({s} \cup kept)) @@ names
          /\ stored' = stored \cup {s}
          /\ count' = [count EXCEPT ![v] = @ + 1]
          /\ sel' = [sel EXCEPT ![v] = s]
          /\ ver' = [ver EXCEPT ![v] = @ + 1]
          \* checkpoint.Store.Reclaim: own checkpoints only.
          /\ sweep' = [sweep EXCEPT ![v] = @ \cup
                 {d \in ({replaced} \cup names[replaced]) \ ({s} \cup kept) :
                     (VMOf(d) = v \/ "sweep-other-vms" \in Bugs) /\ d \notin Closure(pins[v])}]
    /\ UNCHANGED <<exists, pins, holders, fork, point, colVars>>

Sweep(v) ==
    /\ sweep[v] # {}
    /\ stored' = stored \ sweep[v]
    /\ sweep' = [sweep EXCEPT ![v] = {}]
    /\ UNCHANGED <<exists, sel, pins, holders, ver, count, names, fork, point, colVars>>

(***************************************************************************)
(* A fork: pin the parent's selected checkpoint, create the child, publish *)
(* its root. Pinning an already pinned checkpoint writes nothing, unless   *)
(* the collector design names the child in the pin.                       *)
(***************************************************************************)
ForkPin(c) ==
    LET p == ParentOf(c) IN
    /\ c \in Children /\ fork[c] = "idle" /\ c \notin exists
    /\ p \in exists /\ sel[p] # None
    /\ LET s == sel[p] IN
       /\ pins' = [pins EXCEPT ![p] = @ \cup {s}]
       /\ holders' = IF Collector \in {"holders", "holders-late"}
                     THEN (s :> (Holders(s) \cup {c})) @@ holders ELSE holders
       /\ ver' = IF s \in pins[p] /\ Collector \notin {"holders", "holders-late"} THEN ver
                 ELSE [ver EXCEPT ![p] = @ + 1]
       /\ point' = [point EXCEPT ![c] = s]
       /\ fork' = [fork EXCEPT ![c] = "pinned"]
    /\ UNCHANGED <<exists, sel, count, names, stored, sweep, colVars>>

ForkCreate(c) ==
    /\ fork[c] = "pinned"
    /\ exists' = exists \cup {c}
    /\ fork' = [fork EXCEPT ![c] = "created"]
    /\ UNCHANGED <<sel, pins, holders, ver, count, names, stored, sweep, point, colVars>>

\* A fork can fail after its pin, before its child exists.
ForkFail(c) ==
    /\ fork[c] = "pinned"
    /\ fork' = [fork EXCEPT ![c] = "failed"]
    /\ UNCHANGED <<exists, sel, pins, holders, ver, count, names, stored, sweep, point, colVars>>

\* The child's root: it names the parent's checkpoints it reads.
ForkRoot(c) ==
    /\ fork[c] = "created" /\ c \in exists
    /\ LET r == Ckpt(c, 1) IN
       \E kept \in SUBSET names[point[c]] :
          /\ names' = (r :> ({r} \cup kept)) @@ names
          /\ stored' = stored \cup {r}
          /\ sel' = [sel EXCEPT ![c] = r]
          /\ count' = [count EXCEPT ![c] = 1]
          /\ ver' = [ver EXCEPT ![c] = @ + 1]
          /\ fork' = [fork EXCEPT ![c] = "rooted"]
    /\ UNCHANGED <<exists, pins, holders, sweep, point, colVars>>

(***************************************************************************)
(* A delete: remove the record and delete every checkpoint of the VM no    *)
(* pin protects. A pin is ordered against it by the record: one that lands *)
(* first is spared, and one after finds no record.                         *)
(***************************************************************************)
Delete(v) ==
    /\ Deleting /\ v \in exists /\ sel[v] # None
    /\ exists' = exists \ {v}
    /\ stored' = {s \in stored : VMOf(s) # v \/
                    (s \in Closure(pins[v]) /\ "delete-forgets-pins" \notin Bugs)}
    /\ ver' = [ver EXCEPT ![v] = @ + 1]
    /\ UNCHANGED <<sel, pins, holders, count, names, sweep, fork, point, colVars>>

(***************************************************************************)
(* The candidate collector: scan the records one at a time, then release   *)
(* one pin nothing it saw reads, conditional on that record's version, and *)
(* delete what only that pin kept.                                         *)
(***************************************************************************)
Scan(v) ==
    /\ cpc \in {"idle", "scanning"} /\ v \in exists /\ v \notin scanned
    /\ cpc' = "scanning"
    /\ scanned' = scanned \cup {v}
    /\ seenSel' = [seenSel EXCEPT ![v] = sel[v]]
    /\ seenPins' = [seenPins EXCEPT ![v] = pins[v]]
    /\ seenHold' = [c \in DOMAIN holders |-> holders[c]] @@ seenHold
    /\ seenVer' = [seenVer EXCEPT ![v] = ver[v]]
    /\ UNCHANGED <<exists, sel, pins, holders, ver, count, names, stored, sweep, fork, point>>

\* What the scan saw read: every selected checkpoint's names, a pending root's
\* point, and for "holders" every pin a fork that is not over still holds.
Live ==
    LET selected == UNION {IF seenSel[v] # None THEN names[seenSel[v]] ELSE {} : v \in scanned}
        \* A holder whose root the scan saw landed reads what its selection
        \* names, which the scan has. One whose record the scan saw with its
        \* root pending, or did not see at all, reads the pin whole unless its
        \* fork is over: telling a fork that failed from one still running
        \* takes a bound on how long a fork may run, which is the hold.
        \* "holders-late" judges a holder by its state when it releases rather
        \* than by what its scan saw.
        InFlight(h) ==
            IF Collector = "holders-late" THEN fork[h] \in {"pinned", "created"}
            ELSE IF h \in scanned THEN seenSel[h] = None
            ELSE fork[h] # "failed"
        held == IF Collector \in {"holders", "holders-late"}
                THEN UNION {names[c] : c \in {c \in DOMAIN seenHold :
                         \E h \in seenHold[c] : InFlight(h)}}
                ELSE {}
    IN selected \cup held

Release ==
    /\ cpc = "scanning" /\ exists \subseteq scanned
    /\ \E v \in scanned :
          LET candidates == {c \in seenPins[v] : c \notin Live} IN
          IF candidates = {} THEN /\ cpc' = "done" /\ UNCHANGED <<pins, ver, stored>>
          ELSE \E c \in candidates :
             /\ cpc' = "done"
             /\ IF v \in exists /\ ver[v] = seenVer[v]
                THEN /\ pins' = [pins EXCEPT ![v] = @ \ {c}]
                     /\ ver' = [ver EXCEPT ![v] = @ + 1]
                     /\ stored' = stored \ {d \in names[c] :
                            VMOf(d) = v /\ d \notin Live /\ d \notin Closure(pins[v] \ {c})}
                ELSE UNCHANGED <<pins, ver, stored>>
    /\ UNCHANGED <<exists, sel, holders, count, names, sweep, fork, point,
                   seenSel, seenPins, seenHold, seenVer, scanned>>

Next ==
    \/ \E v \in VMs : Publish(v) \/ Sweep(v) \/ Delete(v) \/ Scan(v)
    \/ \E c \in Children : ForkPin(c) \/ ForkCreate(c) \/ ForkFail(c) \/ ForkRoot(c)
    \/ Release

Spec == Init /\ [][Next]_vars

\* Every VM, and every fork in flight, reads only checkpoints in the store.
NoDanglingRead ==
    /\ \A v \in exists : Reads(v) \subseteq stored
    /\ \A c \in Children : fork[c] = "pinned" => names[point[c]] \subseteq stored
=============================================================================
