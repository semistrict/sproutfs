# Live migration

A VM moves between hosts by stopping the source and then running on the
destination while its pages arrive from the source's pager. This is what a
planned host restart or scale-down does for every VM on the host: it is the
drain. It is post-copy only: nothing is uploaded inside the pause, whose
duration is the VMM state capture, the region handoff and destination startup.

## Why it is cheap here

Guest RAM stores and PMEM writes are private pager state — resident or in
scratch spill — until a [checkpoint](vm-memory.md) publishes them. A migration
publishes nothing at all: the source hands the VM over at the checkpoint its
control record already selects, and everything the guest wrote since that
checkpoint stays in the source's frames, where the destination fetches it from.
Uploading that residue inside the pause is the one cost this design exists to
avoid.

The exposure is the source dying during the post-copy, which loses the guest's
writes since the source's last interval checkpoint — the same window, up to one
60 s interval, that any host loss costs. Until the destination reports that it
has fetched every one of those pages, the source may not stop serving.

## Phases

0. **Quiesce the checkpoint loop.** The host's interval checkpoint stops first,
   and stopping it waits for the one it had in flight to finish publishing. A
   checkpoint that is still publishing owns the guest's sealed regions, and a
   hand-off that found one sealed would have to give the migration up and resume
   the guest.
1. **Stop.** The VMM pauses the vCPUs, drains device completions and captures
   the VMM state. Nothing is sealed and nothing is uploaded.
2. **Hand off.** Every region gives its volume up while keeping its frames, and
   reports which of its pages no checkpoint has — the guest is stopped, so that
   set is final. The source then releases the VM handle without publishing. The
   handoff carries the VMM state, the region layout, those unpublished page runs,
   the address of the source's page server, and the sequence the source's control
   record selected when it gave the VM up.
3. **Resume on the destination.** The destination opens the VM: it reads the
   control record, advances its epoch — which fences the source for good — and
   reads the selected checkpoint's root. That is two objects and no page. A record selecting
   any sequence but the one the handoff names is refused with `ErrStale` and
   released again without publishing: a migration publishes nothing, so that
   record was openable by anybody in between — a recovery that took the source
   for gone, an operator — and streaming the source's frames over a writer that
   got in would make one VM's memory out of two writers' pages, with no error
   anywhere. Otherwise it attaches the regions, binding each to the source host
   as well as to its own volume, and starts the VMM with the captured state.
