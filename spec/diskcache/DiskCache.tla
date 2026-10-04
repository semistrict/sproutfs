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
(* The deployment's code starts as K+M, and may be changed on purpose to   *)
(* NextK+NextM. A host reads the code with the list, and holds the codes   *)
(* before it as earlier codes. An envelope under the code k+m is k+m       *)
(* stripes, and any k of them decode it. Each stripe names its code.       *)
(* Each host holds a list of caches: the disks of the generation of the    *)
(* membership it holds. Two hosts on two generations exchange no stripe,   *)
(* which spec/membership checks; here each host's list may still be any   *)
(* list the cluster held, and the read and the keep are checked against    *)
(* it. Weighted rendezvous is a fixed order of all hosts per window, which *)
(* a list keeps whatever the code: that is the property of rendezvous the  *)
(* design rests on. Under a code of width w, stripe i goes on rank         *)
(* ((i - 1) mod n) + 1 of the n hosts in the list, and ranks 1 to w hold   *)
(* the window. A read asks each rank it has not marked down for every      *)
(* stripe of one code it holds of the window, of any index, since a join   *)
(* or a leave moves the later ranks by one (B5). It checks each stripe's   *)
(* key and checksum, and decodes from any k stripes of one envelope and    *)
(* one code; a set that mixes envelopes fails the envelope's SHA-256. It   *)
(* tries its own code and each earlier one. A window it decodes under an   *)
(* earlier code it fills under its own, as a read of the store fills, if   *)
(* rank 1 gives it the fill right, which rank 1 gives once an interval     *)
(* while it holds nothing of the window under its code. A reader that      *)
(* decoded under its own code, and heard from every rank, may send an      *)
(* index no rank holds to a rank that holds fewer stripes than the code    *)
(* puts on it. A cache takes a keep only under its own list's code, for a  *)
(* window that list ranks it for, and drops one for a stripe it holds. The *)
(* filler is held to its own list as well.                                 *)
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
(* envelope's SHA-256 catches as it catches a mixed set; weights; a code   *)
(* the list no longer names, and a cache file of another deployment, which *)
(* are misses; the order a read tries its codes in, which changes how long *)
(* it takes and not what it returns; the write budget; the bound, the      *)
(* token bucket and the probes' timing, which change when a read gives up  *)
(* and not what it returns; pull; the sampled HEAD check.                  *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, Sequences

CONSTANTS
    Hosts,      \* 1..N, every host that may ever run
    Founders,   \* the hosts in the list at the start
    K,          \* data stripes of the code the deployment starts with
    M,          \* its parity stripes
    NextK,      \* data stripes of the code it may change to, 0 for none
    NextM,      \* its parity stripes
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
\* The deployment's codes, each <<k, m>>, in the order it uses them.
Codes == IF NextK = 0 THEN <<<<K, M>>>> ELSE <<<<K, M>>, <<NextK, NextM>>>>
CodeIdx == 1..Len(Codes)
KOf(j) == Codes[j][1]
MOf(j) == Codes[j][2]
Width(j) == KOf(j) + MOf(j)
Idx(j) == 1..Width(j)
Idents == Epochs \X Spans
NoBytes == <<0, 0>>
Min(a, b) == IF a < b THEN a ELSE b
Max(a, b) == IF a > b THEN a ELSE b
Least(S) == CHOOSE a \in S : \A b \in S : a <= b

(***************************************************************************)
(* Ranks. Each window orders every host, and a list keeps that order. A    *)
(* code takes as many of the first ranks as it is wide.                    *)
(***************************************************************************)
Order(x) == [j \in 1..N |-> ((j + x[1] + x[2] - 1) % N) + 1]
Ranking(L, x) == SelectSeq(Order(x), LAMBDA p : p \in L)
RankSet(L, x, j) == LET R == Ranking(L, x) IN {R[r] : r \in 1..Min(Width(j), Len(R))}
Holder(L, x, i, j) == LET R == Ranking(L, x) IN R[((i - 1) % Len(R)) + 1]
Mine(L, x, p, j) == {i \in Idx(j) : Len(Ranking(L, x)) > 0 /\ Holder(L, x, i, j) = p}

\* A stripe as it lies on disk: the key its header names, the envelope it is
\* a stripe of, its code, its index, and whether its checksum holds. An
\* envelope is <<identity, bytes>>.
Stripe(c, i, j) == [key |-> c[1], c |-> c, k |-> j, i |-> i, ok |-> TRUE]

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
    code,      \* the deployment's code, as its place in Codes
    up,        \* whether each host's process answers
    list,      \* each host's copy of the list
    lcode,     \* the code each host's copy names
    marked,    \* the hosts each host has marked down
    rights,    \* the windows each host, as rank 1, gave the fill right for
    disk,      \* host -> the stripes it holds
    \* Counters and history, which the properties read and no host does.
    reads, changes, faults,
    want,      \* identity -> the bytes the store last took under it
    wrong,     \* a read returned bytes the store never took under that name
    misplaced, \* a cache took a stripe its own list does not rank it for
    whole,     \* identity, code -> all its stripes have been readable at once
    taken      \* identity, code -> copies taken since, up to m + 1

vmVars == <<gen, epoch, alive, committed, store, known, tofill>>
listVars == <<members, code, up, list, lcode, marked, rights>>
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
    /\ code = 1
    /\ up = [h \in Hosts |-> h \in Founders]
    /\ list = [h \in Hosts |-> IF h \in Founders THEN Founders ELSE {}]
    /\ lcode = [h \in Hosts |-> 1]
    /\ marked = [h \in Hosts |-> {}]
    /\ rights = [h \in Hosts |-> {}]
    /\ disk = [h \in Hosts |-> {}]
    /\ reads = 0
    /\ changes = 0
    /\ faults = 0
    /\ want = [x \in Idents |-> NoBytes]
    /\ wrong = FALSE
    /\ misplaced = FALSE
    /\ whole = [x \in Idents |-> [j \in CodeIdx |-> FALSE]]
    /\ taken = [x \in Idents |-> [j \in CodeIdx |-> 0]]

Fault(kind) == kind \in Faults /\ faults < MaxFaults /\ faults' = faults + 1
Change == changes < MaxChanges /\ changes' = changes + 1

(***************************************************************************)
(* What a reader with the cluster's list can read of each window under     *)
(* each code: the copies of that code on its ranks with the right key and  *)
(* a good checksum. A step that changes a disk, a host or the list records *)
(* when all of a code's stripes were readable at once, and how many copies *)
(* have been taken since.                                                  *)
(***************************************************************************)
Readable(mem, upv, dk, x, j) ==
    {<<p, st.i>> : <<p, st>> \in {<<p, st>> \in RankSet(mem, x, j) \X UNION {dk[q] : q \in Hosts} :
                                     upv[p] /\ st \in dk[p] /\ st.key = x /\ st.k = j /\ st.ok}}
Indices(S) == {t[2] : t \in S}

Track ==
    LET before(x, j) == Readable(members, up, disk, x, j)
        after(x, j) == Readable(members', up', disk', x, j)
        full(x, j) == Indices(after(x, j)) = Idx(j)
    IN /\ whole' = [x \in Idents |-> [j \in CodeIdx |-> whole[x][j] \/ full(x, j)]]
       /\ taken' = [x \in Idents |-> [j \in CodeIdx |->
                      IF full(x, j) THEN 0
                      ELSE Min(MOf(j) + 1, taken[x][j] + Cardinality(before(x, j) \ after(x, j)))]]

Untracked == UNCHANGED <<whole, taken>>

(***************************************************************************)
(* Keeps. A cache takes a stripe if it is up, the stripe is of its own     *)
(* list's code, the list ranks it for the window, and it does not hold it. *)
(***************************************************************************)
Holds(p, x, i, j) == \E st \in disk[p] : st.key = x /\ st.i = i /\ st.k = j
Ranked(p, st) == st.k = lcode[p] /\ p \in RankSet(list[p], st.key, st.k)

Takes(p, st) ==
    /\ up[p]
    /\ ~Holds(p, st.key, st.i, st.k)
    /\ (Ranked(p, st) \/ "keep-unranked" \in Bugs)

\* The hosts in to are offered the stripes sts(p).
Deliver(to, sts(_)) ==
    /\ disk' = [p \in Hosts |-> IF p \in to THEN disk[p] \cup {st \in sts(p) : Takes(p, st)}
                                ELSE disk[p]]
    /\ misplaced' = (misplaced \/
          \E p \in to : \E st \in sts(p) : Takes(p, st) /\ ~Ranked(p, st))

\* A sender with list L and code j sends each rank it has not marked down
\* its stripes of envelope c under j.
Send(L, j, skip, c) ==
    Deliver(RankSet(L, c[1], j) \ skip, LAMBDA p : {Stripe(c, i, j) : i \in Mine(L, c[1], p, j)})

\* The fill a read hands over, under the reader's code: rank 1 gives the
\* first reader that asks the right to fill, once per window per interval,
\* while it holds nothing of the window under that code.
ReadFill(h, x, c) ==
    LET first == Ranking(list[h], x)[1]
        j == lcode[h]
    IN IF up[first] /\ first \notin marked[h] /\ x \notin rights[first] /\ lcode[first] = j
          /\ ~\E st \in disk[first] : st.key = x /\ st.k = j
       THEN /\ rights' = [rights EXCEPT ![first] = @ \cup {x}]
            /\ Send(list[h], j, marked[h], c)
       ELSE UNCHANGED <<rights, disk, misplaced>>

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

\* A publication sends a part's stripes once its PUT has succeeded, under
\* its host's code. The VM runs on the first host that is up.
PubFill ==
    /\ \E p \in members : up[p]
    /\ \E c \in tofill :
          /\ tofill' = tofill \ {c}
          /\ LET p == Least({q \in members : up[q]})
             IN Send(list[p], lcode[p], marked[p], c)
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
\* The codes a host's read tries: its list's code and every earlier one.
Tries(h) == IF "current-code-only" \in Bugs THEN {lcode[h]} ELSE 1..lcode[h]

Asked(h, x, j) == {p \in RankSet(list[h], x, j) \ marked[h] : up[p]}

\* Whether a rank answers with a stripe of that index. A read that asks
\* rank i only for stripe i puts B5 back.
Answers(h, x, p, i, j) == "read-by-index" \notin Bugs \/ Holder(list[h], x, i, j) = p

\* The stripes of code j the asked ranks answer with, and from which host.
Named(h, x, j) == {<<p, st>> \in Asked(h, x, j) \X UNION {disk[q] : q \in Hosts} :
                      st \in disk[p] /\ st.key = x /\ st.k = j /\ Answers(h, x, p, st.i, j)}

\* What a read keeps of answers, and the envelopes K of them decode under j.
Good(x, answers) == {a \in answers : (a[2].key = x \/ "no-key-check" \in Bugs)
                                     /\ (a[2].ok \/ "no-checksum" \in Bugs)}
Decodes(x, j, answers) ==
    LET good == Good(x, answers)
        of(c) == {a \in good : a[2].c = c}
    IN {c \in {a[2].c : a \in good} : Cardinality({a[2].i : a \in of(c)}) >= KOf(j)}

\* A peer may answer with a stripe of the code it holds under another key.
Wrong(h, x, j) ==
    IF "answer" \in Faults /\ faults < MaxFaults
    THEN {{<<p, st>>} : <<p, st>> \in {<<p, st>> \in Asked(h, x, j) \X UNION {disk[q] : q \in Hosts} :
                                          st \in disk[p] /\ st.key # x /\ st.k = j}}
    ELSE {}

Read(h, x) ==
    /\ up[h] /\ reads < MaxReads
    /\ alive /\ committed /\ x \in Current
    /\ \E j \in Tries(h) : \E bad \in {{}} \cup Wrong(h, x, j) :
       LET decodes == Decodes(x, j, Named(h, x, j) \cup bad)
           silent == {p \in RankSet(list[h], x, lcode[h]) \ marked[h] : ~up[p]}
       IN /\ faults' = IF bad = {} THEN faults ELSE faults + 1
          /\ \/ \E c \in decodes :
                   /\ wrong' = (wrong \/ c # <<x, want[x]>>)
                   /\ \/ UNCHANGED <<rights, disk, misplaced>>
                      \* A hit under the reader's own code. A reader that
                      \* heard from every rank of the window may send an
                      \* index of that code no rank holds to a rank that
                      \* holds fewer stripes than the code puts on it.
                      \/ /\ j = lcode[h]
                         /\ RankSet(list[h], x, j) \subseteq Asked(h, x, j)
                         /\ \E p \in RankSet(list[h], x, j), i \in Idx(j) :
                               /\ ~\E q \in RankSet(list[h], x, j) : Holds(q, x, i, j)
                               /\ Cardinality({r \in Idx(j) : Holds(p, x, r, j)})
                                     < Cardinality(Mine(list[h], x, p, j))
                               /\ Deliver({p}, LAMBDA q : {Stripe(c, i, j)})
                         /\ UNCHANGED rights
                      \* A hit under an earlier code, filled under the
                      \* reader's own.
                      \/ /\ j # lcode[h]
                         /\ ReadFill(h, x, c)
             \/ /\ \A t \in Tries(h) : Decodes(x, t, Named(h, x, t)) = {}
                /\ decodes = {}
                /\ store[x] # NoBytes
                \* A miss under every code: the store answers, and the
                \* read fills under the reader's code.
                /\ ReadFill(h, x, <<x, store[x]>>)
                /\ UNCHANGED wrong
             \/ /\ \A t \in Tries(h) : Decodes(x, t, Named(h, x, t)) = {}
                /\ decodes = {} /\ store[x] = NoBytes
                /\ UNCHANGED <<rights, disk, misplaced, wrong>>
          \* A rank that did not answer is marked down, up to a fifth of the
          \* list and at least one. Its timeouts in a row are this one read.
          /\ marked' = [marked EXCEPT ![h] =
                IF silent # {} /\ Cardinality(@) < Max(1, Cardinality(list[h]) \div 5)
                THEN @ \cup {Least(silent)} ELSE @]
    /\ reads' = reads + 1
    /\ UNCHANGED <<vmVars, members, code, up, list, lcode, changes, want>>
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
    /\ UNCHANGED <<vmVars, members, code, list, lcode, disk, reads, faults, flagVars>>
    /\ Track

Recover(h) ==
    /\ ~up[h] /\ h \in members
    /\ up' = [up EXCEPT ![h] = TRUE]
    /\ list' = [list EXCEPT ![h] = members]
    /\ lcode' = [lcode EXCEPT ![h] = code]
    /\ UNCHANGED <<vmVars, members, code, marked, rights, disk, countVars, flagVars>>
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
    /\ UNCHANGED <<vmVars, code, lcode, reads, faults, flagVars>>
    /\ Track

\* A host joins with an empty disk.
Join(h) ==
    /\ h \notin members
    /\ Change
    /\ members' = members \cup {h}
    /\ up' = [up EXCEPT ![h] = TRUE]
    /\ list' = [list EXCEPT ![h] = members \cup {h}]
    /\ lcode' = [lcode EXCEPT ![h] = code]
    /\ UNCHANGED <<vmVars, code, marked, rights, disk, reads, faults, flagVars>>
    /\ Track

\* The operator changes the deployment's code on purpose, to the next of
\* Codes. The hosts learn of it as they read the list.
ChangeCode ==
    /\ code < Len(Codes)
    /\ code' = code + 1
    /\ UNCHANGED <<vmVars, members, up, list, lcode, marked, rights, disk, countVars, flagVars>>
    /\ Untracked

\* Each host reads the orchestrator's list, and its code, on a timer.
Refresh(h) ==
    /\ up[h] /\ (list[h] # members \/ lcode[h] # code)
    /\ list' = [list EXCEPT ![h] = members]
    /\ lcode' = [lcode EXCEPT ![h] = code]
    /\ UNCHANGED <<vmVars, members, code, up, marked, rights, disk, countVars, flagVars>>
    /\ Untracked

\* A probe that succeeds clears the mark.
Probe(h, p) ==
    /\ up[h] /\ p \in marked[h] /\ up[p]
    /\ marked' = [marked EXCEPT ![h] = @ \ {p}]
    /\ UNCHANGED <<vmVars, members, code, up, list, lcode, rights, disk, countVars, flagVars>>
    /\ Untracked

\* The interval of a fill right ends.
Interval(h) ==
    /\ rights[h] # {}
    /\ rights' = [rights EXCEPT ![h] = {}]
    /\ UNCHANGED <<vmVars, members, code, up, list, lcode, marked, disk, countVars, flagVars>>
    /\ Untracked

Next ==
    \/ \E s \in Spans, b \in Values : Put(s, b)
    \/ Commit \/ PubFill \/ Delete \/ Create \/ ChangeCode
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

\* Every stripe a cache holds it took under a list it held, of that list's
\* code, that ranks it among the window's first k+m.
StripesRanked == ~misplaced

\* A window all of whose stripes under some code the deployment has used
\* were readable at once decodes, under one of the codes it tries, for a
\* reader whose list and code are the cluster's and who has marked no host
\* down, while no more than m of those copies have been taken. So a change
\* of the code leaves every earlier window readable.
SurvivesLosses ==
    \A x \in Idents, h \in Hosts, j \in 1..code :
        LET sees(t) == {r \in Readable(list[h], up, disk, x, t) : Answers(h, x, r[1], r[2], t)}
        IN (whole[x][j] /\ taken[x][j] <= MOf(j) /\ up[h] /\ list[h] = members /\ lcode[h] = code
               /\ marked[h] = {})
               => \E t \in Tries(h) : Cardinality(Indices(sees(t))) >= KOf(t)
=============================================================================
