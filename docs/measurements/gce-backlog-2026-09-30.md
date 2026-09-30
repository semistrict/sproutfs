# GCE runs for TASK-64, TASK-66 and TASK-68, 2026-09-30

One disposable `n2-standard-8` host, `sproutfs-memprobe-backlog-0930`, in
us-east4-a, with nested virtualization. It was deleted afterwards, with its
boot disk. The source was the working tree at 4abc69e8 and the fixes below.
Raw results are in `gce-backlog-2026-09-30/`.

## The VMM's API revision (TASK-68)

The fork builds for x86_64 musl with `--sproutfs-api-revision`.
`TestPulledGuestsFaultWithoutTheObjectStore` starts a supervisor against the
real binary, so `host.Start` asks it for its revision.

The first run failed. Firecracker prints the revision and then logs its own
exit, and the host parsed both lines as one number. The host now reads the
first line. The rerun passes (`qualify-start/`).

## The hostile suite with a public file (TASK-66)

Every VMM of an isolated arena now holds the public file as file 2. The
real-kernel hostile tests ran in both arena modes (`hostile/`).

The first run failed on one assertion: a hostile VMM held files 0, 1 and 2,
and the test expected 0 and 1. That is the design, so the test now expects
the public file too. It still maps every file the VMM holds and finds no other
VM's bytes in any of them. All seven tests pass in both modes.

## A fork child's first pass (TASK-64)

The fan-out (`SPROUTFS_GCE_FANOUT=1`) forks four children from one checkpoint
of a 16 GiB guest. Each child runs one command. `ran-before` runs a binary the
parent ran; `first-run` runs `memprobe 16`, which no parent ran.

| RAM page | Arena | Command | RAM faults per child | Mean fault | Fault time per child |
| --- | --- | --- | ---: | ---: | ---: |
| 2 MiB | isolated | ran-before | 48 | 3.41 ms | 0.16 s |
| 2 MiB | isolated | first-run | 60 | 3.47 ms | 0.21 s |
| 4 KiB | isolated | ran-before | 693 | 1.45 ms | 1.01 s |
| 4 KiB | isolated | first-run | 770 | 1.57 ms | 1.21 s |
| 4 KiB | shared | ran-before | 728 | 0.91 ms | 0.66 s |
| 4 KiB | shared | first-run | 833 | 0.94 ms | 0.78 s |

Fault time per child is the sum of that child's fault times. Concurrent
faults overlap, so the guest's stall is at most that.

The 13,226 faults of 2026-09-23 came from the codex workload at 4 KiB, which
touches far more memory than one small command. These counts cannot be
compared with it directly. What they show:

- At 4 KiB a child still faults hundreds of times on a trivial first pass,
  after its attach has populated about 8,000 RAM pages from its siblings.
- A fault in the isolated arena costs about 60% more than in the shared arena
  (1.5 ms against 0.9 ms). That is TASK-52's question.
- At 2 MiB the count falls by more than ten times, and the total by five.

Neither lever can be judged from this workload alone. The codex fan-out at
4 KiB is the run that decides it.