4. **Post-copy.** Pages the guest touches fault in from the source host's pager
   first, over plain TCP to the handoff's page-server address, and from the
   destination's own checkpoint where the source cannot supply them. A page the
   source served out of its own dirty frames is dirty on the destination too:
   its volume reports those pages as the checkpoint's bytes or as holes, which
   would silently rewind the guest, so the peer backing reports them as bytes of
   its own and the pager holds each as a private frame under a spill
   reservation. The destination's next interval checkpoint publishes them.

   A background stream fetches the unpublished pages first and to completion,
   then the rest of the source's resident set as an optimization. `Done` returns
   only once every unpublished page is here or has failed to arrive, and that is
   what allows `ReleaseMigrated` and the source host's exit; the bulk pass runs
   on behind it, and `Streamed` is what reports that the whole resident set has
   arrived.

   **The one post-copy rule.** A page only the source holds is asked for until
   it arrives, or until something that knows says the source is gone. An
   unpublished page is never satisfiable from the destination's own volume,
   because the checkpoint there predates the guest's write, and nothing in a
   destination can tell a source that stumbled from one that died: a `BUSY`
   reply, a reset connection, a timeout, a listener restarting and a connection
   a budget dropped are all asked again, with backoff, for as long as the
   backing lives. There is no attempt count and no failure threshold. Two
   things end the asking. The source itself, answering that it no longer serves
   this VM, which it does only after a release it agreed to: from then a page
   the checkpoint holds reads from the volume and a page only the source held
   fails with `ErrUnpublishedLost`. And this host closing the backing, which
   says the same thing from this side: a read it interrupts reads the volume
   too, and only one that still holds a page only the source had ends with the
   close's own cause.

   That second end is two different moments. A destination closes the backing
   the moment its post-copy is over, and its guest is running and faulting all
   the while, so one of those faults is on the wire when the close lands: every
   page it asked for is published by then, so it comes back from the volume and
   the guest never notices — a fault that failed would end the memory session
   and the guest with it. The other is the orchestrator ending the migration,
   which discards the destination's received VM when it has lost the source
   host; the pages only that host had are lost with it, the reads waiting for
   them end with the discard's cause rather than as pages lost, and the VM is
   recovered from its checkpoint.

   A run every checkpoint holds is not worth waiting on, busy or broken: it is
   read from the volume this time and decides nothing for the next load, which
   asks the source again. A source serving pages of another size and a reply
   this host cannot read are neither a source that is gone nor one that
   stumbled — they are a source this destination cannot use — so the fault
   fails with that cause and the received VM is torn, exactly as a torn
   post-copy is handled anyway. The resident listing is decided the same way,
   and for the same reason: the caller of that listing is the stream, which the
   destination stops itself, so giving the source up there would send a healthy
   region to a volume that does not hold the pages no checkpoint has.
   Only pages the pager went on to install count as fetched, so `Done` cannot
   report a complete set the source could then release. Serving a page is not
   holding it: the bytes reach a buffer and the pager may still drop the page,
   so the destination strikes one off only when its own pager reports having
   bound it as dirty state of this region. The source keeps the
   same count from its own side: it records each region's unpublished set when
   the migration registers it, strikes off every one of those pages as it
   answers for it, and refuses a release while any remain, keeping the VM
   served. A page is struck off only once the reply carrying it has left this
   host: a reply the source could not send carried nothing, so striking it off
   when the bytes were merely assembled would let the release drop the only
   copy of the guest's writes. Nothing about that rests on the orchestrator's
   table — the source answered the fetches, so it is the one thing that knows —
   and the refusal is
   a `409` the caller repeats once the destination is done. A VM the host is
   giving up rather than handing over, such as a fork hold that outlived its
   deadline or a fan-out that failed, is discarded instead, because its pages
   are going either way. A set that never completes leaves a guest made of
   this host's half and a missing half, and nothing can publish it:
   `Host.Receive` waits for `Done` itself and discards such a VM rather than
   returning it: the VMM process is closed, the handle is released through
   `Handoff` so that nothing dirty is published, and the supervisor is told to
   forget it. What a recovery then opens is the checkpoint the control record
   already selects. Only `ErrUnpublishedLost` says a VM is that, and nothing
   else `Done` returns is grounds for giving one up — the caller's own
   cancellation stops the wait and nothing else, because the stream runs on a
   context of the package's own and `Done` may simply be called again, and
   `ErrClosed` is a stream this host stopped, which says the source may not
   release yet rather than that this guest is torn. Each region's pages are
   fetched over as many of its connections as it has, rather than one page per
   round trip. The held set includes private zero-filled frames allocated by
   [write-ahead](vm-memory.md), even if the guest has not stored into them, so
   peer-page counts measure transferred held pages and can exceed the pages the
   guest explicitly wrote.

Before any region has handed off its volume, a failed migration attempts to
resume the source. Once a region has successfully handed off, that process
cannot resume the VM; the VM is still openable anywhere, at the checkpoint its
control record selects. Resuming a machine also requires the captured VMM state
and matching region configuration.

