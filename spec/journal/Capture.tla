------------------------------- MODULE Capture -------------------------------
(***************************************************************************)
(* How a host's pager takes one region's changed blocks into its journal,  *)
(* and what a seal, a selection and an abandon do to them, as              *)
(* plans/fsync-journal-2026-10-06.md describes it.                         *)
(*                                                                         *)
(* A few pages of a few blocks. A block holds the bytes "x" or "y", and    *)
(* every store flips them, so a block's bytes go back to what they were    *)
(* two stores before, as real bytes can. A digest is the bytes of a block  *)
(* as the last capture of its page took them, or none.                     *)
(*                                                                         *)
(* A block that has not been stored into since some moment must replay     *)
(* with the bytes it holds. One that has been stored into may replay with  *)
(* either, since each was there after that moment. So for the last         *)
(* answered flush, the waiting flush and each checkpoint, the model keeps  *)
(* the blocks not stored into since.                                       *)
(*                                                                         *)
(* The parties:                                                            *)
(*                                                                         *)
(*  - the guest, which stores into a page it maps writable, and otherwise  *)
(*    faults first. A store fault maps the page; a store into a page the   *)
(*    standing seal holds copies on write; a store into any other          *)
(*    write-protected page is a protect trap. Each marks the page          *)
(*    unjournaled and maps it writable;                                    *)
(*  - the guest's flush, one at a time: sent, then answered or failed;     *)
(*  - a capture, in two steps. It takes the region's unjournaled pages     *)
(*    and the standing seal's unjournaled list, and write-protects them.   *)
(*    Then it reads each block, keeps the blocks whose bytes differ from   *)
(*    their digests, sets the digests, and makes the entry at the next     *)
(*    position. A store between the two steps traps and marks its page     *)
(*    again;                                                               *)
(*  - the journal's writer, with one batch in flight and one entry a       *)
(*    batch. The batch lands, with pads over the failed range before it,   *)
(*    then syncs and answers its flush; or it fails. A capture waits for   *)
(*    the batch in flight;                                                 *)
(*  - the seal's pause, which holds the journal lock, so no capture runs   *)
(*    across it. It write-protects the writable pages, records the last    *)
(*    position given out as the covered position, and moves the            *)
(*    unjournaled pages into its unjournaled list, which it holds for      *)
(*    copy on write. Digests says which digests it drops;                  *)
(*  - the selection, which makes the seal's checkpoint and covered         *)
(*    position the record's and drops the list; and the abandon, which     *)
(*    gives every page still on the list back as unjournaled, with no      *)
(*    digests.                                                             *)
(*                                                                         *)
(* A failed batch gives its pages back as unjournaled, with no digests:    *)
(* its entry may not be on the disk, and its flush failed (spec/bugs.md,   *)
(* B6).                                                                    *)
(*                                                                         *)
(* A crash may come in any state. The replay after it reads the disk from  *)
(* the first position, stops at the first empty slot, and applies each     *)
(* entry after the covered position over the selected checkpoint. The      *)
(* invariants judge that replay in every state.                            *)
(*                                                                         *)
(* Left out: more than one region, which only share batches; the ring, its *)
(* wrap and trimming; a batch that lands in part, which the reader stops   *)
(* at as at an empty slot; the origin as a source of digests after the     *)
(* start, which a page has only while the guest has not stored into it;    *)
(* spills, which change no bytes; stores that leave a block's bytes as     *)
(* they were, which a capture skips; and epochs, which Takeover checks.    *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
    Pages,      \* the pages of the region
    Blocks,     \* the blocks of a page
    MaxStores,  \* stores across a run
    MaxPos,     \* positions the journal may give out
    Digests,    \* "keep": a page that was not unjournaled at a seal keeps
                \* its digests; "drop": every page loses them at every seal;
                \* "refresh": as "keep", and the selection gives each page
                \* still on the list the digests of the selected checkpoint's
                \* bytes, which is what the next copy of the published page
                \* takes
    Bugs        \* defects to put back, to show the invariants catch each one

Addrs == Pages \X Blocks
None == "none"
Flip(c) == IF c = "x" THEN "y" ELSE "x"
NoBlocks == [a \in Addrs |-> None]
AllX == [a \in Addrs |-> "x"]

Empty == [kind |-> "empty", blocks |-> NoBlocks]
Pad == [kind |-> "pad", blocks |-> NoBlocks]

NoBatch == [pos |-> 0, from |-> 0, blocks |-> NoBlocks, pages |-> {}, landed |-> FALSE]
NoSeal == [on |-> FALSE, bytes |-> AllX, kept |-> {}, list |-> {}, covered |-> 0]

VARIABLES
    bytes,      \* bytes[a]: what block a holds
    writable,   \* the pages mapped writable: a store needs no fault
    protected,  \* the pages mapped write-protected that no seal holds
    held,       \* the pages the standing seal holds: a store copies on write
    unj,        \* the region's unjournaled pages
    digest,     \* digest[a]: the bytes the last capture took of block a, or None
    capturing,  \* a capture has taken pages and not yet read them
    taken,      \* the pages it took
    batch,      \* the batch in flight, NoBatch for none
    disk,       \* disk[i]: the slot at position i
    nextPos,    \* the next position to give out
    padFrom,    \* the first position after the last batch that synced
    seal,       \* the standing seal: its bytes, the blocks not stored into
                \* since (kept), its unjournaled list and covered position
    sel,        \* the selected checkpoint: its bytes, kept, covered position
    flush,      \* the guest's flush: "idle", "sent", or "batched"
    sending,    \* the blocks not stored into since the waiting flush was sent
    flushed,    \* the blocks not stored into since the last answered flush was sent
    stores

mapping == <<writable, protected, held>>
vars == <<bytes, mapping, unj, digest, capturing, taken, batch, disk,
          nextPos, padFrom, seal, sel, flush, sending, flushed, stores>>

Init ==
    /\ bytes = AllX
    /\ writable = {}
    /\ protected = {}
    /\ held = {}
    /\ unj = {}
    /\ digest = AllX
    /\ capturing = FALSE
    /\ taken = {}
    /\ batch = NoBatch
    /\ disk = [i \in 1..MaxPos |-> Empty]
    /\ nextPos = 1
    /\ padFrom = 1
    /\ seal = NoSeal
    /\ sel = [bytes |-> AllX, kept |-> Addrs, covered |-> 0]
    /\ flush = "idle"
    /\ sending = Addrs
    /\ flushed = Addrs
    /\ stores = 0

TypeOK ==
    /\ bytes \in [Addrs -> {"x", "y"}]
    /\ writable \subseteq Pages /\ protected \subseteq Pages /\ held \subseteq Pages
    /\ writable \cap protected = {} /\ writable \cap held = {} /\ protected \cap held = {}
    /\ held \subseteq seal.list
    /\ unj \subseteq Pages
    /\ digest \in [Addrs -> {"x", "y", None}]
    /\ taken \subseteq Pages
    /\ capturing \/ taken = {}
    /\ \A i \in 1..MaxPos : disk[i].kind \in {"empty", "pad", "blocks"}
    /\ nextPos \in 1..MaxPos + 1 /\ padFrom \in 1..nextPos
    /\ flush \in {"idle", "sent", "batched"}
    /\ batch # NoBatch => flush = "batched"

\* The guest stores into block b of page p. A store into a page it cannot
\* write faults first: a store fault maps the page, a store into a page the
\* seal holds copies on write, and any other is a protect trap.
Store(p, b) ==
    /\ stores < MaxStores
    /\ stores' = stores + 1
    /\ unj' = IF p \in protected /\ "trap-not-marked" \in Bugs THEN unj ELSE unj \cup {p}
    /\ writable' = writable \cup {p}
    /\ protected' = protected \ {p}
    /\ held' = held \ {p}
    /\ bytes' = [bytes EXCEPT ![<<p, b>>] = Flip(@)]
    /\ sending' = IF flush = "idle" THEN sending ELSE sending \ {<<p, b>>}
    /\ flushed' = flushed \ {<<p, b>>}
    /\ seal' = [seal EXCEPT !.kept = @ \ {<<p, b>>}]
    /\ sel' = [sel EXCEPT !.kept = @ \ {<<p, b>>}]
    /\ UNCHANGED <<digest, capturing, taken, batch, disk, nextPos, padFrom,
                   flush>>

Send ==
    /\ flush = "idle"
    /\ flush' = "sent"
    /\ sending' = Addrs
    /\ UNCHANGED <<bytes, mapping, unj, digest, capturing, taken, batch, disk,
                   nextPos, padFrom, seal, sel, flushed, stores>>

\* A capture for the waiting flush takes the unjournaled pages and the
\* standing seal's list, and write-protects them.
Take ==
    /\ flush = "sent"
    /\ ~capturing
    /\ batch = NoBatch
    /\ nextPos <= MaxPos
    /\ LET t == unj \cup seal.list
       IN /\ taken' = t
          /\ writable' = writable \ t
          /\ protected' = protected \cup (writable \cap t)
          /\ unj' = unj \ t
    /\ capturing' = TRUE
    /\ flush' = "batched"
    /\ UNCHANGED <<bytes, held, digest, batch, disk, nextPos, padFrom, seal, sel,
                   sending, flushed, stores>>

\* It reads each block of the pages it took: the sealed copy of a page the
\* seal still holds, which the guest has not stored into since the seal,
\* and the guest's page otherwise. It keeps the blocks whose bytes differ
\* from their digests. A page the guest stored into since the seal leaves
\* the seal's list.
Read ==
    /\ capturing
    /\ LET now(a) == IF a[1] \in held THEN seal.bytes[a] ELSE bytes[a]
           in(a) == a[1] \in taken
           kept == [a \in Addrs |-> IF in(a) /\ digest[a] # now(a) THEN now(a) ELSE None]
       IN /\ batch' = [pos |-> nextPos, from |-> padFrom, blocks |-> kept,
                       pages |-> taken, landed |-> FALSE]
          /\ digest' = [a \in Addrs |-> IF in(a) THEN now(a) ELSE digest[a]]
    /\ seal' = [seal EXCEPT !.list = @ \ (taken \ held)]
    /\ nextPos' = nextPos + 1
    /\ capturing' = FALSE
    /\ taken' = {}
    /\ UNCHANGED <<bytes, mapping, unj, disk, padFrom, sel, flush, sending,
                   flushed, stores>>

\* The disk once batch b lands: pads over the failed range before it, then
\* its entry.
Landed(d, b) ==
    [i \in 1..MaxPos |->
        IF i = b.pos THEN [kind |-> "blocks", blocks |-> b.blocks]
        ELSE IF b.from <= i /\ i < b.pos THEN Pad
        ELSE d[i]]

Land ==
    /\ batch # NoBatch
    /\ ~batch.landed
    /\ disk' = Landed(disk, batch)
    /\ batch' = [batch EXCEPT !.landed = TRUE]
    /\ UNCHANGED <<bytes, mapping, unj, digest, capturing, taken, nextPos,
                   padFrom, seal, sel, flush, sending, flushed, stores>>

\* The sync is done, and the flush is answered.
Answer ==
    /\ batch.landed
    /\ flushed' = sending
    /\ sending' = Addrs
    /\ flush' = "idle"
    /\ batch' = NoBatch
    /\ padFrom' = nextPos
    /\ UNCHANGED <<bytes, mapping, unj, digest, capturing, taken, disk,
                   nextPos, seal, sel, stores>>

\* The write or the sync fails, whether the entry landed or not. The flush
\* fails. The batch gives its pages back as unjournaled, with no digests.
\* The next batch pads from where this one's failed range began.
Fail ==
    /\ batch # NoBatch
    /\ flush' = "idle"
    /\ sending' = Addrs
    /\ batch' = NoBatch
    /\ unj' = IF "failed-keeps-pages" \in Bugs THEN unj ELSE unj \cup batch.pages
    /\ digest' = IF {"failed-keeps-pages", "failed-keeps-digests"} \cap Bugs # {}
                 THEN digest
                 ELSE [a \in Addrs |-> IF a[1] \in batch.pages THEN None ELSE digest[a]]
    /\ IF "reuse-positions" \in Bugs
       THEN /\ nextPos' = batch.pos
            /\ padFrom' = batch.from
       ELSE UNCHANGED <<nextPos, padFrom>>
    /\ UNCHANGED <<bytes, mapping, capturing, taken, disk, seal, sel, flushed, stores>>

\* The seal's pause. It holds the journal lock, so it never falls between a
\* capture's take and its read; a batch may be in flight.
Seal ==
    /\ ~seal.on
    /\ ~capturing
    /\ LET list == IF "seal-drops-unjournaled" \in Bugs THEN {} ELSE unj
       IN /\ seal' = [on |-> TRUE, bytes |-> bytes, kept |-> Addrs, list |-> list,
                      covered |-> nextPos - 1]
          /\ held' = list
          /\ protected' = (protected \cup writable) \ list
    /\ writable' = {}
    /\ unj' = {}
    /\ digest' = [a \in Addrs |->
                     IF \/ Digests = "drop"
                        \/ a[1] \in unj /\ "unjournaled-keeps-digests" \notin Bugs
                     THEN None ELSE digest[a]]
    /\ UNCHANGED <<bytes, capturing, taken, batch, disk, nextPos, padFrom, sel,
                   flush, sending, flushed, stores>>

Select ==
    /\ seal.on
    /\ sel' = [bytes |-> seal.bytes, kept |-> seal.kept, covered |-> seal.covered]
    /\ seal' = NoSeal
    /\ protected' = protected \cup held
    /\ held' = {}
    /\ digest' = IF Digests = "refresh"
                 THEN [a \in Addrs |-> IF a[1] \in seal.list THEN seal.bytes[a] ELSE digest[a]]
                 ELSE digest
    /\ UNCHANGED <<bytes, writable, unj, capturing, taken, batch, disk,
                   nextPos, padFrom, flush, sending, flushed, stores>>

Abandon ==
    /\ seal.on
    /\ unj' = unj \cup seal.list
    /\ digest' = [a \in Addrs |-> IF a[1] \in seal.list THEN None ELSE digest[a]]
    /\ seal' = NoSeal
    /\ protected' = protected \cup held
    /\ held' = {}
    /\ UNCHANGED <<bytes, writable, capturing, taken, batch, disk, nextPos, padFrom,
                   sel, flush, sending, flushed, stores>>

Next ==
    \/ \E p \in Pages, b \in Blocks : Store(p, b)
    \/ Send \/ Take \/ Read \/ Land \/ Answer \/ Fail
    \/ Seal \/ Select \/ Abandon

Spec == Init /\ [][Next]_vars

Symmetry == Permutations(Pages) \cup Permutations(Blocks)

-----------------------------------------------------------------------------
\* The replay of disk d. The first empty slot ends it. Over the selected
\* checkpoint it applies each entry after the covered position, in order.
Stop(d) ==
    IF \E i \in 1..MaxPos : d[i].kind = "empty"
    THEN CHOOSE i \in 1..MaxPos : d[i].kind = "empty" /\ \A j \in 1..i - 1 : d[j].kind # "empty"
    ELSE MaxPos + 1

Last(S) == CHOOSE i \in S : \A j \in S : j <= i

Replay(d) ==
    [a \in Addrs |->
        LET applied == {i \in 1..Stop(d) - 1 :
                            i > sel.covered /\ d[i].kind = "blocks" /\ d[i].blocks[a] # None}
        IN IF applied = {} THEN sel.bytes[a] ELSE d[Last(applied)].blocks[a]]

\* After a crash, every block the guest has not stored into since it sent
\* the last answered flush holds its bytes.
NoLostFlush == \A a \in flushed : Replay(disk)[a] = bytes[a]

\* After a crash, every block the guest has not stored into since the
\* selected checkpoint's seal holds its bytes.
NoRegression == \A a \in sel.kept : Replay(disk)[a] = bytes[a]

\* Every page the guest can store into without a fault is unjournaled.
WritableIsUnjournaled == writable \subseteq unj

\* A digest is what a replay would read for its block once the batch in
\* flight lands.
DigestsDescribeTheJournal ==
    LET d == IF batch # NoBatch THEN Landed(disk, batch) ELSE disk
    IN \A a \in Addrs : digest[a] # None => digest[a] = Replay(d)[a]
=============================================================================
