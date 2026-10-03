# Two-node staging: fsync durable without waiting on object storage

**Status: design agreed and specified, 2026-10-02; nothing implemented.**
Written against `main` at `9eb0ab5`. `spec/stage` models the protocol. The
next step is a prototype of the stage cut in `vmmemory`.

## The ask

A guest's fsync should be durable when it returns, and it should not wait on
object storage. A flush is acknowledged once the bytes it covers are on local
disk on at least two nodes. Object storage stays the long-term home and is
filled behind the guest, as checkpoints are today.

**Goal (Ramon, 2026-10-02):** the simplest system that records durable
commits, where a commit survives:

- the loss of any single node, and
- a power loss of the whole data center, with both nodes going down at once.

Consistency stays with the object store's compare-and-set. The nodes run no
consensus.

What the goal requires, and nothing more:

1. **Both copies on media that survive power loss.** An acknowledgement waits
   for `fdatasync` on both nodes, not for the peer's memory. The disk must
   honour flushes or have power-loss protection. Cloud local SSDs are often
   not persistent across a host failure or maintenance (GCE Local SSD is not),
   so the stage must sit on a disk that is. This has to be checked per
   deployment.
2. **After a power loss, either node alone is enough.** Every acknowledged
   batch is on both disks, so whichever node comes back first can recover the
   VM. Entries past the last acknowledgement were never promised, so it does
   not matter which node holds more of them.
3. **The record names the peer**, so a recovering host knows where to look.

The simplest first version therefore has: the stage cut, an append-only log
synced on both nodes, acknowledgement after both syncs, the peer named in the
record, the peer's epoch fence, truncation after a selected checkpoint, and
recovery by replaying the log over the checkpoint. It leaves out: block
diffing, latest-page materialisation on the peer, recovery placed on the peer,
and relaxing the loss window. Each of those is an optimisation with no effect
on the guarantee.

## Where we are today

- Durable state is one published checkpoint selected by the control record.
  Nothing is durable between checkpoints (`docs/architecture.md`, Loss model).
- A flush (virtio-pmem FLUSH, `host/flush.go`) completes at once unless the VM
  holds an unpublished disk write older than `FlushBound` (120 s default). Then
  it waits for a checkpoint. So an fsync today promises "durable within the
  bound plus one interval", not "durable now".
- The interval checkpoint (60 s) seals the PMEM regions and uploads. Losing a
  host rewinds its VMs' disks to the last selected checkpoint.
- Disks are PMEM mapped through the pager. Guest stores never reach a write
  call, so there is no write stream to log (the LSVD comparison on
  `claude/lsvd-log-layout` reached the same conclusion). Whatever we stage has
  to be read out of the pager's dirty pages at flush time.
- PMEM pages are 2 MiB (HugeTLB). This matters a lot below.

## The proposal in one paragraph

Each VM that opts in gets a **stage**: an append-only log of page images held
on the primary host's local disk and on one **peer** host's local disk. When a
flush arrives, the host takes a **stage cut** of the VM's PMEM regions: it
write-protects the pages dirtied since the previous cut, copies them out,
appends them to the local log and streams them to the peer. When both have
written and synced the batch, every flush that arrived before the cut is
answered. The interval checkpoint keeps running exactly as now. When a
checkpoint is selected, the primary tells the peer to drop the entries it
covers, and drops its own. After a host loss, the VM is reopened at its selected checkpoint plus
the stage entries newer than it, which the peer still holds.

## Prior work this follows

This is primary-backup replication with an external configuration store, a
well-studied design. The proposal takes its rules from these papers rather than
inventing its own.

**The nodes run no consensus.** Object storage stays the only consistency
authority. The control record's compare-and-set decides who the writer is,
which checkpoint is selected, and which peer holds the stage. The two nodes
are durable mirrors that never vote. The one rule a peer enforces itself is
monotonic: it refuses appends from an epoch older than one it has already
seen, just as the store refuses a fenced writer. The papers below are the ones
built on this split: consensus, where it exists, lives only in the
configuration store, which for us is the bucket's CAS.

