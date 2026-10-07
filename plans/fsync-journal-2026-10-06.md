# A durable fsync: a journal on a network disk — 2026-10-06

**Status: decided, not built.** Task: TASK-104. The owner's decisions are in
[Decided](#decided).

## The change

Today a guest's flush returns at once unless the VM holds a disk write older
than the flush bound. So a flushed write is durable only within the bound plus
one interval.

This plan adds a **durable flush mode**. It is one setting,
`SPROUTFS_DURABLE_FLUSH`, read by the hosts and the orchestrator alike, and off
by default. Off, nothing changes: the flush bound works as it does today. On, a
flush returns success only when the bytes it covers are on a network disk that
the cloud replicates. If the host is lost, another host reads those bytes back
before the VM runs again. A flush that cannot be journaled fails with an I/O
error. The object store stays the long-term copy. A checkpoint still makes
everything durable, and it trims the journal.

The guarantee in the mode: **when a flush returns success, every store the
guest made to that disk before it sent the flush survives the loss of its
host.** It does not survive the loss of the zone, because the disks are zonal.

## What is true today

- A flush is a FLUSH request from the VM's virtio-pmem device. The pager hands
  it to the host's callback (`vmmemory/flush.go:33-50`,
  `vmmemory/connection_linux.go:837-849`). A flush of a RAM region is an error
  (`vmmemory/flush.go:34-36`). An error answer reaches the guest as EIO
  (`vmmemory/connection_linux.go:854-870`).
- The host answers at once when the VM's oldest unpublished disk write is
  younger than the bound. Otherwise the flush waits for a checkpoint that the
  host asks for out of turn (`host/flush.go:77-101`, `host/flush.go:108-132`).
  The bound is two intervals, 120 s by default (`host/flush.go:30`,
  `host/flush.go:39-49`, `host/host.go:160-166`).
- A flush of an ephemeral disk, of a VM with no checkpoint loop, or of a VM that
  asked for no checkpoints, completes at once (`host/flush.go:69-86`).
- Flushes of a VM that leaves the host are not answered. The device keeps them
  in its snapshot and sends them again from the next host
  (`host/flush.go:103-104`, docs/vm-memory.md:2250-2255).
- The pager never writes a volume, even for a flush (docs/vm-memory.md:1387-1393).
- A checkpoint for each flush is not an option. Each selection is a
  compare-and-set of the control record: 52.5 ms p50 and 76.3 ms p99 on GCS.
  GCS also slows writes to one object that changes more than about once a
  second (docs/measurements/gce-start-latency-2026-10-04.md:187, 200-206).
- A seal write-protects the region's dirty runs, one command per run, and the
  guest's next store to such a page traps (`vmmemory/checkpoint.go:204-249`,
  `vmmemory/checkpoint.go:536-545`, `vmmemory/region.go:489`). A trap on a
  write-protected page arrives as a protect trap
  (`vmmemory/connection_linux.go:716-723`).
- A PMEM page is 2 MiB by default (`host/supervisor.go:35-36`,
  docs/vm-memory.md:291-297).
- SHA-256 of a 2 MiB page took 1.67 ms on Ice Lake, which has SHA
  instructions, and 5.67 ms on Cascade Lake, which does not; of a 4 KiB page,
  3.3 µs (docs/measurements/gce-dependent-reads-2026-10-03.md:232-233).
- The cluster cache can live on shards: a fixed set of network disks that the
  membership assigns to hosts (docs/hosting.md:1277-1300). A host in shard mode
  keeps no disk of its own and may serve none, one or several shards
  (docs/hosting.md:1166-1171, 1313-1314). The controller moves shards between
  members to keep them even (docs/hosting.md:1210-1216).
- The cloud attaches a network disk to one machine at a time. After a detach,
  every operation on the device fails (`platform/networkdisk.go:16-24`,
  `platform/networkdisk.go:47-56`). The cloud port can describe, attach and
  detach a disk, not create or delete one (`platform/networkdisk.go:41-45`). A
  shard's header carries a lease that a newer assignment wins
  (`checkpoint/diskrestart.go:123-142`).
- Every open advances the epoch and fences the previous writer
  (`control/client.go:180-190`, docs/metadata.md:233-245). A recovery opens a
  VM only on positive evidence that its host is gone
  (`cmd/sproutfs-orchestrator/orchestrator.go:1659-1684`,
  docs/metadata.md:283-299).
- A migration publishes nothing. The destination opens the VM, advances the
  epoch, and fetches the unpublished pages from the source
  (docs/migration.md:24-77). The destination registers the VM before the
  post-copy is done (`host/migrate.go:370-383`).

## Words

- **Durable flush mode**: the setting that turns the journal on.
- **Journal**: a host's write-ahead log of flushed bytes, on its journal disk.
  The guest's own filesystem journal is always called "the guest's journal".
