# Testing

Deterministic simulation is the main correctness tool. The real control-record,
checkpoint and volume implementations run over simulated disks, networks, object
storage and time. Fault injection changes those dependencies; it never
substitutes a simplified storage implementation. Simulation tests use virtual
time. A failing random workload reports its seed, the commands it ran and the
recent simulator events.

A simulated file is sparse, so a hole costs the test process no memory. A
pager allocates its spill file to hold every dirty page it may keep. The
simulated filesystem counts that space, but every allocated page never written
shares one zero page.

`internal/simtest` is the only way to build a simulated deployment. Every
campaign is a schedule, a fault set and an invariant set over the deployment's
`World` ([One harness](#one-harness)).

`just check` is the gate every push must pass. It runs:

- the determinism rule below;
- `gofmt`, `go build` and `go vet` for Linux and macOS;
- `go test ./...`, then the pager, host, migration, simulation and VMM
  packages again with `SPROUTFS_ARENA=shared`;
- every guard in `scripts/mutation/guards.json`
  ([Negative tests in the tree](#negative-tests-in-the-tree));
- `buf lint`, `shellcheck` and the demo flows' shell tests (`just test-shell`);
- the Rust crate's `fmt`, `clippy` and unit tests;
- the TLA+ specs, model-checked with TLC ([Model checking](#model-checking)).

`just test-race` adds the race detector. `just soak` runs one block of the
extended seed sweep ([Seed sweeps](#seed-sweeps)). `just test-knobs` and
`just test-soak-knobs` run the campaigns with tunables drawn from each seed.

## One harness

A `simtest.World` is one running deployment. Each host in it:

- is a real `host.Host` inside a `sim.Process`;
- has its own `sim.Disk`, with `PowerLossFaults` on;
- keeps its deadlines on a `sim.Clock` and draws its jitter from a seeded
  `Entropy`;
- reaches the object store through its own view of it, which a kill removes
  first.

Every VM has a simulated VMM process that stores into it, and a record of the
bytes its guest believes it holds. The volume managers, checkpoint store,
control records, pagers, peer servers and migration coordinator are the real
implementations.

Every campaign drives the world with the same operations: `Store`,
`Checkpoint`, `Keep`, `Migrate`, `Fork`, `CreateFromKept`, `Release`, `Delete`,
`Takeover`, `Kill`, `Restart`, `Shutdown` and `Settle`, plus `KillDuring`, which
runs one operation on a separate goroutine and removes a host in the middle of
it.

One access in four that `Store` draws is a write fault through which the guest
stores nothing. On x86-64 a cold read reaches the pager as a write fault; on
aarch64 so does a guest kernel's first execution of a page. The page must still
read what the guest last wrote, whether the pager publishes its copy or settles
it back onto the page it was copied from.

One access in eight stores a page of zeros, as a guest kernel does to memory it
frees. A checkpoint publishes such a page as a hole and its retire gives the
page back, so the next read is answered by the volume's hole.

Every campaign checks:

- `Verify`: no guest reads bytes it never wrote, read through that guest's own
  mappings.
- `VerifyDurable`: the same bytes, read back through the volume.
- `CheckSelected`: every record selects a checkpoint that some writer of that VM
  published.
- `VerifyKept`: every checkpoint a record keeps reads, straight from the store,
  as the pause it was kept at: every page of its disks, and its memory and the
  store counter in its VMM state when it has state.
- `CheckDeployment`, at the end.

A VM created from a kept checkpoint must read, through its own fault path and
before it stores anything, that checkpoint's pause. Its memory continues at the
pause's store counter when it resumed, and reads zeroes when it booted cold. A
release must be refused exactly when the record pins the checkpoint.

When a VM's host is lost, the VM comes back at the last checkpoint that landed
or at a later one whose publication was interrupted. The control record names
the sequence, and every checkpoint the world took carries the sequence it was
published under, so the test knows which pause to expect and compares the bytes
page for page. Two checkpoints mixed into one VM fail. The VMM state is
restored with the checkpoint, so a takeover that recovers a volume's bytes
without the registers that ran over them also fails.

The campaigns:

| Campaign | What it is |
| --- | --- |
| `TestSeededTopologyCampaign` | The deployment and its concurrent faults, both drawn from the seed. See [Seeded topologies](#seeded-topologies-and-failure-schedules). |
| `TestSeededTopologyUnderBuggify` | The same with the per-site injection on. |
| `TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld` | The kill campaign. See [Losing a host](#losing-a-host). |
| `TestTwoWritersOfOneVMNeverMixAcrossASwizzle` | The two-writer campaign. See [Clogging and swizzling](#clogging-and-swizzling). |
| `TestScheduledWorldReproduces` | The recorded scenario. See [Overlap scheduling](#overlap-scheduling-experiments). |

Each campaign has a `*Soak` twin that runs over a block of the seed range.
`just soak` runs only these.

## One clock, one entropy source

`platform.Clock` is time as a process sees it: `Now`, `Since`, `Sleep`,
`AfterFunc`, `NewTimer` and `NewTicker`. `platform.Entropy` supplies the writer
nonce that reconciles a lost conditional write, and the jitter that keeps a
host's VMs from checkpointing in lockstep. A nil Clock or Entropy in a
configuration means the wall clock and the operating system's random pool.

Both are passed through `host.Config`, `SupervisorConfig`, `control.Config`,
`vmmemory.Config` and vmmigrate's `Options` and `PeerConfig`. `volume` takes
neither: it reads no clock and draws no random value. `checkpoint` takes both
in `CacheConfig`. A new page cache disk draws its identity from the entropy
source. The clock times the rate of keeps, the interval of fill rights and, in
a cache with no table of peers, the waits of reads of the cluster; a cache with
a table uses the table's clock. `HotTierConfig` takes a clock, and the host
passes its own. Tunable values are in `internal/knobs`.

`sim.Clock` is a virtual clock: no time passes unless a test advances it.
`Advance` releases the deadlines it passes in deadline order, and deadlines at
the same moment in an order the seed chooses. One advance runs the callbacks it
released, in that order, on a separate goroutine. `Settle` waits for them.
Inside a `testing/synctest` bubble, `synctest.Wait` then reaches the quiescent
point. `sim.Runtime.NewEntropy` is the matching seeded nonce source: each draw
is distinct and reproducible per seed.

`TestForkHoldExpiresOnTheSimulatedClock` retires a fork hold at the
deployment's four-interval bound, with no wall-clock wait. It requires the
parent to take its sealed pages back and to be checkpointable again.
`TestReleasingAForkHoldDisarmsItsDeadline` requires an ordinary release to
leave no deadline armed.

## Tunables

`internal/knobs` holds the deployment's tunables:

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

`Defaults` is what a deployment runs. `Validate` refuses a set that the
packages would refuse or that contradicts itself, such as a resident arena
larger than the metadata cap that describes it, or a per-VM drain bound above
the bound of the whole drain.

`Randomize` draws a hostile but valid set for one seed, as FoundationDB
randomizes its knobs under buggify. For example:

- A part size of one byte makes every member its own part, so a publication
  goes through all of its interrupted-upload paths.
- A dirty budget near its floor makes eviction and spill common.
- A hold of one checkpoint interval makes a handover race the interval.

One knob in ten keeps its default. Every draw is keyed by its knob's name, so
adding a knob does not change the values of the others.

With `SPROUTFS_TEST_KNOBS` set, the campaigns draw one set per seed and log the
knobs each seed changed. `just test-knobs` runs `internal/simtest` this way, and
`just test-soak-knobs` its extended blocks. So far, drawing knobs has not
changed any outcome in the seeds that run. It is off by default because the
recorded scenario compares its recordings byte for byte across processes.

A campaign fixes the knobs its world is sized around:

- A pager arena holds every VM of the topology twice, because a fork or a
  migration has the parent's and the child's pages on one host at once.
- The dirty budget is fixed with the arena. These campaigns drive their own
  checkpoints and have no interval loop, so a smaller budget would stall a
  store waiting for a checkpoint that nobody takes.
- Read-ahead and write-ahead stay at one page, because the model counts what a
  source holds against what its guest wrote.
- The open-VM bound has a floor at what a takeover holds beside the handle it
  fenced.

A simulated host runs two pagers, as a real host does: one holds its guests'
memory at 4 KiB, the other their disks at 2 MiB. Each has its own arena and
spill file and gets the values the knobs give. A simulated RAM volume has 512
times fewer bytes than the disk beside it, and the same number of pages.

Every seed draws a loss window between two limits: the bound turned off, and a
window wider than any campaign's clocks reach. These worlds advance a host's
clock only to reach a handover's deadline, so a window that fired would hold a
guest waiting for a checkpoint that nobody takes. A campaign requires that
every host, pager, migration and handoff carries the window through every kill
and swizzle, and that no recovery rewinds more than the window allows
(`VerifyLossWindow`, beside `VerifyDurable` at every recovery).

The four scenarios in `losswindow_test.go` test what the window does to a
guest. They run the checkpoint loop, so a store held back by the window has a
loop to ask.

In `unchanged_test.go`, two children of one fork point read every page of
their memory and disk through faults reported as writes, store nothing, and are
checkpointed. Each child must publish no page and hold no private byte
afterwards, and every page it touched must be the parent's page again.

## The no-cheating rule

`internal/testdeterminism` is a `just check` step. It refuses `math/rand`,
`crypto/rand`, and bare `time.Now`, `time.Since`, `time.After`, `time.Sleep`,
`time.NewTimer`, `time.NewTicker`, `time.Tick`, `time.AfterFunc` and
`ctxsync.Sleep` in the non-test code of
`{volume,checkpoint,control,vmmigrate,host,vmmemory,internal/handover,resource,rank,membership,stripe}`
and every directory below them, Linux-only files included.
`TestTheRuleReachesThePortOfZircon` shows it reaches
`vmmemory/internal/zirconvm` with a stray `time.Now`. A stray wall-clock read
decides how long a hold lives, and a stray `math/rand` call decides which VM
checkpoints first; either makes a seed's run unreproducible.

The allowlist is keyed by file and by what is read, with a count per entry, so
a second read added to a listed file needs its own justification. It has one
entry: the two socket deadlines in the pager's client, which the kernel
compares against its own clock. The rule is tested against sources that
contain each forbidden item.

## Models

An independent byte-array model tracks the expected state after acknowledged
writes and discards. Reads are compared against it after checkpoints, reopens,
takeovers, object-store outages and fork divergence. The checkpoint package's
own model publishes random dirty sets over several checkpoints with forks and
reads every byte back. The pager is checked the same way, with randomized
eviction and captures against independent models.

A write is durable only after a checkpoint publishes it, so a reopen is
compared against the model at the checkpoint the control record selects. A
conditional write whose reply was lost must be reconciled to the outcome it
actually had, never guessed. No passing test uses a known incorrect outcome as
its expectation.

Tests assert page identity directly. Locate reports equal identities across a
fork for pages neither side has written, distinct identities once one side
writes, and private identities for bytes still in an overlay. The pager asserts
that an eligible resident page with a matching identity in the same pager is
mapped without a backing read.

## Failure coverage

Small targeted tests pause execution at publication and ownership handoffs.
They cover conditional-write conflicts, lost successful responses, interrupted
uploads, and concurrent opens or takeover at those points. A checkpoint is
interrupted at every step; each interruption must leave the parent readable
and the retry successful. A publication fenced by a later writer must not be
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
its handles hold, its guests give their pages back, and its process ends. Its
disk is left unchanged.

`Kill` models the machine dying:

1. The store view goes first, so nothing the host had in flight can land and
   its shutdown publishes nothing.
2. Its guests' VMM processes go with it.
3. The process is crashed, as a process crash or a power loss. `PowerLoss`
   resolves every modification the disk had not synced into bytes that were
   applied, dropped, torn or garbled.

A killed host is started again in the same test process, on the disk it left.
A host keeps no durable local state, and its spill file is truncated at every
pager start, so a restart recovers only what the object store holds.

`TestAKilledHostRestartsOnItsOwnDiskAndRewindsToItsLastCheckpoint` in
`internal/simtest` runs one script under three endings (orderly close, process
crash, power loss) and asserts the difference. A handle write is durable if and
only if the close published it. A guest's stores survive only as far as its
last checkpoint, because only a capture publishes a page.
`TestAKilledHostIsTakenOverByAnotherHostAtItsLastCheckpoint` has a surviving
host take the record over while the dead host is down, and the dead host's
handle stays fenced.

`TestAHostLostAtAnyOfItsHandoversLosesOnlyWhatNoCheckpointHeld` is the seeded
kill campaign over one `World`. Every seed removes a host at each of four
places:

- in the middle of a checkpoint;
- while it holds a fork point that another host's child is still reading from;
- while it serves the pages of a VM it handed over;
- while it is the host receiving a VM.

The seed chooses the moment inside the operation and the kill mode. The victim
comes back on its own disk, and the VM is recovered by a bystander host or by
that restart, as the seed chooses. In every case:

- The VM reads as one whole generation, every byte of a page and every page of
  the VM, through its volume and through a guest's own fault path.
- That generation is no older than the last checkpoint acknowledged and no
  newer than the last one written.
- The recovered state publishes again and reads the same afterwards.
- Every kill is in the trace, and [the deployment
  check](#the-deployment-check) passes at the end.

`World.KillDuring` reports whether the kill landed inside the operation or
after it. Across all seeds, the kills must interrupt each of the four
scenarios. When a handover's destination dies, the source's hold is the
deployment's four checkpoint intervals; the test reaches that deadline by
advancing the source host's `sim.Clock`. The campaign runs with `Buggify` on
and its runtime on the context, and `checkpoint/one-page-parts`,
`control/slow-write` and `peer/busy` fire during it. Eight seeds run in the
ordinary suite. `SPROUTFS_CRASH_SEEDS` selects another count, and
`TestHostCrashSoak` runs a block of the seed range.

A kill is inside a fork destination's scenario only between the child's
receive returning and its root landing, a fraction of a millisecond at the end
of a fork of about 4 ms. A moment drawn from the fork's start reached that span
on one seed in a dozen or fewer. So that kill is drawn over 2 ms from
`World.ChildReceived`, the moment the receive returned: about half the seeds
kill while the root publishes, the rest after it landed.

The campaign found:

1. The shared `machine` double emptied its page mapping before it detached the
   memory region, so the pager still mapped and protected pages through a map
   the close was clearing.
2. A fork's child handed to another host has no checkpoint until something
   checkpoints it, so with the interval loop off, opening it failed with
   `volume: fork's root checkpoint is not published`. The campaign takes the
   destination's checkpoint where a deployment's interval loop would.
3. Without `sim.WithRuntime` on the context, fault injection does nothing. With
   it, the `migration-corrupt-peer-page`, `pager-zero-new-page` and
   `checkpoint-part-member-offset` guards each fail the campaign.

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
- opening a checkpoint to be one object-store request, counted through the
  simulated store's trace;
- a checkpoint to write into its index object only the segments it changed, and
  to address every other segment in the index object of the checkpoint that
  wrote it;
- an index object to remain for as long as some root addresses a segment in it,
  and no longer;
- a publication interrupted before its index object to leave a checkpoint that
  reads as absent, and a reference a later publication can still land under;
- a page published as zeroes to leave the segment that names it, with nothing
  in any part for that page.

The suite also covers a multi-part checkpoint, a republication under one
reference that produces byte-identical objects, and a publication whose heap
stays bounded by the part size and the encoders instead of by the dirty set.

A publication's pace is stated in simulated time. `sim.Config.Compute` prices
an encode (`blob.WorkEncode`) in bytes a second, spent while it holds its
encoder, and `sim.Work` counts the encodes and the most at once. It also prices
a fault's planning (`vmmemory.WorkPlan`, a unit per page located) and the
decoding of a segment's page table (`checkpoint.WorkPageTable`, by the
segment's bytes).

- `TestAPublicationEncodesAsManyPagesAtOnceAsItHasEncoders` publishes 64 pages
  of 10 ms each through four encoders: four encode at once, and the last part
  lands 160 ms and one part's PUT after the commit began, not 640 ms.
- `TestAPublicationKeepsEveryUploadSlotBusy` publishes 16 free parts through
  four upload slots: four PUTs at once, and the last part lands after four
  rounds of one part's PUT.
- Both check that the index object's PUT began only after the last part
  landed. The guards `checkpoint-encode-one-batch-at-a-time` and
  `checkpoint-upload-one-part-at-a-time` each fail its test.
- `TestAPublicationDoesTheSameWorkUnderAShake` requires the same fingerprint
  and the same time under three shakes.
- A publication takes each batch's encoder itself, in the order it filled the
  batches. When each batch's goroutine took its own, a later batch could take
  the encoder an earlier one needed, and the parts landed 10 to 30 ms late in
  22 runs of 200 on the default processors and in most runs on one.
  `sim.Runtime.WorkPieces` reports the task and instant each piece of priced
  work began. `TestAPublicationAdmitsItsBatchesToTheEncodersInOrder` publishes
  on one processor and on two and requires no batch to begin encoding before
  one filled earlier. The guard `checkpoint-encode-admitted-in-any-order`
  fails it.

The pull tests that read through the cluster run in a synctest bubble. On the
wall clock, a loaded machine sometimes took longer than the cluster read's
bound (10 ms by default) to read the fixture's disk, so the read also asked
the store. `pullAndRead` now also requires no read to have asked the store
that way. No other test outside a bubble was exposed to the same cause. One
code path was: a cache with no table of peers timed its reads of the cluster
by the wall clock and ignored `CacheConfig.Clock`; it now uses that clock.
`TestAReadOfTheClusterWaitsOnTheCachesOwnClock` gives a pull fixture a disk
that takes a simulated second to read and a clock that never moves, and
requires no read to reach its bound. The guard
`checkpoint-cluster-read-on-wall-clock` fails it.

A peer backing's close ends every request it has in flight, but the request
hears of it from a goroutine that `context.AfterFunc` starts. On one processor
the Go scheduler could deliver a reply sent just after the close before that
goroutine ran. Every request a peer backing makes (a page request, a claim or
a listing) now comes back through one function, `request`, which refuses a
reply once the backing has ended. The tests:

- both `TestClosingAPostCopy` tests, which failed in nearly every run on one
  processor before the fix;
- `TestClosingAForkChildEndsTheClaimInFlight`, which closes a received child
  while its claim is on the wire (66 of 75 runs failed on one processor and 6
  of 25 on fifteen);
- `TestClosingAPostCopyEndsTheListingInFlight`, which failed in every run.

The guards `migration-take-a-reply-after-close`,
`migration-take-a-claim-after-close` and
`migration-take-a-listing-after-close` each fail their test. The claim's and
listing's tests hold the request at a seam in `request`, after its reply is
back and before the backing checks its end, and close the backing there.
Without the seam, `check-guards` reported the claim's guard surviving about 3
runs in 30 and the listing's 20 in 60. With it, each test passes 600 of 600 runs
at one, two and eight processors, and its guard fails it 600 of 600.

No other request in `vmmigrate` or `peer` can take a reply sent after its owner
closed. A receive's stream is cancelled at once, and `Received.Close` waits for
it to stop. A table of peers fails every connection and waits for each
connection's reader, and a reader takes a request out of its connection's
table under the lock the failure empties it under. A request whose own caller
cancels can still take a reply that is ready, because a `select` with both
ready picks either; the callers in `vmmigrate` check their own end once the
request returns.

Both the writer and the reader bound a part's table at 1 MiB. One test
checkpoints 4,000 pages of a volume with the longest allowed name, whose
entries are the widest a table holds and need more table space than one part
may hold. The checkpoint must spread its members over parts whose tables each
fit in one read, counted as one request. 3,000 of those widest members fit in
one part, where the earlier 256 KiB bound needed four parts. The entry cost of
a full 64 MiB part of 4 KiB pages is measured through the encoder.

A range read's request count is counted the same way. A 2 MiB run of 512
4 KiB pages must be:

- two reads when one checkpoint published the pages: the segment that locates
  them and the one extent their members lie in;
- four reads when three checkpoints published them;
- two reads when half of them were never written.

A gap of five members inside a part is read through, and a gap of a hundred
splits the request. A run across a part boundary is one read per part, and
across a page-table segment boundary one read of each segment. A second read
through the cache costs nothing. Through a pager, one cold read-ahead run of
512 RAM pages, on a host that has just opened the VM, is two object-store
requests.

Selecting a checkpoint:

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

The pin comes before the fork. A fork whose control record cannot be written
still leaves pinned what it would inherit, and so does a fork abandoned before
it published a root. A grandchild keeps reading the page its grandparent
published after its own parent has rewritten the last page it inherited. A
fork chain deleted in any order leaves every survivor readable. A delete over
a record that cannot be parsed is refused, instead of sweeping the checkpoints
the record's pins would have spared.

Compaction must rewrite the parts of checkpoints that are less than half live,
stream those rewrites instead of holding them, and move no segment. A segment
compaction did not otherwise change stays in the index object of the
checkpoint being emptied, so that checkpoint also stays.

### Forks

A fork publishes nothing before it returns. Forking a running VM adds the
child's control record, and behind it the parent publishes the point once. A
child's first checkpoint uploads no part of what it inherited, a fan-out of
three children uploads those pages once, and a child reads none of them back
after the seal ends. A fork closed before its first checkpoint adds nothing.
Forks are tested for:

- divergence from their parent;
- use before their own first checkpoint is published;
- refusal on an existing identity;
- forks of forks;
- two forks of one parent diverging independently.

While a fork point holds a parent's pages, the parent refuses a second seal,
and refuses a capture before anything pauses its guest. The parent takes its
pages back when the fork point is retired.

On the parent's host, the children read the sealed pages by page identity, so a
second child maps the first child's resident page without a load. Across hosts,
the child pulls only the pages no checkpoint of the parent holds, publishes
them in its first checkpoint, and then survives the loss of the parent's host.
The host suite has one pause start several children at once:

- every child reads the parent's memory as of that pause;
- the interval loop skips the sealed parent instead of failing on it;
- the parent is checkpointed again only after the last child has published.

No page crosses between tenants, except a public template's. The simulated
arena gives a read-only file to the memory regions of one tenant only, in every
suite and campaign; the public file is given to every region as file 2. In one
campaign two tenants each fork their own template of one image on one host.
`World.Sharing` finds the pages each tenant's guests share, and no page, or
file of an isolated arena, that the guests of both tenants map. In another, two
tenants create VMs from one public template. `World.Sharing` counts the public
pages they share and requires every other page to be one tenant's, and every
page of the public file to be a public template's.

Capture is tested for:

- returning without waiting for publication;
- capturing nothing when preparation or resume fails;
- a checkpoint publishing the sealed pager pages of every memory region, and
  retiring them once the checkpoint is selected;
- a failed publication handing every sealed page back to the guest.

### Unsynced writes

`sim.DiskConfig.PowerLossFaults` makes a simulated device resolve every
modification made since a file's last successful `Sync`, instead of discarding
all of them. Each write is resolved as FoundationDB's `AsyncFileNonDurable`
resolves one:

1. At every open, a kill mode other than `NoCorruption` is drawn for the file.
2. For each 4 KiB device page, a mode no worse than the file's is drawn.
3. Each 512 B sector in the page is then applied, dropped, or written with its
   head or its tail replaced by garbage.

The trace names each resolved modification and its outcome: `applied`,
`dropped`, `prefix_truncated` or `sector_garbled`. Every draw is keyed by the
disk, the file and the modification's sequence, so a choice for one file
cannot change another's.

This is off by default; the rest of the suite assumes a device that restores
its last sync exactly. `SyncDurableProbability` models a device that
acknowledges a flush it did not perform. It is opt-in and unused, because
nothing in sproutfs treats a local file as durable.

Three readers are tested against it:

- The pager's spill file carries a checksum per reservation, held by the host.
  A private page whose bytes do not match is refused with
  `vmmemory.ErrSpillCorrupt` instead of being mapped. Without the check, the
  pager gave the guest a zero byte where it had stored its own value, under
  every seed, and reported nothing.
- A VMM's state file has the VMM's format and no checksum of ours, and a short
  file looks the same as a smaller machine. So the file is refused through the
  handle that the power loss invalidated. The test shows the bytes the device
  kept are not the bytes the VMM wrote.
- Parts go straight to the object store, so
  `TestATornPartIsRefusedRatherThanReadAsMembers` damages them itself. Across
  48 seeds, 44 parts came back damaged. Each was refused by its trailer, its
  table or a member envelope, and none decoded to bytes that were not written.

### Clogging and swizzling

`Network.Clog(from, to, until)` blocks one directional link until a simulated
moment, after which it carries traffic again. A dial or a send over a clogged
link is refused with `ErrUnavailable`, as a partition refuses them.
`Network.Swizzle(addrs, window, random)` gives every link among a set of
addresses its own seeded interval: blocked at a moment in the first half of the
window and healed at a moment in the second half, so links do not come back in
the order they went. Both are schedules read through `Runtime.Now`, which
inside a `testing/synctest` bubble is the bubble's virtual clock, so `Swizzle`
returns at once and starts no goroutine. `Network.Clogged(from, to)` reports
the link state. The simulated network does not carry the object store, but
naming the store as an endpoint can still take the store away.

`TestTwoWritersOfOneVMNeverMixAcrossASwizzle` in `internal/simtest` swizzles
two hosts and the store while one VM is handed over and a second VM is taken
over. The handoff pulls the source's unpublished pages over a peer-server link
that is separated, healed, dropping, duplicating, delaying and given new
latency. The takeover advances the epoch while either writer may be unable to
reach the store. In every case:

- The fenced handle stays fenced and never publishes.
- The selected root names only checkpoints a writer holding the epoch
  published, and nothing past that epoch.
- Every page the source held reaches the destination. A first receive fails on
  every seed, and the world retries it under the orchestrator's policy while
  the source holds the pages
  ([migration](migration.md#a-failed-receive-is-tried-again)).
- A fresh reader sees only the surviving writer's pages.

Sixteen seeds run normally. `SPROUTFS_SWIZZLE_SEEDS` selects another count, and
`TestSwizzleSoak` runs a block of the seed range.

`DropNext`, `DuplicateNext`, `DelayNext` and `SetLink` make up the
`simtest.DroppedPeerFrames` fault, the only fault the generated schedule does
not draw. A frame dropped on an open connection leaves a guest's demand fault,
against the peer holding the only copy of an unpublished page, waiting for a
reply that never comes. Waiting is right for that page, because giving up on
it loses the guest's memory. So the drop belongs in a campaign where every
receive is a bounded attempt that is retried. The generated schedule uses
`simtest.DegradedLinks`, the same kit without the drop.

```sh
SPROUTFS_SWIZZLE_SEEDS=300 go test ./internal/simtest \
  -run '^TestTwoWritersOfOneVMNeverMixAcrossASwizzle$' -count=1
```

## The deployment check

`volume.CheckDeployment(ctx, store, prefix, allow...)` lists the whole object
namespace of one deployment and reports every way the durable state is
inconsistent with itself. It runs at the end of a scenario, after every handle
is closed. Every `internal/simtest` campaign and its soak, the recorded
scenario, and the host harness's cleanup call it.

It requires:

- every control record and every part to parse at the format version this
  build writes;
- every checkpoint that a selected, pinned or kept root names to exist, with
  the part count and member bytes the root recorded;
- every member of those parts to be a page or a state that the part's own VM
  published. Its bytes are billed to the VM whose key holds them, so this keeps
  a page billed to the VM that published it when compaction moves it;
- every pinned sequence to be a published checkpoint of the VM whose record
  pins it. A pin names no holder, and no descendant's record names the pin, so
  the check verifies that what a pin protects is whole: the checkpoint and
  every checkpoint its root names;
- every object under `vm/<id>/ckpt/` to be reached by some record's selected
  checkpoint, a pinned or kept checkpoint, or a checkpoint a compaction
  emptied, which is spared for one checkpoint of grace. Naming a checkpoint
  spares all of it;
- the bill to be the store. `volume.StoredBytes` for each tenant, and for the
  VMs of no tenant, must report exactly what the listing holds under each VM.
  Every key is under some VM, so the bills summed over VMs are every byte in the
  store, each billed once.

Each caller names the classes of leftover it expects, and only those go
unreported. Each class is what a host lost at a particular moment leaves
behind and no writer returns for; cleaning it up is a collector's job.

| Allowance | What it admits |
| --- | --- |
| `AllowSupersededEpoch` | The checkpoints of a writer epoch below the record's. A new handle reclaims only what it published itself, so every takeover leaves the checkpoint it opened on and whatever its fenced predecessor abandoned. |
| `AllowUnpublishedIndex` | A checkpoint whose parts are there and whose index object never landed: a publication interrupted before its commit. |
| `AllowUnreferencedCheckpoint` | A published checkpoint of the record's own epoch that nothing selects or pins: a sweep the store refused. |
| `AllowUnrecordedVM` | Objects under a VM with no control record: a create interrupted before its record, a delete interrupted after it, and, with no host lost, the pinned checkpoints a finished delete leaves. Deleting a VM that was ever forked always leaves these. |

The check's first run found two leaks, both fixed:

- A create never reclaimed the first checkpoint it published, because the
  handle counted nothing as its own until it had published again.
- A fork closed before it published its root left its own record behind,
  which made the fork's identity unusable.

## Handovers

A handover is the only operation that can lose a VM's memory, so the campaigns
spend most of their steps on handovers. A migration publishes nothing. The
source stops its guest, gives up its volumes, and serves the pages no
checkpoint holds until the destination reports it has them. A fault that
removes the source costs the VM the pages written since its last checkpoint.
`World.Migrate` models this rewind exactly.

Every hop checks:

- The source's handle is refused a store as soon as the handoff is taken.
- The destination's guest restores the VMM state the source's pause captured,
  and continues at the same store counter.
- The destination's first read, through its own mappings and before it writes
  anything, is the source's last checkpoint plus the pages the source serves.
- A refused migration leaves the guest running where it was, with every memory
  region unsealed, every page writable and its vCPUs running.
- A receive that fails leaves no guest of the VM running on its destination,
  for a migration and for a fork's child, so a retry is sound on any host.
- A receive that fails is retried under the deployment's handover policy, on
  the same host or another, until the source no longer holds the pages. Only
  then is the VM given up and reopened at its checkpoint.
- A receive in flight and a retry end on the orchestrator's rule,
  `handover.Hold.Gone`. A source the world has lost is no longer listed. A
  source cut off by `simtest.IsolatedHost` is listed and silent, so only its
  hold ends the wait. No campaign draws that fault.
  `TestAMigrationWhoseSourceIsCutOffEndsAtItsHold` runs it with a hold shorter
  than the harness's patience (`Config.Hold`), so the clock shows which ended
  the wait.
- A fork's child is received on the same rule, against its parent's host and
  the hold that host keeps the point for it.
  `TestARemoteForkWhoseParentIsCutOffEndsAtItsHold` cuts that host off during
  the child's post-copy: the receive ends at the hold, the fork does not
  happen, and the parent's host retires the point when its own clock gets
  there.
- A receive whose caller hung up goes on, and no other is made while a host
  reports it in flight. It takes the VM in, or fails like any other receive.
  Once the handover is over, no such receive may take the VM in.
  `simtest.OutlivedReceive` makes the caller of one receive hang up as its host
  begins a slow guest start. `simtest.LostReceiveAnswer` lets one receive
  finish and tells its caller it failed. The campaigns draw both, for
  migrations and fork children. `TestAReceiveThatOutlivesItsCallerStartsNoSecondGuest`
  requires one guest started for the VM, on the host whose receive outlived its
  caller.
- A fork child's receive that fails for its caller fails its fan-out, which
  gives up every hold. A child claims its hold before it runs, so one whose
  receive went on finds the hold gone and is discarded, and one whose answer
  was lost after its claim is reported claimed by the give-up and deleted.
  `TestAForkChildWhoseReceiveOutlivesItsFanOutNeverRuns` and
  `TestAForkChildWhoseAnswerWasLostIsDeleted` run the two, and `Settle`
  requires every such receive to end without its child. The orchestrator tests
  of the same names and `TestAForkWhoseCallerHangsUpIsStillRolledBack` cover the
  orchestrator's side; `host/claim_test.go` covers the claim.

The recorded scenario adds the layout refusal: a handoff that would truncate a
memory region or map beyond its volume is refused before any guest starts.

`Handover.Meanwhile` runs after the receive and before the release and the
close of the post-copy, while the destination's memory regions still ask the
source for what they fault. Half of the migrations and forks the schedule
draws use it. Every guest stores, a fork's parent among them. The destination
is checkpointed, which publishes and retires what it received, stores again,
and every guest is read back. A checkpoint whose retire refused to give up a
guest's page (`vmmemory.ErrUndroppable`) fails the run, there and everywhere
else, because the pager keeps the page and nothing else would notice.

`TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes` is that
scenario on its own, for a fork and for a migration. Every page is the
source's at the handoff. The destination stores into all of them, half of them
zeros, and is checkpointed. Its arena is smaller than one guest, so reading the
guest back gives up every page it published and reads it again while the
source still serves. It requires the destination to have read a page it
published (`vmmigrate.ProbePublishedSinceHandoff`), every page to read the
guest's bytes before and after the release, and the volume to hold the last
checkpoint. A fork's parent keeps storing throughout, and none of it reaches
the child. A checkpoint of the parent in the window is refused with
`volume.ErrSealed`. [The migration
notes](migration.md#the-sources-copy-after-the-destination-publishes) record
what it found.

The scenario checks the peer backing's two answers against each other.
`testbacking` records a load whose answer about which pages are the source's
own differs from what `Locate` reports for them, and `Verify` fails the run on
it. The pager believes both answers, so a backing whose answers disagree hands
some guest the wrong bytes. The pager's own test double answers from what the
pager told it it took, not from the backing's rule.

`vmmigrate`'s own suite keeps the tests about the package:

- `Done` returns only after every unpublished page is on the destination.
- The destination's next checkpoint publishes those pages.
- The source's peer server can then be removed.
- Failure to resume.
- Cancellation before a handoff.
- A handoff that waits for a publication another call already had in flight.
- `TestSourceLostAfterHandoffRewindsToTheLastCheckpoint` drops the source's
  pages right after the handoff and requires the VM to come back at the
  checkpoint its control record still selects, rewound by exactly the writes
  since.

The simulator's listener-close regressions require queued clients to
disconnect and in-flight dials to reject a closed listener, while accepted
connections remain usable.

## Seeded topologies and failure schedules

`internal/simtest` generates both the topology and the faults from the seed, as
FoundationDB's `CompoundWorkload::addFailureInjection` does, so it reaches
combinations such as a partitioned source while the store is unavailable while
a second host takes the VM over.

`simtest.NewTopology(random)` draws:

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
it. The deployment check refuses any root segment or part member of one.
`internal/simtest/ephemeral_test.go` states the four requirements as
scenarios: never published, lost with its host and at a stop, reaching neither
a local nor a remote fork, and carried by a migration.

Whether a fork's host is its parent's decides whether the child shares its
parent's pages or pulls them from the parent's peer server. A failing seed
prints its topology, and a printed topology can be reproduced.

`simtest.Fault` is one thing that goes wrong, with `Begin`, `End` and `Holds`.
`Holds` is what the fault must leave true after it ended and the world
quiesced. A fault is a condition of the world, so several can be active at
once.

`simtest.Driver` places every fault of the set in a schedule of 26 steps, at
seeded offsets. One to three faults are active at a time, for a seeded number
of steps. Each step runs a few of every guest's stores, then one operation:

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

Stops and starts are drawn independently, so a stopped VM sits out as many
steps as the seed draws, and faults land on a VM that exists only as its
objects. A host that comes back without that VM must leave it alone, and so
does the world's `Settle`. Every fault's start and end is recorded through
`sim.Trace.Record`, beside the adapter operations it perturbs.

The faults:

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

`dropped-peer-frames` is the same kit plus `DropNext`, and the schedule does
not draw it ([Clogging and swizzling](#clogging-and-swizzling)).

An operation under a fault need not succeed. But:

- **No guest reads bytes it never wrote.** After every step, every page of
  every running VM is read back through the guest's own mappings. A page whose
  only copy is on a peer a fault removed cannot be read; that is reported while
  a fault is active, and the page must be readable once all faults have ended.
- **Every VM's selected checkpoint is one it published.** Every takeover checks
  the sequence the new writer inherits, and the end of the run checks every
  record.
- **`CheckDeployment` passes at the end**, with the allowances this campaign's
  faults justify.
- **Every seal is reported.** After every step, a VM that a fork point holds
  sealed must have a hold for one of its children in its host's `Serving`. A
  hold whose child runs on that same host must owe nothing.

The world's `Settle` runs the orchestrator's survey before every step. It
releases every handover a host still holds whose VM exists, and gives up the
rest. A VM nobody is running (a source that could not resume, a handoff no
destination took while its source held the pages, an unfinished post-copy, or
a lost host) is opened again by a host that can run it, and the model rewinds
to the checkpoint its record selects.

`TestASourcePartitionedWhileTheStoreIsAwayAndASecondHostTakesOver` tests that
combination directly: a destination that cannot reach object storage, and then
one that takes the VM over and only then finds it cannot fetch the pages the
source still holds.

```sh
go test ./internal/simtest -count=1
just soak 1 100          # the seed sweep, which runs every campaign
```

Once every campaign ran real hosts, they found:

- **A deleted VM's identity was reused while a host's page cache still held its
  pages** (fixed in `8ccfb15`). `checkpoint.Cache` keys a page by the VM, the
  checkpoint sequence, the volume and the page. A VM created again under a
  deleted name started at the same epoch and first sequence, so a host that
  held the dead VM's pages served them to the new one. The smallest seed is 45:
  fork `vm-2` from `vm-1`, delete `vm-2`, fork `vm-2` again. Seventeen of the
  first two hundred seeds reach it. Dropping the dead VM's pages from the
  deleting host's cache fixed fourteen; the fix is that a creation draws its
  first epoch from the host's entropy, and `Manager.Create` refuses an identity
  whose `vm/<id>/` namespace still holds objects. See [the
  architecture](architecture.md#identities-and-reclamation).
- **A sweep deleted a checkpoint that a pinned root names.** `protectedBy`
  asked the root for the checkpoints it reads, which leaves out checkpoints the
  root's own compaction emptied and still names. `CheckDeployment` reported a
  part that does not read. Ten of the first two hundred seeds reach it. The fix
  spares everything the pinned root names.
  `TestReclamationSparesThePacksAPinnedIndexOnlyNames` in `checkpoint` covers
  it.
- A frame held by the stalled-stream fault could be a guest's demand fault,
  which has no deadline, and deadlocked the bubble. The hold now ends on the
  caller's cancellation, on the end of the fault, or on a bounded simulated
  wait.
- A checkpoint's sweep runs after its publication, so a host that exits right
  after a checkpoint lands cancels the sweep and leaves an unreferenced
  checkpoint. The campaign gives its sweeps a moment to run before it closes,
  as a draining host would, and each checkpoint the world takes waits for its
  sweep, so a sweep does not race the next step's faults.
- A frame dropped by `Network.DropNext` on a peer-server link hangs the guest,
  because the connection stays open and the reply never comes. So the
  degraded-links fault does not drop frames; the lost-page-replies fault
  models a lost reply, which costs the connection.

## The cluster soak

The campaigns drive model guests: a `simtest` guest is a mapping plus a model
of what it stored. None runs Firecracker, KVM, the real pager's userfaultfd
path, GCS or Kubernetes.

`scripts/demo-gce.sh soak` runs a seeded schedule of forks across two hosts,
migrations, stops and starts on a real cluster, and kills a host once. After
every operation it asks every guest whether its memory and disk still hold the
bytes it wrote. `cmd/sproutfs-guest-witness`, which both guest images carry,
fills a resident buffer and a file of the same size with the pattern of a
`(seed, step)`. The pattern is a pure function of the seed, the step and the
page, so the expectation lives in the script, not the guest.

The soak ends with `volume.CheckDeployment` over the whole namespace, which
`sproutfsctl check` runs through the orchestrator after every VM has been
deleted. Only the templates (one per guest image, named by the image's bytes)
and the checkpoints the deleted VMs pinned may remain. The allowances are the
live deployment's:

- a publication in flight;
- a deleted VM's pinned checkpoints;
- a VM's own root and a template import's intermediate checkpoints;
- the superseded epoch every takeover leaves, including a template whose
  unfinished import a later import recovered.

[The demo notes](demo.md#the-soak) describe the run. The cluster's only fault
is one host killed without grace, the only fault a k3s node can reliably be
asked for. The campaigns cover the fault combinations; the soak covers the real
VMM, pager and object store.

## Overlap scheduling experiments

`sim.NewScheduler(seed)` is the shared controller. Construct it inside a
`testing/synctest` bubble and pass its `Wait` method to `sim.Config.Wait`. Run
the workload, including shutdown of background actors, in another goroutine,
and call `scheduler.Run(done)` from the bubble's parent. Stable zero-width
waits can admit workload operations, and `scheduler.Record` captures
acknowledged or recovered bytes. `scheduler.Recording(runtime.Trace())` returns
independent protobuf streams; write them outside virtual time with
`recording.WriteFiles`.

The one scenario, `TestScheduledWorldReproduces` in `internal/simtest`, runs
over a `World` of three hosts with a fixed seed, in three parts.

The volume part checks complete disk and RAM images against an independent
byte model through:

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
through the volume. The controller orders every guest store and every memory
region seal.

The host part:

- A drain cancelled before it began does no storage or transport work.
- A host is lost under power loss, and its VM is taken over at the checkpoint
  its record selects.
- The host comes back as a fresh process on the disk it left behind.
- The VM is migrated back onto it.

The transport names the host a dial comes from, because the simulated network
models a link between hosts. The ordinary host suite covers real sockets.

```sh
python3 scripts/check-overlap-reproducibility.py --scenario world --seeds 32 --runs 2 --maxprocs 1 2 4 8
```

`sim.WithTask` labels logical callers before concurrent work. The scheduled
harnesses put each memory region's backing loads and authority checks through
`Runtime.Admit`, so two concurrent identical reads are ordered by their logical
caller. Disk and object-store operations enter a gate before they compete for
their shared queue, so spill and page-cache work cannot take the queue in an
uncontrolled order. Ordinary adapters require no controller. Manager shutdown
orders handles by VM identity and acquisition, not Go map order. Disk and dial
attempts already cancelled consume no fault plans or I/O IDs.

A destination's requests to its migration source are admitted through
`vmmigrate.WithAdmission`. A post-copy stream cancelled between two requests
either sends the next request and has it refused, or abandons it before
sending. Both are correct, because every page the stream did not fetch is in
the destination's own checkpoint; the admission point lets the scheduler order
the two.

A request that waits for room (its class's budget at the peer, a slot on a
connection, a dial another request began, or the host's background budget) is
admitted again each time it is woken. Without that, the Go scheduler chose
which woken request took which connection, and seed 3 of
`TestScheduledWorldReproduces` failed in about one run in a few hundred.
`TestARequestWokenFromAWaitForRoomIsAdmittedAgain` holds the woken request at
its second admission and requires the other to go on alone. The guard
`peer-woken-requests-go-on-together` skips the second admission.

The dynamic experiment runs four concurrent clients through three rounds each
of requests, synced disk writes, object publication, replies and object
readback. One put fails before application, and one loses its reply after
application. Every acknowledged value is checked against an independent byte
model, and the disk is power-cycled before durable readback. It uses the real
simulated network, disk and object-store implementations.

`sim.Config.Wait` optionally supplies completion timing control. Network sends
pass their configured latency/jitter range without selecting a completion time
first. Fixed-latency disk and object operations expose a +/-25% experimental
window. Accept and receive also gate delivery into application code. Leaving
`Wait` nil keeps ordinary simulation timing and concurrency.

The controller calls `synctest.Wait` until every other goroutine is at a
durable wait, selects an overlapping completion window, releases one operation
in a seed-keyed order, and discovers the work that completion created before
it chooses again. Cancellation wakes the controller, which handles it at the
next such boundary. This requires stable logical IDs, and stable admission for
requests that share a sequenced resource. It does not govern shared-memory
interactions between I/O boundaries.

Capture and compare separate processes with:

```sh
python3 scripts/check-overlap-reproducibility.py --scenario dynamic --runs 10 --maxprocs 1 2 4 8
```

The script prints a retained output directory. Each process runs the requested
seed count (32 by default) in both workload creation orders. Full `.pb` files
contain length-delimited `sproutfs.sim.v1.TraceEvent` messages in observed
order: submissions, releases, quiescent completions, task lifecycles and byte
checks. The `.adapter.pb` stream keeps every event of the simulator's trace,
with its order, time, operation, resource, outcome and byte count. The
`.execution.pb` projection removes only submission/arrival events and
renumbers the rest, without sorting. The script compares all three forms byte
for byte against the first process, writes hashes and the first full-trace
difference to `report.json`, and exits nonzero if any form differs.

The regular suite runs `Test*ReproducesAcrossProcesses` for the dynamic adapter
workload and the scheduled world scenario. Each launches the test binary in
three fresh processes (`GOMAXPROCS=1`, `4`, and `4` again), each running seed
1 in both caller creation orders against the independent model. Execution and
adapter recordings must match byte for byte across processes. Missing or empty
recordings fail. A mismatch prints the first differing readable record.
Arrival traces are not required to match or to differ.

```sh
go test ./platform/sim ./internal/simtest \
  -run 'ReproducesAcrossProcesses$' -count=1
```

Repetition cannot enumerate every interleaving; the seed campaigns and the race
detector remain complementary.

The schema is in `platform/sim/proto/sproutfs/sim/v1/trace.proto`; regenerate
it with `buf generate`. Each `.txt` companion is decoded from its protobuf
file. Events are buffered in the bubble and written after it exits. To retain
files from one process, set `SPROUTFS_OVERLAP_TRACE_DIR` to a fresh directory
and run `go test ./platform/sim -run '^TestDynamicOverlapTraceFiles$' -count=1`.
`SPROUTFS_OVERLAP_TRACE_SEEDS` selects the seed count. The earlier
declared-batch experiment is still available with `--scenario batch` and
`TestOverlapPrototypeTraceFiles`; it requires every operation to register
upfront and has no adapter trace file.

## Fault injection, probes and fingerprints

Three primitives in `platform/sim` reach into the real volume, checkpoint,
control, pager and migration code. All three read the runtime from the
context, which a harness sets once with `sim.WithRuntime`. In a context without
a runtime, as in every deployment, each returns false.

`sim.Buggify(ctx, id, p)` is FoundationDB's two-level switch. A site is
activated once per run with probability 0.25, by a draw that depends only on
the seed and the site's id. An activated site then fires with probability `p`
on each call. So one seed explores a few faults deeply, and adding a site does
not change which sites another seed activates. A call's draw is keyed by its
number among the site's calls. In a controlled run each task (`sim.WithTask`)
numbers its own, so a task's firings do not depend on how other tasks' calls
fell between its own, and a wake-up the scheduler missed cannot shift what a
seed chose (`TestATasksBuggifyDrawsDoNotDependOnAnotherTasks`). Outside one, the
calls are numbered in arrival order. `sim.BuggifyDelay` makes the
same decision and then waits for a seeded time. Every site is off unless a
campaign calls `Runtime.SetBuggify(true)` or passes `Config.Buggify`, which
keeps the recording and replay comparisons byte-identical. The sites and the
[tunables](#tunables) a seed draws are separate switches.

The code must survive each of these site faults without reporting it to its
caller:

| Site | What it does |
| --- | --- |
| `checkpoint/one-page-parts` | Fills a part at one page, so a checkpoint publishes several of them |
| `checkpoint/give-up-on-existing-part` | Gives up on a part a retry of the same publication finds already written |
| `control/slow-write` | Makes one control-record write take seconds |
| `vmmemory/evict-past-a-free-slot` | Takes a victim although the arena has a free slot |
| `vmmemory/prefetch-slow` | Holds a prefetch's read back for up to 50 ms, so faults meet it in flight |
| `vmmemory/prefetch-refused` | Leaves the rest of a fault's run unread, as if the prefetches in flight were at their bound |
| `vmmemory/prefetch-failed` | Fails a prefetch's read after the backing answered, so its pages are left to their faults |
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

A simulated disk with `DiskConfig.ReadChaos` adds three sites, as
FoundationDB's `AsyncFileChaos` does. They are off on every other disk, because
most of what a host keeps on its disk has no checksum of its own:

| Site | What it does |
| --- | --- |
| `sim/disk-slow-read` | Holds a read for a seeded time |
| `sim/disk-read-bit-flip` | Flips one bit of what a read returns |
| `sim/disk-misdirected-read` | Returns the bytes at the start of another recent write |

The simulated disk also has sites in what a disk limiter reads. The limiter may
report a reading it refuses, but must stay safe: the cache holds no more than
its share, every promise is counted whole, and the cache writes no more than
its budget allows.

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

A simulated disk is on a filesystem of a size the test names, or one the seed
draws between 5 GB and 105 GB with at least 5 GB or 7.5 % free, as
FoundationDB's simulator does. Other writers hold space the test sets and drift
by up to `DriftBytesPerSecond`. A write that needs more than is free fails with
`ErrNoSpace`. `TestTheDiskLimiterStaysSafeUnderFaults` in `resource` runs the
limiter over such a disk for 24 seeds with the sites on, under a cache that
fills whenever it may and spill files that fill as guests spill. It requires
every site to fire and every disk limiter probe to be reached. When the faults
stop and the disk stands still, the share must be exactly what the disk
implies.

### The membership

[The membership](hosting.md#the-membership) is one object every host reads and
any process writes by compare-and-set. `membership` has three sites in its
store. The writers must lose no update and never write an older generation,
and a host must keep the newest generation it read.

| Site | What it does |
| --- | --- |
| `membership/read-fails` | Fails a read of the object, as a store that is down does |
| `membership/write-fails` | Fails a write before the store applies it |
| `membership/reply-lost` | Loses the reply to a write the store applied |

`TestConcurrentWritersNeverLoseAnUpdateOrGoBack` in `membership` runs sixteen
seeds. Four writers change one membership at once: each joins a member with a
disk, serves it and sets its weight three times; two drain and leave; two take
a shared disk from each other repeatedly through release and let-go. Two views
read it. The world takes the store down and back, loses replies, and fails
requests, at moments a `sim.Scheduler` chooses, with the sites on. The store
must apply one line of generations from 1, each one write that
`membership.Step` admits after the one before, each with its own nonce. Every
update that returned must be the generation the store holds at its number. At
the end each member that stayed has its last weight and the others are gone,
and no view went back. Every site must fire and every store and view probe be
reached across the seeds. Under `membership-write-unconditional` the store's
writes stop being one line.

In `peer`, `TestAHolderBehindReadsTheMembershipBeforeItAnswers`,
`TestAHolderOnAnotherGenerationAnswersStaleWithItsOwn` and
`TestAMemberThatLostADiskNeverServesItAgain` run two peer servers over one
disk with views over a simulated store. A holder behind the request reads the
membership and answers. A holder ahead, or one that cannot read it, answers
stale with its own generation and never asks its cache. Once the membership
moves the disk from A to B, A answers not-me under the new generation and
stale under the old one, and B serves. In `checkpoint`,
`TestAReaderBehindItsHoldersReadsTheMembershipAndAsksAgain` and
`TestAFillToHoldersAheadIsSentAgainUnderTheirGeneration` put one host behind
the rest: its reads come from the cluster with no store read but the open of
the index object, and its fills land with none dropped as stale.
`TestADiskIsAssignedToASecondMemberOnlyOnceTheFirstLetItGo` in `membership`
is refused a disk until its member lets it go. In the read campaign in
`checkpoint`, only some hosts read each new membership at once, so the
campaign reaches a holder that caught up, a stale answer and a sender that
caught up. The orchestrator's tests in `cmd/sproutfs-orchestrator` run it as
the controller over a simulated store: a join is one generation and its disk
serves in the next; a host drains before it leaves in four generations, of
which one moves windows; no generation moves the windows of more than one
disk; a quiet host keeps its place; the code never follows the hosts; and two
orchestrators at once leave one line of generations ending where one alone
would.

### Shards

[Shards on network disks](hosting.md#shards-on-network-disks) move between
hosts through the cloud's attach API. `platform/sim`'s `NetworkDisks` is a
cloud of single-writer disks: each holds one device file, which a detach or
its machine's crash power-cuts, losing what was not synced and failing every
handle.

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

`TestShardsSurviveTheirFaultsAndReachTheirProbes` in `internal/simtest` has six
hosts serve six shards under 4+2 for eight seeds. A seeded schedule has hosts
leave as an autoscaler removes them, die with their shards open, die while a
shard moves to them, join again, have a shard detached under them by hand, and
has a process open a shard's device beside its member and keep it while the
shard moves. After every step the guest reads back every page it wrote. Every
site must fire and every shard probe be reached: a shard opened, closed, not
attached yet, lost under its host, and refused by its lease. A shard fenced
while a process still holds its device cannot be reached here, because the
simulated cloud, like Compute Engine, takes a detached disk from every process
of its machine; `TestAStaleMemberThatStillHoldsTheDeviceIsFenced` in
`checkpoint` keeps two handles of one device and reaches the fence.

- `TestHostsScaleUpAndDownWithNoStoreReadForACachedWindow` scales six hosts
  down to three and back, one at a time. After each, every shard serves, the
  hosts serve within one shard of each other, and a VM opened on a host that
  was there reads every page from the shards and nothing from the store but its
  VMM state.
- `TestReadsDuringAShardsMoveHedgeAroundIt` reads the VM while a shard is
  released and not yet served elsewhere, and after a host is lost with its
  shard attached, with no store read.
- In `checkpoint`, `TestAShardMovesWithItsStripes` moves a shard to another
  member and machine, reads the same stripes back, and refuses the member it
  left. `TestAShardReadsBackOnlyTheRegionsItsLeaseNames` opens a device of
  8,191 slots holding a few regions in a few dozen reads.
- In `membership`, a model of hosts, machines and a cloud steps `Next` and
  `Carry`: shards spread over the members, move off a host being removed in the
  order released, closed, detached, let go, assigned, attached, opened,
  serving, and are let go off a dead host only once detached.
- In `host`, three hosts on a simulated cloud serve six shards and move them
  when one leaves; a restarted host is a new member; a host opens a shard only
  while the object, read again, still assigns it there.
- In `cmd/sproutfs-orchestrator`,
  `TestTheOrchestratorMovesShardsOffATerminatingHost` moves the shards off a
  host pod as soon as it is terminating.

Ranking is a pure function, and `rank`'s property tests state it: a join or a
leave changes a window's first k+m by at most one cache, weights spread windows
in proportion within half a point over 100,000 windows, equal scores go to the
lower identity, and the ranks of a few windows are written out so that a host
of any architecture must agree.

### Journals

[Durable flush](architecture.md#durable-flush) writes a host's flushed blocks
to its journal disk. The journal's sites, and the simulated cloud's for making
journal disks:

| Site | What it does |
| --- | --- |
| `journal/write-slow` | Holds a batch's write for up to 50 ms, so commits pile up behind it |
| `journal/write-fails` | Fails a batch's write before it reaches the disk |
| `journal/write-torn` | Writes the first half of a batch, then fails |
| `journal/sync-fails` | Fails a batch's sync |
| `journal/header-write-fails` | Fails a write of the header |
| `sim/network-disk/create-slow` | Holds a disk's creation |
| `sim/network-disk/create-fails` | Fails a disk's creation |
| `sim/network-disk/delete-fails` | Fails a disk's deletion |

`TestAJournalKeepsEveryAnsweredEntryThroughItsFaults` in `journal` runs 24
seeds. Three VMs commit to one journal at once and trim what a checkpoint would
cover, until the power is lost at a point the seed draws, with the sites on and
power losses that tear what was not synced. The next holder reads the journal
back under a newer lease and commits at the next epoch: four holders, and a
fifth that only reads back. Every entry a commit was answered for, and no
checkpoint covered, must read back at its position. Every entry that reads back
must be one a commit made, no position is answered twice, and each holder
answers past everything answered before. Every site must fire and every probe
be reached: a disk formatted, a lease refused, a torn entry, a failed range
padded over, the ring's end padded, a full ring and a batch of several commits.

The other tests, by package:

- `journal`: an answered entry reads back after a power loss, a torn batch ends
  the read back, a failed batch is padded over with nothing after it lost, a
  commit is answered only after its batch syncs, a newer lease refuses the
  disk, a read fences the VM and waits for the batch in flight, trimming by
  covered positions frees the ring, and a close writes empty only when no entry
  is live. Two fuzz tests parse entries and header slots.
- `vmmemory`: a capture takes only the changed blocks, a store after it traps
  and copies nothing, a capture during a seal takes the sealed copy, a store
  under the seal moves the page off the list, a seal keeps only the digests of
  journaled pages, a failed capture gives its pages back, a page of zeros takes
  zero digests, a spilled page is read back, and RAM is refused.
- `host`: a flush is answered once its entry is on the journal, and fails with
  no journal or a failed sync; a selection names the covered position and
  trims; a VM over half the ring waits for its checkpoint; `JOURNAL_READ`
  fences and gives up the VM; a flush is journaled only once the record names
  the journal; another host's open replays what the VM flushed and cold boots
  it; a destination journals nothing until its post-copy ends; a handoff is
  refused while the record names two journals; each host serves the journal
  disk made for its machine, and a drained one passes it on.
- `volume` and `control`: an open keeps the journals and a close writes none, an
  open replays them, an open whose journal is not served takes no epoch, a
  failed replay publishes nothing, a migration's open names its journal and
  replays nothing, a discard opens a VM whose journal is lost, and a migration's
  open of a record naming two is refused.
- `membership`: `TestJournalDisksFollowTheMachines` runs eight seeds, seven
  with the sites on, over a model of hosts, machines and a cloud: two machines
  get two disks, a draining host's disk is kept while it holds entries and then
  given to the next machine, a lost host's disk is read on a survivor until it
  is empty, and a disk free for an hour is deleted.
- `cmd/sproutfs-orchestrator`: a recovery waits for the journal its record
  names to be served.

The guards are `journal-answer-before-sync` and `journal-read-without-fence` in
`journal`; `journal-trap-not-marked`, `journal-seal-drops-unjournaled`,
`journal-digests-survive-unjournaled-seal` and `journal-failed-keeps-digests`
in `vmmemory`; `journal-failed-write-answers`,
`journal-failed-write-keeps-pages`, `journal-covered-after-seal`,
`journal-ignore-half-ring`, `journal-read-keeps-vm`, `journal-answer-unnamed`,
`journal-drop-source-early`, `host-resume-over-a-replay` and
`host-open-a-journal-as-a-shard` in `host`; `volume-publish-a-partial-replay`
in `volume`; and `membership-attach-journal-late`,
`membership-create-no-journal` and `membership-delete-a-live-journal` in
`membership`.

No fingerprint arm runs with journals: two runs of one seed do different work,
because the hosts' checkpoint and trimming loops race the driver.

### Durable flush in the simulation

A world with `Config.Journals` runs [durable
flush](../plans/fsync-journal-2026-10-06.md): each host answers a flush of a
disk from its journal, on a network disk of the world's cloud that the
controller keeps for its machine. The checkpoint loop is on, because a flush is
journaled only once a checkpoint names the journal.

A recovery here is not checked against one whole checkpoint. The world keeps,
for every block of every durable disk, each value the guest stored there, and
a floor: the value the last flush answered with success, or the last
checkpoint that landed, saw. A recovery must read every block at its floor or
at a value stored later. A flush answered after a recovery took the VM is
checked against what that recovery read. `World.Flush` sends a flush,
`World.HoldFlush` holds the host's answer before the guest takes it, and
`World.FlushOn` and `World.FlushAtStart` flush from a guest the world does not
run the VM through: a destination's during its post-copy, or as it starts.

The kills, each a test in `journals_test.go`:

- the host dies at once after the guest has the answer;
- between the journal's sync and the answer;
- during a seal's upload, with a flush answered under the seal;
- the source dies during the post-copy;
- the destination dies during the post-copy of a VM back on a host it ran on;
- a flush on a destination waits for the post-copy, also one sent as its guest
  starts;
- a host isolated and given up by an operator, whose guest can flush nothing
  more;
- a host dies during a scale-down's wait for its journal to empty;
- a VM whose journal holds nothing opens after its disk was let go.

`TestJournalsSurviveTheirFaultsAndReachTheirProbes` runs sixteen seeds of three
hosts and two VMs, rings of 20 MiB, with every site on. A seeded schedule
flushes, fills a ring past half, sends three flushes at once, races a flush
with a checkpoint, migrates, takes a VM over with a flush in flight, detaches
a journal disk under a batch that never synced, kills hosts after an answer and
between the sync and the answer, and has hosts leave and join. After every step
each guest reads back what it wrote, and every flush answered with success is
still there. Across the seeds every journal site fires, every journal probe is
reached but those a single-attach cloud cannot reach, flushes fail, VMs are
recovered from their journals, and a recovery waits for a disk still moving.

| Site | What it does |
| --- | --- |
| `host/journal-capture-slow` | Holds a flush for up to 20 ms after it chose its pages and before its capture, so a checkpoint's pause may come between |

The campaign found five bugs, each fixed with a test: a commit that needed
more room than half the ring waited for trims nobody asked for; the host asked
the VMs holding the most rather than the oldest entries; a failed batch's range
was never padded when no commit fit beside it; commits placed ahead took a
commit's room after the host had asked for it; and a VM whose journal held
nothing could not open once its disk was let go. The kills found two: a
recovery gave up on a holder it could not reach rather than waiting, and a
flush sent before the host registered its VMM was answered with nothing
journaled.

### Probes

`sim.Probe(ctx, name)` marks a place execution reached, as FoundationDB's
`CODE_PROBE` does, so that a campaign can show its faults made code run.
Probes are counted on the runtime, not traced, so registering one changes no
recording. `Runtime.Probes` reports what a run reached, `Runtime.MissedProbes`
what it did not. The registered probes are:

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

Each area below adds probes and has a campaign that requires every one of its
sites to fire and every one of its probes to be reached across its seeds.

**The page cache's disk** marks ten: an item written again by a second chance,
a second chance stopped at half a region, a second chance opening the region
kept free for it, an eviction waiting for a read in flight, a read that finds
another key's item or a damaged one, and, as the disk opens, a region read back
from its table, a region read back by scanning its items, a region given back,
and a header refused. `TestDiskSurvivesItsFaultsAndReachesItsProbes` in
`checkpoint` runs eight seeds of a writer, two readers and a limiter over a
disk with read chaos, every completion released by the scheduler. It then
opens the disk again after a clean close and with a region open, and reads
every page written. A test-only file under the cache, like FoundationDB's
`AsyncFileWriteChecker`, keeps a copy of every byte written, so an item the
cache refuses must be one the disk lied about to that read. Each test envelope
ends in the SHA-256 of what precedes it, so another page's envelope passes it
and only the key check refuses it.

**A fault's prefetch** (`vmmemory/prefetch.go`) marks eight: a run left unread
because its fault followed no recent fault, its pages landed, mapped into the
memory region that asked, a fault waited for it, a run left unread at the
bound, a prefetch an allocation cancelled for its slots, a landed page another
load had made resident first, and a page a migration's source turned out still
to hold. `TestPrefetchSurvivesItsFaultsAndReachesItsProbes` in `vmmemory` runs
twelve seeds of three guests over an arena smaller than their memory with at
most two prefetches in flight: two forks of one checkpoint, whose loads race
for the same identities, and one behind a migration's source that serves two
pages the volume names as its own. Each guest reads and stores, half the time
the page after the last. Every backing read, every prefetch step, each guest
access and each allocation woken by returning slots completes when the seed's
`sim.Scheduler` chooses. Every read must return what the guest last stored or
what its volume holds. `TestPrefetchCampaignReplaysItsSeeds` runs three seeds
twice and requires the same release order and probes. Fault reads and
prefetches are tasks named by their page and window, and the woken allocation
and each guest access pass `sim.Admit`; before 2026-10-04 tasks were numbered
across the host and one seed in ten missed the duplicate probe on some runs.
`TestAPrefetchedPageAnotherLoadMadeResidentFirstIsDropped` reaches that probe
on its own.

**The pager's lock gaps.** The campaign lets a task go on only where it passes
`sim.Admit`, so a race between two holds of a lock is found only if an
admission point sits between them. TASK-105, a fault's READ landing between a
prefetch's check of the reads under way and its send, was missed for want of
one (`vmmemory/prefetch-send`). Since TASK-108 every place the pager lets a
lock go and takes it again either says in a comment why nothing read under the
first hold is relied on after it, or has a seam test that forces the bad order
and a guard (`vmmemory/lockgap_*_test.go`). The gaps on fault, store, prefetch,
eviction and give-back paths admit there: `fault-lookup`, `reserve-runs`,
`run-read`, `supplied`, `populate-take`, `allocate-evict`, `reclaim-step`,
`evict-remove`, `prefetch-land`, `prefetch-landing`, `dirty-wait`,
`give-back-spilled`, `give-back-volume`, `give-back-victim`, `give-fork`,
`fork-file`, `fork-copy`, `move`, `reclaim-private`, `store-read-in`,
`rule-copy` and `settle-reshare`, all under `vmmemory/`. A path that must
finish once begun admits without its cancellation (`admitGoingOn`:
`give-back-share`, `unindex`, `rebind`). The prefetch campaign neither
checkpoints nor forks and never fills the dirty budget, so it does not reach
the fork, move, unindex and dirty-wait gaps; their seam tests do. Each disk
guest has a flusher on a task of its own, as a guest's flush arrives on
another vCPU than its stores, so the campaign reaches the gap between a
refault and a capture and finds `pager-refault-maps-by-a-stale-protection`.

**The mapping rules.** `TestTheMappingRulesSurviveTheirCampaign` runs 64 seeds
of two forks of one checkpoint and a disk at 4 KiB pages, each region pressed
for mappings, so the gap rule makes private the pages between a store and a
private page near it, across read-ahead windows. Each guest runs two vCPUs,
each reading and storing only its own pages, every other one, so a rule meets
the other vCPU's fault or store in the window it takes a page from. Every read
must return what the guest stored or what its volume holds, and the rules must
copy pages. Seeds 7, 11 and 40 find `pager-rule-takes-a-page-of-another-window`.
Until this campaign, every campaign guest ran one vCPU, and no two faults of
one region ever met.

**The soak.** A seeded campaign lets one task go on at a time, at its
admission points, so a race between two of them is found only where an
admission point sits in it. `TestThePagersCampaignsSoakWithoutAScheduler` runs
the prefetch, fork and rules worlds with no scheduler: eight forks of one
checkpoint, as an embedder's boot starts them, six children of one fork point,
two vCPUs a guest, on real goroutines and timers, half as many worlds at once
as the machine has cores. A seed still fixes each world's operations and the
fault sites it activates, but not the order its guests run in, so a failure
names its seed without replaying. It runs only when asked:

```
SPROUTFS_PAGER_SOAK=30m go test -race ./vmmemory -run '^TestThePagersCampaignsSoakWithoutAScheduler$' -timeout 40m
```

`SPROUTFS_PAGER_SOAK_SEED` sets the first seed; it is the clock's otherwise.
Run it on a machine with many cores, not on a laptop that runs anything else.
`scripts/soak-pager-gce.sh all` runs it on a disposable 22-core GCE VM, 15
minutes per page size in each arena, and deletes the VM. On d09c46e3
(2026-10-08) it soaked 24,109 worlds at 4 KiB and 367 at 2 MiB in the isolated
arena and 24,499 and 368 in the shared one, every world clean, under -race and
the mapping audit. The runs before it found the lost store the mapping audit
describes, and a deadlock between a journal capture, a prefetch and a fault
(`TestACaptureNeverWaitsForTheLockOfThePageItsCopyWasMadeFrom`).
A world that runs for five minutes is taken for hung: the soak ends the process
with every goroutine's stack, which names the seed and the locks each waits
for. That deadlock held a GCE soak silent for an hour, until it was sent
SIGQUIT by hand.

**The mapping audit.** A failure that needs two rare events to meet is rarer
still than either. Every `vmmemory` test keeps what each mapping command
installed for each page (`vmmemory/mappingaudit.go`; nil in production) and
checks the pager against it:

- a resolve is valid for what the page is mapped as: writable for a page
  mapped writable, read-only for one mapped read-only or write-protected;
- when the region is let go, at the end of a seal, retire, unseal, handoff or
  detach for every page and at the end of a fault for its window, no page its
  binding says is writable is mapped read-only, because the next fault on it
  resolves it writable. A page whose lock something holds is passed over: a
  store's rule makes a page of another window private before the store's
  command maps it, and a fault on that page waits for its lock;
- a page that is the region's own dirty state is never bound to a root's page.

A finding names the page's latest commands with their callers, refused ones
and the transitions that made the page the region's own among them, and the
binding's state. The audit turned a lost store that the soak found about once
in seventy runs, as an invalid resolution with no cause, into a finding in
five, then into its cause: a fault that let its own unmapped page's lock go
before its lookup, where an eviction spilled the page and the lookup read the
volume (`TestAFaultRefaultsItsOwnPageAnEvictionSpilledBeforeItsLookup`). Tests
that measure the pager's own heap attach without it (`WithoutMappingAudit`).

**A fork point's children while its seal ends.**
`TestAForkPointsChildrenReadWhatItLentWhileItsSealEnds` runs twelve seeds of a
parent that seals a fork point, stores and ends the seal by retire, unseal or
detach, while children of the point read and store its pages. Every read must
return what the child stored or what the point held. A seal's take, a lend, each
retired batch and page, an abandon, a hand-back, an adoption, a sharer's drop, a
lent root's drop and a fork file's end pass `sim.Admit`.
`TestForkCampaignReplaysItsSeeds` runs seeds twice and requires the same order.
The campaign finds `pager-lend-a-page-past-its-checkpoint` in both arenas.
`SPROUTFS_FORK_CAMPAIGN_SEEDS=n` runs seeds 1 to n instead, to sweep the
scheduler's interleavings; a failing seed replays alone by its subtest's name.

**The disk's stripes** mark five: a write that kept several indices of one
envelope, a read that rebuilt an envelope from a parity stripe, a read that
found fewer than k stripes, a read that found the page only under another code,
and a wrong stripe found and forgotten.
`TestDiskStripesSurviveTheirFaultsAndReachTheirProbes` runs the disk workload
over a disk that follows a list of its own cache and one other, with the
cluster cache on for every window, under a code of the table that changes with
the seed (under 4+2 its cache is alone in the list and holds all six indices).
Every hit must be what was written, and the reads that rebuilt an envelope that
failed its check must number no more than the stripes handed over wrong and the
disk's lies. It runs eight seeds. `stripe`'s property tests state splitting and
joining: every envelope of 0, 1 and up to 4,097 bytes rebuilds from every set
of k of its k+m stripes, in any order, under 1+0, 1+1, 2+1, 2+2 and 4+2; one
wrong stripe among k+1 is found wherever it falls among the first k; a stripe of
another code is never used, though a stripe of 2+1 and one of 2+2 of one
envelope have the same length; and the search for k that pass is bounded at 64
sets.

**[Fills](hosting.md#filling-the-cluster)** mark thirteen: a fill right given,
a read that sent nothing for want of one, a right whose answer was lost, a fill
dropped for a full queue, for a spent rate, for want of room in the background
budget, and by its holder, a fill's write the disk refused, a fill placed by a
changed list, and, at the cache a keep reaches, a keep written, a stripe it held
or was writing, a keep refused for a window its list does not rank it for, and
a keep dropped. `TestFillsSurviveTheirFaultsAndReachTheirProbes` in
`checkpoint` runs sixteen seeds. Each draws a cluster of two to seven hosts and
a code of the table, sometimes narrower than the hosts, and runs six rounds
under the scheduler. A round publishes from any host, has many hosts read the
same pages at once through their own caches, and may take a cache off the
list. Some hosts read the new list a round late, so two hosts may rank a window
differently. The cluster runs over real peer servers on the simulated network
with a small queue and rate. Its background budget of 1.5 MiB has no room for a
keep of a whole 2 MiB window, which a host sends its peer under 1+1. A
publication's keep that finds the budget held by the publications' own keeps
waits for one of them to be answered. Every read must be what was published.
At rest every stripe a host holds must be of a window some list ranked it for,
and no window may be filled more than once an interval. The campaign found a
fill placed by a changed list writing a stripe its host was not ranked for; a
cache's own fills are now held to its list as a keep is.

A seed of the fill campaign does the same work on every run: cache identities
are drawn from the seed, and the reads that one store load releases at once
each pass `sim.Admit` before deciding what to read next. Seeds 6, 10 and 12
still did other work in about one run in six to fifteen under a loaded
machine, so another completion still releases several goroutines at once
somewhere.

The fills' properties, on a cluster of real peer servers:

- `TestAStoreReadFillsExactlyTheRankedCaches`: after one host reads a page,
  the stripes of its window and its segment's are on exactly the hosts the
  list ranks, each index on its host, for every reader under 1+1, 2+2 round
  three hosts, and 4+2.
- `TestAColdBurstFillsAWindowOnce`: every host reads one page at once, and the
  page's window and its segment's are each filled once.
- `TestAFaultIsNotSlowedByItsFill`: a store read takes exactly as long with the
  cluster cache off, behind fills whose links are held for a second, and behind
  fills dropped for a spent rate.
- `TestAPublicationFillsNothingBeforeItsPartIsDurable`: while a part's PUT is
  in flight no host holds any of it, and once it lands every window is on its
  ranks. `TestAPartTheStoreRefusedReachesNoCache` fails the PUT, and nothing is
  filled until the publication is tried again.
- `TestAPublicationNeverWaitsForItsFill`: a publication behind a queue of one
  window and a disk that takes a second a write takes exactly as long as with
  the cluster cache off.
- In `internal/simtest`, half the seeds of the topology campaigns give every
  host a cache disk with the cluster cache on for every window
  (`simtest.Config.ClusterCache`), and such a run must have had a host keep a
  stripe a peer sent it.
- `TestOnTwoHostsAVMOpenedOnTheOtherHostReadsItsPagesFromThatHostsDisk`
  suspends a VM on one of two hosts under 1+1 and opens it on the other, which
  reads no part of the store but the VMM state.

A publication's fills wait for room rather than drop
([filling the cluster](hosting.md#filling-the-cluster)). The tests of that pace
run two hosts under 1+1, except the read's test (three under 1+2) and those of
keeps side by side (two or four under 1+1 or 1+3). The other hosts are holders
whose disk takes a millisecond over each write, or a second and a millisecond
for a slow one. The first host publishes pages of noise, a part each:

- `TestAPublicationGoesAtThePaceOfItsSlowestHolder` publishes eight parts
  through a queue with room for two windows. Behind the slow holder the commit
  takes exactly six seconds longer than behind a quick one, and every stripe
  lands. Under `fill-publication-dropped-when-full`, six of the nine windows
  reach no host.
- `TestAPublicationHoldsNoMorePartsThanItsSlotsAndItsQueue` publishes twelve
  parts through two upload slots. When each part's PUT begins, the publication
  holds at most four parts its fills have not finished with. Under the same
  guard it holds all twelve.
- `TestAReadsFillGoesAheadOfAPublications` reads a page from the store two and
  a half seconds into a publication of twelve parts, each window two keeps to
  slow holders. The page's window is on its ranks half a second later than
  when nothing else is filled. Under `fill-reads-behind-publications` it lands
  more than ten seconds later.
- `TestADeadHolderCostsAPublicationTheBoundAtMost` cuts the link to the holder
  under a bound of a second. The first keep waits out the dial's three seconds,
  and the commit takes exactly the bound longer than with the cluster cache
  off. Every window after the two queued is dropped. Under
  `fill-publication-waits-forever` the commit waits for the dial too.
- `TestAPublicationsKeepWaitsForTheBackgroundBudget` holds the publisher's
  background budget full for three seconds. The first keep is tried again at
  growing intervals and goes at 3.13 s, and every window lands. Under a bound
  of a second the keeps are dropped instead.
- `TestAPacedPublicationDoesTheSameWorkUnderAShake` requires the same
  fingerprint and the same moments under three shakes.
- `TestAPublicationsKeepsGoToItsHoldersSideBySide` publishes eight parts
  through a queue with room for two windows, to one slow holder and to three.
  The commit takes exactly six seconds longer than behind quick holders in
  both. Under `fill-keeps-one-at-a-time`, behind three holders it takes
  eighteen seconds longer.
- `TestAPublicationsKeepsInFlightAreBounded` sets the host's keeps in flight
  to one, and behind three slow holders the commit takes eighteen seconds
  longer.
- `TestAHoldersLaneHoldsTwoOfAPublicationsKeeps` publishes to one slow holder
  through a queue with room for every window. Half a second in, the
  publication has begun three windows: a keep on the wire, one behind it, and
  one waiting for room on the lane. A second later it has begun a fourth.
- `TestEachHolderKeepsAPublicationsWindowsInTheirOrder` publishes twelve parts
  to three slow holders through a queue with room for five windows. Each holder
  is asked for the windows page by page, the segment last, with the same work
  at the same moments under three shakes. Under
  `fill-keeps-past-a-blocked-fill` a holder is asked for page 11 before page
  10.
- `TestAPublicationsQueuedWindowsHoldWhatTheQueueCounts` samples the queued
  windows every tenth of a second: they hold exactly the bytes the queue
  counts. Under `fill-windows-hold-their-parts` each window is a view of its
  whole part.

The fixture pings no connection while a keep is answered, because a ping takes
the sequence number, and so the drawn latency, a later frame on its link would
have taken.

**[Reads of the cluster](hosting.md#reading-from-the-cluster)** mark nineteen:
an envelope rebuilt from a peer's stripes and one from this host's own alone, a
miss, a rebuild from a parity stripe, a holder replaced at once, a second
request and one the budget refused, a read of the store past the bound, one the
store won and one the bucket refused, a wrong stripe found and a drop sent for
it, a repair, a timeout, a host marked down, a mark refused for the fifth and
one a probe cleared, and a sampled HEAD check and one that found the part gone.
`TestClusterReadsSurviveTheirFaultsAndReachTheirProbes` in `checkpoint` runs
twelve seeds (about 4 s), each a cluster of two to seven hosts and a code of the
table, on a network with a heavy tail and slow pairs. Each of six rounds
publishes from any host and has many hosts read the same pages at once, while
the seed stalls, refuses or slows one host's links to a reader, loses a host,
restarts one over its file, serves a list that lacks a cache, or deletes a
checkpoint's parts behind the caches. Every read must be what was published, or
fail only for a part that is gone, and at rest every stripe a host holds,
repairs included, must be of a window some list ranked it for.

**[Pulls](hosting.md#pulling-a-vms-memory)** mark seven: a page a presence
check found held and one it found lacking, a presence answer lost, a presence
check asked again under a newer generation, a page or segment a pull found
held, a range or segment it read from the store, and a pull stopped under
pressure. Their sites lose a presence answer and press the host's pulls before
a fetch. `TestPullsSurviveTheirFaultsAndReachTheirProbes` in `checkpoint` runs
eight seeds (about 3 s) on the read campaign's network. Each of five rounds
publishes a checkpoint the cluster holds, or one it lacks, drops some of its
stripes, moves the membership with only some hosts told, and has hosts pull
while others fault and some disks shrink under their pulls. Every pull must end
complete or stopped under pressure, every page must read as published on every
host, and no host may hold a window whole or a stripe no list ranked it for.

**A [hot tier](hosting.md#reading-through-a-hot-tier)** marks sixteen: a hit,
a miss, a read failed by error, past the bound and by corrupt bytes, the hot
tier marked down and a read that skipped it, a fill sent, one that found the
object there, a miss of an object already held for a fill, a fill dropped for
the queue, for the rate, for a regional GET that failed and for a PUT the hot
tier failed, and a sampled HEAD check and one that found the regional object
gone. `TestHotTierSurvivesItsFaultsAndReachesItsProbes` in `checkpoint` runs
twelve seeds (about 2 s) of three to five hosts over one regional bucket and one
hot bucket. The first host's queue holds an index object and no part, and the
second's rate has no room for a part, so both drops happen; half the hosts write
their publications to the hot tier. Each of six rounds publishes and reads
while the seed takes the hot bucket down, fails a fill's regional GET, or
deletes a checkpoint's parts from the regional bucket. Every read must be what
was published, or fail only for a part that is gone, and at rest every object in
the hot bucket must be what a publication wrote under its name, or its first
half.

The hot tier's properties, over a simulated regional bucket and hot bucket:

- `TestAMissIsFilledBehindTheReadAndTheNextReadHits`: a cold read misses the
  index object and the part, sends the regional bucket three GETs, the part
  once more by its fill, and the next read sends it none.
- `TestAPublicationWritesTheHotTierOnlyOnceItsRegionalPutSucceeded`: while a
  part's PUT is held, the hot tier holds nothing of it, and a part the regional
  bucket refused never reaches it.
- `TestAHotTierThatFailsNeverFailsARead`: a hot bucket that is down, slower
  than the bound, holding other bytes, or holding half a part costs only reads
  of the regional bucket, each failure counted by why.
- `TestAReadIsNotSlowedByItsHotTierFill`: a read behind a PUT of ten seconds
  takes exactly as long as behind one of a millisecond.
  `TestAPublicationIsNotSlowedByItsHotTierFill`: so does a publication, and as
  long as one that writes no hot tier.
- `TestAHotTierMarkedDownIsSkippedAndTriedAgain`,
  `TestTheHotTierDropsFillsPastItsQueueOrItsRate`,
  `TestAFillThatCannotReadOrWriteIsDroppedByWhy`,
  `TestTwoHostsFillingOneObjectWriteItOnce`,
  `TestASampledHotHitChecksItsRegionalObject` and
  `TestAStoreRefusesAHotTierBesideTheClusterCache` hold the rest.
- `TestAHotTierReadsAndFillsThroughEveryProvidersAdapter` in
  `platform/internal/real` misses, fills and hits a hot tier through the Cloud
  Storage and S3 adapters over their emulators.
- In `host`, `TestAVMOpenedOnAnotherHostReadsItsCheckpointFromTheHotTier`
  writes a VM on one host and reads it on another with three hits and no miss,
  and `TestAHostRefusesAHotTierBesideTheClusterCache` refuses both.
- `TestSeededTopologyFingerprintIsStable` has an arm with
  every host reading through a hot tier, and requires the hosts to have filled
  it and read from it. The world sets the hot tier's bound, rate, queue and
  sampled checks out of reach. Seeds 1 to 25 of that arm did the same work
  under the shake (2026-10-03).

The reads' properties:

- `TestAPageInTheClusterIsReadWithNoStoreRead`: under 1+1, 2+2 round three and
  4+2, every host reads a published page and its segment from the cluster, with
  no store request but the open of the index object.
- `TestAPageSurvivesLosingDrainingOrRestartingAnyOneHost`: the same after any
  one host of six under 4+2, or of two under 1+1, is lost, drained from the
  list or restarted over its file.
- `TestAHotPageSpreadsItsLoadOverEveryHolder`: six readers of one page ask four
  holders each besides themselves, every holder is asked, and each sends one
  stripe, a quarter of the envelope, to each reader.
- `TestAStalledOrSlowHolderSlowsAReadByTheHedgeDelayAtMost`: on a network of
  fixed latency, a read with one of its first picks stalled takes exactly as
  long as a healthy one, and with one stalled and one slow it takes exactly the
  delay longer for each window that picked both.
- `TestAWrongStripeIsNeverReturnedAndItsHolderIsTold`: a stripe whose checksum
  holds and whose bytes are wrong is found among k+1, or among k and one more
  asked at once, the page reads right, and its holder forgets it.
- `TestTheStoreIsReadOnlyWhenFewerThanKStripesExist`: with six to zero stripes
  left, the store is read exactly when fewer than four remain.
- `TestSecondRequestsStayWithinTheirBudget` and
  `TestStoreReadsPastTheBoundStayWithinTheirBucket` hold the two budgets.
- `TestThreeTimeoutsMarkAHostDownAndOnlyAProbeClearsIt` (which also requires a
  marked host to be sent no fills), `TestAReaderMarksDownAtMostAFifthOfItsList`,
  `TestAMissIsNotAFailureOfTheHost` and `TestARefusedConnectionMarksAHostDown`
  hold the marks.
- `TestRepairSendsOnlyAnIndexNoRankHolds`,
  `TestRepairAfterAJoinSendsTheIndexNoRankHolds`,
  `TestAReaderRebuildsFromAnyIndicesAfterTheRanksShift` (B5) and
  `TestASampledHitChecksItsPartStillExists` hold the rest.
- In `internal/simtest`,
  `TestAVMOpensFromTheClusterAfterAnyOneHostIsLostDrainedOrRestarted` suspends
  a VM on one host of six under 4+2, or of two under 1+1, loses, drains or
  restarts any other host, the suspending host among them, and opens the VM on
  another: it reads no part of the store but the VMM state.

A simulated world sets a read's delay, its bound and its stripe timeout out of
reach (`simtest.Config.ClusterCache`), because whether a read crossed one turns
on hop jitter that concurrent reads draw in the Go scheduler's order. The
world's reads still ask k+1 ranks, replace misses, rebuild from any k and
repair. The read campaign drives the timed paths under a scheduler.

A read that the store answered first, or whose caller gave up, leaves its
requests running on the holders' disks. `Cache.SettleReads` waits for them, for
every read of the store past a bound, and for every read whose caller went on
without it. The restore bench settles the reader after each case. Before it
did, `TestEveryCaseReadsTheGuestBack` failed in about one run in twenty: the
4 KiB run cases left requests for 2,048 pages each on the disks, and how far
they had got decided whether the next case's 2 MiB read passed its bound.
`TestSettlingReadsWaitsOutTheRequestsOfAReadTheStoreAnswered` holds the
property.

### The object store's bounds

[The bounds on the object store](hosting.md#bounds-on-the-object-store) are
tested over a simulated store that holds requests. `sim.ObjectStore` can hold
the next requests of an operation before their reply (`HangNext`), apply the
next writes and then hold their replies (`HangNextAfterApply`), and hold a
GET's or a PUT's body halfway (`StallNextBody`). A hold lasts the store's
`Hold`, an hour by default, unless its caller gives up first. A store with
`ObjectStoreConfig.RequestChaos` adds three sites, off on every other store
because a caller with no bounds waits out every hold:

| Site | What it does |
| --- | --- |
| `sim/object-store/hang` | Holds a request before its reply, applying nothing |
| `sim/object-store/hang-after-apply` | Applies a PUT or a DELETE, then holds its reply |
| `sim/object-store/stall-body` | Holds a GET's or a PUT's body halfway |

`platform/bounded` marks seven probes: a first-byte timeout, a stall timeout, a
request made again, a conditional PUT made again, a body resumed, a body whose
object had changed, and a write that timed out and was not made again.
`TestTheBoundsSurviveTheirFaultsAndReachTheirProbes` runs twelve seeds (about
0.2 s) of four workers over a store with request chaos. The workers create
immutable objects, read them whole, by range and by suffix, head and list them,
add to counters by compare-and-set, and write and delete objects of their own
without a condition. Every read must be what was written. A create refused by
its own landed write must find its own bytes, and an addition refused the same
way must find its own name in the counter. Each counter holds every addition
exactly once. An unconditional write that timed out may or may not have landed,
and the next read must find one of the two. The store's counts must equal the
probes.

The bounds' properties, in virtual time:

- `TestAHungGetIsAbandonedAtItsFirstByteBoundAndTheRetrySucceeds`: a GET held
  before its headers answers at exactly its bound plus one round trip.
- `TestAStalledBodyIsAbandonedAtItsStallBoundAndReadOnFromWhereItStopped`: a
  body held halfway is read on at exactly the stall bound by a ranged GET of the
  rest, for a whole object, a range and a suffix.
- `TestAStalledBodyWhoseObjectChangedIsRefused`,
  `TestAHungCreateWhoseWriteLandedIsMadeAgainAndRefusedByItsOwnObject`,
  `TestAControlRecordWrittenAcrossHungRepliesIsReconciled` (by the writer's
  nonce), `TestAPublicationWhosePartsReplyHungIsSettledByItsDigest`,
  `TestAHungUnconditionalWriteIsNotMadeAgain`,
  `TestAStalledUploadIsAbandonedAtItsStallBound`,
  `TestAHungHeadOrListIsMadeAgain`, `TestACallerThatGivesUpIsNotRetried` and
  `TestABodyHeldUnreadIsNotAStall` hold the rest.

The world bounds every host's view of the store and the orchestrator's on the
bubble's clock, which the store's latency passes on; a host's own clock passes
only when the world advances it. With buggify on, the topology campaigns turn
request chaos on: `TestSeededTopologyUnderBuggify` must reach all three sites,
and it has seen a control record's PUT, a part's, an index object's and a
reclamation's DELETE time out with the guests' bytes right. The fingerprint
test runs without buggify; its seeds did the same work under the shake with the
bounds in place (2026-10-04).

`TestEveryProvidersAdapterGivesUpAHungRequestAndResumesAStalledBody` in
`platform/internal/real` runs the bounds over the Cloud Storage and S3
emulators, through each provider's client, behind a handler that holds the
next request. A create whose write landed and whose reply never came is made
again and refused by its own object; a GET whose headers never come is made
again; a body cut off halfway is read on from where it stopped; and an
unconditional PUT that hangs returns `bounded.ErrTimedOut`. So each client
gives a request up when its context is cancelled, mid-body included, and the
next request on the same client succeeds. Only GCE shows whether a stuck
request there is a stream on a live HTTP/2 connection or a dead connection, and
the tail of times to first byte the defaults should be checked against.

### The peer server's network

Each simulated network fault is off at its zero value:

- a heavy latency tail, in which about one hop in `TailEvery` takes up to
  `TailLatency` longer, until `Network.EndTail` ends it;
- pairs of hosts that stay slow for the whole run once they first connect;
- one link's bandwidth, `LinkBytesPerSecond`, shared by every connection
  between two hosts, so a frame is sent behind the bytes sent before it;
- bounded send buffers, so a sender whose reader stops reading stalls;
- holds: `Network.Hold` and `HoldBoth` keep every byte a link carries until a
  moment and refuse nothing, as a partition does to TCP, and `SwizzleHolding`
  swizzles with holds instead of refusals;
- dials to an address nobody listens at that hang until their caller gives up;
- connections closed at random under a frame (`sim/network/random-close`);
- a flipped bit in a frame's header (`sim/network/header-bit-flip`).

`Network.Framed` gives byte streams over the simulated links, framed by the
same framer the TCP adapter runs. Each write is split into pieces of seeded
lengths, down to a byte, and each read returns a seeded part of what has
arrived. Its site `sim/network/stream-bit-flip` flips a bit in the first bytes
of a write, where a frame's prefix and header are.

`TestThePeerServerCampaignNeverAnswersWrong` in `peer` has one destination ask
a source of this release, a source of the release before, a release two ahead
and a machine that is gone for pages and stripes for thirty simulated seconds,
while the link to the source is held for up to six seconds at a time. Half the
seeds run over framed links and half over byte streams. No answer may be wrong,
and no damage on the way may make a sound peer look broken. The release before
checks no header, so its answers are checked only once the faults stop; the
heavy tail stops with them. After a minute, every peer must answer at once and
right, none may still be marked down but the one that is gone, and the release
two ahead must still be incompatible. Three seeds run normally, the cheapest set
that activates every site of the peer server and the network and reaches every
probe of `peer.Probes`. The soak runs sixty-four.

The peer server's probes are a request answered BUSY, a hello answered
INCOMPATIBLE, a request that waited for its class's budget, a reply to a
request its caller gave up on, a dialer that fell back to version 1, a
connection found dead, a peer marked down, a down peer probed, and a request
that skipped a down peer.

The campaign found three bugs: a write on a stream whose connection closed spun
until its pieces would have arrived; a version-1 connection that owed a reply
was never found dead, because the release before answers no ping; and a down
peer whose probe was answered INCOMPATIBLE stayed down and was probed forever.

`TestAGuestFaultIsAnsweredWhileTheStreamSaturatesTheLink`: sixteen streams fill
a link of 256 MiB/s, and a guest fault asks for one page every 50 ms. The fault
waits behind what the stream has on the link, which the background budget of
4 MiB bounds: about 5 ms. With `peer-unbounded-background` it waits behind
64 MiB, about 100 ms, and the test fails. The stream still runs at nine tenths
of the link or more.

```sh
go test ./peer -run '^TestThePeerServerCampaignNeverAnswersWrong$' -count=1
SPROUTFS_TEST_SOAK=1 go test ./peer -run '^TestThePeerServerCampaignNeverAnswersWrong$' -count=1
```

### Fingerprints

`Runtime.Fingerprint` digests everything the simulated dependencies did: the
resource, the operation, the outcome, the number of bytes, the order on each
resource, and the simulated moment. It is FoundationDB's unseed. It sees only
what a trace event carries, so two writes of one size to different offsets of
one file digest the same; bytes are compared against the models.
`Runtime.WorkFingerprint` drops the order, the moment and the adapter's
operation numbering; a campaign that does not control completion order can
guarantee only this one.

`TestScheduledWorldFingerprintIsStable` asserts the strict fingerprint across
two runs of a seed. `TestSeededTopologyFingerprintIsStable` asserts the work
fingerprint. It excludes connection attempts, bounded by the number of memory
regions in the topology, because how many of a destination's memory regions
dial a source being removed before the first failure marks it fallen is a race
between goroutines. Every other event is identical between two runs of a seed.
Each seed runs with no cache disk, with the cluster cache on, through a hot
tier, and with the cache on shards, which the controller moves as the campaign
kills and restarts hosts. A failure prints the work that differs.

The second run of each seed is shaken (`sim.Config.Shake`). Before and after
every wait in a simulated dependency, and before every send, a goroutine yields
the processor a drawn number of times, so goroutines ready at one simulated
instant reach the dependencies in another order. A race the seed does not
decide then shows up on an idle machine. Without the shake, one such race
failed this test in about two runs of three on a loaded machine and none of
sixty on an idle one.

Shaking with the fault-injection sites on found four more races, each fixed:

- A destination's memory regions stream their pages on several goroutines, and
  each fault took its arena slot and sent its request whenever it ran. The
  faults now take turns in page order until each one's request is on the wire
  ([the post-copy stream](migration.md#phases)). Under the scheduler each
  page's fault is a task and passes `sim.Admit` as it takes its turn.
- The driver began and ended faults while the world still had work due at that
  instant. It now calls `synctest.Wait` before each fault it begins or ends and
  before each step.
- The simulated store refused an operation, and a cut link a frame, at the
  instant it was asked. A refused operation now takes its latency, as a frame
  does.
- A publication handed its parts to the fills as their PUTs ended. It now hands
  them over in their own order.

Over seeds 1 to 50, with no cache disk and with the cluster cache on, the sites
on and four shakes each, 97 of the 100 pairs did the same work in all four runs
(2026-10-03). Seeds 35 without a cache, and 43 and 48 with one, still vary now
and then. In seed 35 a reply whose header a fault flipped makes the destination
close the connection at the instant the source starts its next reply, and
whether that reply is sent is the scheduler's choice. Two requests one link
carries at one instant take its sequence numbers, and so its drawn latencies,
in the order they arrive. The probe campaign and the fingerprint test run none
of these seeds.

`TestSeededTopologyUnderBuggify` runs the same deployment through the same
schedule with the sites on, checks the same bytes, and requires the campaign to
reach every site it is supposed to reach.

```sh
go test ./internal/simtest -run '^TestSeededTopologyUnderBuggify$' -count=1
go test ./internal/simtest -run '^TestSeededTopologyFingerprintIsStable$' -count=1
go test ./internal/simtest -run '^TestScheduledWorldFingerprintIsStable$' -count=1
SPROUTFS_TEST_SOAK=1 go test ./internal/simtest \
  -run '^TestTheCampaignsReachTheirProbes$' -count=1 -timeout=30m
```

The probe campaign runs seeds 1 to 25 and seed 46 of the generated schedule
with the sites on, each with no cache disk and with the cluster cache on, plus
four seeds of the two-writer campaign. It requires every probe the campaigns
are registered to cover to have fired, a fill right given, a keep kept and a
page rebuilt from a peer's stripes among them. `unreachedProbes` in
`internal/simtest/probe_test.go` names the one registered probe no campaign
covers: here the store either answers or fails outright, so no conditional
write loses its reply and is reconciled by its writer's nonce. The list is
asserted in both directions, so a probe that starts firing must be removed from
it.

Seed 46 is there for an eviction during a publication, which none of seeds 1 to
25 reaches; it does on every run, with the cache and without. A fenced
publication is reached by the two-writer campaign, whose takeover happens while
the superseded host is still running.

## Every boundary error is simulated

A campaign finds only the faults it injects. On 2026-10-08 an embedder's VM
died of a mapping refusal the pager mishandled, after weeks of campaigns: the
test client refused only where one test switched refusal on, so no campaign
took the paths a refusal leads down. The first campaign run with a random
refusal found the bug, and the runs after it found two more.

So every interface the system calls across a process or I/O boundary is listed
in a manifest under `scripts/faults/`, one file per area. Each method has an
entry for each error it can return: the `sim.Buggify` site that returns it at
random in the simulated implementation, and the campaign that must fire it. A
method that cannot fail has an entry saying `none` and why. `just check-faults`
(`scripts/check-faults.py`) fails where an interface method has no entry, where
a site is in no `Buggify` call, or where a campaign, run with
`SPROUTFS_FIRED_SITES` naming a file, never fires a site its entries name:
`platform/sim` appends every site a run fires to that file. An entry can be
`deferred` to a backlog task that says why its site is not fired yet; the
checker lists those, and they are debt.

The rule is about the simulated implementation being as wide as the real one.
A switch a test turns on is not enough: the paths behind a fault are taken
only when a campaign, with every other fault going on, meets it at random.
Writing the manifest is also where a missing path shows: the first run of the
pager's found that no campaign mapped a zero, and that no campaign took the
batch path production takes for every fault.

The pager's client (`scripts/faults/vmmemory.json`) refuses any mapping
command at random, as a client out of VMAs does, and the campaigns attach half
their seeds with `batchedMapping`, which takes a fault's runs and an
eviction's revocations in batches as production's client does. It also loses
a command's answer at random, and refuses a revocation, which a region cannot
survive. A campaign takes the machine whose client ended as gone: its host
learns of the end from the region, ends the VMM, which ends every fault the
machine had in flight, and detaches the region once nothing of it runs, as a
host closes a dead VM. Every other guest, a fork point's children above all,
must read exactly what it holds. Each campaign's replay test runs seeds that
inject such a fault. The first runs found that a fork point's child whose
client refused a revocation failed its parent's unseal
(`pager-end-a-step-with-its-sharer`).

The pager's backing, arena and spill are in the same manifest. The campaigns'
backings fail a read, a lookup of identities and a verification at random, as
a volume does where its store, its cache or a peer cannot answer or its VM is
no longer this host's; the arena fails to make a file or allocate a page, as a
memfd does for want of memory; and the spill's simulated disk fails as a
device does. A guest whose request meets one of them has its session ended, as
production ends it on any error a fault is answered with (TASK-112 would serve
a store's refusal once it answers), and its host closes it. The host also
verifies each region on a timer, as a session does.

A seed replays only where every step that goes on beside another is one the
run admits. Turning lost commands on found where that did not hold, each the
Go runtime's choice: an eviction revoked a page from each region that maps it
in a map's order, so which region met the refusal varied; the waiters an
unlock woke raced to take the lock, and a woken writer raced the reader that
woke it; and a detach went on beside the faults it waited for. Regions are now
commanded in the order they attached (`MemoryRegion.serial`), and every pager
lock is waited for without taking it and taken when the run admits the waiter
(`lockAdmitted`, `wlockAdmitted`). `requireReplay` prints the first operation
two runs of a seed part at, which is where such a search begins.

The platform's own ports (`scripts/faults/platform.json`) are the disk, the
object store, the network and the cloud's network disks. Under Buggify every
simulated disk fails each operation at random, as EIO and ENOSPC do, and leaves
what a real failure leaves; the network refuses listens, dials and accepts and
times connections out; the cloud loses the replies to what it did. The store
refuses requests, loses the replies to writes it applied and resets bodies only
where `RequestChaos` is on, until every campaign's callers handle that
(TASK-113), and a network disk's device fails no reads until a host keeps its
journal through one (TASK-114). An answer the simulation gives from the state
it models, such as `ErrNotFound` for a file that is not there, needs no site.
The clock and the entropy cannot fail.

Above the platform and the pager, the manifests are these. `host.json` and
`vmmigrate.json` list the VMM process: the simulated one in `internal/simtest`
refuses each command at random, never answers one and is killed, crashes,
fails to start and fails to close, and a VM whose process ended comes back
elsewhere at a checkpoint it published. `volume.json` lists a memory region's
sealed checkpoint, which the simulated VMM hands each capture with its reads,
settles, shares and retires failing at random, and durable flush's journal
cover and replayer. `peer.json` lists the requests one host makes of another,
each of which may lose its reply after the peer carried it out
(`peer/reply-lost/<request>`). `membership.json` lists the membership's store
and a view of it. `TestSeededTopologyUnderBoundaryFaults` runs the seeds that
between them fire the simulated VMM's and the peers' sites. The control store
calls nothing but `platform.ObjectStore`, which is the platform's manifest.
The guest agent's exec (TASK-116) has no manifest yet.

`orchestrator.json` lists the orchestrator's boundaries: each host's API
(`hostClient`), the Kubernetes API (`pods`) and the bucket's control records
(`records`). The fakes in `cmd/sproutfs-orchestrator` fail every request at
random: one that never reached the far side, one it refused, and one it carried
out whose answer was lost (`orchestrator/host-reply-lost/<method>`). Half the
times a host loses an answer, it is also cut off the pod network for up to two
holds. `TestTheOrchestratorUnderBoundaryFaults` drives creates, forks,
migrations, drains, stops, starts, recoveries, kills, deletes and orchestrator
restarts through them on 128 seeds, with the reconcile timer running. Once the
faults stop and the deployment settles, every VM with a record runs on exactly
one host or starts, every VM the orchestrator said it made still has its
record, and no host runs a VM without one or still holds a handover.
`SPROUTFS_ORCHESTRATOR_SEEDS` selects another seed count; 500 pass.

Its first runs found that a start or a delete went past a quiet host: no
answering host ran the VM, so a start opened it on a second host and a delete
removed its record under a running guest. Each now needs every host to answer,
or a table row that says the VM stopped, and a row says stopped only on a
host's word. Three places wrote it without one: an open whose answer was lost,
a reconcile while the VM's host was quiet, and a handover that ended while its
destination was quiet after a lost answer (`errUnsettled`). A failing seed
prints what the deployment did and what the orchestrator logged.

The campaign runs under a `sim.Scheduler`, so a seed replays exactly. Every
request the fakes receive, and every timer that ends, waits for its turn
(`sim.Admit`): the campaign's own waits, a host's hold, and the orchestrator's
reconcile, flight, source watch, retry and membership timers. Each caller that
goes on beside another is a task of its own (`sim.WithTask`): a campaign step,
an orchestrator process's reconcile, a survey's request to one host, a
migration's source watch, a flight's rewrites and a handover a survey took up.
`TestTheOrchestratorCampaignReplaysItsSeeds` runs 16 seeds twice and requires
the same releases, the same faults and the same log; 500 replay. Before
2026-10-08 the campaign ran without a scheduler, and 19 of its first 32 seeds
went another way on a second run.

## Negative tests in the tree

Most of the fault catalogue is in the tree as `sim.Bug(ctx, id)` guards, at the
site each entry names. `SPROUTFS_SIM_BUG` enables them as a comma-separated
list, which a runtime reads once when it is built. So a catalogue entry is one
test invocation, with no patched source tree and no rebuild:

```sh
SPROUTFS_SIM_BUG=volume-ignore-discard \
  go test ./internal/simtest -run '^TestScheduledWorldReproduces$' -count=1
```

`scripts/mutation/guards.json` lists every guard with the package and test that
kill it and what the guard breaks. Each invocation must fail. `just
check-guards` runs every entry and fails if any passes; `just check` and CI run
it. It builds each package's test binary once, runs each entry once with no
guard on and once with its guard on, and takes about fifteen seconds once the
binaries are built. Entries that name `"goos": "linux"` and `"root": true` (the
`vmmachine` Starter guards) are skipped elsewhere. To show an entry fails every
time:

```sh
python3 scripts/check-guards.py --repeat 5 --guard pager-forget-spill
```

A guard is consulted only under a context that carries a simulated runtime, so
a test that calls the code under a bare `t.Context()` never turns its guard on.
Nothing outside these invocations sets `SPROUTFS_SIM_BUG`, and the mutation
runner clears every other `SPROUTFS_*` setting before it runs them. Ranking
takes no context, so no guard reaches it; Gremlins mutates it instead.

`check-guards` exists because five guards' tests passed with the guard on, on
main, each naming a campaign that no longer reached the guard's path or never
did:

- `pager-forget-spill` named the buggified campaign, whose evictions take only
  clean pages since the isolated arena became the default. It still fails under
  `SPROUTFS_ARENA=shared`.
- `pager-give-back-changed-copy` named the generated schedule, which makes no
  cold copy.
- `migration-give-up-first-receive` named the swizzle campaign. Since the peer
  server's transport waits out a separation inside the receive, no receive
  there fails.
- `migration-corrupt-fallback` and `migration-skip-resume` named the generated
  schedule, whose seeds reach neither path. The `vmmigrate` tests of the
  fallback and of the abandoned migration ran under a bare context and read
  volumes of zeros.

Each now names a focused test (see `guards.json`). Both spill guards sit in
`vmmemory/internal/zirconvm/spillstorage.go`; their tests are the pager's.

The pager's fault-ordering guards ([faults and
read-ahead](vm-memory.md#faults-and-read-ahead)) run over a backing whose reads
take a millisecond a read and a millisecond a page, so each test states what a
fault waited for. Under `pager-read-the-run-first`, every hop of a dependent
chain takes the run's 9 ms instead of the page's 2 ms. Under
`pager-fault-waits-for-its-prefetch`, a fault beside a held prefetch waits an
hour. The fault planning tests run a 4 KiB pager over a real checkpoint store
whose reads take a millisecond, with planning priced at a microsecond per page
located (`vmmemory.WorkPlan`), and count the pages every lookup located
(`sim.WorkPiece.Bytes`). Under `pager-plan-the-window-first`, a forward hop
takes the read and 512 µs instead of the read and 1 µs. Under
`pager-plan-the-window-at-random`, a hop at random locates 512 pages instead of
one. `checkpoint-decode-every-lookup` is counted by
`TestASegmentIsDecodedOncePerCheckpointWhileCached`, which prices a decode by
its bytes (`checkpoint.WorkPageTable`).

The two `zircon-` guards put back Zircon's rules where the port departs from
them ([the plan](../plans/zircon-pager-port-2026-10-05.md), D1 and D4).

The `migration-strip-published-pages`, `migration-strip-published-holes` and
`migration-ask-for-published-pages` guards put back the peer backing's old
rules one at a time. They need a destination that publishes and retires before
its source is released, which
`TestADestinationPublishesWhatItReceivedWhileItsSourceStillServes` provides.
The retire that refuses catches the first; the backing's two answers
disagreeing catch the other two. The generated schedule catches all three as
well. Over seeds 1 to 60 of `TestSeededTopologySoak` and
`TestBuggifiedTopologySoak`, 120 runs, the first fails 88 runs, the second 49
and the third 3, all buggified, because it needs a pager that evicts a page the
destination published.

`migration-stream-in-any-order` has a destination's stream fault its pages all
at once. Its test holds the stream's first request before it is sent: with the
turns no other page is asked for meanwhile, with the guard the other three
pages of the region are. On the generated schedule, this ordering made seed 2
reach an eviction during a publication in about half its runs.

The check in `readIn` that refuses to share a page whose load calls it the
source's own while the extents name it the volume's is not a guard. Once a peer
backing gives both answers by one rule, no load says that, so disabling it
changes nothing a run can see.
`TestASourceServedPageTheExtentsCallPublishedIsNotSharedUnderItsIdentity` in
`vmmemory` tests it against a backing that disagrees.

Some guards were found outside the simulation:

- `host-pull-closed-with-the-machine` by the GCE run of 2026-10-03
  (`docs/measurements/gce-deploy-cache-2026-10-03.md`): a stop ends the machine
  before it publishes, so the stop's checkpoint kept nothing on the disk.
- `peer-payload-after-its-receive` by the same GCE run. Over TCP a payload is
  read under the socket's deadline, which the receive context sets, so every
  keep was reset. The simulated stream reads under no deadline, so its test
  runs over a loopback socket.
- `peer-answer-shares-buffer` by the race detector in
  `TestAGuestFaultIsAnsweredWhileTheStreamSaturatesTheLink`. Whether the next
  reply gets the pooled buffer is the pool's choice, so the package's tests
  fill a buffer with a poison byte as it is released, and the test fails on
  every run.

`peer-refuse-second-hello` and `fill-concurrently` are guards against
scheduler-ordered work that a seed does not reproduce. `fill-concurrently` is
what turned `TestSeededTopologyFingerprintIsStable` red on seed 1 with the
cluster cache on.

`TestAdversarialStarters` runs a fake VMM, not Firecracker, but only on Linux
and as root, because it gives the process's directory to another user. On a
Mac, run its guards through the Lima instance ([Mutation
testing](#mutation-testing)).

## Model checking

Simulation explores the executions its seeds reach. A model checker explores
every execution of a small configuration. `spec/ownership/Ownership.tla` is a
TLA+ model of how one VM changes owner, checked by TLC.

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

A capture never reads an older checkpoint that the capture before it did not
read. The code guarantees this: a publication that fails, or whose outcome the
writer never learns, gives its sealed pages back to the guest as dirty, so the
next capture republishes them. The release's sweep depends on it; without it,
TLC finds a release that deletes what a later selection reads.

`just check-spec` runs the `MC*.cfg` configurations, seconds each, and the
mutants under `spec/ownership/mutants`. A mutant puts one defect back through
the `Bugs` constant and must fail with the invariant it names. The mutants are
a sweep that forgets kept checkpoints, a sweep that takes what the selection
names, a writer that adopts a record of another epoch, a destination that skips
the stale check, a delete that spares no pin, and a pin without the epoch that
names any checkpoint. `just check-spec-deep` runs the larger configurations
under `spec/ownership/deep`, minutes each.

`scripts/tlc.sh` runs TLC from the TLA+ tools 1.7.4 (MIT licence). It fetches
the jar once into `~/.cache/sproutfs` and checks its digest.

`spec/postcopy/PostCopy.tla` models a migration's post-copy:

- the source's book of pages owed, its release and its hold;
- receive attempts that fetch, install, report `Done` or are discarded;
- replies lost after they left the source;
- the orchestrator's retries, its row ageing out of flight, and its survey
  releasing a handover;
- host loss and orchestrator crashes.

Its invariant, `NoSilentLoss`: every page no checkpoint holds is on the serving
source, on its way to a live receive, installed on a destination that runs the
VM, or published. Only a lost host or a hold that ran out may take one.

`spec/recovery/Recovery.tla` models a recovery racing a migration across hosts
that may die. Its survey asks each host in turn and reads the row afterwards.
Its invariant, `NoLiveFence`: a recovery never takes the epoch from a holder
that is alive.

`spec/lineage/Lineage.tla` models a root, its child and its grandchild: forks
that pin and then create the child, roots that name the parent's checkpoints,
sweeps of each VM, and deletes. Its invariant, `NoDanglingRead`: no VM, and no
fork in flight, reads a checkpoint the store no longer has. It also models
candidate designs for the pin collector (TASK-24). A collector that releases a
pin no selected checkpoint reads deletes what a fork in flight is about to
read. The constraints the passing design meets are on TASK-24.

`spec/arena/Arena.tla` models the isolated arena's files: private, tenant
shared, public and fork files, loads by identity, moves, and fork points
lending pages. Its VMMs keep every descriptor they are given, as a compromised
one that ignores `DROP_FILE` does. Its invariant, `Isolated`: no VMM holds a
file with a page of another tenant's VM in it, unless the page is public.

`spec/membership/Membership.tla` is the membership: one object written by
compare-and-set by two hosts and a controller, each from a generation it read;
disks released, let go, assigned, served, removed and listed again as
`membership.Step` admits; a member that lets a disk go only once its own copy
shows the release, and a controller that lets go the disk of a host that died;
hosts that read late and hosts that die; and stripe requests and keeps
delivered to either host, as an address that came to belong to another host
would. A holder behind a request reads the object first, answers only under the
request's generation, and serves only a disk that generation has it serve. Its
invariants are `OneMembership` (no stripe served or placed under a membership
the sender and the holder do not both hold), `NoRegress` (the store applies one
line of generations) and `OneServer` (no two live hosts serve one disk by the
copies they hold). `MCMembership` runs two hosts over six generations in ten
seconds, and `deep/Seven` over seven in about 25. Its mutants drop the
generation check, the assignment check, the condition on a write and the
release before an assignment, and fail `OneMembership`, `OneMembership`,
`NoRegress` and `OneServer`.

`spec/shards/Shards.tla` is a shard moving between members: a controller that
writes the membership one step at a time and acts on the cloud from a snapshot
it read earlier; a cloud that attaches a disk to one machine at a time and takes
it from every process of a machine it detaches it from; hosts, two of which
share a machine, that open a device one process at a time; the lease in the
shard's header, which a host takes as it opens the shard and reads again before
every region it writes; reports of what a host holds that arrive late; and
hosts that die with the shard open. Its invariants are `OneServer` (no two hosts
serve the shard by the copies they hold), `OneOpenerAMachine` and
`NoStaleWrite` (no host writes a region while another assignment holds the
lease). `MCShards` runs three hosts over eight generations in about thirty
seconds, `MCMultiAttach` two hosts under a cloud that may attach the disk to a
second machine, and `deep/Nine` three hosts over nine generations, two of which
may die, in about two minutes. Its mutants let a shard go as soon as it is
released, which fails `OneServer` under the cloud that attaches twice, and also
ignore the lease, which fails `NoStaleWrite`. Under a single-writer cloud, a
shard let go early is still never served twice, because the cloud refuses the
second attach until a detach has taken the device from the first host.

Three specs model the cluster's disk cache that [the
plan](../plans/disk-cache-2026-10-02.md) proposes. Each abstracts the others,
so that no run of TLC takes more than a minute or two.

`spec/diskcache/DiskCache.tla` is the cluster: publications that fail and are
retried, a VM deleted and its name created again, windows striped over the
ranks each host's own list gives (the generation of the membership it holds,
which spec/membership checks two hosts never mix), fills, fill rights, repair,
reads of every rank, hosts marked down, hosts that crash, leave and join, peers
that answer with a wrong stripe, damaged headers, eviction of any stripe, and a
deliberate change of the code. Every stripe names its code. A read tries the
host's code and then each earlier one, and fills a window it rebuilt under an
earlier code under its own. Its invariants are `NoWrongBytes`, `StripesRanked`
and `SurvivesLosses`; the last holds for every code the deployment has used.
Rank 1 gives a fill right once an interval and only while it holds nothing of
the window under its code, and a filler is held to its own list and code as a
cache taking a keep is. Its configurations run four hosts with a 2+1 code, two
with 1+1, three with 2+2 so that stripes go round the hosts, three whose code
changes from 2+1 to 1+1 (`MCChange`) and from 1+1 to 2+1 (`MCWiden`), and three
shards that move between hosts without bound as compute scales (`MCShards`),
whose `MovesKeepStripes` says a window all of whose stripes were readable has
every one readable again once every shard serves. Its mutants put back a read
without the key check, a stripe used without its checksum, a part filled before
its PUT succeeded, a keep taken by a cache its own list does not rank, B5, a
read that tries only its own code (fails `SurvivesLosses` once the code
changes), and a shard that loses its stripes as it moves (fails
`MovesKeepStripes`). `epoch-collision.cfg` is wired as a mutant too: a name
created again that draws its old epoch must fail `NoWrongBytes`, which shows
the model reaches the risk the plan accepts.

`spec/disklog/DiskLog.tla` is one host's disk: its log of regions, eviction
with its second chance, the region it keeps free, reads in flight, the write
budget, and restarts with a torn table. Its invariant is `NoWrongBytes`, and TLC
checks it for deadlock. `EvictionProgresses` is a liveness property, which
`MCEviction.cfg` checks under fairness. Its mutants put back eviction without
its free region, which deadlocks, an unbounded second chance, and a read
without the key check.

`spec/disklimit/DiskLimit.tla` is one host's limiter: the free goal, the spill
promise and its allocation, another writer on the same filesystem, and the
write budget. Its invariants are `PromisesKept` and `GoalKept`. Its mutants put
back a limiter that counts a spill file by its allocation, and B4.

`spec/writeback/Writeback.tla` is a page's dirty life across a checkpoint, as
the pager does it with [the Zircon
port's](../plans/zircon-pager-port-2026-10-05.md) departures D1 to D5. It models
a few pages of one region:

- stores in place, and store faults that copy the page and give the region up
  for their reclaim;
- refaults of spilled pages, which give the region up for their reclaim too;
- eviction in two halves under the page's lock, which spills a dirty page or a
  checkpoint's copy and drops a clean page;
- the pause, and the walk behind it, which meets a reclaim halfway;
- the settle, the upload and a fork point's children reading the checkpoint;
- a publication that lands or fails, the retire and the abandon.

Its invariants are:

- `SealedBytes`: whatever reads a checkpoint reads the bytes of its pause.
- `NoLostWrite`: the guest reads what it last stored, and that write is
  published, in the dirty set, or held by the checkpoint.
- `Reserved`: every private page and every checkpoint's copy owns a
  reservation, and a page that shares the copy owns none.
- `Budget`: no reservation is held twice, so the reservations taken never
  exceed the dirty budget.

`MCWriteback` and `MCFork` run two pages in about two seconds each, and
`deep/Three` runs three pages with fork points in about eighty seconds. Its
mutants put back three defects. Zircon's rule, where a store makes a page the
checkpoint holds Dirty in place, fails `SealedBytes`. The refault that ignored a
retire until 2026-09-22 ([a refault decides again after its
reclaim](vm-memory.md#a-refault-decides-again-after-its-reclaim)) fails
`Reserved`. A walk that skips a page a reclaim holds fails `NoLostWrite`.

`spec/journal` is [durable flush](architecture.md#durable-flush), in two
modules so that no TLC run takes more than a couple of minutes.
`Capture.tla` is one region's captures: stores, flushes, captures, syncs,
seals, selections, abandons, failed batches, and a crash with a replay. A
block holds one of two values and every store flips it, so a block can go back
to bytes an older entry holds, which a digest kept too long would miss. Its
invariants are:

- `NoLostFlush`: after a crash, the replay holds the bytes of every block the
  guest has not stored into since it sent the last answered flush.
- `NoRegression`: after a crash, the replay holds the bytes of every block the
  guest has not stored into since the selected checkpoint's seal.
- `WritableIsUnjournaled`: every page the guest can store into without a fault
  is unjournaled.
- `DigestsDescribeTheJournal`: a block's digest is what a replay would read for
  it once the batch in flight lands.

`MCCapture` runs two pages of two blocks with the pager's rule, a page that was
not unjournaled at a seal keeps its digests, in about a minute; `MCTakeover`
runs three hosts and three epochs in about a minute and a half. `Takeover.tla` is a VM's entries
outliving its host: three hosts and their journal disks, epochs, migrations
with a post-copy, recoveries that read and fence, a host that keeps running
after it is fenced or unreachable, a crash, and a detach and a move of a disk. Its
invariants are `NoLostFlush` for the VM, `NoFencedReplay` (no entry is applied
unless the record names its disk and epoch and it is after the covered
position) and `OneWriter` per disk. `spec/journal/deep` holds larger
configurations of each, and `CaptureDigestsDropped`, which drops every digest
at every seal and passes too. The mutants and what they fail:

| Mutant | Fails |
| --- | --- |
| A seal that forgets the unjournaled pages | `NoLostFlush` |
| A protect trap that does not mark its page | `WritableIsUnjournaled` |
| An unjournaled page that keeps its digests across a seal | `NoLostFlush` |
| A failed batch that keeps its pages' digests (B6) | `NoLostFlush` |
| A failed batch that leaves its pages journaled | `NoLostFlush` |
| A writer that gives out a failed batch's positions again | `NoRegression` |
| A read that does not fence the VM | `NoLostFlush` |
| A destination that drops the source's journal before its post-copy ends | `NoLostFlush` |
| A replay of any epoch | `NoFencedReplay` |
| A cloud that attaches a disk to two machines | `OneWriter` |

`spec/shards` does not model journal disks.

A mutant may expect `deadlock`, or a liveness property, which must then be its
only `PROPERTY`, because TLC does not name the liveness property it finds
violated.

A spec is written from the code by hand, so the simulation ties them together.
The simulated object store reports every change it applies, in its own order
(`sim.ObjectStore.Observe`). Every simulated world checks each change to a
control record or a checkpoint's index object against the ownership spec's
properties by name: `SelectionMoves`, `SelectedReadable`, `PinnedReadable` and
`KeptReadable` (`internal/simtest/ownership.go`). `World.CheckSelected` reports
what it found, so every campaign that ends with it checks its whole trace. A
check that saw no record change in a world with VMs fails.

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
behaviors in the real volume, checkpoint, pager and migration code. It runs the
in-tree guards above, then the two entries of `scripts/mutation/simulation.json`,
which are wrong behaviors in the simulated dependencies and so are applied as
exact source edits to a copy of the tree:

```sh
python3 scripts/mutate-simulation.py --seeds 3
python3 scripts/mutate-simulation.py --seeds 32 --mutant migration-accept-wrong-size
```

The runner copies the current Go sources and test data, including uncommitted
files, into a separate directory and checks the unmodified baseline. It runs
each guard from the baseline binary with `SPROUTFS_SIM_BUG` naming it. It then
applies one source mutation at a time, builds the affected packages, runs their
scheduled scenarios, and, for mutations those miss, the full affected package
suites. Each mutation is restored before the next; the working checkout is
never mutated. The retained directory holds source hashes, the catalogue,
build and test logs, and `report.json`.

`--go-test-exec` runs each test binary through a launcher, and `GOOS` and
`GOARCH` build the binaries for it. The `vmmachine` guards need Linux and root,
so from a Mac they run in the Lima instance:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 python3 scripts/mutate-simulation.py \
  --go-test-exec scripts/mutation/lima-go-test-exec.sh \
  --output "$HOME/.cache/vmmachine-mutations" \
  --mutant vmmachine-accept-reserved-load --mutant vmmachine-skip-owner \
  --mutant vmmachine-give-host-path --mutant vmmachine-prepare-twice \
  --mutant vmmachine-skip-peer-check
```

The output directory must be one the instance mounts. The baseline then runs
the whole `vmmachine` suite there; its Firecracker tests skip, because the
runner clears the `SPROUTFS_*` settings that name the Firecracker assets.

`killed-guard` means the invocation `guards.json` names failed with the guard
enabled. `skipped-goos` is a guard whose entry names another system than the
one the binaries are built for. `killed-scheduled` means a scheduled test
failed with the source mutation installed, and `killed-full` that only the
broader suite caught it. Build errors, missing tests, process errors and
timeouts are separate outcomes, not kills. The command exits nonzero unless a
scheduled test kills every selected mutation and the restored baseline passes.

The cases in `guards.json` and `simulation.json` are hand-selected semantic
faults, not an exhaustive or representative set. More seeds explore the
existing scenarios; they do not add a missing workload, pressure condition or
adversarial input.

### Gremlins

For generated mutations, install [Gremlins](https://gremlins.dev/latest/install/)
and run:

```sh
python3 scripts/mutate-gremlins.py --gremlins /path/to/gremlins --dry-run
python3 scripts/mutate-gremlins.py --gremlins /path/to/gremlins
python3 scripts/mutate-gremlins.py --gremlins /path/to/gremlins --all
```

The wrapper also copies the current source. Gremlins generates and executes
the mutations; the curated catalogue is not used. The default target is
`vmmigrate`, with generated protobuf files excluded. `GOFLAGS` selects only
`TestScheduled.*Reproduces`, and integration mode lets every scheduled scenario
detect mutations in the selected package. `--coverpkg` includes that package's
execution from the host scenario. `--package` chooses another package, and
`--seeds` the scheduled workload count.

`--all` selects every first-party Go package and defaults to the full ordinary
suite in each mutated package. `--integration` runs the entire module's tests
for each mutation, and `--suite scheduled` selects the scheduled scenarios.
Downstream tests can detect package-local survivors, so their results must be
distinguished from integration replays. The source manifest and package
inventory are retained with each run. Go build constraints still apply, so a
macOS run does not exercise Linux-only production code. The wrapper passes the
module root, because passing `./...` to Gremlins can silently produce no
mutations.

The wrapper records each Go test invocation, its mutation and its JSON test
events, without changing Go's arguments or exit status. Read
`audit-summary.json` beside the native results, because Gremlins v0.6.0 can
count Go compilation errors as killed mutants. The audit separates these from
test failures, empty test selections, process errors and timeouts, and checks
that its invocation count agrees with the native execution count.

The [Gremlins campaign](measurements/gremlins-2026-09-11.md) records the pinned
tool version, raw outcomes, survivor checks and a connection-budget regression.
A surviving mutation needs review: it can expose a missing assertion, an
untested input, or an equivalent behavior. Gremlins' default zero thresholds
mean a zero exit status does not imply that all mutants died.

For a change in one Go package, run its ordinary tests, then mutate that
package with the full module available:

```sh
go test ./checkpoint
python3 scripts/mutate-gremlins.py --package checkpoint --suite full \
  --integration --gremlins /path/to/gremlins --output /tmp/checkpoint-mutations
```

Use a new output directory for every run. `--file` limits the mutations to some
production files of the package, named relative to it, and `--run` selects the
tests each mutation runs in a full suite. For test-only changes, select the
production package whose behavior the tests exercise; `--package` includes
subdirectories. Prioritize survivors in data integrity, fencing, authorization,
cancellation and resource ownership over incidental boundary or allocation
changes. A regression test must pass on the correct program and fail with the
exact surviving mutation applied. Keep the before/after evidence separate from
the original campaign's score. For an equivalent mutation, record the reasoning
instead of asserting an unobservable detail. Keep unexplained timeouts as
unresolved outcomes.

The runs below record killed / alive / not covered (/ timed out) before and
after the tests written for survivors. "Changes nothing a run can see" marks
equivalent survivors.

**The page cache's disk:**

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full \
  --file disk.go --file diskformat.go --file diskindex.go --file diskrestart.go --file diskstripes.go \
  --file pull.go \
  --run '^(TestDisk|TestPull|TestAPull|TestOnTwoHosts|TestAReadIsNot|TestALost|TestANewer|TestADiskKeys)' \
  --gremlins /path/to/gremlins --output /tmp/disk-mutations
python3 scripts/mutate-gremlins.py --package stripe --suite full --file stripe.go \
  --gremlins /path/to/gremlins --output /tmp/stripe-mutations
```

**The fills:**

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full --file fill.go --file fillsend.go --file peercache.go --file partwriter.go \
  --run '^(TestAStoreReadFills|TestAColdBurst|TestAFaultIsNot|TestAPublication|TestAPartTheStore|TestACacheKeeps|TestACacheRefuses|TestACacheDrops|TestRankOneGives|TestACacheReports|TestFillsSurvive|TestAPull|TestOnTwoHosts|TestAPulled|TestTheQueue|TestClosingTheCache|TestTheRateOfKeeps|TestTheRateLeaves|TestAKeepIsWritten|TestAFillKeeps|TestAReadsFill|TestADeadHolder|TestAPacedPublication|TestAHostSends|TestAWindowLarger|TestEachHolderKeeps)' \
  --gremlins /path/to/gremlins --output /tmp/fill-mutations
```

- 2026-10-03: 83/16/7 of 106, then 95/7/4. The new tests found that a write
  the disk failed was counted nowhere; it now counts as dropped. The 7 alive
  change nothing a run can see: the default interval, the bug's own wait, which
  cache the ranks-change site takes off the list, a slice's capacity, a
  zero-sized item that fails its header anyway, an error message's arithmetic,
  and a refill of no time.
- 2026-10-04, after a publication's fills began to wait, over `fill.go` and
  `partwriter.go`: 174/51/18 of 255 (12 timed out). Every survivor in
  `partwriter.go` was in untouched lines. Two in the new code of `fill.go` are
  now killed by a test under 1+2. Over `fill.go` alone, after more tests:
  137/24/16 of 177. Three survivors in the new code change nothing a run can
  see: a window that fits the high-water mark exactly, a worker that also
  yields with no prompt work waiting, and a fill told to wait for no time.
  Gremlins reports the worker's own lines uncovered, though every fill test
  runs them.
- 2026-10-04, after keeps went side by side, over `fill.go` and `fillsend.go`:
  150/31/27 of 217 (9 timed out). Two survivors were the bound of a holder's
  lane; `TestAHoldersLaneHoldsTwoOfAPublicationsKeeps` now kills both. The rest
  in the new code change nothing a run can see: the lane of the host's own
  disks left unbounded, a lane left in the map once idle, a fill told to go on
  at once, and the timing of a wait for a busy holder, which no test drives.

**A read's copies and its hedge:**

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full \
  --file clusterread.go --file cache.go --file run.go \
  --run '^(TestTheHedger|TestAReadIsOfTheClass|TestEachSizeClass|TestALargeRead|TestAReadOfThePage|TestAPageReadFromAPeer|TestTheCacheKeeps|TestAPageFills|TestStoreReadsPast|TestAPrefetchs|TestSecondRequests|TestAStalledOrSlow|TestAWrongStripe|TestAPageInTheCluster|TestCache)' \
  --gremlins /path/to/gremlins --output /tmp/cluster-read-mutations
```

2026-10-04: on the changed lines, 39 killed and 3 alive. Two were the length
checks before `sharesReply` compares a stripe's first byte with the envelope's;
`TestAnEmptyStripeBesideAWholeOneIsWrong` now kills both. The third negates
`err == nil` before a load's page count is checked. The whole run: 237/86/62.
`stripe.go`: 69 killed, 4 alive, none in the rebuild.

**The membership**, with the peer server's side of its protocol. The
orchestrator is a package below `cmd`, which Gremlins names wrongly on its own,
so its run adds `--integration`:

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

2026-10-03. `membership`: 109/9/47 of 165, then 113/5. The 5 alive change nothing a run can see: a size
bound at exactly 1 MiB, an interval whose zero the default replaced, a log
line's condition, the lost-reply site's condition, and the first of a view's
two generation checks, which the second repeats under the lock. The 47 not
covered are `case` lines the tests run, `New`'s error branches and the
constants. `peer/cache.go`: 55 of 62 killed; every mutant of the admission and
of `replied` died, and the 5 alive are in untouched code. The orchestrator: all
5 covered killed; the 5 not covered are the interval constant, a `case` line,
and the timer loop, which the tests drive by calling `StepMembership`.

**The shards:**

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

2026-10-04, killed before → after: the cache's shards 17 → 24 (3 alive and 8
not covered before), the membership's steps 39 → 42 (2 alive, 6 not covered),
the host's shards 12 → 15 (8 alive), the simulated cloud 28 → 32 (6 alive),
the orchestrator all 7 covered. The cache's add lost a check `AddShard`
already makes. What is left changes nothing a run can see: a read
of no bytes, the membership's nil check, which of two equally loaded members
takes or gives a shard, how long the slow-close site holds, log lines'
conditions, the size of a simulated disk's filesystem, and simulated disk
errors only a cancelled context makes. The orchestrator's 5 not covered are
its interval constant and timer loop.

**The peer server.** Its page serving moved from `vmmigrate`, whose tests still
drive most of it, so that part runs with `--integration` and those tests:

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

2026-10-03: the first command left 90 of 476 alive (370 killed, 12 timed out, 4
did not build), then 61 of 481 alive with 404 killed. Writing the tests found a
server that crashed when a hello settled on version 1. The second command
killed 95 of 119. A run without `--integration` runs only the mutated file's
own package, so a mutant of `internal/wire` that `peer`'s tests kill counts as
alive there, such as the one that marks every payload checksummed. The rest
cost speed or nothing: the buffer pool's size classes, a buggified delay's
length, a bitmap one byte longer than needed, a map entry left at zero, which
of two connections with room takes a request, a bound whose zero means the
default, and the checksum of a version-2 reply whose every payload is checked
anyway.

**Reads from the cluster:**

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full \
  --file clusterread.go --file clusterdown.go --file peercache.go \
  --run '^(TestAPageInTheCluster|TestAPageSurvives|TestAHotPage|TestAStalledOrSlow|TestAWrongStripe|TestTheStoreIsRead|TestSecondRequests|TestStoreReadsPast|TestThreeTimeouts|TestAReaderMarks|TestAMissIs|TestARefused|TestRepair|TestAReaderRebuilds|TestASampledHit|TestClusterReadsSurvive|TestAFaultIsNotSlowed|TestAColdBurst|TestAStoreReadFills|TestRankOneGives|TestACacheReports|TestTheHedgerFollows|TestProbesWait|TestAHostIsMarkedDown|TestOneRefused)' \
  --gremlins /path/to/gremlins --output /tmp/cluster-read-mutations
```

2026-10-03: 129/54/13 of 196, then 154/29/13. Writing the new tests changed
repair to offer each rank the index the list puts on it first. The rest change nothing a run can see: eleven conditions of a
Buggify site or a guard, which are off in a test that asserts behaviour; five
conditions whose branches differ only in a probe or a counter the tests do not
read for that case; the defaults; the slack in a request's byte bound; a buffer
released on a path where keeping it leaks nothing visible; the boundary of a
timer of zero; the skip of a host marked down, which fails at once if asked;
the check that the reader is among a window's ranks; the spread of a probe's
attempt count; and a repair's count of what is lacking when one index is.

**Reading each window under the code it was stored under** (TASK-85), with the
disk's stripes, the ranking and the orchestrator's code:

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

2026-10-03: the first command 164/35/15 of 217 (3 timed out), then 165/34. Four
survivors were in the new code: a read that counted its own hits only under an
earlier code, and a probe of the disk's earlier code on every read. The two left
in the new code are the `cluster-current-code-only` guard's condition; the
rest are the reads' survivors above. The timeouts are the Buggify site's search
for a code the list does not name, which a mutant makes endless. The second
killed every mutant of the earlier codes, leaving two of the ranking's own
alive and eight of `CodeFor` and `compare` not covered. The third killed all 17
it covered; the 21 not covered are the port parsing and `run`.

**The hot tier**, and the tiers its reads run against with the whole package:

```sh
python3 scripts/mutate-gremlins.py --package checkpoint --suite full --file hottier.go \
  --run '^(TestAMissIsFilled|TestAPublicationWritesTheHot|TestAHotTier|TestAReadIsNotSlowedByItsHotTierFill|TestTheHotTierDrops|TestTwoHostsFilling|TestASampledHotHit|TestAStoreRefusesAHotTier|TestHotTierSurvives|TestAFillThatCannot|TestAClosedHotTier|TestAPublicationIsNotSlowed)' \
  --gremlins /path/to/gremlins --output /tmp/hot-tier-mutations
python3 scripts/mutate-gremlins.py --package checkpoint --suite full --file tier.go \
  --gremlins /path/to/gremlins --output /tmp/tier-mutations
```

2026-10-03: the first command 49/8/19 of 77 (1 timed out), then 51/7/18. Five survivors and the timeout are in
the fault-injection sites. One makes `bug` read `h == nil`, which the rest of the
package's tests catch. One is the sampled check's `headEvery > 0` at zero, which
the default never leaves. Of the 18 not covered, two are the defaults'
constants and the rest are `case` lines the tests run. The second command
killed 22 of 24; the two alive are bounds moved from the store unchanged: a
read of exactly the largest extent, and an object of size zero.

**The object store's bounds:**

```sh
python3 scripts/mutate-gremlins.py --package platform/bounded --suite full \
  --gremlins /path/to/gremlins --output /tmp/bounded-mutations
```

2026-10-04: 41/8/12 of 61 (3 timed out), then 44 killed. One survivor made
a timeout name its stall bound or its first-byte bound alike, which two equal
defaults hide. Its test, with a stall bound shorter than the first-byte bound,
found a fault: an upload that moved from waiting for the first byte to the
stall bound kept the first-byte timer. The watch now arms again for the sooner
deadline. The other kept reading after a stalled reply handed over bytes with
its error. The 5 alive change nothing a run can see: a log line's attempt number
and condition, a timer generation counted down rather than up, a progress of
zero bytes, and a read of zero bytes returned rather than repeated. The 3
timeouts make every attempt time out at once, so the retries never end. The 12
not covered are the two default constants and `case` lines the tests run.

**The port of Zircon's page layer** in `vmmemory/internal/zirconvm`:

```sh
python3 scripts/mutate-gremlins.py --package vmmemory/internal/zirconvm --suite full \
  --gremlins /path/to/gremlins --output /tmp/zirconvm-mutations
```

Its tests read the package's `LICENSE`, so the source snapshot copies every
`LICENSE` beside the Go files.

- 2026-10-05, Zircon's 55 page list cases alone: 343/56/77, mostly the parts
  Zircon reaches only through its VmCowPages tests, then 420/26/30. Every
  survivor is a boundary valid input cannot
  reach or that gives the same result. The 30 not covered are a slot's bit
  layout constants, offsets past the list's end inside an interval, and the
  list's own checks failing.
- Step 5, `pagequeues.go` with Zircon's ten cases: 117/38/78 (7 timed out),
  then 180/17/32 (8 timed out). The survivors are diagnostic loop counts and their log lines, asserts' bounds, a
  ring bound no generation reaches, and the active ratio's product with a
  multiplier of one. 31 of the not covered are compile-time asserts.
- Step 7, `evictor.go` (six of Zircon's seven cases) and `pagequeues.go`, with
  peeks that pass over a page without moving it: 226/42/33 (9 timed out), then
  244/24. The evictor's survivors are bounds where both sides give the same answer and
  the asynchronous path's log lines; the rest are step 5's and a peek bound
  that only does more work.
- Step 8, `pagesource.go` and Zircon's `PagerProxy` in `pagerproxy.go`, with
  the port's own cases (Zircon tests them only through its pager syscalls):
  68/25/4, then 77/17/3. The 3 not covered are the cancel switch's case
  conditions, which the cases reach: each fails a test when applied by hand.
  The survivors are asserts' bounds and changes no caller can see.
- Step 9, the region's layer, `--file` on each of its eleven production files:
  801/257/188 (37 timed out), then 885/178/181 (33 timed out). One survivor was a bug Zircon
  shares: zeroing a child over its parent limit left the parent showing; the
  port departs there. Most survivors left are asserts' bounds, range changes
  past an object's end, the queue counters of steps 5 and 6, and the page
  source's request lists. The resize and pinning paths that would reach the
  rest are not ported.

After a change that spans packages, or before a release, run `--all
--integration` with the full suite, and a Linux run when Linux-specific code
changes. The default wrapper clears opt-in `SPROUTFS_*` settings, so a plain
Linux run does not establish live VM coverage. For a prepared Linux
environment, pass `--go-test-exec /path/to/launcher`. The launcher receives
each compiled test binary and its arguments; it must set the client and
Firecracker asset paths, arrange the required privileges, and execute the
binary without hiding its exit status. The wrapper retains the launcher and its
hash. Check the baseline's JSON test events for the intended live tests.
Restore any temporary huge-page pool after the campaign.

`TestPopulationOrdersRelatedIdentitiesWithoutBlockingOtherPagers` gates two
resident faults while overlapping attachments populate related identities.
Both attachments must finish after release, another pager must progress
independently, and population must reuse resident bytes without cold loads. It
uses `synctest.Wait` to establish blocked phases. Correct and exact-mutant runs
at one and four CPUs validated it.

The control-record reconciliation regressions exercise `Client.Open`,
`Handle.Select` and `Handle.Pin` with object writes that apply but lose their
response, writes that never apply, and a competing writer's takeover. They
assert epoch ownership, fencing, checkpoint selection and the pins a fork left.

## Rust unit tests

The managed-memory client has unit tests for interval generation history,
control-request lifecycles, protocol frames and descriptor transfer, mapping
budgets, and early memory region validation. The history tests compare every
queried interval with an independent per-page model. Protocol tests use a fixed
wire fixture and malformed ancillary input. Control tests check invalid
completions, independent memory regions, cancellation and exhausted request
IDs.

Run these on Linux without KVM, a HugeTLB pool or elevated privileges:

```sh
cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib
```

The crate is Linux-only, so on macOS this command runs nothing. These tests do
not exercise UFFD, page replacement, guest access or the Firecracker
integration.

The ignored `session_drop_releases_mappings_and_closes_retained_controls` test
adds a real UFFD/SCM_RIGHTS session handshake and checks teardown while the
embedding process and a retained control handle stay alive. It needs Linux with
permission to create kernel-mode UFFD descriptors, but neither KVM nor a
HugeTLB pool:

```sh
cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib \
  tests::session_drop_releases_mappings_and_closes_retained_controls \
  -- --exact --ignored --test-threads=1
```

It and the ordinary mapping-drop test identify their mappings by backing or VMA
names in `/proc/self/maps`, because checking whether a freed address is mapped
races with address reuse by other threads.

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

The peer drains UFFD remap events independently of control acknowledgements,
as the real pager does. Malformed batch headers must be rejected before a body
is read. A write-half-close distinguishes rejection from an erroneous read
without a timeout. Run them with the same permissions:

```sh
cargo test --locked --manifest-path rust/sproutfs-vm-memory/Cargo.toml --lib \
  tests::protocol:: -- --ignored --test-threads=1
```

The largest boundary case reserves slightly over 4 GiB of virtual address space
without touching it; allow at least 8 GiB under an address-space limit. The
mutation replay uses an 8 GiB address-space limit and a 30-second process
deadline.

For Rust changes, run the unit suite, then cargo-mutants on Linux (the campaign
used version 27.1.0):

```sh
cargo mutants --dir rust/sproutfs-vm-memory --file src/control.rs \
  --jobs 2 --jobserver-tasks 4 --timeout 15 --build-timeout 180 \
  --output /tmp/control-mutations
```

Omit `--file` for an occasional full-crate run. Keep
`mutants.out/outcomes.json`, the mutation diffs, the test logs and the source
revision. A unit-suite survivor in `Session` or the Linux mapping code still
needs the real Go/Rust interop suites. The example client must be rebuilt from
each mutated crate.

For manual exact-diff replays that share `CARGO_TARGET_DIR`, clean the local
crate before every build (`cargo clean --manifest-path PATH/Cargo.toml -p
sproutfs-vm-memory`). Compile with `cargo test --lib --no-run
--message-format=json`, and require the test executable's compiler-artifact
record to have `fresh: false`. Record its hash, with the exact source and diff,
before you run it. Copying a crate to a new path with its source timestamps is
not enough, because Cargo can reuse the preceding mutant's binary. This applies
to the unmutated baseline too.

## Real transports

Host serving tests use real TCP over a fault-injectable simulated object store.
A host that cannot reach object storage creates nothing. A host that can creates
a VM, checkpoints it, and forks it from a fork point over the running parent. A
second host then takes the VM over by advancing the control record's epoch,
which fences the first. A closed host gives its peer-server port back. The
process that replaces it opens the VM from the control record and the
checkpoint the record selects.

The [Lima suites](vm-memory.md#qualification) test the pager, the mapping
protocol and the Firecracker integration with real KVM and real TCP. They check
physical page sharing through pagemap.

## Continuous integration

[`.github/workflows/check.yml`](../.github/workflows/check.yml) runs `just
check` on every push and pull request, as parallel jobs:

- the Go gate on Linux and on macOS;
- `buf lint`;
- `shellcheck`;
- the Rust crate's `fmt`, `clippy` and unit tests;
- the TLA+ specs.

Every job runs a `just` recipe, so each CI step runs the same way locally.

Only Linux compiles the Linux-only production code, so the Linux Go job covers
more than the macOS one. Both jobs also vet and build for the other operating
system. The Linux-only tests that need no privileges run there (the platform
adapters, the pager wire protocol and the VMM plumbing); the others skip
themselves when their opt-in `SPROUTFS_*` variables are unset.

The [Lima](vm-memory.md#qualification) and GCE suites do not run on GitHub's
runners, which are virtual machines without nested virtualization, so real KVM,
a HugeTLB pool and a Firecracker guest are unavailable. That qualification
happens on Lima or GCE and is recorded under `docs/measurements`. The Rust
crate's `#[ignore]`d UFFD and protocol tests are excluded for the same reason.

### Seed sweeps

`SPROUTFS_TEST_SOAK=1` enables the extended campaigns.
`SPROUTFS_SOAK_SEED_BASE` and `SPROUTFS_SOAK_SEED_COUNT` select the block of
seeds one process runs. `SPROUTFS_SOAK_SUMMARY_DIR` names a directory the
per-seed records are appended to. `internal/testsoak` is the plumbing. The
campaigns that use it are in `internal/simtest`: `TestSeededTopologySoak`,
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
base and a per-block count as inputs. Each job has a 90-minute budget, several
times what a 100-seed block costs, so a seed that stops making progress is
killed. The per-seed summaries are uploaded whether the block passed or failed.

Every seed logs one line and appends the same record as JSON to the summary
directory:

- the simulated time the run explored;
- the wall time the run took;
- the ratio of the two;
- the number of adapter trace events;
- a fingerprint that hashes those events.

The byte-for-byte determinism checks are the `Reproduces` tests, not this
fingerprint.

The ratio differs by campaign. Campaigns with simulated latencies of
microseconds and real computation between them report a ratio far below one.
A campaign that waits out the deadlines its faults impose reports one far above
one: the seeded topology campaign is about a minute of simulated time against a
couple of seconds of wall time, roughly 30x. The recorded scenario takes a
fraction of a second either way. A ratio well below a campaign's usual value
means a real wait has entered a virtual-time test.

A block is a single `go test` process, so a seed that deadlocks panics the
bubble and takes the rest of its block with it. Sweep failures are reproduced
one seed at a time.

## Format fixtures

Nothing is deployed, so a store written by another build is refused, with the
version it was written under named; it is not migrated. Committed fixtures
enforce that:

| Fixture | What it holds |
| --- | --- |
| `volume/testdata/deployment-record-6-index-9-part-5`, `deployment-record-5-index-9-part-5`, `deployment-record-5-index-8-part-4`, `deployment-record-4-index-8-part-4`, `deployment-record-4-index-7-part-4`, `deployment-record-4-part-3`, `deployment-record-4-index-6-part-2`, `deployment-record-4-index-5-part-1`, `deployment-record-3-index-5-part-1` | The whole object namespace of a small deployment: a VM with a history of checkpoints and VMM state whose record keeps its first capture and pins the point it was forked at, and a fork of it whose root names that point's checkpoints. The older dumps are what the builds before the journals, before kept checkpoints, before the page size in the root, before the parts and the index object were split, before the root moved into the last part, before the segmented index and before the pin bump wrote. Opening a VM reads its control record first, and every older dump's record is below format 6, so their test requires that opening each is refused with its record's version named. |
| `control/testdata/record-6`, `record-5`, `record-4`, `record-3`, `record-2` | Two records with pins, one of which keeps two checkpoints and, from format 6, names two journals, at this build's version and at each version committed before it. |
| `journal/testdata/journal-1` | A journal disk whose 48 KiB ring has wrapped: entries of two VMs, a covered position that covers all but one of them, and an entry of a third VM after a pad at the ring's end. It must read back under a newer lease with the entries no checkpoint covered. |
| `checkpoint/testdata/index-7-part-4`, `part-3`, `index-6-part-2`, `index-5-part-1`, `index-4` | The objects of a published checkpoint at this build's formats (its index object and its parts), the objects of the three format sets before it, each refused by the version that moved, and one index table restamped with a version older still. |
| `checkpoint/internal/part/testdata/part-4`, `part-3`, `part-2`, `part-1`, `part-0` | One sealed part holding the VMM state and pages of two volumes, which is everything a part holds; the layout-3 part before it, which also held a segment and the root; the layout-2 part before that, which has a tombstone and no root; the layout-1 part before that, which has no segment member; and a part and table restamped with a version older still. |

The deployment fixture's test loads the fixture into a simulated object store
and runs `CheckDeployment` over it with no allowances. It opens every VM, reads
every byte of every volume and the VMM state the selected checkpoint carries,
and compares them with the committed `contents` manifest. The per-format
fixtures parse the committed bytes back into the values they were written
from. Every superseded fixture must be refused with the version named in the
error text.

Every fixture is written by a `-update` flag on its own test:

```sh
go test ./volume -run TestTheCommittedDeploymentFixture -update
go test ./control -run Fixture -update
go test ./journal -run TestTheCommittedJournalReadsBack -update
go test ./checkpoint -run Fixture -update
go test ./checkpoint/internal/part -run Committed -update
```

`-update` writes only the current fixture. A format bump keeps every committed
fixture, adds one named for the new version, and keeps the old fixture's test.
A superseded fixture is never rewritten, because its value is that its bytes
are the ones that version's build wrote. Use `-update` only for a fixture whose
version has not shipped, never to make a failing old fixture pass.

## Limits

Simulation checks behavior within the modeled failure assumptions and the
executions it explores. Seeds reproduce workloads and dependency choices, but
they do not control the Go scheduler. Explicit gates make critical handoff
races repeatable, and the race detector checks shared-memory access. Tests
against real platform adapters check filesystem and transport behavior an
in-memory model would miss.

Counters printed by the Lima suites are observations, not machine-independent
assertions. No measurement here is performance acceptance; deployment and guest
workloads still have to validate residency budgets, launch latency and
object-storage cost. The collector that releases pins and sweeps what deleted
VMs left pinned has no implementation and so no tests. Its work would be what
the deployment check's allowances name.
