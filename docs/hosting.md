# Hosting

A host runs VMs. One process assembles that role over one object store, one
plain TCP network and one RAM allotment. It owns no durable local state: a VM's
authority is the epoch in its [control record](metadata.md), and its data is the
checkpoint that record selects. It holds no identity either: hosts run on a
trusted cluster network, and the only address one ever dials is the page-server
address a [handoff](migration.md) carries.

## Assembly

Everything a host does is `internal/host`. `Host` is what the deployment runs on
this machine: the object namespace, the shared page cache, the volume manager,
the migration page server, and the loops that keep what it runs durable and
fenced. The supervisor around it — `host.Start`, which returns the `host.Service`
the command serves — owns the two pagers, each over an arena and a spill file of
its own: RAM's 4 KiB pages on an ordinary memfd, PMEM's 2 MiB pages on the
node's HugeTLB pool. It also owns the
Firecracker processes, the templates guest images are imported into, and the
channel to the agent in a guest. `cmd/sproutfs-host` keeps its configuration, its
HTTP handlers and the wiring between them, and nothing else.

Starting a `Host` requires a resource owner with a positive RAM allotment, the
network its page server and its handoffs run over, and the object store with the
deployment's prefix. The host owns, in one lifetime: the control client that
reads and writes the deployment's control records; one checkpoint store with the
host's shared [page cache](volumes.md#page-cache), which is given a cap of its
own rather than the host allotment; the volume manager that opens this host's
VMs; and, when a migration address is configured, the page server.
A host without that address neither drains nor receives.

Which adapter stands behind each of those ports is the command's decision and
nothing else's. `sproutfs-host` builds the GCS object store, the plain TCP
network and the node disk, and hands them over. GCS is the only object store
adapter shipped, because it is the only one a deployment runs on; the port is
the conditional-write contract `internal/platform/internal/real`'s conformance
suite states, so another store's adapter is a package-local addition and a
second `runObjectStoreConformance` caller. The host names no adapter and
neither does `vmmachine`, which is given a `platform.Disks` for the staging
directory each VMM gets.

Every VM the host starts is one RAM volume, `ram0`, plus one volume per PMEM
device; the supervisor gives each VM a single PMEM device, `root`, which the
guest boots from. The supervisor opens a `vmmachine.Scratch` and passes it to
each VMM configuration.

A process restart is a host loss. Nothing under the scratch, and nothing in the
spill file, survives one: opening the scratch deletes its VM directories and
recreates them empty, and the spill file is truncated. There is no
reconciliation, no scan of what a previous process left, and no lock held across
processes — a host that restarts has lost every VM it was running, and the
deployment reopens each of them, wherever they land, from the checkpoint its
control record selects. The deployment must give one host process one scratch
directory.

`sproutfs-host` takes its whole configuration from the environment, reporting
every problem it finds rather than the first, and serves the host API over HTTP:
status, create, open, fork, capture, console, exec, migrate, receive, released,
drain and delete. Each handler is one call on the supervisor plus the shared
JSON failure shape; the API's own types live in `internal/api/host` and link
nothing of the runtime, so the host converts a handoff at its boundary rather
than putting a pager on the wire. The GCS store the command builds is metered
per operation, so that one counter is the deployment's whole object traffic; an
endpoint selects an emulator instead of the ambient Google credentials.

Every one of those requests but the kubelet's probe carries the deployment's
shared bearer token, `SPROUTFS_API_TOKEN`, and is refused with 401 without it;
the same token admits the orchestrator's API, and the same middleware serves
both. It is not authority over any VM — that is the epoch in the control record
— and it names no caller: it says only that the request comes from inside this
deployment, which is what an API that can delete or drain every VM on the pod
network needs before anything else. Restricting the ports to the deployment's
own pods is the cluster's network policy, which `deploy/30-networkpolicy.yaml`
ships. Draining is a POST for the same reason: a GET is what anything that walks
URLs does, and this one migrates every VM off the host, so the preStop hook runs
`sproutfs-host drain` rather than fetching a URL.

