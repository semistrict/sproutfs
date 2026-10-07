------------------------------ MODULE Takeover ------------------------------
(***************************************************************************)
(* How a VM's journal entries outlive its host, as                         *)
(* plans/fsync-journal-2026-10-06.md describes it. Each host writes the    *)
(* entries of the VM it runs into its own journal disk. The control record *)
(* names the journals a replay must read, each with the epoch whose        *)
(* entries it holds and the covered position. An open advances the epoch.  *)
(* A recovery reads each named journal from the host that holds its disk,  *)
(* and that host fences the VM first. A migration names the destination's  *)
(* journal beside the source's until the destination's first checkpoint    *)
(* sealed after its post-copy.                                             *)
(*                                                                         *)
(* One VM of a few pages, and a few hosts. A page's value is the sequence  *)
(* of the stores made to it, each store a new number, so a host that keeps *)
(* running after it is fenced makes values no other host has. An entry     *)
(* holds the values of the pages its capture took: the pages stored into   *)
(* since the capture before. Capture checks how a capture finds those      *)
(* pages and what a seal does to them; here a seal leaves them             *)
(* unjournaled, which only takes more.                                     *)
(*                                                                         *)
(* The parties:                                                            *)
(*                                                                         *)
(*  - the instance of the VM on a host, at the epoch it opened. It         *)
(*    stores, sends a flush, captures, and syncs the batch, which lands    *)
(*    its entry and answers the flush. It captures and syncs only while    *)
(*    it can write its own journal disk. A batch may fail instead, and     *)
(*    its entry may land or not. A running instance seals and selects a    *)
(*    checkpoint in one step; the selection writes the record only if the  *)
(*    epoch is still the instance's;                                       *)
(*  - a migration. The source stops with no batch in flight and hands the  *)
(*    guest, its unjournaled pages and its waiting flush to the            *)
(*    destination, which opens the VM, adds its own journal to the         *)
(*    record's list, and fetches the pages it lacks from the source. The   *)
(*    destination captures nothing until it has them all. It may seal a    *)
(*    checkpoint before then and select it after. A migration is refused   *)
(*    while the list names two journals;                                   *)
(*  - a recovery: an open on a host with no instance, which advances the   *)
(*    epoch, reads each journal the record names from the host that holds  *)
(*    its disk, then selects the replayed state with only its own journal  *)
(*    named. Before the holder answers a read, it gives up any instance    *)
(*    of an older epoch, whose flushes then fail; a write that instance    *)
(*    had in flight may still land;                                        *)
(*  - the hosts, which may crash; and the cloud, which detaches a disk     *)
(*    from a machine, so that every handle on it there fails, and          *)
(*    attaches it to one machine at a time, unless the fault               *)
(*    "multi-attach" lets it attach a second. The controller detaches a    *)
(*    disk a replay needs, from a dead host or, on an operator's force,    *)
(*    from a live one that cannot be reached. The membership gives it to   *)
(*    another host, which opens it. A process never opens its own disk     *)
(*    again once it lost it.                                               *)
(*                                                                         *)
(* A crash of every host may come in any state. The invariants judge the   *)
(* replay that would follow it, of what the disks hold.                    *)
(*                                                                         *)
(* Left out: blocks, digests, the ring, failed ranges and abandoned seals, *)
(* which Capture checks; trimming; the lease, which the single attach      *)
(* makes moot; more than one VM; forks, whose children journal nothing     *)
(* until their root is selected; and the orchestrator, which decides when  *)
(* a migration or a recovery may start. A crash or a detach that touches   *)
(* nothing a replay needs only takes a host away, so it is left out too.   *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANTS
    Hosts,      \* the hosts; each has one journal disk, named after it
    Pages,      \* the pages of the VM's disk
    MaxStores,  \* stores across a run
    MaxEpoch,   \* the last epoch an open may take
    MaxEntries, \* captures across a run
    MaxCrashes, \* hosts that may crash
    MaxDetach,  \* detaches across a run
    Migrating,  \* whether the VM may migrate
    Faults,     \* "multi-attach"
    Bugs        \* defects to put back, to show the invariants catch each one

Disks == Hosts
NoValues == [p \in Pages |-> <<>>]
NoBatch == [on |-> FALSE, pages |-> {}, vals |-> NoValues]
NoSeal == [on |-> FALSE, vals |-> NoValues, covered |-> 0, done |-> FALSE]
NoVM == [state |-> "none", epoch |-> 0, guest |-> NoValues, unj |-> {}, fetched |-> {},
         flush |-> FALSE, sent |-> NoValues, batch |-> NoBatch, seal |-> NoSeal,
         list |-> <<>>, next |-> 1]
Journal(d, e, c) == [disk |-> d, epoch |-> e, covered |-> c]
Entry(e, pages, vals) == [epoch |-> e, pages |-> pages, vals |-> vals]
Pad == Entry(0, {}, NoValues)

VARIABLES
    rec,        \* the control record: epoch, base (the selected checkpoint), list
    disk,       \* disk[d]: the entries on journal disk d, by position
    att,        \* att[d]: the machines disk d is attached to
    opened,     \* opened[d]: the hosts that hold disk d open
    vm,         \* vm[h]: the instance of the VM on host h, see NoVM
    alive,
    answered,   \* the values of the answered flushes as each was sent, less
                \* those a later one among them extends
    stores,     \* stores so far, which numbers the next one
    detaches    \* detaches so far

vars == <<rec, disk, att, opened, vm, alive, answered, stores, detaches>>
cloud == <<att, opened>>

\* Host h can write its own journal disk.
CanWrite(h) == alive[h] /\ h \in att[h] /\ h \in opened[h]

\* Host s holds disk d: it has it open on its machine.
Holds(s, d) == alive[s] /\ s \in att[d] /\ s \in opened[d]

Source == {s \in Hosts : vm[s].state = "source"}

IsPrefix(s, t) == Len(s) <= Len(t) /\ SubSeq(t, 1, Len(s)) = s

\* Values g are values f with later stores of the same guest, or f itself.
Extends(g, f) == \A p \in Pages : IsPrefix(f[p], g[p])

\* The values in S that no other value in S extends.
Newest(S) == {f \in S : ~\E g \in S : g # f /\ Extends(g, f)}

RECURSIVE Sum(_)
Sum(S) == IF S = {} THEN 0 ELSE LET d == CHOOSE d \in S : TRUE IN Len(disk[d]) + Sum(S \ {d})

\* The captures so far: the entries and pads on the disks, and the batches
\* in flight.
Captures == Sum(Disks) + Cardinality({h \in Hosts : vm[h].batch.on})

\* The entries of journal j that a replay applies.
Chosen(j) ==
    LET d == disk[j.disk]
    IN {i \in 1..Len(d) : i > j.covered /\ d[i].pages # {}
                          /\ (d[i].epoch = j.epoch \/ "replay-any-epoch" \in Bugs)}

Last(S) == CHOOSE i \in S : \A k \in S : k <= i

\* Values g with the entries of journal j applied over them in position order.
Apply(g, j) ==
    [p \in Pages |->
        LET I == {i \in Chosen(j) : p \in disk[j.disk][i].pages}
        IN IF I = {} THEN g[p] ELSE disk[j.disk][Last(I)].vals[p]]

RECURSIVE Fold(_, _, _)
Fold(g, l, i) == IF i > Len(l) THEN g ELSE Fold(Apply(g, l[i]), l, i + 1)

\* What a replay of the record reads, in list order.
Replayed == Fold(rec.base, rec.list, 1)

Init ==
    \E h0 \in Hosts :
        /\ rec = [epoch |-> 1, base |-> NoValues, list |-> <<Journal(h0, 1, 0)>>]
        /\ disk = [d \in Disks |-> <<>>]
        /\ att = [d \in Disks |-> {d}]
        /\ opened = [d \in Disks |-> {d}]
        /\ vm = [h \in Hosts |-> IF h = h0
                    THEN [NoVM EXCEPT !.state = "running", !.epoch = 1, !.fetched = Pages]
                    ELSE NoVM]
        /\ alive = [h \in Hosts |-> TRUE]
        /\ answered = {}
        /\ stores = 0
        /\ detaches = 0

TypeOK ==
    /\ rec.epoch \in 1..MaxEpoch
    /\ \A d \in Disks : opened[d] \subseteq att[d]
    /\ \A h \in Hosts : vm[h].state \in {"none", "running", "postcopy", "source", "replay"}
    /\ Cardinality(Source) <= 1

-----------------------------------------------------------------------------
\* The instance on h stores into page p. In the post-copy a store into a
\* page the destination lacks fetches it from the source first.
Store(h, p) ==
    /\ vm[h].state \in {"running", "postcopy"}
    /\ stores < MaxStores
    /\ p \notin vm[h].fetched => Source # {}
    /\ vm' = [vm EXCEPT ![h].guest[p] = Append(@, stores + 1),
                        ![h].unj = @ \cup {p},
                        ![h].fetched = @ \cup {p}]
    /\ stores' = stores + 1
    /\ UNCHANGED <<rec, disk, cloud, alive, answered, detaches>>

Send(h) ==
    /\ vm[h].state \in {"running", "postcopy"}
    /\ ~vm[h].flush
    /\ vm' = [vm EXCEPT ![h].flush = TRUE, ![h].sent = vm[h].guest]
    /\ UNCHANGED <<rec, disk, cloud, alive, answered, stores, detaches>>

\* A capture for the waiting flush. A destination captures only once its
\* post-copy is done.
Capture(h) ==
    /\ vm[h].state = "running"
    /\ vm[h].flush
    /\ ~vm[h].batch.on
    /\ CanWrite(h)
    /\ Captures < MaxEntries
    /\ vm' = [vm EXCEPT ![h].batch = [on |-> TRUE, pages |-> vm[h].unj, vals |-> vm[h].guest],
                        ![h].unj = {}]
    /\ UNCHANGED <<rec, disk, cloud, alive, answered, stores, detaches>>

\* The batch lands and syncs, and the flush is answered.
Sync(h) ==
    /\ vm[h].batch.on
    /\ CanWrite(h)
    /\ disk' = [disk EXCEPT ![h] = Append(@, Entry(vm[h].epoch, vm[h].batch.pages, vm[h].batch.vals))]
    /\ answered' = Newest(answered \cup {vm[h].sent})
    /\ vm' = [vm EXCEPT ![h].batch = NoBatch, ![h].flush = FALSE, ![h].sent = NoValues]
    /\ UNCHANGED <<rec, cloud, alive, stores, detaches>>

\* The batch fails, and so does its flush. If the disk can still be written,
\* the entry may have landed, or the next batch writes a pad over it.
Fail(h) ==
    /\ vm[h].batch.on
    /\ \/ /\ CanWrite(h)
          /\ \E e \in {Entry(vm[h].epoch, vm[h].batch.pages, vm[h].batch.vals), Pad} :
                disk' = [disk EXCEPT ![h] = Append(@, e)]
       \/ /\ ~CanWrite(h)
          /\ UNCHANGED disk
    /\ vm' = [vm EXCEPT ![h].batch = NoBatch, ![h].flush = FALSE, ![h].sent = NoValues,
                        ![h].unj = @ \cup vm[h].batch.pages]
    /\ UNCHANGED <<rec, cloud, alive, answered, stores, detaches>>

\* The seal of instance i on h: its values, and the last position given out
\* on h's journal. A destination's checkpoint before its post-copy is done
\* holds what the record's checkpoint holds for the pages it lacks.
Sealed(h, i) ==
    [on |-> TRUE,
     vals |-> [p \in Pages |-> IF p \in i.fetched THEN i.guest[p] ELSE rec.base[p]],
     covered |-> Len(disk[h]) + (IF i.batch.on THEN 1 ELSE 0),
     done |-> i.state = "running"]

\* The selection of seal z by h writes the record if the epoch is still h's.
\* A checkpoint sealed after the post-copy names only h's journal; one sealed
\* before it keeps the source's journal named.
Selected(h, z) ==
    LET e == vm[h].epoch
        own == Journal(h, e, z.covered)
    IN [rec EXCEPT
           !.base = z.vals,
           !.list = IF z.done \/ "drop-source-early" \in Bugs
                    THEN <<own>>
                    ELSE [k \in DOMAIN rec.list |->
                            IF rec.list[k].disk = h /\ rec.list[k].epoch = e
                            THEN own ELSE rec.list[k]]]

\* A destination in its post-copy seals a checkpoint, which it may select
\* after the post-copy is done.
Seal(h) ==
    /\ vm[h].state = "postcopy"
    /\ ~vm[h].seal.on
    /\ vm' = [vm EXCEPT ![h].seal = Sealed(h, vm[h])]
    /\ UNCHANGED <<rec, disk, cloud, alive, answered, stores, detaches>>

Select(h) ==
    /\ vm[h].seal.on
    /\ rec.epoch = vm[h].epoch
    /\ rec' = Selected(h, vm[h].seal)
    /\ vm' = [vm EXCEPT ![h].seal = NoSeal]
    /\ UNCHANGED <<disk, cloud, alive, answered, stores, detaches>>

\* A running instance seals and selects a checkpoint in one step. Capture
\* checks the flushes that run while a seal stands.
Checkpoint(h) ==
    /\ vm[h].state = "running"
    /\ ~vm[h].seal.on
    /\ rec.epoch = vm[h].epoch
    /\ rec' = Selected(h, Sealed(h, vm[h]))
    /\ UNCHANGED <<disk, cloud, vm, alive, answered, stores, detaches>>

-----------------------------------------------------------------------------
\* A migration from s to t. The source stops with no batch in flight, and
\* the destination opens the VM.
Migrate(s, t) ==
    /\ Migrating
    /\ vm[s].state = "running"
    /\ rec.epoch = vm[s].epoch
    /\ rec.epoch < MaxEpoch
    /\ Len(rec.list) = 1
    /\ ~vm[s].batch.on
    /\ s # t
    /\ vm[t].state = "none"
    /\ CanWrite(t)
    /\ LET e == rec.epoch + 1
       IN /\ rec' = [rec EXCEPT !.epoch = e, !.list = Append(@, Journal(t, e, 0))]
          /\ vm' = [vm EXCEPT
                       ![t] = [NoVM EXCEPT !.state = "postcopy", !.epoch = e,
                                 !.guest = vm[s].guest, !.unj = vm[s].unj,
                                 !.flush = vm[s].flush, !.sent = vm[s].sent],
                       ![s] = [NoVM EXCEPT !.state = "source", !.epoch = vm[s].epoch]]
    /\ UNCHANGED <<disk, cloud, alive, answered, stores, detaches>>

Fetch(t, p) ==
    /\ vm[t].state = "postcopy"
    /\ p \notin vm[t].fetched
    /\ Source # {}
    /\ vm' = [vm EXCEPT ![t].fetched = @ \cup {p}]
    /\ UNCHANGED <<rec, disk, cloud, alive, answered, stores, detaches>>

\* The post-copy is done, and the source lets the VM go.
Done(t) ==
    /\ vm[t].state = "postcopy"
    /\ vm[t].fetched = Pages
    /\ vm' = [h \in Hosts |->
                IF h = t THEN [vm[t] EXCEPT !.state = "running"]
                ELSE IF h \in Source THEN NoVM
                ELSE vm[h]]
    /\ UNCHANGED <<rec, disk, cloud, alive, answered, stores, detaches>>

-----------------------------------------------------------------------------
\* A recovery opens the VM on r and advances the epoch. It keeps the list.
Open(r) ==
    /\ vm[r].state = "none"
    /\ CanWrite(r)
    /\ rec.epoch < MaxEpoch
    /\ rec' = [rec EXCEPT !.epoch = @ + 1]
    /\ vm' = [vm EXCEPT ![r] = [NoVM EXCEPT !.state = "replay", !.epoch = rec.epoch + 1,
                                  !.guest = rec.base, !.list = rec.list]]
    /\ UNCHANGED <<disk, cloud, alive, answered, stores, detaches>>

\* It reads the next journal from s, which holds its disk. Before it
\* answers, the holder fences the VM at the reader's epoch: it gives up an
\* instance of an older epoch, whose flushes then fail. A write that
\* instance had in flight may still land.
Read(r, s) ==
    /\ vm[r].state = "replay"
    /\ vm[r].next <= Len(vm[r].list)
    /\ LET j == vm[r].list[vm[r].next]
           fence == vm[s].epoch < vm[r].epoch /\ "read-no-fence" \notin Bugs
           i == vm[s]
       IN /\ Holds(s, j.disk)
          /\ vm' = [vm EXCEPT ![r].guest = Apply(@, j), ![r].next = @ + 1,
                              ![s] = IF fence THEN NoVM ELSE @]
          /\ IF fence /\ i.batch.on /\ CanWrite(s)
             THEN \E e \in {Entry(i.epoch, i.batch.pages, i.batch.vals), Pad} :
                     disk' = [disk EXCEPT ![s] = Append(@, e)]
             ELSE UNCHANGED disk
    /\ UNCHANGED <<rec, cloud, alive, answered, stores, detaches>>

