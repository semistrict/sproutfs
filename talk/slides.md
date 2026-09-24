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

VMs that move between hosts and fork without copying inherited data

<div class="mt-12 text-lg opacity-70">
VM checkpoints in object storage · a shared memory pager · a Firecracker integration
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
We have a set of hosts and an object store. We want many VMs that are mostly identical: all started from one image, many forked from a running parent, all able to move to another host, and all stored durably off the host they run on.

Most of a VM's data comes from the image, from its parent, or from its own earlier checkpoints. Only a small part is new.

With the usual tools, each of these operations copies the whole VM. A snapshot writes all of memory out and a restore reads it back. A live migration streams memory while the guest changes it. A fork is a snapshot plus a restore. The cost grows with the VM's size, and most of the copied bytes are inherited data that nobody changed.
-->

---

# The requirement

<div class="text-2xl mt-6 p-5 border border-yellow-600 rounded">
Cost scales with what the VM changed, not with what it inherited or its size.
</div>

<v-clicks>

<div class="mt-10 text-xl space-y-4">

- inherited data is **shared, not copied**: in the store, in host memory, on the network
- making a VM durable **pauses it for milliseconds**; the upload happens afterwards
- a host can be **lost**, and the data lost with it is **bounded**

</div>

</v-clicks>

<!--
All three have to hold. Inherited data is shared rather than copied in the object store, in host memory, and between hosts. Making a VM durable pauses it for milliseconds, not for the length of an upload. A VM's durable state is reachable from every host, so a host can be lost or drained, and the data lost is bounded by the loss window.
-->

---

# Terms

<div class="grid grid-cols-2 gap-x-12 gap-y-3 text-lg mt-4">
<div>

**VM** — one identity, one series of checkpoints

**Volume** — `ram0` or a PMEM disk; fixed size; one writer

**Page** — 2 MiB (RAM can use 4 KiB)

**Resident** — a page currently in host memory

**Pager** — owns all resident pages; handles faults

</div>
<div>

**Checkpoint** — makes a VM durable; numbered by *sequence*

**Control record** — current writer and current checkpoint

**Page identity** — the checkpoint that published the page

**Fork** / **migration** — new VM from a pause / same VM on another host

**Host** / **orchestrator** — runs VMs / places and moves them

</div>
</div>

<!--
VM: one identity with one series of checkpoints. Volume: a byte-addressed image of the VM's RAM or of one of its PMEM disks; fixed size; one writer at a time. Page: 2 MiB of a volume. It is the unit that is stored, faulted in and owned. A deployment can run RAM with 4 KiB pages; disks always use 2 MiB. Resident: a page currently in host memory. Pages that have not been touched are loaded on a fault. Pager: one service per host. It owns all resident pages, handles the VMM's page faults, and maps guest memory.

Checkpoint: the operation that makes a running VM durable, and the objects it writes, numbered by a sequence. Control record: the one mutable object per VM. It records which process may write and which checkpoint is current. Page identity: the checkpoint that published a page's bytes. A fork's pages keep the parent's identities until the fork writes them. Fork: a new VM created from one pause of a running VM. Migration: the same VM moved, while running, to another host. Host: a machine that runs VMs. Orchestrator: the process that places VMs and coordinates moves and forks.
-->

---

# Three design decisions

<div class="text-2xl mt-8 space-y-8">

<div v-click>1. A VM's durable state is <b>one published checkpoint</b>. The interval checkpoints the <b>disks</b>; RAM is saved only on request.</div>

<div v-click>2. <b>Every page is named</b> by the checkpoint that published it. A fork keeps its parent's names.</div>

<div v-click>3. A checkpoint is a <b>pause</b> followed by an <b>upload</b>. Only the pause affects latency.</div>

</div>

<!--
The rest of the design depends on these three decisions.

One: a VM's durable state is the one published checkpoint its control record selects. Nothing is durable between checkpoints, so losing a host loses the writes since each VM's last checkpoint. The interval checkpoint covers the disks, not RAM. The target use is an agent sandbox: the disk must survive, and software rebuilds its in-memory state from the disk. RAM is uploaded only by an explicit capture or a suspend. A checkpoint without VMM state is opened by booting the guest from its disks.

Two: each page is named by the checkpoint that published it, and a fork keeps its parent's names. The store, host memory and the network share pages by name: a named page is referenced, not copied. A page that no checkpoint holds reads as zeroes.

