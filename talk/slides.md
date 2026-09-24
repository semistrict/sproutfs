---
theme: default
title: Sproutfs
info: |
  Storage and managed memory for virtual machines that move between hosts
  and fork without copying their inherited disk and memory data.
colorSchema: dark
highlighter: shiki
lineNumbers: false
drawings:
  persist: false
transition: fade
mdc: true
---

# Sproutfs

VMs that move between hosts and fork without copying what they inherited

<div class="mt-12 text-lg opacity-70">
whole-VM checkpoints in object storage · a shared memory pager · a Firecracker integration
</div>

<div class="abs-br m-6 text-sm opacity-50">
github.com/semistrict/sproutfs · Apache-2.0
</div>

---
clicks: 2
---

# The problem

<ProblemFigure />

<!--
A fleet of hosts and an object store. You want many VMs that are mostly the same: every one started from one image, many forked from a running parent, any of them able to move to another host, all durable somewhere other than the host they run on.

What a VM has of its own is its differences. Everything else it inherited: from the image, from the parent it was forked from, from its own past checkpoints.

Done the usual way, every one of those operations is a copy of the whole VM: a snapshot writes all of its memory out and a restore reads it all back; a live migration streams memory while it changes; a fork is a snapshot and a restore. The cost is the VM's size, and almost all of what is copied is inherited bytes that nobody wrote.
-->

---

# The requirement

<div class="text-2xl mt-6 p-5 border border-yellow-600 rounded">
Cost what the VM <b>changed</b>. Never what it inherited. Never its size.
</div>

<v-clicks>

<div class="mt-10 text-xl space-y-4">

- inherited data **shared, not copied** — store, host memory, wire
- durability is a **pause of milliseconds**, not an upload
- a host can be **lost**; the loss is **bounded**

</div>

</v-clicks>

<!--
Three things have to be true at once. Inherited data is shared rather than copied in the object store, in host memory, and on the way between hosts. Making a VM durable pauses it for milliseconds, not for the length of an upload. And a VM's durable state is somewhere every host can reach, so a host can be lost or drained, and what its loss costs is bounded: the writes of one loss window, no more.
-->

---

# The words

<div class="grid grid-cols-2 gap-x-12 gap-y-3 text-lg mt-4">
<div>

**VM** — one identity, one series of checkpoints

**Volume** — `ram0` or a PMEM disk; fixed size; one writer

**Page** — 2 MiB; the unit of everything (RAM may run 4 KiB)

**Resident** — a page in host memory now

**Pager** — owns every resident page; serves the faults

</div>
<div>

**Checkpoint** — makes a VM durable; numbered by *sequence*

**Control record** — who may write, which checkpoint is current

**Page identity** — a page's name: the checkpoint that published it

**Fork** / **migration** — new VM at a pause / same VM, moved

**Host** / **orchestrator** — runs VMs / places and moves them

</div>
</div>

<!--
VM: one machine, one identity, one series of checkpoints. Volume: one byte-addressed image of a VM, its RAM or one of its PMEM disks, fixed size, one writer at a time. Page: 2 MiB of a volume, the unit of what is stored, what faults in, what is owned; a deployment may run RAM at 4 KiB instead, which the disks never do. Resident: a page whose bytes are in host memory right now; a page nothing touched may not be, and comes in on a fault. Pager: the one service per host that owns every resident page, resolves the VMM's page faults, and is what a guest's memory is mapped through.

Checkpoint: the operation that makes a running VM durable, and what it leaves in the object store, numbered by a sequence. Control record: the one mutable object a VM has: who may write it, which checkpoint is current. Page identity: the name of a page's bytes, which checkpoint published them; a fork's pages carry its parent's names until it writes them. Fork: a new VM taken from a running one at one pause of the parent. Migration: the same VM moved, running, to another host. Host: a machine running VMs. Orchestrator: the one process that places VMs on hosts and drives moves and forks between them.
-->

---

# Three decisions

<div class="text-2xl mt-8 space-y-8">

<div v-click>1. <b>One published checkpoint</b> is a VM's whole durable state. The interval keeps the <b>disks</b>; RAM only on request.</div>

