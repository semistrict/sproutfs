# Architecture

Sproutfs moves virtual machines between hosts and forks them without copying
their inherited disk and memory contents. Disk and memory use the same storage
abstraction: a [volume](volumes.md), a byte-addressed image with one writer.

Three decisions determine everything else.

1. A VM's durable state is exactly one published checkpoint, selected by its
   control record. Nothing is durable between checkpoints, so losing a host
   loses every write since its VMs' last checkpoints.
2. Stored data and resident frames share through lineage, without content
   hashes or byte-equality deduplication. Two VMs share a frame because they
   inherited the same checkpoint's bytes, never because the bytes match. A page
   no checkpoint holds reads as zeroes. The one exception is the name of a
   [template](hosting.md), `template-<sha256 of the guest image file>`: a
   template is not a VM that lives on a host but an imported image, and an
   image's identity is its bytes, which is what lets every host name one
   template for one image and import it once between them. It names the
   template and nothing below it — the checkpoints under that identity are its
   own like any VM's, and what forks of it share they share by lineage.
3. A checkpoint's instant — pause, save VMM state, seal the dirty pages,
   resume — is separable from its upload, and only the instant is on anyone's
   latency path. A fork and a migration take the instant and publish nothing:
   the unpublished pages reach the other side through the pager, on the same
   host or over the network.

## Components

| Component | Role |
| --- | --- |
| Control record | One object per VM, at `control/<id>`, a namespace holding nothing else so that listing it is how the deployment's VMs are found. Writer epoch, writer nonce, the selected checkpoint sequence and whether it is published, and the checkpoints of this VM that have been forked, which reclamation must spare and nothing unpins. A record exists exactly while its VM does. Every change is a conditional write, and the epoch holder is the only writer. |
| [Checkpoint](volumes.md) | Two planes under `vm/<id>/ckpt/<seq>/`, and nothing else. The data plane is `part/<n>`: a run of members — the VMM state, when one was saved, then the dirty pages, then compaction's rescues — filled to 64 MiB and closed by a table and a fixed 32-byte trailer, so a part describes itself. The metadata plane is `index`: a fixed header, the 512 MiB page-table segments this checkpoint changed, and last the **root**, whose create-if-absent PUT is the publication's commit. The root addresses each segment of every volume's page table inside the index object of the checkpoint that wrote it, keeping an earlier checkpoint's address for every segment this one did not change, and lists every checkpoint it reads a page from or addresses a segment in, plus the ones its own compaction emptied and spares for a checkpoint. It names no parent: a root is complete on its own, one GET of the index object yields it, and a reader fetches the segments a range falls in. |
| Overlay | The in-memory writes of one open VM since its selected checkpoint, published by the next one. It survives nothing. |
| [Pager](vm-memory.md) | A shared memfd arena serving the host's attached RAM and PMEM regions, keyed by lineage identity. The production frame and the unit of publication are both 2 MiB. |
| [Host](hosting.md) | `internal/host`: everything one host does. Its `Host` opens VMs over object storage and the cluster network, checkpoints them on an interval, fences a VM a later writer took, and serves migration and fork pages; the supervisor around it owns the pager and the VMM processes behind the host API, imports guest images into the templates VMs are forked from, and reaches the agent in a guest. `cmd/sproutfs-host` is its configuration, its HTTP handlers and the adapters it chooses. |
| Orchestrator | `cmd/sproutfs-orchestrator`: allocates VM identities, places VMs on host pods, and drives migrations and forks between hosts. Its SQLite table — states `creating`, `running`, `migrating`, `stopped`, `recovering` — is a view; the control records are the authority. |

## Loss model

A volume write applies to an in-memory overlay and returns. It contacts nothing,
so it cannot fail for a network or storage reason — and it is not durable. What
makes it durable is the next checkpoint: the pages upload, the last part carries
the root, and a conditional write selects it in the control record. Losing the
host before that loses every write since the last selected checkpoint, which is
accepted: a guest write must never wait on object-store latency.

`Status.DirtyBytes` is an upper bound on what a VM would lose right now, and the
volume manager's `Stats` sums it over every VM the host runs.

The volume layer has no automatic trigger. `Checkpoint` and `Snapshot` publish
on demand, `Close` publishes a final checkpoint, and `Handoff` and `ForkPoint`
publish nothing. The interval belongs to the host that runs the guest and knows
when the vCPUs may be paused: every 60 s by default
(`host.Config.CheckpointInterval`, `SPROUTFS_CHECKPOINT_INTERVAL`), each wait
jittered by up to an eighth either side so VMs do not checkpoint in lockstep,
and the next wait measured from the end of the last upload.

A failed publication changes nothing durable: the previous checkpoint stays
selected, the overlay keeps its bytes, and the failure is reported through the
VM's status and retried at the next interval. The exception is a handle a later
writer has fenced. The interval checkpoint is where a fenced host finds out,
because a running VM writes nothing else; it then closes the VMM and releases
the VM rather than leave a guest running whose writes can never be published.

Guest CPU stores are not durability acknowledgements, and neither is a guest
flush: virtio-pmem flush completes without making anything durable. Only a
checkpoint does. Local scratch spill is not durable storage either.

Published checkpoint objects survive host loss under the assumption that the
shared object store remains durable. A configured host can open a VM by identity
once the checkpoint its record selects is published and object storage is
reachable. A fork that has not published its own root index runs on the
host that took it in and nowhere else. Resuming a VMM additionally needs its
captured state and compatible runtime configuration.

