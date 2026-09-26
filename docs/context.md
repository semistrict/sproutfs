# Terminology

These terms are shared across the [architecture](architecture.md) and its
supporting documents.

## Storage

**VM**: One virtual machine. It is the unit of identity, write ownership and
durability. Each VM has one identity, one control record and one series of
checkpoints.

**Tenant**: The owner a VM belongs to, when it belongs to one. The tenant is
part of the VM's identity, `<tenant>/<name>`, and so part of every object key
the VM has: they all live under `tenants/<tenant>/`. Deleting that prefix
removes the tenant and nothing else. No page crosses between tenants: a fork's
child belongs to its parent's tenant, and each tenant imports its own
templates. A VM whose identity names no tenant belongs to none, and its keys
are where every VM's were before tenants existed.

**Volume**: One named, byte-addressed image of a VM: its memory (`ram0`) or one
of its PMEM disks. A volume's size is fixed for the VM's lifetime.

**Ephemeral disk**: A PMEM volume that no checkpoint holds. Its pages live only
in the ephemeral pager of the host that runs the VM. A checkpoint records its
name, size and page size, and none of its pages. It is lost with its host and
at a stop, it reaches no fork, and a migration carries it. A VM opened anywhere
gets it back zeroed at the recorded size.

**Page**: The unit of publication, of faults and of resident ownership.

**Geometry**: A volume's page size, and the number of its pages that one
segment of its page table covers. The host chooses the page size when it
creates the volume. The page size is either 4 KiB or 2 MiB. It is recorded in
every checkpoint of the volume and never changes. A page number is meaningless
without the page size, so every reader divides by the page size the root
recorded, not by a constant. A pager instance also has a page size, fixed when
the pager is built. The pager refuses to attach a volume with any other page
size. A host runs RAM and PMEM at 2 MiB by default and can run RAM at 4 KiB
(`SPROUTFS_RAM_PAGE_BYTES`); the simulation runs RAM at 4 KiB. The mapping protocol carries each session's page size and the
kind of memory its arena uses. Both ends refuse a mismatch before any guest
memory exists.

**Overlay**: What a VM has written through the volume package since its last
checkpoint, held in memory on the host that owns the VM. Only image building
and tests write through the overlay; a pager never does. The overlay is not
durable anywhere: losing that host loses it.

**Control record**: The only mutable object a VM owns in the store. It selects
the writer epoch and the checkpoint. It lists the checkpoints of this VM that
have been forked. These are the pins. It also lists the kept checkpoints.
Reclamation spares pinned and kept checkpoints, and nothing releases a pin. The
record changes only by conditional write. See [Metadata authority](metadata.md).

**Kept checkpoint**: A checkpoint a checkpoint request asked to keep: a
capture, a stop or a suspending stop with keep. It is kept in the write that
selects it, and reclamation spares it and everything it reads, so a VM can be
created from it however far its VM has moved on. A create from one with VMM
state resumes the guest where its pause left it; one without boots cold. A kept
checkpoint no VM was created from can be released. One a VM was created from
is pinned too, and the pin is permanent.

**Checkpoint**: Both the operation that makes a running VM durable and the
objects that operation leaves in the store. The operation has these steps:

1. The vCPUs pause while the VMM state is saved and every memory region's dirty pages
   are sealed.
2. The guest resumes.
3. The sealed pages stream out as parts.
4. The index object is written last. It carries the segments the parts changed
   and the checkpoint's root.
5. A conditional write selects the checkpoint in the control record. From this
   point, the VM survives the loss of this host.

Every VM a host runs has its disks checkpointed on an interval. The interval is
sixty seconds by default. Each wait is jittered by up to an eighth in either
direction, and the next wait is measured from the last upload. The interval's
pause seals only the disks and saves no VMM state, and its checkpoint names no
VMM state. A capture on request and a suspending stop also seal RAM and save
the VMM state. Capture returns without waiting for the upload. The upload can
fail, and its pages then go back to the guest. A VM's entire durable state is
the one checkpoint its record selects. An initial sparse checkpoint writes no
part.

**Cold boot**: Starting a VM over a checkpoint that has no VMM state. Examples
are the interval's checkpoint of the VM's disks, a plain stop's checkpoint, and
a template's checkpoint. The host takes a separate checkpoint that discards the
VM's memory, and boots the kernel over the disks. The guest sees this as a
power cut at that checkpoint. A VM comes back this way after a host loss, and
an operator's cold start requests it.

**Loss window**: How long a VM may hold a disk write that no landed checkpoint
covers: `SPROUTFS_LOSS_WINDOW`, five minutes by default, zero to disable. As a
measurement, the loss window is the age of the VM's oldest such write. Past the
window, the pager admits no further dirty page for that VM while a sealed
checkpoint of it is uploading, and the host requests a checkpoint of that VM
outside the interval's schedule. A store is never held while its checkpoint
still needs a pause, because a held store holds its vCPU and a pause needs
every vCPU. So the window bounds in time what losing a host can cost one VM, as
the dirty budget bounds it in bytes. The lost writes span at most the window
plus the pause of one checkpoint attempt. The age
travels with the pages a handoff moves, so a destination inherits the window
and does not restart it. If a VM can never be checkpointed, the wait ends as a
full dirty budget does: the host deliberately stops that VM and takes a last
checkpoint of what it can still capture.

**Index object**: One checkpoint's metadata, at `vm/<id>/ckpt/<seq>/index`. It
contains a fixed record, the page-table segments the checkpoint changed, the
**root**, and the fixed record again. So the end of the object is enough to
locate the root. For every volume, the root says where each segment of the
volume's page table is fetched from, and which checkpoints this one reads. The
root copies its parent's segment addresses forward and replaces only the
segments its own checkpoint changed. So the root is complete on its own and
does not name a parent. The index object's create-if-absent PUT commits the
publication.

