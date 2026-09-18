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
layout: section
---

# The problem

---

# A VM's memory is the expensive part of it

<v-clicks>

- A sandbox that boots is slow. A sandbox that **restores** is fast — until you have to copy its memory to the host it restores on.
- A fleet of similar sandboxes — one per agent, one per test shard, one per pull request — is the same gigabytes of RAM and disk **written again and again**.
- Moving a running VM to drain a host means **copying its memory while it changes**, and a pre-copy that never converges.
- Every snapshot format that stores memory as a file makes *sharing* a deduplication problem, and dedup by content is a hash table you have to keep somewhere.

</v-clicks>

<v-click>

<div class="mt-8 p-4 border border-gray-600 rounded text-lg">
Sproutfs treats a VM's memory and disks as the <b>same kind of thing</b> — a byte-addressed image with one writer — and never copies what a VM inherited.
</div>

</v-click>

---

# Three decisions

Everything else follows from these.

<v-clicks>

1. **A VM's durable state is exactly one published checkpoint**, selected by its control record. Nothing is durable between checkpoints. Losing a host loses every write since its VMs' last checkpoints — and that is accepted.

2. **Stored data and resident frames share through lineage, never through content.** Two VMs share a frame because they inherited the same checkpoint's bytes. No hashes, no byte comparison. A page no checkpoint holds reads as zeroes.

3. **A checkpoint's instant is separable from its upload**, and only the instant is on anyone's latency path. A fork and a migration take the instant and publish nothing: the unpublished pages reach the other side through the pager.

</v-clicks>

---
layout: section
---

# The model

---

# One VM

<div class="grid grid-cols-2 gap-8">
<div>

**Identity.** One id, never reused. The orchestrator hands them out; the store refuses a create whose prefix has leftovers.

**Control record.** `control/<id>`: the one mutable object. Epoch, nonce, selected checkpoint, pins.

**Volumes.** `ram0` and each PMEM disk. Byte-addressed, fixed size, **one writer**.

**Pages.** 2 MiB. The unit of publication, of a fault, of resident ownership. In the store, in the pager and on the wire.

</div>
<div>

**Checkpoint.** The operation *and* what it leaves behind: `vm/<id>/ckpt/<seq>/`.

**Lineage.** A fork's root names its parent's checkpoints. A checkpoint's pages get an identity `(checkpoint, volume, page)` that never changes — compaction moving the bytes included.

**Writer.** The process holding the epoch. Every open advances the epoch and fences the writer before.

</div>
</div>

<div class="mt-6 text-sm opacity-60">
docs/context.md is the vocabulary; every doc uses one word for each thing.
</div>

---

# The loss model

<div class="grid grid-cols-2 gap-8">
<div>

A guest store lands in a **resident frame**. It contacts nothing. It cannot fail for a network or storage reason.

It is not durable.

What makes it durable is the **next checkpoint**: the dirty pages upload as parts, the index object carries the root, and a conditional write selects the checkpoint in the control record.

Losing the host before that loses every write since the last selected checkpoint.

</div>
<div>

<v-click>

**Why accept it?** A guest write must never wait on object-store latency. The alternative — a write-ahead log per VM — puts a network round trip on the fault path of every first store.

</v-click>

<v-click>

**How much?** The interval is 60 s by default, jittered by an eighth so VMs do not checkpoint in lockstep, measured from the end of the last upload. `Status.DirtyBytes` is the bound on what a VM would lose right now.

</v-click>

<v-click>

**What a flush means.** Nothing. `virtio-pmem` flush completes in the device; the host is not asked. Only a checkpoint is a durability acknowledgement.

</v-click>

</div>
</div>

---
clicks: 7
---

# A checkpoint

<CheckpointInstant />

---

# The seal is page-table work

<div class="grid grid-cols-2 gap-8">
<div>

The pause is three things: stop the vCPUs, save the VMM state, **write-protect** every dirty page of every region in place.

No byte moves. The frames become the checkpoint's while the guest keeps running on them.

