# Hedged k+1 reads against asking every holder on GCE, 2026-10-03

The second measurement of [the disk cache plan](../../plans/disk-cache-2026-10-02.md)
(TASK-81). [The first](gce-stripes-2026-10-03.md) found that asking all k+m
holders made 4+2's tail worse than whole reads at full load, and blamed the
bytes. The plan now asks k+1 holders first and the rest after a delay, under a
budget. This run checks that pattern.

## What ran

The same six `n2-standard-4` hosts and objects as the first run: 4096 objects
of 350,000 bytes, every host a server and a client, 1500 reads/s per client
(9,000 reads/s in all), stripes served from the page cache. Only the
full-memory pass ran, each case in two read modes:

- ask-all: ask the first k+m live ranks at once;
- hedged: ask k+1 of them, chosen by a hash of reader and object, replace a
  miss at once, and ask the rest after the 95th percentile of recent reads
  (at least 500 µs), paid from a budget of one request per twenty fast reads,
  capped at five.

For 1+0 and 4+1, k+1 is every holder, so the two modes send the same requests.
For 4+2 drained, only five holders are live, so they do too. The source was
efc1e1a6. Every host was deleted afterwards and the script checked it.

## Results

4+2, p99 and p99.9 in milliseconds, with server bytes sent per read:

| Case | Ask-all p99 / p99.9 | Hedged p99 / p99.9 | Ask-all sent | Hedged sent |
| --- | --- | --- | --- | --- |
| healthy | 110 / 166 | 35 / 153 | 525 KB | 439 KB |
| slow | 116 / 174 | 21 / 114 | 525 KB | 439 KB |
| drained | 85 / 203 | 104 / 187 | 438 KB | 438 KB |
| drained and slow | 145 / 250 | 91 / 174 | 438 KB | 438 KB |
| drained and stalled | 466 / 579 | 273 / 424 | 350 KB | 350 KB |

Hedged 4+2 asked 5.01 to 5.02 holders per read. 1.4 % to 1.8 % of reads asked
the rest after the delay, and the budget refused 0.9 % to 1.5 %. No read
missed, timed out or returned wrong bytes in either mode. Medians stayed at
0.8 to 0.9 ms everywhere.

Cases where the two modes send the same requests:

| Case | Ask-all p99 / p99.9 | Hedged p99 / p99.9 |
| --- | --- | --- |
| 4+1 healthy | 37 / 102 | 2.0 / 2.4 |
| 4+1 drained and stalled | 41 / 127 | 1.9 / 2.3 |
| 4+1 drained and slow | 36 / 68 | 22 / 22 |
| 1+0 drained | 47 / 127 | 25 / 57 |

## What it shows

**Hedging is the right pattern.** It sends 16 % fewer bytes, needs a second
request on under 2 % of reads, and its tail is never worse than ask-all's by
more than this run's noise. In the healthy and slow cases it cut 4+2's p99 from
110 and 116 ms to 35 and 21 ms.

**This run cannot say how much of that is hedging.** Where the two modes send
the same requests, their tails still differ by up to twenty times: 4+1 healthy
has a p99 of 37 ms in one and 2.0 ms in the other. At this load, on hosts that
are clients and servers at once, the noise between two 20-second runs is as
large as the effect.

**Bytes do not explain 4+2's tail.** 4+2 drained sends 438 KB per read from
five holders, as 4+1 healthy does, yet its p99 is 85 to 104 ms in both modes,
where 4+1 reaches 2 ms in half its cases. Two things differ: in 4+2 every host
holds a stripe of every object, and more reads decode (92 % against 77 %). The
first run's claim that "the bytes, not the code, are the cost" is not
supported.

## What the plan should change

Step 7 reads hedged, as the plan already says. Before step 7 is measured
again, the benchmark should repeat each case to measure the noise, and report
client CPU, Go GC pauses and time queued at each server, so that a tail can be
traced to a cause.

[The third run](gce-stripes-tail-2026-10-03.md) did that. It traced the tail to
the bytes each host serves, not the bytes per read: 4+2 drained serves 6.3 Gb/s
per host where 4+1 healthy serves 5.2.