**Part**: One object of a checkpoint's data, at
`vm/<id>/ckpt/<seq>/part/<n>`. A part is filled to 64 MiB and uploaded as it
fills. It holds a sequence of encoded members: the VMM state, then each
volume's changed pages in page order, then the pages compaction rescued. The
members are followed by a table of at most 1 MiB that names them, and a fixed
trailer that names the table. So a part describes itself.

**Extent**: A member's location in the part that holds it, and the bytes that
one ranged read fetches. A read of a range of a volume is a **run** of pages.
The run's members are grouped by the part they are in and by their position in
that part. Each group is fetched as one extent and decoded from that one
buffer. Publishing in page order places the members of consecutive pages next
to each other. So a pager's cold 2 MiB read-ahead run of 512 4 KiB pages takes
two requests instead of one per page: one for the segment that locates the
pages, and one for the extent that holds them.

**Seal**: Removing the guest's write access to a memory region's dirty pages in place.
Those pages then belong to the checkpoint while the guest keeps running.
Nothing is copied and no bytes move. A later store into a sealed page copies
only that page. So the pause is only page-table work.

**Flush**: A guest's virtio-pmem flush, which is how its fsync reaches the
host. The device holds the flush and asks the host over that disk's memory
session. The guest's flush returns when the host answers. The host answers
immediately if the VM holds no unpublished disk write older than the flush
bound: `SPROUTFS_FLUSH_BOUND`, twice the checkpoint interval (120 s) by default, zero
to disable.
Otherwise the host answers when a checkpoint that covers those writes lands,
and it requests that checkpoint outside the interval's schedule. A flush never
runs a checkpoint directly. The ordering comes from the checkpoint, which is
one pause across all of the VM's disks.

**Reclamation**: After a checkpoint is selected, deleting the checkpoints that
its root no longer names and no pin protects. Each such checkpoint is deleted
whole. Compaction limits what reclamation leaves behind. A checkpoint rewrites
the live pages of checkpoints that are less than half live into its own parts,
up to 64 MiB of live bytes, after the guest has resumed.

**Fork point**: One pause of a running parent. It consists of the checkpoint
the parent has published, the pages sealed since then, and the VMM state saved
with them. Taking a fork point publishes nothing, and the parent keeps running.
So a fork costs the pause and the child's boot, and one pause serves any number
of children. The parent's pages stay sealed until every child has published or
pulled the pages it inherited.

**Fork**: A VM created from a parent's fork point without changing any bytes.
It has its own control record, and its writes are isolated. The parent's
published sequence is pinned in the parent's record. The pin stops reclamation
from deleting the checkpoint the child inherits and every checkpoint that
checkpoint's root names. The pin is permanent. Only a collector, which can see
every fork, may release a pin. A child runs either on the parent's host,
sharing the sealed pages, or on another host, pulling them from the parent's
page server.

**Handoff**: The plain data that starts a VM on another host: the VMM state,
the checkpoint the VM inherits, the runs of unpublished pages, and the address
of the page server that serves them. A migration hands off a VM that the source
released. A fork hands off a child from a parent that keeps running.

**Hold**: How long a source keeps what a handoff needs when nothing releases
it: the pages of a VM it handed over, or the fork point of a child. It is four
checkpoint intervals. A handoff is good for as long as its source holds it, so
a receive that fails is tried again until then. After it, the pages are gone
whether or not anything can reach the source, which is what ends a migration
whose source is listed and unreachable.

**Page identity**: The name of the page whose bytes a range reads, reported as
(checkpoint reference, volume, page). Sparse zeroes have a special identity.
Every page has one name: the checkpoint that published it. A fork inherits its
parent's names. Inherited pages keep the same identity. Compaction can move
their bytes into another checkpoint's parts without changing their identity. A
named page is referenced and never copied: in the store, on the wire, and in
host memory. In host memory, pages with the same identity share one resident
page within a pager.

**Resident page**: The physical backing of one page in one of a host's pagers.
Several memory regions with the same page identity can share it. A host runs one pager
per kind of memory region: one for its guests' RAM and one for their PMEM disks. Each
pager has its own arena, spill file and page size: 2 MiB for PMEM, and 2 MiB
for RAM by default or 4 KiB when configured. So a page count from one pager says nothing about the other, and
everything a host reports across both pagers is in bytes. An arena's memory
matches its page size: the HugeTLB pool for 2 MiB, and an ordinary shared memfd
for 4 KiB.

**Pull**: Copying every page of the checkpoint a VM started from onto the disk
of the host that runs it, in the background while the guest runs. A start
marks a VM to pull, and the VM keeps the mark: the orchestrator records it, and
every start, recovery and migration of the VM carries it. The copy lives in the page
cache's disk, keyed by page identity, and is held while the VM runs on that
host. Once it is complete, a fault on a page that is not resident makes no
request of the object store. The copy is never durable. A VM whose checkpoint
does not fit on the disk is not pulled, and reads the store as any VM does. See
[hosting](hosting.md#pulling-a-vms-memory).

## Cluster

**Host**: A machine that runs VMs and serves their pages to migration
destinations and to forks on other hosts.

**Writer**: The single process allowed to publish a VM's checkpoints. The epoch
in the control record establishes the writer. Every open advances that epoch,
which fences the previous writer.

**Epoch**: The writer token in the control record. It is also the high half of
every checkpoint sequence that writer allocates.

**Drain**: Migrating every VM on a host to other hosts, so that the host
process can exit without rewinding any of its VMs.