A store into a sealed page copies **that one page** — a private 2 MiB frame — and charges the dirty budget for it. The checkpoint goes on reading the sealed original.

</div>
<div>

<v-click>

Measured on GCE (`docs/measurements-*`):

| | pause |
| --- | --- |
| checkpoint, 2 GiB guest under `pnpm install` | 7–9 ms |
| fork instant, 512 MiB guest | 0.1 s |
| migration stop, 512 MiB guest | 0.6–0.75 s |

</v-click>

<v-click>

The upload runs behind the guest. A checkpoint that fails to publish hands its pages back: the previous checkpoint stays selected, nothing durable changed.

</v-click>

</div>
</div>

---
clicks: 3
---

# The store: two planes

<TwoPlanes />

---
clicks: 3
---

# The root and its segments

<RootSegments />

---

# Why no parent pointer

<div class="grid grid-cols-2 gap-8">
<div>

A root **carries forward** every segment address its checkpoint did not change. So opening a VM is:

1. GET the control record → selected sequence
2. GET that checkpoint's index → the root
3. fetch the segments the pages you touch fall in

Nothing walks a chain. Nothing rebuilds an index from a log.

</div>
<div>

<v-click>

The cost is a root that names every segment of every volume: about fifteen bytes per 512 MiB of volume, bounded at **2 MiB** — some 140,000 segments, 70 TiB of volume per VM.

</v-click>

<v-click>

The root also carries **per-segment, per-checkpoint byte sums**: how many live bytes each earlier checkpoint still contributes. Liveness comes from the root alone. Compaction and reclamation never scan.

</v-click>

</div>
</div>

---

# Publication order is the commit protocol

<v-clicks>

1. **Parts** upload as they fill, 64 MiB each, behind the guest. A part that never completes is garbage under a key nothing names.
2. **The index object** is written last, with a **create-if-absent PUT**. That is the commit: the object either exists whole or not at all.
3. **The control record** selects the sequence with a conditional write from the handle's own epoch.
4. **Retire the seal**: the sealed pages become clean under the new lineage. First thing after the selection, last thing under the publication lock.
5. **Reclaim**: delete the checkpoints the old root named that the new one does not, less every pin. Whole checkpoints, index object first.

</v-clicks>

<v-click>

<div class="mt-4 text-sm opacity-70">
A lost reply is reconciled by reading back: only the writer of an epoch can produce a record carrying that epoch's nonce, so "did my write land?" always has an answer.
</div>

</v-click>

---
clicks: 4
---

# Fencing

<Fence />

---

# Sequences are epoch-major

```text
sequence = (epoch << 32) | counter          counter starts at 1 per epoch
```

<v-clicks>

- Every sequence a writer allocates is above everything an earlier epoch could allocate.
- A fenced writer still uploading cannot collide: every checkpoint object is create-if-absent under a key its successor never uses.
- The first epoch is **drawn at random** in `[1, 2³¹)`. Two VMs created under one identity — which the orchestrator never does but nothing can enforce — allocate different sequences, different lineage identities, different keys.
- A fenced host finds out at its next publication, or sooner: `VM.Confirm` re-reads the record on a timer, and a handoff confirms before it pauses — the frames it hands another host are the one thing the store cannot refuse afterwards.

</v-clicks>

---

# Reclamation, compaction, and what is deferred

<div class="grid grid-cols-2 gap-8">
<div>

**Reclamation is a set difference.** After selecting a root: delete the checkpoints the replaced root named, less everything the new root names, less every pin. Only this VM's own checkpoints. Whole.

**Compaction bounds the residue.** A checkpoint less than half live is rewritten into the one being published, up to 64 MiB of live bytes, after the guest resumed. It falls out of the root and reclamation deletes it.

</div>
<div>

<v-click>

**Pins are permanent.** A fork pins the parent's published sequence. Nothing in a deployment can tell that a lineage has ended: a descendant sees neither its siblings nor the forks below it.

</v-click>

<v-click>

