# Testing

Deterministic simulation is the main correctness tool. The real control-record,
checkpoint and volume implementations run over simulated disks, networks, object
storage and time. Fault injection changes those dependencies. It never
substitutes a simplified storage implementation. Simulation tests use virtual
time. When a random workload fails, it reports its seed, the commands it ran and
the recent simulator events.

`internal/simtest` is the only way to build a simulated deployment. Every
campaign is a schedule, a fault set and an invariant set over the deployment's
`World`. See [One harness](#one-harness) below.

`just check` is the gate that every push must pass. It runs:

- the determinism rule below;
- `gofmt`, `go build` and `go vet` for Linux and macOS;
- `go test ./...`;
- `buf lint` and `shellcheck`;
- the Rust crate's `fmt`, `clippy` and unit tests.

`just test-race` adds the race detector. `just soak` runs one block of the
extended seed sweep; see [Seed sweeps](#seed-sweeps) below. `just test-knobs`
and `just test-soak-knobs` run the campaigns with tunables drawn from each seed.

## One harness

A `simtest.World` is one running deployment. Each host in it:

- is a real `host.Host` inside a `sim.Process`;
- has its own `sim.Disk`, with `PowerLossFaults` on;
- keeps its deadlines on a `sim.Clock` and draws its jitter from a seeded
  `Entropy`;
- reaches the deployment's object store through its own view of the store. A
  kill removes that view first.

Every VM in the world has a simulated VMM process that stores into it, and a
record of the bytes its guest believes it holds. Nothing in the world is a mock
of the code under test. The volume managers, the checkpoint store, the control
records, the pagers, the page servers and the migration coordinator are the real
implementations.

Every campaign drives the world with the same short list of operations:
`Store`, `Checkpoint`, `Keep`, `Migrate`, `Fork`, `CreateFromKept`, `Release`,
`Delete`, `Takeover`, `Kill`, `Restart`, `Shutdown` and `Settle`, plus
`KillDuring`. `KillDuring` runs one
operation on a separate goroutine and removes a host in the middle of it.

One access in four that `Store` draws is a write fault through which the guest
stores nothing. This is because a write fault is not always a store. On x86-64,
a cold read reaches the pager as a write fault. On aarch64, so does a guest
kernel's first execution of a page. The model records nothing for such an
access. So the page must read what the guest last wrote, whether the pager
publishes the copy it made or settles it back onto the page it was copied from.

One access in eight stores a page of zeros, which is what a guest kernel does
to memory it frees. A checkpoint publishes such a page as a hole, and its
retire gives the page back. So the next read of it is answered by what the
volume says about a hole.

Every campaign also checks the same list of requirements:

- `Verify`: no guest reads bytes it never wrote, read through that guest's own
  mappings.
- `VerifyDurable`: the same bytes, read back through the volume.
- `CheckSelected`: every record selects a checkpoint that some writer of that VM
  published.
- `VerifyKept`: every checkpoint a record keeps reads, straight from the store,
  as the pause it was kept at: every page of its disks, and its memory and the
  store counter in its VMM state when it has state. So nothing a kept
  checkpoint reads is reclaimed while it is kept.
- `CheckDeployment`, at the end.

A VM created from a kept checkpoint must read, through its own fault path and
before it stores anything, exactly the pause that checkpoint was kept at. It
reads the memory too and continues at that pause's store counter when it
resumed, and zeroes where the memory was when it booted cold. So it never reads
a byte its parent wrote after the pause. A release must be refused exactly when
the record pins the checkpoint.

When a VM's host is lost, the VM comes back at one of the checkpoints it may
have come back at. These are the last checkpoint that landed, plus every later
checkpoint whose publication was interrupted and may or may not have landed. The
test does not guess which one from the bytes. The control record names the
sequence, and every checkpoint the world took carries the sequence it was
published under. So the record's selection identifies the pause, and the bytes
must then match that pause page for page. The test determines the exact outcome
of an interrupted publication instead of tolerating several outcomes. Two
checkpoints mixed into one VM is a failure in either case. The VMM state that
the checkpoint carries is restored with it. So a takeover that recovers a
volume's bytes without the registers that were running over them is also a
failure.

The campaigns:

| Campaign | What it is |
| --- | --- |
| `TestSeededTopologyCampaign` | The deployment and its concurrent faults, both drawn from the seed. See [Seeded topologies](#seeded-topologies-and-failure-schedules). |
| `TestSeededTopologyUnderBuggify` | The same with the per-site injection on. |
| `TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld` | The kill campaign. See [Losing a host](#losing-a-host). |
| `TestTwoWritersOfOneVMNeverMixAcrossASwizzle` | The two-writer campaign. See [Clogging and swizzling](#clogging-and-swizzling). |
| `TestScheduledWorldReproduces` | The one recorded scenario. See [Overlap scheduling](#overlap-scheduling-experiments). |

Each campaign has a `*Soak` twin that runs over a block of the seed range.
`just soak` runs only these twins.

## One clock, one entropy source

`platform.Clock` is time as a process sees it: `Now`, `Since`, `Sleep`,
`AfterFunc`, `NewTimer` and `NewTicker`. `platform.Entropy` supplies
unpredictable bytes. It provides the writer nonce that reconciles a lost
conditional write, and the jitter that stops a host's VMs from checkpointing in
lockstep. A nil Clock or Entropy in a configuration means the wall clock and the
operating system's random pool. So production code never has to name a clock or
an entropy source that it does not need.

Both are passed through `host.Config`, `SupervisorConfig`, `control.Config`,
`vmmemory.Config` and vmmigrate's `Options` and `PeerConfig`. `volume` and
`checkpoint` take neither, because neither reads a clock or draws a random
value. Their tunable values are in `internal/knobs` instead.

`sim.Clock` is a virtual clock. No time passes on it unless a test advances it.
`Advance` releases the deadlines it passes in deadline order. Deadlines at the
same moment are released in an order that the seed chooses, so two holds that
expire together do not always retire in the order they were armed. One advance
runs the callbacks it released in that order, on a separate goroutine. `Settle`
waits for them. Inside a `testing/synctest` bubble, `synctest.Wait` then reaches
the quiescent point. `sim.Runtime.NewEntropy` is the matching seeded nonce
source. It is reproducible per seed, and each draw is still distinct.

Because of this, tests can reach a deadline that is written in checkpoint
intervals. `TestForkHoldExpiresOnTheSimulatedClock` retires a fork hold at the
deployment's four-interval bound, with no wall-clock wait and no polling. It
requires the parent to take its sealed pages back and to be checkpointable
again. `TestReleasingAForkHoldDisarmsItsDeadline` requires the ordinary release
to leave no deadline armed. Before this, the only test of an expiring handover
set its own fifty-millisecond bound and polled the wall clock for it. So no test
exercised the deadline that a deployment actually runs.

## Tunables

`internal/knobs` holds all of the deployment's tunables:

- the part size and root bound;
- the upload, builder and cache budgets;
- the write and open-VM bounds;
- the checkpoint interval, its jitter share, and the loss window, which bounds
  in time what a host loss can cost;
- the epoch interval;
- the hold, measured in checkpoint intervals;
- the pager's resident, logical and dirty budgets, and its read-ahead,
  write-ahead and I/O bounds;
- a drain's concurrency and its two timeouts.

`Defaults` is what a deployment runs, so a run with the defaults is the same as
a run without knobs. `Validate` refuses a set that the packages would refuse or
that contradicts itself. Examples are a resident arena larger than the metadata
cap that describes it, and a per-VM drain bound above the bound of the whole
drain.

`Randomize` draws a hostile but valid set for one seed, as FoundationDB
randomizes its knobs under buggify. For example:

- A part size of one byte makes every member its own part. So a publication
  goes through all of its interrupted-upload paths at once.
- A dirty budget near its floor makes eviction and spill common instead of rare.
- A hold of one checkpoint interval makes a handover race the interval that
  would have made the handover unnecessary.

One knob in ten keeps its default, so a seed produces a mixture of values
instead of a uniformly tiny deployment. Every draw is keyed by its knob's name,
so adding a knob does not change the values the other knobs take.

The campaigns draw one set per seed when `SPROUTFS_TEST_KNOBS` is set, and they
log the knobs that each seed changed. `just test-knobs` runs `internal/simtest`
this way, and `just test-soak-knobs` runs its extended blocks this way. So far,
drawing knobs has not changed any outcome in the seeds that run.

The opt-in is off by default because the recorded scenario compares its
recordings byte for byte across processes. A seed that also chose its tunables
would be comparing a different run.

A campaign fixes the few knobs that its world is sized around:

- A pager arena must hold every VM of the topology twice, because a fork or a
  migration has the parent's pages and the child's pages on one host at once.
- The dirty budget is fixed with the arena. These campaigns have no interval
  loop to respond to a pager's pressure, because they drive their own
  checkpoints. So a budget smaller than what the guests on one host can dirty
  would stall a store waiting for a checkpoint that nobody will take.
- Read-ahead and write-ahead stay at one page, because the model counts what a
  source holds against what its guest wrote.
- The open-VM bound has a floor at what a takeover holds beside the handle it
  fenced.

The seed chooses every other knob.

A simulated host runs two pagers, as a real host does. One holds its guests'
memory at 4 KiB and the other holds their disks at 2 MiB. Each pager has its own
arena and spill file. The knobs describe one pager, so each pager gets the
values the knobs give. A simulated RAM volume has 512 times fewer bytes than the
disk beside it, and the same number of pages. This keeps a campaign's run time
where it was.

The campaigns constrain the loss window in the same way and for the same reason.
Every seed draws a window between two limits: turning the bound off, and a
window wider than any campaign's clocks reach. These worlds advance a host's
clock only to reach a handover's deadline. A window that fired in one of them
would hold a guest back, waiting for a checkpoint that nobody takes. That ends
in the deliberate stop, which these models do not follow. A campaign requires
two things of the window:

- every host, pager, migration and handoff carries it through every kill and
  every swizzle;
- no recovery rewinds more than the window allows. `VerifyLossWindow` checks
  this beside `VerifyDurable` at every recovery.

The four scenarios in `losswindow_test.go` test what the window does to a
guest. They run the checkpoint loop, so that a store held back by the window has
a loop to ask.

`unchanged_test.go` tests what the settle behind a checkpoint's pause does to a
guest. Two children of one fork point read every page of their memory and their
disk through faults that are all reported as writes. They store nothing, and
then they are checkpointed. Each child must publish no page and hold no private
byte afterwards. Every page it touched must be the parent's page again.

## The no-cheating rule

`internal/testdeterminism` is a `just check` step. It refuses `math/rand`,
`crypto/rand`, and bare `time.Now`, `time.Since`, `time.After`, `time.Sleep`,
`time.NewTimer`, `time.NewTicker`, `time.Tick`, `time.AfterFunc` and
`ctxsync.Sleep` in the non-test code of
`{volume,checkpoint,control,vmmigrate,host,vmmemory}`. This includes
the Linux-only files that this machine does not build. A stray wall-clock read
decides how long a hold lives. A stray `math/rand` call decides which VM
checkpoints first. If a run cannot reproduce either, its seed reports nothing
useful.

The allowlist is keyed by file and by what is read, and each entry has a count.
So a second read added to a file that is already listed is a new decision, and
it must be justified. The list currently has one entry: the two socket deadlines
in the pager's client. The kernel compares these deadlines against its own
clock, and no clock given to this process can be passed to the kernel. The rule
is tested against sources that contain each forbidden item, so it cannot pass by
finding nothing.

## Models

An independent byte-array model tracks the expected state after acknowledged
writes and discards. Reads are compared against it after checkpoints, reopens,
takeovers, object-store outages and fork divergence. The checkpoint package has
its own model. It publishes random dirty sets over several checkpoints with
forks, and checks them by reading every byte back. The pager is checked the same
way, with randomized eviction and randomized captures against independent
models.

A write is durable only after a checkpoint publishes it. So a reopen is compared
against the model at the checkpoint that the control record selects. A
conditional write whose reply was lost has an ambiguous outcome. The tests
require it to be reconciled to the outcome it actually had, and never guessed. A
passing test never uses a known incorrect outcome as its expectation.

Tests assert page identity directly. Locate reports equal identities across a
fork for pages that neither side has written, distinct identities once one side
writes, and private identities for bytes still in an overlay. The pager asserts
that an eligible resident page with a matching identity in the same pager is
mapped without a backing read.

## Failure coverage

Small targeted tests pause execution at publication and ownership handoffs.
They cover conditional-write conflicts, lost successful responses, interrupted
uploads, and concurrent opens or takeover at those points. A checkpoint is
interrupted at every step. Each interruption must leave the parent readable and
the retry successful. A publication fenced by a later writer must not be
selected.

Seeded workloads combine writes, discards, checkpoints and reopens. The
control-record suite covers:

- concurrent opens taking distinct epochs;
- a fenced handle staying fenced;
- a selection that does not advance;
- a lost conditional-write reply reconciled by the writer's nonce;
- a refused write leaving the handle usable;
- a corrupt record being refused.

### Losing a host

A `World` host ends in one of two ways.

`Shutdown` is the orderly close. It publishes a final checkpoint of everything
its handles hold. Its guests give their pages back, and then its process ends.
Its disk is left unchanged.

`Kill` models the machine dying:

1. The store goes first. So nothing the host had in flight can still land, and
   its shutdown publishes nothing.
2. Its guests' VMM processes go with it, because memory is not durable anywhere.
3. The process is then crashed. `PowerLoss` resolves every modification that the
   disk had not synced into bytes that were applied, dropped, torn or garbled.

A killed host is started again in the same test process, on the disk it left
behind. A host keeps no durable local state, and its scratch spill file is
truncated at every pager start. So a restart recovers only what the
deployment's object store holds.

`TestAKilledHostRestartsOnItsOwnDiskAndRewindsToItsLastCheckpoint` in
`internal/simtest` runs one script under all three endings and asserts the
difference between them. A handle write is durable if and only if the close
published it. A guest's stores survive only as far as its last checkpoint,
because only a capture publishes a page.
`TestAKilledHostIsTakenOverByAnotherHostAtItsLastCheckpoint` tests the other
way a VM comes back. A surviving host takes the record over while the dead host
is still down, and the dead host's handle stays fenced.

`TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld` is the seeded
campaign. It is a schedule over one `World`. Every seed removes a host at each
of the four places where a host can be lost:

- in the middle of a checkpoint;
- while it holds a fork point that another host's child is still reading from;
- while it serves the pages of a VM it handed over;
- while it is the host receiving a VM.

The seed chooses the moment inside the operation and the kill mode. The victim
comes back on its own disk. The VM is then recovered by a bystander host or by
that restart, as the seed chooses. The requirements are the same regardless of
what the kill interrupted:

- The VM reads as one whole generation: every byte of a page, and every page of
  the VM. It is read through its volume and again through a guest's own fault
  path. So two mixed checkpoints and a byte that no guest wrote are both
  failures.
- That generation is one of the checkpoints the VM may have come back at. It is
  no older than the last one acknowledged and no newer than the last one
  written.
- The recovered state publishes again and reads the same afterwards, because a
  state that no host can make durable is not a recovery.
- Every kill is in the trace, and the store still holds a deployment
  ([the deployment check](#the-deployment-check) runs at the end of every
  scenario).

On seeds whose drawn moment falls inside the operation, the kill lands inside
it. On the other seeds it lands after the operation. `World.KillDuring` reports
which. When a handover's destination dies, the source's hold on the handover is
the deployment's four checkpoint intervals. The test reaches that deadline by
advancing the `sim.Clock` that the source host keeps it on, not by waiting four
minutes. Across all seeds, the kills must interrupt each of the four scenarios,
and not only land after it finished. A kill that always arrives late tests
nothing that an orderly close does not test. The campaign runs with `Buggify` on
and its runtime on the context. So the kills land around production code that
is also misbehaving. `checkpoint/one-page-parts`, `control/slow-write` and
`vmmigrate/source-busy` fire during it. Eight seeds run in the ordinary suite.
`SPROUTFS_CRASH_SEEDS` selects any other count, and `TestHostCrashSoak` runs a
block of the seed range.

The campaign found three problems:

1. The shared `machine` double emptied its page mapping before it detached the
   memory region. So the pager was still mapping and protecting pages through a map
   that the close was clearing. No earlier test had closed a machine with a
   capture in flight.
2. A fork's child handed to another host has no checkpoint of its own until
   something checkpoints it. So with the interval loop off, opening the child
   failed with `volume: fork's root checkpoint is not published`. The
   destination's own checkpoint publishes it. The campaign takes that checkpoint
   where a deployment's interval loop would.
3. Without `sim.WithRuntime` on the context, the whole fault-injection
   apparatus does nothing. With it, the `migration-corrupt-peer-page`,
   `pager-zero-new-page` and `checkpoint-part-member-offset` guards each kill
   the campaign. This shows that the campaign is not vacuous.

```sh
SPROUTFS_CRASH_SEEDS=500 go test ./internal/simtest \
  -run '^TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld$' -count=1
```

### Parts, index objects and reclamation

A checkpoint's dirty pages upload as parts. The segments they changed and the
root go into the checkpoint's index object. The checkpoint suite requires:

- a part's member table and trailer to describe every member;
- a checkpoint reopened from its index object to match the published
  checkpoint, byte for byte of its root;
- a published checkpoint to consist only of an index object and its parts;
- opening a checkpoint to be a single object-store request, counted through the
  simulated store's trace;
- a checkpoint to write into its index object only the segments it changed, and
  to address every other segment in the index object of the checkpoint that
  wrote it;
- an index object to remain for as long as some root addresses a segment in it,
  and no longer;
- a publication interrupted before its index object to leave a checkpoint that
  reads as absent, and a reference that a later publication can still land
  under;
- a page published as zeroes to leave the segment that names it, with nothing in
  any part for that page.

The suite also covers a multi-part checkpoint, a republication under one
reference that produces byte-identical objects, and a publication whose heap
stays bounded by the part size instead of by the dirty set.

Both the writer and the reader bound a part's table at 1 MiB. One test takes a
checkpoint of 4,000 pages of a volume with the longest allowed name. Its entries
are the widest that a table holds, and together they need more table space than
one part may hold. The test requires the checkpoint to spread its members over
parts whose tables each fit in one read. Reading a part's table is counted
through the simulated store's trace and must be one request. The bound admits
what a part of 4 KiB pages fills to. 3,000 of those widest members fit in one
part, where the earlier 256 KiB bound would have needed four parts. The entry
cost of a full 64 MiB part of 4 KiB pages is measured through the encoder, not
estimated.

The number of requests that a range read costs is counted the same way. A 2 MiB
run of 512 4 KiB pages must be:

- two reads when one checkpoint published the pages: the segment that locates
  them and the one extent their members lie in;
- four reads when three checkpoints published them;
- two reads when half of them were never written.

A gap of five members inside a part is read through, and a gap of a hundred
splits the request. A run across a part boundary is one read per part. A run
across a page-table segment boundary is one read of each segment. A second read
through the cache costs nothing. The same count is required end to end through a
pager. One cold read-ahead run of 512 RAM pages, on a host that has just opened
the VM, is two object-store requests.

Reclamation is tested directly. Selecting a checkpoint:

- deletes, whole, every earlier checkpoint that the new root no longer names;
- keeps a checkpoint from which the root still reads a single page;
- spares a sequence that a fork pinned;
- spares a kept sequence and every checkpoint its root names, and a VM created
  from it reads its bytes after the VM has moved past it;
- never touches the checkpoint that the handle opened on.

Releasing a kept checkpoint deletes it and the checkpoints only it read from,
and leaves nothing the deployment check reports. A release of a kept
checkpoint a VM was created from is refused. A released checkpoint that is
still selected stays until the next selection. A VM's delete takes its kept
checkpoints that no VM was created from.

Tests show that the pin comes before the fork. A fork whose control record
cannot be written still leaves pinned what it would inherit. So does a fork
abandoned before it published a root. A grandchild keeps reading the page that
its grandparent published, after its own parent has rewritten the last page it
inherited. A permanent pin provides this. A fork chain deleted in any order
leaves every survivor readable. A delete over a record that cannot be parsed is
refused, instead of sweeping the checkpoints that the record's pins would have
spared.

Compaction must rewrite the parts of checkpoints that are less than half live.
It must stream those rewrites instead of holding them, and it must move no
segment. A segment that compaction did not otherwise change stays in the index
object of the checkpoint being emptied. So that checkpoint also stays.

### Forks

A fork is a handoff from a running parent, so it publishes nothing. The tests
assert this against the object store. Forking a running VM adds the child's
control record and no other key. A fork closed before its first checkpoint adds
nothing more. Forks are tested for:

- divergence from their parent;
- use before their own first checkpoint is published;
- refusal on an existing identity;
- forks of forks;
- two forks of one parent diverging independently.

While a fork point holds a parent's pages, the parent refuses a second seal. It
also refuses a capture before anything pauses its guest. The parent takes its
pages back when the fork point is retired.

On the parent's host, the children read the sealed pages by page identity. So a
second child maps the first child's resident page without a load. Across hosts,
the child pulls only the pages that no checkpoint of the parent holds. It
publishes them in its own first checkpoint. After that, it survives the loss of
the parent's host. The host suite shows one pause starting several children at
once:

- every child reads the parent's memory as of that pause;
- the interval loop skips the sealed parent instead of failing on it;
- the parent is checkpointed again only after the last child has published.

No page crosses between tenants. The simulated arena gives a read-only file to
the memory regions of one tenant only, in every suite and campaign. A campaign
has two tenants each fork their own template of one image on one host.
`World.Sharing` then finds the pages each tenant's guests share, and no page, or
file of an isolated arena, that the guests of both tenants map.

Capture is tested for:

- returning without waiting for publication;
- capturing nothing when preparation or resume fails;
- a checkpoint publishing the sealed pager pages of every memory region, and retiring
  them once the checkpoint is selected;
- a failed publication handing every sealed page back to the guest.

### Unsynced writes

`sim.DiskConfig.PowerLossFaults` makes a simulated device resolve every
modification made since a file's last successful `Sync`, instead of discarding
all of them. Each write is resolved the way FoundationDB's
`AsyncFileNonDurable` resolves one:

1. At every open, a kill mode is drawn for the file. It is never
   `NoCorruption`.
2. For each 4 KiB device page, a mode no worse than the file's mode is drawn.
3. Each 512 B sector within the page is then applied, dropped, or written with
   its head or its tail replaced by garbage.

The trace names each resolved modification and its outcome: `applied`,
`dropped`, `prefix_truncated` or `sector_garbled`. Every draw is keyed by the
disk, the file and the modification's sequence. So adding a choice for one file
cannot change the choices made for another file.

This is off by default. The rest of the suite assumes a device that restores its
last sync exactly. `SyncDurableProbability` also models a device that
acknowledges a flush it did not perform. It is opt-in and no consumer uses it,
because nothing in sproutfs treats a local file as durable.

Three readers are tested against it:

- The pager's spill file carries a checksum per reservation. The host holds the
  checksum, not the file, because the file is scratch by design. A private page
  whose bytes do not match the checksum is refused with
  `vmmemory.ErrSpillCorrupt` instead of being mapped. Without that check, the
  pager gave the guest a zero byte where the guest had stored its own value.
  This happened under every seed, and the pager reported nothing.
- A VMM's state file cannot carry a checksum of ours. Its format belongs to the
  VMM, and a short file looks the same as a smaller machine. So the file is
  refused through the handle that the power loss invalidated. The test shows
  that the bytes the device kept are not the bytes the VMM wrote.
- Parts go straight to the object store and never touch a disk. So
  `TestATornPartIsRefusedRatherThanReadAsMembers` damages them itself. Across
  48 seeds, 44 parts came back damaged. Each one was refused by its trailer, its
  table or a member envelope. None decoded to bytes that were not written.

### Clogging and swizzling

`Network.Clog(from, to, until)` blocks one directional link until a simulated
moment. After that moment the link carries traffic again, without any call to
heal it. A dial or a send over a clogged link is refused with `ErrUnavailable`,
as a partition refuses them. `Network.Swizzle(addrs, window, random)` gives
every link among a set of addresses its own seeded interval. Each link is
blocked at a moment inside the first half of the window and healed at a moment
inside the second half. So the links do not come back in the order they went
away. Both are schedules read through `Runtime.Now`. `Runtime.Now` uses the
standard library until a clock is injected. Inside a `testing/synctest` bubble,
it is that bubble's virtual clock. So `Swizzle` returns at once and starts no
goroutine that has to undo it. `Network.Clogged(from, to)` reports the link
state. The simulated network does not carry the object store, but naming the
store as an endpoint in this state can still take the store away.

`TestTwoWritersOfOneVMNeverMixAcrossASwizzle` in `internal/simtest` is the
campaign. It swizzles two hosts and the store while one VM is handed over and a
second VM is taken over. The handoff pulls the source's unpublished pages over a
page-server link. That link is separated, healed, dropping, duplicating,
delaying and given new latency. The takeover advances the epoch while either
writer may be unable to reach the store. The requirements are the same whatever
the swizzle does:

- The fenced handle stays fenced and never publishes.
- The selected root names only checkpoints that a writer holding the epoch
  published, and nothing past that epoch.
- Every page the source held reaches the destination instead of being lost to a
  takeover. A first receive fails on every seed, so this is the deployment's
  retry at work: the world retries a failed receive under the same policy as
  the orchestrator, while the source holds the pages
  ([migration](migration.md#a-failed-receive-is-tried-again)).
- A fresh reader sees only the surviving writer's pages.

Sixteen seeds run normally. `SPROUTFS_SWIZZLE_SEEDS` selects any other count,
and `TestSwizzleSoak` runs a block of the seed range.

`DropNext`, `DuplicateNext`, `DelayNext` and `SetLink` make up the
`simtest.DroppedPageServerFrames` fault. It is the only fault that the generated
schedule does not draw. Suppose a frame is dropped on an open connection, and a
guest has a demand fault against the peer that holds the only copy of an
unpublished page. The only possible outcome is that the guest waits for a reply
that never comes. Waiting is the right behavior for that page, because giving up
on it loses the guest's memory. So the drop belongs in a campaign where every
receive is a bounded attempt that is retried, as the deployment retries one
while the source holds the pages. The generated schedule uses
`simtest.DegradedLinks` instead, which is the same kit without the drop.

```sh
SPROUTFS_SWIZZLE_SEEDS=300 go test ./internal/simtest \
  -run '^TestTwoWritersOfOneVMNeverMixAcrossASwizzle$' -count=1
```

## The deployment check

`volume.CheckDeployment(ctx, store, prefix, allow...)` lists the whole object
namespace of one deployment. It reports every way in which the durable state is
inconsistent with itself. It runs at the end of a scenario, after every handle
is closed, regardless of what the scenario did. Every `internal/simtest`
campaign and its soak, the recorded scenario, and the host harness's cleanup all
call it. If a scenario never reaches the check, nobody looks at that scenario's
leftovers.

It requires:

- every control record and every part to parse at the format version that this
  build writes;
- every checkpoint that a selected, pinned or kept root names to exist, with
  the part count and the member bytes that the root recorded;
- every member of those parts to be a page or a state that the part's own VM
  published. Its bytes are billed to the VM whose key holds them, so this is
  what keeps a page billed to the VM that published it when compaction moves
  it;
- every pinned sequence to be a published checkpoint of the VM whose record pins
  it. This is the only thing a pin must agree with. A pin names no holder, and
  no descendant's record names the pin, because nothing releases a pin. So the
  check verifies that what a pin protects is whole: the checkpoint and every
  checkpoint its root names. A grandchild that reads through the pin needs this;
- every object under `vm/<id>/ckpt/` to be reached by one of: some record's
  selected checkpoint, a pinned or kept checkpoint, or a checkpoint that a
  compaction emptied, which is spared for one checkpoint of grace. Naming a checkpoint
  spares all of it. Its index object holds the segments that some root still
  addresses in it, and its parts hold the pages that some root still reads;
- the bill to be the store. `volume.StoredBytes` for each tenant, and for the
  VMs of no tenant, must report exactly what the listing holds under each VM.
  Every key is under some VM, so the bills summed over VMs are every byte in
  the store, each billed once.

Each caller names the classes of leftover it expects, and only those classes go
unreported. Each class is something that a host lost at a particular moment
leaves behind, and that no writer ever returns for. Cleaning it up is a
collector's job, not a writer's. So a scenario that kills hosts names which of
these leftovers it expects, instead of skipping the check:

| Allowance | What it admits |
| --- | --- |
| `AllowSupersededEpoch` | The checkpoints of a writer epoch below the record's. A new handle reclaims only what it published itself, so every takeover leaves the checkpoint it opened on and whatever its fenced predecessor abandoned. |
| `AllowUnpublishedIndex` | A checkpoint whose parts are there and whose index object never landed: a publication interrupted before its commit. |
| `AllowUnreferencedCheckpoint` | A published checkpoint of the record's own epoch that nothing selects or pins: a sweep the store refused. |
| `AllowUnrecordedVM` | Objects under a VM with no control record: a create interrupted before its record, a delete interrupted after it, and, with no host lost at all, the pinned checkpoints a finished delete leaves. Deleting a VM that was ever forked always leaves these. |

The check's first run found two leaks. Both are fixed, not allowed:

- A create never reclaimed the first checkpoint it published, because the
  handle counted nothing as its own until it had published again.
- A fork closed before it ever published its root left its own record behind.
  That made the fork's identity unusable permanently.

## Handovers

A handover is the only operation that can lose a VM's memory, so the campaigns
spend most of their steps on handovers. A migration publishes nothing. The
source stops its guest and gives up its volumes. It keeps serving the pages that
no checkpoint holds until the destination reports that it has them. So a fault
that removes the source costs the VM the pages written since its last
checkpoint. This is the post-copy exposure, not a defect. `World.Migrate`
models this rewind exactly instead of tolerating it.

Every hop in the campaigns makes the same checks:

- The source's handle is refused a store as soon as the handoff is taken.
- The destination's guest restores the VMM state that the source's pause
  captured, and continues at the same store counter.
- The destination's first read is the source's last checkpoint plus the pages
  the source serves. It is read through the destination's own mappings, before
  the destination writes anything.
- A refused migration leaves the guest running where it was, with every memory region
  unsealed, every page writable and its vCPUs running.
- A receive that fails leaves no guest of the VM running on its destination,
  for a migration and for a fork's child. That is what makes a retry sound on
  any host.
- A receive that fails is retried under the deployment's handover policy, on
  the same host or another, until the source no longer holds the pages. Only
  then is the VM given up and reopened at its checkpoint. So a fault that heals
  while the source holds the pages costs the VM nothing.
- A receive in flight and a retry end on the same rule as the orchestrator's,
  `handover.Hold.Gone`. A source the world has lost is no longer listed. A
  source cut off by `simtest.IsolatedHost` is listed and says nothing, so only
  its hold ends the wait. No campaign draws that fault.
  `TestAMigrationWhoseSourceIsCutOffEndsAtItsHold` runs it with a hold shorter
  than the harness's patience, set by `Config.Hold`, so the clock shows which
  one ended the wait.

The recorded scenario adds the layout refusal. A handoff that would truncate a
memory region or map beyond its volume is refused before any guest starts.

A destination runs its guest from the receive on, and its memory regions ask
the source for what they fault until the source is released. `Handover.Meanwhile`
is that window: it runs after the receive and before the release and the close
of the post-copy. Half of the migrations and forks the schedule draws use it.
Every guest stores, a fork's parent among them. The destination is checkpointed,
which publishes and retires what it received. It stores again, and every guest
is read back. A checkpoint whose retire refused to give up a page of the
guest's (`vmmemory.ErrUndroppable`) fails the run, there and everywhere else.
The pager refuses such a retire and keeps the guest's memory, so nothing else
would ever notice it.

`TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes` is the
scenario on its own, for a fork and for a migration. Every page is the
source's at the handoff. The destination stores into all of them, half of
them zeros, and is checkpointed. Its arena is smaller than one guest, so
reading the guest back gives up every page it published and reads it again
while the source still serves. It requires the destination to have read a
page it published (`vmmigrate.ProbePublishedSinceHandoff`), every page to
read the guest's bytes before and after the release, and the volume to hold
the last checkpoint. A fork's parent keeps storing throughout, and none of it
reaches the child. A checkpoint of the parent in the window is refused with
`volume.ErrSealed`, so what its page server serves stays the pause.
[The migration notes](migration.md#the-sources-copy-after-the-destination-publishes)
record what it found.

The scenario checks the peer backing's two answers against each other rather
than against a rule of its own. `testbacking` records a load whose answer
about which pages are the source's own differs from what `Locate` reports for
them, and `Verify` fails the run on it. The pager believes both answers, so a
backing whose answers disagree hands some guest the wrong bytes, whether or
not the run goes on to read that page. The pager's own test double answers
from what the pager told it it took, not from the backing's rule, for the same
reason: it once stripped a page's identity for ever because the backing did,
and so it agreed with the defect instead of catching it.

`vmmigrate`'s own suite keeps the tests that are about the package and
not about a deployment:

- `Done` returns only after every unpublished page is on the destination, and
  never while a page is still only on the source.
- The destination's next checkpoint publishes those pages.
- The source's page server can then be removed entirely.
- Failure to resume.
- Cancellation before a handoff.
- A handoff that waits for a publication that another call already had in
  flight.
- `TestSourceLostAfterHandoffRewindsToTheLastCheckpoint` drops the source's
  pages right after the handoff. It requires the VM to come back at the
  checkpoint that its control record still selects, rewound by exactly the
  writes since that checkpoint.

The simulator's listener-close regressions require queued clients to disconnect
and in-flight dials to reject a closed listener, while accepted connections
remain usable. Otherwise source shutdown could leave a page fault waiting on a
connection that no server will ever accept.

## Seeded topologies and failure schedules

The campaigns that this one replaced permuted a fixed list of faults over a
fixed topology, one fault at a time. `internal/simtest` instead generates both
the topology and the faults from the seed, as FoundationDB's
`CompoundWorkload::addFailureInjection` does. A one-fault-at-a-time campaign
cannot reach a combination such as "the source is partitioned while the store
is unavailable while a second host takes the VM over", however many seeds it
runs.

`simtest.NewTopology(random)` draws the deployment:

- two to four hosts;
- two to four VMs;
- one to three volumes of one to three pages each: memory, and half the time a
  disk and half the time an [ephemeral disk](volumes.md#ephemeral-disks);
- which VMs are forks of which, and which of those forks are creates from one
  of the parent's kept checkpoints;
- the host each VM starts on.

Every simulated host runs an ephemeral pager beside its RAM and PMEM pagers. A
guest's model zeroes its ephemeral disk in every state a checkpoint or a fork
point holds, and keeps it in the state a migration carries. So every campaign
that verifies a recovery, a fork or a migration also requires that no
checkpoint held the disk, that a fork got it zeroed and that a migration moved
it. The deployment check at a campaign's end refuses any root segment or part
member of one. `internal/simtest/ephemeral_test.go` states the same four
requirements as scenarios: never published, lost with its host and at a stop,
reaching neither a local nor a remote fork, and carried by a migration.

A fork's host may be the same as its parent's. That decides whether the child
shares its parent's pages or pulls them from the parent's page server. A failing
seed prints its topology, and a printed topology can be reproduced.

`simtest.Fault` is one thing that goes wrong. It has `Begin`, `End` and
`Holds`. `Holds` is what the fault must leave true after it has ended and the
world has quiesced. A fault is a condition of the world, not a phase of one
migration. So three faults can be active at once.

`simtest.Driver` places every fault of the set in a schedule of 26 steps, at
seeded offsets. One to three faults are active at a time, for a seeded number
of steps. Each step runs a few of every guest's stores, and then one operation:

- a checkpoint;
- a checkpoint that is kept, of the disks alone or with the memory and the VMM
  state;
- a migration, half of them with the destination checkpointed before its
  source is released (see [Handovers](#handovers));
- a fork, or a create from one of the parent's kept checkpoints, warm or cold,
  whether or not the parent runs, and half of the forks with the child
  checkpointed before its parent is released;
- a stop, which may suspend and may keep;
- a start;
- a delete;
- the release of a kept checkpoint;
- a host lost and started again.

Stops and starts are drawn independently, not as pairs. So a stopped VM sits
out as many steps as the seed draws before it is started. During those steps,
the faults land on a VM that exists only as its objects. A host that comes back
without that VM must leave it alone. A stopped VM is the only VM running nowhere
that does not need repair, so the world's `Settle` leaves it alone. Otherwise a
stop whose VM came back by itself at the next step would not be a stop. Every
fault's start and end is recorded in the simulator's trace through
`sim.Trace.Record`, beside the adapter operations it perturbs.

The faults are the migration campaign's ten faults, restated as conditions,
plus the faults that only a generated topology can express:

| Fault | What it is |
| --- | --- |
| `store-unavailable` | Object storage answers nobody. |
| `host-loses-store` | One host cannot reach object storage while every other host can. |
| `partitioned-pages` | Two hosts cannot reach each other's page servers. |
| `swizzled-links` | Every link among the hosts, their page servers and the store is blocked at its own seeded moment and healed at another. |
| `lost-page-replies` | One host's page reply is dropped after the source has already answered it. |
| `stalled-stream` | The first frame one host receives is held until whatever asked for it gives up. |
| `lost-host` | A whole host is taken away at a moment and started again when the fault ends. |
| `refused-stop` | One VM's migration pause fails after its guest has stopped and a memory region is sealed. |
| `refused-start` | One host's half of a receive fails before the guest is started. |
| `degraded-links` | The page-server links duplicate, delay and slow what they carry. |
| `forgotten-releases` | The release after a receive is never made, as if the orchestrator restarted in between. The source keeps its hold until the survey at the next step ends it. |

`dropped-page-server-frames` is the same kit plus `DropNext`, and the schedule
does not draw it. See [Clogging and swizzling](#clogging-and-swizzling) for the
reason, and for the campaign that does draw it.

An operation under a fault is not required to succeed. A migration may be
refused, a checkpoint may not land, and a takeover may be unavailable. But none
of these four things may ever happen:

- **No guest reads bytes it never wrote.** After every step, every page of
  every running VM is read back through that guest's own mappings and compared
  with the bytes the guest stored. A page whose only copy is on a peer that a
  fault has removed cannot be read at all. That is the expected effect of the
  fault, not a wrong byte. So it is reported while a fault is active, and it
  must be readable once all faults have ended.
- **Every VM's selected checkpoint is one it published.** Every takeover checks
  the sequence the new writer inherits against the sequences that this
  campaign's writers published. The end of the run checks every record.
- **`CheckDeployment` passes at the end**, with the allowances that this
  campaign's faults justify: what a host lost at a given moment leaves, what a
  VM deleted after it was forked leaves, and the checkpoint that a sweep could
  not delete because the store refused it.
- **Every seal is reported.** After every step, a VM that a fork point holds
  sealed must have a hold for one of its children in its host's `Serving`. A
  hold whose child runs on that same host must owe nothing. A hold the host did
  not report is one the survey could not end.

The world's `Settle` runs the orchestrator's survey before every step. It
releases every handover a host still holds whose VM exists, and gives up the
rest.

A VM that nobody is running is opened again by a host that can run it. Such a
VM can be a source that could not resume, a handoff that no destination took
while its source held the pages, a post-copy that did not finish, or a host
that was lost. The model then
rewinds to the bytes that were made durable by the checkpoint its record
selects. That rewind is the post-copy exposure, not a defect. So the model
follows it exactly instead of tolerating it.

`TestASourcePartitionedWhileTheStoreIsAwayAndASecondHostTakesOver` tests this
combination directly instead of waiting for a seed to draw it. It covers both
halves: a destination that cannot reach object storage at all, and then a
destination that does take the VM over and only then finds that it cannot fetch
the pages the source still holds.

```sh
go test ./internal/simtest -count=1
just soak 1 100          # the seed sweep, which runs every campaign
```

Once every campaign ran real hosts, the campaigns found the following: two defects
in the system, and then several problems in the simulated world.

- **A deleted VM's identity was reused, and a host's page cache still held the
  pages that name it** (fixed in `8ccfb15`). `checkpoint.Cache` keys a page by
  its identity: the VM, the checkpoint sequence, the volume and the page. It
  does this because a page's bytes are immutable under its identity. A delete
  freed the identity. A VM created again under that name started at the same
  epoch and the same first sequence. So every page of its first checkpoint had
  the same name as the deleted VM's page. A host that still held the dead VM's
  pages served them to the VM that replaced it. The new VM then read bytes that
  none of its guests wrote. The smallest seed is 45: fork `vm-2` from `vm-1`,
  delete `vm-2`, fork `vm-2` again, and the new child's root reads the old
  child's bytes. Seventeen of the first two hundred seeds reach it, all of them
  by forking an identity that had been deleted.

  It was unreachable before, because the earlier campaigns built bare volume
  managers with no page cache. Only a `World` of real `host.Host`s has a page
  cache. Dropping the dead VM's pages from the deleting host's cache fixes
  fourteen of the seventeen seeds. It cannot fix the rest, because any host that
  ever read the old VM's pages holds them under the same name. So the real fix
  is an identity that a delete cannot hand out again. A creation now draws its
  first epoch from the host's entropy, so no two creations share a sequence.
  `Manager.Create` refuses an identity whose `vm/<id>/` namespace still holds
  objects. See [the architecture](architecture.md#identities-and-reclamation).

- **A sweep deleted a checkpoint that a pinned root names.** Reclamation says
  that it spares a pinned checkpoint, "itself and every checkpoint that root
  names". But `protectedBy` asked the root for the checkpoints it *reads*. That
  leaves out the checkpoints that the root's own compaction emptied and that the
  root still names. Suppose a fork is taken on a checkpoint that had just
  compacted another checkpoint empty. The next sweep deleted that emptied
  checkpoint. The pinned root then named an object that could not be fetched.
  `CheckDeployment` reports this as a part that does not read, which is how a
  hole in what a fork inherits appears from outside. It is reachable only where
  a fork point, a compaction and a later sweep line up: ten of the first two
  hundred seeds. The fix spares everything that the pinned root names.
  `TestReclamationSparesThePacksAPinnedIndexOnlyNames` in `checkpoint`
  covers the case.
- A frame held by the stalled-stream fault deadlocked the whole bubble. The
  fault was written for a migration's post-copy, which gives up on its own
  deadline. But the first frame a host receives may be a guest's demand page
  fault, which has no deadline. The guest waited for a page that the fault was
  holding. The step that would have ended the fault was blocked by that guest.
  The hold now ends on the caller's cancellation, on the end of the fault, or on
  a bounded simulated wait. The bounded wait models a connection that died, not
  a harness that stopped.
- A checkpoint's sweep runs after its publication, with the publication lock
  released. So a host that exits right after a checkpoint lands cancels the
  sweep and leaves the replaced checkpoint behind. `CheckDeployment` reports it
  as an unreferenced checkpoint of the record's own epoch. The campaign gives
  its sweeps a moment to run before it closes, as a draining host would. So the
  allowance it keeps for that class covers only the sweeps that its
  store-outage faults actually refused. Each checkpoint the world takes also
  waits for its sweep. A sweep left running into the next step raced that
  step's faults: a fault that failed the store took some of its deletes on one
  run of a seed and none on the next, so the seed did not reproduce its work.
- A frame dropped by `Network.DropNext` on a page-server link hangs the guest
  permanently, and no other outcome is possible. The connection stays open and
  the sender believes it sent the frame, so the reply never comes. A guest's
  demand fault against the peer that holds the only copy of an unpublished page
  waits for the reply instead of giving up. That is the right behavior for that
  page, because giving up on it loses the guest's memory. A reliable
  message-framed connection cannot lose a frame while it stays open. So the
  degraded-links fault does not drop frames. A lost reply really costs the
  connection, and the lost-page-replies fault models that.

## The same discipline on a cluster

These campaigns drive model guests. A `simtest` guest is a mapping plus a model
of what it stored. So every page can be read back and compared after every
step. None of the campaigns runs Firecracker, KVM, the real pager's userfaultfd
path, GCS or Kubernetes. Each of those can lose a page in ways that a simulation
cannot reach.

`scripts/demo-gce.sh soak` is the counterpart on a real cluster, and it has the
same shape. It runs a seeded schedule of forks across two hosts, migrations,
stops and starts, and it kills a host once. After every operation, it asks every
guest whether its memory and its disk still hold the bytes it wrote.
`cmd/sproutfs-guest-witness`, which both guest images carry, makes that question
answerable. It fills a resident buffer and a file of the same size with the
pattern of a `(seed, step)`. The pattern is a pure function of the seed, the
step and the page. So the expectation lives in the script, not in the guest.
This matches a campaign, whose model lives in the world and not in the guest it
checks. So nothing about what a VM should hold crosses a fork, a migration or a
stop, and a guest cannot agree with itself about a wrong value.

The soak ends as a campaign does, with `volume.CheckDeployment` over the whole
object namespace. `sproutfsctl check` runs it through the orchestrator after
every VM has been deleted. At that point only the templates and the checkpoints
that the deleted VMs pinned may remain. There is one template per guest image,
named by the image's bytes. The allowances there are the live deployment's, not
a campaign's:

- a publication in flight;
- a deleted VM's pinned checkpoints;
- a VM's own root and a template import's intermediate checkpoints;
- the superseded epoch that every takeover leaves, including a template whose
  unfinished import a later import recovered.

[The demo notes](demo.md#the-soak) describe the run itself.

The two cannot share faults. A campaign injects a partitioned link or an
unavailable store at a moment it drew. The cluster gets one host killed without
grace, because that is the only fault that a k3s node can reliably be asked for.
The campaigns cover the fault combinations. The soak covers the real VMM, the
real pager and the real object store.

## Overlap scheduling experiments

`sim.NewScheduler(seed)` is the shared controller. Construct it inside a
`testing/synctest` bubble and pass its `Wait` method to `sim.Config.Wait`. Run
the workload, including shutdown of background actors, in another goroutine.
The bubble's parent calls `scheduler.Run(done)`. Stable zero-width waits can
admit workload operations, and `scheduler.Record` captures acknowledged or
recovered bytes. After the run, `scheduler.Recording(runtime.Trace())` returns
independent protobuf streams for comparison. Write them outside virtual time
with `recording.WriteFiles`.

There is one scenario, `TestScheduledWorldReproduces` in `internal/simtest`. It
runs over a `World` of three hosts with a fixed seed. It has three parts, which
match what one deployment does.

The volume part checks complete disk and RAM images against an independent byte
model through:

- concurrent writes;
- checkpoints;
- immutable snapshots;
- a fork over a checkpoint;
- a discard;
- parent and fork divergence;
- failed publication and retry;
- lost publication replies;
- a takeover that fences the handle holding the VM;
- the epoch-major sequence that the takeover then publishes under;
- a cancelled write.

Semantic records contain hashes only after the complete bytes have been
compared with the model.

The handover part moves one guest from host to host through a seeded
permutation of six cases:

- a healthy handoff;
- a layout that would truncate a memory region or map beyond its volume;
- an interval checkpoint before the migration;
- a destination that cannot read the control record;
- a destination that cannot start the guest;
- a source whose frames are held until whatever asked for them gives up.

The world's checks run at every hop: restored VMM counters, the destination's
first read, and a checkpoint of what the destination received, read back
through the volume. The controller orders every guest store and every memory region
seal.

The host part does to whole machines what a deployment does:

- A drain cancelled before it began does no storage or transport work.
- A host is lost under power loss, and its VM is taken over at the checkpoint
  that its record selects.
- The host comes back as a fresh process on the disk it left behind.
- The VM is migrated back onto it.

The transport names the host that a dial comes from, because the simulated
network models a link between hosts. The ordinary host suite covers real
sockets.

```sh
python3 scripts/check-overlap-reproducibility.py --scenario world --seeds 32 --runs 2 --maxprocs 1 2 4 8
```

`sim.WithTask` labels logical callers before concurrent work. The scheduled
harnesses put each memory region's backing loads and authority checks through
`Runtime.Admit`. So two concurrent identical reads are ordered by their logical
caller, not by completion. Disk and object-store operations also enter a gate
before they compete for their shared queue. So spill and page-cache work cannot
acquire the queue in an uncontrolled order. Ordinary adapters require no
controller. Manager shutdown orders handles by VM identity and acquisition, so
it does not depend on Go map order. Disk and dial attempts that are already
canceled do not consume fault plans or I/O IDs.

A destination's requests to its migration source are admitted the same way,
through `vmmigrate.WithAdmission`. Asking the source for pages is a decision,
not an adapter operation. A post-copy stream cancelled between two requests
does one of two things. It either sends the next request and has it refused on
the wire, or it abandons the request before anything is sent. A pooled
connection is taken without dialing, and a dial refuses an already-cancelled
context before it records anything. So without this admission point, nothing is
admitted between the cancellation and the send. Both outcomes are correct,
because every page the stream did not fetch is in the destination's own
checkpoint. The admission point lets the scheduler order the two outcomes. It
does not change either outcome.


The dynamic experiment runs four concurrent clients through three rounds each
of requests, synced disk writes, object publication, replies and object
readback. Accepting a connection starts its handler. A reply causes the next
request, and failed publication creates retry work. One put fails before
application, and one loses its reply after application. Every acknowledged
value is checked against an independent byte model, and the disk is
power-cycled before durable readback. This uses the real simulated network,
disk and object-store implementations. The layer scenarios above extend the
same controller to volumes, pagers, VM handoff and complete hosts.

`sim.Config.Wait` optionally supplies completion timing control. Network sends
pass their configured latency/jitter range without selecting a completion time
first. Fixed-latency disk and object operations expose a +/-25% experimental
window. Accept and receive also gate delivery into application code. Leaving
`Wait` nil preserves ordinary simulation timing and concurrency.

The controller calls `synctest.Wait` until every other goroutine is at a
durable wait. It then selects an overlapping completion window and releases one
operation in a seed-keyed order. It then discovers the work that this completion
created before it chooses again. Cancellation wakes the controller, and the
controller handles it at the next such boundary. This requires stable logical
IDs, and stable admission for requests that share a sequenced resource. It does
not govern arbitrary shared-memory interactions between I/O boundaries.

Capture and compare separate processes with:

```sh
python3 scripts/check-overlap-reproducibility.py --scenario dynamic --runs 10 --maxprocs 1 2 4 8
```

The script prints a retained output directory. Each process runs the requested
seed count (32 by default) in both workload creation orders. Full `.pb` files
contain length-delimited `sproutfs.sim.v1.TraceEvent` messages in observed
order. These include submissions, releases, quiescent completions, task
lifecycles and byte checks. The separate `.adapter.pb` stream preserves every
event in the simulator's existing trace, including its original order, time,
operation, resource, outcome and byte count. These are the adapter's existing
trace points, not every Go scheduler transition.

The `.execution.pb` projection removes only submission/arrival events and
renumbers the remaining sequence. It preserves timestamps, outcomes, payloads
and observed execution order. It never sorts events. The script compares all
three forms byte for byte against the first process, matching seed and creation
order. It writes hashes and the first full-trace difference to `report.json`.
It exits nonzero if any form differs. The normal test requires execution and
adapter traces to match across reversed creation. It never requires arrival
traces to differ.

The regular suite also runs `Test*ReproducesAcrossProcesses` for the dynamic
adapter workload and the scheduled world scenario. Each test launches the same
test binary in three fresh processes: `GOMAXPROCS=1`, `4`, and `4` again. Each
process runs seed 1 in both caller creation orders and checks the existing
independent model. Execution and adapter recordings must then match byte for
byte across processes, including times, outcomes, semantic observations and
event order. Missing or empty recordings fail the test. A mismatch prints the
first differing readable record.

```sh
go test ./platform/sim ./internal/simtest \
  -run 'ReproducesAcrossProcesses$' -count=1
```

These checks enforce deterministic observable execution at the controlled
boundaries. They do not require goroutines to arrive at a wait in the same
order. Full submission traces remain diagnostic, and the stricter standalone
comparison above still reports their differences. Ungated competition that
changes a recorded outcome, adapter operation or scheduling decision fails the
cross-process comparison. Repetition cannot enumerate every possible
interleaving. The larger seed campaigns and the race detector remain
complementary.

The schema is in `platform/sim/proto/sproutfs/sim/v1/trace.proto`.
Regenerate it with `buf generate`. Each `.txt` companion is a readable rendering
decoded from the protobuf file. Events are buffered in the virtual-time bubble
and written after the bubble exits. To retain files from one process, set
`SPROUTFS_OVERLAP_TRACE_DIR` and run
`go test ./platform/sim -run '^TestDynamicOverlapTraceFiles$' -count=1`.
Use a fresh directory for each invocation.

Use `SPROUTFS_OVERLAP_TRACE_SEEDS` to select the count when you invoke a trace
test directly. The earlier two-action, declared-batch experiment is still
available with `--scenario batch` and `TestOverlapPrototypeTraceFiles`. It
requires every operation to register upfront, and it has no separate adapter
trace file. These experiments supplement the ordinary migration fault campaign.
They do not replace its uncontrolled concurrency coverage.

## Fault injection, probes and fingerprints

Three primitives in `platform/sim` reach into the real volume,
checkpoint, control, pager and migration code. All three read the runtime from
the context. A harness puts the runtime there once with `sim.WithRuntime`. In a
context without a runtime, each primitive does a lookup and returns false. This
covers every real deployment and every test that did not request a runtime.

`sim.Buggify(ctx, id, p)` is FoundationDB's two-level switch. A site is
activated once per run with probability 0.25. The draw depends only on the seed
and the site's id. An activated site then fires with probability `p` on each
call. So one seed explores a few faults deeply instead of every fault
shallowly, and adding a site cannot change which sites another seed activates.
`sim.BuggifyDelay` makes the same decision and then waits for a seeded time.
Every site is off unless a campaign calls `Runtime.SetBuggify(true)` or passes
`Config.Buggify`. This keeps the recording and replay comparisons
byte-identical. The sites are the second half of what FoundationDB means by
buggify. The [tunables](#tunables) that a seed draws are the first half. Here
the two are separate switches, so a campaign can use either one.

The code must survive each of these site faults without reporting it to its
caller:

| Site | What it does |
| --- | --- |
| `checkpoint/one-page-parts` | Fills a part at one page, so a checkpoint publishes several of them |
| `checkpoint/give-up-on-existing-part` | Gives up on a part a retry of the same publication finds already written |
| `control/slow-write` | Makes one control-record write take seconds |
| `vmmemory/evict-past-a-free-slot` | Takes a victim although the arena has a free slot |
| `vmmigrate/source-busy` | Answers BUSY as a source at its per-peer budget does |

`sim.Probe(ctx, name)` marks a place that execution reached, as FoundationDB's
`CODE_PROBE` does. A fault is useful only if it makes code run. The harness
exists to rule out faults that no execution ever reached. Probes are counted on
the runtime, not traced, so registering a probe changes no recording.
`Runtime.Probes` reports what a run reached, and `Runtime.MissedProbes` reports
what it did not reach. The registered probes are:

- a fenced publication;
- a reconciled lost reply;
- a compaction rewrite;
- an eviction during a publication;
- a volume fallback;
- a destination loading a page it published while it still asks its source;
- a receive tried again.

`Runtime.Fingerprint` digests everything the simulated dependencies did: the
resource, the operation, the outcome, the number of bytes, the order on each
resource, and the simulated moment. It is FoundationDB's unseed. It sees only
what a trace event carries. So two writes of one size to different offsets of
one file produce the same digest. Bytes are compared against the models, not
here. `Runtime.WorkFingerprint` drops the order, the moment and the adapter's
operation numbering. A campaign that does not control completion order can
guarantee only this fingerprint.

`TestScheduledWorldFingerprintIsStable` asserts the strict fingerprint across
two runs of a seed, because that scenario chooses every completion order.
`TestSeededTopologyFingerprintIsStable` asserts the work fingerprint instead.
It excludes connection attempts, and it bounds how many it excludes by the
number of memory regions in the topology. Each of a destination's memory regions separately
discovers that a source is being removed. How many of them dial before the
first failure marks the source as fallen is a race between goroutines, not a
choice the seed made. Every other event of that campaign is identical between
two runs of a seed: every object-store request, every disk operation, every page
served, every byte and every outcome.

The campaign under fault injection is `TestSeededTopologyUnderBuggify`. It runs
the same deployment through the same schedule with the sites on, and it checks
the same bytes. It requires the campaign to reach every site it is supposed to
reach, because fault injection at a site that nothing drives proves nothing.

```sh
go test ./internal/simtest -run '^TestSeededTopologyUnderBuggify$' -count=1
go test ./internal/simtest -run '^TestSeededTopologyFingerprintIsStable$' -count=1
go test ./internal/simtest -run '^TestScheduledWorldFingerprintIsStable$' -count=1
SPROUTFS_TEST_SOAK=1 go test ./internal/simtest \
  -run '^TestTheCampaignsReachTheirProbes$' -count=1 -timeout=30m
```

The probe campaign runs twenty-five seeds of the generated schedule with the
sites on, plus four seeds of the two-writer campaign. It requires every probe
that the campaigns are registered to cover to have fired. No campaign in this
repository covers two of the six registered probes. `unreachedProbes` in
`internal/simtest/probe_test.go` names them. Here the store either answers or
fails outright, so no conditional write ever loses its reply and is reconciled
by its writer's nonce. The pagers evict, but never while the memory region that a page
is taken from is sealed.

A fenced publication was removed from the list when the two-writer campaign
moved onto the shared harness. That campaign's takeover happens while the
superseded host is still running. A schedule whose takeovers all follow a host
that is gone cannot reach this. The list is asserted in both directions. So a
probe that starts firing must be deleted from the list, and a probe that stops
firing is lost coverage.

## Negative tests in the tree

Most of the fault catalogue is in the tree as `sim.Bug(ctx, id)` guards, at the
site that each entry names. `SPROUTFS_SIM_BUG` enables them as a
comma-separated list, which a runtime reads once when it is built. So a
catalogue entry is one test invocation. It needs no patched source tree and no
rebuild, and no entry silently stops matching when the code around it moves.
Three of the fifteen curated mutations had already stopped matching before they
were converted.

`scripts/mutation/guards.json` names the invocation that kills each guard:

```sh
SPROUTFS_SIM_BUG=volume-ignore-discard \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=volume-shift-write \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=volume-unbounded-write \
  go test ./volume -run '^TestWriteBatchIsOneGenerationAppliedInOrder$' -count=1
SPROUTFS_SIM_BUG=volume-drop-captured-state \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=checkpoint-part-member-offset \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=checkpoint-reclaim-live-checkpoint \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=volume-reclaim-kept \
  go test ./internal/simtest -run '^TestACreateFromAKeptCheckpointReadsThatCheckpoint$' -count=1
SPROUTFS_SIM_BUG=checkpoint-compact-another-vm \
  go test ./volume -run '^TestEachPageIsBilledToTheVMThatPublishedIt$' -count=1
SPROUTFS_SIM_BUG=migration-accept-wrong-size \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=migration-accept-missing-memoryRegion \
  go test ./vmmigrate -run '^TestReceiveRefusesAMachineMissingAMemoryRegion$' -count=1
SPROUTFS_SIM_BUG=migration-corrupt-peer-page \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=migration-corrupt-fallback \
  go test ./internal/simtest -run '^TestSeededTopologyCampaign$' -count=1
SPROUTFS_SIM_BUG=migration-skip-resume \
  go test ./internal/simtest -run '^TestSeededTopologyCampaign$' -count=1
SPROUTFS_SIM_BUG=migration-give-up-first-receive \
  go test ./internal/simtest -run '^TestTwoWritersOfOneVMNeverMixAcrossASwizzle$' -count=1
SPROUTFS_SIM_BUG=migration-ignore-source-hold \
  go test ./internal/simtest -run '^TestAMigrationWhoseSourceIsCutOffEndsAtItsHold$' -count=1
SPROUTFS_SIM_BUG=migration-strip-published-pages \
  go test ./internal/simtest -run '^TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes$' -count=1
SPROUTFS_SIM_BUG=migration-strip-published-holes \
  go test ./internal/simtest -run '^TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes$' -count=1
SPROUTFS_SIM_BUG=migration-ask-for-published-pages \
  go test ./internal/simtest -run '^TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes$' -count=1
SPROUTFS_SIM_BUG=pager-zero-new-page \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=pager-forget-spill \
  go test ./internal/simtest -run '^TestSeededTopologyUnderBuggify$' -count=1
```

Each invocation must fail. Three of them belong to the generated schedule and
not to the recorded scenario, because they break a fault's own path:

- `pager-forget-spill` needs a pager that evicts enough to spill a private page
  and fault it back.
- `migration-corrupt-fallback` needs a destination whose source is removed
  mid-stream.
- `migration-skip-resume` needs a migration that was abandoned after its guest
  had already stopped.

These three show that the per-site injection and the ambient faults are worth
their cost. `migration-give-up-first-receive` belongs to the two-writer
campaign. It gives a handoff up after its first failed receive. The campaign's
separated links fail a first receive on every one of its sixteen seeds, and it
requires the guest to be handed over, not taken over.
`migration-ignore-source-hold` belongs to its own scenario. It keeps a
migration waiting on a listed source that nothing can reach after the source's
hold is over. No campaign cuts a source off while it stays listed, so the
scenario is the only place where the hold is the one evidence left. With the
guard on, the migration ends at the harness's patience instead of at the hold.

The `migration-strip-published-pages`, `migration-strip-published-holes` and
`migration-ask-for-published-pages` guards need a destination that publishes
and retires before its source is released, which no scenario had before
`TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes`. They put
back the peer backing's old rules one at a time. The first is the one that
killed a fan-out's children, and the retire that refuses catches it. The other
two are caught by the backing's two answers disagreeing. Together they are the
rules this scenario replaced, which read a page of zeros back as the bytes the
guest zeroed. The generated schedule catches all three as well. Over seeds 1 to
60 of `TestSeededTopologySoak` and `TestBuggifiedTopologySoak`, 120 runs, the
first fails 88 runs and the second 49. The third fails 3, all buggified,
because it needs a pager that evicts a page the destination published.

The fix to `readIn` that the scenario was also meant to reach is not a guard.
It refuses to share a page whose load calls it the source's own while the
extents name it the volume's. Once a peer backing gives both answers by one
rule, no load says that, so disabling it changes nothing a run can see. Under
the old rules the scenario fails with it and without it, because the load path
hands the guest the same bytes. `TestASourceServedPageTheExtentsCallPublishedIsNotSharedUnderItsIdentity`
in `vmmemory` keeps it tested against a backing that disagrees.

Five guards break the host's side of the Starter contract in `vmmachine`:

```sh
SPROUTFS_SIM_BUG=vmmachine-accept-reserved-load \
  go test ./vmmachine -run '^TestAdversarialStarters$' -count=1
SPROUTFS_SIM_BUG=vmmachine-skip-owner \
  go test ./vmmachine -run '^TestAdversarialStarters$' -count=1
SPROUTFS_SIM_BUG=vmmachine-give-host-path \
  go test ./vmmachine -run '^TestAdversarialStarters$' -count=1
SPROUTFS_SIM_BUG=vmmachine-prepare-twice \
  go test ./vmmachine -run '^TestAdversarialStarters$' -count=1
SPROUTFS_SIM_BUG=vmmachine-skip-peer-check \
  go test ./vmmachine -run '^TestAdversarialStarters$' -count=1
```

`TestAdversarialStarters` runs a fake VMM, not Firecracker, but it runs only on
Linux and as root, because it gives the process's directory to another user.
On a Mac, run these through the Lima instance, as the next section shows.

A guard acts only where the context carries a runtime. So a test that kills a
guard must use a harness that carries a runtime. Nothing outside these
invocations sets `SPROUTFS_SIM_BUG`, and the mutation runner clears every other
`SPROUTFS_*` setting before it runs them.

## Mutation testing

The curated campaign checks whether the scenarios detect specific wrong
behaviors in the real volume, checkpoint, pager and migration code. It first
runs the in-tree guards above. It then runs the two remaining entries. These
are wrong behaviors in the simulated dependencies, so they cannot be guards in
production code. They are still applied as exact source edits to a copy of the
tree. Run the campaign with:

```sh
python3 scripts/mutate-simulation.py --seeds 3
python3 scripts/mutate-simulation.py --seeds 32 --mutant migration-accept-wrong-size
```

The runner copies the current Go sources and test data, including uncommitted
files, into a separate directory. It checks the unmodified baseline. It runs
each guard from the baseline binary with `SPROUTFS_SIM_BUG` naming it, with no
patch and no rebuild. It then applies one exact source mutation at a time,
builds the affected packages, and runs their scheduled scenarios. For mutations
that those scenarios miss, it also runs the full affected package suites. Each
mutation is restored before the next one starts, and the working checkout is
never mutated. The retained directory contains source hashes, the exact
catalogue, build and test logs, and `report.json`.

`--go-test-exec` runs each test binary through a launcher, and `GOOS` and
`GOARCH` in the environment build the binaries for it. The `vmmachine` guards
need Linux and root, so from a Mac they run in the Lima instance:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 python3 scripts/mutate-simulation.py \
  --go-test-exec scripts/mutation/lima-go-test-exec.sh \
  --output "$HOME/.cache/vmmachine-mutations" \
  --mutant vmmachine-accept-reserved-load --mutant vmmachine-skip-owner \
  --mutant vmmachine-give-host-path --mutant vmmachine-prepare-twice \
  --mutant vmmachine-skip-peer-check
```

The output directory must be one the instance mounts. The baseline then runs
the whole `vmmachine` suite there. Its Firecracker tests skip, because the
runner clears the `SPROUTFS_*` settings that name the Firecracker assets.

`killed-guard` means the invocation that `guards.json` names failed with the
guard enabled. `killed-scheduled` means a scheduled test failed with the source
mutation installed. `killed-full` means only the broader suite caught it. Build
errors, missing tests, process errors and timeouts are separate outcomes, not
successful kills. The command exits nonzero unless a scheduled test kills every
selected mutation and the restored baseline passes. The command is designed to
expose coverage gaps. Reproducing a bug is not enough for it to pass. The
normal Go suite always asserts correct behavior.

The cases in `scripts/mutation/guards.json` and
`scripts/mutation/simulation.json` are hand-selected semantic faults. They are
not an exhaustive operator campaign or a representative percentage of all
possible bugs. More seeds explore the existing scenarios. They do not add a
missing workload, pressure condition or adversarial input.

For automatically generated mutations, install
[Gremlins](https://gremlins.dev/latest/install/) and run:

```sh
python3 scripts/mutate-gremlins.py --gremlins /path/to/gremlins --dry-run
python3 scripts/mutate-gremlins.py --gremlins /path/to/gremlins
python3 scripts/mutate-gremlins.py --gremlins /path/to/gremlins --all
```

This separate wrapper also copies the current source. Gremlins generates and
executes the mutations. It does not use the curated catalogue. The default
target is `vmmigrate`, with generated protobuf files excluded.
`GOFLAGS` selects only `TestScheduled.*Reproduces`, and integration mode lets
every scheduled scenario detect mutations in the selected package. `--coverpkg`
includes that package's execution from the host scenario. Use `--package` to
choose another package, and `--seeds` to change the scheduled workload count.

`--all` selects every first-party Go package, and defaults to the full ordinary
suite in each mutated package. Add `--integration` to run the entire module's
tests for each mutation, or use `--suite scheduled` to select the scheduled
scenarios explicitly. Downstream tests can detect package-local survivors, so
their results must be distinguished from integration replays. The source
manifest and package inventory are retained with each run. Go build constraints
still apply, so a macOS run does not exercise Linux-only production code.
Gremlins expects a directory. The wrapper uses the module root for this scope,
because passing `./...` to Gremlins can silently produce no mutations.

The wrapper records each real Go test invocation, its mutation and its JSON
test events, without changing Go's arguments or exit status. Read
`audit-summary.json` alongside the native results, because Gremlins v0.6.0 can
count Go compilation errors as killed mutants. The audit separates these from
test failures, empty test selections, process errors and timeouts. It also
verifies that its invocation count agrees with the native execution count.

The [Gremlins campaign](measurements/gremlins-2026-09-11.md) records the pinned
tool version, raw outcomes, survivor checks and a new connection-budget
regression. A surviving mutation needs review. It can expose a missing
assertion, an untested input, or an equivalent behavior. Gremlins' default zero
thresholds also mean that a zero exit status does not imply that all mutants
died.

For a change in one Go package, start with its ordinary tests. Then mutate that
package with the full module available to detect the changes. For example:

```sh
go test ./checkpoint
python3 scripts/mutate-gremlins.py --package checkpoint --suite full \
  --integration --gremlins /path/to/gremlins --output /tmp/checkpoint-mutations
```

Choose a new output directory for every run. Repeat for each changed production
package. For test-only changes, select the production package whose behavior
the tests exercise. `--package` includes subdirectories. Review the surviving
diffs and the audited outcomes. Prioritize changes to data integrity, fencing,
authorization, cancellation and resource ownership over incidental boundary or
allocation changes. A regression test must pass on the correct program and
fail with the exact surviving mutation applied. Keep the before/after evidence
separate from the original campaign's score. For an equivalent mutation, record
the reasoning instead of adding an assertion about an unobservable
implementation detail. Keep unexplained timeouts as unresolved outcomes.

After a substantial change that spans packages, or periodically before a
release, run `--all --integration` with the full suite. This is intentionally an
occasional campaign. The changed-package command above is the ordinary
development workflow. Include a Linux run when Linux-specific code changes.
Qualify the real pager/client and Firecracker prerequisites before you count
their tests as exercised. The default wrapper clears opt-in `SPROUTFS_*`
settings, so a plain Linux run alone does not establish live VM coverage.
For a prepared Linux environment, pass `--go-test-exec /path/to/launcher`.
The executable launcher receives each compiled test binary and its arguments.
It must set the client and Firecracker asset paths, arrange the required
privileges, and execute the binary without hiding its exit status. The wrapper
retains the launcher and its hash as evidence. Check the baseline's JSON test
events for the intended live tests. Restore any temporary huge-page pool after
the campaign.

`TestPopulationOrdersRelatedIdentitiesWithoutBlockingOtherPagers` gates two
resident faults while overlapping attachments populate related identities.
Both attachments must finish after release, another pager must progress
independently, and population must reuse resident bytes without cold loads.
The test uses `synctest.Wait` to establish blocked phases, and asserts liveness
directly. The recorded correct and exact-mutant runs at one and four CPUs
validated these scenarios. They do not enumerate every possible scheduler
interleaving.

The control-record reconciliation regressions exercise `Client.Open`,
`Handle.Select` and `Handle.Pin` with three cases: object writes that apply but
lose their response, writes that never apply, and a competing writer's
takeover. They assert epoch ownership, fencing, checkpoint selection and the
pins that a fork left.

## Rust unit tests

The managed-memory client has ordinary unit tests for interval generation
history, control-request lifecycles, protocol frames and descriptor transfer,
mapping budgets, and early memory region validation. The history tests compare every
queried interval with an independent per-page model. Protocol tests use a
fixed wire fixture and deliberately malformed ancillary input. Control tests
check invalid completions, independent memory regions, cancellation and exhausted
request IDs.

Run these on Linux without KVM, a HugeTLB pool or elevated privileges:

```sh
cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib
```

The crate is Linux-only, so running this command on macOS does not execute
these tests. They complement the real pager/client suites below. They do not
exercise UFFD, page replacement, guest access or the Firecracker integration.

The ignored `session_drop_releases_mappings_and_closes_retained_controls` test
adds a real UFFD/SCM_RIGHTS session handshake. It checks teardown while the
embedding process and a retained control handle stay alive. It requires Linux
with permission to create kernel-mode UFFD descriptors. It needs neither KVM
nor a populated HugeTLB pool. Run it from an account with that permission:

```sh
cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib \
  tests::session_drop_releases_mappings_and_closes_retained_controls \
  -- --exact --ignored --test-threads=1
```

Both this test and the ordinary mapping-drop test identify their mappings by
backing or VMA names in `/proc/self/maps`. A check of only whether a freed
address is mapped would race with address reuse by other threads.

The ignored `tests::protocol` tests exercise `Session::connect` and
`Session::run` through a real Unix peer. They cover:

- attachment and READY validation;
- the one memory region that a handshake asks for, in both RAM and PMEM kinds;
- rejected commands and unchanged generations;
- immediate retries;
- ordered disjoint batches;
- the 1024-run boundary;
- budget rejection acknowledgements;
- files: a read-only file mapped private, grown and dropped, a span of runs
  from two files, and every file, drop and descriptor the client refuses.

The peer drains UFFD remap events independently of control acknowledgements, as
the real pager does. Malformed batch headers must be rejected before a body is
read. A write-half-close distinguishes rejection from an erroneous read without
relying on a timeout.

Run them with the same Linux UFFD permissions as the lifecycle test:

```sh
cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib \
  tests::protocol:: -- --ignored --test-threads=1
```

They need no KVM or physical huge pages. The largest boundary case reserves
slightly over 4 GiB of virtual address space. Allow at least 8 GiB when you
impose an address-space limit. The test does not touch that memory. The
mutation replay uses an 8 GiB address-space limit and a 30-second process
deadline.

For Rust changes, run the unit suite first, and then use cargo-mutants on Linux
(the campaign used version 27.1.0). Mutate a changed source file with:

```sh
cargo mutants --dir rust/sproutfs-vm-memory --file src/control.rs \
  --jobs 2 --jobserver-tasks 4 --timeout 15 --build-timeout 180 \
  --output /tmp/control-mutations
```

Omit `--file` for an occasional full-crate run. Keep `mutants.out/outcomes.json`,
the mutation diffs, the test logs and the source revision or snapshot. A
unit-suite survivor in `Session` or in the Linux mapping code still needs the
real Go/Rust interop suites. The example client must be rebuilt from each
mutated crate, because an unchanged client binary cannot test a Rust mutation.

For manual exact-diff replays that share `CARGO_TARGET_DIR`, clean the local
crate before every build (`cargo clean --manifest-path PATH/Cargo.toml -p
sproutfs-vm-memory`). Compile with `cargo test --lib --no-run
--message-format=json`, and require the test executable's compiler-artifact
record to have `fresh: false`. Record its hash, along with the exact source and
diff, before you run it. Copying a crate to a new path while preserving source
timestamps is not enough, because Cargo can reuse the preceding mutant's
binary. This check applies to the unmutated baseline too.

## Real transports

Host serving tests use real TCP over a fault-injectable simulated object store.
A host that cannot reach object storage creates nothing. A host that can reach
it creates a VM, checkpoints it, and forks it from a fork point over the running
parent. A second host then takes the VM over by advancing the control record's
epoch, which fences the first host. A closed host gives its page-server port
back. The process that replaces it opens the VM from the control record and the
checkpoint that the record selects, and owns no local state.

The [Lima suites](vm-memory.md#qualification) provide integration evidence for
the pager, the mapping protocol and the Firecracker integration. They use real
KVM and real TCP. They check physical page sharing through pagemap, which no
functional byte test can establish.

## Continuous integration

[`.github/workflows/check.yml`](../.github/workflows/check.yml) runs `just
check` on every push and pull request. It is split into jobs that run in
parallel:

- the Go gate on Linux and on macOS;
- `buf lint`;
- `shellcheck`;
- the Rust crate's `fmt`, `clippy` and unit tests.

Every job runs a `just` recipe, so a developer can run every CI step the same
way locally.

For the Go job, Linux is the most important host. It is the only host that
compiles the Linux-only production code, so it covers strictly more than the
macOS job. Both jobs also vet and build for the other operating system, because
the module must keep compiling for the deployment target and for a development
machine. The Linux-only tests that need no privileges run there: the platform
adapters, the pager wire protocol and the VMM plumbing. They can run there
because they skip themselves when their opt-in `SPROUTFS_*` variables are
unset.

The [Lima](vm-memory.md#qualification) and GCE suites are out of scope for
GitHub's runners, and both workflows say so. Those runners are virtual machines
without nested virtualization. So real KVM, a HugeTLB pool and a Firecracker
guest are unavailable at any budget. That qualification happens on Lima or GCE
and is recorded under `docs/measurements`. The Rust crate's `#[ignore]`d UFFD
and protocol tests are excluded for the same reason: they need permission to
create kernel-mode userfaultfd descriptors.

### Seed sweeps

A campaign is a seed range, not a fixed list. `SPROUTFS_TEST_SOAK=1` enables
the extended campaigns. `SPROUTFS_SOAK_SEED_BASE` and `SPROUTFS_SOAK_SEED_COUNT`
select the block of seeds that one process runs. `SPROUTFS_SOAK_SUMMARY_DIR`
names a directory that the per-seed records are appended to.
`internal/testsoak` is the plumbing. The campaigns that use it are all in
`internal/simtest`: `TestSeededTopologySoak`, `TestBuggifiedTopologySoak`,
`TestHostCrashSoak`, `TestSwizzleSoak` and `TestScheduledWorldSoak`.

```sh
just soak            # seeds 1..100
just soak 301 100    # one block of the sweep
just soak 13 1       # one seed, which is how a sweep failure is reproduced
just soak-race 13 1  # the same seed under the race detector
```

[`.github/workflows/soak.yml`](../.github/workflows/soak.yml) runs the sweep
nightly as eight jobs of one block each. It also runs on manual dispatch, with
a seed base and a per-block count as inputs. Each job has a 90-minute budget,
which is several times what a 100-seed block costs. So a slow runner does not
fail the night, but a seed that stops making progress is still killed instead
of running for six hours. The per-seed summaries are uploaded whether the block
passed or failed. On a failure, they show which seed stopped and what the
earlier seeds cost.

Every seed logs one line and appends the same record as JSON to the summary
directory. The record contains:

- the simulated time the run explored;
- the wall time the run took;
- the ratio of the two;
- the number of adapter trace events;
- a fingerprint that hashes those events.

The fingerprint is cheap enough to keep for every seed. The byte-for-byte
determinism checks are the `Reproduces` tests above, not this fingerprint.

The ratio is the number to watch, and it differs by campaign. Some campaigns
have simulated latencies of microseconds and do real computation between them.
These report a ratio far below one. Their wall time is dominated by the pagers,
volumes and checkpoints that the simulator drives, not by waits. A campaign that
waits out the deadlines its faults impose reports a ratio far above one. For
that reason, the seeded topology campaign is about a minute of simulated time
against a couple of seconds of wall time, roughly 30x. The recorded scenario
takes a fraction of a second either way, because the controller resolves one
operation at a time. A sweep watches for a ratio that drops well below its
campaign's usual value. That drop means a real wait has entered a virtual-time
test.

A block is a single `go test` process. So a seed that deadlocks panics the
bubble and takes the rest of its block with it. Sweep failures are therefore
reproduced one seed at a time.

The first sweep found a deadlock in the migration campaign that this harness
replaced. It has since been fixed. Eight of that campaign's first sixteen seeds
deadlocked, always at a `stream-canceled` hop. That fault cancelled the
post-copy stream when the first page reply arrived, and the campaign waited for
that arrival. Some hops need no page from the source: the migration was taken
over a fresh checkpoint, so the source holds no unpublished page and no resident
page. Such a hop never gets a page reply. So the wait never ended, and the
bubble deadlocked. The three-cycle soak reaches such a hop, and the one-cycle
suite does not. This is why seeds 1, 7 and 23 passed in the ordinary run. The
fault now stalls the first frame of any kind. The empty listing that says the
source holds nothing is such a frame. So on every hop, the cancel lands on a
request that the stream is waiting for. The campaign asserts that the cancel is
what ended the stream. It waits on a timer instead of a bare receive, so a
stall that stops firing names its hop instead of panicking the bubble. The
deadlock was reproduced with `just soak 13 1`.

## Format fixtures

Nothing is deployed. So the contract for a store written by another build is
that the store is refused, with the version it was written under named. The
store is not migrated. Committed fixtures enforce that contract:

| Fixture | What it holds |
| --- | --- |
| `volume/testdata/deployment-record-5-index-8-part-4`, `deployment-record-4-index-8-part-4`, `deployment-record-4-index-7-part-4`, `deployment-record-4-part-3`, `deployment-record-4-index-6-part-2`, `deployment-record-4-index-5-part-1`, `deployment-record-3-index-5-part-1` | The whole object namespace of a small deployment: a VM with a history of checkpoints and VMM state whose record keeps its first capture and pins the point it was forked at, and a fork of it whose root names that point's checkpoints. The older dumps are what the builds before kept checkpoints, before the page size in the root, before the parts and the index object were split, before the root moved into the last part, before the segmented index and before the pin bump wrote. Opening a VM reads its control record first, and every older dump's record is below format 5, so their test requires that opening each is refused with its record's version named. |
| `control/testdata/record-5`, `record-4`, `record-3`, `record-2` | Two records with pins, one of which keeps two checkpoints, at this build's version and at each version committed before it. |
| `checkpoint/testdata/index-7-part-4`, `part-3`, `index-6-part-2`, `index-5-part-1`, `index-4` | The objects of a published checkpoint at this build's formats — its index object and its parts — the objects of the three format sets before it, each refused by the version that moved, and one index table restamped with a version older still. |
| `checkpoint/internal/part/testdata/part-4`, `part-3`, `part-2`, `part-1`, `part-0` | One sealed part holding the VMM state and pages of two volumes, which is everything a part holds; the layout-3 part before it, which also held a segment and the root; the layout-2 part before that, which has a tombstone and no root; the layout-1 part before that, which has no segment member; and a part and table restamped with a version older still. |

The deployment fixture's test loads the fixture into a simulated object store
and runs `CheckDeployment` over it with no allowances. It opens every VM. It
reads every byte of every volume and the VMM state that the selected checkpoint
carries, and compares them with the committed `contents` manifest. The
per-format fixtures parse the committed bytes back into the values they were
written from. Every superseded fixture must be refused with the version named in
the error text.

`-update` writes only the current fixture. A superseded fixture is never
rewritten, because its value is that its bytes are the ones the build of that
version actually wrote.

Every fixture is written by a `-update` flag on its own test:

```sh
go test ./volume -run TestTheCommittedDeploymentFixture -update
go test ./control -run Fixture -update
go test ./checkpoint -run Fixture -update
go test ./checkpoint/internal/part -run Committed -update
```

A format bump keeps every fixture that is already committed, adds one named for
the new version, and keeps the old fixture's test. The old bytes are useful only
while they are the bytes that the old build actually wrote. So `-update` is for
a fixture whose version has not shipped. Never use it to make a failing old
fixture pass.

## Limits

Simulation checks behavior within the modeled failure assumptions and the
executions it explores. Seeds reproduce workloads and dependency choices, but
they do not control the Go scheduler. Explicit gates make critical handoff
races repeatable, and race detection separately checks shared-memory access.
Tests against real platform adapters check filesystem and transport behavior
that an in-memory model would miss.

Counters printed by the Lima suites are observations, not machine-independent
assertions. No measurement here is performance acceptance. Deployment and guest
workloads still have to validate residency budgets, launch latency and
object-storage cost. The collector that releases pins and sweeps what deleted
VMs left pinned has no tests, because it has no implementation. Its work would
be what the deployment check's allowances name. The scenarios that pass with
those allowances measure how much of that work a real deployment would
accumulate.
