# Metadata authority

Object storage is the durable authority for every VM's ownership and checkpoint
selection. There is no consensus service and no local metadata disk. The
orchestrator's SQLite table records where each VM runs but is the authority for
nothing. It is rebuilt by surveying hosts and listing the bucket. Hosts share
one object store and one deployment prefix. Under that prefix, a VM's control
record is at `control/<id>` and its checkpoint objects are under
`vm/<id>/ckpt/`, or under `tenants/<tenant>/` for a VM of a tenant. The records
have their own namespace so that listing the deployment's VMs does not cost a
request per checkpoint object ever written.

TLC model-checks this protocol, with lost replies, against the invariants in
[Model checking](testing.md#model-checking).

## The control record

A VM's control record is the only mutable object the VM owns. It contains:

- the **format version**;
- the **VM identity**, which must match the identity the record was read under;
- the **epoch**, the writer token that every open advances;
- the **writer nonce**, random bytes the writer chose when it took that epoch,
  by which a writer whose conditional write lost its reply recognises its own
  work;
- the **selected checkpoint**, a sequence number, opaque to the record; the
  [checkpoint layout](volumes.md#objects) turns it into object keys;
- the **pins**, the checkpoints of this VM that have been forked, in ascending
  order;
- the **kept checkpoints**, the checkpoints a checkpoint request kept, in
  ascending order, each with its sequence, the time it was selected, and
  whether it holds VMM state;
- the **created flag**, which says that the selected checkpoint's index object
  exists.

This is format 5. Format 4 had no kept checkpoints, format 3 marked a
tombstone, format 2 named each pin's holders and the parent checkpoint a record
pinned, and format 1 stored pins as bare sequences. None of them parses.

A pin is permanent. It records that a fork was taken at that checkpoint, not
that a fork still reads it. A grandchild's root names its grandparent's
checkpoints directly, and neither the grandparent's record nor the child's
shows this, so no participant can tell whether a pin is still needed. A release
based on one descendant's view could delete a checkpoint another descendant
reads. Releasing a pin is a job for a collector that can survey every record
and root (TASK-24 in the [backlog](../backlog/tasks)).

One pin covers every child of one fork point, and repeating a pin writes
nothing. A VM forked at many distinct checkpoints holds one pin per checkpoint.
`MaximumPins` (4096) bounds the count.

The holder of the record's epoch writes everything in the record, with two
exceptions: a pin, and the release of a [kept checkpoint](#kept-checkpoints). A
VM that nobody runs can still be forked: a create can start from its published
checkpoint ([hosting](hosting.md#creating-a-vm-from-a-checkpoint)). Taking the
epoch to pin would fence a host that turns out to run the VM. So `Client.Pin`
reads the record and writes it back with the pin added, under `IfMatch` against
the version it read, keeping the epoch and the nonce. A record that moved is
read again.

Such a pin may name only three kinds of checkpoint:

- the published checkpoint the record selects. A writer's sweep never deletes
  its own selection or anything that checkpoint's index names. A selection that
  lands between the read and the write moves the record, so the pin reads again
  and names the new selection.
- a kept checkpoint. Every sweep that could reach it read a record that keeps
  it, because the keep was written with its selection. A release is conditional
  on the record, as the pin is, so the two cannot both land.
- a checkpoint a pin already keeps. That costs no write.

Any other checkpoint may be in a sweep that read the record before this pin
landed, so it is refused.

If a writer holds the epoch, the pin moves the record under it. The writer's
next write is refused. It reads the record back, finds its own epoch and nonce,
adopts the record with the pin, and makes its change again. Its next selection
reports the pin, and its reclamation spares the pinned checkpoint. A release
moves the record and is adopted the same way. Only a later open fences a
writer.

The record allocates and selects sequences, so the names built on them are
defined here:

- A checkpoint reference is the (VM, sequence) pair.
- A [page identity](context.md) names a page as (reference, volume, page).
- An extent is the run of volume bytes that reads from one such page.

The pager and the migration wire name pages by these names, independent of the
store that holds them.

Checkpoint objects are immutable and written create-if-absent: one index object
at `vm/<id>/ckpt/<seq>/index` and the parts at `vm/<id>/ckpt/<seq>/part/<n>`.
Their layout and format versions are in [volumes](volumes.md#objects). A store
written under an older version is refused with the version named, and is not
migrated. An older control record is refused the same way. Opening a checkpoint
is one GET of its index object. A publication uploads the parts, waits for
them, writes the index object as its commit, and then selects the checkpoint in
the control record.

A new fork's record selects a checkpoint that the fork's first publication has
not written, so its created flag is false. Opening it on any host reports that
the fork is pending and leaves the epoch unchanged, so the open does not fence
the host that holds the fork.

### Kept checkpoints

Reclamation deletes a VM's older checkpoints as soon as a newer one replaces
them. A checkpoint request can keep its checkpoint instead
(`Handle.SelectKept`). The write that selects the checkpoint also records it as
kept, so no sweep can see it selected and replaced without seeing it kept.
Reclamation and compaction then spare it and every checkpoint its root names,
as for a pin. Only kept checkpoints cost storage beyond what the selected
checkpoint reads.

A kept checkpoint records that someone may fork it later; a pin records that
someone did. The two are separate lists. A fork of a kept checkpoint pins it,
and it is then in both.

`Client.Release` gives a kept checkpoint up. It is refused with `ErrForked` if
a pin also holds the checkpoint, and with `ErrNotKept` if the record does not
keep it. A release needs no epoch. It keeps the epoch and the nonce and is
conditional on the record it read, so a pin and a release of one checkpoint
are ordered by the record: whichever lands second reads again and is refused.
`MaximumKept` (4096) bounds the list.

The release is followed by a sweep of what only the released checkpoint held:
reclamation with the released checkpoint in place of the replaced one. The
candidates are the released checkpoint and the checkpoints of this VM its root
names. The sweep spares everything the selected root names and everything still
pinned or kept. Nothing newer than the selected checkpoint names anything the
selected root does not, so a writer that keeps publishing loses nothing to it.
A released checkpoint that is still selected stays until a later selection
replaces it.

Deleting a VM spares only its pins. A kept checkpoint no fork was taken from
goes with the VM.

## Conditional publication

A new record is written with `IfNoneMatch`. An update reads the object with its
version tag and writes it back with `IfMatch` against that version. A failed
condition on an update means a later writer has taken the VM over. A failed
condition on a create means the VM exists, unless the existing record carries
this attempt's own nonce: then it is a retry inside the store's client, refused
by the object its first attempt wrote.

The create reads the record back before it answers, because a caller told that
its VM exists would lose the VM it owns. A fork's child publishes its root with
its own first checkpoint; if the create reported that the child exists, nothing
could create or open it. For the same reason, a fork that cannot build its
child's handle after writing the record deletes the record.

Deletion takes no epoch, so the caller must close the writer first. The delete
reads the record for its pins and removes it with `IfMatch` against the version
it read. A record that moved is read again, so a pin added without the writer
during the delete is spared. Removing the record prevents later opens. The
VM's checkpoint objects are then deleted, each checkpoint's index object first.
A create under the same identity is refused while objects remain there, which
happens when the sweep left pinned checkpoints behind.

The record is the only thing that says which of the VM's objects a sweep may
take, so a VM that has no record is not swept. A finished delete of a VM that
was ever forked leaves the same state as an interrupted one: no record, and
objects a fork still reads. So repeating a delete is harmless and finishes
nothing. What an interrupted sweep left belongs to a collector.

The sweep leaves the checkpoints the record pinned and every checkpoint their
roots name. A child or a grandchild may still read through them, and nothing
the delete can read says whether one exists. So a VM that was ever forked frees
its identity and leaves those objects for a collector. A record that cannot be
parsed is not deleted, because its pins are what the sweep would have to spare.

A missing response does not prove a write failed. A writer reconciles by
reading the record back. If it finds the record it meant to write, the write
landed. If it finds the record it already had, it did not. Only the writer of
an epoch, and a pin or release made without the epoch, produce a record that
carries that epoch's nonce.

The writer may also find a record with its own epoch and nonce that is neither
of those. This happens when a reply and the read-back both failed, so the
writer tracks a version the store has moved past, or when a pin or release was
written without the epoch. The store refuses the writer's next write against
its stale version. That refusal is not a takeover. The writer adopts the record
it finds and makes its change again, and a change that already landed writes
nothing. Only a foreign epoch or nonce fences the writer. A refusal whose
record cannot be read fences nothing, because a pin or release refuses a write
as a takeover does. So an interrupted creation, takeover or selection is
finished by repeating it.

## Fencing and selection

Every open conditionally advances the epoch. This fences the writer that held
the previous epoch: its next control-record write fails, and it reports that
the VM must be reopened. Two racing opens take distinct epochs.

Checkpoint sequences are epoch-major: the epoch in the high 32 bits, a counter
from one in the low 32 bits. Every sequence a writer allocates is higher than
any an earlier epoch could allocate, and every checkpoint object is written
create-if-absent, so a fenced writer still uploading cannot collide with its
successor.

A creating handle draws its starting epoch at random from [1, 2³¹), from the
same entropy source as its writer nonce. Later opens count up from it, which
leaves at least 2³¹ takeovers before `ErrEpochExhausted`. Two VMs created under
one identity, as when a name is reused after a delete, therefore allocate
different sequences, page identities and object keys. The orchestrator does not
reuse names, but nothing here can enforce that, and a VM must never be served
the bytes or keys of a VM that held its name before.

A create is refused when `vm/<id>/` holds any object that no control record
accounts for: the pinned checkpoints a deleted VM left, or the objects of a
create interrupted before it wrote its record. Repeating a create whose record
was written opens the VM instead, which finishes an interrupted create.

Selection is a conditional write from the handle's own epoch. The sequence must
belong to that epoch and must not go backwards. Reselecting the sequence that
is already selected and created writes nothing, so a repeated publication is
safe. An older epoch is refused.

That refusal comes only when a fenced writer next publishes, which may be a
whole interval later, and never while a fork point holds the VM sealed. So
`VM.Confirm` re-reads the control record and fails the handle, as a refused
selection does, if the epoch is no longer the handle's. A host calls it on a
timer for every VM it holds. A record that cannot be read fails nothing,
because only a changed epoch is evidence of a takeover.

A handoff also confirms the record itself before the pause, because the pages
it gives another host are the one thing the store cannot refuse afterwards.
`Migrate` and the fork point refuse the handoff if the read fails.

An epoch is taken only on positive evidence that the previous holder is gone.
The orchestrator's recovery refuses while any host pod it cannot account for
might still run the VM.

A survey asks each host at its own moment, so it can miss a host that opened
the VM meanwhile: a migration can land between asking its destination and
asking its source. So a recovery or a start reads the VM's epoch before it
surveys, and the open takes the next epoch only from that one
(`Client.OpenAfter`). If any open moved the epoch since, the host refuses the
reopen with `ErrMoved` and fences nothing. `spec/recovery` found this; see
[spec/bugs.md](../spec/bugs.md).

If two hosts both report one VM, one of them holds a handle that can never
publish. The orchestrator then refuses every request that must name the VM's
host until the disagreement is resolved. Its table cannot break the tie,
because it records what the orchestrator last did, not which host holds the
epoch.

## Latency boundary

A volume write applies to an in-memory overlay and returns without contacting
object storage. Creation, opening, checkpoint publication and selection,
deletion, and cold page reads pay object-storage latency. Nothing on the
guest's write path does. A guest flush is durable within the flush bound plus one interval
([architecture](architecture.md#loss-model)). Checkpoints
run on the interval and on request. The only thing a volume can verify
synchronously is that this handle still owns its VM. Checkpoints need not be
available during an outage of the authority store. A failed publication leaves
the previous checkpoint selected and the overlay intact, and the next
checkpoint retries.

Background uploads do not hold the VM lock during object-store I/O. Capture and
installation still synchronize local state briefly. Each handle has at most one
control-record write in flight. A caller waiting for its turn waits under its
own context.
