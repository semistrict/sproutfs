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

Three: a checkpoint is a pause followed by an upload, and only the pause affects latency. The pause stops the vCPUs and write-protects the dirty pages. On the interval this covers the disks only; a capture covers every memory region and also saves the VMM state. It takes milliseconds. The upload runs while the guest continues. A fork or migration takes the pause and uploads nothing; the pager moves the unpublished pages to the other side.
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

An fsync is a guest flush that the device holds until the host answers. The host answers immediately if the VM has no unpublished disk write older than the flush bound (twice the checkpoint interval, 120 s by default). Otherwise it answers after the checkpoint it requests has landed. As a result, data is durable within the flush bound plus one interval after an fsync returns, and fsync blocks if the disks cannot be published.
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
The pause stops the vCPUs and write-protects the dirty pages in place, for each memory region being checkpointed. A memory region is one volume mapped into one VMM process. The interval covers the disks; a capture covers every memory region and also saves the VMM state. No data is copied. The sealed pages belong to the checkpoint while the guest keeps running.

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

**memory region** — one volume in one VMM; loads from the volume or a **backing**

</div>
<div class="space-y-5">

<div v-click>Rust <code>sproutfs-vm-memory</code> manages the mappings inside the VMM; it does not depend on Firecracker</div>

<div v-click>a private page reserves its spill slot <b>before</b> the store resumes</div>

<div v-click>bounds are derived from the node and logged at startup</div>

</div>
</div>

<!--
vmmemory manages the host's guest memory. The arena is a fixed-size, sealed HugeTLB memfd with 2 MiB slots. There are explicit budgets for resident, logical and dirty pages; the dirty budget sizes the spill file, which is the pager's total disk allowance. Faults are resolved over userfaultfd, with private copy-on-write, eviction, read-ahead and write-ahead.

A memory region is one volume mapped into one VMM process. It loads pages from the volume, or from a backing placed in front of it. A migration destination uses a backing to read from its source.

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

# The page layer comes from Zircon

<div class="text-base opacity-70 mb-3">Fuchsia's kernel · <code>zircon/kernel/vm</code> at revision <code>90e54e09</code> · MIT licence</div>

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**built for a pager outside the kernel** — pages come from a user-space pager through page requests

**Clean → Dirty → AwaitingClean → Clean** — its writeback is our seal

**eviction splits two kinds** — a page the pager can supply again is dropped; an anonymous page is compressed

**its unit tests** — 261 cases

</div>
<div class="space-y-5">

<div v-click><b>copied, not redesigned</b> — a Go port that reads like its source, line by line</div>

<div v-click><b>the licence</b> — Zircon's <code>LICENSE</code> in <code>vmmemory/internal/zirconvm</code>; every ported file names its source files and the revision, and a test holds each one to it</div>

<div v-click><b>FreeBSD</b> — read for comparison only; some of its <code>sys/vm</code> is BSD-4-Clause, which is not OSI-approved</div>

</div>
</div>

<!--
The pager's page layer is a Go port of the page layer of Zircon, Fuchsia's kernel. It is copied from zircon/kernel/vm at fuchsia revision 90e54e09, and it is not redesigned. We chose Zircon for four reasons.

One: Zircon is built for a pager outside the kernel. A VMO's pages can come from a user-space pager through page requests. The pager supplies pages, is asked before a clean page becomes dirty, and writes dirty pages back. Our pager is that user-space pager, and its sources are the store, the cluster and a peer.

Two: Zircon's dirty tracking is our seal. A page a user pager backs is Clean, Dirty or AwaitingClean. A writeback makes the Dirty pages AwaitingClean and takes write access away, and its end makes them Clean. Our seal does the same while the guest runs.

Three: its eviction makes the same split as ours. A clean page a pager backs is dropped and read again. An anonymous page is compressed. Ours drops a named page and spills a region's own page.

Four: it comes with its tests. Zircon's VM has 261 unit test cases.

Zircon's kernel is under the MIT licence, which is compatible with our Apache-2.0. The package keeps Zircon's LICENSE verbatim. Every ported file begins with its source's copyright line and a line naming the Zircon files it comes from at that revision. A test fails on a file without them, or one that names a file that does not exist at that revision.

FreeBSD's sys/vm was read for comparison. Nothing of it is copied: some of its files are under the four-clause BSD licence, which is not OSI-approved.

Source: plans/zircon-pager-port-2026-10-05.md, Why Zircon and Licence; docs/vm-memory.md, the package layout.
-->