**The collector is deferred, by decision.** Until one exists the store grows without bound: every checkpoint a VM was forked at, everything its root names, and the lineage a deleted VM leaves behind are kept forever. `docs/open-work.md` says so first.

</v-click>

</div>
</div>

---
layout: section
---

# The pager

---

# One host-wide pager

<div class="grid grid-cols-2 gap-8">
<div>

`internal/vmmemory` owns the host's guest memory:

- an **arena**: a fixed-size sealed HugeTLB memfd of 2 MiB slots
- explicit **resident, logical and dirty budgets** — the dirty budget sizes a **spill file**, which is the pager's whole disk cap
- fault resolution over userfaultfd, private copy-on-write, eviction, read-ahead and write-ahead

A region is one volume attached to one VMM process. Loads ask the volume, or a backing put in front of it.

</div>
<div>

<v-click>

The Rust library `sproutfs-vm-memory` owns the mappings inside the VMM process. It has no Firecracker dependency; the Firecracker fork maps its guest memory through it.

</v-click>

<v-click>

Every private page takes a spill slot **before** the write resumes, so a store never has to ask anything for room. Pool exhaustion is an allocation error, not a fallback to small pages.

</v-click>

<v-click>

Production bounds are chosen by the supervisor from the node — read-ahead 8 MiB, I/O permits four per processor — and logged at start.

</v-click>

</div>
</div>

---
clicks: 4
---

# Sharing by lineage

<LineageShare />

---

# What identity buys

<v-clicks>

- **Population before vCPUs run.** A restored or forked machine maps every page whose identity is already resident in the pager, without a load. A fork's eager population maps the parent's resident set.
- **No dedup table.** The sharing index is keyed by `(checkpoint, volume, page)`, which the volume already knows. Nothing hashes 2 MiB.
- **Sealed frames get a name.** A fork point names the frames it sealed under a reference that publishes nothing; every child of that instant maps them. The name lasts exactly as long as the seal.
- **Sparse zeroes cost nothing.** A page no checkpoint holds maps the shared zero page. The first store replaces the whole 2 MiB range with a private frame.
- **A one-byte store costs a page.** 2 MiB copied, 2 MiB charged, 2 MiB published. This is the trade the workload measurement examines.

</v-clicks>

---
layout: section
---

# Fork and migration

---
clicks: 4
---

# A fork is one instant, any number of children

<ForkFanOut />

---

# What a fork costs and leaves

<div class="grid grid-cols-2 gap-8">
<div>

**Costs.** One pause on the parent — the same instant as a checkpoint — and the child's boot. Nothing uploaded. On GCE, forking two children took 0.1 s of pause; the children ran 1.6–10 s later.

**Leaves.** A pin on the parent's published sequence, and one control record per child selecting a root over it. A fork that ends before it leaves no object behind.

**Publishes.** The child's root, as soon as the child holds every inherited page — that is what makes it a VM any host can open. Before that, opening it anywhere reports `ErrForkPending`.

</div>
<div>

<v-click>

**Templates are forks too.** A guest image is imported once per deployment into `template-<sha256 of the image>`; a create is a fork of that template. Every VM of one image shares its frames by lineage.

</v-click>

<v-click>

**Holds.** The parent's frames stay sealed while any child holds the instant. A hold ends when the child publishes or pulls its pages, when the orchestrator gives the handover up, or at the host's deadline of four checkpoint intervals.

</v-click>

</div>
</div>

---
clicks: 7
---

# Migration is post-copy only

<PostCopy />

---

# The one post-copy rule

<div class="text-xl mt-4 mb-6 p-4 border border-yellow-600 rounded">
A page only the source holds is asked for <b>until it arrives</b>, or until something that <b>knows</b> says the source is gone.
</div>

<v-clicks>

