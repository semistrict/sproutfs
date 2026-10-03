------------------------------- MODULE Stage --------------------------------
(***************************************************************************)
(* Two-node staging, as plans/two-node-staging-2026-10-02.md designs it.   *)
(* Nothing implements it yet; this is the protocol the code must follow.   *)
(*                                                                         *)
(* A guest's flush returns once the state it covers is synced on the       *)
(* writer's own disk and on one peer's. The control record, written only   *)
(* by compare-and-set, is the one authority: it names the writer's epoch,  *)
(* the peer and the selected checkpoint. The nodes run no consensus. A     *)
(* peer holds batches it never reads, refuses a batch from an epoch older  *)
(* than one it has been fenced at, and drops what a selected checkpoint    *)
(* covers.                                                                 *)
(*                                                                         *)
(* A state is <<e, i>>: the history of the guest instance that epoch e     *)
(* opened, up to its i-th write. Instance e starts from base[e], the state *)
(* its recovery read. A batch is cumulative: it carries the whole state it *)
(* was cut at, as a stage cut carries every page written since the last    *)
(* checkpoint.                                                             *)
(*                                                                         *)
(* Failures: one host lost with its disk, one power loss of every host at  *)
(* once (memory and unsynced writes go, synced disks stay), and a recovery *)
(* while the old writer still runs, which only the fence stops from        *)
(* acknowledging. A lost message is one never delivered, which a safety    *)
(* check already covers, so messages are not lost separately.              *)
(*                                                                         *)
(* The property: no recovery comes back without a flush any earlier       *)
(* instance acknowledged.                                                  *)
(*                                                                         *)
(* Left out: the pager's cut against seal, settle and copy-on-write; pages *)
(* (a state stands for all of them); migration and forks; the orchestrator *)
(* survey, which spec/recovery checks.                                     *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
    Hosts,       \* the hosts, each with a disk
    MaxWrites,   \* writes one instance may make
    MaxEpoch,    \* opens, counting the first
    MaxDeaths,   \* hosts lost with their disk
    MaxPower,    \* power losses of every host at once
    MaxMessages, \* messages in flight at once
    Bugs         \* defects to put back, to show the invariant catches each one

VARIABLES
    record,    \* the control record: [epoch, peer, ck]
    base,      \* base[e]: the state instance e started from
    run,       \* run[h]: an instance runs its guest on h
    my,        \* my[h]: the epoch of the instance on h
    cur,       \* cur[h]: its state in memory
    bel,       \* bel[h]: the peer it acknowledges through
    cut,       \* cut[h]: the state of the cut in flight, or NoState
    peerOk,    \* peerOk[h]: the peer acknowledged that cut
    cand,      \* cand[h]: the peer it is catching up to replace bel[h]
    candAt,    \* candAt[h]: the state it sent the candidate
    candOk,    \* candOk[h]: the candidate acknowledged it
    vol,       \* vol[h]: the newest batch a peer wrote and has not synced
    dur,       \* dur[h]: the newest batch h's log holds synced, or NoState
    fence,     \* fence[h]: the highest epoch h has seen
    msgs,      \* messages in flight
    alive,     \* hosts that have not been lost
    acked,     \* states whose flush returned to the guest
    rec,       \* the recovery in progress: [phase, n, e, peer, old]
    deaths, power

vars == <<record, base, run, my, cur, bel, cut, peerOk, cand, candAt, candOk,
          vol, dur, fence, msgs, alive, acked, rec, deaths, power>>

NoState == <<0, 0>>
NoBase == <<0, 1>>      \* no state: epoch 0 never writes
NoHost == "none"
Epochs == 1..MaxEpoch

\* The state s holds the write c: c is in s's own history or in the base its
\* instance started from.
RECURSIVE Holds(_, _)
Holds(s, c) ==
    IF c[1] = 0 THEN TRUE
    ELSE IF s[1] = 0 THEN FALSE
    ELSE IF s[1] = c[1] THEN s[2] >= c[2]
    ELSE IF s[1] < c[1] THEN FALSE
    ELSE Holds(base[s[1]], c)

\* The newest of a set of states: the latest instance's furthest write.
Newest(S) == CHOOSE s \in S : \A t \in S : s[1] > t[1] \/ (s[1] = t[1] /\ s[2] >= t[2])

\* A log is kept as its newest batch. Batches are cumulative, so the newest
\* holds every older one of its instance, and a log only takes batches from
\* instances at least as new as any it was fenced at, so the newest is all a
\* recovery reads.
Add(log, s) == Newest({log, s})

\* The hosts are interchangeable, which a configuration may declare.
Symmetry == Permutations(Hosts)

Init ==
    /\ \E first, second \in Hosts :
          /\ first # second
          /\ record = [epoch |-> 1, peer |-> second, ck |-> NoState]
          /\ run = [h \in Hosts |-> h = first]
          /\ my = [h \in Hosts |-> IF h = first THEN 1 ELSE 0]
          /\ cur = [h \in Hosts |-> IF h = first THEN <<1, 0>> ELSE NoState]
          /\ bel = [h \in Hosts |-> IF h = first THEN second ELSE NoHost]
    /\ base = [e \in Epochs |-> IF e = 1 THEN NoState ELSE NoBase]
    /\ cut = [h \in Hosts |-> NoState]
    /\ peerOk = [h \in Hosts |-> FALSE]
    /\ cand = [h \in Hosts |-> NoHost]
    /\ candAt = [h \in Hosts |-> NoState]
    /\ candOk = [h \in Hosts |-> FALSE]
    /\ vol = [h \in Hosts |-> NoState]
    /\ dur = [h \in Hosts |-> NoState]
    /\ fence = [h \in Hosts |-> 0]
    /\ msgs = {}
    /\ alive = Hosts
    /\ acked = {}
    /\ rec = [phase |-> "idle", n |-> NoHost, e |-> 0, peer |-> NoHost, old |-> NoHost,
              src |-> NoHost]
    /\ deaths = 0
    /\ power = 0

instVars == <<run, my, cur, bel, cut, peerOk, cand, candAt, candOk>>

Send(m) == msgs' = msgs \cup {m}

(***************************************************************************)
(* The guest writes.                                                       *)
(***************************************************************************)
Write(h) ==
    /\ run[h] /\ cur[h][2] < MaxWrites
    /\ cur' = [cur EXCEPT ![h] = <<@[1], @[2] + 1>>]
    /\ UNCHANGED <<record, base, run, my, bel, cut, peerOk, cand, candAt, candOk,
                   vol, dur, fence, msgs, alive, acked, rec, deaths, power>>

(***************************************************************************)
(* A flush takes a cut: the state goes to the writer's own log and to the  *)
(* peer the writer acknowledges through. The writer's own append and sync  *)
(* are one step here; the peer's are two, so a peer can be caught holding  *)
(* a batch it has not synced.                                              *)
(***************************************************************************)
Cut(h) ==
    /\ run[h] /\ cut[h] = NoState /\ bel[h] # NoHost
    /\ my[h] >= fence[h] \/ "no-fence" \in Bugs
    /\ cut' = [cut EXCEPT ![h] = cur[h]]
    /\ peerOk' = [peerOk EXCEPT ![h] = FALSE]
    /\ dur' = [dur EXCEPT ![h] = Add(@, cur[h])]
    /\ Send([type |-> "append", to |-> bel[h], from |-> h, e |-> my[h], s |-> cur[h]])
    /\ UNCHANGED <<record, base, run, my, cur, bel, cand, candAt, candOk,
                   vol, fence, alive, acked, rec, deaths, power>>

\* A peer takes a batch unless it has been fenced past the batch's epoch. It
\* writes and syncs the batch, then acknowledges it, in one step: nothing
\* else the model checks happens between the three. Put back wrong, it
\* acknowledges on receipt and syncs later, so a power loss in between takes
\* an acknowledged batch with it.
Receive(m) ==
    /\ m \in msgs /\ m.type = "append" /\ m.to \in alive
    /\ IF m.e >= fence[m.to] \/ "no-fence" \in Bugs
       THEN /\ fence' = [fence EXCEPT ![m.to] = IF m.e > @ THEN m.e ELSE @]
            /\ msgs' = (msgs \ {m}) \cup
                  {[type |-> "ack", to |-> m.from, from |-> m.to, s |-> m.s]}
            /\ IF "ack-before-sync" \in Bugs
               THEN /\ vol' = [vol EXCEPT ![m.to] = Add(@, m.s)]
                    /\ UNCHANGED dur
               ELSE /\ dur' = [dur EXCEPT ![m.to] = Add(@, m.s)]
                    /\ UNCHANGED vol
       ELSE /\ msgs' = msgs \ {m}
            /\ UNCHANGED <<fence, vol, dur>>
    /\ UNCHANGED <<record, base, instVars, alive, acked, rec, deaths, power>>

\* The late sync of a peer that acknowledged first.
Sync(h) ==
    /\ h \in alive /\ vol[h] # NoState
    /\ dur' = [dur EXCEPT ![h] = Add(@, vol[h])]
    /\ vol' = [vol EXCEPT ![h] = NoState]
    /\ UNCHANGED <<record, base, instVars, fence, msgs, alive, acked, rec, deaths, power>>

\* An acknowledgement counts only from the peer or the candidate it was for.
AckIn(m) ==
    /\ m \in msgs /\ m.type = "ack" /\ m.to \in alive
    /\ msgs' = msgs \ {m}
    /\ peerOk' = [peerOk EXCEPT ![m.to] =
          @ \/ (run[m.to] /\ m.from = bel[m.to] /\ m.s = cut[m.to])]
    /\ candOk' = [candOk EXCEPT ![m.to] =
          @ \/ (run[m.to] /\ m.from = cand[m.to] /\ m.s = candAt[m.to])]
    /\ UNCHANGED <<record, base, run, my, cur, bel, cut, cand, candAt,
                   vol, dur, fence, alive, acked, rec, deaths, power>>

\* The flushes the cut covers return once both disks have it.
Acknowledge(h) ==
    /\ run[h] /\ cut[h] # NoState
    /\ peerOk[h] \/ "local-only" \in Bugs
    /\ acked' = acked \cup {cut[h]}
    /\ cut' = [cut EXCEPT ![h] = NoState]
    /\ UNCHANGED <<record, base, run, my, cur, bel, peerOk, cand, candAt, candOk,
                   vol, dur, fence, msgs, alive, rec, deaths, power>>

(***************************************************************************)
(* The checkpoint selects the writer's state by compare-and-set, and then  *)
(* both logs drop what it covers. A pause and an upload that run apart     *)
(* change nothing here: an upload from a fenced writer fails its           *)
(* compare-and-set, and a later state only holds more.                     *)
(***************************************************************************)
Checkpoint(h) ==
    /\ run[h] /\ record.epoch = my[h]
    /\ record' = [record EXCEPT !.ck = cur[h]]
    /\ UNCHANGED <<base, instVars, vol, dur, fence, msgs, alive, acked, rec, deaths, power>>

\* What a drop may release: the selected checkpoint, or, put back wrong, the
\* writer's memory.
Covered(h) == IF "drop-ahead" \in Bugs THEN cur[h] ELSE record.ck

\* The writer drops from its own log and its peer's at once. A drop that
\* arrives late releases only what a selected checkpoint holds, and that
\* stays true, so its timing does not matter.
DropLogs(h) ==
    /\ run[h] /\ record.epoch = my[h] /\ bel[h] # NoHost
    /\ dur' = [g \in Hosts |->
          IF g \in {h, bel[h]} /\ Holds(Covered(h), dur[g]) THEN NoState ELSE dur[g]]
    /\ UNCHANGED <<record, base, instVars, vol, fence, msgs, alive, acked, rec, deaths, power>>

(***************************************************************************)
(* Replacing the peer: catch a candidate up with everything the writer     *)
(* holds, then name it in the record by compare-and-set, then acknowledge  *)
(* through it. Flushes may still return through the old peer while the     *)
(* candidate catches up, so the switch waits until the candidate holds     *)
(* every flush the writer has acknowledged, sending it the writer's state  *)
(* again if it has fallen behind. A cut in flight at the switch is         *)
(* abandoned and its flushes wait for the next one.                        *)
(***************************************************************************)
\* Only the record's writer replaces its peer here. A fenced writer that tries
\* fails the compare-and-set at the switch, and what it sent a candidate is
\* older than anything a later instance writes there.
CatchUp(h, p) ==
    /\ run[h] /\ record.epoch = my[h]
    /\ cand[h] \in {NoHost, p} /\ p \in alive /\ p # h /\ p # bel[h]
    /\ cand' = [cand EXCEPT ![h] = p]
    /\ candAt' = [candAt EXCEPT ![h] = cur[h]]
    /\ candOk' = [candOk EXCEPT ![h] = FALSE]
    /\ Send([type |-> "append", to |-> p, from |-> h, e |-> my[h], s |-> cur[h]])
    /\ UNCHANGED <<record, base, run, my, cur, bel, cut, peerOk,
                   vol, dur, fence, alive, acked, rec, deaths, power>>

Switch(h) ==
    /\ run[h] /\ cand[h] # NoHost
    /\ \/ /\ candOk[h]
          /\ \/ \A c \in acked : c[1] = my[h] => Holds(candAt[h], c)
             \/ "switch-stale" \in Bugs
       \/ "switch-early" \in Bugs
    /\ IF record.epoch = my[h]
       THEN /\ record' = [record EXCEPT !.peer = cand[h]]
            /\ bel' = [bel EXCEPT ![h] = cand[h]]
            /\ cut' = [cut EXCEPT ![h] = NoState]
       ELSE UNCHANGED <<record, bel, cut>>
    /\ cand' = [cand EXCEPT ![h] = NoHost]
    /\ UNCHANGED <<base, run, my, cur, peerOk, candAt, candOk,
                   vol, dur, fence, msgs, alive, acked, rec, deaths, power>>

(***************************************************************************)
(* Recovery: open by compare-and-set, fence the peer the record names,     *)
(* read the newest state it holds (or the checkpoint), sync it to the new  *)
(* writer's own log, and start the new instance from it. The old writer    *)
(* may still be running. A recovery on the peer's own host has one disk    *)
(* where it needs two, so it acknowledges nothing until it has caught up   *)
(* a new peer and named it.                                                *)
(***************************************************************************)
Open(n) ==
    /\ rec.phase = "idle" /\ n \in alive /\ ~run[n] /\ record.epoch < MaxEpoch
    /\ record' = [record EXCEPT !.epoch = @ + 1]
    /\ rec' = [phase |-> "fencing", n |-> n, e |-> record.epoch + 1,
               peer |-> record.peer, src |-> NoHost,
               old |-> CHOOSE h \in Hosts \cup {NoHost} :
                          (h \in Hosts /\ my[h] = record.epoch) \/
                          (h = NoHost /\ \A g \in Hosts : my[g] # record.epoch)]
    /\ UNCHANGED <<base, instVars, vol, dur, fence, msgs, alive, acked, deaths, power>>

\* The recovery reads the peer, or, with the peer lost, the old writer's own
\* log. Either is fenced first: from then on it takes no batch, its own or
\* the peer's, from an older epoch, so nothing newer than what the read sees
\* can be acknowledged.
Fence(src) ==
    /\ rec.phase = "fencing" /\ src \in alive
    /\ src = rec.peer \/ (src = rec.old /\ rec.peer \notin alive)
    /\ fence' = [fence EXCEPT ![src] = IF rec.e > @ THEN rec.e ELSE @]
    /\ rec' = [rec EXCEPT !.phase = "reading", !.src = src]
    /\ UNCHANGED <<record, base, instVars, vol, dur, msgs, alive, acked, deaths, power>>

\* What a disk holds, synced or not; a read sees both.
OnDisk(h) == {dur[h], vol[h]}

Read ==
    /\ rec.n \in alive
    /\ \E src \in {rec.peer, rec.old} \cap alive :
          /\ \/ rec.phase = "reading" /\ src = rec.src
             \/ /\ "read-unfenced" \in Bugs /\ rec.phase = "fencing"
                /\ src = rec.peer \/ rec.peer \notin alive
          /\ LET r == Newest({record.ck} \cup OnDisk(src)) IN
             /\ base' = [base EXCEPT ![rec.e] = r]
             /\ run' = [run EXCEPT ![rec.n] = TRUE]
             /\ my' = [my EXCEPT ![rec.n] = rec.e]
             /\ cur' = [cur EXCEPT ![rec.n] = <<rec.e, 0>>]
             /\ bel' = [bel EXCEPT ![rec.n] =
                    IF record.peer = rec.n THEN NoHost ELSE record.peer]
             /\ dur' = IF "recover-unsynced" \in Bugs THEN dur
                       ELSE [dur EXCEPT ![rec.n] = Add(@, r)]
             /\ cut' = [cut EXCEPT ![rec.n] = NoState]
             /\ cand' = [cand EXCEPT ![rec.n] = NoHost]
    /\ rec' = [rec EXCEPT !.phase = "idle"]
    /\ UNCHANGED <<record, peerOk, candAt, candOk, vol, fence, msgs, alive,
                   acked, deaths, power>>

(***************************************************************************)
(* Failures.                                                               *)
(***************************************************************************)
\* A host is lost with its disk.
Die(h) ==
    /\ h \in alive /\ deaths < MaxDeaths
    /\ alive' = alive \ {h}
    /\ run' = [run EXCEPT ![h] = FALSE]
    /\ vol' = [vol EXCEPT ![h] = NoState]
    /\ dur' = [dur EXCEPT ![h] = NoState]
    /\ deaths' = deaths + 1
    /\ UNCHANGED <<record, base, my, cur, bel, cut, peerOk, cand, candAt, candOk,
                   fence, msgs, acked, rec, power>>

\* Every host loses power at once: every guest and every unsynced write goes.
PowerLoss ==
    /\ power < MaxPower
    /\ run' = [h \in Hosts |-> FALSE]
    /\ vol' = [h \in Hosts |-> NoState]
    /\ cut' = [h \in Hosts |-> NoState]
    /\ cand' = [h \in Hosts |-> NoHost]
    /\ msgs' = {}
    /\ power' = power + 1
    /\ UNCHANGED <<record, base, my, cur, bel, peerOk, candAt, candOk, dur, fence,
                   alive, acked, rec, deaths>>

Next ==
    \/ \E h \in Hosts :
          \/ Write(h) \/ Cut(h) \/ Sync(h) \/ Acknowledge(h)
          \/ Checkpoint(h) \/ DropLogs(h) \/ Switch(h)
          \/ Open(h) \/ Die(h)
          \/ \E p \in Hosts : CatchUp(h, p)
    \/ \E m \in msgs : Receive(m) \/ AckIn(m)
    \/ \E h \in Hosts : Fence(h)
    \/ Read \/ PowerLoss

Spec == Init /\ [][Next]_vars

\* A state constraint: at most MaxMessages messages in flight.
FewMessages == Cardinality(msgs) <= MaxMessages

\* Every recovered instance starts from a state that holds every flush an
\* earlier instance acknowledged.
NoAckedLoss ==
    \A c \in acked : \A e \in Epochs :
        (base[e] # NoBase /\ c[1] < e) => Holds(base[e], c)
=============================================================================