<div v-click>2. <b>Every page has one name</b>: the checkpoint that published it. A fork inherits its parent's names.</div>

<div v-click>3. A checkpoint is a <b>pause</b> and an <b>upload</b>. Only the pause is on the latency path.</div>

</div>

<!--
Everything else follows from these.

One: a VM's durable state is exactly one published checkpoint, selected by its control record. Nothing is durable between checkpoints. Losing a host loses every write since its VMs' last checkpoints. What a host keeps durable on its own is a VM's disks: the interval checkpoints them and not its RAM, because the target is an agent sandbox, where the disk must survive and software recovers its in-memory state from it. RAM is uploaded only by an explicit capture or a suspending stop; a checkpoint without VMM state is opened by booting over its disks.

Two: every page has one name, the checkpoint that published it, and a fork inherits its parent's names. Sharing in the store, in host memory and on the wire is sharing by name: a page with a name is never copied, only referenced. A page no checkpoint holds reads as zeroes.

Three: a checkpoint is a pause and an upload, and only the pause is on anyone's latency path. The pause stops the vCPUs and write-protects the dirty pages — of the disks alone on the interval, of every region with the VMM state saved on a capture: milliseconds. The upload runs behind the running guest. A fork and a migration take only the pause and upload nothing: the unpublished pages reach the other side through the pager.
-->

---
layout: section
---

# The model

---

# One VM

<div class="grid grid-cols-2 gap-x-12 gap-y-3 text-lg mt-2">
<div>

**Identity** — one id, never reused

**Objects** — `vm/<id>/ckpt/<seq>/…`

**Record** — `control/<id>`
- **epoch** — who may write; advanced by every open
- **nonce** — that writer's random mark
- **selected** — the checkpoint that *is* the VM
- **pins** — sequences forked at; never deleted

</div>
<div>

**Writer** — holds the epoch; an open **fences** the last one

**Page identity** — `(checkpoint, volume, page)`; never changes

**Shared** — one resident page, many VMs, until one writes it

</div>
</div>

<!--
Identity: one id, never reused. The orchestrator hands them out; the store refuses to create a VM under an id that has objects left behind.

Everything a checkpoint stores lives under vm/<id>/ckpt/<seq>/. The control record lives at control/<id>, outside that namespace, because the two hold different sets: control/ is exactly the VMs that exist, while vm/ keeps every identity that ever left objects behind — a deleted VM that was forked leaves its pinned checkpoints there for ever — so listing vm/ would return the dead with the living. It holds the epoch, a counter saying which process may write this VM, advanced by every open; the nonce, a random mark of the process that took that epoch; the selected sequence, the checkpoint that is the VM right now; and the pins, sequences this VM was forked at, which must never be deleted.

The writer is the process holding the current epoch. Every open advances the epoch, which fences the writer before it: its next write to the record is refused.

Every page a checkpoint publishes gets the name (checkpoint, volume, page). It never changes, not even when the bytes are later moved into another checkpoint's objects. A fork's checkpoints name its parent's checkpoints, so the fork copies nothing.

One resident page may be the same 2 MiB of host memory for several VMs: a parent's page 3 and its child's page 3, until one of them writes it. The pager keeps a bounded number of resident pages.
-->

---
clicks: 4
---

# The loss model

<LossTimeline />

<!--
A guest store lands in a resident page. It contacts nothing, so it cannot fail for a network or storage reason. It is not durable.

The next checkpoint makes it so: the dirty pages upload in parts, the data objects; then the index object, whose last piece is the root, the map of every page; then a conditional write selects the checkpoint in the control record.

Losing the host before that loses every disk write since the last selected checkpoint, and all of RAM: the interval checkpoints disks alone, so the VM comes back by cold booting over its last disk checkpoint, a power cut its filesystem's journal recovers from. That is the design: a guest write never waits on the object store.

The interval is 60 s, jittered, and it is a target, not a bound. The loss window is the bound on disk writes: once a VM has held an unpublished disk write for longer than it, five minutes by default, zero to disable, its stores wait until a checkpoint lands, and the checkpoint is asked for out of turn. The age travels with a migration or a fork. Bytes are bounded too, by the dirty budget. An fsync is a guest flush the device holds until the host answers: at once while the VM holds no disk write older than the flush bound, 60 s by default, and otherwise when the checkpoint it asks for lands. So an fsync that returned is durable within the bound plus one interval, and a guest whose disks cannot be published stops making fsync progress.
-->

