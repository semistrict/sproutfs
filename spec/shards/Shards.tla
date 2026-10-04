------------------------------- MODULE Shards -------------------------------
(***************************************************************************)
(* A shard moving between members (docs/hosting.md, "Shards on network    *)
(* disks"): the membership says which member serves the shard, a          *)
(* controller carries that out through the cloud's attach API, and each    *)
(* member opens the shard's device on its machine and serves it.          *)
(*                                                                         *)
(* The membership is a line of generations, as spec/membership checks it  *)
(* is; here one controller writes it, from the newest generation, one     *)
(* step at a time: release, let go, assign, serve. It lets a shard go     *)
(* only once the member's host has reported it closed, or is dead, and    *)
(* the cloud has it on no machine. The controller attaches and detaches   *)
(* from a snapshot of the membership it read earlier, which may be stale,  *)
(* as a controller that crashed between reading and acting, or two at     *)
(* once, act: it attaches a shard attaching or serving to its member's    *)
(* machine, and detaches it from any other machine, and from every machine *)
(* once it is released, or releasing and its member's host has reported   *)
(* it closed. Reports may be stale too.                                    *)
(*                                                                         *)
(* The cloud attaches a disk to one machine at a time, unless the fault    *)
(* "multi-attach" lets it attach one to a second machine, as a disk set   *)
(* up for several writers would be. A detach takes the device from every  *)
(* process of that machine. One process of a machine opens a device at a  *)
(* time. A host opens the shard while the membership it holds assigns it  *)
(* there, takes the lease in the shard's header for that assignment       *)
(* unless the lease names a newer one, and checks the lease again before   *)
(* every region it writes. It serves the shard while it holds it open and *)
(* its membership has it serving there under the assignment it opened it  *)
(* under. A host reads the membership late, and may die with the shard    *)
(* open; a dead host's handles go with it, and its machine keeps the      *)
(* shard attached.                                                         *)
(*                                                                         *)
(* Left out: the bytes, which every read checks whoever serves them       *)
(* (spec/diskcache); several writers of the membership (spec/membership); *)
(* a host started again, which is a new member; more than one shard, which *)
(* move apart.                                                             *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Hosts,      \* the hosts, each a member
    Shared,     \* hosts that share machine 0; every other host is its own machine
    MaxGen,     \* the generations the controller may write
    MaxCrashes, \* hosts that may die
    Faults,     \* "multi-attach"
    Bugs        \* defects put back, for the mutants

None == 0
Machine(h) == IF h \in Shared THEN 0 ELSE h
Machines == {Machine(h) : h \in Hosts}
States == {"attaching", "serving", "releasing", "released"}

VARIABLES
    objs,     \* objs[g]: the shard at generation g: [state, member, assigned]
    gen,      \* the newest generation
    snap,     \* the generation the controller acts on the cloud from
    view,     \* view[h]: the generation host h holds
    reported, \* reported[h]: whether h last reported the shard open
    att,      \* the machines the shard is attached to
    opened,   \* opened[h]: the assignment h opened the shard under, None if not open
    lease,    \* the assignment the shard's header leases it to
    alive,
    crashes,
    staleWrite \* a host wrote a region while another assignment held the lease

vars == <<objs, gen, snap, view, reported, att, opened, lease, alive, crashes, staleWrite>>

Initial == [state |-> "released", member |-> None, assigned |-> 0]

Init ==
    /\ objs = [g \in 0..MaxGen |-> Initial]
    /\ gen = 0
    /\ snap = 0
    /\ view = [h \in Hosts |-> 0]
    /\ reported = [h \in Hosts |-> FALSE]
    /\ att = {}
    /\ opened = [h \in Hosts |-> None]
    /\ lease = 0
    /\ alive = [h \in Hosts |-> TRUE]
    /\ crashes = 0
    /\ staleWrite = FALSE

Shard == objs[gen]

\* The controller writes the next generation.
Write(next) ==
    /\ gen < MaxGen
    /\ objs' = [objs EXCEPT ![gen + 1] = next]
    /\ gen' = gen + 1

Release ==
    /\ Shard.state \in {"attaching", "serving"}
    /\ Write([Shard EXCEPT !.state = "releasing"])
    /\ UNCHANGED <<snap, view, reported, att, opened, lease, alive, crashes, staleWrite>>

\* Its member's host has reported it closed, or is dead, and the cloud has
\* it on no machine. The mutant lets it go at once.
Let ==
    /\ Shard.state = "releasing"
    /\ \/ (~reported[Shard.member] \/ ~alive[Shard.member]) /\ att = {}
       \/ "let-early" \in Bugs
    /\ Write([state |-> "released", member |-> None, assigned |-> 0])
    /\ UNCHANGED <<snap, view, reported, att, opened, lease, alive, crashes, staleWrite>>

Assign(h) ==
    /\ Shard.state = "released" /\ alive[h]
    /\ Write([state |-> "attaching", member |-> h, assigned |-> gen + 1])
    /\ UNCHANGED <<snap, view, reported, att, opened, lease, alive, crashes, staleWrite>>

\* Its member's host has reported it open.
Serve ==
    /\ Shard.state = "attaching" /\ reported[Shard.member]
    /\ Write([Shard EXCEPT !.state = "serving"])
    /\ UNCHANGED <<snap, view, reported, att, opened, lease, alive, crashes, staleWrite>>

\* The controller reads the membership it will act on the cloud from.
Snap ==
    /\ snap' = gen
    /\ UNCHANGED <<objs, gen, view, reported, att, opened, lease, alive, crashes, staleWrite>>

\* Attach the shard to its member's machine, from the snapshot. A
\* single-writer disk attached elsewhere is refused.
Attach ==
    LET s == objs[snap] IN
    /\ s.state \in {"attaching", "serving"}
    /\ Machine(s.member) \notin att
    /\ att = {} \/ "multi-attach" \in Faults
    /\ att' = att \cup {Machine(s.member)}
    /\ UNCHANGED <<objs, gen, snap, view, reported, opened, lease, alive, crashes, staleWrite>>

\* Detach the shard from machine m, from the snapshot: a machine that is not
\* its member's, or any once it is released, or releasing and closed by its
\* member's host. Every process of m loses the device.
Detach(m) ==
    LET s == objs[snap] IN
    /\ m \in att
    /\ \/ s.state \in {"attaching", "serving"} /\ m # Machine(s.member)
       \/ s.state = "released"
       \/ s.state = "releasing" /\ (~reported[s.member] \/ ~alive[s.member])
    /\ att' = att \ {m}
    /\ opened' = [h \in Hosts |-> IF Machine(h) = m THEN None ELSE opened[h]]
    /\ UNCHANGED <<objs, gen, snap, view, reported, lease, alive, crashes, staleWrite>>

\* A host reads the membership.
Read(h) ==
    /\ alive[h] /\ view[h] < gen
    /\ view' = [view EXCEPT ![h] = gen]
    /\ UNCHANGED <<objs, gen, snap, reported, att, opened, lease, alive, crashes, staleWrite>>

\* A host reports what it holds; the report may be read much later.
Report(h) ==
    /\ alive[h]
    /\ reported' = [reported EXCEPT ![h] = opened[h] # None]
    /\ UNCHANGED <<objs, gen, snap, view, att, opened, lease, alive, crashes, staleWrite>>

\* The assignment the membership a host holds gives it, 0 for none.
Assigned(h) ==
    LET s == objs[view[h]]
    IN IF s.member = h /\ s.state \in {"attaching", "serving"} THEN s.assigned ELSE 0

\* A host opens the shard on its machine, if no process of the machine holds
\* it, and takes the lease unless it names a newer assignment.
Open(h) ==
    /\ alive[h] /\ opened[h] = None /\ Assigned(h) > 0
    /\ Machine(h) \in att
    /\ ~\E o \in Hosts : Machine(o) = Machine(h) /\ opened[o] # None
    /\ lease <= Assigned(h) \/ "ignore-lease" \in Bugs
    /\ lease' = Assigned(h)
    /\ opened' = [opened EXCEPT ![h] = Assigned(h)]
    /\ UNCHANGED <<objs, gen, snap, view, reported, att, alive, crashes, staleWrite>>

\* A host closes the shard once its membership no longer assigns it there
\* under the assignment it opened it under.
Close(h) ==
    /\ alive[h] /\ opened[h] # None /\ Assigned(h) # opened[h]
    /\ opened' = [opened EXCEPT ![h] = None]
    /\ UNCHANGED <<objs, gen, snap, view, reported, att, lease, alive, crashes, staleWrite>>

\* A host writes a region of the shard: it checks the lease first, and a
\* lease another assignment took fences it, which closes the shard.
WriteRegion(h) ==
    /\ alive[h] /\ opened[h] # None
    /\ IF lease = opened[h] \/ "ignore-lease" \in Bugs
       THEN /\ staleWrite' = (staleWrite \/ lease # opened[h])
            /\ UNCHANGED opened
       ELSE /\ opened' = [opened EXCEPT ![h] = None]
            /\ UNCHANGED staleWrite
    /\ UNCHANGED <<objs, gen, snap, view, reported, att, lease, alive, crashes>>

\* A host dies, its handles with it; its machine keeps the shard attached.
Crash(h) ==
    /\ alive[h] /\ crashes < MaxCrashes
    /\ alive' = [alive EXCEPT ![h] = FALSE]
    /\ opened' = [opened EXCEPT ![h] = None]
    /\ crashes' = crashes + 1
    /\ UNCHANGED <<objs, gen, snap, view, reported, att, lease, staleWrite>>

Next ==
    \/ Release \/ Let \/ Serve \/ Snap \/ Attach
    \/ \E h \in Hosts : Assign(h) \/ Read(h) \/ Report(h) \/ Open(h) \/ Close(h) \/ WriteRegion(h) \/ Crash(h)
    \/ \E m \in Machines : Detach(m)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
\* A host serves the shard while it holds it open and the membership it
\* holds has it serving there under the assignment it opened it under.
Serving(h) ==
    LET s == objs[view[h]]
    IN alive[h] /\ opened[h] # None /\ s.member = h /\ s.state = "serving" /\ s.assigned = opened[h]

\* No two hosts serve the shard at once.
OneServer == Cardinality({h \in Hosts : Serving(h)}) <= 1

\* No two processes hold the shard open on one machine.
OneOpenerAMachine == \A m \in Machines : Cardinality({h \in Hosts : Machine(h) = m /\ opened[h] # None}) <= 1

\* No host writes a region of the shard while another assignment holds its
\* lease.
NoStaleWrite == ~staleWrite

=============================================================================