Three: a checkpoint is a pause followed by an upload, and only the pause affects latency. The pause stops the vCPUs and write-protects the dirty pages. On the interval this covers the disks only; a capture covers every region and also saves the VMM state. It takes milliseconds. The upload runs while the guest continues. A fork or migration takes the pause and uploads nothing; the pager moves the unpublished pages to the other side.
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
- **epoch** — which process may write; incremented on every open
- **nonce** — random value of that writer
- **selected** — the current checkpoint
- **pins** — sequences that forks were taken at; never deleted

</div>
<div>

**Writer** — holds the epoch; an open **fences** the previous writer

**Page identity** — `(checkpoint, volume, page)`; never changes

**Shared** — one resident page used by many VMs until one writes it

</div>
</div>

<!--
Identity: one id, never reused. The orchestrator assigns ids, and the store refuses to create a VM under an id that still has objects.

Checkpoint objects are stored under vm/<id>/ckpt/<seq>/. The control record is stored separately at control/<id>. The two sets differ: control/ lists exactly the VMs that exist, while vm/ also keeps objects of deleted VMs whose checkpoints are pinned by forks. Listing vm/ would therefore include deleted VMs.

The record holds four fields. The epoch says which process may write the VM and is incremented on every open. The nonce is a random value chosen by the process that took the epoch. Selected is the current checkpoint. Pins are sequences that forks were taken at; they are never deleted.

The writer is the process holding the current epoch. Each open increments the epoch, which fences the previous writer: its next write to the record is refused.

Every published page is named (checkpoint, volume, page). The name does not change, even when compaction moves the bytes into another checkpoint's objects. A fork's checkpoints refer to its parent's checkpoints, so the fork copies nothing.

One resident page can be shared by several VMs, for example page 3 of a parent and of its child, until one of them writes it. The number of resident pages is bounded.
-->

---
clicks: 4
---

# The loss model

<LossTimeline />

<!--
A guest store writes to a resident page. It does not contact the network or the store, so it cannot fail for those reasons, and it is not durable.

The next checkpoint makes it durable: the dirty pages upload in parts, then the index object with the root, and then a conditional write selects the checkpoint in the control record.

If the host is lost before that, the disk writes since the last selected checkpoint are lost, and so is RAM. The interval checkpoints disks only, so the VM restarts by booting from its last disk checkpoint, as after a power failure; the guest filesystem's journal handles recovery. This trade-off keeps guest writes from ever waiting on the object store.

The interval is 60 s with jitter. It is a target, not a guarantee. The loss window is the guarantee for disk writes: if a VM has held an unpublished disk write for longer than the window (5 minutes by default; 0 disables it), its stores block until a checkpoint lands, and the host requests one immediately. The age of unpublished writes is carried across migrations and forks. The dirty budget bounds unpublished data in bytes.

An fsync is a guest flush that the device holds until the host answers. The host answers immediately if the VM has no unpublished disk write older than the flush bound (60 s by default). Otherwise it answers after the checkpoint it requests has landed. As a result, data is durable within the flush bound plus one interval after an fsync returns, and fsync blocks if the disks cannot be published.
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

<div v-click><b>no data is copied</b></div>

<div v-click>a store to a sealed page → <b>one private copy</b>, counted against the dirty budget</div>

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
The pause stops the vCPUs and write-protects the dirty pages in place, for each region being checkpointed. A region is one volume mapped into one VMM process. The interval covers the disks; a capture covers every region and also saves the VMM state. No data is copied. The sealed pages belong to the checkpoint while the guest keeps running.

If the guest stores to a sealed page, the pager copies that page to a new private page, which counts against the dirty budget. The dirty budget limits how much unpublished data a host holds. The checkpoint keeps reading the sealed original.

The numbers are from GCE: the pause of each 60 s disk checkpoint while the guest runs cargo build, against Cloud Storage; and a fork's pause and a migration's stop for 512 MiB guests.

The upload runs after the guest resumes. If publication fails, the pages return to the guest, the previous checkpoint stays selected, and nothing durable changes.
-->

---
clicks: 3
---

# A checkpoint in the store

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

1. **parts** upload as they fill, while the guest runs
2. **index object** last — the checkpoint is *published*
3. **record** selects it — the only contended write; this is where fencing applies
4. **retire the seal** — sealed pages become clean and named, and release their reservations
5. **reclaim** — old root − new root − pins

