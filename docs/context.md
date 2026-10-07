# Terminology

## Storage

**VM**: One virtual machine: the unit of identity, write ownership and
durability. Each VM has one identity, one control record and one series of
checkpoints.

**Tenant**: The owner a VM belongs to, if any. The tenant is part of the VM's
identity, `<tenant>/<name>`, and every object key of the VM is under
`tenants/<tenant>/`. Deleting that prefix removes the tenant and nothing else.
No page crosses between tenants, except a public template's: a fork's child
belongs to its parent's tenant, and a tenant's templates are its own. A VM of
no tenant has its keys at the top of the deployment prefix.

**Public template**: A template of no tenant, `template-<digest>`. A VM of any
tenant can be created from it and shares its pages. A host imports its
configured images as public templates.

**Volume**: One named, byte-addressed image of a VM: its memory (`ram0`) or one
of its PMEM disks. Its size is fixed for the VM's lifetime.

**Ephemeral disk**: A PMEM volume that no checkpoint holds. Its pages live only
in the ephemeral pager of the host that runs the VM. A checkpoint records its
name, size and page size. It is lost with its host and at a stop, reaches no
fork, and is carried by a migration. A VM opened elsewhere gets it back zeroed.
See [volumes](volumes.md#ephemeral-disks).

**Page**: The unit of publication, of faults and of resident ownership.

**Geometry**: A volume's page size, 4 KiB or 2 MiB, and the number of its pages
that one segment of its page table covers. The host chooses the page size when
it creates the volume. It is recorded in every checkpoint of the volume and
never changes, and readers divide by the page size the root recorded. A pager's
page size is fixed when it is built, and it refuses a volume of another. A host
runs RAM and PMEM at 2 MiB by default and can run either at 4 KiB
(`SPROUTFS_RAM_PAGE_BYTES`, `SPROUTFS_PMEM_PAGE_BYTES`). The simulation runs
RAM at 4 KiB.

**Overlay**: What a VM has written through the volume package since its last
checkpoint, held in memory on the host that owns the VM. Only image building
and tests write through it; a pager never does. It is not durable.

**Control record**: The only mutable object a VM owns in the store. It holds
the writer epoch, the selected checkpoint, the pins (checkpoints of this VM
that have been forked), the kept checkpoints and the journals that may hold
flushed writes newer than the selected checkpoint. It changes only by
conditional write. See [metadata](metadata.md).

**Kept checkpoint**: A checkpoint that a checkpoint request (a capture, or a
stop or suspending stop with keep) asked to keep. Reclamation spares it and
everything it reads, so a VM can be created from it later. A create from one
with VMM state resumes the guest; one without boots cold. A kept checkpoint no
VM was created from can be released. See
[metadata](metadata.md#kept-checkpoints).

**Checkpoint**: The operation that makes a running VM durable, and the objects
it leaves in the store. The steps:

1. The vCPUs pause while the VMM state is saved, if included, and the dirty
   pages of each memory region in the checkpoint are sealed.
2. The guest resumes.
3. The sealed pages stream out as parts.
4. The index object is written last, with the segments the parts changed and
   the root.
5. A conditional write selects the checkpoint in the control record. From then
   on, the VM survives the loss of this host.

The host checkpoints every VM's disks on an interval, 60 s by default. A
capture on request and a suspending stop also seal RAM and save VMM state. A
VM's durable state is the one checkpoint its record selects. An initial sparse
checkpoint writes no part. See
[architecture](architecture.md#loss-model).

**Cold boot**: Starting a VM over a checkpoint that has no VMM state, such as an
interval checkpoint, a plain stop's or a template's. The host takes a
checkpoint that discards the VM's memory and boots the kernel over the disks.
The guest sees a power cut at that checkpoint.

**Loss window**: How long a VM may hold a disk write that no landed checkpoint
covers (`SPROUTFS_LOSS_WINDOW`, five minutes by default, zero to disable). As a
measurement, the age of the VM's oldest such write. Past it, the pager admits
no further dirty page for that VM while a sealed checkpoint of it is uploading,
and the host requests a checkpoint out of turn. See
[architecture](architecture.md#loss-model).

**Index object**: One checkpoint's metadata, at `vm/<id>/ckpt/<seq>/index`: a
fixed record, the page-table segments the checkpoint changed, the **root**, and
the fixed record again. For each volume, the root says where each segment of
the page table is and which checkpoints this one reads. It copies its parent's
segment addresses forward and names no parent. The object's create-if-absent
PUT commits the publication.

**Part**: One object of a checkpoint's data, at `vm/<id>/ckpt/<seq>/part/<n>`,
filled to 64 MiB and uploaded as it fills. It holds members (the VMM state,
then each volume's changed pages in page order, then the pages compaction
rescued), a table of at most 1 MiB that names them, and a fixed trailer that
names the table.

**Extent**: A member's location in the part that holds it, and the bytes one
ranged read fetches. A read of a range of a volume is a **run** of pages, whose
members are fetched one extent per group of adjacent members in a part. See
[volumes](volumes.md#reads).

**Seal**: Removing the guest's write access to a memory region's dirty pages in
place. The pages then belong to the checkpoint while the guest runs. Nothing is
copied; a later store into a sealed page copies only that page.

**Flush**: A guest's virtio-pmem flush, which is how its fsync reaches the
host, over that disk's memory session. With durable flush off, the host
answers at once if the VM holds no unpublished disk write older than the flush
bound (`SPROUTFS_FLUSH_BOUND`, twice the checkpoint interval by default, zero
to disable), and otherwise when a checkpoint covering those writes lands. See
[architecture](architecture.md#loss-model). With it on, the host answers once
the disk's changed blocks are in its journal.

**Reclamation**: After a checkpoint is selected, deleting the checkpoints its
root no longer names and nothing protects. **Compaction** rewrites the live
pages of checkpoints less than half live into the new checkpoint's parts, up to
64 MiB of live bytes, after the guest resumes.

**Fork point**: One pause of a running parent: the checkpoint the parent has
published, the pages sealed since, and the VMM state saved with them. Taking it
publishes nothing, and the parent keeps running. One point serves any number of
children. The parent's pages stay sealed until every child has published its
first checkpoint or pulled the pages it inherited.

**Fork**: A VM created from a parent's fork point without changing any bytes.
It has its own control record, and its writes are isolated. The parent's
published sequence is pinned in the parent's record, permanently. A child runs
on the parent's host, sharing the sealed pages, or on another host, pulling
them from the parent's peer server.

**Handoff**: The data that starts a VM on another host: the VMM state, the
checkpoint the VM inherits, the runs of unpublished pages, and the address of
the peer server that serves them. A migration hands off a VM the source
released. A fork hands off a child from a parent that keeps running.

**Hold**: How long a source keeps what a handoff needs when nothing releases
it: the pages of a VM it handed over, or a child's fork point. Four checkpoint
intervals. A failed receive is retried until the hold ends. After it, the pages
are gone whether or not the source is reachable.

**Page identity**: The name of the page whose bytes a range reads, as
(checkpoint reference, volume, page). Sparse zeroes have a special identity. A
page is named by the checkpoint that published it, and a fork inherits its
parent's names. Compaction moves bytes without changing the identity. Pages
with the same identity share one resident page within a pager.

**Resident page**: The physical backing of one page in one of a host's pagers.
Memory regions with the same page identity share it. A host runs one pager per
kind of memory region, each with its own arena, spill file and page size, so
everything a host reports across pagers is in bytes. A 2 MiB arena uses the
HugeTLB pool, and a 4 KiB arena an ordinary shared memfd.

**Pull**: Fetching every page of the checkpoint a VM started from, in the
background while the guest runs, so its faults read the hosts' disks and not
the object store. It is a prefetch with no guarantee. A start marks a VM to
pull, and every start, recovery and migration of the VM carries the mark.
Inside the share the cluster cache is on for, a pull asks each window's ranks
what they hold (a **presence check**) and fills the cluster with what it lacks.
Outside it, the pull copies the pages onto the host's page cache disk. See
[hosting](hosting.md#pulling-a-vms-memory).

**Hot tier**: A second bucket, closer to the hosts than the regional bucket,
that holds copies of checkpoint objects under the same names. Reads try it
first, and a miss or failure reads the regional bucket and fills the hot tier
behind the read. The regional bucket is the only durable copy. A host refuses
to run with both a hot tier and the cluster's disk cache. See
[hosting](hosting.md#reading-through-a-hot-tier).

## Cluster

**Host**: A machine that runs VMs and serves their pages to migration
destinations and to forks on other hosts.

**Peer server**: The one channel between hosts, one per host on one port. It
serves the pages a handoff left on the host, stripe reads, keeps, drops and
presence checks for the cluster's disk cache, and reads of the journal disks
the host holds. It speaks a framed protocol over TCP. See
[the peer server](migration.md#the-peer-server).

**Peer**: Another host, as this host's table of peers sees it: one per remote
host, with a pool of connections per class, the budget the remote host gave
each class, and whether it is down.

**Class**: What a request is for, which decides its connections and the budget
it counts against at the server. A guest fault is the fault class. A disk cache
read that a fault waits on is the stripe class. The post-copy stream and a
request for a fill right are bulk reads. Keeps are bulk writes. A bulk request
never shares a connection or a budget with a fault.

**Busy**: A peer server's answer to a request that would take its class past
the class's budget. It says how much the class holds, may hold, and asked for.
The connection stays open.

**Background budget**: The bytes one host's bulk work may have in flight at all
its peers. It admits unpublished post-copy pages first, then the rest of the
stream, then fills, then repairs. Fills and repairs over it are dropped. While a
guest fault waits, it shrinks to a quarter.

**Down**: A peer that failed hard: a failed dial or hello, or a connection that
heard nothing for four seconds. See [liveness](migration.md#liveness).

**Marked down**: A host that a reader of the cluster's disk cache stops asking
for stripes and sending fills, after three of its stripe requests to it in a row
timed out or one connection was refused. See
[hosting](hosting.md#reading-from-the-cluster).

**Writer**: The single process allowed to publish a VM's checkpoints, set by
the epoch in the control record.

**Epoch**: The writer token in the control record. Every open advances it,
which fences the previous writer. It is the high half of every checkpoint
sequence the writer allocates.

**Drain**: Migrating every VM off a host, so that the host process can exit
without rewinding any of its VMs.

**Window**: The pages of one volume, in one aligned 2 MiB span, that one
checkpoint published: the unit the cluster's disk cache places. One envelope at
a 2 MiB page, up to 512 at 4 KiB. A page-table segment is its own window.

**Membership**: One object, at `membership`: a generation, the deployment's
code, every member and every disk. Anything that routes requests between hosts
reads it, and the disk cache places windows by it. It changes only by
compare-and-set. Every process holds a copy, never the authority. See
[hosting](hosting.md#the-membership).

**Generation**: The count of the membership's changes. A request that routes by
the membership names its sender's generation. A host behind it reads the
membership first, and a host ahead of it answers that the sender is stale.

**Member**: A host in the membership: its identity, its peer server's address,
and its state (joining, active or draining). The identity is the one in its
cache file's header, or, for a host that serves shards, one drawn when its
process starts.

**Disk**: A disk in the membership: its identity, its volume, its kind (cache
or journal), its weight from its size, the member it is assigned to, its state
(attaching, serving, releasing, released, or deleting for a journal disk), and
the generation that assigned it. A journal disk also has the machine it is
reserved for and whether it is empty. Windows are ranked over cache disks, so a
disk that moves to another member keeps its windows. A journal disk ranks no
window.

**Shard**: One network disk of a fixed set that may hold the cluster cache
instead of the hosts' own disks: a single-writer Hyperdisk Balanced on GCP. It
is a disk of the membership whose identity is derived from its volume's name,
and it moves between members as compute scales. See
[hosting](hosting.md#shards-on-network-disks).

**Lease**: The end of a shard's header region: the generation of the assignment
it was last opened under, the member's identity, and the regions it has opened.
A member of an older assignment is refused the shard, and a member reads the
lease again before every region it writes, so one that lost the shard stops
writing it. A journal disk's header carries a lease too, which its holder takes
as it opens the disk and reads again on every pass.

**Rank**: A disk's place in one window's order. Each disk scores the window as
its weight over -ln(u), where u is a hash of the disk's identity and the
window. The highest score ranks first, and ties go to the lower identity. The
disks ranked 1 to k+m hold the window's stripes.

**Code**: The deployment's erasure code: k data and m parity stripes per
envelope, any k of which rebuild it. k = 1 is whole copies. It is in the
membership, 4+2 when unset. A host that has read no membership holds its own
disk under 1+0. See [hosting](hosting.md#the-code).

**Earlier code**: A code the deployment used before, listed after the current
one, newest first. A window stored under it is read and rebuilt under it until
it ages out.

**Stripe**: One of the k+m pieces of an envelope under a code. It names its
index, its code and its envelope's length. Stripe i of a window goes on rank
((i − 1) mod n) + 1 of its n ranked disks.

**Fill**: Putting a window's stripes on the disks the membership ranks for it.
A read of the store, a publication and a pull fill. A read's or pull's fill is
dropped when there is no room; a publication's waits, up to a bound. See
[hosting](hosting.md#filling-the-cluster).

**Keep**: The peer-server request that fills a cache: the stripes of one window
for that disk, each with its own checksum, under the sender's generation.

**Read from the cluster**: A read of a page that misses in memory, inside the
share. It takes this host's own stripes of the window, asks k+1 of the window's
ranks for theirs, and rebuilds the page from any k distinct indices. The store
is read only for a page the cluster cannot rebuild.

**Second request**: Asking the rest of a window's ranks once k stripes have not
arrived after about the 95th percentile of the reader's recent reads of the
same size.

**Repair**: A stripe a reader sends to a rank that holds fewer of a window's
stripes than the code puts on it. It is a keep of the lowest priority.

**Fill right**: The right to fill a window from a read of the store. The
window's rank 1 gives it to the first reader that asks, once per window per
interval, so a burst of readers fills a window once.

## Durable flush

**Durable flush**: An optional mode, off by default (`SPROUTFS_DURABLE_FLUSH`).
On, a flush of a disk returns success only once every store the guest made to
that disk before it is on the host's journal disk, and it survives the loss of
the host. A flush that cannot be journaled fails with an I/O error. See
[architecture](architecture.md#durable-flush).

**Journal**: A host's write-ahead log of flushed blocks, on its journal disk
(package `journal`). The guest's own filesystem journal is always called the
guest's journal.

**Journal disk**: The network disk that holds a journal: two header slots and
a ring. Each machine of the host pool has one reserved for it while the mode
is on, and the host on that machine writes it. A host may hold others for
reading, after their writers were lost. The orchestrator creates and deletes
them. A journal disk is **free** when it is released and reserved for no
machine, and **empty** when its last holder closed it with no live entry. See
[hosting](hosting.md#journal-disks).

**Journal generation**: A number drawn when a journal disk is formatted, in its
header and in every entry. A record that names a journal names its
generation, so a disk formatted again is known to have lost its entries.

**Block**: 4 KiB of a page. A 4 KiB page is one block. The journal holds
blocks, not pages.

**Digest**: The SHA-256 of one block, as the journal last took it. The pager
keeps the digests of each page it has captured, in memory: 16 KiB for a 2 MiB
page. They are never written.

**Unjournaled**: A page the guest may have stored into since its last capture.
Every page the guest can store into without a fault is unjournaled.

**Capture of a disk**: Taking a disk's changed blocks for a flush: the pager
write-protects the unjournaled pages, reads them, and keeps the blocks whose
digest changed. The guest's next store to such a page takes a **protect trap**,
which makes it writable and unjournaled again and copies nothing. This is not
the host API's capture, which checkpoints a whole VM. See
[managed VM memory](vm-memory.md#capturing-for-a-durable-flush).

**Entry**: The changed blocks of one memory region from one capture, with the
VM, its epoch, the volume and the journal's generation. A large capture makes
several entries.

**Batch**: The entries one write puts on the ring and one sync makes durable.
A journal has one batch in flight. The flushes that arrive meanwhile join the
next, and each is answered once its batch has synced.

**Position**: An entry's logical byte position in its journal. It only grows,
and the process that gave it out never gives it again.

**Covered position**: The last position whose entries a checkpoint holds. The
selection writes it beside the journal it is in. A replay applies only the
entries after it.

**Trimming**: Freeing the ring of entries no record needs: those of an epoch or
a journal the VM's record no longer names, and those at or before the covered
position it names.

**Replay**: Writing a VM's entries back into its disks when a host opens it, in
the order the record names the journals and in position order within each. A
host reads them with `JOURNAL_READ` from whichever host holds the disk, which
fences the VM at the reader's epoch first. A VM anything was replayed into is
cold booted.