Creating a VM forks a template: each guest image is imported once, into a VM
that is never booted, and every VM created from that image inherits the
template's published checkpoint without copying a byte. A template is an
ordinary VM with an ordinary control record — that is what lets a fork inherit a
published checkpoint — under a reserved identity namespace, which is what keeps
this bookkeeping out of the deployment's list of VMs.

**A template is named by its image.** The identity is `template-<sha256 of the
image file>`, computed by the host over the file it was configured with. A
template is not a VM that lives on a host: it is an imported image, and an
image's identity is its bytes. So every host configured with one image names one
template, and what a starting host does depends only on what the deployment
already holds of it:

- **published** — its control record pins the checkpoint it selects — and the
  host opens nothing and writes nothing. It reads that checkpoint the way any
  host reads a checkpoint it did not publish, and remembers it for `create`.
  This is the case for every restarted pod, and for the second host of a pair;
- **absent**, and the host creates the template, writes the image into its root
  volume, checkpoints it and pins that checkpoint, which is what publishes it;
- **a record with no pin**, which is an import that has not finished. The host
  waits for it — an import in flight is the ordinary reason, and opening the
  record would take the epoch out from under the host doing it — and past that
  wait takes the host that wrote it for gone and recovers the template as any VM
  is recovered: it takes the epoch, which fences a writer that is alive after
  all, and imports again under it. What the interrupted attempt published stays
  where it is, a superseded epoch's checkpoints like any other takeover's.

Two hosts starting together race on the record's create-if-absent; the one that
loses is refused the create and is then in the third case above.

The imports run at startup, behind the API, and a host is ready when every image
it is configured with has a published template, whoever imported it: an image
nothing has yet costs a whole file read and a checkpoint, and one another pod
has already imported costs a read of one control record. A host that cannot read
its images stays unready and says why, rather than taking VMs it would fail to
create. Liveness is a separate endpoint, so a host doing an honest import is not
restarted for failing readiness.

No host deletes a template: another host may be forking from it, and an image
that changed under its name is a template of its own rather than the same one
holding other bytes. The templates of images nothing creates from any more are a
collector's, like every other pinned checkpoint — the checkpoint a template pins is
the fork point every VM created from it was taken at.

One thing comes with that and is worth saying plainly: the RAM a VM gets is the
size the image's template was imported at, and the template was imported once.
`SPROUTFS_VM_MEMORY_BYTES` and a template's own `name=path:bytes` are read at
that import, so raising either gives no new memory to a VM created from an image
the deployment already holds. A cold start with a new size is what changes one
VM's shape.

Software in the guest is reached over the VM's vsock, which carries exec and
nothing else.

## The checkpoint loop

`Host.AddMachine` records which VMM process belongs to which VM, which is what
makes a VM drainable and what starts its checkpoint loop: the host checkpoints
every VM it runs every `Config.CheckpointInterval`, sixty seconds by
default, each wait jittered by up to an eighth either side so VMs do not checkpoint
in lockstep, and waits for each publication before scheduling the next. That
loop is the only thing that makes a running guest durable, so the interval
bounds what losing this host rewinds a VM by. A failure is logged and retried at
the next interval, and the loop stops when the VM is removed, migrated away or
the host closes. An interval whose VM is sealed by a fork point is skipped: the
child takes those pages first. A negative interval disables the loop, which is
what a caller driving its own checkpoints wants.

The interval bounds that rewind only while publications land. `Config.LossWindow`
bounds it when they do not: five minutes by default, zero to disable, and never
shorter than the interval — a window below it is one every VM is past before its
first checkpoint is even due. While a VM's oldest unpublished write is older than
the window, the pager admits no further dirty page for it, and the host's own
part is the loop. A publication that failed while the window is exceeded is
retried at an eighth of the interval, doubling to the interval, rather than an
interval later: the guest is held back for the whole of that wait, so an
interval's patience is exactly what it must not spend. Inside the window nothing
changes, because retrying eight times as often would only multiply what a store
outage costs the deployment in requests. That backoff is the one wait a request
out of turn does not cut short — the store the window holds back asks again
every time this loop signals, and a capture that cannot be published gives it
nothing, so answering each ask would spin a host that cannot reach the store.