</div>

</v-clicks>

<!--
Parts upload as they fill, 64 MiB each, while the guest runs. A part that never completes is garbage under a key that nothing refers to.

The index object is written last with a create-if-absent PUT. At that point the checkpoint is published: complete and readable. No one else can write that key, because sequences are epoch-major and only one process holds the epoch. The precondition therefore always succeeds; it only makes the object immutable.

The control record then selects the sequence with a conditional write from the writer's epoch. This is the only contended step, and it is where a fenced writer is refused. After it, the checkpoint is the VM's state.

Retiring the seal: the sealed pages are now published. Each page takes its name from this checkpoint, returns its dirty reservation, and stays mapped to the guest as a clean page. Pages the guest wrote in the meantime were already copied; those copies are the new dirty state. This step runs right after selection, because until then every store to a sealed page still makes a copy.

Reclaim: delete the checkpoints that the old root referenced and the new root does not, except pinned ones. Whole checkpoints are deleted, index object first.

If a reply is lost, the writer reads the record back. Only the writer of an epoch can produce a record with that epoch's nonce, so it can always tell whether its write landed.
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

- a new epoch's sequences are **greater** than all older ones
- every checkpoint object is **create-if-absent** under its own key
- the first epoch is **random** in `[1, 2³¹)`
- a fenced host finds out at its next write, or from the **epoch timer**

</div>

</v-clicks>

<!--
Every sequence a writer allocates is greater than any sequence an earlier epoch could allocate. A fenced writer that is still uploading cannot collide with the new writer, because every checkpoint object is create-if-absent under a key the new writer never uses.

The first epoch is random. If two VMs were ever created under the same id, which the orchestrator does not do but cannot rule out, they would still allocate different sequences, page identities and keys.

A fenced host finds out at its next publication, or earlier: it re-reads the record on a timer, and a migration or fork checks the record before pausing, because the pages it hands to another host cannot be recalled afterwards.
-->

---
clicks: 3
---

# Reclamation and compaction

<Reclaim />

<!--
Reclamation is a set difference. After selecting a new root, delete the checkpoints the old root referenced, minus those the new root references, minus pinned ones. Only this VM's own checkpoints are deleted, and only whole checkpoints.

Compaction limits leftover data. A checkpoint that is less than half live is rewritten into the checkpoint being published, up to 64 MiB of live data, after the guest resumes. The old checkpoint then drops out of the root and reclamation deletes it.

Pins are permanent. A fork pins the parent's published sequence. No component can determine that nothing reads through a pin any more, because a descendant sees neither its siblings nor its own forks.

We have not built a collector yet. Until we do, the store keeps growing: every checkpoint a VM was forked at, everything its root references, and pinned data of deleted VMs are kept indefinitely.
-->

---
layout: section
---

# The pager

---
clicks: 2
---

# The page cache duplicates shared data

<Duplication />

<!--
The kernel's page cache works per file. If each VM gets its own writable copy of the image, even a reflink is a separate inode, so N VMs hold N copies of the same data in host memory. A virtio-blk disk adds another copy in each guest's own page cache.

The pager keeps one resident page per name regardless of how many VMs map it, and the guest accesses it through PMEM DAX, so there is only one copy in host memory.
-->

---

# Why not the kernel's page cache

<v-clicks>

<div class="text-xl space-y-4 mt-2">

1. **duplication** — per file on the host, and again per guest with virtio-blk
2. **sharing by name** — across VMs and across checkpoints
3. **durability in one pause** — not writeback
4. **backing is not just a file** — a page may be on another host
5. **explicit budgets** — stall instead of swap; HugeTLB cannot be swapped anyway
6. **testable in simulation**

</div>

</v-clicks>

<!--
The pager does for guest memory what the kernel's page cache and swap do for files: it decides what is resident, faults the rest in, shares pages, tracks dirty pages, writes back, evicts and spills. We do not use the kernel for this, for six reasons.

One: duplication, as shown on the previous slide.

Two: sharing is by name, across VMs and checkpoints. A child's memory is its parent's checkpoints plus its own writes, at 2 MiB over tens of thousands of pages. With kernel mappings this needs one VMA per run, runs into the kernel's mapping limit, and changes at every checkpoint.

Three: durability is one pause of the whole VM, not writeback. The kernel writes back when it decides to; a checkpoint freezes all dirty pages at one moment while the guest continues.

