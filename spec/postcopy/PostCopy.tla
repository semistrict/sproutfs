----------------------------- MODULE PostCopy ------------------------------
(***************************************************************************)
(* A migration's post-copy, as docs/migration.md describes it and          *)
(* vmmigrate/pagesource.go, vmmigrate/receive.go, host/migrate.go and      *)
(* cmd/sproutfs-orchestrator implement it.                                 *)
(*                                                                         *)
(* A migration publishes nothing. The pages no checkpoint holds are only   *)
(* on the source until a destination installs them and its next           *)
(* checkpoint publishes them. The parties are:                             *)
(*                                                                         *)
(*  - the source's page server, which serves those pages, strikes a page   *)
(*    off its book once a reply carrying it has left, refuses a release    *)
(*    while its book holds a page, and gives everything up when its hold   *)
(*    runs out (PageSource.Release, Host.expire);                          *)
(*  - receive attempts, one at a time. Each fetches pages, installs them,  *)
(*    and either reports Done and runs the VM, or is discarded             *)
(*    (Host.receive);                                                      *)
(*  - the orchestrator, which releases the source once a receive reports   *)
(*    Done, retries a failed receive while the source holds the pages,     *)
(*    and writes its row again while it drives the handover. It may crash  *)
(*    and come back. Its survey releases a handover whose row has aged out *)
(*    of flight only once a host runs the VM; one that no host runs or     *)
(*    receives it takes up again, with the handoff the source keeps        *)
(*    (orchestrator.release, orchestrator.resume, Host.Handed).            *)
(*                                                                         *)
(* A host can be lost, and a hold can run out: either may lose the pages,  *)
(* by design. Nothing else may.                                            *)
(*                                                                         *)
(* Left out: which checkpoint the destination opens, which spec/ownership  *)
(* checks; the bulk stream past the pages no checkpoint holds; budgets and *)
(* BUSY, which delay a fetch but decide nothing; forks.                    *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    Pages,        \* the pages no checkpoint holds, at the handoff
    MaxAttempts,  \* receive attempts the orchestrator may start
    MaxFaults,    \* lost replies, discards and crashes across a run
    Bugs          \* defects to put back, to show the invariant catches each one

VARIABLES
    src,       \* "serving", "released", "expired" or "lost"
    book,      \* the pages the source has not struck off
    wire,      \* pages a reply has carried that the destination has not installed
    dst,       \* the attempt in flight: "none", "receiving", "running", "discarded" or "lost"
    has,       \* the pages the current attempt has installed
    attempts,  \* attempts started
    durable,   \* a checkpoint of the destination has published the pages
    orch,      \* "up" or "down"
    row,       \* the orchestrator's row: "migrating" or "running"
    fresh,     \* whether the row is young enough to be taken at its word
    designed,  \* a host or a hold took the pages with it
    handoff,   \* "known", or "lost" with an orchestrator that alone held it
    faults

vars == <<src, book, wire, dst, has, attempts, durable, orch, row, fresh,
          designed, handoff, faults>>

Init ==
    /\ src = "serving"
    /\ book = Pages
    /\ wire = {}
    /\ dst = "none"
    /\ has = {}
    /\ attempts = 0
    /\ durable = FALSE
    /\ orch = "up"
    /\ row = "migrating"
    /\ fresh = TRUE
    /\ designed = FALSE
    /\ handoff = "known"
    /\ faults = 0

Fault == faults < MaxFaults /\ faults' = faults + 1

\* Where the pages are, besides the source.
Installed == dst = "running" /\ has = Pages
Arriving == dst = "receiving" /\ has \cup wire = Pages

\* A loss is by design when nothing but the host or hold that went had them.
Unsaved == ~durable /\ ~Installed

(***************************************************************************)
(* The source.                                                             *)
(***************************************************************************)
\* A reply leaves with one page; the page is struck off once it has left.
Send(p) ==
    /\ src = "serving" /\ dst = "receiving"
    /\ p \notin has \cup wire
    /\ wire' = wire \cup {p}
    /\ book' = IF "strike-before-send" \in Bugs THEN book ELSE book \ {p}
    /\ UNCHANGED <<src, dst, has, attempts, durable, orch, row, fresh, designed, handoff, faults>>

\* PageSource.Release: refused while the book holds a page.
Released == IF src = "serving" /\ (book = {} \/ "release-ignores-book" \in Bugs)
            THEN src' = "released" ELSE UNCHANGED src

Expire ==
    /\ src = "serving"
    /\ src' = "expired"
    /\ designed' = (designed \/ Unsaved)
    /\ UNCHANGED <<book, wire, dst, has, attempts, durable, orch, row, fresh, faults, handoff>>

SourceLost ==
    /\ src \in {"serving", "released"}
    /\ src' = "lost"
    /\ designed' = (designed \/ (src = "serving" /\ Unsaved))
    /\ UNCHANGED <<book, wire, dst, has, attempts, durable, orch, row, fresh, faults, handoff>>

(***************************************************************************)
(* A receive attempt.                                                      *)
(***************************************************************************)
\* The orchestrator starts an attempt while the source may still hold the
\* pages and no attempt is in flight.
Start ==
    /\ orch = "up" /\ row = "migrating" /\ handoff = "known"
    /\ src = "serving"
    /\ dst \in {"none", "discarded", "lost"}
    /\ attempts < MaxAttempts
    /\ dst' = "receiving"
    /\ has' = {}
    /\ wire' = {}
    /\ attempts' = attempts + 1
    /\ fresh' = TRUE
    /\ UNCHANGED <<src, book, durable, orch, row, designed, handoff, faults>>

Install(p) ==
    /\ dst = "receiving" /\ p \in wire
    /\ has' = has \cup {p}
    /\ wire' = wire \ {p}
    /\ UNCHANGED <<src, book, dst, attempts, durable, orch, row, fresh, designed, handoff, faults>>

\* A reply that left the source and never arrived: a connection reset after
\* the kernel took the bytes.
LoseReply(p) ==
    /\ p \in wire
    /\ Fault
    /\ wire' = wire \ {p}
    /\ UNCHANGED <<src, book, dst, has, attempts, durable, orch, row, fresh, designed, handoff>>

\* Done: every page is installed, and the receive returns the running VM.
Done ==
    /\ dst = "receiving" /\ has = Pages
    /\ dst' = "running"
    /\ UNCHANGED <<src, book, wire, has, attempts, durable, orch, row, fresh, designed, handoff, faults>>

\* Host.receive gives a received VM up, at any point before it returns: its
\* caller's context ended, registering it failed, or Done failed. What it
\* installed goes with it.
Discard ==
    /\ dst = "receiving"
    /\ Fault
    /\ dst' = "discarded"
    /\ has' = {}
    /\ wire' = {}
    /\ UNCHANGED <<src, book, attempts, durable, orch, row, fresh, designed, handoff>>

Checkpoint ==
    /\ Installed /\ ~durable
    /\ durable' = TRUE
    /\ UNCHANGED <<src, book, wire, dst, has, attempts, orch, row, fresh, designed, handoff, faults>>

DestLost ==
    /\ dst \in {"receiving", "running"}
    /\ dst' = "lost"
    /\ designed' = (designed \/ (Installed /\ ~durable /\ src # "serving"))
    /\ has' = {}
    /\ wire' = {}
    /\ UNCHANGED <<src, book, attempts, durable, orch, row, fresh, faults, handoff>>

(***************************************************************************)
(* The orchestrator.                                                       *)
(***************************************************************************)
\* The receive returned the running VM: the row says so, and the source is
\* released.
Landed ==
    /\ orch = "up" /\ row = "migrating" /\ dst = "running"
    /\ row' = "running"
    /\ Released
    /\ UNCHANGED <<book, wire, dst, has, attempts, durable, orch, fresh, designed, handoff, faults>>

\* A survey releases a handover whose row is not in flight, or has aged out
\* of it, unless a host reports a receive of it in flight.
Reconcile ==
    /\ orch = "up" /\ src = "serving"
    /\ (row = "running" \/ ~fresh)
    /\ (dst # "receiving" \/ "release-under-receive" \in Bugs)
    \* A handover no host runs and none receives is taken up again (Start),
    \* not released. Before B2 was fixed it was released.
    /\ (dst = "running" \/ "release-undriven" \in Bugs)
    /\ Released
    /\ UNCHANGED <<book, wire, dst, has, attempts, durable, orch, row, fresh, designed, handoff, faults>>

\* The source keeps the handoff for as long as it holds the pages. Before B2
\* was fixed it lived in the orchestrator alone, and went with it.
Crash ==
    /\ orch = "up"
    /\ Fault
    /\ orch' = "down"
    /\ handoff' = IF "handoff-in-memory" \in Bugs THEN "lost" ELSE handoff
    /\ UNCHANGED <<src, book, wire, dst, has, attempts, durable, row, fresh, designed>>

\* The row ages while nothing writes it: past inFlightFor, which is under the
\* hold, it is no longer taken at its word. A live orchestrator writes it on
\* each look at the source while a receive runs, and before each retry.
Age ==
    /\ fresh
    /\ orch = "down" \/ ("rows-age-while-driven" \in Bugs /\ dst = "receiving")
    /\ fresh' = FALSE
    /\ UNCHANGED <<src, book, wire, dst, has, attempts, durable, orch, row, designed, handoff, faults>>

Restart ==
    /\ orch = "down"
    /\ orch' = "up"
    /\ UNCHANGED <<src, book, wire, dst, has, attempts, durable, row, fresh, designed, handoff, faults>>

Next ==
    \/ \E p \in Pages : Send(p) \/ Install(p) \/ LoseReply(p)
    \/ Expire \/ SourceLost
    \/ Start \/ Done \/ Discard \/ Checkpoint \/ DestLost
    \/ Landed \/ Reconcile \/ Crash \/ Age \/ Restart

Spec == Init /\ [][Next]_vars

(***************************************************************************)
\* Every page no checkpoint holds is somewhere it can still be had: on the
\* serving source, on its way to a live receive, installed in a destination
\* that runs the VM, or published. Only a lost host or an expired hold may take
\* it, and then it is lost by design.
NoSilentLoss ==
    designed \/ durable \/ src = "serving" \/ Installed \/ Arriving
=============================================================================