```go
// vmmemory
// ReadResident copies one resident page for a peer, if this host holds it;
// false when it does not, and separately whether the page it served is this
// host's own state that no checkpoint has. It never loads.
func (r *Region) ReadResident(ctx context.Context, page uint64, dst []byte) (held, unpublished bool, err error)
// Resident lists the pages this region currently holds, for a bulk stream, and
// Unpublished the subset of them no checkpoint has. A region that cannot answer
// reports why: an empty listing is one a destination acts on by fetching
// nothing and reading the volume instead, which for the unpublished set means
// rewinding the guest to the last checkpoint. A source whose volumes cannot say
// what they hold keeps serving them — only Discard gives such a VM up.
func (r *Region) Resident() ([]uint64, error)
func (r *Region) Unpublished() ([]uint64, error)
// Handoff gives up the volume while keeping the frames, after the guest is
// stopped. Verification stops touching the volume; a flush, a seal, a
// population or any fault reports ErrHandedOff; serving continues until Detach.
// A region a checkpoint still has sealed reports ErrSealed and keeps its volume.
func (r *Region) Handoff(ctx context.Context) error

// vmmachine
// Backings replaces, by volume name, the backing a region attaches with. The
// volume stays the region's identity — name, size, writer, lineage, every
// write — and only what the pager loads through changes, which is how a
// destination's regions fault from the host that still holds their pages.
// Every name must be a region the machine maps, and the backing must be the
// size of the volume it stands in front of; a start refuses anything else
// rather than silently binding a destination to its own volumes.
type Config struct { ...; Backings map[string]vmmemory.Backing }
// Regions names every region by the volume it maps: ram0 and one per PMEM
// device id.
func (p *Process) Regions() map[string]*vmmemory.Region
// Stop pauses the vCPUs, drains device completions and returns the VMM state,
// leaving the process paused. It seals nothing and uploads nothing: the frames
// it leaves behind are what the destination fetches. Prepare is the capture
// that seals and resumes.
func (p *Process) Stop(ctx context.Context) ([]byte, error)

// volume
// Handoff publishes nothing. It marks the handle terminal and releases it, so
// the next open anywhere starts at the checkpoint the control record already
// selects and fetches everything written since from this host's pager. It waits
// for a publication already in flight, so a checkpoint still uploading is not
// left half-selected.
func (vm *VM) Handoff(ctx context.Context) error

// vmmigrate
// PageSource serves the pages one host holds for another to peers over the host
// network: a migrated VM's regions, or the instant a fork was taken at. One per
// host, registered under the identity of the VM that runs elsewhere. It opens a
// listener on Network at Address, or takes one the caller already opened. Hosts
// share a trusted network, so every peer that reaches it is served, bounded per
// remote address.
type PageSource struct{ ... }
func NewPageSource(ctx context.Context, config SourceConfig) (*PageSource, error)
func (s *PageSource) Serve(vmID string, pages map[string]Pages)
// Release stops serving a VM once every page it holds has been fetched, and
// refuses while any is outstanding. Discard stops serving one whose pages are
// going either way, which is what a VM the host is giving up rather than
// handing over takes.
func (s *PageSource) Release(vmID string) error
func (s *PageSource) Discard(vmID string)
// PeerBacking wraps a volume as a pager Backing whose Load asks the source
// host first and reads the volume for every page a checkpoint holds, and which
// reports the pages the source served out of its own dirty frames so the pager
// holds them privately. Locate is the volume's except for those pages, which it
// reports as bytes of this region alone so nothing resolves them against a
// checkpoint lacking them. A page only the source holds is asked for until it
// arrives, the source says it no longer serves the VM, or Close ends this
// region's half of the migration.
func NewPeerBacking(config PeerConfig) (*PeerBacking, error)
// Close ends that: every connection dropped and nothing asked of the source
// again. A read it interrupts reads the volume, exactly as one interrupted by
// the source saying it has gone does; one still holding a page only the source
// had ends with the close's own cause. It is what a destination whose post-copy
// is over does, and what discarding a received VM comes to.
func (b *PeerBacking) Close() error
// Migrate runs phases 1 and 2 on the source: stop the process, give the
// regions' volumes up, record which of their pages no checkpoint has, release
// the VM without publishing, and serve the frames from there on. It returns the
// VMM state the destination restores.
func Migrate(ctx context.Context, vm *volume.VM, process Runtime, source *PageSource, opts Options) (Handoff, error)
// Receive runs phases 3 and 4 on the destination: open the VM, attach
// regions through PeerBacking, start the VMM from the state, and stream the
// resident set in the background. It reports when the stream has finished so
// the source may release.
func Receive(ctx context.Context, manager *volume.Manager, handoff Handoff, dial Dialer, start StartFunc) (*Received, error)
// StartFunc is the supervisor the destination host supplies: one backing per
// region, keyed by the volume that region maps, and every one of them must be
// what the machine attaches with — vmmachine.Config.Backings for a Firecracker
// supervisor, host.MigrationConfig.StartVM for the host that wires it.
// A start that drops them binds the destination to its own volumes: nothing
// ever faults to the source and the post-copy is a cold read of storage. A
// machine that maps fewer regions than the source had is refused and closed.
type StartFunc func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (Runtime, error)
```