Four: the backing is not only a file. After a fork or migration, a page may be on another host. A filesystem in front of the store cannot fetch from a peer, and every fault would go through the kernel at 4 KiB.

Five: the budgets belong to the host. Resident, logical and dirty pages are admitted explicitly, so a guest that dirties memory faster than it publishes is checkpointed early or stalled, not swapped or killed. HugeTLB memory cannot be swapped anyway.

Six: the same pager runs in the simulation on a simulated arena, disk and clock. The kernel's page cache cannot.
-->

---

# One pager per host

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**arena** — sealed HugeTLB memfd with 2 MiB slots

**budgets** — resident · logical · dirty → **spill file**

**faults** — userfaultfd; copy-on-write; read-ahead; eviction

**region** — one volume in one VMM; loads from the volume or a **backing**

</div>
<div class="space-y-5">

<div v-click>Rust <code>sproutfs-vm-memory</code> manages the mappings inside the VMM; it does not depend on Firecracker</div>

<div v-click>a private page reserves its spill slot <b>before</b> the store resumes</div>

<div v-click>bounds are derived from the node and logged at startup</div>

</div>
</div>

<!--
internal/vmmemory manages the host's guest memory. The arena is a fixed-size, sealed HugeTLB memfd with 2 MiB slots. There are explicit budgets for resident, logical and dirty pages; the dirty budget sizes the spill file, which is the pager's total disk allowance. Faults are resolved over userfaultfd, with private copy-on-write, eviction, read-ahead and write-ahead.

A region is one volume mapped into one VMM process. It loads pages from the volume, or from a backing placed in front of it. A migration destination uses a backing to read from its source.

The Rust library manages the mappings inside the VMM process. It does not depend on Firecracker; the Firecracker fork maps guest memory through it.

Each private page reserves a spill slot before the store resumes, so a store never needs to ask for space later. If the pool is exhausted, allocation fails; there is no fallback to small pages.

The host process derives the production bounds from the node, such as 8 MiB read-ahead and four I/O permits per processor, and logs them at startup.
-->

---
clicks: 3
---

# Sharing by identity

<IdentityShare />

---

# What identity provides

<v-clicks>

<div class="text-xl space-y-5 mt-4">

- **population before vCPUs run** — resident pages are mapped without a load
- **sharing is a map lookup** — keyed by `(checkpoint, volume, page)`
- **sealed pages are named** — by a fork point, for as long as the seal lasts
- **sparse zeroes are free** — mapped to the shared zero page
- **a one-byte store seals a whole page** — unchanged pages are dropped before upload

</div>

</v-clicks>

<!--
Population before vCPUs run: a restored or forked VM maps every page whose name is already resident, without loading it. A fork maps its parent's entire resident set before its vCPUs start.

Sharing is a map lookup: the pager's index is keyed by (checkpoint, volume, page), which the volume already knows for every page.

Sealed pages are named: a fork point is the pause a fork is taken at. It seals the parent's dirty pages like a checkpoint and names them under a reference that is never published. Every child of that fork point maps them. The name lasts as long as the seal.

Sparse zeroes are free: a page no checkpoint holds maps to the shared zero page. The first store replaces the 2 MiB range with a private page.

A one-byte store costs a whole page in memory: 2 MiB copied and counted against the dirty budget. Less is uploaded. Before the upload, the settle step drops sealed pages whose bytes did not change, and compression reduces the rest. In our disk checkpoint measurements, the upload was never larger than the data the guest changed.
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

# Fork cost and results

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**cost** — one pause of the parent, plus the child's boot; **no upload**

**creates** — a pin on the parent and a record for the child

**publishes** — the child's root, once it holds its inherited pages

</div>
<div class="space-y-5">

<div v-click><b>templates are forks</b> — named by the image digest, imported once</div>

<div v-click><b>holds</b> — the parent stays sealed until the child publishes or pulls its pages; deadline 4 intervals</div>

</div>
</div>

<!--
Cost: one pause of the parent, the same pause as a checkpoint, plus the child's boot. Nothing is uploaded. On GCE the pause was 0.1 s, and the child ran 1.6 to 10 s later.

Creates: a pin on the parent's published sequence and a control record for the child that selects a root over it. A fork that fails leaves no objects behind.

Publishes: the child's root, once the child holds every inherited page. After that any host can open the child. Before that, opening it reports that the fork is pending.

