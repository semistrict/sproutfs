# Hosting

A host runs VMs. One process provides that role over one object store, one plain
TCP network and one RAM allotment. The host has no durable local state. A VM's
authority is the epoch in its [control record](metadata.md), and its data is the
checkpoint that record selects. The host also has no identity. Hosts run on a
trusted cluster network, and the only address a host ever dials is the
page-server address that a [handoff](migration.md) carries.

## Assembly

All host logic is in `host`. `Host` is what the deployment runs on this
machine. It contains:

- the object namespace;
- the shared page cache;
- the volume manager;
- the migration page server;
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
The supervisor also drives the VMM processes, owns the templates that guest
images are imported into, and reaches the agent in a guest. It does not start a
VMM. A `vmmachine.Starter` does, as [running the VMM](#running-the-vmm)
describes.
`cmd/sproutfs-host` contains only its configuration, its HTTP handlers and the
wiring between them.

Starting a `Host` requires:

- a resource owner with a positive RAM allotment;
- the network that its page server and its handoffs run over;
- the object store with the deployment's prefix.

The host owns the following, all with the same lifetime:

- the control client that reads and writes the deployment's control records;
- one checkpoint store with the host's shared
  [page cache](volumes.md#page-cache), which has its own cap and does not use
  the host allotment;
- the volume manager that opens this host's VMs;
- the page server, when a migration address is configured.

A host without a migration address can neither drain nor receive.

Only the command chooses which adapter implements each of those ports.
`sproutfs-host` builds the GCS object store, the plain TCP network and the node
disk, and passes them in. GCS is the only object store adapter shipped, because
it is the only one a deployment runs on. The port is the conditional-write
contract that the conformance suite in `platform/internal/real`
states. So an adapter for another store is a package-local addition plus a
second `runObjectStoreConformance` caller. The host names no adapter. Neither
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

A process restart is a host loss. Nothing in the scratch directory or in the
spill file survives a restart. Opening the scratch deletes its VM directories
and recreates them empty, and the spill file is truncated. There is no
reconciliation, no scan of what a previous process left, and no lock held
across processes. A host that restarts has lost every VM it was running. The
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
register the pages with the page server. The loser, whose memory regions had already
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

A host configured with a migration address serves a second protocol at that
address: the page server. The page server holds the memory of every VM the host
has handed to another host, and of every child it has forked onto another host.
A child forked onto the same host is not there. That child maps the pages
instead of fetching them, so none of it is ever served. The page server serves
any peer that reaches it, bounded per remote address to eight connections and
8 MiB of pages in flight. The cluster's network policy, not this process,
restricts that port to this deployment's hosts.

`Host.Drain` migrates every VM the host runs, four at a time by default. A drain
is planned work whose cost is one host's memory. Moving all of it at once would
put all of that memory on the network at the same time. `Host.Drain` returns
one handoff per VM that moved, and joins the errors of the VMs that did not
move. Those VMs keep running here. A failure before the handoff leaves the VM
running and checkpointing on the interval. A later failure can leave the guest
stopped, and it must then be reopened at its last checkpoint, as described in
[migration](migration.md).

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
holds is unknown. On the destination side, each receive logs a line when the
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
  orchestrator trusts its record of a handover.
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

## Budgets

The host takes one `Resources` owner, which accounts only RAM: the pager's
pages. `Status().Resources` reports its reservations and configured total.

Disk is not shared and not accounted. Each component that writes to the node's
disk has its own fixed cap:

- the page cache's allotment;
- the pager's spill file, `SPROUTFS_SPILL_BYTES`, which bounds the dirty pages
  the pager admits;
- the ephemeral pager's spill file, `SPROUTFS_EPHEMERAL_BYTES`, which bounds
  the ephemeral disks the host admits.

So nothing has to be reclaimed across components, no ledger orders them, and a
full disk is a configuration error, not a code path.

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

- cache usage and its cap;
- the volume manager's totals;
- the pager's counters, including the free space in the logical cap, which is
  what admits a VM;
- the object traffic;
- what the page server has served.

## Shutdown

Quiesce caller operations first. Closing the host stops the checkpoint loops,
then the page server, then the VM handles, and then closes the cache. Each VM
publishes a final checkpoint if anything is dirty, so an orderly shutdown loses
nothing. A failure there is logged and does not block the release, and the
bytes it could not publish are lost. Cancelling the wait stops only the wait.
Cleanup continues, and the close can be retried with a fresh context.

Close proves nothing about object storage. A host that exits without closing its
VMs loses every write since their last checkpoints.
