# Where 4+2's tail comes from on GCE, 2026-10-03

The third measurement of [the disk cache plan](../../plans/disk-cache-2026-10-02.md)
(TASK-81). [The second](gce-stripes-hedged-2026-10-03.md) found that at 9,000
reads/s 4+2's p99 was 85 to 145 ms even where it sent the same 438 KB per read
from five holders as 4+1, whose p99 was about 2 ms. It also found that two
20-second runs of the same requests could differ twenty times in p99. This run
finds where that time goes.

## What ran

Six new `n2-standard-4` hosts in us-east4-a, as before: every host a server and
a client, 4096 objects of 350,000 bytes, stripes served from the page cache.
Every host was deleted afterwards, with its disks, and the script checked that
none remained.

The benchmark now records, for each case:

- the client's CPU, its Go collector's cycles and pauses, and how long a
  ready goroutine waited for a thread (`runtime/metrics`);
- the client host's CPU by kind, steal included, and its TCP segments sent,
  sent again and timed out (`/proc`);
- for every stripe reply, the server's time: queued before it served the
  request, reading the store, and waiting behind the replies ahead of it on
  the same connection (the reply carries these);
- for every hit, the time to rebuild the object, and the part of the round
  trip of the stripe that completed it that the server did not spend (called
  network below: the request and the reply on the wire, and the client reading
  the reply);
- for every hit of 10 ms or more (the tail), the mean of each part of its time,
  and which server's stripe completed it.

Each case ran 10 s, three times. Each round ran every case once, in its own
order. Three runs on the same hosts, one after another:

- A: all six hosts read at 1500 reads/s each (9,000 in all), under healthy,
  drained, and drained and slow, in both read modes;
- B: all six at 1250 reads/s each, healthy and drained;
- C: one host reads at 1500 reads/s; the other five only serve.

The source was 40c15ef0 for A and 1fe929b6 for B and C; the benchmark is the
same in both. Raw results are in `gce-stripes-tail-2026-10-03/`.

## Results

### The noise between rounds

Run A, p99 in milliseconds of each round, in the order the rounds ran:

| Case | Round 1 | Round 2 | Round 3 | Largest over smallest |
| --- | --- | --- | --- | --- |
| 1+0 healthy, ask-all | 37.2 | 28.6 | 34.1 | 1.3 |
| 4+1 healthy, ask-all | 37.2 | 38.3 | 2.4 | 16 |
| 4+1 healthy, hedged | 2.3 | 2.0 | 19.7 | 9.8 |
| 4+1 drained, hedged | 6.8 | 1.9 | 60.3 | 31 |
| 4+2 healthy, hedged | 24.9 | 19.1 | 68.2 | 3.6 |
| 4+2 drained, hedged | 161.5 | 262.1 | 120.6 | 2.2 |

A bad round was one host at a time. Sometimes one client saw every server
slow. In 4+1 healthy hedged, five clients had a p99 of 1.9 to 2.0 ms in every
round, and one had 15.3, 16.1 and 42.5 ms: host 3 in rounds 1 and 3, host 1
in round 2. Sometimes every client saw one server slow. In 4+1 drained most
reads have no spare stripe, and in each bad round one server completed 53 to
88 % of the tail reads, not always the same server.

### What the time was not spent on

Over every case of run A:

- client CPU: 0.39 to 0.89 cores per client, of 4;
- host CPU, client and server and kernel: 0.75 to 1.70 cores per host, of 4;
  softirq 0.25 to 0.46 cores; steal 0;
- Go collector: at most 11 ms of pauses per case, over all six clients and
  10 s; the longest 1 % of pauses at most 1.7 ms; 0.4 % of the client's CPU;
- ready goroutines waited at most 0.36 ms at p99;
- rebuilding an object: at most 1.0 ms at p99;
- at the servers: queued at most 0.28 ms and reading the store at most
  0.15 ms at p99, except the slow server's injected 20 ms;