- **Journal disk**: the network disk that holds a journal.
- **Block**: 4 KiB of a page. A 4 KiB page is one block.
- **Digest**: the SHA-256 of one block, as its last entry left it.
- **Entry**: the changed blocks of one memory region, taken by one capture.
- **Batch**: the entries written by one write and made durable by one sync.
- **Position**: an entry's logical byte position in its journal. It only grows.
- **Capture**: taking a region's changed blocks into an entry.
- **Unjournaled**: a page the guest may have stored into since its last
  capture.
- **Covered position**: the last position whose entries a checkpoint covers.

## Where the journal lives

**Each host has one journal disk of its own,** while the mode is on. It is a
network disk of the same kind as a shard, and it is attached, opened and fenced
by the same machinery: the membership assigns it, the controller attaches it
through `platform.NetworkDisks`, the host opens the device for itself alone,
and a lease in its header refuses an older assignment. The lease is read when
the disk is opened and on every pass, not before each answer. It does not rank
windows.

A cache shard does not fit, for three reasons:

1. A host may hold no shard. Shards are a fixed set and hosts scale
   (docs/hosting.md:1284-1287, 1313-1314).
2. Shards move on their own schedule. The controller rebalances them and moves
   them off draining hosts (docs/hosting.md:1210-1216). A VM's journal would
   then move away from the VM, or block every flush during a move of 13 to
   14.6 s (docs/measurements/gce-shards-2026-10-04.md:370).
3. The cluster's fills and reads use the shard's device. A flush would queue
   behind them.

The machine has a limit of its own: 400 MiB/s and 25,000 IOPS of Hyperdisk
for a 4-vCPU C3 (docs/measurements/gce-shards-2026-10-04.md:62). Google applies
it to the machine, not the disk, so a host that also serves a shard shares it
with its journal.

**Size and kind.** The first setting is 32 GiB of Hyperdisk Balanced,
provisioned 6,000 IOPS and 400 MiB/s. Step 9 measures Hyperdisk Balanced
against pd-ssd and sets the default from the measured p99. A pd-ssd read 4 KiB
at one in flight in 299 µs against 467 µs for Hyperdisk Balanced
(docs/measurements/gce-shards-2026-10-04.md:221, 235), and pd-ssd attaches to
N2 hosts, which Hyperdisk Balanced does not.

### The orchestrator makes and removes journal disks

Journal disks follow the hosts, so their number changes with the hosts. There
is no fixed pool and no PersistentVolumeClaim. The orchestrator creates and
deletes the disks through the cloud's API. `platform.NetworkDisks` gains
`List`, `Create` and `Delete`
(`platform/networkdisk.go:41-45`), with Compute Engine's `disks.list`,
`disks.insert` and `disks.delete` behind them.

- **Names and labels.** A journal disk is named `sproutfs-journal-<random>` and
  labelled `sproutfs-journal=<deployment>`. Its identity is derived from its
  name, as a shard's is (`membership/controller.go:63`).
- **Create.** The orchestrator creates the disk in the node's zone, then adds
  it to the membership as free. A crash between the two leaves a labelled disk
  the membership does not list. The next pass adds it as free, with its
  contents unknown, so it is opened and read once before it is trusted to be
  empty.
- **Delete.** The orchestrator marks the disk deleting in the membership,
  deletes it in the cloud, then removes it from the membership. Each pass
  repeats whatever step is left, so a crash between them converges.
- **Free and empty.** A disk is free when no member holds it and no machine
  has it reserved. It is empty when its last holder closed it with no live
  entry, which the holder writes into the disk's header and reports. Only a
  free and empty disk is reused or deleted.
- **Deletion after one hour.** A disk that has been free and empty for an hour
  is deleted. Kubernetes' cluster autoscaler removes a node it has not needed
  for ten minutes by default, so nodes come and go within an hour, and a disk
  kept that long is reused by the next join without a create. An hour of a
  32 GiB disk costs little. The orchestrator keeps the time in memory; a
  restarted orchestrator starts the hour again, which only delays a delete.

**Permissions.** The orchestrator's service account needs
`compute.disks.create`, `compute.disks.delete`, `compute.disks.list` and
`compute.disks.setLabels` in the zones of its nodes, besides what shards
already need: `compute.disks.get`, `compute.disks.use`,
`compute.instances.get`, `compute.instances.attachDisk`,
`compute.instances.detachDisk` and `compute.zoneOperations.get`
(docs/hosting.md:1387-1395). It also needs to list Kubernetes nodes.

### Scale-up

1. A node joins the host pool. The orchestrator sees the node before the host
   pod on it is ready.
2. It reserves a free and empty journal disk in the node's zone for that
   machine, or creates one. The membership records the reservation.
3. It attaches the disk to the machine. This runs while the node boots and the
   host pod starts, so the attach call's 6 to 7 s
   (docs/measurements/gce-shards-2026-10-04.md:385-386) is mostly hidden.
4. The host joins as a member. The membership assigns it the disk reserved for
   its machine.
5. The host opens the disk, takes the lease, and reports it served. The
   orchestrator places no VM on the host until then.

