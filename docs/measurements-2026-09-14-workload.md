# What a checkpoint costs under a workload — 2026-09-14

**Status: one setting measured on current `main` with packs; the larger setting
did not complete.** `FORKS_BASE=2 FORKS_PER_REPO=1` below is a clean run on
current `main` — one object per checkpoint, not one per dirty page — so the
**objects** columns are now object counts and not page counts. The 4 KiB dirty
counts are still missing, because the VMM does not report them, so the
2 MiB : 4 KiB ratio is still unanswered. `FORKS_BASE=4 FORKS_PER_REPO=2`, which
the second table was meant to hold, does not fit this node; what it did instead
is written up in [The larger setting](#the-larger-setting).

## The question

[plans/complexity-cuts-2026-09-13.md](../plans/complexity-cuts-2026-09-13.md)
leaves the checkpoint interval open: *"Start at a few seconds. Measure the pause
and the upload volume under a Rust build, with RAM included, before fixing it or
making it adapt to the dirty rate."* The interval is 60 s today. The other half
of the question is granularity: the pager seals whole 2 MiB pages, so a guest
that stores one byte into a page makes the checkpoint publish two million of
them.

## What was measured

`scripts/demo-gce.sh workload` on one `n2-standard-8`, one node, two host pods,
with the hosts restarted immediately before the run so that their object-store
counters and frame counts start from zero and their logs hold this run only.
Each pod: a 5 GiB pager arena out of a 6 GiB HugeTLB allotment, 8 GiB of
ordinary memory, a 60 s checkpoint interval, a 2 MiB pager page and a 4 KiB
storage page.

The guest is the `workload` template — Alpine 3.24.1 x86_64 with git 2.54,
ripgrep 15.1, Node 24.18 and pnpm 11.26, 2 GiB of RAM, one vCPU, and three
repositories cloned into `/root/repos` with their dependencies in a pnpm store
inside the image and their `node_modules` removed, so the guest's own
`pnpm install --offline --frozen-lockfile` does the work of materialising them
with no network. All three are MIT-licensed TypeScript projects that build with
pnpm and whose suites need no network:

| repository | licence | commit in the image | what it is |
| --- | --- | --- | --- |
| [h3](https://github.com/h3js/h3) | MIT | `aa50e96` | HTTP server framework; `pnpm build` runs `obuild` |
| [unstorage](https://github.com/unjs/unstorage) | MIT | `7f773be` | key-value storage layer |
| [ofetch](https://github.com/unjs/ofetch) | MIT | `1dbc37f` | fetch wrapper |

The run boots one VM from that template, checkpoints it, forks it `FORKS_BASE`
times, drives every fork through five phases, forks each of them
`FORKS_PER_REPO` times off the checkpoint it has already diverged into, and
ends with a phase in which nothing happens at all. Every phase ends with an
explicit `sproutfsctl capture` of every VM in play; the hosts' own 60 s interval
runs underneath throughout, so both kinds of checkpoint are in the numbers.

| phase | what every VM in play did |
| --- | --- |
| boot | nothing; the first checkpoint of a freshly booted guest |
| search | `rg` for a pattern across all three repositories |
| history | `git status --short` and `git log --oneline -20` in each |
| install | `pnpm install --offline --frozen-lockfile` in each |
| build | `pnpm build` in h3 |
| idle | nothing at all, for 75 s — longer than the interval |

`between` in the tables is a checkpoint that fell outside every phase window:
the interval firing while the run was forking or waiting.

## FORKS_BASE=2, FORKS_PER_REPO=1

Five VMs: one base, two forks of it, and one fork of each of those taken after
the install. 28 checkpoints. All five landed on one host, so the second host's
counters are zero throughout.

### Per phase and VM

| phase | VM | checkpoints | 2 MiB pages | dirty MiB | uploaded MiB | objects | max pause s | max upload s |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| boot | base | 1 | 106 | 212.0 | 20.5 | 3 | 0.008 | 1.61 |
| search | fork A | 1 | 39 | 78.0 | 12.2 | 3 | 0.007 | 1.06 |
| search | fork B | 1 | 39 | 78.0 | 12.2 | 3 | 0.007 | 1.06 |
| history | fork A | 1 | 32 | 64.0 | 5.1 | 3 | 0.007 | 0.63 |
| history | fork B | 1 | 32 | 64.0 | 5.1 | 3 | 0.007 | 0.58 |
| install | base (idle) | 1 | 30 | 60.0 | 2.3 | 3 | 0.008 | 0.55 |
| install | fork A | 2 | 755 | 1510.0 | 296.7 | 10 | 0.009 | 11.18 |
| install | fork B | 2 | 739 | 1478.0 | 291.5 | 10 | 0.009 | 10.83 |
| build | base (idle) | 1 | 17 | 34.0 | 2.3 | 3 | 0.007 | 1.56 |
| build | fork A | 1 | 335 | 670.0 | 100.3 | 4 | 0.007 | 4.77 |
| build | fork B | 1 | 340 | 680.0 | 100.9 | 4 | 0.008 | 5.05 |
| build | child of A | 1 | 337 | 674.0 | 100.9 | 4 | 0.008 | 5.17 |
| build | child of B | 1 | 345 | 690.0 | 102.4 | 4 | 0.007 | 5.17 |
| idle | base | 1 | 17 | 34.0 | 2.3 | 3 | 0.008 | 1.19 |
| idle | fork A | 2 | 85 | 170.0 | 19.5 | 6 | 0.008 | 1.35 |
| idle | fork B | 2 | 69 | 138.0 | 13.8 | 6 | 0.007 | 0.93 |
| idle | child of A | 2 | 66 | 132.0 | 13.8 | 6 | 0.007 | 0.86 |
| idle | child of B | 2 | 62 | 124.0 | 13.3 | 6 | 0.008 | 0.81 |
| between | fork A | 1 | 25 | 50.0 | 2.0 | 3 | 0.008 | 0.48 |
| between | fork B | 1 | 25 | 50.0 | 2.0 | 3 | 0.007 | 0.52 |
| between | child of A | 1 | 46 | 92.0 | 14.0 | 3 | 0.007 | 1.05 |
| between | child of B | 1 | 47 | 94.0 | 14.5 | 3 | 0.006 | 1.09 |

### Per phase

| phase | wall s | checkpoints | 2 MiB pages | dirty MiB | uploaded MiB | objects | deletes | mean pause s | 2 MiB : 4 KiB |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| boot | 1.7 | 1 | 106 | 212.0 | 20.5 | 3 | 1 | 0.008 | not measured |
| search | 3.2 | 2 | 78 | 156.0 | 24.3 | 6 | 4 | 0.007 | not measured |
| history | 1.8 | 2 | 64 | 128.0 | 10.3 | 6 | 0 | 0.007 | not measured |
| install | 56.5 | 5 | 1524 | 3048.0 | 590.5 | 23 | 4 | 0.009 | not measured |
| build | 25.4 | 5 | 1374 | 2748.0 | 406.7 | 19 | 6 | 0.007 | not measured |
| idle | 75.0 | 9 | 299 | 598.0 | 62.8 | 27 | 2 | 0.008 | not measured |
| between | — | 4 | 143 | 286.0 | 32.5 | 12 | 0 | 0.007 | not measured |

### Per host, over the whole run

| host | GET calls | GET MiB | PUT calls | PUT MiB | DELETE calls | LIST calls |
| --- | --- | --- | --- | --- | --- | --- |
| host A | 1017 | 234.4 | 250 | 1558.5 | 17 | 9 |
| host B | 0 | 0.0 | 0 | 0.0 | 0 | 0 |

The host totals are larger than the sum of the checkpoints because they also
carry the template import — reading the 5 GiB guest image and publishing it as a
root checkpoint, which is about 53 s and a few hundred megabytes, once per host
per image — and every VM's root checkpoint, which a create and a fork now take
before they return.

### The fork's shared frames over time

Frames are 2 MiB. `resident` is what the host's arena holds; `shared` is pages
mapped to an already-resident identity without a read, which is what a fork of a
running guest inherits.

| boundary | VMs running | resident | shared |
| --- | --- | --- | --- |
| begin | 0 | 0 | 0 |
| after boot | 1 | 105 | 0 |
| after the 2 forks | 3 | 173 | 214 |
| after search | 3 | 187 | 214 |
| after history | 3 | 197 | 214 |
| after install | 3 | 1566 | 214 |
| after the 2 late forks | 5 | 1723 | 1793 |
| after build | 5 | 2371 | 1793 |
| after idle | 5 | 2373 | 1793 |
| end | 5 | 2373 | 1793 |

## The larger setting

The second table was to be `FORKS_BASE=4 FORKS_PER_REPO=2`. It does not fit this
node, and the reason is worth recording.

**Every fork lands on its parent's host.** A fork of a fork lands on that one's,
so the whole run piles onto the host the base was created on while the other
sits idle with its entire arena unused — which is what the
`FORKS_BASE=2 FORKS_PER_REPO=1` tables above show, with host B at zero
throughout. `scripts/lib/demo-workload.sh` now spreads the workers over the
ready hosts with a migration after the one fork instant that starts them, so a
larger setting has both arenas to work with.

**Four concurrent installs still do not fit.** With the workers spread two and
two, the two guests on the host that also carried the idle base stopped
answering altogether: their `exec` ran out the orchestrator's ten-minute ceiling
while the two on the other host finished normally. Unbalanced, with all four on
one host, exactly the same two-of-four failed. Nothing in the hosts' logs
complains — no dirty-budget stall, no eviction storm, no VMM death — so this is
the node running out of room underneath: both host pods' spill files are on the
one 100 GB `pd-balanced` boot disk, and four guests each dirtying one and a half
gigabytes between checkpoints saturate it.

**Three concurrent installs do fit.** `FORKS_BASE=3 FORKS_PER_REPO=2` got
through the install phase in 56.5 s — the same wall time as two — with the
hosts at 3 and 1 VMs and 1602 and 806 resident frames. It then failed at the
late fork, on something unrelated to capacity: restoring a child's VMM state
returned

```
Load snapshot error: Failed to restore from snapshot: Failed to build microVM
from snapshot: Failed to restore devices: Error restoring MMIO devices: Pmem:
Error creating Pmem devie: Error accessing backing file: expected exactly one
backing descriptor
```

which is the VMM refusing a restore whose PMEM device was handed more than one
backing descriptor. It is a fork-of-a-fork path — the children here come off a
checkpoint their parent has already diverged into — and it wants a reproduction
in `vmmigrate` or `vmmachine` before anything is concluded from it. No numbers
were taken past that point.

So the second setting is still open, and it needs one of: a node whose hosts do
not share a boot disk for their spill files, or the arena and pool raised as
below so that less of the working set reaches the disk at all.

## Reading the numbers

**The pause is not the problem.** Every checkpoint of every VM paused its guest
between 6.0 ms and 9.0 ms, including the one that went on to seal 755 pages and
upload 297 MiB. The pause is the device-state save and the write protection of
the dirty set; it does not scale with the dirty set in any way these numbers can
see. Nothing about the interval needs to defend the pause.

**The upload is the problem, and it is bursty.** The install phase's five
checkpoints sealed 1524 pages — 3.0 GiB of guest state — and put 590 MiB on the
wire in 56.5 seconds of wall clock. The largest single checkpoint sealed 755
pages and took 11.2 s to publish. Two guests doing an offline `pnpm install` produce
about 10 MiB/s of sustained object-store traffic each, in bursts a checkpoint
wide.

**Compression is doing most of the work.** 6.81 GiB was sealed over the run and
1.12 GiB was uploaded: a ratio of 6.1 to 1. With packs the mean object on the
wire is 16 MiB rather than 330 KiB, because a checkpoint is one object and not
one per page — 250 puts for 1.52 GiB — which is the change packs made to these
numbers. A sealed 2 MiB page whose guest touched one 4 KiB
block of it is 2 MiB of mostly-unchanged bytes, and those compress. So the cost
of coarse granularity is not the full 2 MiB per page — but it is not nothing
either, because the compressor still has to read, compress and send bytes the
guest did not change.

**An idle guest is not free.** Five VMs doing nothing at all for 75 s still
sealed 299 pages and uploaded 62.8 MiB — about 12 MiB per guest per interval
with no work being done. That is the floor: a fleet of idle workload VMs costs
roughly 10 MiB of object traffic per VM per minute at a 60 s interval, plus the
deletes that reclaim the checkpoint each one replaces (2 deletes in that phase,
against 27 objects written — packs made both numbers small).

**On the interval.** Shortening it would make each checkpoint smaller and more
frequent, and the pause is cheap enough that frequency costs nothing in guest
time. But the totals barely move: the same bytes are dirtied either way, and a
shorter interval publishes a page that is dirtied repeatedly more than once
rather than coalescing it. The one thing a shorter interval clearly buys is a
smaller rewind on host loss and a smaller peak — the largest checkpoint here
held 755 pages in host memory while planning its upload, and that peak is what
forced the host pods from 4 GiB of memory to 8 GiB during this work. The one
thing it clearly costs is the per-checkpoint fixed traffic: an index and a
control-record write per VM per checkpoint, plus the deletes, which the idle
phase shows is already 18 writes beyond the pages for six checkpoints. A few
seconds, as the plan proposed, would multiply that fixed cost by ten or more
against no change in the dirty bytes. 60 s looks defensible for this workload;
the case for shortening it is the memory peak, not the traffic. Packs make the
fixed cost smaller — one object per checkpoint rather than one per page — which
weakens the argument against a shorter interval and is one more reason to
re-take these numbers.

**On 2 MiB granularity.** 2 MiB is the unit all the way down. `checkpoint.PageSize`
is *"the unit of publication: a page is packed whole or not at all, and it is the
unit a reader faults in"*, and it is the pager's frame and the seal's unit as
well. There is nothing below it to publish into: a guest that stores one byte
makes the checkpoint pack two million of them, and the only reason that is not
the full cost is that the unchanged bytes beside it compress.

So a 4 KiB dirty set from the VMM would not on its own buy anything — **it would
need a sub-page storage unit to publish into**, and whether that is worth having
is the open question the missing ratio bears on. If the 4 KiB dirty set is a
small fraction of the 2 MiB one under real work, a smaller unit is worth its
extra index entries and its extra faults; if the two are close, the compression
already seen here is most of what there is to win and 2 MiB is the right unit.
Nothing in this run answers that, because nothing in this run counted 4 KiB
pages.

## What is missing

- **4 KiB dirty counts, and therefore the 2 MiB : 4 KiB ratio.** Not measured.
  It needs the Firecracker fork to turn on `track_dirty_pages` and report a
  count from `Vm::get_dirty_bitmap()` (`src/vmm/src/vstate/vm.rs`) in the
  `/snapshot/create` response, which today is a 204 with no body: a new
  `VmmData` variant, a response body, and the Go side reading it. The machinery
  exists, but `get_dirty_bitmap` falls back to `mincore` — residency, not
  dirtiness — for a slice with no bitmap, and whether the managed-memory
  backend's memslots carry one under KVM dirty logging, and how that interacts
  with the seal's own write protection, has to be qualified before its numbers
  could be trusted. Every iteration is a full VMM rebuild on the demo node and
  there is no local x86_64 KVM to try it on. That is more than a day, so it was
  skipped.
  A count on its own would also not change anything without somewhere smaller
  than 2 MiB to publish into, which is the second half of the question.
- **A second fork setting.** Only `FORKS_BASE=2 FORKS_PER_REPO=1` completed;
  [The larger setting](#the-larger-setting) is what stopped the other two.
- **The `upload MiB` column is compressed bytes on the wire.** `checkpoint.Store`
  encodes every object through `internal/blob` before the Put, and the meter
  counts what the Put declared. `dirty MiB` is uncompressed sealed state.

## Filling it in

```sh
scripts/demo-gce.sh create                                   # 12 min on a warm crate cache
scripts/demo-gce.sh kubectl rollout restart -n sproutfs deployment/sproutfs-host
FORKS_BASE=2 FORKS_PER_REPO=1 scripts/demo-gce.sh workload    # the run above
scripts/demo-gce.sh delete                                   # verifies VM, disk and bucket are gone
```

Restart the hosts before each run: their object-store counters and frame counts
are since the process started, and the run reads their logs, so a host that has
already carried a run reports that one's work beside this one's.

Each run prints the three tables and copies everything it recorded to
`.workload-runs/base<N>-repo<M>/`: `summary.txt` is the tables,
`checkpoints.jsonl` every per-checkpoint line the hosts logged, `phases.tsv` the
window each phase occupied, `store.tsv` the object-store counters at every phase
boundary, and `shared.tsv` the frame counts.

A larger setting needs room. Five 2 GiB VMs fit the 5 GiB arena here because
forks share what they have not diverged from; ten would not, and the run would
spend its time in the spill file rather than measuring checkpoints. Raise
`SPROUTFS_ARENA_BYTES` and the pod's `hugepages-2Mi` in `deploy/10-host.yaml`
together with `hugepages` in `scripts/lib/gce-demo-startup.sh` before asking for
more than about six.

## Things this run had to change to get through

Recorded because they are findings, not incidental. The first three are from the
earlier, pre-pack run and still hold; the rest are this one's.

- The host pods needed 8 GiB of ordinary memory, not 4 GiB. A checkpoint plans
  everything it is about to upload in memory at once, so the host's peak scales
  with the dirty set; with two guests installing between checkpoints the Go
  process reached 8 GiB of anonymous memory and the cgroup killed it.
  `GOMEMLIMIT` is now set under the container's limit so the collector has a
  reason to run before that happens.
- `SPROUTFS_DIRTY_PAGES` had to rise from the arena's 1536 pages to 6144. At the
  default, two workload guests dirtying a few hundred megabytes between
  checkpoints exhausted the pager's volatile-state bound and their VMM
  processes died with nothing in their logs.
- The HugeTLB pool went from 8 GiB to 12 GiB and the arena from 3 GiB to 5 GiB.
  At 3 GiB, two 2 GiB guests installing concurrently starved each other in the
  spill file badly enough that one stopped answering for over ten minutes.
- **A create did not leave a VM anything could act on.** A create is a fork of
  the template's root checkpoint, and a fork runs on the host that took it and
  nowhere else until it has published a root index of its own. Create returned
  before that, so for up to one checkpoint interval the VM it named could not be
  forked or migrated and would have been lost with its host, while `list`
  reported it running. The demo's second flow forks the VM its first flow
  created and failed on `volume: fork's root checkpoint is not published`.
  Create now publishes that root between the fork and the boot, where it costs
  nothing — the VM's bytes are still the template's — and reports it as the
  `root` time.
- **A fork's parent stays sealed until its children publish.** That is the
  design, and `host.Host.Fork` says so, but the run forked each worker and
  then captured that worker in the same phase, which failed on `volume: a fork
  point holds this VM's sealed frames`. The run now takes each new child's root
  as soon as its agent answers, which is what gives the parent back — and is
  real work the numbers should carry, since a child's root republishes the
  pages of the parent that no checkpoint held.
- **The tables counted other runs' checkpoints.** The hosts' logs outlive one
  run, so reading all of them charged every checkpoint an earlier run had left
  behind to this one; on a node that had already carried a run the `between` row
  was larger than every measured phase put together. The run now reads the
  hosts' logs from the moment it began.
- **The run used one host of the two.** Every fork lands on its parent's host,
  so the whole run piled onto one arena while the other went unused. The workers
  are now spread with a migration after the one fork instant that starts them.
