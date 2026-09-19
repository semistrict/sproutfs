# Testing

Deterministic simulation is the main correctness tool. The actual
control-record, checkpoint and volume implementations run over simulated disks,
networks, object storage and time. Fault injection changes those dependencies; it never
substitutes a simplified storage implementation. Simulation tests use virtual
time, and a random workload reports its seed, the commands it ran and the
recent simulator events when it fails.

`internal/simtest` is the one way a simulated deployment is built, and every
campaign is a schedule, a fault set and an invariant set over its `World`; see
[One harness](#one-harness) below.

`just check` is the gate every push has to pass: the determinism rule below,
`gofmt`, `go build` and `go vet` for Linux and macOS, `go test ./...`, `buf
lint`, `shellcheck`, and the Rust crate's `fmt`, `clippy` and unit tests. `just
test-race` adds the race detector. `just soak` runs one block of the extended
seed sweep; see [Seed sweeps](#seed-sweeps) below. `just test-knobs` and `just
test-soak-knobs` run the campaigns with a seed's own tunables.

## One harness

A `simtest.World` is one running deployment: every host is a real `host.Host`
inside a `sim.Process`, on a `sim.Disk` of its own with `PowerLossFaults` on,
keeping its deadlines against a `sim.Clock` and its jitter against a seeded
`Entropy`, reaching the deployment's object store through a view of its own
that a kill takes away first; and every VM that exists has the simulated VMM
process storing into it and the bytes that guest believes it has. Nothing in
there is a mock of the thing under test — the volume managers, the checkpoint
store, the control records, the pagers, the page servers and the migration
coordinator are the real ones.

What a campaign drives it with is the same short list whatever the campaign is:
`Store`, `Checkpoint`, `Migrate`, `Fork`, `Delete`, `Takeover`, `Kill`,
`Restart`, `Shutdown` and `Settle`, plus `KillDuring`, which runs one operation
on a goroutine of its own and takes a host away in the middle of it. One access
in four that `Store` draws is a write fault the guest stores nothing through,
because a write fault is not always a store — a cold read reaches the pager as
one on x86-64, and so does a guest kernel's first execution of a page on
aarch64. The model records nothing for it, so the page has to read what the
guest last wrote whether the pager publishes the copy it made or settles it
back onto the page it was copied from. What it
requires of them is also the same list: `Verify` (no guest reads bytes it never
wrote, read through that guest's own mappings), `VerifyDurable` (the same bytes
read back through the volume), `CheckSelected` (every record selects a
checkpoint some writer of that VM published), and `CheckDeployment` at the end.

A VM whose host is lost comes back at one of the checkpoints it may have come
back at: the last one that landed, plus every later one whose publication was
interrupted and may or may not have landed. Which one is not guessed from the
bytes — the control record names the sequence, and every checkpoint the world
took carries the sequence it was published under, so the record's selection
picks the pause and the bytes are then required to be that pause's, page for
page. An interrupted publication is answered for exactly rather than tolerated,
and two checkpoints mixed into one VM is a failure either way. The VMM state the
checkpoint carries is restored with it, so a takeover that recovered a volume's
bytes without the registers that were running over them is a failure too.

The campaigns:

| Campaign | What it is |
| --- | --- |
| `TestSeededTopologyCampaign` | The deployment and its concurrent faults, both drawn from the seed. See [Seeded topologies](#seeded-topologies-and-failure-schedules). |
| `TestSeededTopologyUnderBuggify` | The same with the per-site injection on. |
| `TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld` | The kill campaign. See [Losing a host](#losing-a-host). |
| `TestTwoWritersOfOneVMNeverMixAcrossASwizzle` | The two-writer campaign. See [Clogging and swizzling](#clogging-and-swizzling). |
| `TestScheduledWorldReproduces` | The one recorded scenario. See [Overlap scheduling](#overlap-scheduling-experiments). |

Each has a `*Soak` twin over a block of the seed range, and `just soak` runs
them and nothing else.

## One clock, one entropy source

`platform.Clock` is the passage of time as a process sees it — `Now`, `Since`,
`Sleep`, `AfterFunc`, `NewTimer`, `NewTicker` — and `platform.Entropy` is the
unpredictable bytes beside it: the writer nonce a lost conditional write is
reconciled by, and the jitter that keeps a host's VMs from checkpointing in
lockstep. A nil Clock or Entropy anywhere in a configuration means the wall
clock and the operating system's pool, so production code never names one it
does not need.

They are threaded through `host.Config`, `SupervisorConfig`, `control.Config`,
`vmmemory.Config` and vmmigrate's `Options` and `PeerConfig`. `volume` and
`checkpoint` take neither, because neither reads a clock or draws a random
value; what is tunable about them is in `internal/knobs` instead.

`sim.Clock` is a virtual clock nothing elapses on its own. `Advance` releases
the deadlines it passes, in deadline order and, within one moment, in an order
the seed chooses, so two holds that expire together do not always retire in the
order they were armed. One advance runs the callbacks it released in that
order, on a goroutine of its own; `Settle` waits for them, and inside a
`testing/synctest` bubble `synctest.Wait` reaches the quiescent point after it.
`sim.Runtime.NewEntropy` is the matching seeded nonce source: reproducible per
seed, and still distinct draw to draw.

This is what makes a deadline written in checkpoint intervals reachable.
`TestForkHoldExpiresOnTheSimulatedClock` retires a fork hold at the
deployment's own four-interval bound with no wall-clock wait and no polling,
and requires the parent to take its sealed pages back and be checkpointable
again; `TestReleasingAForkHoldDisarmsItsDeadline` requires the ordinary release
to leave nothing armed. Before this the only test of an expiring handover set a
fifty-millisecond bound of its own and polled the wall clock for it, so the
deadline a deployment actually runs was never the one under test.

## Tunables

`internal/knobs` is the deployment's tunables in one place: the part size
and root bound, the upload, builder and cache budgets, the write and open-VM
bounds, the checkpoint interval with its jitter share and the loss window it
bounds a host loss in time with, the epoch interval, the
hold measured in checkpoint intervals, the pager's resident, logical and dirty
budgets with its read-ahead, write-ahead and I/O bounds, and a drain's
concurrency and its two timeouts. `Defaults` is what a deployment runs, so a
run with them is the run without them; `Validate` refuses a set the packages
would refuse or that contradicts itself — a resident arena larger than the
metadata cap describing it, a per-VM drain bound above the whole drain's.

`Randomize` draws a hostile but valid set for one seed, as FoundationDB
randomises its own knobs under buggify: a part size of one byte makes every
member its own part and a publication all of its interrupted-upload paths at
once; a dirty budget near its floor makes eviction and spill ordinary rather
than rare; a hold of one checkpoint interval makes a handover race the interval
that would have made it unnecessary. One knob in ten keeps its default, so a
seed is a mixture rather than a uniformly tiny deployment. Every draw is keyed
by its knob's name, so adding a knob does not move the values the others take.

The campaigns draw one set per seed under `SPROUTFS_TEST_KNOBS`, and the knobs
a seed moved are logged with it. `just test-knobs` runs `internal/simtest` that
way and `just test-soak-knobs` its extended blocks. Drawing knobs has changed no
outcome yet in the seeds that run.

The opt-in is off by default because the recorded scenario compares its
recordings byte for byte across processes, and a seed that also chose its
tunables would be comparing a different run. A campaign holds the few knobs its
own world is sized around: the pager arena has to hold every VM of the topology
twice over, because a fork or a migration has the parent's pages and the
child's on one host at once, and the dirty budget goes with it, because there is
no interval loop in these campaigns to answer the pager's pressure — they drive
their own checkpoints, so a budget smaller than what the guests on one host can
dirty would stall a store on a checkpoint nobody is going to take. Read-ahead
and write-ahead stay at one page, because the model counts what a source holds
against what its guest wrote, and the open-VM bound is floored at what a
takeover holds beside the handle it fenced. Everything else is the seed's.

The loss window is held the same way and for the same reason. Every seed draws
one, between turning the bound off and leaving it wider than anything a
campaign's clocks reach: these worlds advance a host's clock only to reach a
handover's deadline, and a window that actually fired in one of them would hold
a guest back waiting for a checkpoint nobody takes, which ends in the deliberate
stop these models do not follow. What a campaign requires of the window is that
every host, pager, migration and handoff carries it through every kill and every
swizzle, and that no recovery ever rewinds more than it allows —
`VerifyLossWindow`, beside `VerifyDurable`, at every recovery. What the window
actually does to a guest is the four scenarios in `losswindow_test.go`, which
run the checkpoint loop precisely so a store held back by the window has a loop
to ask.

What the settle behind a checkpoint's pause does to a guest has a scenario of
its own, `unchanged_test.go`: two children of one fork point read every page of
their memory and their disk through faults that all claim to be writes, store
nothing, and are checkpointed. Each must publish no page at all and hold no
private byte afterwards — every page it touched is the parent's page again.

## The no-cheating rule

`internal/testdeterminism` is a `just check` step that refuses `math/rand`,
`crypto/rand`, and bare `time.Now`, `time.Since`, `time.After`, `time.Sleep`,
`time.NewTimer`, `time.NewTicker`, `time.Tick`, `time.AfterFunc` and
`ctxsync.Sleep` in the non-test code of `internal/{volume,checkpoint,control,
vmmigrate,host,vmmemory}`, including the Linux-only files this machine does not
build. A stray wall-clock reading decides how long a hold lives and a stray
`math/rand` decides which VM checkpoints first, and a run that reproduces
neither is a run whose seed reports nothing.

The allowlist is keyed by file and by what is read, and it carries a count: a
second reading added to an already-listed file is a new decision and has to be
argued for. It currently holds one entry — the two socket deadlines in the
pager's client, which are moments the kernel compares against its own clock
and which no clock this process is given can be handed. The rule is itself
tested against sources containing each thing it forbids, so it cannot pass by
finding nothing.

## Models

An independent byte-array model tracks the state expected after acknowledged
writes and discards. Reads are compared against it after checkpoints, reopens,
takeovers, object-store outages and fork divergence. The checkpoint
package has its own model: random dirty sets published over several
checkpoints with forks, checked by reading every byte back. The pager is
checked the same way, with randomized eviction and randomized captures against
independent models.

A write is durable only once a checkpoint publishes it, so the model a reopen is
compared against is the model at the checkpoint the control record selects. A
conditional write whose reply was lost has an ambiguous outcome; the tests
require it to be reconciled to the one it actually had, never to be guessed. A
known incorrect outcome is never made the expectation of a passing test.

Page identity is asserted directly: locate reports equal identities across a fork for
pages neither side has written, distinct identities once one writes, and
private identities for bytes still in an overlay. The pager asserts that an
eligible resident page with a matching identity in the same pager is mapped
without a backing read.

## Failure coverage

Small targeted tests pause execution at publication and ownership handoffs.
They cover conditional-write conflicts, successful responses that were lost,
interrupted uploads, and concurrent opens or takeover at those exact points.
A checkpoint is interrupted at every step and must leave the parent readable
and the retry successful; a publication fenced by a later writer must not be
selected.

Seeded workloads combine writes, discards, checkpoints and reopens. The
control-record suite covers concurrent opens taking distinct epochs, a fenced
handle staying fenced, a selection that does not advance, a lost
conditional-write reply reconciled by the writer's nonce, a refused write
leaving the handle usable, and a corrupt record being refused.

### Losing a host

A `World` host ends one of two ways. `Shutdown` is the orderly close: it
publishes a final checkpoint of everything its handles hold, its guests give
their pages back and its process then ends, leaving its disk alone. `Kill` is
the machine dying: the store goes first, so nothing the host had in flight can
still land and its shutdown publishes nothing; its guests' VMM processes go with
it, because memory is durable nowhere; and the process is then crashed, with
`PowerLoss` resolving every modification the disk had not synced into bytes that
were applied, dropped, torn or garbled. A killed host is started again in the
same test process, on the disk it left behind. A host keeps no durable local
state — its scratch spill file is truncated at every pager start — so what a
restart recovers is what the deployment's object store holds.

`TestAKilledHostRestartsOnItsOwnDiskAndRewindsToItsLastCheckpoint` in
`internal/simtest` runs one script under all three endings and is where the
difference is asserted: a handle write is durable exactly when the close
published it, and a guest's stores survive only as far as its last checkpoint,
because nothing but a capture ever publishes a page.
`TestAKilledHostIsTakenOverByAnotherHostAtItsLastCheckpoint` is the other way a
VM comes back: a surviving host takes the record over while the dead one is
still dead, and the dead host's handle stays fenced.

`TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld` is the seeded
campaign, a schedule over one `World`. Every seed takes a host away at each of
the four places one can be lost: in the middle of a checkpoint, while it holds a fork point another
host's child is still reading out of, while it serves the pages of a VM it
handed over, and while it is the host taking one in. The moment inside the
operation and the kill mode are drawn from the seed, the victim comes back on
its own disk, and the VM is then recovered by a bystander host or by that
restart, as the seed chooses. Whatever the kill interrupted, the requirements do
not move:

- the VM reads as one whole generation — every byte of a page, every page of the
  VM — through its volume and again through a guest's own fault path, so two
  checkpoints mixed and a byte no guest wrote are both failures;
- that generation is exactly one of the checkpoints the VM may have come back
  at, which is no older than the last one acknowledged and no newer than the
  last one written;
- what was recovered publishes again and reads the same afterwards, because a
  state no host can make durable is not a recovery;
- every kill is in the trace, and the store still holds a deployment
  ([the deployment check](#the-deployment-check) runs at the end of every
  scenario).

The kill lands inside the operation on the seeds whose drawn moment falls
inside it and after it on the rest, and `World.KillDuring` reports which. The
source's hold on a handover whose destination died is the deployment's own four
checkpoint intervals, reached by advancing the `sim.Clock` that host keeps it
against rather than by waiting four minutes for it. The seeds are required
between them to cut into every one of the four scenarios rather than to land
after it had finished, since a kill that always arrives late tests nothing an
orderly close does not. The campaign runs with `Buggify` on and its runtime on
the context, so the kills land around production code that is itself
misbehaving; `checkpoint/one-page-parts`, `control/slow-write` and
`vmmigrate/source-busy` fire during it. Eight seeds run in the ordinary suite,
`SPROUTFS_CRASH_SEEDS` selects any other count, and `TestHostCrashSoak` runs a
block of the seed range.

Three things it found. The shared `machine` double emptied its page mapping
before detaching the region, so the pager was still mapping and protecting
through a map the close was clearing; nothing before this had closed a machine
with a capture in flight. A fork's child handed to another host has no
checkpoint of its own until something checkpoints it, so with the interval loop
off the child opened as `volume: fork's root checkpoint is not published`; the
destination's own checkpoint is what publishes it, and the campaign takes it where a
deployment's interval loop would. And without `sim.WithRuntime` on the context
the whole fault-injection apparatus is a no-op: with it the campaign is killed
by the `migration-corrupt-peer-page`, `pager-zero-new-page` and
`checkpoint-part-member-offset` guards, which is what says it is not vacuous.

```sh
SPROUTFS_CRASH_SEEDS=500 go test ./internal/simtest \
  -run '^TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld$' -count=1
```

### Parts, index objects and reclamation

A checkpoint's dirty pages upload as parts, and the segments they changed and
the root go into its index object. The checkpoint suite requires a part's member
table and trailer to describe every member, and a checkpoint reopened out of its
index object to be the one that was published, byte for byte of its root. It
also requires that a published checkpoint is an index object and its parts and
nothing else, that opening one is a single object-store request counted through
the simulated store's trace, that a checkpoint writes into its index object only
the segments it changed and addresses every other in the index object of the
checkpoint that wrote it, that an index object stays for exactly as long as some
root addresses a segment in it, that a publication interrupted before its index
object leaves a checkpoint that reads as absent and a reference a later
publication can still land under, and that a page published as zeroes leaves the
segment naming it with nothing in any part to say so. It also covers a
multi-part checkpoint, a republication under one reference producing
byte-identical objects, and a publication whose heap stays bounded by the part
size rather than by the dirty set.

A part's table is bounded at 256 KiB by the writer as well as by the reader: a
checkpoint of 1400 volumes with the longest names a volume may have, which is
more table than one part may hold, is required to spread its members over parts
whose tables each fit in what one read holds, and reading a part's table is
counted through the simulated store's trace and required to be one request.

Reclamation is tested directly: selecting a checkpoint deletes whole every
earlier checkpoint the new root no longer names, keeps one it
still reads a single page from, spares a sequence a fork pinned, and never
touches the checkpoint the handle opened on. The pin is shown to precede the
fork: a fork whose own control record cannot be written still leaves what it
would inherit pinned, and so does one abandoned before it published a root. A
grandchild is shown to go on reading the page its grandparent published after
its own parent has rewritten the last page it inherited, which is what a
permanent pin buys; a fork chain deleted in any order leaves every survivor
readable; and a delete over a record that cannot be parsed is refused rather
than sweeping the checkpoints its pins would have spared. Compaction is required to
rewrite the parts of checkpoints that are less than half live, to stream those
rewrites rather than hold them, and to move no segment: a segment the compaction
did not otherwise change stays in the index object of the checkpoint being
emptied, which therefore stays too.

### Forks

A fork is a handoff from a running parent, so it publishes nothing. That is
asserted against the object store itself: forking a running VM adds the child's
control record and no other key, and a fork closed before its first checkpoint
adds nothing more. Forks are tested for divergence from their parent, use
before their own first checkpoint is published, refusal on an existing identity,
forks of forks, and two forks of one parent diverging independently. A parent
whose pages a fork point holds refuses a second seal and refuses a capture
before anything pauses its guest, and takes them back when the point is retired.

On the parent's own host the children read the sealed pages by page
identity, so a second child maps the first's resident page without a load. Across hosts
the child pulls exactly the pages no checkpoint of the parent holds, publishes
them in its own first checkpoint, and survives the loss of the parent's host
once it has. The host suite shows one pause starting several children at
once: every one of them reads the parent's memory at that pause, the interval
loop leaves the sealed parent alone rather than failing on it, and the parent is
checkpointed again only once the last child has published.

Capture is tested for returning without waiting for publication, for a failed
preparation or resume capturing nothing, for a checkpoint publishing the sealed
pager pages of every region and retiring them once it is selected, and for a
failed publication handing every sealed page back to the guest.

### Unsynced writes

`sim.DiskConfig.PowerLossFaults` makes a simulated device resolve every
modification made since a file's last successful `Sync` instead of discarding
all of them. Each write is resolved as FoundationDB's `AsyncFileNonDurable`
resolves one: a kill mode is drawn for the file at every open — never
`NoCorruption` — a mode no worse than the file's is drawn for each 4 KiB device
page, and each 512 B sector within it is then applied, dropped, or written with
its head or its tail replaced by garbage. The trace names each resolved
modification and its outcome: `applied`, `dropped`, `prefix_truncated` or
`sector_garbled`. Every draw is keyed by the disk, the file and the
modification's own sequence, so adding a choice for one file cannot move the
choices made for another.

This is off by default, and a device that restores exactly its last sync stays
the assumption the rest of the suite is written against. `SyncDurableProbability`
additionally models a device that acknowledges a flush it did not perform; it is
opt-in and driven by no consumer, because nothing in sproutfs treats a local
file as durable.

Three readers are driven across it. The pager's spill file carries a checksum
per reservation, held by the host rather than by the file, because the file is
scratch by construction: a private page whose bytes do not match it is refused
with `vmmemory.ErrSpillCorrupt` instead of being mapped. Without that check the
pager handed the guest a zero byte where the guest had stored its own value,
under every seed, and reported nothing. A VMM's state file can carry no checksum
of ours — its format is the VMM's, and a short file is indistinguishable from a
smaller machine — so what refuses it is the handle the power loss invalidated,
and the test shows that the bytes the device kept are not the ones the VMM
wrote. Parts go straight to the object store and never touch a disk, so
`TestATornPartIsRefusedRatherThanReadAsMembers` stages them itself: across
48 seeds, 44 parts came back damaged and every one was refused by its trailer,
its table or a member envelope, never decoding to bytes that were not written.

### Clogging and swizzling

`Network.Clog(from, to, until)` blocks one directional link until a simulated
moment and then carries traffic again with nothing called to heal it; a dial or
a send over it is refused with `ErrUnavailable`, as a partition refuses them.
`Network.Swizzle(addrs, window, random)` gives every link among a set its own
seeded interval — blocked inside the first half of the window, healed inside the
second — so the order links come back in is not the order they went away in.
Both are schedules read through `Runtime.Now`, which takes the standard library
until a clock is injected and inside a `testing/synctest` bubble is that
bubble's virtual clock; `Swizzle` therefore returns at once and starts no
goroutine that has to undo itself. `Network.Clogged(from, to)` reports the link
state, which is how the object store — not carried by the simulated network —
can still be taken away by naming it as an endpoint.

`TestTwoWritersOfOneVMNeverMixAcrossASwizzle` in `internal/simtest` is the
campaign. Two hosts and the store are swizzled while one VM is handed over and a
second is taken over: the handoff pulls the source's unpublished pages over a
page-server link that is separated, healed, dropping, duplicating, delaying and
re-latencied, and the takeover advances the epoch while either writer may be
unable to reach the store. Whatever the swizzle does, the requirements do not
move: the fenced handle stays fenced and never publishes, the selected root
names only checkpoints a writer holding the epoch published and nothing past
that epoch, every page the source held reaches the destination rather than
being lost to a takeover, and a fresh reader sees only the surviving writer's
pages. Sixteen seeds run normally; `SPROUTFS_SWIZZLE_SEEDS` selects any other
count and `TestSwizzleSoak` runs a block of the seed range.

`DropNext`, `DuplicateNext`, `DelayNext` and `SetLink` are the
`simtest.DroppedPageServerFrames` fault, which is the one fault the generated schedule
does not draw. A frame dropped on an open connection has exactly one outcome for
a guest's demand fault against the peer holding the only copy of an unpublished
page, which is to wait for a reply that never comes; waiting is the right answer
for that page, since giving up on it loses the guest's memory. So the drop
belongs to a campaign whose every fetch is a bounded attempt that is retried,
which is what a drain of a host that is going away does, and the generated
schedule takes `simtest.DegradedLinks` — the same kit without the drop —
instead.

```sh
SPROUTFS_SWIZZLE_SEEDS=300 go test ./internal/simtest \
  -run '^TestTwoWritersOfOneVMNeverMixAcrossASwizzle$' -count=1
```

## The deployment check

`volume.CheckDeployment(ctx, store, prefix, allow...)` lists one deployment's
whole object namespace and reports every way its durable state disagrees with
itself. It runs at the end of a scenario, once every handle is closed, whatever
that scenario did: every one of `internal/simtest`'s campaigns and their soaks,
the recorded scenario, and the host harness's own cleanup all call it. A
scenario that never gets there is a scenario whose leftovers nobody looked at.

What it requires:

- every control record and every part parses at the format version this
  build writes;
- every checkpoint a selected or pinned root names exists, with the part count
  and the member bytes that root recorded;
- every pinned sequence is a published checkpoint of the VM whose record pins
  it. That is all a pin has to agree with: it names no holder and no
  descendant's record names it, because nothing releases one, so what the check
  can say is that what a pin protects — the checkpoint and every
  checkpoint its root names — is whole, which is what a grandchild reading
  through it needs;
- every object under `vm/<id>/ckpt/` is reached by some record's selected
  checkpoint, by a pinned one, or by the one checkpoint of grace a compaction's
  emptied checkpoint is spared for. Naming a checkpoint spares it whole: its
  index object holds the segments some root still addresses in it, and its parts
  hold the pages some root still reads.

Each caller names the classes of leftover it expects, and only those are not
reported. Every class is something a host lost at a particular moment leaves
and no writer ever comes back for — a collector's, not a writer's — so a
scenario that kills hosts says which debt it is rather than skipping the check:

| Allowance | What it admits |
| --- | --- |
| `AllowSupersededEpoch` | The checkpoints of a writer epoch below the record's. A new handle reclaims only what it published itself, so every takeover leaves the checkpoint it opened on and whatever its fenced predecessor abandoned. |
| `AllowUnpublishedIndex` | A checkpoint whose parts are there and whose index object never landed: a publication interrupted before its commit. |
| `AllowUnreferencedCheckpoint` | A published checkpoint of the record's own epoch that nothing selects or pins: a sweep the store refused. |
| `AllowUnrecordedVM` | Objects under a VM with no control record: a create interrupted before its record, a delete interrupted after it, and — with no host lost at all — the pinned checkpoints a finished delete leaves, which is what deleting a VM that was ever forked always leaves behind. |

Two leaks it found on its first run are fixed rather than allowed: a create
never reclaimed the first checkpoint it published, because the handle counted
nothing as its own until it had published again; and a fork closed before it ever
published its root left its own record behind, which burnt that identity for
good.

## Handovers

A handover is the one operation a VM's memory can be lost by, so it is what the
campaigns spend most of their steps on. A migration publishes nothing: the
source stops its guest, gives its volumes up and keeps serving the pages no
checkpoint holds until the destination reports having them. What a fault that
takes the source away therefore costs the VM is exactly the pages written since
its last checkpoint, which is the post-copy exposure rather than a defect —
`World.Migrate` follows that rewind exactly instead of tolerating it.

Every hop the campaigns run makes the same checks: the source's handle is
refused a store the moment the handoff is taken, the destination's guest
restored the VMM state the source's pause captured and continues at the same
store counter, the destination's first read is the source's last checkpoint plus
the pages it serves — read through its own mappings before it writes anything
of its own — and a refused migration leaves the guest running where it was, with
every region unsealed, every page writable and its vCPUs running. The recorded
scenario adds the layout refusal: a handoff that would truncate a region or map
beyond its volume is refused before anything starts a guest.

`internal/vmmigrate`'s own suite keeps what is about the package rather than
about a deployment: that `Done` returns only once every unpublished page is on
the destination and never while one is still only on the source, that the
destination's own next checkpoint publishes them, that the source's page server
is then removable entirely, failure to resume, cancellation before a handoff, a
handoff that waits for a publication another call already had in flight, and
`TestSourceLostAfterHandoffRewindsToTheLastCheckpoint`, which drops the source's
pages right after the handoff and requires the VM to come back at the
checkpoint its control record still selects, rewound by exactly the writes since
it. The simulator's listener-close regressions require queued clients to
disconnect and in-flight dials to reject a closed listener, while accepted
connections remain usable. Otherwise source shutdown could leave a page fault
waiting on a connection no server will ever accept.

## Seeded topologies and failure schedules

The campaigns this one replaced permuted a fixed list of faults over a fixed
topology, one fault at a time. `internal/simtest` generates both halves from the
seed instead, which is what FoundationDB's
`CompoundWorkload::addFailureInjection` does: a combination like "the source is
partitioned while the store is unavailable while a second host takes the VM
over" is unreachable however many seeds a one-fault-at-a-time campaign runs.

`simtest.NewTopology(random)` draws the deployment: two to four hosts, two to
four VMs, one or two volumes of one to three pages each, which of the VMs are
forks of which, and the host each one starts on — a fork's host may be its
parent's, which is the difference between a child that shares its parent's
pages and one that pulls them out of the parent's page server. A failing seed
prints its topology, and a topology printed is a topology reproduced.

`simtest.Fault` is one thing that goes wrong: `Begin`, `End`, and `Holds` — what
it must leave true once it has ended and the world has quiesced. A fault is a
condition of the world rather than a phase of one migration, which is what lets
three of them be on at once. `simtest.Driver` places every fault of the set in a
schedule of 26 steps at seeded offsets, one to three at a time for a seeded run
of steps, and runs one operation per step with a few of every guest's own stores
in front of it: a checkpoint, a migration, a fork, a stop, a start, a delete or a
host lost and started again. A stop and a start are drawn independently rather
than as a pair, so a seed's stopped VMs sit out however many steps it draws
before starting them — which is where the faults land on a VM that is nothing but
its objects, and where a host that comes back without it has to leave it alone.
A stopped VM is the one reason for a VM to be running nowhere that is not
something to repair, so the world's `Settle` leaves it be: a stop whose VM came
back by itself at the next step would be no stop at all. Every fault's start and end is recorded in the simulator's trace
through `sim.Trace.Record`, beside the adapter operations it perturbs.

The faults are the migration campaign's ten, restated as conditions, plus the
ones only a generated topology can express:

| Fault | What it is |
| --- | --- |
| `store-unavailable` | Object storage answers nobody. |
| `host-loses-store` | One host cannot reach object storage while every other host can. |
| `partitioned-pages` | Two hosts cannot reach each other's page servers. |
| `swizzled-links` | Every link among the hosts, their page servers and the store is blocked at its own seeded moment and healed at another. |
| `lost-page-replies` | One host's page reply is dropped after the source has already answered it. |
| `stalled-stream` | The first frame one host receives is held until whatever asked for it gives up. |
| `lost-host` | A whole host is taken away at a moment and started again when the fault ends. |
| `refused-stop` | One VM's migration pause fails after its guest has stopped and a region is sealed. |
| `refused-start` | One host's half of a receive fails before the guest is started. |
| `degraded-links` | The page-server links duplicate, delay and slow what they carry. |

`dropped-page-server-frames` is the same kit plus `DropNext`, and the schedule does not
draw it; see [Clogging and swizzling](#clogging-and-swizzling) for why, and for
the campaign that does.

What an operation owes under a fault is not success. A migration may be refused,
a checkpoint may not land, a takeover may be unavailable; what may never happen
is any of these three:

- **No guest reads bytes it never wrote.** Every page of every running VM is
  read back through that guest's own mappings after every step and compared to
  the bytes that guest stored. A page whose only copy is on a peer a fault has
  taken away cannot be read at all, which is what the fault means rather than a
  byte read wrong, so it is reported while a fault is on and required to read
  once they have all ended.
- **Every VM's selected checkpoint is one it published.** Every takeover checks
  the sequence the new writer inherits against the sequences this campaign's
  writers published, and the end of the run checks every record.
- **`CheckDeployment` passes at the end**, with the allowances this campaign's
  own faults earn: what a host lost at a moment leaves, what a VM deleted
  after it was forked leaves, and the checkpoint a sweep the store refused
  could not take.

A VM nobody is running — a source that could not resume, a destination that
could not take it, a post-copy that did not finish, a host that was lost — is
opened again by a host that can, and the model rewinds to the bytes the
checkpoint its record selects made durable. That rewind is the post-copy
exposure rather than a defect, which is why the model follows it exactly instead
of tolerating it.

`TestASourcePartitionedWhileTheStoreIsAwayAndASecondHostTakesOver` names the
combination rather than waiting for a seed to draw it, in both its halves: a
destination that cannot reach object storage at all, and then one that does take
the VM over and only then finds it cannot fetch the pages the source still
holds.

```sh
go test ./internal/simtest -count=1
just soak 1 100          # the seed sweep, which runs every campaign
```

What it found once every campaign ran real hosts — a defect that is open, the
defect that came before it, and the things about the simulated world after
them:

- **A deleted VM's identity was reused, and a host's page cache still held the
  pages that name it** (fixed in `8ccfb15`). `checkpoint.Cache` keys a page by
  its identity — the VM, the checkpoint sequence, the volume and the
  page — because a page's bytes are immutable under its identity. A
  delete freed the identity, and a VM created again under that name started at
  the same epoch and the same first sequence, so every page of its first
  checkpoint was named exactly as the deleted VM's was. A host that still
  holds the dead VM's pages serves them to the one that replaced it, and the new
  VM reads bytes no guest of it ever wrote. The smallest seed is 45: fork
  `vm-2` from `vm-1`, delete `vm-2`, fork `vm-2` again, and the new child's root
  reads the old child's bytes. Seventeen of the first two hundred seeds reach
  it, every one of them by forking an identity that had been deleted.

  It was unreachable until now because the campaigns that ran before this one
  built bare volume managers with no page cache at all; only a `World` of real
  `host.Host`s has one. Dropping the dead VM's pages from the deleting host's
  cache fixes fourteen of the seventeen and cannot fix the rest — any host that
  ever read the old VM's pages holds them under the same name — so what the
  defect actually wanted is an identity a delete cannot hand out again. A
  creation now draws its first epoch from the host's entropy, so no two
  creations share a sequence, and `Manager.Create` refuses an identity whose
  `vm/<id>/` namespace still holds objects; see
  [volumes](volumes.md#identities-and-reclamation).

- **A sweep took a checkpoint a pinned root names.** Reclamation spares a pinned
  checkpoint and, it says, "itself and every checkpoint that root
  names" — but `protectedBy` asked the root for the checkpoints it *reads*, which
  leaves out the ones its own compaction emptied and it still names. A fork
  taken on a checkpoint that had just compacted another empty therefore lost
  that one at the next sweep, and the pinned root went on naming an object
  nothing could fetch: `CheckDeployment` reports it as a part that does not read,
  which is what a hole in what a fork inherits looks like from outside. It is
  reachable only where a fork's point, a compaction and a later sweep line up
  — ten of the first two hundred seeds. Fixed by sparing what the pinned root
  names, with `TestReclamationSparesThePacksAPinnedIndexOnlyNames` in
  `internal/checkpoint` for the case.
- A frame held by the stalled-stream fault deadlocked the whole bubble. The
  fault was written for a migration's post-copy, which gives up on a deadline of
  its own, but the first frame a host receives may be a guest's own demand page
  fault, and that has no deadline: the guest waited for a page the fault was
  holding, and the step that would have ended the fault was the step the guest
  was blocking. The hold now ends on the caller's cancellation, on the end of
  the fault, or on a bounded simulated wait, which is a connection that died
  rather than a harness that stopped.
- A checkpoint's sweep runs behind its publication with the publication lock
  released, so a host that exits the moment after a checkpoint lands cancels
  the sweep and leaves the checkpoint it replaced behind. `CheckDeployment`
  reports it as an unreferenced checkpoint of the record's own epoch. The
  campaign gives its sweeps a moment before it closes, as a host draining itself
  would, so that the allowance it keeps for that class covers only the sweeps
  its store-outage faults actually refused.
- A frame dropped by `Network.DropNext` on a page-server link hangs the guest
  for ever, and has no other outcome it could have. The connection stays open
  and the sender believes it sent, so the reply never comes; and a guest's
  demand fault against the peer that holds the only copy of an unpublished page
  waits for it rather than giving up, which is the right answer for that page —
  giving up on it loses the guest's memory. A reliable message-framed connection
  cannot lose a frame while staying open, so the degraded-links fault does not
  ask it to: what a lost reply really costs is the connection, which is what the
  lost-page-replies fault takes.

## The same discipline on a cluster

These campaigns drive model guests: a `simtest` guest is a mapping and a model
of what it stored, which is what lets every page be read back and compared after
every step. Nothing in them runs Firecracker, KVM, the real pager's userfaultfd
path, GCS or Kubernetes, and each of those is a place a page can be lost that a
simulation cannot reach.

`scripts/demo-gce.sh soak` is the counterpart over a real cluster, and it is the
same shape: a seeded schedule of forks across two hosts, migrations, stops and
starts, a host killed once, and after every one of them every guest is asked
whether its memory and its disk still hold exactly the bytes it wrote. What
makes that question answerable is `cmd/sproutfs-guest-witness`, which both guest
images carry: a resident buffer and a file of the same size filled with the
pattern of a `(seed, step)`, where the pattern is a pure function of the seed,
the step and the page. The expectation therefore lives in the script rather than
in the guest — exactly as a campaign's model lives in the world rather than in
the guest it is checking — so nothing about what a VM should hold crosses a
fork, a migration or a stop, and a guest cannot agree with itself about the
wrong thing.

The soak ends the way a campaign does, with `volume.CheckDeployment` over the
whole object namespace: `sproutfsctl check` runs it through the orchestrator
once every VM has been deleted, when nothing but the templates — one per guest
image, named by the image's bytes — and the checkpoints the deleted VMs pinned may
remain. The allowances there are the live deployment's rather than a campaign's
— a publication in flight, a deleted VM's pinned checkpoints, a VM's own root and a
template import's intermediate checkpoints, and the superseded epoch every
takeover leaves, a template whose unfinished import a later one recovered
included — and
[the demo notes](demo.md#the-soak) describe the run itself.

What the two cannot share is the faults. A campaign injects a partitioned link
or an unavailable store at a moment it drew; the cluster gets one host killed
without grace, because that is the only fault a k3s node can be asked for
reliably. The campaigns are where the combinations live, and the soak is where
the real VMM, the real pager and the real object store are.

## Overlap scheduling experiments

`sim.NewScheduler(seed)` is the shared controller. Construct it inside a
`testing/synctest` bubble and pass its `Wait` method to `sim.Config.Wait`. Run the
workload, including shutdown of background actors, in another goroutine; the
bubble's parent calls `scheduler.Run(done)`. Stable zero-width waits can admit
workload operations, and `scheduler.Record` captures acknowledged or recovered
bytes. After the run, `scheduler.Recording(runtime.Trace())` returns independent
protobuf streams for comparison; write them outside virtual time with
`recording.WriteFiles`.

There is one scenario, `TestScheduledWorldReproduces` in `internal/simtest`,
over a `World` of three hosts with a fixed seed. It has three halves, because
that is what one deployment does.

The volume half checks complete disk and RAM images against an independent byte
model through concurrent writes, checkpoints, immutable snapshots, a fork over a
checkpoint, a discard, parent and fork divergence, failed publication and retry,
lost publication replies, a takeover that fences the handle holding the VM, the
epoch-major sequence the takeover then publishes under, and a cancelled write.
Semantic records contain hashes only after the complete bytes have been compared
with the model.

The handover half moves one guest from host to host through a seeded permutation
of six cases: a healthy handoff, a layout that would truncate a region or map
beyond its volume, an interval checkpoint before the migration, a destination
that cannot read the control record, a destination that cannot start the guest,
and a source whose frames are held until whatever asked for them gives up. The
world's own checks run at every hop — restored VMM counters, the destination's
first read, and a checkpoint of what it received read back through the volume —
and every guest store and every region seal is ordered by the controller.

The host half is what a deployment does to whole machines: a drain cancelled
before it began does no storage or transport work at all, a host is lost under
power loss and its VM is taken over at the checkpoint its record selects, the
host comes back as a fresh process on the disk it left behind, and the VM is
migrated back onto it. The transport names the host a dial comes from, which is
what the simulated network models a link between; real sockets are covered by
the ordinary host suite.

```sh
python3 scripts/check-overlap-reproducibility.py --scenario world --seeds 32 --runs 2 --maxprocs 1 2 4 8
```

`sim.WithTask` labels logical callers before concurrent work. The scheduled
harnesses put each region's backing loads and authority checks through
`Runtime.Admit`, so two concurrent identical reads are ordered by their logical
caller rather than by completion. Disk and object-store operations also enter a
gate before competing for their shared queue, so spill and page-cache work
cannot acquire it in an uncontrolled order. Ordinary adapters require no
controller. Manager shutdown orders handles by VM identity and acquisition,
which does not depend on Go map order. Already-canceled disk and dial attempts
do not consume fault plans or I/O IDs.

A destination's requests to its migration source are admitted the same way,
through `vmmigrate.WithAdmission`. Asking the source for pages is a decision
rather than an adapter operation: a post-copy stream cancelled between two
requests either sends the next one and has it refused on the wire or abandons it
before anything is sent, and because a pooled connection is taken without
dialing — and a dial refuses an already-cancelled context before it records
anything — there is otherwise nothing admitted between the cancellation and the
send. Both outcomes are correct, since every page the stream did not fetch is in
the destination's own checkpoint; the admission point exists to let the
scheduler order them rather than to change either one.


The dynamic experiment runs four concurrent clients through three rounds each
of requests, synced disk writes, object publication, replies and object readback.
Accepting a connection starts its handler; a reply causes the next request, and
failed publication creates retry work. One put fails before application and one
loses its reply after application. Every acknowledged value is checked against
an independent byte model, and the disk is power-cycled before durable readback.
This uses the actual simulated network, disk and object-store implementations.
The layer scenarios above extend the same controller to volumes, pagers, VM
handoff and complete hosts.

`sim.Config.Wait` optionally supplies completion timing control. Network sends
pass their configured latency/jitter range without selecting a completion time
first; fixed-latency disk and object operations expose a +/-25% experimental
window. Accept and receive also gate delivery into application code. Leaving
`Wait` nil preserves ordinary simulation timing and concurrency.

The controller calls `synctest.Wait` until every other goroutine is at a
durable wait, selects an overlapping completion window, and releases one
operation in a seed-keyed order. It then discovers the work that completion
created before choosing again. Cancellation wakes the controller and is handled
at the next such boundary. This requires stable logical IDs and stable admission
for requests sharing a sequenced resource; it does not govern arbitrary
shared-memory interactions between I/O boundaries.

Capture and compare separate processes with:

```sh
python3 scripts/check-overlap-reproducibility.py --scenario dynamic --runs 10 --maxprocs 1 2 4 8
```

The script prints a retained output directory. Each process runs the requested seed count (32 by default) in
both workload creation orders. Full `.pb` files contain length-delimited
`sproutfs.sim.v1.TraceEvent` messages in observed order, including submissions,
releases, quiescent completions, task lifecycles and byte checks. The separate
`.adapter.pb` stream preserves every event in the simulator's existing trace,
including its original order, time, operation, resource, outcome and byte count.
These are the adapter's existing trace points, not every Go scheduler transition.

The `.execution.pb` projection removes only submission/arrival events and
renumbers the remaining sequence. It preserves timestamps, outcomes, payloads
and observed execution order; it never sorts events. The script compares all
three forms byte for byte against the first process, matching seed and creation
order, and writes hashes and the first full-trace difference to `report.json`.
It exits nonzero if any form differs. The normal test requires execution and
adapter traces to match across reversed creation; it never requires arrival
traces to differ.

The regular suite also runs `Test*ReproducesAcrossProcesses` for the dynamic
adapter workload and the scheduled world scenario. Each launches the
same test binary in three fresh processes: `GOMAXPROCS=1`, `4`, and `4` again.
Each process runs seed 1 in both caller creation orders and checks the existing
independent model. Execution and adapter recordings must then match byte for
byte across processes, including times, outcomes, semantic observations and
event order. Missing or empty recordings fail the test. A mismatch prints the
first differing readable record.

```sh
go test ./internal/platform/sim ./internal/simtest \
  -run 'ReproducesAcrossProcesses$' -count=1
```

These checks enforce deterministic observable execution at the controlled
boundaries. They do not require goroutines to arrive at a wait in the same
order: full submission traces remain diagnostic, and the stricter standalone
comparison above continues to report their differences. Ungated competition
that changes a recorded outcome, adapter operation or scheduling decision will
fail the cross-process comparison. Repetition cannot enumerate every possible
interleaving; the larger seed campaigns and race detector remain complementary.

The schema is in `internal/platform/sim/proto/sproutfs/sim/v1/trace.proto`;
regenerate it with `buf generate`. Each `.txt` companion is a readable rendering
decoded from the protobuf file. Events are buffered in the virtual-time bubble
and written after it exits. To retain files from one process, set
`SPROUTFS_OVERLAP_TRACE_DIR` and run
`go test ./internal/platform/sim -run '^TestDynamicOverlapTraceFiles$' -count=1`.
Use a fresh directory for each invocation.

Use `SPROUTFS_OVERLAP_TRACE_SEEDS` to select the count when invoking a trace
test directly. The earlier two-action, declared-batch experiment remains available with
`--scenario batch` and `TestOverlapPrototypeTraceFiles`. It requires every
operation to register upfront and has no separate adapter trace file. These
experiments supplement the ordinary migration fault campaign; they do not
replace its uncontrolled concurrency coverage.

## Fault injection, probes and fingerprints

Three primitives in `internal/platform/sim` reach into the real volume,
checkpoint, control, pager and migration code. All three read the runtime out
of the context, which a harness puts there once with `sim.WithRuntime`; a
context without one — every real deployment, and every test that did not ask —
makes each of them a lookup and a return of false.

`sim.Buggify(ctx, id, p)` is FoundationDB's two-level switch. A site is
activated once per run with probability 0.25, drawn from the seed and the
site's id alone, and an activated site then fires with probability `p` on each
call. So one seed explores a few faults deeply rather than every fault
shallowly, and adding a site cannot change which sites another seed activates.
`sim.BuggifyDelay` is the same decision followed by a seeded wait. Every site
is off unless a campaign calls `Runtime.SetBuggify(true)` or passes
`Config.Buggify`, which is what keeps the recording and replay comparisons
byte-identical. It is the other half of what FoundationDB means by buggify: the
[tunables](#tunables) a seed draws are the first half, and the two are separate
switches here so a campaign can take either.

The sites, each a fault the code is required to survive without telling its
caller:

| Site | What it does |
| --- | --- |
| `checkpoint/one-page-parts` | Fills a part at one page, so a checkpoint publishes several of them |
| `checkpoint/give-up-on-existing-part` | Gives up on a part a retry of the same publication finds already written |
| `control/slow-write` | Makes one control-record write take seconds |
| `vmmemory/evict-past-a-free-slot` | Takes a victim although the arena has a free slot |
| `vmmigrate/source-busy` | Answers BUSY as a source at its per-peer budget does |

`sim.Probe(ctx, name)` marks a place execution reached, as FoundationDB's
`CODE_PROBE` does: the point of a fault is the code it makes run, and a fault
nobody ever reached is what the whole harness exists to rule out. Probes are
counted on the runtime rather than traced, so registering one changes no
recording. `Runtime.Probes` reports what a run reached and
`Runtime.MissedProbes` what it did not. The registered probes are a fenced
publication, a reconciled lost reply, a compaction rewrite, an eviction during
a publication, and a volume fallback.

`Runtime.Fingerprint` digests everything the simulated dependencies did — which
resource, which operation, to what outcome, over how many bytes, in what order
on each resource, at what simulated moment — and is FoundationDB's unseed. It
sees exactly what a trace event carries, so two writes of one size to different
offsets of one file digest alike; bytes are compared against the models, not
here. `Runtime.WorkFingerprint` drops the order, the moment and the adapter's
own operation numbering, which is what a campaign that does not control
completion order can promise.

`TestScheduledWorldFingerprintIsStable` asserts the strict fingerprint across
two runs of a seed, because that scenario chooses every completion order itself.
`TestSeededTopologyFingerprintIsStable` asserts the work fingerprint instead,
excluding connection attempts and bounding how many it excludes by the number of
regions the topology has: a destination's regions each discover a source that is
being taken away for themselves, and how many of them dial before the first
failure marks the source fallen is a race between goroutines rather than a
choice the seed made. Every other event of that campaign — every object-store
request, every disk operation, every page served, every byte and every outcome —
is identical between two runs of a seed.

The campaign under fault injection is `TestSeededTopologyUnderBuggify`, which
runs the same deployment through the same schedule with the sites on and checks
the same bytes. It requires the campaign to reach every site it is supposed to,
because a site nobody drives is fault injection that proves nothing.

```sh
go test ./internal/simtest -run '^TestSeededTopologyUnderBuggify$' -count=1
go test ./internal/simtest -run '^TestSeededTopologyFingerprintIsStable$' -count=1
go test ./internal/simtest -run '^TestScheduledWorldFingerprintIsStable$' -count=1
SPROUTFS_TEST_SOAK=1 go test ./internal/simtest \
  -run '^TestTheCampaignsReachTheirProbes$' -count=1 -timeout=30m
```

The probe campaign runs twenty-five seeds of the generated schedule with the
sites on, plus four of the two-writer campaign, and requires every probe the
campaigns are registered to cover to have fired. Two of the five registered
probes are covered by no campaign in this repository, and `unreachedProbes` in
`internal/simtest/probe_test.go` names them: the store either answers or fails
outright here, so no conditional write ever loses its reply and is reconciled by
its writer's nonce, and the pagers evict but never while the region a page is
taken from is sealed. A fenced publication came off the list when the two-writer
campaign moved onto the one harness: its takeover happens while the superseded
host is still running, which is what a schedule whose takeovers all follow a
host that is gone cannot reach. The list is asserted in both directions, so a
probe that starts firing is a line to delete and one that stops firing is
coverage lost.

## Negative tests in the tree

Most of the fault catalogue is in the tree as `sim.Bug(ctx, id)` guards at the
site each entry names, enabled by `SPROUTFS_SIM_BUG` as a comma-separated list
that a runtime reads once when it is built. A catalogue entry is then one test
invocation, with no patched source tree, no rebuild, and no entry that silently
stops matching when the code around it moves — three of the fifteen curated
mutations had already stopped matching before they were converted.

`scripts/mutation/guards.json` names the invocation that kills each one:

```sh
SPROUTFS_SIM_BUG=volume-ignore-discard \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=volume-shift-write \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=volume-unbounded-write \
  go test ./internal/volume -run '^TestWriteBatchIsOneGenerationAppliedInOrder$' -count=1
SPROUTFS_SIM_BUG=volume-drop-captured-state \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=checkpoint-part-member-offset \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=checkpoint-reclaim-live-checkpoint \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=migration-accept-wrong-size \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=migration-accept-missing-region \
  go test ./internal/vmmigrate -run '^TestReceiveRefusesAMachineMissingARegion$' -count=1
SPROUTFS_SIM_BUG=migration-corrupt-peer-page \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=migration-corrupt-fallback \
  go test ./internal/simtest -run '^TestSeededTopologyCampaign$' -count=1
SPROUTFS_SIM_BUG=migration-skip-resume \
  go test ./internal/simtest -run '^TestSeededTopologyCampaign$' -count=1
SPROUTFS_SIM_BUG=pager-zero-new-page \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=pager-forget-spill \
  go test ./internal/simtest -run '^TestSeededTopologyUnderBuggify$' -count=1
```

Each must fail. Three of them belong to the generated schedule rather than to
the recorded scenario, because what they break is a fault's own path:
`pager-forget-spill` needs a pager that evicts enough to spill a private page
and fault it back, `migration-corrupt-fallback` needs a destination whose source
is taken away mid-stream, and `migration-skip-resume` needs a migration that was
abandoned after its guest had already stopped. That is the per-site injection
and the ambient faults paying for themselves.

A guard only answers where a runtime is in the context, so a test that kills one
is a test whose harness carries it. Nothing outside these invocations ever sets
`SPROUTFS_SIM_BUG`, and the mutation runner clears every other `SPROUTFS_*`
setting before it runs them.

## Mutation testing

The curated campaign checks whether the scenarios detect specific wrong
behaviors in the real volume, checkpoint, pager and migration code. It runs the
in-tree guards above first, and then the two remaining entries, which are wrong
behaviors in the simulated dependencies themselves and so cannot be guards in
production code: they are still applied as exact source edits to a copy of the
tree. Run it with:

```sh
python3 scripts/mutate-simulation.py --seeds 3
python3 scripts/mutate-simulation.py --seeds 32 --mutant migration-accept-wrong-size
```

The runner copies the current Go sources and test data, including uncommitted
files, into a separate directory. It checks the unmodified baseline, runs each
guard from the baseline binary with `SPROUTFS_SIM_BUG` naming it — no patch and
no rebuild — and then applies one exact source mutation at a time, builds the
affected packages, and runs their scheduled scenarios. For mutations those scenarios miss, it also runs the full
affected package suites. Each mutation is restored before the next starts;
the working checkout is never mutated. The retained directory contains source
hashes, the exact catalogue, build and test logs, and `report.json`.

`killed-guard` means the invocation `guards.json` names failed with the guard
enabled. `killed-scheduled` means a scheduled test failed with the source
mutation installed. `killed-full` means only the broader suite caught it. Build errors, missing
tests, process errors and timeouts are separate outcomes, not successful kills.
The command exits nonzero unless every selected mutation is killed by a
scheduled test and the restored baseline passes. This deliberately exposes
coverage gaps; it is not a command expected to pass merely because a bug is
reproduced. The normal Go suite always asserts correct behavior.

The cases in `scripts/mutation/guards.json` and
`scripts/mutation/simulation.json` are hand-selected semantic
faults, not an exhaustive operator campaign or a representative percentage of
all possible bugs. More seeds explore existing scenarios; they do not add a
missing workload, pressure condition or adversarial input. 

For automatically generated mutations, install
[Gremlins](https://gremlins.dev/latest/install/) and run:

```sh
python3 scripts/mutate-gremlins.py --gremlins /path/to/gremlins --dry-run
python3 scripts/mutate-gremlins.py --gremlins /path/to/gremlins
python3 scripts/mutate-gremlins.py --gremlins /path/to/gremlins --all
```

This separate wrapper also copies the current source. Gremlins generates and
executes the mutations; it does not use the curated catalogue. The default
target is `internal/vmmigrate`, with generated protobuf files excluded.
`GOFLAGS` selects only `TestScheduled.*Reproduces`, and integration mode lets
every scheduled scenario detect mutations in the selected package. `--coverpkg`
includes that package's execution from the host scenario. Use `--package` to
choose another package and `--seeds` to change the scheduled workload count.

`--all` selects every first-party Go package and defaults to the full ordinary
suite in each mutated package. Add `--integration` to run the entire module's
tests for each mutation, or use `--suite scheduled` to select the scheduled
scenarios explicitly. Package-local survivors can be detected by downstream
tests, so their results must be distinguished from integration replays. The
source manifest and package inventory are retained with each run. Go build
constraints still apply: a macOS run does not exercise Linux-only production
code. Gremlins expects a directory; the wrapper uses the module root for this
scope, since passing `./...` to Gremlins can silently produce no mutations.

The wrapper records each real Go test invocation, its mutation and JSON test
events without changing Go's arguments or exit status. Read
`audit-summary.json` alongside the native results: Gremlins v0.6.0 can count
Go compilation errors as killed mutants. The audit separates these from test
failures, empty test selections, process errors and timeouts. It also verifies
that its invocation count agrees with the native execution count.

The [Gremlins campaign](measurements/gremlins-2026-09-11.md) records the pinned
tool version, raw outcomes, survivor checks and a new connection-budget
regression. A surviving mutation needs review: it can expose a missing
assertion, an untested input, or an equivalent behavior. Gremlins' default zero
thresholds also mean a zero exit status does not imply that all mutants died.

For a change in one Go package, start with its ordinary tests, then mutate that
package with the full module available to detect the changes. For example:

```sh
go test ./internal/checkpoint
python3 scripts/mutate-gremlins.py --package internal/checkpoint --suite full \
  --integration --gremlins /path/to/gremlins --output /tmp/checkpoint-mutations
```

Choose a new output directory for every run. Repeat for each changed production
package; for test-only changes, select the production package whose behavior
the tests exercise. `--package` includes subdirectories. Review the surviving
diffs and the audited outcomes: prioritize changes to data integrity, fencing,
authorization, cancellation and resource ownership before incidental boundary
or allocation changes. A regression test must pass on the correct program and
fail with the exact surviving mutation applied. Keep the before/after evidence
separate from the original campaign's score. Record an equivalent mutation's
reasoning rather than adding an assertion about an unobservable implementation
detail, and keep unexplained timeouts as unresolved outcomes.

After a substantial change spanning packages, or periodically before a release,
run `--all --integration` with the full suite. This is intentionally an
occasional campaign; the changed-package command above is the ordinary
development workflow. Include a Linux run when Linux-specific code changes,
and qualify the real pager/client and Firecracker prerequisites before counting
their tests as exercised. The default wrapper clears opt-in `SPROUTFS_*`
settings, so a plain Linux run alone does not establish live VM coverage.
For a prepared Linux environment, pass `--go-test-exec /path/to/launcher`.
The executable launcher receives each compiled test binary and its arguments;
it must set the client and Firecracker asset paths, arrange the required
privileges, and execute the binary without hiding its exit status. The wrapper
retains the launcher and its hash as evidence. Check the baseline's JSON test
events for the intended live tests, and restore any temporary huge-page pool
after the campaign.

`TestPopulationOrdersRelatedIdentitiesWithoutBlockingOtherPagers` gates two
resident faults while overlapping attachments populate related identities.
Both attachments must finish after release, another pager must progress
independently, and population must reuse resident bytes without cold loads.
The test uses `synctest.Wait` to establish blocked phases and asserts liveness
directly. The recorded correct and exact-mutant runs at one and four CPUs validated
these scenarios; they do not enumerate every possible scheduler interleaving.

The control-record reconciliation regressions exercise `Client.Open`,
`Handle.Select` and `Handle.Pin` with object writes that apply but lose their
response, writes that never apply, and a competing writer's takeover. They
assert epoch ownership, fencing, checkpoint selection and the pins a fork left.

## Rust unit tests

The managed-memory client has ordinary unit tests for interval generation
history, control-request lifecycles, protocol frames and descriptor transfer,
mapping budgets, and early region validation. The history tests compare every
queried interval with an independent per-page model. Protocol tests use a
fixed wire fixture and deliberately malformed ancillary input; control tests
check invalid completions, independent regions, cancellation and exhausted
request IDs.

Run these on Linux without KVM, a HugeTLB pool or elevated privileges:

```sh
cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib
```

The crate is Linux-only, so running this command on macOS does not execute
these tests. They complement the real pager/client suites below; they do not
exercise UFFD, page replacement, guest access or the Firecracker integration.

The ignored `session_drop_releases_mappings_and_closes_retained_controls` test
adds a real UFFD/SCM_RIGHTS session handshake and checks teardown while the
embedding process and a retained control handle stay alive. It requires Linux
with permission to create kernel-mode UFFD descriptors, but needs neither KVM
nor a populated HugeTLB pool. Run it from an account with that permission:

```sh
cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib \
  tests::session_drop_releases_mappings_and_closes_retained_controls \
  -- --exact --ignored --test-threads=1
```

Both this test and the ordinary mapping-drop test identify their mappings by
backing or VMA names in `/proc/self/maps`. Checking only whether a freed address
is mapped would race with address reuse by other threads.

The ignored `tests::protocol` tests exercise `Session::connect` and
`Session::run` through a real Unix peer. They cover attachment and READY
validation, the one region a handshake asks for in both RAM and PMEM kinds,
rejected commands and unchanged generations, immediate retries, ordered disjoint
batches, the 1024-run boundary, and budget rejection acknowledgements. The peer
drains UFFD remap events independently of control acknowledgements, as the real
pager does. Malformed batch headers must reject before reading a body; a
write-half-close distinguishes rejection from an erroneous read without relying
on a timeout.

Run them with the same Linux UFFD permissions as the lifecycle test:

```sh
cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib \
  tests::protocol:: -- --ignored --test-threads=1
```

They need no KVM or physical huge pages. The largest boundary case reserves
slightly over 4 GiB of virtual address space; allow at least 8 GiB when imposing
an address-space limit. It does not touch that memory. The mutation replay uses
an 8 GiB address-space limit and a 30-second process deadline.

For Rust changes, run the unit suite first and use cargo-mutants on Linux
(the campaign used version 27.1.0). Mutate a changed source file with:

```sh
cargo mutants --dir rust/sproutfs-vm-memory --file src/control.rs \
  --jobs 2 --jobserver-tasks 4 --timeout 15 --build-timeout 180 \
  --output /tmp/control-mutations
```

Omit `--file` for an occasional full-crate run. Keep `mutants.out/outcomes.json`,
the mutation diffs, test logs and the source revision or snapshot. A unit-suite
survivor in `Session` or the Linux mapping code still needs the real Go/Rust
interop suites: rebuilding the example client from each mutated crate is
essential, since an unchanged client binary cannot test a Rust mutation.

For manual exact-diff replays sharing `CARGO_TARGET_DIR`, clean the local crate
before every build (`cargo clean --manifest-path PATH/Cargo.toml -p
sproutfs-vm-memory`). Compile with `cargo test --lib --no-run
--message-format=json` and require the test executable's compiler-artifact
record to have `fresh: false`. Record its hash along with the exact source and
diff before running it. Copying a crate to a new path while preserving source
timestamps is insufficient: Cargo can reuse the preceding mutant's binary.
This check applies to the unmutated baseline too.

## Real transports

Host serving tests use real TCP over a fault-injectable simulated object store.
A host that cannot reach object storage creates nothing; one that can creates a
VM, checkpoints it, forks it from a fork point over the running parent, and a
second host takes it over by advancing the control record's epoch, which fences
the first. A closed host gives its page-server port back, and the process that
replaces it opens the VM from the control record and the checkpoint that record
selects, owning no local state.

The [Lima suites](vm-memory.md#qualification) provide integration evidence for
the pager, mapping protocol and Firecracker integration. They use real KVM and
real TCP, and they check physical page sharing through pagemap, which no
functional byte test can establish.

## Continuous integration

[`.github/workflows/check.yml`](../.github/workflows/check.yml) runs `just
check` on every push and pull request, split into jobs that run side by side:
the Go gate on Linux and on macOS, `buf lint`, `shellcheck`, and the Rust
crate's `fmt`, `clippy` and unit tests. Every job runs a `just` recipe, so
there is nothing in CI a developer cannot run the same way locally.

Linux is the host whose Go job matters most: it is the only one that compiles
the Linux-only production code, so it covers strictly more than the macOS job
does. Both jobs vet and build for the other operating system as well, because
the module has to keep compiling for the deployment target and for a
development machine. The Linux-only tests that need no privileges — the
platform adapters, the pager wire protocol, the VMM plumbing — run there
because they skip themselves when their opt-in `SPROUTFS_*` variables are
unset.

The [Lima](vm-memory.md#qualification) and GCE suites are out of scope for
GitHub's runners and say so in both workflows. Those runners are virtual
machines without nested virtualization, so real KVM, a HugeTLB pool and a
Firecracker guest are unavailable at any budget; that qualification happens on
Lima or GCE and is recorded under `docs/measurements`. The Rust crate's
`#[ignore]`d UFFD and protocol tests are excluded for the same reason: they
need permission to create kernel-mode userfaultfd descriptors.

### Seed sweeps

A campaign is a seed range rather than a fixed list.
`SPROUTFS_TEST_SOAK=1` enables the extended campaigns,
`SPROUTFS_SOAK_SEED_BASE` and `SPROUTFS_SOAK_SEED_COUNT` select the block of
seeds one process runs, and `SPROUTFS_SOAK_SUMMARY_DIR` names a directory the
per-seed records are appended to. `internal/testsoak` is the plumbing; the
campaigns that use it are all in `internal/simtest`: `TestSeededTopologySoak`,
`TestBuggifiedTopologySoak`, `TestHostCrashSoak`, `TestSwizzleSoak` and
`TestScheduledWorldSoak`.

```sh
just soak            # seeds 1..100
just soak 301 100    # one block of the sweep
just soak 13 1       # one seed, which is how a sweep failure is reproduced
just soak-race 13 1  # the same seed under the race detector
```

[`.github/workflows/soak.yml`](../.github/workflows/soak.yml) runs the sweep
nightly as eight jobs of one block each, and on manual dispatch with a seed
base and a per-block count as inputs. Each job has a 90-minute budget, which is
several times what a 100-seed block costs, so a slow runner is not a red night
while a seed that stops making progress is still killed rather than billed for
six hours. The per-seed summaries are uploaded whether the block passed or
failed, since on a failure they say which seed stopped and what the seeds
before it cost.

Every seed logs one line, and appends the same record as JSON to the summary
directory: the simulated time the run explored, the wall time it took, their
ratio, the number of adapter trace events, and a fingerprint hashing them. The
fingerprint summarizes a run cheaply enough to keep for every seed; the
byte-for-byte determinism checks are the `Reproduces` tests above, not this.

The ratio is the number to watch, and it differs by campaign. A campaign whose
simulated latencies are microseconds and whose work between them is real
computation reports far below one — wall time is dominated by the pagers,
volumes and checkpoints the simulator is driving rather than by any wait — while
one that waits out the deadlines its faults make it wait out reports far above
one. The seeded topology campaign is about a minute of simulated time against a
couple of seconds of wall time, roughly 30x, for that reason; the recorded
scenario is a fraction of a second either way, because the controller resolves
one operation at a time. What a sweep is watching for is a ratio that collapses
well below its own campaign's: that is a real wait that crept into a
virtual-time test.

A block is a single `go test` process, so a seed that deadlocks panics the
bubble and takes the rest of its block with it. Sweep failures are therefore
reproduced one seed at a time.

The first sweep found one in the migration campaign this harness replaced,
since fixed: eight of its first sixteen seeds deadlocked, always at a
`stream-canceled` hop. That fault
cancelled the post-copy stream when the first page reply arrived, and the
campaign waited for that arrival; a hop whose destination needs no page from
the source — the migration was taken over a fresh checkpoint, so the source
holds no unpublished page and no resident one either — never has a page reply,
so the wait never ended and the bubble deadlocked. The three-cycle soak reaches
such a hop where the one-cycle suite does not, which is why seeds 1, 7 and 23
passed in the ordinary run. The fault now stalls the first frame of any kind,
and the empty listing that says the source holds nothing is one, so the cancel
lands on a request the stream is waiting for on every hop; the campaign asserts
that the cancel is what ended the stream, and waits on a timer rather than a
bare receive, so a stall that stops firing names its hop instead of panicking
the bubble. It was reproduced with `just soak 13 1`.

## Format fixtures

Nothing is deployed, so the contract for a store written by another build is
that it is refused with the version it was written under named — not that it is
migrated. Committed fixtures are what hold that contract:

| Fixture | What it holds |
| --- | --- |
| `internal/volume/testdata/deployment-record-4-index-7-part-4`, `deployment-record-4-part-3`, `deployment-record-4-index-6-part-2`, `deployment-record-4-index-5-part-1`, `deployment-record-3-index-5-part-1` | The whole object namespace of a small deployment: a VM with a history of checkpoints and VMM state whose record pins the point it was forked at, and a fork of it whose root names that point's checkpoints. The four older dumps are what the builds before the parts and the index object were split, before the root moved into the last part, before the segmented index and before the pin bump wrote, and their test requires that opening each is refused with the version that moved named — the part layout's for the first, the index object's for the next two, the record's for the last. |
| `internal/control/testdata/record-4`, `record-3`, `record-2` | Two records with pins, at this build's version and at each version committed before it. |
| `internal/checkpoint/testdata/index-7-part-4`, `part-3`, `index-6-part-2`, `index-5-part-1`, `index-4` | The objects of a published checkpoint at this build's formats — its index object and its parts — the objects of the three format sets before it, each refused by the version that moved, and one index table restamped with a version older still. |
| `internal/checkpoint/internal/part/testdata/part-4`, `part-3`, `part-2`, `part-1`, `part-0` | One sealed part holding the VMM state and pages of two volumes, which is everything a part holds; the layout-3 part before it, which also held a segment and the root; the layout-2 part before that, which has a tombstone and no root; the layout-1 part before that, which has no segment member; and a part and table restamped with a version older still. |

The deployment fixture's test loads it into a simulated object store, runs
`CheckDeployment` over it with no allowances, opens every VM, reads every byte
of every volume and the VMM state the selected checkpoint carries, and compares
them against the committed `contents` manifest. The per-format fixtures parse
the committed bytes back into exactly the values they were written from, and
every superseded fixture must be refused with the version named in the error
text.

`-update` writes only the current fixture. A superseded one is never rewritten:
what it is worth is that its bytes are the ones the build of that version
actually wrote.

Every fixture is written by a `-update` flag on its own test:

```sh
go test ./internal/volume -run TestTheCommittedDeploymentFixture -update
go test ./internal/control -run Fixture -update
go test ./internal/checkpoint -run Fixture -update
go test ./internal/checkpoint/internal/part -run Committed -update
```

A format bump keeps every fixture that is already committed and adds one named
for the new version, and leaves the old one's test in place. The old bytes are
worth something only while they are the bytes the old build actually wrote, so
`-update` is for a fixture whose version has not shipped — never for making a
failing old fixture pass.

## Limits

Simulation checks behavior within the modeled failure assumptions and the
executions it explores. Seeds reproduce workloads and dependency choices but do
not control the Go scheduler; explicit gates make critical handoff races
repeatable, and race detection separately checks shared-memory access. Tests
against real platform adapters check filesystem and transport behavior an
in-memory model would miss.

Counters printed by the Lima suites are observations, not machine-independent
assertions, and no measurement here is performance acceptance. Deployment and
guest workloads still have to validate residency budgets, launch latency and
object-storage cost. The collector that releases pins and sweeps what deleted
VMs left pinned has no tests because it has no implementation: what would be its
work is exactly what the deployment check's allowances name, and the scenarios
that pass those allowances are the measure of how much of it a real deployment
would accumulate.