## VM lifecycle

1. The orchestrator allocates a VM identity and chooses a host.
2. Creation publishes a first checkpoint of the root alone and then creates the
   control record that selects it. Opening reads that record, advances its
   epoch — which fences the previous writer — and reads the selected
   checkpoint's root. The overlays start empty; there is nothing to replay.
3. The pager attaches the VM's `ram0` and PMEM volumes and populates the pages
   whose lineage identity is already resident in the same pager before vCPUs
   run. Missing pages load on demand.
4. Volume writes apply to the overlay and return. The interval checkpoint pauses
   the guest, saves VMM state, seals every dirty page by write protection and
   resumes; the sealed pages stream out as parts behind the running guest,
   the last part carries the root, and the control record selects it.
5. A fork takes that same instant and publishes nothing. It is a handoff,
   whatever host the child lands on. One `ForkPoint` serves any number of
   children: the parent pins its last published sequence, once and for good, and
   keeps its handle, and each child gets a control record selecting a root over
   that sequence. Where the child lands changes only how the pages the parent
   holds that no checkpoint has reach it: on the parent's own host it maps the
   sealed frames through the pager, and with `--to <host>` its pager pulls them
   out of the parent's page server, as a migration destination does. The
   destination publishes the child's root as soon as it holds them all, and that
   is what makes it a VM any host can open. A fork that ends before it leaves no
   object behind.
6. A live move is post-copy only. The source stops the guest, saves VMM state
   and hands the VM over without uploading anything. The destination opens the
   same identity, advances the epoch, resumes from the supplied state, and
   faults the pages the source holds out of its page server, with the bulk
   stream behind the running guest; its next checkpoint is what makes them
   durable. The source is released once every unpublished page has reached the
   destination. A page no checkpoint holds exists only on that source, so the
   destination asks for it until it arrives: nothing in it can tell a source
   that stumbled from one that died, and reading its own volume for one would
   rewind the guest past its own write. What ends that is the source's own
   answer that it no longer serves the VM, or the orchestrator ending the
   migration, which it does when it has lost the source host — the pages are
   lost with it, and the VM is recovered from its checkpoint, rewound by the
   writes since. See [migration](migration.md) for failure handling.

## Identities and reclamation

The orchestrator must supply VM identities that are never reused. Sproutfs has
no global VM identity registry or limit on the number of VM identities used over
time; syntax and local resource limits still apply. Every checkpoint object is
written under its publisher's VM identity, and a fork's root goes on naming its
parent's checkpoints, which is why a fork copies nothing. A VM's control record lives
outside that namespace, at `control/<id>`, so that listing the deployment's VMs
reads a page of keys rather than walking every checkpoint object.

Selecting a checkpoint reclaims a set difference: the checkpoints the replaced
root named, and the replaced checkpoint itself, less everything the new root
names and everything a pin protects. A dead checkpoint goes whole, its last part
first, and only ever this VM's own — another VM's checkpoints are never touched. A
pinned sequence is spared along with every checkpoint its root names, so a
fork's whole lineage survives, the part of it a grandchild reads
directly included. A pin is written before the child that holds it exists and is
permanent: nothing in a deployment can establish that no lineage reads through a
checkpoint, because a descendant sees neither its siblings nor the forks taken
below it, so releasing a pin belongs to a collector.

Cold pages would otherwise keep mostly dead parts alive, so each checkpoint also
compacts: a checkpoint of this VM's with less than half its bytes still live is
rewritten into the checkpoint being published, up to 64 MiB of live bytes at a
time, after the guest has resumed. The emptied checkpoint then falls out of the root
and reclamation deletes it.

A handle reclaims only the checkpoints it published itself, so the one it opened
on is left for a collector. Deleting a VM removes its control record, which is
what makes the identity reusable, and then sweeps that VM's own checkpoints —
except the pinned ones, whose objects a descendant may still read and which are
therefore a collector's. A record that cannot be parsed refuses the delete, for
want of the pins that say what to spare. That collector, rooted in the
orchestrator's live set, is deferred indefinitely by decision, and until it
exists the store grows without bound: what a pin covers is kept forever. When
written it must protect in-flight publications and forks.

## Current state

Control records, two-plane checkpoint storage with reclamation and compaction,
volumes, the pager, capture, fork by handoff, post-copy migration, the host and
the orchestrator are implemented and covered by simulation tests. Recorded
Firecracker integration qualification covers Linux aarch64 with real KVM in the
[documented test environment](vm-memory.md#qualification); the pager's 2 MiB
HugeTLB qualification is recorded on x86_64. Other environments and production
workload/scale acceptance remain unqualified. Larger-guest measurements exist,
but the recorded workload run is partial and is not performance acceptance. No
production deployment is recorded in this repository. Collection — which is what
releases pins and sweeps the lineages of deleted VMs — and legacy-data
compatibility/migration paths, are not
implemented; compatibility requirements for any deployment's stored data must be
assessed before changing its formats.

Reliability work uses seeded workloads and simulated dependencies to exercise
the durability guarantees above. Seeds reproduce dependency choices, not
arbitrary Go scheduler interleavings; explicit gates make selected races
repeatable, and race detection checks shared-memory access separately. See
[testing](testing.md) for the scope and limitations of this evidence.