- An unpublished page is never satisfiable from the destination's own volume: the checkpoint there predates the guest's write. Reading it would **rewind the guest** silently.
- Nothing in a destination can tell a source that stumbled from one that died. A `BUSY`, a reset connection, a timeout, a restarting listener: all are asked again, with backoff. **No attempt count, no failure threshold.**
- Two things end the asking: the source itself answering that it no longer serves the VM — which it does only after a release it agreed to — or the orchestrator ending the migration because it has **positive evidence** the source host is gone.
- Then the VM is recovered from its checkpoint, rewound by the writes since. The same window any host loss costs.

</v-clicks>

---

# The handoff is plain data

```text
Handoff {
  VMID, Sequence          the checkpoint the record selected when the source gave the VM up
  VMMState                registers, devices — what the pause captured
  Regions[]               name, size, and the runs of pages no checkpoint has
  PageServer              where the source serves them from
}
```

<v-clicks>

- A migration hands off a VM the source **released**. A fork hands off a child from a parent that **keeps running**. Same data, one receive path on the destination.
- The destination refuses a record selecting any other sequence with `ErrStale`: a migration publishes nothing, so that record was openable by anyone in between, and streaming frames over another writer's open would make one VM's memory out of two writers' pages with no error anywhere.
- Local child or remote child changes only how the unpublished pages arrive: through the shared pager, or pulled over TCP.

</v-clicks>

---
layout: section
---

# The deployment

---
class: text-sm
---

# Host and orchestrator

<div class="grid grid-cols-2 gap-8">
<div>

**Host** — `internal/host`, `cmd/sproutfs-host`

- opens VMs over object storage and the cluster network
- checkpoints every VM on the interval
- confirms its epochs on a timer; a fenced VM is closed, not left running
- serves migration and fork pages
- owns the pager and the VMM processes
- drains on `preStop`: migrate every VM, wait until it serves nothing

</div>
<div>

**Orchestrator** — `cmd/sproutfs-orchestrator`

- allocates identities, places VMs, drives both halves of every migration and fork
- watches a migration's source and ends the migration only on positive evidence
- its SQLite table — `creating running migrating stopped recovering` — is a **view**; the control records are the authority
- refuses to name a VM's host while two hosts claim it

</div>
</div>

<v-click>

<div class="mt-6 text-sm opacity-70">
Kubernetes: hosts are a Deployment with maxSurge 0 / maxUnavailable 1, a 2 MiB HugeTLB pool per node, one bearer token for the whole control plane, one object store. Templates named by image digest mean a rolled host pod imports nothing again.
</div>

</v-click>

---

# Stop, start, cold start

<v-clicks>

- **Stop** publishes a final checkpoint and closes the VM. The difference between a stop and a host loss is exactly the writes since the last checkpoint.
- **Start** reopens it, warm: the VMM restores from the checkpoint's captured state; the pages fault in.
- **Cold start** discards the memory in a checkpoint of its own — a `DiscardMemory` publication — and boots the guest instead of resuming it. The one place a VM's shape may change: `--memory`, and `--disk` grown from inside the guest with the ext4 resize ioctl, because the guest kernel refuses a write open of its mounted root.
- **Delete** removes the record — which frees the identity — then sweeps the VM's own checkpoints, except the pinned ones a descendant may still read.

</v-clicks>

---

# The Firecracker integration

<div class="grid grid-cols-2 gap-8">
<div>

A fork of Firecracker, branch `sproutfs`, Apache-2.0:

- guest memory is a mapping the Rust library controls: missing-fault and write-protect traps over userfaultfd, 2 MiB frames, a control protocol (version 6) between the VMM and the pager
- checkpoint = snapshot + resume; the pause is the VMM's own
- the vsock transport is reset only on restore — a snapshot leaves a guest's connections alone (found on GCE: every checkpoint was killing every command in the guest)
- a `handoff` flag drops connections when the guest leaves the host

</div>
<div>

<v-click>

Guest side: an init that names `/dev/root`, an agent for exec and console, and a **witness** that fills memory and disk with seeded data and checks it — after every fork, migration, stop and cold start.

</v-click>

<v-click>

Qualified on Linux with real KVM: Lima on Apple silicon (aarch64), and x86_64 on GCE for HugeTLB and the soak.

