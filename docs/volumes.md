# Volumes and checkpoints

A VM owns named volumes. `ram0` holds its RAM, and each PMEM device, including
the root disk, has one. All of a VM's volumes are published together in one
[checkpoint](#checkpoints), which is the VM's entire durable state. An
[ephemeral disk](#ephemeral-disks) is the exception: no checkpoint holds it.
The storage layer has no filesystem.

## Geometry

Each volume has its own page size, 4 KiB or 2 MiB, chosen by the VM's creator.
Other sizes are refused. The page size is recorded in every checkpoint of the
volume and is fixed for its life, because changing it would rename every page.
A volume's size is a whole number of 4 KiB sectors, so a 2 MiB-page volume may
end in a short page.

No code infers geometry from a volume's name or divides a page number by a
constant. Every reader divides by the geometry the root records, whether it
locates a range, reads a page, sums what a checkpoint holds, or compacts. A page
identity is `(checkpoint, volume, page)` in that volume's unit, so a store into
one 4 KiB page renames only that page, not its 511 neighbours in the same
2 MiB.

Geometry also sets how many pages one segment of the page table covers: 256 at
2 MiB and 16,384 at 4 KiB. That keeps the root at about fifteen bytes per
segment and a segment at most a few hundred kilobytes ([objects](#objects)).

A host creates each volume with the page size of the pager that will map it:
2 MiB unless the deployment runs that pager at 4 KiB
(`SPROUTFS_RAM_PAGE_BYTES`, `SPROUTFS_PMEM_PAGE_BYTES`). A session states its
page size when it attaches, and a pager refuses a volume of another page size.
This also catches a memory region that reached the wrong one of a host's
pagers.

## Writes

A write replaces one range in the VM's in-memory overlay and returns. A batched
write replaces several ranges as one overlay generation, so a checkpoint holds
all of them or none. A discard makes a range read as zeroes, including
inherited bytes, and costs one overlay entry of any size.

A write reads no checkpoint, makes no round trip and does not wait.
`MaxWriteBytes` bounds its payload, 2 MiB by default. This path serves image
import and tests. A pager never writes to a volume.

Nothing else bounds an overlay. Its contents are owed to no store; they are what
losing this host would cost. `Status.DirtyBytes` reports that, and the manager
sums it over its VMs. A running guest's dirty pages belong to the pager.

`Verify` confirms that this handle still owns its VM. It makes nothing durable
and orders nothing. Only a checkpoint makes data durable, so a caller that needs
the bytes in object storage calls `Checkpoint`.

A write is refused only when the handle is terminal: closed, handed off, or
fenced. A fenced handle keeps serving reads and accepting writes until its next
checkpoint discovers the fence. Then every operation reports that the VM must
be reopened, and the bytes the handle held are lost.

## Reads

An open VM reads one view: the overlay over the selected checkpoint, and the
part members its root names. A read makes no round trip except to fetch a cold
page. `Load` is the same call, for a pager's fault path.

A read of a range is one **run** of pages. The pages are grouped by the part
that holds their members and by position in it. A publication writes a volume's
changed pages in page order, so consecutive pages' members are adjacent. Each
group is fetched as one **extent**, one ranged read, and decoded from that
buffer. A gap of up to 64 KiB between two wanted members of a part is read
rather than split, because a request's cost is its latency, not its length. A
larger gap, or an extent past 4 MiB, splits. Parts are fetched concurrently. A
page no checkpoint wrote has no member, reads as zeroes and costs nothing.

A run covers at most 16 MiB of volume, in whole pages, which bounds what one
reader holds decoded. A longer read is split. 16 MiB is also the largest
read-ahead run a pager may use, so a pager's load is never split. A pager's
cold 2 MiB read-ahead run of 512 4 KiB pages takes two requests: one for the
segment and one for the extent. Before the pages of a run were grouped, it took
513.

`Locate` reports the page identity of every byte of a range, as sorted,
adjacent extents that each lie inside one page. A page the overlay touched
anywhere reports this VM's next checkpoint reference, because that checkpoint
will publish the whole page. Every other page reports the identity its
checkpoint gives it, which a fork inherits unchanged.

`Locate` takes a context and may fetch, because the page table is segmented.
For a page the overlay does not hold, it reads the segment the page falls in
(512 MiB of a 2 MiB-page volume, 64 MiB of a 4 KiB-page volume): one member,
decoded into the segment's **page table**. The [page cache](#page-cache) keeps
the table under the segment's identity, so later ranges in that segment,
through any handle, are answered from it. A segment no checkpoint wrote reads
as zeroes and costs nothing.

A range costs one lookup per segment it crosses. `Locate` reads the range's
entries off the table in order (`pageTable.locate`), so a fault's window of
2,048 pages is one lookup and a scan
([planning a fault](vm-memory.md#planning-a-fault)).

## Ephemeral disks

An ephemeral disk is a volume that no checkpoint holds
(`VolumeSpec.Ephemeral`), for a writable layer the guest does not need after a
failure, such as a sandbox's scratch filesystem. Its only copy is in the pager
of the host that runs the VM, in the arena or the pager's spill file.

The volume package holds none of its bytes:

- A write or a discard is refused with `ErrEphemeral`.
- A read returns zeroes.
- A pager's sealed pages for it are refused with `ErrEphemeral`.

Every checkpoint records the disk in its root with its name, size, page size and
an `ephemeral` marker, and no segment. A publication offered a page of it fails
before writing any part (`checkpoint.ErrEphemeral`). The deployment check
refuses a root that addresses a segment of one, and a part that holds a member
of one.

So:

- **A host loss** loses it. The VM opens elsewhere with the disk zeroed at the
  size its root records.
- **A stop** or a suspend publishes the other disks and loses this one.
- **A fork** leaves it out. Every child gets it zeroed.
- **A migration** carries it. The source reports every page of it as
  unpublished, so the destination fetches all of them before the source is
  released.

A guest restored over a zeroed disk, such as a fork's child or a VM resumed
from a capture, finds the disk empty.

A fork can give its child its own ephemeral disks (`Manager.Fork` with added
specs). A create uses this to give a new VM the disk it asked for. A disk added
under the name of one the parent has takes the new size. A cold boot may resize
an ephemeral disk, because it holds nothing then.

Older roots carry no marker, so every volume in them is checkpointed. The marker
did not change the index format. A build that predates it refuses a root that
carries it, as it refuses any root field it does not know.

## Checkpoints

A checkpoint is the published state of one VM at one write generation:

1. Write every dirty page of every volume into parts, and upload them.
2. Write the index object, with the segments those pages changed and the root.
3. Select the checkpoint in the VM's [control record](metadata.md). From then
   on the bytes survive the loss of this host.

Dirty pages come from the overlay, and from a pager as sealed pages, which it
supplies per volume as a `DirtySource`. A pager page is a store page, so each
sealed page becomes one member, read directly from the page the guest ran on.
When the checkpoint is selected, the publication retires every source it read,
which makes those pages clean under their new identity. A publication that
never lands returns the pages to the guest.

The retire is the last step under the publication lock, right after the
selection. While a seal stands, the guest copies every store into a private
page, so nothing else, including the reclamation sweep, may run between the
selection and the retire.

This layer has no automatic trigger:

- `Checkpoint` publishes the overlay on demand. It does nothing when nothing is
  dirty, except for a fork that has not published its root.
- `Snapshot` takes the publication lock, has its caller seal the guest, and
  publishes in the background with VMM state and sealed pages attached, even
  when nothing is dirty. `SnapshotDisks` does the same for the disks alone.
  Both take `Terms`: `Keep` keeps the checkpoint in the write that selects it,
  and `Retry` decides what a failed publication does.
- `Close` publishes a final checkpoint.
- `Handoff` and `ForkPoint` publish nothing.

The host owns the interval ([loss model](architecture.md#loss-model)). Two
captures of one guest serialize on the publication lock.

Writes continue into a new overlay generation while a publication runs. After
it, the overlay entries at or below the published generation are dropped. A
failed publication changes nothing durable: the previous checkpoint stays
selected, the overlay keeps every byte, and the failure is reported in
`Status.CheckpointError`, not to any write.

Callers quiesce writes before closing or handing off. A write accepted after
`Close`'s checkpoint was captured is acknowledged and dropped. `Close` publishes
only what the overlay holds. A managed pager's pages reach storage only through
a capture, so a caller that needs guest PMEM and RAM takes a capture before
closing.

### Handoff

A VM that moves to another host hands off instead of closing. `Handoff`
publishes nothing. It waits for any publication in flight, marks the handle
terminal, and releases it. After the mark every operation reports
`ErrHandedOff`, and `Status` reports `HandedOff` and the selected checkpoint,
where the destination opens. Everything written since that checkpoint is in the
source pager's memory, and the destination faults it from there
([migration](migration.md)). A handoff canceled while it waits has given nothing
up and can be repeated. One canceled after the mark leaves the handle handed
off, and `Close` finishes the release.

A handoff of a fork that has not published its first checkpoint is refused with
`ErrForkPending`. Only that handle can publish the fork's root. Releasing it
would leave an identity no host can open, and the fork point would never be
retired, so the parent would stay sealed and could not be checkpointed, fenced
or migrated. `Close` gives such a fork up and publishes nothing. A fork that
ends before its root was taken leaves no object behind. Closing it retires its
hold on the fork point, which returns the sealed pages to the parent. The pin
remains.

A handoff does not decide where the VM runs next. The next open takes it: the
destination, or the source if the migration is abandoned.

### Opening

Opening reads the control record, advances its epoch, which fences any host
that held the VM, and reads the selected checkpoint's root with one GET of its
index object. It reads no page and no page table. The root is O(segments), and
segments load on demand. Nothing is verified up front. The overlay starts
empty.

A 4 KiB-page volume's page tables also load lazily
([fault planning](measurements/gce-fault-planning-2026-10-04.md)). Loading all
64 of a 4 GiB volume's at open took 85 ms from the cluster and 130 ms from the
store, sixteen at a time, before the guest could run. It grows to about 1.4 s
and 320 MiB of tables at 64 GiB. Loaded by the first fault that touches each
segment, a table costs that fault 2 to 6 ms more, once per segment.

Open does not wait for an in-flight publication. A record that selects a
checkpoint never published, as for a fork whose first checkpoint has not
landed, reports `ErrForkPending` and leaves the epoch unchanged.

### Publication order

1. The parts.
2. The index object, which carries the root and is the commit.
3. The control record's selection.

The index object is written only after every part is durable, so a checkpoint
never becomes openable before what it names exists. A publication that fails
before selection leaves only unreferenced objects. A retry produces
byte-identical objects and settles by digest, and reselecting is idempotent; this
is how a lost reply is reconciled. A different root under the same reference is
a conflict.

Sequences are epoch-major, every object is written create-if-absent, and a
creating handle draws its epoch at random
([fencing and selection](metadata.md#fencing-and-selection)). With a fixed
starting epoch, two VMs of one name would allocate the same sequences: a page
cache would serve the second VM the first's bytes by page identity, and its
publications would collide with the first's objects. Creating a VM is refused
with `ErrIdentityUsed` when anything no control record accounts for is stored
under its identity.

A publication burns its sequence even if it fails. Rewrites under one
reference are idempotent only within one `Commit`. A later checkpoint seals
everything dirtied since, so reusing an abandoned sequence would make one
reference name two contents, and every later interval would conflict. This is
common for a pager-backed VM, whose stores go to pages, not the overlay. The
32-bit counter per epoch lasts thousands of years at one checkpoint a minute. A
fork's failed root publication burns the sequence its record selected at
creation, and the retry's selection moves the record to the sequence the root
landed under.

`Concurrency` bounds the objects the checkpoint store uploads at once, across
all its publications: eight if the caller does not size it, otherwise half the
host's cores, between 8 and 64. A part is sealed and uploaded once it holds
`PartBytes` of encoded members, 64 MiB by default, so a typical checkpoint
costs one PUT.

A publication takes one slot of `MaxBuilders` before its first member and holds
it to the end. `MaxBuilders` equals `Concurrency` if unsized, otherwise a
quarter of the host's cores, between 2 and 8. A publication that writes nothing
takes no slot. A sealed part stays in memory until its upload finishes, so a
publication can have parts in flight while it fills the next.

A publication encodes pages in parallel. Encoding a page, its XXH3-128 and its
Zstandard, is nearly all the CPU a publication spends. The publication reads
its pages in page order on its own goroutine and hands them to the store's
encoders in batches: a 2 MiB page alone, or small pages up to 1 MiB together.
It keeps one more batch than there are encoders, and takes each batch's encoder
in the order it filled them, so a later batch never holds the encoder an
earlier one waits for. Parts take the envelopes in read order, so a part holds
the same bytes however the encodes finish, and a retry writes the same parts.
Each page's segment entry is written as the page lands in its part. Encoding
one page at a time ran at about 62 MB/s on a 4-vCPU Cascade Lake host
([measurement](measurements/gce-publication-throughput-2026-10-04.md)).

The memory publication costs a host is `MaxBuilders` plus `Concurrency` times
`PartBytes`, plus, per builder, one more batch than there are encoders. It does
not grow with the number of dirty VMs. A publication waiting for a builder slot
holds no batch.

### Reclamation

A root names every checkpoint it reads: its own, the ones whose index objects
hold the segments it addresses, and the ones those segments' pages name. The
root records the last group and what each is read for, so nothing opens a
segment to find them. It also names the checkpoints its own compaction emptied,
and spares them for one checkpoint. A sweep driven by a root read back from
storage, as every sweep after a takeover is, therefore owes the same grace as
the writer. Selecting checkpoint C over P reclaims:

```
dead = (named(P) ∪ {P}) − named(C) − protected
```

Each dead checkpoint is deleted whole, index object first, so it becomes
unopenable before anything it names goes. If a sweep cannot delete the index
object, it deletes nothing else of that checkpoint and leaves it for a later
sweep. Deletion is idempotent. A failure is logged and not retried, because
what it misses is only unreferenced. Deletions have their own budget, two by
default, apart from the upload budget.

The sweep runs after the publication lock is released, once the checkpoint is
durable and its pages are back with the guest. Nothing waits for it. Two sweeps
of one VM never contend: each starts from the root the previous one made
current.

Reclamation spares:

1. A checkpoint emptied by the current checkpoint's compaction, for that one
   checkpoint, because a reader that holds the replaced view still reads
   through it.
2. A pinned sequence, and every checkpoint its root names, read or emptied. A
   grandchild's root names those checkpoints directly, and a root that names a
   checkpoint nobody can fetch leaves a hole in what a fork inherits. The record
   the selection returns says which sequences are pinned. Each pinned root is
   read once and remembered, since a pin is permanent. A selection over a
   pinned checkpoint still deletes what no pin and no new root names.
3. A kept sequence, the same way. A release sweeps the released checkpoint as
   if it had just been replaced ([metadata](metadata.md#kept-checkpoints)).
4. Another VM's checkpoints, which a fork's inherited entries name.
5. Checkpoints this handle did not publish, such as the one it opened on,
   because it cannot account for what the previous writer was doing. They
   belong to a collector, and no collector exists.

### Compaction

A page that nothing rewrites keeps its parts alive after the rest of their
members are dead. Every publication bounds this. While it plans checkpoint N, it
measures each checkpoint the new root reads: the live bytes are the lengths the
root's entries name in its parts, against what the root records those parts
cost. A checkpoint less than half live is rewritten, lowest live fraction
first, up to 64 MiB of live bytes. Its pages are read, through the page cache
when cached, and written into N's parts. A checkpoint with no live bytes
leaves the root.

Measuring opens no segment, because each segment entry in the root records what
its pages read from each checkpoint. A segment is never moved and never counts
toward part liveness. It stays in the index object of the checkpoint that wrote
it while any root addresses it, so an emptied checkpoint keeps its index object
while that holds. Compaction opens only the segments whose pages it moves,
which it must rewrite anyway.

N keeps naming an emptied checkpoint, marked as emptied by N, so reclamation
spares it for one checkpoint: a reader that holds the view N replaced still
reads through those parts. `Checkpoints` reports what a root reads from, so it
does not list an emptied checkpoint. N+1 drops it, and the sweep after that
deletes it. So the dead bytes in the parts a VM reads stay under twice its live
bytes.

Rewriting a page moves its bytes but not its identity. The segment records the
entry's *origin*, the checkpoint the page was first published under, beside the
checkpoint whose part now holds it. Compaction carries the origin forward, so a
fork of the older view and the compacted root report the same identity. A page
never moved has no separate origin. A segment is identified by the checkpoint
that wrote it, its volume and its number, and is never moved, so it needs no
origin.

None of this runs during the vCPU pause. Another VM's checkpoints are never
rewritten, nor is a pinned or kept checkpoint or any checkpoint its root names:
a fork reads its whole view through them, and rewriting one would copy bytes
that reclamation can never free.

### Objects

Every object is stored under the identity of the VM that published it. No
object is named by its content. A [template](hosting.md)'s VM identity is the
sha256 of its guest image, so every host uses one name for one image and the
hosts import it once between them. A template of no tenant is public: a VM of
any tenant forks it, and its objects lie outside every tenant's namespace, so
no tenant is billed for them and deleting a tenant leaves them. The layout:

```
control/<id>                            control record
vm/<id>/ckpt/<seq>/index                the index object: header, segments, root
vm/<id>/ckpt/<seq>/part/<n>             the data: part n, from zero
```

A VM of a tenant, `<tenant>/<name>`, has the same keys under
`tenants/<tenant>/`: `tenants/<tenant>/control/<name>` and
`tenants/<tenant>/vm/<name>/`. A fork across tenants is refused before anything
is written (`volume.ErrOtherTenant`), because a fork reads its parent's pages by
identity.

The index object holds the page table. It is small, rewritten in pieces every
checkpoint, and read on every open. The parts hold the guest bytes. They are
large, immutable, and kept until compaction. The control record is outside the
checkpoint namespace, so the orchestrator can list VMs without walking their
checkpoint objects.

A checkpoint writes its changes into a few parts, not one object per dirty page.
A part is:

1. Members: the VMM state if the checkpoint saved one, then the changed pages in
   ascending volume-name and page-number order, then compaction's rescues.
2. A table that names every member: whether it is a page or the state, its
   extent, and the origin of a page compaction moved.
3. A fixed 32-byte trailer that names the table and the part layout version,
   and, in the last part only, how many parts the checkpoint has.

Reads of pages never touch the table, because the root's segments carry the
same offsets. Only consistency checking and the refusal of a superseded layout
read it.

The table is at most 1 MiB. A part is sealed when the next member's entry would
push its table past that, as it is at 64 MiB of members. A reader requests the
part's last 1 MiB plus 32 bytes as one suffix range, and an object shorter than
that is returned whole. It decodes the trailer and the table from those bytes,
instead of a HEAD and two reads. The bound lets a part fill to its byte target.
An entry for a 4 KiB page of a volume with a short name like `ram0` is about 29
bytes, because every field is written even when zero. So a megabyte holds about
36,000 entries, or about 3,700 at a 255-byte volume name, and a full 64 MiB
part of 16,384 4 KiB pages uses about 470 KiB. With the earlier 256 KiB bound,
such a part was sealed at about 8,700 members, about 34 MiB, at twice the PUTs.
The bound belongs to the store, not to the layout, so raising it changed no
version.

Part layout 5 and index format 9 are current. All earlier layouts are refused,
and nothing is migrated. A part's version is sixteen bytes from its end and its
magic in the last eight, where every earlier layout put them, so an older part
is refused by the version it names before its table is parsed. The index
object's header carries its version. Index format 8 and part layout 4 differed
only in their envelopes, which were of version 1. Version 7 roots state no page
size, so their page numbers are 2 MiB pages. Deployments whose roots were a
whole index object, or a member of the last part, are refused as well.

VMM state is inherited like a page. A checkpoint that captured no state names
the state member of the checkpoint it replaces, and compaction moves that member
with the pages. A capture's own state replaces it. Two kinds of checkpoint name
no state, and a VM opened at either is booted, not restored:

- a cold boot's, because a cold boot discards the memory the state described;
- `SnapshotDisks`, the host's interval checkpoint of a VM's disks, because no
  earlier state was captured over those disks.

A page is published whole or not at all, so the page table holds one entry per
page that has bytes: the checkpoint that holds them, the part, and the member's
extent. The table is split into **segments** of 256 pages (512 MiB) at 2 MiB
and 16,384 pages (64 MiB) at 4 KiB. A checkpoint writes the segments it changed
into its own index object and keeps its parent's entry for every other, so it
writes O(changed segments) of table, not O(volume). The **root**, at the end of
the index object, records each volume's size and geometry, then one entry per
segment:

- the checkpoint that wrote the segment;
- where the segment is in that checkpoint's index object;
- the checkpoints the segment's pages name.

A root entry is fifteen bytes. A 2 MiB-page volume costs two entries, thirty
bytes, per GiB, about 120 KiB at 4 TiB. A 4 KiB-page volume costs sixteen,
about 240 bytes, per GiB. `maximumRootSize`, 2 MiB decoded, allows about
140,000 segments: 70 TiB at 2 MiB pages, or 8.5 TiB at 4 KiB. A publication
with a larger root is refused. A segment costs about twenty bytes per page
entry: about 330 KiB for a 4 KiB-page volume, 560 KiB with every entry at its
widest, and a few kilobytes at 2 MiB. Both are within `maximumSegmentSize`,
1 MiB, and one range read.

An index object is:

1. A fixed 32-byte record that names the format version and the root's offset
   and length.
2. The segments this checkpoint changed, in volume-name and segment order.
3. The root.
4. The same record again.

Opening reads the object's last 256 KiB, which holds the closing record and,
for all but the largest VMs, the whole root. A longer root costs one more GET.
No reader fetches a whole index object. An index object is bounded at 1 GiB,
which a writer holds in memory and sends in one PUT. A 4 KiB-page volume writes
about 5 MiB of segments per GiB it dirtied, so the bound admits about 200 GiB
dirtied at once, more than a host's dirty budget lets a VM hold.

A segment is self-contained. It has its own checkpoint and origin lists, and
each page names both by position and carries its number relative to the
segment's first page. It is read as one range GET of the index object of the
checkpoint that wrote it, through the page cache, which keys it by its identity
(that checkpoint, its volume and its number) and keeps it decoded. Two roots
that address one segment share one copy.

The root is also self-contained. It lists every checkpoint it reads, including
its parent's and, for a fork, its parent VM's. Each segment entry names the
checkpoints its pages locate members in *and how many bytes they hold there*.
These sums are computed when the segment is encoded, so they cannot go stale,
and reclamation and compaction never open a segment. The root names no parent
and carries no reference and no version: its key names the VM and the
sequence, and the index header carries the version. The bytes it records for a
checkpoint are that checkpoint's part bytes, never its index object.

An absent page, and every page of an absent segment, reads as zeroes. A page
whose bytes are all zero is dropped, and its segment is written again without
it. A segment whose last page is dropped loses its entry. Every object is
written create-if-absent, so a retried publication must produce byte-identical
objects. Members go into the parts as the VMM state, then each volume's changed
pages in number order, then compaction's rescues. The index object takes each
volume's changed segments in number order, then the root.

If the guest touched any part of a page, the whole page is read back and
written, so one store renames the whole page. Publishing less than a page, with
4 KiB dirty tracking and patch members over a base page, was rejected on
2026-09-16: it would reduce uploads for scattered small writes, at the cost of
chained reads, a larger index, and either KVM dirty logging or a compare at
upload.

Each member, including the root, is an independent raw-or-Zstandard envelope
with its decoded length and an XXH3-128 check. The raw fallback caps expansion
at the 32-byte envelope header. `internal/blob/blob.go` describes the header.
Envelope format 2 is current. Format 1, SHA-256 in a 48-byte header, is
refused with its version named.

The digest finds corruption, such as a torn write, a flipped bit or a wrong
stripe. It does not resist a forger: only the deployment's own hosts write what
a host reads, and a forger who could write there could write any digest.
SHA-256 of a 2 MiB page took 5.7 ms on Cascade Lake, which has no SHA
instructions ([measurement](measurements/gce-dependent-reads-2026-10-03.md)).
On an Apple M5 Pro, XXH3-128 of a 2 MiB page takes 0.1 ms and SHA-256 0.8 ms
(`BenchmarkDigest` in `internal/blob`).

The size limits are:

- a root, 2 MiB decoded;
- VMM state, 64 MiB;
- a page, its volume's page size;
- a part, the size it is sealed at plus the member that filled it, its table
  and its trailer.

The codec is fast Zstandard with a 1 MiB window and no dictionary. It has
separate pools of synchronous workers for encoding and decoding, so a page
fault never queues behind a checkpoint's encoding. A raw envelope uses neither.
A host sizes the pools from its cores: 2 to 16 encoders and 4 to 32 decoders.
An unsized caller gets four of each.

Every object carries the XXH3-128 of its contents, in hex, as an attribute. A
publication that finds an object already under its key compares it with one
HEAD, which tells its own retry from a reused reference. It never reads back
what it wrote. An object without the attribute is read and compared.

### Page cache

A host supplies one page cache to every store and checkpoint it serves. It has
its own cap, 1 GiB by default, apart from the pager's, so cached pages never
take memory a guest needs. Entries are decoded pages, each charged its bytes
plus bookkeeping, and the least recently used are evicted. A fork shares its
parent's object keys, so it hits what the parent loaded. There is no cache per
VM.

A run is one cache operation. Cached pages are served, and the missing ones are
fetched together, in extents, under one slot of `MaxConcurrentLoads`. A host
sizes `MaxConcurrentLoads` from its cores and its cache arena, 16 to 256 and at
most the pages the arena holds; it is 16 if unsized. Concurrent readers of one
page share its fetch, whichever run carried it. Each can cancel, and a load's
context ends when its last waiter leaves. Readers copy the bytes they borrow,
so eviction cannot take bytes in use. An object too large for the cap is read
and copied but not kept. A lack of capacity never fails a read.

The cache stores members keyed by page identity, not extents, so compaction
moving bytes costs no refetch and two readers of a page share one copy. An
extent has no identity, since what it carries depends on the run that asked for
it; caching extents would duplicate overlapping runs and refetch half-cached
ones. A segment is keyed by its own identity.

The cache keeps a segment as its **page table**: decoded once into an array of
one twenty-byte entry per page up to the last it locates
(`checkpoint/pagetable.go`), 320 KiB for a whole segment of a 4 KiB-page
volume. Every index that addresses the segment uses that table and checks it
against its own root. Tables are charged to the cache's budget and evicted with
the pages. A table in use is never the one evicted. A publication leaves the
tables of the segments it wrote in the cache, so the VM that published keeps
faulting without fetching them. A store made with no cache, as tools and tests
make, keeps the tables in each index for the index's life.

Decoding a 4 KiB-page segment through the generated protobuf took about 6 ms
and 7.6 MB of allocation on GCE, once per index that read it
([fault first](measurements/gce-fault-first-2026-10-04.md)). The table is
parsed straight off the wire. It accepts and refuses exactly what the generated
message did (`FuzzPageTableDecodesAsTheProtobufDid`), and on an Apple M5 it
decodes a whole segment in 0.64 ms with 5 allocations, against 2.3 to 3.8 ms
and 16,559.

Clearing the cache prevents in-flight loads from repopulating it. A cached
object is never evidence that a publication landed. An ambiguous publication is
reconciled against object storage.

### The page cache's disk

The page cache has a second tier on the host's own disk (`CacheConfig.Disk`).
It holds each member's and each segment's encoded envelope, byte for byte,
keyed by the same identity as the memory tier, so a read from it is checked
like a read from the store. Nothing is published from it. A newer checkpoint's
page has a new identity, so the copy it replaced is never read for it.

Outside the share the cluster cache is on for, the disk holds what a **pull**
copied (every page of one checkpoint, and the segments that locate them, for a
VM [marked to pull its memory](hosting.md#pulling-a-vms-memory)) and what that
VM's later checkpoints published, which each publication writes to the disk as
it uploads. A read that misses in memory looks on the disk before the store.

Inside the share, the disk is one part of the cluster's cache, and **fills**
put windows on it ([filling the cluster](hosting.md#filling-the-cluster)). A
read of the store, once its callers have their pages, and a publication, once
each part and then the index object is durable, split each window they have
under the membership's code. This host's own stripes go to its disk through one
bounded queue of writes, and the rest go to their holders as keeps. The disk
takes a keep only for a window its membership ranks it for, and drops a stripe
it already holds or is writing. A fill that finds the queue full is dropped,
and the store serves what it did not put there.

Inside the share, a run's pages that miss in memory are grouped by window. For
each window the read takes this host's own stripes, asks k+1 of the window's
ranks for theirs, and rebuilds each page from any k distinct indices. Only a
page the cluster cannot rebuild is read from the store, and that read fills the
cluster behind it ([reading from the cluster](hosting.md#reading-from-the-cluster)).
A stripe a peer sends is checked as an item read from this disk is: its key,
index, code and checksum, then the page by its envelope.

**The log.** The disk is a log of fixed-size **disk regions**, 64 MiB each
(`CacheConfig.DiskRegionBytes`). One region is open at a time. Its space is
allocated when it opens, where the file supports it, so a write never fails
half way through a region. Envelopes are appended as they arrive. Each is an
**item** with a header: its key, its stripe's index and code, its length, the
length of the envelope it is a stripe of, and a CRC32C of the header and the
bytes. A host alone keeps every envelope whole, as stripe 0 of the code 1+0. A
host that follows the membership keeps the stripes the membership ranks its
disk for, under the membership's code, and a read rebuilds an envelope from any
k of them of one code: the membership's, or an earlier one
([the code](hosting.md#the-code)). `checkpoint/diskformat.go` describes the
format, version 3. A file of version 1 (whole envelopes with no envelope
length) or 2 (envelopes of version 1) is emptied.

When the open region is full, it is **closed**: the region's items are synced,
the region's table is written at its end, and the region is synced again. The
table holds the region's sequence number, each item's key, offset and length,
and the file's generation, under its own checksum. The first sync keeps a table
from naming an item that is not on the disk. The file's first region-sized span
holds only its header, so every region starts on a region boundary.

**Restarts.** The disk outlives the host process. A page's bytes never change
under its identity, so nothing on the disk can be stale, only absent or
damaged, and every read checks for both. The file's header holds its format
version, the region size, the cache's identity (drawn when the file is made,
reported in `DiskStats.Identity`), the deployment (the object store's kind,
bucket and prefix) and a generation. When the cache is made
(`CacheConfig.Deployment`), the header is checked:

- A file with no header, a damaged one, or one of another format, region size
  or deployment is emptied and made again under a new identity and generation.
  Page identities are unique only within one deployment.
- Otherwise each region is read back from its end. A region with a table of
  this file's generation is indexed from its table. Tables are read newest
  first by sequence number, so closed regions keep their order in the log, and
  the newest copy of an item a second chance wrote twice is the one kept.
- A region with no table was open when the host stopped. It is given back.
- A region whose table fails its checksum is scanned by its items' headers.
  Each intact item is indexed and a damaged one is skipped. The region counts
  as the oldest. A scan that finds nothing intact gives the region back.
- A region whose table is of another generation was left by an older file and
  is given back.

The rebuilt index keeps to its memory bound: the newest regions are indexed
first, and the region that would pass the bound is given back with every older
one. The disk then gives regions back, oldest first, until it holds its share
less one region, before it serves a read. A region damaged after its table was
written costs a store read, because every read checks the key and the checksum.
A clean close of the cache closes the open region with its table. A slot opened
again has its old trailer cleared first, so a table that a failed punch left is
never read as the new region's. `checkpoint/diskrestart.go` holds the rules.

**Where the file is.** The host keeps the file in a node directory that
outlives the pod (`SPROUTFS_CACHE_DIR`), on the filesystem its scratch is on.
It takes the first file there, `cache-0`, `cache-1` and so on, that no other
process holds locked, and holds the lock while it runs. So hosts on one node
never share a file, and a host that replaces one that exited reads back what it
kept. The spill files stay in the pod's own scratch
([the cache's file](hosting.md#the-caches-file)).

**Reads.** A read checks the key, index and code in each item's header against
what it asked for, then the checksum. An item that fails is a miss, and the
index forgets it. The read rebuilds the envelope from the items that pass and
checks its XXH3-128. If it holds more than k stripes and the first k fail, it
tries other sets of k and forgets the stripe that does not belong. If none
pass, it forgets them all. The page is then read from the store, or, inside the
share, from the window's other ranks. So damage, a torn write, a wrong stripe or
a wrong index costs a request, never wrong bytes. A stripe that a peer's read
finds wrong is forgotten when the peer says so (`Cache.Drop`).

**Serving peers.** A peer's read of a window is answered with every item the
disk holds of the pages it names, under the read's code, of any index, as they
lie on the disk, header and checksum included, up to what the read may hold.
Nothing is decoded or checked on the way out; the reader checks each item. An
item served counts as a read of it for the second chance.

**The index.** The in-memory index is kept per **window**: the pages of one
volume, in one aligned 2 MiB span, that one checkpoint published. A segment is
its own window. A window is keyed by an 8-byte hash of its identity, and
the key check on every read catches two windows that share a hash. An entry
holds the region, the window's first offset, the code, which indices each page
holds, and which pages are present with each item's length and read counter, in
4 bytes an item. An entry with few items lists them instead. A host holds
several indices of a window only where the membership has fewer disks than the
code is wide; their items lie next to each other in index order, so the window
still costs one entry. A window written at two different times has an entry for
each run of its items. The index counts its own memory. Past
`CacheConfig.DiskIndexBytes`, 64 MiB by default, the disk refuses writes.

**The share.** The disk may hold as many whole regions as its share allows. A
host gives the cache a `DiskBudget`, its disk limiter, which alone sets the
share; a `CacheConfig.DiskBytes` beside it is refused. Without a budget, as in
a test, the share is `CacheConfig.DiskBytes`. The budget admits or refuses each
write by kind: repairs first, then second chances, then fills from reads, then
fills from publications. Fills may use every region of the share but one, which
is kept for the second chance.

**Eviction.** Nothing about a VM evicts anything. When a fill needs a region and
the share has none left, the oldest closed region is the victim. Before it is
given back, its items read at least once since they were written
(`CacheConfig.DiskSecondChanceReads`) are written again into the open region,
in order. This **second chance** writes at most half a region, and nothing when
the cache is over its share or the budget refuses it. If it would need more
than the free region, it stops and the rest of the victim goes, so every
eviction gives space back. A region given back is punched out of the file. An
evicted region leaves the index at once, and its space is given back when its
last reader has finished. When the share falls, the host calls `Cache.FitDisk`,
and the disk gives regions back, oldest first and with no second chance, until
it holds one region less than its share.

`Store.Pull` refuses, before fetching anything, a checkpoint larger than the
share's fill regions can hold, while some of its windows would be kept whole;
with the cluster cache on for every window, none are. The size is in the root:
the bytes each segment entry records plus the segments' lengths. A page the disk
already holds is not copied again. A pull holds nothing: its pages are ordinary
items and leave when their region is evicted.

A pull runs behind every fault. It fetches the segments one at a time, and the
members of each in extents. Inside the share it reads each segment through the
cluster, asks the ranks of the pages' windows which stripes they hold
(`peer.Presence`, which answers a bitmap of pages for each index of the code),
and leaves out every page of which the ranks hold k distinct indices. It hands
what it read to the fills. Its reads are marked as a prefetch
(`checkpoint.WithPrefetch`): they take none of the cache's load slots and join
none of its flights, so a fault for a page the pull has not reached fetches it
at once. Before each request the pull waits until no load of the cache is in
flight, and all the pulls on a host share two requests. Memory pressure
(`resource.Budget.Pressure`) and `Cache.FitDisk` cancel every pull's requests in
flight, and the pull ends with `ErrPressure`.

### The hot tier

A store may read through a hot tier instead (`Config.HotTier`): a second bucket
with copies of checkpoint objects under the same names. A store refuses to have
both a hot tier and the cluster cache. Every read of a checkpoint object (the
tail of an index object and the rest of its root, a segment, a member, an extent
of a part) runs against the hot tier first and, on a miss or any failure, against
the regional bucket, so a hot tier never fails a read. A miss is filled behind
the read with a create-if-absent PUT of the whole object, and a publication
writes each part and its index object once the regional PUT has succeeded. The
deployment's check and a part's table read the regional bucket alone.
Reclamation deletes from the regional bucket alone, so what it deletes stays in
the hot tier until something expires it
([hosting](hosting.md#reading-through-a-hot-tier)).

## Captures and forks

A capture pauses the guest, saves VMM state, seals memory, and resumes the
guest. It takes a checkpoint of every volume at one write generation with the
VMM state attached, starts publication in the background, and returns once the
local checkpoint exists ([managed VM memory](vm-memory.md)). The capture's
reference is known before its objects upload, so its pages have their identity
at once. Waiting on the capture reports when publication became durable.

### The fork point

A fork needs only a checkpoint's pause (stop the vCPUs, save the VMM state,
seal the dirty set, resume), not its upload, as a [migration](migration.md)
does. It takes the pause from a parent that keeps running.

`VM.ForkPoint` runs the caller's pause under the publication lock and returns a
`ForkPoint`, which holds:

- the checkpoint the parent's control record selects, pinned in that record
  before the point is returned;
- the pages no checkpoint of the parent holds: everything dirty since that
  checkpoint, now sealed.

Nothing is published. The parent keeps its handle, its volumes and its pages. A
pause may seal nothing, as for an imported template, which has no guest; such a
point is only the published checkpoint and the pin.

The pin is a conditional write from the parent's own epoch, so a fork of a
fenced parent is refused, and reclamation can never delete what the child
inherits. There is one pin per point, taken when the point is made and again,
idempotently, by every `Fork` from it. No operation releases it
([metadata](metadata.md#the-control-record)). A parent forked at many distinct
checkpoints uses one of `MaximumPins` (4096) for each. A fork that fails after
the pin leaves the pin, which costs only that it was taken early.

A parent whose pages a fork point holds is `Status.Sealed`. A memory region can
have only one seal outstanding, so the parent cannot be captured or forked
again; a capture request is refused before its guest is touched. The seal ends
when the last child of the point has retired it. A child retires it when every
page it inherited is published by the child or fetched by it.

The parent publishes the point once, behind the fork (`publishPoint`), as a
checkpoint under the sequence the point took, and pins it. A child on
the parent's host waits for that publication and builds its first checkpoint on
the point, so it uploads only what it wrote; a fan-out of N children uploads the
parent's unpublished pages once. When the last hold retires, the sealed pages
become clean under the point's identity, so no child reads them back. A child
on another host fetched those pages into its own pager and publishes them as
its own. If the point's publication fails, its children publish what they
inherited, and the parent's next checkpoint publishes those pages again.

### The child

`Manager.Fork` takes the point's pin again and creates the child's control
record, which selects a first checkpoint over the parent's that does not exist
yet. The record names no parent, because nothing releases a pin. Each child is
one hold on the point, so a fan-out costs the parent one pause.

On the parent's host, the child reads the pages written since that checkpoint
through the point, with no copy, and siblings that fault a page share it,
because every child of one point gives those pages the same identity. On
another host, `Manager.Inherit` rebuilds the point from the pinned checkpoint,
and the child's pager pulls those pages from the parent's peer server,
post-copy.

`Manager.InheritPublished` builds the same point over a published checkpoint of
a VM that nothing need run, such as a stopped VM. With no writer to pin with, it
pins the checkpoint without the epoch, which may name only the published
checkpoint the record selects, a kept one, or one a pin already holds
([metadata](metadata.md#the-control-record)). A child of such a point resumes
from the checkpoint's VMM state when it has one
([hosting](hosting.md#creating-a-vm-from-a-checkpoint)).

`Manager.Release` gives up a kept checkpoint that no fork was taken from and
sweeps what only it held.

The child's first checkpoint is its own root, and publishes the pages it
inherited as its own. Only then can any host open the child. Before then,
opening it reports `ErrForkPending`, and a host loss loses it. A fork that ends
before its first checkpoint never touches the store; `Close` publishes no root,
so it leaves only the control record it was given.

A child need not run. `Host.CaptureInto` creates a child on the parent's host,
publishes its root straight from the fork point with the VMM state the point
saved, and closes it ([hosting](hosting.md#capturing-a-vm-into-a-new-vm)).

Creating or starting a fork never loads a full disk or memory image. A fork's
reads share its parent's objects and, within one pager, its parent's resident
pages.

## Deletion

The caller closes the writer first. Then:

1. Read the record.
2. Remove the record, on the condition that it is still the version read. A
   record that moved is read again, so a pin added without the writer is
   spared. Nothing can open the VM after this.
3. Delete every object under the VM's checkpoint prefix that no pin covers,
   each checkpoint's index object first. A kept checkpoint no fork was taken
   from goes with the rest.

The rules for a missing or unparseable record, pinned checkpoints, and reuse of
the identity are in [conditional publication](metadata.md#conditional-publication).
A VM that left pinned checkpoints behind leaves its name refused until a
collector frees them. A collector is left with:

- the pinned checkpoints of deleted VMs;
- every pin that nothing reads through any more, or ever did;
- what a crash leaves: the objects of a writer that died mid-checkpoint or
  published after being fenced, and of a delete interrupted between the
  record's removal and the sweep.

See [the architecture](architecture.md#identities-and-reclamation).

## Billing

An embedder bills each page to the VM that published it.
`volume.StoredBytes(ctx, store, prefix, tenant)` reports what one tenant's VMs
hold, per VM: the control record and every object under `vm/<id>/`. The empty
tenant reports the VMs of no tenant. The host API serves the same report at
`GET /stored?tenant=<tenant>`.

The number is what the store lists, not a count kept beside it. It includes
what a crash, a fence or an interrupted sweep left, and excludes what
reclamation deleted.

- A fork reads its parent's pages where the parent published them, so those
  bytes stay the parent's. A pin keeps them after the parent is deleted, so a
  deleted VM that was forked stays in the report, with no record, until a
  collector frees what it pinned.
- Compaction rewrites a page only into a later checkpoint of the VM that
  published it, so the bill moves only within that VM. For one checkpoint the
  VM pays for both copies.
- Reclamation reduces only the VM's own bill, by the bytes it deleted.

The report costs one listing of the tenant's control records and one of its
checkpoint objects: a LIST request per thousand keys, and no GET or HEAD. A VM
has one record, and a checkpoint has its index object and one part per 64 MiB.
So a tenant of a thousand VMs with ten checkpoints each costs about twenty-one
requests. The report is for a billing run, not for polling, so it is not part
of `/status`.

The [deployment check](testing.md#the-deployment-check) requires each tenant's
report to match what a listing of the whole deployment holds under each VM, and
every part to hold only members its own VM published.

## VM integration

The [Firecracker integration](vm-memory.md) maps `ram0` and each PMEM volume
through the host pager. A guest PMEM flush reaches the pager and completes when
the host says the VM's disks are fresh enough. Guest PMEM and RAM stores remain
private pager state, resident or in scratch spill, until a checkpoint publishes
them. Scratch spill is not durable.
