# Publishing a guest's memory to Cloud Storage on GCE, 2026-10-04

[The real application's restore](gce-real-app-restore-2026-10-03.md) found
the suspend slow. `stop --suspend` of a Valkey guest with a 4 GiB heap
published 2.8 GB at about 50 MB/s, in 57 s, and the source host used 1.3 of
its 4 processors. That is longer than the restore and the walk together. This
report finds why and measures the fix.

## The cause

A publication read, hashed and compressed every page on one goroutine:
`writeEdits` called `partWriter.add`, which encoded the page into the part's
body before the next page was read. Encoding a page is its SHA-256 and its
Zstandard. That is nearly all the processor a publication spends, so a
publication ran at one processor's pace. The host's pool of encoders had two
on these hosts, but one publication never used more than one. Uploads were
already concurrent, up to eight at once, but parts came too slowly to fill
more than one slot on average.

The simulation shows it deterministically. `sim.Config.Compute`
prices an encode in simulated time while it holds its encoder. A publication
of 64 pages at 10 ms each through four encoders encoded one page at a time,
the most at once was 1, and it took 640 ms of encoding.

The profile on GCE shows the same, below.

## What ran

One disposable `n2-highmem-4` in us-east4-a (4 vCPUs, which are two cores
with two threads each, 32 GB, Intel Cascade Lake, no SHA instructions), and
the bench's GCS bucket in the same region.

`cmd/sproutfs-publishbench` holds a 4 GiB guest in memory, 2,048 pages of
2 MiB, as a pager's arena does. Its pages compress to 46 % of their size, as
the Valkey heap did (6.09 GB to about 2.8 GB): each 64-byte line is 28 bytes
of noise and a text record that repeats across 16 lines. It publishes every
page through the real checkpoint store and the real Cloud Storage adapter,
with eight upload slots and two part builders, as a host on this machine
sizes them. It times the commit, the process's CPU, each PUT and the PUTs in
flight, and takes a CPU profile of each round. Every object is deleted after
each round.

`scripts/bench-publish-gce.sh` ran four cases a round, three rounds, in an
order that alternates by round:

- **before**: main at 6a01a213, with the host's two encoders.
- **before-e4**: the same with four encoders.
- **after**: this branch at dcc4674a, with the host's two encoders.
- **after-e4**: the same with four encoders.

Raw results and profiles are in `gce-publication-throughput-2026-10-04/`.

## Results

Each publication uploaded 1,989 MB in 30 parts and one index object. The
median over three rounds, with the range:

| Case | seconds | uploaded MB/s | raw MB/s | CPUs used | PUTs in flight, mean | PUT MB/s while running |
| --- | --- | --- | --- | --- | --- | --- |
| before | 32.3 (31.8–32.3) | 61.6 (61.5–62.6) | 133 | 1.27 (1.25–1.27) | 0.78 | 80.7 |
| before-e4 | 31.7 (31.7–31.8) | 62.7 (62.5–62.8) | 136 | 1.27 (1.26–1.28) | 0.78 | 80.7 |
| after | 20.4 (20.1–20.6) | 97.4 (96.6–98.9) | 210 | 2.49 (2.49–2.51) | 1.16 | 83.1 |
| after-e4 | 17.7 (17.4–17.8) | 112.6 (111.7–114.0) | 243 | 3.55 (3.53–3.58) | 2.27 | 49.3 |

| Case | round 0 | round 1 | round 2 |
| --- | --- | --- | --- |
| before | 61.5 | 62.6 | 61.6 |
| before-e4 | 62.5 | 62.7 | 62.8 |
| after | 96.6 | 97.4 | 98.9 |
| after-e4 | 111.7 | 114.0 | 112.6 |

Uploaded MB/s, by round.

### Where the CPU went

From the CPU profile of round 1 of each case, in seconds of CPU over the
commit:

| | before | after | after-e4 |
| --- | --- | --- | --- |
| commit | 31.8 s | 20.4 s | 17.5 s |
| CPU, all | 40.1 | 51.1 | 62.4 |
| on the publication's own goroutine (`writeEdits`) | 30.9 | 2.4 | 2.4 |
| encoding: `AppendEncode` | 27.0 | 37.4 | 48.0 |
| of which zstd | 15.3 | 21.0 | 28.4 |
| of which SHA-256 | 11.7 | 16.3 | 19.6 |
| part bodies grown by doubling (`growslice`) | 3.3 | under 0.3 | under 0.3 |
| SHA-256 of each part for its digest attribute | 5.5 | 7.6 | 8.6 |
| HTTP/2 and TLS of the PUTs | 2.0 | 2.3 | 2.3 |
| reading the pages | 0.5 | 0.9 | 0.8 |

## What it shows

**The publication was one goroutine at full speed.** Before the fix,
`writeEdits` used 30.9 s of CPU in a 31.8 s commit. The rest of the process
used 9 s, mostly the SHA-256 of each part on the upload goroutines. Four
encoders instead of two changed nothing: 62.7 MB/s against 61.6. The uploads
were not the limit: one PUT moved about 80 MB/s while it ran, and on average
less than one was in flight.

**The fix publishes 1.58 times as fast with the same encoders.** The pages are
now encoded side by side, as many at once as the store has encoders.
The publication's own goroutine reads the pages and fills the parts, and
uses 2.4 s of CPU for 2,048 pages. With the host's two encoders, a 4 GiB
guest takes 20.4 s instead of 32.3 s, at 97 MB/s. With four, 17.7 s and
113 MB/s.

**It is not twice as fast because two threads share a core.** An n2 vCPU is
one thread of a core. The two encoders ran on the two threads of one core
part of the time, and each encode then took longer: about 18 ms of CPU a page
against 13 ms when one encoder ran alone. At four encoders it was 23 ms. The
host is out of processor at four: it used 3.55 of 4, and each PUT then moved
less, 49 MB/s while it ran, because the uploads' own goroutines waited for a
processor.

**SHA-256 is now the largest single cost.** On these CPUs it is 47 % of all
the CPU the publication spends: 32 % checks each page's envelope and 15 %
gives each part its digest attribute. zstd at its fastest level is 41 %.

**Doubling a part's body as it grew cost 8 %.** A part's body started small and
was copied each time it doubled, about 3.3 s of 40. A part's body is now made
room for once.

## Decisions

- **The host keeps two encoders on four vCPUs.** Four would publish 16 %
  faster, at the cost of every processor on the host while a publication
  runs. A guest and the fault path's decodes need them. A suspend publishes
  while the guest it suspends is running again, and the host may run other
  guests. The pool stays half the vCPUs, as before, and now a publication
  uses all of it.
- **A publication has one more batch encoding than there are encoders.** The
  extra batch is ready when an encoder finishes. More would hold more pages
  in memory for no gain: the simulation keeps every encoder busy with one
  more.
- **Pages are still read in order on one goroutine.** Reading a page took
  0.4 ms of the 13 to 18 ms an encode took. Reading on the encoders'
  goroutines would make the source's reads concurrent, and a simulated
  source's reads of the store would then reach it in an order the seed does
  not choose.

## What it does not show

- The real application's suspend was not run again. It needs a cluster of
  two nodes with nested virtualization and a guest image built on them. The
  bench reproduces the publication's part of it: 62 MB/s and 1.27 CPUs
  against the suspend's 50 MB/s and 1.3. The suspend also reads each page
  out of the pager, which in the isolated arena hashes it with BLAKE3, and
  settles the seal first. Those run on the publication's goroutine and are
  outside this bench.
- Machines with SHA instructions. The encode cost there is far smaller, and
  the uploads may become the limit.
- Two publications at once, which share the encoders and the upload slots.

## What the plan should change

1. **Make SHA-256 cheaper or rarer.** It is the largest cost of a publication
   on these CPUs, as it was of a cluster read
   ([the real application's restore](gce-real-app-restore-2026-10-03.md)).
   The digest attribute of each part could be computed once from bytes the
   publication already hashes, and a faster digest in the next envelope
   version would serve both paths. Measure on a machine type with SHA
   instructions first.
2. **Run the real application's suspend again** with this change, on the next
   GCE run of that bench.

## Cleanup

The host, its disk and every object under the run's prefix were deleted.
`gcloud compute instances list`, `gcloud compute disks list` and
`gcloud storage ls` show no `sproutfs-publish-` host, disk or object.
