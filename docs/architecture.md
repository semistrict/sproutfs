# Architecture

Sproutfs runs virtual machines whose disks and memory are stored in object
storage. It can move a running VM to another host, and fork it, without
copying the disk and memory the VM already has. Disks and RAM are both
[volumes](volumes.md): fixed-size images with one writer.

## What is durable

A VM is durable up to its last published checkpoint. Its control record says
which checkpoint that is. Anything written after it is lost if the host dies,
except, in the optional [durable flush](#durable-flush) mode, disk writes a
guest flushed.

The host checkpoints a VM's disks on an interval. RAM is saved only when asked:
a capture, a suspend, or a stop that keeps it. A VM whose checkpoint has no RAM
boots from its disks.

## How pages are shared

A page is named by the checkpoint that published it. A fork starts with its
parent's pages and their names. VMs that have a page with the same name share
it: in the store, in a host's memory and on the network. A page that no
checkpoint has written reads as zeros.

A template's VM is named by its image instead:
`template-<sha256 of the guest image file>`, so the hosts import each image
once between them (see [hosting](hosting.md)). VMs of any tenant can be created
from a public template and share its pages. Nothing else is shared between
tenants.

## What a checkpoint costs the guest

A checkpoint pauses the guest briefly. The host stops the vCPUs, saves the VMM
state if RAM is included, write-protects the dirty pages and resumes. That pause
is the only time the guest waits.

The upload runs after the guest resumes, but it still uses the host's CPU, disk
and network, and slows the guest's page faults. On GCE, filling the cluster
cache with no rate limit raised the slowest 1% of faults on the publishing host
from 27 to 45 ms. The fill rate limit bounds this
([measurements](measurements/gce-fill-defaults-2026-10-06.md)).

A fork or a migration pauses the guest but does not wait for an upload. The new
VM gets the pages that are not published yet from the old one, through the
pager. They are published afterwards: by the parent for a fork, and by the
destination's next checkpoint for a migration.

## Components

| Component | Role |
| --- | --- |
| Control record | One object per VM, at `control/<id>`. It holds the writer epoch, the writer nonce, the selected checkpoint sequence, whether that checkpoint is published, the pins (checkpoints of this VM that were forked), the kept checkpoints and the journals that may hold flushed writes newer than the checkpoint. Every change is a conditional write ([metadata](metadata.md#the-control-record)). |
| [Checkpoint](volumes.md#objects) | An index object and its parts under `vm/<id>/ckpt/<seq>/`. A part, `part/<n>`, holds the VMM state if one was saved, the dirty pages in page order, and the pages compaction rescued. It is filled to 64 MiB and ends with a table of at most 1 MiB and a 32-byte trailer. The `index` object holds a fixed record, the page-table segments this checkpoint changed, the root, and the fixed record again. Its create-if-absent PUT commits the publication. The root records each volume's geometry and, for each segment, its address in the index object of the checkpoint that wrote it. It names no parent. |
| Overlay | The in-memory writes of one open VM since its selected checkpoint. The next checkpoint publishes them. The overlay does not survive any failure. |
| [Pager](vm-memory.md) | The host's page cache for guest memory: an arena of memfds keyed by page identity, and a spill file. It replaces the kernel's page cache and swap for guest memory ([why](vm-memory.md#why-a-pager-of-its-own)). A host runs one pager per kind of memory region, each with its own arena, spill file and page size. Both default to 2 MiB pages over the node's HugeTLB pool; a deployment may run RAM at 4 KiB over ordinary memory, as the simulation does. A pager refuses a volume of another page size. In the isolated mode each memory region has a private file ([the isolated arena](vm-memory.md#the-isolated-arena)). |
| [Host](hosting.md) | `host.Host` opens VMs over object storage and the cluster network, checkpoints them on an interval, fences a VM that a later writer took, and serves pages for migrations and forks. The supervisor around it owns the pagers, drives the VMM processes (each started by a `vmmachine.Starter`, which an embedder may supply), imports guest images into templates, and reaches the agent in a guest. `cmd/sproutfs-host` holds the configuration, the HTTP handlers and the adapters. |
| Orchestrator | `cmd/sproutfs-orchestrator` allocates VM identities, places VMs on host pods, and drives migrations and forks. Its SQLite table, with the states `creating`, `running`, `migrating`, `stopped` and `recovering`, is a view. The control records are the authority. |
| [Membership](hosting.md#the-membership) | One object, at `membership`: the hosts in the cluster, the cache disk each serves, and the deployment's code. It changes only by compare-and-set, one generation per change. A request between hosts names its sender's generation, so two hosts never exchange a stripe under different memberships. |
| [Shard](hosting.md#shards-on-network-disks) | One network disk of a fixed set that may hold the cluster cache instead of the hosts' own disks. Windows are ranked over shards, so scaling compute moves no window. The membership assigns each shard to a host, and the orchestrator attaches it through the cloud's attach API. |
| [Journal disk](hosting.md#journal-disks) | In the durable flush mode, one network disk per machine of the host pool. It holds the host's journal: the changed blocks its guests flushed since their last checkpoints. The orchestrator creates, attaches and deletes these disks, and the membership assigns each to a host. |

## Loss model

A volume write applies to an in-memory overlay and returns. It contacts
nothing, so it cannot fail on the network or storage, and it is not durable.
The next checkpoint makes it durable: the pages upload, and a conditional write
selects the checkpoint in the control record. If the host is lost before that,
every write since the last selected checkpoint is lost. A guest write never
waits on object-store latency.

`Status.DirtyBytes` is an upper bound on what a VM would lose now. The volume
manager's `Stats` sums it over the host's VMs.

An [ephemeral disk](volumes.md#ephemeral-disks) is outside this model. No
checkpoint holds it, its writes count toward neither the dirty budget nor the
loss window, and its pages live in a third pager.

The volume layer has no automatic trigger:

- `Checkpoint` and `Snapshot` publish on demand.
- `SnapshotDisks` publishes only a VM's disks.
- `Close` publishes a final checkpoint.
- `Handoff` and `ForkPoint` publish nothing.

The host owns the interval, because it knows when the vCPUs can be paused. The
default is 60 s (`host.Config.CheckpointInterval`,
`SPROUTFS_CHECKPOINT_INTERVAL`). Each wait is jittered by up to an eighth either
way and measured from the end of the last upload.

The interval checkpoints only a VM's disks. Its pause stops the vCPUs and seals
the PMEM memory regions. It does not seal or upload RAM, or capture VMM state.
The target workload is an agent sandbox: its disk must survive a host loss, and
its software recovers in-memory state from the disk. All disks are sealed in
one pause, so the checkpoint is one point in time across them.

A disk checkpoint names no VMM state, not even the previous checkpoint's,
because old registers over new disks would describe a guest that never
existed. A VM opened at a checkpoint without state is cold booted: the host
takes a checkpoint that discards its memory (`Host.Starting`) and boots the
kernel over its disks. The guest sees a power cut at that checkpoint. After a
host loss, a VM whose last checkpoint came from the interval restarts cold.

RAM is uploaded only on request. A capture (the host API's `capture`) and a
`stop --suspend` seal every memory region and capture the VMM state, so the
next start resumes the guest. A plain stop publishes only the disks. A
migration and a fork upload nothing; RAM moves from pager to pager.

The interval bounds what a host loss costs a VM's disks while publications
succeed. The **loss window** bounds it when they fail: how long a VM may hold a
disk write that no landed checkpoint covers (`host.Config.LossWindow`,
`SPROUTFS_LOSS_WINDOW`, five minutes by default, zero to disable). While a VM's
oldest unpublished write is older than the window, the pager admits no further
dirty page for it, as for a store past the dirty budget, and the host requests
a checkpoint out of turn ([the checkpoint loop](hosting.md#the-checkpoint-loop)).
A VM's lost writes then span at most the window plus one checkpoint pause.
After an outage longer than the window, older writes are still lost, but the
guest could not build on them past the window. RAM is outside the window: RAM
memory regions do not age, and a RAM pager's dirty budget must hold every
private RAM page its guests create. A store past that budget stops the VM.

A store into a page the guest already dirtied, and that no seal covers, does
not fault and is not held. The requested checkpoint seals every dirty page in
its pause, so this gap is at most one pause.

A handoff carries the age of each memory region's oldest unpublished write,
and the destination dates the pages it receives from it on its own clock, so it
inherits the window. A VM that can never be checkpointed (its loop is off, or
its memory region belongs to no VM the host runs) is stopped by the host with a
last checkpoint of what it can capture, as for a full dirty budget. A fork hold
ends at its deadline and the parent is checkpointed then, so a parent's stores
wait.

A failed publication changes nothing durable. The previous checkpoint stays
selected, the overlay keeps its bytes, and the failure is reported in the VM's
status and retried: at the next interval inside the window, and past it after an
eighth of the interval, doubling up to the interval. A fenced host discovers
the fence at its interval checkpoint, then closes the VMM and releases the VM,
so no guest runs whose writes can never be published.

A guest CPU store is not a durability acknowledgement. A guest flush (its fsync
reaching the virtio-pmem device) is one. With [durable flush](#durable-flush)
off, the default, it holds only within a bound. The host completes it
at once if the VM holds no unpublished disk write older than
`host.Config.FlushBound` (`SPROUTFS_FLUSH_BOUND`, twice the interval, 120 s, by
default; zero to disable). Otherwise the flush waits for a checkpoint that
covers those writes, which the host requests out of turn. A flush of fresh
disks takes no checkpoint. So:

- When a flush returns, nothing older than the bound is unpublished.
- The data it flushed is durable within the bound plus one interval.
- A guest whose disks cannot be published stops making fsync progress.

A checkpoint is one point in time across the VM's disks, so nothing written
after a flush is durable unless everything before it is, as after a power cut.
Local scratch spill is not durable storage.

Published checkpoint objects survive host loss while the object store is
durable. A configured host can open a VM once the checkpoint its record selects
is published and the store is reachable. A fork that has not published its root
runs only on the host that took it in. Resuming a VMM also needs its captured
state and a compatible runtime configuration.

### Durable flush

Durable flush is an optional mode, off by default. One setting,
`SPROUTFS_DURABLE_FLUSH`, turns it on for the hosts and the orchestrator; its
value names the cloud of the journal disks, `gce`. Off, the flush bound
answers a flush as above, and no control record names a journal.

On, a flush of a disk returns success only once the blocks the guest changed
before it are on the host's journal disk, a network disk the cloud replicates.
The guarantee: **when a flush returns success, every store the guest made to
that disk before the flush survives the loss of its host.** It does not survive
the loss of the zone, because the disks are zonal. A flush that cannot be
journaled fails, and the guest reads an I/O error. There is no fallback to a
checkpoint, and the flush bound does not apply.

A flush:

1. captures the disk's changed 4 KiB blocks: those of its unjournaled pages
   whose SHA-256 differs from what the journal holds
   ([the capture](vm-memory.md#capturing-for-a-durable-flush));
2. joins the next batch of the journal, which writes and syncs the flushes that
   arrived while the batch before was in flight;
3. is answered once its batch has synced. It waits for nothing in the object
   store.

A flush is journaled only once the VM's control record names this host's
journal at the VM's epoch, because a recovery reads only the journals the
record names. So the first flush after an open asks for a checkpoint out of
turn and waits for its selection.

A checkpoint still makes everything durable, and it trims the journal. Its
selection names the host's journal and the **covered position**, the last
position whose entries the checkpoint holds. A stop's and a close's checkpoint
hold every store and name no journal. A full journal is back-pressure, not a
failure: the host asks for checkpoints out of turn, and flushes wait for room.
In the mode, the interval and the loss window bound what a host loss costs
**unflushed** writes. Flushed writes are not lost.

After a host loss, the membership assigns the lost host's journal disk to
another host, which reads it back ([hosting](hosting.md#journal-disks)). An
open that is not a migration's reads the VM's entries from every journal its
record names, after each covered position, and writes their blocks into the
VM's disks before anything runs it. The host that holds the disk fences the VM
at the reader's epoch before it answers, so a host that still runs the VM
answers no later flush. A VM anything was replayed into is cold booted,
whatever VMM state its checkpoint holds, and its cold boot's checkpoint
publishes the replayed blocks.

RAM, ephemeral disks and VMs with no checkpoint loop are not journaled. A
flush of an ephemeral disk or of such a VM completes at once, as with the mode
off.

## VM lifecycle

1. The orchestrator allocates a VM identity and chooses a host.
2. Creation publishes a first checkpoint that holds only the root, then creates
   the control record that selects it. Opening reads the record, advances its
   epoch, which fences the previous writer, and reads the selected root. The
   overlays start empty, so there is nothing to replay.
3. The pager attaches the VM's `ram0` and PMEM volumes. Before the vCPUs run,
   it populates the pages whose identity is already resident in that pager.
   Missing pages load on demand.
4. Volume writes apply to the overlay. The interval checkpoint pauses the
   guest, seals its dirty disk pages by write protection and resumes it. The
   sealed pages stream out as parts while the guest runs, and the control
   record then selects the checkpoint.
5. A fork takes the same pause, publishes nothing before it returns, and is a
   handoff on whichever host the child lands. One `ForkPoint` serves any number
   of children. The parent pins its last published sequence permanently and
   keeps its handle. Behind the fork, the parent publishes the point once as its
   own checkpoint, and a child on its host builds its first checkpoint
   on it, so a fan-out uploads the parent's unpublished pages once. Each child
   gets a control record that selects a root over the pinned sequence. On the
   parent's host the child maps the sealed pages through the pager. With
   `--to <host>`, its pager pulls them from the parent's peer server. The
   child's host publishes the child's root behind the running child once it
   holds those pages. Until then the child exists only on that host, and a fork
   that ends earlier leaves no object behind.
6. A live move is post-copy only. The source stops the guest, saves VMM state
   and hands the VM over without uploading anything. The destination opens the
   same identity, advances the epoch, resumes from the state, and faults in the
   source's pages from its peer server while a bulk stream runs. Its next
   checkpoint makes those pages durable. The source is released once every
   unpublished page has reached the destination. Until then the destination
   keeps asking for a page only the source holds: it cannot tell a stalled
   source from a dead one, and reading its own volume would roll the guest
   back. The asking ends when the source answers that it no longer serves the
   VM, or when the orchestrator ends the migration on evidence that the source
   no longer has the pages (its pod is not listed, it answers without them, or
   its hold is over). The VM is then recovered from its checkpoint without the
   writes since. A receive that fails for any other reason is tried again while
   the source holds the pages. See [migration](migration.md).

## Identities and reclamation

The orchestrator must supply VM identities that are never reused. There is no
global identity registry and no limit on identities used over time. Every
checkpoint object is written under its publisher's identity, and a fork's root
names its parent's checkpoints, so a fork copies nothing. Control records are
at `control/<id>`, outside `vm/`, because the two hold different sets:

- `control/` holds the VMs that exist now. A record is removed with its VM.
- `vm/` holds every identity that ever left objects. A deleted VM that was
  forked leaves its pinned checkpoints there with no record, so `vm/` only
  grows, and a listing of it would return deleted VMs.

So listing `control/` returns the deployment's VMs. A create is refused if
`vm/<id>/` holds anything no record accounts for. A delete removes the record
first, then sweeps the prefix the record governed.

Selecting a checkpoint reclaims the checkpoints the replaced root named, plus
the replaced checkpoint, minus everything the new root names and everything a
pin or a keep protects ([reclamation](volumes.md#reclamation)). Releasing a
kept checkpoint that no VM was created from sweeps what only it held
([metadata](metadata.md#kept-checkpoints)). A dead checkpoint is deleted whole.
Reclamation deletes only this VM's own checkpoints. A pinned sequence is spared
with every checkpoint its root names, so everything a fork inherits survives,
including parts a grandchild reads directly. A pin is written before the child
exists and is permanent. No component can establish that no descendant reads
through a checkpoint, because a descendant sees neither its siblings nor the
forks taken below it. Only a collector may release a pin.

Each checkpoint also compacts. A checkpoint of this VM with less than half its
bytes live is rewritten into the one being published, up to 64 MiB of live
bytes at a time, after the guest resumes. The emptied checkpoint then drops out
of the root and is reclaimed.

A handle reclaims only the checkpoints it published. The one it opened on is
left for a collector. Deleting a VM removes its control record, which makes the
identity reusable, then sweeps its own checkpoints except the pinned ones. A
record that cannot be parsed is not deleted. The collector would be rooted in
the orchestrator's live set. It is deferred indefinitely, and until it exists
the store grows without bound. It must protect in-flight publications and
forks.

## Current state

Control records, checkpoint storage with reclamation and compaction, volumes,
the pager, capture, fork by handoff, post-copy migration, the host and the
orchestrator are implemented and covered by simulation tests. Firecracker
integration is qualified on Linux aarch64 with real KVM in the
[documented test environment](vm-memory.md#qualification), and the pager's
2 MiB HugeTLB mode on x86_64. Other environments, and production workload and
scale, are not qualified. The recorded workload run is partial. No production
deployment is recorded. Durable flush is implemented, covered by package
tests, TLA+ models and a simulation campaign that kills hosts after flushes,
and measured on GCE (docs/measurements/gce-fsync-journal-2026-10-07.md). Not
implemented:

- collection, which releases pins and sweeps what deleted VMs left pinned;
- compatibility and migration paths for legacy data.

Assess the compatibility requirements of a deployment's stored data before its
formats change.

Seeded workloads and simulated dependencies exercise the durability guarantees
above. Seeds reproduce dependency choices, not Go scheduler interleavings.
Explicit gates make selected races repeatable, and the race detector checks
shared memory separately. See [testing](testing.md).
