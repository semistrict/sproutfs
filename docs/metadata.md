# Metadata authority

Object storage is the durable authority for every VM's ownership and checkpoint
selection. There is no consensus service and no local metadata disk. The
orchestrator's SQLite table records where each VM is running, but it is not the
authority for anything. It is rebuilt by surveying hosts and listing the bucket.
Hosts share one object store and one deployment prefix. Under that prefix, a
VM's control record is at `control/<id>` and its checkpoint objects are under
`vm/<id>/ckpt/`, or under `tenants/<tenant>/` for a VM of a tenant. The records have a separate namespace because listing them is
how the deployment's VMs are found. If that listing also had to walk the
checkpoint objects, it would cost a request for every object ever written.

## The control record

A VM's control record is the only mutable object the VM owns. It contains:

- the **format version**;
- the **VM identity**, which must match the identity the record was read under;
- the **epoch**, the writer token that every open advances;
- the **writer nonce**, random bytes the writer chose when it took that epoch.
  A writer whose conditional write lost its reply uses the nonce to recognise
  its own work;
- the **selected checkpoint**, a sequence number. The record treats it as
  opaque, and the [checkpoint layout](volumes.md#objects) turns it into object
  keys;
- the **pins**, the checkpoints of this VM that have been forked, in ascending
  sequence order;
- the **kept checkpoints**, the checkpoints of this VM that a checkpoint
  request kept, in ascending sequence order. Each one has its sequence, the
  time it was selected, and whether it holds VMM state;
- the **created flag**, which says that the selected checkpoint has been
  published. This means its index object, which holds its root, exists.

This is format 5. Format 4 had no kept checkpoints. Format 3 marked a
tombstone. Format 2 named each pin's holders, and the parent checkpoint that a
record held a pin on. Format 1 stored pins as bare sequences. None of these
formats parses.

A pin is permanent. It records that a fork was taken at that checkpoint. It does
not record that a fork still reads the checkpoint. Nothing in a deployment
releases a pin, because no participant can tell whether the pin is still
needed. A grandchild's root names its grandparent's checkpoints directly.
Neither the grandparent's record nor the child's record shows this. A release
based on one descendant's view is a guess. A wrong guess deletes a checkpoint
that another descendant still reads, without that descendant being consulted.
So releasing a pin is a job for a collector, which can survey every record and
every root in the deployment. See TASK-24 in the [backlog](../backlog/tasks).

One pin covers every child of one fork point, and repeating a pin writes
nothing. So a fan-out of any size costs one pin. But a VM forked at many
distinct checkpoints accumulates one pin per checkpoint, permanently.
`MaximumPins` (4096) is the limit on that count until a collector releases some.

The holder of the record's epoch writes everything in the record. A pin is one
exception, and the release of a kept checkpoint is the other (see
[kept checkpoints](#kept-checkpoints)). A VM that nobody runs has no writer,
and it can still be forked: a create can start from its published checkpoint
(see [hosting](hosting.md#creating-a-vm-from-a-checkpoint)). Taking the epoch to pin
would fence a host that turns out to run the VM after all. So `Client.Pin` adds
a pin without the epoch. It reads the record and writes it back with the pin
added, under `IfMatch` against the version it read. It keeps the epoch and the
nonce. A record that moved in between is read again.

Such a pin may name only three kinds of checkpoint:

- the published checkpoint the record selects. A writer's sweep never deletes
  the checkpoint its own selection selected, nor anything that checkpoint's
  index names. A selection that lands between the read and the write moves the
  record, so the pin reads again and names the new selection.
- a kept checkpoint. Every sweep that could reach it read a record that keeps
  it, because the keep was written with its selection. A release is
  conditional on the record, as the pin is, so the two cannot both land.
- a checkpoint that a pin already keeps. That costs no write.

Any other checkpoint may be in the middle of a sweep that read the record
before this pin landed. So it is refused, not pinned too late.

If a writer does hold the epoch, the pin moves the record under it. Its next
write is refused. It reads the record back, finds its own epoch and nonce, and
adopts that record with the pin. It then makes its change again over it. So its
next selection reports the pin, and its reclamation spares the pinned
checkpoint from then on. A release moves the record in the same way, and the
writer adopts it in the same way. Only a later open that takes the VM over
fences a writer.

The record allocates and selects sequences, so the names built on sequences are
defined with it:

- A checkpoint reference is the (VM, sequence) pair.
- A [page identity](context.md) names a page as (reference, volume, page).
- An extent is the run of volume bytes that reads from one such page.

The pager and the migration wire name pages by these names. They do not depend
on the store that holds the pages.

Checkpoint objects are immutable and written create-if-absent. A checkpoint
consists of:

- one index object at `vm/<id>/ckpt/<seq>/index`. It holds a fixed header, the
  page-table segments the checkpoint changed, and the root over every volume's
  segments;
- its parts at `vm/<id>/ckpt/<seq>/part/<n>`. They hold the VMM state and the
  pages.

The index format is 8 and the part layout is 4. The older versions are:

- Format 7 stated no volume's page size, so its page numbers are always 2 MiB
  pages.
- Formats 5 and 6 of the index object held only the root. In format 5, the
  root held the whole page table.
- Part layouts 1 to 3 stored the root as the last member of the last part.

None of these versions is readable here. A store written under any of them is
refused, and the refusal names the version it carries. The store is not
migrated. An older control record is handled the same way.

The writer bounds a part's tail at 1 MiB. The tail is the table that names the
part's members, plus the 32-byte trailer that names the table. So reading any
part's table is one suffix range of the object, not a HEAD and two reads.
Opening a checkpoint is one GET of its index object. A publication uploads the
parts, waits for them, and writes the index object, which is its commit. The
control record then selects the checkpoint.

A newly created fork's record selects a checkpoint that the fork's first
publication has not written yet, so its created flag is false. Opening that VM
on any host reports that the fork is still pending. The epoch is left unchanged,
so an open that could never succeed does not fence the host that holds the
fork.

### Kept checkpoints

Reclamation deletes a VM's older checkpoints as soon as a newer one replaces
them. A checkpoint request can ask to keep its checkpoint instead
(`Handle.SelectKept`). The write that selects the checkpoint also records it as
kept, so no sweep can see it selected and replaced without seeing it kept.
Reclamation and compaction then spare it, and every checkpoint its root names,
as they spare a pinned one. Only kept checkpoints cost storage beyond what the
selected checkpoint reads.

Keeping is not forking. A kept checkpoint records that someone may fork it
later. A pin records that someone did. So the two are separate lists, and the
record can tell a kept checkpoint from a forked one. A fork of a kept
checkpoint pins it, and the checkpoint is then in both lists.

`Client.Release` gives a kept checkpoint up. It is refused with `ErrForked` for
a checkpoint that a pin also holds, because the pin is permanent. It is refused
with `ErrNotKept` for a checkpoint the record does not keep. A release needs no
epoch, like a pin without the writer. It keeps the epoch and the nonce, and it
is conditional on the record as it was read. So a pin and a release of one
checkpoint are ordered by the record: whichever lands first makes the other
read again and be refused. `MaximumKept` (4096) bounds the list.

The release is followed by a sweep of what only the released checkpoint held.
It is reclamation with the released checkpoint in place of the replaced one.
The candidates are the released checkpoint and the checkpoints of this VM that
its root names. The sweep spares everything the selected root names and
everything that stays pinned or kept. Nothing newer than the selected
checkpoint names anything the selected root does not, so a writer that keeps
publishing loses nothing to the sweep. A released checkpoint that is still
selected stays until a later selection replaces it.

Deleting a VM spares only its pins. A kept checkpoint that no fork was taken
from goes with the VM.

## Conditional publication

A new record is written with `IfNoneMatch`. An update reads the object with its
opaque version tag, and writes it back with `IfMatch` against that version. A
failed condition on an update means that a later writer has taken the VM over.
A failed condition on a create means that the VM already exists, with one
exception. If the existing record carries this attempt's own nonce, the failure
is a retry inside the store's client, refused by the object that its first
attempt wrote.

The create reads the record back before it answers. It does this because a
caller told that its VM exists would lose the VM it owns. For a fork's child,
the child's own first checkpoint publishes the root. If the create reported
that the child exists, nothing could then create or open the child. For the same
reason, a fork that cannot build its child's handle after writing that record
deletes the record again.

Deletion is a separate operation. It takes no epoch, so the caller must close
the writer first. The delete reads the record for its pins and then removes it
with `IfMatch` against the version it read. A record that moved is read again.
So a pin added without the writer while the delete runs is one its sweep
spares. Removing the control record prevents later opens. The VM's
checkpoint objects are then deleted too, each checkpoint's index object first.
Deleting them frees the identity. Checkpoint objects are named by the VM and a
sequence. A VM created under a deleted VM's identity would be refused if it
found objects there. A create does refuse in that way when the sweep left pinned
checkpoints behind. The record is removed first, so nothing can open the VM
while its objects are being deleted.

The record is also the only thing that says which of the VM's objects a sweep
may take. So a VM that has no record is not swept. This case is not only an
interrupted delete. A finished delete of a VM that was ever forked leaves the
same state: no record, and objects that a fork still reads. A repeated delete
that swept what it found would destroy those objects. So repeating a delete is
harmless and finishes nothing. What an interrupted sweep left belongs to a
collector.

The sweep leaves the checkpoints that the record pinned, and every checkpoint
that their roots name. These are the fork points the VM was taken at. A
descendant of the VM may still read through them. That descendant may be a
child, or a grandchild whose own root names those checkpoints. Nothing this
delete can read says whether such a descendant exists. So a VM that was ever
forked frees its identity and leaves those objects for a collector. A record
that cannot be parsed is not deleted. Its pins are what the sweep would have to
spare, and an identity that nobody can delete is the smaller loss.

A missing response does not prove that a write failed. A writer reconciles by
reading the record back. If it finds the record it meant to write, the write
landed. If it finds the record it already had, the write did not land. Only the
writer of an epoch, and a pin added without the epoch, produce a record that
carries that epoch's nonce.

One case remains. The writer may find a record with its own epoch and nonce that
is neither of those two records. This happens in two ways. A reply is lost and
the read-back also fails, so the writer tracks a version that the store has
moved past. Or a pin or a release was written without the epoch. Either way the store refuses
the writer's next write against its stale version. That refusal is not a
takeover. The writer adopts the record it finds and makes its change again over
it. A change that had already landed then writes nothing. Only a foreign epoch
or nonce fences the writer. A refusal whose record cannot be read fences
nothing either, because a pin or a release refuses a write as a takeover does. The writer's
next write reads what happened. This behaviour lets each of these be finished
by repeating it: an interrupted creation, an interrupted takeover, and an
interrupted checkpoint selection.

## Fencing and selection

Every open conditionally advances the epoch. This fences the writer that held
the previous epoch. Its next control-record write fails, and it reports that the
VM must be reopened. Two racing opens take distinct epochs, so they never share
a writer token.

Checkpoint sequences are epoch-major. The epoch is in the high 32 bits, and a
counter starting at one is in the low 32 bits. So every sequence a writer
allocates is higher than every sequence that any earlier epoch could have
allocated. Every checkpoint object is written create-if-absent. So a fenced
writer that is still uploading objects cannot collide with its successor.

A VM's starting epoch is drawn at random, not fixed. A creating handle takes a
random epoch in [1, 2³¹), from the same entropy source as its writer nonce.
Later opens count up from it. This leaves at least 2³¹ takeovers before
`ErrEpochExhausted`. The random start means that two VMs created under one
identity allocate different sequences. They therefore get different page
identities and different object keys. Two VMs share an identity when a name is
reused after a delete. The orchestrator does not reuse names, but nothing here
can enforce that. A VM must never be served the bytes or the keys of a VM that
previously held its name.

A create is refused when the identity's `vm/<id>/` prefix holds any object and
no control record accounts for it. Those objects are either the pinned
checkpoints that a deleted VM left, or the objects of a create interrupted
before it wrote its record. A VM published there would write into keys that do
not belong to it. Repeating a create whose record was written opens the VM
instead. This is how an interrupted create is finished.

Selection is a conditional write from the handle's own epoch. The sequence must
belong to that epoch and must not go backwards. Reselecting the sequence that is
already selected and already created writes nothing. This makes a repeated
publication safe. An older epoch is refused.

This refusal keeps two writers of one VM from ever mixing their writes. But it
happens only when a fenced writer next tries to publish. That may be a whole
checkpoint interval later. It never happens while a fork point holds the VM
sealed. So a handle can also be checked directly. `VM.Confirm` re-reads the
control record. If the record's epoch is no longer the handle's epoch, it fails
the handle in the same way that a refused selection does. A host calls it on a
timer for every VM it holds. This is how a takeover reaches a writer that is
publishing nothing. A record that cannot be read fails nothing, because only a
changed epoch is evidence of a takeover.

A handoff also checks, and it does so itself instead of relying on that timer.
The pages a handoff gives to another host are the one thing the store cannot
refuse afterwards. So `Migrate` and the fork point confirm the record before the
pause, and a failed read refuses the handoff.

Taking a VM over fences a writer that may still be running a guest. So an epoch
is taken only on positive evidence that the previous holder is gone. The
orchestrator's recovery refuses while any host pod that it cannot account for
might still be running the VM. The same reasoning applies in the other
direction. If two hosts both report one VM, the deployment disagrees with
itself, and one of those hosts holds a handle that can never publish. The
orchestrator then refuses every request that must name the VM's host until the
disagreement is resolved. It does not pick one of the two hosts. Its table does
not break such a tie, because the table records what the orchestrator last did,
not which host holds the epoch.

## Latency boundary

A volume write applies to an in-memory overlay and returns without contacting
object storage. Creation, opening, checkpoint publication and selection,
deletion, and cold page reads pay object-storage latency. Nothing on the
guest's write path does. A guest flush is not a durability point. Checkpoints
run on the interval and on request. The only thing a volume can verify
synchronously is that this handle still owns its VM. Checkpoints do not need to
be available during an outage of the authority store. A failed publication
leaves the previous checkpoint selected and the overlay intact, and the next
checkpoint retries.

Background uploads do not hold the VM lock during object-store I/O. Capture and
installation still synchronize local state briefly. Each handle has at most one
control-record write in flight at a time. A caller waiting for its turn waits
under its own context.