Templates are forks. A guest image is imported once per deployment into a template named by the image's digest, and creating a VM forks that template. All VMs from one image share resident pages by name.

Holds: the parent's pages stay sealed while the child holds the fork point. A hold ends when the child publishes or pulls its pages, when the orchestrator abandons the handoff, or at the host's deadline of four checkpoint intervals.
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

- migration: the source is **released** · fork: the parent **keeps running** · same data
- a record at any other sequence → `ErrStale`; nothing is streamed
- same host: shared pager · other host: pulled over TCP
- a page only the source has is requested **until it arrives**

</div>

</v-clicks>

<!--
A handoff is plain data: the VM and the sequence its record selected when the source released it; the VMM state from the pause; for each region, its name, its size, the runs of pages that no checkpoint has, and the age of the oldest of them; and the address the source serves them from.

A migration hands off a VM the source has released. A fork hands off a child while the parent keeps running. The data is the same, and the destination uses one receive path for both.

The destination refuses a record that selects any other sequence. A migration publishes nothing, so anyone could have opened the VM in between, and streaming pages over another writer's open would silently mix two writers' pages in one VM.

Whether the child is on the same host or another only changes how the unpublished pages arrive: through the shared pager, or over TCP.

A page that only the source has is requested until it arrives, with backoff and no retry limit. The destination cannot distinguish a slow source from a dead one, and its own volume would return data from before the guest's write. The orchestrator ends a migration only when there is evidence that the source host is gone; the VM then reopens from its checkpoint.
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
The host opens VMs over object storage and the cluster network, checkpoints every VM's disks on the interval, answers guest flushes, checks its epochs on a timer, and closes a fenced VM instead of leaving it running. It serves migration and fork pages, owns the pager and the VMM processes, and drains before it exits: it migrates every VM away and waits until it serves no pages.

The orchestrator assigns ids, places VMs, and coordinates both sides of every migration and fork. It polls every host periodically for what it runs and serves, and it ends a migration only when there is evidence the source is gone. Its SQLite table is a cache; the control records are authoritative. If two hosts claim the same VM, it does not report either as the VM's host.

On Kubernetes, hosts are a Deployment with maxSurge 0 and maxUnavailable 1. The preStop drain has 30 minutes to move every VM before the pod is killed. Each node has a 2 MiB HugeTLB pool; the whole control plane uses one bearer token and one object store. Because templates are named by image digest, a restarted host pod does not import anything again.
-->

---

# Stop, start, cold start

<v-clicks>

<div class="text-xl space-y-5 mt-4">

- **stop** — final checkpoint of the disks; close
- **stop --suspend** — also saves memory and VMM state
- **start** — resumes a suspended VM; boots any other
- **cold start** — discard memory and boot; the only time the VM can be resized
- **delete** — remove the record; delete the VM's own checkpoints except pinned ones

</div>

</v-clicks>

<!--
Stop publishes a final checkpoint of the disks and closes the VM. No disk data is lost, and the next start boots the guest from the disks.

Stop with suspend also publishes memory and VMM state, and the next start resumes the guest where it was, with pages faulted in on demand. Memory is uploaded only when requested.

Cold start discards memory in a separate checkpoint and boots the guest instead of resuming it. It is the only time a VM can be resized: more memory, and a larger root volume that the guest then grows into.

Delete removes the record, which frees the id, and then deletes the VM's own checkpoints, except pinned ones that a descendant may still read.
-->

---

# The Firecracker integration

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**Firecracker fork**, branch `sproutfs`

- guest memory mapped through the Rust library: userfaultfd, control protocol v9
- capture = snapshot + resume; interval = pause + seal disks + resume
- a guest flush is **held** until the host answers
- vsock reset only on restore
- `handoff` drops connections when the VM leaves

</div>
<div class="space-y-5">

<div v-click><b>guest</b> — init, exec agent, console, and a <b>witness</b> that fills and checks memory and disk</div>

<div v-click><b>tested on</b> — Lima on aarch64; GCE x86_64</div>

</div>
</div>

<!--
The Firecracker fork is on branch sproutfs, Apache-2.0. Guest memory is a mapping controlled by the Rust library: missing-page and write-protect faults over userfaultfd, the pager's page size, and a control protocol (version 9) between the VMM and the pager. A capture is a snapshot followed by a resume. The interval checkpoint pauses the vCPUs, seals the disks and resumes.

