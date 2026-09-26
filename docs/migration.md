# Live migration

A VM moves between hosts in two steps. The source host stops the VM. The
destination host then runs it while its pages arrive from the source's pager. A
planned host restart or scale-down does this for every VM on the host; this is
the drain. Migration is post-copy only. Nothing is uploaded during the pause.
The pause lasts for the VMM state capture, the memory region handoff and destination
startup.

## Why it is cheap here

Guest RAM stores and PMEM writes are private pager state, either resident or in
scratch spill, until a [checkpoint](vm-memory.md) publishes them. A migration
publishes nothing. The source hands the VM over at the checkpoint that its
control record already selects. Everything the guest wrote since that checkpoint
stays in the source's pages, and the destination fetches it from there. This
design exists to avoid uploading those pages during the pause.

The risk is that the source dies during the post-copy. That loses the guest's
writes since the source's last interval checkpoint. This is the same window that
any host loss costs: up to one 60 s interval. The source must keep serving until
the destination reports that it has fetched every one of those pages.

## Phases

0. **Quiesce the checkpoint loop.** The host's interval checkpoint stops first.
   Stopping it waits for the checkpoint in flight to finish publishing. A
   checkpoint that is still publishing owns the guest's sealed memory regions. If the
   handoff found a sealed memory region, it would have to abandon the migration and
   resume the guest.
1. **Stop.** The VMM pauses the vCPUs, drains device completions and captures
   the VMM state. Nothing is sealed and nothing is uploaded.
2. **Hand off.** Every memory region gives up its volume but keeps its pages. Each
   memory region reports which of its pages no checkpoint has. The guest is stopped, so
   that set is final. The source then releases the VM handle without publishing.
   The handoff carries:
   - the VMM state;
   - the memory region layout;
   - the unpublished page runs;
   - the address of the source's page server;
   - the sequence that the source's control record selected when the source
     gave up the VM.
3. **Resume on the destination.** The destination opens the VM. It reads the
   control record and advances its epoch, which fences the source permanently.
   It then reads the selected checkpoint's root. That is two objects and no
   pages. If the record selects any sequence other than the one the handoff
   names, the open fails with `ErrStale`, and the VM is released again without
   publishing. The reason is that a migration publishes nothing, so anybody
   could have opened that record in between. Examples are a recovery that
   assumed the source was gone, or an operator. If the destination streamed the
   source's pages over the pages of a writer that got in, the VM's memory would
   mix two writers' pages, and no error would be reported. If the sequence
   matches, the destination attaches the memory regions. It binds each memory region to the
   source host and to its own volume. It then starts the VMM with the captured
   state.
