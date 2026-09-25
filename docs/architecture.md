# Architecture

Sproutfs moves virtual machines between hosts and forks them. It does not copy
their inherited disk and memory contents to do this. Disk and memory use the
same storage abstraction: a [volume](volumes.md), which is a byte-addressed
image with one writer.

The design rests on three decisions.

1. A VM's durable state is one published checkpoint, which its control record
   selects. Nothing is durable between checkpoints, so losing a host loses
   every write since the last checkpoint of each of its VMs. The only state a
   host keeps durable on its own schedule is a VM's disks: the interval
   checkpoints the disks but not RAM. A VM whose checkpoint has no VMM state
   restarts by booting over its disks.
2. Every page has one name: the checkpoint that published it. A fork inherits
   its parent's names. A named page is referenced and never copied, in the
   store, in host memory and on the wire. Two VMs share a resident page because
   they inherited the bytes of the same checkpoint. A page that no checkpoint
   holds reads as zeroes. The exception is the name of a
   [template](hosting.md), which is `template-<sha256 of the guest image file>`.
   A template is an imported image, not a VM that lives on a host. An image's
   identity is its bytes, so every host uses the same template name for the
   same image, and the hosts import it once between them. The name identifies
   only the template. The checkpoints under that identity belong to the
   template, as any VM's checkpoints belong to that VM. Forks of the template
   share pages by page identity.
3. A checkpoint has two steps: a pause and an upload. The pause stops the
   vCPUs, saves the VMM state, seals the dirty pages and resumes the vCPUs. Only
   the pause is on any latency path. A fork and a migration take the pause and
   publish nothing. The unpublished pages reach the other side through the
   pager, on the same host or over the network.

## Components

