# Hosting

A host runs VMs. One process provides that role over one object store, one
network and one RAM allotment. The host has no durable local state. A VM's
authority is the epoch in its [control record](metadata.md), and its data is the
checkpoint that record selects. The host also has no identity of its own. Its
page cache's disk has one, which names it in
[the list of caches](#the-list-of-caches). A host dials the peer-server
address that a [handoff](migration.md) carries, and the addresses of the
caches in that list, which its cache [fills](#filling-the-cluster). Who may
reach those addresses is the [transport's](#transport) business.

## Assembly

All host logic is in `host`. `Host` is what the deployment runs on this
machine. It contains:

- the object namespace;
- the shared page cache;
- the volume manager;
- the peer server, and the table of peers it reaches other hosts through;
- the loops that keep its VMs durable and fenced.

The supervisor around it is `host.Start`, which returns the `host.Service` that
the command serves. The supervisor owns the two pagers. Each pager has its own
arena and its own spill file. By default both use 2 MiB pages from the node's
HugeTLB pool. A deployment may instead run RAM at 4 KiB on an ordinary memfd
(`SPROUTFS_RAM_PAGE_BYTES=4096`). This uses a tenth of the memory for forks that
write little and in scattered places, and it is slower at everything else. Such
a node sets its shared memory's transparent huge pages to `advise`. This lets
the RAM arena allocate a zero run's whole 2 MiB blocks as huge pages, and leaves
all other shared memory on the node unchanged; see [the arena](vm-memory.md).
A deployment may likewise run PMEM, and the ephemeral pager with it, at 4 KiB
(`SPROUTFS_PMEM_PAGE_BYTES=4096`). This exists to measure whether publishing
4 KiB pages saves enough checkpoint and fsync traffic on disks written a few
blocks at a time to be worth the extra index entries and faults. A disk stays a
whole number of 2 MiB, because Firecracker requires it of a PMEM device.
The supervisor builds the host's [disk limiter](#budgets) once the spill files
are open and hold their extents. It refuses to start when the disk cannot
allocate a spill file, or cannot keep its promises under its goals.
The supervisor also drives the VMM processes, owns the templates that guest
images are imported into, and reaches the agent in a guest. It does not start a
VMM. A `vmmachine.Starter` does, as [running the VMM](#running-the-vmm)
describes.
`cmd/sproutfs-host` contains only its configuration, its HTTP handlers and the
wiring between them.

Starting a `Host` requires:

- a resource owner with a positive RAM allotment;
- the network that its peer server and its handoffs run over;
- the object store with the deployment's prefix.

The host owns the following, all with the same lifetime:

- the control client that reads and writes the deployment's control records;
- one checkpoint store with the host's shared
  [page cache](volumes.md#page-cache), which has its own cap and does not use
  the host allotment;
- the volume manager that opens this host's VMs;
- the peer server, when a migration address is configured;
- the table of peers, one for the host, which every receive dials through.

A host without a migration address can neither drain nor receive.

### Transport

Hosts reach one another over one channel, the
[peer server](migration.md#the-peer-server). Each host serves its own on one
port, and dials the others through its table of peers. `platform.Network`
frames that channel, and a `platform.Transport` carries the bytes underneath: a
stream listener and a stream dialer. `adapters.NewNetwork` frames over
`adapters.TCP`, the default. It is plain TCP and authenticates no peer, so
hosts on it must share a trusted network, and a network policy must keep
everything else off the peer server's port. That port is
`SPROUTFS_PAGE_SERVER_PORT`, which keeps the name it had.

The framer sends each frame in one vectored write, and refuses one over
16 MiB. A payload that is a range of a file goes behind the header with
`sendfile` when the stream is a TCP socket on Linux, so its bytes never pass
through the process. `adapters.TCP` turns keepalive on, probing after five idle
seconds every two seconds three times, and on Linux sets `TCP_USER_TIMEOUT` to
ten seconds, so the kernel gives up on a peer that stops acknowledging. The
peer server's own pings find a dead peer sooner; see
[liveness](migration.md#liveness).

A deployment that authenticates its hosts passes `adapters.NewNetworkOver` its
own transport, for example mutual TLS or its mesh's dialer. That transport
decides who is a host. Its listener closes a peer it cannot authenticate, and
its dialer fails on a source it cannot authenticate. Sproutfs never sees the
credentials. `internal/testnet.MutualTLS` is such a transport, and the tests in
`host/transport_test.go` migrate over it: two hosts that trust each other
migrate a VM; a peer server serves no stranger; a destination fetches nothing
from a source it does not trust.

A destination that cannot reach its source waits for the pages only the source
holds until its caller gives up, whether the source is down or refused. A
refused source is the same to it as an unreachable one. The receive then fails,
and the VM is discarded, as with any receive whose stream does not complete.

The host API is not on this channel. `api/host.NewClient` takes an
`*http.Client`, so a caller can reach the API over the same fabric. An embedder
that runs the host as a library serves its own API in front of it.

Only the command chooses which adapter implements each of those ports.
`sproutfs-host` builds the object store, the plain TCP network and the node
disk, and passes them in. An embedder passes a network over its own
[transport](#transport). `SPROUTFS_OBJECT_STORE` selects the object store:
`gcs`, the default, or `s3`. The port is the conditional-write contract that
the conformance suite in `platform/internal/real` states, and each adapter runs
it against an emulator. The S3 suite also runs against a real bucket when
`SPROUTFS_TEST_S3_BUCKET` names one. S3 names an object written in one PUT by
the MD5 of its body, so two writes of the same bytes share an ETag. A
compare-and-set there is a compare-and-set on the bytes, which is what every
caller of the port means by one. The host names no adapter. Neither
does `vmmachine`, which receives a `platform.Disks` for the staging directory
each VMM gets.

Every VM the host starts has one RAM volume, `ram0`, plus one volume per PMEM
device. The supervisor gives each VM a PMEM device, `root`, which the guest
boots from. A VM created with an ephemeral disk has a second PMEM device,
`ephemeral`, after the root. The supervisor opens a `vmmachine.Scratch` and
passes it to each VMM configuration.

A host given `SPROUTFS_EPHEMERAL_BYTES` runs a third pager for
[ephemeral disks](volumes.md#ephemeral-disks). That setting is its spill file,
which is every ephemeral disk the host admits, and
`SPROUTFS_EPHEMERAL_ARENA_BYTES` (256 MiB by default) is its share of the
HugeTLB pool. Both are whole 2 MiB pages, and the host's memory allotment
grows by the arena. A host without the setting runs no ephemeral pager and
refuses a VM with an ephemeral disk. A deployment that creates ephemeral disks
gives every host one, because a VM with one may be opened, received or
recovered on any host. The orchestrator places a create by memory alone, so a
host without room for the disk refuses the create.

`GET /stored?tenant=<tenant>` reports what one tenant's VMs hold in the object
store, per VM, for an embedder's billing. It lists the store, so any host
answers for every VM of the tenant, including VMs no host runs and deleted VMs
whose pinned checkpoints remain. See [billing](volumes.md#billing).

`GET /metrics` is `hostapi.Metrics` of the host's `Status`, in the Prometheus
text format. A program that embeds a host serves the same text from its own
endpoint: it reads `Status` from the supervisor and passes it to
`hostapi.Metrics`. It covers why a guest stalls (the pager's dirty and window
waits and stalls, refused mappings, repeated faults), how long faults, loads,
seals and object-store calls take (histograms), what the interval checkpoints
did, the migrations, forks and receives by outcome, the VMs a host gave up and
why, template imports, the peers a host has asked anything of by whether they
are up, down or incompatible (`sproutfs_peers`), and `sproutfs_build_info`. The
peer server's series keep the names the peer server had. No series names a VM
or a tenant.

Prometheus pulls, and a host's counters start at zero when its process does.
So anything a host counts just before it exits is never scraped. Each of those
events is recorded somewhere that outlives the process:

| Event | Where it lasts |
| ----- | -------------- |
| A drain moving a VM | the drain reports to the orchestrator for each VM, the `host: drained` log line, and the destination's `sproutfs_receives_total` and `sproutfs_received_pause_seconds` |
| The final checkpoint of each VM at shutdown | the VM's control record, which selects it; `volume: final checkpoint on close failed` when it fails |
| The whole shutdown | the `host: shut down` log line, with what the host did in its life |
| Exiting while still serving migrated pages | the `host: exiting while still serving migrated pages` log line |
| A fatal error | the `sproutfs-host: exiting` log line |
| The process killed outright | nothing from the process. The last scrape's `sproutfs_loss_window_seconds` bounds what its VMs lost, and each VM reopens at the checkpoint its control record selects |

## Running the VMM

The host prepares a VM's memory and drives its VMM. It does not start the VMM
process. `SupervisorConfig.Starter` does that, on every path a VM takes to run
on a host: a create, an open, and a receive of a migrated or forked VM. A
program that embeds a host writes its own Starter. `cmd/sproutfs-host` uses
`vmmachine.Firecracker`, which runs Firecracker directly.

A start has three steps.

1. The Starter calls `Launch.Prepare` with a placement: the host directory for
   the process's files, the same directory as the VMM names it, and the user
   the VMM runs as. The two paths differ when the VMM runs in a jailer's
   chroot. `Prepare` creates the directory, gives it to that user, and opens
   one socket per memory region. It returns the memory the VMM must be started
   with: the API socket path, the RAM socket and size, and the managed PMEM
   devices, all as the VMM names them.
2. The Starter starts the VMM however it likes: under a jailer, in a network
   namespace and a cgroup, with network interfaces, drives, read-only PMEM
   files and a vsock of its own. For a boot, `Memory.Configure` adds the
   managed memory to the Starter's configuration document. For a restore, the
   Starter adds new host names for moved devices to `Memory.Load`. It returns
   the process as a `vmmachine.VMM`.
3. The host takes the process over. It accepts the memory sessions, issues the
   snapshot load of a restore, and waits for every session before the guest
   runs.

The load stays with the host because the sessions attach during it, and a
restored guest must not resume before every one of them is up.

A Starter also reports the managed-memory API revision its VMM speaks. The host
asks at start and refuses to start unless it is `vmmachine.APIRevision`. See
[the Firecracker build](vm-memory.md#firecracker-build-and-process-lifecycle).

The memory sessions admit only a connection from the PID the Starter reported.
So a jailer must exec the VMM in the same process. It must not daemonize or
fork into a new PID namespace.

Drives and plain PMEM files that a Starter adds must be read-only. A checkpoint
holds only the volumes, so a disk the guest could write to outside them would
come back from a checkpoint without its writes. The VMM refuses to capture a VM
with one. A restore's devices are in its VMM state, so drives and PMEM files
keep the paths the VM's first boot gave them, on whichever host restores it. A
Starter that adds them uses paths that are the same on every host, such as
paths inside its chroot. Network interfaces and the vsock can move, through
`Memory.Load`.

The host reads a guest's console and reaches its agent only through what the
Starter's process offers. A `vmmachine.ConsoleVMM` keeps a console, and a
`vmmachine.VsockVMM` names its vsock socket. `vmmachine.Spawn` runs a command
as a child with its console kept in memory. That command may be a jailer.
`Starter.Boots` says whether the Starter can boot a kernel.

A process restart is a host loss. Nothing in the VMM scratch or in the spill
files survives a restart. Opening the scratch deletes its VM directories and
recreates them empty, and the spill files are truncated. There is no
reconciliation of VMs, no scan of what a previous process left for them, and
no lock held across processes. A host that restarts has lost every VM it was
running. The one file that survives is the page cache's disk, because it holds
only copies of what the store holds: the host reads it back rather than
emptying it (see below). The
deployment reopens each of them, on whatever host they land, from the
checkpoint its control record selects. The deployment must give each host
process its own scratch directory.

`sproutfs-host` reads its whole configuration from the environment. It reports
every configuration problem it finds, not only the first. It serves the host
API over HTTP: status, create, open, fork, capture, console, exec, migrate,
receive, released, drain, stop, kept, release and delete. Each handler is one call on the supervisor
plus the shared JSON failure shape. The API's types live in `api/host`
and do not link the runtime. So the host converts a handoff at its boundary
instead of putting a pager on the wire. The GCS store that the command builds is
metered per operation, so that one counter measures all of the deployment's
object traffic. Setting an endpoint selects an emulator instead of the ambient
Google credentials.

Every request except the kubelet's probe must carry the deployment's shared
bearer token, `SPROUTFS_API_TOKEN`. A request without it is refused with 401.
The same token admits requests to the orchestrator's API, and the same
middleware serves both. The token grants no authority over any VM; that
authority is the epoch in the control record. The token also does not identify
the caller. It only shows that the request comes from inside this deployment.
An API that can delete or drain every VM on the pod network needs that check
first. The cluster's network policy restricts the ports to the deployment's own
pods, and `deploy/30-networkpolicy.yaml` ships that policy. Draining is a POST
for the same reason. Tools that crawl URLs issue GETs, and a drain migrates
every VM off the host. So the preStop hook runs `sproutfs-host drain` instead of
fetching a URL.

Creating a VM forks a template. Each guest image is imported once into a VM that
is never booted. Every VM created from that image inherits the template's
published checkpoint without copying any bytes. A template is an ordinary VM
with an ordinary control record, which lets a fork inherit a published
checkpoint. It lives under a reserved identity namespace, which keeps templates
out of the deployment's list of VMs.

**A template is named by its image.** The identity is
`template-<sha256 of the image file>`. The host computes it over the file it
was configured with. A template does not belong to a host. It is an imported
image, and an image's identity is its bytes. So every host configured with the
same image names the same template. What a starting host does depends only on
the template's current state in the deployment:

- **published**: its control record pins the checkpoint it selects. The host
  opens nothing and writes nothing. It reads that checkpoint the way any host
  reads a checkpoint it did not publish, and remembers it for `create`. This is
  the case for every restarted pod, and for the second host of a pair.
- **absent**: the host creates the template, writes the image into its root
  volume, checkpoints it and pins that checkpoint. Pinning the checkpoint
  publishes the template.
- **a record with no pin**: an import has not finished. The host waits for it.
  The usual reason is an import in progress, and opening the record would take
  the epoch from the host doing the import. After the wait, the host assumes
  that the host that wrote the record is gone. It recovers the template like
  any other VM: it takes the epoch, which fences that writer if it is still
  alive, and imports again under the new epoch. Checkpoints published by the
  interrupted attempt stay in place, like a superseded epoch's checkpoints after
  any other takeover.

A configured image is imported as a public template, of no tenant, so a VM of
every tenant is created from the one import of it (`control.Public`). An image
imported on request for a tenant is that tenant's alone. No guest runs as a
template: a create, open, capture or receive that names one is refused.

**An import reads only the image's data.** A root image is usually a large
sparse file. When the source is an `*os.File`, the host asks the kernel for
its data extents with `SEEK_DATA` and `SEEK_HOLE`. A source that implements
`host.SparseSource` reports its own extents. Both passes read only those
extents: the digest hashes each hole as the zeroes it reads as, and the import
writes nothing for it. So a sparse image and the same image written in full
name one template. A filesystem that cannot report holes, such as a FUSE
filesystem without `lseek`, is read in full. The digest still hashes every
zero, which costs CPU and no reads.

Two hosts that start at the same time race on the record's create-if-absent.
The losing host's create is refused, and that host is then in the third case
above.

The imports run at startup, behind the API. A host is ready when every image it
is configured with has a published template, whichever host imported it. An
image that nothing has imported yet costs a full file read and a checkpoint. An
image that another pod has already imported costs a read of one control record.
A host that cannot read its images stays unready and reports why, instead of
accepting VMs it would fail to create. Liveness is a separate endpoint, so a
host that is still importing is not restarted for failing readiness.

A guest image does not have to be configured. A builder's image can be
imported on request (`ImportTemplate`, `POST /templates`, with the image as the
body). The host stages an image that is not a seekable file under its scratch
directory, because an import reads the image twice: once for the digest that
names the template, and once for its bytes. It then imports it as it imports a
configured one, and reports the template's identity. Any host creates from
that template by its identity, `template-<digest>`, as a create's `template`.
That host reads the template's control record, finds the checkpoint it pins,
and forks it. It needs no image and no import. An identity nothing imported is
refused, and so is one whose import has not published yet. A host configured
with `SPROUTFS_TEMPLATES=none` has no images of its own, is ready at once, and
creates only from templates imported on request.

No host deletes a template, because another host may be forking from it. An
image whose content changed under the same name is a different template, not
the same template with other bytes. Templates of images that nothing creates
from any more are left to a collector, like every other pinned checkpoint. The
checkpoint a template pins is the fork point of every VM created from it.

A VM does not have to keep its template's shape. A create names the VM's RAM,
the size its root volume grows to, and its processors. It publishes the VM's
first checkpoint at that shape, through the same publication a cold boot uses
(see [resizing at a cold boot](#resizing-at-a-cold-boot)), before the first
boot. A template never ran, so nothing is lost by discarding its memory. What a
create does not name is the template's: its RAM and disk as imported, and the
host's processor count.

A create may also ask for an ephemeral disk (`CreateRequest.Ephemeral`,
`sproutfsctl create --ephemeral 8G`), in whole 2 MiB pages. The fork that
creates the VM adds it, zeroed, and the VM's first checkpoint records it.
Admission charges it to the ephemeral pager by its size. A create from a
checkpoint that already has one gives it the new size, or keeps its size when
the request names none.

Software in the guest is reached over the VM's vsock, which carries only exec.

## The checkpoint loop

`Host.AddMachine` records which VMM process belongs to which VM. This makes the
VM drainable and starts its checkpoint loop. The host checkpoints the disks of
every VM it runs every `Config.CheckpointInterval`, sixty seconds by default.
Each wait is jittered by up to an eighth either way, so VMs do not checkpoint in
lockstep. The host waits for each publication before scheduling the next. Each
checkpoint is `CaptureDisks`. Its pause seals only the VM's PMEM memory regions and
captures no VMM state. So RAM is never uploaded on the interval, and a VM opened
at such a checkpoint is cold booted over its disks (`Host.Starting`). This loop
is the only thing that makes a running guest's disks durable, so the interval
bounds how far losing this host rewinds them. A failure is logged and retried at
the next interval. The loop stops when the VM is removed or migrated away, or
when the host closes. An interval is skipped when a fork point has sealed the
VM, because the child takes those pages first. A negative interval disables the
loop, for a caller that drives its own checkpoints.

A VM may ask for an interval of its own (`MachineTerms.CheckpointInterval`,
which `CreateRequest` and `OpenRequest` carry). A positive one is clamped to
between `Config.MinimumCheckpointInterval`, one second by default, and the
host's own interval: a VM may ask for a tighter bound on what a host loss
costs it, never a looser one, so the loss window is never shorter than any
VM's interval. Its flush bound, where the host configures none, is two of its
own intervals. A negative interval asks for none at all. The VM takes no
interval turns, is held to no loss window, and has every flush complete at
once. It is still checkpointed when it stops, when it moves, and when the pager
needs its dirty pages back. A host loss loses everything it wrote since it
started. A migration carries the interval to the next host. A fork's children
take the host's, and an open asks again. The orchestrator does not forward it:
it is for an embedder that drives the hosts itself. `Status` reports each VM's
resolved interval.

The interval bounds that rewind only while publications succeed.
`Config.LossWindow` bounds it when they fail. It is five minutes by default,
zero disables it, and it is never shorter than the interval, because every VM
would exceed a shorter window before its first checkpoint is due. While a VM's
oldest unpublished write is older than the window, and a sealed checkpoint of
it is uploading, the pager admits no further dirty pages for it; see
[managed VM memory](vm-memory.md#bounded-host-pager) for when a store past the
window goes through instead. The window applies to disks only. RAM memory
regions do not age and do not request checkpoints, because no checkpoint the
loop takes would publish them.

The host's part is the loop. It takes a checkpoint out of turn when a VM's
oldest unpublished write reaches three quarters of the window, so the pause
comes while the guest still runs. The clock does this, not a store: a guest
that only rewrites pages it has already dirtied needs no new page, so no store
of it ever reaches the pager's window check. Once its checkpoint has sealed
those pages, its next store into one needs a copy, and the window holds it.
After a failed attempt the loop waits for the window itself instead, so a store
outage is not retried at every wake.
If a publication fails while the window is exceeded, the loop does not give its
pages back. It publishes the same sealed checkpoint again, under the same
reference, after an eighth of the interval, doubling up to the interval. The
guest's stores are held behind that checkpoint, and each held store holds the
vCPU that made it. A pause needs every vCPU, so a checkpoint that gave its
pages back could not be taken again while those stores wait. The sealed one
needs no pause, and it lands as soon as the store answers. The retries stop
when the loop does, so a migration, a stop or a removal gets the pages back
without waiting for the store.

A guest's flush reaches the host through the pager. `Config.FlushBound`
(`SPROUTFS_FLUSH_BOUND`) is twice the checkpoint interval by default, so
120 s; zero completes every flush at once. Two intervals leave room for a write
made just after one checkpoint to be sealed and published by the next. It is the maximum age of the VM's oldest unpublished disk write
at which a flush still completes at once. Past that age, the flush waits until a
checkpoint covers the write. The loop takes that checkpoint out of the
interval's turn. A flush of fresh disks takes no checkpoint. When a VM leaves
the host (migrated, stopped or given up), its waiting flushes go unanswered,
and its device asks the next host again.

Inside the window, a failed publication gives its pages back and is retried a
full interval later, because retrying eight times as often would only multiply
the requests that a store outage costs the deployment.

The window belongs to a VM, not a memory region, because the checkpoint that ends it
covers the VM: one pause seals every memory region a VM maps. The pager has no concept
of a VM. So the host reports the age, in the same way that it provides the
checkpoint. `Pressure.Oldest` reports the oldest unpublished write across every
memory region of the VM that maps the queried memory region. `Host.LossWindow` reports that
age per VM, and whether the VM's stores are waiting. `/status`, `/metrics` and
`sproutfsctl list` show these values.

A guest can fill the host's dirty budget long before its interval comes round.
It then waits for a checkpoint that nothing has scheduled. `Config.Pager` is how
the host learns about this. The pager asks the host for an immediate checkpoint
of the memory region with the largest dirty set. The loop takes it out of the
interval's turn, and the stalled stores complete when it retires. The host
accepts for a VM it runs whose volume no fork point has sealed. Otherwise it
declines, so the pager can offer another memory region. If no memory region can be
checkpointed, the budget is taken back from the VM that holds the most of it.
The pager asks the host to stop the memory region with the largest dirty set,
then the next, down to the memory region whose store is waiting. The host stops
the first one that belongs to a VM it runs, deliberately. The waiting store
then waits for that VM's pages to come back. It fails only when its own VM is
the one stopped. So a guest that holds little of a budget is never stopped for
one that holds much. This matters most for RAM. No checkpoint the loop takes
gives RAM back, so a guest that stores into all of its RAM holds that much of
the budget until it stops. Its neighbour's next fresh store would otherwise be
the one stopped. A store that the loss window blocks ends with its own VM
stopped when no checkpoint of that VM can ever be taken, because the window is
that VM's alone. The pager reports this case
separately, as a window stall instead of a budget stall, because the two mean
different things for a deployment. A budget stall means that its guests dirty
pages faster than their checkpoints drain. A window stall means that a guest's
writes cannot be made durable at all.

That stop uses the same give-up path as every other loss of a VM, with one last
checkpoint inside it. The steps are:

1. The registration is dropped and this caller takes ownership of the close. So
   a stall, a takeover and the watcher finding the same process dead close the
   VM only once between them.
2. The fork points taken on the VM are retired. This comes first because a
   child reads them out of the process that is about to close, and because a VM
   that a fork point holds sealed cannot be captured.
3. The last checkpoint captures whatever the VMM can still be paused for, before
   the process is closed, because the process's memory regions hold those pages.
4. The VM is given up and reported through `MachineClosed`, like a fenced VM.

If the failed store has already killed the VMM, that capture is not possible,
and the stop keeps nothing that the kill did not keep. What the stop adds is a
logged reason, which the kill never had.

The loop also stops when another host has taken the VM's control record. That
handle can never publish again. A refused publication is one way a fenced host
finds out. Not every VM reaches that point, so it is not the only way. Every
`Config.EpochInterval` (two seconds by default), a host re-reads the control
record of every VM it holds. It reads a few at a time, not one after another,
and closes any VM whose epoch has moved past the handle it holds. If each round
did one small read per VM in series, it would take the slowest read times the
number of VMs. When the store has a bad minute, that is longer than the
interval, so the check would fall behind when a takeover is most likely.

That interval bounds how long a host that was fenced between checkpoints keeps
running a guest whose writes have nowhere to go. The same bound applies to a
host that was fenced while a fork point holds a VM's pages sealed, because such
a VM is never checkpointed. A record that cannot be read changes nothing. Only
the record itself, naming an epoch this host does not hold, is evidence of a
takeover. A negative interval disables the timer, for a test that drives the
check itself. In either case the host gives up the whole VM:

- it retires the fork points taken on the VM and stops serving their children's
  pages;
- it stops serving the VM's own pages;
- it stops the VMM;
- it releases the VM's volumes;
- it reports the VM through `Config.MachineClosed`, so the supervisor forgets it
  and refuses exec against it.

Nothing a fenced host produces after the takeover can become that VM's state,
because the store refuses every control-record write it makes. From this point
on, the host also serves nothing it holds as that VM's state.

A timer is not enough for a handoff. A handoff is the only operation in which a
host gives another host pages that no checkpoint holds. The store cannot refuse
those pages the way it refuses a fenced writer's publication. The destination
post-copies whatever it is served on top of the checkpoint that the real writer
published. Two kinds of source would build one VM's memory from two writers'
pages:

- a source that was taken over between epoch ticks;
- a source whose store reads fail while its pod network works, so no tick ever
  tells it anything.

So `Migrate` and the fork point each re-read the control record before the
pause, with one GET, and refuse if the epoch has moved. A failed read also
refuses the handoff. This is the only place where an unreadable record is
treated as evidence. The store later catches everything else a stale handle
does, but it does not catch this. The VM keeps running here in either case. The
next attempt, or the epoch timer if the takeover was real, resolves it.

Only one handover of a VM runs at a time. It is reserved under the same lock
that protects the registration. Without this, two callers that each found the
registration would both stop the guest, give up every memory region's volume and
register the pages with the peer server. The loser, whose memory regions had already
given up their volumes, would then give up the VM and close the process that
the winner's destination was about to fault pages from. Instead, the second
caller is told, and the VM is left as the first caller left it. A VM that a
fork point holds sealed is refused in the same way. So is a fork that has not
published a checkpoint of its own. That handle is the only thing that could
ever publish that root. Handing it over would leave an identity that no host
can open and a parent that stays sealed permanently. Both refusals happen
before the guest is stopped.

`AddMachine` also watches the VMM process through `Machine.Wait`. It gives the
VM up in the same way when that process ends unexpectedly. This happens when:

- the host kills the process because its memory session failed;
- the kernel kills it;
- it crashes.

The checkpoint loop would not notice for an interval. It would then find a
socket that refuses the connection. It would log and retry for as long as the
host runs, while the supervisor reports a guest that no longer exists. The
watcher stops with the machine, so a process that this host ends on purpose (a
handoff, a removal or a shutdown) is not reported as a death. When a process
does die, the report comes from the process. `vmmachine` writes a single error
record with the VM, the pid, the cause the kill carried and the tail of the
console. The memory session that ended writes what ended it: the memory region, and
the page when the failure was a fault.

Every mapped memory region must use `Host.Resources()`. Registration rejects a machine
whose memory regions use another budget and leaves cleanup to the supervisor. The
receive path closes a mismatched runtime before streaming pages or registering
it.

## Draining a host

A host configured with a migration address serves the peer server at that
address. It holds the memory of every VM the host has handed to another host,
and of every child it has forked onto another host. A child forked onto the
same host is not there. That child maps the pages instead of fetching them, so
none of it is ever served. The peer server serves any peer its
[transport](#transport) accepts. Each remote host's faults may hold 8 MiB
there at once, and its bulk reads and its bulk writes 16 MiB each, over all its
connections. A request past that is answered `BUSY`. Over the default plain
TCP, the cluster's network policy, not this process, restricts that port to
this deployment's hosts.

`Host.Drain` migrates every VM the host runs, four at a time by default. A drain
is planned work whose cost is one host's memory. Moving all of it at once would
put all of that memory on the network at the same time. `Host.Drain` returns
one handoff per VM that moved, and joins the errors of the VMs that did not
move. Those VMs keep running here. A failure before the handoff leaves the VM
running and checkpointing on the interval. A receive that fails after the
handoff is tried again while this host holds the pages. Only if no destination
takes the VM in that time is the guest reopened at its last checkpoint, as
described in [migration](migration.md#a-failed-receive-is-tried-again).

The process's drain does not choose destinations. It asks the orchestrator to
move each VM. The orchestrator drives both halves of the migration and is told
when each one starts and finishes. The destination's `Host.Receive` opens the
VM, starts the VMM from the captured state and streams the pages in. The stream
reports completion only when every page that the source holds and no checkpoint
has is on the destination. A migration publishes nothing, so those pages exist
nowhere else. When the stream completes, the source's `Host.ReleaseMigrated`
stops serving that VM and closes the process that held its pages.

The release comes from the orchestrator, so a migration has the same deadline
as a fork's hold. Four checkpoint intervals after the handoff, a source that
nothing has released gives up those pages itself and closes the stopped VMM
process that maps them. Without the deadline, an orchestrator that restarted
between the handoff and the release would pin the source's arena for as long as
the host runs. The destination loses nothing it already fetched, and reads the
rest from the checkpoint its record selects.

`/status` reports an `outstanding` count per VM next to `serving`. The count is
the number of pages this host still holds that no checkpoint has and that the
destination has not fetched. It shows which of two states a VM in that list is
in:

- a handover that is still pulling its pages across;
- a handover that has all its pages and is waiting only for the release.

That is the question to ask about a drain that is not finishing. A VM whose
volumes cannot be listed reports `-1`, because the number of pages it still
holds is unknown. A fork's child on its parent's own host fetches nothing. Its
count is every page the fork point holds for it until the host takes it in, and
zero after. Its release is refused until then.

`/status` also reports `receiving`: every VM a receive is in flight for on this
host. A VM is in it from the moment the host admits the receive until the host
has taken the VM in or given it up. A receive whose caller hung up stays in it,
because nothing on the host stops for want of a caller. The orchestrator asks
no other host to take a VM while one reports it here
([migration](migration.md#a-failed-receive-is-tried-again)).

On the destination side, each receive logs a line when the
post-copy finishes. The line carries the pages served, the requests that the
source refused for its per-peer budget, and the duration. It also carries the
latency of the guest's own faults to the source (p50, p99 and maximum, and the
p99 of the wait for a connection) and the p99 of the stream's requests. The destination also
logs a line whenever a read of pages that only the source has waits longer than
a few seconds. Such a wait stops a guest thread for the same length of time.

The deployment's preStop hook drains and then exits. The drain returns only when
nothing is left to hand over, which means `Status().Serving` is empty. That is
the contract. When `Serving` is empty, every page this host held is on a
destination or in object storage, so exiting loses nothing. Exiting earlier
loses every write since each VM's last checkpoint, including dirty RAM, dirty
PMEM and unpublished local forks.

The drain sets its own bounds, because nothing else bounds it. The hook has no
deadline. After the hook starts, a termination grace period runs, and when it
ends the pod is killed with everything it still holds. The bounds are:

- Four VMs are handed over at once, each with its own 60-second deadline. This
  is shorter than the four intervals for which a source serves an unreleased
  handover's pages. It is also shorter than the two minutes for which the
  orchestrator trusts its record of a handover. The deadline bounds the
  request, not the handover. Once the source has stopped a guest, the
  orchestrator carries the handover on until a destination takes the VM or the
  hold is over, and the wait for `Serving` to empty waits for it.
- The whole drain has 30 minutes, including the wait for `Serving` to empty. A
  host full of VMs needs that long at four at a time.
- The orchestrator client has a timeout, so a connection that nobody answers
  cannot outlast either bound.

The preStop command that waits on the drain gives up at 31 minutes. This is a
backstop for an answer that never comes back over the loopback. It is only one
minute longer than the drain's own bound. A client that waited much longer
would use up the shutdown's share of the grace period waiting for an answer
that is not coming. A VM whose
handover ran out of time is left running here, keeps being checkpointed, and is
reported as remaining.

`terminationGracePeriodSeconds` is the hook's 31 minutes plus the shutdown after
it. The shutdown is two 30-second halves run in sequence: first stopping the
API, then the supervisor's close, in which every VM publishes a final
checkpoint. So 31 minutes plus one minute gives the manifest's 1920 seconds. A
shorter grace period turns an orderly exit into a host loss.

The supervisor owns VMM processes and the shared pager. After it finishes the
required capture or handoff, it closes the processes and detaches their
memory regions. It then calls the pager's `Close` before closing its arena and spill
handles. If pager cleanup fails, unproven allocations stay charged, and cleanup
must be retried. Closing one process must not close a pager that still serves
other VMs.

## Stopping a VM

`Host.Stop` deliberately ends a VM that this host runs, and leaves the VM in
place so it can be opened again. It captures a checkpoint of the guest's disks
and waits for it. When the stop suspends the guest, the checkpoint also includes
its memory and VMM state, so the next start resumes the guest instead of booting
it. Only then does the stop close the VMM process, return the pages and release
the handle. The control record and the objects stay in place, so any host can
open the VM again at the bytes the stop published. This is the difference
between a stop and a host loss: a host loss loses the writes since each VM's
last checkpoint. The stop reports the checkpoint it published, because the VM
comes back at that checkpoint and nothing else records it. The handle that knew
it is released by the time the stop returns.

The checkpoint comes first, and nothing is given up until it has landed. If the
store refuses the publication, the VM stays as it was: running, registered and
checkpointed on the interval. The alternative would be a stop that reported a
failure and lost the guest's last writes anyway.

A stop can keep the checkpoint it publishes (`StopRequest.Keep`), as a capture
can (`CaptureRequest.Keep`). See [kept checkpoints](#kept-checkpoints).

A stop of a VM that something still holds sealed is refused, as a delete of
such a VM is. A fork point holds the pages of the process that a stop would
close. A child on another host reads the pages that no checkpoint holds from
that process. So closing the process would remove the point in the middle of a
fault. The stop does not retire anything to get past this, and here a stop
differs from a delete. A delete ends the VM permanently and stops the children
from reading it as it goes. A stopped VM will come back.

Starting a stopped VM again is `Host.Open`, the same call a recovery makes. This
process does not need to remember anything about a stopped VM. The difference is
in the control plane, in what it requires before it calls `Host.Open`; see
[the deployment's API](../deploy/README.md#the-orchestrator-api).

## Starting a VM cold

A VM always comes back where it was. Its selected checkpoint holds the VMM state
and every page of guest RAM, and an open restores them. A cold start is the
exception. The reasons for it are the usual reasons to reboot any machine:

- the guest is wedged;
- the kernel or init on the disk changed;
- to stop paying for memory the guest does not need to keep.

`Host.OpenCold` opens the VM and discards its memory in one publication. Every
page of the RAM volume is dropped, so those pages read as zeroes and are left
out of the new root. The VMM state member is dropped too, so the new root names
none. `volume.VM.DiscardMemory` performs both as one operation under the VM's
publication lock. Doing only half would leave a VM that can be neither resumed
nor booted: memory without the state captured over it, or state without the
memory it describes. The guest is then started through the existing path, which
boots the kernel because there is no state to restore. A template's children
already start that way.

The root volume is what the last checkpoint published. So the guest's filesystem
sees the boot as a power cut after that checkpoint. **The filesystem's journal
recovers what it can, and this system guarantees nothing beyond that.** The new
root no longer references the pages that the discarded memory held. The usual
set difference reclaims them, except where a pin protects them. The checkpoint
that dropped them is small.

A cold start applies only to a VM that this host does not run. The operator
must stop a running VM first. A cold start is refused before anything is
discarded if this host's Starter cannot boot a kernel. It
is also refused if the pager could not map all of the VM's memory regions.

### Resizing at a cold boot

A cold boot is the only moment when a VM's shape can change, because nothing in
memory describes the shape any more. A create's first checkpoint is one. So
`OpenCold` and `Host.Reshape` take a `ColdShape`:

- `MemoryBytes` sets the RAM volume's size in the discarding publication. Any
  size the host admits is allowed, larger or smaller, because the memory is
  discarded anyway.
- `RootBytes` grows the root volume in the same publication. The new pages read
  as zeroes, which is what a filesystem grown in place expects. After the boot,
  the guest takes them with `sproutfs-guest-witness grow /`. Shrinking is
  refused, because the volume must not remove the end of a filesystem.
- `VCPUs` sets how many processors the guest boots with. The checkpoint
  records the count, and every later checkpoint and every fork keeps it, so a
  VM booted cold after its host is lost boots with its own count, on any host.
  The host gives it to the Starter as `Launch.VCPUs`. A VM that records none
  boots with the Starter's default. A restore takes its count from the VMM
  state.

All three are refused for a warm start, at every layer that carries them. After a
resize, a VM's committed RAM is its own and no longer its template's. The
control plane's record of the VM tracks this; see
[the deployment's API](../deploy/README.md#the-orchestrator-api).

## Creating a VM from a checkpoint

A create can start from another VM's published checkpoint instead of a
template (`CreateRequest.From`). That VM belongs to the same tenant and need not
run anywhere. A stopped VM's last checkpoint is its whole state, and this is how
a new VM starts from it. The create is the same path as a create from a
template: a fork of a published checkpoint, the new VM's own root, and a start.
The new VM copies no byte.

How it starts depends on what the checkpoint holds (`Host.CreateRoot`):

- A checkpoint with VMM state resumes. The new VM's root names that state and
  the memory the checkpoint holds, and the guest is restored where the
  checkpoint's pause left it, as a fork of a running VM is.
- A checkpoint without state boots cold. Its memory is discarded in the root,
  and the guest boots its kernel over the disk it inherits.
- A create that names a shape boots cold whatever the checkpoint holds,
  because a shape can change only at a cold boot. A shape that names no size
  keeps the checkpoint's.
- A create that adds an ephemeral disk the checkpoint does not have at that
  size also boots cold. The VMM state does not describe that device, and a
  guest cannot resume onto a device it never had.

`CreateResult.Resumed` says which happened, and `sproutfsctl create` prints
"resumed" for a VM that resumed.

The checkpoint must be pinned in the other VM's control record before the fork,
as every fork's is. A stopped VM has no writer to pin with. Taking its epoch to
pin would fence a host that turns out to run it after all. So the pin is
written without the epoch (`volume.Manager.InheritPublished`, and
`control.Client.Pin` below it). It keeps the record's epoch and nonce, and it
is conditional on the record as it was read. See
[metadata](metadata.md#the-control-record) for why that is safe.

This limits which checkpoint a create may name. By default it is the one the
VM's record selects, and that one must be published. A create may also name a
kept checkpoint, or one that a pin already holds, such as an earlier fork
point. Any other checkpoint is refused with `control.ErrNotPublished`, because
the VM's writer may be reclaiming it. A pending fork, whose root has not
landed, is refused the same way. So is an identity that already exists. The
API answers these with 409.

If the other VM is in fact running, nothing breaks. The new VM inherits its
last published checkpoint, which for a running VM is its last interval
checkpoint of the disks. The running VM's writer adopts the pin at its next
selection and spares the pinned checkpoint from then on.

### Kept checkpoints

A VM moves past each checkpoint as soon as it publishes the next one, and
reclamation deletes the older one. To go back to an earlier point later, a
checkpoint request asks to keep its checkpoint: a capture
(`CaptureRequest.Keep`, `sproutfsctl capture --keep`), a stop or a suspending
stop (`StopRequest.Keep`, `sproutfsctl stop --keep`), or the host's own
`Capture` and `CaptureDisks` (`volume.Terms.Keep`). The checkpoint is kept in
the write that selects it. From then on reclamation spares it and everything
it reads, however many checkpoints the VM publishes after it. Only kept
checkpoints cost storage beyond what the VM's selected checkpoint reads. A
capture into a new VM takes no keep, because its root is the checkpoint the new
VM's record selects.

`GET /vms/{id}/kept` (`sproutfsctl kept VM`) lists a VM's kept checkpoints:
each one's sequence, when it was selected, whether it holds VMM state, and
whether a VM was created from it. Any host answers for any VM, because the
list is the VM's control record.

A create from a kept checkpoint works whether or not its VM runs, and whatever
the VM has published since. It pins the checkpoint, like every fork. A kept
checkpoint that no VM was created from can be released
(`POST /vms/{id}/kept/{checkpoint}/release`,
`sproutfsctl release VM@CHECKPOINT`). The release deletes what only that
checkpoint held. A kept checkpoint that a VM was created from is pinned, and
its release is refused with `control.ErrForked` (409): the pin is permanent,
because a descendant may read through it. Deleting a VM deletes its kept
checkpoints that no VM was created from. See
[metadata](metadata.md#kept-checkpoints) for the record and the sweep.

## Capturing a VM into a new VM

`Host.CaptureInto` captures a VM this host runs into a new VM that never boots
(`CaptureRequest.Into`). It is a fork whose child publishes its root here and
is then closed, without a VMM:

1. The source pauses once, as a fork's parent does. The pause saves its VMM
   state and seals its memory regions, and the source runs again. Its
   published checkpoint is pinned by its own writer.
2. The child is created over that fork point, on this host.
3. The child's root is published. It reads the pages the pause sealed through
   the fork point, as a child on the parent's own host does, and it carries
   the VMM state the pause saved.
4. The child's handle is closed, and the fork point is retired.

Nothing is served over the network and no VMM starts. The source keeps
running throughout. Its seal ends when the capture returns, on every path: the
capture holds the fork point itself until the child's root has landed or
failed. A child whose root did not land is closed, which removes its record.
An identity that already exists is refused before anything is published, and
the source is unsealed again.

The new VM is then like a stopped VM. Any host can open it, and it resumes the
guest where the pause left the source. A create can also start from it (see
[creating a VM from a checkpoint](#creating-a-vm-from-a-checkpoint)).

## Pulling a VM's memory

A VM's pages load from object storage the first time the guest touches them. A
clean page the pager evicts is read from object storage again. So every cold
fault pays the store's latency, for the life of the VM. A VM can instead pay
that cost once, up front, in the background: its start marks it to **pull**
its whole memory. The mark is `Pull` on the host API's create, open and fork,
and `--pull` on `sproutfsctl create`, `start` and `fork`.

While this host runs a marked VM, every page of the checkpoint it started from
is copied onto this host's disk. The pages need not become resident in memory.
The copy lives in the [page cache's disk](volumes.md#the-page-caches-disk),
keyed by page identity. Once it is complete, a fault on a page that is not
resident reads the disk and makes no request of the object store, while the
disk holds that page. That holds for a page the guest never touched and for
one the pager evicted since. With the cluster cache off, as a deployment
runs it today, every page is kept whole on this disk. A window inside the
share the cluster cache is turned on for is a fill
([filling the cluster](#filling-the-cluster)): the pull hands it over and
does not wait, and its stripes go to the caches the list ranks for it, this
host's own among them.

- **The guest runs while the copy is made.** The pull starts when the machine
  is registered, after its VMM runs. A fault is never queued behind it: the
  pull takes none of the page cache's load slots, joins no fault's fetch, and
  makes no request while a fault's read of the store is in flight. Every pull
  on the host shares two requests in flight.
- **It is bounded and falls back whole.** The disk limiter sets the disk's
  share ([budgets](#budgets)). The disk is a log of 64 MiB regions, and every
  region of the share but one may be filled. A pull is refused
  before it fetches anything if the checkpoint, as its root records it, is
  larger than those regions hold. A VM that does not fit is not pulled at all,
  and its faults read the store as any other VM's do. So does a VM on a host
  that keeps no disk. A pull that fails part way keeps what it copied, and the
  store serves the rest.
- **It holds nothing.** The pages a pull copies are ordinary entries of the
  disk. When the disk needs room, it gives back its oldest region, and the
  pages in it that were read since they were written get a second chance. A
  pulled page goes the same way as any other. So the pages of a VM that runs
  long, and of the VMs pulled after it, can push its first pages out, and those
  are read from the store again.
- **The disk survives a restart.** A host that starts opens the existing
  file instead of truncating it. The file's header names the cache's identity
  and the deployment: the object store's kind, its bucket and its prefix. A
  file of this deployment, format and region size is read back: each closed
  region from its table, in the order of the log; a region whose table is torn
  by scanning its items; and the region that was open when the host stopped
  is given back. If the cache then holds more than the disk limiter's share,
  it gives its oldest regions back before it serves anything. A file with no
  header, a damaged one, or one of another deployment, format or region size
  is emptied and made again under a new identity. So a pulled VM opened again
  on the same node after a restart reads its pages from the disk. The file
  is in `SPROUTFS_CACHE_DIR`, which the host manifest makes a directory of the
  node's own, so it outlives the pod as well as the container
  ([the cache's file](#the-caches-file)).
- **Nothing on the disk is authority.** A copy the disk lost or damaged fails
  its key, checksum or envelope check and is read from the store. A newer
  checkpoint's page has a new identity, so the copy of the page it replaced is
  never read for it.
- **Forks share one copy.** A page the disk already holds, from another pull or
  a publication, is not copied again.

What the pull covers is the checkpoint a VM's volumes sit on. For a create,
that is the root the create published, which names the template's or the
parent's pages. For an open, it is the checkpoint the control record selects.
For a migration's receive, it is the checkpoint the destination opened. The
pages no checkpoint holds are not the pull's: they come from the source's
pager, on a fault or in the stream behind the guest, and they are this host's
own dirty pages from then on, resident or spilled, until the next checkpoint
publishes them. So a pull never asks the source for anything.

A fork's child is pulled once its root has published. The child reads its
parent's checkpoint and the pages the parent held that no checkpoint had, and
its root republishes those pages as its own. The pull waits for that root, so
it covers both kinds. Until then the child reads the second kind from the
parent's sealed pages or the parent's peer server, as any child does.

A checkpoint the VM publishes later adds its pages to the same copy as it
uploads them (`Publication.Keep`): each part once it is durable, and the
segments once the index object is. So a page the guest wrote after the pull
began, and that a later checkpoint published, is read from the disk too when
the pager evicts it. That covers every publication: an interval checkpoint, a
stop, and the fork point a fork publishes behind its children. The copying ends
when the VM stops running here, and the keeping only when the VM's handle
closes, after its last publication. A stop ends the VM's machine before it
publishes, so a pull that closed with the machine would keep nothing of the
stop's checkpoint; the GCE run of 2026-10-03 found that, and a reopened VM read
the pages it wrote last from the store. A write the disk
refuses keeps nothing more of that publication, and its pages are read from
the store. Inside the cluster share every publication fills the cluster
itself, a pulled VM's or not, so the keeping leaves those windows to it.

A stop or a migration away gives nothing up. The pages stay on the disk until
it needs their space, so a VM opened on this host again reads them there. The
mark stays with the VM. The orchestrator records it in its
table, and every open it drives carries it: a start, a recovery after a host
loss, and each receive of a migration, a drain's included. A migration's
handoff also carries the mark the source's machine has. The table is the only
record of the mark for a VM nothing runs. For a running VM its host reports the
mark, and a survey writes it down again. A host reports each marked
VM's progress in its status (`hostapi.VM.Pull`), and the disk's use beside the
page cache's (`Resources.CacheDiskUsed`).

## The list of caches

The hosts' disks are to become one cache for the cluster
([the plan](../plans/disk-cache-2026-10-02.md)). For that, every host must know
every cache, and which caches hold each window. The list and the ranks below
are that. For the windows the cluster cache is turned on for, a host keeps on
its own disk the stripes the list ranks its cache for ([the code](#the-code)),
and fills its peers with theirs ([filling the cluster](#filling-the-cluster)).
Nothing reads from a peer yet.

**A host's cache.** A host with a page cache disk reports its cache in
`/status`, under `cache`:

- `identity`: the identity in the cache file's header, in hex. It is drawn when
  the file is made, so a host restarted over the same file keeps it.
- `weight`: the size of the disk the cache is given, in steps of 16 GiB,
  rounded to the nearest, and at least one. The size is the filesystem less the
  free-space floor, the reserve and the promises, under the used goal. The host
  reads it once, when it starts. It never follows the limiter's share, which moves as the disk fills,
  because every change of a weight moves windows between hosts.
- `address`: the peer-server address, which `page_address` also reports
  under the name it had.

A host that keeps no cache disk, or gives it no space, reports no `cache` and is
in no list.

**The orchestrator's list.** Every survey asks each host pod for its status.
The orchestrator keeps the cache each pod last reported. A pod that answered
without one has none. A pod that did not answer keeps the cache it reported
before. A quiet host may be serving its windows perfectly well, and a list
that dropped it would move every window it holds. A pod the Kubernetes API no
longer lists leaves the list. Two pods that report one identity are a copied
disk, and the list holds it once, as the first pod by name reported it.
`GET /hosts` shows each pod's cache.

`GET /caches` serves the list: `k`, `m`, and the caches in identity order. The
code is the orchestrator's `SPROUTFS_CACHE_CODE`, written as `4+2`. Unset, it is
the code for the most caches the orchestrator has listed since it started:

| Caches | Code |
| --- | --- |
| 1 | 1+0 |
| 2 | 1+1 |
| 3 | 2+1 |
| 4 or 5 | 2+2 |
| 6 or more | 4+2 |

So a drain does not change the code. A code that followed the list would make
every stripe in the cluster a miss. An orchestrator that restarts while hosts
are drained can choose a smaller code, which costs a refill from the store. A
deployment sets the code for the size it usually runs at.

**Each host's copy.** A host reads the list as it starts, and then every ten
seconds on its own clock. It keeps the last list it read. A read that fails
leaves the list as it was, so an orchestrator that is down changes nothing.
Until a read succeeds, a host holds its own cache alone, under the code 1+0.
`/status` reports the list it holds under `caches`: `k`, `m`, the caches, when
the last read that succeeded finished (`read`), the reads, the failures, and
why the last read failed (`error`). The host logs when its reads start failing
and when they recover. Two hosts that hold different lists disagree only about
whom to ask, and the worst a stale list costs is a miss.

**Ranks.** The package `rank` places windows. A window is the pages of one
volume, in one aligned 2 MiB span, that one checkpoint published, and a segment
is a window of its own. Each cache scores a window by its weight over -ln(u),
where u is a 64-bit hash of the cache's identity and the window, mapped into
(0, 1). Equal scores go to the lower identity. `List.Ranks` is the caches
ranked 1 to k+m. `List.Holders` puts stripe i on rank ((i − 1) mod n) + 1, so a
list shorter than k+m takes the stripes round its caches. The scores are
compared in integer arithmetic, with a fixed-point logarithm, so hosts of
different architectures rank alike. A host alone ranks first for every window,
and its one stripe is the envelope whole.

## The code

Each envelope in the cluster cache is split into the stripes of an erasure code,
by Reed-Solomon (package `stripe`, over `github.com/klauspost/reedsolomon`,
MIT). Under the code k+m, the envelope is split into k data stripes of equal
length, the last padded with zeros, and m parity stripes are computed from
them. Any k distinct indices rebuild it. The code is systematic: stripes 0 to
k − 1 are the envelope itself, so a reader that has them does no decoding. A
code with k = 1 is whole copies: every stripe is the envelope, so 1+1 is two
copies, with no second mechanism for replication.

The code is a deployment setting, the orchestrator's `SPROUTFS_CACHE_CODE`
([the list of caches](#the-list-of-caches)), and every host reads it with the
list. An operator sets it for the size the cluster usually runs at:

| Hosts | Code | Extra disk | Survives |
| --- | --- | --- | --- |
| 1 | 1+0 | none | nothing: a single host has no peer |
| 2 | 1+1 | 100 % | one host lost or slow |
| 3 | 2+1 | 50 % | one host lost or slow |
| 4 or 5 | 2+2 | 100 % | two hosts lost or slow |
| 6 or more | 4+2 | 50 % | two hosts lost or slow |

**The share it is on for.** The cluster cache is rolled out a share of
windows at a time: `SPROUTFS_CACHE_CLUSTER_PERCENT`, 0 to 100, and 0 when
unset (`CacheConfig.ClusterPercent`). A window is inside the share by a hash
of the window, so every host puts it on the same side, and raising the share
only adds windows. A window outside the share is kept whole on the host that
reads it, under 1+0, whatever the list says, exactly as before there were
stripes; only the windows inside it are placed by the list's ranks and code.
The deployment leaves it at 0 until hosts read stripes from each other, so
that no deployment of three hosts or more, whose list holds other caches,
reads from the store a pulled page it read from its own disk before.

**What a host keeps.** For each window inside the share, `List.Holders` puts
stripe i on rank ((i − 1) mod n) + 1 of the window's n ranked caches. A host
keeps every index
whose holder is its own cache, under the list's code, and nothing of a window
the list does not rank it for. A list shorter than k+m takes the stripes round
its caches, so a host may hold several indices of one window; with 4+2 on five
hosts, any one host can still be lost. A host alone holds each envelope whole,
under 1+0. Each stripe is stored as an item that names its index, its code and
its envelope's length ([the page cache's disk](volumes.md#the-page-caches-disk)).

**What a read takes.** A read asks the disk for every index it holds of the
code the window is kept under, the list's inside the share and 1+0 outside, checks each item's key, index, code and checksum, and rebuilds the
envelope from the first k that pass. It then checks the envelope's SHA-256 as a
read of the store does. If that fails with more than k stripes in hand, it
rebuilds from other sets of k, at most 64 of them, and the stripes that do not
match the envelope that passed are named wrong and forgotten. With exactly k,
which one is wrong cannot be told, and all are forgotten. A stripe of another
code is a miss and never part of an envelope, so a deployment that changes
its code refills from the store and reads no wrong bytes.

Nothing is read from a peer yet. Inside the share, a host whose own stripes do
not make k reads the page from the store. So under 1+1 every host reads its
windows from its own disk, and under 2+1 and wider a host reads from its disk
only the windows it holds k indices of. On a 2 MiB envelope, splitting the
stripes of 4+2 takes 0.17 ms and rebuilding from four stripes 0.07 ms, or
0.19 ms with two of them parity; on a 4 KiB envelope, 1.0 µs, 0.6 µs and
1.4 µs (Apple M5 Pro, `go test ./stripe -bench .`).

## Filling the cluster

Inside the share the cluster cache is turned on for, a host puts each window
it has in hand on the caches the list ranks for it. That is a **fill**. Three
things fill:

- **A read of the store.** The run the store served is split under the list's
  code, and each stripe goes to the cache that holds it. The fill starts once
  the read's callers have their pages, never before.
- **A publication.** Each part is filled once its PUT has succeeded, and the
  segments once the index object's has. So no cache holds the bytes of a part
  the store refused. Every publication fills: an interval checkpoint, a
  capture, a stop, a fork point and a template import.
- **A pull.** What a pull copies of a window inside the share is a fill.

A fill splits each envelope with `stripe.Split` and puts stripe i on the cache
`List.Holders` names. This host's own stripes go to its own disk. Every other
cache that holds some gets one **keep**: a peer-server request with that
cache's stripes of the window, each as its disk stores it, with its own
checksum.

**Nothing waits on a fill.** A host holds the fills handed to it, and its
peers' keeps, in one queue, `CacheConfig.FillQueueBytes` (64 MiB by default).
One worker does the fills one at a time, in the order they were handed over.
It asks for a read's fill right, writes this host's own stripes and sends each
keep, and it takes the next fill once the last keep is answered. Every write
to this host's own disk, its own fills' and its peers' keeps', is done by
another worker, one at a time. Keeps go out within a rate per host,
`CacheConfig.FillBytesPerSecond` (128 MiB/s by default, with a burst of one
second of it), and within the host's background budget at the fill priority. A
fill that finds the queue full, the rate spent or the budget without room is
dropped. Its window is read from the store the next time. A fault, a
publication and a pull never wait for a fill.

**One fill at a time.** Keeps sent beside each other reach a holder's link,
its connection and the background budget in whatever order the Go scheduler
runs them. So which keep a dropped frame or a partition takes, and which finds
the budget full, would not follow from the order the fills were handed over
in, and a simulated run would not reproduce. A keep is background work within a
rate, so waiting for its answer costs a fill nothing it needs. The writes have
a worker of their own because a keep waits for its holder's writes, and a
holder's writes must never wait for that holder's own keeps.

**Fill rights.** A cold burst would fill one window many times: many hosts miss
it at once, each reads the store, and each would send its stripes. So a fill
from a read needs the window's fill right. Behind its own read, the reader asks
the window's rank 1 with a stripe read that wants no bytes. Rank 1 gives the
right to the first reader that asks, once per window per interval
(`CacheConfig.FillRightInterval`, ten seconds by default), and only while it
holds nothing of the pages asked for. Every other reader sends nothing. A rank
1 that is down or cannot be asked gives no right. A publication and a pull need
none. Until hosts read stripes from each other, a cache answers a stripe read
with no stripes, only the right.

**What a cache takes.** A cache takes a keep only for a window inside the share
that its own list ranks it for, under its own list's code. Its own fills are
held to the same rule, because the list a fill was placed by may have changed
since. It drops every stripe it holds, or is writing for another keep, as a
duplicate. Each write asks the disk's write budget at the fill's priority: a
fill from a publication is refused last, and a fill from a read before it (see
[budgets](#budgets)). A keep says which it is.

**What a host reports.** `/status` reports under `cache_fill` the windows the
host filled from reads and from publications, the reads it filled nothing of
for want of the right, the rights its cache gave out, the stripes it sent that
their holders kept and their bytes, the stripes kept on its disk, the stripes
dropped by reason, the duplicates, the stripes of keeps it refused, and its
queue. The reasons are `queue`, `rate`, `budget` (this host's background
budget), `busy` (the holder's budget for this host), `down`, `stale` (an
address that answered for another cache), `peer` (the holder dropped it),
`disk` (this host's disk refused it) and `failed`. `/metrics` carries the same
as `sproutfs_cache_fills_total`, `sproutfs_cache_fill_*` and
`sproutfs_cache_keep_stripes_refused_total`.

Under 1+1 every host holds each window whole. So a VM suspended on one host
and opened on the other reads its pages from that host's own disk, where the
first host's publication put them. Under a wider code a host holds fewer than
k stripes of most windows, and reads those from the store until hosts read
stripes from each other ([the plan](../plans/disk-cache-2026-10-02.md), step
7).

## Nested VMs

A nested VM is experimental. It is a VM whose guest may run VMs of its own. A
create asks for one with `nested` (`sproutfsctl create --nested`), and only an
Intel x86_64 host runs one. Every other guest is offered neither VMX nor SVM, so
it cannot start a VM at all.

A nested VM is captured, suspended, forked and migrated like any other VM, and
its RAM is paged like any other VM's. That rests on one thing the Firecracker
fork does: it never offers a nested guest the three VMX controls that make the
host's KVM map the guest's pages behind the host page tables (TPR shadow,
APIC-access virtualisation and posted interrupts). The guest's own VMs run
without them, with more exits for their interrupts. The fork also saves and
restores the guest's nested state in every snapshot, so a VM its guest runs
survives the capture, the fork or the migration. AMD's SVM has pins of its own
that the fork does not remove, so an AMD host runs no nested VM.
`vmmachine/nested.go` says why, next to the code, and
[writers that bypass the page tables](vm-memory.md#writers-that-bypass-the-page-tables)
says what the pager relies on.

The VM stays nested wherever it runs, because its checkpoints record it. A
create from a nested VM's checkpoint makes a nested VM only if it asks for one
again.

## Budgets

The host takes one `Resources` owner, which accounts only RAM: the pager's
pages. `Status().Resources` reports its reservations and configured total.

One disk limiter, `resource.DiskLimiter`, decides how much of the node's disk
the host may use, for everything the host writes. It reads the filesystem
under the scratch directory (`platform.DiskSpace`) every ten seconds on the
host's clock, and whenever it is asked.

The users that cannot give space back are counted at their promises, not at
what they hold:

- each pager's spill file, at the dirty pages it may hold, which is its share
  of `SPROUTFS_SPILL_BYTES`;
- the ephemeral pager's spill file, `SPROUTFS_EPHEMERAL_BYTES`;
- each running VMM's staging, at the largest state a capture may write, 64 MiB;
- an image staged for an import, at what it holds.

A store must never fail for want of disk, so each spill file's whole extent is
allocated (`fallocate`) when its pager starts, and each is counted at that
promise. A sparse spill file would not be enough, however the limiter counted
it: the space it had not used yet would be only free space on the filesystem,
which another writer on the node can take. With the extent allocated, another
writer finds the filesystem full, not the guest. A released spill slot keeps
its blocks for the same reason. A disk that cannot allocate a spill file's
extent refuses the host at start, with `ErrInvalidConfig`.

The limiter keeps every goal it is given, and needs at least one:

- `SPROUTFS_DISK_FREE_BYTES`, the least the filesystem keeps free;
- `SPROUTFS_DISK_FREE_PERCENT`, the least share of it kept free;
- `SPROUTFS_DISK_USED_BYTES`, the most the host holds.

A host given none keeps a tenth of the filesystem free. The floor is the larger
of the two free-space goals. The room is what the filesystem has free plus what
the host holds. The cache's share is the room less the floor, the promises and
the reserve, less a band. The used goal caps the promises and the cache
together. The smaller share binds, and `/status` and `/metrics` name the goal
that binds. Nothing else caps the cache's disk.

The reserve, `SPROUTFS_DISK_RESERVE_BYTES` (1 GiB by default), is what the
cache leaves free above the floor for promises not yet made. A promise may take
it, and the cache then gives back what restores it. It matters most where two
hosts share a filesystem. Each sees the other's cache only as space the
filesystem does not have free. Without the reserve, a cache that filled the
disk to the floor would leave a host whose own cache is empty no room to start
or receive a VM, and nothing would tell the full cache to give space back.

The band keeps the cache back from the floor and the reserve by a fifth of the
headroom it has left, at most `SPROUTFS_DISK_BAND_BYTES` (4 GiB by default). As
the disk fills, the share falls a little at each reading, so the cache gives
back a few regions at a time rather than all of them at once. At or below the
floor there is no band, and the share is what keeps the floor.

The limiter acts on readings smoothed over a minute, as FoundationDB's
Ratekeeper smooths free space. One odd reading moves them about a seventh of
the way, so it cannot empty the cache. A reading that fails, or that says more
is available than the filesystem holds, changes nothing and is reported.
`/status` shows the last raw reading beside the smoothed one.

A cache over its share is told to give regions back until it holds one region,
64 MiB, less than its share. A cache within that region of its share is left
alone, so a share at a region's edge does not evict and refill.

When even an empty cache does not fit, the host has promised more than the
disk can keep. A host whose promises the filesystem could not keep under its
goals with nothing else on it refuses to start, and says so. One that does not
fit only because other writers hold space starts, and reports itself unready
with the reason. The other writer may be another host's cache on the same
node, which gives space back as its own goals push it. A host that refused
instead would truncate its spill files as it exited, and the other writer would
never see the pressure. While it is unready, as when the disk fills from
outside later, the host refuses to start a VMM or stage an image that would
promise more. It never takes space back from a spill file.

The limiter also keeps the disk cache's write budget:
`SPROUTFS_CACHE_WRITE_BYTES_PER_DAY` on average, at most
`SPROUTFS_CACHE_WRITE_BURST_BYTES` ahead of it (an hour's average by default).
It is measured by the device's own count of bytes written
(`platform.DeviceWrites`). On Linux that is the `stat` file of the block device
under the scratch directory, and a host that cannot read one refuses a budget.
The device counts every writer, so others' writes are charged too. The cache's
own writes are charged when they are admitted, and not again when the device
counts them. A count that goes backwards is a replaced device, and is counted
from there. A count that jumps charges at most a burst of debt.

The cache asks before each write, with a priority: 0 for a repair, 1 for a
second chance, 2 for a fill from a store read, 3 for a fill from a publication.
A pull's copy is written at a publication's priority, and a keep at the
priority its sender says it has. Priority p is admitted only while (3 - p) quarters of a burst are left after
the write. So as the budget runs down, repairs are refused first and fills from
publications last. A refused write costs a store read later, never a wrong
byte.

The page cache's disk is the cache. It is no promise: it holds what the
limiter leaves, `CacheShare()`, and nothing else caps it. Each write it makes asks `Admit`, at the priority of its kind. When the
share falls below what it holds, the limiter calls its `Shrink`, and the disk
gives regions back, oldest first and with no second chance, until it holds its
share less one region. A pull that does not fit in the share is refused before
it fetches anything. A host that restarts finds the cache's file holding what
it kept before the cache is made, so until the cache registers, the limiter
counts what the file holds as the cache's, not as another writer's. The cache
read back is then fitted to that share before it serves anything.

- **VMM staging files** live in each process's own directory under the scratch,
  and are removed with the process. Configuration and restore files are removed
  after startup. A capture's state file is read back and deleted. The capture is
  limited to 64 MiB by the file-size limit the VMM process runs under, so an
  oversized state file is refused instead of read. Failed cleanup is reported
  and can be retried. The scratch owner's `Close` refuses live VMMs and closes
  stopped ones. Close the processes first, then the scratch owner.
- **VMM console output** is drained into a 1 MiB in-memory ring buffer per VMM
  process. It uses no disk and no budget. Output beyond that capacity replaces
  the oldest bytes; it does not block or kill the VM. The ring is removed with
  the process. A read names an offset in the whole output and returns at most
  256 KiB. If a read asks for an offset before the retained output, it is
  answered from the oldest retained byte and reports that offset. This is how a
  reader that fell behind learns that output was dropped. This is diagnostics,
  not an audit log.
- **An exec's answer** comes from the guest's agent, and a guest runs untrusted
  code. So the host bounds what the answer can cost it. It waits for the
  command's own timeout plus 30 seconds. A request that names no timeout gets
  the agent's 30 seconds, and none gets more than ten minutes. It reads at most
  `guest.MaxResultBytes` of the answer, which is two full output streams as JSON
  spells them, and 64 KiB of its headers. An answer that is late, too long or
  not an exec result fails the exec. The error quotes at most 512 bytes of what
  the agent sent.
- **The page cache** keeps decoded pages within its own cap,
  `SPROUTFS_CACHE_BYTES`, and evicts unused entries before a retention fails.
  The cap is separate from the allotment that a guest's pages come from, so
  nothing has to be reclaimed between them. A miss that does not fit is served
  without being kept. Concurrent misses remain bounded.
- **Checkpoint uploads** are bounded by the checkpoint store, not per
  publication: half this machine's cores, between 8 and 64, shared by every
  checkpoint the host publishes. The part builders behind them have a separate
  bound: a quarter of the cores, between 2 and 8. Together with the parts in
  flight, this determines how much memory publication uses on this host.
- **Open VMs**: one manager owns at most 4,096 live handles by default, with the
  per-write bound described in [volumes](volumes.md#writes). There is no bound
  on unpublished bytes. A write waits for nothing, and the bytes it leaves
  unpublished are what losing the host would cost. `Stats().DirtyBytes` reports
  that amount, summed over every VM the host runs.
- **The pager** budgets resident, logical and dirty pages across the host; see
  [managed VM memory](vm-memory.md#bounded-host-pager). A VM is admitted
  against the logical budget. That budget bounds per-memory-region metadata, and the
  pager checks it one attachment at a time. Without an earlier check, a VM
  whose memory regions exceed it would have its first memory region admitted and its VMM
  started, and would then be killed. So a create, an open and a receive each
  check what the cap has left before starting a VMM, and refuse a VM that
  cannot fit. A fork is a handoff, so the receive that takes in a child admits
  it, on whichever host that is. The orchestrator admits the whole fan-out
  against that host before the parent is paused for it.

The host's status reports:

- cache usage and its cap, in memory and on disk;
- what the disk limiter chose: the goals and the one that binds, the raw and
  smoothed readings, the floor, the reserve and the band, each promise and what
  it holds, the cache's share, the write budget, and why the host is unready;
- the volume manager's totals;
- the pager's counters, including the free space in the logical cap, which is
  what admits a VM;
- the object traffic;
- what the peer server has served;
- under `peers`, each host this host has asked anything of: the version it
  speaks, its connections of each class, and whether it is down, and why, or of
  a release this host cannot talk to;
- its cache's identity, weight and address, and the list of caches it holds;
- what its fills of the cluster did, under `cache_fill`
  ([filling the cluster](#filling-the-cluster));
- the page cache's disk under `cache_disk`: the file it claimed, its
  identity, the regions and entries it holds, the reads it served without the
  object store (`hits`), the copies it lost, the regions it gave back, and what
  the host did with the regions it found in the file when it started
  (`opened`: read back from their tables, scanned, or given back). `/metrics`
  carries the same counters as `sproutfs_cache_disk_*`.

### The cache's file

The page cache's disk is a file in `SPROUTFS_CACHE_DIR`, or in the scratch
directory where that is unset. The host takes the first file there, `cache-0`,
`cache-1` and so on up to `cache-63`, whose lock no other process holds, and
holds the lock (`flock`) while it runs. The lock ends with the process, however
it ends. So two hosts that share the directory never share a file, and a host
that starts after another exited takes the lowest file free, with what that
host kept in it. The cache's identity is in the file's header, so it moves
with the file. A host that finds a file of another deployment empties it.

The directory must be on the filesystem the scratch directory is on, because
one limiter measures one filesystem. A host whose cache directory is on
another refuses to start and names both.

The host manifest makes `SPROUTFS_CACHE_DIR` a `hostPath` directory on the
node's disk, one per namespace, and leaves the spill files and the VMM scratch
in the pod's `emptyDir`. A restart is a host loss for those, and the kubelet
frees their space with the pod. The cache holds only copies of what the bucket
holds, each under a name that never names other bytes, so a replaced pod reads
it back. It is a `hostPath` rather than a local `PersistentVolume`, because a
claim would pin the pod to its node. A pod that moves to another node starts
there with whatever cache that node holds. A cache file no host holds stays
on the node until a host takes it again. Until then the hosts running there
count it as space another writer holds.

Hosts that share a filesystem see each other only as space it does not have
free, so each sets `SPROUTFS_DISK_USED_BYTES` to its part of the disk. Then the
first cache to fill does not take all of it, and a host that restarts finds
room for its spill files at once. Within that, the reserve keeps room for a
VM's staging however full the other caches are.

## Shutdown

Quiesce caller operations first. Closing the host stops the checkpoint loops,
then the peer server and the table of peers, then the VM handles, and then
closes the cache. Each VM
publishes a final checkpoint if anything is dirty, so an orderly shutdown loses
nothing. A failure there is logged and does not block the release, and the
bytes it could not publish are lost. Cancelling the wait stops only the wait.
Cleanup continues, and the close can be retried with a fresh context.

Close proves nothing about object storage. A host that exits without closing its
VMs loses every write since their last checkpoints.
