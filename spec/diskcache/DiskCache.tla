----------------------------- MODULE DiskCache -----------------------------
(***************************************************************************)
(* The cluster's disk cache, as plans/disk-cache-2026-10-02.md designs it: *)
(* which hosts hold a window's stripes, how stripes get there, and what a  *)
(* read returns.                                                           *)
(*                                                                         *)
(* One VM name publishes one checkpoint. Each of its windows is one        *)
(* envelope, named by <<epoch, span>>. A part's PUT may fail before or     *)
(* after it lands, and is retried under the same reference. A retry        *)
(* carries the same bytes; a reference reused for other contents carries   *)
(* other bytes, and the store refuses them under a key it holds. A part    *)
(* is filled once its PUT has succeeded. The VM may be deleted and its     *)
(* name created again under an epoch drawn from Epochs.                    *)
(*                                                                         *)
(* An envelope is K+M stripes, and any K of them decode it. Each host      *)
(* holds a list of caches: the disks of the generation of the membership  *)
(* it holds. Two hosts on two generations exchange no stripe, which       *)
(* spec/membership checks; here each host's list may still be any list    *)
(* the cluster held, as the generation it held when it last asked is, and *)
(* the read and the keep are checked against it. Weighted rendezvous is a  *)
(* fixed order of all                                                      *)
(* hosts per window, which a list keeps: that is the property of           *)
(* rendezvous the design rests on. Stripe i goes on rank                   *)
(* ((i - 1) mod n) + 1 of the n hosts in the list, and ranks 1 to K+M hold *)
(* the window. A read asks each rank it has not marked down for every      *)
(* stripe it holds of the window, of any index, since a join or a leave    *)
(* moves the later ranks by one (B5). It checks each stripe's key and      *)
(* checksum, and decodes from any K stripes of one envelope; a set that    *)
(* mixes envelopes fails the envelope's SHA-256. On a miss it reads the    *)
(* store, and fills if rank 1 gave it the fill right, which rank 1 gives   *)
(* once an interval while it holds nothing of the window. A reader that    *)
(* decoded, and heard from every rank, may send an index no rank holds to  *)
(* a rank that holds fewer stripes than the code puts on it. A cache       *)
(* takes a keep only for a window its own list ranks it for, and drops one *)
(* for a stripe it holds. The filler is held to its own list as well.      *)
(*                                                                         *)
(* Each host's disk is the set of stripes it holds. Eviction may take any  *)
(* of them, and a header may be damaged. How a disk lays its stripes out,  *)
(* evicts them and recovers them is spec/disklog; how much it may hold is  *)
(* spec/disklimit. Nothing here waits, so TLC checks no deadlock here.     *)
(*                                                                         *)
(* Left out: the memory tier and the pager; segments, which are windows    *)
(* filled once the index object lands; more than one checkpoint per VM,    *)
(* since a sequence names one checkpoint (spec/ownership checks it);       *)
(* reclamation; the window index, whose every fault reaches a reader as a  *)
(* wrong stripe from a peer; damaged bytes under a good header, which the  *)
(* envelope's SHA-256 catches as it catches a mixed set; weights; a change *)
(* of code, and a cache file of another deployment, which are misses; the  *)
(* write budget; the bound, the token bucket and the probes' timing, which *)
(* change when a read gives up and not what it returns; pull; the sampled  *)
(* HEAD check.                                                             *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, Sequences

CONSTANTS
    Hosts,      \* 1..N, every host that may ever run
    Founders,   \* the hosts in the list at the start
    K,          \* data stripes
    M,          \* parity stripes
    Spans,      \* the windows of the checkpoint
    Epochs,     \* the epochs a create draws from
    Values,     \* the bytes a part's PUT may carry
    MaxGens,    \* VMs of the one name, each created after the last is deleted
    MaxReads,   \* reads across the run
    MaxChanges, \* crashes, leaves and joins across the run
    Faults,     \* faults that may happen: "put", "answer", "damage", "evict"
    MaxFaults,  \* faults across the run
    Bugs        \* defects to put back, to show the properties catch each one

N == Cardinality(Hosts)
Idx == 1..(K + M)
Idents == Epochs \X Spans
NoBytes == <<0, 0>>
Min(a, b) == IF a < b THEN a ELSE b
Max(a, b) == IF a > b THEN a ELSE b
Least(S) == CHOOSE a \in S : \A b \in S : a <= b

(***************************************************************************)
(* Ranks. Each window orders every host, and a list keeps that order.      *)
(***************************************************************************)
Order(x) == [j \in 1..N |-> ((j + x[1] + x[2] - 1) % N) + 1]
Ranking(L, x) == SelectSeq(Order(x), LAMBDA p : p \in L)
RankSet(L, x) == LET R == Ranking(L, x) IN {R[j] : j \in 1..Min(K + M, Len(R))}
Holder(L, x, i) == LET R == Ranking(L, x) IN R[((i - 1) % Len(R)) + 1]
Mine(L, x, p) == {i \in Idx : Len(Ranking(L, x)) > 0 /\ Holder(L, x, i) = p}

\* A stripe as it lies on disk: the key its header names, the envelope it is
\* a stripe of, its index, and whether its checksum holds. An envelope is
\* <<identity, bytes>>.
Stripe(c, i) == [key |-> c[1], c |-> c, i |-> i, ok |-> TRUE]

VARIABLES
    \* The VM and the store.
    gen,       \* the VM of the name now, counting creates
    epoch,     \* its epoch
    alive,     \* whether it exists
    committed, \* whether its checkpoint committed
    store,     \* identity -> the bytes the store holds, or NoBytes
    known,     \* envelopes whose PUT the writer knows succeeded
    tofill,    \* envelopes a publication has yet to fill
    \* The hosts.
    members,   \* the orchestrator's list of caches
    up,        \* whether each host's process answers
    list,      \* each host's copy of the list
    marked,    \* the hosts each host has marked down
    rights,    \* the windows each host, as rank 1, gave the fill right for
    disk,      \* host -> the stripes it holds
    \* Counters and history, which the properties read and no host does.
    reads, changes, faults,
    want,      \* identity -> the bytes the store last took under it
    wrong,     \* a read returned bytes the store never took under that name
    misplaced, \* a cache took a stripe its own list does not rank it for
    whole,     \* identity -> all K+M stripes have been readable at once
    taken      \* identity -> copies taken since, up to M + 1

vmVars == <<gen, epoch, alive, committed, store, known, tofill>>
listVars == <<members, up, list, marked, rights>>
countVars == <<reads, changes, faults>>
flagVars == <<want, wrong, misplaced>>
vars == <<vmVars, listVars, disk, countVars, flagVars, whole, taken>>

Init ==
    /\ gen = 1
    /\ epoch = Least(Epochs)
    /\ alive = TRUE
    /\ committed = FALSE
    /\ store = [x \in Idents |-> NoBytes]
    /\ known = {}
    /\ tofill = {}
    /\ members = Founders
    /\ up = [h \in Hosts |-> h \in Founders]
    /\ list = [h \in Hosts |-> IF h \in Founders THEN Founders ELSE {}]
    /\ marked = [h \in Hosts |-> {}]
    /\ rights = [h \in Hosts |-> {}]
    /\ disk = [h \in Hosts |-> {}]
    /\ reads = 0
    /\ changes = 0
    /\ faults = 0
    /\ want = [x \in Idents |-> NoBytes]
    /\ wrong = FALSE
    /\ misplaced = FALSE
    /\ whole = [x \in Idents |-> FALSE]
    /\ taken = [x \in Idents |-> 0]

Fault(kind) == kind \in Faults /\ faults < MaxFaults /\ faults' = faults + 1
Change == changes < MaxChanges /\ changes' = changes + 1

(***************************************************************************)
(* What a reader with the cluster's list can read of each window: the      *)
(* copies on its ranks with the right key and a good checksum. A step that *)
(* changes a disk, a host or the list records when all K+M were readable   *)
(* at once, and how many copies have been taken since.                     *)
(***************************************************************************)
Readable(mem, upv, dk, x) ==
    {<<p, st.i>> : <<p, st>> \in {<<p, st>> \in RankSet(mem, x) \X UNION {dk[q] : q \in Hosts} :
                                     upv[p] /\ st \in dk[p] /\ st.key = x /\ st.ok}}
Indices(S) == {t[2] : t \in S}

Track ==
    LET before(x) == Readable(members, up, disk, x)
        after(x) == Readable(members', up', disk', x)
        full(x) == Indices(after(x)) = Idx
    IN /\ whole' = [x \in Idents |-> whole[x] \/ full(x)]
       /\ taken' = [x \in Idents |->
                      IF full(x) THEN 0
                      ELSE Min(M + 1, taken[x] + Cardinality(before(x) \ after(x)))]

Untracked == UNCHANGED <<whole, taken>>

(***************************************************************************)
(* Keeps. A cache takes a stripe if it is up, ranked for the window under  *)
(* its own list, and does not hold it.                                     *)
(***************************************************************************)
Holds(p, x, i) == \E st \in disk[p] : st.key = x /\ st.i = i

Takes(p, st) ==
    /\ up[p]
    /\ ~Holds(p, st.key, st.i)
    /\ (p \in RankSet(list[p], st.key) \/ "keep-unranked" \in Bugs)

\* The hosts in to are offered the stripes sts(p).
Deliver(to, sts(_)) ==
    /\ disk' = [p \in Hosts |-> IF p \in to THEN disk[p] \cup {st \in sts(p) : Takes(p, st)}
                                ELSE disk[p]]
    /\ misplaced' = (misplaced \/
          \E p \in to : \E st \in sts(p) : Takes(p, st) /\ p \notin RankSet(list[p], st.key))

\* A sender with list L sends each rank it has not marked down its stripes
\* of envelope c.
Send(L, skip, c) ==
    Deliver(RankSet(L, c[1]) \ skip, LAMBDA p : {Stripe(c, i) : i \in Mine(L, c[1], p)})

(***************************************************************************)
(* The VM and the store.                                                   *)
(***************************************************************************)
Current == {<<epoch, s>> : s \in Spans}
Landed(x) == \E e \in known : e[1] = x

\* A part's PUT, retried until the writer knows it succeeded. An attempt the
\* store refuses changes nothing, so it is left out.
Put(s, b) ==
    LET x == <<epoch, s>>
        v == <<gen, b>>
        early == IF "fill-before-put" \in Bugs THEN {<<x, v>>} ELSE {}
    IN /\ alive /\ ~committed /\ ~Landed(x)
       /\ store[x] \in {NoBytes, v}
       /\ \/ /\ store' = [store EXCEPT ![x] = v]
             /\ want' = [want EXCEPT ![x] = v]
             /\ known' = known \cup {<<x, v>>}
             /\ tofill' = tofill \cup {<<x, v>>}
             /\ UNCHANGED faults
          \/ /\ Fault("put")  \* landed, and the reply was lost
             /\ store' = [store EXCEPT ![x] = v]
             /\ want' = [want EXCEPT ![x] = v]
             /\ tofill' = tofill \cup early
             /\ UNCHANGED known
          \/ /\ Fault("put")  \* failed before it landed
             /\ tofill' = tofill \cup early
             /\ UNCHANGED <<store, want, known>>
       /\ UNCHANGED <<gen, epoch, alive, committed, listVars, disk, reads, changes,
                      wrong, misplaced>>
       /\ Untracked

Commit ==
    /\ alive /\ ~committed
    /\ \A x \in Current : Landed(x)
    /\ committed' = TRUE
    /\ UNCHANGED <<gen, epoch, alive, store, known, tofill, listVars, disk, countVars,
                   flagVars>>
    /\ Untracked

\* A publication sends a part's stripes once its PUT has succeeded. The VM
\* runs on the first host that is up.
PubFill ==
    /\ \E p \in members : up[p]
    /\ \E c \in tofill :
          /\ tofill' = tofill \ {c}
          /\ LET p == Least({q \in members : up[q]})
             IN Send(list[p], marked[p], c)
    /\ UNCHANGED <<gen, epoch, alive, committed, store, known, listVars, countVars,
                   want, wrong>>
    /\ Track

\* The store deletes the VM's objects. Every disk keeps its stripes.
Delete ==
    /\ alive /\ gen < MaxGens
    /\ alive' = FALSE
    /\ committed' = FALSE
    /\ store' = [x \in Idents |-> IF x[1] = epoch THEN NoBytes ELSE store[x]]
    /\ known' = {}
    /\ tofill' = {}
    /\ UNCHANGED <<gen, epoch, listVars, disk, countVars, flagVars>>
    /\ Untracked

\* The name created again draws its epoch.
Create ==
    /\ ~alive
    /\ gen' = gen + 1
    /\ epoch' \in Epochs
    /\ alive' = TRUE
    /\ UNCHANGED <<committed, store, known, tofill, listVars, disk, countVars, flagVars>>
    /\ Untracked

(***************************************************************************)
(* Reads.                                                                  *)
(***************************************************************************)
Asked(h, x) == {p \in RankSet(list[h], x) \ marked[h] : up[p]}

\* Whether a rank answers with a stripe of that index. A read that asks
\* rank i only for stripe i puts B5 back.
Answers(h, x, p, i) == "read-by-index" \notin Bugs \/ Holder(list[h], x, i) = p

\* The stripes the asked ranks answer with, and from which host.
Named(h, x) == {<<p, st>> \in Asked(h, x) \X UNION {disk[q] : q \in Hosts} :
                   st \in disk[p] /\ st.key = x /\ Answers(h, x, p, st.i)}

Read(h, x) ==
    /\ up[h] /\ reads < MaxReads
    /\ alive /\ committed /\ x \in Current
    /\ \E answers \in {Named(h, x)} \cup
             (IF "answer" \in Faults /\ faults < MaxFaults
              \* A peer answers with a stripe it holds under another key.
              THEN {Named(h, x) \cup {<<p, st>>} :
                       <<p, st>> \in {<<p, st>> \in Asked(h, x) \X UNION {disk[q] : q \in Hosts} :
                                         st \in disk[p] /\ st.key # x}}
              ELSE {}) :
       LET good == {a \in answers : (a[2].key = x \/ "no-key-check" \in Bugs)
                                    /\ (a[2].ok \/ "no-checksum" \in Bugs)}
           of(c) == {a \in good : a[2].c = c}
           decodes == {c \in {a[2].c : a \in good} :
                         Cardinality({a[2].i : a \in of(c)}) >= K}
           silent == {p \in RankSet(list[h], x) \ marked[h] : ~up[p]}
       IN /\ faults' = IF answers = Named(h, x) THEN faults ELSE faults + 1
          /\ \/ \E c \in decodes :
                   \* A hit. A reader that heard from every rank of the
                   \* window may send an index no rank holds to a rank that
                   \* holds fewer stripes than the code puts on it.
                   /\ wrong' = (wrong \/ c # <<x, want[x]>>)
                   /\ \/ UNCHANGED <<disk, misplaced>>
                      \/ /\ RankSet(list[h], x) \subseteq Asked(h, x)
                         /\ \E p \in RankSet(list[h], x), i \in Idx :
                               /\ ~\E q \in RankSet(list[h], x) : Holds(q, x, i)
                               /\ Cardinality({j \in Idx : Holds(p, x, j)})
                                     < Cardinality(Mine(list[h], x, p))
                               /\ Deliver({p}, LAMBDA q : {Stripe(c, i)})
                   /\ UNCHANGED rights
             \/ /\ decodes = {}
                /\ store[x] # NoBytes
                \* A miss: the store answers. Rank 1 gives the first reader
                \* that asks the right to fill, once per window per interval,
                \* while it holds nothing of the window.
                /\ LET c == <<x, store[x]>>
                       first == Ranking(list[h], x)[1]
                   IN IF up[first] /\ first \notin marked[h] /\ x \notin rights[first]
                         /\ ~\E st \in disk[first] : st.key = x
                      THEN /\ rights' = [rights EXCEPT ![first] = @ \cup {x}]
                           /\ Send(list[h], marked[h], c)
                      ELSE UNCHANGED <<rights, disk, misplaced>>
                /\ UNCHANGED wrong
             \/ /\ decodes = {} /\ store[x] = NoBytes
                /\ UNCHANGED <<rights, disk, misplaced, wrong>>
          \* A rank that did not answer is marked down, up to a fifth of the
          \* list and at least one. Its timeouts in a row are this one read.
          /\ marked' = [marked EXCEPT ![h] =
                IF silent # {} /\ Cardinality(@) < Max(1, Cardinality(list[h]) \div 5)
                THEN @ \cup {Least(silent)} ELSE @]
    /\ reads' = reads + 1
    /\ UNCHANGED <<vmVars, members, up, list, changes, want>>
    /\ Track

(***************************************************************************)
(* Disks and hosts.                                                        *)
(***************************************************************************)
\* Eviction, at any time, of any stripe.
Evict(h) ==
    /\ \E st \in disk[h] :
          /\ Fault("evict")
          /\ disk' = [disk EXCEPT ![h] = @ \ {st}]
    /\ UNCHANGED <<vmVars, listVars, reads, changes, flagVars>>
    /\ Track

\* A header damaged so that it names another window.
Damage(h) ==
    /\ \E st \in disk[h], y \in Idents :
          /\ st.ok /\ y # st.key
          /\ Fault("damage")
          /\ disk' = [disk EXCEPT ![h] = (@ \ {st}) \cup {[st EXCEPT !.key = y, !.ok = FALSE]}]
    /\ UNCHANGED <<vmVars, listVars, reads, changes, flagVars>>
    /\ Track

\* The process dies. What it held in memory goes; its cache file stays.
Crash(h) ==
    /\ up[h] /\ h \in members
    /\ Change
    /\ up' = [up EXCEPT ![h] = FALSE]
    /\ marked' = [marked EXCEPT ![h] = {}]
    /\ rights' = [rights EXCEPT ![h] = {}]
    /\ UNCHANGED <<vmVars, members, list, disk, reads, faults, flagVars>>
    /\ Track

Recover(h) ==
    /\ ~up[h] /\ h \in members
    /\ up' = [up EXCEPT ![h] = TRUE]
    /\ list' = [list EXCEPT ![h] = members]
    /\ UNCHANGED <<vmVars, members, marked, rights, disk, countVars, flagVars>>
    /\ Track

\* A drained or lost host leaves the list, and its disk goes with it.
Leave(h) ==
    /\ h \in members /\ Cardinality(members) > 1
    /\ Change
    /\ members' = members \ {h}
    /\ up' = [up EXCEPT ![h] = FALSE]
    /\ list' = [list EXCEPT ![h] = {}]
    /\ marked' = [marked EXCEPT ![h] = {}]
    /\ rights' = [rights EXCEPT ![h] = {}]
    /\ disk' = [disk EXCEPT ![h] = {}]
    /\ UNCHANGED <<vmVars, reads, faults, flagVars>>
    /\ Track

\* A host joins with an empty disk.
Join(h) ==
    /\ h \notin members
    /\ Change
    /\ members' = members \cup {h}
    /\ up' = [up EXCEPT ![h] = TRUE]
    /\ list' = [list EXCEPT ![h] = members \cup {h}]
    /\ UNCHANGED <<vmVars, marked, rights, disk, reads, faults, flagVars>>
    /\ Track

\* Each host reads the orchestrator's list on a timer.
Refresh(h) ==
    /\ up[h] /\ list[h] # members
    /\ list' = [list EXCEPT ![h] = members]
    /\ UNCHANGED <<vmVars, members, up, marked, rights, disk, countVars, flagVars>>
    /\ Untracked

\* A probe that succeeds clears the mark.
Probe(h, p) ==
    /\ up[h] /\ p \in marked[h] /\ up[p]
    /\ marked' = [marked EXCEPT ![h] = @ \ {p}]
    /\ UNCHANGED <<vmVars, members, up, list, rights, disk, countVars, flagVars>>
    /\ Untracked

\* The interval of a fill right ends.
Interval(h) ==
    /\ rights[h] # {}
    /\ rights' = [rights EXCEPT ![h] = {}]
    /\ UNCHANGED <<vmVars, members, up, list, marked, disk, countVars, flagVars>>
    /\ Untracked

Next ==
    \/ \E s \in Spans, b \in Values : Put(s, b)
    \/ Commit \/ PubFill \/ Delete \/ Create
    \/ \E h \in Hosts, x \in Idents : Read(h, x)
    \/ \E h \in Hosts : Evict(h) \/ Damage(h) \/ Crash(h) \/ Recover(h)
    \/ \E h \in Hosts : Leave(h) \/ Join(h) \/ Refresh(h) \/ Interval(h)
    \/ \E h, p \in Hosts : Probe(h, p)

Spec == Init /\ [][Next]_vars

(***************************************************************************)
(* Properties.                                                             *)
(***************************************************************************)
\* Every read returns the bytes the store last took under the identity it
\* asked for, or misses.
NoWrongBytes == ~wrong

\* Every stripe a cache holds it took under a list it held that ranks it
\* among the window's first K+M.
StripesRanked == ~misplaced

\* A window whose K+M stripes were all readable at once decodes for a reader
\* whose list is the cluster's and who has marked no host down, while no
\* more than M of those copies have been taken.
SurvivesLosses ==
    \A x \in Idents, h \in Hosts :
        LET sees == {t \in Readable(list[h], up, disk, x) : Answers(h, x, t[1], t[2])}
        IN (whole[x] /\ taken[x] <= M /\ up[h] /\ list[h] = members /\ marked[h] = {})
               => Cardinality(Indices(sees)) >= K
=============================================================================
