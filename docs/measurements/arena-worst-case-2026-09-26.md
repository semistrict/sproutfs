# The isolated arena's worst cases on GCE — 2026-09-26

This is the second run of TASK-2.6. [The first run](arena-modes-2026-09-26.md)
measured a fan-out whose pages were already published and shared, so it never
paid for what the isolated arena copies. This run measures those copies. The
default is still `shared`.

## How it was measured

`scripts/demo-gce.sh arena-worst` runs `scripts/lib/demo-arena-worst.sh` on the
node. One run measures both modes at a 2 MiB RAM page, and then at 4 KiB. It
took 18 minutes and 41 seconds, from a created cluster to the deployment put
back.

For each mode and page the script restarts the host pods, so both pagers start
empty. It turns the interval checkpoint, the loss window and the flush bound
off, so only the run's own captures and forks publish anything. At 4 KiB the
hosts use their own object prefix, because a template keeps the page it was
imported at. Every guest checks what it reads against what its writer read.

The cluster is the demo's: one `n2-standard-8`, two host pods, a 3.75 GiB RAM
arena and a 1.25 GiB PMEM arena per host. A host admits guests whose RAM adds up
to its RAM arena, so the sizes are:

1. **Fork of unpublished pages.** A 512 MiB parent writes 357 MiB of random
   bytes, and with no checkpoint forks three children on its own host. Each
   child reads all 357 MiB.
2. **First inheritance.** A 1 GiB parent writes 786 MiB and is captured. It
   forks two children on its own host, and each child reads all of it. Then
   the parent reads it again.
3. **Checkpoint of an all-dirty guest.** A 3.5 GiB guest, the largest a host
   admits, writes 3188 MiB and is captured. The capture holds 3.4 GiB of dirty
   pages.
4. **Restore back.** A 2 GiB guest writes 1 GiB and is stopped with its memory.
   It is started on the other host, stopped again, and started back on its own
   host, where it reads the 1 GiB. This is the first run's shape.

Cases 1, 2 and 4 ran three times per mode at 2 MiB. Case 3 ran once per mode,
and at 4 KiB each case ran once. Each cell is the median, then the minimum and
maximum. First output includes about half a second of `kubectl exec`. CPU is
the host process's user and system time.

The run also needed two changes, committed before it: the host status now
reports moved pages, fork copies, revocations, loaded pages and copy-on-writes,
and a deadlock the run found is fixed (see below).

## Results at 2 MiB

| | shared | isolated |
| --- | --- | --- |
| 1. fork pause | 0.100 (0.091–0.107) s | 0.090 (0.088–0.109) s |
| 1. fork total, three children | 10.71 (10.38–10.81) s | 11.43 (11.24–13.59) s |
| 1. first output, slowest child | 11.49 (11.47–11.57) s | 12.96 (12.32–14.70) s |
| 1. every child reads 357 MiB, slowest | 6.09 (6.04–6.18) s | 7.25 (7.09–7.35) s |
| 1. fork copies | 0 | 236 (232–236) |
| 1. host CPU, fork | 14.4 (14.2–14.8) s | 15.8 (15.5–15.8) s |
| 1. uploaded during the fork | 1114 MiB | 1114 MiB |
| 1. read from the store by the children | 1047 MiB | 1043 MiB |
| 1. saved memory after the reads | 0 | 0 |
| 2. capture upload | 5.26 (5.11–5.54) s | 5.73 (5.71–5.89) s |
| 2. fork total, two children | 1.70 (1.68–2.01) s | 2.50 (2.47–2.54) s |
| 2. first output, slowest child | 2.47 (2.11–2.51) s | 3.23 (3.03–3.27) s |
| 2. every child reads 786 MiB, slowest | 1.77 (1.64–2.70) s | 1.62 (1.59–1.75) s |
| 2. moved pages | 0 | 428 (426–429) |
| 2. revoked pages | 21 (16–24) | 450 (445–451) |
| 2. host CPU, fork and reads | 1.02 (0.96–1.22) s | 2.34 (2.26–2.37) s |
| 2. owner reads 786 MiB again | 1.58 (1.56–1.64) s | 2.06 (2.04–2.22) s |
| 2. owner's faults in that read | 17 (15–18) | 410 (409–412) |
| 2. owner's copy-on-writes in that read | 11 (9–12) | 403 (402–403) |
| 2. saved memory at the end | 1248 (1224–1444) MiB | 812 (670–834) MiB |
| 3. capture pause | 0.008 s | 0.009 s |
| 3. capture upload, 3.4 GiB | 16.65 s | 18.67 s |
| 3. host CPU per capture | 32.9 s | 36.0 s |
| 4. start away | 0.751 (0.521–0.839) s | 0.748 (0.544–0.818) s |
| 4. start back | 0.756 (0.702–0.782) s | 1.652 (1.647–1.672) s |
| 4. start back to first output | 2.00 (1.90–2.14) s | 2.93 (2.78–3.08) s |
| 4. read 1 GiB back | 2.24 (2.22–2.31) s | 1.99 (1.99–2.18) s |
| 4. pages mapped by identity at start | 16 (16–23) | 549 (547–550) |
| 4. moved pages at start | 0 | 519 (517–550) |
| 4. host CPU at start | 0.32 (0.29–0.32) s | 1.84 (1.84–1.92) s |

## Results at 4 KiB, one run each

