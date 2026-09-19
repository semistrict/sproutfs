# Terminology

These terms are shared across the [architecture](architecture.md) and its
supporting documents.

## Storage

**VM**: One virtual machine. It is the unit of identity, of write ownership and
of durability: one identity, one control record, one series of checkpoints.

**Volume**: One named byte-addressed image of a VM: its memory, `ram0`, or one
of its PMEM disks. A volume's size is fixed for the VM's lifetime.

**Page**: The unit of publication, of a fault and of resident ownership.

**Geometry**: A volume's page size and how many of its pages one segment of its
page table covers. The host chooses the page size when it creates the volume —
4 KiB or 2 MiB, and nothing else — and it is recorded in every checkpoint of
that volume and fixed for its life: a page number means nothing without it, so
every reader divides by what the root recorded rather than by a constant of its
own. The pager, the wire and the VMM still have one page, 2 MiB, so a volume of
any other page size is refused when it is attached; RAM at 4 KiB is
[planned](../plans/ram-pmem-page-geometry-2026-09-19.md) and not yet built.

**Overlay**: What a VM has written through the volume package since its last
checkpoint — image building and tests, never a pager — held in memory on the
host that owns it. It is durable nowhere: losing that host loses it.

**Control record**: The one mutable object a VM owns in the store. It selects
the writer epoch and the checkpoint, lists the checkpoints of this VM that have
been forked — the pins, which reclamation spares and nothing gives back — and
changes only by conditional write. See [Metadata authority](metadata.md).

**Checkpoint**: Both the operation that makes a running VM durable and what it
leaves in the store. The vCPUs pause while the VMM state is saved and every
region's dirty pages are sealed; the guest resumes; the sealed pages stream out
as parts, and the index object carrying the segments they changed and the
checkpoint's root is written last; a conditional
write then selects that checkpoint in the control record, which is when the VM
survives the loss of this host. Every VM a
host runs is checkpointed on an interval — sixty seconds by default, each wait
jittered by up to an eighth either side, the next measured from the last upload —
and on request. Capture returns without waiting for the upload; the upload can
fail, and its pages then go back to the guest. A VM's whole durable state is
the one checkpoint its record selects; an initial sparse checkpoint writes no
part at all.

**Loss window**: How long a VM may hold a write no landed checkpoint covers —
`SPROUTFS_LOSS_WINDOW`, five minutes by default, zero to disable — and, as a
measurement, the age of its oldest such write. Past the window the pager admits
no further dirty page for that VM: every store that needs a dirty reservation
waits, and a checkpoint of that VM is asked for out of the interval's turn. So
what losing a host can cost one VM is bounded in time as the dirty budget bounds
it in bytes: the lost writes span at most the window plus one checkpoint
attempt's pause. The age travels with the pages a handoff moves, so a
destination inherits the window rather than restarting it, and where a VM can
never be checkpointed the wait ends as a full dirty budget does — the host stops
that VM deliberately, with a last checkpoint of what it can still capture.

**Index object**: One checkpoint's metadata, at
`vm/<id>/ckpt/<seq>/index`: a fixed header, the page-table segments the
checkpoint changed, and the **root**, which says for every volume where each 512
MiB segment of its page table is fetched from and which checkpoints this one
reads. The root carries its parent's segment addresses forward and replaces only
the segments its own checkpoint changed, so it is complete on its own and names
no parent. The index object's create-if-absent PUT is the publication's commit.

**Part**: One object of a checkpoint's data, at
`vm/<id>/ckpt/<seq>/part/<n>`: filled to 64 MiB and uploaded as it fills, a run
of encoded members — the VMM state and pages — followed by a table naming them
and a fixed trailer naming the table, so a part describes itself.

**Seal**: Taking the guest's write access to a region's dirty pages away in
place, so those pages become the checkpoint's while the guest keeps running.
Nothing is copied and no byte moves — a store into a sealed page copies that
one page — so the pause is page-table work.

**Flush**: A guest's virtio-pmem flush. It makes nothing durable: the device
completes it itself and the host is not asked. Ordering is the checkpoint's,
which is one pause of the whole machine.

**Reclamation**: Deleting, after a checkpoint is selected, the checkpoints its
root no longer names and no pin protects, whole. Compaction bounds what that
leaves behind: a checkpoint rewrites the live pages of checkpoints that are less
than half live into its own parts, up to 64 MiB of live bytes, after the guest
has resumed.

**Fork point**: One pause of a running parent: the checkpoint it has
published, the pages sealed since, and the VMM state saved with them. Nothing
is published to take one and the parent keeps running, so a fork costs the
pause and the child's boot, and one pause serves any number of children. The
parent's pages stay sealed until every child has published or pulled the pages
it inherited.

**Fork**: A VM created from a parent's fork point without changing a byte. It
has its own control record and its writes are isolated; the parent's published
sequence is pinned in the parent's record, which keeps reclamation off the
checkpoint the child inherits and off every checkpoint its root names. The pin
is permanent: only a collector, which can see every fork, may release one. A
child runs on the parent's host, sharing the sealed pages, or on another host,
pulling them from the parent's page server.

**Handoff**: The plain data that starts a VM on another host: the VMM state,
the checkpoint it inherits, the runs of unpublished pages and the page-server
address they are served from. A migration hands off a VM the source released; a
fork hands off a child from a parent that keeps running.

**Page identity**: The name of the page whose bytes a range reads, reported
as (checkpoint reference, volume, page); sparse zeroes have a special identity.
Every page has one name — the checkpoint that published it — and a fork
inherits its parent's names. Inherited pages retain the same identity —
compaction moving their bytes into another checkpoint's parts does not change
it — and a page with a name is referenced, never copied: in the store, on the
wire, and in host memory, where pages of the same identity share one resident
page within a pager.

**Resident page**: The physical backing of one page in a host's pager,
possibly shared by several regions with the same page identity.

## Cluster

**Host**: A machine that runs VMs and serves their pages to migration
destinations and to forks on other hosts.

**Writer**: The single process allowed to publish a VM's checkpoints,
established by the epoch in the control record. Every open advances that epoch,
which fences the writer before it.

**Epoch**: The writer token in the control record, and the high half of every
checkpoint sequence that writer allocates.

**Drain**: Migrating every VM a host runs to other hosts, so the process can
exit without rewinding any of them.