- **Vertical Paxos** (Lamport, Malkhi, Zhou, PODC 2009), specifically its
  primary-backup variant, Vertical Paxos II. With an external configuration
  master, f+1 replicas tolerate f failures, so two copies survive one loss.
  The price is that every change of replica set must go through the master,
  and a new configuration may take writes only after it has the old one's
  state. Here the master is the control record and its conditional write.
  [paper](https://www.microsoft.com/en-us/research/publication/vertical-paxos-and-primary-backup-replication/)
- **PacificA** (Lin, Chen, Yang, Zhou, MSR 2008). This is the practical
  version of the same idea for log-based storage. An entry is committed only
  when every replica in the current configuration has it. A configuration
  manager owns the replica set. A new replica joins as a candidate and is
  added only after it has caught up. On a primary change, the new primary
  reconciles from the surviving replica. Sections 4 to 7 follow it.
  [paper](https://www.microsoft.com/en-us/research/publication/pacifica-replication-in-log-based-distributed-storage-systems/)
- **DRBD protocol C** (Linux kernel). This is the same acknowledgement rule at
  the block layer: a write completes when both the local disk and the peer's
  disk have it. After a peer outage, DRBD resyncs only the blocks a bitmap
  marks dirty. Here the pager's unpublished set plays the bitmap's role.
  [LWN](https://lwn.net/Articles/329543/)
- **Remus** (Cully et al., NSDI 2008). It replicates a whole VM, RAM included,
  to a backup every few tens of milliseconds, and holds network output until
  the backup has the checkpoint. It is the VM-level alternative. We do not
  adopt it: we only promise disks, and fsync is the commit point, so no output
  needs holding.
  [paper](https://www.usenix.org/conference/nsdi-08/remus-high-availability-asynchronous-virtual-machine-replication)
- **RemusDB** (Minhas et al., PVLDB 2011). It is Remus for database VMs. Its
  checkpoint compression sends each dirty page as a delta (XOR with the copy
  last sent, then run-length encoded), keeping a cache of recently sent pages.
  That is the known answer to section 2's 2 MiB amplification.
  [paper](https://www.vldb.org/pvldb/vol4/p738-minhas.pdf)
- **LSVD** (Hajkazemi et al., EuroSys 2022). It logs every block write to a
  local SSD and streams batches to object storage. The repo already compared
  its object layout (TASK-66). Its local log is single-node, so it does not
  survive host loss. This proposal adds the second node.
  [paper](https://doi.org/10.1145/3492321.3524271)

Also relevant, though not followed directly:

- **Chain Replication** (van Renesse and Schneider, OSDI 2004). With two
  nodes, the chain is just primary then peer. It matters only if we ever want
  three copies.
- **Amazon EBS and Physalia** (Brooker et al., "Millions of Tiny Databases",
  NSDI 2020). EBS replicates each volume to a small replica set inside one
  zone. Physalia is the configuration store that decides which replica is
  primary, kept close to the replicas so that reconfiguration stays available
  during partitions. Our control record lives in object storage, so
  reconfiguration is as available and as slow as the bucket. That is fine
  while reconfiguration is rare, which section 6 assumes.
- **Amazon Aurora** (Verbitski et al., SIGMOD 2017). It writes the log to a
  4-of-6 quorum across three zones, so it needs no configuration master on the
  write path. That is the alternative to our f+1 choice: more copies and no
  reconfiguration stall, at three times the bytes.
- **Blizzard** (Mickens et al., NSDI 2014). It is a fast cloud block device
  that acknowledges writes before they are durable and keeps crash
  consistency as a prefix of the write order. It is the opposite trade to this
  one, and close to what SproutFS does today.

What following them changes in this draft:

1. **Reconfiguration goes through the record, never around it** (Vertical
   Paxos II, PacificA). The primary acknowledges a flush only to the peer the
   record names. Swapping peers is a candidate catch-up, then a conditional
   write, then acknowledgements resume (section 6).
2. **Commit means every replica in the configuration** (PacificA). With two
   replicas, any survivor holds every acknowledged flush. That is why recovery
   needs only the peer (section 7).
3. **No lease.** PacificA uses leases so an old primary stops serving before a
   new one starts. Our old primary only matters through its acknowledgements,
   and the peer's epoch fence stops those. Reads are local to the one writer.
   The fence must still be a step of the TLA+ spec.

## Pieces

### 1. Stage cut in the pager (the new mechanism)

The pager already tracks a dirty set as runs and can write-protect runs in one
command each (the seal, `vmmemory/checkpoint.go`, `docs/vm-memory.md`, Seal).
A stage cut needs a second, finer notion of dirty: **unstaged**, meaning
written since the last cut. It is a subset of unpublished.

A cut on one memory region:

1. Under the protection lock, write-protect the unstaged runs and move them to
   the cut. One command per run, as the seal does. No vCPU pause.
2. Copy each page of the cut into a stage buffer, under that page's lock.
3. Hand the buffer to the stager. Clear nothing else: the pages stay dirty and
   unpublished, and the next checkpoint still seals and uploads them.

A store into a page protected by a cut takes a write-protect fault that is new
to the pager: the page is not sealed, so it is not copied. The fault marks it
unstaged again and lifts the protection. If its cut copy is still running, the
fault waits on the page lock for that one copy (tens of microseconds for
2 MiB), so a copy never sees a page mid-store. This is dirty logging, not
copy-on-write, so a cut costs no extra memory.

Why no pause: a flush only promises the stores that completed before it. Every
such store is in the unstaged set when the cut protects it. A store racing the
cut may or may not make this cut, which is exactly what a power cut on real
PMEM allows for unflushed stores. A cut covers every PMEM region of the VM, so
the existing rule holds: nothing written after a flush is durable unless
everything before it is.

Interaction with the seal: a checkpoint seal and a stage cut both
write-protect. A page can be sealed (copy-on-write for the checkpoint) and
unstaged at the same time. The rule I would start from: the seal does not touch
the unstaged set, a copy-on-write fault of a sealed page leaves the new private
page unstaged, and a cut reads the guest's current page, never the sealed copy.
This lock and state interplay is the riskiest part and should get a TLA+ spec
before code (see Verification).

### 2. Write amplification with 2 MiB pages

A cut ships whole pager pages. With 2 MiB PMEM pages, a database that appends
4 KiB to its WAL and fsyncs ships 2 MiB per fsync, to two disks and over the
network. At 1,000 fsyncs/s that is 2 GB/s per VM. This is the main cost of the
design and needs an answer before it is usable for fsync-heavy guests.

Options, cheapest first:

- **Compress the page image** with the codec checkpoint members already use.
  Helps zeros, not a busy WAL.
- **Diff against the last staged image.** Keep a shadow of each page as last
  staged, for pages that were staged and then dirtied again (the hot set). A
  cut sends only the 4 KiB blocks that differ, by direct comparison, no hash
  (consistent with the settle's no-hash rule). Memory cost is the hot set,
  bounded by a shadow budget; a page without a shadow ships whole.
- **Run PMEM at 4 KiB.** The pager supports 4 KiB for RAM. PMEM at 4 KiB loses
  HugeTLB and multiplies faults and metadata, and changes disk geometry for
  stored data.

**Decided (Ramon, 2026-10-02, confirmed after the measurement below): whole
pages, compressed.** A diff needs a second copy of every page that is cut and
then written again, and that is not worth it in the first version.

The cost is known. A traced sandbox session
(`perf-testing/2mib-pages-and-rollout-testing-2026-10-02.md` in the project
files) found small sqlite transactions dirty 22 KiB of 4 KiB blocks but 3.4 MiB
of 2 MiB pages per fsync, 159x; a log appender is about 240x. Bulk work is
1.2–3.6x. So fsync-heavy guests pay most of the peer bandwidth. Two ways out if
that matters in practice:

- run such a deployment with `SPROUTFS_PMEM_PAGE_BYTES=4096` (merged in #1;
  disks and templates are re-imported at that page size), or
- add RemusDB's XOR delta against a small, capped cache of pages last sent. The
  peer does not change (section 4), and the batch format already allows block
  diffs (section 3).

### 3. The stage log and the stager (host)

- New package (say `stage`) with the log format: a batch is a header (VM,
  writer epoch, stage position, list of (volume, page, length)) followed by
  page images or block diffs, and a checksum. Positions increase by one per
  batch.
- Local log on a dedicated node-disk directory through `platform.Disks`,
  preallocated, written with direct I/O and `fdatasync` per batch. The page
  cache disk and spill file are separate and stay scratch.
- The stager is per VM and does group commit: one cut in flight; flushes that
  arrive during it wait for the next cut.
- `host/flush.go` gains a third path: if the VM stages, `flushed` records the
  flush with the position it needs (the next cut) and the stager answers it
  when both nodes have that position. Fresh-disk and no-checkpoint paths are
  unchanged. With staging on, the flush bound becomes zero: every fsync is
  exact.

### 4. Replication to the peer

- The peer's only job is durability for the rare node loss (Ramon,
  2026-10-02). It holds opaque batches and, in the common case, is told to
  drop them. It parses no pages, runs no VM and needs no pager, so any node
  with a durable disk can be a peer.
- In the first version the peer is another host running the same daemon, and
  staging rides the existing
  host-to-host channel (`platform.Network`, the page-server port) as new
  message types: APPEND(batch), ACK(position), DROP(up to position),
  FENCE(epoch), READ(range).
- The primary sends the batch to the peer in parallel with its own local write.
  The flush is acknowledged when the local `fdatasync` and the peer's ACK (sent
  after the peer's `fdatasync`) have both returned.
- The peer keeps, per VM, the highest epoch it has seen and refuses appends
  from a lower one. That is what fences an old primary (below).
- The peer only appends. It never applies a batch (Ramon, 2026-10-02). In the
  normal case the primary uploads the checkpoint and tells the peer to release
  everything it covers, so the peer never rebuilds a page. Only recovery
  replays the log, in order, over the checkpoint.
- If deltas are ever added (section 2), the peer stays the same: a delta is
  only replayed at recovery, against the checkpoint's version of the page.

### 5. The control record: who holds the stage

A recovering host must know where the stage lives. So the record gains a
**stage** field: the peer's identity and the stage epoch. It changes only when
the peer changes, which is a conditional write on the reconfiguration path,
never on the fsync path. Each checkpoint records the **stage position** taken
at its pause, so after selection everything at or below it is covered and both
nodes can drop it.

This is a record format change (format 6) and a root or record addition, so
the compatibility assessment the architecture asks for applies.

### 6. Losing the peer

If the peer stops acknowledging, flushes stall. Two things run at once:

- the host asks for a checkpoint out of turn, as today's stale flush does, and
  a selected checkpoint answers every flush it covers;
- the orchestrator picks a new peer, following PacificA's candidate rule:
  1. The primary sends the candidate every page still unpublished, as DRBD
     resyncs from its bitmap. Flushes keep waiting.
  2. Once the candidate has synced all of it, the primary writes the record:
     the new peer and the next stage epoch, as one conditional write. Flushes
     may still return through the old peer while the candidate catches up,
     so the switch waits until the candidate holds every flush acknowledged
     so far, and sends it the newer state if not (found by `spec/stage`).
  3. Only after that write lands does the primary acknowledge again, and only
     to the new peer.

Whichever finishes first answers the flushes. So object storage is the
fallback, not the common path. The primary never drops to acknowledging on
its own disk alone. That would be the one-copy configuration PacificA allows
and Ramon ruled out. Ramon accepted the one CAS wait on peer replacement
(2026-10-02).

### 7. Losing the primary: recovery

Recovery follows `spec/recovery` (open only on positive evidence the holder is
gone, or an operator's force) with one more step:

1. Open advances the epoch in the record as today.
2. The opener sends FENCE(new epoch) to the peer. From then on the old primary
   gets no ACK, so it can acknowledge no further flush. If the peer is lost,
   the opener reads the old primary's own log instead, and fences that log
   first: a primary's local append honours the fence as the peer's does.
   Otherwise a stale ACK still in flight from the dead peer lets the old
   primary acknowledge a cut the read missed (found by `spec/stage`).
3. The opener reads the entries above the checkpoint's stage position, writes
   them to its own log and syncs them, and only then applies them over the
   checkpoint as unpublished dirty pages. Without the sync, the recovered
   state sits only on the peer until the new instance's first cut, and losing
   the peer then loses flushes acknowledged before the recovery (found by
   `spec/stage`).
4. The guest cold boots over those disks, as after any disk-only checkpoint.
   To the guest this is a power cut at its last acknowledged fsync instead of
   at the last checkpoint.
5. The new host checkpoints promptly and sets up a fresh stage.

Recovering **on the peer's host** keeps the pages local, but leaves one disk
where two are needed: that instance acknowledges nothing until it has caught
up a new peer and named it in the record. On any other host, the peer can serve them the way a migration source serves
post-copy pages, which reuses the existing fault path.

If the peer is unreachable too, recovery must wait, or an operator forces it
and accepts the rewind to the checkpoint. That is an availability cost: the VM
now depends on two hosts to be recoverable without loss.

If the primary only restarted (process crash or reboot with the disk intact),
its own local log serves the same role and no other host is needed.

### 8. Migration and forks

- **Migration.** The source's stage stays valid until the destination has its
  own: the destination takes a stage cut of everything unpublished (forcing
  the pulls it needs) or a checkpoint lands, and only then does the record
  move the stage. Until then the source and peer keep theirs.
- **Fork.** A child inherits unpublished pages it has not staged. Its first
  flush must stage everything unpublished it maps, or wait for the parent's
  fork point to publish. Default: stage it.

### 9. What it does to the loss window

With a stage, a VM's unpublished writes are already durable on two nodes, so
an object-store outage no longer needs to freeze the guest at five minutes.
The bound becomes stage disk capacity and the dirty budget. A later step could
let the pager evict staged dirty pages to the stage log instead of the spill
file. I would leave the window as is in the first version.

## Costs and tradeoffs

- **Fsync latency** goes from about zero (fresh disks) to one cut plus
  max(local fdatasync, network RTT + peer fdatasync). On NVMe with power-loss
  protection in one zone, sub-millisecond to a few milliseconds.
- **Bytes**: every staged byte is written to two disks and sent once over the
  network, on top of the checkpoint upload. 2 MiB pages make this large
  without diffing.
- **Faults**: every first store to a page after a cut takes a write-protect
  fault.
- **Disk**: stage space is bounded by unpublished bytes, which during an object
  store outage keep growing. SSD endurance from WAL-heavy guests.
- **Availability**: recovery without loss needs the peer reachable. Placement
  must keep primary and peer in different failure domains.
- **Complexity**: a new fault kind in the pager, a replication protocol with
  fencing, a record format change, and new rules for migration and fork.

## Verification

- `spec/stage/Stage.tla` models the protocol: cuts, the peer, checkpoints and
  drops, peer replacement, recovery with fencing, one host lost, one power
  loss of every host, and a recovery while the old primary still runs. It
  found the three rules marked "found by `spec/stage`" above. Its invariant:
  every recovery starts from a state that holds every flush an earlier
  instance acknowledged. Eight mutants in `spec/stage/mutants` each put back
  one broken rule (acknowledging on one disk, acknowledging before the sync,
  no fence, dropping ahead of the checkpoint, switching peers early or stale,
  reading before fencing, recovering without the sync), and a whole search
  catches each one. The clean model is too large to search whole in a check,
  so `MCStage.cfg` runs 200,000 seeded random behaviours instead; that is
  evidence, not proof.
- A spec or extension of `spec/arena` for the cut against seal, settle,
  reclaim and copy-on-write.
- Simulation: a stage peer on the simulated network and disks with
  `PowerLossFaults`, kill either host at random points, and check every
  acknowledged flush against the byte model. Then the nightly soak.

## Open questions for Ramon

1. ~~**Two copies how?**~~ **Decided (Ramon, 2026-10-02):** primary plus
   exactly one peer, and both must acknowledge.
2. **Per VM or per deployment?** Default: per VM, in `MachineTerms`, like the
   checkpoint interval.
3. **Exact fsync?** With staging on, should every flush wait for its cut (flush
   bound zero)? Default: yes.
4. ~~**2 MiB pages.**~~ **Decided (Ramon, 2026-10-02):** whole pages,
   compressed; no diff in the first version, kept after seeing the 159x
   per-fsync measurement.
5. **Who picks the peer?** Default: the orchestrator, at create and open,
   written to the record.
6. **Loss window.** Keep it unchanged in the first version (default), or relax
   it for staged VMs?
7. **Does the LSVD branch matter here?** I don't think its object layout
   changes this design; the stage sits in front of whichever layout wins.

## Suggested first steps

1. ~~Spec the stage protocol~~ (done: `spec/stage`). The cut against seal,
   settle and copy-on-write is still to spec.
2. Prototype the cut in `vmmemory` and measure fsync latency and bytes per
   fsync on a WAL workload with 2 MiB pages.
3. Then the stager, the peer protocol and the record change.