---
clicks: 7
---

# A checkpoint

<CheckpointInstant />

---

# The seal

<div class="grid grid-cols-2 gap-10 mt-4">
<div class="text-xl space-y-5">

<div v-click>pause: stop vCPUs · <b>write-protect</b> dirty pages · (a capture also saves VMM state)</div>

<div v-click><b>no byte moves</b></div>

<div v-click>a store into a sealed page → <b>one private copy</b>, charged to the dirty budget</div>

</div>
<div>

<v-click>

| pause | |
| --- | --- |
| disk checkpoint, cargo build | 6–14 ms |
| fork, 512 MiB guest | 0.1 s |
| migration stop | 0.6–0.75 s |

</v-click>

</div>
</div>

<!--
The pause is two things: stop the vCPUs, and write-protect every dirty page of the regions being checkpointed, a region being one volume as mapped into one VMM process, in place — the disks on the interval, every region with the VMM state saved as well on a capture. No byte moves. The pages become the checkpoint's while the guest keeps running on them.

A store into a sealed page copies that one page into a private page of its own, which counts against the pager's dirty budget: the bound on how much unpublished state a host holds. The checkpoint goes on reading the sealed original.

The numbers are from GCE: the pause of each 60 s disk checkpoint of a guest running cargo build, against Cloud Storage; a fork's pause and a migration's stop for 512 MiB guests.

The upload runs behind the guest. A checkpoint that fails to publish hands its pages back: the previous checkpoint stays selected, nothing durable changed.
-->

---
clicks: 3
---

# What a checkpoint is in the store

<TwoPlanes />

---
clicks: 3
---

# The root and its segments

<RootSegments />

---

# Publication order

<v-clicks>

<div class="text-xl space-y-5 mt-4">

1. **parts** upload as they fill — behind the guest
2. **index object** last — the checkpoint is *published*
3. **record** selects it — the one contended write; the fence
4. **retire the seal** — sealed pages are clean, named, unreserved
5. **reclaim** — old root − new root − pins

</div>

</v-clicks>

<!--
Parts upload as they fill, 64 MiB each, behind the guest. A part that never completes is garbage under a key nothing names.

The index object is written last, with a create-if-absent PUT. The checkpoint is now published: complete and readable by anyone. Nobody else can be writing that key, the sequence is epoch-major and one writer holds the epoch, so the precondition costs nothing and only makes the object immutable.

The control record selects the sequence with a conditional write from the writer's own epoch. This is the one contended step, and the one where a fenced writer is refused: the checkpoint is now the VM's state.

Retire the seal: the sealed pages are published now. Each takes its name under this checkpoint, gives its dirty reservation back, and stays mapped to the guest as a clean page. A page the guest stored into meanwhile already copied itself; that copy is the new dirty state. Done first after the selection, because until then every store into a sealed page still copies.

Reclaim: delete the checkpoints the old root named that the new one does not, less every pin. Whole checkpoints, index object first.

A lost reply is reconciled by reading back: only the writer of an epoch can produce a record carrying that epoch's nonce, so "did my write land?" always has an answer.
-->

---
clicks: 4
---

# Fencing

<Fence />

---

# Sequences are epoch-major

```text
sequence = (epoch << 32) | counter
```

<v-clicks>

<div class="text-xl space-y-5 mt-6">

- a new epoch's sequences are **above** every older one
- every checkpoint object is **create-if-absent** under its own key
- the first epoch is **random** in `[1, 2³¹)`
- a fenced host learns at its next write — or on the **epoch timer**

</div>

</v-clicks>

<!--
Every sequence a writer allocates is above everything an earlier epoch could allocate. A fenced writer still uploading cannot collide: every checkpoint object is create-if-absent under a key its successor never uses.

The first epoch is drawn at random. Two VMs created under one identity, which the orchestrator never does but nothing can enforce, allocate different sequences, different page identities, different keys.

