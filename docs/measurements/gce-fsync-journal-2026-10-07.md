# Durable flush through a journal disk on GCE — 2026-10-07

**Question.** What does durable flush cost on GCE, on Hyperdisk Balanced and on
pd-ssd? What do recovery and scaling cost around the journal disks? Which kind
and size should a journal disk be by default? This is step 9 of
[the plan](../../plans/fsync-journal-2026-10-06.md) (TASK-104).

**Answer.** A durable 4 KiB flush took 3.0 ms at p50 and 4.0 to 4.1 ms at p99
in a guest, on either kind. Two thirds of it is the capture, which hashes the
whole 2 MiB page. The disk adds 0.3 to 0.4 ms. The guest's flushes of one
disk never overlapped, so one disk sustained about 330 flushes a second. A
host powered off after a flush lost nothing: its VM ran again on the other
node 86.5 s later, with the flushed block. Almost all of that was the journal
disk's move. The replay, the publication and the boot took 1 s together.

**Default: pd-ssd, 32 GiB.** Its write and sync had the lower p99 at every
size from 4 KiB to 64 KiB. In a guest the two kinds were equal, since the
capture dominates. pd-ssd also attaches to N2 hosts. 32 GiB holds about 140
VMs flushing 4 KiB as fast as they can.

## Setup

- `scripts/bench-fsync-journal-gce.sh` and the scripts it runs, as committed
  with this report, over main at 860ee6f7 with 0b7ecae1, e287c7ea and
  92a70559, in us-east4-a. Every machine was a c3-standard-4 (4 vCPUs,
  16 GB, Intel Sapphire Rapids, with SHA instructions) with nested
  virtualization.
- **Raw disks.** One machine with a 32 GiB Hyperdisk Balanced at 6,000 IOPS
  and 400 MB/s, as the orchestrator makes one, and a 32 GiB pd-ssd.
  `scripts/lib/fsync-fio.sh` writes forwards through 8 GiB, one write in
  flight, for 15 s a job: a buffered write and an fsync of the device, as the
  journal does, and an O_DIRECT and O_DSYNC write. It ran twice. The second
  run writes the span once first, and measures one job on blocks never
  written. Both devices report a volatile write cache and FUA.
- **Cluster.** Two nodes in one k3s cluster, one host pod each, deploy/ as
  `scripts/lib/app-restore-manifest.py` adapts it: a 3 GiB HugeTLB arena,
  half RAM and half PMEM, 8 GiB of memory, the alpine template with 1 GiB of
  RAM, a 60 s checkpoint interval. The orchestrator runs on the first node.
- **In a guest.** `sproutfs-guest-witness flush` (new): each of N threads
  writes 4 KiB into a file of its own and calls fdatasync, for 10 s. The
  files are written whole first, so a flush carries no filesystem metadata.
  `--no-sync` only writes. The guest's disk is PMEM with DAX.
- Each case ran with durable flush off, then on over Hyperdisk Balanced journal
  disks the orchestrator created, then on over a pd-ssd journal disk the bench
  made and labelled. The host's `/metrics` were read before and after each
  case. `sproutfs_journal_written_bytes_total` is new.
- Every machine, disk and object was deleted at the end, and the script
  checked that none remained.

## Raw disks: a write and its sync, one in flight

Microseconds, second run. The first run agreed within 15% at p50 and p99.
Its p99.9 on pd-ssd at 4 and 8 KiB was 5.4 ms.