4. **Post-copy.** When the guest touches a page, the page faults in from the
   source host's pager first, over plain TCP to the handoff's page-server
   address. If the source cannot supply it, the page comes from the
   destination's own checkpoint. A page that the source served from its dirty
   pages is also dirty on the destination. The destination's volume would report
   those pages as the checkpoint's bytes or as holes, which would silently
   rewind the guest. So the peer backing reports them as its own bytes, and the
   pager holds each one as a private page under a spill reservation. The
   destination's next interval checkpoint publishes them.

   A background stream first fetches all the unpublished pages. It then fetches
   the rest of the source's resident set, as an optimization. `Done` returns
   only when every unpublished page has arrived or has failed to arrive. `Done`
   returning is what allows `ReleaseMigrated` and the source host's exit. The
   bulk pass continues after that. `Streamed` reports when the whole resident
   set has arrived.

   **Failure during post-copy.** A page that only the source holds is requested
   until it arrives, or until a party that knows says the source is gone. The
   destination's own volume can never supply an unpublished page, because its
   checkpoint predates the guest's write. The destination cannot tell a source
   that stumbled from one that died. So it retries all of the following, with
   backoff, for as long as the backing exists:
   - a `BUSY` reply;
   - a reset connection;
   - a timeout;
   - a listener restarting;
   - a connection dropped by a budget.

   There is no attempt count and no failure threshold. Two things stop the
   retries:
   - The source answers that it no longer serves this VM. It does this only
     after a release it agreed to. After that, a page that the checkpoint holds
     is read from the volume, and a page that only the source held fails with
     `ErrUnpublishedLost`.
   - This host closes the backing. This has the same meaning from the
     destination's side. A read that the close interrupts is served from the
     volume. Only a read that still needs a page that only the source had fails,
     with the close's cause.

   Closing the backing happens at two different moments. First, a destination
   closes the backing as soon as its post-copy is over. Its guest is running
   and faulting throughout, so a fault can be on the wire when the close
   happens. Every page that fault asked for is already published by then, so
   the read returns from the volume and the guest does not notice. If the fault
   failed instead, it would end the memory session and the guest. Second, the
   orchestrator ends the migration when it has lost the source host. That
   discards the destination's received VM. The pages that only that host had
   are lost with it. Reads waiting for them end with the discard's cause, not
   as lost pages, and the VM is recovered from its checkpoint.

   The destination does not wait for a run that every checkpoint holds, whether
   the source is busy or broken. It reads the run from the volume this time.
   This does not affect the next load, which asks the source again. Two cases
   are neither a gone source nor a stumbling one: a source that serves pages of
   a different size, and a reply this host cannot read. Both mean the
   destination cannot use this source. The fault fails with that cause and the
   received VM is torn, the same way any torn post-copy is handled. The
   resident listing is handled the same way, for the same reason. The caller of
   that listing is the stream, and the destination stops the stream itself.
   Giving up the source there would send a healthy memory region to a volume that does
   not hold the pages no checkpoint has.

   Only pages that the pager installed count as fetched. So `Done` cannot
   report a complete set that the source could then release. Serving a page is
   not the same as holding it. The bytes reach a buffer, and the pager may still
   drop the page. So the destination marks a page as fetched only when its own
   pager reports that it bound the page as dirty state of this memory region.

   The source keeps the same count from its side. When the migration registers
   a memory region, the source records the memory region's unpublished set. It marks off each
   of those pages as it answers for it. It refuses a release while any remain,
   and keeps serving the VM. A page is marked off only after the reply carrying
   it has left this host. A reply that the source could not send carried
   nothing. If the source marked a page off when the bytes were only assembled,
   the release could drop the only copy of the guest's writes. This check does
   not depend on the orchestrator's table. The source answered the fetches, so
   only the source knows. The refusal is a `409`, and the caller retries once
   the destination is done.

   If the host is giving a VM up instead of handing it over, the VM is
   discarded, because its pages are lost in either case. Examples are a fork
   hold that outlived its deadline and a fan-out that failed.

   If the set never completes, the guest consists of this host's half and a
   missing half, and nothing can publish it. `Host.Receive` waits for `Done`
   and discards such a VM instead of returning it:
   - the VMM process is closed;
   - the handle is released through `Handoff`, so nothing dirty is published;
   - the supervisor is told to forget the VM.

   A recovery then opens the checkpoint that the control record already
   selects.

   Only `ErrUnpublishedLost` means a VM is in that state. No other error from
   `Done` is grounds for giving a VM up. The caller's own cancellation only
   stops the wait, because the stream runs on the package's own context and
   `Done` can be called again. `ErrClosed` means this host stopped the stream.
   It means the source may not release yet, not that this guest is torn.

   Each memory region fetches its pages over all of its connections, instead of one
   page per round trip. The held set includes private zero-filled pages
   allocated by [write-ahead](vm-memory.md), even if the guest has not stored
   into them. So peer-page counts measure transferred held pages and can exceed
   the pages the guest explicitly wrote.

Before any memory region has handed off its volume, a failed migration attempts to
resume the source. After a memory region has handed off, that process cannot resume
the VM. The VM can still be opened anywhere, at the checkpoint its control
record selects. Resuming a machine also requires the captured VMM state and
matching memory region configuration.