A fenced host finds out at its next publication, or sooner: it re-reads the record on a timer, and a migration or a fork confirms before it pauses, because the pages it hands another host are the one thing the store cannot refuse afterwards.
-->

---
clicks: 3
---

# Reclamation and compaction

<Reclaim />

<!--
Reclamation is a set difference. After selecting a root: delete the checkpoints the replaced root named, less everything the new root names, less every pin. Only this VM's own checkpoints. Whole.

Compaction bounds the residue. A checkpoint less than half live is rewritten into the one being published, up to 64 MiB of live bytes, after the guest resumed. It falls out of the root and reclamation deletes it.

Pins are permanent. A fork pins the parent's published sequence. Nothing in a deployment can tell that nothing reads through it any more: a descendant sees neither its siblings nor the forks below it.

The collector is deferred, by decision. Until one exists the store grows without bound: every checkpoint a VM was forked at, everything its root names, and what a deleted VM leaves pinned are kept forever.
-->

---
layout: section
---

# The pager

---
clicks: 2
---

# The page cache duplicates what VMs share

<Duplication />

<!--
The kernel's page cache caches per file. Give each VM its own writable copy of the image, and a reflink is still its own inode, and N VMs hold N copies of the same bytes in host memory. Then the guest does it again: a virtio-blk disk gets its own page cache inside each guest's RAM.

The pager keeps one resident page per name, however many VMs map it, and the guest reaches it over PMEM DAX: the host's copy is the only copy.
-->

---

# Why not the kernel's page cache

<v-clicks>

<div class="text-xl space-y-4 mt-2">

1. **it duplicates** — per file on the host, again per guest with virtio-blk
2. **sharing is by name** — across VMs, across checkpoints
3. **durability is one pause** — not writeback
4. **the backing is not only a file** — a page may be on another host
5. **the budgets are the host's** — stall, don't swap; HugeTLB anyway
6. **it runs in the simulation**

</div>

</v-clicks>

<!--
For a guest's memory the pager does what the kernel's page cache and swap do for a file: decides what is resident, faults the rest in, shares, tracks what was dirtied, writes back, evicts, spills. The kernel has all of that. It is not used.

One, duplication, as on the last slide. Two: sharing is by name, across VMs and checkpoints. A child's memory is its parent's checkpoints plus its own writes, at 2 MiB over tens of thousands of pages: one VMA per run, against the kernel's mapping limit, rearranged at every checkpoint. Three: durability is one pause of the whole machine, not writeback; the kernel writes back when it chooses, a checkpoint freezes every dirty page at one moment while the guest runs on. Four: the backing is not only a file; after a fork or a migration a page may be on another host, and a filesystem in front of the store cannot fault from a peer, while every fault would go through the kernel and back at 4 KiB. Five: the budgets are the host's; resident, logical and dirty pages are admitted explicitly, so a guest that dirties faster than it publishes is checkpointed out of turn or stalled, not swapped or killed, and HugeTLB is unswappable anyway. Six: the same pager on a simulated arena, disk and clock is what the campaigns exercise; the kernel's page cache cannot be.
-->

---

# One host-wide pager

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**arena** — sealed HugeTLB memfd, 2 MiB slots

**budgets** — resident · logical · dirty → **spill file**

**faults** — userfaultfd; copy-on-write; read-ahead; eviction

**region** — one volume in one VMM; loads from the volume or a **backing**

</div>
<div class="space-y-5">

<div v-click>Rust <code>sproutfs-vm-memory</code> — the mappings inside the VMM; no Firecracker dependency</div>

<div v-click>a private page takes its spill slot <b>before</b> the write resumes</div>

<div v-click>bounds chosen from the node; logged at start</div>

</div>
</div>

<!--
internal/vmmemory owns the host's guest memory: an arena, a fixed-size sealed HugeTLB memfd of 2 MiB slots; explicit resident, logical and dirty budgets, where the dirty budget sizes a spill file that is the pager's whole disk cap; fault resolution over userfaultfd, private copy-on-write, eviction, read-ahead and write-ahead.

A region, one volume mapped into one VMM process, loads its pages from the volume, or from a backing put in front of it, which is how a migration destination reads from its source.