</v-click>

</div>
</div>

---
layout: section
---

# Evidence

---

# One simulation harness

<div class="grid grid-cols-2 gap-8">
<div>

`simtest.World` is one running deployment: every host is a **real** `host.Host` inside a `sim.Process`, on a `sim.Disk` with power-loss faults, against a `sim.Clock` and seeded entropy, reaching a simulated object store.

Nothing in it is a mock of the thing under test. The volume managers, the checkpoint store, the control records, the pagers, the page servers and the migration coordinator are the real ones.

Go's `testing/synctest` makes simulated time free: a campaign that waits out a 60 s interval costs microseconds.

</div>
<div>

<v-click>

Campaigns drive it with one short list — `Store Checkpoint Migrate Fork Delete Takeover Kill Restart Shutdown Settle KillDuring` — and require one short list:

- **Verify**: no guest reads bytes it never wrote
- **VerifyDurable**: the same bytes through the volume
- **CheckSelected**: every record selects a checkpoint some writer published
- **CheckDeployment**: the store agrees with itself

</v-click>

<v-click>

A VM that lost its host comes back at the checkpoint its record names, and the bytes must be **that instant's**, page for page. An interrupted publication is answered for exactly.

</v-click>

</div>
</div>

---

# What the campaigns do to it

| campaign | what it explores |
| --- | --- |
| seeded topology | hosts, VMs, forks and a fault schedule all drawn from the seed |
| the same, buggified | plus per-site fault injection with probes and a fingerprint |
| the kill campaign | a host lost at any of its handovers loses only what no checkpoint held |
| the two-writer swizzle | every link blocked and healed at its own instant; two writers never mix |
| the recorded scenario | one schedule that must reproduce, event for event |

<v-click>

Each has a `*Soak` twin over a block of the seed range. The nightly sweep runs eight blocks of 100 seeds on GitHub's hosted runners. A failing seed reproduces anywhere with `just soak <seed> 1`.

</v-click>

<v-click>

Power loss on the simulated disk returns unsynced writes **applied, dropped, torn or garbled**. Swizzled links **drop, duplicate, delay and slow** what they carry.

</v-click>

---
class: text-sm
---

# The first night it ran in public

<v-clicks>

- Seeds 21 and 227 of the swizzle campaign failed with a **deadlock panic** — and under it, "the fenced writer published".
- Neither was real. `World.HostOf` reported the host a failed takeover had merely *tried*, so the campaign believed the fence had landed when the store was unreachable. The first writer was never fenced. Its checkpoint was refused by nothing.
- The panic: a `t.Fatal` inside a synctest bubble left the world's hosts waiting on simulated time. It hid the failure and took every seed after it down. Worlds now close on cleanup, inside the bubble.
- On the way: a **drain report** that a handover failed overwrote the row the orchestrator's own migration had written — a VM stopped at the source, volumes given up, marked *running*.
- And a fact for the open-work list: the deployment's drain tries a receive **once**. A destination briefly unreachable costs the guest its writes since its last checkpoint.

</v-clicks>

<v-click>

<div class="mt-4 text-sm opacity-70">
The lesson is not the bugs. It is that a harness which lies about where a VM runs makes every campaign above it lie too — and the sweep found it in one night.
</div>

</v-click>

---
class: text-sm
---

# Measured on a real cluster

Two hosts on GCE, 512 MiB Alpine guests each carrying a 256 MiB memory witness and a 256 MiB file witness, six rounds. 166 operations, 146 checks, every one holding.

| operation | pause or checkpoint | behind it | wall, slowest |
| --- | --- | --- | --- |
| fork, two local children | 0.1 s | 1.6–10 s to run the children | 10 s |
| fork, two remote children | 0.1 s | 5–20 s of post-copy | 20 s |
| migrate | 0.6–0.75 s | 0.3–4.9 s of stream | 5.7 s |
| stop | the checkpoint it published | — | 6.3 s |
| start, warm | the checkpoint it came back at | — | 1.0 s |
| start, cold, memory and disk grown | the discarding checkpoint | grow 0.5 s | 2.1 s |

