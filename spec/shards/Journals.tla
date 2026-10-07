------------------------------ MODULE Journals ------------------------------
(***************************************************************************)
(* Journal disks in the membership (plans/fsync-journal-2026-10-06.md,     *)
(* "Where the journal lives"; membership/journals.go and                   *)
(* host/journaldisks.go). Each machine in the host pool keeps a journal    *)
(* disk reserved for it, and the member on that machine writes it. The     *)
(* controller creates and deletes the disks through the cloud's API, and   *)
(* the membership assigns them as it assigns shards. Shards checks how one *)
(* disk moves between members; this module checks what is new for a        *)
(* journal disk: it is reserved for a machine, it holds live entries that  *)
(* must outlive a lost host, and it is created and deleted.                *)
(*                                                                         *)
(* A journal disk in the membership has a state, a member, the generation  *)
(* that assigned it, the machine it is reserved for, and an empty mark.    *)
(* The state "deleting" is a disk on its way out of the cloud. A disk      *)
(* assigned to the member on the machine it is reserved for is that        *)
(* member's own, which it writes; a disk assigned to any other member is   *)
(* read.                                                                   *)
(*                                                                         *)
(* The controller writes the membership by compare-and-set, one step a     *)
(* generation, from the newest one. Its steps look at the cloud's disks    *)
(* and attachments, the hosts it can reach, the machines in the pool, and  *)
(* what the hosts report. It acts on the cloud from any generation it read *)
(* earlier: a controller that crashed between reading and acting, or two   *)
(* at once. So a crash between a call of the cloud and a write of the      *)
(* membership is any interleaving of the two. The cloud attaches a disk to *)
(* one machine at a time, refuses to delete a disk that is attached, and   *)
(* detaches the disks of a machine it deletes. A detach takes the device   *)
(* from every process of that machine.                                     *)
(*                                                                         *)
(* A host opens a disk while the membership it holds assigns the disk to   *)
(* it, attaching or serving, and the disk is attached to its machine, if   *)
(* no process of the machine holds it, and takes the lease in the disk's   *)
(* header unless that names a newer assignment. It reads the lease only    *)
(* then: a write does not read it. A host writes its own disk while it     *)
(* runs a VM. The orchestrator places a VM on a host whose own disk serves *)
(* in a membership it read, which may be old. An entry is live from its    *)
(* write until a checkpoint drops it, at any time. A host reports each     *)
(* disk it holds, and whether it is empty: no live entry, and, for its     *)
(* own, no VM. The controller marks a disk empty when its holder reports   *)
(* it so while the disk is read or releasing, and unmarks it when the      *)
(* holder reports live entries again. It releases a read disk marked       *)
(* empty. The holder closes a releasing disk once the membership it holds  *)
(* marks it empty; a close that finds live entries opens the disk again.   *)
(*                                                                         *)
(* The rules are those of commit 130a2b85 with the fix of B7               *)
(* (spec/bugs.md): a host reports its own disk empty only once the         *)
(* membership it holds releases it (part 1), and takes no VM from then on  *)
(* (part 2); and a report names the assignment the host holds the disk     *)
(* under, and the controller marks a disk empty only on a report of its    *)
(* assignment (part 3). Each part put back is a mutant.                    *)
(*                                                                         *)
(* A host may die; go quiet, alive and holding what it holds but out of    *)
(* the controller's reach, so it is drained as lost, and come back; or     *)
(* start again under the same identity, holding nothing. A host of         *)
(* another identity may start on the machine of a dead one.                *)
(*                                                                         *)
(* Left out: bytes and positions, which spec/journal checks; zones; the    *)
(* hour a free disk waits, which may pass at any time here; the windows a  *)
(* shard ranks, which a journal disk does not; and "the fewest journal     *)
(* disks", where any active member may read here. Reports are read fresh:  *)
(* a report says which assignment it holds, a disk is let go only once the *)
(* cloud has it on no machine, and Shards checks late reports against      *)
(* both. An empty report read late still says the disk had no live entry   *)
(* when it was made, and with the fix of B7 nothing writes the disk after. *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Start,      \* "bare", "running" or "spare", below
    Hosts,      \* the hosts, each a member while it is in the membership
    Again,      \* hosts that start on machine 1 after the host there dies
    Disks,      \* the journal disks the cloud may create, in order
    Later,      \* machines that join the pool later; the rest start in it
    MaxGen,     \* the generations the controller may write
    MaxLosses,  \* deaths, silences and restarts of hosts
    MaxLeaves,  \* machines that may leave the pool
    Bugs        \* defects put back, for the mutants

None == 0
Machine(h) == IF h \in Again THEN 1 ELSE h
Machines == {Machine(h) : h \in Hosts}

NoDisk == [st |-> "absent", member |-> None, assigned |-> 0, reserved |-> None, empty |-> FALSE]
NoHold == [st |-> "none", a |-> 0]

VARIABLES
    objs,     \* objs[g]: the membership at generation g: [d, m]
    gen,      \* the newest generation
    view,     \* view[h]: the generation host h holds
    hold,     \* hold[h][d]: open, or closed to open again, under an assignment; or none
    vms,      \* vms[h]: host h runs a VM
    exists,   \* exists[d]: "no", "yes" or "deleted", in the cloud
    att,      \* att[d]: the machines d is attached to
    lease,    \* lease[d]: the assignment d's header leases it to
    live,     \* live[d]: the machines whose writers' entries on d are live
    started,
    alive,
    wanted,   \* the controller reaches the host
    pool,     \* pool[m]: "out", "in", "leaving" or "gone"
    losses,
    leaves,
    staleWrite, \* a host wrote d while another assignment held its lease
    lostLive    \* the cloud deleted a disk with live entries

vars == <<objs, gen, view, hold, vms, exists, att, lease, live, started, alive, wanted, pool,
          losses, leaves, staleWrite, lostLive>>
hostVars == <<view, hold, vms, started, alive, wanted>>
cloudVars == <<exists, att>>
diskVars == <<lease, live>>
countVars == <<losses, leaves, staleWrite, lostLive>>

\* Start "bare" is a deployment with no host and no journal disk yet. Start
\* "running" is one at generation 1 in which every host but those of Again
\* is an active member, and each disk numbered as such a host is its own:
\* reserved for its machine, attached to it, and held open by the host.
\* Start "spare" is "running" with every other disk free and empty, as one
\* is for an hour after its host left.
Running == Start \in {"running", "spare"}
First == IF Running THEN 1 ELSE 0
Owned(d) == Running /\ d \in Hosts \ Again
Spare(d) == Start = "spare" /\ ~Owned(d)
Up(h) == Running /\ h \notin Again

Init ==
    /\ objs = [g \in 0..MaxGen |->
                [d |-> [x \in Disks |->
                          CASE Owned(x) -> [st |-> "serving", member |-> x, assigned |-> 1, reserved |-> x,
                                          empty |-> FALSE]
                            [] Spare(x) -> [NoDisk EXCEPT !.st = "released", !.empty = TRUE]
                            [] OTHER -> NoDisk],
                 m |-> [h \in Hosts |-> IF Up(h) THEN "active" ELSE "none"]]]
    /\ gen = First
    /\ view = [h \in Hosts |-> First]
    /\ hold = [h \in Hosts |-> [d \in Disks |-> IF Owned(d) /\ d = h THEN [st |-> "open", a |-> 1] ELSE NoHold]]
    /\ vms = [h \in Hosts |-> FALSE]
    /\ exists = [d \in Disks |-> IF Owned(d) \/ Spare(d) THEN "yes" ELSE "no"]
    /\ att = [d \in Disks |-> IF Owned(d) THEN {d} ELSE {}]
    /\ lease = [d \in Disks |-> IF Owned(d) THEN 1 ELSE 0]
    /\ live = [d \in Disks |-> {}]
    /\ started = [h \in Hosts |-> Up(h)]
    /\ alive = [h \in Hosts |-> Up(h)]
    /\ wanted = [h \in Hosts |-> Up(h)]
    /\ pool = [m \in Machines |-> IF m \in Later THEN "out" ELSE "in"]
    /\ losses = 0
    /\ leaves = 0
    /\ staleWrite = FALSE
    /\ lostLive = FALSE

TypeOK ==
    /\ gen \in 0..MaxGen
    /\ \A h \in Hosts, d \in Disks : hold[h][d].st \in {"none", "open", "reopen"}
    /\ \A d \in Disks : att[d] \subseteq Machines

-----------------------------------------------------------------------------
\* What the controller sees.

M == objs[gen]
Free(r) == r.st = "released" /\ r.reserved = None
\* The pool is the machines the host pods run on; a pod terminating keeps
\* its machine in it.
InPool(m) == pool[m] \in {"in", "leaving"}
Leaving(h) == pool[Machine(h)] # "in"

\* Host h holds d open and the controller can reach it.
Holds(h, d) == wanted[h] /\ hold[h][d].st = "open"

\* A disk assigned in r to a member that is not on the machine it is
\* reserved for is read; the member writes its own.
Reading(r) == r.member # None /\ r.reserved # Machine(r.member)

\* Whether host h reports d empty: no live entry, and, for its own disk, no
\* VM. With the fix of B7, a host reports its own disk empty only once the
\* membership it holds releases it, and from then on takes no VM.
ReleasedHere(h, d) ==
    LET r == objs[view[h]].d[d] IN r.member = h /\ r.st = "releasing"
ReportsEmpty(h, d) ==
    LET r == objs[hold[h][d].a].d[d] IN
    /\ live[d] = {}
    /\ Reading(r) \/ (~vms[h] /\ (ReleasedHere(h, d) \/ "report-before-release" \in Bugs))

\* The cloud lists d. Under the bug "stale-list", B8, a list read before a
\* write of the membership that lost its compare-and-set, and taken up again
\* over the newer generation, still lists a disk deleted since. The design
\* with the fix acts on a list only over the generation read before it.
Listed(d) == exists[d] = "yes" \/ ("stale-list" \in Bugs /\ exists[d] = "deleted")

-----------------------------------------------------------------------------
\* The steps of the controller, each a guard on the newest generation M and
\* the generation it writes. Idle below asks whether any is enabled.

DrainOk(h) == M.m[h] = "active" /\ (~wanted[h] \/ Leaving(h))
Drained(h) ==
    [M EXCEPT !.m[h] = "draining",
              !.d = [x \in Disks |-> IF M.d[x].member = h /\ M.d[x].st \in {"attaching", "serving"}
                                     THEN [M.d[x] EXCEPT !.st = "releasing"] ELSE M.d[x]]]

\* A releasing disk is let go once its member's host does not hold it and the
\* cloud has it on no machine. Its mark stays.
LetOk(d) ==
    LET r == M.d[d] IN
    /\ r.st = "releasing" /\ ~Holds(r.member, d) /\ att[d] = {}
Let(d) == [M EXCEPT !.d[d] = [@ EXCEPT !.st = "released", !.member = None, !.assigned = 0]]

\* A disk read or releasing whose holder reports it empty is marked empty;
\* one marked whose holder reports it is not is unmarked. With the fix of
\* B7, only a report of the disk's assignment counts. A writer's own disk
\* serving is never marked, though under part 1 of that fix its host never
\* reports it empty, so the model finds no harm in marking it.
MarkOk(d) ==
    LET r == M.d[d] IN
    /\ r.st \in {"serving", "releasing"} /\ Holds(r.member, d)
    /\ hold[r.member][d].a = r.assigned \/ "mark-any-assignment" \in Bugs
    /\ IF r.empty
       THEN ~ReportsEmpty(r.member, d)
       ELSE /\ r.st = "releasing" \/ Reading(r)
            /\ ReportsEmpty(r.member, d)
Mark(d) == [M EXCEPT !.d[d].empty = ~@]

\* A read disk marked empty is released.
ReleaseReadOk(d) ==
    LET r == M.d[d] IN r.st = "serving" /\ r.empty /\ wanted[r.member] /\ Reading(r)
ReleaseRead(d) == [M EXCEPT !.d[d].st = "releasing"]

\* A disk the cloud lists and the membership does not is one a controller
\* has just made: it is added, free and empty.
AddOk(d) == Listed(d) /\ M.d[d].st = "absent"
Add(d) == [M EXCEPT !.d[d] = [NoDisk EXCEPT !.st = "released", !.empty = TRUE]]

\* A disk being deleted that the cloud no longer lists leaves the membership.
RemoveOk(d) == M.d[d].st = "deleting" /\ exists[d] = "deleted"
Remove(d) == [M EXCEPT !.d[d] = NoDisk]

LeaveOk(h) == M.m[h] = "draining" /\ \A d \in Disks : M.d[d].member # h
Leave(h) == [M EXCEPT !.m[h] = "none"]

JoinOk(h) == M.m[h] = "none" /\ wanted[h] /\ ~Leaving(h)
Join(h) == [M EXCEPT !.m[h] = "active"]

UnreserveOk(d) == M.d[d].st = "released" /\ M.d[d].reserved # None /\ ~InPool(M.d[d].reserved)
Unreserve(d) == [M EXCEPT !.d[d].reserved = None]

\* A machine in the pool with no disk reserved for it is given a free disk
\* that is empty.
ReserveOk(d, m) ==
    /\ InPool(m) /\ \A x \in Disks : M.d[x].reserved # m
    /\ Free(M.d[d]) /\ (M.d[d].empty \/ "reserve-not-empty" \in Bugs)
Reserve(d, m) == [M EXCEPT !.d[d].reserved = m]

\* A member is assigned the disk reserved for its machine, which it writes;
\* a free disk that is not empty is assigned to any member, which reads it.
\* An assigned disk is not empty.
Takers == {h \in Hosts : M.m[h] = "active" /\ wanted[h] /\ ~Leaving(h)}
WriterOk(d, h) == h \in Takers /\ M.d[d].st = "released" /\ M.d[d].reserved = Machine(h)
ReaderOk(d, h) == h \in Takers /\ Free(M.d[d]) /\ ~M.d[d].empty
Assign(d, h) ==
    [M EXCEPT !.d[d] = [@ EXCEPT !.st = "attaching", !.member = h, !.assigned = gen + 1, !.empty = FALSE]]

ServeOk(d) == M.d[d].st = "attaching" /\ Holds(M.d[d].member, d)
Serve(d) == [M EXCEPT !.d[d].st = "serving"]

\* A free and empty disk, an hour on, is marked deleting.
DeletingOk(d) == Free(M.d[d]) /\ (M.d[d].empty \/ "delete-not-empty" \in Bugs)
Deleting(d) == [M EXCEPT !.d[d].st = "deleting"]

Changes ==
    \/ \E h \in Hosts : DrainOk(h) \/ LeaveOk(h) \/ JoinOk(h)
    \/ \E d \in Disks : LetOk(d) \/ MarkOk(d) \/ ReleaseReadOk(d) \/ AddOk(d) \/ RemoveOk(d) \/ ServeOk(d)
                        \/ UnreserveOk(d) \/ DeletingOk(d)
    \/ \E d \in Disks, m \in Machines : ReserveOk(d, m)
    \/ \E d \in Disks, h \in Hosts : WriterOk(d, h) \/ ReaderOk(d, h)

Write(next) ==
    /\ gen < MaxGen
    /\ objs' = [objs EXCEPT ![gen + 1] = next]
    /\ gen' = gen + 1
    /\ UNCHANGED <<hostVars, cloudVars, diskVars, pool, countVars>>

Change ==
    \/ \E h \in Hosts :
        \/ DrainOk(h) /\ Write(Drained(h))
        \/ LeaveOk(h) /\ Write(Leave(h))
        \/ JoinOk(h) /\ Write(Join(h))
    \/ \E d \in Disks :
        \/ LetOk(d) /\ Write(Let(d))
        \/ MarkOk(d) /\ Write(Mark(d))
        \/ ReleaseReadOk(d) /\ Write(ReleaseRead(d))
        \/ AddOk(d) /\ Write(Add(d))
        \/ RemoveOk(d) /\ Write(Remove(d))
        \/ ServeOk(d) /\ Write(Serve(d))
        \/ UnreserveOk(d) /\ Write(Unreserve(d))
        \/ DeletingOk(d) /\ Write(Deleting(d))
    \/ \E d \in Disks, m \in Machines : ReserveOk(d, m) /\ Write(Reserve(d, m))
    \/ \E d \in Disks, h \in Hosts : (WriterOk(d, h) \/ ReaderOk(d, h)) /\ Write(Assign(d, h))

-----------------------------------------------------------------------------
\* The controller acts on the cloud from a generation S it read, as Carry
\* does: a disk attaching or serving goes to its member's machine, a free
\* disk reserved for a machine in the pool goes to that machine at once, a
\* releasing disk stays while its holder holds it, and every other disk
\* goes nowhere. A member out of reach keeps what it has.

Target(S, d) ==
    LET r == S.d[d] IN
    CASE r.st \in {"attaching", "serving"} /\ wanted[r.member] -> Machine(r.member)
      [] r.st = "released" /\ r.reserved # None /\ InPool(r.reserved) -> r.reserved
      [] OTHER -> None

Keep(S, d) ==
    LET r == S.d[d] IN
    \/ r.st \in {"attaching", "serving"} /\ ~wanted[r.member]
    \/ r.st = "releasing" /\ Holds(r.member, d)

AttachOk(S, d) == exists[d] = "yes" /\ att[d] = {} /\ Target(S, d) # None

DetachOk(S, d, m) == m \in att[d] /\ ~Keep(S, d) /\ m # Target(S, d)

\* A machine in the pool has no disk reserved, and there is no free empty
\* disk to reserve and no disk listed and not yet added: the controller
\* creates one, in the cloud only. The next pass that lists it adds it.
CreateWanted(S) ==
    /\ \E m \in Machines : InPool(m) /\ \A x \in Disks : S.d[x].reserved # m
    /\ ~\E x \in Disks : Free(S.d[x]) /\ S.d[x].empty
    /\ ~\E x \in Disks : exists[x] = "yes" /\ S.d[x].st = "absent"

DeleteOk(S, d) == S.d[d].st = "deleting" /\ exists[d] = "yes" /\ att[d] = {}

Attach(d) ==
    \E s \in 0..gen :
        /\ AttachOk(objs[s], d)
        /\ att' = [att EXCEPT ![d] = {Target(objs[s], d)}]
        /\ UNCHANGED <<objs, gen, hostVars, exists, diskVars, pool, countVars>>

\* Every process of m loses the device.
Detach(d, m) ==
    \E s \in 0..gen :
        /\ DetachOk(objs[s], d, m)
        /\ att' = [att EXCEPT ![d] = @ \ {m}]
        /\ hold' = [h \in Hosts |-> IF Machine(h) = m THEN [hold[h] EXCEPT ![d] = NoHold] ELSE hold[h]]
        /\ UNCHANGED <<objs, gen, view, vms, started, alive, wanted, exists, diskVars, pool, countVars>>

\* The cloud names a new disk at random; here it is the first not yet made.
Create(d) ==
    \E s \in 0..gen :
        /\ CreateWanted(objs[s])
        /\ exists[d] = "no" /\ \A x \in Disks : x < d => exists[x] # "no"
        /\ exists' = [exists EXCEPT ![d] = "yes"]
        /\ UNCHANGED <<objs, gen, hostVars, att, diskVars, pool, countVars>>

Delete(d) ==
    \E s \in 0..gen :
        /\ DeleteOk(objs[s], d)
        /\ exists' = [exists EXCEPT ![d] = "deleted"]
        /\ lostLive' = (lostLive \/ live[d] # {})
        /\ UNCHANGED <<objs, gen, hostVars, att, diskVars, pool, losses, leaves, staleWrite>>

-----------------------------------------------------------------------------
\* The hosts.

\* The assignment the membership host h holds gives it to open d, 0 for none.
Assigned(h, d) ==
    LET r == objs[view[h]].d[d]
    IN IF r.member = h /\ r.st \in {"attaching", "serving"} THEN r.assigned ELSE 0

\* The membership h holds still assigns d to it under assignment a.
Still(h, d, a) ==
    LET r == objs[view[h]].d[d]
    IN r.member = h /\ r.st \in {"attaching", "serving", "releasing"} /\ r.assigned = a

\* The membership h holds releases d, held under a, and marks it empty.
Marked(h, d, a) ==
    LET r == objs[view[h]].d[d]
    IN r.member = h /\ r.st = "releasing" /\ r.assigned = a /\ r.empty

Own(h, d) == Machine(h) = objs[hold[h][d].a].d[d].reserved

Read(h) ==
    /\ alive[h] /\ view[h] < gen
    /\ view' = [view EXCEPT ![h] = gen]
    /\ UNCHANGED <<objs, gen, hold, vms, started, alive, wanted, cloudVars, diskVars, pool, countVars>>

\* Open d under assignment a, or open it again after a close that found live
\* entries.
CanOpen(h, d, a) ==
    /\ Machine(h) \in att[d]
    /\ ~\E o \in Hosts \ {h} : Machine(o) = Machine(h) /\ hold[o][d].st \in {"open", "reopen"}
    /\ lease[d] <= a

OpenOk(h, d) ==
    /\ alive[h] /\ hold[h][d].st = "none" /\ Assigned(h, d) > 0
    /\ CanOpen(h, d, Assigned(h, d))

Open(h, d) ==
    /\ OpenOk(h, d)
    /\ lease' = [lease EXCEPT ![d] = Assigned(h, d)]
    /\ hold' = [hold EXCEPT ![h][d] = [st |-> "open", a |-> Assigned(h, d)]]
    /\ UNCHANGED <<objs, gen, view, vms, started, alive, wanted, cloudVars, live, pool, countVars>>

ReopenOk(h, d) == alive[h] /\ hold[h][d].st = "reopen"

Reopen(h, d) ==
    /\ ReopenOk(h, d)
    /\ IF Still(h, d, hold[h][d].a) /\ CanOpen(h, d, hold[h][d].a)
       THEN /\ lease' = [lease EXCEPT ![d] = hold[h][d].a]
            /\ hold' = [hold EXCEPT ![h][d].st = "open"]
       ELSE /\ hold' = [hold EXCEPT ![h][d] = NoHold]
            /\ UNCHANGED lease
    /\ UNCHANGED <<objs, gen, view, vms, started, alive, wanted, cloudVars, live, pool, countVars>>

\* The orchestrator places a VM on a host whose own disk serves in a
\* membership it read, which may be old. With the fix of B7, the host
\* refuses it once the membership it holds releases its own disk.
Place(h) ==
    /\ alive[h] /\ ~vms[h]
    /\ \E d \in Disks :
        /\ hold[h][d].st = "open" /\ Own(h, d)
        /\ ~ReleasedHere(h, d) \/ "place-on-releasing" \in Bugs
        /\ \E s \in 0..gen :
            LET r == objs[s].d[d]
            IN objs[s].m[h] = "active" /\ r.member = h /\ r.st = "serving" /\ r.reserved = Machine(h)
    /\ vms' = [vms EXCEPT ![h] = TRUE]
    /\ UNCHANGED <<objs, gen, view, hold, started, alive, wanted, cloudVars, diskVars, pool, countVars>>

\* The host's VMs move away.
Unplace(h) ==
    /\ vms[h]
    /\ vms' = [vms EXCEPT ![h] = FALSE]
    /\ UNCHANGED <<objs, gen, view, hold, started, alive, wanted, cloudVars, diskVars, pool, countVars>>

\* A VM flushes: the host writes an entry into its own disk, without reading
\* the lease.
WriteEntry(h, d) ==
    /\ alive[h] /\ vms[h] /\ hold[h][d].st = "open" /\ Own(h, d)
    /\ Machine(h) \notin live[d] \/ lease[d] # hold[h][d].a
    /\ live' = [live EXCEPT ![d] = @ \cup {Machine(h)}]
    /\ staleWrite' = (staleWrite \/ lease[d] # hold[h][d].a)
    /\ UNCHANGED <<objs, gen, hostVars, cloudVars, lease, pool, losses, leaves, lostLive>>

\* A checkpoint drops a machine's entries on d.
Trim(d, m) ==
    /\ m \in live[d]
    /\ live' = [live EXCEPT ![d] = @ \ {m}]
    /\ UNCHANGED <<objs, gen, hostVars, cloudVars, lease, pool, countVars>>

\* A host closes a disk its membership no longer assigns it under the
\* assignment it holds it under, and a releasing disk once that membership
\* marks it empty. A close that finds live entries opens the disk again.
CloseOk(h, d) ==
    /\ alive[h] /\ hold[h][d].st = "open"
    /\ ~Still(h, d, hold[h][d].a) \/ Marked(h, d, hold[h][d].a)

Close(h, d) ==
    /\ CloseOk(h, d)
    /\ hold' = [hold EXCEPT ![h][d] = IF live[d] = {} \/ ~Still(h, d, hold[h][d].a)
                                      THEN NoHold ELSE [@ EXCEPT !.st = "reopen"]]
    /\ UNCHANGED <<objs, gen, view, vms, started, alive, wanted, cloudVars, diskVars, pool, countVars>>

\* A host starts on a machine in the pool where no host is alive: a new
\* identity, or one that started before, again.
Boot(h) ==
    /\ ~alive[h] /\ pool[Machine(h)] = "in"
    /\ started[h] => losses < MaxLosses
    /\ \A o \in Hosts : Machine(o) = Machine(h) => ~alive[o]
    /\ started' = [started EXCEPT ![h] = TRUE]
    /\ alive' = [alive EXCEPT ![h] = TRUE]
    /\ wanted' = [wanted EXCEPT ![h] = TRUE]
    /\ view' = [view EXCEPT ![h] = gen]
    /\ losses' = IF started[h] THEN losses + 1 ELSE losses
    /\ UNCHANGED <<objs, gen, hold, vms, cloudVars, diskVars, pool, leaves, staleWrite, lostLive>>

\* A host dies; its handles and VMs go with it, and its machine keeps its
\* disks.
Crash(h) ==
    /\ alive[h] /\ losses < MaxLosses
    /\ alive' = [alive EXCEPT ![h] = FALSE]
    /\ wanted' = [wanted EXCEPT ![h] = FALSE]
    /\ hold' = [hold EXCEPT ![h] = [d \in Disks |-> NoHold]]
    /\ vms' = [vms EXCEPT ![h] = FALSE]
    /\ losses' = losses + 1
    /\ UNCHANGED <<objs, gen, view, started, cloudVars, diskVars, pool, leaves, staleWrite, lostLive>>

\* A host goes quiet: the controller cannot reach it, and it goes on.
Quiet(h) ==
    /\ wanted[h] /\ losses < MaxLosses
    /\ wanted' = [wanted EXCEPT ![h] = FALSE]
    /\ losses' = losses + 1
    /\ UNCHANGED <<objs, gen, view, hold, vms, started, alive, cloudVars, diskVars, pool, leaves, staleWrite, lostLive>>

Back(h) ==
    /\ alive[h] /\ ~wanted[h]
    /\ wanted' = [wanted EXCEPT ![h] = TRUE]
    /\ UNCHANGED <<objs, gen, view, hold, vms, started, alive, cloudVars, diskVars, pool, countVars>>

-----------------------------------------------------------------------------
\* The pool: a machine joins; is chosen to leave, so its host drains; and is
\* deleted, with its host and its attachments, drained or not.

JoinPool(m) ==
    /\ pool[m] = "out"
    /\ pool' = [pool EXCEPT ![m] = "in"]
    /\ UNCHANGED <<objs, gen, hostVars, cloudVars, diskVars, countVars>>

Choose(m) ==
    /\ pool[m] = "in" /\ leaves < MaxLeaves
    /\ pool' = [pool EXCEPT ![m] = "leaving"]
    /\ leaves' = leaves + 1
    /\ UNCHANGED <<objs, gen, hostVars, cloudVars, diskVars, losses, staleWrite, lostLive>>

Gone(m) ==
    /\ pool[m] = "leaving"
    /\ pool' = [pool EXCEPT ![m] = "gone"]
    /\ alive' = [h \in Hosts |-> alive[h] /\ Machine(h) # m]
    /\ wanted' = [h \in Hosts |-> wanted[h] /\ Machine(h) # m]
    /\ vms' = [h \in Hosts |-> vms[h] /\ Machine(h) # m]
    /\ hold' = [h \in Hosts |-> IF Machine(h) = m THEN [d \in Disks |-> NoHold] ELSE hold[h]]
    /\ att' = [d \in Disks |-> att[d] \ {m}]
    /\ UNCHANGED <<objs, gen, view, started, exists, diskVars, countVars>>

Next ==
    \/ Change
    \/ \E d \in Disks : Attach(d) \/ Create(d) \/ Delete(d) \/ \E m \in Machines : Detach(d, m) \/ Trim(d, m)
    \/ \E h \in Hosts : Read(h) \/ Boot(h) \/ Crash(h) \/ Quiet(h) \/ Back(h) \/ Place(h) \/ Unplace(h)
    \/ \E h \in Hosts, d \in Disks : Open(h, d) \/ Reopen(h, d) \/ WriteEntry(h, d) \/ Close(h, d)
    \/ \E m \in Machines : JoinPool(m) \/ Choose(m) \/ Gone(m)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
\* No two hosts hold a journal disk open at once: one writer, or one reader.
OneHolder ==
    \A d \in Disks : Cardinality({h \in Hosts : hold[h][d].st \in {"open", "reopen"}}) <= 1

\* No host writes a disk while another assignment holds its lease.
NoStaleWrite == ~staleWrite

\* A host writes only the disk reserved for its machine: the membership
\* reserves a disk a host holds to write for that host's machine, or, once
\* the machine left the pool, for none.
WriterOnItsMachine ==
    \A h \in Hosts, d \in Disks :
        hold[h][d].st = "open" /\ Own(h, d) => M.d[d].reserved \in {Machine(h), None}

\* A disk the membership marks empty, and no host holds, holds no live
\* entry.
EmptyIsTrue ==
    \A d \in Disks : (M.d[d].empty /\ \A h \in Hosts : hold[h][d].st = "none") => live[d] = {}

\* A disk with live entries is reserved for no machine but its writer's.
NoLiveReuse == \A d \in Disks : M.d[d].reserved # None => live[d] \subseteq {M.d[d].reserved}

\* No disk with live entries is deleted, or on its way to be.
NoLiveDelete == ~lostLive /\ \A d \in Disks : M.d[d].st = "deleting" => live[d] = {}

\* The cloud attaches a disk to one machine at a time.
AttachedOnce == \A d \in Disks : Cardinality(att[d]) <= 1

\* The membership offers no disk the cloud has deleted: a disk it lists is
\* in the cloud, or on its way out. Such a disk could be reserved and
\* assigned, never attached, and never let go.
ListedExists == \A d \in Disks : M.d[d].st \notin {"absent", "deleting"} => exists[d] = "yes"

-----------------------------------------------------------------------------
\* Convergence. Whenever the controller has nothing left to do, from the
\* newest generation and on the cloud, and the hosts have read the
\* membership and done what it asks, no entry is live and no draining host
\* runs a VM, then the membership and the cloud agree and every reachable
\* host in the pool writes a disk reserved for its machine. A crash anywhere
\* between a call of the cloud and a write of the membership leaves a step
\* to do, never a state that stays wrong.

Idle ==
    /\ ~Changes
    /\ ~CreateWanted(M)
    /\ \A d \in Disks : ~AttachOk(M, d) /\ ~DeleteOk(M, d) /\ \A m \in Machines : ~DetachOk(M, d, m)

Settled ==
    /\ \A h \in Hosts : alive[h] => view[h] = gen
    /\ \A h \in Hosts, d \in Disks : ~OpenOk(h, d) /\ ~ReopenOk(h, d) /\ ~CloseOk(h, d)
    /\ \A h \in Hosts : vms[h] => M.m[h] = "active"
    /\ \A d \in Disks : live[d] = {}

Converged ==
    /\ \A d \in Disks : (exists[d] = "yes") = (M.d[d].st # "absent")
    /\ \A d \in Disks : M.d[d].st \in {"absent", "released", "serving"}
    /\ \A d \in Disks : att[d] \subseteq {Target(M, d)}
    /\ \A h \in Hosts : M.m[h] # "draining"
    /\ \A h \in Hosts :
        wanted[h] /\ M.m[h] = "active" /\ ~Leaving(h) =>
            \E d \in Disks : M.d[d].st = "serving" /\ M.d[d].member = h /\ M.d[d].reserved = Machine(h)

Converges == Idle /\ Settled => Converged

=============================================================================