The Rust library owns the mappings inside the VMM process. It has no Firecracker dependency; the Firecracker fork maps its guest memory through it.

Every private page takes a spill slot before the write resumes, so a store never has to ask anything for room. Pool exhaustion is an allocation error, not a fallback to small pages.

Production bounds are chosen by the host process from the node it is on, read-ahead 8 MiB, I/O permits four per processor, and logged at start.
-->

---
clicks: 3
---

# Sharing by identity

<IdentityShare />

---

# What identity buys

<v-clicks>

<div class="text-xl space-y-5 mt-4">

- **population before vCPUs run** — resident names map without a load
- **sharing is a map lookup** — keyed by `(checkpoint, volume, page)`
- **sealed pages get a name** — a fork point's, for as long as the seal
- **sparse zeroes cost nothing** — the shared zero page
- **a one-byte store seals a page** — the settle drops what did not change; the upload tracks the bytes that did

</div>

</v-clicks>

<!--
Population before vCPUs run: a restored or forked machine maps every page whose name is already resident in the pager, without a load. A fork maps its parent's whole resident set before its vCPUs run.

Sharing costs a map lookup: the pager's sharing index is keyed by (checkpoint, volume, page), which the volume already knows for every page it serves.

Sealed pages get a name: a fork point, the pause a fork is taken at, which seals the parent's dirty pages exactly as a checkpoint's pause does, names the pages it sealed under a reference that publishes nothing; every child of that fork point maps them. The name lasts exactly as long as the seal.

Sparse zeroes cost nothing: a page no checkpoint holds maps the shared zero page. The first store replaces the whole 2 MiB range with a private page.

A one-byte store costs a page in memory: 2 MiB copied and charged against the dirty budget. What is published is less: the settle behind the pause drops every sealed page whose bytes did not change, and compression takes most of the rest. Measured on disk checkpoints, the upload never exceeded the bytes the guest changed.
-->

---
layout: section
---

# Fork and migration

---
clicks: 4
---

# A fork is one pause

<ForkFanOut />

---

# What a fork costs and leaves

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**costs** — one pause on the parent; the child's boot; **no upload**

**leaves** — a pin on the parent; a record for the child

**publishes** — the child's root, once it holds its inherited pages

</div>
<div class="space-y-5">

<div v-click><b>templates are forks</b> — named by the image's digest, imported once</div>

<div v-click><b>holds</b> — the parent stays sealed until the child publishes or pulls; deadline 4 intervals</div>

</div>
</div>

<!--
Costs: one pause on the parent, the same pause as a checkpoint's, and the child's boot. Nothing uploaded. On GCE the pause was 0.1 s; the child ran 1.6 to 10 s later.

Leaves: a pin on the parent's published sequence, and one control record for the child selecting a root over it. A fork that ends before it leaves no object behind.

Publishes: the child's root, as soon as the child holds every inherited page; that is what makes it a VM any host can open. Before that, opening it anywhere reports that the fork is pending.

Templates are forks too. A guest image is imported once per deployment into a template named by the image's digest; a create is a fork of that template. Every VM of one image shares its resident pages by name.

Holds: the parent's pages stay sealed while the child holds the fork point. A hold ends when the child publishes or pulls its pages, when the orchestrator gives the handoff up, or at the host's deadline of four checkpoint intervals.
-->

---
clicks: 7
---

# Migration is post-copy only

<PostCopy />

---

# The handoff

```text
Handoff { VMID, Sequence, VMMState, Regions[] (unpublished runs, age), PageServer }
```

<v-clicks>

<div class="text-xl space-y-5 mt-6">

- migration: the source **released** · fork: the parent **keeps running** · same data
- a record at any other sequence → `ErrStale`; nothing is streamed
- same host: shared pager · other host: pulled over TCP
- a page only the source holds is asked for **until it arrives**

</div>

</v-clicks>

<!--
A handoff is plain data: the VM and the sequence its record selected when the source gave it up; the VMM state the pause captured; per region, its name, size, the runs of pages no checkpoint has and how old the oldest of them is; and where the source serves them from.

A migration hands off a VM the source released. A fork hands off a child from a parent that keeps running. Same data, one receive path on the destination.

