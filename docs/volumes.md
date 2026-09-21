# Volumes and checkpoints

A VM owns named volumes: one named `ram0` for its RAM, and one per PMEM device,
the root disk among them. All of them are published together in one
[checkpoint](#checkpoints), which is the VM's entire durable state. A filesystem
is not part of the storage abstraction.

## Geometry

Each volume is published in pages of its own size. Whoever creates the VM says
which — 4 KiB or 2 MiB, and no other size is accepted — and that choice is
recorded in every checkpoint of the volume and fixed for its life: a page number
means nothing without a page size, so a checkpoint that changed one would rename
every page of that volume. A volume's size is a whole number of 4 KiB sectors
whatever its page size, so a 2 MiB-page volume may end in a short page and a
4 KiB-page one never does.

Nothing infers a volume's geometry from its name, and nothing divides a page
number by a constant of the package doing the dividing: the root records the
geometry of every volume, and a reader — locating a range, reading a page,
summing what a checkpoint still holds, compacting it — divides by what it read
there. A page identity is `(checkpoint, volume, page)` with the page number in
that volume's own unit, so a store into one 4 KiB page renames that page and
leaves its 511 neighbours in the same 2 MiB to the checkpoints that published
them.

A volume's geometry also says how many pages one segment of its page table
covers: 256 at 2 MiB per page, 16,384 at 4 KiB. Both cover enough volume that
the root stays about fifteen bytes per segment, and little enough that a segment
encodes to a few hundred kilobytes at most — see
[the layout](#objects) for what that costs per GiB.

A host creates each volume in the page of the pager that will map it: a VM's
`ram0` at 4 KiB and each of its PMEM devices at 2 MiB. The pager, the mapping
protocol and the VMM carry that page end to end — a session states it when it
attaches — and a pager refuses a volume published in any other page size when it
is attached, which is also how a region that reached the wrong one of a host's
two pagers is caught.

## Writes

A write replaces one range in the VM's in-memory overlay and returns. A batched
write replaces several ranges as one overlay generation, so a checkpoint holds
all of them or none. A discard makes a range read as zeroes, including bytes
inherited from the checkpoint, and costs one overlay entry however large the
range is.

A write contacts nothing. It reads no checkpoint, takes no network round trip
and waits for nothing, which is the point: a guest write must never block on
object-store latency. Its payload is bounded by `MaxWriteBytes`, 2 MiB by
default. This path is for writers outside a pager — image import and tests; a
pager never writes to a volume at all.

Nothing else bounds an overlay. There is no pending-byte budget and no
back-pressure, because there is nothing to push back against: what an overlay
holds is not owed to any store, it is simply what losing this host would cost.
`Status.DirtyBytes` reports the overlay's share of that, and the manager sums it
over every VM it runs. A running guest's dirty pages are the pager's, and the
checkpoint that seals them reports those.

`Verify` makes nothing durable and orders nothing. It confirms that this handle
still owns its VM and returns; durability is a checkpoint, and a checkpoint
happens on the interval or on request, never because a guest flushed. A caller
that needs the bytes in object storage calls `Checkpoint`.

A write is refused only when the handle itself is terminal: closed, handed off,
or fenced by a later writer. A fenced handle keeps serving reads from what it
holds and even accepts writes into it, until its next checkpoint discovers the
fence; from then on every operation reports that the VM must be reopened. The
bytes it held were never durable and are gone.

## Reads

Once a VM is open, reads are served from one consistent view: the overlay above
the selected checkpoint, and the part members its root names. A read takes no
round trip but a cold page fetch. `Load` is the same call under another name,
for a pager whose fault path must not do anything else.

A read of a range is one **run** of pages, and what a cold one costs in requests
is what the layout allows rather than how many pages the run holds. The pages
are grouped by the part their members are in and by where in that part they sit:
a publication writes a volume's changed pages in page order, so the members of
consecutive pages are adjacent, and a run of them is fetched as one **extent** —
one ranged read — and decoded out of that one buffer. Two members of the same
part separated by up to 64 KiB of bytes nothing wants are read through rather
than split at, because a request costs its latency and not its length; a larger
gap splits, and so does an extent that has grown past 4 MiB. Independent parts
are fetched at once. A page no checkpoint ever wrote has no member, reads as
zeroes and costs nothing. A run covers at most 16 MiB of volume, in whole
pages, which bounds what one reader holds decoded; a longer read is several
runs, and 16 MiB is the largest read-ahead run a pager may be configured with,
so no pager's load is ever split. So a pager's cold 2 MiB read-ahead run of 512 4 KiB
pages is two requests — the segment that locates them, and the extent their
members lie in — where it was 513 before the pages of a run were grouped.

`Locate` reports the page identity of every byte of a range as sorted
adjacent extents covering it exactly, each inside one page of that volume's own
geometry. A page the overlay
touched anywhere reports this VM's next checkpoint reference for all of it,
because that is the checkpoint that will publish the whole page; it is private
and unshared until then. Every other page reports the identity the checkpoint
gives it, which a fork inherits unchanged.

It takes a context and may fetch, because the page table is segmented: locating
a page the overlay does not hold reads the segment it falls in — 512 MiB of a
2 MiB-page volume, 64 MiB of a 4 KiB-page one — which is
one member however many pages it names, and every later range inside that
segment is answered from what the handle already holds. A range inside a segment
no checkpoint has written costs nothing at all: an absent segment reads as
zeroes, as an absent page does.

## Checkpoints

A checkpoint is the published state of one VM at one write generation, and it is
the whole of that VM's durable state. Publishing one writes every dirty page of
every volume into parts, uploads them, writes the index object holding the
segments those pages changed and the root, and then selects that checkpoint in
the VM's [control record](metadata.md) — which is the moment those bytes
survive the loss of this host.

A VM's dirty pages come from two places. The overlay holds what was written
through this package. A pager holds the rest as sealed pages, supplied per
volume as a `DirtySource`: a pager page is a store page, so each sealed page is
one member of a part, read straight out of the page the guest was running on
rather than copied into this package first. When the checkpoint is selected the
publication retires every source it read, which is what makes those pages clean
under their new identity; a publication that never lands hands them back to the
guest instead. The retire is the first thing that follows the selection and the
last thing the publication lock covers: a guest whose seal still stands copies
every store it makes into a private page, so nothing else the publication has
left to do — the reclamation sweep above all — may come between the two.

This layer has no automatic trigger. `Checkpoint` publishes the overlay on
demand and does nothing when nothing is dirty, except on a fork that has not
published its root, where there is always something to publish; `Snapshot` takes
the publication lock, has its caller seal the guest, and publishes in the
background with VMM state and sealed pages attached, even when nothing is
dirty; `Close` publishes a final checkpoint, and neither `Handoff` nor
`ForkPoint` publishes anything. The interval belongs to the host that runs the
guest and knows when the vCPUs may be paused: every VM it runs is checkpointed
every `CheckpointInterval`, 60 s by default, each wait jittered by up to an
eighth either side so VMs do not checkpoint in lockstep, and the next wait is measured
from the end of the previous upload. Two captures of one guest serialize on the
publication lock.

Writes continue into a new overlay generation while a publication runs, and the
overlay entries at or below the published generation are dropped afterwards. A
failed publication changes nothing durable: the previously selected checkpoint
stays selected, the overlay keeps every byte, and the failure is reported
through `Status.CheckpointError` rather than returned to any write.

Callers quiesce writes before closing or handing a VM off. `Close` publishes a
final checkpoint, so a write accepted after that checkpoint was captured is
acknowledged and then dropped. It publishes only what the overlay holds: the
pages a managed pager holds reach storage through a capture, so a caller that
needs guest PMEM and RAM included takes one before closing.

### Handoff

A VM that is moving rather than going away hands over instead of closing.
`Handoff` publishes nothing. It waits for a publication already in flight, marks
the handle terminal — every later operation on it, including another handoff,
reports `ErrHandedOff` — and releases it. `Status` then reports `HandedOff` and
the checkpoint the control record selects, which is the state the destination
opens at.

Publishing there would be the one cost a post-copy
[migration](migration.md) exists to avoid: everything the guest wrote since the
last checkpoint is in the source pager's memory, and the destination faults it
out of them rather than out of storage. The source may not stop serving until
the destination has fetched every one of those pages. `Snapshot` is neither
Close nor Handoff: it keeps the VM and publishes in the background.

Handoff waits for a publication another call already had in flight before taking
its own. A handoff canceled while it waits has given nothing up and can be
repeated; one canceled after the mark leaves the handle handed off, and `Close`
finishes the release.

A fork that has not published its own first checkpoint is refused with
`ErrForkPending`. That handle is the only thing that could ever publish that
root: releasing it without publishing leaves an identity no host can open, and
the fork point it reads through is then retired by nobody, so its parent stays
sealed for good — never checkpointed, never fenced, never migratable. `Close` is
what gives such a fork up, and it publishes nothing either: a fork that ends
before its root was taken leaves no object behind, and closing it retires its
hold on the parent's fork point, which gives the parent its sealed pages back.
The pin stays.

Handing a VM off does not decide where it runs. Its control record is simply
free, so the next open takes it — the destination, or the source again when the
migration is abandoned after the handoff, which is an ordinary open on a host
that happens to already have the VM's pages.

### Opening

Opening reads the control record, advances its epoch — which fences whatever
host held the VM before — and reads the selected checkpoint's root. Reading the
root is one request: a GET of the checkpoint's index object, whose header says
where in it the root is. That is two objects, no page and no page table: the
root is O(segments) whatever the volume holds, and the segments load on demand
with the pages they locate. Nothing is verified up front. The overlay
starts empty, because nothing is durable between checkpoints and there is
nothing to replay.

Open does not wait for an in-flight publication: it uses the selected
checkpoint. A record whose selected checkpoint has never been published — a
fork whose first checkpoint has not landed — reports `ErrForkPending`, and the
epoch is left alone, so the host holding that fork is not fenced by an open that
could never have succeeded.

### Publication order

Publication order is fixed: the checkpoint's parts, then its index object, which
carries the root and is the commit, then the control record selects the
checkpoint. The index object is written only once every part is durable, so a
checkpoint is never openable before what it names is there, and a publication
that fails before selection is invisible and leaves only unreferenced objects.
The objects one publication writes are idempotent under its own reference — a
retry produces byte-identical objects and settles by digest — and so is
selecting a checkpoint already selected, which is how a lost conditional-write
reply is reconciled. A different root under the same reference is a conflict.

Checkpoint sequences are epoch-major — the writer epoch in the high 32 bits, a
counter starting at one in the low 32 — so a fenced writer still uploading
objects can never collide with its successor under the create-if-absent rule
every checkpoint object is written with.

A creating handle **draws** its epoch, uniformly in [1, 2³¹), rather than
starting every VM at the same one; every later open counts up from there, which
leaves at least 2³¹ takeovers. Identities are never reused — the orchestrator
allocates them and does not hand one out twice — but nothing in a deployment can
enforce that, and two VMs that started at the same epoch under one name would
allocate the same sequences: the same page identities, which a page cache
keys resident pages by, so the second VM would be served the first's bytes out
of memory; and the same object keys, which are written create-if-absent, so its
publications would collide with whatever the first left behind. A drawn epoch
makes both impossible whatever the name.

Creating a VM is refused outright when anything is already stored under its
identity and no control record accounts for it (`ErrIdentityUsed`). What is
there is either the checkpoints a deleted VM left pinned — which a fork still reads
through — or a create interrupted before its record, and neither is this VM's to
publish into. A create interrupted *after* its record is finished by repeating
it, which opens the VM instead.

A sequence is burnt by the publication that took it, whatever becomes of that
publication. Repeating objects under one reference is idempotent only inside a
single `Commit`, which rewrites byte-identical bytes; a later checkpoint seals
whatever the guest has dirtied since, so handing an abandoned sequence back
would have one reference name two contents and conflict on every interval from
then on. That is not a corner case for a pager-backed VM: a guest's stores go
into pages rather than the overlay, so nothing this package can see moves
between attempts. Burning a sequence costs nothing — the counter is 32 bits
within a writer epoch, thousands of years at a checkpoint a minute — and a
fork's root is no exception: a failed root publication burns the sequence its
record was created selecting, and the retry's selection moves the record to the
sequence the root actually landed under.

The objects a publication uploads are bounded by `Concurrency` — eight for a
caller that sizes none, and half a host's core count, between 8 and 64, for one
that does — and that budget belongs to the checkpoint store rather than to one
publication: every publication the store begins draws on the same slots, last
parts included. A host whose VMs all become dirty at once therefore uploads
with one bound rather than one per VM. A part is sealed and uploaded as soon as
it holds `PartBytes` of encoded members, 64 MiB by default, so a large
checkpoint costs a few PUTs and a bounded amount of memory while a typical one
costs a single PUT for everything it changed.

The part builders are bounded the same way. A publication takes one slot of
`MaxBuilders` — `Concurrency` for a caller that sizes none, and a quarter of a
host's core count between 2 and 8 for one that does — before it writes
its first member and holds it until it finishes. A publication that writes
nothing takes no slot.

A sealed part is held until its upload finishes, and that upload runs under an
upload slot, so a publication can hold a part in flight and be filling the next
builder at the same time. The memory publication costs a host is therefore
`MaxBuilders` plus `Concurrency` times `PartBytes` — the builders, and the
sealed parts in flight — however many of its VMs became dirty at once, rather
than one part builder per VM.

### Reclamation

A root names every checkpoint it reads — its own, whose index object holds the
root itself, the one whose index object holds each segment it addresses, and the
ones that segment's pages name, which the root records, with what each is read
for, so nothing has to open a segment to find out — so a checkpoint no root
names holds nothing anyone can reach. It names one more kind: the checkpoints
its own compaction emptied, which it reads nothing from and spares for a
checkpoint. The root carries both, so a sweep driven by a root read back out of
storage — which is every sweep after a takeover — owes that grace exactly as the
writer that published it did. Selecting checkpoint N therefore reclaims, with P
the checkpoint it replaced and C the new one:

```
dead = (named(P) ∪ {P}) − named(C) − protected
```

Each dead checkpoint goes whole, its index object first: while that object is
there the checkpoint is openable, so it stops being openable before anything it
names goes, and a sweep that cannot delete it deletes nothing else — what it
leaves is the whole checkpoint, for a repeat to sweep again. That catches a checkpoint
which stopped being read several selections ago, not only the one just replaced.
Deletion is idempotent, and a failure is logged and not retried:
everything it misses is merely unreferenced — including a sweep abandoned
because the handle it belonged to was closed under it. Deletions have a small
budget of their own, two by default, rather than the upload one: a sweep is
never urgent and must not hold the slots a checkpoint needs to become durable.

The sweep runs with the publication lock released, after the checkpoint is
durable and its pages are back with the guest. It deletes objects nothing
reads, so nothing waits for it: not the guest, not the next capture of it, and
not a caller waiting on the checkpoint. Two sweeps of one VM never contend
either, because each one's candidates come from the root it replaced and the
next starts from the root this one made current.

Four things are spared. A checkpoint the current one's compaction emptied is
left for that one checkpoint, because a reader holding the view it replaced
still reads through it. A sequence a fork was taken at is pinned in the control
record and left whole, and so is every checkpoint that one's own root names —
the ones it reads and the ones its own compaction emptied alike: that expansion
is the whole point, because a grandchild's root names those checkpoints directly
and no record here says so, and because a root naming a checkpoint nothing can
fetch is a hole in what a fork inherits. The record the selection returned is
what says which sequences are pinned, and each pinned root is read once and
remembered for the life of the store — a pin is permanent, so the answer never
goes stale. That rule covers the checkpoint just replaced like any other: a
selection over a pinned checkpoint sweeps as usual and finds nothing the pin
protects to delete, so the checkpoints it replaced that no pin and no new root
names still go. Another VM's checkpoints are never touched, which is what a
fork's inherited entries name. And a handle reclaims only the checkpoints it published itself, so
the one it opened on is left behind: a handle cannot account for what the writer
before it was doing. Those are a collector's, and there is none; a deleted VM's
own checkpoints are swept by the delete, except the ones a pin covers.

### Compaction

A page nothing rewrites keeps its checkpoint's parts alive after every other
member of them is dead, so parts would accumulate dead bytes without bound.
Every publication bounds that. While planning checkpoint N it measures each
checkpoint the new root still reads: live bytes are the lengths its entries name
in that checkpoint's parts, and the total is what the root records those parts
cost. A checkpoint less than half live is rewritten, lowest fraction first, up
to 64 MiB of live bytes — its pages are read, through the page cache where they
are already there, and written into N's parts. One with no live bytes left is
not compacted at all: nothing reads it, so it simply leaves the root.

Compaction works over the parts alone. Measuring opens no segment: each
segment entry of the root records what that segment's pages read from each
checkpoint, so live bytes are a sum over entries the checkpoint already holds —
and the segments the checkpoint itself changed it has in hand, with their new
tables. A segment is never moved and never counted as part liveness. It lives in
the index object of the checkpoint that wrote it for as long as any root
addresses it, so a checkpoint whose parts are emptied keeps its index object
while a later root still addresses a segment in it. The only segments a
compaction opens are the ones whose pages it moves, which it must rewrite
anyway.

An emptied checkpoint does not leave the root at once. Checkpoint N goes on
naming it, marked emptied by N, so that reclamation spares it for that one
checkpoint: a reader still holding the view N replaced reads its pages through
those parts, and deleting them would turn a healthy VM's read into an I/O error.
`Checkpoints` reports what a root reads from and so does not list it. Checkpoint
N+1 drops it and the sweep after that deletes it, so dead bytes in the parts a
VM still reads stay under twice its live bytes.

Rewriting a page moves its bytes, not the page. The segment that locates it
records the entry's *origin* — the checkpoint the page was first published
under — beside the checkpoint whose part now holds it, and compaction carries
the origin forward, so the page identity a fork of the older view reports and
the one the compacted root reports stay equal. A page compaction has never moved
carries no origin of its own: the checkpoint holding it is the one that
published it. A segment is identified by the checkpoint that wrote it, its
volume and its number, the way a page is by its origin, and nothing moves it, so
it needs no origin of its own.

Nothing here runs under the vCPU pause: a checkpoint's reads happen after the
guest has resumed. Another VM's checkpoints are never rewritten, and neither is
a pinned one: a pinned sequence protects every checkpoint that one's root names,
not only itself, because the fork reads its whole view through them and
rewriting one would copy bytes reclamation can never free.

### Objects

Every object lives under the identity of the VM that published it. Nothing is
named by content, with one exception: the identity of a
[template](hosting.md) is the sha256 of the guest image it holds, so that every
host names one template for one image and they import it once between them. That
names the VM and not its objects — a template's checkpoints are laid out below
exactly like any other VM's.

```
control/<id>                            control record
vm/<id>/ckpt/<seq>/index                the index object: header, segments, root
vm/<id>/ckpt/<seq>/part/<n>             the data: part n, from zero
```

A checkpoint is its data and one **index object**. The index object holds the
page table: small, rewritten in pieces every checkpoint, and read on every open.
The **parts** hold the guest bytes: large, immutable, left behind
until compaction. The index object's create-if-absent PUT is the commit — while
it is there the checkpoint is published, and until it is there the checkpoint is
absent — and it is written only once every part it names is durable.

A VM's one mutable object is kept out of its checkpoint namespace so that the
orchestrator can list the deployment's VMs without walking every checkpoint
object they have written.

A checkpoint writes what it changed into a few parts rather than one object per
page, because a PUT per dirty page would cost a PUT per page per VM per
interval. A part is a concatenation of members — the VMM state, when the
checkpoint saved one, then the changed pages in ascending volume-name and
page-number order, then compaction's rescues — followed by a table naming every
member and a fixed 32-byte trailer naming the table, the part layout version
and, in the last part alone, how many parts the checkpoint has. The table says
of each member whether it is a page or the state, and carries each page's origin
as well as its extent, for the pages compaction moved. A part therefore
describes itself; normal reads never touch the table, because the root's
segments carry the same offsets.

A part's tail is bounded, so a reader fetches its table in one request. The
table is at most 1 MiB: a part is sealed when the next member's entry would
carry its table past that, exactly as it is sealed at the 64 MiB of members it
fills to. A checkpoint of very many small members is therefore bounded by its
table rather than by its body, and writes several small parts instead of one
part with an unbounded table. A reader asks for the part's last 1 MiB plus 32
bytes as one suffix range — an object shorter than that comes back whole —
decodes the trailer from the end of what arrives and takes the table out of the
same bytes, which the trailer must locate inside them. Reading a part's table is
therefore one round trip rather than three: a HEAD for the part's size, a read
of the trailer and a read of what it named. It stays one round trip at a
megabyte, because a request costs its latency rather than its length, and it is
a request nothing on the page path makes: the root's segments carry the same
offsets, so only consistency checking and the refusal of a superseded layout
read a table at all.

The bound is a megabyte because a part must fill to its target on the bytes it
holds and not stop short on the entries naming them. A 64 MiB part of 4 KiB
pages holds 16,384 members, and one entry of a volume named as briefly as `ram0`
costs about 29 bytes — the repeated field's tag and length prefix, the name, and
the page, offset, length, state and two origin fields, which are written whether
or not they are zero. A megabyte is therefore about 36,000 of those, or about
3,700 of the widest kind a 255-byte volume name makes, and a full part of 4 KiB
pages spends about 470 KiB of it. At 256 KiB such a part was sealed after about
8,700 members, some 34 MiB, so a checkpoint of small pages cost about twice the
PUTs its bytes needed. The bound is the store's and not the part layout's — the
layout does not know how large a part may be — so raising it moved no format
version.

Part layout version 4 is the current one; another version is refused from the
trailer, before the table is parsed. The version sits sixteen bytes from the end
of a part and the magic in the last eight, where every earlier layout put them,
so a part whose trailer was a different size is still refused by the version it
names rather than as a tail that is not a trailer. Index format 8 is the current
one, and its header carries it: an object at the index key that is not one of
these is refused by the version it does carry, and a deployment written when the
root was the last member of a part — a checkpoint with no index object at
all — is refused by that part's layout version.

VMM state is inherited like a page. Only a capture pauses the guest for it, so
an interval checkpoint has none of its own and goes on naming the state member
of the checkpoint it replaces — and that checkpoint's parts — which is what
keeps the VM restorable between captures. A capture's own state member replaces
it, and compaction moves an inherited one with the pages when the parts holding
it become mostly dead.

A page is published whole or not at all, so the page table holds one entry per
page that has bytes: the checkpoint that holds them, the part, and the member's
extent within it. That table is split into **segments** of as many pages as the
volume's geometry says — 256 pages, 512 MiB, at 2 MiB per page, and 16,384
pages, 64 MiB, at 4 KiB — and a checkpoint writes the segments it changed into
its own index object. The **root** ends that object, and it is what the
checkpoint says about itself: per volume, its size and its geometry, then one
entry per segment giving the checkpoint that wrote the
segment, where in that checkpoint's index object it is, and the checkpoints that
segment's pages name. A checkpoint writes the segments whose page table it
changed and keeps the parent's entry for every other, so it writes O(changed
segments) of table rather than O(volume), whether one page changed or all of
them.

A root entry is fifteen bytes, so what a volume costs there follows from what
one of its segments covers: two entries — thirty bytes — per GiB of a
2 MiB-page volume, about 120 KiB for a 4 TiB one; sixteen entries, about 240
bytes, per GiB of a 4 KiB-page volume. `maximumRootSize` of 2 MiB is therefore
about 140,000 segments either way, which is 70 TiB of a 2 MiB-page volume and
8.5 TiB of a 4 KiB-page one.

A segment is about twenty bytes an entry, so one of a 4 KiB-page volume is about
330 KiB — 560 KiB with every entry at its widest — against a few kilobytes at
2 MiB per page. Both are inside the `maximumSegmentSize` of 1 MiB below, and
both keep a segment one range read.

An index object is a fixed 32-byte record naming the index format version and
the root's offset and length, then the segments this checkpoint changed in
volume-name and segment-number order, then the root, then the same record again.
Opening a checkpoint is one GET of its end — the last 256 KiB, which holds the
closing record and, for any VM but the very largest, the whole root; a longer
root costs one more GET of the rest of it — so what an open costs does not grow
with what the checkpoint changed, and a part is read from its end in the same
way. No reader fetches an index object whole. It is bounded at 1 GiB, which is
what a writer holds in memory and sends in one PUT: a checkpoint of a 4 KiB-page
volume writes about 5 MiB of segments per GiB of it that it dirtied, so the
bound admits one that dirtied some 200 GiB at once, which is more than a host's
dirty budget lets a VM hold unpublished.

A segment is complete on its own too: its own checkpoint list and origin list,
with each page naming both by position and carrying its number relative to the
segment's first page, about twenty bytes an entry. It is read as one range get on the index object of the checkpoint that
wrote it, through the same page cache the pages use. A segment is *identified*
by that checkpoint, its volume and its number — the way a page is identified by
its origin, its volume and its number — and the cache keys it by that identity,
so two roots addressing the same segment share one copy however each of them
found it; the offset and length are only where to fetch it.
`maximumSegmentSize` of 1 MiB bounds a segment, and `maximumRootSize` of 2 MiB
bounds a root — about 140,000 segments, above which a publication is refused.

The root is complete on its own: it lists every checkpoint it reads, including
its parent's and, for a fork, its parent VM's, and each segment entry names the
checkpoints that segment's pages locate members in *and how many bytes they hold
there*, so reclamation and compaction work from the root and never open a
segment. Those sums are computed when a segment is encoded, and a segment is
encoded whenever its entries change, so they cannot go stale. It names no parent
and carries no reference and no format version of its own: the key it lives
under names the VM and the sequence, and the index object's header carries the
version. The bytes a root records for a checkpoint are what its parts hold —
nothing of the index object is ever counted there.

An absent page reads as zeroes and so does every page of an absent segment; a
page whose bytes are all zero is dropped rather than written, and the segment
that named it is written again without it. That segment is the whole record that
the page is gone, and a segment whose last page goes loses its entry altogether.
Volume sizes are whole 4 KiB sectors whatever the page size, and every object is written with a
create-if-absent condition, so a retried publication must produce byte-identical
objects: members go into the parts as the VMM state, then each volume's changed
pages in number order, then compaction's rescues; then the index object takes
each volume's changed segments in number order, and then the root.

A page the guest touched anywhere is read back from the VM in full and written,
which is why one store into a page changes the identity of all of it. Publishing
less than a page — 4 KiB dirty tracking with patch members over a base page —
was considered and decided against on 2026-09-16: it would cut upload volume
for scattered small writes at the cost of chained reads, a larger index and
either KVM dirty logging or a compare at upload, and the owner judged the
saving not worth that. A sealed
pager page is exactly one member: a pager serves only volumes whose page is its
own, so its page and the store's page are one unit, and the upload reads the
page the guest was running on.

Each member, the root among them, uses an independent raw-or-Zstandard envelope
with decoded length and SHA-256 integrity verification. Raw fallback avoids
expansion beyond the 48-byte envelope. Checksums verify what was read; a page's
name stays the checkpoint that published it. A root is limited to 2 MiB decoded, VMM
state to 64 MiB, and a page to the page size of the volume it belongs to. One part is bounded by what it can hold:
the size it is sealed at, plus the member that filled it, its table and its
trailer. The codec uses fast Zstandard, a 1 MiB
compression window, no external dictionary, and two pools of shared synchronous
workers — one for encoding and one for decoding, so a guest's page fault never
queues behind a checkpoint's encoding, and a raw envelope takes neither. A host
sizes both from its core count (2..16 encoders, 4..32 decoders); a caller that
sizes none gets four of each;
the window is codec history, not a storage or paging unit. Every object carries the digest of its
logical contents as an attribute, so a publication that finds an object already
under its key settles from one HEAD whether it is the same publication retried
or a reference reused for other contents — it does not read back what it wrote.
A part's bytes are raw and a retry's are identical, which is what makes the
comparison exact; a member's envelope is inside those bytes and is never
compared on its own. An object written without the attribute falls back to being
read and compared. Index format 8 and part layout 4 are the layout above; the
layouts before them — version 7 among them, whose roots state no volume's page
size and whose page numbers are therefore 2 MiB pages and nothing else — the
deployments whose roots were the whole of an index
object, and the ones whose roots were part members, are all rejected. There is
no data migration.

### Page cache

One host supplies one page cache to every store and checkpoint it serves. It
fills a cap of its own — 1 GiB by default — rather than competing with the pager
for one allotment, so disposable pages can never take the memory a guest needs
and the pager never has to reclaim across a concern to get them back. Entries
are decoded pages, each charged a small bookkeeping amount on top of its bytes,
and pressure evicts the least recently used. Because a fork inherits its
parent's object keys, its reads hit the entries the parent already loaded; there
is no cache per VM.

A run of pages is one cache operation: the pages it already holds are served
from it, and the ones it does not are fetched together, as one load, in the
extents above. The load holds one slot of `MaxConcurrentLoads` — which a host
sizes from its core count and its cache arena, 16 to 256 and never more pages
than the arena holds, and which is 16 for a caller that sizes none — however
many pages it is missing, because what it issues is one request per extent and
not one per page. Concurrent readers of one page share its fetch whichever run
carried it, and each waiter can cancel independently: a load's context ends when
the last caller waiting on any of its pages has left. Readers pin their borrowed
bytes through copying, so eviction cannot return bytes still in use; an object
the cap leaves no room for is still read and copied out, just not retained. Lack
of cache capacity never fails a read.

The cached unit is the member, not the extent that fetched it. The cache is
keyed by the page's identity rather than by where its
member sits, so compaction moving those bytes into another checkpoint's parts
costs no refetch, and two readers of one page share the one copy however each of
them reached it. An extent has no identity of its own — which members it carries
depends on which run asked for it and on what else its part holds — so caching
extents would give two readers of overlapping runs two copies of the pages they
share and make a run that is half cached fetch again the half it has. A segment
is keyed by its own identity — the checkpoint that
wrote it, its volume and its number — for the same reason. Clearing it prevents in-flight loads from repopulating it. A cached
object is never evidence that a publication landed; ambiguous publication is
reconciled against object storage.

## Captures and forks

A capture pauses the guest to save VMM state and seal memory, resumes it, then
takes a checkpoint of every volume at one write generation with the VMM state
attached. It starts publication in the background and returns once the local
checkpoint exists. See [managed VM memory](vm-memory.md). Its reference is known
before its objects are uploaded, so the pages it will publish have their identity at once, and
waiting on it reports when publication became durable.

### The fork point

A checkpoint is a pause — stop the vCPUs, save the VMM state, seal the dirty
set, resume — and an upload. A fork needs only the pause, exactly as a
[migration](migration.md) does, and takes it from a parent that keeps running.

`VM.ForkPoint` runs the caller's pause under the publication lock and returns a
`ForkPoint`: the checkpoint the parent's control record already selects, pinned
in that record before the point is returned, and the pages no checkpoint of the
parent holds — everything dirty since that checkpoint, now sealed. Nothing is
published. The parent gives nothing up: it keeps its handle, its volumes and its
pages, and the sealed pages stay its own. A point whose pause seals nothing —
an imported template, which has no guest — is the published checkpoint and the
pin alone.

The pin is a conditional write from the parent's own epoch, so a fork whose
parent has been fenced is refused rather than created, and reclamation can never
delete the checkpoints the child inherits. It is a mark on the point rather than a
count of who reads it: one pin, taken when the point is made and again,
idempotently, by every `Fork` from it, so a fan-out of any size costs one and a
fork repeated after a failure costs nothing more.

Nothing gives a pin back. Retiring the point does not, deleting the child does
not, and a child that has rewritten every page it inherited does not — because
none of them can tell. A grandchild forked from that child reads the
grandparent's checkpoints through its own root, and neither the child nor the
grandparent has any way to see it, so a release reasoned from one descendant's
view cannot be right. Releasing a pin belongs to a collector, which can
survey every record and root in the deployment; see
[open work](open-work.md). Until there is one, a fork costs its parent the
checkpoint it was taken at for good, and a parent forked at many distinct
checkpoints spends one of `MaximumPins` (4096) on each.

A fork that fails after the pin — its record could not be written, its host was
lost — leaves the pin standing. That costs only eagerness: the pin says a
fork may read through that checkpoint, and one that never started reads
nothing.

A parent whose pages a fork point holds is `Status.Sealed`: one seal of a
region is outstanding at a time, so nothing may capture or fork it again, and a
capture that asks is refused before its guest is touched. The seal ends when the
last child of that point has retired it, which is when every page it inherited
is either published by the child or fetched by it. The parent's next checkpoint
then publishes those pages as its own, which is why one interval's dirty set is
uploaded twice when both sides live that long.

### The child

`Manager.Fork` pins the parent's checkpoint, which is the pin the point already
took, and creates the child's control record selecting a first checkpoint that does
not exist yet over it. The child's record names no parent: nothing would read
it, because nothing releases a pin. Any number of
children start from one point — each is one hold on it, and the seal ends when the last
is retired — so a fan-out of forks costs the parent one pause. On the parent's
host the child reads the pages written since that checkpoint through the point
itself, so they cost no copy and, once a sibling has faulted one, no second
page: every child of one fork point gives those pages the same identity.
On another host `Manager.Inherit` rebuilds the point from the pinned checkpoint
alone and the child's pager pulls those pages out of the parent's page server,
post-copy.

The child's first checkpoint is its own root: it publishes the pages it
inherited as its own and only then is the child a VM any host can open. Opening
it before that reports `ErrForkPending`, a host loss before then loses it, and a
fork that ends before it never touches the store at all — `Close` publishes no
root on its own, so a fork nothing inherited leaves only the control record it
was given.

Creating or starting a fork never loads a full disk or memory image, and a
fork's reads share its parent's objects and, within the same pager, its parent's
resident pages.

## Deletion

The caller must close the writer before deleting a VM. Deletion reads the
record, removes it unconditionally — after which nothing can open the VM — and
then deletes what that VM published: every object under its checkpoint prefix
that no pin of it covers, each checkpoint's index object first. Removing the
identity's one mutable object is what makes the identity usable again, and
deleting the objects is what leaves it usable: a create is refused while
anything is stored under an identity no record accounts for, so a VM that left
pinned checkpoints behind leaves its name refused as well as its objects, for a
collector to free.

A VM with no record is not swept at all, and repeating a delete finishes
nothing. The record is the only thing that says which objects the sweep may
take, and a VM whose delete has finished is itself a record-less identity whose
pinned objects a fork still reads: a repeat that swept what it found would
take them. What an interrupted sweep left is a collector's.

A checkpoint the record pinned is spared, with every checkpoint its root names. Those
are the fork points the VM was taken at, and a descendant may still be reading
through them: a child whose root names those checkpoints, or a grandchild whose own
root does. Nothing the delete can read says whether one does, so the objects
stay. They are a collector's, which is the only thing that can establish that
no root in the deployment reads them. A VM that was never forked therefore
takes everything with it, and one that was leaves its fork points behind
while freeing its identity.

A record that cannot be parsed is not deleted at all. Its pins are exactly what
the sweep would have to spare, and running the sweep without them would take
checkpoints out from under whoever reads them; an identity nobody can delete is the
lesser loss, and the record can be repaired.

What is left for a collector is therefore the pinned checkpoints of deleted VMs,
every pin nothing reads through any more or ever did, and what a crash leaves: the
objects of a writer that died mid-checkpoint or published after being fenced,
and the objects of a delete interrupted between the record's removal and the
sweep. See [the architecture](architecture.md#identities-and-reclamation) for
the layout that collector is built for.

## VM integration

The [Firecracker integration](vm-memory.md) maps the single `ram0` volume and
each PMEM volume through the host pager. A guest PMEM flush makes nothing
durable and completes at the device; guest PMEM and RAM stores alike remain
private pager state, resident or in scratch spill, until a checkpoint publishes
those pages. A local scratch spill is not a durability mechanism at all.