| Component | Role |
| --- | --- |
| Control record | One object per VM, at `control/<id>`. The `control/` namespace holds only control records, so listing it finds the deployment's VMs. The record holds the writer epoch, the writer nonce, the selected checkpoint sequence, whether that checkpoint is published, and the checkpoints of this VM that have been forked. Reclamation must spare those forked checkpoints, and nothing unpins them. A record exists while its VM exists, and not longer. Every change is a conditional write, and only the epoch holder writes. |
| [Checkpoint](volumes.md) | Its data and one index object under `vm/<id>/ckpt/<seq>/`. Nothing else is stored there. The data is `part/<n>`. A part is a run of members: the VMM state if one was saved, then the dirty pages in page order, then the pages compaction rescued. A part is filled to 64 MiB and closed by a table of at most 1 MiB and a fixed 32-byte trailer, so each part describes itself. Pages are published in page order, so a read of a range of a volume can fetch the members of consecutive pages as one **extent**, in one ranged read. A pager's cold 2 MiB read-ahead run of 512 4 KiB pages therefore takes two requests instead of one per page: one for the segment that locates the pages, and one for the extent that holds their members. The `index` object is written last. It contains a fixed record, the page-table segments this checkpoint changed, the **root**, and the fixed record again. Its create-if-absent PUT commits the publication. The root records each volume's geometry: its page size and the number of pages one segment covers (512 MiB of a 2 MiB-page volume, 64 MiB of a 4 KiB-page volume). For each segment of that volume's page table, the root holds the segment's address inside the index object of the checkpoint that wrote it. For a segment this checkpoint did not change, the root keeps the earlier checkpoint's address. The root lists every checkpoint it reads a page from or addresses a segment in. It also lists the checkpoints its own compaction emptied, which it spares for one checkpoint. The root does not name a parent. It is complete on its own: one GET of the end of the index object returns it, and a reader then fetches the segments that a range falls in. |
| Overlay | The in-memory writes of one open VM since its selected checkpoint. The next checkpoint publishes them. The overlay does not survive any failure. |
| [Pager](vm-memory.md) | The host's page cache for guest memory. It is a shared memfd arena that serves attached memory regions, keyed by page identity. It replaces the kernel's page cache and swap for guest memory ([why](vm-memory.md#why-a-pager-of-its-own)). A host runs one pager instance per kind of memory region: one for RAM and one for PMEM. Each instance has its own arena, spill file and page size. A memory region attaches to the pager of its kind. A pager uses the same page size for every memory region it serves, so it refuses to attach a volume published with any other page size. By default both pagers use 2 MiB pages over the node's HugeTLB pool. A deployment may instead run RAM at 4 KiB over an arena of ordinary memory, as the simulation does. In that mode, a guest's store into its memory copies and owns 4 KiB, while inherited RAM is still mapped in contiguous runs of up to 2 MiB, with one command per run. The mapping protocol carries each session's page size and its arena's kind. Both ends refuse a mismatch before any guest memory exists. |
| [Host](hosting.md) | `internal/host` contains everything one host does. Its `Host` opens VMs over object storage and the cluster network, checkpoints them on an interval, fences a VM that a later writer took, and serves pages for migrations and forks. The supervisor around it owns the two pagers and the VMM processes behind the host API. The supervisor also imports guest images into the templates that VMs are forked from, and reaches the agent in a guest. `cmd/sproutfs-host` holds the host's configuration, its HTTP handlers and the adapters it chooses. |
| Orchestrator | `cmd/sproutfs-orchestrator` allocates VM identities, places VMs on host pods, and drives migrations and forks between hosts. Its SQLite table has the states `creating`, `running`, `migrating`, `stopped` and `recovering`. The table is a view. The control records are the authority. |

## Loss model

A volume write applies to an in-memory overlay and returns. The write contacts
nothing, so it cannot fail because of the network or storage. It is also not
durable. The next checkpoint makes it durable: the pages upload, the last part
carries the root, and a conditional write selects the checkpoint in the control
record. If the host is lost before that, every write since the last selected
checkpoint is lost. This is accepted, because a guest write must never wait on
object-store latency.

`Status.DirtyBytes` is an upper bound on what a VM would lose at this moment.
The volume manager's `Stats` sums it over every VM the host runs.

The volume layer has no automatic trigger:

- `Checkpoint` and `Snapshot` publish on demand.
- `SnapshotDisks` publishes only a VM's disks.
- `Close` publishes a final checkpoint.
- `Handoff` and `ForkPoint` publish nothing.

The host owns the interval, because the host runs the guest and knows when the
vCPUs can be paused. The default is every 60 s
(`host.Config.CheckpointInterval`, `SPROUTFS_CHECKPOINT_INTERVAL`). Each wait
is jittered by up to an eighth in either direction, so VMs do not checkpoint in
lockstep. The next wait is measured from the end of the last upload.

The interval checkpoints only a VM's **disks**. Its pause stops the vCPUs and
seals the PMEM memory regions. It does not seal or upload RAM, and it does not capture
VMM state. The target workload is an agent sandbox. In a sandbox, the disk must
survive the loss of a host, and the software in the guest recovers its
in-memory state from the disk. This works because most software does not
assume that RAM outlives the machine. Uploading RAM every interval would cost
far more than it gains. All disks are sealed inside one pause, so the checkpoint
is one point in time across all of them.

A checkpoint of the disks names **no VMM state**. It does not name the previous
checkpoint's state either, although a checkpoint that captured no state would
otherwise keep naming it. The reason is that the previous registers combined
with the new disks would describe a guest that never existed. A VM opened at a
checkpoint without state is **cold booted**. The host takes a separate
checkpoint that discards the VM's memory (`Host.Starting`), then boots the
kernel over the VM's disks. For the guest, this is a power cut at that
checkpoint, and its filesystem recovers what its journal recovers. So after a
host loss, a VM whose last checkpoint came from the interval restarts cold at
that checkpoint.

RAM is uploaded only on request. An explicit capture (the host API's `capture`)
seals every memory region and captures the VMM state. A stop that asks to suspend
(`stop --suspend`) does the same, so the next start resumes the guest where it
stopped. A plain stop publishes only the disks, like the interval, and the next
start boots the guest. A migration and a fork upload nothing in either case,
because RAM moves from pager to pager.

The interval bounds what a host loss costs a VM's disks when everything works.
The **loss window** bounds it when nothing works. The loss window is how long a
VM may hold a disk write that no landed checkpoint covers:
`host.Config.LossWindow`, `SPROUTFS_LOSS_WINDOW`, five minutes by default, zero
to disable. While a VM's oldest unpublished write is older than the window, the
pager admits no further dirty page for that VM. Every store that needs a dirty
reservation waits, in the same way that a store past the dirty budget waits.
The host also requests a checkpoint of that VM outside the interval's schedule.
So the lost writes of one VM span at most the window plus the pause of one
checkpoint attempt, measured from the first lost write to the last. If the host
is lost after an outage longer than the window, writes older than the window
are still lost, because nothing can publish during an outage. But the guest has
been unable to build on those writes since the window expired. RAM is outside
the window. No interval checkpoint covers a RAM write, so RAM memory regions do not age
and do not request a checkpoint under dirty pressure. A RAM pager's dirty budget
must hold every private RAM page its guests create. A store past that budget
stops the VM, as with any full budget.

A store into a page that the guest has already dirtied, and that no seal covers,
does not fault and is not blocked. The checkpoint that the pager requests seals
every dirty page during its pause, so from that pause on, every store of that VM
waits. The gap runs from the moment the window expires to that seal, which is
at most one pause away.

A migration or a fork moves unpublished pages to another host, and their age
moves with them. For each memory region, the handoff carries the age of that memory region's
oldest unpublished write. The destination dates the pages it receives from that
age, on its own clock. So a destination inherits the window and does not
restart it. Some VMs can never be checkpointed: their checkpoint loop is off,
or their memory region belongs to no VM the host runs. A store waiting on the window
for such a VM would wait for an event that never happens. It ends the same way
a full dirty budget ends: the host deliberately stops that VM and takes a last
checkpoint of what it can still capture. A fork hold is different. The hold
ends at its deadline and the parent is checkpointed then, so a parent's stores
wait.

A failed publication changes nothing durable. The previous checkpoint stays
selected, the overlay keeps its bytes, and the failure is reported through the
VM's status and retried. While the VM is inside its window, the retry happens at
the next interval. While the VM is past its window, the retry happens after an
eighth of the interval, and the delay doubles up to the full interval. The
exception is a handle that a later writer has fenced. A fenced host discovers
the fence at the interval checkpoint, because a running VM writes nothing else.
The host then closes the VMM and releases the VM. It does this so that no guest
keeps running whose writes can never be published.

Guest CPU stores are not durability acknowledgements. A guest's flush (its
fsync reaching the virtio-pmem device) is a durability acknowledgement within a
bound. The flush reaches the pager. The host completes it immediately if the VM
holds no unpublished disk write older than `host.Config.FlushBound`
(`SPROUTFS_FLUSH_BOUND`, twice the checkpoint interval by default, so 120 s,
zero to disable). Otherwise the flush
waits until a checkpoint covers those writes, and the host requests that
checkpoint outside the interval's schedule. A flush never takes a checkpoint
when the disks are fresh. As a result:

- When a flush returns, nothing older than the bound is unpublished.
- The data it flushed is durable within the bound plus one interval.
- A guest whose disks cannot be published stops making fsync progress. It is
  not told that its disks are durable.

The ordering is the same as after a power cut. A checkpoint is one point in time
across the VM's disks, so nothing written after a flush is durable unless
everything written before it is durable too. Local scratch spill is not durable
storage.

Published checkpoint objects survive host loss, assuming that the shared object
store stays durable. A configured host can open a VM by identity once the
checkpoint its record selects is published and object storage is reachable. A
fork that has not published its own root index can run only on the host that
took it in. Resuming a VMM also needs its captured state and a compatible
runtime configuration.

## VM lifecycle

1. The orchestrator allocates a VM identity and chooses a host.
2. Creation publishes a first checkpoint that contains only the root, and then
   creates the control record that selects it. Opening reads that record and
   advances its epoch, which fences the previous writer. Opening then reads the
   selected checkpoint's root. The overlays start empty, so there is nothing to
   replay.
3. The pager attaches the VM's `ram0` and PMEM volumes. Before the vCPUs run,
   it populates the pages whose identity is already resident in the same pager.
   Missing pages load on demand.
4. Volume writes apply to the overlay and return. The interval checkpoint
   pauses the guest, seals every dirty page of its disks by write protection,
   and resumes the guest. The sealed pages stream out as parts while the guest
   runs. The last part carries the root, and the control record selects it.
5. A fork takes the same pause and publishes nothing. A fork is a handoff on
   any host the child lands on. One `ForkPoint` serves any number of children.
   The parent pins its last published sequence once and permanently, and keeps
   its handle. Each child gets a control record that selects a root over that
   sequence. The child's location changes only how it receives the pages that
   the parent holds and no checkpoint has:
   - On the parent's own host, the child maps the sealed pages through the
     pager.
   - With `--to <host>`, the child's pager pulls them from the parent's page
     server, as a migration destination does.

   The destination publishes the child's root as soon as it holds all of those
   pages. After that, any host can open the child. A fork that ends before then
   leaves no object behind.
6. A live move is post-copy only. The source stops the guest, saves VMM state
   and hands the VM over without uploading anything. The destination opens the
   same identity, advances the epoch, and resumes from the supplied state. It
   faults in the pages the source holds from the source's page server, and the
   bulk stream runs while the guest runs. The destination's next checkpoint
   makes those pages durable. The source is released once every unpublished
   page has reached the destination. A page that no checkpoint holds exists
   only on the source, so the destination keeps asking for it until it
   arrives. The destination cannot tell a source that stalled from one that
   died. If it read the page from its own volume instead, it would roll the
   guest back past the guest's own write. The asking ends in one of two ways:
   the source answers that it no longer serves the VM, or the orchestrator ends
   the migration. The orchestrator ends it when it has lost the source host. In
   that case the pages are lost with the host, and the VM is recovered from its
   checkpoint, without the writes made since. See [migration](migration.md) for
   failure handling.

## Identities and reclamation

The orchestrator must supply VM identities that are never reused. Sproutfs has
no global VM identity registry and no limit on the number of VM identities used
over time. Syntax and local resource limits still apply. Every checkpoint object
is written under its publisher's VM identity. A fork's root keeps naming its
parent's checkpoints, which is why a fork copies nothing. A VM's control record
is stored outside that namespace, at `control/<id>`, because the two namespaces
hold different sets:

- `control/` holds the VMs that currently exist. A record is present while its
  VM exists, and is removed with it.
- `vm/` holds every identity that ever left objects behind. A deleted VM that
  was ever forked leaves its pinned checkpoints there, with no record, for a
  collector that does not exist. So `vm/` only grows. A listing of it, by
  delimiter or otherwise, would return deleted VMs along with live ones, and
  each entry would have to be probed for a record.

Listing `control/` returns only the deployment's VMs. The split also keeps two
rules short:

- A create is refused if `vm/<id>/` holds anything that no record accounts for.
- A delete removes the record first, and then sweeps the prefix the record
  governed.

Selecting a checkpoint reclaims a set difference: the checkpoints the replaced
root named, plus the replaced checkpoint, minus everything the new root names
and everything a pin protects. A dead checkpoint is deleted whole, starting
with its last part. Reclamation deletes only this VM's own checkpoints. It never
touches another VM's checkpoints. A pinned sequence is spared together with
every checkpoint its root names. So everything a fork inherits survives,
including the parts that a grandchild reads directly. A pin is written before
the child that holds it exists, and the pin is permanent. No component in a
deployment can establish that no descendant reads through a checkpoint, because
a descendant sees neither its siblings nor the forks taken below it. So only a
collector may release a pin.

Without compaction, cold pages would keep mostly dead parts alive. So each
checkpoint also compacts. If a checkpoint of this VM has less than half its
bytes still live, it is rewritten into the checkpoint being published, up to
64 MiB of live bytes at a time, after the guest has resumed. The emptied
checkpoint then drops out of the root, and reclamation deletes it.

A handle reclaims only the checkpoints it published. The checkpoint it opened on
is left for a collector. Deleting a VM first removes its control record, which
makes the identity reusable. The delete then sweeps that VM's own checkpoints,
except the pinned ones. A descendant may still read the objects of the pinned
checkpoints, so those are left for a collector. If a record cannot be parsed,
the delete is refused, because the pins that say what to spare cannot be read.
That collector would be rooted in the orchestrator's live set. It is
deliberately deferred indefinitely. Until it exists, the store grows without
bound, and everything a pin covers is kept permanently. When the collector is
written, it must protect in-flight publications and forks.

## Current state

Control records, checkpoint storage with reclamation and compaction, volumes,
the pager, capture, fork by handoff, post-copy migration, the host and the
orchestrator are implemented and covered by simulation tests. Recorded
Firecracker integration qualification covers Linux aarch64 with real KVM in the
[documented test environment](vm-memory.md#qualification). The pager's 2 MiB
HugeTLB qualification is recorded on x86_64. Other environments, and production
workload and scale acceptance, remain unqualified. Larger-guest measurements
exist, but the recorded workload run is partial and is not performance
acceptance. No production deployment is recorded in this repository. Two things
are not implemented:

- collection, which releases pins and sweeps what deleted VMs left pinned;
- compatibility and migration paths for legacy data.

Before any deployment's stored data formats change, the compatibility
requirements for that data must be assessed.

Reliability work uses seeded workloads and simulated dependencies to exercise
the durability guarantees above. Seeds reproduce dependency choices, not
arbitrary Go scheduler interleavings. Explicit gates make selected races
repeatable, and race detection checks shared-memory access separately. See
[testing](testing.md) for the scope and limits of this evidence.