The destination refuses a record selecting any other sequence: a migration publishes nothing, so that record was openable by anyone in between, and streaming pages over another writer's open would make one VM's memory out of two writers' pages with no error anywhere.

Local child or remote child changes only how the unpublished pages arrive: through the shared pager, or pulled over TCP.

A page only the source holds is fetched until it arrives, with backoff and no attempt limit: the destination cannot tell a slow source from a dead one, and its own volume would answer with bytes from before the guest's write. The orchestrator ends a migration only on evidence the source host is gone; the VM then reopens from its checkpoint.
-->

---
layout: section
---

# The deployment

---
clicks: 3
---

# Host and orchestrator

<Topology />

<!--
The host opens VMs over object storage and the cluster network, checkpoints every VM's disks on the interval, answers its guests' flushes, confirms its epochs on a timer and closes a fenced VM rather than leave it running, serves migration and fork pages, owns the pager and the VMM processes, and drains before it exits: migrates every VM away, then waits until it serves no pages.

The orchestrator allocates identities, places VMs, and drives both halves of every migration and fork. It surveys every host on an interval, what each runs and serves, and ends a migration only on positive evidence its source is gone. Its SQLite table is a view; the control records are the authority. It refuses to name a VM's host while two hosts claim it.

On Kubernetes: hosts are a Deployment with maxSurge 0 and maxUnavailable 1, whose preStop drain has thirty minutes to move every VM away before the pod is given up, a 2 MiB HugeTLB pool per node, one bearer token for the whole control plane, one object store. Templates named by image digest mean a rolled host pod imports nothing again.
-->

---

# Stop, start, cold start

<v-clicks>

<div class="text-xl space-y-5 mt-4">

- **stop** — a final checkpoint of the disks; close
- **stop --suspend** — memory and VMM state too
- **start** — resume a suspended VM; boot one that was not
- **cold start** — discard memory; boot; the one place the shape may change
- **delete** — remove the record; sweep the VM's own checkpoints; keep the pinned

</div>

</v-clicks>

<!--
Stop publishes a final checkpoint of the disks and closes the VM: nothing written to disk is lost, and the start after it boots the guest over them. Stop with suspend publishes memory and VMM state as well, and the start after it resumes the guest where it was, the pages faulting in. Uploading memory is the one thing a stop does only when asked.

Cold start discards the memory in a checkpoint of its own and boots the guest instead of resuming it. It is the one place a VM's shape may change: more memory, and a root volume the guest grows into.

Delete removes the record, which frees the identity, then sweeps the VM's own checkpoints, except the pinned ones a descendant may still read.
-->

---

# The Firecracker integration

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**fork of Firecracker**, branch `sproutfs`

- guest memory mapped through the Rust library: userfaultfd, control protocol v9
- capture = snapshot + resume; interval = pause + seal disks + resume
- a guest flush is **held** until the host answers
- vsock reset only on restore
- `handoff` drops connections on the way out

</div>
<div class="space-y-5">

<div v-click><b>guest</b> — init, exec agent, console, and a <b>witness</b> that fills and checks memory and disk</div>

<div v-click><b>qualified</b> — Lima on aarch64; GCE x86_64</div>

</div>
</div>

<!--
The Firecracker fork, branch sproutfs, Apache-2.0. Guest memory is a mapping the Rust library controls: missing-fault and write-protect traps over userfaultfd, the pager's page, a control protocol, version 9, between the VMM and the pager. A capture is a snapshot and a resume; the interval's checkpoint pauses the vCPUs, seals the disks and resumes. A virtio-pmem flush is a request the device holds, off the VMM thread, until the host answers; the flushes it holds are in its snapshot, so they survive a migration and are sent again to the next host. The vsock transport is reset only on restore, so a checkpoint leaves a guest's connections alone. A handoff flag drops connections when the guest leaves the host.

Guest side: an init that names /dev/root, an agent for exec and console, and a witness that fills memory and disk with seeded data and checks it after every fork, migration, stop and cold start.

Qualified on Linux with real KVM: Lima on Apple silicon, and x86_64 on GCE for HugeTLB and the soak.
-->

