# Volumes and checkpoints

A VM owns named volumes. The volume named `ram0` holds its RAM, and each PMEM
device has one volume. The root disk is one of the PMEM devices. All of a VM's
volumes are published together in one [checkpoint](#checkpoints). That
checkpoint is the VM's entire durable state. The storage abstraction does not
include a filesystem.

## Geometry

Each volume has its own page size, and it is published in pages of that size.
The creator of the VM chooses the page size: 4 KiB or 2 MiB. Other sizes are
refused. The page size is recorded in every checkpoint of the volume and is
fixed for the volume's life. A page number has no meaning without a page size,
so a checkpoint that changed the page size would rename every page of the
volume. A volume's size is a whole number of 4 KiB sectors for either page
size. So a volume with 2 MiB pages may end in a short page, and a volume with
4 KiB pages never does.

No code infers a volume's geometry from its name. No code divides a page number
by a constant defined in its own package. The root records the geometry of
every volume, and a reader divides by the geometry it read from the root. This
applies when a reader locates a range, reads a page, sums what a checkpoint
still holds, or compacts a checkpoint. A page identity is
`(checkpoint, volume, page)`, and the page number is in that volume's unit. So
a store into one 4 KiB page renames only that page. Its 511 neighbours in the
same 2 MiB keep the checkpoints that published them.

A volume's geometry also sets how many pages one segment of its page table
covers: 256 pages at 2 MiB per page, and 16,384 pages at 4 KiB. In both cases a
segment covers enough of the volume to keep the root at about fifteen bytes per
segment. It also covers little enough that a segment encodes to at most a few
hundred kilobytes. See [the layout](#objects) for the cost per GiB.

A host creates each volume with the page size of the pager that will map it. A
VM's `ram0` uses 4 KiB, and each of its PMEM devices uses 2 MiB. The pager, the
mapping protocol and the VMM carry that page size end to end. A session states
the page size when it attaches. A pager refuses to attach a volume published in
any other page size. This check also catches a memory region that reached the wrong
one of a host's two pagers.

## Writes

A write replaces one range in the VM's in-memory overlay and then returns. A
batched write replaces several ranges as one overlay generation, so a
checkpoint holds either all of them or none. A discard makes a range read as
zeroes, including bytes inherited from the checkpoint. A discard costs one
overlay entry regardless of the range's size.

A write does not contact any other component. It reads no checkpoint, makes no
network round trip, and does not wait. This is deliberate, because a guest
write must never block on object-store latency. `MaxWriteBytes` bounds a
write's payload, and its default is 2 MiB. This path serves writers outside a
pager, which are image import and tests. A pager never writes to a volume.

No other limit bounds an overlay. There is no pending-byte budget and no
back-pressure. Back-pressure is unnecessary because the overlay's contents are
not owed to any store. They are only what the loss of this host would cost.
`Status.DirtyBytes` reports the overlay's part of that cost, and the manager
sums it over every VM it runs. A running guest's dirty pages belong to the
pager, and the checkpoint that seals them reports them.

`Verify` does not make anything durable and does not order anything. It
confirms that this handle still owns its VM, and then returns. Only a
checkpoint makes data durable, and a checkpoint runs on the interval or on
request. A guest's flush waits for a checkpoint only when the VM's disks are
staler than the host's flush bound. A flush never starts a checkpoint of its
own. A caller that needs the bytes in object storage calls `Checkpoint`.

A write is refused only when the handle is terminal: closed, handed off, or
fenced by a later writer. A fenced handle continues to serve reads from the data
it holds, and it still accepts writes into that data. This continues until its
next checkpoint discovers the fence. After that, every operation reports that
the VM must be reopened. The bytes the handle held were never durable, and they
are lost.

## Reads

After a VM is open, reads come from one consistent view. The view is the
overlay on top of the selected checkpoint, plus the part members that the
checkpoint's root names. A read makes no round trip except to fetch a cold
page. `Load` is the same call under another name. It exists for a pager whose
fault path must do nothing else.

A read of a range is one **run** of pages. The number of requests for a cold
run depends on the layout, not on the number of pages in the run. The pages are
grouped by the part that holds their members and by their position in that
part. A publication writes a volume's changed pages in page order, so the
members of consecutive pages are adjacent. A group of adjacent members is
fetched as one **extent**, which is one ranged read, and decoded from that one
buffer. When two members of the same part are separated by up to 64 KiB of
unwanted bytes, the read includes the gap instead of splitting there. This is
cheaper because a request's cost is its latency, not its length. A larger gap
splits the extent. An extent that grows past 4 MiB also splits. Independent
parts are fetched concurrently. A page that no checkpoint ever wrote has no
member. It reads as zeroes and costs nothing.

A run covers at most 16 MiB of volume, in whole pages. This bounds how much one
reader holds decoded. A longer read is split into several runs. 16 MiB is also
the largest read-ahead run a pager may be configured with, so a pager's load is
never split. As a result, a pager's cold 2 MiB read-ahead run of 512 4 KiB
pages takes two requests: one for the segment that locates the pages, and one
for the extent that holds their members. Before the pages of a run were
grouped, it took 513.

`Locate` reports the page identity of every byte of a range. It returns sorted,
adjacent extents that cover the range exactly. Each extent lies inside one page
of that volume's geometry. If the overlay touched any part of a page, `Locate`
reports this VM's next checkpoint reference for the whole page, because that
checkpoint will publish the whole page. Until then the page is private and
unshared. Every other page reports the identity its checkpoint gives it, and a
fork inherits that identity unchanged.

`Locate` takes a context and may fetch, because the page table is segmented. To
locate a page that the overlay does not hold, it reads the segment the page
falls in. A segment covers 512 MiB of a 2 MiB-page volume or 64 MiB of a
4 KiB-page volume. Reading a segment fetches one member, regardless of how many
pages the segment names. Every later range inside that segment is answered from
data the handle already holds. A range inside a segment that no checkpoint has
written costs nothing, because an absent segment reads as zeroes, like an
absent page.

## Checkpoints

A checkpoint is the published state of one VM at one write generation. It is
the VM's entire durable state. Publishing a checkpoint has these steps:

1. Write every dirty page of every volume into parts, and upload the parts.
2. Write the index object. It holds the segments those pages changed, and the
   root.
3. Select the checkpoint in the VM's [control record](metadata.md).

The bytes survive the loss of this host from the moment of selection.

A VM's dirty pages come from two sources. The overlay holds data written
through this package. A pager holds the rest as sealed pages, which it supplies
per volume as a `DirtySource`. A pager page is the same unit as a store page, so
each sealed page becomes one member of a part. The upload reads each sealed page
directly from the page the guest was running on. It does not copy the page into
this package first. When the checkpoint is selected, the publication retires
every source it read. Retiring makes those pages clean under their new
identity. If a publication never lands, it returns the pages to the guest
instead.

The retire happens immediately after the selection, and it is the last step
under the publication lock. While a guest's seal stands, the guest copies every
store it makes into a private page. So no other remaining step of the
publication may run between the selection and the retire. This applies in
particular to the reclamation sweep.

This layer has no automatic trigger:

- `Checkpoint` publishes the overlay on demand. It does nothing when nothing is
  dirty. The exception is a fork that has not published its root. Such a fork
  always has something to publish.
- `Snapshot` takes the publication lock, has its caller seal the guest, and
  publishes in the background with VMM state and sealed pages attached. It does
  this even when nothing is dirty. `SnapshotDisks` does the same for the disks
  alone. Both take `Terms`: `Keep` keeps the checkpoint in the write that
  selects it, and `Retry` decides what a failed publication does.
- `Close` publishes a final checkpoint.
- `Handoff` and `ForkPoint` publish nothing.

The host that runs the guest owns the interval, because that host knows when the
vCPUs may be paused. The host checkpoints every VM it runs every
`CheckpointInterval`, 60 s by default. Each wait is jittered by up to an eighth
in either direction, so that VMs do not checkpoint in lockstep. The next wait is
measured from the end of the previous upload. Two captures of one guest
serialize on the publication lock.

Writes continue into a new overlay generation while a publication runs. After
the publication, the overlay entries at or below the published generation are
dropped. A failed publication changes nothing durable. The previously selected
checkpoint stays selected, and the overlay keeps every byte. The failure is
reported through `Status.CheckpointError`. It is not returned to any write.

Callers quiesce writes before they close a VM or hand it off. `Close` publishes
a final checkpoint. A write accepted after that checkpoint was captured is
acknowledged and then dropped. `Close` publishes only what the overlay holds.
The pages a managed pager holds reach storage only through a capture. So a
caller that needs guest PMEM and RAM included takes a capture before closing.

### Handoff

A VM that moves to another host hands off instead of closing. `Handoff`
publishes nothing. It waits for any publication already in flight, then marks
the handle terminal, then releases it. After the mark, every operation on the
handle reports `ErrHandedOff`, including another handoff. `Status` then reports
`HandedOff` and the checkpoint that the control record selects. The destination
opens at that checkpoint.

Publishing at handoff would add the cost that post-copy
[migration](migration.md) exists to avoid. Everything the guest wrote since the
last checkpoint is in the source pager's memory. The destination faults those
pages from that memory, not from storage. The source must keep serving until the
destination has fetched every one of those pages. `Snapshot` is neither a close
nor a handoff. It keeps the VM and publishes in the background.

Handoff waits for a publication that another call already had in flight before
it takes its own turn. A handoff canceled during that wait has given nothing up
and can be repeated. A handoff canceled after the mark leaves the handle handed
off, and `Close` finishes the release.

A handoff of a fork that has not published its own first checkpoint is refused
with `ErrForkPending`. Only that handle can ever publish the fork's root.
Releasing the handle without publishing would leave an identity that no host
can open. Also, nobody would then retire the fork point that the fork reads
through. The parent would stay sealed permanently, so it could never be
checkpointed, fenced or migrated. `Close` is how such a fork is given up, and
`Close` publishes nothing for it either. A fork that ends before its root was
taken leaves no object behind. Closing it retires its hold on the parent's fork
point, which returns the sealed pages to the parent. The pin remains.

Handing a VM off does not decide where it runs next. Its control record is
free, so the next open takes it. That open may come from the destination. It may
also come from the source, if the migration is abandoned after the handoff.
That is an ordinary open on a host that already has the VM's pages.

### Opening

Opening reads the control record, advances its epoch, and reads the selected
checkpoint's root. Advancing the epoch fences any host that held the VM before.
Reading the root is one request: a GET of the checkpoint's index object. The
object's header says where the root is inside it. Opening therefore reads two
objects, and no page and no page table. The root is O(segments) regardless of
what the volume holds, and segments load on demand with the pages they locate.
Nothing is verified up front. The overlay starts empty, because nothing is
durable between checkpoints and there is nothing to replay.

Open does not wait for an in-flight publication. It uses the selected
checkpoint. A record may select a checkpoint that has never been published. This
is the case for a fork whose first checkpoint has not landed. Opening such a
record reports `ErrForkPending` and leaves the epoch unchanged. So an open that
could never succeed does not fence the host that holds the fork.

### Publication order

Publication order is fixed:

1. The checkpoint's parts.
2. Its index object. The index object carries the root and is the commit.
3. The control record's selection of the checkpoint.

The index object is written only after every part is durable. So a checkpoint
never becomes openable before the objects it names exist. A publication that
fails before selection is invisible and leaves only unreferenced objects. The
objects one publication writes are idempotent under its reference. A retry
produces byte-identical objects and settles by digest. Selecting a checkpoint
that is already selected is also idempotent. This is how a lost reply to a
conditional write is reconciled. A different root under the same reference is a
conflict.

Checkpoint sequences are epoch-major. The writer epoch is in the high 32 bits,
and a counter starting at one is in the low 32 bits. Every checkpoint object is
written create-if-absent. So a fenced writer that is still uploading objects
can never collide with its successor.

A creating handle **draws** its epoch uniformly from [1, 2³¹). It does not start
every VM at the same epoch. Every later open counts up from the drawn epoch,
which leaves at least 2³¹ takeovers. Identities are never reused, because the
orchestrator allocates them and never issues one twice. But nothing in a
deployment can enforce that. If two VMs with one name started at the same
epoch, they would allocate the same sequences, with two consequences:

- They would have the same page identities. A page cache keys resident pages by
  page identity, so the second VM would be served the first VM's bytes from
  memory.
- They would have the same object keys. Objects are written create-if-absent,
  so the second VM's publications would collide with the objects the first VM
  left behind.

A drawn epoch prevents both, regardless of the name.

Creating a VM is refused when anything is already stored under its identity and
no control record accounts for it (`ErrIdentityUsed`). The stored objects are
one of two things. They are either checkpoints that a deleted VM left pinned,
which a fork still reads through, or the objects of a create that was
interrupted before it wrote its record. This VM must not publish into either. A
create interrupted *after* it wrote its record is finished by repeating it. The
repeat opens the VM instead.

A publication burns the sequence it took, whatever happens to the publication.
Repeating objects under one reference is idempotent only within a single
`Commit`, which rewrites byte-identical bytes. A later checkpoint seals
everything the guest has dirtied since. If an abandoned sequence were reused,
one reference would name two contents, and every later interval would conflict.
This is a normal case for a pager-backed VM, not a rare one. The guest's stores
go into pages, not into the overlay, so this package sees nothing change
between attempts. Burning a sequence costs nothing. The counter has 32 bits per
writer epoch, which lasts thousands of years at one checkpoint a minute. A
fork's root follows the same rule. A failed root publication burns the sequence
that the fork's record selected when it was created. The retry's selection
moves the record to the sequence that the root actually landed under.

`Concurrency` bounds the objects a publication uploads. It is eight for a
caller that does not size it. For a caller that does, it is half the host's core
count, between 8 and 64. The budget belongs to the checkpoint store, not to one
publication. Every publication the store starts draws on the same slots,
including for its last parts. So a host whose VMs all become dirty at once
uploads under one bound, not one bound per VM. A part is sealed and uploaded as
soon as it holds `PartBytes` of encoded members, 64 MiB by default. So a large
checkpoint costs a few PUTs and a bounded amount of memory, and a typical
checkpoint costs a single PUT for everything it changed.

Part builders are bounded in the same way. A publication takes one slot of
`MaxBuilders` before it writes its first member, and it holds the slot until it
finishes. `MaxBuilders` equals `Concurrency` for a caller that does not size it.
For a caller that does, it is a quarter of the host's core count, between 2 and
8. A publication that writes nothing takes no slot.

A sealed part is held in memory until its upload finishes. The upload runs
under an upload slot, so a publication can have one part in flight while it
fills the next builder. The memory that publication costs a host is therefore
`MaxBuilders` plus `Concurrency` times `PartBytes`: the builders, plus the
sealed parts in flight. This bound holds however many of the host's VMs became
dirty at once. It is not one part builder per VM.

### Reclamation

A root names every checkpoint it reads:

- its own checkpoint, whose index object holds the root;
- the checkpoint whose index object holds each segment the root addresses;
- the checkpoints that each segment's pages name.

The root records the last group, and what each checkpoint is read for, so
nothing has to open a segment to find them. A checkpoint that no root names
holds nothing anyone can reach.

A root also names one more kind of checkpoint: the checkpoints that its own
compaction emptied. The root reads nothing from them, and it spares them for one
checkpoint. The root carries both kinds. So a sweep driven by a root read back
from storage owes the same grace period as the writer that published the root.
Every sweep after a takeover is driven that way. Selecting checkpoint N
therefore reclaims the following set, where P is the checkpoint it replaced and
C is the new one:

```
dead = (named(P) ∪ {P}) − named(C) − protected
```

Each dead checkpoint is deleted whole, index object first. While the index
object exists, the checkpoint is openable. Deleting it first makes the
checkpoint unopenable before anything it names is deleted. If a sweep cannot
delete the index object, it deletes nothing else. It leaves the whole
checkpoint for a later sweep to try again. This rule catches a checkpoint that
stopped being read several selections ago, not only the one just replaced.
Deletion is idempotent. A failure is logged and not retried, because everything
it misses is only unreferenced. That includes a sweep abandoned because its
handle was closed during the sweep. Deletions have their own small budget, two
by default, separate from the upload budget. A sweep is never urgent and must
not hold the slots a checkpoint needs to become durable.

The sweep runs after the publication lock is released, once the checkpoint is
durable and its pages are back with the guest. It deletes only objects that
nothing reads, so nothing waits for it. The guest does not wait, the guest's
next capture does not wait, and a caller waiting on the checkpoint does not
wait. Two sweeps of one VM never contend. Each sweep's candidates come from the
root it replaced, and the next sweep starts from the root this sweep made
current.

Reclamation spares five things:

1. A checkpoint emptied by the current checkpoint's compaction is kept for that
   one checkpoint. A reader that holds the replaced view still reads through
   it.
2. A sequence that a fork was taken at is pinned in the control record and kept
   whole. Every checkpoint that the pinned checkpoint's root names is also kept,
   both the ones it reads and the ones its own compaction emptied. This
   expansion is essential for two reasons. A grandchild's root names those
   checkpoints directly, and no record here says so. Also, a root that names a
   checkpoint nobody can fetch leaves a hole in what a fork inherits. The record
   that the selection returned says which sequences are pinned. Each pinned
   root is read once and remembered for the life of the store. A pin is
   permanent, so the remembered answer never goes stale. This rule applies to
   the checkpoint just replaced like any other. A selection over a pinned
   checkpoint sweeps as usual and finds nothing the pin protects to delete. The
   replaced checkpoints that no pin and no new root names are still deleted.
3. A sequence a checkpoint request kept is spared the same way, with every
   checkpoint its root names, so a VM can be created from it later. Unlike a
   pin, a keep can be released while no fork was taken from it. The release
   sweeps the released checkpoint as if it had just been replaced: its own
   checkpoints that the selected root does not name and nothing else protects
   are deleted. See [metadata](metadata.md#kept-checkpoints).
4. Another VM's checkpoints are never touched. A fork's inherited entries name
   such checkpoints.
5. A handle reclaims only checkpoints it published itself. So it leaves behind
   the checkpoint it opened on, because a handle cannot account for what the
   previous writer was doing. Those checkpoints belong to a collector, and no
   collector exists. The delete sweeps a deleted VM's own checkpoints, except
   the ones a pin covers.

### Compaction

A page that nothing rewrites keeps its checkpoint's parts alive after every
other member of those parts is dead. Without a bound, parts would accumulate
dead bytes indefinitely. Every publication bounds this. While it plans
checkpoint N, it measures each checkpoint that the new root still reads. Live
bytes are the lengths that the root's entries name in that checkpoint's parts.
The total is what the root records those parts cost. A checkpoint that is less
than half live is rewritten, lowest live fraction first, up to 64 MiB of live
bytes. Its pages are read, through the page cache when they are already cached,
and written into N's parts. A checkpoint with no live bytes left is not
compacted. Nothing reads it, so it just leaves the root.

Compaction works only on parts. Measuring opens no segment. Each segment entry
in the root records what that segment's pages read from each checkpoint. So
live bytes are a sum over entries the checkpoint already holds. The checkpoint
also already has the segments it changed, with their new tables. A segment is
never moved and never counts toward part liveness. It stays in the index object
of the checkpoint that wrote it as long as any root addresses it. So a
checkpoint whose parts are emptied keeps its index object while a later root
still addresses a segment in it. Compaction opens only the segments whose pages
it moves, and it must rewrite those segments anyway.

An emptied checkpoint does not leave the root immediately. Checkpoint N keeps
naming it, marked as emptied by N, so reclamation spares it for that one
checkpoint. A reader that still holds the view N replaced reads its pages
through those parts. Deleting the parts would turn a healthy VM's read into an
I/O error. `Checkpoints` reports what a root reads from, so it does not list an
emptied checkpoint. Checkpoint N+1 drops it, and the sweep after that deletes
it. So the dead bytes in the parts a VM still reads stay under twice its live
bytes.

Rewriting a page moves its bytes but does not change the page. The segment that
locates the page records the entry's *origin*, which is the checkpoint the page
was first published under. It records the origin next to the checkpoint whose
part now holds the page. Compaction carries the origin forward. So the page
identity that a fork of the older view reports equals the identity that the
compacted root reports. A page that compaction has never moved has no separate
origin, because the checkpoint that holds it is the one that published it. A
segment is identified by the checkpoint that wrote it, its volume and its
number, as a page is identified by its origin. Nothing moves a segment, so a
segment needs no separate origin.

None of this runs during the vCPU pause. A checkpoint's reads happen after the
guest has resumed. Another VM's checkpoints are never rewritten, and a pinned
or kept checkpoint is never rewritten. A pinned or kept sequence protects every
checkpoint that its root names, not only itself. The fork reads its whole view through those
checkpoints, and rewriting one would copy bytes that reclamation can never
free.

### Objects

Every object is stored under the identity of the VM that published it. No
object is named by its content, with one exception. The identity of a
[template](hosting.md) is the sha256 of the guest image it holds. So every host
uses the same template name for the same image, and the hosts import it only
once between them. This names the VM, not its objects. A template's checkpoints
use the same layout as any other VM's checkpoints:

```
control/<id>                            control record
vm/<id>/ckpt/<seq>/index                the index object: header, segments, root
vm/<id>/ckpt/<seq>/part/<n>             the data: part n, from zero
```

A VM of a tenant, `<tenant>/<name>`, has the same keys under that tenant's
namespace: `tenants/<tenant>/control/<name>` and `tenants/<tenant>/vm/<name>/`.
Every key a tenant has is under `tenants/<tenant>/`, so deleting that prefix
removes the tenant and no other. A fork across tenants is refused before
anything is written (`volume.ErrOtherTenant`), because a fork reads its
parent's pages by their identity, which is the one way a page could cross.

A checkpoint consists of its data and one **index object**. The index object
holds the page table. It is small, rewritten in pieces every checkpoint, and
read on every open. The **parts** hold the guest bytes. They are large and
immutable, and they remain until compaction. The index object's
create-if-absent PUT is the commit. While the index object exists, the
checkpoint is published. Before it exists, the checkpoint is absent. The index
object is written only after every part it names is durable.

The control record is a VM's only mutable object. It is kept outside the
checkpoint namespace, so that the orchestrator can list the deployment's VMs
without walking every checkpoint object they have written.

A checkpoint writes its changes into a few parts, not one object per page. A PUT
per dirty page would cost a PUT per page per VM per interval. A part has this
layout:

1. A concatenation of members: the VMM state if the checkpoint saved one, then
   the changed pages in ascending volume-name and page-number order, then
   compaction's rescues.
2. A table that names every member.
3. A fixed 32-byte trailer. It names the table and the part layout version. In
   the last part only, it also states how many parts the checkpoint has.

The table says whether each member is a page or the state. For each page it
carries the extent and also the origin, for the pages that compaction moved. So
a part describes itself. Normal reads never touch the table, because the root's
segments carry the same offsets.

A part's tail is bounded, so a reader fetches the table in one request. The
table is at most 1 MiB. A part is sealed when the next member's entry would
push its table past that size. This works the same way as sealing at 64 MiB of
members. So a checkpoint of very many small members is bounded by its table
rather than by its body. It writes several small parts instead of one part with
an unbounded table. A reader requests the part's last 1 MiB plus 32 bytes as
one suffix range. An object shorter than that is returned whole. The reader
decodes the trailer from the end of the returned bytes and takes the table from
the same bytes. The trailer must locate the table inside those bytes. Reading a
part's table is therefore one round trip instead of three. The three would be a
HEAD for the part's size, a read of the trailer, and a read of the table. It
stays one round trip at a megabyte, because a request's cost is its latency, not
its length. The page path never makes this request, because the root's segments
carry the same offsets. Only consistency checking and the refusal of a
superseded layout read a table.

The bound is a megabyte so that a part fills to its target size in bytes. It
must not stop early because of the entries that name those bytes. A 64 MiB part
of 4 KiB pages holds 16,384 members. One entry for a volume with a short name
like `ram0` costs about 29 bytes. The entry holds the repeated field's tag and
length prefix, the name, and the page, offset, length, state and two origin
fields. These fields are written even when they are zero. So a megabyte holds
about 36,000 such entries, or about 3,700 of the widest kind, which a 255-byte
volume name produces. A full part of 4 KiB pages uses about 470 KiB of the
bound. With the earlier 256 KiB bound, such a part was sealed after about 8,700
members, about 34 MiB. A checkpoint of small pages then cost about twice the
PUTs its bytes needed. The bound belongs to the store, not to the part layout,
because the layout does not know how large a part may be. So raising the bound
did not change any format version.

Part layout version 4 is current. A part with any other version is refused from
its trailer, before the table is parsed. The version is sixteen bytes from the
end of a part, and the magic is in the last eight bytes. Every earlier layout
put them in the same places. So a part whose trailer had a different size is
still refused by the version it names. It is not rejected as a tail that is not
a trailer. Index format 8 is current, and the index object's header carries it.
An object at the index key that is not format 8 is refused by the version it
carries. Some older deployments stored the root as the last member of a part,
so their checkpoints have no index object. Those are refused by that part's
layout version.

VMM state is inherited like a page. A checkpoint that captured no state keeps
naming the state member of the checkpoint it replaces, and that checkpoint's
parts. A capture's own state member replaces the inherited one. When the parts
that hold an inherited state member become mostly dead, compaction moves it with
the pages. Two kinds of checkpoint name no state:

- A cold boot's checkpoint, because a cold boot discards the memory that the
  state described.
- `SnapshotDisks`, the host's interval checkpoint of a VM's disks, because its
  disks are not the ones that any earlier state was captured over.

A VM opened at either kind of checkpoint is booted, not restored.

A page is published whole or not at all. So the page table holds one entry per
page that has bytes: the checkpoint that holds them, the part, and the member's
extent within that part. The table is split into **segments**. The volume's
geometry sets a segment's page count: 256 pages (512 MiB) at 2 MiB per page,
and 16,384 pages (64 MiB) at 4 KiB. A checkpoint writes the segments it changed
into its own index object. The **root** is at the end of that object and
describes the checkpoint. For each volume, the root records the volume's size
and geometry, then one entry per segment. Each segment entry gives:

- the checkpoint that wrote the segment;
- where the segment is in that checkpoint's index object;
- the checkpoints that the segment's pages name.

A checkpoint writes the segments whose page table it changed. It keeps the
parent's entry for every other segment. So it writes O(changed segments) of
table, not O(volume), whether one page changed or all of them.

A root entry is fifteen bytes. A volume's cost in the root follows from how much
of the volume one segment covers:

- A 2 MiB-page volume costs two entries, thirty bytes, per GiB. That is about
  120 KiB for a 4 TiB volume.
- A 4 KiB-page volume costs sixteen entries, about 240 bytes, per GiB.

`maximumRootSize` of 2 MiB therefore allows about 140,000 segments for either
page size. That is 70 TiB of a 2 MiB-page volume, or 8.5 TiB of a 4 KiB-page
volume.

A segment costs about twenty bytes per entry. So a segment of a 4 KiB-page
volume is about 330 KiB, or 560 KiB with every entry at its widest. A segment
of a 2 MiB-page volume is a few kilobytes. Both sizes are within the
`maximumSegmentSize` of 1 MiB described below. Both keep a segment to one range
read.

An index object has this layout:

1. A fixed 32-byte record that names the index format version and the root's
   offset and length.
2. The segments this checkpoint changed, in volume-name and segment-number
   order.
3. The root.
4. The same 32-byte record again.

Opening a checkpoint is one GET of the object's end, the last 256 KiB. That
range holds the closing record and, for all but the very largest VMs, the whole
root. A longer root costs one more GET for the rest of it. So the cost of an
open does not grow with how much the checkpoint changed. A part is read from its
end in the same way. No reader fetches a whole index object. An index object is
bounded at 1 GiB, which is what a writer holds in memory and sends in one PUT.
A checkpoint of a 4 KiB-page volume writes about 5 MiB of segments per GiB of
the volume that it dirtied. So the bound admits a checkpoint that dirtied about
200 GiB at once. That is more than a host's dirty budget lets a VM hold
unpublished.

A segment is also self-contained. It has its own checkpoint list and origin
list. Each page names both by position and carries its number relative to the
segment's first page, at about twenty bytes per entry. A segment is read as one
range GET on the index object of the checkpoint that wrote it, through the same
page cache that the pages use. A segment is *identified* by that checkpoint, its
volume and its number. A page is identified in the same way, by its origin, its
volume and its number. The cache keys a segment by its identity. So two roots
that address the same segment share one cached copy, however each root found
it. The offset and length only say where to fetch the segment.
`maximumSegmentSize` of 1 MiB bounds a segment. `maximumRootSize` of 2 MiB
bounds a root, at about 140,000 segments. A publication with a larger root is
refused.

The root is also self-contained. It lists every checkpoint it reads, including
its parent's and, for a fork, its parent VM's. Each segment entry names the
checkpoints in which that segment's pages locate members, *and how many bytes
they hold there*. So reclamation and compaction work from the root and never
open a segment. These sums are computed when a segment is encoded. A segment is
encoded whenever its entries change, so the sums cannot go stale. The root
names no parent, and it carries no reference and no format version. The key it
is stored under names the VM and the sequence, and the index object's header
carries the version. The bytes a root records for a checkpoint are the bytes
that checkpoint's parts hold. They never include any of the index object.

An absent page reads as zeroes, and so does every page of an absent segment. A
page whose bytes are all zero is dropped instead of written, and the segment
that named it is written again without it. That segment is the only record
that the page is gone. A segment whose last page is dropped loses its entry.
Volume sizes are whole 4 KiB sectors for either page size. Every object is
written with a create-if-absent condition, so a retried publication must
produce byte-identical objects. Members go into the parts in this order: the
VMM state, then each volume's changed pages in number order, then compaction's
rescues. The index object then takes each volume's changed segments in number
order, and then the root.

If the guest touched any part of a page, the whole page is read back from the VM
and written. This is why one store into a page changes the identity of the
whole page. Publishing less than a page was considered and rejected on
2026-09-16. That design would use 4 KiB dirty tracking with patch members over
a base page. It would reduce upload volume for scattered small writes. Its costs
would be chained reads, a larger index, and either KVM dirty logging or a
compare at upload. The owner judged that the saving was not worth those costs.
A sealed pager page is exactly one member. A pager serves only volumes whose
page size matches its own, so the pager's page and the store's page are the
same unit. The upload reads the page the guest was running on.

Each member, including the root, uses an independent raw-or-Zstandard envelope,
with the decoded length and SHA-256 integrity verification. The raw fallback
prevents expansion beyond the 48-byte envelope. Checksums verify what was read.
A page's name remains the checkpoint that published it. The size limits are:

- A root is limited to 2 MiB decoded.
- VMM state is limited to 64 MiB.
- A page is limited to the page size of its volume.
- A part is bounded by the size it is sealed at, plus the member that filled
  it, its table and its trailer.

The codec uses fast Zstandard with a 1 MiB compression window and no external
dictionary. It has two pools of shared synchronous workers, one for encoding and
one for decoding. So a guest's page fault never queues behind a checkpoint's
encoding. A raw envelope uses neither pool. A host sizes both pools from its
core count: 2 to 16 encoders and 4 to 32 decoders. A caller that does not size
them gets four of each. The window is codec history, not a storage or paging
unit.

Every object carries the digest of its logical contents as an attribute. When a
publication finds an object already under its key, one HEAD tells it whether
the object is from the same publication retried or from a reference reused for
other contents. The publication does not read back what it wrote. A part's
bytes are raw and a retry's bytes are identical, so the comparison is exact. A
member's envelope is inside those bytes and is never compared separately. An
object written without the attribute is read and compared instead.

Index format 8 and part layout 4 are the layout described above. All earlier
layouts are rejected. These include version 7, whose roots state no volume's
page size, so its page numbers are always 2 MiB pages. They also include the
deployments whose roots were an entire index object, and the deployments whose
roots were part members. There is no data migration.

### Page cache

A host supplies one page cache to every store and checkpoint it serves. The
cache has its own cap, 1 GiB by default. It does not share one allotment with
the pager. So disposable pages can never take memory that a guest needs, and
the pager never has to reclaim across concerns to get that memory back. Entries
are decoded pages. Each entry is charged its bytes plus a small bookkeeping
amount. Under pressure, the least recently used entries are evicted. A fork
inherits its parent's object keys, so its reads hit the entries that the parent
already loaded. There is no cache per VM.

A run of pages is one cache operation. The pages already in the cache are
served from it. The missing pages are fetched together as one load, in the
extents described above. The load holds one slot of `MaxConcurrentLoads`
regardless of how many pages it is missing, because it issues one request per
extent, not one per page. A host sizes `MaxConcurrentLoads` from its core count
and its cache arena: 16 to 256, and never more than the number of pages the
arena holds. It is 16 for a caller that does not size it. Concurrent readers of
one page share its fetch, whichever run carried the page. Each waiter can cancel
independently. A load's context ends when the last caller waiting on any of its
pages has left. Readers copy the bytes they borrow, so eviction cannot return
bytes that are still in use. An object too large for the cap is still read and
copied out, but it is not retained. A lack of cache capacity never fails a
read.

The cache stores members, not the extents that fetched them. It is keyed by
the page's identity, not by the member's location. So compaction moving those
bytes into another checkpoint's parts costs no refetch. Two readers of one page
share one copy, however each reader reached it. An extent has no identity,
because which members it carries depends on which run requested it and on what
else its part holds. Caching extents would give two readers of overlapping runs
two copies of the pages they share. It would also make a half-cached run fetch
again the half it already has. For the same reason, a segment is keyed by its
own identity: the checkpoint that wrote it, its volume and its number. Clearing
the cache prevents in-flight loads from repopulating it. A cached object is
never evidence that a publication landed. An ambiguous publication is
reconciled against object storage.

## Captures and forks

A capture pauses the guest, saves VMM state, seals memory, and resumes the
guest. It then takes a checkpoint of every volume at one write generation, with
the VMM state attached. It starts publication in the background and returns once
the local checkpoint exists. See [managed VM memory](vm-memory.md). The
capture's reference is known before its objects are uploaded, so the pages it
will publish have their identity immediately. Waiting on the capture reports
when publication became durable.

### The fork point

A checkpoint has two steps. The first is a pause: stop the vCPUs, save the VMM
state, seal the dirty set, and resume. The second is an upload. A fork needs
only the pause, as a [migration](migration.md) does. It takes the pause from a
parent that keeps running.

`VM.ForkPoint` runs the caller's pause under the publication lock and returns a
`ForkPoint`. A `ForkPoint` holds:

- the checkpoint that the parent's control record already selects, pinned in
  that record before the point is returned;
- the pages that no checkpoint of the parent holds. These are everything dirty
  since that checkpoint, now sealed.

Nothing is published. The parent gives nothing up. It keeps its handle, its
volumes and its pages, and the sealed pages still belong to it. A pause may seal
nothing, as for an imported template, which has no guest. Such a point is only
the published checkpoint and the pin.

The pin is a conditional write from the parent's own epoch. So a fork whose
parent has been fenced is refused, not created, and reclamation can never delete
the checkpoints the child inherits. The pin marks the point. It does not count
the readers of the point. There is one pin. It is taken when the point is made,
and again, idempotently, by every `Fork` from the point. So a fan-out of any
size costs one pin, and a fork repeated after a failure costs nothing more.

No operation releases a pin. Retiring the point does not release it. Deleting
the child does not. A child that has rewritten every page it inherited does not
either. None of them can tell whether the pin is still needed. A grandchild
forked from that child reads the grandparent's checkpoints through its own root,
and neither the child nor the grandparent can see that. So a release based on
one descendant's view cannot be correct. Releasing a pin is a job for a
collector, which can survey every record and root in the deployment. See
TASK-24 in the [backlog](../backlog/tasks). Until a collector exists, a fork permanently costs
its parent the checkpoint it was taken at. A parent forked at many distinct
checkpoints uses one of `MaximumPins` (4096) for each.

A fork can fail after the pin, for example because its record could not be
written or its host was lost. The pin then stays. The only cost is that the pin
was taken early. The pin says that a fork may read through that checkpoint, and
a fork that never started reads nothing.

A parent whose pages a fork point holds is `Status.Sealed`. Only one seal of a
memory region can be outstanding at a time, so the parent cannot be captured or forked
again. A capture request is refused before its guest is touched. The seal ends
when the last child of that point has retired it. A child retires it when every
page it inherited is either published by the child or fetched by it. The
parent's next checkpoint then publishes those pages as its own. This is why one
interval's dirty set is uploaded twice when both parent and child live that
long.

### The child

`Manager.Fork` pins the parent's checkpoint. This is the same pin that the
point already took. It then creates the child's control record. The record
selects a first checkpoint, over the parent's checkpoint, that does not exist
yet. The child's record names no parent, because nothing would read that field:
nothing releases a pin. Any number of children can start from one point. Each
child is one hold on the point, and the seal ends when the last child is
retired. So a fan-out of forks costs the parent one pause.

On the parent's host, the child reads the pages written since that checkpoint
through the point. So those pages cost no copy. After a sibling has faulted a
page, the page also costs no second page, because every child of one
fork point gives those pages the same identity. On another host,
`Manager.Inherit` rebuilds the point from the pinned checkpoint alone. The
child's pager then pulls those pages from the parent's page server, post-copy.

`Manager.InheritPublished` builds the same point over a published checkpoint of
a VM that nothing need run, such as a stopped VM. There is no writer to pin
with, so it pins the checkpoint first without the epoch. It may name only the
published checkpoint the record selects, a kept one, or one a pin already
holds. See [metadata](metadata.md#the-control-record). A child of such a point
resumes from the checkpoint's VMM state when it has one
([hosting](hosting.md#creating-a-vm-from-a-checkpoint)).

`Manager.Release` gives up a kept checkpoint that no fork was taken from and
sweeps what only it held.

The child's first checkpoint is its own root. It publishes the pages the child
inherited as the child's own. Only after that can any host open the child.
Before then, opening the child reports `ErrForkPending`, and a host loss loses
the child. A fork that ends before its first checkpoint never touches the
store. `Close` does not publish a root, so such a fork leaves only the control
record it was given.

A child need not ever run. `Host.CaptureInto` creates a child on the parent's
host and publishes its root straight from the fork point, with the VMM state
the point saved, and then closes it. See
[hosting](hosting.md#capturing-a-vm-into-a-new-vm).

Creating or starting a fork never loads a full disk or memory image. A fork's
reads share its parent's objects and, within the same pager, its parent's
resident pages.

## Deletion

The caller must close the writer before deleting a VM. Deletion has these
steps:

1. Read the record.
2. Remove the record, on the condition that it is still the version read. A
   record that moved is read again, so a pin added without the writer in
   between is spared. After this, nothing can open the VM.
3. Delete what the VM published: every object under its checkpoint prefix that
   no pin of the VM covers. Each checkpoint's index object is deleted first. A
   kept checkpoint no fork was taken from is deleted with the rest.

Removing the identity's only mutable object makes the identity usable again.
Deleting the objects keeps it usable. A create is refused while anything is
stored under an identity that no record accounts for. So a VM that left pinned
checkpoints behind also leaves its name refused, along with its objects, until a
collector frees them.

A VM with no record is not swept, and repeating a delete finishes nothing. The
record is the only thing that says which objects the sweep may take. A VM whose
delete has finished is also an identity with no record, and a fork may still
read its pinned objects. A repeated delete that swept whatever it found would
delete them. What an interrupted sweep leaves belongs to a collector.

A checkpoint that the record pinned is spared, along with every checkpoint its
root names. These are the fork points the VM was taken at, and a descendant may
still read through them. That descendant may be a child whose root names those
checkpoints, or a grandchild whose own root names them. Nothing the delete can
read says whether such a descendant exists, so the objects stay. They belong to
a collector, because only a collector can establish that no root in the
deployment reads them. So a VM that was never forked takes all its objects with
it. A VM that was forked leaves its fork points behind and still frees its
identity.

A record that cannot be parsed is not deleted. Its pins are what the sweep
would have to spare. A sweep without them would delete checkpoints that their
readers still need. An identity that nobody can delete is the smaller loss, and
the record can be repaired.

A collector is therefore left with:

- the pinned checkpoints of deleted VMs;
- every pin that nothing reads through any more, or that nothing ever read
  through;
- what a crash leaves: the objects of a writer that died mid-checkpoint or
  published after being fenced, and the objects of a delete interrupted between
  the record's removal and the sweep.

See [the architecture](architecture.md#identities-and-reclamation) for the
layout that the collector is built for.

## Billing

An embedder bills each page to the VM that published it.
`volume.StoredBytes(ctx, store, prefix, tenant)` reports what one tenant's VMs
hold, per VM. A VM's bytes are its control record and every object under
`vm/<id>/`. The empty tenant reports the VMs of no tenant. The host API serves
the same report at `GET /stored?tenant=<tenant>`.

The number is what the store lists, not a count kept beside it. So it is
exact. It includes whatever a crash, a fence or an interrupted sweep left
behind, and it excludes whatever reclamation deleted.

Every object is stored under the VM that published it, and so is every page:

- A fork reads its parent's pages where the parent published them. Those
  bytes stay the parent's. A pin keeps them after the parent is deleted, so a
  deleted VM that was ever forked stays in the report, with no record, until a
  collector frees what it pinned.
- Compaction rewrites a page only into a later checkpoint of the VM that
  published it, never into another VM's. The page keeps its origin. So the
  bill moves with the bytes only within that VM. For one checkpoint the VM pays
  for both copies. The sweep after the next checkpoint deletes the old copy.
- Reclamation deletes a VM's own checkpoints, so it reduces only that VM's
  bill, by exactly the bytes it deleted.

The report costs one listing of the tenant's control records and one of its
checkpoint objects. That is a LIST request per thousand keys, and no GET or
HEAD. A VM has one record. A checkpoint has its index object and one part per
64 MiB it wrote. So a tenant of a thousand VMs with ten checkpoints each costs
about twenty-one requests. The report is for a billing run, not for polling,
so it is not part of `/status`.

The [deployment check](testing.md#the-deployment-check) holds the bill to the
store. Each tenant's report must match what a listing of the whole deployment
holds under each VM. Every part must hold only members its own VM published.

## VM integration

The [Firecracker integration](vm-memory.md) maps the single `ram0` volume and
each PMEM volume through the host pager. A guest PMEM flush reaches the pager
and completes when the host says the VM's disks are fresh enough. Guest PMEM
stores and RAM stores both remain private pager state, resident or in scratch
spill, until a checkpoint publishes those pages. A local scratch spill is not a
durability mechanism.