---

# What was taken

<div class="text-base opacity-70 mb-3">from <code>zircon/kernel/vm/</code>, into the pager</div>

<div class="text-lg space-y-3">

<div><code>vm_page_list.cc</code> — the page list: a region's state per page, zero intervals, dirty runs</div>

<div><code>page_queues.cc</code> — the page queues: reclaim order, the don't-need queue, the zero-fork queue</div>

<div><code>compression.cc</code> · <code>slot_page_storage.cc</code> — the spill: a spilled page is a reference to a slot of the spill file</div>

<div><code>evictor.cc</code> — the evictor: an allocation short of a slot evicts</div>

<div><code>page_source.cc</code> · <code>pager_proxy.cc</code> — page requests: a fault's read and a prefetch</div>

<div><code>vm_cow_pages.cc</code> · <code>vm_object_paged.cc</code> — a region's layer and the identity roots; snapshot-on-write only</div>

</div>

<v-click>

<div class="mt-8 text-xl">

**stays ours** — the memfd arena · isolation · the mapping protocol · the userfaultfd connection

</div>

</v-click>

<!--
The page list holds a region's state per page. A page with state of its own has a slot. A run of zeros is a zero interval, which holds its two ends and nothing between them. The pages a seal protects are the list's dirty runs.

The page queues order resident pages for reclaim. An idle page is in the don't-need queue and goes first. A cold copy pins its origin in the zero-fork queue. Aging follows faults only: a userfaultfd pager cannot read the VMM's accessed bits.

The spill keeps Zircon's compressed references. A spilled page is a reference to storage, and the storage is the spill file in the shape of Zircon's slot storage. LZ4 is not ported; a page is stored as it is.

The evictor's synchronous path is what an allocation short of a slot runs. Our fair share is a filter on its candidates.

The page source turns a fault's read and a prefetch into READ requests. A fault that meets a page a prefetch is reading waits on that request. PagerProxy becomes a goroutine per request.

VmCowPages is used twice. Each region has a layer that holds the pages it owns. Each published checkpoint and volume a region reads is an identity root, whose pages are Clean and never change. Zircon finds a page by walking up a chain of parents. A region's content is a mosaic of many checkpoints, so the walk is one lookup by identity instead. Of Zircon's three kinds of snapshot, only snapshot-on-write is taken: named pages never change, and a fork point's pages are frozen while it holds them.

What stays ours: the arena is a set of memfds, not physical memory. Isolation divides those files by who may read them. The mapping protocol issues commands to another process, not page table writes. The userfaultfd connection is how faults reach the pager on Linux.

Source: plans/zircon-pager-port-2026-10-05.md, Zircon's objects and ours, and Part by part.
-->

---

# Five departures

<div class="text-xl space-y-4 mt-4">

<div><b>D1</b> — a store into a page a checkpoint holds gets a copy. Zircon makes the page Dirty again in place.</div>

<div><b>D2</b> — a Dirty or AwaitingClean page can be spilled. Zircon never reclaims one.</div>

<div><b>D3</b> — the pause only write-protects. The walk that marks the pages runs after the vCPUs resume.</div>

<div><b>D4</b> — a failed checkpoint gives its pages back Dirty. Zircon has no abandon.</div>

<div><b>D5</b> — a page holds its spill slot before it is Dirty. Zircon allocates when it compresses, and that can fail.</div>

</div>

<v-click>

<div class="mt-8 text-xl">

`spec/writeback` checks them: **SealedBytes · NoLostWrite · Reserved · Budget**. Zircon's rule for D1, put back as a mutant, fails **SealedBytes**.

</div>

</v-click>

<!--
The port copies Zircon's code except at five places. Each is marked in the ported file beside the line it changes.

D1: Zircon lets a store make an AwaitingClean page Dirty again in place, and the writeback reads bytes that changed after it began. A checkpoint here must hold exactly the bytes of its pause across every page, because a fork's children and a restore read it as one point in time. So the checkpoint keeps its page and the store gets a copy.

D2: RAM is checkpointed only on request, so its dirty set is bounded by the spill and not by writeback. We spill dirty pages, as Zircon compresses an anonymous one.

D3: Zircon's writeback walks every page and then takes write access away. At 4 KiB that walk would be most of the pause. Our pause issues only the range protections, and the walk runs behind it.

D4: a publication that does not land makes each page Dirty again with its own reservation, and revokes its read-only mapping.

D5: a store takes its spill slot before the guest resumes, so a spill never needs space.

