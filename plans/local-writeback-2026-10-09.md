# A user pager with a local disk — 2026-10-09

The owner has decided that the pager should work the way the system it was
ported from works: Zircon's page layer, which we have, under a user pager that
writes dirty pages back to a local disk, which we do not have. Checkpoints to
object storage become the tier behind that disk. This plan says why, what the
design is, what changes and in what steps. It changes no code. The work is
TASK-122 and its subtasks.

## Why

An embedder's PostgreSQL benchmark (TASK-122, `scripts/demo-gce.sh postgres`)
runs a guest whose data is larger than its memory: 12 GiB written and read at
random, then pgbench over 4.5 GB of tables. On the same GCE node, held to the
guest's 8 GiB and 4 CPUs, plain Linux did 6,614 tps and 23,825 random 8 KiB
reads a second. Sproutfs did 2,290 tps and about 80 reads a second, and its
guest stopped answering for up to 25 s while a checkpoint uploaded.

Every one of those costs comes from one fact: a page is clean only once a
checkpoint has published it to object storage.

- A dirty page can leave memory only into the spill file, and the spill holds
  only what the dirty budget allows. When the budget is full, every store
  waits for a whole checkpoint upload.
- An evicted page that a checkpoint published has no local copy, so it is
  read back from the store, 2 MiB at 40 to 80 ms.
- A store after a seal copies the page, because the checkpoint holds the
  page's bytes in memory until it lands.
- Eviction spills a dirty page inside the fault that needs its slot.

## What Zircon does, and what we ported

Zircon splits paging in two. The kernel caches pages, tracks which are dirty
and runs the writeback protocol: a page a user pager backs is Clean, Dirty or
AwaitingClean (`vm/vm_cow_pages.cc`), and the evictor takes only clean pages of
such an object and reads them again from the pager. The user pager, in Fuchsia
a filesystem such as Fxfs, decides where the bytes live: it supplies pages from
its disk and writes dirty pages back to it when asked.

We ported the kernel half (`plans/zircon-pager-port-2026-10-05.md`,
`vmmemory/internal/zirconvm`). Our user pager writes back only to object
storage, through checkpoints, so Zircon's writeback is our seal and our
publication, and Clean means published. The spill file stands in for the
writeback Zircon assumes: it is compression to disk (`spillstorage.go`, after
`slot_page_storage.cc`), which Zircon itself has only in memory.

So the missing piece is the user pager's disk. With it, the ported machinery
works as it does in Fuchsia: dirty pages are written back locally and become
clean, the evictor drops clean pages, and a miss is supplied from the disk.

