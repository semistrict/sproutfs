# Hosting

A host runs VMs. One process provides that role over one object store, one
network and one RAM allotment. The host has no durable local state. A VM's
authority is the epoch in its [control record](metadata.md), and its data is the
checkpoint that record selects. The host's identity is its page cache disk's,
which names it in [the membership](#the-membership). A host dials the
peer-server address a [handoff](migration.md) carries and the addresses of the
membership's members, which its cache reads from and
[fills](#filling-the-cluster). The [transport](#transport) decides who may
reach them.

## Assembly

All host logic is in `host`. `Host` is what the deployment runs on this
machine. Starting one requires a resource owner with a positive RAM allotment,
the network its peer server and handoffs run over, and the object store with
the deployment's prefix. It owns, all with the same lifetime:

- the control client that reads and writes the deployment's control records;
- one checkpoint store with the host's shared
  [page cache](volumes.md#page-cache), which has its own cap and does not use
  the host allotment;
- the volume manager that opens this host's VMs;
- the peer server, when a migration address is configured, and the table of
  peers that every receive dials through;
- the loops that keep its VMs durable and fenced.

A host without a migration address can neither drain nor receive.

The supervisor around it is `host.Start`, which returns the `host.Service` that
the command serves. The supervisor owns the two pagers, each with its own arena
and spill file. By default both use 2 MiB pages from the node's HugeTLB pool.
`SPROUTFS_RAM_PAGE_BYTES=4096` runs RAM at 4 KiB on an ordinary memfd: a tenth
of the memory for forks that write little and in scattered places, and slower
at everything else. Such a node sets its shared memory's transparent huge pages
to `advise`, so the RAM arena can allocate a zero run's whole 2 MiB blocks as
huge pages; see [the arena](vm-memory.md). `SPROUTFS_PMEM_PAGE_BYTES=4096` runs
PMEM, and the ephemeral pager with it, at 4 KiB, to measure whether 4 KiB pages
save enough checkpoint and fsync traffic to be worth the extra index entries
and faults. A disk stays a whole number of 2 MiB, because Firecracker requires
it of a PMEM device.

The supervisor builds the host's [disk limiter](#budgets) once the spill files
are open and hold their extents. It refuses to start when the disk cannot
allocate a spill file, or cannot keep its promises under its goals. It also
drives the VMM processes, owns the templates, and reaches the agent in a
guest. A `vmmachine.Starter` starts the VMM ([running the VMM](#running-the-vmm)).
`cmd/sproutfs-host` contains only its configuration, its HTTP handlers and the
wiring between them.

Every VM has one RAM volume, `ram0`, plus one volume per PMEM device. Each VM
gets a PMEM device, `root`, which the guest boots from. A VM created with an
ephemeral disk has a second PMEM device, `ephemeral`, after the root. The
supervisor passes a `vmmachine.Scratch` to each VMM configuration.

A host given `SPROUTFS_EPHEMERAL_BYTES` runs a third pager for
[ephemeral disks](volumes.md#ephemeral-disks). That setting is its spill file,
which is every ephemeral disk the host admits, and
`SPROUTFS_EPHEMERAL_ARENA_BYTES` (256 MiB by default) is its share of the
HugeTLB pool. Both are whole 2 MiB pages, and the host's memory allotment
grows by the arena. A host without the setting refuses a VM with an ephemeral
disk. A deployment that creates ephemeral disks gives every host one, because
such a VM may be opened, received or recovered on any host. The orchestrator
places a create by memory alone, so a host without room for the disk refuses
the create.

`GET /stored?tenant=<tenant>` reports what one tenant's VMs hold in the object
store, per VM, for billing. It lists the store, so any host answers for every
VM of the tenant, including VMs no host runs and deleted VMs whose pinned
checkpoints remain. See [billing](volumes.md#billing).

`GET /metrics` is `hostapi.Metrics` of the host's `Status`, in the Prometheus
text format. `hostapi.MetricFamilies` gives the same metrics as data: each
family's name, help, kind and samples, with a histogram's buckets, sum and
count. An embedder registers them with its own metrics library, or serves the
text of `hostapi.Metrics`; sproutfs links no metrics library. The metrics cover the pager's dirty and window waits and
stalls, refused mappings and repeated faults; histograms of faults, loads,
seals and object-store calls; interval checkpoints; migrations, forks and
receives by outcome; VMs given up and why; template imports; peers by whether
they are up, down or incompatible (`sproutfs_peers`); and
`sproutfs_build_info`. No series names a VM or a tenant.

A host's counters start at zero with its process, so a scrape never sees what
a host counts just before it exits. These events are recorded elsewhere:

| Event | Where it lasts |
| ----- | -------------- |
| A drain moving a VM | the drain reports to the orchestrator for each VM, the `host: drained` log line, and the destination's `sproutfs_receives_total` and `sproutfs_received_pause_seconds` |
| The final checkpoint of each VM at shutdown | the VM's control record, which selects it; `volume: final checkpoint on close failed` when it fails |
| The whole shutdown | the `host: shut down` log line, with what the host did in its life |
| Exiting while still serving migrated pages | the `host: exiting while still serving migrated pages` log line |
| A fatal error | the `sproutfs-host: exiting` log line |
| The process killed outright | nothing from the process. The last scrape's `sproutfs_loss_window_seconds` bounds what its VMs lost, and each VM reopens at the checkpoint its control record selects |

**Timing a start.** Each create, open and receive logs `host: a VM runs`, and
each fork logs `host: a VM forked` on the parent's host. The line names the VM
and the request (`how`), and gives each step in milliseconds from the
request's arrival: the fork and root of a create, the open and state read of
an open, the receive's open, the VMM's phases (process, state load, sessions,
ready), the resume (`release`), and the parent's confirm, pause, seal, pin and
handoff. It also gives the object-store calls by kind
(`control record conditional put`, `index get`, `part get`), their union
(`store_ms`), the calls made after the guest ran (`store_after`), and each
memory region's attach and populate. `running_ms` is when the VMM reported the
vCPUs running. One second later, `host: a VM's first faults` counts each memory
region's guest faults before the guest ran and in that second, with the time
the guest waited and its first fault. `cmd/sproutfs-startbench` joins these
lines with its own timings
([start latency](measurements/gce-start-latency-2026-10-04.md)).

### Transport

Hosts reach one another through the
[peer server](migration.md#the-peer-server). Each host serves its own on one
port, `SPROUTFS_PAGE_SERVER_PORT`, and dials the others through its table of
peers. `platform.Network` frames that channel over a `platform.Transport`: a
stream listener and a stream dialer. `adapters.NewNetwork` frames over
`adapters.TCP`, the default. It is plain TCP and authenticates no peer, so
hosts on it must share a trusted network, and a network policy must keep
everything else off the peer server's port.

The framer sends each frame in one vectored write, and refuses one over
16 MiB. A payload that is a range of a file goes with `sendfile` when the
stream is a TCP socket on Linux. `adapters.TCP` turns keepalive on, probing
after five idle seconds every two seconds three times, and on Linux sets
`TCP_USER_TIMEOUT` to ten seconds. The peer server's pings find a dead peer
sooner; see [liveness](migration.md#liveness).

A deployment that authenticates its hosts passes `adapters.NewNetworkOver` its
own transport, such as mutual TLS. Its listener closes a peer it cannot
authenticate, and its dialer fails on a source it cannot authenticate.
Sproutfs never sees the credentials. `internal/testnet.MutualTLS` is such a
transport, and `host/transport_test.go` migrates over it: two hosts that trust
each other migrate a VM; a peer server serves no stranger; a destination
fetches nothing from a source it does not trust.

A destination treats a refused source as an unreachable one: it waits for the
pages only the source holds until its caller gives up. The receive then fails
and the VM is discarded.

The host API is not on this channel. `api/host.NewClient` takes an
`*http.Client`. An embedder that runs the host as a library serves its own
API.

Only the command chooses the adapters. `sproutfs-host` builds the object
store, the plain TCP network and the node disk, and passes them in.
`SPROUTFS_OBJECT_STORE` selects the object store: `gcs`, the default, or `s3`.
The port is the conditional-write contract that the conformance suite in
`platform/internal/real` states, and each adapter runs it against an emulator.
The S3 suite also runs against a real bucket when `SPROUTFS_TEST_S3_BUCKET`
names one. S3 names an object written in one PUT by the MD5 of its body, so a
compare-and-set there is a compare-and-set on the bytes. `vmmachine` receives
a `platform.Disks` for each VMM's staging directory.

### Bounds on the object store

On GCE, one GET of Cloud Storage once waited 52 minutes for its response
headers. Every request to an object store now waits within two bounds:

- **First byte.** How long a request waits for the store to start answering:
  the whole reply of a HEAD, a LIST or a DELETE, the headers of a GET, and the
  reply of a PUT once the store has taken its whole body.
  `SPROUTFS_STORE_FIRST_BYTE_TIMEOUT`, 10 s by default.
- **Stall.** How long a body may go between two bytes: a GET's body while its
  reader waits, and a PUT's body while the store takes it.
  `SPROUTFS_STORE_STALL_TIMEOUT`, 10 s by default.

There is no bound on the whole request, because objects range from a few
hundred bytes to a 64 MiB part. Time a caller holds a body without reading it
does not count.

An attempt past a bound is cancelled. Then:

- **A HEAD, a GET or a LIST** is made again, for as long as the caller's
  context lives. A guest fault has no deadline, and failing it would kill the
  VM. Each timeout is logged and counted.
- **A PUT with a condition** is made again. If the first attempt landed, the
  object it wrote refuses the second. Every caller of a conditional write
  settles a refusal against what the store holds: a control record by its
  writer's nonce, a part or an index object by its digest, a hot tier fill as
  found there, and a change of the membership by reading it again.
- **A PUT without a condition, or a DELETE,** is not made again, since a
  second attempt could undo a later writer's change. The caller gets
  `bounded.ErrTimedOut`: unavailability with an unknown outcome.
- **A GET whose body stalls** is asked for the rest of its bytes. The new
  reply must have the same ETag and size, or the read fails with
  `bounded.ErrChanged`. Only a control record or the membership can change.

`adapters.NewObjectStore` is the only way a process opens a store, and it
returns a `*bounded.Store`, over GCS and S3 alike. `host.SupervisorConfig`
takes only a bounded store and hot tier. A hot tier's bucket uses the
deployment's bounds.

`/status` reports per operation `first_byte_timeouts`, `stall_timeouts` and
`retries` beside `calls`, in `store` and in `hot_tier_store`. `/metrics`
carries them as `sproutfs_store_timeouts_total{operation,bound}` and
`sproutfs_store_retries_total{operation}`, and as `sproutfs_hot_tier_store_*`.

There is no hedged GET yet. A read of the cluster already reads the store past
its own bound, and a read of the hot tier falls back to the regional bucket.
The delay to hedge at needs the tail of times to first byte on GCE, which is
not measured.

## Running the VMM

The host prepares a VM's memory and drives its VMM. `SupervisorConfig.Starter`
starts the VMM process on every path: a create, an open, and a receive of a
migrated or forked VM. An embedder writes its own Starter. `cmd/sproutfs-host`
uses `vmmachine.Firecracker`, which runs Firecracker directly.

A start has three steps.

1. The Starter calls `Launch.Prepare` with a placement: the host directory for
   the process's files, the same directory as the VMM names it, and the user
   the VMM runs as. The two paths differ when the VMM runs in a jailer's
   chroot. `Prepare` creates the directory, gives it to that user, and opens
   one socket per memory region. It returns the API socket path, the RAM
   socket and size, and the managed PMEM devices, all as the VMM names them.
2. The Starter starts the VMM however it likes: under a jailer, in a network
   namespace and a cgroup, with network interfaces, drives, read-only PMEM
   files and a vsock of its own. For a boot, `Memory.Configure` adds the
   managed memory to the Starter's configuration document. For a restore, the
   Starter adds new host names for moved devices to `Memory.Load`. It returns
   the process as a `vmmachine.VMM`.
3. The host accepts the memory sessions, issues the snapshot load of a
   restore, and waits for every session before the guest runs, because the
   sessions attach during the load.

A Starter reports the managed-memory API revision its VMM speaks, and the host
refuses to start unless it is `vmmachine.APIRevision`. See
[the Firecracker build](vm-memory.md#firecracker-build-and-process-lifecycle).

The memory sessions admit only a connection from the PID the Starter reported,
so a jailer must exec the VMM in the same process, without daemonizing or
forking into a new PID namespace.

Drives and plain PMEM files that a Starter adds must be read-only, because a
checkpoint holds only the volumes. The VMM refuses to capture a VM with a
writable one. A restore's devices are in its VMM state, so drives and PMEM
files keep the paths of the VM's first boot on whichever host restores it; a
Starter uses paths that are the same on every host, such as paths inside its
chroot. Network interfaces and the vsock can move, through `Memory.Load`.

A `vmmachine.ConsoleVMM` keeps a console, and a `vmmachine.VsockVMM` names its
vsock socket; the host reaches a guest's console and agent only through these.
`vmmachine.Spawn` runs a command, which may be a jailer, as a child with its
console kept in memory. `Starter.Boots` says whether the Starter can boot a
kernel.

A process restart is a host loss. Opening the scratch deletes its VM
directories and recreates them empty, and the spill files are truncated. There
is no reconciliation of VMs and no lock held across processes. Only the page
cache's disk survives, because it holds only copies of what the store holds
([the cache's file](#the-caches-file)). The deployment reopens each VM, on
whatever host, from the checkpoint its control record selects. Each host
process needs its own scratch directory.

`sproutfs-host` reads its whole configuration from the environment and reports
every configuration problem it finds. It serves the host API over HTTP:
status, create, open, fork, capture, console, exec, migrate, receive,
released, drain, stop, kept, release and delete. Each handler is one call on
the supervisor plus the shared JSON failure shape. The API's types live in
`api/host` and do not link the runtime, so the host converts a handoff at its
boundary. The GCS store the command builds is metered per operation. Setting
an endpoint selects an emulator instead of the ambient Google credentials.

Every request except the kubelet's probe must carry the deployment's shared
bearer token, `SPROUTFS_API_TOKEN`, or is refused with 401. The same token and
middleware serve the orchestrator's API. The token neither grants authority
over a VM (the epoch in the control record does) nor identifies the caller; it
shows only that the request comes from inside the deployment.
`deploy/30-networkpolicy.yaml` restricts the ports to the deployment's own
pods. Draining is a POST, because crawlers issue GETs, so the preStop hook
runs `sproutfs-host drain`.

### Templates

Creating a VM forks a template. Each guest image is imported once into a VM
that never boots, and every VM created from it inherits the template's
published checkpoint without copying bytes. A template is an ordinary VM with
an ordinary control record, under a reserved identity namespace that keeps it
out of the deployment's list of VMs.

A template's identity is `template-<sha256 of the image file>-<pages>`, where
the pages are its RAM volume's and its root's, as `2m` or `4k`: a host of 2 MiB
RAM pages and 4 KiB disk pages names `template-<digest>-2m-4k`. So every host
configured with the same image and the same pages names the same template, and
hosts of different pages sharing a store import the image once for each
geometry, because a pager maps only a volume published in its own page. A
starting host acts on the template's state in the deployment:

- **published**: its control record pins the checkpoint it selects. The host
  reads that checkpoint and remembers it for `create`, and writes nothing.
  This is the case for every restarted pod, and for the second host of a pair.
- **absent**: the host creates the template, writes the image into its root
  volume, checkpoints it and pins that checkpoint, which publishes it.
- **a record with no pin**: an import has not finished. The host waits for it.
  After the wait it recovers the template like any other VM: it takes the
  epoch, which fences the importing host if it is still alive, and imports
  again. Checkpoints of the interrupted attempt stay, as after any takeover.

Two hosts that start at once race on the record's create-if-absent; the loser
is then in the third case.

A configured image is imported as a public template, of no tenant
(`control.Public`). An image imported on request for a tenant is that tenant's
alone. A create, open, capture or receive that names a template is refused.

An import reads only the image's data. For an `*os.File` the host asks the
kernel for its data extents with `SEEK_DATA` and `SEEK_HOLE`; a source that
implements `host.SparseSource` reports its own. The digest hashes each hole as
zeroes and the import writes nothing for it, so a sparse image and the same
image written in full name one template. A filesystem that cannot report
holes, such as FUSE without `lseek`, is read in full.

The imports run at startup, behind the API. A host is ready when every image
it is configured with has a published template, whichever host imported it.
A host that cannot read its images stays unready and reports why. Liveness is
a separate endpoint, so a host still importing is not restarted.

An image can also be imported on request (`ImportTemplate`, `POST /templates`,
with the image as the body). The host stages an image that is not a seekable
file under its scratch directory, because an import reads it twice: once for
the digest, once for its bytes. It reports the template's identity, and any
host of the same pages creates from `template-<digest>-<pages>` without the
image. An identity nothing
imported is refused, as is one whose import has not published. A host with
`SPROUTFS_TEMPLATES=none` has no images of its own, is ready at once, and
creates only from templates imported on request.

No host deletes a template, because another host may be forking from it.
Templates nothing creates from any more are left to a collector, like every
other pinned checkpoint.

A create names the VM's RAM, the size its root volume grows to, and its
processors. It publishes the VM's first checkpoint at that shape before the
first boot, as a cold boot does
([resizing at a cold boot](#resizing-at-a-cold-boot)). What a create does not
name is the template's RAM and disk, and the host's processor count.

A create may ask for an ephemeral disk (`CreateRequest.Ephemeral`,
`sproutfsctl create --ephemeral 8G`), in whole 2 MiB pages. The fork adds it
zeroed, the VM's first checkpoint records it, and admission charges it to the
ephemeral pager. A create from a checkpoint that already has one gives it the
new size, or keeps its size when the request names none.

Software in the guest is reached over the VM's vsock, which carries only exec.

## The checkpoint loop

`Host.AddMachine` registers a VM's VMM process, which makes the VM drainable
and starts its checkpoint loop. The host checkpoints the disks of every VM it
runs every `Config.CheckpointInterval`, sixty seconds by default, jittered by
up to an eighth either way. It waits for each publication before scheduling
the next. Each checkpoint is `CaptureDisks`: its pause seals only the VM's
PMEM memory regions and captures no VMM state, so RAM is never uploaded on the
interval, and a VM opened at such a checkpoint is cold booted over its disks
(`Host.Starting`). The interval bounds how far losing this host rewinds a
guest's disks. A failure is logged and retried at the next interval. The loop
stops when the VM is removed or migrated away, or when the host closes. An
interval is skipped while a fork point has sealed the VM. A negative interval
disables the loop.

A VM may ask for its own interval (`MachineTerms.CheckpointInterval`, carried
by `CreateRequest` and `OpenRequest`). A positive one is clamped to between
`Config.MinimumCheckpointInterval`, one second by default, and the host's
interval, so the loss window is never shorter than any VM's interval. Its
flush bound, where the host configures none, is two of its own intervals. A
negative interval asks for none: the VM is held to no loss window, every flush
completes at once, and it is checkpointed only when it stops, moves, or the
pager needs its dirty pages back. A migration carries the interval to the
next host. A fork's children take the host's, and an open asks again. The
orchestrator does not forward it. `Status` reports each VM's resolved
interval.

`Config.LossWindow` bounds the rewind when publications fail. It is five
minutes by default, zero disables it, and it is never shorter than the
interval. While a VM's oldest unpublished write is older than the window and a
sealed checkpoint of it is uploading, the pager admits no further dirty pages
for it; see [managed VM memory](vm-memory.md#bounded-host-pager). The window
applies to disks only, because no checkpoint the loop takes publishes RAM.

The loop takes a checkpoint out of turn when a VM's oldest unpublished write
reaches three quarters of the window. The clock triggers this, because a guest
that only rewrites pages it has already dirtied never reaches the pager's
window check. After a failed attempt the loop waits for the window itself.
If a publication fails while the window is exceeded, the loop keeps the pages
sealed and publishes the same checkpoint again, under the same reference,
after an eighth of the interval, doubling up to the interval. The guest's
stores, and the vCPUs that made them, are held behind it, so a fresh pause
would not be possible. The retries stop when the loop does. Inside the window,
a failed publication gives its pages back and is retried a full interval
later, unless the checkpoint was one asked for out of turn: whatever asked,
a flush waiting for the journal to be named or a ring that needs room, asked
once and is still owed it, so it is tried again after the same backoff. A
checkpoint that landed and whose seal the pager could not end reports that it
landed; the seal is logged and ends at the next capture's release.

`Config.FlushBound` (`SPROUTFS_FLUSH_BOUND`) is twice the checkpoint interval
by default, so 120 s; zero completes every flush at once. It is the maximum
age of the VM's oldest unpublished disk write at which a guest's flush still
completes at once. Past it, the flush waits until a checkpoint covers the
write, which the loop takes out of turn. When a VM leaves the host, its
waiting flushes go unanswered, and its device asks the next host again. A
migration or a stop that is refused leaves the VM, and its waiting flushes,
where they were. With
[durable flush](architecture.md#durable-flush) on, the host's journal answers
a flush instead, and the loop also takes a checkpoint out of turn when the
VM's record does not yet name the journal, when the journal is three quarters
full, and when one VM holds more than half of it.

The window belongs to a VM, because one pause seals every memory region a VM
maps. `Pressure.Oldest` reports the oldest unpublished write across every
memory region of the VM. `Host.LossWindow` reports that age per VM, and
whether its stores are waiting. `/status`, `/metrics` and `sproutfsctl list`
show these values.

A guest can fill the host's dirty budget before its interval comes round.
Through `Config.Pager` the pager asks the host for an immediate checkpoint of
the memory region with the largest dirty set, and the stalled stores complete
when it retires. The host accepts for a VM it runs whose volume no fork point
has sealed, and otherwise declines, so the pager can offer another region. If
no region can be checkpointed, the pager asks the host to stop the region with
the largest dirty set, then the next, down to the region whose store is
waiting. The host stops the first one that belongs to a VM it runs. So a
guest that holds little of the budget is never stopped for one that holds
much. This matters most for RAM, which no loop checkpoint gives back. A store
that the loss window blocks ends with its own VM stopped when no checkpoint of
that VM can be taken. The pager reports that as a window stall: a guest's
writes cannot be made durable. A budget stall means guests dirty pages faster
than their checkpoints drain.

That stop uses the same give-up path as every other loss of a VM:

1. The registration is dropped and this caller takes ownership of the close,
   so a stall, a takeover and the process watcher close the VM only once.
2. The fork points taken on the VM are retired, because their children read
   from the process about to close, and a sealed VM cannot be captured.
3. A last checkpoint captures whatever the VMM can still be paused for.
4. The VM is given up and reported through `MachineClosed`.

If the failed store already killed the VMM, the stop only adds a logged
reason.

The loop also stops when another host has taken the VM's control record.
Every `Config.EpochInterval` (two seconds by default) a host re-reads the
control record of every VM it holds, a few at a time, and closes any VM whose
epoch has moved past its handle. The interval bounds how long a fenced host
keeps running a guest, including one a fork point holds sealed. A record that
cannot be read changes nothing; only a record naming another epoch is
evidence of a takeover. A negative interval disables the timer. The host then
gives up the whole VM:

- it retires the fork points taken on the VM and stops serving their children's
  pages;
- it stops serving the VM's own pages;
- it stops the VMM;
- it releases the VM's volumes;
- it reports the VM through `Config.MachineClosed`, so the supervisor forgets it
  and refuses exec against it.

The store refuses every control-record write a fenced host makes.

A handoff gives another host pages that no checkpoint holds, which the store
cannot refuse. A source taken over between epoch ticks, or one whose store
reads fail while its network works, would build one VM's memory from two
writers' pages. So `Migrate` and the fork point each re-read the control
record before the pause, with one GET, and refuse if the epoch has moved or
the read fails. The VM keeps running here either way.

Only one handover of a VM runs at a time, reserved under the lock that
protects the registration; a second caller is refused and the VM left as the
first caller left it. A VM a fork point holds sealed is refused the same way,
as is a fork that has not published its own checkpoint, since only that handle
could publish its root. Both refusals happen before the guest is stopped.

`AddMachine` also watches the VMM process through `Machine.Wait`, and gives
the VM up the same way when it ends unexpectedly: killed by the host because
its memory session failed, killed by the kernel, or crashed. The watcher stops
with the machine, so a process this host ends on purpose (a handoff, a removal
or a shutdown) is not reported as a death. `vmmachine` writes one error record
with the VM, the pid, the kill's cause and the tail of the console. The memory
session that ended writes the memory region and, for a fault, the page.

Every mapped memory region must use `Host.Resources()`. Registration rejects a
machine whose regions use another budget. The receive path closes a mismatched
runtime before streaming pages or registering it.

## Draining a host

A host configured with a migration address serves the peer server there. It
holds the memory of every VM it has handed to another host, and of every child
it has forked onto another host; a child on the same host maps the pages
instead. The peer server serves any peer its [transport](#transport) accepts.
Each remote host's faults may hold 8 MiB there at once, and its bulk reads,
bulk writes and stripe reads 16 MiB each, over all its connections. A request
past that is answered `BUSY`.

`Host.Drain` migrates every VM the host runs, four at a time by default, so a
drain does not put the host's whole memory on the network at once. It returns
one handoff per VM that moved, and joins the errors of the VMs that did not.
Those keep running and checkpointing here. A receive that fails after the
handoff is tried again while this host holds the pages; only if no
destination takes the VM in that time is the guest reopened at its last
checkpoint ([migration](migration.md#a-failed-receive-is-tried-again)).

The drain asks the orchestrator to move each VM. The orchestrator drives both
halves of the migration. The destination's `Host.Receive` opens the VM, starts
the VMM from the captured state and streams the pages in. The stream completes
only when every page the source holds and no checkpoint has is on the
destination. Then the source's `Host.ReleaseMigrated` stops serving the VM and
closes the process that held its pages.

Four checkpoint intervals after the handoff, a source that nothing has
released gives up those pages itself and closes the stopped VMM process, so an
orchestrator restart cannot pin the source's arena. The destination keeps what
it fetched and reads the rest from the checkpoint its record selects.

`/status` reports an `outstanding` count per VM next to `serving`: the pages
this host still holds that no checkpoint has and the destination has not
fetched. It tells a handover still pulling its pages from one waiting only for
the release. A VM whose volumes cannot be listed reports `-1`. A fork's child
on its parent's host counts every page the fork point holds for it until the
host takes it in, and zero after; its release is refused until then.

`/status` also reports `receiving`: every VM a receive is in flight for on
this host, from admission until the host has taken the VM in or given it up,
whether or not the caller is still there. The orchestrator asks no other host
to take a VM while one reports it here
([migration](migration.md#a-failed-receive-is-tried-again)).

Each receive logs a line when the post-copy finishes, with the pages served,
the requests the source refused for its per-peer budget, the duration, the
latency of the guest's faults to the source (p50, p99 and maximum, and the p99
of the wait for a connection), and the p99 of the stream's requests. The
destination also logs whenever a read of pages only the source has waits more
than a few seconds.

The preStop hook drains and then exits. The drain returns only when
`Status().Serving` is empty, so every page this host held is on a destination
or in object storage. Exiting earlier loses every write since each VM's last
checkpoint, including dirty RAM, dirty PMEM and unpublished local forks. With
durable flush on, the moved VMs' records name this host's journal until each
destination's next checkpoint, and the drain then waits for the journal to
hold no live entry. A disk that still holds live entries when the host exits
is read back by its next holder, as after a host loss
([journal disks](#journal-disks)).

The hook has no deadline, so the drain sets its own:

- Four VMs are handed over at once, each with a 60-second deadline, shorter
  than the four intervals a source serves an unreleased handover and the two
  minutes the orchestrator trusts its record of a handover. The deadline
  bounds the request, not the handover: once the source has stopped a guest,
  the orchestrator carries the handover on, and the wait for `Serving` to
  empty waits for it.
- The whole drain has 30 minutes, including that wait.
- The orchestrator client has a timeout.

The preStop command gives up at 31 minutes. A VM whose handover ran out of
time keeps running and checkpointing here, and is reported as remaining.

`terminationGracePeriodSeconds` is the hook's 31 minutes plus the shutdown:
30 seconds to stop the API, then 30 for the supervisor's close, in which every
VM publishes a final checkpoint. That is the manifest's 1920 seconds. A
shorter grace period turns an orderly exit into a host loss.

After the required capture or handoff, the supervisor closes the VMM
processes and detaches their memory regions, then calls the pager's `Close`
before closing its arena and spill handles. If pager cleanup fails,
unproven allocations stay charged and cleanup must be retried. Closing one
process must not close a pager that serves other VMs.

## Stopping a VM

`Host.Stop` ends a VM this host runs and leaves it to be opened again. It
captures a checkpoint of the guest's disks and waits for it. A suspending stop
also captures memory and VMM state, so the next start resumes the guest. Only
then does the stop close the VMM process, return the pages and release the
handle. The control record and objects stay, so any host can open the VM at
the bytes the stop published. The stop reports that checkpoint.

If the store refuses the publication, the VM stays running, registered and
checkpointed on the interval.

A stop can keep its checkpoint (`StopRequest.Keep`), as a capture can
(`CaptureRequest.Keep`). See [kept checkpoints](#kept-checkpoints).

A stop of a VM that a fork point holds sealed is refused, because a child on
another host reads pages from the process the stop would close. Unlike a
delete, a stop retires nothing to get past this.

Starting a stopped VM again is `Host.Open`, the call a recovery makes. What
differs is what the control plane requires first; see
[the deployment's API](../deploy/README.md#the-orchestrator-api).

## Starting a VM cold

An open restores a VM's VMM state and every page of its RAM from its selected
checkpoint. A cold start instead reboots it: when the guest is wedged, when
the kernel or init on the disk changed, or to stop paying for memory the
guest does not need.

`Host.OpenCold` opens the VM and discards its memory in one publication: every
page of the RAM volume is dropped, and so is the VMM state member.
`volume.VM.DiscardMemory` does both under the VM's publication lock, since
either alone leaves a VM that can be neither resumed nor booted. The guest
then boots its kernel, as a template's children do.

The guest's filesystem sees the boot as a power cut after the last
checkpoint. Its journal recovers what it can; nothing more is guaranteed. The
pages the discarded memory held are reclaimed by the usual set difference,
except where a pin protects them.

A cold start applies only to a VM this host does not run. It is refused
before anything is discarded if this host's Starter cannot boot a kernel, or
if the pager could not map all of the VM's memory regions.

### Resizing at a cold boot

A VM's shape can change only at a cold boot, when nothing in memory describes
it. A create's first checkpoint is one. `OpenCold` and `Host.Reshape` take a
`ColdShape`:

- `MemoryBytes` sets the RAM volume's size, larger or smaller, within what the
  host admits.
- `RootBytes` grows the root volume. The new pages read as zeroes, and the
  guest takes them with `sproutfs-guest-witness grow /`. Shrinking is refused.
- `VCPUs` sets how many processors the guest boots with. Every later
  checkpoint and fork keeps the count, so a VM booted cold after its host is
  lost boots with its own count. The host gives it to the Starter as
  `Launch.VCPUs`. A VM that records none boots with the Starter's default. A
  restore takes its count from the VMM state.

All three are refused for a warm start, at every layer. After a resize the
VM's committed RAM is its own, not its template's; see
[the deployment's API](../deploy/README.md#the-orchestrator-api).

## Creating a VM from a checkpoint

A create can start from another VM's published checkpoint instead of a
template (`CreateRequest.From`). That VM belongs to the same tenant and need
not run anywhere. The path is a create's: a fork of a published checkpoint,
the new VM's own root, and a start. No byte is copied.

How it starts depends on the checkpoint (`Host.CreateRoot`):

- A checkpoint with VMM state resumes the guest where the checkpoint's pause
  left it, as a fork of a running VM does.
- A checkpoint without state boots cold over the disk it inherits.
- A create that names a shape boots cold. A shape that names no size keeps
  the checkpoint's.
- A create that adds an ephemeral disk the checkpoint does not have at that
  size boots cold, since the VMM state does not describe that device.

`CreateResult.Resumed` says which happened, and `sproutfsctl create` prints
"resumed".

The checkpoint must be pinned in the other VM's control record before the
fork. Taking a stopped VM's epoch to pin would fence a host that turns out to
run it, so the pin is written without the epoch
(`volume.Manager.InheritPublished`, `control.Client.Pin`). It keeps the
record's epoch and nonce and is conditional on the record as read. See
[metadata](metadata.md#the-control-record) for why that is safe.

By default the create names the checkpoint the VM's record selects, which must
be published. It may also name a kept checkpoint, or one a pin already holds,
such as an earlier fork point. Any other checkpoint is refused with
`control.ErrNotPublished`, because the VM's writer may be reclaiming it. So
are a pending fork whose root has not landed and an identity that already
exists. The API answers these with 409.

If the other VM is running, the new VM inherits its last published
checkpoint, its last interval checkpoint of the disks. Its writer adopts the
pin at its next selection.

### Kept checkpoints

Reclamation deletes a VM's older checkpoints once it publishes a newer one. A
checkpoint request can keep its checkpoint: a capture (`CaptureRequest.Keep`,
`sproutfsctl capture --keep`), a stop or suspending stop (`StopRequest.Keep`,
`sproutfsctl stop --keep`), or the host's `Capture` and `CaptureDisks`
(`volume.Terms.Keep`). The checkpoint is kept in the write that selects it,
and reclamation then spares it and everything it reads. Only kept checkpoints
cost storage beyond what the selected checkpoint reads. A capture into a new
VM takes no keep, because its root is the new VM's selected checkpoint.

`GET /vms/{id}/kept` (`sproutfsctl kept VM`) lists a VM's kept checkpoints:
each one's sequence, when it was selected, whether it holds VMM state, and
whether a VM was created from it. Any host answers, because the list is the
VM's control record.

A create from a kept checkpoint works whether or not its VM runs, and pins it.
A kept checkpoint no VM was created from can be released
(`POST /vms/{id}/kept/{checkpoint}/release`,
`sproutfsctl release VM@CHECKPOINT`), which deletes what only it held. One a
VM was created from is pinned for good, and its release is refused with
`control.ErrForked` (409). Deleting a VM deletes its kept checkpoints that no
VM was created from. See [metadata](metadata.md#kept-checkpoints).

## Capturing a VM into a new VM

`Host.CaptureInto` captures a VM this host runs into a new VM that never boots
(`CaptureRequest.Into`): a fork whose child publishes its root here and is
closed, without a VMM.

1. The source pauses once, as a fork's parent does: it saves its VMM state,
   seals its memory regions, and runs again. Its own writer pins its published
   checkpoint.
2. The child is created over that fork point, on this host.
3. The child's root is published. It reads the sealed pages through the fork
   point and carries the saved VMM state.
4. The child's handle is closed, and the fork point is retired.

Nothing is served over the network. The source runs throughout, and its seal
ends when the capture returns, on every path. A child whose root did not land
is closed, which removes its record. An identity that already exists is
refused before anything is published.

Any host can then open the new VM, which resumes the guest where the pause
left the source, or create from it
([creating a VM from a checkpoint](#creating-a-vm-from-a-checkpoint)).

## Pulling a VM's memory

A guest's pages load from object storage on first touch, and again after the
pager evicts them clean. A VM can instead fetch its whole memory up front, in
the background: its start marks it to **pull**. The mark is `Pull` on the host
API's create, open and fork, and `--pull` on `sproutfsctl create`, `start` and
`fork`.

While this host runs a marked VM, every page of the checkpoint it started from
is fetched in the background, without becoming resident. Once the pull is
complete, a fault on a page not resident reads the hosts' disks, not the
store, for as long as they hold that page. A pull is a prefetch with no
guarantee.

Where a page goes depends on the cluster share:

- **Inside the share** the pull fills the cluster and copies nothing onto this
  host's disk. It reads each segment through the cluster
  ([reading from the cluster](#reading-from-the-cluster)) to list the pages,
  and asks the first k+m ranks of each window which stripes they hold: one
  presence check to each rank per window, none for this host's own disks. A
  page counts as held when the ranks hold k distinct indices of it. The pull
  reads from the store only the pages the cluster lacks, and hands them to the
  fills ([filling the cluster](#filling-the-cluster)). A pull of a checkpoint
  the cluster holds reads nothing from the store.
- **Outside the share** the pull copies the page whole onto this host's
  [page cache's disk](volumes.md#the-page-caches-disk), keyed by page identity.
  With the cluster cache off, every page goes this way.

There is no whole copy inside the share: it would save about half a
millisecond a page, only for a restart on this host, and cost the cluster a
second copy and write (decided with the owner on 2026-10-02).

- **The guest runs while the pull fetches.** The pull starts when the machine
  is registered. It takes none of the page cache's load slots, joins no
  fault's fetch, and makes no request while a fault's load is in flight. All
  pulls on a host share two requests in flight, presence checks included. Its
  reads are marked as a prefetch
  ([prefetch](vm-memory.md#faults-and-read-ahead)): its peer requests use the
  bulk class under the background budget, and its reads of the cluster send
  no second request, never hedge to the store, and do not update the delay.
- **Pressure stops it.** When the host's memory budget refuses a reservation
  no cache can make room for, or the disk limiter shrinks the cache, every
  pull's reads in flight are cancelled, and each pull stops with
  `checkpoint.ErrPressure` in its status. A pull does not restart by itself.
- **It is bounded.** The disk limiter sets the disk's share
  ([budgets](#budgets)). The disk is a log of 64 MiB regions, and every region
  of the share but one may be filled. While some windows are kept whole, a
  pull is refused before it fetches anything if the checkpoint, as its root
  records it, is larger than those regions hold. With the share between 0 and
  100 the whole checkpoint is counted; at 100 nothing is refused. A VM that
  does not fit, or runs on a host that keeps no disk and fills no cluster, is
  not pulled. A pull that fails part way keeps what it did.
- **It holds nothing.** Pulled pages are ordinary entries of the disk, given
  back with their region when the disk needs room (with a second chance for
  those read since they were written).
- **The disk survives a restart**, so a pulled VM opened again on the same
  node reads its pages from it ([the cache's file](#the-caches-file)).
- **Nothing on the disk is authority.** A lost or damaged copy fails its key,
  checksum or envelope check and is read from the store. A newer checkpoint's
  page has a new identity.
- **Forks share one copy.** A page the disk already holds is not copied again.

The pull covers the checkpoint a VM's volumes sit on: for a create, the root
the create published; for an open, the checkpoint the control record selects;
for a receive, the checkpoint the destination opened. Pages no checkpoint
holds come from the source's pager, so a pull never asks the source for
anything. A fork's child is pulled once its root has published, so the pull
covers the pages the parent held that no checkpoint had.

A checkpoint the VM publishes later adds its pages to the same copy as it
uploads them (`Publication.Keep`): each part once durable, the segments once
the index object is. This covers an interval checkpoint, a stop, and a fork
point. The copying ends when the VM stops running here, and the keeping only
when its handle closes, after its last publication, so a stop's checkpoint is
kept (the GCE run of 2026-10-03 found it was not, before this). A write the
disk refuses keeps nothing more of that publication. Inside the cluster share
every publication fills the cluster, so the keeping leaves those windows to
it.

A stop or a migration away gives nothing up: the pages stay on the disk until
it needs the space. The mark stays with the VM. The orchestrator records it,
and every open it drives carries it: a start, a recovery, and each receive. A
migration's handoff also carries it. For a VM nothing runs, the orchestrator's
table is the only record; for a running VM, its host reports the mark and a
survey writes it down again. A host reports each marked VM's progress
(`hostapi.VM.Pull`) and the disk's use beside the page cache's
(`Resources.CacheDiskUsed`).

## The membership

The hosts' disks are one cache for the cluster
([the plan](../plans/disk-cache-2026-10-02.md)). The membership and the ranks
tell every host every disk, which host serves it, and which disks hold each
window. For a window inside the cluster share, a host keeps the stripes the
membership ranks it for ([the code](#the-code)), fills its peers with theirs
([filling the cluster](#filling-the-cluster)), and reads a page from the
hosts' disks before the store
([reading from the cluster](#reading-from-the-cluster)).

**One object.** The membership is one object at `membership` under the
deployment's prefix (package `membership`). The orchestrator's and each host's
are copies. It holds:

- a **generation**, which counts its changes;
- the deployment's code, k and m;
- every **member**: a host's identity, its peer-server address, and its
  state: joining, active or draining;
- every **disk**: the identity in its cache file's header, the name of its
  volume, its kind (cache or journal), its weight, the member it is assigned
  to or none, its state (attaching, serving, releasing, released, or deleting
  for a journal disk), and the generation that assigned it to that member. A
  [journal disk](#journal-disks) also has the machine it is reserved for and
  whether it is empty;
- the nonce of the process that wrote this generation.

It is protobuf, format version 2, and at most 1 MiB. Version 1 had no kind,
machine or empty flag.

**Changed only by compare-and-set.** A change reads the object, builds the
next generation, and writes it conditional on the object it read (`IfMatch`
on its ETag, or `IfNoneMatch` for the first). The generation goes up by one.
A write another writer beat is tried again from a fresh read. A write whose
reply was lost is read back: one that carries the writer's nonce landed; one
that later changes cover is made again over what is there. Any process may
change the membership.

`membership.Step` says what may follow what, and `Store.Update` writes nothing
it refuses:

- A disk goes from attaching to serving, from either to releasing, and from
  releasing to released, assigned to nobody. It never goes back.
- A disk is assigned only from released, or as it is added, and never to a
  draining member. The generation that assigns it is the one it carries.
- A disk is removed only once released. A member leaves only once it is
  draining and assigned no disk.
- A journal disk keeps its kind. It is deleting only from released and empty,
  never comes back from it, and is removed only once deleting. Its
  reservation changes only while it is released, and an assignment clears its
  empty flag.

**Ranks are over disks.** Every listed cache disk ranks windows, whatever its
state. A journal disk ranks none.
While nobody serves a disk, a reader asks the next rank. So a disk that moves
to another member keeps its windows; only adding or removing a disk, or
changing a weight, moves windows.

**A host's identity is its disk's.** A host with a page cache disk is a member
under the identity in its cache file's header, drawn when the file is made. A
pod replaced on the same node opens the same file and keeps its place; a pod on another node takes that
node's file's identity. A host
reports itself in `/status` under `member`:

- `identity`: its identity, in hex;
- `address`: its peer-server address, which `page_address` also reports;
- `disks`: its disk, with its `identity`, its `volume` (the cache file, such
  as `cache-0`), its `weight` and its `state` in the membership it holds. The
  weight is the size of the disk the cache is given, in steps of 16 GiB,
  rounded to the nearest, and at least one: the filesystem less the
  free-space floor, the reserve and the promises, under the used goal. The
  host reads it once, at start, because every change of a weight moves
  windows;
- `generation`: the generation of the membership it holds.

A host that keeps no cache disk, or gives it no space, is no member and never
reads the membership. A host that serves shards keeps no disk of its own, is a
member under an identity drawn when its process starts, and reports under
`disks` the shards it holds open, with its machine
([shards on network disks](#shards-on-network-disks)).

**The orchestrator moves it one step at a time.** Every five seconds the
orchestrator surveys the host pods and takes one step towards them
(`membership.Next`). It keeps the member each pod last reported. A pod that
answered without one has none. A pod that did not answer keeps what it
reported before, since draining it would move every window it holds. A pod the
Kubernetes API no longer lists is gone. Two pods that report one identity are
a copied disk, held once, as the first pod by name reported it. The steps, in
order:

0. The membership takes the deployment's codes.
1. A member whose pod is gone or terminating drains: it is draining, and its
   disks are releasing.
2. A releasing disk is let go once nobody serves it: a host's own disk once
   its member's pod is gone, or reports the disk releasing in a membership at
   or after the generation that assigned it; a shard once its member's host is
   gone or no longer reports it open, and the cloud has it attached to no
   machine. A pod replaced on its node comes back over the same disk: if the
   orchestrator saw the old pod terminating, the member drained, the new pod's
   report lets the disk go, and the member leaves and joins again with it
   (steps 5, 6 and 8). Before 2026-10-04 only a gone pod let its disk go
   ([the real application's restore](measurements/gce-real-app-restore-2026-10-04.md)).
3. A released disk no pod reports, and that is not a shard, is removed. This
   is the leave, and it moves that disk's windows.
4. A shard not listed is added, released.
5. A draining member with no disk leaves.
6. A pod that is not listed, and not terminating, joins, with its own disk
   attaching, in one generation.
7. A member follows its pod's address.
8. An attaching disk its member reports is served.
9. A disk follows the weight its member reports, and a shard the weight of
   its network disk.
10. A released shard is assigned to the member that serves the fewest disks,
    among the members whose pods run, are not terminating and report a
    machine.
11. While no shard is attaching or releasing, a member that serves two more
    shards than another releases one.

With durable flush on, the steps for [journal disks](#journal-disks) come
between 9 and 10.

The code is the orchestrator's `SPROUTFS_CACHE_CODE`, written as `4+2`, and
4+2 when unset. The earlier codes are its `SPROUTFS_CACHE_EARLIER_CODES`,
newest first, comma-separated, at most three. The orchestrator writes both in
one generation that moves no disk. See [the code](#the-code). `GET /hosts`
shows each pod's member.

**Each host's copy.** A host reads the object as it starts, then every thirty
seconds on its own clock, and at once whenever a peer names a newer
generation. A read that fails or finds an older generation leaves the copy as
it was. Until a read succeeds, a host holds its own disk alone, at generation
zero, under 1+0. `/status` reports under `membership` the generation held, its
code and earlier codes, its members and disks, when the last successful read
finished (`read`), the reads, the failures, and why the last read failed
(`error`). The host logs when its reads start failing and when they recover.

**Every request names its generation.** A stripe read, a keep, a drop, a
presence check and a fill right name the disk they expect and the sender's
generation. The peer server answers:

- A host behind the request reads the membership first.
- A host on another generation then, ahead of the sender or unable to read
  the membership, answers `CACHE_STATUS_STALE` with its own generation.
- A host that the membership at that generation does not have serve the disk
  named answers not me.
- Otherwise it answers, and the cache checks a keep's ranks and a fill
  right's under that same generation.

Every answer names the generation that assigned the disk to the host
answering. A sender refuses an answer under another generation, or naming
another assignment of the disk than its own membership's. A sender told it is
stale reads the membership and asks again, at most three times: a read of the
cluster under the newer ranks, a keep, a drop or a fill right to the member
that serves the disk under the newer generation.

**Ranks.** The package `rank` places windows. A window is the pages of one
volume in one aligned 2 MiB span that one checkpoint published; a segment is a
window of its own. Each disk scores a window by its weight over -ln(u), where
u is a 64-bit hash of the disk's identity and the window, mapped into (0, 1).
Equal scores go to the lower identity. `List.Ranks` is the disks ranked 1 to
k+m. `List.Holders` puts stripe i on rank ((i − 1) mod n) + 1, so a membership
of fewer disks than k+m takes the stripes round its disks. Scores are compared
in integer arithmetic with a fixed-point logarithm, so hosts of different
architectures rank alike. A host alone ranks first for every window, and its
one stripe is the envelope whole.

## Shards on network disks

With the cache on the hosts' own disks, a join hides a stripe of about
(k+m)/(N+1) of the windows, and a host removed takes its stripes with it
([the plan](../plans/disk-cache-2026-10-02.md#shards-on-network-disks)). A
deployment may instead keep its cache on a fixed set of **shards**: each one
network disk, a single-writer Hyperdisk Balanced on GCP or gp3 on AWS, with
the disk log on it. Windows are ranked over the shards, so the ranking changes
only when the shard set is changed. A shard moves to another machine in
seconds.

**The membership is the authority.** Which member serves which shard is in the
membership. The orchestrator carries it out through Compute Engine's
`instances.attachDisk` and `instances.detachDisk` (`platform.NetworkDisks`).
Kubernetes provisions the disks and never attaches them: each shard is a
PersistentVolumeClaim from a StorageClass, labelled
`app.kubernetes.io/component=sproutfs-shard`, and no pod mounts it. Each bound
claim's PersistentVolume names the disk by its CSI volume handle,
`projects/<project>/zones/<zone>/disks/<name>`. A shard's identity is derived
from that handle (`membership.ShardIdentity`), and its weight from the disk's
size. Kubernetes cannot attach a volume to a running pod, so a host taking a
shard through a claim would restart with its VMs.

**Members are hosts.** A host given `SPROUTFS_SHARDS=gce` keeps no cache disk
of its own. It is a member under an identity drawn when its process starts,
so a restarted host never takes its predecessor's assignment. It reports in
`/status`, under `member`, the shards it holds open and `machine`, its node's
name (`SPROUTFS_NODE_NAME`), the Compute Engine instance a shard is attached
to. A host may serve several shards.

**A move.** Each orchestrator pass takes one step of the membership, then asks
the cloud for what the membership calls for, from the cloud's own list of
attachments (`membership.ShardControl`, `membership.Carry`):

1. The membership assigns a released shard to the member serving the fewest,
   attaching, at the next generation.
2. The orchestrator detaches the disk from any machine but the member's, and
   attaches it to the member's machine.
3. The host, once its membership assigns it the shard, reads the object again
   and opens the shard only if it still assigns it there under the same
   generation. It opens `/dev/disk/by-id/google-<name>` with `O_EXCL`. The
   cache takes the shard's lease for that generation and reads its regions
   back from their tables. The host reports the shard open.
4. The membership marks it serving, and the host serves it while its
   membership has it serving there, under that generation.
5. A host the autoscaler removes is drained as soon as its pod is terminating:
   its shards are releasing.
6. The host closes a shard as soon as its membership does not assign it
   there: the open region is closed with its table, and the device is closed.
7. The orchestrator detaches the disk from every machine the cloud lists.
8. Once the host reports it closed, or is gone, and the cloud has it on no
   machine, the membership releases it, back to step 1.

On GCE a Hyperdisk Balanced shard holding 32 GiB moved in 13 to 15 s with
passes of one second, whether its host drained or died: about 10 s of Compute
Engine's calls, 0.3 s of read back, and the rest passes
([measured](measurements/gce-shards-2026-10-04.md#moving-a-shard)). Reads hedge
around it: under 4+2 two shards may move at once with no read of the store.

**Crash points.** Every pass derives what it asks the cloud for from the
membership in the store and the disks as the cloud reports them, never from
what it asked before. So a shard attached and not recorded, or recorded and
not attached, converges whichever process crashed, and two orchestrators
converge too. A host that dies with a shard open leaves it attached: its
member drains, the orchestrator detaches the disk, the membership lets it go,
and another member opens it; the open region left without a table is given
back. A shard that cannot be described is left as it is.

**Fencing.** No shard is served by two members:

- The cloud attaches a single-writer disk to one machine at a time, and a
  detach takes the device from every process of that machine.
- A shard is let go only once its host has closed it and the cloud has it on
  no machine.
- On one machine, `O_EXCL` admits one process to the device.
- The shard's header region ends with a lease: the generation of the
  assignment it was opened under and the member's identity. A member whose
  assignment is older than the lease is refused the shard, and a member reads
  the lease again before every region it opens or closes and every pass, and
  stops writing a shard whose lease another member took.

Every answer for a disk also names the generation that assigned it
([the membership](#the-membership)). `spec/shards` checks that the first three
keep `OneServer` with a controller and hosts acting on stale copies, and that
the lease keeps `NoStaleWrite` under a cloud that attaches a disk to two
machines. The lease also records the regions the file has ever opened, so a
large device holding a few regions is read back in a few reads.

**The disk limiter.** A shard's share is the device less its header region,
and every write is admitted. The host's limiter counts only the host's own
disk, its spill files and its VMM staging.

**Configuration.** The host's `SPROUTFS_SHARDS=gce` and `SPROUTFS_NODE_NAME`
(from the downward API), with no `SPROUTFS_CACHE_DIR`. The orchestrator's
`SPROUTFS_SHARDS=gce`, and `SPROUTFS_SHARD_CLAIMS`, the label selector of the
shards' claims in its namespace. The orchestrator needs to list claims and get
PersistentVolumes, and its service account needs `compute.disks.get` and
`compute.disks.use` on the shards, `compute.instances.get`,
`compute.instances.attachDisk` and `compute.instances.detachDisk` on the
nodes, and `compute.zoneOperations.get`. `deploy/README.md` gives the
manifests and the sizing. The AWS adapter, over EC2's `AttachVolume` and
`DetachVolume`, is not written.

## Journal disks

With [durable flush](architecture.md#durable-flush) on, each machine of the
host pool has one journal disk reserved for it, and the host on that machine
writes its journal there ([the plan](../plans/fsync-journal-2026-10-06.md)). A
host may also hold other journal disks for reading, after their writers were
lost. A journal disk is a disk of the membership of kind journal. It ranks no
window, and it is not a shard: a host may serve no shard, shards move on their
own schedule, and a flush would queue behind the cache's reads and fills.

**The orchestrator makes and deletes them.** There is no fixed pool and no
PersistentVolumeClaim. `platform.NetworkDisks` lists, creates and deletes
disks, through Compute Engine's `disks.list`, `disks.insert` and
`disks.delete`. A journal disk is named `sproutfs-journal-<random>`, labelled
`sproutfs-journal=<namespace>`, and its identity is derived from its name
(`membership.JournalIdentity`). It is `SPROUTFS_JOURNAL_BYTES` on the
orchestrator, 32 GiB by default. The pool is the machines the host pods are
scheduled on, terminating pods included, as the orchestrator's survey lists
them.

**The steps.** Each orchestrator pass takes one step of the membership
(`membership.Next`, between its steps 9 and 10), then acts on the cloud
(`membership.ShardControl`, `membership.JournalControl`):

1. A labelled disk the membership does not list is added, released and empty.
   A disk leaves the membership only once the cloud no longer lists it, so a
   disk the membership does not list is one a pass has just created.
2. A disk marked deleting that the cloud no longer lists is removed.
3. A disk free and empty for an hour is marked deleting. The orchestrator
   keeps that time in memory; a restarted one starts the hour again.
4. A disk held for reading, or releasing, is marked empty when its holder
   reports no live entry, and unmarked when its holder reports one again. A
   disk its writer serves is never marked: a flush may write it at any moment.
5. A disk held for reading that is empty is released.
6. A released disk reserved for a machine no longer in the pool is reserved
   for none.
7. A machine in the pool with no disk is reserved a free and empty one.
8. A member is assigned the released disk reserved for its machine, which it
   writes.
9. A released disk that is not empty and reserved for no machine is assigned
   for reading to the member holding the fewest journal disks.

A disk is **free** when it is released and reserved for no machine, and
**empty** when its last holder closed it with no live entry. Only a free and
empty disk is reserved again or deleted. An assignment clears the empty flag.
After the step, the pass deletes in the cloud the disks marked deleting,
creates one disk when a machine of the pool has none and none is free, and
attaches a reserved disk to its machine at once, while the host pod starts. It
detaches as for a shard. A crash between any two of these is a pass that does
what is left.

**The host.** A host given `SPROUTFS_DURABLE_FLUSH=gce` and its node's name
is a member on that machine; one with no cache disk takes an identity drawn
when its process starts, as a shard host does. It opens each journal disk the membership assigns it once the cloud has attached
it and the object, read again, still assigns it there under the same
generation. It opens the device as a shard's, takes the lease in the header,
and reads the journal back. It reads the lease again on every pass, once a
second, not before each answer: the design trusts the cloud never to attach
one disk to two machines. It reports each disk it holds in `/status` under
`member`, with whether it holds a live entry. Its own disk is reported empty
only while the host runs no VM. Every 30 s it reads the control records of the
VMs each disk holds entries of, a few at a time, and trims what no record
names. It serves `JOURNAL_READ` from every disk it holds.

**Scale-up.** A pod scheduled on a new node puts the node in the pool. The
orchestrator reserves a free and empty disk for it, or creates one, and
attaches it while the pod starts. The host joins, is assigned the disk, opens
it, and reports `journal.served` in `/status`. The orchestrator places no VM
on a host whose journal is not served, because every flush there would fail.

**Scale-down.** A terminating pod's member drains, and its journal disk is
releasing. The host keeps it open while its VMs move away. Each destination's
first checkpoint after its post-copy drops this journal from the VM's record
([migration](migration.md#durable-flush-and-the-handoff)), and trimming frees
the entries. Once the host runs no VM and holds no live entry, it reports the
disk empty, and the membership marks it so. The host closes a released disk
only once the membership marks it empty, and writes empty into its header. If
the close finds live entries, written since, the host opens the disk again and
reports them, and the mark is taken off. Then the disk is detached and let go.
When the node leaves the pool, the disk is free and empty. The drain's last
step waits for this: after its VMs have moved, it trims the journal every
poll rather than every 30 s, and returns once it holds no live entry. A pod
that exits first anyway leaves a disk that is not empty, which is handled as
after a host loss.

**Host loss.** The member drains, the orchestrator detaches the disk, and it is
let go, released and not empty. A pod that comes back on the same node is
assigned it as its own and reads it back. Once the node leaves the pool, the
disk is assigned for reading to the member holding the fewest journal disks,
which reads it back and serves the lost host's entries to the hosts that
recover its VMs. Once no record names the disk, that member reports it empty,
and it is released, closed, detached and free. A disk attaches only in its own
zone, and the controller does not yet choose a member by zone; a deployment in
one zone, as today's is, does not need it.

**Recovery waits for the journals.** The orchestrator does not reopen a VM
while a journal its record names is served by no member, and a recovery tries
again for up to two minutes. A host's open is refused the same way, before it
takes the epoch (`volume.ErrJournalPending`). `sproutfsctl recover VM
--discard-journal` opens the VM without the flushes of a journal that is not
served or whose disk was formatted again. Each journal discarded is logged, and
a checkpoint that names none follows.

**Configuration.** `SPROUTFS_DURABLE_FLUSH=gce` on the hosts and the
orchestrator, and `SPROUTFS_NODE_NAME` on the hosts. The orchestrator's service
account needs `compute.disks.list`, `compute.disks.create`,
`compute.disks.setLabels` and `compute.disks.delete` besides the shards'
permissions. `deploy/README.md` gives the manifests.

## The code

Each envelope in the cluster cache is split into the stripes of a
Reed-Solomon erasure code (package `stripe`, over
`github.com/klauspost/reedsolomon`, MIT). Under k+m, the envelope is split
into k data stripes of equal length, the last padded with zeros, and m parity
stripes are computed from them. Any k distinct indices rebuild it. Stripes 0
to k − 1 are the envelope itself, so a reader that has them does no decoding.
A code with k = 1 is whole copies: 1+1 is two copies.

The code is the orchestrator's `SPROUTFS_CACHE_CODE`, which it writes in
[the membership](#the-membership), and 4+2 when unset. It never follows the
number of hosts, since a change would leave every stripe in the cluster to the
store at once. An operator sets it for the size the cluster usually runs at:

| Hosts | Code | Extra disk | Survives |
| --- | --- | --- | --- |
| 1 | 1+0 | none | nothing: a single host has no peer |
| 2 | 1+1 | 100 % | one host lost or slow |
| 3 | 2+1 | 50 % | one host lost or slow |
| 4 or 5 | 2+2 | 100 % | two hosts lost or slow |
| 6 or more | 4+2 | 50 % | two hosts lost or slow |

4+2 on fewer than six hosts takes the stripes round them. On five or three
hosts any one can still be lost. On two, losing either loses every window.

**Changing the code.** An operator who changes the code writes the old code
first in `SPROUTFS_CACHE_EARLIER_CODES`, for example
`SPROUTFS_CACHE_CODE=2+1` and `SPROUTFS_CACHE_EARLIER_CODES=4+2`. Every stripe
names its code. A read tries the membership's code first, then each earlier
code, newest first, asking the window's ranks under that code (a window's rank
order does not depend on the code). No envelope is rebuilt from stripes of two
codes. A window rebuilt under an earlier code is filled under the new code
([filling the cluster](#filling-the-cluster)), and its old stripes age out.
Fills and repairs use only the membership's code. So a change of the code
costs no read of the store for a window the cluster held, and each earlier
code costs one more round of requests when the codes before it found nothing.
Once `earlier_hits` in `cache_read` stops growing, the old code can leave
`SPROUTFS_CACHE_EARLIER_CODES`. A stripe of a code the membership does not
name is a miss.

**The share it is on for.** `SPROUTFS_CACHE_CLUSTER_PERCENT`, 0 to 100, and 0
when unset (`CacheConfig.ClusterPercent`), is the share of windows the cluster
cache is on for. A window is inside it by a hash of the window, so every host
agrees, and raising the share only adds windows. A window outside the share is
kept whole on the host that reads it, under 1+0. The manifest sets 0. Hosts
read stripes from each other, so a deployment can raise it a share at a time
while watching `cache_read`. A host given a
[hot tier](#reading-through-a-hot-tier) and a share above 0 refuses to start.

**What a host keeps.** For each window inside the share, a host keeps every
index `List.Holders` puts on its own disk, under the membership's code, while
the membership has it serve that disk. A membership of fewer disks than k+m
puts several indices of a window on one host. A host alone holds each
envelope whole, under 1+0. Each stripe is stored as an item that names its
index, its code and its envelope's length
([the page cache's disk](volumes.md#the-page-caches-disk)).

**What a read of the disk takes.** Outside the share a read asks the disk for
the envelope whole. Inside it, the read takes every index of the membership's
code the disk holds, then the window's other ranks
([reading from the cluster](#reading-from-the-cluster)), then the same under
each earlier code. It checks each item's key, index, code and checksum, and
rebuilds the envelope from the first k of one code that pass, then checks the
envelope's XXH3-128. If that fails with more than k stripes in hand, it
rebuilds from other sets of k, at most 64, and forgets the stripes that do not
match the envelope that passed. With exactly k, all are forgotten.

Under 1+1 every host holds each window whole and reads it from its own disk
with no request; under wider codes a host asks its peers for the rest. On a
2 MiB envelope, splitting the stripes of 4+2 takes 0.17 ms and rebuilding from
four stripes 0.07 ms, or 0.19 ms with two of them parity; on a 4 KiB envelope,
1.0 µs, 0.6 µs and 1.4 µs (Apple M5 Pro, `go test ./stripe -bench .`).

## Filling the cluster

Inside the cluster share, a host puts each window it has in hand on the disks
the membership ranks for it. That is a **fill**. Three things fill:

- **A read of the store.** The run the store served is split under the
  membership's code, once the read's callers have their pages. A window a read
  of the cluster rebuilt under an earlier code is filled the same way
  ([changing the code](#the-code)).
- **A publication.** Each part is filled once its PUT has succeeded, and the
  segments once the index object's has, so no cache holds a part the store
  refused. Parts are handed over in part order, not upload completion order.
  Every publication fills: an interval checkpoint, a capture, a stop, a fork
  point and a template import.
- **A pull.** A pull reads from the store only the windows the cluster lacks,
  and each is a fill ([pulling a VM's memory](#pulling-a-vms-memory)). It
  needs no fill right, and one that finds the queue full is dropped.

A fill splits each envelope with `stripe.Split` and puts stripe i on the disk
`List.Holders` names. This host's own stripes go to its own disk. The member
that serves each other holder disk gets one **keep**: a peer-server request
with that disk's stripes of the window, each with its own checksum, under the
generation the fill was placed by. A disk no member serves gets nothing, and
its stripes are dropped as `stale`.

**A fault and a pull never wait on a fill.** A host holds its fills and its
peers' keeps in one queue, `CacheConfig.FillQueueBytes` (64 MiB by default).
One worker decides the fills in the order they were handed over, and each
fill's holders in rank order. It asks for a read's fill right, takes each
keep's bytes from a rate per host, `CacheConfig.FillBytesPerSecond`
(192 MiB/s by default, with a burst of one second of it), and its room in the
host's background budget, then hands the keep to the lane of the member that
serves the holder's disk. This host's own stripes have a lane of their own.
Every write to this host's own disk is done by another worker, one at a time.
A fill of a read or a pull that finds the queue full, the rate spent or the
budget without room is dropped, and its window is read from the store next
time.

**A publication goes at the pace of its fills.** A window a suspend or stop
did not fill is read from the store by the restore on another host, and those
windows are the restore's tail
([the real application's restore](measurements/gce-real-app-restore-2026-10-03.md)).
So a publication's fills wait rather than drop:

- A publication hands a window over only while the queue holds less than
  three quarters of its bound. Windows that wait get room in the order they
  began to wait.
- A window taken into the queue holds a copy of its envelopes, not its part,
  so the queue's bytes are what the fills hold.
- A part keeps its upload slot until its windows are handed over, so a
  publication holds no more parts than the store has slots, besides the queue.
- A publication's keep that finds the rate spent waits for it, leaving a
  quarter of a second of the rate to reads. Its keeps have three quarters of
  the background budget. One that finds the budget held by the publications'
  own keeps waits until one is answered. One that finds it full of other work,
  or whose holder answers BUSY, is tried again after 10 ms, doubling up to half
  a second. A keep larger than its share of the whole budget is dropped at
  once.
- No wait lasts longer than `CacheConfig.FillWaitBound` (10 s by default). A
  publication that waited it out once waits no more, and its later fills that
  find no room are dropped. A holder that is gone is marked down within a few
  seconds, and keeps to it are then dropped at once.

On GCE an 8 GiB guest published from an Ice Lake host under 4+2 used to drop
two thirds of its stripes at the default queue. Paced and sent one keep at a
time, it dropped none, and took 69 s instead of 33 s
([the measurement](measurements/gce-fill-backpressure-2026-10-04.md)). With its
keeps side by side and the rate unbound, it drops none and commits in 38 s,
and in 28 s with a 1 GiB queue, which costs 3.1 GiB of memory rather than 5.6
([the measurement](measurements/gce-fill-side-by-side-2026-10-04.md)).

**The defaults.** The rate protects the faults of the guests on a publishing
host. On GCE a chain of faults on the publisher took 12 ms at p99 with nothing
publishing. Beside an 8 GiB publication paced at 128 MiB/s it took 27 ms, and
unpaced 45 ms. At 192 MiB/s it took 31 ms, and the publication committed in
54 s rather than 84 s. At 256 MiB/s the faults paid as much as unpaced. The
holders' faults paid little at any rate. A larger queue only waits less once
the rate binds: at 256 MiB/s a 1 GiB queue committed 3 s sooner and cost the
publisher 2.5 GiB more
([the measurement](measurements/gce-fill-defaults-2026-10-06.md)).

**A read's fill never waits behind a publication's.** The last quarter of the
queue is left to the fills of reads, repairs and peers' keeps. The worker
takes them before any of a publication's fills still to do, and between one
holder of a publication's fill and the next. Each lane carries a read's keep
before the publication's keeps behind the one on the wire.

**Keeps side by side.** A window's keeps go to its holders' lanes, which run
beside each other, so a publication goes at the pace of one holder. Each lane
carries one keep on the wire and holds one more behind it. The host holds
`CacheConfig.FillKeepsInFlight` (16 by default) on every lane together. When
the next holder's lane is full, the worker waits for room there and does a
read's fills meanwhile; it does not go on to the next fill, whose keep could
then overtake. A read's keeps never wait for a lane.

One worker decides the keeps in fill order so that drops, budget refusals and
partitions follow from that order and a simulated run reproduces. A lane has
one keep on the wire because a member answers one connection's requests
concurrently, which would reorder two keeps. A keep that a stale answer sends
to another member goes from its first member's lane; that happens only while
a disk moves. The writes have their own worker because a keep waits for its
holder's writes, and a holder's writes must never wait for its own keeps.

**Fill rights.** Many hosts missing one window at once would each fill it. So
a fill from a read needs the window's fill right. Behind its read, the reader
asks the window's rank 1 with a stripe read that wants no bytes. Rank 1 gives
the right to the first reader that asks, once per window per
`CacheConfig.FillRightInterval` (ten seconds by default), and only while it
holds nothing of the pages asked for. A rank 1 that is down or cannot be asked
gives no right. A publication and a pull need none. A stripe read that wants
bytes is never given a right.

**What a cache takes.** A cache takes a keep only under the generation the
keep names, for a window inside the share that this generation ranks its disk
for, under its code, while it has this host serve the disk. Its own fills are
held to the same rule under the membership it holds when it writes them. It
drops every stripe it already holds, or is writing, as a duplicate. Each write
asks the disk's write budget at the fill's priority, which a keep states
([budgets](#budgets)).

**What a host reports.** `/status` reports under `cache_fill` the windows
filled from reads and from publications, the reads not filled for want of the
right, the rights given out, the stripes sent that holders kept and their
bytes, the stripes kept on its disk, the stripes dropped by reason, the
duplicates, the stripes of keeps refused, the queue and its high-water mark,
and its publications' waits: how many, how long in all, and how many waited
out the bound. The reasons are `queue`, `rate`, `budget` (this host's
background budget), `busy` (the holder's budget for this host), `down`,
`stale` (a host that does not serve the disk at its address, a disk no member
serves, or a holder on another generation that a newer one did not settle),
`peer` (the holder dropped it), `disk` (this host's disk refused it) and
`failed`. `/metrics` carries the same as `sproutfs_cache_fills_total`,
`sproutfs_cache_fill_*` and `sproutfs_cache_keep_stripes_refused_total`.

## Reading from the cluster

Inside the cluster share, a page is read in this order:

1. this host's memory tier, then the pager's arena;
2. the cluster: this host's own stripes of the window, then its peers', under
   the membership's code and then under each earlier code
   ([changing the code](#the-code));
3. the object store.

**Its own stripes first.** A read takes every stripe of the window this host's
own disk holds, of any index, while the membership has it serve that disk. If
they make k distinct indices of every page it wants, the read is done.

**Then k+1 of the ranks.** Otherwise the read asks k+1 of the window's first
k+m ranks, counting this host when it is one of them and holds a stripe, and
skipping hosts marked down. Which ones it asks first is a hash of this host's
cache and the window (`rank.Pick`), so readers of one window spread over its
holders. One request asks a holder for every stripe of the run's pages in
that window, of any index, and the read rebuilds from any k distinct indices.

**A miss is replaced at once.** A holder that answers with nothing, answers
`BUSY`, or fails is replaced at once by the next rank not yet asked.

**The rest after a delay.** If k stripes of every page have not arrived after
a delay, the read asks every rank not yet asked. The delay is kept per size
class: up to 4 KiB, then each class four times the last, up to 16 MiB. A
class's delay is the 95th percentile of this host's recent times to k stripes
in that class, over its last 256 reads and updated every 32, and never less
than `CacheConfig.ClusterHedgeFloor` (0.5 ms by default). A class with fewer
than 32 reads takes the delay of the nearest class that has them, the larger
first; with none, the floor. These second requests come from a budget, as
FoundationDB's do: a read that had its stripes within the delay adds a
twentieth of a request, a second request takes one, and the budget holds five
at most and starts full.

**Rebuilt and checked.** Each stripe's key, index, code and checksum are
checked as it arrives. A page is rebuilt from any k distinct indices
(`stripe.Join`), and its envelope must decode and its XXH3-128 hold. A stripe
that fails its checks, or that a rebuild finds is not the envelope's, is not
used, and its holder is sent a drop (`Peer.Drop`) behind the fills. With
exactly k stripes that rebuild nothing, the read asks one more rank at once.

**Copies.** A stripe is read in place in its reply's buffer, which the read
holds until it finishes. The data stripes are copied once into the envelope
buffer, which is not zeroed first when all of them are in hand; a missing data
stripe is rebuilt there from parity. A raw page is a view of its envelope,
which the memory tier keeps. The page is copied once more into the caller's
buffer. Under k = 1 a peer's stripe is copied before the read finishes. On an
Apple M5 Pro, a reader's own work for a 2 MiB page from its data stripes went
from 1.63 to 1.05 ms with this, and with two parity stripes from 1.81 to
1.17 ms, allocating one buffer of the page's size rather than five
(`BenchmarkAReaderRebuildsA2MiBPage`).

**The store past the bound.** A read that has not rebuilt its pages within a
bound also reads the store, and takes whichever answers first. The bound is
four delays of the read's size class, and never less than
`CacheConfig.ClusterBound` (10 ms by default). These reads come from a token
bucket: every read that asked the cluster adds a twentieth of one, the bucket
holds five at most and starts full. Past the bucket, the read waits for its
stripes. A page with fewer than k stripes among the holders asked is read from
the store, and that read fills the cluster.

**Nothing waits on a request.** A stripe request goes on until it is answered
or times out (`CacheConfig.ClusterStripeTimeout`, one second), whether or not
its read still needs it. How it ended feeds its holder's down mark.

**Hosts marked down.** A reader marks a host down after three of its stripe
requests to it in a row time out, or after one refused connection (the table
of peers' hard-failure mark). While a host is marked down, this reader asks it
for no stripes and sends it no fills (`down` in `cache_fill`). A probe goes
after ten seconds, then at intervals half as long again, up to sixty seconds,
spread by a hash of the host and the attempt. Only a successful probe clears
the mark. A miss, `BUSY`, an answer for another disk, a stale answer and a
stripe that fails its checks are not failures of the host. A reader marks
down at most a fifth of its membership's disks, and always at least one host.

**Repair.** A read that heard from every rank of the window knows what each
holds. For each index of a page it rebuilt that no rank holds, it sends the
stripe to a rank that holds fewer of the window's stripes than the code puts
on it, in rank order, never an index another rank holds. A read that has its
pages before every rank answers hears the rest behind its caller. A repair is
a keep at the repair priority, in the same queue and rate, within half the
background budget, and is dropped when it finds no room. A window that is not
read ages out.

**A sampled check of the store.** One hit of the disk tier in
`CacheConfig.HeadCheckEvery` (10,000) has the part it was served from, or the
index object for a segment, checked with a HEAD behind the fills. This catches
a reclamation that deleted what a root still reads. A part found missing is
logged as an error with the page's identity, and counted.

**Stripe reads have their own class.** A stripe read uses two connections per
peer of its own, within a budget of 16 MiB per peer at the holder. Replies
leave a connection in request order, so a stripe behind 2 MiB pages would wait
for them ([the peer server's run](measurements/gce-peer-server-2026-10-03.md)
measured 27 ms at p99 that way). The table bounds the stripe bytes in flight
at all peers to 64 MiB.

**Serving bandwidth.** A host serves its peers within
`SPROUTFS_CACHE_SERVE_BYTES_PER_SECOND` (500 MiB/s, with a burst of a tenth of
a second). A read past it is answered `BUSY`, and its reader asks another
holder. The tail of cluster reads follows the bytes each host serves, well
before its NIC's rate
([measurement](measurements/gce-stripes-tail-2026-10-03.md)), so the default
is about 40 % of a 10 Gb/s NIC until the deployment's machine type is
measured.

**A host's view.** `/status` reports under `cache_read` the envelopes read
from the cluster and missed, those this host's own stripes rebuilt alone,
those rebuilt under an earlier code (`earlier_hits`), the requests, the
holders replaced, the second requests and those the budget refused, the reads
of the store past the bound by outcome, the wrong stripes and the drops sent,
the repairs, the timeouts, the marks made, refused for the fifth and cleared,
the hosts down now, the HEAD checks and what they found missing, each size
class's delay and bound and the reads it is drawn from (`classes`, by
`up_to_bytes`), and what the peer server served of the cache: reads, stripes,
bytes, and reads answered `BUSY` for the bandwidth. `/metrics` carries the
same as `sproutfs_cache_reads_total`, `sproutfs_cache_read_*` (classes
labelled `up_to_bytes`) and `sproutfs_cache_serve_*`. `cache_memory`
(`sproutfs_cache_memory_*`) counts the pages and page tables the memory tier
holds, the reads it served, sent on to the disk, the cluster or the store, or
joined to a fetch in flight, and the page tables' bytes and the lookups a held
table answered, that loaded a segment, and that a publication kept.

**Measured.** On six `n2-standard-4` hosts under 4+2, an 8 GiB guest's pages
read back on another host in 16.4 s from the cluster and 28.1 s from GCS. A
page took 58 ms at the median and 136 ms at p99 from the cluster, 106 and
218 ms from the store. With one host lost during the read, no page was read
from the store and the time did not change
([measurement](measurements/gce-cluster-reads-2026-10-03.md)). A restored
Valkey guest whose requests each wait on the page the last one named walked
its 5 GiB heap in 22 s from the cluster and 56 s from the store: a fault's
read took 30 ms against 92 ms, about half of it SHA-256 and zstd on hosts
without SHA instructions
([measurement](measurements/gce-real-app-restore-2026-10-03.md)). In the
bench, a chain of 2 MiB pages took 10.3 ms a hop from the cluster and 41.5 ms
from GCS, and a chain of 4 KiB pages 0.65 ms and 24.6 ms; the guest in order
was 1.6 times as fast. SHA instructions took the 2 MiB hop to 6.5 ms
([measurement](measurements/gce-dependent-reads-2026-10-03.md)). These
predate envelope format 2, which checks a page with XXH3-128 rather than
SHA-256 ([the envelope](volumes.md)). On an Apple M5 Pro that change took a
reader's own work for a 2 MiB page from its data stripes from about 1.1 ms to
0.5 ms, measured under load.

## Reading through a hot tier

A hot tier is a second bucket that holds copies of checkpoint objects under
their own names. It is an alternative to the cluster cache. A host given a hot
tier (`SPROUTFS_HOT_TIER`) and a cluster share above 0
(`SPROUTFS_CACHE_CLUSTER_PERCENT`) refuses to start and names both.
`host.StartHost` refuses `Config.HotTier` beside `Config.Cache.ClusterPercent`,
and `checkpoint.NewStore` refuses a hot tier beside a cache that fills the
cluster.

**Off by default.** With `SPROUTFS_HOT_TIER` unset a host has none. The
manifests under `deploy/` do not set it; only the hot tier benches turn one on,
through `sproutfs-restorebench -hot-bucket`. This was decided on 2026-10-06:
on AWS the cluster cache on gp3 volumes was faster than an S3 Express hot
tier, at the same cost or less. The code stays, with its tests.

**Configured by a URL.** `SPROUTFS_HOT_TIER` is `gs://bucket/prefix` or
`s3://bucket/prefix`. An `endpoint` query parameter points the client at an
emulator or an S3-compatible server. The hot tier uses the same object store
adapters as the deployment's bucket: a read is a ranged GET, a fill is a
create-if-absent PUT, and a check is a HEAD. The intended hot tier is a bucket
close to the hosts, such as a zonal bucket. Google Cloud's zonal Rapid Bucket
takes writes only through its own gRPC API, so it cannot be one
([measurement](measurements/gce-hot-tier-2026-10-03.md)). On AWS it is an S3
Express One Zone directory bucket, named by its zone's ID:
`s3://bucket--use1-az4--x-s3/prefix`. The AWS SDK makes a session at the
bucket's zone and signs each request with it. A directory bucket lists its
keys in no order, so the adapter refuses to list one; a hot tier never lists.
On six hosts in one zone a warm hot tier on S3 Express read a chain of 2 MiB
pages at 17 ms a hop and 4 KiB pages at 4.2 ms, against 102 and 26 ms from
regional S3 Standard and 6.2 and 1.2 ms from the cluster cache on gp3 volumes
([measurement](measurements/aws-hot-tier-2026-10-04.md)).

**The read order.** A read of a checkpoint object goes:

1. this host's memory tier, then the pager's arena;
2. the page cache's disk, where it holds what a pull copied;
3. the hot tier, where one is configured;
4. the regional bucket.

A read of the hot tier runs under a bound, 500 ms by default
(`HotTierConfig.Bound`). A miss goes to the regional bucket, and the object is
filled behind it. Any other failure also goes to the regional bucket, with no
fill: a failed request, a read past the bound, or bytes that do not make what
the read wanted. So a hot tier that is down, slow, lost or emptied never fails
a read. Three failures in a row mark it down; reads skip it for ten seconds,
then try it again.

**Filling.** Two things fill the hot tier:

- **A miss.** Once the regional bucket has answered, the object is handed to
  the fills. A fill GETs the whole object from the regional bucket unless the
  read already holds it all, so a miss of a 4 KiB page copies its whole part,
  up to 64 MiB.
- **A publication.** Each part once its regional PUT has succeeded, in part
  order, and the index object once its PUT has.
  `HotTierConfig.SkipPublications` turns this off.

A fill is a create-if-absent PUT of the same bytes under the same name. Page
objects are immutable and named by the checkpoint that wrote them, and a VM's
starting epoch is drawn, so a name never stands for two contents and no copy
is stale.

**Nothing waits on a fill.** One worker does the fills one at a time, in
order. They are bounded in bytes, `HotTierConfig.QueueBytes` (256 MiB by
default), and in rate, `HotTierConfig.BytesPerSecond` (128 MiB/s, with a burst
of one second of it). A fill that finds either spent is dropped. A miss of an
object a fill is already held for is not filled twice.

**The regional bucket stays the only durable copy.** Nothing reads the hot
tier to decide what is published, and reclamation deletes from the regional
bucket alone. Expiry of the hot tier is not written. One hit in
`HotTierConfig.HeadCheckEvery` (10,000 by default) has its regional object
checked with a HEAD behind the fills, and one found gone is logged as an
error. The hot tier's bucket should hold this deployment alone, under the
same prefix rules as the regional bucket.

**A host's view.** `/status` reports under `hot_tier` the reads the hot tier
answered and missed, the reads it failed by why (`error`, `slow`,
`corrupt`), the reads that skipped it while marked down, the times it was
marked and whether it is now, the fills handed over from reads and from
publications, the misses of an object already held for a fill, the fills it
took and their bytes, the fills that found the object there, the fills
dropped by why (`queue`, `rate`, `read`, `write`, `closed`), the sampled
checks and what they found missing, and the queue. `/metrics` carries the same
as `sproutfs_hot_tier_*`.

**Measured.** On two `n2-standard-4` hosts, with a second regional bucket as
the hot tier, a warm hot tier answered every dependent read but was no faster:
55 ms at the median for a 2 MiB page against 46 ms from the regional bucket,
and 29 against 27 ms for a 4 KiB page. The cluster cache under 1+1, read from
the host's own SSD, took 9.4 ms and 0.2 ms. A cold hot tier filled in about
one walk of 500 reads
([measurement](measurements/gce-hot-tier-2026-10-03.md)). A zonal bucket
reachable through the standard API was not available to measure.

## Nested VMs

A nested VM is experimental: a VM whose guest may run VMs of its own. A create
asks for one with `nested` (`sproutfsctl create --nested`), and only an Intel
x86_64 host runs one. Every other guest is offered neither VMX nor SVM.

A nested VM is captured, suspended, forked, migrated and paged like any other.
The Firecracker fork never offers a nested guest the three VMX controls that
make the host's KVM map the guest's pages behind the host page tables (TPR
shadow, APIC-access virtualisation and posted interrupts), so the guest's own
VMs take more exits for their interrupts. The fork saves and restores the
guest's nested state in every snapshot. AMD's SVM has pins of its own that the
fork does not remove, so an AMD host runs no nested VM. `vmmachine/nested.go`
says why, and
[writers that bypass the page tables](vm-memory.md#writers-that-bypass-the-page-tables)
says what the pager relies on.

A VM's checkpoints record that it is nested. A create from a nested VM's
checkpoint makes a nested VM only if it asks for one.

## Budgets

The host takes one `Resources` owner, which accounts only RAM: the pager's
pages. `Status().Resources` reports its reservations and configured total.

One disk limiter, `resource.DiskLimiter`, decides how much of the node's disk
the host may use. It reads the filesystem under the scratch directory
(`platform.DiskSpace`) every ten seconds on the host's clock, and whenever it
is asked.

The users that cannot give space back are counted at their promises:

- each pager's spill file, at the dirty pages it may hold, its share of
  `SPROUTFS_SPILL_BYTES`;
- the ephemeral pager's spill file, `SPROUTFS_EPHEMERAL_BYTES`;
- each running VMM's staging, at the largest state a capture may write, 64 MiB;
- an image staged for an import, at what it holds.

A store must never fail for want of disk, so each spill file's whole extent is
allocated (`fallocate`) when its pager starts, and a released spill slot keeps
its blocks. Another writer then finds the filesystem full, not the guest. A
disk that cannot allocate a spill file's extent refuses the host at start,
with `ErrInvalidConfig`.

The limiter keeps every goal it is given, and needs at least one:

- `SPROUTFS_DISK_FREE_BYTES`, the least the filesystem keeps free;
- `SPROUTFS_DISK_FREE_PERCENT`, the least share of it kept free;
- `SPROUTFS_DISK_USED_BYTES`, the most the host holds.

A host given none keeps a tenth of the filesystem free. The floor is the
larger of the two free-space goals. The room is what the filesystem has free
plus what the host holds. The cache's share is the room less the floor, the
promises and the reserve, less a band. The used goal caps the promises and the
cache together. The smaller share binds, and `/status` and `/metrics` name the
goal that binds.

The reserve, `SPROUTFS_DISK_RESERVE_BYTES` (1 GiB by default), is what the
cache leaves free above the floor for promises not yet made. A promise may
take it, and the cache then gives back what restores it. Where two hosts share
a filesystem, each sees the other's cache only as space not free, so without
the reserve a full cache would leave the other host no room to start or
receive a VM.

The band keeps the cache back from the floor and the reserve by a fifth of the
headroom it has left, at most `SPROUTFS_DISK_BAND_BYTES` (4 GiB by default),
so a filling disk shrinks the cache a few regions at a time. At or below the
floor there is no band.

The limiter acts on readings smoothed over a minute, as FoundationDB's
Ratekeeper smooths free space; one odd reading moves them about a seventh of
the way. A reading that fails, or says more is available than the filesystem
holds, changes nothing and is reported. `/status` shows the last raw reading
beside the smoothed one.

A cache over its share gives regions back until it holds one region, 64 MiB,
less than its share. A cache within that region of its share is left alone.

A host whose promises the filesystem could not keep under its goals with
nothing else on it refuses to start. One that does not fit only because other
writers hold space starts and reports itself unready with the reason, since
the other writer may be another host's cache that will give space back. While
unready, as when the disk fills from outside later, the host refuses to start
a VMM or stage an image that would promise more. It never takes space back
from a spill file.

The limiter also keeps the disk cache's write budget:
`SPROUTFS_CACHE_WRITE_BYTES_PER_DAY` on average, at most
`SPROUTFS_CACHE_WRITE_BURST_BYTES` ahead of it (an hour's average by default).
It is measured by the device's own count of bytes written
(`platform.DeviceWrites`): on Linux, the `stat` file of the block device under
the scratch directory, and a host that cannot read one refuses a budget. The
device counts every writer. The cache's own writes are charged when admitted,
and not again when the device counts them. A count that goes backwards is a
replaced device and is counted from there. A count that jumps charges at most
a burst of debt.

The cache asks before each write, with a priority: 0 for a repair, 1 for a
second chance, 2 for a fill from a store read, 3 for a fill from a
publication. A pull's copy is written at a publication's priority, and a keep
at the priority its sender states. Priority p is admitted only while (3 - p)
quarters of a burst are left after the write, so repairs are refused first and
fills from publications last. A refused write costs a store read later.

The page cache's disk holds what the limiter leaves, `CacheShare()`, and
nothing else caps it. Each write asks `Admit`, at the priority of its kind.
When the share falls below what it holds, the limiter calls `Shrink`, and the
disk gives regions back, oldest first and with no second chance, until it
holds its share less one region. A `Shrink` first stops every pull on the
host, cancelling its reads. A host that restarts finds the cache's file
holding what it kept, so until the cache registers, the limiter counts the
file as the cache's, not another writer's; the cache read back is then fitted
to the share before it serves anything.

- **VMM staging files** live in each process's directory under the scratch,
  and are removed with the process. Configuration and restore files are
  removed after startup. A capture's state file is read back and deleted; the
  file-size limit the VMM runs under refuses one over 64 MiB. Failed cleanup is
  reported and can be retried. The scratch owner's `Close` refuses live VMMs
  and closes stopped ones, so close the processes first.
- **VMM console output** is drained into a 1 MiB in-memory ring buffer per VMM
  process, with no disk. Output beyond that replaces the oldest bytes. A read
  names an offset in the whole output and returns at most 256 KiB. A read of
  an offset before the retained output is answered from the oldest retained
  byte and reports that offset, so a reader learns that output was dropped.
- **An exec's answer** comes from a guest, which runs untrusted code. The host
  waits for the command's own timeout plus 30 seconds; a request with no
  timeout gets the agent's 30 seconds, and none gets more than ten minutes. It
  reads at most `guest.MaxResultBytes` of the answer, two full output streams
  as JSON, and 64 KiB of its headers. An answer that is late, too long or not
  an exec result fails the exec, quoting at most 512 bytes of it.
- **The page cache** keeps decoded pages within its own cap,
  `SPROUTFS_CACHE_BYTES`, separate from the guests' allotment, and evicts
  unused entries before a retention fails. A miss that does not fit is served
  without being kept.
- **Checkpoint uploads** are bounded by the checkpoint store, shared by every
  publication on the host: half the machine's cores, between 8 and 64. The
  part builders have a separate bound: a quarter of the cores, between 2 and
  8. Each publication encodes its pages on the host's encoders, and holds one
  more batch of pages than there are encoders. A part keeps its upload slot
  until the cluster has taken its windows
  ([filling the cluster](#filling-the-cluster)). These and the fill queue bound
  the memory publication uses.
- **Open VMs**: one manager owns at most 4,096 live handles by default, with the
  per-write bound described in [volumes](volumes.md#writes). There is no bound
  on unpublished bytes; they are what losing the host would cost.
  `Stats().DirtyBytes` reports them, summed over every VM.
- **The pager** budgets resident, logical and dirty pages across the host; see
  [managed VM memory](vm-memory.md#bounded-host-pager). A VM is admitted
  against the logical budget, which the pager checks one attachment at a
  time. So a create, an open and a receive each check what the cap has left
  before starting a VMM, and refuse a VM that cannot fit, rather than start it
  and have it killed. The receive that takes in a fork's child admits it. The
  orchestrator admits the whole fan-out against that host before the parent
  is paused.

The host's status reports:

- cache usage and its cap, in memory and on disk;
- what the disk limiter chose: the goals and the one that binds, the raw and
  smoothed readings, the floor, the reserve and the band, each promise and what
  it holds, the cache's share, the write budget, and why the host is unready;
- the volume manager's totals;
- the pager's counters, including the free space in the logical cap;
- the object traffic;
- what the peer server has served;
- under `peers`, each host this host has asked anything of: the version it
  speaks, its connections of each class, and whether it is down, and why, or of
  a release this host cannot talk to;
- under `member` and `membership`, as in [the membership](#the-membership);
- `cache_fill` ([filling the cluster](#filling-the-cluster)) and `cache_read`
  ([reading from the cluster](#reading-from-the-cluster));
- the page cache's disk under `cache_disk`: the file it took, its identity,
  the regions and entries it holds, the reads it served without the object
  store (`hits`), the copies it lost, the regions it gave back, and what the
  host did with the regions it found in the file at start (`opened`: read back
  from their tables, scanned, or given back). `/metrics` carries these as
  `sproutfs_cache_disk_*`.

### The cache's file

The page cache's disk is a file in `SPROUTFS_CACHE_DIR`, or in the scratch
directory where that is unset. The host takes the first file there, `cache-0`,
`cache-1` and so on up to `cache-63`, whose lock no other process holds, and
holds the lock (`flock`) while it runs. So two hosts that share the directory
never share a file, and a host that starts after another exited takes the
lowest free file with what it kept. The cache's identity is in the file's
header, so it moves with the file.

A starting host reads the file back if its header names this deployment (the
object store's kind, its bucket and its prefix), format and region size: each
closed region from its table, a region with a torn table by scanning its
items, and the region open when the host stopped is given back. A file with no
header, a damaged one, or one of another deployment, format or region size is
emptied and made again under a new identity.

The directory must be on the scratch directory's filesystem, because one
limiter measures one filesystem. A host whose cache directory is on another
refuses to start and names both.

The host manifest makes `SPROUTFS_CACHE_DIR` a `hostPath` directory on the
node's disk, one per namespace, and leaves the spill files and the VMM scratch
in the pod's `emptyDir`, which the kubelet frees with the pod. The cache holds
only copies of what the bucket holds, each under a name that never names other
bytes, so a replaced pod reads it back. It is a `hostPath` rather than a local
`PersistentVolume`, because a claim would pin the pod to its node. A pod that
moves starts with whatever cache its new node holds. A cache file no host
holds stays on the node, counted by the hosts there as another writer's space.

Hosts that share a filesystem each set `SPROUTFS_DISK_USED_BYTES` to their
part of the disk, so the first cache to fill does not take all of it and a
restarted host finds room for its spill files. Within that, the reserve keeps
room for a VM's staging.

A host that serves shards counts none of them
([shards on network disks](#shards-on-network-disks)).

## Shutdown

Quiesce caller operations first. Closing the host stops the checkpoint loops,
then the peer server and the table of peers, then the VM handles, and then
closes the cache. Each VM publishes a final checkpoint if anything is dirty.
A failure there is logged and does not block the release, and the bytes it
could not publish are lost. Cancelling the wait stops only the wait; cleanup
continues, and the close can be retried with a fresh context.

A host that exits without closing its VMs loses every write since their last
checkpoints.
