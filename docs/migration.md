# Live migration

A VM moves between hosts in two steps. The source host stops the VM. The
destination host then runs it while its pages arrive from the source's pager.
Migration is post-copy only, and nothing is uploaded during the pause. The pause
covers the VMM state capture, the memory region handoff and the destination's
open. A planned host restart or scale-down migrates every VM on the host: the
drain.

Guest RAM and PMEM writes are private pager state, resident or in scratch
spill, until a [checkpoint](vm-memory.md) publishes them. The source hands the
VM over at the checkpoint its control record already selects, and the
destination fetches everything written since from the source's pages. If the
source dies during the post-copy, the guest loses its writes since the source's
last interval checkpoint, as with any host loss: up to one 60 s interval. So the source keeps serving
until the destination reports that it has fetched every one of those pages.

## Phases

0. **Quiesce the checkpoint loop.** The host's interval checkpoint stops first,
   waiting for a checkpoint in flight to finish publishing. That checkpoint owns
   the guest's sealed memory regions, and a handoff that found one sealed would
   have to abandon the migration and resume the guest.
1. **Stop.** The VMM pauses the vCPUs, drains device completions and captures
   the VMM state. Nothing is sealed or uploaded.
2. **Hand off.** Every memory region gives up its volume but keeps its pages,
   and reports which of its pages no checkpoint has. The guest is stopped, so
   that set is final. The source releases the VM handle without publishing. The
   handoff carries:
   - the VMM state;
   - the memory region layout;
   - the unpublished page runs;
   - the address of the source's peer server;
   - the sequence the source's control record selected when it gave up the VM;
   - whether the VM is marked to [pull its memory](hosting.md#pulling-a-vms-memory).
     A marked VM's destination pulls the checkpoint it opens.
3. **Resume on the destination.** The destination opens the VM: it reads the
   control record, advances its epoch, which fences the source, and reads the
   selected root. If the record selects any sequence other than the one the
   handoff names, the open fails with `ErrStale` and the VM is released again
   without publishing. A migration publishes nothing, so a recovery or an
   operator could have opened the record in between, and streaming the source's
   pages over another writer's would silently mix two writers' pages. If the
   sequence matches, the destination binds each memory region to the source
   host and to its own volume, and starts the VMM with the captured state.
4. **Post-copy.** A page the guest touches faults in from the source's pager
   first, over the [peer server](#the-peer-server) at the handoff's address. If
   the source cannot supply it, the page comes from the destination's
   checkpoint. A page the destination has published since the handoff comes
   from its volume without asking, because the source holds at best the version
   before it. A page the source served from its dirty pages is dirty on the
   destination too: the peer backing reports it as its own bytes, the pager
   holds it as a private page under a spill reservation, and the destination's
   next interval checkpoint publishes it. Reading it from the volume would
   rewind the guest.

   A background stream fetches all the unpublished pages, then the rest of the
   source's resident set. `Done` returns when every unpublished page has
   arrived or failed to, which allows `ReleaseMigrated` and the source host's
   exit. The bulk pass continues after that, and `Streamed` reports when it has
   finished.

   **Failure during post-copy.** The destination's volume can never supply an
   unpublished page, because its checkpoint predates the guest's write, and the
   destination cannot tell a slow source from a dead one. So it retries, with
   backoff and no attempt limit, every `BUSY` reply, reset connection, timeout,
   restarting listener, dead connection and peer marked down. Two things stop
   the retries:
   - The source answers that it no longer serves this VM, which it does only
     after a release it agreed to. A page the checkpoint holds is then read from
     the volume, and a page only the source held fails with
     `ErrUnpublishedLost`.
   - This host closes the backing. A read the close interrupts is served from
     the volume. A read that still needs a page only the source had fails with
     the close's cause.

   A destination closes the backing as soon as its post-copy is over. A fault
   on the wire at that moment asks only for pages already published, so it is
   served from the volume and the guest does not notice. The orchestrator also
   ends the migration when it has lost the source host. That discards the
   destination's received VM, whose waiting reads end with the discard's cause,
   and the VM is recovered from its checkpoint.

   The destination does not wait for a run that every checkpoint holds. It
   reads the run from the volume this time and asks the source again on the
   next load. A source that serves pages of another size, or a reply this host
   cannot read, means this source is unusable: the fault fails with that cause
   and the received VM is torn down. The resident listing is handled the same
   way.

   Only pages the destination's pager has bound as dirty state of the memory
   region count as fetched, because a page that reached a buffer may still be
   dropped. So `Done` cannot report a complete set that the source could then
   release.

   The source keeps the same count. When the migration registers a memory
   region, the source records its unpublished set and marks each page off once
   the reply carrying it has left this host. It refuses a release (`409`) while
   any remain and keeps serving; the caller retries once the destination is
   done. This check does not depend on the orchestrator's table.

   A host that gives a VM up instead of handing it over, such as a fork hold
   past its deadline or a failed fan-out, discards it, because its pages are
   lost either way.

   If the set never completes, the guest is half on this host and half missing,
   and nothing can publish it. `Host.Receive` waits for `Done` and discards such
   a VM: it closes the VMM process, releases the handle through `Handoff` so
   nothing dirty is published, and tells the supervisor to forget the VM. The
   handoff is still good while the source holds the pages, so the orchestrator
   tries the receive again
   ([a failed receive is tried again](#a-failed-receive-is-tried-again)). Only
   when no destination takes the VM in that time does a recovery open the
   selected checkpoint.

   Only `ErrUnpublishedLost` means a VM is torn. The caller's cancellation only
   stops the wait, because the stream runs on the package's own context and
   `Done` can be called again. `ErrClosed` means this host stopped the stream,
   so the source may not release yet.

   Each memory region fetches over all of its connections. The held set
   includes private zero-filled pages allocated by
   [write-ahead](vm-memory.md), so peer-page counts can exceed the pages the
   guest wrote.

Before any memory region has handed off its volume, a failed migration tries to
resume the source. After one has, this process cannot resume the VM, but it can
be opened anywhere at the checkpoint its record selects.

The main calls:

| Call | Role |
| --- | --- |
| `vmmemory.MemoryRegion.ReadResident`, `Resident`, `Unpublished` | Serve one resident page to a peer, and list the held and unpublished pages. A region that cannot answer says why, because an empty listing makes a destination read the volume and rewind the guest. |
| `vmmemory.MemoryRegion.Handoff` | Give up the volume and keep the pages. A region a checkpoint still has sealed reports `ErrSealed`. |
| `vmmachine.Config.Backings`, `Process.Stop` | Bind each memory region to a backing by volume name, and stop the VMM without sealing or uploading. |
| `volume.VM.Handoff` | Release the handle without publishing. |
| `peer.Server`, `peer.Table` | The peer server, and this host's view of every other host. `Server.Release` refuses while any page is outstanding; `Server.Discard` gives a VM up. |
| `vmmigrate.PeerBacking` | A pager backing that asks the source first and reads the volume for pages a checkpoint holds. `Close` ends this memory region's half of the migration. |
| `vmmigrate.Migrate`, `vmmigrate.Receive` | Phases 1 and 2 on the source, and 3 and 4 on the destination. The destination's `StartFunc` must attach every backing it is given, or the post-copy becomes a cold read of storage. A machine that maps fewer memory regions than the source had is refused and closed. |

Pages cross the [peer server](#the-peer-server). A page request names the VM,
the volume and a run of pages. The reply has a bitmap with one bit per
requested page, a payload with the set pages in ascending order, and a second
bitmap of the pages that are the source's own state that no checkpoint has. A
clear bit means this host does not hold the page, and the destination reads it
from its volume.

A resident request lists the pages a memory region holds, in runs, bounded per
reply. The bulk stream walks this list and faults those pages in through the
pager's ordinary load path, never writing into a memory region directly,
because the load path keeps a page shared by identity with the host's other
VMs.

A memory region's stream keeps four faults in flight. The faults take turns in
page order until each read is under way (its first request is on the wire, or
its read of the volume has begun), so each fault's arena slot, evicted page and
place on the link are decided in page order. Without the turn, the Go scheduler
chose that order, and a simulation seed could not reproduce which page a later
store evicted. The round trip, decoding and install still overlap. On the GCE
links of 2026-10-03, one page took about 28 ms, mostly the page codec
([measurement](measurements/gce-peer-server-2026-10-03.md)). The turn covers
only in-memory work and waits the next request would make anyway, so four
requests stay in flight; this has not been measured on GCE. Where the arena is
full and each slot's eviction spills a private page, those evictions now run
one after another. A guest's own faults take no turn.

A guest's fault reads its own page first and prefetches the rest of its run
behind it ([faults and read-ahead](vm-memory.md#faults-and-read-ahead)). A
stream's fault reads its whole run at once (`vmmemory.WithStream`). A prefetch
never takes a page the extents name as the source's alone, and drops a page the
source says it still holds: only a fault may take such a page as the
destination's dirty state and tell the backing.

A guest's fault is the fault class; the stream and prefetches are bulk reads.
They use different connections and budgets at the source, so a fault never
waits behind the stream. A request over its class's budget is answered `BUSY`.
For a run every checkpoint holds, the destination then reads the volume; for a
run with an unpublished page, it asks again with backoff. The destination logs,
when the post-copy finishes, how long each kind of request took in total and
waiting for room.

If the source says it does not serve the VM, the memory region reads from its
volume from then on, and this is logged once. A page no checkpoint has then
fails.

Inside the pause, the memory regions give up their volumes before the VM is
released, because a region that kept its volume across the release would fail
its next verification. A failure before the first region's handoff completes
makes the host try to resume the guest, which can also fail. After a region has
handed off, `ErrStopped` reports that this process will not run the VM again,
and the host discards the VM instead of returning it to the interval: its fork
points are retired, its pages stop being served, its VMM process is closed, its
handle is released, and the supervisor is told. An interval over it would seal
memory regions that no longer own their volumes.

During open, the destination reads the control record and the selected root,
and no checkpoint page. The simulated suite asserts that access pattern. The
measured pause is an observation for that workload, not a latency target.

`Host.Migrate(ctx, vmID, destination)` serves the drain hook and `Host.Receive`
the destination. `Host.Drain` runs `Host.Migrate` over every VM on the host,
four at a time by default. The deployment must arrange the drain before exit.
`sproutfsctl migrate VM [--to HOST]` drives a migration through the
orchestrator, which picks the emptiest other host when none is named. See
[hosting](hosting.md#draining-a-host).

While a destination receives, the orchestrator checks the source every
`SourceWatchInterval`. If the source no longer has the pages, the receive ends,
the destination's half-received VM is discarded, the VM is recovered from the
checkpoint its record selects, and the in-flight row is removed. Ending a
receive tears a guest down, so the evidence must be positive:

- the Kubernetes API no longer lists the pod;
- the host answers but neither runs the VM nor serves its pages;
- the source's hold is over.

A host that is only quiet may still have a healthy guest, so the destination
waits for it while the hold lasts. The source reports its hold with the handoff
(`hold_seconds`), four checkpoint intervals by default. It armed that deadline
before it answered, so the orchestrator counts from when the handoff arrived,
and the source's deadline has passed when the count ends. After that the pages
are gone, whether the source is alive or not. This assumes the two clocks run
at nearly the same rate; a drift of a few parts per million is a few
milliseconds over four minutes. A source whose process stalled past its
deadline may serve briefly after it, but no destination is asked to take the
handoff then. A source that reports no hold promises nothing, so its silence is
never evidence.

The recovery that follows accepts the source's silence, because the source gave
its volumes up before any destination was asked, and only an open by the
orchestrator could run the VM there again. Any other host that does not answer
still refuses the recovery.

Nothing else the Kubernetes API reports is used. A `NotReady` node or a pod
being deleted says nothing about whether the process serves pages, and a
container's restart count arrives too late to tie to the process that handed
the VM over. The rule is `handover.Hold.Gone` in `internal/handover`, which the
receive in flight and the retries below both apply.

A fork's child is received under the same watch, wherever it lands. The
parent's host reports its hold with the fork (`hold_seconds`), and `Serving`
lists the child until it is released. If the parent's host is cut off while its
pod is still listed, the child's receive ends at the hold, its destination
gives up what it received, the fork fails, and the parent keeps running.

## The peer server

Every host runs one peer server, on one port. It is the only channel between
hosts. It carries the pages a handoff left on a host, and the disk cache's
stripe requests. The code is in `peer`. It was the page server in `vmmigrate`,
and its metrics keep the page server's names. The design follows FoundationDB
and CockroachDB
([FoundationDB notes](research/foundationdb-transport-2026-10-03.md),
[CockroachDB notes](research/cockroachdb-rpc-2026-10-03.md)).

### Frames

The protocol is framed over TCP. A frame is a 20-byte prefix (a magic number,
the prefix's version, and the header and payload lengths), a protobuf header
and a payload, at most 16 MiB in all.

A frame leaves in one vectored write. A payload that is a range of a file goes
behind it with `sendfile` on Linux. The receiver reads a payload into a pooled
buffer of the length the prefix gave.

A header of version 2 ends with a CRC32C of the rest of it. A header that fails
it, or a prefix whose payload length disagrees with a header that passed, is
damage: the connection closes and the caller asks again. A page reply's payload
carries its own CRC32C. A stripe's carries none, because each stripe has its own
checksum.

### Versions

A dialer opens every connection with a hello naming the oldest and newest
version it speaks and the connection's class. The server answers with the
newest version both speak, the class's budget, and how many requests the
connection may have in flight. A dialer with no common version is answered
`INCOMPATIBLE` with the server's range, and the connection closes. That peer is
not marked down.

This release speaks versions 1 and 2. Version 1 is the release before: no
hello, one request at a time per connection, and no header checksum. Its server
closes a connection whose first frame is a hello, and the dialer then dials
again without one. This release's server reads a first frame that is not a
hello as a version 1 request. Tests run this release against a frozen copy of
the release before, both ways, and a whole migration from it.

### Requests

Version 2 carries several requests on a connection at once. Each has an id that
its reply names. The server reads the next request while earlier ones are
answered, but replies leave in request order. A request that must not wait
behind another goes on another connection.

The requests are:

- pages, resident and claim, for handoffs;
- ping, answered at once;
- read, keep, drop, presence and probe of stripes, for the disk cache. A read
  of stripes that wants no bytes asks only for a window's fill right.

A stripe request names the cache it expects. A server whose cache is another
answers `NOT_ME`, as a host that took over a reused address would. The server
hands stripe requests to a `peer.Cache`, which the checkpoint cache implements.

### Peers and classes

Each host keeps one table of peers, one per remote host. A peer has a pool of
connections per class: two for faults, two for bulk reads, one for bulk writes
and two for stripe reads. Replies leave a connection in request order, so a
stripe on a fault connection waited behind every 2 MiB page ahead of it, 27 ms
at p99 on [GCE](measurements/gce-peer-server-2026-10-03.md); stripes therefore
have their own class. A server of the release before reads the stripe class as
faults.

The server counts each class of each remote host against its own budget: 8 MiB
for faults, and 16 MiB each for bulk reads, bulk writes and stripe reads. A host
is its address without the port, so all its connections share its budgets. A
request that would take its class past the budget is answered `BUSY`, with
what the class holds, may hold, and asked for, and the connection stays open.
The dialer knows each budget from the hello and waits for room before it sends,
so `BUSY` covers what it could not see, such as another process on its host. A
server also has a serving bandwidth for stripes
(`ServerConfig.StripeBytesPerSecond`); a stripe read past it is answered
`BUSY`, and the reader asks another holder.

### Liveness

A connection that hears nothing for a second is pinged. One that hears nothing
for four seconds is dead: it is closed and its peer is marked down. A slow reply
is heard as its bytes come. TCP keepalive and, on Linux, a `TCP_USER_TIMEOUT` of
ten seconds are a second line. A connection idle for thirty seconds is closed
without marking anything; a server does the same to a version 2 connection
that sends it nothing for thirty seconds.

A peer is marked down only by a hard failure: a failed dial or hello, a connect
that took more than three seconds, or a dead connection. A caller giving up, or
a slow request whose connection still answers pings, is not one. A reader of
the cluster's cache keeps its own marks beside these
([hosts marked down](hosting.md#reading-from-the-cluster)). A request that can
do without a down peer skips it: a page a checkpoint holds, a stripe another
rank holds. The table probes a down peer about a second after the mark, then
half as long again each time, up to every ten seconds.

The release before answers no ping. A version 1 connection is dead when it has
owed a reply and heard nothing for thirty-four seconds: that release's
thirty-second request timeout, and four more.

### The background budget

A link carries bytes in order, so a fault's reply waits behind every stream byte
sent before it. Each host bounds its bulk work at all its peers by one
background budget, 16 MiB by default. A bulk read waits for room before it is
sent. Unpublished pages go first, then the rest of the stream, then fills and
repairs of the disk cache, which are dropped, never queued, when there is no
room. A repair has half the budget. While a guest fault waits on any peer, or a
stripe read for one, the budget shrinks to a quarter. A host also bounds the
stripe bytes its reads have in flight, 64 MiB by default.

The [GCE run](measurements/gce-peer-server-2026-10-03.md) measures a fault's
latency while a post-copy stream fills the link.

## A failed receive is tried again

The source gives its volumes up before any destination is asked to take the VM,
so a failed receive cannot resume the guest where it was. It also changes
nothing the handoff rests on: the destination published nothing, the control
record still selects the handoff's checkpoint, and the source still serves the
unpublished pages. So the orchestrator tries the receive again for as long as
the source holds those pages.

The policy is `handover.Default` in `internal/handover`:

- The first retry waits one second, and each later wait doubles, up to fifteen
  seconds.
- One destination gets two attempts in a row. The next goes to the host with
  room that has failed least. The same host is tried again only when no other
  has room. A named destination is where the handover starts, not where it must
  end.
- The retries stop when the source's hold is over; the last wait ends with the
  hold. A source that reports no hold is tried once.

Before each retry, the orchestrator surveys the hosts and acts only on positive
evidence:

- A host that runs the VM ends the handover there. A receive whose answer was
  lost may have taken the VM, and a receive elsewhere would fence that guest.
- A source that no longer has the pages has taken the handoff with it. The VM
  is recovered from its checkpoint. The look at the end of the hold always
  finds this.
- The destination that failed must answer before another host is tried,
  because a quiet one may still be finishing the receive.
- A host that reports a receive of the VM in flight holds every other receive
  back, on any host. That receive either takes the VM, which the first rule then
  finds, or ends, and the retries go on.

A host reports its receives in flight in `Status` (`receiving`), from admission
until it has taken the VM in or given it up. The orchestrator waits while the
host says the receive is going on, and no longer than the source's hold.

A source that reported no hold gets one look after its one receive. If that
look shows nothing, the VM is left stopped and the failure reported.

The epoch keeps two destinations from both holding the VM: each open fences the
one before, and a destination whose record selects another checkpoint than the
handoff names refuses it with `ErrStale`. A host admits one receive of a VM at a
time, and the orchestrator sends none elsewhere while one is reported in
flight. A receive still in flight when the source's pages are gone waits for
pages nobody has; the recovery that follows opens the VM elsewhere and fences
it, so only one can publish.

Once the source has stopped the guest, the handover no longer depends on the
request that started it. A drain's request gives up after 60 seconds, but the
orchestrator retries until a destination takes the VM or the hold is over, and
the drain waits for `Serving` to be empty. A restarted orchestrator takes the
handover up again with the handoff the source keeps.

## Ephemeral disks move with the VM

An [ephemeral disk](volumes.md#ephemeral-disks) is carried like any other
memory region, because the guest's filesystem on it keeps running. The source
reports every page it holds as unpublished, and the destination fetches all of
them before the source is released and maps the disk on its own ephemeral
pager. The handoff marks the memory region `Ephemeral`, and a destination
refuses a handoff whose marker disagrees with the volume it opened. A large
ephemeral disk is streamed whole and holds the source until it is. A fork does
not carry it.

## A fork is a handoff from a parent that keeps running

A [fork](volumes.md#the-fork-point) uses the same mechanism, but the source
keeps running. There is one fork path, a handoff, wherever the child lands. The
child's host changes only how the inherited pages reach it.

- **Phase 1** is the capture's pause instead of the stop: the vCPUs pause, the
  VMM state is saved, the dirty set is sealed and the guest resumes.
- **Phase 2** gives nothing up and publishes nothing. For a child going to
  another host, the source registers the fork point with its page source under
  the child's identity, and serves only the pages no checkpoint of the parent
  holds. For a child the parent's own host takes in, nothing is registered.
- **Phase 3** creates the child. The handoff carries `Parent` and
  `ParentCheckpoint`, and the destination rebuilds the point from that pinned
  checkpoint. The pin was written on the parent's host, by the holder of the
  parent's epoch, before the handoff was built. The parent's own host skips the
  rebuild and creates the child from the point it holds.
- **Phase 4** is the backing. On another host it is `PeerBacking`, whose stream
  fetches the parent's served pages first, and `Done` reports when the parent
  may stop serving. On the parent's host it is the local backing: attaching it
  offers the parent's sealed pages to the pager under the point's identity, so
  every inherited page is present at once, `Done` reports immediately, and
  nothing is copied or dialed.

The destination publishes the child's root as soon as `Done` reports, behind the
running child (`Host.rootBehind`), so the fork returns without waiting. A root
the store refuses is tried again, from a quarter of a second doubling to
thirty. Until it lands, nothing outside that host can open the child, and the
host reports it (`VM.RootPending`). If the host is lost before then, the child
is lost, and its record selects a root that was never published. The
orchestrator frees that identity when a recovery or start finds the child
running nowhere, with the same evidence a recovery needs, and reports it gone
(HTTP 410). Meanwhile the child can be neither forked nor migrated
(`ErrForkPending`). Publishing the root releases the child's own hold on the
point. A child on the parent's host waits for the parent's publication of the
point, so its root uploads nothing it inherited.

Releasing the parent is `ReleaseMigrated` under the child's identity. It closes
no process. It stops serving those pages and retires the fork point, which
returns the sealed pages to the parent and lets its interval checkpoint run
again.

A parent sealed permanently could not be checkpointed, fenced or migrated, and
its dirty set would only grow. So every hold has a deadline of four checkpoint
intervals. When it passes, the host retires the point for that child, and the
child falls back to the checkpoint its record selects. The orchestrator
reconciles from the other side: every survey compares each host's `Serving` set
with its table and releases every entry with no operation in flight, so a
release missed by a restart happens on the first survey.

The table believes a row in flight for two minutes, which bounds how long a row
outlives the orchestrator that wrote it. A fan-out receives its children one
after another, so the fork rewrites each child's row every thirty seconds until
that child's receive has finished or the fork has failed. A migration rewrites
its row on each look at its source while the receive runs.

A survey releases a migrated VM's source only once a host runs the VM. A
handover that no host runs or receives, with no operation in flight, was
started by an orchestrator that restarted. The source keeps the handoff while
it holds the pages and hands it out again (`GET /vms/{id}/handoff`), and the
survey carries the VM over under the same retries and hold.

A survey leaves a handover alone while any host reports a receive of its VM in
flight, however old the row. The source's count of what it still owes is no
evidence that the destination has the pages: a reply can be lost after the
source struck its page off, and a receive discarded after the source sent
everything leaves the count at zero. `spec/postcopy` found this; see
[spec/bugs.md](../spec/bugs.md). The `Serving` set includes every handover the
host holds, including a child taken in on its parent's host, whose hold keeps
the parent sealed.

That local hold works like a served one. Until the host takes the child in, the
hold's outstanding count is every page the point holds for it, and the host
refuses its release. Once the child is taken in, it maps every page it
inherited, the count is zero, and the release is accepted.

A handover is given up instead of released when its VM does not exist and
nothing is creating it: no host runs it or reports a receive of it, no
operation is in flight for it, and the table has no row for it or the bucket
has no control record of it. Such a VM is the child of a fork that failed or
was never taken in, for example because the orchestrator restarted between the
handoff and the receive. A migrated VM always has a record, so a migration is
never given up this way. A receive in flight counts even when the fork that
sent it has died. The source must refuse a release of such pages, which are the
only copy of the parent's writes, so `POST /vms/{id}/abandoned` gives them up
and refuses nothing.

One pause serves any number of children, each one hold on the point, so a
fan-out costs the parent one pause and one request. A second pause is refused
while one is outstanding.

Deleting the parent ends every hold on it. `Host.Delete` retires the points
taken on the VM before it closes the process that holds their pages, so a
child's next fault for a page it had not fetched fails and reports it, rather
than reading the checkpoint. If the VM is still sealed after that, the delete is
refused and the VM keeps running. The holder is then a child created here whose
first checkpoint has not published, or a capture in flight. A delete does not
touch the parent's checkpoints a pin covers.

`Host.Fork` builds a handoff for every child and holds the point for each. The
orchestrator drives both halves: `sproutfsctl fork VM [--count N] [--to HOST]`,
with the parent's host as the default destination. It gives each handoff to its
destination's `Receive` and then tells the parent's host to release that child.
Before the parent is paused, the orchestrator allocates each child's identity,
records its host and parent, and admits the whole fan-out against the
destination. A host never forks on its own.

A fan-out that did not happen leaves nothing behind. One rollback covers every
case:

- The parent's host gives up every hold of a fan-out it could not hand over
  completely.
- The orchestrator gives up every child of a fan-out whose destination refused,
  and deletes whichever children exist under the identities it named.
- A child that was taken in is released, because it holds every page it
  inherited.
- A child that never started is given up on the source.
- A child whose receive failed for its caller may still be on its way. A child
  runs only once it has claimed its hold: when its destination has every page
  it inherited, it asks the parent's host over the peer server whether the hold
  stands. A hold that stands is marked claimed and the child runs; otherwise the
  destination discards the child. The parent's host decides a claim and a
  give-up of one hold one at a time, so exactly one wins. A give-up after the
  claim says so (`POST /vms/{id}/abandoned` answers `claimed`), and the
  orchestrator deletes the child. A child on its parent's host claims its hold
  as it is bound to the point.
- The rollback runs on the fork's context without its cancellation, because a
  caller that hangs up is the commonest way a fan-out fails.

Two cases are not covered. An orchestrator that dies during a fork gives up no
hold, so a child that lands runs, listed under its parent. A give-up that cannot
reach the parent's host is logged and not retried, so a child that claimed its
hold first is not learned of.

The fork calls are `volume.VM.ForkPoint`, `volume.Manager.Inherit`,
`volume.Manager.Fork`, `volume.ForkPoint.Share` (the local backing's attach),
`vmmigrate.Fork` (a nil source is a child the parent's host takes in),
`vmmigrate.Options.Point`, `host.Host.Fork` and `host.Host.Receive`.

### A post-copy child's own published pages

A post-copy child was once told that its own published pages had no object. Its
guest crashed a second or so after it resumed, usually in the kernel's timer
wheel (`__run_timers` on a node whose `pprev` was `dead000000000122`, the
poison `hlist_del` leaves), otherwise in `rb_erase`, `profile_tick` or
`process_one_work`, at a wild address, or by hanging. The guest had read an
older version of a page it wrote.

A destination reports no identity for the pages its handoff named, so the pager
loads them through the peer backing. That set was fixed for the backing's life,
so it still reported no identity after the child's own checkpoint published
them. A retire gives up a page the volume holds no object for, because an
all-zero page is published as a hole. So the retire released the only copy of
the guest's bytes, and the next read returned the fork point's version. The
rate grew with the page count: the fan-out fixture's handoff set is about 8300
pages at 4 KiB against about 16 at 2 MiB, which is why it appeared with the
page-geometry plan's fourth step.

The first fix was incomplete; see
[the source's copy after the destination publishes](#the-sources-copy-after-the-destination-publishes).
Three safeguards stay:

- `vmmemory.ErrUndroppable` fails a retire of a page the volume holds no object
  for unless the page is zeros. The checkpoint stays durable, the page stays
  sealed, and the guest keeps its memory.
- `volume.ErrRetired` refuses a hold on a fork point its last holder retired.
  When added, it caught a second defect: two children forked one after the
  other through the manager, each releasing its hold as it closed, took the
  second child from a point whose seal had ended.
- `TestFirecrackerForkChildrenSurviveTheirFirstSeconds` reproduces the failure
  in about a hundred seconds per run. It keeps the arms that isolated the defect
  (`SPROUTFS_FORK_ARM`: the whole checkpoint, the capture without the settle,
  the bare pause, one child, no interval). Before the fix: 0/8 with no interval,
  0/8 for a bare pause, 0/8 for a capture and seal, 5–8/8 for the whole
  checkpoint; 0/16 after.

### The source's copy after the destination publishes

A destination runs its guest from the receive on, and its memory regions ask
the source until the source is released and the bulk stream ends. Meanwhile
the guest stores, and a checkpoint publishes and retires what it received. A
fork's child publishes its root right after the receive, so every remote fork
has this window.

The source's pages do not change in it. A fork's peer server serves the frozen
fork point, and the parent is not checkpointed while the point is held. A
migration's source stopped its guest before it took the handoff's set. But once
the destination publishes a page, the source holds at best the version before
it.

Two rules got this wrong:

- A load asked the source for every page. A page the destination had published
  and the arena had given back was read again from the source, which answered
  with the version before.
- `Locate` stripped a page of the handoff's set while the checkpoint the volume
  named for it predated the handoff. A hole names no checkpoint, so it always
  seemed to. A zero page the destination published was a hole, its retire gave
  it back, and the next read got the source's bytes from before the guest
  zeroed them.

One rule now decides both. A page of the handoff's set is the source's until
this host takes it, and the volume's from then on. `Locate` strips exactly the
pages not yet taken. A load asks the source only for those, and for pages
outside the set that the volume names by a checkpoint from before the handoff
or by a hole, which the source may serve faster than object storage. A page
this VM has published since the handoff is read from the volume. The pager's
check in `readIn`, which refuses to share a page the load calls the source's
own, is no longer reachable from a peer backing and stays as a check.

`TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes` in
`internal/simtest` is the scenario, and half of the generated campaigns'
migrations and forks run it. It fails under each of three guards that put the
old rules back: `migration-strip-published-pages`,
`migration-strip-published-holes` and `migration-ask-for-published-pages`. See
[Negative tests in the tree](testing.md#negative-tests-in-the-tree).

## Qualification

The simulated suite runs two volume managers and two pagers over one simulated
world, with a guest that stores continuously. It requires that:

- the destination's byte model equals the source's at the stop;
- the pause reads and writes no object of the volumes;
- every page the source held is served by the source and never requested again;
- every unpublished page is fetched before `Done` returns and is published by
  the destination's next checkpoint;
- `Done` does not return while one of those pages is only on the source.

A failure in the stop must leave the guest running here with its memory intact.
If the source is lost before the destination fetched its unpublished pages, the
VM must remain openable at its selected checkpoint, rewound by exactly the
writes since. A destination machine that does not map every memory region the
source had is refused before it runs and is closed. The page protocol's tests:

- a partially resident memory region is answered in one request;
- an unserved volume stops the retries on the one answer that says so;
- an unreachable source costs each load one round trip for the pages a
  checkpoint holds;
- a request over the per-peer budget is answered `BUSY`, and its connection
  stays open;
- a busy source is not mistaken for a gone one;
- a VM handed over by the release before arrives whole.

Failure during post-copy, under the simulated clock and network:

- A source that resets twenty connections in a row still serves the page, and
  the fault completes.
- A source silent for ten simulated minutes leaves the fault waiting, not
  failed, and it completes with the right bytes when the source answers.
- A source that says it no longer serves the VM fails a fault for a page only
  it held with `ErrUnpublishedLost`; pages a checkpoint holds come from the
  volume.
- Discarding the received VM ends a waiting fault with the cancellation's cause.
  An unreachable source ends the same way: `Done` waits, and the discard ends
  the wait.

A busy source is retried until it serves the unpublished pages, and the
destination ends with the source's bytes, not the checkpoint's. `Done` returns
while the bulk stream still runs.

In the simulated deployment, the source is lost during the post-copy, the
handover ends at the loss, and the VM comes back on the remaining host at its
selected checkpoint. A second scenario cuts the source off from every host and
the deployment while it runs and stays listed: the handover ends when the
source's hold does, and the source gives the pages up at its own deadline. The
orchestrator's tests state the same rule: a quiet listed source ends the
migration at its hold and not before, the VM is recovered past its silence, and
any other quiet host still refuses the recovery. A third scenario and an
orchestrator test cut off a fork's parent's host while a child on another host
is in post-copy: the child's receive ends at the hold, the fork does not happen,
and the parent runs on.

Retried receives are tested at three levels:

- The simulated world retries under the orchestrator's policy. The swizzle
  campaign hands a guest over while every link separates and heals, so a first
  receive fails on every seed, and the guest must be handed over, not taken
  over. Every campaign requires that a failed receive leaves no guest running
  on its destination. Scenarios cover a destination cut off from the store that
  takes the VM once the link is back, a destination that keeps refusing and is
  left for another host, a handoff nobody takes that is given up only at the
  end of the hold, and a receive that outlives its caller while its guest
  starts slowly, ending with one guest started.
- The orchestrator's tests cover each rule: a retry on the same host, a move to
  another host with room, the end of the hold, a source that no longer serves
  the VM, a receive that took the VM but lost its answer, a quiet destination, a
  receive reported in flight that lands and one that fails, and a request that
  gave up before the handover ended.
- The host suite refuses a second receive of a VM while one is in flight, and
  reports the first in `Status` until it ends.

The host suite runs the same migration between two hosts over loopback TCP,
including a drain of every VM on one host.

A fork on the parent's host must receive its child over the pages the seal
froze: every inherited page mapped by identity, none loaded back, no connection
dialed. Children of one point must map each other's pages, not copies. A fork
onto another host must name and pull only the pages no checkpoint of the parent
holds. Neither side may see the other's later stores. The child's root is
published when the child holds those pages, not at the next interval. With the
interval loop off on every host, a third host opens the child once its root has
landed, and a receive returns before that even with the store refusing every
upload. If the parent's host is lost after that, the child opens anywhere and
reads back its fork point. Before the child has published, opening it reports
`ErrForkPending`. Wherever the child is:

- a delete retires the holds this host has;
- a parent that something else holds sealed is refused;
- a hold that nothing releases is given up at its deadline.

The full-guest suite migrates a real Firecracker guest between two pagers and
two managers in one process over loopback TCP. The guest stores into its RAM and
its DAX disk until the vCPUs stop. Every destination memory region starts
through a `PeerBacking`, so the source's peer server is on the VMM's fault
path. The guest comes back with its counters intact, read through the console.
`PeerStats` shows the touched pages came from the source, including a page the
guest had not reached that only the source holds. The suite compares what the
source serves byte for byte with the destination's checkpoint. After `Release`,
the fault path reads the volume, the peer count stops, and the region never
asks that source again. The suite reports the stop-to-resume pause, how many
pages of each memory region the handoff named as unpublished, and each region's
peer and volume page counts.

### Page payload compression

Page requests and replies require payload format 1. Each reply carries one raw
or Zstandard blob of the present pages in bitmap order, normally one 2 MiB page.
CRC32C covers the transmitted bytes, and the blob checks its decoded length and
contents before any byte reaches guest memory. Source admission charges full
logical page bytes. Older page protocols are rejected. `Handoff.State` is the
runtime's raw state. Checkpoint VMM-state objects are compressed.