The page protocol is two requests over the framed host codec. A page request
names the VM, the volume and a run of pages; its reply carries a bitmap with one
bit per requested page and, in the payload frame, exactly the pages whose bit is
set, in ascending order. A second bitmap marks which of those pages are the
source's own state that no checkpoint has. A clear present bit is not an error:
it is the plain fact that this host does not hold that page, and the destination
reads it from its own volume. A resident request lists what a region holds, in
runs, bounded per reply, which is what a destination's bulk stream walks before
it faults those pages in through the pager's ordinary load path — streamed bytes
are never written into a region directly, because the load path is what keeps a
frame shared by lineage with the other VMs on that host. Both are bounded per
peer, and a peer is the destination host rather than one of its connections: a
destination opens a connection per region and dials again whenever one breaks,
each with an ephemeral port of its own, so counting those separately would bind
neither budget and would grow the table of peers with every reconnect. A
connection over the budget is closed, and a request over the bytes-in-flight
budget is answered `BUSY`, which the destination reads from its volume this time
unless the run still holds a page no checkpoint has, in which case it asks again
with backoff until the source serves it. That is why a region gives its
connections back the moment it has no request in flight, keeping one: the
connection budget is a bound on what one destination host holds at once across
every region of every VM it is receiving, so connections a region pooled for a
burst that is over are a bound held against the regions still asking — and what
they ask for is the pages no checkpoint has, which they ask for for ever. A
region that kept a whole burst's connections for the life of its receive would
make the bound a deadlock rather than a queue. A source that says it does not serve
the VM sends that region to its volume permanently, logged once — for every page
but those, which have no copy in the volume and fail the fault instead; every
other answer is asked again, under the rule above.

Ordering inside the pause is fixed and asymmetric. The regions give their
volumes up before the VM is released, because a region that kept its volume
across the release would fail its next verification. A failure before the first
region's handoff completes attempts to resume the guest here; resumption can
itself fail. After a region has handed its volume off, resumption is no longer
possible, and `ErrStopped` says so: this process will not run that VM again. The
host acts on it. A VM whose migration stopped it and then failed is discarded
rather than put back on the checkpoint interval: its fork points are retired,
its pages stop being served, its VMM process is closed, its handle is released
and the supervisor is told. Putting it back would leave a stopped guest whose
interval seals regions that no longer own the volumes they map, with a VMM still
running and a handle nothing closes. Reopening the VM advances its epoch even if
the source did not finish releasing it. Resuming the same machine execution additionally requires its captured VMM
state.

The pause contains the VMM state capture, the region handoff and the
destination's open. It uploads nothing. The destination reads the control record
and the selected checkpoint's root from object storage, and no checkpoint page
during open; the simulated suite asserts that access pattern. The measured pause
is an observation for that workload, not a general latency target.

