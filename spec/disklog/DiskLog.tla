------------------------------ MODULE DiskLog ------------------------------
(***************************************************************************)
(* One host's disk cache, as plans/disk-cache-2026-10-02.md lays it out:   *)
(* a log of disk regions, eviction with a bounded second chance, the       *)
(* region kept free, reads in flight, and restarts.                        *)
(*                                                                         *)
(* Stripes arrive to be written, from fills and repairs. A stripe is       *)
(* written at once if no stripe waits and there is room, and otherwise     *)
(* waits in a bounded queue. One region is open, and a full one closes. A  *)
(* stripe opens a region only if one more region is still kept free. At    *)
(* its share, a waiting stripe makes room: the cache gives back the oldest *)
(* closed region, and first writes up to half of its hot stripes again     *)
(* into the region kept free, unless the write budget is spent. Over its   *)
(* share, it gives regions back and writes nothing again. A region a read  *)
(* is in flight from is not given back. Reads make stripes hot.            *)
(*                                                                         *)
(* A restart gives back the open region, which has no table, and the       *)
(* newest closed one if its table was torn and its scan failed. A read in  *)
(* flight across a restart stands for any stale location the index could   *)
(* give: the slot it names may hold another stripe by then, and the key    *)
(* check is what keeps it from being returned.                             *)
(*                                                                         *)
(* The share comes from spec/disklimit, here a number of regions that      *)
(* moves between ShareMin and ShareMax. Which stripes arrive is            *)
(* spec/diskcache. Each stripe is named by its key alone.                  *)
(*                                                                         *)
(* Left out: the window index and its memory budget; the read counter's    *)
(* threshold, which a hot bit stands for; the two syncs of a close, whose  *)
(* order the scan relies on; the limiter's two marks; sendfile.            *)
(***************************************************************************)
EXTENDS Integers, FiniteSets, Sequences

CONSTANTS
    Keys,        \* the stripes that may arrive
    Regions,     \* disk regions in the cache file
    Slots,       \* stripes in a region
    Queue,       \* stripes the write queue holds
    ShareMin,    \* the least share the limiter gives, in regions
    ShareMax,    \* the most
    MaxArrivals, \* stripes that arrive across the run
    MaxReads,    \* reads across the run
    MaxShifts,   \* changes of the share or the budget across the run
    MaxCrashes,  \* restarts across the run
    Overrun,     \* whether the write budget may run out
    Heat,        \* whether reads keep coming, apart from the ones counted
    Bugs         \* defects to put back, to show the properties catch each one

Range(s) == {s[i] : i \in DOMAIN s}
Min(a, b) == IF a < b THEN a ELSE b
Least(S) == CHOOSE a \in S : \A b \in S : a <= b

VARIABLES
    disk,     \* region -> the stripes in it, in order
    open,     \* the open region, or 0
    fifo,     \* the closed regions, oldest first
    queue,    \* stripes that wait to be written
    share,    \* the limiter's share, in regions
    spent,    \* whether the write budget is spent
    up,       \* whether the process runs
    rd,       \* reads in flight: the key asked for and the slot the index named
    arrivals, reads, shifts, crashes,
    wrong     \* a read returned a stripe of another key

vars == <<disk, open, fifo, queue, share, spent, up, rd, arrivals, reads, shifts,
          crashes, wrong>>
diskVars == <<disk, open, fifo, queue>>
envVars == <<share, spent, up>>
countVars == <<arrivals, reads, shifts, crashes>>

Init ==
    /\ disk = [r \in 1..Regions |-> <<>>]
    /\ open = 0
    /\ fifo = <<>>
    /\ queue = <<>>
    /\ share = ShareMax
    /\ spent = FALSE
    /\ up = TRUE
    /\ rd = {}
    /\ arrivals = 0
    /\ reads = 0
    /\ shifts = 0
    /\ crashes = 0
    /\ wrong = FALSE

Used == {r \in 1..Regions : r = open \/ r \in Range(fifo)}
FreeRegions == (1..Regions) \ Used
Held == Cardinality(Used)
Locs == UNION {{<<r, j>> : j \in DOMAIN disk[r]} : r \in Used}
At(l) == disk[l[1]][l[2]]
Holds(k) == (\E l \in Locs : At(l).key = k) \/ (\E j \in DOMAIN queue : queue[j].key = k)
Reading(r) == \E q \in rd : q.loc[1] = r

\* One region is kept free for the second chance, and the cache file holds
\* Regions.
Spare == IF "no-free-region" \in Bugs THEN 0 ELSE 1
CanOpen == Held + Spare + 1 <= Min(share, Regions)
OverShare == Held + Spare > share

(***************************************************************************)
(* Writes.                                                                 *)
(***************************************************************************)
\* Write st into the open region, which closes once it is full.
Append1(r, st) ==
    LET s == Append(disk[r], st)
    IN /\ disk' = [disk EXCEPT ![r] = s]
       /\ IF Len(s) = Slots
          THEN /\ fifo' = Append(fifo, r)
               /\ open' = 0
          ELSE /\ open' = r
               /\ UNCHANGED fifo

\* A stripe arrives. It is written at once if nothing waits and there is
\* room; it waits if the queue has room; otherwise it is dropped.
Arrive(k) ==
    /\ up /\ arrivals < MaxArrivals /\ ~Holds(k) /\ Len(queue) < Queue
    /\ arrivals' = arrivals + 1
    /\ LET st == [key |-> k, hot |-> FALSE]
       IN IF queue = <<>> /\ open # 0 THEN Append1(open, st) /\ UNCHANGED queue
          ELSE IF queue = <<>> /\ CanOpen THEN Append1(Least(FreeRegions), st) /\ UNCHANGED queue
          ELSE queue' = Append(queue, st) /\ UNCHANGED <<disk, open, fifo>>
    /\ UNCHANGED <<envVars, rd, reads, shifts, crashes, wrong>>

WriteQ ==
    /\ up /\ queue # <<>> /\ open # 0
    /\ Append1(open, Head(queue))
    /\ queue' = Tail(queue)
    /\ UNCHANGED <<envVars, rd, countVars, wrong>>

\* A region's space is allocated when it opens.
OpenRegion ==
    /\ up /\ queue # <<>> /\ open = 0 /\ CanOpen
    /\ open' = Least(FreeRegions)
    /\ UNCHANGED <<disk, fifo, queue, envVars, rd, countVars, wrong>>

\* The hot stripes of the oldest closed region, as many as bound, cold again.
Chance(bound) ==
    LET d == disk[Head(fifo)]
        hot == SelectSeq([j \in DOMAIN d |-> j], LAMBDA j : d[j].hot)
        chosen == SubSeq(hot, 1, Min(bound, Len(hot)))
    IN [n \in DOMAIN chosen |-> [d[chosen[n]] EXCEPT !.hot = FALSE]]

\* Give the oldest closed region back, and write kept into a free region.
GiveBack(kept) ==
    LET v == Head(fifo)
        r == Least(FreeRegions)
    IN IF kept = <<>>
       THEN /\ disk' = [disk EXCEPT ![v] = <<>>]
            /\ fifo' = Tail(fifo)
            /\ UNCHANGED open
       ELSE /\ FreeRegions # {}
            /\ disk' = [disk EXCEPT ![v] = <<>>, ![r] = kept]
            /\ IF Len(kept) = Slots
               THEN /\ fifo' = Append(Tail(fifo), r)
                    /\ UNCHANGED open
               ELSE /\ fifo' = Tail(fifo)
                    /\ open' = r

\* At its share, a waiting stripe makes room. Up to half the victim's hot
\* stripes go into the region kept free, unless the budget is spent. A
\* cache that keeps no region free can write them only into its open
\* region, and it has none.
EvictForRoom ==
    /\ up /\ queue # <<>> /\ open = 0
    /\ ~CanOpen /\ ~OverShare
    /\ fifo # <<>> /\ ~Reading(Head(fifo))
    /\ LET bound == IF spent THEN 0
                    ELSE IF "unbounded-second-chance" \in Bugs THEN Slots
                    ELSE Slots \div 2
           kept == Chance(bound)
       IN /\ kept # <<>> => "no-free-region" \notin Bugs
          /\ GiveBack(kept)
    /\ UNCHANGED <<queue, envVars, rd, countVars, wrong>>

\* Over its share, the cache gives the oldest region back and writes
\* nothing again.
EvictOver ==
    /\ up /\ OverShare
    /\ fifo # <<>> /\ ~Reading(Head(fifo))
    /\ GiveBack(IF "unbounded-second-chance" \in Bugs THEN Chance(Slots) ELSE <<>>)
    /\ UNCHANGED <<queue, envVars, rd, countVars, wrong>>

\* Over its share with no closed region, the open region closes.
CloseEarly ==
    /\ up /\ OverShare /\ fifo = <<>> /\ open # 0
    /\ fifo' = <<open>>
    /\ open' = 0
    /\ UNCHANGED <<disk, queue, envVars, rd, countVars, wrong>>

\* Nothing to give back: the waiting writes are dropped.
DropQ ==
    /\ up /\ queue # <<>> /\ open = 0 /\ ~CanOpen /\ fifo = <<>>
    /\ queue' = <<>>
    /\ UNCHANGED <<disk, open, fifo, envVars, rd, countVars, wrong>>

(***************************************************************************)
(* Reads. The index names a slot, and the read holds its region until the  *)
(* stripe has been read. A read checks the key it finds.                   *)
(***************************************************************************)
ReadStart(k) ==
    /\ up /\ reads < MaxReads
    /\ \E l \in Locs :
          /\ At(l).key = k
          /\ rd' = rd \cup {[key |-> k, loc |-> l]}
    /\ reads' = reads + 1
    /\ UNCHANGED <<diskVars, envVars, arrivals, shifts, crashes, wrong>>

ReadDone(q) ==
    /\ q \in rd
    /\ rd' = rd \ {q}
    /\ LET l == q.loc
           there == up /\ l[1] \in Used /\ l[2] \in DOMAIN disk[l[1]]
       IN IF there /\ (At(l).key = q.key \/ "no-key-check" \in Bugs)
          THEN /\ wrong' = (wrong \/ At(l).key # q.key)
               /\ disk' = [disk EXCEPT ![l[1]][l[2]].hot = TRUE]
          ELSE UNCHANGED <<disk, wrong>>
    /\ UNCHANGED <<open, fifo, queue, envVars, countVars>>

\* Reads that keep coming, for the liveness check.
Touch ==
    /\ Heat /\ up
    /\ \E l \in Locs :
          /\ ~At(l).hot
          /\ disk' = [disk EXCEPT ![l[1]][l[2]].hot = TRUE]
    /\ UNCHANGED <<open, fifo, queue, envVars, rd, countVars, wrong>>

(***************************************************************************)
(* The world: the limiter's share, the write budget, restarts.             *)
(***************************************************************************)
Shift ==
    /\ shifts < MaxShifts
    /\ share' \in {s \in ShareMin..ShareMax : s # share}
    /\ shifts' = shifts + 1
    /\ UNCHANGED <<diskVars, spent, up, rd, arrivals, reads, crashes, wrong>>

\* The device's counter says the write budget is spent, for the rest of the
\* run. It counts as a shift, so that a run can end with the budget unspent.
Spend ==
    /\ Overrun /\ ~spent /\ shifts < MaxShifts
    /\ spent' = TRUE
    /\ shifts' = shifts + 1
    /\ UNCHANGED <<diskVars, share, up, rd, arrivals, reads, crashes, wrong>>

Crash ==
    /\ up /\ crashes < MaxCrashes
    /\ up' = FALSE
    /\ queue' = <<>>
    /\ crashes' = crashes + 1
    /\ UNCHANGED <<disk, open, fifo, share, spent, rd, arrivals, reads, shifts, wrong>>

\* The tables are read back in order. The open region is given back. The
\* newest closed region's table may be torn: its scan keeps it, or fails and
\* gives it back.
Recover ==
    /\ ~up
    /\ up' = TRUE
    /\ \E scanned \in BOOLEAN :
          LET torn == IF fifo # <<>> /\ ~scanned THEN {fifo[Len(fifo)]} ELSE {}
              lose == (IF open # 0 THEN {open} ELSE {}) \cup torn
          IN /\ disk' = [r \in 1..Regions |-> IF r \in lose THEN <<>> ELSE disk[r]]
             /\ fifo' = IF torn # {} THEN SubSeq(fifo, 1, Len(fifo) - 1) ELSE fifo
             /\ open' = 0
    /\ UNCHANGED <<queue, share, spent, rd, countVars, wrong>>

\* Nothing is owed: no stripe waits, no read is in flight, and the cache is
\* not over its share with a region to give back. TLC reports a deadlock
\* only where something is owed.
Quiet == queue = <<>> /\ rd = {} /\ ~(up /\ OverShare /\ Held > 0)

Finished == Quiet /\ UNCHANGED vars

Next ==
    \/ \E k \in Keys : Arrive(k) \/ ReadStart(k)
    \/ \E q \in rd : ReadDone(q)
    \/ WriteQ \/ OpenRegion \/ EvictForRoom \/ EvictOver \/ CloseEarly \/ DropQ
    \/ Touch \/ Shift \/ Spend \/ Crash \/ Recover
    \/ Finished

Spec == Init /\ [][Next]_vars

\* Eviction, restarts and reads in flight are fair; nothing else need happen.
LiveSpec ==
    /\ Spec
    /\ WF_vars(EvictOver) /\ WF_vars(CloseEarly) /\ WF_vars(Recover)
    /\ \A k \in Keys, l \in (1..Regions) \X (1..Slots) :
          WF_vars(ReadDone([key |-> k, loc |-> l]))

(***************************************************************************)
(* Properties.                                                             *)
(***************************************************************************)
\* Every read returns a stripe of the key it asked for, or misses.
NoWrongBytes == ~wrong

\* While the cache is over its share, the regions it holds fall.
EvictionProgresses ==
    \A n \in 1..Regions :
        (up /\ OverShare /\ Held = n) ~> (Held < n \/ ~OverShare \/ ~up)
=============================================================================