The window is a VM's, not a region's, because the checkpoint that ends it is: one
pause seals every region a VM maps. The pager holds no idea of a VM, so the host
answers for the age as it answers for the checkpoint — `Pressure.Oldest` reports
the oldest unpublished write across every region of the VM that maps the region
it is asked about. `Host.LossWindow` reports that age per VM together with
whether its stores are waiting, which `/status`, `/metrics` and `sproutfsctl
list` carry.

A guest can fill the host's dirty budget long before its interval comes round,
and it is then waiting for a checkpoint nothing has scheduled. `Config.Pager`
is how the host hears about that: the pager asks it for an immediate checkpoint
of the region holding the largest dirty set, the loop takes it out of the
interval's turn, and the stalled stores land when it retires. The host answers
for a VM it runs whose volume no fork point has sealed, and otherwise declines
so the pager can offer another region. Where no region can be checkpointed, the
pager reports the stall instead, and the host stops that VM deliberately. A store
the loss window holds back ends the same way where no checkpoint of that VM can
ever be taken, and the pager reports that apart — a window stall rather than a
budget stall, because the two say different things about a deployment: one that
its guests dirty faster than their checkpoints drain, the other that a guest's
writes cannot be made durable at all. That
stop is the same give-up every other loss of a VM goes through, with one last
checkpoint inside it: the registration is dropped and the close claimed, so a
stall, a takeover and the watcher finding the same process dead close the VM
once between them; the fork points taken on it are retired first, both because
a child reads them out of the process this is about to close and because a VM
one of them holds sealed cannot be captured at all; then the last checkpoint
takes whatever the VMM can still be paused for, before the process is closed,
since its regions are where those pages live; and then the VM is given up and
reported through `MachineClosed` like a fenced one. Where the failed store has
already killed the VMM, that capture is no longer possible and the stop keeps
nothing the kill would have kept either; what it adds is a logged reason, which
the kill has never had.

The loop also stops when another host has taken the VM's control record: that
handle can never publish again, and a refused publication is one place a fenced
host finds out. It is not a place every VM reaches, so it is not the only one.
A host re-reads the control record of every VM it holds every
`Config.EpochInterval`, two seconds by default, a few at a time rather than one
after another, and closes any whose epoch has moved past the handle it holds. A
round of one small read per VM, serialised, would take the slowest of them times
their number, which on a store having a bad minute is longer than the interval
itself — the check would fall behind exactly when a takeover is most likely.

That interval is what bounds how long a host fenced
between checkpoints — or fenced while a fork point holds a VM's pages sealed,
which is never checkpointed at all — goes on running a guest whose writes have
nowhere to go. A record that cannot be read changes nothing: only the record
itself, naming an epoch this host does not hold, is evidence of a takeover, and
a negative interval disables the timer for a test that drives the check itself.
Either way the host gives the whole VM up: it retires the fork points taken on
it and stops serving their children's pages, stops serving the VM's own, stops
the VMM, releases the VM's volumes, and reports the VM through
`Config.MachineClosed` so the supervisor forgets it and refuses exec against
it. Nothing a fenced host produced after the takeover can become that VM's
state — its every control-record write is refused — and from here nothing it
holds is served as that VM's state either.

A timer is not enough for a handoff. Handing a VM over is the one thing a host
does that gives another host pages no checkpoint holds, and the store cannot
refuse those the way it refuses a fenced writer's publication: the destination
post-copies whatever it is served over the checkpoint the real writer published,
so a source that was taken over between epoch ticks — or one whose reads of the
store are failing while its pod network is fine, which never reaches a tick that
tells it anything — would build one VM's memory out of two writers' pages. So
`Migrate` and the fork point both re-read the control record themselves before
the pause, one GET, and refuse on an epoch that has moved. A read that fails
refuses the handoff too, which is the one place a record that cannot be read is
not treated as no evidence: everything else a stale handle does is caught by the
store later, and this is not. The VM goes on running here either way, so the
next attempt — or the epoch timer, if the takeover was real — is what settles
it.

One handover of a VM runs at a time, claimed under the same lock the
registration lives behind. Two callers that found one registration each stopped
the guest, gave every region's volume up and registered the pages with the page
server; the loser, whose regions had already given their volumes up, then gave
the VM up and closed the process whose pages the winner's destination was about
to fault out of. The second caller is told instead, and the VM it asked about is
left exactly as the first left it. A VM a fork point holds sealed is refused the
same way, and so is a fork that has not published a checkpoint of its own: that
handle is the only thing that could ever publish that root, so handing it over
would leave an identity no host can open and a parent sealed for good. Both
refusals come before the guest is stopped.