```go
// vmmemory
// ReadResident copies one resident page for a peer, if this host holds it;
// false when it does not, and separately whether the page it served is this
// host's own state that no checkpoint has. It never loads.
func (r *MemoryRegion) ReadResident(ctx context.Context, page uint64, dst []byte) (held, unpublished bool, err error)
// Resident lists the pages this memory region currently holds, for a bulk stream, and
// Unpublished the subset of them no checkpoint has. A memory region that cannot answer
// reports why: an empty listing is one a destination acts on by fetching
// nothing and reading the volume instead, which for the unpublished set means
// rewinding the guest to the last checkpoint. A source whose volumes cannot say
// what they hold keeps serving them — only Discard gives such a VM up.
func (r *MemoryRegion) Resident() ([]uint64, error)
func (r *MemoryRegion) Unpublished() ([]uint64, error)
// Handoff gives up the volume while keeping the pages, after the guest is
// stopped. Verification stops touching the volume; a flush, a seal, a
// population or any fault reports ErrHandedOff; serving continues until Detach.
// A memory region a checkpoint still has sealed reports ErrSealed and keeps its volume.
func (r *MemoryRegion) Handoff(ctx context.Context) error

// vmmachine
// Backings replaces, by volume name, the backing a memory region attaches with. The
// volume stays the memory region's identity — name, size, writer, page identities,
// every write — and only what the pager loads through changes, which is how a
// destination's memory regions fault from the host that still holds their pages.
// Every name must be a memory region the machine maps, and the backing must be the
// size of the volume it stands in front of; a start refuses anything else
// rather than silently binding a destination to its own volumes.
type Config struct { ...; Backings map[string]vmmemory.Backing }
// MemoryRegions names every memory region by the volume it maps: ram0 and one per PMEM
// device id.
func (p *Process) MemoryRegions() map[string]*vmmemory.MemoryRegion
// Stop pauses the vCPUs, drains device completions and returns the VMM state,
// leaving the process paused. It seals nothing and uploads nothing: the pages
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
// network: a migrated VM's memory regions, or the fork point a fork was taken at. One per
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
// reports the pages the source served out of its own dirty memory so the pager
// holds them privately. Locate is the volume's except for those pages, which it
// reports as bytes of this memory region alone so nothing resolves them against a
// checkpoint lacking them. A page only the source holds is asked for until it
// arrives, the source says it no longer serves the VM, or Close ends this
// memory region's half of the migration.
func NewPeerBacking(config PeerConfig) (*PeerBacking, error)
// Close ends that: every connection dropped and nothing asked of the source
// again. A read it interrupts reads the volume, exactly as one interrupted by
// the source saying it has gone does; one still holding a page only the source
// had ends with the close's own cause. It is what a destination whose post-copy
// is over does, and what discarding a received VM comes to.
func (b *PeerBacking) Close() error
// Migrate runs phases 1 and 2 on the source: stop the process, give the
// memory regions' volumes up, record which of their pages no checkpoint has, release
// the VM without publishing, and serve the pages from there on. It returns the
// VMM state the destination restores.
func Migrate(ctx context.Context, vm *volume.VM, process Runtime, source *PageSource, opts Options) (Handoff, error)
// Receive runs phases 3 and 4 on the destination: open the VM, attach
// memory regions through PeerBacking, start the VMM from the state, and stream the
// resident set in the background. It reports when the stream has finished so
// the source may release.
func Receive(ctx context.Context, manager *volume.Manager, handoff Handoff, dial Dialer, start StartFunc) (*Received, error)
// StartFunc is the supervisor the destination host supplies: one backing per
// memory region, keyed by the volume that memory region maps, and every one of them must be
// what the machine attaches with — vmmachine.Config.Backings for a Firecracker
// supervisor, host.MigrationConfig.StartVM for the host that wires it.
// A start that drops them binds the destination to its own volumes: nothing
// ever faults to the source and the post-copy is a cold read of storage. A
// machine that maps fewer memory regions than the source had is refused and closed.
type StartFunc func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (Runtime, error)
```

The page protocol has two requests over the framed host codec.

