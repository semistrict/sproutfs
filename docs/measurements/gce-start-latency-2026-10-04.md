# How long a start, a restore and a fork take until the guest runs, on GCE, 2026-10-04

This measures four ways a VM starts, 400 times each, from the request to the
guest running and to the guest's agent answering:

- **cold**: a create from the alpine template. The child boots its kernel.
- **restore**: an open of a VM suspended with `stop --suspend`. Half the
  restores are on the host that suspended it and half on the other host.
- **fork-local**: a fork of a running parent into a child on the parent's host.
- **fork-remote**: a fork of the same kind of parent into a child on the other
  host.

Each start is split into its steps. The largest step of every case except the
cold start is the VMM's state load, and most of that is the pager's populate
of the root disk and of RAM: 170 to 190 ms of a start's 340 to 450 ms. The store
is 23 to 45 % of each case at the median. A fork that wrote nothing to the
store would save about 80 ms on the same host and 107 ms on another host.

## What ran

Two disposable `n2-highmem-4` nodes in us-east4-a, made with
`--min-cpu-platform="Intel Ice Lake"`. Both reported `Intel(R) Xeon(R) CPU @
2.60GHz` with 4 processors. They were one k3s cluster running `deploy/` with
one host pod per node on the node's own network, as
`scripts/lib/app-restore-manifest.py` adapts it, with the cluster cache under
code 1+1 at 100 % share and the isolated arena. The store was the bench's GCS
bucket in the same region. The template was the alpine guest image: 1 GiB of
RAM and a 2 GiB root disk, both in 2 MiB pages, and 2 vCPUs. Firecracker ran
jailed, as `deploy/` runs it.

`scripts/bench-start-gce.sh all` made the nodes, built the image on the first
node, deployed, and ran `scripts/lib/start-run.sh` there. That runs
`cmd/sproutfs-startbench drive` against the two hosts' API, one start at a
time and at least 250 ms apart, so a start does not queue behind the
uploads the previous one left running. It carries a fork as the orchestrator
does: the fork on the parent's host, the receive on the child's host, and the
release on the parent's host. The orchestrator itself is not on the path; its
survey and its table are not measured here. Each child, and each cold-started
VM, is deleted once its agent has answered. The restore VM is suspended again
after each open. Then `sproutfs-startbench store` timed the store's calls
directly, 400 rounds, and `sproutfs-startbench summary` joined every start with
its host's log lines. Every one of the 1,600 starts succeeded.

The revision was efecf23a, with the summary fix of the next commit. Raw
samples and both hosts' whole logs are in `gce-start-latency-2026-10-04/`.

## What is measured

The guest's first instruction is not observable from the host. Each host
times its own steps and logs them as `host: a VM runs`
([hosting](../hosting.md)). Its `running_ms` is when the VMM
reported the vCPUs running:

- for a restore and a fork's child, the end of the resume (`release`);
- for a boot, the VMM's first answer, which Firecracker gives once it has
  built the VM and started its vCPUs.

**To running** is from the driver sending the start's first request to that
moment: the request's send, half of its round trip beyond the host's own
time, and the host's `running_ms`. For a fork the first request is the fork
on the parent's host, so it includes the parent's pause.
**Agent answers** is from the same first request to the first `true` the guest's
agent ran, asked every 5 ms from the start of the request.

## Results

| Case | to running p50 ms | p90 | p99 | max | agent answers p50 | p99 | store p50 | p99 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cold | 476 | 492 | 514 | 565 | 1,461 | 1,515 | 166 | 196 |
| restore, same host | 450 | 738 | 7,337 | 10,453 | 509 | 7,391 | 201 | 7,093 |
| restore, other host | 453 | 1,359 | 8,425 | 10,452 | 517 | 8,489 | 198 | 8,170 |
| fork-local | 342 | 354 | 380 | 473 | 380 | 435 | 78 | 111 |
| fork-remote | 375 | 394 | 439 | 645 | 506 | 598 | 107 | 165 |

The store column is the union of the store calls before the guest ran. For a
fork it adds the parent's confirm to the child host's calls.