`AddMachine` also watches the VMM process itself, through `Machine.Wait`, and
gives the VM up the same way when that process ends without being asked to: the
host kills it because its memory session failed, the kernel kills it, or it
crashes. The checkpoint loop would not notice for an interval, and what it would
find then is a socket that refuses the connection, which it logs and retries for
as long as the host runs while the supervisor reports a guest that no longer
exists. The watcher stops with the machine, so a process this host ends on
purpose — a handoff, a removal, a shutdown — is not reported as a death. The
account of one that is comes from the process: `vmmachine` writes a single error
record naming the VM, the pid, the cause the kill carried and the tail of the
console, and the memory session that ended writes what ended it — the region,
and the page when the failure was a fault.

Every mapped region must use `Host.Resources()`. Registration rejects a machine
whose regions use another budget and leaves cleanup to the supervisor; the
receive path closes a mismatched runtime before streaming pages or registering
it.

## Draining a host

A host configured with a migration address serves a second protocol there: the
page server that holds the memory of every VM it has handed to another host, and
of every child it has forked onto one. A child it forked onto itself is not
there: that child maps the pages rather than fetching them, so nothing of it is
ever served. It serves any peer that reaches it,
bounded per remote address by eight connections and 8 MiB of pages in flight.
Restricting that port to this deployment's hosts is the cluster's network
policy, not this process's.

`Host.Drain` migrates every VM the host runs, bounded to four at a time by
default: a drain is planned work whose cost is one host's memory, and moving all
of it at once would put all of it on the network together. It returns one
handoff per VM that moved and joins the failures of the ones that did not, which
keep running here. A failure before the handoff leaves the VM running and
checkpointing on the interval; a later one can leave the guest stopped and
needing to be reopened at its last checkpoint, as described in
[migration](migration.md).

The process's drain does not choose destinations: it asks the orchestrator to
move each VM, which drives both halves of the migration and is told when each
one starts and finishes. The destination's `Host.Receive` opens the VM, starts
the VMM from the captured state and streams the pages in. That stream reports
completion only once every page the source holds that no checkpoint has is on
the destination: a migration publishes nothing, so those pages exist nowhere
else. When it does, the source's `Host.ReleaseMigrated` stops serving that VM
and closes the process that held its pages.

That word is the orchestrator's, so a migration carries the same deadline a
fork's hold does: four checkpoint intervals after the handoff, a source
nothing has released gives those pages up itself, closing the stopped VMM
process that maps them. Otherwise an orchestrator that restarted between the
handoff and the release pins the source's arena for as long as the host runs.
The destination loses nothing it already fetched and reads the rest from the
checkpoint its record selects.

`/status` reports, beside `serving`, an `outstanding` count per VM in it: how
many pages this host still holds that no checkpoint has and that the
destination has not fetched. It is what says which of two things a name in that
list is — a handover still pulling its pages across, or one that has them all
and is waiting only for the word that releases it — which a drain that is not
finishing is the question about. A VM whose volumes cannot be listed reports
`-1`, because what it still holds is unknown. The destination's own side of it
is a line per receive when the post-copy finishes, carrying the pages served,
the requests its source refused for its per-peer budget and how long it took,
and a line whenever one read of pages only the source has has been waiting for
it past a few seconds — which is a guest thread stopped for exactly that long.

The deployment's preStop hook drains and then exits. The drain returns only when
nothing is left to hand over — `Status().Serving` is empty — and that is the
contract: once it is empty, every page this host held is either on a destination
or in object storage, so exiting costs nothing. Exiting before it loses every
write since each VM's last checkpoint, including dirty RAM, dirty PMEM and
unpublished local forks.