| disk | write | write + fsync p50 / p99 / p99.9 | O_DSYNC write p50 / p99 / p99.9 | per second, fsync |
| --- | --- | --- | --- | --- |
| Hyperdisk Balanced | 4 KiB, never written | 381 / 928 / 1,844 | – | 2,413 |
| Hyperdisk Balanced | 4 KiB | 377 / 871 / 1,727 | 367 / 823 / 1,778 | 2,480 |
| Hyperdisk Balanced | 8 KiB | 477 / 1,065 / 1,957 | 489 / 1,204 / 2,179 | 1,964 |
| Hyperdisk Balanced | 16 KiB | 533 / 1,182 / 2,197 | 510 / 1,073 / 1,942 | 1,772 |
| Hyperdisk Balanced | 32 KiB | 585 / 1,202 / 2,334 | 578 / 1,253 / 2,023 | 1,616 |
| Hyperdisk Balanced | 64 KiB | 755 / 1,669 / 2,867 | 766 / 1,614 / 2,507 | 1,247 |
| pd-ssd | 4 KiB, never written | 307 / 577 / 1,418 | – | 3,099 |
| pd-ssd | 4 KiB | 303 / 593 / 2,089 | 301 / 518 / 1,028 | 3,121 |
| pd-ssd | 8 KiB | 337 / 645 / 1,320 | 297 / 481 / 1,303 | 2,867 |
| pd-ssd | 16 KiB | 342 / 563 / 981 | 326 / 586 / 1,434 | 2,839 |
| pd-ssd | 32 KiB | 378 / 568 / 1,129 | 367 / 561 / 954 | 2,574 |
| pd-ssd | 64 KiB | 550 / 828 / 1,678 | 537 / 791 / 1,352 | 1,788 |

- pd-ssd is faster at p50 and p99 at every size. At 64 KiB its p99 is half
  of Hyperdisk Balanced's.
- At p99.9 neither wins: pd-ssd is slower at 4 KiB in both runs, faster from
  16 KiB up.
- The buffered write with fsync costs the same as an O_DSYNC write. A block
  never written costs no more than a rewrite.
- A sync is 0.3 to 0.4 ms. The plan's estimate of 0.6 ms took a write at 16
  deep; one in flight is faster.

## Flushes in a guest

Each case ran 10 s. Latency is of one write with its flush, in microseconds.

| durable flush | threads | flushes | MiB/s | p50 / p99 / p99.9 | PMEM protect traps a second | capture mean ms |
| --- | --- | --- | --- | --- | --- | --- |
| off | 1 | 73,846 | 28.8 | 130 / 198 / 303 | 0 | – |
| off | 8 | 72,326 | 28.2 | 1,308 / 2,598 / 6,180 | 0 | – |
| off | 64 | 70,011 | 27.3 | 11,527 / 20,420 / 48,103 | 0 | – |
| on, Hyperdisk Balanced | 1 | 3,141 | 1.23 | 3,033 / 4,305 / 43,368 | 314 | 1.98 |
| on, Hyperdisk Balanced | 8 | 2,930 | 1.14 | 27,393 / 34,145 / 36,466 | 568 | 2.58 |
| on, Hyperdisk Balanced | 64 | 2,958 | 1.14 | 227,308 / 247,505 / 255,605 | 569 | 2.55 |
| on, pd-ssd | 1 | 3,252 | 1.27 | 2,984 / 5,315 / 14,255 | 326 | 1.98 |
| on, pd-ssd | 8 | 2,948 | 1.15 | 27,302 / 34,789 / 37,397 | 569 | 2.59 |
| on, pd-ssd | 64 | 2,910 | 1.12 | 225,337 / 354,563 / 375,149 | 555 | 2.56 |

One thread flushing for longer, across checkpoint intervals:

| journal disk | seconds | flushes | p50 / p99 / p99.9 / max µs | journal bytes per guest byte | journal MiB/s | live MiB, peak |
| --- | --- | --- | --- | --- | --- | --- |
| Hyperdisk Balanced | 150 | 48,915 | 3,017 / 4,032 / 5,483 / 198,900 | 2.02 | 2.58 | 154 |
| pd-ssd | 75 | 24,558 | 3,004 / 4,093 / 5,785 / 9,340 | 2.05 | 2.62 | 172 |

- **A durable flush is 23 times slower than today's.** 3.0 ms against
  0.13 ms at p50 for one thread.
- **The capture is most of it.** Its mean was 1.98 ms. SHA-256 ran at
  1.38 GB/s on 4 KiB and 1.45 GB/s on 2 MiB here, so hashing the 512 blocks
  of a 2 MiB page takes about 1.45 ms. The write and sync add 0.3 to 0.4 ms,
  and the guest's own round trip 0.13 ms.