The restore's tail is the store's. In 56 of the 400 restores, the epoch's
advance waited 200 ms to 10 s. Without those 56, a restore took 448 ms at the
median and 575 ms at p99. See [the store](#the-store).

### The steps

Milliseconds, p50 and p99. Indented steps are inside the one above them.

**Cold.** Largest: the VMM's start, and inside it the populate. The root
publication is next.

| Step | p50 | p99 | store calls in it |
| --- | --- | --- | --- |
| fork | 53 | 72 | create the child's control record |
| root | 184 | 208 | two conditional writes of the record (113 ms), the index's create (53 ms) |
| VMM start | 238 | 250 | |
| &nbsp;&nbsp;process | 12 | 13 | |
| &nbsp;&nbsp;sessions: attach and populate | 182 | 192 | |
| &nbsp;&nbsp;ready: the VM is built and its vCPUs start | 43 | 47 | |

**Restore.** Largest: the VMM's state load, 228 ms, of which the populate is
172 ms. The same host and the other host differ by under 5 ms at every step.

| Step | p50 | p99 | store calls in it |
| --- | --- | --- | --- |
| open | 142 | 7,037 | two control record reads (52 ms), the epoch's advance (60 ms; p99 6,952 ms), index reads |
| state | 57 | 79 | the index and the part that holds the VMM state (35 ms) |
| VMM start | 240 | 249 | |
| &nbsp;&nbsp;process | 11 | 12 | |
| &nbsp;&nbsp;state load, with attach and populate | 228 | 236 | |
| release | 9 | 12 | |

**Fork on the same host.** Largest: the state load, 242 ms, of which the
populate is 191 ms.

| Step | p50 | p99 | store calls in it |
| --- | --- | --- | --- |
| parent: confirm | 29 | 45 | a read of the parent's record |
| parent: seal, with the pause | 2.9 | 3.5 | |
| parent: pin and handoff | 0.0 | 0.0 | a write of the parent's record in 3 forks of 400 |
| fork's round trip beyond the host | 1.2 | 4.3 | |
| child: open | 50 | 65 | create the child's control record (50 ms) |
| child: VMM start | 254 | 287 | |
| &nbsp;&nbsp;state load, with attach and populate | 242 | 274 | |
| child: release | 3 | 7 | |

**Fork to another host.** Largest: the state load, 226 ms, of which the
populate is 170 ms.

| Step | p50 | p99 | store calls in it |
| --- | --- | --- | --- |
| parent: confirm | 28 | 44 | a read of the parent's record |
| parent: seal, with the pause | 2.9 | 63 | |
| fork's round trip beyond the host | 1.3 | 3.8 | |
| child: open | 79 | 102 | the parent checkpoint's index (29 ms), create the child's record (50 ms) |
| child: VMM start | 238 | 253 | |
| &nbsp;&nbsp;state load, with attach and populate | 226 | 240 | |
| child: release | 25 | 37 | |

The child's release on another host takes 25 ms against 3 ms on the same
host. This run does not show why.

### The populate

Every start populates before the guest runs. The pager maps the pages already
resident under their identity, so the guest takes no fault on them:

| Case | root pages | root ms | RAM pages | RAM ms |
| --- | --- | --- | --- | --- |
| cold | 1,009 | 120 | 512 | 61 |
| restore | 1,009–1,017 | 118 | 460–481 | 54 |
| fork-local | 1,009 | 122 | 472 | 69 |
| fork-remote | 1,017 | 115 | 460 | 55 |

The root's populate is 8 mapping commands and the RAM's 4, so its cost is per
page, about 0.12 ms per 2 MiB page, not per command. The two populates run one
after the other: the sessions phase of a boot, 182 ms, is their sum. The root
disk is the larger, and the alpine guest reads a few of its pages in its first
second: 0 to 5 faults.

### First faults

The second line a host logs, a second after the guest runs, counts each
memory region's guest faults. p50, and p99 in brackets:

| Case | RAM faults before running | RAM faults in the first second | ms waited in them | root faults in the first second |
| --- | --- | --- | --- | --- |
| cold | 4 | 12 (13) | 12 (15) | 5 (5) |
| restore | 5 | 55 (62) | 130–138 (192–209) | 1–3 (4) |
| fork-local | 5 | 34 (54) | 40 (81) | 3 (3) |
| fork-remote | 5 | 45 (50) | 169 (235) | 0 (0) |

The first RAM fault of a restore or a fork comes 170 to 190 ms before the
guest runs, inside the state load: the VMM's restore reads guest memory. It
waits 1 ms on the same host and 7 to 9 ms when the page comes from the store or
the parent's host. Those faults are inside the state load, so its time already
counts them. The agent then answers 40 to 130 ms after the guest
runs. A cold boot's agent answers about a second after its kernel starts,
which is the guest's own boot.

### The store

Measured directly from the first node, 400 rounds of one small object
(512 bytes, a control record's size), one call at a time:

| Call | p50 ms | p90 | p99 | max |
| --- | --- | --- | --- | --- |
| create if absent | 46.5 | 54.9 | 65.7 | 95.0 |
| read | 28.2 | 33.9 | 45.2 | 520 |
| compare-and-set on the ETag | 52.5 | 60.3 | 76.3 | 81.6 |
| delete | 29.8 | 34.8 | 40.7 | 47.5 |

On the hosts' start paths, the conditional writes of the control record took:

| Write | count | p50 ms | p90 | p99 | over 1 s |
| --- | --- | --- | --- | --- | --- |
| a create's (a fresh record) | 403 | 56 | 63 | 69 | 0 |
| a receive's child record (fresh) | 800 | 50 | 58 | 65 | 0 |
| an open's epoch advance | 400 | 59 | 433 | 6,952 | 36 |

A write to a new object behaves as the direct numbers do. The epoch's advance
rewrites the one record the suspend before it wrote about a second earlier,
and GCS limits how often one object may change. 14 % of those writes waited
over 200 ms, 9 % over 1 s, and some reached the 10 s first-byte bound and were
made again. The bench restores one VM every 1.3 s, which is much more often
than a VM is usually stopped and started, so this tail is the bench's cadence
as much as the restore's. It would also hit any VM whose record is written
more than about once a second, for example a suspend and a resume close
together.

The store's share at the median: 166 of 476 ms for a cold start (35 %), 201 of
450 ms for a restore (45 %), 78 of 342 ms for a fork on the same host (23 %)
and 107 of 375 ms for a fork to another host (28 %).

## What a fork that writes nothing to the store would save

A fork's path to the running child makes these store calls:

- the parent's confirm: a read of its control record, 29 ms;
- on another host, a read of the index of the parent's checkpoint, 29 ms;
- the child's control record, created if absent, 50 ms.

The pin of the parent's checkpoint is written once per published checkpoint:
in 3 forks of 400 on the same host and 7 of 400 to another host. The other
forks write nothing to the parent's record.

A fork that touched the store only behind the running child would save 79 ms
of 342 ms on the same host (to about 263 ms) and 107 ms of 375 ms on another
host (to about 268 ms) at the median, and 111 and 165 ms at p99. That is a
quarter of a fork. The child already exists only on its host until its root
publishes, so writing its record behind the child does not change what a host
loss costs it. The confirm is the harder part: it is what stops a fenced host
from handing out pages ([migration](../migration.md#a-fork-is-a-handoff-from-a-parent-that-keeps-running)). It could run beside the
VMM's start rather than before it, and a child would be discarded if it fails.
This is worth doing, so the backlog has TASK-89 for it. The populate is the
larger cost, 170 to 190 ms, and is the first thing to cut.

## Also seen

- **The VMM process step is 11 ms at p50 and p99 in every case.** The host
  looks for the VMM's API socket every 10 ms (`vmmachine`'s `awaitAPI`), so
  most of the step is likely that wait.
- **`kubectl logs` lost most start lines.** The agent's polling logs an error
  on the host for every refused exec, so the container's log rotated every
  few minutes and `kubectl logs` prints only the current file. The bench now
  reads every host's rotated files from its node.
- **A child deleted before its root lands** logs `volume: snapshot publication
  failed` with `managed-memory-region closed`, 10 times in 800 forks here. The
  child is gone, so nothing is lost.
- A deleted VM logs `vmmemory: the memory session failed` at error level, as
  on [2026-10-04](gce-real-app-restore-2026-10-04.md#also-seen): 3,252 times
  here, twice per start.

## What the plan should change

1. **Cut the populate's per-page cost.** It is 0.12 ms per 2 MiB page and
   runs before every start. The root disk alone is 115 to 122 ms for 2 GiB.
   Populating RAM and the root at once, or the root only on its first fault,
   would take most of it off the path. TASK-27 and TASK-28 cover the
   populate and the restore's phases.
2. **A store-free fork** (TASK-89), for a quarter of a fork.
3. **Keep a control record from changing more than about once a second**, or
   expect seconds of tail when it does.

## Cleanup

`scripts/bench-start-gce.sh delete` deleted both nodes and the run's objects
and verified that none remained. Afterwards `gcloud compute instances list`
and `gcloud compute disks list` showed nothing named `sproutfs-startbench`, and
`gcloud storage ls gs://echophase-sproutfs-bench/sproutfs-bench/` showed no
`sproutfs-startbench-` prefix.