The drain bounds itself, because nothing else does: the hook carries no deadline
and what waits behind it is a termination grace period after which the pod is
killed with everything it still holds. Four VMs are handed over at once, each
with a 60-second deadline of its own, the whole drain with 80 seconds including
the wait for `Serving` to empty, and the orchestrator client with a timeout so
that a connection nobody answers cannot outlast either. The preStop command
waiting on that drain gives up at 90 seconds, which is a backstop for an answer
that never comes back over the loopback at all and has to be the shorter one:
a client that waited longer than the server's own bound would spend the
shutdown's share of the grace period waiting for nothing. A VM whose hand-over
ran out of time is left running here and goes on being checkpointed, and is
reported as remaining.

`terminationGracePeriodSeconds` is the hook's 90 seconds plus the shutdown
behind it, which is two 30-second halves run one after the other — stopping the
API, then the supervisor's close, in which every VM publishes a final checkpoint
— so 90 and 60 is the manifest's 150. A shorter one turns an orderly exit into a
host loss.

The supervisor owns VMM processes and the shared pager. After finishing the
required capture or handoff, it closes the processes and detaches their regions,
then calls the pager's `Close` before closing its arena and spill handles. Pager
cleanup failure keeps unproven allocations charged and must be retried; closing
one process must not close a pager still serving other VMs.

## Stopping a VM

`Host.Stop` is the deliberate end of a VM this host runs that leaves the VM
behind. It captures a checkpoint of everything the guest still holds and waits
for it, and only then closes the VMM process, gives the pages back and releases
the handle. The control record and the objects stay where they are, so any host
can open the VM again at exactly the bytes the stop published — which is the
whole difference between a stop and losing the host, where the writes since each
VM's last checkpoint go with it. It reports the checkpoint it published, because
that is the pause the VM comes back at and nothing else records it: the handle
that knew is released by the time the stop answers.

The checkpoint comes first and nothing is given up until it has landed. A
publication the store refused leaves the VM exactly as it was — running,
registered, checkpointed on the interval — because the alternative is a stop that
reported a failure and lost the guest's last writes anyway.

A VM something still holds sealed is refused, as a delete of one is: a fork
point holds the pages of the process a stop would close, and a child elsewhere
reads the pages no checkpoint holds out of them, so closing it would take the
point away mid-fault. Nothing is retired to get past that, which is where a
stop parts company with a delete — a delete ends the VM for good and stops the
children reading it on the way out, and a stop is a VM that is coming back.