spec/writeback models a few pages of one region across a checkpoint: stores, the pause, the walk, eviction, refault, the upload, a fork point, and a publication that lands or fails. SealedBytes: whatever reads a checkpoint reads the bytes of its pause. NoLostWrite: the guest reads what it last stored. Reserved: every private page and every checkpoint's copy owns one reservation. Budget: no reservation is held twice. Zircon's in-place rule fails SealedBytes. The code has the same rules as guards: zircon-dirty-awaiting-clean-in-place and zircon-abandon-leaves-awaiting-clean.

Source: plans/zircon-pager-port-2026-10-05.md, Departures; docs/testing.md, spec/writeback and the zircon guards.
-->

---

# Testing the port

<v-clicks>

<div class="text-xl space-y-5 mt-4">

- **110 of Zircon's 261 unit tests** ported: page list 55 · VMO 33 · page queues 10 · evictor 6 · compression 4 · address space 2
- the page list, page queue and VMO cases run at **4 KiB and 2 MiB**, in simulated bubbles
- the pager, host, migration and simulation suites run under **both cores**, in both arena modes
- every mutation guard is killed under **both cores**: 314 of 314 runs
- the port found a **Zircon bug** — zeroing a child past its parent's end left the parent's bytes showing

</div>

</v-clicks>

<!--
Zircon's VM has 261 unit test cases. 110 of them port. The rest test physical memory, pinning, address spaces, attribution by process and the slab allocator, which do not apply to a pager over memfds. Each ported test has a sentence name, and its comment names the Zircon case it came from. The page list, page queue and VMO cases run in synctest bubbles at both page sizes, because a pager here has one page size for its life, 4 KiB or 2 MiB.

During the port both cores ran behind a switch. The vmmemory, host, vmmigrate, simtest and vmmachine suites ran under each, in the isolated and the shared arena. Every guard in the guard list was killed under both: 314 of 314 runs, 157 guards. On GCE the Linux pager suite passed under the ported core at both page sizes, apart from tests that fail under the old core too.

Mutation testing found a bug Zircon has too. ZeroPagesLocked took a gap as zero when the whole gap did not see the parent. A gap across the parent's end then left its first part showing the parent. The port sends any gap that starts below the parent's end to the walk by offset, and says so beside the line.

Source: plans/zircon-pager-port-2026-10-05.md, Which Zircon tests port; TASK-92.3 to 92.12 notes; docs/testing.md, mutation testing of zirconvm.
-->

---

# The ported core against the old one

<div class="text-base opacity-70 mb-3">Mac M5 Pro · one test binary, cores alternated · medians of 10 runs</div>

<div class="text-base">

| | old core | ported core | |
| --- | --- | --- | --- |
| 4 KiB faults, forward | 347.0 µs | 336.9 µs | −3 % |
| the walk behind a 4 KiB capture, 1,024 dirty pages | 9.80 ms | 9.06 ms | −8 % |
| fork fan-out | 156 ms | 156 ms | equal |
| one 4 KiB fault at random | — | — | **3.6–5 % slower** |

</div>

<v-clicks>

<div class="text-xl mt-6 space-y-3">

