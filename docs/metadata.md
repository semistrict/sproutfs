# Metadata authority

Object storage is the durable authority for every VM's ownership and checkpoint
selection. There is no consensus service and no local metadata disk; the
orchestrator's SQLite table records where a VM is running, but it is authority
for nothing and is rebuilt by surveying hosts and listing the bucket. Hosts
share one object store and one deployment prefix, under which a VM's control
record lives at `control/<id>` and its checkpoint objects under
`vm/<id>/ckpt/`. The records have a namespace of their own because listing them
is how the deployment's VMs are found, and a listing that had to walk the
checkpoint objects too would cost a request for every object anyone ever wrote.

## The control record

A VM's control record is the only mutable object it owns. It carries:

- the **format version**;
- the **VM identity**, which must be the one the record was read under;
- the **epoch**, the writer token every open advances;
- the **writer nonce**, random bytes the writer chose when it took that
  epoch, so a writer whose conditional write lost its reply can recognise its
  own work;
- the **selected checkpoint**, a sequence number the record treats as opaque and
  the [checkpoint layout](volumes.md#objects) turns into object keys;
- the **pins**, the checkpoints of this VM that have been forked, in ascending
  order of sequence;
- the **created flag**, which says the selected checkpoint has been published:
  its index object, which holds its root, is there.

This is format 4. Format 3 marked a tombstone, format 2 named each pin's
holders and the parent checkpoint a record held a pin on, and format 1's pins
were bare sequences; none of them parses.

A pin is permanent. It says a fork was taken at that checkpoint, not that one
is still reading it, and nothing in a deployment gives one back. The reason is
that no participant can tell: a grandchild's root names its grandparent's
checkpoints directly, and neither the grandparent's record nor the child's says
that it does. A release reasoned from one descendant's point of view is a guess,
and a wrong one takes a checkpoint out from under a descendant nobody asked.
Releasing a pin is therefore a collector's, which can
survey every record and every root in the deployment; see
[open work](open-work.md).

Because one pin covers every child of one fork point and a repeated pin writes
nothing, a fan-out of any size costs one — but a VM forked at many distinct
checkpoints accumulates one pin each, for good, and `MaximumPins` (4096) is
what that may reach before a collector has released some.

Pins are written by the record's own epoch holder, like everything else in the
record. The record therefore has exactly one writer at a time, and a write
settles in a single attempt: nothing moves underneath it but a later open
taking the VM over, which is a fence.

Because the record is what allocates and selects sequences, the names built on
them live with it: a checkpoint reference is the (VM, sequence) pair, a
[page identity](context.md) names a page as (reference, volume, page) and an
extent is the run of volume bytes that reads from one such page. The pager and
the migration wire name pages by those without depending on the store that holds
them.

Checkpoint objects are immutable and written create-if-absent. A checkpoint is
one index object at `vm/<id>/ckpt/<seq>/index`, holding a fixed
header, the page-table segments the checkpoint changed and the root over every
volume's segments; and its parts at `vm/<id>/ckpt/<seq>/part/<n>`, holding the
VMM state and the pages. The index format is 8 and the part layout is 4.
Format 7 stated no volume's page size, so its page numbers are 2 MiB pages and
nothing else; formats 5 and 6 of the index object held the root alone — 5 held
the whole page
table in it — and part layouts 1 to 3 kept the root as the last member of the
last part; none of them reads here, and a store written under any of them is
refused with the version it carries named rather than migrated, exactly as an
older control record is. A part's tail — the table naming its members and the
32-byte trailer naming it — is bounded at 256 KiB by the writer, so reading any
part's table is one suffix range of the object rather than a HEAD and two reads,
and opening a checkpoint is one GET of its index object. A publication uploads
the parts, waits for them, and writes the index object, which is its commit; the
control record then selects the checkpoint. A newly created fork's record selects a checkpoint
its own first publication has not written yet, so its created flag is false;
opening that VM anywhere reports that the fork is still pending, and the epoch is
left alone so the host holding the fork is not fenced by an open that could never
have succeeded.

## Conditional publication

A new record is written with `IfNoneMatch`. An update reads the object with its
opaque version tag and writes back with `IfMatch` against that exact version. A
failed condition on an update means a later writer has taken the VM over; on a
create it means the VM already exists — unless the record that is there carries
this attempt's own nonce, which is a retry inside the store's client refused by
the object its first attempt wrote. The create reads the record back before
answering, because a caller told its VM exists would have lost the one it owns:
a fork's child, whose root is published by the child's own first checkpoint,
could then be neither created nor opened by anything. A fork that cannot build
its child's handle after writing that record deletes it again for the same
reason.

Deletion is a separate unconditional operation. The caller must close the writer
first. Removing the control record prevents subsequent opens, and the VM's
checkpoint objects are then deleted too, each checkpoint's index object first. That
is what frees the identity: checkpoint objects are named by the VM and a
sequence, and a VM created under a deleted VM's identity that found objects
under it would be refused — which is what a create does when the sweep left
pinned checkpoints behind. The record goes first, so nothing can open the VM while
its objects are going.

The record is also the only thing that says which of that VM's objects a sweep
may take, so a VM that has no record is not swept at all. That is not only an
interrupted delete: a finished delete of a VM that was ever forked leaves
exactly that — no record, and objects a fork still reads — so a repeat that
swept what it found would destroy them. Repeating a delete is therefore harmless
and finishes nothing; what an interrupted sweep left is a collector's.

What the sweep leaves is the checkpoints that record pinned, and every
checkpoint their roots name. Those are the fork points the VM was taken at, and a
descendant of it — a child, or a grandchild whose own root names those —
may still read through them; nothing this delete can read says whether one does.
So a VM that was ever forked frees its identity and leaves those objects behind
for a collector. A record that cannot be parsed at all is not deleted: its pins
are exactly what the sweep would have to spare, and an identity nobody can
delete is the lesser loss.

A missing response does not prove that a write failed. A writer reconciles by
reading the record back: the one it meant to write means the write landed, and
the one it already had means it did not. Only the writer of an epoch can produce
a record carrying that epoch's nonce, so the answer is never ambiguous — and a
record of the writer's own epoch and nonce that is neither of those two is
ambiguous only about which of its writes it is: a lost reply whose read-back
failed too leaves the writer tracking a version the store has moved past, and
the write it makes next is refused against it. That is not a takeover, so the
writer adopts the record it finds and repeats the refused call. Only a foreign
epoch or nonce fences it. This is what lets an interrupted creation, an
interrupted takeover and an interrupted checkpoint selection each be finished by
repeating them.

## Fencing and selection

Every open conditionally advances the epoch, which fences whatever writer held
the previous one: its next control-record write fails and it reports that the VM
must be reopened. Two opens that race take distinct epochs; neither shares a
writer token with the other.

Checkpoint sequences are epoch-major — the epoch in the high 32 bits, a counter
starting at one in the low 32 — so every sequence a writer allocates is above
every sequence any earlier epoch could have allocated. A fenced writer that is
still uploading objects therefore cannot collide with its successor under the
create-if-absent rule that governs every checkpoint object.

The epoch a VM starts at is drawn rather than fixed: a creating handle takes a
random epoch in [1, 2³¹) from the same entropy its writer nonce comes from, and
the opens that follow count up from it, which leaves at least 2³¹ takeovers
before `ErrEpochExhausted`. What that buys is that two VMs created under one
identity — a name reused after a delete, which the orchestrator does not do but
nothing here can enforce — allocate different sequences, and therefore different
page identities and different object keys. A VM must never be served the
bytes or the keys of a VM that held its name before.

A create is refused altogether when the identity's `vm/<id>/` prefix holds any
object and no control record accounts for it: those objects are the pinned
checkpoints a deleted VM left, or a create interrupted before its record, and a VM
published there would write into keys that are not its own. Repeating a create
whose record did land opens the VM instead, which is how an interrupted create
is finished.

Selection is a conditional write from the handle's own epoch. The sequence must
belong to that epoch and must not go backwards; reselecting the sequence already
selected and already created writes nothing, which is what makes a repeated
publication safe; an older epoch is refused.

That refusal is what keeps two writers of one VM from ever mixing, but it only
happens when a fenced writer next tries to publish, which may be a whole
checkpoint interval away and never at all while a fork point holds the VM
sealed. So a handle can also be asked: `VM.Confirm` re-reads the control record
and fails the handle exactly as a refused selection does when the record's epoch
is no longer the handle's. A host calls it on a timer for every VM it holds,
which is how a takeover reaches a writer that is publishing nothing. A record
that cannot be read fails nothing: only an epoch that has moved is evidence. A
handoff asks too, and it asks on its own account rather than trusting that timer
— the pages it hands another host are the one thing the store cannot refuse
afterwards — so `Migrate` and the fork point confirm the record before the
pause, and a read that fails refuses the handoff.

Because taking a VM over fences a writer that may still be running a guest, an
epoch is taken only on positive evidence that the previous holder is gone — the
orchestrator's recovery refuses while any host pod it cannot account for might
still be running the VM. The same reasoning runs the other way: two hosts both
reporting one VM is the deployment disagreeing with itself, one of them holding
a handle that can never publish, and the orchestrator refuses every request that
must name a VM's host until the disagreement is settled rather than picking a
claimant. Its table breaks no such tie — it records what the orchestrator last
did, not which host holds the epoch.

## Latency boundary

A volume write applies to an in-memory overlay and returns without contacting
object storage at all. Creation, opening, checkpoint publication and selection,
deletion and cold page reads pay object-storage latency; nothing on the guest's
write path does. A guest flush is not a durability point at all: checkpoints run
on the interval and on request, and the only thing a volume can verify
synchronously is that this handle still owns its VM. Availability of checkpoints
during an authority-store outage is not a requirement: a failed publication
leaves the previous checkpoint selected and the overlay intact, and the next
checkpoint retries.

Background uploads do not hold the VM lock across object-store I/O; capture and
installation still synchronize local state briefly. One control-record write per
handle is in flight at a time, and a caller waiting its turn waits under its own
context.