Starting one again is `Host.Open`, which is the same call a recovery makes:
there is nothing about a stopped VM for this process to remember. What differs
is above it, in what the control plane requires before it asks — see
[the deployment's API](../deploy/README.md#the-orchestrator-api).

## Starting a VM cold

A VM always comes back exactly where it was: its selected checkpoint holds the
VMM state and every page of guest RAM, and an open restores them. A cold start
is the exception, and the reasons are the reasons one reboots any machine — a
guest that is wedged, a kernel or init change on the disk, or simply not to pay
for memory the guest does not need to keep.

`Host.OpenCold` opens the VM and, in one publication, discards it: every page of
the RAM volume is dropped, which makes those pages read as zeroes and leaves
them out of the new root, and the VMM state member is dropped with it, so the
new root names none. `volume.VM.DiscardMemory` is that one operation, under the
VM's publication lock, because half of either is a VM that can neither be
resumed nor booted — memory without the state captured over it, or state without
the memory it describes. The guest is then started through the existing path,
which boots the kernel because there is no state to restore; a template's
children already come up that way.

The root volume is exactly what the last checkpoint published, so the guest's
filesystem sees the boot as a power cut after that checkpoint: **the journal
recovers what a journal recovers, and nothing here promises more.** The pages
the discarded memory held are unreferenced by the new root and reclaimed by the
usual set difference except where a pin protects them, and the checkpoint that
dropped them is small.

It applies to a VM this host does not run. A running one is stopped first, by
the operator. A cold start this host could not boot cold — it has no kernel
configured — is refused before anything is discarded, and so is one whose
regions the pager could not all map.

### Resizing at a cold boot

A cold boot is the one moment a VM's shape can change, because nothing in memory
describes it any more. `OpenCold` therefore takes a `ColdShape`:

- `MemoryBytes` sets the RAM volume's size in the discarding publication. Any
  size the host admits is allowed, up or down; the memory is being discarded
  anyway.
- `RootBytes` grows the root volume in the same publication. The new pages read
  as zeroes, which is what a filesystem grown in place expects, and the guest
  takes them after the boot with `sproutfs-guest-witness grow /`. Shrinking is
  refused: the end of a filesystem is not the volume's to cut.

Both are refused for a warm start, at every layer that carries them. From a
resize on, a VM's committed RAM is its own rather than its template's, which is
what the control plane's own record of it tracks — see
[the deployment's API](../deploy/README.md#the-orchestrator-api).

## Budgets

The host takes one `Resources` owner, and it accounts RAM alone: the pager's
pages. `Status().Resources` reports its reservations and configured total.

Disk is not shared and not accounted. Each concern that writes to the node's
disk has a fixed cap of its own — the page cache's allotment, and the pager's
spill file, `SPROUTFS_SPILL_BYTES`, which is what bounds the dirty pages the
pager admits — so nothing has to be reclaimed across concerns, no ledger orders
them, and a full disk is a configuration error rather than a path through the
code.

- **VMM staging files** live in each process's own directory under the scratch,
  which goes with the process. Configuration and restore files are removed after
  startup, and a capture's state file is read back and deleted. The capture is
  bounded at 64 MiB by the file-size limit the VMM process runs under, so an
  oversized state file is refused rather than read. Failed cleanup is reported
  and can be retried. The scratch owner's `Close` refuses live VMMs and closes
  the stopped ones; close the processes, then the scratch owner.
- **VMM console output** is drained into a 1 MiB in-memory ring buffer per VMM
  process, which touches no disk and no budget. Output past that capacity
  displaces the oldest bytes rather than blocking or killing the VM, and the
  ring goes with the process. A read names an offset in the whole output and
  returns at most 256 KiB; a read from before what is still retained is answered
  from the oldest byte the ring has and reports that offset, which is how a
  reader that fell behind learns output was dropped. This is diagnostics, not an
  audit log.
- **The page cache** retains decoded pages inside a cap of its own,
  `SPROUTFS_CACHE_BYTES`, and yields unused entries before a retention fails. It
  is not taken from the allotment a guest's pages come out of, so nothing has
  to be reclaimed between them, and a miss that does not fit is served without
  being retained. Concurrent misses remain bounded.
- **Checkpoint uploads** are bounded by the checkpoint store, not by one
  publication: half this machine's cores, between 8 and 64, shared by every
  checkpoint the host publishes. The part builders behind them are bounded
  separately, at a quarter of the cores between 2 and 8, which with the parts in
  flight is what publication costs this host in memory.
- **Open VMs** bound the live handles one manager owns, 4,096 by default, with
  the per-write bound described in [volumes](volumes.md#writes). There is no
  bound on unpublished bytes: a write waits for nothing, and what it leaves
  unpublished is what losing the host would cost. `Stats().DirtyBytes` is that
  amount, summed over every VM the host runs.
- **The pager** budgets resident, logical and dirty pages host-wide; see
  [managed VM memory](vm-memory.md#bounded-host-pager). The logical budget is
  the one a VM is admitted against: it bounds per-region metadata, the pager
  checks it one attachment at a time, and a VM whose regions overrun it would
  otherwise have its first region admitted, its VMM started and then killed.
  So a create, an open and a receive each ask what the cap has left before
  anything starts a VMM, and a VM that cannot fit is refused. A fork is a
  handoff, so a child is admitted by the receive that takes it in, on whichever
  host that is; the orchestrator admits the whole fan-out against that host
  before the parent is paused for it.

The host's status reports cache usage and its cap, the volume manager's totals,
the pager's counters — including what the logical cap still has free, which is
what admits a VM — the object traffic and what the page server has served.

## Shutdown

Quiesce caller operations first. Closing the host stops the checkpoint loops,
then the page server, then the VM handles, and closes the cache. Each VM
publishes a final checkpoint if anything is dirty, which is what makes an
orderly shutdown lose nothing; a failure there is logged and does not block the
release, and the bytes it could not publish are lost. Cancelling the wait stops
only the wait; cleanup continues, and the close can be retried with a fresh
context.

Close proves nothing about object storage. A host that exits without closing its
VMs loses every write since their last checkpoints.