- TCP sent again at most 1.3 segments in 10,000; at most 2 retransmission
  timeouts per case across six hosts. No queue discipline dropped a packet, and
  no host dropped a received packet (`/proc/net/softnet_stat`).

### Where the tail's time went

In run A, the tail's time was in two parts: network, and waiting at the
server behind earlier replies on the same connection. Tail share is the share
of hits that took 10 ms or more, the mean over rounds. The rest are means per
tail read, in ms:

| Case | Bytes sent per serving host | Tail share | Network | Wait | Everything else |
| --- | --- | --- | --- | --- | --- |
| 1+0 healthy, ask-all | 4.2 Gb/s | 5.8 % | 21.5 | 1.5 | 0.5 |
| 4+1 healthy, hedged | 5.2 Gb/s | 0.7 % | 22.3 | 0.8 | 0.8 |
| 4+2 healthy, hedged | 5.3 Gb/s | 3.8 % | 23.1 | 6.2 | 1.0 |
| 4+2 healthy, ask-all | 6.3 Gb/s | 13.4 % | 25.7 | 29.4 | 0.6 |
| 4+2 drained, hedged | 6.3 Gb/s | 18.5 % | 23.9 | 28.2 | 0.5 |

The servers' wait over every reply, not only the tail's, tells the same. In
the healthy and drained cases that sent 6.3 Gb/s per host, its p99 was 58 to
273 ms in each round. In those that sent 5.3 Gb/s or less, it was 0.1 to
36 ms, and 0.1 ms in most rounds.

### Bytes per serving host

p99 in ms of the three rounds, smallest first, against the bytes each serving
host sent. A drained host serves nothing, so its share falls on the five
others.

| Case | Run | Per serving host | p99 by round |
| --- | --- | --- | --- |
| 4+1 healthy, hedged | B | 4.4 Gb/s | 1.9, 1.9, 1.9 |
| 4+1 drained, hedged | B | 4.4 Gb/s | 1.8, 1.9, 1.9 |
| 4+2 healthy, hedged | B | 4.4 Gb/s | 1.9, 1.9, 1.9 |
| 4+1 healthy, hedged | A | 5.2 Gb/s | 2.0, 2.3, 19.7 |
| 4+1 drained, hedged | A | 5.2 Gb/s | 1.9, 6.8, 60.3 |
| 4+2 healthy, hedged | A | 5.3 Gb/s | 19.1, 24.9, 68.2 |
| 4+2 drained, hedged | B | 5.3 Gb/s | 1.9, 2.0, 18.6 |
| 4+2 drained, ask-all | B | 5.3 Gb/s | 1.9, 29.1, 32.2 |
| 4+2 healthy, ask-all | B | 5.3 Gb/s | 25.4, 27.5, 64.5 |
| 4+2 drained, hedged | A | 6.3 Gb/s | 120.6, 161.5, 262.1 |
| 4+2 drained, ask-all | A | 6.3 Gb/s | 89.1, 178.3, 203.4 |
| 4+2 healthy, ask-all | A | 6.3 Gb/s | 95.4, 105.9, 257.9 |

Whole reads have no spare stripe, so they show a slow stripe directly. At
4.2 Gb/s per host (A) their p99 was 22 to 40 ms; at 3.5 Gb/s (B) it was 4.4 to
11.7 ms.

### One reader

In run C one host read at 1500 reads/s and took in up to 6.4 Gb/s, as much as
any host took in during run A. Each server sent about a sixth as much as in A.
Every case, every code and both read modes had a p99 of 1.75 to 1.95 ms in all
three rounds. The reader used 0.84 cores.

### A slow host

In run B, round 2 of 4+1 healthy ask-all, host 1's client had a p99 of 157 ms
and 18 % of its reads in the tail. The other five had 5.8 to 42.5 ms. Host 1's
tail reads were completed by all five other servers, and none by its own. They
spent 33 ms in the network and 19 ms waiting at the servers. Host 1's CPU was
1.19 cores, against 1.21 on host 0, whose p99 was 6.6 ms. It sent 22 of
3.4 million segments again, against 43 on host 0. Its collector paused 0.9 ms
in all.