### Scale-down

1. The autoscaler chooses a node. Its host pod is terminating, and its member
   drains, as today (docs/hosting.md:1185-1199).
2. The VMs move away by migration. Each destination asks for a checkpoint out
   of turn as soon as its post-copy is done.
3. The host keeps its journal disk open for reading. It waits until each moved
   VM's record no longer names its journal: the destination's first full
   checkpoint drops it (see [Migration](#migration)). That is about one
   publication after the post-copy.
4. With no live entry left, the host writes empty into the header, closes the
   disk, and reports it closed.
5. The orchestrator detaches the disk. It is free and empty.
6. The host pod exits, and the node can go.

The host pod's termination grace period must cover the drain and this wait.
`deploy/` sets it. A pod killed before the wait ends is a host loss, below:
nothing is lost, and recovery is slower.

### Host loss

1. The member drains as gone. The orchestrator detaches its journal disk. If
   the machine was deleted, the cloud has already detached it.
2. The membership assigns the disk to a surviving member in its zone, the one
   holding the fewest journal disks, for reading only.
3. That member opens it, takes the lease, reads it back, and serves its entries
   to the hosts that recover the lost host's VMs.
4. Once no control record names the disk, the member writes empty into the
   header and closes it. The disk is detached and free.

A member writes into one journal disk and may hold others for reading. A disk
can attach only in its own zone, so recovery waits while no host runs in that
zone. A deployment in one zone, as today's is, never waits for this.

## What a flush writes

### Which bytes

Guest stores to a PMEM disk are CPU stores into mapped memory. Nothing logs
them. So the journal cannot be a log of writes. It is a log of **changed
blocks of dirty pages**, taken when a flush asks.

The pager keeps a set of unjournaled pages per region, as runs, like the dirty
runs (`vmmemory/checkpoint.go:536-545`). One rule keeps it right:

> Every page the guest can store into without a fault is unjournaled.

So every path that maps a page writable for the guest marks it unjournaled:
a store fault, a copy on write after a seal, a refault after a spill. A capture
takes the set and write-protects its runs, one command per run, as a seal does.
The guest's next store to such a page takes a protect trap. For a page that no
seal holds, the trap marks the page unjournaled and takes the protection off.
It copies nothing. A trap on a sealed page copies on write as today, and the
new copy is unjournaled.

The pager keeps a SHA-256 digest of each block of each page it has captured:
16 KiB for a 2 MiB page. A capture reads each block of an unjournaled page once
into the batch buffer, hashes that copy, and keeps the block only if its digest
differs from the one held. Kept blocks replace their digests. So the digests
always describe what the journal holds for that page.

A store that lands while the capture reads a block may or may not be in the
entry. Either is allowed: that store came after the flush, and the page is
unjournaled again, so the next capture reads it. The replayed state is one
that a power cut of a real persistent-memory device could leave: every store
before an answered flush is there, and later stores are there or not, at
8-byte granularity.

**Why blocks and not pages.** A 2 MiB page at the machine's 400 MiB/s takes
5 ms to write. A database that appends 4 KiB to its log and flushes would pay
that on every flush, and 400 MiB/s would allow only 200 such flushes a second
per host. A 4 KiB block costs one small write.

**Where a page's digests come from**, when the pager holds none for it:

1. the page's origin, for a page with an origin still resident: the capture
   hashes the origin. A cold copy pins its origin
   (docs/vm-memory.md:1562-1567, 1715-1724), so a cold read on x86-64 costs
   hashing but no journal bytes;
2. the digest of a zero block, for a page made from zeros;
3. otherwise none: the capture writes all 512 blocks.

The hashing runs on the settle's workers (`Config.SettleWorkers`,
docs/vm-memory.md:1617-1633), so the pages of one capture are hashed in
parallel. A host without SHA instructions hashes about 3.4 times slower; this
is the same question as TASK-96.

A capture of a page the pager has spilled reads it back from the spill and
checks its checksum, as the give-back does.

### The entry

```
offset size
0      4    magic "SFJE"
4      1    format version, 1
5      1    kind: 0 pad, 1 blocks
6      2    zero
8      8    position
16     8    the journal's generation, from its header
24     8    the VM's epoch
32     4    the entry's length, header to checksum
36     4    the number of blocks
40     2    length of the VM identity
42     2    length of the volume's name
44     4    zero
48          the VM identity, the volume's name, padded to 8
            the block numbers, 8 bytes each: the volume's byte offset / 4096
            the blocks, 4 KiB each
end-16 16   XXH3-128 of everything before it
```

A pad entry has no names and no blocks. It fills a batch to the next 4 KiB
boundary and fills the end of the ring. The entry's checksum is XXH3-128, as in
`internal/blob/blob.go:228` and the parts
(`checkpoint/internal/part/part.go:49`): it finds torn and damaged entries.
The SHA-256 digests stay in the pager's memory and are never written.

### The journal disk

The disk begins with two header slots of 4 KiB. A header is rewritten into the
other slot, so a torn header write leaves the older one. Each slot holds:

```
0   4  magic "SFJH"
4   1  format version, 1
5   1  empty: 1 when its last holder closed it with no live entry
6   2  zero
8   8  the slot's counter; the valid slot with the higher counter wins
16  16 the disk's identity
32  8  the generation, drawn when the journal is formatted
40  8  where the ring starts, and 48 8 its length
56  8  the tail hint: a position at or before the oldest live entry
64  8  the lease: the membership generation that assigned the disk
72  16 the lease: the member's identity
88  16 XXH3-128 of the slot's first 88 bytes
```

The rest is a ring. A position maps to the ring's start plus the position
modulo its length. An entry never wraps; a pad fills the end.

**Reading back.** A host that opens a journal disk takes its lease, then reads
entries from the tail hint forwards. It stops at the first entry whose magic,
version, generation, position or checksum is wrong. That position is the head.
It indexes the entries it read by VM and epoch.

**Positions are never reused by the process that gave them out.** A batch whose
write or sync failed may be partly on the disk. The next batch first writes a
pad over the failed range, in the same write, and then its own entries after
it. Otherwise a reader would stop at the failed range and miss what follows,
and a covered position could cover entries written later.

### Group commit

Each journal disk has one writer and at most one batch in flight. Flushes that
arrive while a batch is in flight wait. When it completes, the writer captures
every region with a waiting flush into the next batch, up to 8 MiB, writes it,
syncs it (`platform.File.Sync`, `platform/networkdisk.go:52-53`), and answers
its flushes. Batches complete in position order, so answers do too.

A capture of one VM's regions holds that VM's journal lock. The seal's pause
takes the same lock (see below).

### What a flush waits for

1. Its capture: the protect commands, the hashing and the copies.
2. The write and sync of the batch that holds its entry.
3. The batch before it, if one was in flight.

It waits for nothing in the object store and no control-record write.

From the measured disks and processors, before step 9 measures the real thing:

| Case | Estimate |
| --- | --- |
| A flush of a few changed blocks, Hyperdisk Balanced | one small write and sync: about 0.6 ms p50 and 1.2 ms p99, from a 4 KiB write at 16 deep, 614 and 1,245 µs (docs/measurements/gce-shards-2026-10-04.md:224). With a batch in flight ahead of it, up to twice that: about 1.2 ms p50 and 2.5 ms p99 |
| The same on gp3 | 4 KiB write at 16 deep, 979 and 1,253 µs (docs/measurements/aws-hot-tier-2026-10-04.md:168) |
| Hashing an unjournaled 2 MiB page | 1.67 ms with SHA instructions, 5.67 ms without, per page and worker (docs/measurements/gce-dependent-reads-2026-10-03.md:232-233) |
| A page with no digests | 2 MiB more to write: about 5 ms at 400 MiB/s |

So a flush that touches one or two pages costs about 3 to 4 ms at p50 on a host
with SHA instructions, and most of it is hashing. The cost of a sync on these
devices is not measured. Step 9 measures write and sync together at one in
flight, and the hashing in place.

## Seals, checkpoints and the journal

### The covered position

A checkpoint's seal fixes the VM's disks at one point. Entries captured before
that point hold nothing newer than the checkpoint. Entries captured after it
may.

The seal's pause takes the VM's journal lock. So no capture runs during it, and
every entry has its position before or after the seal. The host records the
VM's last captured position at the pause. When the checkpoint is selected, the
selection writes that position into the control record as the covered
position. A replay applies only entries after it.

This applies to every seal: the interval's (`host/capture.go:86-95`), a
capture's, a stop's and a fork point's.

### The unjournaled set and the digests across a seal

- **Seal.** The unjournaled pages the seal takes go with the checkpoint, as its
  unjournaled list. The region's set restarts empty. Those pages lose their
  digests.
- **A flush while the seal stands.** A capture takes the region's unjournaled
  set and the checkpoint's unjournaled list. For a page the guest has stored
  into since the seal, it captures the guest's page and drops the page from the
  list. Otherwise it captures the sealed copy, which never changes. Without
  this, a flush answered while a checkpoint uploads would not cover the stores
  made just before the seal, and a publication that then failed would lose them.
- **Selected.** The list is dropped. The checkpoint holds those bytes.
- **Abandoned** (`vmmemory/checkpoint.go:428-441`). Every page still on the
  list becomes unjournaled again. Every page handed back loses its digests.

**The digests of the other pages.** A page that was not unjournaled at the seal
holds exactly the bytes its last entry left. Those are also the bytes the
checkpoint holds for it, and the bytes a replay starts from whether the
checkpoint lands or not. So its digests could stay, and its next capture would
write only the blocks that changed. That is the rule to keep, but only if
`MCCapture` passes with it (step 1). If TLC finds a counterexample, every
digest is dropped at every seal, and the first capture of each page after a
seal writes the whole page: about 5 ms more, once per page per interval.

### Trimming

An entry is dead when the control record of its VM no longer names its journal
and epoch, or its position is at or before the covered position the record
names. The holder of a journal disk learns this:

- for a VM it runs, from its own selections;
- for any other VM with entries on the disk, from that VM's control record,
  read every 30 s, a few at a time, and at once during a drain. A missing
  record means the VM is deleted.

The tail is the oldest live entry. The holder writes a new tail hint into the
header when the tail has moved, at most once a second.

### When the journal fills

A full ring is back-pressure, not a failure.

- When live bytes pass three quarters of the ring, the host asks for
  checkpoints out of turn, for the VMs with the most live bytes first, through
  the path the dirty budget uses (`host/interval.go:291`).
- No VM may hold more than half the ring. A VM past that waits for its own
  checkpoint.
- When the ring is full, captures wait for trimming. A flush is never answered
  without its entry on the disk.

A VM whose checkpoints keep failing holds its entries. The loss window already
stops its stores (docs/architecture.md:121-137). Its flushes then wait for
space too.

**What fills it.** Live bytes are the journal's write rate times the time from
one selection to the next. At 400 MiB/s for 60 s that is 24 GiB, so 32 GiB with
the three-quarter mark holds a host that writes at the machine's limit. Step 9
measures live bytes under real workloads.

### The mode, the flush bound and the loss window

**Off.** The flush bound works as it does today (`host/flush.go:77-132`). No
journal disk is made or opened, and the control record names no journal.

**On.** A flush is answered by its entry, never by the flush bound and never by
a checkpoint. A flush that cannot be journaled fails, and the guest reads EIO.
That covers a host with no journal disk served, a write or sync that fails,
and a disk that was detached. ext4 in the guest then aborts its journal and
remounts the filesystem read-only, so the guest stops writing rather than going
on as if its data were safe.

The orchestrator places a VM only on a host whose journal is served, so a guest
sees EIO only after a real failure. `/metrics` and `/status` report
`durable_flush`, which says whether the mode is on.

The loss window and the interval stay. In the mode they bound what a host loss
costs **unflushed** writes. Flushed writes are not lost. The interval also
trims the journal.

## Recovery after a host loss

### The control record names the journals

The record gains a list of journals: each entry names a journal disk's
identity, its generation, the VM's epoch whose entries it holds, and the
covered position. This is control-record format 6 (format 5 today,
docs/metadata.md:37-41; `control/record.go:16-52`).

- A selection writes the list (`control/client.go:466-480`,
  `volume/publish.go:616-621`). Usually it is one journal: this host's, this
  epoch, the covered position.
- A stop, a close and a suspend select a final checkpoint that covers every
  store. They write an empty list.
- An open keeps the list as it is. A migration's destination adds its own
  journal (see below).
- No change adds a control-record write. The record changes at the same moments
  it changes today. With the mode off, the list is always empty.

### Moving the journal disk

See [Host loss](#host-loss). A shard moved in 13 to 14.6 s on GCE
(docs/measurements/gce-shards-2026-10-04.md:341-372). A journal disk will move
in about the same time. A VM recovered after a host loss therefore starts about
15 s later than today.

### Replay before the VM runs

A recovery open (and any open other than a migration's) does this:

1. It opens the record and advances the epoch, as today
   (`control/client.go:190`). The list of journals is kept.
2. For each journal in the list, in order, it asks the member that holds the
   disk for the VM's entries of that epoch after the covered position. This is
   a new peer request, `JOURNAL_READ`. It names the reader's new epoch.
3. It writes each block into the VM's volume (`volume/operations.go:114`), in
   position order. These writes go into the overlay.
4. If it wrote any block, the VM is cold booted, even if the selected
   checkpoint holds VMM state. Old registers over newer disks would describe a
   guest that never existed (docs/architecture.md:103-112).
5. `Host.Starting` takes its checkpoint (`host/coldboot.go:151-166`). It now
   publishes the replayed blocks too. Its selection writes a list with only
   this host's journal.
6. Only then does the guest start.

If a journal's disk is not served by anyone yet, the open fails with
`ErrJournalPending` and changes nothing else. The orchestrator tries again, as
it does while a receive is in flight
(`cmd/sproutfs-orchestrator/orchestrator.go:1636-1657`).

If a journal's generation does not match, the disk was formatted again and its
entries are gone. The open fails with `ErrJournalLost`. Only an operator's
`--discard-journal` opens such a VM, and it logs what it discards.

### Fencing a host that is still alive

Three things keep a living host from losing an answered flush or mixing its
entries into a newer state:

1. **The holder fences the old writer.** Entries for a VM's epoch are written
   only by the host that holds that epoch, into its own journal. A
   `JOURNAL_READ` goes to the member that holds the disk. That member is the
   disk's only writer. Before it answers, it records that the VM is fenced at
   the reader's epoch. From then on it captures nothing for that VM at an older
   epoch, and every flush of it fails. If it runs the VM, it gives it up, as a
   fenced host does (`host/fence.go:69-100`). So every entry it ever answered a
   flush for is in what the reader receives.
2. **A detached host cannot write.** If the old host cannot be reached, the
   recovery waits. An operator's force drains its member, and the controller
   detaches its journal disk. After that every write it makes fails
   (`platform/networkdisk.go:53-54`), so its flushes fail too.
3. **A replay reads only what the record names.** Entries of an epoch that the
   record no longer names, or at or before its covered position, are never
   applied. So a fenced host's entries are never written over a newer
   checkpoint.

This trusts the cloud never to attach one disk to two machines, as the shards
do.

## Migration

The source stops the guest and hands it over. It publishes nothing, as today.

- **The handoff carries the unjournaled runs** of each region
  (`vmmigrate/vmmigrate.go:120-159` gains a field). These are the pages the
  guest stored into since their last capture on the source.
- **Flushes held at the stop move with the guest** and are sent again to the
  destination (docs/vm-memory.md:2250-2255). The source answers none of them.
- **The destination's open adds its own journal to the record's list**, after
  the source's. The list then names two journals.
- **The destination answers flushes only after the post-copy is done.** Its
  first capture needs the source's unjournaled pages, and it may not have them
  yet. The stream fetches unpublished pages first, so this is seconds. It holds
  no digests at first, so those first entries are whole pages.
- **The destination asks for a checkpoint out of turn when the post-copy is
  done. Its first selection sealed after that drops the source's journal from
  the list.** Only that checkpoint holds every page the source held.
- **The source keeps the VM's entries** until the record no longer names its
  journal. It reads the record on its epoch timer (`host/host.go:167-176`).
- **A handoff is refused while the list names two journals.** The host takes a
  checkpoint out of turn and the drain tries again. So a list never names more
  than two.

If the destination dies before that selection, a recovery replays the source's
entries, then the destination's. If the source dies during the post-copy, the
migration ends as today, and the recovery replays the source's entries from
wherever its journal disk is served.

The destination does not need the source's journal in the normal case. It needs
it only for a recovery.

## Forks

The parent is unchanged. Its fork point is a seal like any other.

A child exists only on its host until its root is published
(docs/architecture.md:209-227), and a record that selects an unpublished root
cannot be opened (docs/volumes.md:298-302). So nothing could replay a child's
entries. **A child answers its flushes only after its root is selected.** After
that, its flushes go through its host's journal like any VM's. The parent's
journal is never read for a child.

## RAM and ephemeral disks

RAM is out of scope. A flush is a disk's, and a flush of a RAM region is an
error (`vmmemory/flush.go:34-36`). RAM is durable only through a capture, as
today.

An ephemeral disk's flush completes at once, as today
(`host/flush.go:75-81`, `vmmemory/region.go:358`). Nothing is journaled for it.

## Failures

In the mode:

| Failure | What happens |
| --- | --- |
| A write or sync fails | The batch's flushes fail with EIO. The journal reopens: it reads back to find its head, then writes a pad over the failed range in its next batch. Later flushes are journaled again if that works. |
| The journal is full | Captures wait for trimming. The host asks for checkpoints out of turn. A VM over half the ring waits for its own. Nothing fails. |
| The disk is detached while VMs run | Every operation fails, so every flush fails with EIO. If this host was fenced, the epoch timer closes its VMs within 2 s (`host/host.go:312`). If not, its member asks for a journal disk again, and flushes fail until one is served. |
| A torn batch at a power loss | Reading back stops at the first bad entry. No answered flush is in a torn batch, because a batch is answered only after its sync. |
| The holder of a named journal is unreachable | Recovery waits with `ErrJournalPending` until the disk is served, or until an operator forces the old host's disk off it. |
| A host with no journal disk served | Its flushes fail with EIO. The orchestrator places no VM on it. |
| A VM with no checkpoint loop | Its flushes complete at once and nothing is journaled, as today (`host/flush.go:69-73`). It asked for disks that are not durable, and no checkpoint would ever trim its entries. |

## Format changes

There is no production deployment (docs/architecture.md:313-316), so no data
needs migrating. Each change bumps its version, and the older one is refused by
name, as the store does today (docs/metadata.md:116-118).

- The control record: format 6, with the list of journals.
- The journal disk: a new format, version 1.
- The membership: a disk gains a kind, cache or journal, a reserved machine and
  a deleting state. Only cache disks rank windows.
- The peer protocol: `JOURNAL_READ`.
- The handoff: the unjournaled runs.

## Steps

Each step lands with `just check` green and changes no behaviour while the mode
is off. Every step follows the repo's practice: synctest bubbles over
`platform/sim`, Buggify sites with probes that a campaign asserts, `sim.Bug`
guards in `scripts/mutation/guards.json` that `just check-guards` must kill,
and Gremlins before and after on the new code (docs/testing.md:1337, 2106,
3076).

### 1. Model-check the rules

`spec/journal`, factored by concern so each TLC run ends within two minutes:

- `MCCapture`: one region of two pages of two blocks; stores, flushes,
  captures, syncs, seals, selections, abandons and a crash with a replay. A
  digest is modelled as the block's value. Each store writes a larger value
  than the block held, so a lost store shows as a smaller value. Invariants:
  `NoLostFlush` (after a replay every block holds at least the value it held
  when the last answered flush was sent), `NoRegression` (a replay never holds
  less than the selected checkpoint), and the rule that every page writable
  without a fault is unjournaled. It is run with digests kept across a seal
  for pages that were not unjournaled at it. If that passes, the rule stays;
  if not, the plan drops every digest at every seal.
- `MCTakeover`: three hosts, their journal disks, one VM; epochs, migration
  with post-copy, recovery reads that fence, a host that keeps running after
  it is fenced, detaches and moves of disks. Invariants: `NoLostFlush` at the
  VM's level, `NoFencedReplay` (no entry is applied unless the record names its
  disk and epoch and it is after the covered position), and `OneWriter` per
  disk.
- Deep configurations under `spec/journal/deep`, for `just check-spec-deep`.
- Mutants, each caught: the seal keeps the unjournaled set, a protect trap does
  not mark, an unjournaled page keeps its digests across a seal, a replay takes
  any epoch, the source's journal leaves the list before the post-copy is done,
  a read does not fence the holder, a failed range's positions are reused.

### 2. The journal on a disk

A new package, `journal`: the header slots, lease and empty flag, entries, the
ring, pads, the group-commit writer, reading back, `JOURNAL_READ`'s server
side, and trimming by covered positions. It works on a `platform.File`.

Tests over `platform/sim` disks with `PowerLossFaults` (docs/testing.md:641-680):
every entry answered before a power loss reads back; a torn batch ends the
read; a failed write's range is padded and nothing after it is lost; positions
never repeat; a lease of a newer assignment refuses the disk. Format fixtures
under `journal/testdata`, a fuzz test of the parser, and a guard
`journal-answer-before-sync`.

### 3. The control record names its journals

Format 6: `Record.Journals`. `Select` and `SelectKept` take the list. An open
keeps it, and a migration open adds to it. A stop and a close write an empty
list. Tests for each, the format fixtures, and the record's TLA+ spec
(`spec/ownership`) updated if its state changes.

### 4. The pager captures changed blocks

In `vmmemory`: the unjournaled runs, the protect trap on a page no seal holds,
`MemoryRegion.Capture`, the SHA-256 digests per block and their sources (the
origin, zeros), the hashing on the settle's workers, the checkpoint's
unjournaled list across a seal, a selection and an abandon, the digest rule at
a seal as step 1 decided, and capture of a spilled page.

Tests: the rule that every writable page is unjournaled, checked after every
step of the existing pager campaigns; a store during a capture lands in the
next one; a flush during a seal covers the stores before it, also when the
publication is then abandoned; the Linux userfaultfd test of the new trap on
2 MiB HugeTLB pages and on 4 KiB pages. Guards: `journal-trap-not-marked`,
`journal-seal-keeps-unjournaled`, `journal-digests-survive-unjournaled-seal`.

### 5. The host answers flushes from the journal

In `host`: the mode (`SPROUTFS_DURABLE_FLUSH`, off by default); the journal disk
agent, built like the shard agent (`host/shards.go:21-37`); the flush path
through capture, batch, sync and answer; EIO for a flush that cannot be
journaled; the covered position at the seal and in the selection; the
three-quarter mark, the half-ring limit and captures that wait on a full ring;
the holder's side of `JOURNAL_READ` and its fence; `/status` and `/metrics`
(flush latency, hashing time, live bytes, failed flushes, `durable_flush`).
With the mode off, the flush path is today's.

Tests: a flush is answered only after its batch syncs; a full ring holds
captures and asks for checkpoints; a failed write or sync fails its flushes
with EIO and the next batch pads its range; a host with no journal fails
flushes; a read fences the holder's own VM; with the mode off, today's flush
tests pass unchanged. Guards: `journal-covered-after-seal`,
`journal-read-without-fence`, `journal-full-answers`,
`journal-failed-write-answers`.

### 6. Journal disks in the membership and the orchestrator

In `platform`: `NetworkDisks` gains `List`, `Create` and `Delete`, in the sim
(with Buggify sites: create slow, create fails, delete fails) and in the
Compute Engine adapter.

In `membership`: the disk kind, a reserved machine, and the deleting state;
`Next` and `Carry` for journal disks (`membership/controller.go:111`,
`membership/attach.go:41`): reserve a free and empty disk for a new machine or
call for a create, assign a member the disk reserved for its machine, give a
lost member's disk to a survivor in its zone for reading, release a disk whose
holder reports it empty, delete one free and empty for an hour. Journal disks
rank no window. `spec/shards` gains journal disks and keeps `OneServer`.

In the orchestrator: list nodes, create and delete disks, attach at a node's
arrival, place VMs only on hosts whose journal is served, and retry a recovery
on `ErrJournalPending`. A drain waits until the host reports its journal empty.
In `deploy/`: the mode's setting, the permissions, and a termination grace
period that covers a drain and one checkpoint.

Tests: scale-up attaches before the host joins; scale-down detaches only an
empty disk; a lost host's disk is read on a survivor and released once
trimmed; each crash point between create, membership and delete converges.

### 7. Replay, migration and forks

Replay on open, in record order, through `JOURNAL_READ`; the cold boot when
anything was replayed; `Host.Starting` publishing the replayed blocks. The
handoff's unjournaled runs; the destination's flushes waiting for the
post-copy; its checkpoint out of turn after the post-copy; the source keeping
its entries until the record drops its journal; a handoff refused while the
list names two journals. A fork's child answering flushes only after its root
is selected.

### 8. The simulation proves it

In `internal/simtest`, with the mode on:

- `TestAFlushAnsweredJustBeforeItsHostDiesIsThereWhereTheVMOpensNext`: a guest
  stores, flushes, gets its answer; the host is killed at once (docs/testing.md:304-320);
  the VM is recovered on another host; every block the guest stored before the
  flush holds its value or a later one.
- The same with the kill between the sync and the answer, during a seal's
  upload, during a migration's post-copy (source killed, then destination
  killed), during a scale-down's wait, and with the old host kept running but
  cut off.
- A campaign, `TestJournalsSurviveTheirFaultsAndReachTheirProbes`, with
  Buggify sites (a write slow, a write failing, a torn batch at power loss, the
  disk detached mid-batch, a full ring, a capture racing a seal, a disk moving
  during a recovery, a create failing) and probes that each must reach. Every
  flush either succeeds and survives, or fails with EIO; none succeeds and is
  lost.
- A fingerprint arm with journals, stable under shake.
- The guards from steps 2, 4 and 5 named in `scripts/mutation/guards.json`, and
  `journal-replay-any-epoch` and `journal-drop-source-early`, each killed by its
  tests. Gremlins before and after on `journal`, the pager's capture and the
  host's flush path.

This step checks TASK-104's second criterion. Step 1 checks the third.

### 9. Measure on GCE

`scripts/bench-fsync-journal-gce.sh` on c3-standard-4 hosts with the mode on;
first Hyperdisk Balanced, then pd-ssd:

- fio on the raw journal disk: 4 KiB to 64 KiB writes with a sync, one in
  flight, p50, p99 and p99.9;
- in a guest, flush latency at 1, 8 and 64 threads that each write 4 KiB and
  flush, with the mode on and off;
- the throughput cost: the guest's write rate with and without flushes, protect
  traps a second, hashing time, journal bytes per guest byte, and live bytes
  over an interval;
- a host killed after a flush: the time until the VM runs again, split into the
  disk's move, the replay and the publication;
- a scale-up and a scale-down: Compute Engine's create, attach and detach
  calls, and the scale-down's wait for the journal to empty.

The report is `docs/measurements/gce-fsync-journal-<date>.md`. Every VM, disk
and object is deleted and the script checks that none remain. Each run is kept
short. This step checks TASK-104's fourth criterion and sets the journal disk's
default kind and size.

### 10. The documents

After the trimming of `docs/` that is under way now: the mode, the loss model
and the flush in `docs/architecture.md`, the new words in `docs/context.md`,
format 6 in `docs/metadata.md`, journal disks and their scaling in
`docs/hosting.md`, the capture in `docs/vm-memory.md`, the handoff in
`docs/migration.md`, the tests in `docs/testing.md`, a property in
`docs/properties/`, and `deploy/README.md`.

## Decided

The owner decided these on 2026-10-06.

1. **One journal disk per host.** Not a region on a cache shard. The
   orchestrator creates a disk when a host joins and none is free, and deletes
   one free and empty for an hour. Scale-up attaches in parallel with the
   node's boot; scale-down waits until the moved VMs' records no longer name
   the journal; a lost host's disk is read on a survivor and released once
   trimmed. No PersistentVolumeClaims.
2. **Changed 4 KiB blocks, found by SHA-256 digests.** 16 KiB of digests per
   2 MiB page, in the pager's memory. Whether digests survive a seal is
   decided by `MCCapture` in step 1; they survive only if TLC passes.
3. **Durable flush is an optional mode, off by default.** Off: today's flush
   bound, unchanged. On: a flush that cannot be journaled fails with EIO, with
   no fallback to a checkpoint; a full ring is back-pressure. `durable_flush`
   is a metric of whether the mode is on.
4. **Trust the cloud's single attach.** The lease is read when a disk is
   opened and on every pass, not before each answer.
5. **A migration keeps the source's journal named** until the destination's
   first checkpoint after the post-copy.

## Not in this plan

- Surviving the loss of a zone. Regional disks would do it, at a higher write
  latency.
- The AWS adapter for attaching, creating and deleting volumes, which TASK-86
  also lacks for attaching. The design is the same over gp3.
- RAM.
