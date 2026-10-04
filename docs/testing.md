# Testing

Deterministic simulation is the main correctness tool. The real control-record,
checkpoint and volume implementations run over simulated disks, networks, object
storage and time. Fault injection changes those dependencies. It never
substitutes a simplified storage implementation. Simulation tests use virtual
time. When a random workload fails, it reports its seed, the commands it ran and
the recent simulator events.

A simulated file is sparse, as a file on a real filesystem is. A hole costs the
test process no memory. A pager allocates its spill file to hold every dirty
page it may keep. The simulated filesystem counts that space against its
total, but every allocated page that was never written shares one zero page,
so it costs the test process almost no memory either.

`internal/simtest` is the only way to build a simulated deployment. Every
campaign is a schedule, a fault set and an invariant set over the deployment's
`World`. See [One harness](#one-harness) below.

`just check` is the gate that every push must pass. It runs:

- the determinism rule below;
- `gofmt`, `go build` and `go vet` for Linux and macOS;
- `go test ./...`;
- `buf lint` and `shellcheck`;
- the Rust crate's `fmt`, `clippy` and unit tests;
- the TLA+ specs, model-checked with TLC (see [Model checking](#model-checking)).

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
records, the pagers, the peer servers and the migration coordinator are the real
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
`vmmemory.Config` and vmmigrate's `Options` and `PeerConfig`. `volume` takes
neither, because it reads no clock and draws no random value. `checkpoint`
takes only an entropy source, in `CacheConfig`, from which a new page cache
disk draws its identity; the host passes its own. Their tunable values are in
`internal/knobs` instead.

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
`{volume,checkpoint,control,vmmigrate,host,vmmemory,internal/handover,resource,rank,membership,stripe}`. This includes
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
`peer/busy` fire during it. Eight seeds run in the ordinary suite.
`SPROUTFS_CRASH_SEEDS` selects any other count, and `TestHostCrashSoak` runs a
block of the seed range.

The fork destination's scenario is the narrow one. A kill is inside it only
when it lands after the child's receive returned and before its root landed,
a fraction of a millisecond at the end of a fork of about 4 ms. A moment drawn
from the fork's start reached that span on one seed in a dozen or fewer, and
which one moved with the scheduler. So that kill is drawn from
`World.ChildReceived`, the moment the receive returned, over 2 ms: about half
the seeds kill while the root publishes, and the rest after it landed.

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
stays bounded by the part size and the encoders instead of by the dirty set.

A publication's pace is stated in simulated time. `sim.Config.Compute` prices
an encode (`blob.WorkEncode`) in bytes a second, spent while it holds its
encoder, and `sim.Work` counts the encodes and the most at once.
`TestAPublicationEncodesAsManyPagesAtOnceAsItHasEncoders` publishes 64 pages of
10 ms each through four encoders: four encode at once, and the last part lands
exactly 160 ms and one part's PUT after the commit began, not 640 ms.
`TestAPublicationKeepsEveryUploadSlotBusy` publishes 16 free parts through
four upload slots: four PUTs at once, and the last part lands after exactly
four rounds of one part's PUT. Both check that the index object's PUT began
only after the last part landed. `TestAPublicationDoesTheSameWorkUnderAShake`
requires the same fingerprint and the same time under three shakes. The
guards `checkpoint-encode-one-batch-at-a-time` and
`checkpoint-upload-one-part-at-a-time` serialize the encodes and the uploads,
and each fails its test.

A publication takes each batch's encoder itself, in the order it filled the
batches. When each batch's goroutine took its own, the Go scheduler chose
which batch got a free encoder: on one processor it runs the goroutine started
last first, so a later batch took the encoder an earlier one needed, and the
publication waited a whole encode for the earlier one. The parts then landed
10 to 30 ms late, in 22 runs of 200 on the default processors and in most
runs on one. `sim.Runtime.WorkPieces` reports
the task and the instant each piece of priced work began, and the publication
names each batch's encode as a task in a simulation.
`TestAPublicationAdmitsItsBatchesToTheEncodersInOrder` publishes on one
processor and on two and requires no batch to begin encoding before one
filled earlier. The guard `checkpoint-encode-admitted-in-any-order` restores
the old admission and fails it.

The pull tests that read through the cluster run in a synctest bubble. A read
of the cluster reads the store as well once its bound has passed, 10 ms by
default, and on the wall clock a loaded machine sometimes took longer than
that to read the fixture's own disk: the read then also asked the store, and
the test counted a request it wanted none of. `pullAndRead` now also requires
no read to have asked the store that way.

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

A fork is a handoff from a running parent, so it publishes nothing before it
returns. The tests assert this against the object store: forking a running VM
adds the child's control record, and behind it the parent publishes the point
once. A child's first checkpoint uploads no part of what it inherited, a fan-out
of three children uploads those pages once, and a child reads none of them back
after the seal ends. A fork closed before its first checkpoint adds nothing of
its own. Forks are tested for:

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

No page crosses between tenants, except a public template's. The simulated
arena gives a read-only file to the memory regions of one tenant only, in
every suite and campaign, except the public file, which every region is given
as file 2. A campaign has two tenants each fork their own template of one
image on one host. `World.Sharing` then finds the pages each tenant's guests
share, and no page, or file of an isolated arena, that the guests of both
tenants map. Another has two tenants create VMs from one public template.
`World.Sharing` counts the public pages they share and requires every other
page to be one tenant's, and every page of the public file to be a public
template's.

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
peer-server link. That link is separated, healed, dropping, duplicating,
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
`simtest.DroppedPeerFrames` fault. It is the only fault that the generated
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
- A fork's child is received on the same rule, watched against its parent's
  host under the hold that host keeps the point for it.
  `TestARemoteForkWhoseParentIsCutOffEndsAtItsHold` cuts that host off during
  the child's post-copy. The receive ends at the hold, the fork does not
  happen, and the parent's host retires the point when its own clock gets
  there.
- A receive whose caller hung up goes on where it was sent, and no other is
  made while a host reports it in flight. It takes the VM in, which ends the
  handover there, or fails like any other receive. Once the handover is over,
  no such receive may still take the VM in. `simtest.OutlivedReceive` makes the
  caller of one receive hang up as its host begins to start the guest, and
  makes that start slow. `simtest.LostReceiveAnswer` lets one receive run to
  its end and then tells its caller it failed. The campaigns draw both, for a
  migration's receive and a fork child's alike.
  `TestAReceiveThatOutlivesItsCallerStartsNoSecondGuest` requires one guest
  started for the VM, on the host whose receive outlived its caller.
- A fork child's receive that fails for its caller fails its fan-out, which
  gives up every hold. A child claims its hold before it runs, so one whose
  receive went on finds the hold gone and is discarded by its destination, and
  one whose answer was lost after its claim is reported claimed by the give-up
  and deleted. `TestAForkChildWhoseReceiveOutlivesItsFanOutNeverRuns` and
  `TestAForkChildWhoseAnswerWasLostIsDeleted` run the two, and `Settle`
  requires every such receive to end without its child. The orchestrator tests
  of the same names, and `TestAForkWhoseCallerHangsUpIsStillRolledBack`, cover
  the orchestrator's side; the host tests in `host/claim_test.go` cover the
  claim itself.

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
`volume.ErrSealed`, so what its peer server serves stays the pause.
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
- The source's peer server can then be removed entirely.
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
shares its parent's pages or pulls them from the parent's peer server. A failing
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
| `partitioned-pages` | Two hosts cannot reach each other's peer servers. |
| `swizzled-links` | Every link among the hosts, their peer servers and the store is blocked at its own seeded moment and healed at another. |
| `lost-page-replies` | One host's page reply is dropped after the source has already answered it. |
| `stalled-stream` | The first frame one host receives is held until whatever asked for it gives up. |
| `lost-host` | A whole host is taken away at a moment and started again when the fault ends. |
| `refused-stop` | One VM's migration pause fails after its guest has stopped and a memory region is sealed. |
| `refused-start` | One host's half of a receive fails before the guest is started. |
| `outlived-receive` | The caller of one host's next receive, a migration's or a fork child's, hangs up as the guest starts, and the receive goes on. |
| `lost-receive-answer` | One host's next receive runs to its end and its caller is told it failed. |
| `degraded-links` | The peer-server links duplicate, delay and slow what they carry. |
| `forgotten-releases` | The release after a receive is never made, as if the orchestrator restarted in between. The source keeps its hold until the survey at the next step ends it. |

`dropped-peer-frames` is the same kit plus `DropNext`, and the schedule
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
- A frame dropped by `Network.DropNext` on a peer-server link hangs the guest
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

A request that waits for room is admitted again each time it is woken. It may
wait for its class's budget at the peer, for a slot on a connection, for a dial
that another request began, or for the host's background budget. One room
given back wakes every request that waits for it, beside the request whose dial
or reply gave it back. Without a second admission, the Go scheduler chose which
of them took which connection and which went first on it. In one run of the
scheduled scenario in a few hundred, a guest's fault and its post-copy stream's
list of resident pages went to the source in either order, so seed 3 failed
`TestScheduledWorldReproduces` now and then.
`TestARequestWokenFromAWaitForRoomIsAdmittedAgain` holds the woken request at
its second admission and requires the other to go on alone. The guard
`peer-woken-requests-go-on-together` skips the second admission.


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
| `peer/busy` | Answers BUSY as a peer server at a peer's budget does |
| `peer/slow-answer` | Holds one reply for up to two seconds, as a disk that stalls does |
| `peer/stall` | Stops reading one connection for up to six seconds, as a paused process does |
| `checkpoint/disk-failed-write` | Fails the write of a page cache disk item |
| `checkpoint/disk-short-write` | Writes half of a page cache disk item, then fails |
| `checkpoint/disk-failed-sync` | Fails a sync while a disk region closes |
| `checkpoint/disk-torn-table` | Writes the second half of a closed disk region's table, with its trailer |
| `checkpoint/disk-failed-punch` | Fails the punch that gives a disk region back |
| `checkpoint/disk-failed-allocate` | Fails the allocation of a disk region as it opens |
| `checkpoint/disk-torn-header` | Tears the page cache disk's header as the disk opens |
| `checkpoint/disk-torn-table-on-open` | Tears a disk region's table as the disk opens and reads it back |
| `checkpoint/disk-wrong-stripe` | Hands a read a stripe whose checksum holds and whose bytes are wrong, as a peer that answers with a wrong stripe does |
| `checkpoint/disk-code-changed` | Reads under a code of the table the list does not name, as after the deployment changed its code and dropped the old one from its earlier codes |
| `checkpoint/disk-short-list` | Places a window by the list less every other cache, a list shorter than the code is wide |
| `checkpoint/fill-queue-full` | Has the queue of writes to the host's disk report itself full, so the fill is dropped |
| `checkpoint/fill-lose-right` | Loses the answer that carried a fill right, so rank 1 gave it and nobody fills |
| `checkpoint/fill-ranks-change` | Places a fill by the list less one of the caches it ranks, as a list that changed between the read and the fill |
| `checkpoint/fill-send-twice` | Sends a keep twice, as a sender that lost the first answer does |
| `checkpoint/fill-refuse-write` | Has the write budget refuse a fill's write to the host's own disk |
| `checkpoint/keep-drop` | Has a cache drop a keep, as one whose write budget is spent does |
| `checkpoint/cluster-wrong-stripe` | Hands a read of the cluster a stripe whose checksum holds and whose bytes are wrong |
| `checkpoint/cluster-damaged-item` | Damages an item a peer sent, so it fails its checksum |
| `checkpoint/cluster-lose-answer` | Loses a holder's answer to a read of the cluster |
| `checkpoint/cluster-store-hedge-now` | Has a read of the cluster reach its bound at once, and read the store too |
| `checkpoint/cluster-false-timeout` | Counts a holder's answer as a timeout of its host |
| `checkpoint/hot-tier-down` | Fails a read of the hot tier after its round trip, as a bucket that is down does |
| `checkpoint/hot-tier-slow` | Holds a read of the hot tier for up to twice its bound |
| `checkpoint/hot-tier-refuse` | Refuses a fill's PUT, as a bucket out of quota or permission does |
| `checkpoint/hot-tier-lose-reply` | Loses the reply to a read or a fill the hot tier carried out |
| `checkpoint/hot-tier-partial` | Cuts a reply of the hot tier short, or has it keep only the first half of a fill |

A simulated disk with `DiskConfig.ReadChaos` adds three sites of its own, as
FoundationDB's `AsyncFileChaos` does. They are off on every other disk, because
most of what a host keeps on its disk has no checksum of its own:

| Site | What it does |
| --- | --- |
| `sim/disk-slow-read` | Holds a read for a seeded time |
| `sim/disk-read-bit-flip` | Flips one bit of what a read returns |
| `sim/disk-misdirected-read` | Returns the bytes at the start of another recent write |

The simulated disk has sites of its own, in what a disk limiter reads. A
reading the limiter refuses is reported, so the limiter may report these. It
must stay safe whatever they do: the cache holds no more than its share, every
promise is counted whole, and the cache writes no more than its budget allows.

| Site | What it does |
| --- | --- |
| `sim/disk/space-fails` | Fails a reading of the filesystem's space |
| `sim/disk/space-inconsistent` | Reports more space available than the filesystem has |
| `sim/disk/space-low` | Reports less space available than is free, once |
| `sim/disk/outside-fills` | Has another writer take a share of what is free, for good |
| `sim/disk/drift-fast` | Has other writers drift ten times as fast |
| `sim/disk/device-writes-fail` | Fails a reading of the device's write counter |
| `sim/disk/device-writes-jump` | Moves the device's counter forward by up to 64 GiB |
| `sim/disk/device-writes-reset` | Starts the device's counter again at zero, as a replaced device does |

A simulated disk is on a filesystem of a size the test names, or of one the seed
draws between 5 GB and 105 GB with at least 5 GB or 7.5 % of it free, as
FoundationDB's simulator draws one. Other writers on it hold space the test
sets, and drift by up to `DriftBytesPerSecond` for each second between two
readings. A write that needs more than is free fails with `ErrNoSpace`.
`TestTheDiskLimiterStaysSafeUnderFaults` in `resource` runs the limiter over
such a disk for 24 seeds with the sites on, under a cache that fills whenever it
may and spill files that fill as guests spill. It requires every site to fire
and every disk limiter probe to be reached. When the faults stop and the disk
stands still, the share must be exactly what the disk as it is implies.

[The membership](hosting.md#the-membership) is one object every host reads
and any process writes by compare-and-set. `membership` has three sites in
its store. The writers must lose no update and never write an older
generation, and a host must keep the newest generation it read, whatever they
do.

| Site | What it does |
| --- | --- |
| `membership/read-fails` | Fails a read of the object, as a store that is down does |
| `membership/write-fails` | Fails a write before the store applies it |
| `membership/reply-lost` | Loses the reply to a write the store applied |

`TestConcurrentWritersNeverLoseAnUpdateOrGoBack` in `membership` drives them.
Four writers change one membership at once for sixteen seeds: each joins a
member with a disk, serves it and sets its weight three times, two of them
drain and leave, and two take a shared disk from each other again and again,
through release and let-go. Two views read it as it changes. The world takes
the store down and brings it back, loses replies after the store applied a
write, and fails requests, at moments a `sim.Scheduler` chooses against the
writers, with the sites on. The store must apply one line of generations from
1, each one write that `membership.Step` admits after the one before, each
with a nonce of its own; every update that returned must be the generation
the store holds at its number; at the end each member that stayed must have
its last weight and the others must be gone; and no view may go back. Every
site must fire and every store and view probe be reached across the seeds.
With `membership-write-unconditional` a writer writes over another's change,
and the store's writes stop being one line.

The protocol every request follows is stated beside it.
`TestAHolderBehindReadsTheMembershipBeforeItAnswers`,
`TestAHolderOnAnotherGenerationAnswersStaleWithItsOwn` and
`TestAMemberThatLostADiskNeverServesItAgain` in `peer` run two peer servers
over one disk, as two pods over a copied cache file, with views over a
simulated store. A holder behind the request reads the membership and
answers; a holder ahead, or one that cannot read it, answers stale with its
own generation and never asks its cache; and once the membership moves the
disk from A to B through release, let-go, assignment and serving, A answers
not me under the new generation and stale under the old one, and B serves.
`TestAReaderBehindItsHoldersReadsTheMembershipAndAsksAgain` and
`TestAFillToHoldersAheadIsSentAgainUnderTheirGeneration` in `checkpoint` put
one host behind the rest: its reads come from the cluster with no read of the
store but the open of the index object, and its fills land on the ranks with
none dropped as stale. `TestADiskIsAssignedToASecondMemberOnlyOnceTheFirstLetItGo`
in `membership` asks for a disk its member serves, and is refused until the
member lets it go. In the read campaign in `checkpoint`, only some hosts read
each new membership at once and the rest learn of it from a peer, so the
campaign must reach a holder that caught up, a stale answer and a sender that
caught up. The orchestrator's tests in `cmd/sproutfs-orchestrator` run it as
the controller over a simulated store: a join is one generation and its disk
serves in the next, a host drains before it leaves in four generations of
which one moves windows, no generation moves the windows of more than one
disk, a quiet host keeps its place, the code never follows the hosts, and two
orchestrators at once leave one line of generations that ends where one alone
would.

[Shards on network disks](hosting.md#shards-on-network-disks) move between
hosts through the cloud's attach API. `platform/sim`'s `NetworkDisks` is a
cloud of single-writer disks: each is a simulated disk of its own holding one
device file, which a detach, or the crash of its machine, power-cuts, so what
the machine had not synced is lost and every handle of it fails. The cloud and
a host's shards have sites of their own:

| Site | What it does |
| --- | --- |
| `sim/network-disk/attach-slow` | Holds an attach for up to half a minute |
| `sim/network-disk/attach-fails` | Fails an attach before the cloud does it |
| `sim/network-disk/attach-reply-lost` | Attaches the disk and tells its caller the attach failed |
| `sim/network-disk/detach-slow` | Holds a detach for up to half a minute |
| `sim/network-disk/detach-fails` | Fails a detach before the cloud does it |
| `sim/network-disk/describe-fails` | Fails a read of where a disk is attached |
| `host/shard-open-fails` | Fails an open of a shard's device, as one still settling after its attach |
| `host/shard-close-slow` | Holds a released shard open for up to ten seconds |

`TestShardsSurviveTheirFaultsAndReachTheirProbes` in `internal/simtest` drives
them. Six hosts serve six shards under 4+2, and for eight seeds a seeded
schedule has hosts leave as an autoscaler removes them, die with their shards
open, die while a shard is moving to them, join again, have a shard detached
under them by hand, and has a process open a shard's device beside its member
and keep it while the shard moves. After every step the guest reads back every
page it wrote. Across the seeds every site must fire and every shard probe be
reached: a shard opened, closed, not attached yet, lost under its host, and
refused by its lease. A shard fenced while a process still holds its device is
not among them, because the simulated cloud, as Compute Engine does, takes a
detached disk from every process of its machine, so that process's handle is
gone first; `TestAStaleMemberThatStillHoldsTheDeviceIsFenced` in `checkpoint`
keeps two handles of one device and reaches the fence.

The properties are stated beside it. `TestHostsScaleUpAndDownWithNoStoreReadForACachedWindow`
scales six hosts down to three and back, one at a time; after each, every
shard serves, the hosts serve within one shard of each other, and a VM opened
on a host that was there reads every page from the shards and nothing from the
store but its VMM state. `TestReadsDuringAShardsMoveHedgeAroundIt` reads the
VM while a shard is released and closed and not yet served elsewhere, and
after a host is lost with its shard attached, with no read of the store. In
`checkpoint`, `TestAShardMovesWithItsStripes` keeps a window on a shard, moves
it to another member and machine, and reads the same stripes back there, and
refuses the member it left; `TestAShardReadsBackOnlyTheRegionsItsLeaseNames`
opens a device of 8,191 slots that holds a few regions in a few dozen reads. In
`membership`, a model of hosts, machines and a cloud steps `Next` and `Carry`:
shards spread over the members, move off a host being removed in the order
released, closed, detached, let go, assigned, attached, opened, serving, and are
let go off a dead host only once detached. In `host`, three hosts on a
simulated cloud serve six shards and move them when one leaves, a host
started again is a new member, and a host opens a shard only while the object,
read again, still assigns it there. In `cmd/sproutfs-orchestrator`,
`TestTheOrchestratorMovesShardsOffATerminatingHost` moves the shards off a
host pod as soon as it is terminating.

Ranking itself is a pure function, so `rank`'s property tests state it: a join or a
leave changes a window's first k+m by at most one cache, weights spread windows
in proportion within half a point over 100,000 windows, equal scores go to the
lower identity, and the ranks of a few windows are written out, so a host of
any architecture must agree with them.

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
- a receive tried again;
- a disk cache told that its share fell;
- a disk cache inside its share and above its stop mark, left alone;
- a disk limiter whose promises do not fit;
- a cache write refused for its priority that a publication's fill would have
  been admitted for;
- a write of the membership whose reply was lost and that its nonce found
  landed, one another writer's change beat, and one whose outcome later
  changes hid, which the change finds done or makes again; a view that
  adopted a newer generation and one whose read failed;
- a holder behind a request's generation that caught up, a request answered
  stale, a request for a disk the holder does not serve, and a sender that
  caught up after a stale answer.

The page cache's disk marks ten more: an item written again by a second
chance, a second chance stopped at half a region, a second chance opening the
region kept free for it, an eviction waiting for a read in flight, a read
that finds another key's item or a damaged one, and, as the disk opens, a
region read back from its table, a region read back by scanning its items, a
region given back, and a header refused. A topology campaign keeps a cache
disk only on the seeds that turn the cluster cache on, and its workload is not
the disk's, so `TestDiskSurvivesItsFaultsAndReachesItsProbes` in `checkpoint`
drives them. It runs eight seeds of a writer, two readers and a limiter over a disk
with read chaos, with every completion released by the scheduler and the sites
on. Then it opens the disk again twice: after a clean close, and with a region
open, as after a crash. After each open it reads every page written. It
requires every disk site to fire and every disk probe to be reached across
the seeds. A test-only file under the cache, like FoundationDB's
`AsyncFileWriteChecker`, keeps a copy of every byte written. So the test tells a
disk that lied from a cache that misread: an item the cache refuses must be one
the disk lied about to that read. Each test envelope ends in the SHA-256 of
what comes before it, and a read checks that as an envelope's own check does,
so another page's envelope passes it and only the key check refuses it.

The disk's stripes mark five more: a write that kept several indices of one
envelope, a read that rebuilt an envelope from a parity stripe, a read that
found fewer than k stripes, a read that found the page only under another
code, and a wrong stripe found and forgotten.
`TestDiskStripesSurviveTheirFaultsAndReachTheirProbes` runs the same workload
over a disk that follows a list of its own cache and one other, with the
cluster cache on for every window, under a code of the table that changes
with the seed (under 4+2 its cache is alone in the list, and holds all six
indices). With the stripe sites on beside the others,
every hit must be what was written, whichever k stripes rebuilt it, and the
reads that rebuilt an envelope that failed its check must number no more than
the stripes handed over wrong and the lies of the disk. Every stripe site must
fire and every stripe probe be reached across its eight seeds. Splitting and
joining are pure functions, so `stripe`'s property tests state them: every
envelope of 0, 1 and up to 4,097 bytes rebuilds from every set of k of its
k+m stripes, in any order, under 1+0, 1+1, 2+1, 2+2 and 4+2; one wrong stripe
among k+1 is found wherever it falls among the first k; a stripe of another
code is never used, though a stripe of 2+1 and one of 2+2 of one envelope are
the same length; and the search for k that pass is bounded at 64 sets.

[Fills](hosting.md#filling-the-cluster) mark thirteen more: a fill right
given, a read that sent nothing for want of one, a right whose answer was lost,
a fill dropped for a full queue, for a spent rate, for want of room in the
background budget, and by its holder, a fill's write the disk refused, a fill
placed by a changed list, and, at the cache a keep reaches, a keep written, a
stripe it held or was writing, a keep refused for a window its list does not
rank it for, and a keep dropped. `TestFillsSurviveTheirFaultsAndReachTheirProbes`
in `checkpoint` drives them. Each of its sixteen seeds draws a cluster of two
to seven hosts and a code of the table, sometimes narrower than the hosts, and
runs six rounds under the scheduler with the sites on. A round publishes from
any host, has many hosts read the same pages at once, each through its own
cache, and may take a cache off the list. Some hosts read the new list at once
and the rest a round later, so two hosts may rank a window differently. The
cluster runs over real peer servers on the simulated network, with a small
queue and rate, so a burst spends them. Its background budget of 1.5 MiB has
no room for a keep of a whole 2 MiB window, which a host sends its peer under
1+1. A host sends one keep at a time, so its
own keeps never spend the budget against each other. Every read must be
what was published. At rest every stripe a host holds must be of a window some
list the cluster held ranks it for, and the reads must have filled no window
more than once an interval. Every fill site must fire and every fill probe be
reached across the seeds. The campaign found a fill placed by a changed list
writing a stripe its own host was not ranked for; a cache's own fills are now
held to its list as a keep is.

A seed of the campaign does the same work on every run. Two things stood in
the way. The harness drew each cache's identity from the operating system, so
the list ranked other hosts for a window on every run; it now draws them from
the seed. And a load of the store that several reads of a host wait for
releases them all at once when it ends. They went on to decide what each read
next, whether from this host's disk or from the store, in the order the Go
scheduler ran them. Each now passes `sim.Admit` there, so in a controlled run
the scheduler takes them one at a time. Outside a controlled run it does
nothing. Seeds 6, 10 and 12 still did other work in about one run in six to
fifteen under a loaded machine, so another completion still releases several
goroutines at once somewhere.

The fills' properties are stated exactly beside it, on a cluster of real peer
servers. `TestAStoreReadFillsExactlyTheRankedCaches`: after one host reads a
page, the stripes of its window and of its segment's are on exactly the hosts
the list ranks, each index on the host it is put on, for every reader under
1+1, 2+2 round three hosts and 4+2. `TestAColdBurstFillsAWindowOnce`: every
host reads one page at once, and the page's window and its segment's are each
filled once. `TestAFaultIsNotSlowedByItsFill`: a read of the store takes
exactly as long in simulated time with the cluster cache off, behind fills
whose links are held for a second, and behind fills dropped for a spent rate.
`TestAPublicationFillsNothingBeforeItsPartIsDurable`: while a part's PUT is in
flight no host holds any of it, and once it lands every window is on its
ranks; `TestAPartTheStoreRefusedReachesNoCache` fails the PUT, and nothing is
filled until the publication is tried again. `TestAPublicationNeverWaitsForItsFill`:
a publication behind a queue of one window and a disk that takes a second a
write takes exactly as long as with the cluster cache off. In `internal/simtest`,
half the seeds of the topology campaigns give every host a cache disk with the
cluster cache on for every window (`simtest.Config.ClusterCache`), so the hosts
fill each other through every fault of the schedule, and such a run must have
had a host keep a stripe a peer sent it.
`TestOnTwoHostsAVMOpenedOnTheOtherHostReadsItsPagesFromThatHostsDisk` suspends
a VM on one of two hosts under 1+1 and opens it on the other, which reads no
part of the store but the VMM state.

[Reads of the cluster](hosting.md#reading-from-the-cluster) mark nineteen
more: an envelope rebuilt from a peer's stripes and one from this host's own
alone, a miss, a rebuild from a parity stripe, a holder replaced at once, a
second request and one the budget refused, a read of the store past the bound,
one the store won and one the bucket refused, a wrong stripe found and a drop
sent for it, a repair, a timeout, a host marked down, a mark refused for the
fifth and one a probe cleared, and a sampled HEAD check and one that found
the part gone. `TestClusterReadsSurviveTheirFaultsAndReachTheirProbes` in
`checkpoint` drives them. Each of its twelve seeds draws a cluster of two to
seven hosts and a code of the table, on a network with a heavy tail and slow
pairs, every completion released by the scheduler and the sites on. Each of
six rounds publishes from any host and has many hosts read the same pages at
once, while the seed stalls one host's links to a reader, refuses them, slows
them, loses a host, restarts one over its file, serves a list that lacks a
cache, or deletes a checkpoint's parts behind the caches. Every read must be
what was published, or fail only for a part that is gone, and at rest every
stripe a host holds, repairs among them, must be of a window some list ranked
it for. Every read site must fire and every read probe be reached across the
seeds (about 4 s).

A [hot tier](hosting.md#reading-through-a-hot-tier) marks sixteen more: a
hit, a miss, a read failed by error, past the bound and by corrupt bytes, the
hot tier marked down and a read that skipped it, a fill sent, one that found
the object there, a miss of an object already held for a fill, a fill dropped
for the queue, for the rate, for a regional GET that failed and for a PUT the
hot tier failed, and a sampled HEAD check and one that found the regional
object gone. `TestHotTierSurvivesItsFaultsAndReachesItsProbes` in
`checkpoint` drives them. Each of its twelve seeds runs three to five hosts
over one regional bucket and one hot bucket, every completion released by the
scheduler and the sites on. The first host's queue holds an index object and
no part beside it, and the second's rate has no room for a part, so both
drops happen; half the hosts write their publications to the hot tier. Each
of six rounds publishes from any host and has many hosts read the same pages
at once, while the seed takes the hot bucket down for the round, fails a
fill's regional GET, or deletes a checkpoint's parts from the regional bucket.
Every read must be what was published, or fail only for a part that is gone,
and at rest every object in the hot bucket must be what a publication wrote
under its name, or its first half. Every site must fire and every probe be
reached across the seeds (about 2 s).

The hot tier's properties are stated exactly beside it, over a simulated
regional bucket and a simulated hot bucket.
`TestAMissIsFilledBehindTheReadAndTheNextReadHits`: a cold read misses the
index object and the part, sends the regional bucket three GETs, the part
once more by its fill, and the next read sends it none.
`TestAPublicationWritesTheHotTierOnlyOnceItsRegionalPutSucceeded`: while a
part's PUT is held for a second, the hot tier holds nothing of it, and a part
the regional bucket refused never reaches it. `TestAHotTierThatFailsNeverFailsARead`:
a hot bucket that is down, slower than the bound, holding other bytes, or
holding half a part, costs only reads of the regional bucket, each failure
counted by why. `TestAHotTierMarkedDownIsSkippedAndTriedAgain`,
`TestAReadIsNotSlowedByItsHotTierFill` (a read behind a PUT of ten seconds
takes exactly as long as behind one of a millisecond),
`TestAPublicationIsNotSlowedByItsHotTierFill` (so does a publication, and as
long as one that writes no hot tier),
`TestTheHotTierDropsFillsPastItsQueueOrItsRate`,
`TestAFillThatCannotReadOrWriteIsDroppedByWhy`,
`TestTwoHostsFillingOneObjectWriteItOnce`, `TestASampledHotHitChecksItsRegionalObject`
and `TestAStoreRefusesAHotTierBesideTheClusterCache` hold the rest.
`TestAHotTierReadsAndFillsThroughEveryProvidersAdapter` in
`platform/internal/real` misses, fills and hits a hot tier through the Cloud
Storage and S3 adapters over their emulators. In `host`,
`TestAVMOpenedOnAnotherHostReadsItsCheckpointFromTheHotTier` writes a VM on
one host and reads it on another with three hits and no miss, and
`TestAHostRefusesAHotTierBesideTheClusterCache` refuses both.
`TestSeededTopologyFingerprintIsStable` runs each seed a third time with every
host reading through a hot tier, and requires the hosts to have filled it and
read from it. The world sets the hot tier's bound, rate, queue and sampled
checks out of reach, as it does the cluster's timing. Seeds 1 to 25 of that
arm did the same work under the shake (2026-10-03).

The reads' properties are stated exactly beside it.
`TestAPageInTheClusterIsReadWithNoStoreRead`: under 1+1, 2+2 round three and
4+2, every host reads a published page and its segment from the cluster, with
no request of the store but the open of the index object.
`TestAPageSurvivesLosingDrainingOrRestartingAnyOneHost`: the same after any
one host of six under 4+2, or of two under 1+1, is lost, drained from the list
or restarted over its file. `TestAHotPageSpreadsItsLoadOverEveryHolder`: six
readers of one page ask four holders each besides themselves, every holder is
asked, and each sends one stripe, a quarter of the envelope, to each reader.
`TestAStalledOrSlowHolderSlowsAReadByTheHedgeDelayAtMost`: on a network of
fixed latency, a read with one of its first picks stalled takes exactly as long
as a healthy one, and with one stalled and one slow it takes exactly the delay
longer for each window that picked both. `TestAWrongStripeIsNeverReturnedAndItsHolderIsTold`:
a stripe whose checksum holds and whose bytes are wrong is found among k+1, or
among k and one more asked at once, the page reads right, and its holder
forgets it. `TestTheStoreIsReadOnlyWhenFewerThanKStripesExist`: with six to
zero stripes left, the store is read exactly when fewer than four remain.
`TestSecondRequestsStayWithinTheirBudget` and
`TestStoreReadsPastTheBoundStayWithinTheirBucket` hold the two budgets.
`TestThreeTimeoutsMarkAHostDownAndOnlyAProbeClearsIt`,
`TestAReaderMarksDownAtMostAFifthOfItsList`, `TestAMissIsNotAFailureOfTheHost`
and `TestARefusedConnectionMarksAHostDown` hold the marks, and the first also
that a marked host is sent no fills. `TestRepairSendsOnlyAnIndexNoRankHolds`, `TestRepairAfterAJoinSendsTheIndexNoRankHolds`,
`TestAReaderRebuildsFromAnyIndicesAfterTheRanksShift` (B5) and
`TestASampledHitChecksItsPartStillExists` hold the rest. In
`internal/simtest`, `TestAVMOpensFromTheClusterAfterAnyOneHostIsLostDrainedOrRestarted`
suspends a VM on one host of six under 4+2, or of two under 1+1, loses,
drains or restarts any other host, the suspending host among them, and opens
the VM on another: it reads no part of the store but the VMM state.

A simulated world sets a read's delay, its bound and its stripe timeout out of
reach (`simtest.Config.ClusterCache`). Whether a read crossed one of them
turns on each hop's jitter, and concurrent reads draw it in the order the Go
scheduler sends them, so the seed would not decide whether a second request,
a read of the store or a timeout happened. The world's reads still ask k+1
ranks, replace misses, rebuild from any k and repair, which the answers alone
decide. The read campaign above drives the timed paths under a scheduler.

[The bounds on the object store](hosting.md#bounds-on-the-object-store) are
tested over a simulated store that holds requests. `sim.ObjectStore` can hold
the next requests of an operation before their reply (`HangNext`), apply the
next writes and then hold their replies (`HangNextAfterApply`), and hold a
GET's or a PUT's body halfway (`StallNextBody`). A hold lasts the store's
`Hold`, an hour by default, unless its caller gives up first. A store with
`ObjectStoreConfig.RequestChaos` adds three sites of its own. They are off on
every other store, because a caller with no bounds waits out every hold:

| Site | What it does |
| --- | --- |
| `sim/object-store/hang` | Holds a request before its reply, applying nothing |
| `sim/object-store/hang-after-apply` | Applies a PUT or a DELETE, then holds its reply |
| `sim/object-store/stall-body` | Holds a GET's or a PUT's body halfway |

`platform/bounded` marks seven probes: a first-byte timeout, a stall timeout,
a request made again, a conditional PUT made again, a body resumed, a body
whose object had changed, and a write that timed out and was not made again.
`TestTheBoundsSurviveTheirFaultsAndReachTheirProbes` runs twelve seeds of
four workers over a store with request chaos, every completion released by
the scheduler and the sites on. The workers create immutable objects, read
them whole, by range and by suffix, head and list them, add to counters by
compare-and-set, and write and delete objects of their own without a
condition. Every read must be what was written. A create refused by its own
landed write must find its own bytes, and an addition refused the same way
must find its own name in the counter. Each counter must hold every
addition exactly once. An unconditional write that timed out may or may not
have landed, and the next read must find one of the two. The store's counts
must equal the probes. Every site must fire and every probe be reached
across the seeds (about 0.2 s).

The bounds' properties are stated exactly beside it, in virtual time.
`TestAHungGetIsAbandonedAtItsFirstByteBoundAndTheRetrySucceeds`: a GET held
before its headers answers at exactly its bound plus one round trip, with the
bytes asked for. `TestAStalledBodyIsAbandonedAtItsStallBoundAndReadOnFromWhereItStopped`:
a body held halfway is read on at exactly the stall bound, by a ranged GET of
the rest, for a whole object, a range and a suffix.
`TestAStalledBodyWhoseObjectChangedIsRefused`,
`TestAHungCreateWhoseWriteLandedIsMadeAgainAndRefusedByItsOwnObject`,
`TestAControlRecordWrittenAcrossHungRepliesIsReconciled` (by the writer's
nonce, as a lost reply always was),
`TestAPublicationWhosePartsReplyHungIsSettledByItsDigest`,
`TestAHungUnconditionalWriteIsNotMadeAgain`,
`TestAStalledUploadIsAbandonedAtItsStallBound`, `TestAHungHeadOrListIsMadeAgain`,
`TestACallerThatGivesUpIsNotRetried` and `TestABodyHeldUnreadIsNotAStall`
hold the rest. The world bounds every host's view of the store and the
orchestrator's, on the bubble's clock, which is the clock the store's latency
passes on. A host's own clock passes only when the world advances it, so a
bound on it would never fire. With buggify on, the topology campaigns turn
request chaos on, so the real hosts meet hung and stalled requests:
`TestSeededTopologyUnderBuggify` must reach all three sites, and it has seen a
control record's PUT, a part's, an index object's and a reclamation's DELETE
time out and the guests' bytes stay right. The fingerprint test runs without
buggify, and its seeds did the same work under the shake with the bounds in
place (2026-10-04).

`TestEveryProvidersAdapterGivesUpAHungRequestAndResumesAStalledBody` in
`platform/internal/real` runs the bounds over the Cloud Storage and S3
emulators, through each provider's own client, behind a handler that holds
the next request. A create whose write landed and whose reply never came is
made again and refused by its own object; a GET whose headers never come is
made again; a body cut off halfway is read on from where it stopped; and an
unconditional PUT that hangs comes back as `bounded.ErrTimedOut`. So each
client gives a request up when its context is cancelled, mid-body included,
and the next request on the same client succeeds. Only GCE shows whether a
stuck request there is a stream on a live HTTP/2 connection, which a cancel
resets and a retry can share, or a connection that is gone; and the tail of
times to first byte that the defaults should be checked against.

### The peer server's network

The simulated network models what FoundationDB's simulator does to a link and
this one once did not. Each fault is off at its zero value, so a world that
asks for none runs exactly as it did:

- a heavy latency tail, in which about one hop in `TailEvery` takes up to
  `TailLatency` longer;
- pairs of hosts that stay slow for the whole run once they first connect;
- one link's bandwidth, `LinkBytesPerSecond`, shared by every connection
  between two hosts, so a frame is sent behind the bytes sent before it;
- bounded send buffers, so a sender whose reader stops reading stalls;
- holds: `Network.Hold` and `HoldBoth` keep every byte a link carries until a
  moment, and refuse nothing, as a partition does to TCP, and
  `SwizzleHolding` swizzles with holds instead of refusals;
- dials to an address nobody listens at that hang until their caller gives
  up, as a dial to a machine that is gone does;
- connections closed at random under a frame (`sim/network/random-close`);
- a flipped bit in a frame's header (`sim/network/header-bit-flip`).

`Network.Framed` is the network as a host's real one is: byte streams over
the simulated links, framed by the same framer the TCP adapter runs. Each write
is split into pieces of seeded lengths, down to a byte, and each read returns a
seeded part of what has arrived. So every frame crosses in pieces, and the real
framer puts it back together. Its site `sim/network/stream-bit-flip` flips a
bit in the first bytes of a write, where a frame's prefix and header are.

`TestThePeerServerCampaignNeverAnswersWrong` in `peer` drives all of it. One
destination host asks a source of this release, a source of the release
before, a release two ahead and a machine that is gone, for pages and stripes,
for thirty simulated seconds, while the link to the source is held for up to
six seconds at a time. Half the seeds run over framed links and half over byte
streams. No answer may be wrong, and no damage on the way may make a sound peer
look broken. The release before checks no header, so its answers are checked
only once the faults stop. Then, after a minute, every peer must answer at
once and right, none may still be marked down but the one that is gone, and
the release two ahead must still be incompatible. Three seeds run normally,
the cheapest set that between them activates every site of the peer server and
the network and reaches every probe of `peer.Probes`. The soak runs
sixty-four.

The peer server's probes are a request answered BUSY, a hello answered
INCOMPATIBLE, a request that waited here for its class's budget, a reply to a
request its caller gave up on, a dialer that fell back to version 1, a
connection found dead, a peer marked down, a down peer probed, and a request
that skipped a down peer.

The campaign found three bugs. A write on a stream whose connection closed
spun until its pieces would have arrived. A connection of version 1 that owed
a reply it would never get was never found dead, because the release before
answers no ping. And a down peer whose probe was answered INCOMPATIBLE stayed
down and was probed for ever.

`TestAGuestFaultIsAnsweredWhileTheStreamSaturatesTheLink` is the
small-behind-large problem. Sixteen streams fill a link of 256 MiB/s, and a
guest fault asks for one page every 50 ms. The fault waits behind what the
stream has on the link, which the background budget of 4 MiB bounds: about
5 ms. With `peer-unbounded-background` it waits behind what the source allows,
64 MiB, about 100 ms, and the test fails. The stream still runs at nine
tenths of the link or more.

```sh
go test ./peer -run '^TestThePeerServerCampaignNeverAnswersWrong$' -count=1
SPROUTFS_TEST_SOAK=1 go test ./peer -run '^TestThePeerServerCampaignNeverAnswersWrong$' -count=1
```

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
served, every byte and every outcome. Each seed runs with no cache disk, with
the cluster cache on, where the hosts fill each other beside everything else
they do, through a hot tier, and with the cache on shards, which the
controller moves as the campaign kills and restarts hosts. A failure prints
the work that differs between the two runs.

The second run of each seed is shaken (`sim.Config.Shake`). Before and after
every wait in a simulated dependency, and before every send, a goroutine
yields the processor a drawn number of times. So goroutines that are ready at
one simulated instant reach the dependencies in another order than in the first
run. The draws are noise, not a choice: they are taken in whatever order the
goroutines ask. A race that the seed does not decide shows up on an idle
machine, rather than only on a loaded one where the Go scheduler orders two
runs differently by itself. Without the shake, the race that step 6 brought in
failed this test in about two runs of three on a loaded machine and in none of
sixty on an idle one.

Shaking the campaign with the fault-injection sites on found four more races of
this kind. In each, goroutines reached something the run observes at one
simulated instant, and the Go scheduler chose their order:

- A destination's memory regions stream their pages on several goroutines. Each
  fault took its arena slot, drew the site that evicts past a free slot, and
  sent its request whenever it ran, so which page a later store evicted
  changed from run to run. The faults now take turns in page order until each
  one's request is on the wire. See [the post-copy stream](migration.md#phases).
  Under the scheduler each page's fault is a task of its own, and it passes
  `sim.Admit` as it takes its turn: the turn comes free as the fault before
  it sends, which frees that connection to a guest's fault waiting for it at
  the same moment.
- The driver began and ended faults while the world still had work due at
  that instant, such as a publication's retry, or traffic a fault had just let
  go of. It now waits until the world has done everything it can at the
  instant (`synctest.Wait`) before each fault it begins or ends and before each
  step.
- The simulated store refused an operation at the instant it was asked while
  it was down, and a cut link refused a frame at the instant it was sent. The
  caller then failed in the same instant as everything else that instant held:
  a publication's first part failed while it built the second, and a
  connection closed under a frame that arrived as it did. A refused operation
  now takes its latency, as a frame does.
- A publication's uploads handed their parts to the fills as their PUTs
  ended, and a host's one worker of fills took them in that order. A
  publication now hands its parts over in their own order.

Over seeds 1 to 50, with no cache disk and with the cluster cache on, the
sites on and four shakes each, 97 of the 100 pairs then did the same work in
all four runs (2026-10-03). Before, 7 of the 50 pairs of seeds 1 to 25 did
not. Seeds 35 without a cache, and 43 and 48 with one, still vary now and
then. In seed 35 a reply whose header a fault flipped makes the destination
close the connection at the instant the source starts its next reply, and
whether that reply is sent into the closed connection or never sent is the
scheduler's choice. Two requests that one link carries at one instant take
its sequence numbers, and so its drawn latencies, in the order they arrive.
None of these seeds is one the probe campaign or the fingerprint test runs.

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

The probe campaign runs seeds 1 to 25 and seed 46 of the generated schedule
with the sites on, each once with no cache disk and once with the cluster
cache on, plus four seeds of the two-writer campaign. It requires every probe
that the campaigns are registered to cover to have fired, a fill right given
and a keep kept among them, and a page rebuilt from a peer's stripes. The peer
server's, the membership's and the page cache disk's, stripes', fills' and
reads' probes are reached here too, and each is asserted by its own campaign.
No campaign in this
repository covers one of the registered probes, which `unreachedProbes` in
`internal/simtest/probe_test.go` names. Here the store either answers or fails
outright, so no conditional write ever loses its reply and is reconciled by its
writer's nonce.

Seed 46 is there for an eviction during a publication, which none of seeds 1
to 25 reaches. Seed 2 used to reach it in about half its runs. Its stream's
faults took arena slots in the order the Go scheduler ran them, and under the
site that evicts past a free slot that order decided which page a later store
evicted, and so whether it was a page of a sealed memory region. Now that the
faults take turns, seed 2 never does. Seed 46 does on every run, with the cache
and without it.

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
SPROUTFS_SIM_BUG=volume-rooted-before-its-hold-goes \
  go test ./volume -run '^TestAForkIsRootedOnlyOnceItsHoldOnThePointIsGone$' -count=1
SPROUTFS_SIM_BUG=checkpoint-compact-another-vm \
  go test ./volume -run '^TestEachPageIsBilledToTheVMThatPublishedIt$' -count=1
SPROUTFS_SIM_BUG=migration-accept-wrong-size \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=migration-accept-missing-memoryRegion \
  go test ./vmmigrate -run '^TestReceiveRefusesAMachineMissingAMemoryRegion$' -count=1
SPROUTFS_SIM_BUG=migration-corrupt-peer-page \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=migration-corrupt-fallback \
  go test ./vmmigrate -run '^TestADestinationWhoseSourceIsGoneReadsTheCheckpoint$' -count=1
SPROUTFS_SIM_BUG=migration-skip-resume \
  go test ./vmmigrate -run '^(TestFailedStopResumesTheGuest|TestMigrationReportsFailedResumption)$' -count=1
SPROUTFS_SIM_BUG=migration-give-up-first-receive \
  go test ./internal/simtest -run '^(TestAReceiveIsRetriedUntilItsDestinationReachesTheStore|TestAHandoffIsGivenUpOnlyWhenItsSourceStopsHoldingIt)$' -count=1
SPROUTFS_SIM_BUG=migration-ignore-source-hold \
  go test ./internal/simtest -run '^(TestAMigrationWhoseSourceIsCutOffEndsAtItsHold|TestARemoteForkWhoseParentIsCutOffEndsAtItsHold)$' -count=1
SPROUTFS_SIM_BUG=migration-retry-beside-a-receive \
  go test ./internal/simtest -run '^TestAReceiveThatOutlivesItsCallerStartsNoSecondGuest$' -count=1
SPROUTFS_SIM_BUG=migration-strip-published-pages \
  go test ./internal/simtest -run '^TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes$' -count=1
SPROUTFS_SIM_BUG=migration-strip-published-holes \
  go test ./internal/simtest -run '^TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes$' -count=1
SPROUTFS_SIM_BUG=migration-ask-for-published-pages \
  go test ./internal/simtest -run '^TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes$' -count=1
SPROUTFS_SIM_BUG=migration-stream-in-any-order \
  go test ./vmmigrate -run '^TestAStreamPageWaitsForThePageBeforeItToBeAskedFor$' -count=1
SPROUTFS_SIM_BUG=pager-zero-new-page \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
SPROUTFS_SIM_BUG=pager-forget-spill \
  go test ./vmmemory -run '^TestASpilledPageFaultsBackWhatTheGuestStored$' -count=1
SPROUTFS_SIM_BUG=pager-give-back-changed-copy \
  go test ./vmmemory -run '^TestAColdCopyTheGuestStoredIntoIsKept$' -count=1
SPROUTFS_SIM_BUG=spill-sparse \
  go test ./vmmemory -run '^TestASpillSucceedsOnADiskFilledFromOutside$' -count=1
SPROUTFS_SIM_BUG=diskcache-skip-key-check \
  go test ./checkpoint -run '^TestDiskReadChecksKeyAndChecksum$' -count=1
SPROUTFS_SIM_BUG=diskcache-skip-checksum \
  go test ./checkpoint -run '^TestDiskReadChecksKeyAndChecksum$' -count=1
SPROUTFS_SIM_BUG=diskcache-unbounded-second-chance \
  go test ./checkpoint -run '^TestDiskSecondChanceIsBoundedAtHalfARegion$' -count=1
SPROUTFS_SIM_BUG=diskcache-no-free-region \
  go test ./checkpoint -run '^TestDiskEvictsTheOldestRegionFirst$' -count=1
SPROUTFS_SIM_BUG=diskcache-evict-under-reader \
  go test ./checkpoint -run '^TestDiskReadInFlightKeepsItsRegion$' -count=1
SPROUTFS_SIM_BUG=diskcache-table-before-sync \
  go test ./checkpoint -run '^(TestDiskRegionsFillInOrderAndCloseWithATable|TestDiskPowerLossAroundClosingARegion)$' -count=1
SPROUTFS_SIM_BUG=diskcache-pull-frees-on-close \
  go test ./checkpoint -run '^TestPullsShareOneCopyAndClosingFreesNothing$' -count=1
SPROUTFS_SIM_BUG=diskcache-restart-trusts-open-region \
  go test ./checkpoint -run '^TestDiskGivesBackTheRegionOpenAtTheRestart$' -count=1
SPROUTFS_SIM_BUG=diskcache-restart-ignores-deployment \
  go test ./checkpoint -run '^TestDiskEmptiesAFileThatIsNotItsOwn$' -count=1
SPROUTFS_SIM_BUG=diskcache-restart-skips-scan \
  go test ./checkpoint -run '^TestDiskScansARegionWhoseTableIsTorn$' -count=1
SPROUTFS_SIM_BUG=diskcache-mix-codes \
  go test ./checkpoint -run '^TestDiskReadsNoStripeOfAnotherCode$' -count=1
SPROUTFS_SIM_BUG=diskcache-current-code-only \
  go test ./checkpoint -run '^TestDiskReadsAPageUnderTheCodeItWasKeptUnder$' -count=1
SPROUTFS_SIM_BUG=diskcache-one-stripe-a-page \
  go test ./checkpoint -run '^TestDiskHoldsEveryIndexOfAPageRoundAShortList$' -count=1
SPROUTFS_SIM_BUG=diskcache-stripes-not-round \
  go test ./checkpoint -run '^TestDiskHoldsEveryIndexOfAPageRoundAShortList$' -count=1
SPROUTFS_SIM_BUG=diskcache-keep-wrong-stripe \
  go test ./checkpoint -run '^TestDiskFindsAndForgetsAWrongStripe$' -count=1
SPROUTFS_SIM_BUG=diskcache-share-ignored \
  go test ./checkpoint -run '^TestAPulledCheckpointIsKeptWholeOutsideTheClusterShare$' -count=1
SPROUTFS_SIM_BUG=stripe-mix-codes \
  go test ./stripe -run '^TestAStripeOfAnotherCodeIsNeverMixedIn$' -count=1
SPROUTFS_SIM_BUG=stripe-stop-at-first-failure \
  go test ./stripe -run '^TestOneWrongStripeAmongKPlusOneIsFound$' -count=1
SPROUTFS_SIM_BUG=store-request-unbounded \
  go test ./platform/bounded -run '^TestAHungGetIsAbandonedAtItsFirstByteBoundAndTheRetrySucceeds$' -count=1
SPROUTFS_SIM_BUG=store-resume-any-object \
  go test ./platform/bounded -run '^TestAStalledBodyWhoseObjectChangedIsRefused$' -count=1
```

Each invocation must fail. `just check-guards` runs every entry and fails if
any of them passes; `just check` runs it, and so does CI. It builds each
package's test binary once, runs each entry once with no guard on, and then
once with its guard on. It takes about fifteen seconds once the binaries are
built. The `vmmachine` entries name `"goos": "linux"` and `"root": true`, and
the check skips them anywhere else, because their test skips itself there. To
show that an entry fails every time rather than once, run it again and again:

```sh
python3 scripts/check-guards.py --repeat 5 --guard pager-forget-spill
```

A guard is only consulted under a context that carries a simulated runtime. A
test that calls the code under a bare `t.Context()` never turns its guard on,
however directly it tests the property. The guard is only as good as the test
that names it, and that test has to run the guard's path under the runtime.

The check exists because five guards rotted without anyone seeing. Their
tests passed with the guard on, on main. Each named a campaign that no longer
reached the guard's path, or never did:

- `pager-forget-spill` named the buggified campaign. Since the isolated arena
  became the default, that campaign's evictions take only clean pages, so it
  spills nothing. It still fails under `SPROUTFS_ARENA=shared`.
- `pager-give-back-changed-copy` named the generated schedule, which gave RAM
  back on an interval. That pass was removed, and only the cold-copy
  give-back is left. The campaign makes no cold copy.
- `migration-give-up-first-receive` named the swizzle campaign. Its separated
  links used to fail a first receive on every seed. Since the peer server's
  transport waits out a separation inside the receive, no receive fails.
- `migration-corrupt-fallback` and `migration-skip-resume` named the generated
  schedule. Its seeds reach neither path, and it passed with either guard on
  from the first commit of this repository. The `vmmigrate` tests of the
  fallback and of the abandoned migration did not consult them. They ran
  under a bare context, and the fallback tests read volumes of zeros, which a
  fallback that returns zeros matches.

Each now names a focused test. `TestASpilledPageFaultsBackWhatTheGuestStored`
spills a page the guest stored into, twice, and faults it back.
`TestAColdCopyTheGuestStoredIntoIsKept` gives back a cold copy the guest
stored into, under the fixture's runtime.
`TestADestinationWhoseSourceIsGoneReadsTheCheckpoint` releases the source
before the destination reads anything, over a checkpoint of nonzero bytes.
`TestFailedStopResumesTheGuest` and `TestMigrationReportsFailedResumption`
abandon a migration at its pause under the cluster's runtime. The retry
scenarios fail a first receive on purpose.

`spill-sparse` leaves a pager's spill file sparse. Its test fills the
simulated filesystem from outside once the pager has started, and the guest's
next spill then fails for want of space.

The sixteen `diskcache-` guards break the page cache's disk. Each is killed by a
test of the one property it breaks. `diskcache-table-before-sync` is killed
twice. The close's operations are checked in order, and a power loss around
the table write leaves a table naming items the device did not keep. The three
`diskcache-restart-` guards break the disk's open after a restart: one indexes
the region that was open, which has no table; one keeps a file of another
deployment; and one gives back a region whose table is torn without scanning
it. The five guards of the disk's stripes look a stripe up under any code,
read under the list's code alone so a page kept under the earlier code is a
miss, find a page's first item whatever index was asked for, put stripe i on
rank i alone so a short list drops indices, and keep a stripe found wrong.
`diskcache-share-ignored` places every window by the membership whatever
share the cluster cache is on for, so a host whose membership holds others keeps a
pulled checkpoint as stripes it cannot rebuild alone. The two
`stripe-` guards rebuild from a stripe of another code whose length fits, and
give up when the first k fail instead of trying other sets.
`pager-give-back-changed-copy` gives back a cold copy the guest stored into,
as though it still held its origin's bytes. Its test finds the copy given back
and the guest's store gone. `migration-give-up-first-receive` gives a handoff up
after its first failed receive. In one retry scenario the destination cannot
reach the store for ten seconds, so the handover must retry it; in the other
the destination refuses until the source's hold is over, and the handoff must
last that long.
`migration-ignore-source-hold` belongs to two scenarios of its own. It keeps a
receive waiting on a listed source that nothing can reach after the source's
hold is over: a migration's source in one, a fork's parent's host in the
other. No campaign cuts a source off while it stays listed, so these scenarios
are the only place where the hold is the one evidence left. With the guard on,
each receive ends at the harness's patience instead of at the hold, and each
scenario fails on its own.
`migration-retry-beside-a-receive` belongs to its own scenario too. It sends a
handoff's next receive while a host still reports an earlier one in flight.
Only a receive whose caller hung up is in flight when a retry looks, and no
campaign draws that fault. With the guard on, a second host starts a guest of
the VM, and the receive that outlived its caller takes the VM in after the
handover ended.

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

`migration-stream-in-any-order` has a destination's stream fault its pages
all at once again, without turns. Its test holds the stream's first request
before it is sent. With the turns, no other page is asked for meanwhile. With
the guard, the other three pages of the region are. On the generated schedule
the guard is what made seed 2 reach an eviction during a publication in about
half its runs.

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

Eight guards break the disk limiter in `resource`:

```sh
SPROUTFS_SIM_BUG=disklimit-count-spill-by-allocation \
  go test ./resource -run '^TestASpillFileCountsAtItsPromiseWhileSparse$' -count=1
SPROUTFS_SIM_BUG=disklimit-ignore-other-writers \
  go test ./resource -run '^TestTheShareFollowsTheDiskFilledFromOutside$' -count=1
SPROUTFS_SIM_BUG=disklimit-no-hysteresis \
  go test ./resource -run '^TestTheCacheStopsOneRegionBelowItsShare$' -count=1
SPROUTFS_SIM_BUG=disklimit-admit-past-budget \
  go test ./resource -run '^TestTheWriteBudgetRefusesLowPrioritiesFirst$' -count=1
SPROUTFS_SIM_BUG=disklimit-refuse-high-before-low \
  go test ./resource -run '^TestTheWriteBudgetRefusesLowPrioritiesFirst$' -count=1
SPROUTFS_SIM_BUG=disklimit-take-from-spill \
  go test ./resource -run '^TestPromisesThatDoNotFitMakeTheHostUnready$' -count=1
SPROUTFS_SIM_BUG=disklimit-no-reserve \
  go test ./resource -run '^TestARestartedHostFindsRoomAnotherHostsCacheGivesBack$' -count=1
SPROUTFS_SIM_BUG=disklimit-capacity-from-free-space \
  go test ./resource -run '^TestPromisesAreFeasibleWhileOnlyOtherWritersLeaveNoRoom$' -count=1
```

The last two are about hosts that share a filesystem. Without the reserve, a
cache fills the disk to the floor, and a host beside it whose cache is empty
has no room to promise a VM's staging. Judging promises by the space other
writers leave now refuses a host that could wait for them.

Four guards break how a host takes its disk:

```sh
SPROUTFS_SIM_BUG=host-refuse-start-while-the-disk-is-held \
  go test ./host -run '^TestAHostStartsUnreadyWhileAnotherWriterHoldsItsRoom$' -count=1
SPROUTFS_SIM_BUG=host-cache-share-a-file \
  go test ./host -run '^TestAHostClaimsACacheFileNoOtherHostHolds$' -count=1
SPROUTFS_SIM_BUG=host-cache-on-another-filesystem \
  go test ./host -run '^TestACacheDirectoryOnAnotherFilesystemIsRefused$' -count=1
SPROUTFS_SIM_BUG=host-pull-closed-with-the-machine \
  go test ./host -run '^TestAPulledVMKeepsItsStopsCheckpointOnTheDisk$' -count=1
```

The first refuses a host that another writer's space keeps from fitting now,
which truncates its spill files as it exits, so the other writer never gives
space back. The second opens the page cache's file without its lock, so two
hosts on a node share it. The third accepts a cache directory the limiter does
not measure. The fourth closes a pulled VM's pull when its machine ends. A stop
ends the machine before it publishes, so the stop's checkpoint keeps nothing on
the disk, and a VM opened on the same host again reads the pages it wrote last
from the store. The GCE run of 2026-10-03 found this
(docs/measurements/gce-deploy-cache-2026-10-03.md).

Fifteen guards break the peer server:

```sh
SPROUTFS_SIM_BUG=peer-mark-down-when-cancelled \
  go test ./peer -run '^TestACancelledRequestMarksNothingDown$' -count=1
SPROUTFS_SIM_BUG=peer-close-when-busy \
  go test ./peer -run '^TestARequestOverItsBudgetIsAnsweredBusyAndTheConnectionStays$' -count=1
SPROUTFS_SIM_BUG=peer-one-budget-for-every-class \
  go test ./peer -run '^TestAFaultIsAnsweredWhileTheBulkClassIsAtItsBudget$' -count=1
SPROUTFS_SIM_BUG=peer-reply-out-of-order \
  go test ./peer -run '^TestRepliesLeaveInTheOrderTheirRequestsCame$' -count=1
SPROUTFS_SIM_BUG=peer-no-fallback \
  go test ./peer -run '^TestThisReleaseFetchesPagesFromThePreviousRelease$' -count=1
SPROUTFS_SIM_BUG=peer-ignore-silence \
  go test ./peer -run '^(TestASilentPeerIsFoundDeadAndProbedBack|TestASilentPreviousReleaseIsFoundDeadAfterItsRequestTimeout)$' -count=1
SPROUTFS_SIM_BUG=peer-unbounded-background \
  go test ./peer -run '^TestAGuestFaultIsAnsweredWhileTheStreamSaturatesTheLink$' -count=1
SPROUTFS_SIM_BUG=peer-answer-for-another-cache \
  go test ./peer -run '^TestAReusedAddressAnswersNotMe$' -count=1
SPROUTFS_SIM_BUG=peer-queue-keeps \
  go test ./peer -run '^TestAKeepOverTheBackgroundBudgetIsDropped$' -count=1
SPROUTFS_SIM_BUG=peer-unbounded-stripes \
  go test ./peer -run '^TestAReaderBoundsItsStripeBytesInFlight$' -count=1
SPROUTFS_SIM_BUG=peer-refuse-second-hello \
  go test ./peer -run '^TestAHelloDeliveredTwiceLeavesTheConnectionServing$' -count=1
SPROUTFS_SIM_BUG=peer-stripes-in-fault-class \
  go test ./peer -run '^TestAStripeReadNeverWaitsBehindAPage$' -count=1
SPROUTFS_SIM_BUG=peer-serve-past-budget \
  go test ./peer -run '^TestAServerAnswersBusyPastItsServingBandwidth$' -count=1
SPROUTFS_SIM_BUG=peer-answer-shares-buffer \
  go test ./peer -run '^TestAnAnswersPagesOutliveItsReplysBuffer$' -count=1
SPROUTFS_SIM_BUG=peer-payload-after-its-receive \
  go test ./peer -run '^TestAKeepIsKeptOverTCP$' -count=1
```

The first five break what each end promises the other. A caller giving up is
read as the peer failing. A peer at its budget loses its connection instead of
hearing that it is busy. One class's requests count against another's budget.
Replies leave a connection in the order they were built rather than the order
their requests came. A dialer stops talking to the release before. The sixth
leaves a connection that hears nothing open, version 1's as well as version
2's. The next four break the budgets that keep bulk work behind faults: the
background budget a guest fault's reply would otherwise wait behind, a cache
request that names another cache, a keep queued instead of dropped, and the
bound on stripe bytes in flight. The last closes a connection over a second
hello, which a duplicating link delivers. The dialer's first request goes the
moment the first hello is answered, so whether that request is answered is the
Go scheduler's choice, and a seed does not reproduce its run. The next sends
stripe reads over the fault class, where a stripe waits behind every page
ahead of it on its connection. The next serves stripes past a host's serving
bandwidth. The last hands back a page that no encoder shrank as a slice of
its reply's pooled buffer, which the next reply is read into: the race
detector found it in `TestAGuestFaultIsAnsweredWhileTheStreamSaturatesTheLink`.
The last ends a request's receive context before its payload is read. Over
TCP a payload is read under the socket's deadline, which that context sets,
so every keep was reset; the simulated stream does not read under one, which
is why only the GCE run of 2026-10-03 found it, and why its test runs over a
loopback socket.

Eight guards break the cluster's fills:

```sh
SPROUTFS_SIM_BUG=fill-before-durable \
  go test ./checkpoint -run '^TestAPublicationFillsNothingBeforeItsPartIsDurable$' -count=1
SPROUTFS_SIM_BUG=keep-unranked \
  go test ./checkpoint -run '^TestACacheRefusesAKeepItsListDoesNotRankItFor$' -count=1
SPROUTFS_SIM_BUG=fill-blocks-read \
  go test ./checkpoint -run '^TestAFaultIsNotSlowedByItsFill$' -count=1
SPROUTFS_SIM_BUG=no-fill-right \
  go test ./checkpoint -run '^TestAColdBurstFillsAWindowOnce$' -count=1
SPROUTFS_SIM_BUG=fill-queue-waits \
  go test ./checkpoint -run '^TestAPublicationNeverWaitsForItsFill$' -count=1
SPROUTFS_SIM_BUG=keep-while-writing \
  go test ./checkpoint -run '^TestACacheDropsAKeepItHoldsOrIsWriting$' -count=1
SPROUTFS_SIM_BUG=fill-concurrently \
  go test ./checkpoint -run '^TestAHostSendsItsKeepsOneAtATime$' -count=1
SPROUTFS_SIM_BUG=fill-parts-in-any-order \
  go test ./checkpoint -run '^TestAPublicationHandsItsPartsOverInTheirOrder$' -count=1
```

The first fills the cluster with a part before its PUT has succeeded, which is
the model's `fill-before-put` mutant: the stripes are on their hosts while the
PUT is still in flight. The second takes a keep, and writes a fill of its
own, for a window the cache's list does not rank it for. The third fills in
front of the read, so the fault waits a second for links held under its
fill. The fourth fills from every read of a cold burst, six times where one
would do. The fifth has a publication wait for room in the queue rather than
drop the fill, which costs it six seconds behind a slow disk. The sixth
queues a keep of stripes already being written, and it waits a second for the
first write rather than being dropped at once. The seventh asks fill rights
and sends keeps on goroutines of their own, as step 6 first did. Three keeps go
to one holder before the first is answered, and on a link that drops or
duplicates a frame, which keep it takes is the Go scheduler's choice. That is
what turned `TestSeededTopologyFingerprintIsStable` red on seed 1 with the
cluster cache on. The eighth hands each part of a publication to the fills as
its PUT ends, rather than in part order. Its test holds the first part's PUT
while the others land: with the order kept nothing is filled, and with the
guard a later part's window is. The fill campaign
(`TestFillsSurviveTheirFaultsAndReachTheirProbes`) kills `keep-unranked` and
`no-fill-right` too.

Fifteen guards break the reads of the cluster:

```sh
SPROUTFS_SIM_BUG=cluster-read-by-index \
  go test ./checkpoint -run '^TestAReaderRebuildsFromAnyIndicesAfterTheRanksShift$' -count=1
SPROUTFS_SIM_BUG=cluster-ask-every-holder \
  go test ./checkpoint -run '^TestAHotPageSpreadsItsLoadOverEveryHolder$' -count=1
SPROUTFS_SIM_BUG=cluster-same-holders-for-every-reader \
  go test ./checkpoint -run '^TestAHotPageSpreadsItsLoadOverEveryHolder$' -count=1
SPROUTFS_SIM_BUG=cluster-no-second-request \
  go test ./checkpoint -run '^TestAStalledOrSlowHolderSlowsAReadByTheHedgeDelayAtMost$' -count=1
SPROUTFS_SIM_BUG=cluster-hedge-unbudgeted \
  go test ./checkpoint -run '^TestSecondRequestsStayWithinTheirBudget$' -count=1
SPROUTFS_SIM_BUG=cluster-keep-wrong-stripe \
  go test ./checkpoint -run '^TestAWrongStripeIsNeverReturnedAndItsHolderIsTold$' -count=1
SPROUTFS_SIM_BUG=cluster-store-unbounded \
  go test ./checkpoint -run '^TestStoreReadsPastTheBoundStayWithinTheirBucket$' -count=1
SPROUTFS_SIM_BUG=cluster-mark-on-miss \
  go test ./checkpoint -run '^TestAMissIsNotAFailureOfTheHost$' -count=1
SPROUTFS_SIM_BUG=cluster-mark-every-host \
  go test ./checkpoint -run '^TestAReaderMarksDownAtMostAFifthOfItsList$' -count=1
SPROUTFS_SIM_BUG=cluster-probe-at-once \
  go test ./checkpoint -run '^TestThreeTimeoutsMarkAHostDownAndOnlyAProbeClearsIt$' -count=1
SPROUTFS_SIM_BUG=cluster-fill-marked-down \
  go test ./checkpoint -run '^TestThreeTimeoutsMarkAHostDownAndOnlyAProbeClearsIt$' -count=1
SPROUTFS_SIM_BUG=cluster-repair-held-index \
  go test ./checkpoint -run '^TestRepairAfterAJoinSendsTheIndexNoRankHolds$' -count=1
SPROUTFS_SIM_BUG=cluster-head-never \
  go test ./checkpoint -run '^TestASampledHitChecksItsPartStillExists$' -count=1
SPROUTFS_SIM_BUG=cluster-current-code-only \
  go test ./checkpoint -run '^TestAChangedCodeReadsEveryEarlierWindowWithNoStoreRead$' -count=1
SPROUTFS_SIM_BUG=cluster-no-refill \
  go test ./checkpoint -run '^TestFillsAfterACodeChangeAreUnderTheNewCode$' -count=1
```

The first takes from each rank only the index the list puts on it, which a
change of ranks leaves few of (B5): after a seventh cache joins, the reads go
to the store. The next two ask every rank at once, and ask every reader's
first k+1 in rank order, so the last rank is never asked. The next two never
ask the rest after the delay, so a read waits for its stalled holder, and ask
it whatever the budget holds. The next leaves a wrong stripe with its holder.
The next reads the store past the bound whatever the bucket holds. The next
four break the marks: a miss counted as a timeout, a mark past a fifth of the
list, a probe every second rather than from ten seconds on, and fills sent to
a host marked down. The next offers a rank the index the list puts on it
whether or not another rank holds it: after a join, the new cache is sent an
index a holder below it still holds. The next never checks a sampled hit's
part. The last two break a change of the code: a read that tries only the
list's code, so after a change every earlier window is read from the store,
and a window read under the earlier code that is never filled under the new
one.

Five guards break the hot tier:

```sh
SPROUTFS_SIM_BUG=hot-tier-fill-before-durable \
  go test ./checkpoint -run '^TestAPublicationWritesTheHotTierOnlyOnceItsRegionalPutSucceeded$' -count=1
SPROUTFS_SIM_BUG=hot-tier-read-fails \
  go test ./checkpoint -run '^TestAHotTierThatFailsNeverFailsARead$' -count=1
SPROUTFS_SIM_BUG=hot-tier-beside-cluster \
  go test ./checkpoint -run '^TestAStoreRefusesAHotTierBesideTheClusterCache$' -count=1
SPROUTFS_SIM_BUG=hot-tier-fill-waits \
  go test ./checkpoint -run '^(TestAReadIsNotSlowedByItsHotTierFill|TestAPublicationIsNotSlowedByItsHotTierFill)$' -count=1
SPROUTFS_SIM_BUG=hot-tier-unbounded-queue \
  go test ./checkpoint -run '^TestTheHotTierDropsFillsPastItsQueueOrItsRate$' -count=1
```

The first writes a part to the hot tier before its regional PUT has
succeeded: the hot tier holds the part while that PUT is still in flight. The
second fails a read whose hot tier failed, rather than read the regional
bucket: a hot tier that is down, slow or holds other bytes fails the read.
The third gives a store a hot tier beside a cache that fills the cluster. The
fourth copies a missed object in front of the read, and writes a published
one in front of the publication, which then wait ten seconds for a slow
PUT. The fifth holds every fill whatever the queue's
bound. The hot tier campaign (`TestHotTierSurvivesItsFaultsAndReachesItsProbes`)
kills `hot-tier-read-fails` too.

Eight guards break the membership:

```sh
SPROUTFS_SIM_BUG=membership-write-unconditional \
  go test ./membership -run '^TestConcurrentWritersNeverLoseAnUpdateOrGoBack$' -count=1
SPROUTFS_SIM_BUG=membership-assign-without-release \
  go test ./membership -run '^TestADiskIsAssignedToASecondMemberOnlyOnceTheFirstLetItGo$' -count=1
SPROUTFS_SIM_BUG=membership-serve-stale-generation \
  go test ./peer -run '^TestAHolderBehindReadsTheMembershipBeforeItAnswers$' -count=1
SPROUTFS_SIM_BUG=membership-serve-stale-assignment \
  go test ./peer -run '^TestAMemberThatLostADiskNeverServesItAgain$' -count=1
SPROUTFS_SIM_BUG=membership-ignore-stale-answer \
  go test ./checkpoint -run '^(TestAReaderBehindItsHoldersReadsTheMembershipAndAsksAgain|TestAFillToHoldersAheadIsSentAgainUnderTheirGeneration)$' -count=1
SPROUTFS_SIM_BUG=host-weight-from-share \
  go test ./host -run '^TestACachesWeightIsItsDiskNotItsShare$' -count=1
SPROUTFS_SIM_BUG=orchestrator-code-follows-the-hosts \
  go test ./cmd/sproutfs-orchestrator -run '^TestTheCodeNeverFollowsTheHosts$' -count=1
SPROUTFS_SIM_BUG=orchestrator-drop-quiet-member \
  go test ./cmd/sproutfs-orchestrator -run '^TestAQuietHostKeepsItsPlace$' -count=1
```

The first writes the membership without the condition on the object it read:
two writers of one generation both land, and the store's writes stop being
one line. The second admits assigning a disk its member still holds. The
next two break a holder: one answers under whatever generation it holds,
reading no newer one and telling no sender it is stale, so a holder behind a
read answers under its old generation and the read is refused; one serves
the disk a request names whether or not the membership has it serve it, so a
host that lost a disk asks its cache. The fifth breaks a sender, which takes
a stale answer as a miss: a reader behind its holders reads the store, and a
publisher behind them drops its keeps. The sixth weighs a disk by the
limiter's share, which other writers move. The last two are the
orchestrator's: a code taken from the table for the number of disks wanted
now, which a drain changes, and a quiet host forgotten, so the membership
drains it. Ranking takes no context, so no guard reaches it, and Gremlins
mutates it instead.

Five guards break the shards:

```sh
SPROUTFS_SIM_BUG=shard-ignore-lease \
  go test ./checkpoint -run '^(TestAShardMovesWithItsStripes|TestAStaleMemberThatStillHoldsTheDeviceIsFenced)$' -count=1
SPROUTFS_SIM_BUG=membership-let-attached-shard \
  go test ./membership -run '^TestAShardIsLetGoOnlyOnceTheCloudHasItOnNoMachine$' -count=1
SPROUTFS_SIM_BUG=membership-detach-held-shard \
  go test ./membership -run '^TestCarryDetachesAReleasingShardOnlyOnceItsHostClosedIt$' -count=1
SPROUTFS_SIM_BUG=host-open-shard-without-reading-again \
  go test ./host -run '^TestAHostOpensAShardOnlyWhileTheObjectStillAssignsItThere$' -count=1
SPROUTFS_SIM_BUG=orchestrator-keep-terminating-host \
  go test ./cmd/sproutfs-orchestrator -run '^TestTheOrchestratorMovesShardsOffATerminatingHost$' -count=1
```

The first ignores a shard's lease: a member opens a shard under an assignment
older than the lease, and one whose lease another member took goes on
writing it. The second lets a releasing shard go while the cloud still has it
attached, or cannot say where it is. The third detaches a releasing shard from
under the host that still holds it open. The fourth opens a shard by the
membership the host holds without reading the object again, so a host behind
takes a shard that moved to another host of its machine and keeps that host
from opening it. The last takes a terminating host pod for one that stays, so
its shards stay on it until its node is gone.

`TestAdversarialStarters` runs a fake VMM, not Firecracker, but it runs only on
Linux and as root, because it gives the process's directory to another user.
On a Mac, run these through the Lima instance, as the next section shows.

A guard acts only where the context carries a runtime. So a test that kills a
guard must use a harness that carries a runtime. Nothing outside these
invocations sets `SPROUTFS_SIM_BUG`, and the mutation runner clears every other
`SPROUTFS_*` setting before it runs them.

## Model checking

Simulation explores the executions its seeds reach. A model checker explores
every execution of a small configuration. `spec/ownership/Ownership.tla` is a
TLA+ model of how one VM changes owner. TLC checks it.

The model has one VM, its control record and its checkpoints. Its parties are
the ones [Metadata authority](metadata.md) describes:

- the writer at each epoch, which publishes, selects, keeps, pins fork points,
  reclaims, confirms, hands off and closes;
- opens that take the epoch over, as a recovery or as a migration destination;
- pins and releases made without the epoch;
- the release's sweep, reclamation, and deletion.

Every record write is conditional. Any reply may be lost, and the read that
settles it may fail too.

The invariants are:

- `SelectedReadable`: the selected checkpoint and everything it names are in the
  store.
- `PinnedReadable`: every checkpoint ever pinned stays readable, after its
  record is gone too.
- `KeptReadable`: a kept checkpoint stays readable while it is kept.
- `NoCollision`: no sequence is committed twice.
- `NoMixedGuest`: a migration destination runs a guest only over that guest's
  own checkpoint.
- `SelectionMoves`: only the epoch holder moves the selection, only forward,
  and only to a checkpoint of its own epoch. Pins are only added.

A checkpoint names itself and some of what the guest's state reads. A capture
never reads an older checkpoint that the capture before it did not read. This
is what the code guarantees: a publication that fails, or whose outcome the
writer never learns, gives its sealed pages back to the guest as dirty, so the
next capture republishes them. The release's sweep depends on it. Without it,
TLC finds a release that deletes what a later selection reads.

`just check-spec` runs the `MC*.cfg` configurations. Each takes seconds. It
also runs the mutants under `spec/ownership/mutants`. A mutant puts one defect
back through the `Bugs` constant and must fail with the invariant it names. So a
spec that stops catching anything fails the check. The mutants are a sweep that
forgets kept checkpoints, a sweep that takes what the selection names, a writer
that adopts a record of another epoch, a destination that skips the stale
check, a delete that spares no pin, and a pin without the epoch that names any
checkpoint. `just check-spec-deep` runs the larger configurations under
`spec/ownership/deep`, which take minutes each.

`scripts/tlc.sh` runs TLC from the TLA+ tools 1.7.4 (MIT licence). It fetches
the jar once into `~/.cache/sproutfs` and checks its digest.

`spec/postcopy/PostCopy.tla` models a migration's post-copy:

- the source's book of pages owed, its release and its hold;
- receive attempts that fetch, install, report `Done` or are discarded;
- replies lost after they left the source;
- the orchestrator's retries, its row ageing out of flight, and its survey
  releasing a handover;
- host loss and orchestrator crashes.

Its invariant, `NoSilentLoss`, is that every page no checkpoint holds is on
the serving source, on its way to a live receive, installed on a destination
that runs the VM, or published. Only a lost host or a hold that ran out may
take one.

`spec/recovery/Recovery.tla` models a recovery racing a migration across
hosts that may die. Its survey asks each host in turn, and it reads the row
afterwards. Its invariant, `NoLiveFence`, is that a recovery never takes the
epoch from a holder that is alive.

`spec/lineage/Lineage.tla` models a root, its child and its grandchild: forks
that pin and then create the child, roots that name the parent's checkpoints,
sweeps of each VM, and deletes. Its invariant, `NoDanglingRead`, is that no VM,
and no fork in flight, reads a checkpoint the store no longer has. It also
models candidate designs for the pin collector (TASK-24). A collector that
releases a pin no selected checkpoint reads deletes what a fork in flight is
about to read. The constraints the passing design meets are on TASK-24.

`spec/arena/Arena.tla` models the isolated arena's files: private, tenant
shared, public and fork files, loads by identity, moves, and fork points
lending pages. Its VMMs keep every descriptor they are given, as a compromised
one that ignores `DROP_FILE` does. Its invariant, `Isolated`, is that no VMM
holds a file with a page of another tenant's VM in it, unless the page is
public.

`spec/membership/Membership.tla` is the membership: one object written by
compare-and-set by two hosts and a controller, each from a generation it
read; disks released, let go, assigned, served, removed and listed again as
`membership.Step` admits; a member that lets a disk go only once its own
copy shows the release, and a controller that lets go the disk of a host
that died; hosts that read late and hosts that die; and stripe requests and
keeps delivered to either host, as an address that came to belong to
another host would. A holder behind a request reads the object first,
answers only under the request's generation, and serves only a disk that
generation has it serve. Its invariants are `OneMembership` (no stripe served
or placed under a membership the sender and the holder do not both hold),
`NoRegress` (the store applies one line of generations) and `OneServer` (no
two live hosts serve one disk by the copies they hold). `MCMembership` runs
two hosts over six generations in ten seconds, and `deep/Seven` over seven
in about 25. Its mutants drop the generation check, the assignment check,
the condition on a write and the release before an assignment, and fail
`OneMembership`, `OneMembership`, `NoRegress` and `OneServer`. The first run
found the model's own gap: a host that wrote a let-go had not adopted the
generation it wrote, and still served the disk by its old copy.

`spec/shards/Shards.tla` is a shard moving between members: a controller
that writes the membership one step at a time and acts on the cloud from a
snapshot it read earlier, a cloud that attaches a disk to one machine at a
time and takes it from every process of a machine it detaches it from, hosts
of which two share a machine and open a device one process at a time, the
lease in the shard's header, which a host takes as it opens the shard and
reads again before every region it writes, reports of what a host holds that
arrive late, and hosts that die with the shard open. Its invariants are
`OneServer` (no two hosts serve the shard by the copies they hold),
`OneOpenerAMachine` and `NoStaleWrite` (no host writes a region while another
assignment holds the lease). `MCShards` runs three hosts over eight
generations in about thirty seconds, `MCMultiAttach` two hosts under a cloud
that may attach the disk to a second machine, and `deep/Nine` three hosts over
nine generations, two of which may die, in about two minutes. Its mutants let
a shard go as soon as it is released, which fails `OneServer` under the cloud
that attaches twice, and ignore the lease as well, which fails `NoStaleWrite`.
The model shows the guards are layered: under a single-writer cloud, a shard
let go early is still never served twice, because the cloud refuses the second
attach until a detach has taken the device from the first host.

Three specs model the cluster's disk cache that
[the plan](../plans/disk-cache-2026-10-02.md) proposes, before its code. Each
abstracts the others to the little it needs, so that no run of TLC takes more
than a minute or two.

`spec/diskcache/DiskCache.tla` is the cluster: publications that fail and are
retried, a VM deleted and its name created again, windows striped over the
ranks each host's own list gives (the generation of the membership it holds,
which spec/membership checks two hosts never mix), fills, fill rights, repair, reads of every
rank, hosts marked down, hosts that crash, leave and join, peers that answer
with a wrong stripe, damaged headers, eviction of any stripe, and a deliberate
change of the code. Every stripe names its code. A read tries the host's code
and then each earlier one, and fills a window it rebuilt under an earlier
code under its own. Its invariants are `NoWrongBytes`, `StripesRanked` and
`SurvivesLosses`; the last holds for every code the deployment has used, so a
change of the code leaves every earlier window readable. Rank 1 gives a fill
right once an interval and only while it holds nothing of the window under
its code, and a filler is held to its own list and code as a cache taking a
keep is, as `checkpoint`'s fills do. Its configurations run four hosts with a
2+1 code, two with 1+1, three with 2+2 so that stripes go round the hosts,
and three whose code changes from 2+1 to 1+1 (`MCChange`) and from 1+1 to
2+1 (`MCWiden`), and three shards that move between hosts without bound as
compute scales (`MCShards`), whose `MovesKeepStripes` says a window all of
whose stripes were readable has every one readable again once every shard
serves. Its mutants put back a read without the key check, a stripe
used without its checksum, a part filled before its PUT succeeded, a keep
taken by a cache its own list does not rank, B5, and a read that tries only
its own code, which fails `SurvivesLosses` once the code changes, and a
shard that loses its stripes as it moves, which fails `MovesKeepStripes`.
`epoch-collision.cfg` is wired as a mutant too: a name created again that
draws its old epoch must fail `NoWrongBytes`, which shows the model reaches
the risk the plan accepts.

`spec/disklog/DiskLog.tla` is one host's disk: its log of regions, eviction
with its second chance, the region it keeps free, reads in flight, the write
budget, and restarts with a torn table. Its invariant is `NoWrongBytes`, and
TLC checks it for deadlock. `EvictionProgresses` is a liveness property,
which `MCEviction.cfg` checks under fairness. Its mutants put back eviction
without its free region, which deadlocks, an unbounded second chance, and a
read without the key check.

`spec/disklimit/DiskLimit.tla` is one host's limiter: the free goal, the
spill promise and its allocation, another writer on the same filesystem, and
the write budget. Its invariants are `PromisesKept` and `GoalKept`. Its
mutants put back a limiter that counts a spill file by its allocation, and
B4.

A mutant may expect `deadlock`, or a liveness property, which must then be
its only `PROPERTY`, because TLC does not name the liveness property it finds
violated.

A spec is written from the code by hand, so the two can drift apart with
nothing failing. The simulation ties them together. The simulated object store
reports every change it applies, in its own order (`sim.ObjectStore.Observe`).
Every simulated world checks each change to a control record or a checkpoint's
index object against the ownership spec's properties, by their names:
`SelectionMoves`, `SelectedReadable`, `PinnedReadable` and `KeptReadable`
(`internal/simtest/ownership.go`). `World.CheckSelected` reports what it found,
so every campaign that ends with it checks its whole trace. A check that saw no
record change in a world with VMs fails, so it cannot pass by checking nothing.

[`spec/bugs.md`](../spec/bugs.md) lists every real defect a spec has found.
Each fixed one has a mutant that puts it back.

The ownership model leaves out:

- more than one VM, so the child a fork starts;
- the pager and the post-copy;
- creation, which is the model's initial state;
- the orchestrator, which decides when an open, a migration or a delete may
  happen. A delete starts only once no writer holds the record.

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
guard enabled. `skipped-goos` is a guard whose entry names another system than
the one the binaries are built for. `killed-scheduled` means a scheduled test failed with the source
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
package. `--file` limits the mutations to some production files of the package,
named relative to it, and `--run` selects the tests each mutation runs in a full
suite. The page cache's disk is mutated this way:

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full \
  --file disk.go --file diskformat.go --file diskindex.go --file diskrestart.go --file diskstripes.go \
  --file pull.go \
  --run '^(TestDisk|TestPull|TestAPull|TestOnTwoHosts|TestAReadIsNot|TestALost|TestANewer|TestADiskKeys)' \
  --gremlins /path/to/gremlins --output /tmp/disk-mutations
python3 scripts/mutate-gremlins.py --package stripe --suite full --file stripe.go \
  --gremlins /path/to/gremlins --output /tmp/stripe-mutations
```

The fills are mutated the same way:

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full --file fill.go --file peercache.go \
  --run '^(TestAStoreReadFills|TestAColdBurst|TestAFaultIsNot|TestAPublication|TestAPartTheStore|TestACacheKeeps|TestACacheRefuses|TestACacheDrops|TestRankOneGives|TestACacheReports|TestFillsSurvive|TestAPull|TestOnTwoHosts|TestAPulled|TestTheQueue|TestClosingTheCache|TestTheRateOfKeeps|TestAKeepIsWritten|TestAFillKeeps)' \
  --gremlins /path/to/gremlins --output /tmp/fill-mutations
```

On 2026-10-03 it first killed 83 of 106 mutants, with 16 alive and 7 not
covered. Tests of what the survivors changed brought it to 95 killed, 7 alive
and 4 not covered: the queue's bound at exactly two windows, a cache closed
under its fills, the rate's refill, items that do not fill a keep's payload,
a fill of several pages of one window, a fill right for the last page of a
window, and a keep written at its fill's priority. They also found that a
write the disk failed was counted nowhere; it now counts as dropped. The rest
change nothing a run can see: the default interval, which the tests name
rather than repeat, the bug's own wait, which cache the ranks-change site
takes off the list, a slice's capacity, a zero-sized item that fails its
header anyway, an error message's arithmetic, and a refill of no time. Two
`case` lines Gremlins reports uncovered are run by the refusal tests.

The membership is mutated the same way, with the peer server's side of its
protocol. The orchestrator is a package below `cmd`, which Gremlins names
wrongly on its own, so its run adds `--integration`:

```sh
python3 scripts/mutate-gremlins.py --package membership --suite full \
  --file membership.go --file change.go --file controller.go --file store.go --file view.go --file format.go \
  --gremlins /path/to/gremlins --output /tmp/membership-mutations
python3 scripts/mutate-gremlins.py --package peer --suite full --file cache.go \
  --gremlins /path/to/gremlins --output /tmp/peer-cache-mutations
python3 scripts/mutate-gremlins.py --package cmd/sproutfs-orchestrator --suite full --integration \
  --file membership.go --run '^(TestTheMembership|TestAHostDrains|TestEachStep|TestAConfiguredCode|TestTheCodeNever|TestAQuietHostKeeps|TestTwoOrchestrators|TestTheCodeIs)' \
  --gremlins /path/to/gremlins --output /tmp/orchestrator-membership-mutations
```

On 2026-10-03 the first killed 109 of 165 mutants, with 9 alive and 47 not
covered. Tests of what four survivors changed brought it to 113 killed and 5
alive: draining the member listed first, draining a member that holds no
disk, a view that reads the generation it holds and must not say it changed,
and a host that comes back while its disk is released but still listed. The
5 left change nothing a run can see: a size bound at exactly 1 MiB, an
interval whose zero the default has already replaced, a log line's
condition, the condition of the lost-reply site, and the first of a view's
two checks of the generation it holds, which the second repeats under the
lock. The 47 not covered are `case` lines of `switch` statements the tests
run, the error branches of `New`'s checks, and the constants. The second
killed 55 of 62 in `cache.go`; every mutant of the admission and of
`replied` died but one `case` line Gremlins reports not covered, and the 5
alive are in code this change did not touch: the page bitmap's growth, the
sign of a busy answer's shortfall, a read that names no pages, and the
condition of a guard. The third killed all 5 it covered; the 5 not covered are
the interval constant, a `case` line, and the loop that steps on a timer,
which the tests drive by calling `StepMembership`.

The shards are mutated the same way, a package at a time:

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full --file shards.go --file peercache.go \
  --run '^(TestAShard|TestAStaleMember|TestRemovingAShard|TestACacheKeeps|TestACacheRefuses|TestACacheDrops|TestACacheReports|TestRankOneGives)' \
  --gremlins /path/to/gremlins --output /tmp/shard-cache-mutations
python3 scripts/mutate-gremlins.py --package membership --suite full --file attach.go --file shardcontrol.go \
  --file controller.go --gremlins /path/to/gremlins --output /tmp/shard-membership-mutations
python3 scripts/mutate-gremlins.py --package host --suite full --file shards.go \
  --run '^(TestHostsServe|TestAHostStartedAgain|TestAHostOpensAShard|TestAHostReopensAShard)' \
  --gremlins /path/to/gremlins --output /tmp/shard-host-mutations
python3 scripts/mutate-gremlins.py --package platform/sim --suite full --file networkdisk.go \
  --run '^(TestANetworkDisk|TestTheClouds|TestAMachineThatCrashes)' \
  --gremlins /path/to/gremlins --output /tmp/network-disk-mutations
python3 scripts/mutate-gremlins.py --package cmd/sproutfs-orchestrator --suite full --integration \
  --file membership.go --run '^(TestTheOrchestratorMovesShards|TestTheMembership|TestAHostDrains|TestEachStep|TestAQuietHostKeeps)' \
  --gremlins /path/to/gremlins --output /tmp/orchestrator-shard-mutations
```

On 2026-10-04 the first pass killed, of the cache's shards, 17 with 3 alive
and 8 not covered; of the membership's steps and its carrying out, 39 with 2
alive and 6 not covered; of the host's shards, 12 with 8 alive; of the
simulated cloud, 28 with 6 alive; and of the orchestrator's, all 7 it
covered. Tests of what the survivors changed brought them to 24, 42, 15, 32
and 7 killed: a removal waits for a read in flight, a shard the cache keeps
already is refused, a pass of the controller over a cloud that cannot
describe a disk keeps its listed weight, and one with nothing to do reports
no change, a shard assigned to its host again is reopened, a shard's share
is its device less its header region, a host that left keeps no shard, a
write that ends at a device's last byte is taken, and a crash leaves another
machine's disk alone. The cache's add lost a check AddShard already makes.
What is left alive changes nothing a run can see: a read of no bytes, which
an earlier branch takes, the membership's nil check, which the reads of the
cluster drive outside this selection, which of two members equally loaded
takes a shard or gives one up, how long the slow-close site holds a shard,
log lines' conditions, the size of a simulated disk's filesystem, and errors
of the simulated disk that only a cancelled context makes. The orchestrator's
5 not covered are its interval constant and the loop that steps on a timer.

The peer server is mutated the same way. Its page serving moved from
`vmmigrate`, whose tests still drive most of it, so that part runs with
`--integration` and those tests:

```sh
python3 scripts/mutate-gremlins.py --package peer --suite full \
  --file background.go --file cache.go --file class.go --file conn.go --file handoffs.go \
  --file liveness.go --file requests.go --file server.go --file session.go --file table.go \
  --file version.go --file buffers.go --file internal/wire/codec.go \
  --gremlins /path/to/gremlins --output /tmp/peer-mutations
python3 scripts/mutate-gremlins.py --package peer --suite full --integration \
  --file handoffs.go --file session.go \
  --run '^Test(PeerServer|ResidentListing|BusySource|UnknownVolume|AnUnreachableSource|AVMHandedOverByThePreviousRelease|PeerBudgets|Premortem|PageReplies|ReleaseRefuses|UnreachableSource|DoneReturns|AGuestStoreCounts|ForkAcrossHosts|MigrationMoves|ReleasedSource|APageIsFetched|ARelease|AReplyThatNever|ALoadIsNot|OneBrokenReply|Pages|Resident|AnUnknownVM|Replies|ARequest|AFault|ALateReply|ThisRelease|ThePreviousRelease|AHello|ADestination|AServer)' \
  --gremlins /path/to/gremlins --output /tmp/peer-handoff-mutations
```

On 2026-10-03 the first command left 90 of 476 mutants alive: 370 killed, 12
timed out and 4 did not build. Tests of what the survivors changed left 61 of
481 alive, with 404 killed. Writing them found a server that crashed when a
hello settled on version 1. The second command killed 95 of 119. A run without
`--integration` runs only the tests of the mutated file's own package, so a
mutant of `internal/wire` that `peer`'s tests kill counts as alive there; the
one that marks every payload checksummed is such a mutant. The rest cost speed
or nothing: the size classes of the buffer pool, the length of a buggified
delay, a bitmap one byte longer than it needs, a map entry left at zero, which
of two connections with room takes a request, a bound whose zero means the
default, and the checksum of a reply of version 2 whose every payload is
checked anyway.

The reads from the cluster are mutated the same way:

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full \
  --file clusterread.go --file clusterdown.go --file peercache.go \
  --run '^(TestAPageInTheCluster|TestAPageSurvives|TestAHotPage|TestAStalledOrSlow|TestAWrongStripe|TestTheStoreIsRead|TestSecondRequests|TestStoreReadsPast|TestThreeTimeouts|TestAReaderMarks|TestAMissIs|TestARefused|TestRepair|TestAReaderRebuilds|TestASampledHit|TestClusterReadsSurvive|TestAFaultIsNotSlowed|TestAColdBurst|TestAStoreReadFills|TestRankOneGives|TestACacheReports|TestTheHedgerFollows|TestProbesWait|TestAHostIsMarkedDown|TestOneRefused)' \
  --gremlins /path/to/gremlins --output /tmp/cluster-read-mutations
```

On 2026-10-03 it first killed 129 of 196 mutants, with 54 alive and 13 not
covered. Tests of what the survivors changed brought it to 154 killed, 29
alive and 13 not covered: the hedger's window and budget, the store's bucket
at exactly one read, the probe's growth and its cap, a mark on the third
timeout and not the second, a refused connection, a reader's picks, and a
repair that must skip the first rank short of its stripes. Writing them
changed repair to offer each rank the index the list puts on it first. The
rest change nothing a run can see. Eleven are conditions of a Buggify site or
a guard, which are off in a test that asserts behaviour. Five flip a
condition whose two branches differ only in a probe or a counter the tests do
not read for that case, and Gremlins reports the probe-only `case` lines as
not covered. Others are the defaults, which the tests set rather than repeat;
the slack in a request's byte bound; a buffer released on a path where
keeping it leaks nothing a test can see; the boundary of a timer of zero; the
skip of a host the table has marked down, which fails at once if asked; the
check that the reader is among a window's ranks, which only changes the asks
of a reader holding stripes from an old placement; the spread of a probe's
attempt count; and a repair's count of what is lacking when one index is.

Reading each window under the code it was stored under (TASK-85) was mutated
with the reads and the disk's stripes together:

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full \
  --file clusterread.go --file diskstripes.go \
  --run '^(TestAChangedCode|TestFillsAfterACodeChange|TestRepairAfterACodeChange|TestADroppedEarlierCode|TestDisk|TestAPageInTheCluster|TestAPageSurvives|TestAHotPage|TestAStalledOrSlow|TestAWrongStripe|TestTheStoreIsRead|TestSecondRequests|TestStoreReadsPast|TestThreeTimeouts|TestAReaderMarks|TestAMissIs|TestARefused|TestRepair|TestAReaderRebuilds|TestASampledHit|TestClusterReadsSurvive|TestAFaultIsNotSlowed|TestAColdBurst|TestAStoreReadFills|TestRankOneGives|TestACacheReports|TestTheHedgerFollows|TestProbesWait|TestAHostIsMarkedDown|TestOneRefused|TestPull|TestAPull|TestOnTwoHosts|TestALost|TestANewer|TestADiskKeys)' \
  --gremlins /path/to/gremlins --output /tmp/code-change-mutations
python3 scripts/mutate-gremlins.py --package rank --suite full --file rank.go \
  --gremlins /path/to/gremlins --output /tmp/rank-mutations
python3 scripts/mutate-gremlins.py --package cmd/sproutfs-orchestrator --suite full --integration \
  --file membership.go --file main.go \
  --run '^(TestTheMembership|TestAConfiguredCode|TestTheCodeNever|TestAQuietHostKeeps|TestTheCodeIs)' \
  --gremlins /path/to/gremlins --output /tmp/orchestrator-code-mutations
```

On 2026-10-03 the first first killed 164 of 217 mutants, with 35 alive, 15
not covered and 3 timed out. Four of the survivors were in the new code: a
read that counted its own hits only under an earlier code, and a probe of the
disk's earlier code on every read. Tests of both brought it to 165 killed and
34 alive. The two left in the new code are the condition of the
`cluster-current-code-only` guard, which is off in a test that asserts
behaviour; the rest are the survivors the reads' campaign above names. The
`case` lines of the disk's read under each code are reported not covered,
and the timeouts are the Buggify site's search for a code the list does not
name, which a mutant makes endless. The second killed every mutant of the
earlier codes, and left alive two of the ranking's own and not covered eight
of `CodeFor` and `compare`. The third killed all 17 it covered; the 21 not
covered are the port parsing and `run`, which these tests do not reach.

The hot tier is mutated the same way, and the tiers its reads run against
with the whole package:

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full --file hottier.go \
  --run '^(TestAMissIsFilled|TestAPublicationWritesTheHot|TestAHotTier|TestAReadIsNotSlowedByItsHotTierFill|TestTheHotTierDrops|TestTwoHostsFilling|TestASampledHotHit|TestAStoreRefusesAHotTier|TestHotTierSurvives|TestAFillThatCannot|TestAClosedHotTier|TestAPublicationIsNotSlowed)' \
  --gremlins /path/to/gremlins --output /tmp/hot-tier-mutations
python3 scripts/mutate-gremlins.py --package checkpoint --suite full --file tier.go \
  --gremlins /path/to/gremlins --output /tmp/tier-mutations
```

On 2026-10-03 the first command first killed 49 of 77 mutants, with 8 alive,
19 not covered and 1 timed out. A test of the fills dropped for a regional
GET that failed and for a PUT the hot tier refused, whose counts no test
read, and a test of the fills handed to a closed hot tier brought it to 51
killed, 7 alive, 18 not covered and 1 timed out. Five of the survivors and
the timeout are in the fault-injection sites and the short body one of them
returns, which are off in a test that asserts behaviour. One makes `bug` read
`h == nil`: every publication of a store with no hot tier then dereferences
nil, which the rest of the package's tests catch and the selected tests do
not run. One is the sampled check's `headEvery > 0` at zero, which the
default never leaves. Of the 18 not covered, two are the defaults' constants,
and the rest are `case` lines of `switch` statements the selected tests run:
the queue's bound among them, which `hot-tier-unbounded-queue` shows a test
holds. The second command killed 22 of 24. The two alive are bounds moved
from the store unchanged: a read of exactly the largest extent, and an object
of size zero.

The bounds on the object store are mutated with their whole package:

```sh
python3 scripts/mutate-gremlins.py --package platform/bounded --suite full \
  --gremlins /path/to/gremlins --output /tmp/bounded-mutations
```

On 2026-10-04 it first killed 41 of 61 mutants, with 8 alive, 12 not covered
and 3 timed out. A test of what two survivors changed brought it to 44
killed. One made the bound a timeout names its stall bound or its first-byte
bound alike, which two equal defaults hide; its test, with a stall bound
shorter than the first-byte bound, found a real fault: an upload that moved
from waiting for the first byte to the stall bound kept the first-byte
timer, so it was cancelled at the longer bound. The watch now arms again for
the sooner deadline. The other kept reading after a stalled reply handed over
bytes with its error, over the bytes just read. The 5 alive change nothing a
run can see: a log line's attempt number and condition, a timer generation
counted down rather than up, a progress of zero bytes, and a read of zero
bytes returned rather than repeated. The 3 timeouts are the first-byte
default never applied and the watch's re-arm condition, which make every
attempt time out at once, so the retries never end. The 12 not covered are
the two default constants and `case` lines the tests run.

For test-only changes, select the production package whose behavior
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
epoch, which fences the first host. A closed host gives its peer-server port
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
- the Rust crate's `fmt`, `clippy` and unit tests;
- the TLA+ specs.

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