---
layout: section
---

# Evidence

---

# One simulation harness

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

a **real** deployment in one process: real hosts, pagers, page servers, store code

simulated: process · disk · clock · network · object store

`testing/synctest` — simulated time is free

</div>
<div class="space-y-5">

<div v-click>drive: <code>Store Checkpoint CheckpointDisks Migrate Fork Stop Suspend Kill Restart …</code></div>

<div v-click>require: <b>Verify · VerifyDurable · CheckSelected · CheckDeployment · VerifyLossWindow</b></div>

<div v-click>a lost VM comes back at the record's checkpoint, <b>page for page</b></div>

</div>
</div>

<!--
A simtest World is one running deployment: every host is a real host inside a simulated process, on a simulated disk with power-loss faults, against a simulated clock and seeded entropy, reaching a simulated object store. Nothing in it is a mock of the thing under test. The volume managers, the checkpoint store, the control records, the pagers, the page servers and the migration coordinator are the real ones. Go's synctest makes simulated time free: a campaign that waits out a 60 s interval costs microseconds.

Campaigns drive it with one short list and require one short list: no guest reads bytes it never wrote; the same bytes through the volume; every record selects a checkpoint some writer published; the store agrees with itself; and a lost host rewinds a VM by at most the loss window.

A VM that lost its host comes back at the checkpoint its record names, and the bytes must be that checkpoint's, page for page — with its memory zeroes when that checkpoint was of the disks alone, because it has no registers to resume. An interrupted publication is answered for exactly. That oracle is what found a checkpoint of the disks naming the registers of the capture before it.
-->

---

# The campaigns

| | |
| --- | --- |
| seeded topology | hosts, VMs, forks, faults — all from the seed |
| under buggify | plus fault injection at named sites |
| kill | a host lost at any handoff loses only the unpublished |
| swizzle | every link cut and healed on its own clock; two writers never mix |
| recorded | one schedule, reproduced event for event |

<v-clicks>

<div class="text-xl mt-6 space-y-3">

- nightly: **8 × 100 seeds** on hosted runners; `just soak <seed> 1` reproduces one
- power loss: writes **applied, dropped, torn or garbled** · links: **drop, duplicate, delay, slow**

</div>

</v-clicks>

<!--
Each campaign has a soak twin over a block of the seed range. The nightly sweep runs eight blocks of 100 seeds on GitHub's hosted runners. A failing seed reproduces anywhere with just soak, the seed, and a count of one.

Power loss on the simulated disk returns unsynced writes applied, dropped, torn or garbled. Swizzled links drop, duplicate, delay and slow what they carry.
-->

---

# Measured on a real cluster

<div class="text-base opacity-70 mb-3">two hosts on GCE · 512 MiB guests · memory and disk witnesses · 166 operations, 146 checks, all holding · 2026-09-17, when a stop and the interval still captured memory</div>

| | pause | behind it | wall, slowest |
| --- | --- | --- | --- |
| fork, same host | 0.1 s | 1.6–10 s to run the child | 10 s |
| fork, other host | 0.1 s | 5–20 s post-copy | 20 s |
| migrate | 0.6–0.75 s | 0.3–4.9 s stream | 5.7 s |
| stop | — | — | 6.3 s |
| start, warm | — | — | 1.0 s |
| start, cold, grown | — | grow 0.5 s | 2.1 s |

<v-click>

<div class="mt-4 text-base opacity-70">object store: ~17,000 GETs / 18 GiB · ~1,300 PUTs / 41 GiB · 1,100 deletes</div>

</v-click>

<!--
Two hosts on GCE, 512 MiB Alpine guests each carrying a 256 MiB memory witness and a 256 MiB file witness, six rounds. 166 operations, 146 checks, every one holding: after every mutation, every fork, every migration, every stop and warm start, every cold start with a larger memory and a grown root, and a host killed in the last round.
-->

---

# What a disk checkpoint costs

<div class="text-base opacity-70 mb-3">GCE · Cloud Storage · the disk checkpointed every 60 s · 2 MiB pages · changed = 4 KiB blocks the guest really changed</div>