<v-click>

Object store over the run: ~17,000 GETs for 18 GiB, ~1,300 PUTs for 41 GiB, 1,100 deletes.
Eight runs failed before this one passed; the defects are listed in `docs/measurements-2026-09-17-soak.md`.

</v-click>

---
class: text-sm
---

# What a checkpoint costs under a workload

One base VM, two forks, one fork of each: git, ripgrep, `pnpm install --offline`, `pnpm build` on three TypeScript repositories. 28 checkpoints.

| phase | dirty | uploaded | max pause | max upload |
| --- | --- | --- | --- | --- |
| boot | 212 MiB | 20.5 MiB | 8 ms | 1.6 s |
| search (`rg`) | 78 MiB | 12.2 MiB | 7 ms | 1.1 s |
| install (`pnpm`) | 1510 MiB | 297 MiB | 9 ms | 11.2 s |

<v-clicks>

- The pause is **single-digit milliseconds** whatever the guest did. The upload scales with what it dirtied.
- Dirty is counted in 2 MiB pages; uploaded is compressed. The 2 MiB : 4 KiB ratio is still unmeasured — the VMM does not report 4 KiB dirtiness.
- The larger setting did not fit the node; that is written up, not hidden.

</v-clicks>

---
layout: section
---

# What it is not, and what is open

---
class: text-sm
---

# Honest boundaries

<div class="grid grid-cols-2 gap-8">
<div>

**Not a filesystem.** A volume is a byte image with one writer. The guest formats it.

**Not content-addressed.** Sharing is lineage. Two VMs that wrote the same bytes share nothing.

**Not durable between checkpoints.** Up to one interval of writes is lost with a host.

**Not garbage-collected.** Pins are permanent; the collector is deferred indefinitely; the store grows.

</div>
<div>

<v-click>

**Open, and recorded in `docs/open-work.md`:**

- a migration whose source is unreachable but still listed waits for it — evidence, not a timeout, is what is missing
- a drain tries its receive once
- a child forked onto its parent's own host holds the instant invisibly to the survey
- recovery of running VMs after a host loss is proven in simulation and over fakes, not yet on a cluster: the one soak's kill landed on an empty host

</v-click>

</div>
</div>

---

# The shape of the repository

```text
cmd/sproutfs-host            the host process: HTTP handlers, adapters, configuration
cmd/sproutfs-orchestrator    identities, placement, migrations, forks, the table
cmd/sproutfs-guest-witness   fill / mutate / check / grow, inside the guest
internal/checkpoint          the two-plane store: index objects, parts, roots, reclamation
internal/control             control records: conditional writes, epochs, pins
internal/volume              volumes, overlays, publication, forks, handoffs
internal/vmmemory            the pager: arena, faults, seal, spill, eviction
internal/vmmigrate           page server and peer backing: the wire
internal/host                one host: checkpoint loop, drain, fork, migrate, templates
internal/simtest             the one simulation harness and its campaigns
internal/platform/sim        processes, disks, clocks, networks, faults
rust/sproutfs-vm-memory      the mappings inside the VMM process
third_party/firecracker      the fork, branch sproutfs
docs/  plans/                the design, its decisions, and what was measured
```

<div class="mt-4 text-sm opacity-70">
Every design change has a plan under <code>plans/</code> with its status; every measurement a document under <code>docs/</code>.
</div>

---
layout: center
class: text-center
---

# Three decisions

<div class="text-xl mt-8 space-y-4 text-left max-w-3xl mx-auto">

<v-click>1. One published checkpoint is the whole of a VM's durable state.</v-click>

<v-click>2. Sharing is lineage, never content.</v-click>

<v-click>3. The instant is separable from the upload, and only the instant is on the latency path.</v-click>

</div>

<v-click>

<div class="mt-12 opacity-70">
github.com/semistrict/sproutfs
</div>

</v-click>