\* It has read every journal: its first checkpoint selects the replayed
\* state and names only its own journal. Then the guest runs.
Start(r) ==
    /\ vm[r].state = "replay"
    /\ vm[r].next > Len(vm[r].list)
    /\ rec.epoch = vm[r].epoch
    /\ rec' = [rec EXCEPT !.base = vm[r].guest, !.list = <<Journal(r, vm[r].epoch, 0)>>]
    /\ vm' = [vm EXCEPT ![r].state = "running", ![r].fetched = Pages,
                        ![r].list = <<>>, ![r].next = 1]
    /\ UNCHANGED <<disk, cloud, alive, answered, stores, detaches>>

-----------------------------------------------------------------------------
\* Disk d holds entries a replay of the record needs.
Named(d) == \E k \in DOMAIN rec.list : rec.list[k].disk = d

\* Host h runs the VM, or holds a disk a replay needs. Losing any other host
\* only takes away a place the VM could go.
Involved(h) == vm[h].state # "none" \/ \E d \in Disks : Named(d) /\ h \in opened[d]

Crash(h) ==
    /\ alive[h]
    /\ Involved(h)
    /\ Cardinality({g \in Hosts : ~alive[g]}) < MaxCrashes
    /\ alive' = [alive EXCEPT ![h] = FALSE]
    /\ vm' = [vm EXCEPT ![h] = NoVM]
    /\ opened' = [d \in Disks |-> opened[d] \ {h}]
    /\ UNCHANGED <<rec, disk, att, answered, stores, detaches>>