A virtio-pmem flush is a request the device holds, off the VMM thread, until the host answers. Pending flushes are included in the device's snapshot, so they survive a migration and are re-sent to the new host.

The vsock transport is reset only on restore, so a checkpoint does not interrupt guest connections. A handoff flag drops connections when the guest leaves the host.

In the guest: an init that names /dev/root, an agent for exec and the console, and a witness that fills memory and disk with seeded data and checks it after every fork, migration, stop and cold start.

Tested on Linux with KVM: Lima on Apple silicon, and x86_64 on GCE for HugeTLB and the soak test.
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

`testing/synctest` — simulated time costs nothing

</div>
<div class="space-y-5">

<div v-click>operations: <code>Store Checkpoint CheckpointDisks Migrate Fork Stop Suspend Kill Restart …</code></div>

<div v-click>checks: <b>Verify · VerifyDurable · CheckSelected · CheckDeployment · VerifyLossWindow</b></div>

<div v-click>a lost VM comes back at the record's checkpoint, <b>byte for byte</b></div>

</div>
</div>

<!--
A simtest World is one running deployment. Each host is a real host inside a simulated process, with a simulated disk that can lose power, a simulated clock, seeded randomness, and a simulated object store. The components under test are not mocked: the volume managers, the checkpoint store, the control records, the pagers, the page servers and the migration coordinator are the production code. Go's synctest makes simulated time free, so a test that waits through a 60 s interval takes microseconds.

Tests run a list of operations and check a list of properties: no guest reads data it never wrote; the volume returns the same data; every record selects a checkpoint that some writer published; the store is internally consistent; and a lost host loses at most the loss window of a VM's data.

A VM whose host was lost comes back at the checkpoint its record names, and its data must match that checkpoint exactly. If that checkpoint covered only the disks, memory must read as zeroes, because there are no registers to resume. This check found a bug where a disk checkpoint still referenced the registers from an earlier capture.
-->

---

# The test campaigns

| | |
| --- | --- |
| seeded topology | hosts, VMs, forks and faults generated from the seed |
| buggify | also injects faults at named points in the code |
| kill | a host lost at any point of a handoff loses only unpublished data |
| swizzle | links cut and restored independently; two writers never mix |
| recorded | one schedule, replayed exactly |

<v-clicks>

<div class="text-xl mt-6 space-y-3">

- nightly: **8 × 100 seeds** on hosted runners; `just soak <seed> 1` reproduces one
- power loss: writes **applied, dropped, torn or corrupted** · links: **drop, duplicate, delay, slow**

</div>

</v-clicks>

<!--
Each campaign has a soak variant that runs a range of seeds. The nightly run covers eight ranges of 100 seeds on GitHub's hosted runners. A failing seed can be reproduced anywhere with just soak, the seed, and a count of one.

On simulated power loss, unsynced writes are applied, dropped, torn or corrupted. Swizzled links drop, duplicate, delay and slow down traffic.
-->

---

# Measured on a cluster

<div class="text-base opacity-70 mb-3">2 GCE hosts · 512 MiB guests · 166 operations, 146 checks passed · 2026-09-17, before disk-only checkpoints</div>

| | pause | after the pause | total, slowest |
| --- | --- | --- | --- |
| fork, same host | 0.1 s | 1.6–10 s until the child runs | 10 s |
| fork, other host | 0.1 s | 5–20 s post-copy | 20 s |
| migrate | 0.6–0.75 s | 0.3–4.9 s stream | 5.7 s |
| stop | — | — | 6.3 s |
| start, warm | — | — | 1.0 s |
| start, cold, resized | — | resize 0.5 s | 2.1 s |

<v-click>

<div class="mt-4 text-base opacity-70">object store: ~17,000 GETs / 18 GiB · ~1,300 PUTs / 41 GiB · 1,100 deletes</div>

</v-click>

<!--
Two hosts on GCE running 512 MiB Alpine guests, each with a 256 MiB memory witness and a 256 MiB file witness, over six rounds. There were 166 operations and 146 checks, and all passed. Checks ran after every change, fork, migration, stop and warm start, every cold start with more memory and a larger root, and after a host was killed in the last round. At the time of this run, stops and interval checkpoints still saved memory.
-->

---

# Cost of a disk checkpoint

<div class="text-base opacity-70 mb-3">GCE · Cloud Storage · disk checkpointed every 60 s · 2 MiB pages · changed = 4 KiB blocks the guest actually changed</div>