- **The disk's kind does not show in the guest.** The long runs agree within
  2% at p50 and p99.
- **One disk's flushes do not overlap.** With durable flush off or on, 1, 8
  and 64 threads made the same number of flushes, and each thread's latency
  grew with the count. So one disk sustains one flush per latency: about 7,000
  a second off, about 330 on. The serialization is below the journal: it is
  there with the journal off too. Where (guest driver, VMM or pager) was not
  measured. The journal's group commit can only batch flushes of different
  disks.
- **Writes without a flush cost nothing.** 4.1 GB/s for one thread with the
  mode off or on. Protect traps came about once a flush (314 to 328 a second
  for one thread), and almost never without one.
- **Each 4 KiB block costs 8 KiB of journal.** An entry of one block is its
  header, names, block number and checksum beside the 4 KiB, so the batch is
  padded to 8 KiB. A batch of more blocks would cost less per block. The
  short cases' bytes include each file written whole before the clock, so
  only the long runs give this ratio.
- **Live bytes follow the interval.** They rose about 2.6 MiB/s and fell to
  near zero at each selection, every 60 s, to a peak of 154 to 172 MiB.
- The 10 s cases' p99.9 varied: 43 ms on Hyperdisk Balanced, 14 ms on
  pd-ssd. Over the long runs it was 5.5 to 5.8 ms on both. The long run on
  Hyperdisk Balanced had one flush of 199 ms; why was not measured.

## A host powered off after a flush

The guest wrote 4 KiB of random bytes with `dd conv=fsync` on the second node.
The node was powered off at once through sysrq. The bench then deleted its
Kubernetes node and pod, and asked for a recovery until one succeeded. The VM
ran again on the first node with the block intact.

| from power-off | step |
| --- | --- |
| 0 s | the second node powered off |
| 2.5 s | its node and pod deleted, as an operator would |
| 6.9 s | the orchestrator calls detach |
| 22.9 s | detach done: 16.0 s, from a machine that was off |
| 47.0 s | attach to the first node called, after five membership steps, 5 s apart |
| 50.2 s | attach done: 3.1 s |
| 76.5 s | the first host opens the disk for reading |
| 85.5 s | the recovery that succeeded begins |
| 85.8 s | replay done: 14,335 entries, 14,371 blocks, 292 ms with the open |
| 86.4 s | the cold boot's checkpoint published: 535 ms |
| 86.5 s | the VM runs |

- **The disk's move is 85 s of the 86.5.** The cloud's calls are 19 s of it.
- **The host learned of its assignment 26 s late.** A host reads the
  membership every 30 s unless a peer prompts it (`membership/view.go:18`).
  It opened the disk at the next read.
- **The membership takes one step a pass, every 5 s.** Dropping the lost
  member, releasing its disk and assigning it for reading took five passes
  between the detach and the attach.
- **A recovery asked too early failed instead of waiting.** The first one, at
  7.7 s, found the disk still served by the lost member, dialled it, and failed
  with "no connection and hello in time". It did not return
  `ErrJournalPending`, so the orchestrator did not retry it. The bench did.
- The recovered VM's checkpoint dropped the journal from its record. The
  first host then found the disk empty and released it, and it was detached
  2 min 17 s after the power-off.

## Scale-up and scale-down

| call | took |
| --- | --- |
| create a journal disk | 0.7 s, twice |
| attach a journal disk to a running node | 2.9 to 4.2 s, six times |
| detach from a running node | 2.8 to 3.7 s, four times |
| detach from a node powered off | 16.0 s, once |

- **Scale-up reused a disk.** A third node joined. The lost node's disk was
  free and empty by then, so the orchestrator reserved it and created none.
  The orchestrator sees a machine only once a host pod is listed on it. The
  attach began 14 s after the pod was scheduled and took 2.9 s. The host
  served the disk 17 s after that, again at its next read of the membership.
  The node itself took 9 s to create and 54 s to join the cluster.
