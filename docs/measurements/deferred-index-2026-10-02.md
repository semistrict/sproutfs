# Deferred index objects — 2026-10-02

**Status: simulated.** Three stores ran the same page workloads on the
simulated object store. The times come from the store's latency model, not a
real store's. TASK-81 tracks the work.

## The question

TASK-66 compared the index layout with an LSVD-style log layout (branch
`claude/lsvd-log-layout`). The log layout saved upload because it did not
rewrite page-table segments at every checkpoint. That saving does not need a
second layout. A checkpoint can **defer** its index object: its last part
carries what changed, and the next index object writes every segment changed
since the last one ([volumes](../volumes.md#deferred-index-objects)). How much
of the log layout's saving does that keep, and what does it cost?

## What was measured

`just compare-index` runs `TestCompareIndexCadences` at `9eb0ab5` plus this
change. It uses the same workloads as TASK-66. The VM has 2 GiB of RAM in 4 KiB
pages and an 8 GiB root disk in 2 MiB pages. Its first checkpoint writes half
of its RAM, scattered, and the first GiB of its disk. An eighth of each written
page is random and the rest is zeroes. The store answers a GET or LIST in 10 ms
and a PUT in 20 ms, at 500 MiB/s. Opens, and the reads after them, run on a
second store with an empty cache.

- `index` writes an index object at every checkpoint, which is today's
  behaviour and the default.
- `deferred/16` and `deferred/64` write one at every 16th and every 64th
  checkpoint (`Config.IndexEvery`).

### Publishing a checkpoint

Mean per checkpoint over 16 checkpoints of each size. The RAM pages are
scattered, and each checkpoint also writes one root-disk page.

| RAM pages | cadence | PUTs | part MiB | index KiB | compaction GETs | mean commit ms | worst commit ms |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 64 | index | 2.0 | 0.29 | 1018.6 | 0 | 43 | 43 |
| 64 | deferred/16 | 1.1 | 0.29 | 74.8 | 0 | 22 | 43 |
| 64 | deferred/64 | 1.0 | 0.29 | 0.0 | 0 | 21 | 21 |
| 1024 | index | 2.0 | 0.86 | 1293.8 | 0 | 44 | 44 |
| 1024 | deferred/16 | 1.1 | 0.86 | 84.6 | 0 | 23 | 44 |
| 1024 | deferred/64 | 1.0 | 0.86 | 0.0 | 0 | 22 | 22 |
| 8192 | index | 2.0 | 5.18 | 1510.0 | 4 | 73 | 376 |
| 8192 | deferred/16 | 1.1 | 5.18 | 101.1 | 4 | 52 | 353 |
| 8192 | deferred/64 | 1.0 | 5.13 | 0.0 | 0 | 30 | 30 |
| 32768 | index | 2.8 | 31.05 | 1723.6 | 82 | 383 | 729 |
| 32768 | deferred/16 | 1.5 | 26.99 | 104.9 | 82 | 274 | 807 |
| 32768 | deferred/64 | 1.3 | 24.51 | 104.9 | 84 | 190 | 1779 |

A deferred checkpoint is one round of PUTs, so it commits in half the time. The
index object that ends a run of deferred checkpoints holds every segment the run
changed, and costs about what one ordinary index object does. Averaged over 16
checkpoints, that is about 75 to 105 KiB of index per checkpoint, where the
index layout uploads 1 to 1.7 MiB every time.

The worst commits are slower at the largest dirty sets: 1.8 s at `deferred/64`.
That checkpoint runs a compaction its predecessors skipped, because compaction
leaves alone the checkpoints a deferred index replays.

### A minute of writes at each checkpoint interval

The guest writes 2,000 RAM pages a second, and four in five land in a hot set
of 8,192 pages.

| interval s | cadence | checkpoints | PUTs | part MiB | index MiB | index share |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | index | 60 | 120 | 118.48 | 108.08 | 48% |
| 1 | deferred/16 | 60 | 63 | 97.79 | 5.23 | 5% |
| 1 | deferred/64 | 60 | 60 | 76.00 | 0.00 | 0% |
| 5 | index | 12 | 24 | 82.99 | 22.61 | 21% |
| 5 | deferred/16 | 12 | 13 | 67.72 | 1.81 | 3% |
| 5 | deferred/64 | 12 | 13 | 76.50 | 1.76 | 2% |
| 15 | index | 4 | 8 | 41.95 | 7.74 | 16% |
| 15 | deferred/16 | 4 | 4 | 34.96 | 0.00 | 0% |
| 15 | deferred/64 | 4 | 4 | 32.31 | 0.00 | 0% |
| 60 | index | 1 | 4 | 56.77 | 1.90 | 3% |
| 60 | deferred/16 | 1 | 2 | 29.72 | 0.00 | 0% |
| 60 | deferred/64 | 1 | 2 | 27.20 | 0.00 | 0% |

This is the log layout's saving: at a 1 s interval the index falls from nearly
half of what a checkpoint uploads to 5%. Part bytes fall too, because
compaction does not rewrite pages out of the checkpoints a deferred index still
names. The deferred records themselves ride in the parts. They are a root
without segments and the pages a checkpoint zeroed, and do not show at this
scale.

### Opening a checkpoint

An open on another host after each of 64 checkpoints of 1,024 RAM pages.

| cadence | mean requests | worst requests | mean KiB read | mean ms | worst ms |
| --- | --- | --- | --- | --- | --- |
| index | 1.0 | 1 | 256.0 | 10 | 10 |
| deferred/16 | 12.0 | 36 | 2732.2 | 56 | 66 |
| deferred/64 | 48.5 | 84 | 7289.3 | 80 | 96 |

This is the cost. An open of a deferred checkpoint makes these requests in
order:

1. A GET that finds no index object.
2. A listing of the checkpoint's parts.
3. Its last part's table.
4. The base's index object and every part table of the chain, at once.
5. The base's segments the chain changed, one ranged GET per index object that
   holds them.

That is five round trips where an indexed open takes one. A checkpoint that
writes an index object opens as it always has.

### Reading after an open

One 4 KiB fault, then 64 read-ahead runs of 2 MiB of RAM at random.

| cadence | first fault requests | first fault ms | runs requests | runs ms | page entries at open | after runs |
| --- | --- | --- | --- | --- | --- | --- |
| index | 2 | 20 | 3050 | 4317 | 0 | 204669 |
| deferred/16 | 1 | 10 | 3012 | 4045 | 262268 | 262268 |
| deferred/64 | 1 | 10 | 3020 | 4064 | 262268 | 262268 |

A replay leaves every segment it changed decoded, so the first fault needs no
segment fetch. An open plus a first fault costs 30 ms with an index object and
66 ms at `deferred/16`.

### Forking

A fork's first checkpoint always writes an index object, so a fork's checkpoint
and open cost the same at every cadence. The fork's index object holds the
segments its parent left pending, which is why it is about 250 KiB larger.

| cadence | first checkpoint PUTs | PUT KiB | commit ms | open requests | open ms | first fault ms |
| --- | --- | --- | --- | --- | --- | --- |
| index | 2 | 1854.2 | 44 | 1 | 10 | 20 |
| deferred/16 | 2 | 2101.5 | 44 | 1 | 10 | 20 |
| deferred/64 | 2 | 2103.7 | 44 | 1 | 10 | 20 |

### What the store holds

Sampled after every checkpoint of the whole run and its reclamation.

| cadence | checkpoints | mean part MiB | mean metadata MiB | mean total MiB | peak total MiB | final total MiB |
| --- | --- | --- | --- | --- | --- | --- |
| index | 206 | 461.57 | 54.38 | 515.95 | 672.54 | 646.69 |
| deferred/16 | 206 | 464.68 | 5.28 | 469.96 | 698.65 | 541.09 |
| deferred/64 | 206 | 482.75 | 1.90 | 484.65 | 741.73 | 528.48 |

Index metadata falls by a factor of ten, because fewer index objects are kept
alive by a segment or two. A chain is held until the next index object, so the
peak is 4% higher at `deferred/16` and 10% higher at `deferred/64`.

## Against the log layout

Publishing costs match the log layout's (TASK-66), PUT for PUT: one round, and
an index object only at the end of a run. Opening is dearer: 56 ms against
27 ms for log/16. The log layout's map object held every segment and was read
whole in one GET. Here the base's index object holds only what it changed, so a
replay also reads the segments the chain touched from the objects that hold
them.

Building this as an evolution of the index layout found two faults the log
layout's design shared:

- The log layout put the last part, which is the commit, while earlier parts
  could still be in flight. A host lost between the two would leave a
  committed checkpoint that does not open. Here the last part waits for every
  earlier part (`TestDeferredCommitWaitsForEveryEarlierPart`).
- A replay rebuilds a segment from the base's copy, so the deferred index must
  name the checkpoints whose index objects hold those copies. Without that,
  reclamation deleted one during this comparison, and a later open failed
  (`TestDeferredIndexKeepsTheSegmentsItsBaseAddresses`).

## What it means

At the 60 s default, deferral saves one PUT and 20 ms per checkpoint, and none
of it is on the pause path. It earns its open cost only at short intervals:
below about 15 s, the index is a sixth or more of every upload. 16 is a
reasonable cadence, because it bounds an open at about 60 ms on this model.

## What this does not show

- The latency model is the simulator's. A real LIST is slower than a GET.
- The workload is synthetic: uniform scattered RAM writes and one hot set.
- Nothing in the host sets `IndexEvery` yet, so no simulation campaign
  exercises deferral under failures. The lockstep and crash tests in
  `checkpoint/deferred_test.go` are the evidence for its correctness.
- The deployment audit still reports an unreached deferred checkpoint as a
  publication that never finished, because it has no index object.