\* The cloud detaches disk d from machine m; every handle on it there fails.
\* The controller detaches a disk a replay needs once its holder is dead, or
\* at once on an operator's force, as for a host that is alive but unreachable.
Detach(d, m) ==
    /\ m \in att[d]
    /\ Named(d)
    /\ detaches < MaxDetach
    /\ att' = [att EXCEPT ![d] = @ \ {m}]
    /\ opened' = [opened EXCEPT ![d] = @ \ {m}]
    /\ detaches' = detaches + 1
    /\ UNCHANGED <<rec, disk, vm, alive, answered, stores>>

\* The membership gives disk d, which a replay needs, to host h, which is
\* not its owner; the cloud attaches it to h's machine, and h opens it.
Attach(d, h) ==
    /\ Named(d)
    /\ alive[h]
    /\ h # d
    /\ h \notin att[d]
    /\ att[d] = {} \/ "multi-attach" \in Faults
    /\ att' = [att EXCEPT ![d] = @ \cup {h}]
    /\ opened' = [opened EXCEPT ![d] = @ \cup {h}]
    /\ UNCHANGED <<rec, disk, vm, alive, answered, stores, detaches>>

Next ==
    \/ \E h \in Hosts :
          \/ Send(h) \/ Capture(h) \/ Sync(h) \/ Fail(h)
          \/ Seal(h) \/ Select(h) \/ Checkpoint(h)
          \/ Done(h) \/ Open(h) \/ Start(h) \/ Crash(h)
          \/ \E p \in Pages : Store(h, p) \/ Fetch(h, p)
          \/ \E t \in Hosts : Migrate(h, t) \/ Read(h, t)
    \/ \E d \in Disks, h \in Hosts : Detach(d, h) \/ Attach(d, h)

Spec == Init /\ [][Next]_vars

Symmetry == Permutations(Hosts)

-----------------------------------------------------------------------------
\* After every host is lost, the replay holds each page as it was when an
\* answered flush was sent, or with later stores of that guest after it.
NoLostFlush == \A f \in answered : Extends(Replayed, f)

\* A replay applies no entry unless the record names its disk and epoch and
\* it is after the covered position.
NoFencedReplay ==
    \A k \in DOMAIN rec.list :
        LET j == rec.list[k]
        IN \A i \in Chosen(j) : disk[j.disk][i].epoch = j.epoch /\ i > j.covered

\* At most one host can write a journal disk.
OneWriter == \A d \in Disks : Cardinality({h \in opened[d] : alive[h] /\ h \in att[d]}) <= 1
=============================================================================