The virtual NIC's transmit rings were full more often under more load. The
kernel had to queue a packet again because a ring was full 10,300 to 13,300
times over run A's 54 cases, on each of the five hosts that served in all of
them. Over run B's 36 cases it did so 2,100 to 3,000 times on each host.

## What it shows

**The tail is not CPU, the collector or decoding.** No host used more than
half its CPU. The Go collector, the scheduler, decoding, the servers' queues
and the store together account for under 1 ms of a tail read. The rest is the
network, and replies waiting at the server for earlier replies to the same
reader to leave.

**The tail follows the bytes each host serves.** At 4.4 Gb/s per host, 14 of
the 15 rounds of 4+1 and 4+2 cases had a p99 under 2 ms, and one had 59 ms. At
5.2 to 5.3 Gb/s, 7 of 24 rounds were under 2.5 ms and the rest reached 79 ms.
At 6.3 Gb/s, no round of a healthy or drained case was under 89 ms. That is on
hosts rated for 10 Gb/s that also take in as much as they send. One host taking
in 6.4 Gb/s while the others serve little has no tail, so the limit is not the
reader's alone.

**4+2 drained moves bytes onto every host.** In 4+2 every host holds a stripe
of every object. When one is drained, the five others serve its share: 6.3 Gb/s
each, in both read modes, where healthy hedged 4+2 serves 5.3. In 4+1, the
holder that stands in for a drained one holds nothing and sends nothing, so
each host serves 5.2 Gb/s drained or not. At 1250 reads/s per client, 4+2
drained serves 5.3 Gb/s per host, and its p99 (1.9, 2.0, 18.6 ms) matches 4+1
healthy at 1500 (2.0, 2.3, 19.7 ms). So bytes do explain 4+2's tail, counted
per serving host rather than per read. The second report was wrong about this.

**The noise is one slow host at a time.** For seconds at a time, every stripe
sent to one host, or sent by one host, is slow, while that host's CPU,
collector and retransmissions look like every other host's. A spare stripe
hides one slow holder, not a slow reader, and a read with no spare hides
neither. A 10-second case either catches such a period or not, so its p99 is
2 ms or 60 ms. At 4.4 Gb/s per host one round in fifteen caught one; at 5.2 to
5.3 Gb/s most did.

**What is left unexplained.** Why the network slows at about half the hosts'
rated bandwidth, with no drops and almost no retransmissions. The full transmit
rings point at the virtual NIC or what is behind it, but nothing here shows
where. These runs also cannot tell whether a host's sending alone sets the
limit, or its sending and receiving together. Whether `n2-standard-8` hosts, at
16 Gb/s, move the limit in proportion is not measured: the project's quota does
not hold six of them.

## What the plan should change

The read pattern and the code stand. Hedged k+1 reads keep a healthy 4+2
cluster at 5.3 Gb/s per host where asking every holder sends 6.3, and here
that is the difference between their tails. Below the limit, 4+2 still keeps
a drained and a slow host from the tail, which 4+1 does not
([first run](gce-stripes-2026-10-03.md)).

Two things change:

- A host's serving bandwidth is a budget. At the expected read rate, after a
  drain has moved a host's share onto the others, each host should serve well
  below its NIC's rate: about 40 % here, 4.4 of 10 Gb/s, kept p99 under 2 ms
  in 14 rounds of 15. Under 4+2 on six hosts a drain adds a fifth to each of
  the other five.
- Every GCE measurement runs each case in at least three rounds, each in its own
  order, and reports the bytes each serving host sent and the spread of p99
  over the rounds. One 20-second window of one case is not a result.

The plan now says both, and that a drain moves load under 4+2 and not
under 4+1.
