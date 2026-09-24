# What a checkpoint costs under a workload — 2026-09-14

**Status: one setting measured on current `main`, which writes one object per
checkpoint. The larger setting did not complete.** The
`FORKS_BASE=2 FORKS_PER_REPO=1` run below is a clean run on current `main`.
Current `main` writes one object per checkpoint instead of one per dirty page,
so the **objects** columns now count objects and not pages. The 4 KiB dirty
counts are still missing because the VMM does not report them, so the
2 MiB : 4 KiB ratio is still unanswered. The second table was meant to hold
`FORKS_BASE=4 FORKS_PER_REPO=2`, but that setting does not fit this node.
[The larger setting](#the-larger-setting) describes what happened instead.

## The question

[plans/complexity-cuts-2026-09-13.md](../plans/complexity-cuts-2026-09-13.md)
leaves the checkpoint interval open: *"Start at a few seconds. Measure the pause
and the upload volume under a Rust build, with RAM included, before fixing it or
making it adapt to the dirty rate."* The interval is 60 s today. The other half
of the question is granularity. The pager seals whole 2 MiB pages, so if a
guest stores one byte into a page, the checkpoint publishes two million bytes.

## What was measured

`scripts/demo-gce.sh workload` ran on one `n2-standard-8`, with one node and
two host pods. The hosts were restarted immediately before the run. Their
object-store counters and page counts therefore started from zero, and their
logs held only this run. Each pod had:

- a 5 GiB pager arena out of a 6 GiB HugeTLB allotment;
- 8 GiB of ordinary memory;
- a 60 s checkpoint interval;
- a 2 MiB pager page and a 4 KiB storage page.

The guest is the `workload` template: Alpine 3.24.1 x86_64 with git 2.54,
ripgrep 15.1, Node 24.18 and pnpm 11.26, with 2 GiB of RAM and one vCPU. Three
repositories are cloned into `/root/repos`. Their dependencies are in a pnpm
store inside the image, and their `node_modules` directories are removed. The
guest's `pnpm install --offline --frozen-lockfile` therefore does the work of
materialising the dependencies, without a network. All three are MIT-licensed
TypeScript projects that build with pnpm and whose test suites need no network:

| repository | licence | commit in the image | what it is |
| --- | --- | --- | --- |
| [h3](https://github.com/h3js/h3) | MIT | `aa50e96` | HTTP server framework; `pnpm build` runs `obuild` |
| [unstorage](https://github.com/unjs/unstorage) | MIT | `7f773be` | key-value storage layer |
| [ofetch](https://github.com/unjs/ofetch) | MIT | `1dbc37f` | fetch wrapper |

The run does the following:

1. Boots one VM from that template and checkpoints it.
2. Forks it `FORKS_BASE` times.
3. Drives every fork through five phases.
4. Forks each of those forks `FORKS_PER_REPO` times, from the checkpoint that
   fork has already diverged into.
5. Ends with a phase in which nothing happens.

Every phase ends with an explicit `sproutfsctl capture` of every VM in play.
The hosts' 60 s interval also runs throughout, so the numbers include both
kinds of checkpoint.

| phase | what every VM in play did |
| --- | --- |
| boot | nothing; the first checkpoint of a freshly booted guest |
| search | `rg` for a pattern across all three repositories |
| history | `git status --short` and `git log --oneline -20` in each |
| install | `pnpm install --offline --frozen-lockfile` in each |
| build | `pnpm build` in h3 |
| idle | nothing at all, for 75 s — longer than the interval |

In the tables, `between` is a checkpoint that fell outside every phase window.
These are interval checkpoints that fired while the run was forking or waiting.

## FORKS_BASE=2, FORKS_PER_REPO=1

Five VMs: one base, two forks of it, and one fork of each of those taken after
the install. 28 checkpoints. All five VMs ran on one host, so the second host's
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
include:

- The template import. It reads the 5 GiB guest image and publishes it as a
  root checkpoint. This takes about 53 s and a few hundred megabytes, once per
  host per image.
- Every VM's root checkpoint. A create and a fork now take it before they
  return.

### The fork's shared pages over time

Frames are 2 MiB. `resident` is the number of pages the host's arena holds.
`shared` is the number of pages mapped to an already-resident identity without
a read. A fork of a running guest inherits its pages this way.

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

The second table was planned for `FORKS_BASE=4 FORKS_PER_REPO=2`. That setting
does not fit this node. This section records why.

**Every fork lands on its parent's host.** A fork of a fork lands on that
fork's host. The whole run therefore piles onto the host the base was created
on, while the other host sits idle with its entire arena unused. The
`FORKS_BASE=2 FORKS_PER_REPO=1` tables above show this, with host B at zero
throughout. `scripts/lib/demo-workload.sh` now spreads the workers over the
ready hosts, with a migration after the single fork point that starts them. A
larger setting can therefore use both arenas.

**Four concurrent installs still do not fit.** With the workers spread two per
host, the two guests on the host that also held the idle base stopped
answering. Their `exec` calls reached the orchestrator's ten-minute limit,
while the two guests on the other host finished normally. With all four on one
host, the same two of four failed. The hosts' logs report no problem: no
dirty-budget stall, no eviction storm, no VMM death. The node itself therefore
ran out of room. Both host pods' spill files are on the one 100 GB
`pd-balanced` boot disk, and four guests that each dirty 1.5 GB between
checkpoints saturate it.

**Three concurrent installs do fit.** `FORKS_BASE=3 FORKS_PER_REPO=2` finished
the install phase in 56.5 s, the same wall time as two. The hosts had 3 and 1
VMs, and 1602 and 806 resident pages. The run then failed at the late fork, for
a reason unrelated to capacity. Restoring a child's VMM state returned:

```
Load snapshot error: Failed to restore from snapshot: Failed to build microVM
from snapshot: Failed to restore devices: Error restoring MMIO devices: Pmem:
Error creating Pmem devie: Error accessing backing file: expected exactly one
backing descriptor
```

This means the VMM refused a restore because the PMEM device received more
than one backing descriptor. The failure is on the fork-of-a-fork path: the
children here come from a checkpoint their parent has already diverged into. It
needs a reproduction in `vmmigrate` or `vmmachine` before any conclusion is
drawn from it. No numbers were taken after that point.

The second setting is therefore still open. It needs one of the following:

- a node whose hosts do not share a boot disk for their spill files;
- the arena and pool raised as described below, so that less of the working
  set reaches the disk.

## Reading the numbers

**The pause is not the problem.** Every checkpoint of every VM paused its guest
for between 6.0 ms and 9.0 ms. This includes the checkpoint that then sealed
755 pages and uploaded 297 MiB. The pause is the device-state save plus the
write protection of the dirty set. These numbers show no growth of the pause
with the dirty set. The choice of interval does not need to account for the
pause.

**The upload is the problem, and it is bursty.** The install phase's five
checkpoints sealed 1524 pages, which is 3.0 GiB of guest state. They sent
590 MiB on the wire in 56.5 seconds of wall-clock time. The largest single
checkpoint sealed 755 pages and took 11.2 s to publish. Two guests running an
offline `pnpm install` each produce about 10 MiB/s of sustained object-store
traffic, in bursts that last one checkpoint.

**Compression is doing most of the work.** The run sealed 6.81 GiB and uploaded
1.12 GiB, a ratio of 6.1 to 1. Because a checkpoint is now one object instead
of one per page, the mean object on the wire is 16 MiB instead of 330 KiB:
250 PUTs for 1.52 GiB. That is how the one-object change affected these
numbers. A sealed 2 MiB page in which the guest touched one 4 KiB block is
2 MiB of mostly unchanged bytes, and those bytes compress. Coarse granularity
therefore does not cost the full 2 MiB per page. It still costs something,
because the compressor must read, compress and send bytes the guest did not
change.

**An idle guest is not free.** Five VMs that did nothing for 75 s still sealed
299 pages and uploaded 62.8 MiB. That is about 12 MiB per guest per interval
with no work done. This is the floor. At a 60 s interval, a fleet of idle
workload VMs costs about 10 MiB of object traffic per VM per minute. It also
costs the deletes that reclaim the checkpoint each new one replaces. That phase
had 2 deletes against 27 objects written. Writing one object per checkpoint
made both numbers small.

**On the interval.** A shorter interval would make each checkpoint smaller and
more frequent. The pause is cheap, so higher frequency costs nothing in guest
time. However, the totals barely change. The same bytes are dirtied either
way, and a shorter interval publishes a repeatedly dirtied page more than once
instead of coalescing it.

A shorter interval clearly gives two benefits: a smaller rewind on host loss,
and a smaller peak. The largest checkpoint here held 755 pages in host memory
while it planned its upload. That peak forced the host pods from 4 GiB of
memory to 8 GiB during this work.

A shorter interval clearly costs more fixed traffic per checkpoint: an index
and a control-record write per VM per checkpoint, plus the deletes. The idle
phase shows that this is already 18 writes beyond the pages for six
checkpoints. An interval of a few seconds, as the plan proposed, would multiply
that fixed cost by ten or more, with no change in the dirty bytes.

60 s looks defensible for this workload. The argument for a shorter interval is
the memory peak, not the traffic. Writing one object per checkpoint instead of
one per page makes the fixed cost smaller. That weakens the argument against a
shorter interval, and it is another reason to measure these numbers again.

**On 2 MiB granularity.** 2 MiB is the unit at every layer.
`checkpoint.PageSize` is documented as the unit of publication. A page is
published whole or not at all, and it is the unit a reader faults in. It is
also the pager's page and the unit of the seal. There is no smaller unit to
publish into. If a guest stores one byte, the checkpoint publishes two million
bytes. The only reason that is not the full cost is that the unchanged bytes
around it compress.

A 4 KiB dirty set from the VMM would therefore not help by itself. **It would
need a sub-page storage unit to publish into.** Whether that unit is worth
having is the open question that the missing ratio would answer:

- If the 4 KiB dirty set is a small fraction of the 2 MiB one under real work,
  a smaller unit is worth its extra index entries and its extra faults.
- If the two are close, the compression seen here is most of the possible
  gain, and 2 MiB is the right unit.

This run does not answer that, because it did not count 4 KiB pages.

## What is missing

- **4 KiB dirty counts, and therefore the 2 MiB : 4 KiB ratio.** Not measured.
  The Firecracker fork would need to turn on `track_dirty_pages` and report a
  count from `Vm::get_dirty_bitmap()` (`src/vmm/src/vstate/vm.rs`) in the
  `/snapshot/create` response. That response is currently a 204 with no body,
  so this needs a new `VmmData` variant, a response body, and Go code that
  reads it. The machinery exists. However, for a slice with no bitmap,
  `get_dirty_bitmap` falls back to `mincore`, which reports residency and not
  dirtiness. Before its numbers can be trusted, two things must be checked:
  whether the managed-memory backend's memslots carry a bitmap under KVM dirty
  logging, and how that interacts with the seal's write protection. Every
  iteration needs a full VMM rebuild on the demo node, and there is no local
  x86_64 KVM to test on. That is more than a day of work, so it was skipped.
  A count alone would also change nothing without a unit smaller than 2 MiB to
  publish into. That is the second half of the question.
- **A second fork setting.** Only `FORKS_BASE=2 FORKS_PER_REPO=1` completed.
  [The larger setting](#the-larger-setting) explains what stopped the other
  two.
- **The `upload MiB` column is compressed bytes on the wire.**
  `checkpoint.Store` encodes every object through `internal/blob` before the
  Put, and the meter counts the size the Put declared. `dirty MiB` is
  uncompressed sealed state.

## Filling it in

```sh
scripts/demo-gce.sh create                                   # 12 min on a warm crate cache
scripts/demo-gce.sh kubectl rollout restart -n sproutfs deployment/sproutfs-host
FORKS_BASE=2 FORKS_PER_REPO=1 scripts/demo-gce.sh workload    # the run above
scripts/demo-gce.sh delete                                   # verifies VM, disk and bucket are gone
```

Restart the hosts before each run. Their object-store counters and page counts
accumulate from process start, and the run reads their logs. A host that has
already carried a run reports that run's work together with the current run's
work.

Each run prints the three tables and copies everything it recorded to
`.workload-runs/base<N>-repo<M>/`:

- `summary.txt`: the tables;
- `checkpoints.jsonl`: every per-checkpoint line the hosts logged;
- `phases.tsv`: the time window of each phase;
- `store.tsv`: the object-store counters at every phase boundary;
- `shared.tsv`: the page counts.

A larger setting needs more room. Five 2 GiB VMs fit the 5 GiB arena here
because forks share the pages they have not diverged from. Ten would not fit,
and the run would spend its time in the spill file instead of measuring
checkpoints. Before asking for more than about six, raise
`SPROUTFS_ARENA_BYTES` and the pod's `hugepages-2Mi` in `deploy/10-host.yaml`,
together with `hugepages` in `scripts/lib/gce-demo-startup.sh`.

## Things this run had to change to get through

These are recorded because they are findings. The first three come from the
earlier run, before a checkpoint was written as one object, and still apply.
The rest come from this run.

- The host pods needed 8 GiB of ordinary memory, not 4 GiB. A checkpoint plans
  its entire upload in memory at once, so the host's peak memory scales with
  the dirty set. With two guests installing between checkpoints, the Go process
  reached 8 GiB of anonymous memory and the cgroup killed it. `GOMEMLIMIT` is
  now set below the container's limit, so the garbage collector runs before
  that happens.
- `SPROUTFS_DIRTY_PAGES` had to rise from the arena's 1536 pages to 6144. At
  the default, two workload guests that dirtied a few hundred megabytes between
  checkpoints exhausted the pager's volatile-state bound. Their VMM processes
  died with nothing in their logs.
- The HugeTLB pool went from 8 GiB to 12 GiB, and the arena from 3 GiB to
  5 GiB. At 3 GiB, two 2 GiB guests installing concurrently competed for the
  spill file so badly that one stopped answering for over ten minutes.
- **A create did not leave a VM that other operations could act on.** A create
  is a fork of the template's root checkpoint. A fork runs only on the host
  that took it until it has published its own root index. Create returned
  before that. For up to one checkpoint interval, the VM it named could not be
  forked or migrated and would have been lost with its host, while `list`
  reported it as running. The demo's second flow forks the VM that its first
  flow created, and it failed with
  `volume: fork's root checkpoint is not published`. Create now publishes that
  root between the fork and the boot, where it costs nothing because the VM's
  bytes are still the template's. It reports this as the `root` time.
- **A fork's parent stays sealed until its children publish.** This is
  intended, and `host.Host.Fork` documents it. The run forked each worker and
  then captured that worker in the same phase, which failed with
  `volume: a fork point holds this VM's sealed pages`. The run now takes each
  new child's root checkpoint as soon as its agent answers, which releases the
  parent. This is real work that the numbers should include, because a child's
  root republishes the parent's pages that no checkpoint held.
- **The tables counted other runs' checkpoints.** The hosts' logs persist
  across runs. Reading all of them charged this run for every checkpoint that
  an earlier run had left. On a node that had already carried a run, the
  `between` row was larger than all measured phases combined. The run now reads
  the hosts' logs only from the moment it began.
- **The run used only one of the two hosts.** Every fork lands on its parent's
  host, so the whole run piled onto one arena while the other was unused. The
  workers are now spread with a migration after the single fork point that
  starts them.
