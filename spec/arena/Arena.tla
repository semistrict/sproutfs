------------------------------ MODULE Arena --------------------------------
(***************************************************************************)
(* The isolated arena, as docs/vm-memory.md (The isolated arena) describes *)
(* it and vmmemory/isolation.go implements it.                             *)
(*                                                                         *)
(* A VMM may be compromised, and it keeps every descriptor it is given:    *)
(* one that ignores DROP_FILE still holds the file. So the property is     *)
(* about descriptors, not mappings: no VMM ever holds a file in which a    *)
(* page of another tenant's VM is, unless that page is public.             *)
(*                                                                         *)
(* A VMM is given its region's private file, its tenant's shared file and  *)
(* the public file, and the fork file of each fork point it is a child of. *)
(* A page loaded by identity goes in the shared file of the identity's     *)
(* tenant, or the public file for a public template's, and only a region   *)
(* that may read the identity loads it (mayRead). A published page another *)
(* region inherits moves out of its owner's private file the same way. A   *)
(* fork point lends its parent's pages to children of the parent's tenant  *)
(* only, in a fork file of its own. Every file is a memfd made for it, and *)
(* one that is dropped is closed, never made again for something else.     *)
(*                                                                         *)
(* Left out: offsets, slots and the window, which decide where in a file a *)
(* page goes and not which file; the mapping protocol, which the hostile   *)
(* VMM tests exercise.                                                     *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS
    OfA,        \* the memory regions of tenant a's VMs
    OfB,        \* the memory regions of tenant b's VMs
    MaxForks,   \* fork points each region may take
    Bugs        \* defects to put back, to show the property catches each one

Public == "public"
Regions == OfA \cup OfB
Tenants == {"a", "b"}
TenantOf == [r \in Regions |-> IF r \in OfA THEN "a" ELSE "b"]

\* A page is named by the region whose VM published it, or is a public
\* template's.
Pages == Regions \cup {Public}
PageTenant(p) == IF p = Public THEN Public ELSE TenantOf[p]

\* Files: a region's private file, a tenant's shared file, the public file,
\* and the k-th fork file of a region.
Private(r) == <<"private", r, 0>>
Shared(t) == <<"shared", t, 0>>
PublicFile == <<"public", Public, 0>>
Fork(r, k) == <<"fork", r, k>>

VARIABLES
    given,  \* region -> the files its VMM holds a descriptor to
    holds,  \* file -> the pages in it
    forks,  \* region -> the fork points it has taken
    open    \* region -> its fork file now, if a seal is on

vars == <<given, holds, forks, open>>

Files == {Private(r) : r \in Regions} \cup {Shared(t) : t \in Tenants} \cup {PublicFile}
         \cup {Fork(r, k) : r \in Regions, k \in 1..MaxForks}

Init ==
    /\ given = [r \in Regions |-> {Private(r), Shared(TenantOf[r]), PublicFile}]
    \* Each region holds its own published page in its private file.
    /\ holds = [f \in Files |-> IF f[1] = "private" THEN {f[2]} ELSE {}]
    /\ forks = [r \in Regions |-> 0]
    /\ open = [r \in Regions |-> <<"none", "none", 0>>]

\* mayRead: a region reads an identity of its own tenant, or a public one.
MayRead(r, p) == PageTenant(p) = TenantOf[r] \/ PageTenant(p) = Public
                 \/ "no-mayread" \in Bugs

\* loadFile: where a page loaded or moved by identity goes: its identity's
\* tenant's shared file. Without mayRead, a region loading another tenant's
\* identity would load it where it maps from, its own tenant's.
LoadFile(r, p) ==
    IF PageTenant(p) = Public THEN PublicFile
    ELSE IF "no-mayread" \in Bugs THEN Shared(TenantOf[r])
    ELSE Shared(PageTenant(p))

Add(f, p) == holds' = [holds EXCEPT ![f] = @ \cup {p}]

\* A region loads a page by its identity.
Load(r, p) ==
    /\ MayRead(r, p)
    /\ Add(LoadFile(r, p), p)
    /\ UNCHANGED <<given, forks, open>>

\* A page a region published moves out of its private file when another
\* region inherits it. The moved page's identity is still the publisher's.
Move(r, owner) ==
    /\ owner \in holds[Private(owner)]
    /\ MayRead(r, owner)
    /\ Add(LoadFile(r, owner), owner)
    /\ UNCHANGED <<given, forks, open>>

\* A pooled arena would make a new file in a closed one's memory, so the
\* model with that defect names every fork file after the first region's.
Root == CHOOSE r \in Regions : TRUE

\* A fork point lends the parent's pages to a child in a fork file, which the
\* child is given. Only a child of the parent's tenant.
ForkLend(parent, child) ==
    /\ child # parent
    /\ TenantOf[child] = TenantOf[parent] \/ "fork-across-tenants" \in Bugs
    /\ forks[parent] < MaxForks \/ open[parent][1] = "fork"
    /\ LET k == IF open[parent][1] = "fork" THEN open[parent][3] ELSE forks[parent] + 1
           f == IF "pooled-files" \in Bugs THEN Fork(Root, 1) ELSE Fork(parent, k)
       IN /\ open' = [open EXCEPT ![parent] = f]
          /\ forks' = [forks EXCEPT ![parent] = k]
          /\ holds' = [holds EXCEPT ![f] = @ \cup holds[Private(parent)]]
          /\ given' = [given EXCEPT ![child] = @ \cup {f}]

\* The seal ends: DROP_FILE goes to every child, which a compromised one
\* ignores, the copies are given up, and the file is closed.
EndFork(parent) ==
    /\ open[parent][1] = "fork"
    /\ holds' = [holds EXCEPT ![open[parent]] = {}]
    /\ open' = [open EXCEPT ![parent] = <<"none", "none", 0>>]
    /\ UNCHANGED <<given, forks>>

Next ==
    \/ \E r \in Regions, p \in Pages : Load(r, p)
    \/ \E r, owner \in Regions : Move(r, owner)
    \/ \E parent, child \in Regions : ForkLend(parent, child)
    \/ \E r \in Regions : EndFork(r)

Spec == Init /\ [][Next]_vars

\* No VMM holds a file with a page of another tenant's VM in it, unless the
\* page is public.
Isolated ==
    \A r \in Regions, f \in Files :
        f \in given[r] => \A p \in holds[f] : PageTenant(p) \in {TenantOf[r], Public}
=============================================================================