| | shared | isolated |
| --- | --- | --- |
| 1. fork total | 19.6 s | 33.9 s |
| 1. first output, slowest child | 20.6 s | 35.2 s |
| 1. every child reads 358 MiB, slowest | 10.2 s | 10.6 s |
| 1. fork copies | 0 | 18,557 |
| 1. host CPU, fork | 24.6 s | 47.5 s |
| 2. fork total | 2.06 s | 5.31 s |
| 2. every child reads 791 MiB, slowest | 6.5 s | 35.2 s |
| 2. moved pages | 0 | 224,316 |
| 2. revoked pages | 71 | 224,364 |
| 2. host CPU, fork and reads | 3.8 s | 38.0 s |
| 2. owner reads 791 MiB again | 6.2 s | 20.9 s |
| 2. owner's copy-on-writes in that read | 3,556 | 104,529 |
| 2. host pod memory at the end, of 8 GiB | 5005 MiB | 7985 MiB |
| 3. capture pause | 0.83 s | 0.74 s |
| 3. capture upload, 3.4 GiB | 30.6 s | 65.7 s |
| 3. host CPU per capture | 67.1 s | 138.2 s |

## Where the isolated arena is worst

1. **First inheritance at 4 KiB.** The children take 5.4 times as long to read
   their memory, 35.2 s against 6.5 s, and the host spends ten times the CPU.
   Every page the children inherit is moved: copied, hashed and taken from the
   owner. `move` handles one page, and its `rebind` sends the owner one
   revocation per page, so 224,316 moves are 224,364 revoked pages, each a round
   trip to the owner's VMM.
2. **Checkpoint of an all-dirty guest at 4 KiB.** The upload takes 2.15 times as
   long and twice the CPU: 138 s against 67 s for 3.4 GiB. At 2 MiB the same
   capture costs 12% more time and 9% more CPU. The BLAKE3 digest of 880,000
   pages of 4 KiB does not explain 71 s of CPU. This run did not find the
   cause. It needs a CPU profile of the host during that capture.
3. **Fork of unpublished pages at 4 KiB.** The fork takes 1.7 times as long,
   33.9 s against 19.6 s, and twice the CPU. It copied only 18,557 pages,
   72 MiB, into the fork file: the populate's budget of 16,384 pages and a few
   faults. That copy does not explain 23 s of CPU either. This also needs a
   profile.
4. **Restore back at 2 MiB.** The start takes 2.2 times as long, 1.65 s against
   0.76 s, in all three runs and in the earlier five. The gap is real.
5. **First inheritance at 2 MiB.** The fork takes 0.8 s longer for 428 moves,
   and the owner's second read takes 0.5 s longer.

## Why the restore back is slower

A start's populate maps runs of pages whose slots are consecutive, before the
guest runs. In the isolated arena the guest's published pages are still in its
old private file, each at its own index, so they form long runs. The populate
maps 549 pages, and each is in another memory region's private file, so each is
moved: copied into the shared file and hashed. That is 519 moves and 1.5 s more
host CPU at start. In the shared arena the same pages sit in scattered slots, so
the populate maps 16 of them and the rest fault in during the read. The read
back is then 0.25 s faster in the isolated arena, because its pages are already
mapped. From the start to the end of the read, the isolated arena is about
0.7 s slower.

## Surprising, and not fixed

- **The owner copies every page that moved.** After a move, the owner's next
  access to the page is a store fault, not a read fault: 403 copy-on-writes for
  428 moved pages at 2 MiB, and 104,529 at 4 KiB. So the owner ends with a
  private copy of each page as well as the shared one, and the move saves no
  memory. The saved memory at the end is 812 MiB against 1248 MiB in the shared
  arena. At 4 KiB the host pod reached 7985 MiB of its 8 GiB. The guest only
  reads here, and the shared arena's owner takes 11 copy-on-writes in the same
  read. This run did not find why the refault arrives as a store.
- **A local fork of unpublished pages shares nothing, in both modes.** Each
  child uploads the pages it inherited as its own first checkpoint, inside the
  fork: 1114 MiB for three children of 357 MiB. When the parent's seal ends,
  every child's mapping of those pages is taken away (`dropSharers`), and each
  child reads them back from the store: 1047 MiB. The saved memory after the
  reads is zero. This is how `ForkPoint` and `dropSharers` describe it, and it
  is the same in both arenas, but it is the most expensive case measured here.

## Fixed on the way

A host stopped answering in the first 4 KiB attempt. Its pager was deadlocked.
`eachBinding` took a memory region's binding map lock and then the host lock,
and a store applying the rules (`joinsRun`, `placedRun`) takes them the other
way round. A status request reached `eachBinding` through `PrivateBytes` while a
guest stored, and every fault, verification and status request of that pager
waited. The kubelet killed the pod for its liveness probe. This is not specific
to either arena. The scan now takes the host lock first, and
`TestAScanOfTheBindingsWaitingForTheHostLockHoldsNoBindingLock` holds it.

## What this does not show

- The demo runs its VMMs as root, so the isolated arena protects nothing here.
  These are its costs, not a deployment that gets its protection.
- Cases 3 and every 4 KiB case ran once. Two earlier runs of the first script
  version, five repetitions each at 2 MiB with slightly larger guests, agree
  with the table: restore back 0.76 (0.75–0.80) s against 1.69 (1.61–1.71) s, and
  a 3.2 GiB capture upload 16.5 (16.3–17.0) s against 19.1 (18.6–19.3) s.

The raw files are under `.workload-runs/arena-worst-20260926T220651Z` on the
machine that ran them.