A page request names the VM, the volume and a run of pages. The reply carries a
bitmap with one bit per requested page. The payload frame carries only the
pages whose bit is set, in ascending order. A second bitmap marks which of those pages
are the source's own state that no checkpoint has. A clear present bit is not an
error. It means this host does not hold that page, and the destination reads it
from its own volume.

A resident request lists the pages a memory region holds, in runs, bounded per reply. A
destination's bulk stream walks this list and then faults those pages in
through the pager's ordinary load path. Streamed bytes are never written into a
memory region directly, because the load path is what keeps a page shared by identity
with the other VMs on that host.

Both requests are bounded per peer. A peer is the destination host, not one of
its connections. A destination opens a connection per memory region and dials again
whenever one breaks, each time with a new ephemeral port. If each connection
counted separately, neither budget would bind, and the peer table would grow
with every reconnect. A connection over the budget is closed. A request over
the bytes-in-flight budget is answered `BUSY`. The destination then reads the
run from its volume this time. If the run holds a page no checkpoint has, the
destination instead retries with backoff until the source serves it.

For this reason a memory region returns its connections as soon as it has no request
in flight, and keeps one. The connection budget bounds what one destination
host holds at once, across every memory region of every VM it is receiving.
Connections that a memory region pooled for a finished burst count against the memory regions
that are still asking. Those memory regions ask for pages no checkpoint has, and they
keep asking indefinitely. If a memory region kept a whole burst's connections for the
life of its receive, the bound would become a deadlock instead of a queue.

A memory region has four connections by default. The post-copy stream may use at most
three of them, so one is always left for the guest's own faults. Each
connection carries one request at a time, so without this a fault could wait
behind the stream's requests while a vCPU is stopped on it. A memory region with one
connection shares it between the two. The destination records how long each
kind of request took, in total and waiting for a connection, and logs both
when the post-copy finishes.

If the source says it does not serve the VM, the memory region reads from its volume
permanently, and this is logged once. The exception is pages that no checkpoint
has. They have no copy in the volume, so the fault fails. Every other answer is
retried under the rule above.

The order of steps inside the pause is fixed and not symmetric. The memory regions give
up their volumes before the VM is released, because a memory region that kept its
volume across the release would fail its next verification. If a failure
happens before the first memory region's handoff completes, the host attempts to
resume the guest here. Resumption can also fail. After a memory region has handed off
its volume, resumption is no longer possible, and `ErrStopped` reports this:
this process will not run that VM again. The host acts on this error. If a
migration stopped a VM and then failed, the VM is discarded instead of
returning to the checkpoint interval:

- its fork points are retired;
- its pages stop being served;
- its VMM process is closed;
- its handle is released;
- the supervisor is told.

Returning it to the interval would leave a stopped guest whose interval seals
memory regions that no longer own the volumes they map. Its VMM would still be running,
and nothing would close its handle. Reopening the VM advances its epoch even if
the source did not finish releasing it. Resuming the same machine execution
also requires its captured VMM state.

The pause contains the VMM state capture, the memory region handoff and the
destination's open. It uploads nothing. During open, the destination reads the
control record and the selected checkpoint's root from object storage, and no
checkpoint page. The simulated suite asserts that access pattern. The measured
pause is an observation for that workload, not a general latency target.