| workload | changed | sealed | published | uploaded | pause |
| --- | --- | --- | --- | --- | --- |
| pnpm install | 1.6 MiB | 212 MiB | 30 MiB | 1.6 MiB | 3 ms |
| cargo build, 7 checkpoints | 6.1 GiB | 11.7 GiB | 6.9 GiB | 1.7 GiB | 6–14 ms |
| Valkey append-only log | 3.0 GiB | 3.1 GiB | 3.0 GiB | 23 MiB | 4 ms |

<v-clicks>

<div class="text-xl mt-6 space-y-3">

- the pause is a few **milliseconds** for every workload
- 2 MiB pages cost **memory**, not upload: unchanged pages are dropped and the rest is compressed
- the upload was never larger than **what changed**

</div>

</v-clicks>

<!--
Measured on GCE against Cloud Storage, with the host checkpointing only the disk every 60 seconds. Changed is the data the guest actually changed, in 4 KiB blocks, measured by checksumming each page when it becomes private and again at the checkpoint. Sealed is the amount covered by 2 MiB pages. Published is what remains after dropping pages whose bytes did not change. Uploaded is after compression.

Small scattered writes, as from a package manager, seal about a hundred times the changed data, but almost all of it is dropped or compressed. Large writes, as from a compiler or a log, seal about as much as changed. Valkey's benchmark writes identical values, so its compression ratio is unrealistically good.
-->

---

# Open issues

---

# Open issues

<v-clicks>

<div class="text-xl space-y-5 mt-4">

- **no garbage collector** — pins are permanent and the store grows; postponed
- **an unreachable source that is still listed** — the migration waits; it needs evidence, not a timeout
- **a drain tries each receive once**
- **a local fork's hold is not visible** to the orchestrator; only the deadline ends it
- **recovery after a real host loss** — tested in simulation, not yet on a cluster
- **disk checkpoints and the flush bound** — not yet tested on GCE

</div>

</v-clicks>

<!--
Garbage collector: pins are permanent, and the store grows without limit until we build a collector. We have postponed it.

If a migration's source is unreachable but still listed, the migration waits. The orchestrator ends a migration only on evidence, never on a timeout, and for a pod it cannot reach it has no evidence beyond what the Kubernetes API reports.

A drain tries each receive once. If a destination is briefly unreachable, the guest loses its writes since its last checkpoint. The source keeps the pages until its deadline, and nothing retries.

A child forked onto its parent's own host holds the fork point without the orchestrator seeing it. Only the host's own deadline ends a hold whose child never publishes.

Recovery after a real host loss is tested in simulation and with fakes, not yet on a cluster. The one soak test's kill hit a host that was running nothing. The next run should use a seed whose kill hits a loaded host.

Disk-only checkpoints, cold boot from a checkpoint without VMM state, and blocking flushes are tested in the simulation, the host test suite and Lima. They have not yet been tested together on GCE.
-->

---

# The repository

```text
cmd/sproutfs-host            the host process
cmd/sproutfs-orchestrator    ids, placement, migrations, forks
cmd/sproutfs-guest-witness   fill / mutate / check / grow, in the guest
internal/checkpoint          the store: parts, index objects, roots, reclamation
internal/control             control records
internal/volume              volumes, publication, forks, handoffs
internal/vmmemory            the pager
internal/vmmigrate           page server and peer backing
internal/host                one host: checkpoint loop, drain, fork, migration, templates
internal/simtest             the simulation harness and its campaigns
internal/platform/sim        simulated processes, disks, clocks, networks, faults
rust/sproutfs-vm-memory      the mappings inside the VMM
third_party/firecracker      the fork, branch sproutfs
docs/  plans/                design, decisions, measurements
```

---

# Summary

<div class="text-xl mb-4">
Cost scales with what the VM changed, not with what it inherited or its size.
</div>

| | pause | data moved |
| --- | --- | --- |
| disk checkpoint | 3–14 ms | about the disk data changed since the last one |
| fork | 0.1 s | none: the child maps the parent's pages by name |
| migration | 0.6–0.75 s | pages no checkpoint has, while the guest runs |
| host loss | — | disk writes since the last checkpoint (at most the loss window), and RAM |

<v-click>

<div class="mt-10 text-center text-2xl">
github.com/semistrict/sproutfs
</div>

</v-click>
