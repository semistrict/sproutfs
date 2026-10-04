----------------------------- MODULE Membership -----------------------------
(***************************************************************************)
(* The membership (package membership, docs/hosting.md#the-membership):   *)
(* one object in the object store that says which disk each host serves   *)
(* and which disks rank a window, changed only by compare-and-set, and the *)
(* protocol every request that routes by it follows.                      *)
(*                                                                         *)
(* The object is a line of generations. A writer reads it and later writes *)
(* the next generation, conditional on the generation it read; any process *)
(* may write, the hosts and a controller among them. A step releases a    *)
(* disk, lets it go, assigns it, serves it, removes it or lists it again, *)
(* as Step admits. A disk's member lets it go only once it has read the    *)
(* release itself, so it has stopped serving it; a controller lets go a    *)
(* disk whose member is dead.                                              *)
(*                                                                         *)
(* Each host holds a copy, a generation it read, which only moves forward. *)
(* A host serves a disk while its copy assigns the disk to it, serving.   *)
(* A request names the sender's generation and the disk. The holder reads *)
(* the object first when it is behind, answers stale when the generations *)
(* differ, and otherwise serves the disk only if its copy has it serve it. *)
(* A keep is a request for a window's stripe, taken only if the holder's  *)
(* copy ranks the disk for the window. Ranks are a fixed order of disks   *)
(* per window, which the listed disks keep, as rendezvous does.            *)
(*                                                                         *)
(* Left out: the stripes' bytes and the code, which spec/diskcache        *)
(* checks; a reply lost after a write landed, which the writer settles by *)
(* reading the object back, and which changes no generation; weights;    *)
(* attaching a network disk, which TASK-86 carries out.                    *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets

CONSTANTS
    N,          \* hosts 1..N, and disks 1..N, disk i first on host i
    MaxGen,     \* the generations a run may write
    MaxCrashes, \* hosts that may die
    Bugs        \* defects put back, for the mutants

Hosts == 1..N
Disks == 1..N
None == 0
Controller == N + 1
Writers == Hosts \cup {Controller}
States == {"attaching", "serving", "releasing", "released"}
\* Two windows: one ranks the lowest-numbered disk listed first, the other
\* the highest.
Windows == {"low", "high"}

Initial == [listed |-> Disks, assign |-> [d \in Disks |-> d], state |-> [d \in Disks |-> "serving"]]

VARIABLES
    objs,     \* objs[g]: the membership at generation g, for every g written
    gen,      \* the generation the object holds
    snap,     \* snap[w]: the generation writer w last read
    view,     \* view[h]: the generation host h holds
    alive,    \* alive[h]
    history,  \* every generation written, in the order the store applied it
    crashes,
    wrong     \* the first broken promise, or "" while none is

vars == <<objs, gen, snap, view, alive, history, crashes, wrong>>

Init ==
    /\ objs = [g \in 0..MaxGen |-> Initial]
    /\ gen = 0
    /\ snap = [w \in Writers |-> 0]
    /\ view = [h \in Hosts |-> 0]
    /\ alive = [h \in Hosts |-> TRUE]
    /\ history = <<>>
    /\ crashes = 0
    /\ wrong = ""

\* The disk ranked for window w under m: the first listed disk in w's order.
Rank(m, w) ==
    IF m.listed = {} THEN {}
    ELSE IF w = "low" THEN {CHOOSE d \in m.listed : \A e \in m.listed : d <= e}
    ELSE {CHOOSE d \in m.listed : \A e \in m.listed : d >= e}

\* Host h serves disk d under m.
Serves(m, h, d) == d \in m.listed /\ m.assign[d] = h /\ m.state[d] = "serving"

\* A host reads the object: its copy moves forward to what it holds now. A
\* writer that is a host reads into its copy too.
Read(w) ==
    /\ snap' = [snap EXCEPT ![w] = gen]
    /\ view' = IF w \in Hosts /\ alive[w] THEN [view EXCEPT ![w] = gen] ELSE view
    /\ UNCHANGED <<objs, gen, alive, history, crashes, wrong>>

Refresh(h) ==
    /\ alive[h]
    /\ view' = [view EXCEPT ![h] = gen]
    /\ UNCHANGED <<objs, gen, snap, alive, history, crashes, wrong>>

Crash(h) ==
    /\ alive[h] /\ crashes < MaxCrashes
    /\ alive' = [alive EXCEPT ![h] = FALSE]
    /\ crashes' = crashes + 1
    /\ UNCHANGED <<objs, gen, snap, view, history, wrong>>

\* The steps a writer may build from the generation it read, as Step admits
\* them.
Release(m, d) ==
    IF d \in m.listed /\ m.state[d] \in {"attaching", "serving"}
    THEN {[m EXCEPT !.state[d] = "releasing"]} ELSE {}

\* Its member lets a disk go once its own copy shows the release, or a
\* controller does once its member is dead.
LetGo(w, m, d) ==
    IF d \in m.listed /\ m.state[d] = "releasing" /\
       (w = m.assign[d] \/ (w = Controller /\ m.assign[d] \in Hosts /\ ~alive[m.assign[d]]))
    THEN {[m EXCEPT !.state[d] = "released", !.assign[d] = None]} ELSE {}

\* A disk is assigned only once released, unless the mutant assigns it from
\* any state.
Assign(m, d, h) ==
    IF d \in m.listed /\ (m.state[d] = "released" \/ "assign-without-release" \in Bugs) /\ m.assign[d] # h
    THEN {[m EXCEPT !.state[d] = "attaching", !.assign[d] = h]} ELSE {}

Serve(m, d) ==
    IF d \in m.listed /\ m.state[d] = "attaching"
    THEN {[m EXCEPT !.state[d] = "serving"]} ELSE {}

Remove(m, d) ==
    IF d \in m.listed /\ m.state[d] = "released" THEN {[m EXCEPT !.listed = @ \ {d}]} ELSE {}

List(m, d) ==
    IF d \notin m.listed THEN {[m EXCEPT !.listed = @ \cup {d}, !.state[d] = "released", !.assign[d] = None]}
    ELSE {}

Changes(w, m) ==
    UNION ({Release(m, d) : d \in Disks} \cup {LetGo(w, m, d) : d \in Disks} \cup
           {Assign(m, d, h) : d \in Disks, h \in Hosts} \cup {Serve(m, d) : d \in Disks} \cup
           {Remove(m, d) : d \in Disks} \cup {List(m, d) : d \in Disks})

\* A writer writes the generation after the one it read, conditional on the
\* object still holding that generation. The mutant writes whatever the
\* object holds now.
Write(w) ==
    /\ snap[w] < MaxGen
    /\ gen = snap[w] \/ "unconditional-write" \in Bugs
    /\ \E next \in Changes(w, objs[snap[w]]) :
        /\ objs' = [objs EXCEPT ![snap[w] + 1] = next]
        /\ gen' = snap[w] + 1
        /\ history' = Append(history, snap[w] + 1)
    /\ snap' = [snap EXCEPT ![w] = snap[w] + 1]
    \* A host holds what it wrote.
    /\ view' = IF w \in Hosts /\ alive[w] /\ view[w] < snap[w] + 1 THEN [view EXCEPT ![w] = snap[w] + 1]
               ELSE view
    /\ UNCHANGED <<alive, crashes, wrong>>

\* The generation a holder answers a request of generation g under: it reads
\* the object first when it is behind. The mutant answers under the copy it
\* holds.
Answering(h, g) ==
    IF view[h] < g /\ "no-generation-check" \notin Bugs THEN gen ELSE view[h]

\* Whether a holder answering under held serves a request of generation g
\* for disk d: only under the request's generation, and only for a disk that
\* generation has it serve. Each mutant drops one of the two checks.
Answers(h, g, held, d) ==
    /\ held = g \/ "no-generation-check" \in Bugs
    /\ Serves(objs[held], h, d) \/ "no-assignment-check" \in Bugs

\* What answering a request of generation g for disk d broke, if anything:
\* a stripe served or placed under a membership the sender and the holder do
\* not both hold, or by a host the sender's membership does not have serve
\* the disk.
Broke(h, g, held, d, what) ==
    IF wrong = "" /\ (held # g \/ ~Serves(objs[g], h, d)) THEN what ELSE wrong

\* A sender asks the host it routes disk d to under its own copy for a
\* stripe of d. Any host may get it: an address may come to belong to
\* another host.
Ask(s, d, h) ==
    /\ alive[s] /\ alive[h]
    /\ LET g == view[s]
           held == Answering(h, g)
       IN /\ Serves(objs[g], objs[g].assign[d], d)
          /\ view' = [view EXCEPT ![h] = held]
          /\ wrong' = IF Answers(h, g, held, d) THEN Broke(h, g, held, d, "served") ELSE wrong
    /\ UNCHANGED <<objs, gen, snap, alive, history, crashes>>

\* A sender keeps window w's stripe on the disk its copy ranks for w. A
\* holder takes it only under the sender's generation, for a disk that
\* generation ranks for w and has it serve.
Keep(s, w, h) ==
    /\ alive[s] /\ alive[h]
    /\ LET g == view[s]
           held == Answering(h, g)
       IN \E d \in Rank(objs[g], w) :
            /\ view' = [view EXCEPT ![h] = held]
            /\ wrong' = IF Answers(h, g, held, d) /\ d \in Rank(objs[held], w)
                         THEN Broke(h, g, held, d, "placed") ELSE wrong
    /\ UNCHANGED <<objs, gen, snap, alive, history, crashes>>

Next ==
    \/ \E w \in Writers : Read(w) \/ Write(w)
    \/ \E h \in Hosts : Refresh(h) \/ Crash(h)
    \/ \E s \in Hosts, d \in Disks, h \in Hosts : Ask(s, d, h)
    \/ \E s \in Hosts, w \in Windows, h \in Hosts : Keep(s, w, h)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
\* No stripe is served or placed under a membership the sender and the
\* holder do not both hold.
OneMembership == wrong = ""

\* The store applies one line of generations: each write is the generation
\* after the last, so none is lost and none goes back.
NoRegress == \A i \in 1..Len(history) : history[i] = i

\* No two live hosts serve one disk at once by the copies they hold: a disk
\* is released, and its member has read that, before it is assigned again.
OneServer ==
    \A d \in Disks : Cardinality({h \in Hosts : alive[h] /\ Serves(objs[view[h]], h, d)}) <= 1
=============================================================================