- the fault at random's cost is **still being attributed**
- metadata per page a region reads, one page in 512: **21,828 B → 389 B** with the page list
- the old core is **deleted**; only the ported core stays <span class="opacity-70">(owner's decision, 2026-10-06)</span>

</div>

</v-clicks>

<!--
Metadata: the page list replaced blocks of 256 bindings. A region that reads one page in 512 holds 389 bytes for each page it read, against 21,828 with the blocks. That is from sparse_metadata_test.go, 2026-10-05.

The fault and capture numbers are Go benchmarks on the Mac, from one test binary with the core chosen by SPROUTFS_PAGER_CORE, ten runs of each core alternated. A chain of forward 4 KiB faults is 3 % faster. The walk behind a 4 KiB capture's pause is 8 % faster. A fork fan-out in the migration suite takes 156 ms a run under either core.

A single 4 KiB fault at random is slower. At step 12 it was 3,072 ns against 2,858, or 7.5 %. Giving each page a lock that allocates nothing, and smaller frames and bindings, took it to 3.6 to 5 % slower than before step 12. A CPU profile diff shows no single hot spot. We are still finding where the rest goes.

The plan was to switch the default only after a GCE run of the fault chains, the capture pause, a fork fan-out and a warm restore under both cores. On 2026-10-06 the owner decided to delete the old core now and keep only the ported one, without that gate. Forward faults and the walk are faster; the fault at random is the cost.

Source: TASK-92.4 notes (metadata); TASK-92.12 notes (benchmarks); TASK-92 notes, 2026-10-06 (the decision and the 3.6–5 %).
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
Handoff { VMID, Sequence, VMMState, MemoryRegions[] (unpublished runs, age), PeerServer }
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
A handoff is plain data: the VM and the sequence its record selected when the source released it; the VMM state from the pause; for each memory region, its name, its size, the runs of pages that no checkpoint has, and the age of the oldest of them; and the address the source serves them from.

A migration hands off a VM the source has released. A fork hands off a child while the parent keeps running. The data is the same, and the destination uses one receive path for both.

The destination refuses a record that selects any other sequence. A migration publishes nothing, so anyone could have opened the VM in between, and streaming pages over another writer's open would silently mix two writers' pages in one VM.

Whether the child is on the same host or another only changes how the unpublished pages arrive: through the shared pager, or over TCP.

A page that only the source has is requested until it arrives, with backoff and no retry limit. The destination cannot distinguish a slow source from a dead one, and its own volume would return data from before the guest's write. The orchestrator ends a migration only when there is evidence that the source host is gone; the VM then reopens from its checkpoint.
-->

---
layout: section
---

# The cluster's disk cache

---

# Where a restored page comes from

<div class="text-xl mt-6 space-y-5">

1. **this host's memory** — if it was not evicted
2. **the hosts' disks** — every host's local SSD, as **one cache**
3. **the object store** — only when no host holds it

</div>

<v-click>

<div class="mt-10 text-xl">

| | one read, GCE |
| --- | --- |
| a 350 KB object from the hosts' disks, 4+2 | **0.96 ms** median |
| a 2 MiB page from Cloud Storage, 16 in flight | **106 ms** median |

</div>

</v-click>

<!--
A VM stopped hours ago starts again. Each page it touches should come from the closest tier that still has it: this host's memory, then the disks of all the hosts, then the object store. The object store is the last resort.

The hosts run on large, fast local SSDs that were not used for this. A read from another host's disk takes about a millisecond; a GET from the store about a hundred. A guest's faults after a restore are often dependent: the next page is known only when the last one arrives, so each one pays that latency in turn.

The numbers are from two GCE runs on n2-standard-4 hosts: the stripe benchmark, idle, and the restore benchmark's store case.
-->

---

# One cache, not one per host

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**shared** — every VM on a host reads through one cache, whatever its tenant

**distributed first** — no tier of whole local copies

**window** — one volume, one aligned 2 MiB span, one checkpoint

</div>
<div class="space-y-5">

<div v-click><b>membership</b> — one object, changed only by compare-and-set; every request names its generation</div>

<div v-click><b>ranks</b> — weighted rendezvous: each disk scores a window <code>w / −ln(u)</code></div>

<div v-click><b>weight</b> — the disk, in 16 GiB steps, never its moving share</div>

<div v-click>a join or a leave moves <b>at most one</b> holder of a window</div>

</div>
</div>

<!--
The cache is per host, not per VM or tenant. A page is cached once and every VM that names it reads that copy.

There is no tier of whole local copies. A page read whole from the local disk would save about half a millisecond over a read from the cluster, and keeping it whole on every host that reads it would cost the cluster most of its capacity. So the hosts' disks behave as one cache whose size is the sum of their disks.

The unit of placement is the window: the pages of one volume in one aligned 2 MiB span that one checkpoint published. A read-ahead run asks the same hosts for all its pages in one request each.

Placement is computed, not recorded. Every host holds a copy of the membership: one object in the object store that says which disk each host serves, changed only by compare-and-set. Each disk scores a window by w over minus ln u, where u is a hash of the disk's identity and the window, and w is its weight. Rendezvous ranks the next disk exactly, which repair depends on. The comparison is done in integers, so hosts of different architectures rank alike. Every request between hosts names the generation of the membership its sender holds. A host behind it reads the object first; a host ahead of it says the sender is stale. So two hosts never exchange a stripe under different memberships.
-->

---
clicks: 7
---

# Reed-Solomon stripes

<ErasureCode />

<!--
A window's envelopes are split with Reed-Solomon, klauspost/reedsolomon, into k data stripes and m parity stripes. Any k of the k+m rebuild the envelope. At 4+2 that is 1.5 times the bytes of one copy, and survives two hosts lost or slow.

A fill puts stripe i on rank i, but no reader relies on that. Ranks shift when hosts join and leave, so a holder may hold any index. A reader asks k+1 holders, picked by a hash of reader and window so the readers of a hot page spread over every holder, and rebuilds from the first k distinct indices that arrive. A miss is replaced at once. If k have not arrived after the 95th percentile of recent reads, it asks the rest, within a budget that grows a twentieth of a request per fast read.

Every stripe carries its key, index, code and CRC32C, and the rebuilt envelope carries its XXH3-128. A wrong stripe is never returned: another set of k is tried, and its holder is told to drop it.
-->

---

# Which code

| hosts | code | extra disk | survives |
| --- | --- | --- | --- |
| 1 | 1+0 | none | nothing |
| 2 | 1+1 | 100 % | one host lost or slow |
| 3 | 2+1 | 50 % | one host lost or slow |
| 4 or 5 | 2+2 | 100 % | two hosts lost or slow |
| 6 or more | **4+2** | 50 % | two hosts lost or slow |

<v-click>

<div class="mt-6 text-lg">

GCE, one host drained and one stalled: **4+2** p99.9 1.95 ms · **4+1** 67 % of reads timed out · **whole copies** 14 % misses, 19 % timed out

</div>

</v-click>

<!--
The code is a deployment setting, written in the membership, not derived from the hosts that are up. A drain takes six hosts to five for a while, and a code that followed them would turn every stripe into a miss. While the membership holds fewer disks than k+m, stripes go round the disks it has.

k = 1 is whole copies: 1+1 keeps each envelope whole on two hosts, so two hosts are enough. Replication is not a second mechanism; it is the code at k = 1.

On six GCE hosts, the stripe benchmark drained one host and stalled another, as a rolling restart with one bad host does. Only 4+2 kept every read fast. 4+1 survives one or the other. Whole copies survive neither.
-->

---

# The bandwidth budget

<div class="text-base opacity-70 mb-3">six GCE n2-standard-4 hosts, 10 Gbps · every host reading and serving at once · p99 per round</div>

| served per host | p99 |
| --- | --- |
| 4.4 Gb/s | under 2 ms in 14 of 15 rounds |
| 5.2–5.3 Gb/s | 1.9 to 79 ms |
| 6.3 Gb/s | never under 89 ms |

<v-clicks>

<div class="text-xl mt-6 space-y-3">

- the tail follows **bytes served per host** — not CPU, GC, decoding or queueing
- under 4+2 every host holds a stripe of every object, so a **drain moves load** onto the rest
- a host serves at most **500 MiB/s** of stripes, and answers **BUSY** past it

</div>

</v-clicks>

<!--
The first full-load run made 4+2 look worse than whole copies. Tracing it showed the cause was bytes served per host, not the code: no host used more than 1.7 of its 4 CPUs, GC pauses were under 2 ms, and server queueing under 0.3 ms. Past about half the NIC's rate, replies waited in the network.

Under 4+2 every host holds a stripe of every window, so a drained host's share moves onto the other five. Under 4+1 the host that stands in holds nothing to send. So serving is a budget: a deployment keeps each host under about 40 % of its NIC after a drain, until its own machine type is measured.
-->

---

# Autoscaling and a cache on the hosts' SSDs

<div class="text-base opacity-70 mb-3">an autoscaler adds and removes hosts through the day · 4+2 on ten hosts</div>

<v-clicks>

<div class="text-xl space-y-6 mt-4">

<div><b>a join hides stripes</b> — the new host enters the first six ranks of 6/11 of windows, <b>about 55 %</b>; each loses a stripe its readers no longer ask for, until a read repairs it</div>

<div><b>a leave loses stripes</b> — the host's disk goes with it; three removals before repair lose <b>about 17 %</b> of windows</div>

<div><b>the code followed the host count</b> — crossing a threshold turned <b>every</b> window into a miss <span class="opacity-70">(fixed: the code is a setting, and a stripe is read by its own code)</span></div>

</div>

</v-clicks>

<!--
The cache was designed for hosts that stay. An autoscaler changes the host count through the day, and a cache whose disks are the hosts' own pays for each change.

A join: rendezvous puts the new host among a window's first k+m ranks for about k+m over N+1 of the windows, six in eleven when a tenth host joins under 4+2. Each of those windows now has one stripe on a host its readers no longer ask, and only a read of that window repairs it. Several joins with no reads in between push cold windows below four reachable stripes.

A leave: the host's local SSD goes with the host. Under 4+2 a window survives two lost stripes, so three removals before repair catches up lose the windows with a stripe on all three: C(6,3) over C(10,3), about 17 % of them.

The code: with none configured, the orchestrator picked the code from the most hosts it had seen, and a stripe of another code was a miss, so crossing a threshold emptied the cache. That is fixed: the code is a deployment setting, and every stripe is read by the code it was stored under.
-->

---

# Shards on network disks

<div class="text-base opacity-70 mb-3">decided 2026-10-03 · not built yet</div>

<div class="grid grid-cols-2 gap-10 mt-2 text-xl">
<div class="space-y-5">

**a fixed set of shards** — each one network disk with the disk log on it

**ranked over shards**, never hosts — scaling compute moves no window

**every cloud has it** — Hyperdisk Balanced on GCP, gp3 on AWS

</div>
<div class="space-y-5">

<div v-click><b>a shard moves</b> — detached and attached to another host in seconds, which autoscaling can afford</div>

<div v-click><b>the membership assigns shards</b> — one object in the store, changed by compare-and-set; a shard is released before it is assigned again</div>

<div v-click><b>fenced</b> — a host serves a shard only under the generation that assigns it there</div>

</div>
</div>

<!--
The fix is to stop tying the cache's disks to the hosts. The cache becomes a fixed number of shards, each a network disk with the same disk log on it. Windows are ranked over the shards, so the ranking changes only when the cache is resized on purpose, and adding or removing compute moves nothing.

Network disks are the oldest storage every cloud offers: Persistent Disk and Hyperdisk on GCP, EBS on AWS. Nothing needs allowlisting. A network disk outlives the machine it is attached to, and can be detached and attached to another machine in seconds.

Which host serves which shard is in the membership: one object in the object store, changed only by compare-and-set on its generation. Correctness rests on that alone; usually the orchestrator makes the changes, one step at a time. A shard is released from one host before it is assigned to another, and a host serves a shard only under the generation that assigns it, so a host that lost a shard can never serve it again. While a shard moves, reads hedge around it, as they do around a slow host.

The cost is latency: a network disk is below a millisecond by Google's figures, where local NVMe is faster but dies with the machine.
-->

---

# Filling the cluster

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**a store read** — split and sent to the ranks *behind* the read

**a publication** — each part once its PUT succeeded

**a pull** — what it copies

</div>
<div class="space-y-5">

<div v-click><b>keep</b> — a peer-server request; a holder takes only what its own list ranks it for</div>

<div v-click><b>fill right</b> — rank 1 gives one per window per 10 s, so a cold burst fills once</div>

<div v-click><b>nothing waits on a fill</b> — a full queue or a spent rate drops it</div>

</div>
</div>

<!--
Three things bring a window to the cluster. A store read splits what the store served and sends each stripe to its rank once the read's callers have their pages. A publication fills each part once its PUT has succeeded, so no cache holds bytes the store refused; an interval checkpoint, a capture, a stop, a fork point and a template import all fill. A pull's copies are fills too.

A keep carries a holder's stripes as its disk stores them. A holder refuses a window its own list does not rank it for, and drops stripes it already holds or is writing.

A cold burst would fill one window many times. So a read's fill needs the window's fill right, which rank 1 gives to the first reader that asks, once per window per ten seconds, as Memcache's leases do.

A fault, a publication and a pull never wait for a fill. Fills go through one 64 MiB queue per host, within 128 MiB/s and the host's background budget; anything over is dropped, and the window is read from the store next time.
-->

---

# On one cache disk

<div class="grid grid-cols-2 gap-10 mt-4 text-xl">
<div class="space-y-5">

**a log of 64 MiB regions** — each item has its key and CRC32C

**a table** at each region's end — sync the items, write the table, sync

**FIFO eviction** — a second chance for what was read, at most half a region

</div>
<div class="space-y-5">

<div v-click><b>restart</b> — tables read back in order; the open region given back; a torn table scanned</div>

<div v-click><b>one disk limiter</b> — goals: % free, bytes free, bytes used; the strictest wins</div>

<div v-click><b>outlives the machine</b> — today a <code>hostPath</code> file per host; next, a network disk per shard</div>

</div>
</div>

<!--
Each cache disk is one file, today on a host's local SSD and next on a shard's network disk, written as a log of 64 MiB regions, allocated whole when they open. Each item has a header with its key, stripe index, code and CRC32C. When a region fills, its items are synced, a table is written at its end, and that is synced too. Eviction takes the oldest region; the stripes in it that were read most get a second chance in the open region, at most half a region, and none when the cache is over its share. One region is always kept free for that.

On restart, the host reads every region's table back in sequence order. The region that was open at a crash is given back. A region whose table is torn is scanned by item headers. A file of another deployment is emptied.

One limiter bounds everything the host writes: spill files, ephemeral disks, staging and the cache. It follows any combination of goals, the strictest winning, and gives space back gradually as the disk nears them. Today the cache lives in a hostPath directory that outlives the pod, one locked file per host. With shards it lives on a network disk that outlives the machine.
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

a **real** deployment in one process: real hosts, pagers, peer servers, store code

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
A simtest World is one running deployment. Each host is a real host inside a simulated process, with a simulated disk that can lose power, a simulated clock, seeded randomness, and a simulated object store. The components under test are not mocked: the volume managers, the checkpoint store, the control records, the pagers, the peer servers and the migration coordinator are the production code. Go's synctest makes simulated time free, so a test that waits through a 60 s interval takes microseconds.

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

# The model found two bugs in the plan

<div class="text-base opacity-70 mb-3">TLA+, before any code: <code>spec/diskcache</code> · <code>spec/disklog</code> · <code>spec/disklimit</code> · every run under a couple of minutes</div>

<v-clicks>

<div class="text-xl space-y-6 mt-4">

<div><b>B4</b> — a sparse spill file's promised space is only free space. Another writer takes it, the cache gives everything back, and a guest's dirty page still cannot spill. <span class="opacity-70">Fix: allocate spill files whole.</span></div>

<div><b>B5</b> — a join moves every later stripe off its rank. A reader that asks rank i for stripe i cannot decode, though k stripes exist. <span class="opacity-70">Fix: take any index; repair only an index no rank holds.</span></div>

</div>

</v-clicks>

<!--
The disk cache was modelled before it was built, in three specs, one per concern, so that every TLC run ends in a couple of minutes: the cluster, one host's log, and the limiter. Each has mutants that must fail its invariants.

B4: the limiter counted each spill file at its full promise, but a sparse file's unused promise is just free filesystem space. Anything else on the node could take it, and then a guest's store would need to spill and could not, with the cache already empty. Spill files are now allocated whole when their pager starts, and slots are not punched when released.

B5: the plan put stripe i on rank i and had readers ask rank i for it. A host joining near the top of a window's ranks shifts every holder below it, so the reader would find the wrong index everywhere and fall back to the store, and repair would write duplicates. Readers now take any index, and repair sends only an index no rank holds.
-->

---

# Reading a guest from the cluster

<div class="text-base opacity-70 mb-3">six GCE n2-standard-4 hosts · 4+2 · Cloud Storage · 8 GiB of 2 MiB pages read back on another host, 16 in flight · 3 rounds</div>

| | total | p50 | p99 | p99 spread | slowest page |
| --- | --- | --- | --- | --- | --- |
| from the cluster | 16.4 s | 58 ms | 136 ms | 3.3 ms | 202 ms |
| one host lost mid-read | 16.4 s | 57 ms | 134 ms | 4.5 ms | 200 ms |
| from the store | 28.1 s | 106 ms | 218 ms | 51 ms | 845 ms |

<v-clicks>

<div class="text-xl mt-6 space-y-3">

- losing a host cost **nothing**, and no page came from the store
- each holder served ~1.7 GB, within **2.5 %** of the others
- from the cluster the reader was **CPU-bound**: 3.9 of its 4 CPUs

</div>

</v-clicks>

<!--
Host 0 published 8 GiB of incompressible pages, and the publication's fills put each window's stripes on its six ranks. Host 1 read every page back, 16 at a time, through the cluster, through the store, and through the cluster with host 3's peer server closed two seconds in.

The cluster was 1.7 times as fast with a tight tail; the store's tail moved by 51 ms between rounds. A lost host changed nothing: its requests were replaced at once and it was marked down on the refused connection.

This is a bulk sequential read, and both paths were limited by the reader's CPU per page, not by where the page came from. The cluster's real advantage is a dependent fault, which pays one read's latency at a time; that is measured next.
-->

---

# Open issues

---

# Open issues

<v-clicks>

<div class="text-lg mt-4">

- **no garbage collector** — pins are permanent and the store grows; postponed
- **an unreachable source that is still listed** — the migration waits; it needs evidence, not a timeout
- **a drain tries each receive once**
- **a local fork's hold is not visible** to the orchestrator; only the deadline ends it
- **recovery after a real host loss** — tested in simulation, not yet on a cluster
- **disk checkpoints and the flush bound** — not yet tested on GCE
- **the cluster cache is off in the deployment** — its share of windows is 0 until the rollout raises it
- **shards on network disks** — designed for autoscaling, not built yet
- **serving copies through memory** — not yet `sendfile`
- **the page layer ported from Zircon** — a 4 KiB fault at random is 3.6–5 % slower, not yet attributed; not yet timed on GCE

</div>

</v-clicks>

<!--
Garbage collector: pins are permanent, and the store grows without limit until we build a collector. We have postponed it.

If a migration's source is unreachable but still listed, the migration waits. The orchestrator ends a migration only on evidence, never on a timeout, and for a pod it cannot reach it has no evidence beyond what the Kubernetes API reports.

A drain tries each receive once. If a destination is briefly unreachable, the guest loses its writes since its last checkpoint. The source keeps the pages until its deadline, and nothing retries.

A child forked onto its parent's own host holds the fork point without the orchestrator seeing it. Only the host's own deadline ends a hold whose child never publishes.

Recovery after a real host loss is tested in simulation and with fakes, not yet on a cluster. The one soak test's kill hit a host that was running nothing. The next run should use a seed whose kill hits a loaded host.

Disk-only checkpoints, cold boot from a checkpoint without VMM state, and blocking flushes are tested in the simulation, the host test suite and Lima. They have not yet been tested together on GCE.

The cluster cache is built and measured, but the deployment turns it on for none of its windows yet. A setting raises the share of windows gradually, as mcrouter's shadowing does, once the pull asks the cluster first.

The cache's disks are still the hosts' own SSDs. Shards on network disks, which keep the cache whole through autoscaling, are designed and tracked as TASK-86, and not built yet.

A host serving stripes still reads them into memory and writes them out. Sending them from the disk with sendfile is the next step, if a plain copy turns out to cost enough to matter.

A single 4 KiB fault at random is 3.6 to 5 % slower under the page layer ported from Zircon. A CPU profile diff shows no single hot spot, and we are still finding where it goes.

The plan measured the ported core against the old one on GCE before switching: fault chains, the capture pause, a fork fan-out and a warm restore. On 2026-10-06 the owner waived that gate, and the old core is deleted. The ported core has passed the Linux suites on GCE, but no GCE run timed it against the old one.
-->

---

# The repository

```text
cmd/sproutfs-host            the host process
cmd/sproutfs-orchestrator    ids, placement, migrations, forks
cmd/sproutfs-guest-witness   fill / mutate / check / grow, in the guest
checkpoint          the store, and the page cache: memory, disk log, fills, cluster reads
membership          the membership object, its steps, each host's copy
rank                windows, rendezvous ranking over the disks
stripe              Reed-Solomon split and join, finding a wrong stripe
resource            budgets and the disk limiter
control             control records
volume              volumes, publication, forks, handoffs
vmmemory            the pager
vmmemory/internal/zirconvm   the page layer, ported from Zircon (MIT)
peer                the peer server: frames, peers, classes, liveness
vmmigrate           handoffs and the peer backing
host                one host: checkpoint loop, drain, fork, migration, templates
internal/simtest             the simulation harness and its campaigns
platform/sim        simulated processes, disks, clocks, networks, faults
rust/sproutfs-vm-memory      the mappings inside the VMM
third_party/firecracker      the fork, branch sproutfs
docs/  plans/                design, decisions, measurements
```

---

# Summary

<div class="text-xl mb-4">
Cost scales with what the VM changed, not with what it inherited or its size.
</div>

<div class="text-base">

| | pause | data moved |
| --- | --- | --- |
| disk checkpoint | 3–14 ms | about the disk data changed since the last one |
| fork | 0.1 s | none: the child maps the parent's pages by name |
| migration | 0.6–0.75 s | pages no checkpoint has, while the guest runs |
| restore from the cluster | — | pages from the hosts' disks; a lost host costs nothing |
| host loss | — | disk writes since the last checkpoint (at most the loss window), and RAM |

</div>

<div class="mt-3 text-lg opacity-80">
The pager's page layer is a port of Zircon's, with 110 of its tests and five departures a spec checks.
</div>

<style>
td, th { padding-top: 0.4rem; padding-bottom: 0.4rem; }
</style>

<v-click>

<div class="mt-10 text-center text-2xl">
github.com/semistrict/sproutfs
</div>

</v-click>
