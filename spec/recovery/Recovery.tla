----------------------------- MODULE Recovery ------------------------------
(***************************************************************************)
(* The orchestrator's recovery of one VM, against the hosts that may run   *)
(* it, as docs/metadata.md (Fencing and selection) describes it and        *)
(* orchestrator.reopen implements it.                                      *)
(*                                                                         *)
(* An open takes the VM's epoch, which fences whichever host held it: that *)
(* host's guest may go on running, but it can never publish again. So a    *)
(* recovery opens only on positive evidence that the holder is gone: every *)
(* listed host answered its survey, none runs the VM, serves its pages or  *)
(* receives it, and no operation of the orchestrator's own is in flight    *)
(* for it. A survey asks each host in turn, not all at one instant, and    *)
(* the row is read after the survey.                                       *)
(*                                                                         *)
(* Alongside it the orchestrator migrates the VM: the source stops the     *)
(* guest and serves its pages, the destination opens it and runs it, the   *)
(* row says where it went, and the source is released. A host may die;    *)
(* its pod stays listed until something deletes it.                       *)
(*                                                                         *)
(* The property: a recovery never fences a holder that is alive.           *)
(*                                                                         *)
(* Left out: the control record itself, which spec/ownership checks; the   *)
(* pages, which spec/postcopy checks; force, which is an operator's word   *)
(* that a quiet host is gone.                                              *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Hosts,   \* the hosts of the deployment
    Bugs     \* defects to put back, to show the property catches each one

VARIABLES
    holder,    \* the host whose open holds the VM's epoch
    epoch,     \* the VM's epoch, which every open advances
    runs,      \* the hosts whose guest of the VM runs
    serving,   \* the hosts serving pages of the VM they handed over
    receiving, \* the hosts receiving the VM
    alive,     \* the hosts whose process is up
    listed,    \* the hosts whose pod the Kubernetes API lists
    row,       \* the orchestrator's row: "running", "migrating", "recovering"
    mig,       \* the migration: "idle", "stopped", "receiving", "opened", "landed", "done"
    from, to,  \* its source and destination
    rpc,       \* the recovery: "idle", "surveying", "checked", "opening", "done"
    seen,      \* what the survey learned of each host it asked
    asked,     \* the hosts the survey has asked
    expect,    \* the epoch the recovery read before it surveyed
    target,    \* the host the recovery opens on
    fencedLive \* a recovery took the epoch from a holder that was alive

vars == <<holder, epoch, runs, serving, receiving, alive, listed, row, mig, from, to,
          rpc, seen, asked, expect, target, fencedLive>>

None == "none"
Quiet == "quiet"

Init ==
    /\ \E h \in Hosts :
          /\ holder = h
          /\ runs = {h}
    /\ epoch = 1
    /\ serving = {}
    /\ receiving = {}
    /\ alive = Hosts
    /\ listed = Hosts
    /\ row = "running"
    /\ mig = "idle"
    /\ from = None
    /\ to = None
    /\ rpc = "idle"
    /\ seen = [h \in Hosts |-> None]
    /\ asked = {}
    /\ expect = 0
    /\ target = None
    /\ fencedLive = FALSE

hostVars == <<runs, serving, receiving, alive, listed>>
recVars == <<rpc, seen, asked, expect, target>>

(***************************************************************************)
(* An open: the epoch moves to the host that opened, fencing the holder.   *)
(***************************************************************************)
Open(h) ==
    /\ holder' = h
    /\ epoch' = epoch + 1

(***************************************************************************)
(* The migration, which the orchestrator drives with its row in flight.    *)
(***************************************************************************)
MigStart ==
    /\ mig = "idle"
    /\ \E src \in runs, dst \in Hosts :
          /\ src \in alive /\ dst \in alive /\ dst # src
          /\ from' = src /\ to' = dst
    /\ row' = "migrating"
    /\ mig' = "stopped"
    /\ UNCHANGED <<holder, epoch, hostVars, recVars, fencedLive>>

\* The source stops the guest and serves its pages.
MigStop ==
    /\ mig = "stopped" /\ from \in alive /\ from \in runs
    /\ runs' = runs \ {from}
    /\ serving' = serving \cup {from}
    /\ receiving' = receiving \cup {to}
    /\ mig' = "receiving"
    /\ UNCHANGED <<holder, epoch, alive, listed, row, from, to, recVars, fencedLive>>

\* The destination opens the VM and runs it.
MigOpen ==
    /\ mig = "receiving" /\ to \in alive
    /\ Open(to)
    /\ runs' = runs \cup {to}
    /\ receiving' = receiving \ {to}
    /\ mig' = "opened"
    /\ UNCHANGED <<serving, alive, listed, row, from, to, recVars, fencedLive>>

\* The row says where the VM went.
MigLanded ==
    /\ mig = "opened"
    /\ row' = "running"
    /\ mig' = "landed"
    /\ UNCHANGED <<holder, epoch, hostVars, from, to, recVars, fencedLive>>

\* The source is released.
MigRelease ==
    /\ mig = "landed"
    /\ serving' = serving \ {from}
    /\ mig' = "done"
    /\ UNCHANGED <<holder, epoch, runs, receiving, alive, listed, row, from, to,
                   recVars, fencedLive>>

(***************************************************************************)
(* Hosts die, and a dead host's pod is deleted.                            *)
(***************************************************************************)
Die(h) ==
    /\ h \in alive
    /\ alive' = alive \ {h}
    /\ runs' = runs \ {h}
    /\ serving' = serving \ {h}
    /\ receiving' = receiving \ {h}
    /\ UNCHANGED <<holder, epoch, listed, row, mig, from, to, recVars, fencedLive>>

Unlist(h) ==
    /\ h \notin alive /\ h \in listed
    /\ listed' = listed \ {h}
    /\ UNCHANGED <<holder, epoch, runs, serving, receiving, alive, row, mig, from, to,
                   recVars, fencedLive>>

(***************************************************************************)
(* The recovery: read the epoch, survey each listed host in turn, read the *)
(* row, open.                                                              *)
(***************************************************************************)
RecBegin ==
    /\ rpc = "idle"
    /\ rpc' = "surveying"
    /\ expect' = epoch
    /\ UNCHANGED <<holder, epoch, hostVars, row, mig, from, to, seen, asked, target, fencedLive>>

\* One host's answer, at the moment it is asked.
Ask(h) ==
    /\ rpc = "surveying" /\ h \in listed /\ h \notin asked
    /\ asked' = asked \cup {h}
    /\ seen' = [seen EXCEPT ![h] =
          IF h \notin alive THEN Quiet
          ELSE IF h \in runs \/ h \in serving \/ h \in receiving THEN "holds" ELSE "empty"]
    /\ UNCHANGED <<holder, epoch, hostVars, row, mig, from, to, rpc, expect, target, fencedLive>>

\* Every listed host asked: refuse on anything that is not evidence of a loss,
\* then read the row.
RecCheck ==
    /\ rpc = "surveying" /\ listed \subseteq asked
    /\ IF \E h \in asked : seen[h] \in {"holds", Quiet}
       THEN rpc' = "done"
       ELSE rpc' = IF row = "running" THEN "checked" ELSE "done"
    /\ UNCHANGED <<holder, epoch, hostVars, row, mig, from, to, seen, asked, expect, target, fencedLive>>

\* The open, on any live host. It is conditional on the epoch the recovery
\* read before it surveyed, so an open anyone made since refuses it.
RecOpen ==
    /\ rpc = "checked"
    /\ \E h \in alive :
          IF epoch # expect /\ "open-ignores-epoch" \notin Bugs
          THEN /\ rpc' = "done"
               /\ UNCHANGED <<holder, epoch, runs, fencedLive, target>>
          ELSE /\ fencedLive' = (fencedLive \/ holder \in alive)
               /\ Open(h)
               /\ runs' = runs \cup {h}
               /\ target' = h
               /\ rpc' = "done"
    /\ UNCHANGED <<serving, receiving, alive, listed, row, mig, from, to, seen, asked, expect>>

Next ==
    \/ MigStart \/ MigStop \/ MigOpen \/ MigLanded \/ MigRelease
    \/ \E h \in Hosts : Die(h) \/ Unlist(h) \/ Ask(h)
    \/ RecBegin \/ RecCheck \/ RecOpen

Spec == Init /\ [][Next]_vars

\* A recovery never takes the epoch from a holder that is alive.
NoLiveFence == ~fencedLive
=============================================================================