| workload | changed | sealed | published | uploaded | pause |
| --- | --- | --- | --- | --- | --- |
| pnpm install | 1.6 MiB | 212 MiB | 30 MiB | 1.6 MiB | 3 ms |
| cargo build, 7 checkpoints | 6.1 GiB | 11.7 GiB | 6.9 GiB | 1.7 GiB | 6–14 ms |
| Valkey append-only log | 3.0 GiB | 3.1 GiB | 3.0 GiB | 23 MiB | 4 ms |

<v-clicks>

<div class="text-xl mt-6 space-y-3">

- the pause is **milliseconds**, whatever the guest did
- 2 MiB pages cost **memory**, not upload: the settle drops unchanged pages, compression the rest
- the upload never exceeded **what changed**

</div>

</v-clicks>

<!--
Measured on GCE against Cloud Storage, the host checkpointing the disk alone every 60 seconds. Changed is what the guest really changed, counted in 4 KiB blocks by summing each page when it became private and again at the checkpoint. Sealed is what the 2 MiB pages took; published is what was left after the settle dropped the pages whose bytes had not changed; uploaded is compressed.

Small scattered writes, a package manager's, seal a hundred times what changed; almost all of it is dropped or compressed away. Large writes, a compiler's or a log's, seal about what changed. Valkey's benchmark writes identical values, which flatters its compression.
-->

---

# What is open

---

# What is open

<v-clicks>

<div class="text-xl space-y-5 mt-4">

- **the collector** — pins are permanent; the store grows; deferred by decision
- **an unreachable but listed source** — the migration waits; evidence, not a timeout
- **a drain tries its receive once**
- **a local fork's hold is invisible** to the survey; only the deadline ends it
- **recovery after a real host loss** — proven in simulation, not yet on a cluster
- **disk checkpoints and the flush bound** — not yet qualified on GCE

</div>

</v-clicks>

<!--
The collector: pins are permanent and the store grows without bound until one exists. Deferred by decision.

A migration whose source is unreachable but still listed waits for it. The orchestrator ends a migration on evidence, never on a timeout, and for a pod it cannot reach it has none beyond the Kubernetes API's own.

A drain tries its receive once. A destination briefly unreachable costs the guest its writes since its last checkpoint; the source keeps the pages until its deadline, and nothing asks again.

A child forked onto its parent's own host holds the fork point invisibly to the orchestrator's survey; only the host's own deadline ends a hold whose child never publishes.

Recovery after a real host loss is proven in simulation and over fakes, not yet on a cluster: the one soak's kill landed on a host running nothing. A seed whose kill lands on a loaded host is the next run to take.

Disk-only checkpoints, cold boot on a checkpoint without state, and the blocking flush are proven in the simulation, the host suite and Lima; the GCE qualification of them together is still to run.
-->

---

# The repository

```text
cmd/sproutfs-host            the host process
cmd/sproutfs-orchestrator    identities, placement, moves, forks
cmd/sproutfs-guest-witness   fill / mutate / check / grow, in the guest
internal/checkpoint          the store: parts, index objects, roots, reclamation
internal/control             control records
internal/volume              volumes, publication, forks, handoffs
internal/vmmemory            the pager
internal/vmmigrate           page server and peer backing
internal/host                one host: loop, drain, fork, migrate, templates
internal/simtest             the harness and its campaigns
internal/platform/sim        processes, disks, clocks, networks, faults
rust/sproutfs-vm-memory      the mappings inside the VMM
third_party/firecracker      the fork, branch sproutfs
docs/  plans/                the design, its decisions, what was measured
```

---

# The requirement, answered

<div class="text-xl mb-4">
Cost what the VM <b>changed</b>. Never what it inherited. Never its size.
</div>

| | the pause | what moves |
| --- | --- | --- |
| disk checkpoint | 3–14 ms | about the disk bytes changed since the last one |
| fork | 0.1 s | nothing: the child maps the parent's pages by name |
| migration | 0.6–0.75 s | the pages no checkpoint has, behind the running guest |
| losing a host | — | disk writes since the last checkpoint, at most the loss window; RAM, by design |

<v-click>

<div class="mt-10 text-center text-2xl">
github.com/semistrict/sproutfs
</div>

</v-click>