- **Scale-down did not wait for the journal.** The node's host pod was deleted.
  Its drain moved the VM in 3.4 s and the pod was gone 6.7 s after the delete.
  The detach began 4.8 s later and took 3.2 s. The disk still held live
  entries: 21 s later a journal disk was attached to the other node, which
  the membership does only to read one. The wait
  for the journal to empty is not on main (65693683 is on the
  `journal-replay` branch), so it was not measured.
- During that drain the destination's post-copy found the source gone and
  read the rest of the VM from the log. The VM ran on.

## What it means for the journal disk

- **Kind: pd-ssd.** The plan sets the default by the measured p99. pd-ssd's is
  lower at every write size, by a third at 4 KiB and half at 64 KiB. It
  attaches to N2 hosts too. Its p99.9 at 4 KiB was worse in both runs, by 0.4
  and 3.3 ms. In a guest the kinds were equal. Hyperdisk Balanced's higher
  provisioned throughput, 400 MiB/s against the 240 MiB/s a persistent disk
  read at most on a C3 in the shard measurement, matters only to flushes of large writes, which this run did
  not measure. The orchestrator creates Hyperdisk Balanced now:
  `cmd/sproutfs-orchestrator/main.go:226` passes an empty
  `GCENetworkDisksConfig`. A journal adapter with `DiskType: "pd-ssd"` and no
  provisioning would change it.
- **Size: 32 GiB.** A VM flushing 4 KiB as fast as one disk allows writes
  2.6 MiB/s of journal and holds at most 172 MiB. The three-quarter mark of
  32 GiB, 24 GiB, holds about 140 of those. A guest that writes large blocks
  and flushes could reach the machine's limit; the plan's 24 GiB for 60 s at
  400 MiB/s still holds for that.
- **The capture, not the disk, sets the latency.** Hashing the page is about
  1.45 ms of each 3 ms flush.

## What would make recovery and scaling faster

Not built; each is a change the numbers point at.

- The journal disk agent reads the membership at once when the orchestrator
  assigns it a disk, or every second while a disk is assigned and not open:
  26 s off a recovery and 17 s off a scale-up.
- The membership takes the steps of a lost member's journal disk in one pass:
  about 20 s off a recovery.
- The orchestrator treats a holder that does not answer `JOURNAL_READ` as
  `ErrJournalPending` and retries.
- The drain's wait for the journal to empty (65693683) reaches main.

## The capture against real userfaultfd

`TestManagedPagerCaptureProtectsAndTrapsOnUFFD` ran as root on the raw
machine, with the memory crate's client built there, twice. The 2 MiB case
passes. The 4 KiB case fails before the capture runs:

```
=== RUN   TestManagedPagerCaptureProtectsAndTrapsOnUFFD
=== RUN   TestManagedPagerCaptureProtectsAndTrapsOnUFFD/page-4096
    kernel_linux_test.go:483: invalid managed-memory-region
=== RUN   TestManagedPagerCaptureProtectsAndTrapsOnUFFD/page-2097152
--- FAIL: TestManagedPagerCaptureProtectsAndTrapsOnUFFD (0.03s)
    --- FAIL: TestManagedPagerCaptureProtectsAndTrapsOnUFFD/page-4096 (0.01s)
    --- PASS: TestManagedPagerCaptureProtectsAndTrapsOnUFFD/page-2097152 (0.03s)
FAIL
```

The pager refuses the client's memory region at attach
(`vmmemory/connection_linux.go:211-212`). `startNative` without backings
makes each one `pages × 2 MiB` (`vmmemory/kernel_linux_test.go:550`). The
client announces `pages × page`, 8 KiB at 4 KiB pages
(`rust/sproutfs-vm-memory/examples/support/client_linux.rs:53`). The lengths
differ, so the 4 KiB case never reaches its capture. The test, not the
capture, needs the fix.

## Not measured

- Flushes of writes larger than 4 KiB, and many VMs flushing at once on one
  host.
- Where one disk's flushes serialize.
- The scale-down's wait for the journal to empty: not on main.
- A disk create during scale-up: the run reused a free disk. The creates at
  deploy took 0.7 s.

The raw results were kept outside the repository.