The host exposes `Host.Migrate(ctx, vmID, destination)` for the drain
hook and `Host.Receive` for the destination daemon; `Host.Drain` runs the first
over every VM the host holds, four at a time by default. The deployment must
arrange the drain and destination handoffs before exit. `sproutfsctl migrate VM
[--to HOST]` drives it through the orchestrator, which picks the emptiest
other host when no destination is named. See [hosting](hosting.md#draining-a-host).

The orchestrator holds the other end of the post-copy rule. While a
destination is receiving, it surveys the host that handed the VM over, every
`SourceWatchInterval`; losing that host ends the receive, which is what
discards the destination's half-received VM, and the VM is then recovered from
the checkpoint its control record still selects, with the in-flight row going
with it. The evidence has to be positive, because a guest is torn down by it: a
pod the Kubernetes API no longer lists, or a host that answers and neither runs
the VM nor serves its pages, which is a host that came back without the frames
it was holding. A host that is merely quiet is a host whose guest may be
perfectly well, so the destination goes on waiting for it — and a source that
is alive but unreachable ends the wait itself, because its own handover
deadline of four checkpoint intervals gives those frames up, after which its
next answer is the one that says it no longer serves the VM.

## A fork is a handoff from a parent that keeps running

A [fork](volumes.md#the-fork-instant) is this same mechanism with the source
left running. There is one fork path and it is a handoff, whatever host the
child lands on: where it lands changes only how the pages it inherits reach it.

Phase 1 becomes the capture's pause rather than the migration's stop — the vCPUs
pause, the VMM state is saved, the dirty set is sealed and the guest resumes —
and phase 2 gives nothing up: no region hands its volume off, the VM handle is
not released, and nothing is published. For a child bound for another host, what
the source registers with its page source is the fork point, under the child's
identity, and what it serves out of it is exactly the pages no checkpoint of the
parent holds; everything else is in object storage, where the child reads it
from. For a child the parent's own host takes in, nothing is registered at all:
that host has the frames.

Phase 3 creates the child instead of opening the VM: the handoff carries
`Parent` and `ParentCheckpoint`, the destination rebuilds the instant from that
pinned checkpoint, and the child's control record selects a checkpoint that its
own first publication writes. The pin was written on the parent's host before
the handoff was built, by the only writer that holds the parent's epoch, and
nothing gives it back: the destination has no way to know what else reads that
lineage, and neither has the parent. A destination that is the parent's own host
skips the rebuild: it holds the instant itself, so the child is created from it
and reads the pages written since the pinned checkpoint through it.

Phase 4 is the backing, and it is the whole of the difference between the two
destinations. On another host it is `PeerBacking`: it marks the pages the parent
served as this host's own, the background stream fetches them first and to
completion, and `Done` reports when the parent may stop serving. On the parent's
own host it is the local backing: attaching it offers the parent's sealed frames
to the pager under the identity the instant gives them, so every inherited page
is present the moment the region attaches, `Done` reports immediately, no byte
is copied and nothing is dialed.

The destination publishes the child's root index as soon as `Done` reports.
Until it lands nothing outside that host can open the child — a host lost in the
meantime loses it, and nothing can seal it, so it can be neither forked nor
migrated — so a fork is not finished until it has one. Publishing it is also
what gives back the hold the child's own handle has on the instant.

Releasing the parent is `ReleaseMigrated` under the child's identity, and unlike
a migration it closes no process: it stops serving those pages and retires the
fork point, which hands the sealed frames back to the parent's guest. The parent
is not checkpointed while a fork point holds its frames, so the release is also
what lets its interval checkpoint run again.

That release is not the only thing that ends the hold, because a parent sealed
for good is a VM nothing can checkpoint, fence or migrate and whose dirty set
only grows. Every hold carries a deadline of four checkpoint intervals, wherever
its child went: when it passes, the host stops holding the instant for that
child and retires the point itself, and the child falls back to the checkpoint
its record already selects. The
orchestrator reconciles from the other side — every survey reads each host's
`Serving` set against its table and releases whatever has no operation in
flight, so an orchestrator that restarted between the handoff and the release
makes it on its first survey, while a migration or fork that is still running is
left alone. That set is every handover the host holds, a child taken in on its
parent's own host included: such a child is served nothing over the wire, but
the hold on the instant is what keeps the parent sealed, and a host that
reported it as holding nothing would be telling the deployment that a parent
nothing can checkpoint is a parent nothing is waiting on.

A handover of a VM no host runs and the table has never heard of is given up
rather than released. It is the child of a fan-out that failed, so nothing holds
the pages the source kept for it and nothing ever will: a release of those is
the one request the source must refuse, because they are the only copy of the
parent's writes since its last checkpoint. `POST /vms/{id}/abandoned` is the
word for that, and it refuses nothing.

One instant serves any number of children: each is one hold on the point, and
the seal ends when the last is retired, so a fan-out of forks costs the parent
one pause and one request. A second instant is refused while one is outstanding,
because only one seal of a region is.

Deleting the parent ends every hold on it the same way. `Host.Delete` retires
the points taken on that VM before it closes the process whose frames they are:
what a child holds is an instant in this host's memory, not an object, so a
parent deleted under one would leave the page server offering an instant whose
frames are gone, and every page the child had not yet fetched would come back
absent and be read from the checkpoint instead. Retired first, the child's next
fault for one of those pages fails and says so. A VM still sealed after that is
refused, exactly as a migration of one is, and left running: the holder is not
one of this host's own holds — a child created here whose first checkpoint has not
published yet holds the point through its own handle, and a capture in flight
holds it through the publication — and deleting it would take the instant out
from under whoever has it. The parent's checkpoint objects are untouched by a
delete where a pin covers them — a child still inherits them — and reclaiming a deleted lineage is the
collector's.

A fork on the parent's own host skips the network entirely, and it is the same
call: `Host.Fork` builds a handoff for every child and holds the instant for
each of them, and a child with no destination named is served nothing, because
the host that takes it in is the one holding the frames. The orchestrator drives
both halves for every child — `sproutfsctl fork VM [--count N] [--to HOST]`,
defaulting to the parent's host — giving each handoff to its destination's
`Receive` and then telling the parent's host to release that child. Every fork
goes through the orchestrator, which allocates each child's identity and records
its host and its parent before the parent is paused, and which admits the whole
fan-out against the host that will take it in before the parent is paused for
it; a host never forks on its own.

A fan-out that did not happen leaves nothing behind, wherever the children were
going, and it is one rollback. The parent's host gives up every hold of a
fan-out it could not hand over whole. The orchestrator gives up every child of
one whose destination refused: whichever children exist under the identities it
named are deleted, and deleting an identity that was never created removes
nothing. A child that was taken in is released — it holds every page it
inherited, which is what the release says — and one that never started is given
up on the source instead, because nothing fetched what its hold keeps and
nothing ever will. The children of
a failed request are guests nobody asked for, holding a host's memory under
names only that request ever knew.

```go
// volume
// ForkPoint seals the guest and returns the instant a child starts from,
// publishing nothing and giving nothing up. The parent stays sealed until the
// last child has retired the point.
func (vm *VM) ForkPoint(ctx context.Context, prepare PrepareFunc) (*ForkPoint, error)
// Inherit rebuilds that instant on a host that never held the parent, from the
// checkpoint the parent pinned.
func (m *Manager) Inherit(ctx context.Context, parent control.Ref) (*ForkPoint, error)
func (m *Manager) Fork(ctx context.Context, id string, point *ForkPoint) (*VM, error)

// Share offers the parent's sealed frames to this host under the identity the
// instant gives them, which is the local backing's attach.
func (f *ForkPoint) Share(ctx context.Context) error

// vmmigrate
// Pages is what a page source serves one volume out of: a migrated VM's region,
// or a fork point. RegionPages and ForkPages adapt each.
type Pages interface { ... }
// Fork registers a fork point under the child's identity and describes it. The
// pause already happened; nothing is stopped and nothing is released. A nil
// source is a child the parent's own host takes in: it is served nothing.
func Fork(ctx context.Context, child string, point *volume.ForkPoint, source *PageSource, opts Options) (Handoff, error)
// Options.Point is the instant a child whose parent runs here is received over.
// Receive creates the child from it and binds its regions to a local backing.

// host
// Fork hands every child of one instant over, to another host or to this one.
func (h *Host) Fork(ctx context.Context, parent string, children []string, destination platform.Address) ([]vmmigrate.Handoff, error)
// Receive takes a handoff: a migrated VM, or a fork's child. It publishes a
// child's root index as soon as that child holds every page its parent had.
func (h *Host) Receive(ctx context.Context, handoff vmmigrate.Handoff) (*vmmigrate.Received, error)
```

## Qualification

The simulated suite runs two volume managers and two pagers over one simulated
world, with a guest that stores continuously through a simulated mapping. It
requires that the destination's byte model equal the source's at the stop, that
the pause read and write no object of the volumes at all, that every page the
source held be served by the source and never asked for again, that every page
no checkpoint had be fetched before `Done` returns and published by the
destination's own next checkpoint, and that `Done` not return while one of them
is still only on the source. A failure in the stop must leave the guest running
here with its memory intact, and losing the source before the destination
fetched its unpublished pages must leave the VM openable at the checkpoint its
control record still selects, rewound by exactly the writes since it. A
destination whose supervisor starts a machine that does not map every region the
source had is refused before that machine runs, and the machine is closed. The
page protocol is covered on its own: a partially resident region answered in one
request, an unserved volume ending the asking on the one answer that says so, an
unreachable source costing each load its round trip and no more for the pages a
checkpoint holds, a connection over the per-peer budget refused, and a busy
source not mistaken for a gone one.

The one post-copy rule is four requirements, under the simulated clock and
network. A source that resets twenty connections in a row still serves the page
afterwards and the fault completes. A source silent for ten simulated minutes
leaves the fault waiting rather than failed, and completes it with the right
bytes when it answers. A source answering that it no longer serves the VM fails
a fault for a page only it held at once, with `ErrUnpublishedLost`, and serves
the pages a checkpoint holds from the volume. Discarding the received VM ends a
waiting fault with the cancellation's cause and nothing else — which is also
what an unreachable source comes to: `Done` waits, and the discard is what ends
it. Beside them, a busy source is asked again until it serves the pages no
checkpoint holds and the destination ends with its bytes rather than the
checkpoint's, and `Done` returns while the bulk resident stream is still
running.

The deployment's half is in the simulated deployment: losing the host that
handed a VM over, while its destination is in the post-copy, ends that handover
at the moment of the loss rather than at the end of anybody's patience, and the
VM comes back on the host that is left at the checkpoint its record selects.

The host suite runs the same migration between two hosts over loopback TCP,
including a drain that moves every VM one host runs.

Forks are qualified on the same model. A fork on the parent's own host has to
receive its child over the frames the seal froze — every inherited page mapped
by identity, no page loaded back and no connection dialed — and the children of
one instant have to map each other's frames rather than their own copies. A fork
onto another host has to name and pull exactly the pages no checkpoint of the
parent holds and nothing else, and leave neither side able to see the other's
later stores. Either way the child's root is published when it holds those pages
and not at whatever interval checkpoint comes first: with the interval loop off
on every host, a third host opens the child as soon as its handoff is done.
Losing the parent's host after that leaves the child openable anywhere, reading
back the instant it was forked at; before the child has published, opening it
anywhere reports `ErrForkPending`. A parent deleted or left unreleased under a
child is handled the same way wherever that child is: the delete retires the
holds this host has, a parent something else holds sealed is refused, and a hold
nothing releases is given up by its deadline.

The full-guest suite migrates a real Firecracker guest between two pagers and two
managers in one process, over loopback TCP: the
guest stores into its RAM and its DAX disk until the vCPUs stop, and every
region of the destination is started through a `PeerBacking`, so the source's
page server is on the VMM's own fault path rather than beside it. The guest comes
back with its counters intact, read back through the console, and the pages those
faults touched are shown by `PeerStats` to have come from the source; a fault on
a page the guest has not reached and only the source holds is served by the
source too. What the source serves is compared byte for byte against what the
destination's own checkpoint holds. After `Release` the same fault path reads the
volume, the peer count stops moving, and the region never asks that source again.
It reports the stop-to-resume pause, how many pages of each region the handoff
named as unpublished, and the peer and volume page counts of every region.

### Page payload compression

Page requests and replies require payload format 1. Each successful reply carries
one independent raw-or-Zstandard blob containing the present pages in bitmap
order, normally one 2 MiB page. CRC32C covers transmitted bytes; the blob checks
its decoded length and contents before any bytes reach guest memory. Source
admission still charges full logical page bytes. Old page protocols are rejected.
The control plane's `Handoff.State` remains the runtime's raw state; checkpoint
VMM-state objects are compressed.