The host exposes `Host.Migrate(ctx, vmID, destination)` for the drain hook and
`Host.Receive` for the destination daemon. `Host.Drain` runs `Host.Migrate`
over every VM the host holds, four at a time by default. The deployment must
arrange the drain and destination handoffs before exit.
`sproutfsctl migrate VM [--to HOST]` drives a migration through the
orchestrator. When no destination is named, the orchestrator picks the
emptiest other host. See [hosting](hosting.md#draining-a-host).

The orchestrator enforces the other side of the post-copy rule. While a
destination is receiving, the orchestrator checks the host that handed the VM
over, every `SourceWatchInterval`. If that host is lost, the receive ends. This
discards the destination's half-received VM. The VM is then recovered from the
checkpoint its control record still selects, and the in-flight row is removed
with it. The evidence must be positive, because it causes a guest to be torn
down. Two things count as evidence:

- the Kubernetes API no longer lists the pod;
- the host answers but neither runs the VM nor serves its pages, which means
  the host came back without the pages it was holding.

A host that is only quiet may still have a healthy guest, so the destination
keeps waiting for it. A source that is alive but unreachable ends the wait from
its own side. Its handover deadline of four checkpoint intervals makes it give
those pages up, and its next answer then says that it no longer serves the VM.

## Ephemeral disks move with the VM

An [ephemeral disk](volumes.md#ephemeral-disks) is carried like any other
memory region. The guest keeps running across the handoff, and so does its
filesystem on that disk, so dropping the disk would corrupt a live filesystem.
No checkpoint holds any page of it, so the source reports every page it holds
as unpublished. The destination fetches all of them before the source is
released, and maps the disk on its own ephemeral pager. The handoff marks the
memory region `Ephemeral`, and a destination refuses a handoff whose marker
disagrees with the volume it opened. A migration of a VM with a large
ephemeral disk therefore streams that disk and holds the source until it has.

A fork does not carry it. The fork point holds no page of an ephemeral disk, so
the child's disk reads as zeroes wherever it lands.

## A fork is a handoff from a parent that keeps running

A [fork](volumes.md#the-fork-point) uses the same mechanism, but the source
keeps running. There is one fork path, and it is a handoff, whichever host the
child lands on. The child's host changes only how the inherited pages reach it.

Phase 1 is the capture's pause instead of the migration's stop. The vCPUs
pause, the VMM state is saved, the dirty set is sealed and the guest resumes.
Phase 2 gives nothing up. No memory region hands off its volume, the VM handle is not
released, and nothing is published. For a child going to another host, the
source registers the fork point with its page source, under the child's
identity. From the fork point it serves only the pages that no checkpoint of the
parent holds. Everything else is in object storage, and the child reads it from
there. For a child that the parent's own host takes in, nothing is registered,
because that host already has the pages.

Phase 3 creates the child instead of opening the VM. The handoff carries
`Parent` and `ParentCheckpoint`. The destination rebuilds the point from that
pinned checkpoint. The child's control record selects a checkpoint that the
child's own first publication writes. The pin was written on the parent's host
before the handoff was built, by the only writer that holds the parent's epoch.
Nothing releases the pin, because neither the destination nor the parent can
know what else reads that checkpoint. If the destination is the parent's own
host, it skips the rebuild. It already holds the point, so it creates the child
from the point, and the child reads the pages written since the pinned
checkpoint through it.

Phase 4 is the backing, and the backing is the only difference between the two
kinds of destination. On another host it is `PeerBacking`. It marks the pages
the parent served as this host's own. The background stream fetches them first
and to completion, and `Done` reports when the parent may stop serving. On the
parent's own host it is the local backing. Attaching it offers the parent's
sealed pages to the pager under the identity the point gives them. So every
inherited page is present as soon as the memory region attaches, `Done` reports
immediately, no bytes are copied and nothing is dialed.

The destination publishes the child's root index as soon as `Done` reports.
Until then, nothing outside that host can open the child. If the host is lost
before then, the child is lost. Nothing can seal the child, so it can be
neither forked nor migrated. So a fork is not finished until the child has a
root index. Publishing it also releases the hold that the child's own handle
has on the point.

Releasing the parent is `ReleaseMigrated` under the child's identity. Unlike a
migration, it closes no process. It stops serving those pages and retires the
fork point, which returns the sealed pages to the parent's guest. The parent is
not checkpointed while a fork point holds its pages, so the release also lets
its interval checkpoint run again.

The release is not the only thing that ends the hold. A parent that stays sealed
permanently cannot be checkpointed, fenced or migrated, and its dirty set only
grows. So every hold has a deadline of four checkpoint intervals, wherever its
child went. When the deadline passes, the host stops holding the point for that
child and retires the point. The child falls back to the checkpoint its record
already selects. The orchestrator reconciles from the other side. Every survey
compares each host's `Serving` set with the orchestrator's table and releases
every entry that has no operation in flight. So if the orchestrator restarted
between the handoff and the release, it makes the release on its first survey.
A migration or fork that is still running is left alone. The `Serving` set
includes every handover the host holds, including a child taken in on its
parent's own host. Such a child is served nothing over the wire, but its hold on
the point keeps the parent sealed. If the host reported that child as holding
nothing, the deployment would see a parent that cannot be checkpointed as a
parent that nothing is waiting on.

A handover is given up instead of released when no host runs its VM and the
table has no record of it. Such a VM is the child of a fan-out that failed.
Nothing holds the pages the source kept for it, and nothing ever will. A release
of those pages is the one request the source must refuse, because they are the
only copy of the parent's writes since its last checkpoint.
`POST /vms/{id}/abandoned` is the request that gives them up, and it refuses
nothing.

One pause serves any number of children. Each child is one hold on the point,
and the seal ends when the last hold is retired. So a fan-out of forks costs the
parent one pause and one request. A second pause is refused while one is
outstanding, because a memory region can have only one seal at a time.

Deleting the parent ends every hold on it in the same way. `Host.Delete` retires
the points taken on that VM before it closes the process that holds their
pages. A child holds a point in this host's memory, not an object. If the
parent were deleted while a child held a point, the page server would offer a
point whose pages are gone. Every page the child had not yet fetched would come
back absent and be read from the checkpoint instead. Because the point is
retired first, the child's next fault for one of those pages fails and reports
the failure. If the VM is still sealed after that, the delete is refused, as a
migration of a sealed VM is, and the VM keeps running. In that case the holder
is not one of this host's own holds. It is one of the following:

- a child created here whose first checkpoint has not published yet, which
  holds the point through its own handle;
- a capture in flight, which holds the point through the publication.

Deleting the VM would remove the point from under that holder. A delete does
not touch the parent's checkpoint objects that a pin covers, because a child
still inherits them. The collector reclaims what a deleted VM left pinned.

A fork on the parent's own host skips the network entirely, and it uses the
same call. `Host.Fork` builds a handoff for every child and holds the point for
each child. A child with no named destination is served nothing, because the
host that takes it in already holds the pages. The orchestrator drives both
halves for every child. The command is
`sproutfsctl fork VM [--count N] [--to HOST]`, and the destination defaults to
the parent's host. The orchestrator gives each handoff to its destination's
`Receive` and then tells the parent's host to release that child. Every fork
goes through the orchestrator. Before the parent is paused, the orchestrator
allocates each child's identity, records the child's host and parent, and
admits the whole fan-out against the host that will take it in. A host never
forks on its own.

A fan-out that did not happen leaves nothing behind, wherever the children were
going. One rollback covers every case:

- The parent's host gives up every hold of a fan-out that it could not hand
  over completely.
- The orchestrator gives up every child of a fan-out whose destination refused.
  It deletes whichever children exist under the identities it named. Deleting
  an identity that was never created removes nothing.
- A child that was taken in is released, because it holds every page it
  inherited, which is what a release states.
- A child that never started is given up on the source instead, because
  nothing fetched the pages its hold keeps, and nothing ever will.

The children of a failed request are guests that nobody asked for. They hold a
host's memory under names that only that request knew.

```go
// volume
// ForkPoint seals the guest and returns the point a child starts from,
// publishing nothing and giving nothing up. The parent stays sealed until the
// last child has retired the point.
func (vm *VM) ForkPoint(ctx context.Context, prepare PrepareFunc) (*ForkPoint, error)
// Inherit rebuilds that point on a host that never held the parent, from the
// checkpoint the parent pinned.
func (m *Manager) Inherit(ctx context.Context, parent control.Ref) (*ForkPoint, error)
func (m *Manager) Fork(ctx context.Context, id string, point *ForkPoint) (*VM, error)

// Share offers the parent's sealed pages to this host under the identity the
// point gives them, which is the local backing's attach.
func (f *ForkPoint) Share(ctx context.Context) error

// vmmigrate
// Pages is what a page source serves one volume out of: a migrated VM's memory region,
// or a fork point. MemoryRegionPages and ForkPages adapt each.
type Pages interface { ... }
// Fork registers a fork point under the child's identity and describes it. The
// pause already happened; nothing is stopped and nothing is released. A nil
// source is a child the parent's own host takes in: it is served nothing.
func Fork(ctx context.Context, child string, point *volume.ForkPoint, source *PageSource, opts Options) (Handoff, error)
// Options.Point is the fork point a child whose parent runs here is received over.
// Receive creates the child from it and binds its memory regions to a local backing.

// host
// Fork hands every child of one fork point over, to another host or to this one.
func (h *Host) Fork(ctx context.Context, parent string, children []string, destination platform.Address) ([]vmmigrate.Handoff, error)
// Receive takes a handoff: a migrated VM, or a fork's child. It publishes a
// child's root index as soon as that child holds every page its parent had.
func (h *Host) Receive(ctx context.Context, handoff vmmigrate.Handoff) (*vmmigrate.Received, error)
```

### A post-copy child's own published pages

A post-copy child was once told that its own published pages had no object. The defect killed a child's guest a second or so after it resumed. Usually the crash was in the kernel's timer wheel: `__run_timers` on a node whose `pprev` was `dead000000000122`, the poison value that `hlist_del` leaves. Otherwise the guest crashed in `rb_erase`, `profile_tick` or `process_one_work`, jumped to a wild address, or hung silently. All of these were one defect: the guest read an older version of a page it had written.

A destination reports no identity for the pages its handoff named. So the pager loads those pages through the peer backing, where the source answers. It does not resolve them against a checkpoint, because no checkpoint holds them. That set was fixed for the backing's lifetime. So the backing kept reporting no identity for those pages after the child's own checkpoint had published them. The pager trusted the answer. A retire gives up a page when the volume holds no object for it, because a publication writes an all-zero page as a sparse hole, and the volume can reproduce such a page without an object. Here the answer was wrong. The retire revoked the guest's mapping and released the only copy of bytes the guest had written. The guest's next read of that page returned the fork point's version. The failure rate scaled with the page count: the fan-out fixture's handoff set is about 8300 pages at 4 KiB, against about 16 at 2 MiB. That is why the defect appeared with the page-geometry plan's fourth step, although that step did not cause it.

`PeerBacking.Locate` now removes a page from that set only while the checkpoint the volume names for the page predates the handoff. That covers another VM's checkpoint, and this VM's own checkpoints up to the sequence the handoff selected. Both conditions are required, and each has a test that fails without it. Both are needed because a migration keeps the same VM, and its unpublished pages are written after its own last checkpoint.

Three safeguards were deliberately left in place:
- `vmmemory.ErrUndroppable` refuses the retire instead of trusting the answer. A page that is given up because the volume holds no object for it must be a page the volume can reproduce without an object, so it must be zeros. Any other page fails the retire. The checkpoint stays durable, the page stays sealed, and the guest keeps its memory.
- `volume.ErrRetired` refuses a hold on a fork point that its last holder retired. It caught a second defect when it was added. Forking two children through the manager one after the other, with each child's hold released as the child closed, took the second child from a fork point whose seal had ended.
- `TestFirecrackerForkChildrenSurviveTheirFirstSeconds` reproduces the whole failure in about a hundred seconds per run instead of ten minutes. It keeps the arms that isolated the defect (`SPROUTFS_FORK_ARM`: the whole checkpoint, the capture without the settle, the bare pause, one child, no interval). The sequence of runs that found the defect was 0/8 with no interval, 0/8 for a bare pause, 0/8 for a capture and seal, 5–8/8 for the whole checkpoint, and 0/16 after the fix.

## Qualification

The simulated suite runs two volume managers and two pagers over one simulated
world. Its guest stores continuously through a simulated mapping. The suite
requires that:

- the destination's byte model equals the source's at the stop;
- the pause reads and writes no object of the volumes;
- every page the source held is served by the source and never requested again;
- every page that no checkpoint had is fetched before `Done` returns, and is
  published by the destination's own next checkpoint;
- `Done` does not return while one of those pages is still only on the source.

A failure in the stop must leave the guest running here with its memory intact.
If the source is lost before the destination fetched its unpublished pages, the
VM must remain openable at the checkpoint its control record still selects,
rewound by exactly the writes since that checkpoint. If the destination's
supervisor starts a machine that does not map every memory region the source had, the
destination is refused before that machine runs, and the machine is closed. The
page protocol has its own tests:

- a partially resident memory region is answered in one request;
- an unserved volume stops the retries on the one answer that says so;
- an unreachable source costs each load one round trip and no more for the
  pages a checkpoint holds;
- a connection over the per-peer budget is refused;
- a busy source is not mistaken for a gone one.

Failure during post-copy has four requirements, tested under the simulated
clock and network:

- A source that resets twenty connections in a row still serves the page
  afterwards, and the fault completes.
- A source that is silent for ten simulated minutes leaves the fault waiting,
  not failed. When the source answers, the fault completes with the right
  bytes.
- A source that answers that it no longer serves the VM immediately fails a
  fault for a page only it held, with `ErrUnpublishedLost`. The pages a
  checkpoint holds are then served from the volume.
- Discarding the received VM ends a waiting fault with the cancellation's cause
  and nothing else. An unreachable source ends the same way: `Done` waits, and
  the discard ends the wait.

The suite also tests two more cases. A busy source is retried until it serves
the pages no checkpoint holds, and the destination ends with the source's bytes,
not the checkpoint's. `Done` returns while the bulk resident stream is still
running.

The deployment's half is tested in the simulated deployment. The host that
handed a VM over is lost while its destination is in the post-copy. The
handover ends at the moment of the loss, not when some timeout expires. The VM
comes back on the remaining host at the checkpoint its record selects.

The host suite runs the same migration between two hosts over loopback TCP,
including a drain that moves every VM one host runs.

Forks are qualified on the same model. A fork on the parent's own host must
receive its child over the pages the seal froze. Every inherited page is mapped
by identity, no page is loaded back, and no connection is dialed. The children
of one fork point must map each other's pages, not their own copies. A fork
onto another host must name and pull only the pages that no checkpoint of the
parent holds. Neither side may see the other's later stores. In both cases the
child's root is published when the child holds those pages, not at the next
interval checkpoint. With the interval loop off on every host, a third host
opens the child as soon as its handoff is done. If the parent's host is lost
after that, the child can still be opened anywhere, and it reads back the point
it was forked at. Before the child has published, opening it anywhere reports
`ErrForkPending`. A parent that is deleted or left unreleased under a child is
handled the same way wherever the child is:

- the delete retires the holds this host has;
- a parent that something else holds sealed is refused;
- a hold that nothing releases is given up at its deadline.

The full-guest suite migrates a real Firecracker guest between two pagers and
two managers in one process, over loopback TCP. The guest stores into its RAM
and its DAX disk until the vCPUs stop. Every memory region of the destination is
started through a `PeerBacking`, so the source's page server is on the VMM's own
fault path, not beside it. The guest comes back with its counters intact, read
back through the console. `PeerStats` shows that the pages those faults touched
came from the source. A fault on a page that the guest has not reached and that
only the source holds is also served by the source. The suite compares what the
source serves byte for byte against what the destination's own checkpoint
holds. After `Release`, the same fault path reads the volume, the peer count
stops increasing, and the memory region never asks that source again. The suite
reports:

- the stop-to-resume pause;
- how many pages of each memory region the handoff named as unpublished;
- the peer and volume page counts of every memory region.

### Page payload compression

Page requests and replies require payload format 1. Each successful reply
carries one independent raw or Zstandard blob. The blob contains the present
pages in bitmap order, normally one 2 MiB page. CRC32C covers transmitted
bytes. The blob checks its decoded length and contents before any bytes reach
guest memory. Source admission still charges full logical page bytes. Old page
protocols are rejected. The control plane's `Handoff.State` remains the
runtime's raw state. Checkpoint VMM-state objects are compressed.