This is also the shape of LSVD (Hajkazemi et al., "Beating the I/O Bottleneck:
A Case for Log-Structured Virtual Disks", EuroSys '22): a local SSD log as the
write-back cache of a virtual disk whose durable home is object storage.

## The design

**Three tiers.** The arena holds the pages a guest is using. Under it, each
host keeps a log on its local disk, the node's local SSD where it has one: the
working copy of every page its memory regions have written or read. Under that,
object storage holds checkpoints. A page is in the arena, in the log, or only
in the store.

**The log is slots, not an append log** (decided 2026-10-09, before step 1).
Every version the log keeps is exactly one page of its pager, so any free slot
of the file takes any version and nothing is ever compacted: no segments, no
collection, no write amplification, and no headroom past the slots promised.
The spill file already is such a file. It grows into the log: a slot holds a
private version, which its reservation names and which must stay until it is
published or freed, or a published version, which its identity names and
which may be dropped whenever a slot is wanted. Publication relabels a slot
and moves no byte. Writes stay runs of consecutive slots where the free slots
allow, as the spill's writes are now. What an append log would add is
sequential writes and replay after a restart; a local SSD's own translation
layer makes the first moot, and the log is scratch, so the second is a later
step that does not need its layout. "Log" below means this file.

**Three meanings of clean.** A page is Dirty when the arena holds bytes the log
does not. It is Written when the log holds its bytes: the arena may drop it.
It is Published when a landed checkpoint holds the version the log holds.
Zircon's AwaitingClean is a page being written to the log.

**Writeback.** A writer moves Dirty pages to the log behind the guest, in runs
appended to the log's open segment, oldest first, before the evictor needs
their slots. A page written back is write-protected again, so the guest's next
store traps and makes it Dirty, as a filesystem's page does after writeback.
Eviction takes Written pages and drops them. A Dirty page the evictor reaches
waits for the writer; writing it from inside a fault is the fallback, not the
path.

**Supply.** A miss reads the page from the log where it is there, and from the
page cache, the cluster or the store as today where it is not.

**Checkpoints from the log.** A seal pauses the guest, write-protects what is
still Dirty and lets the writer write it. The checkpoint is then a list of log
versions, one per page changed since the last one. The guest runs on: a store
into a page whose version the checkpoint holds makes the page Dirty again and
writes a new version, so it copies nothing in memory. The upload reads the
versions from the log, sequentially, paced behind the guest's I/O, and the
guest never waits for it. A version stays in the log until a newer one is
published or the page is freed.

**The budgets.** The dirty budget becomes small: it bounds pages the writer has
not reached, in memory. What bounds unpublished data is the log's capacity,
hundreds of gigabytes on a local SSD, and the loss window, which bounds it in
time as now. Stores wait only when the log is full, which is a host short of
disk, not a guest writing faster than the network.

**What a lost log costs.** The log is scratch, like the spill file now: a host
that loses its disk or its process loses the log, and its VMs are what their
last checkpoints hold. Durability is unchanged. Reading the log back after a
process restart is a later step.

**Migration and forks.** A destination has no log of the VM. It reads pages
from the source and the store as today, and its own log fills as it runs. A
fork's child reads its parent's pages as today; what it writes goes to its own
host's log.

**The disk cache.** The log holds this host's own pages, published or not, so
a running VM's refaults never reach it. The disk cache keeps its role for pages
of other VMs: templates, forks, restores. The two may become one later, since
both are local copies keyed by page identity.

## What changes

- `zirconvm.SpillStorage` becomes the log: its slots hold private versions
  under reservations, as now, and published versions under their identities,
  each with its CRC32C, the published ones dropped oldest first when a slot is
  wanted. It keeps what the spill file guarantees: space it promises is
  allocated before it is promised.
- The dirty states gain Written beside Clean. A writeback to the log is
  Zircon's writeback; a publication changes no dirty state, it marks versions
  published.
- A writer goroutine, and the evictor's asynchronous path with a free-slot
  target (`zirconvm.Evictor.EvictAsynchronous`, never called today).
- `volume.DirtySource` reads sealed pages from the log, not from the arena.
- The host's configuration: the log's directory and size, its own write
  budget, and the disk limiter's share for it.
- Revocations and write-protections batched per run, because writeback adds a
  write-protect fault per page per cycle, and at 4 KiB that is the fault
  path's cost (TASK-122.7).

What stays: the seal's pause, the publication protocol, the loss window, the
page queues, the harvest, the identity roots and the sharing index.

## Steps

Each step lands with a simulation campaign or test that fails before it, guards
for the bugs it closes, and the benchmark beside plain GCE.

1. **Keep what is written.** The spill file keeps a published version: a
   checkpoint's page whose bytes are in its reservation when the checkpoint
   retires is relabelled under its identity instead of freed, and a load
   reads a version the log holds before the store. A slot's bytes count as a
   version only while no store can have changed the page since they were
   written, so a refault that hands the guest its page writable drops them.
   Gate: every suite and campaign; a test publishes a spilled page, evicts it
   and refaults it from the log (TASK-122.8).
2. **Keep what is evicted.** Eviction of a published page the log does not
   hold writes it to a free slot, and drops it; a refault reads the log
   before the store (absorbs TASK-122.2). The file's slots stop being the
   dirty budget: the pager admits reservations against its dirty budget, the
   file has that many slots plus room for versions, and a version write takes
   only a slot no reservation can be refused for, so the dirty budget is
   never short of a slot a version holds. Gate: the benchmark's random reads
   come from local disk.
3. **Write back.** The writer, Written pages, the asynchronous evictor.
   Gate: no eviction inside a fault on the benchmark's write phases.
4. **Checkpoints from the log.** Seal by log version, upload from the log, the
   small dirty budget (absorbs TASK-122.3). Gate: no probe over 1 s; copies
   after a seal near zero; pgbench up.
5. **Batch the page-table commands.** Revocations and write-protections per
   run (TASK-122.7).
6. **The root at 4 KiB.** Measure it with steps 1 to 5 in place, and make
   read-ahead adaptive (TASK-110). Gate: random reads within 2× of plain.
7. **The arena's split** for DAX guests (TASK-122.6).

## Open questions

- How large a log a host keeps, and how a VM is admitted against it.
- The log's write budget on local SSD: its endurance against a guest's
  write rate.
- Whether the log replaces the disk cache for this host's own pages at once,
  or after step 2 has been measured.
