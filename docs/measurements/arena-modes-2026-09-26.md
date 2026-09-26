# The shared and the isolated arena on GCE — 2026-09-26

This is TASK-2.6, step 6 of [the isolated arena plan](../../plans/isolated-arena-2026-09-25.md):
the same measurements on the demo cluster in each arena mode. The owner decides
the default from these numbers. The default is still `shared`.

## How it was measured

`scripts/demo-gce.sh arena` runs `scripts/lib/demo-arena.sh` on the node. It
measures whichever mode the hosts run. The hosts were restarted before each
run, so each run had fresh pagers.

```sh
scripts/demo-gce.sh kubectl rollout restart -n sproutfs deployment/sproutfs-host
scripts/demo-gce.sh arena
SPROUTFS_DEMO_ARENA=isolated scripts/demo-gce.sh redeploy
scripts/demo-gce.sh kubectl rollout restart -n sproutfs deployment/sproutfs-host
scripts/demo-gce.sh arena
```

The cluster is the demo's: one `n2-standard-8`, two host pods, a 3.75 GiB RAM
arena and a 1.25 GiB PMEM arena per host, both of 2 MiB pages, and GCS in the
same region. The run does three things:

1. **A fan-out.** A 512 MiB guest writes 256 MiB of random bytes into a tmpfs,
   is captured, and writes 64 MiB more that no checkpoint holds. It forks five
   children on its own host. Every child then reads all 320 MiB. "First
   output" is the time from the fork request to the slowest child's first
   answered command. It includes about half a second of `kubectl exec`.
2. **Checkpoints.** A 2 GiB guest on the other host writes 1 GiB of random
   bytes and is captured, three times. The CPU is the host process's user and
   system time between the capture's start and its end.
3. **Restores.** That guest is stopped with its memory and started on the other
   host ("away"), and then back on its own host ("back"). Each time it reads
   the 1 GiB back.

Mappings are the lines of `/proc/<pid>/maps` of each Firecracker process on
the host.

Each figure is one run. Where a step was repeated, both columns show the
spread.

## Results

| | shared | isolated |
| --- | --- | --- |
| fan-out pause, five children | 0.111 s | 0.116 s |
| fan-out total | 9.23 s | 10.24 s |
| fan-out first output, slowest child | 9.99 s | 10.93 s |
| every child reads 320 MiB, slowest | 2.92 s | 2.60 s |
| fan-out host, RAM unique / mapped / saved | 1182 / 2488 / 1470 MiB | 1176 / 2488 / 1466 MiB |
| fan-out host, PMEM unique / mapped / saved | 118 / 140 / 62 MiB | 114 / 140 / 66 MiB |
| mappings per VMM on the fan-out host | 118 mean, 131 most | 112 mean, 116 most |
| 1 GiB capture pause | 5–8 ms | 5–8 ms |
| 1 GiB capture upload | 6.57–7.98 s, mean 7.07 s | 7.06–8.60 s, mean 7.63 s |
| host CPU per 1 GiB capture | 11.55–12.33 s, mean 11.86 s | 12.65–13.11 s, mean 12.84 s |
| restore away: start, first output | 0.77 s, 2.11 s | 0.82 s, 2.51 s |
| restore away: read 1 GiB back | 14.5 s | 14.7 s |
| restore away: mappings of the VMM | 371 | 373 |
| restore back: start, first output | 0.76 s, 1.39 s | 1.64 s, 2.39 s |
| restore back: read 1 GiB back | 2.14 s | 2.11 s |
| restore back: mappings of the VMM | 153 | 118 |

Each capture uploaded 1039 to 1042 MiB in 19 objects in both modes.

## What the numbers say

- The fan-out saves the same memory in both modes: about 1.5 GiB on the host
  that ran six guests. The isolated arena's fork file and its moves did not
  cost resident memory that this reading can see.
- A capture costs the isolated arena about 8% more host CPU and 8% more upload
  time. That is the BLAKE3 digest of every page it uploads. The pause does not
  change.
- The fan-out took about a second longer in the isolated arena, with one run
  each. Each child took about 1.8 s to receive, one after the other, in both
  modes.
- Mappings per VMM are about the same. At 2 MiB pages the plan expected no
  change, because HugeTLB mappings never merge. The isolated arena's most was
  lower here.
- The restores are close, apart from one "back" start of 1.64 s against
  0.76 s. One run is not enough to tell that from noise.

## What this does not show

- The RAM pager runs 2 MiB pages here. The plan's 16 GiB capture at 4 KiB,
  where the digest is 512 times as many calls, was not run: the demo node has
  no room for it.
- The demo runs its VMMs as root. The isolated arena protects nothing unless the
  VMM runs as another user, and the host does not refuse an unjailed VMM yet.
  So these are the isolated arena's costs, not a deployment that gets its
  protection.
- Both modes passed the demo's five flows and the feature checks on the
  cluster. [The GCE run](gce-2026-09-26.md) has those.

The raw files are under `.workload-runs/arena-20260926T180959Z` (shared) and
`.workload-runs/arena-20260926T190831Z` (isolated) on the machine that ran them.
