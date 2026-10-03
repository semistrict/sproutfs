----------------------------- MODULE DiskLimit -----------------------------
(***************************************************************************)
(* One host's disk limiter, as plans/disk-cache-2026-10-02.md designs it:  *)
(* a free goal, the spill promise and its allocation, another writer on    *)
(* the same filesystem, and the write budget.                              *)
(*                                                                         *)
(* Space is counted in regions. The cache's share is what the free goal    *)
(* leaves once the spill promise is counted whole:                         *)
(*                                                                         *)
(*     share = Cap - other - GoalFree - Promise                            *)
(*                                                                         *)
(* which is the plan's formula with the host's use counting each spill     *)
(* file at its promise. The cache opens a region only if one more is still *)
(* kept free within its share, and allocates the region kept free only for *)
(* a second chance, while it gives a region back. Over its share it gives  *)
(* regions back. A spill file is allocated whole when its pager starts     *)
(* (B4); Spill = "sparse" is the plan as written, which allocates it as    *)
(* pages spill and gives space back as they leave. Another writer takes    *)
(* and gives back space as it likes, as long as the filesystem has it.     *)
(*                                                                         *)
(* The write budget is a level the device's counter raises and a new day   *)
(* resets. As it rises, the cache drops repairs, then second chances, then *)
(* fills from the store, then fills from publications.                     *)
(*                                                                         *)
(* Which regions the cache holds, and what is in them, is spec/disklog.    *)
(* Nothing here waits, so TLC checks no deadlock here.                     *)
(*                                                                         *)
(* Left out: the use goal and the percentage, which are the same           *)
(* comparison against another number; the reserve, which the share takes   *)
(* off as it takes off the goal; the limiter's two marks; a timer          *)
(* reading that is out of date, which makes the cache give back late but   *)
(* opens no region, since one is opened only after a fresh reading;        *)
(* ephemeral spill files and VMM staging, which are promises like the      *)
(* spill file's.                                                           *)
(***************************************************************************)
EXTENDS Integers

CONSTANTS
    Cap,      \* the filesystem, in regions
    GoalFree, \* the free goal, in regions
    Promise,  \* the spill promise, in regions
    Spill,    \* "allocated" at the promise when the pager starts, or "sparse"
    OtherMax, \* what another writer may take
    Regions,  \* regions in the cache file
    Bugs      \* defects to put back, to show the properties catch each one

Min(a, b) == IF a < b THEN a ELSE b

\* The order in which a spent write budget drops writes.
Priority(kind) ==
    CASE kind = "repair" -> 1
      [] kind = "second" -> 2
      [] kind = "store" -> 3
      [] kind = "publication" -> 4

Kinds == {"repair", "store", "publication"}

VARIABLES
    held,    \* regions the cache holds
    alloc,   \* what the spill file has allocated
    other,   \* what another writer holds
    over,    \* how far writes run over budget, 0 to 4
    refused, \* a spill file was refused space it was promised
    overran  \* a write of the host's own left the filesystem short of its goal

vars == <<held, alloc, other, over, refused, overran>>

Init ==
    /\ held = 0
    /\ alloc = IF Spill = "allocated" THEN Promise ELSE 0
    /\ other = 0
    /\ over = 0
    /\ refused = FALSE
    /\ overran = FALSE

FsFree == Cap - other - held - alloc
SpillCounted == IF "spill-by-allocation" \in Bugs THEN alloc ELSE Promise
Share == Cap - other - GoalFree - SpillCounted
Fits == held + 2 <= Min(Share, Regions)
OverShare == held + 1 > Share

\* The host has promised more than the disk keeps: it reports itself
\* unready and admits no VM that would promise more.
Unready == held = 0 /\ OverShare

(***************************************************************************)
(* The cache.                                                              *)
(***************************************************************************)
\* A write of a kind the budget admits opens a region. The limiter reads
\* the filesystem first.
Open(kind) ==
    /\ over < Priority(kind) /\ Fits
    /\ held' = held + 1
    /\ overran' = (overran \/ FsFree - 1 < GoalFree)
    /\ UNCHANGED <<alloc, other, over, refused>>

\* At its share, a second chance writes into the region kept free, then
\* the victim is given back.
SecondChance ==
    /\ over < Priority("second") /\ held > 0 /\ ~Fits /\ ~OverShare
    /\ overran' = (overran \/ FsFree - 1 < GoalFree)
    /\ UNCHANGED <<held, alloc, other, over, refused>>

GiveBack ==
    /\ held > 0
    /\ held' = held - 1
    /\ UNCHANGED <<alloc, other, over, refused, overran>>

(***************************************************************************)
(* The spill file, another writer, and the device's counter.               *)
(***************************************************************************)
SpillGrow ==
    /\ Spill = "sparse" /\ alloc < Promise /\ ~refused
    /\ IF FsFree >= 1
       THEN /\ alloc' = alloc + 1
            /\ overran' = (overran \/ FsFree - 1 < GoalFree)
            /\ UNCHANGED refused
       ELSE /\ refused' = TRUE
            /\ UNCHANGED <<alloc, overran>>
    /\ UNCHANGED <<held, other, over>>

SpillShrink ==
    /\ Spill = "sparse" /\ alloc > 0
    /\ alloc' = alloc - 1
    /\ UNCHANGED <<held, other, over, refused, overran>>

OtherWrite ==
    /\ other < OtherMax /\ FsFree >= 1
    /\ other' = other + 1
    /\ UNCHANGED <<held, alloc, over, refused, overran>>

OtherFree ==
    /\ other > 0
    /\ other' = other - 1
    /\ UNCHANGED <<held, alloc, over, refused, overran>>

Overrun ==
    /\ over < 4
    /\ over' = over + 1
    /\ UNCHANGED <<held, alloc, other, refused, overran>>

NewDay ==
    /\ over > 0
    /\ over' = 0
    /\ UNCHANGED <<held, alloc, other, refused, overran>>

Next ==
    \/ \E kind \in Kinds : Open(kind)
    \/ SecondChance \/ GiveBack
    \/ SpillGrow \/ SpillShrink \/ OtherWrite \/ OtherFree \/ Overrun \/ NewDay

Spec == Init /\ [][Next]_vars

(***************************************************************************)
(* Properties.                                                             *)
(***************************************************************************)
\* A spill file is never refused space it was promised.
PromisesKept == ~refused

\* Nothing the host writes leaves its filesystem short of the free goal.
\* Another writer may; the host then gives regions back, and once it holds
\* none it reports itself unready.
GoalKept == ~overran
=============================================================================
