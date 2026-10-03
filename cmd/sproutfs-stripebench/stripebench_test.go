package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net"
	"reflect"
	"runtime/metrics"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

var benchCodes = []code{{1, 0}, {4, 1}, {4, 2}}

const (
	whole = iota
	fourPlusOne
	fourPlusTwo
)

// subsets calls f with every subset of k of 0 to n-1.
func subsets(n, k int, f func([]int)) {
	var pick func(from int, chosen []int)
	pick = func(from int, chosen []int) {
		if len(chosen) == k {
			f(chosen)
			return
		}
		for i := from; i < n; i++ {
			pick(i+1, append(chosen, i))
		}
	}
	pick(0, nil)
}

// Any 4 of the 5 stripes of 4+1, and any 4 of the 6 of 4+2, rebuild the
// object, including an object whose size is not a multiple of 4. Only a set
// missing a data stripe decodes.
func TestAnyFourStripesRebuildTheObject(t *testing.T) {
	for _, size := range []int{350_000, 1001} {
		set := objectSet{servers: 6, objects: 1, objectBytes: size, seed: 7, codes: benchCodes}
		object := make([]byte, size)
		set.fill(0, object)
		for _, c := range []code{{4, 1}, {4, 2}} {
			l, err := newLayout(c, size)
			if err != nil {
				t.Fatal(err)
			}
			tried := 0
			subsets(c.n(), c.k, func(kept []int) {
				tried++
				stripes, err := l.stripes(object)
				if err != nil {
					t.Fatal(err)
				}
				for i := range stripes {
					if !slices.Contains(kept, i) {
						stripes[i] = nil
					}
				}
				out := make([]byte, size)
				decoded, err := l.join(stripes, out)
				if err != nil {
					t.Fatalf("%v of %d bytes from stripes %v: %v", c, size, kept, err)
				}
				if !bytes.Equal(out, object) {
					t.Fatalf("%v of %d bytes from stripes %v rebuilt the wrong bytes", c, size, kept)
				}
				if want := !slices.Equal(kept, []int{0, 1, 2, 3}); decoded != want {
					t.Fatalf("%v from stripes %v decoded=%v, want %v", c, kept, decoded, want)
				}
			})
			// C(5,4) and C(6,4).
			if want := map[int]int{5: 5, 6: 15}[c.n()]; tried != want {
				t.Fatalf("%v tried %d sets of four, want %d", c, tried, want)
			}
		}
	}
}

// testHedgeMin is the hedge delay in tests. Every read on the bubble's clock
// takes no time, so the delay never rises above its floor.
const testHedgeMin = 500 * time.Microsecond

// cluster is servers and a client over in-memory connections, so it runs
// inside a synctest bubble on the bubble's clock.
type cluster struct {
	set    objectSet
	client *client
}

func newCluster(t *testing.T, servers int) *cluster {
	t.Helper()
	set := objectSet{servers: servers, objects: 48, objectBytes: 4099, seed: 3, codes: benchCodes}
	c, err := newClient(set, 1, testHedgeMin)
	if err != nil {
		t.Fatal(err)
	}
	c.timeout = time.Second
	for i := range set.servers {
		var store bytes.Buffer
		held, _, err := buildStore(set, i, &store)
		if err != nil {
			t.Fatal(err)
		}
		s := &server{set: set, index: i, data: bytes.NewReader(store.Bytes()), held: held}
		serverEnd, clientEnd := net.Pipe()
		go s.serveConn(serverEnd)
		c.peers = append(c.peers, newPeer(i, clientEnd, c.bufs, &c.log))
	}
	t.Cleanup(c.close)
	if err := c.hello(); err != nil {
		t.Fatal(err)
	}
	return &cluster{set: set, client: c}
}

func (cl *cluster) object(t *testing.T, o uint32) []byte {
	t.Helper()
	b := make([]byte, cl.set.objectBytes)
	cl.set.fill(o, b)
	return b
}

// readAt reads one object and checks the outcome, the exact latency on the
// bubble's clock and, on a hit, the bytes.
func (cl *cluster) readAt(t *testing.T, codeIndex int, o uint32, live []int, want outcome, latency time.Duration) {
	t.Helper()
	r, err := cl.client.read(askAll, codeIndex, o, live, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	what := fmt.Sprintf("object %d under %v from servers %v", o, benchCodes[codeIndex], live)
	if r.outcome != want || r.latency != latency {
		t.Fatalf("%s: outcome %d after %v, want outcome %d after %v", what, r.outcome, r.latency, want, latency)
	}
	if want == hit && !bytes.Equal(r.object, cl.object(t, o)) {
		t.Fatalf("%s: rebuilt the wrong bytes", what)
	}
	cl.client.bufs.put(r.object)
}

// A 4+2 read waits for neither a drained server nor a slow or stalled one: it
// takes no time at all on the bubble's clock. A 4+1 read waits for the slow
// server exactly when both it and the drained one hold stripes of the object,
// and a whole read waits whenever the slow server holds the object.
func TestADrainedAndASlowServerDoNotSlowAFourPlusTwoRead(t *testing.T) {
	const drained, slow, delay = 5, 4, 20 * time.Millisecond
	for _, cond := range []condition{
		{name: "drained-slow", drained: true, slow: true},
		{name: "drained-stall", drained: true, stall: true},
	} {
		t.Run(cond.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cl := newCluster(t, 6)
				s := schedule{drained: drained, slow: slow, slowDelay: delay}
				if err := cl.client.setModes(s, cond); err != nil {
					t.Fatal(err)
				}
				late, lateOutcome := delay, hit
				if cond.stall {
					late, lateOutcome = cl.client.timeout, timedOut
				}
				live := []int{0, 1, 2, 3, 4}
				waited := map[int]int{}
				for o := range uint32(cl.set.objects) {
					ranks := rank(everyServer(6), o)
					cl.readAt(t, fourPlusTwo, o, live, hit, 0)

					// 4+1 lives on the first five ranks. With one of them
					// drained, the read needs all four others, so it waits
					// when the slow server is one of them.
					if slices.Contains(ranks[:5], drained) && slices.Contains(ranks[:5], slow) {
						cl.readAt(t, fourPlusOne, o, live, lateOutcome, late)
						waited[fourPlusOne]++
					} else {
						cl.readAt(t, fourPlusOne, o, live, hit, 0)
					}

					// A drained server's whole objects are asked of the next
					// rank, which holds nothing and may be the slow server.
					missLate := miss
					if cond.stall {
						missLate = timedOut
					}
					switch {
					case ranks[0] == slow:
						cl.readAt(t, whole, o, live, lateOutcome, late)
						waited[whole]++
					case ranks[0] == drained && ranks[1] == slow:
						cl.readAt(t, whole, o, live, missLate, late)
					case ranks[0] == drained:
						cl.readAt(t, whole, o, live, miss, 0)
					default:
						cl.readAt(t, whole, o, live, hit, 0)
					}
				}
				// The objects cover both cases of each code; without that the
				// test would check less than it says.
				if waited[fourPlusOne] == 0 || waited[fourPlusOne] == cl.set.objects || waited[whole] == 0 {
					t.Fatalf("of %d objects, %d 4+1 reads and %d whole reads waited: the objects do not cover both cases",
						cl.set.objects, waited[fourPlusOne], waited[whole])
				}
			})
		})
	}
}

// Every object rebuilds through the servers from any four of its 4+2
// holders, with any two servers drained, and from any four of its 4+1
// holders, with any one drained.
func TestEveryObjectRebuildsFromAnyFourServers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl := newCluster(t, 6)
		for _, c := range []struct {
			codeIndex, drained int
		}{{fourPlusTwo, 2}, {fourPlusOne, 1}} {
			subsets(6, 6-c.drained, func(live []int) {
				for o := range uint32(cl.set.objects) {
					cl.readAt(t, c.codeIndex, o, live, hit, 0)
				}
			})
		}
	})
}

// A case at a fixed rate under a drained and a slow server, in either read
// mode: every read is a hit, every 4+2 read takes no time, the slowest 4+1
// read takes the slow server's delay, and each read asked the five live
// servers. With one server drained, k+1 of 4+2 is every live holder, so the
// hedged read has no one left to hedge to.
func TestACaseRecordsEveryRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl := newCluster(t, 6)
		want, err := newExpected(cl.set)
		if err != nil {
			t.Fatal(err)
		}
		s := schedule{load: "idle", rate: 200, concurrency: 64, duration: time.Second,
			drained: 5, slow: 4, slowDelay: 20 * time.Millisecond, seed: 1}
		cond := condition{name: "drained-slow", drained: true, slow: true}
		for _, mode := range []readMode{askAll, hedged} {
			for _, c := range []struct {
				codeIndex int
				max       time.Duration
			}{{fourPlusTwo, 0}, {fourPlusOne, 20 * time.Millisecond}} {
				res, err := cl.client.runCase(t.Context(), s, caseKey{cond: cond, code: c.codeIndex, mode: mode}, time.Now(), want)
				if err != nil {
					t.Fatal(err)
				}
				if res.Issued == 0 || res.Hits != res.Issued || res.Misses != 0 || res.TimedOut != 0 {
					t.Fatalf("%s: %d reads, %d hits, %d misses, %d timed out; want every read a hit",
						res.Name, res.Issued, res.Hits, res.Misses, res.TimedOut)
				}
				if res.Max != c.max {
					t.Fatalf("%s: slowest read %v, want %v", res.Name, res.Max, c.max)
				}
				if res.Servers.Reads != 5*res.Issued || res.Requests != 5*res.Issued {
					t.Fatalf("%s: servers got %d reads and the client sent %d for %d reads, want five each",
						res.Name, res.Servers.Reads, res.Requests, res.Issued)
				}
				if res.Second != 0 || res.Refused != 0 || res.SecondShare != 0 {
					t.Fatalf("%s: %d second requests, %d refused, share %v; want none",
						res.Name, res.Second, res.Refused, res.SecondShare)
				}
				if want := float64(res.Servers.Sent) / float64(res.Issued); res.SentPerRead != want {
					t.Fatalf("%s: %v bytes sent per read, want %v", res.Name, res.SentPerRead, want)
				}
			}
		}
	})
}

// count is n observations of a latency.
type count struct {
	at time.Duration
	n  uint64
}

// buckets is the bucket counts of a histogram of the given counts.
func buckets(counts ...count) map[int]uint64 {
	out := map[int]uint64{}
	for _, c := range counts {
		if c.n > 0 {
			out[bucketOf(c.at)] += c.n
		}
	}
	return out
}

// Under a drained and a slow server, each case says where its time went.
// Every read asks the five live servers once, and the servers' replies say
// that the slow one queued every request for its delay and did nothing else
// take time. A 4+1 read that needs the slow server's stripe takes the delay,
// is in the tail, and its time is all that stripe's queue; a 4+2 read never
// needs it, so the 4+2 cases have no tail.
func TestACaseBreaksDownWhereItsTimeWent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl := newCluster(t, 6)
		want, err := newExpected(cl.set)
		if err != nil {
			t.Fatal(err)
		}
		const slow, delay = 4, 20 * time.Millisecond
		s := schedule{load: "idle", rate: 200, concurrency: 64, duration: time.Second,
			drained: 5, slow: slow, slowDelay: delay, seed: 1, tailFrom: 10 * time.Millisecond}
		cond := condition{name: "drained-slow", drained: true, slow: true}
		for _, mode := range []readMode{askAll, hedged} {
			for _, codeIndex := range []int{fourPlusOne, fourPlusTwo} {
				res, err := cl.client.runCase(t.Context(), s, caseKey{cond: cond, code: codeIndex, mode: mode}, time.Now(), want)
				if err != nil {
					t.Fatal(err)
				}
				n := res.Issued
				if want := []uint64{n, n, n, n, n, 0}; !slices.Equal(res.Asked, want) {
					t.Fatalf("%s: servers asked %v, want %v", res.Name, res.Asked, want)
				}
				for _, h := range []struct {
					name string
					got  histogram
					want map[int]uint64
				}{
					{"queued", res.ServerQueued, buckets(count{0, 4 * n}, count{delay, n})},
					{"read", res.ServerRead, buckets(count{0, 5 * n})},
					{"waited", res.ServerWaited, buckets(count{0, 5 * n})},
					{"network", res.Network, buckets(count{0, n})},
					{"decode", res.Decode, buckets(count{0, n})},
				} {
					if !maps.Equal(h.got.counts, h.want) {
						t.Fatalf("%s: %s %v, want %v", res.Name, h.name, h.got.counts, h.want)
					}
				}
				// The 4+1 reads that need the slow server's stripe.
				late := uint64(0)
				tail := tailStats{From: s.tailFrom}
				if codeIndex == fourPlusOne {
					late = res.Latency.counts[bucketOf(delay)]
					tail = tailStats{From: s.tailFrom, Reads: late, Sum: path{Queue: time.Duration(late) * delay},
						ByServer: []uint64{0, 0, 0, 0, late}}
				}
				if want := buckets(count{0, n - late}, count{delay, late}); !maps.Equal(res.Latency.counts, want) {
					t.Fatalf("%s: latencies %v, want %v", res.Name, res.Latency.counts, want)
				}
				if !reflect.DeepEqual(res.Tail, tail) {
					t.Fatalf("%s: tail %+v, want %+v", res.Name, res.Tail, tail)
				}
				// Some 4+1 reads need the slow server and some do not; without
				// both the test checks less than it says.
				if codeIndex == fourPlusOne && (late == 0 || late == n) {
					t.Fatalf("%s: %d of %d reads needed the slow server", res.Name, late, n)
				}
			}
		}
	})
}

// setMode sets how the given servers answer reads from now on: after delay,
// or never when stall is set.
func (cl *cluster) setMode(t *testing.T, servers []int, delay time.Duration, stall bool) {
	t.Helper()
	req := request{op: opMode, arg: uint64(delay)}
	if stall {
		req.flags = modeStall
	}
	for _, s := range servers {
		if _, err := cl.client.peers[s].call(req, controlTimeout); err != nil {
			t.Fatal(err)
		}
	}
}

// seen is what a test checks of a read.
type seen struct {
	outcome  outcome
	latency  time.Duration
	requests int
	second   bool
	refused  bool
}

// readExactly reads one object and checks the outcome, the exact latency on
// the bubble's clock, the requests sent, whether it hedged or was refused
// and, on a hit, the bytes. It returns where the read's time went.
func (cl *cluster) readExactly(t *testing.T, mode readMode, codeIndex int, o uint32, live []int, want seen) (path, int) {
	t.Helper()
	r, err := cl.client.read(mode, codeIndex, o, live, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := seen{outcome: r.outcome, latency: r.latency, requests: r.requests, second: r.second, refused: r.refused}
	if got != want {
		t.Fatalf("%v read of object %d under %v from servers %v: got %+v, want %+v",
			mode, o, benchCodes[codeIndex], live, got, want)
	}
	if want.outcome == hit && !bytes.Equal(r.object, cl.object(t, o)) {
		t.Fatalf("object %d: rebuilt the wrong bytes", o)
	}
	cl.client.bufs.put(r.object)
	return r.path, r.critical
}

// askedBy reads object o in hedged mode, checks it is a hit at once from
// five requests, and returns the servers the requests reached.
func (cl *cluster) askedBy(t *testing.T, o uint32) []int {
	t.Helper()
	before, err := cl.client.serverStats()
	if err != nil {
		t.Fatal(err)
	}
	cl.readExactly(t, hedged, fourPlusTwo, o, everyServer(cl.set.servers), seen{outcome: hit, requests: 5})
	after, err := cl.client.serverStats()
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for i := range after {
		if after[i].Reads != before[i].Reads {
			out = append(out, i)
		}
	}
	return out
}

// A healthy hedged 4+2 read asks five of the six holders, takes no time and
// sends no second request; asking all asks six.
func TestAHealthyHedgedReadAsksKPlusOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl := newCluster(t, 6)
		all := everyServer(6)
		before, err := cl.client.serverStats()
		if err != nil {
			t.Fatal(err)
		}
		for o := range uint32(cl.set.objects) {
			cl.readExactly(t, hedged, fourPlusTwo, o, all, seen{outcome: hit, requests: 5})
			cl.readExactly(t, askAll, fourPlusTwo, o, all, seen{outcome: hit, requests: 6})
		}
		after, err := cl.client.serverStats()
		if err != nil {
			t.Fatal(err)
		}
		var reads uint64
		for i := range after {
			reads += after[i].Reads - before[i].Reads
		}
		if want := uint64(11 * cl.set.objects); reads != want {
			t.Fatalf("servers got %d reads, want %d: five per hedged read and six per read of all", reads, want)
		}
	})
}

// One stalled holder among the five asked costs nothing: the other four
// answer. With two stalled, the read asks the sixth holder after exactly the
// hedge delay, which twenty reads within the delay have paid for. That read's
// time is all hedge, and the sixth holder's stripe completed it.
func TestAHedgedReadAsksTheRestAfterTheDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl := newCluster(t, 6)
		all := everyServer(6)
		for o := range uint32(hedgeEarn) {
			cl.readExactly(t, hedged, fourPlusTwo, o, all, seen{outcome: hit, requests: 5})
		}
		o := uint32(hedgeEarn)
		order := pick(rank(all, o), cl.client.reader, o, 5)
		cl.setMode(t, order[:1], 0, true)
		cl.readExactly(t, hedged, fourPlusTwo, o, all, seen{outcome: hit, requests: 5})
		cl.setMode(t, order[1:2], 0, true)
		p, critical := cl.readExactly(t, hedged, fourPlusTwo, o, all,
			seen{outcome: hit, latency: testHedgeMin, requests: 6, second: true})
		if p != (path{Hedge: testHedgeMin}) || critical != order[5] {
			t.Fatalf("the hedged read's time went %+v, completed by server %d; want all hedge, completed by server %d",
				p, critical, order[5])
		}
	})
}

// A reply's header carries the server's time on the read, each part capped
// at 4.29 s.
func TestAReplySaysTheServersTime(t *testing.T) {
	h := replyHeader{id: 7, status: statusHit, stripe: 3, length: 87_500, serverTimes: serverTimes{
		queued: 3 * time.Millisecond, read: 41 * time.Microsecond, waited: 5 * time.Second}}
	var b bytes.Buffer
	frame := make([]byte, replyHeaderBytes)
	h.put(frame)
	b.Write(frame)
	got, err := readReplyHeader(&b)
	if err != nil {
		t.Fatal(err)
	}
	want := h
	want.waited = 4_294_967_295 * time.Nanosecond
	if got != want {
		t.Fatalf("the header came back as %+v, want %+v", got, want)
	}
}

// One round keeps the order of conditions, codes and read modes. Several
// rounds each run every case once, each in its own order, the same for every
// client given the same seed.
func TestRoundsRunEveryCaseInTheirOwnOrder(t *testing.T) {
	conds := []condition{conditions[0], conditions[3]}
	modes := []readMode{askAll, hedged}
	name := func(k caseKey) string { return fmt.Sprintf("%s/%d/%v/%d", k.cond.name, k.code, k.mode, k.repeat) }
	names := func(keys []caseKey) []string {
		var out []string
		for _, k := range keys {
			out = append(out, name(k))
		}
		return out
	}
	one := names(caseOrder(conds, 2, modes, 1, 1))
	if want := []string{
		"healthy/0/ask-all/0", "healthy/0/hedged/0", "healthy/1/ask-all/0", "healthy/1/hedged/0",
		"drained/0/ask-all/0", "drained/0/hedged/0", "drained/1/ask-all/0", "drained/1/hedged/0",
	}; !slices.Equal(one, want) {
		t.Fatalf("one round ran %v, want %v", one, want)
	}
	two := names(caseOrder(conds, 2, modes, 2, 1))
	if want := []string{
		"healthy/1/ask-all/0", "drained/1/hedged/0", "healthy/0/hedged/0", "drained/0/hedged/0",
		"drained/1/ask-all/0", "drained/0/ask-all/0", "healthy/0/ask-all/0", "healthy/1/hedged/0",
		"healthy/1/hedged/1", "drained/1/ask-all/1", "healthy/1/ask-all/1", "healthy/0/hedged/1",
		"drained/0/ask-all/1", "drained/0/hedged/1", "drained/1/hedged/1", "healthy/0/ask-all/1",
	}; !slices.Equal(two, want) {
		t.Fatalf("two rounds ran %v, want %v", two, want)
	}
	if again := names(caseOrder(conds, 2, modes, 2, 1)); !slices.Equal(again, two) {
		t.Fatalf("the same seed ran %v, then %v", two, again)
	}
}

// The p99 of each round, and of every round together, by case in the order
// of conditions, codes and read modes, whatever order the rounds ran in. One
// bad round of two shows in its own p99 and not in the p99 of both.
func TestASpreadIsEachRoundsP99(t *testing.T) {
	result := func(cond, read string, repeat int, latencies ...time.Duration) caseResult {
		c := caseResult{Name: "full/memory/" + cond + "/4+2/" + read, Condition: cond, Code: "4+2", Read: read, Repeat: repeat}
		for _, d := range latencies {
			for range 100 {
				c.Latency.observe(d)
			}
		}
		c.summarize()
		return c
	}
	// Healthy: 98 % of hits at 1 ms and 2 % at 40 ms in the first round, at
	// 2 ms in the second. Drained: every hit at 1 ms.
	r := record{Repeats: 2, Cases: []caseResult{
		result("drained", "hedged", 0, slices.Repeat([]time.Duration{time.Millisecond}, 99)...),
		result("healthy", "ask-all", 0, append(slices.Repeat([]time.Duration{time.Millisecond}, 98), 40*time.Millisecond, 40*time.Millisecond)...),
		result("healthy", "ask-all", 1, append(slices.Repeat([]time.Duration{time.Millisecond}, 98), 2*time.Millisecond, 2*time.Millisecond)...),
		result("drained", "hedged", 1, slices.Repeat([]time.Duration{time.Millisecond}, 99)...),
	}}
	var got []string
	for _, s := range r.spreads() {
		got = append(got, fmt.Sprintf("%s %v %v", s.name, s.p99, s.all.quantile(0.99)))
	}
	want := []string{
		"full/memory/healthy/4+2/ask-all [40ms 2ms] 2.015232ms",
		"full/memory/drained/4+2/hedged [1ms 1ms] 1ms",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("spreads %q, want %q", got, want)
	}
}

// A runtime histogram's counts between two samples, each at its bucket's
// middle, or at its finite bound.
func TestRuntimeHistogramSince(t *testing.T) {
	bounds := []float64{math.Inf(-1), 0, 1e-6, 2e-6, math.Inf(1)}
	before := &metrics.Float64Histogram{Counts: []uint64{0, 1, 2, 0}, Buckets: bounds}
	after := &metrics.Float64Histogram{Counts: []uint64{0, 1, 5, 1}, Buckets: bounds}
	h := histogramSince(before, after)
	if want := buckets(count{1500 * time.Nanosecond, 3}, count{2 * time.Microsecond, 1}); !maps.Equal(h.counts, want) ||
		h.total != 4 || h.max != 2*time.Microsecond {
		t.Fatalf("got %v, %d in all, max %v; want %v, 4 in all, max 2µs", h.counts, h.total, h.max, want)
	}
}

// A host's counters from Linux's /proc, and what they counted between two
// readings.
func TestHostCounters(t *testing.T) {
	const snmp = "Ip: Forwarding DefaultTTL\nIp: 1 64\n" +
		"Tcp: RtoAlgorithm MaxConn OutSegs RetransSegs\nTcp: 1 -1 1000 7\n"
	const netstat = "TcpExt: SyncookiesSent TCPTimeouts\nTcpExt: 0 2\n"
	before, err := parseHost("cpu  100 5 30 900 10 1 20 4 0 0\ncpu0 1 2 3\n", snmp, netstat)
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseHost("cpu  300 5 50 1100 10 2 60 9 0 0\n",
		strings.ReplaceAll(snmp, "1 -1 1000 7", "1 -1 5000 9"), strings.ReplaceAll(netstat, "0 2", "0 3"))
	if err != nil {
		t.Fatal(err)
	}
	want := hostStats{Hosts: 1, User: 1050 * time.Millisecond, System: 300 * time.Millisecond,
		IRQ: 10 * time.Millisecond, SoftIRQ: 200 * time.Millisecond, Steal: 40 * time.Millisecond,
		Idle: 9100 * time.Millisecond, TCPOut: 1000, TCPRetrans: 7, TCPTimeouts: 2}
	if before != want {
		t.Fatalf("read %+v, want %+v", before, want)
	}
	want = hostStats{Hosts: 1, User: 2 * time.Second, System: 200 * time.Millisecond, IRQ: 10 * time.Millisecond,
		SoftIRQ: 400 * time.Millisecond, Steal: 50 * time.Millisecond, Idle: 2 * time.Second,
		TCPOut: 4000, TCPRetrans: 2, TCPTimeouts: 1}
	if got := after.since(before); got != want {
		t.Fatalf("counted %+v, want %+v", got, want)
	}
}

// When every holder is slower than the read's bound, each read spends one
// second request until the budget is empty, and then sends none. Two hundred
// reads within the delay earned ten requests, but the budget holds five, so
// across the whole run five reads hedged.
func TestWhenEveryHolderIsSlowTheBudgetRunsOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl := newCluster(t, 6)
		all := everyServer(6)
		for i := range 2 * hedgeMax * hedgeEarn {
			cl.readExactly(t, hedged, fourPlusTwo, uint32(i%cl.set.objects), all, seen{outcome: hit, requests: 5})
		}
		cl.setMode(t, all, 2*cl.client.timeout, false)
		for o := range uint32(hedgeMax) {
			cl.readExactly(t, hedged, fourPlusTwo, o, all,
				seen{outcome: timedOut, latency: cl.client.timeout, requests: 6, second: true})
		}
		for o := range uint32(2 * hedgeMax) {
			cl.readExactly(t, hedged, fourPlusTwo, o, all,
				seen{outcome: timedOut, latency: cl.client.timeout, requests: 5, refused: true})
		}
	})
}

// A holder that holds nothing of the object is replaced at once by the next
// holder, without waiting for the delay and without the budget, which is
// empty. Seven servers, the object's first rank drained: its six holders
// among the live servers include the seventh rank, which holds nothing. The
// reader is one whose five include that server, and one of the other four is
// stalled, so the read needs the replacement.
func TestAMissIsReplacedAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl := newCluster(t, 7)
		o := uint32(0)
		ranks := rank(everyServer(7), o)
		empty := ranks[6]
		live := slices.Clone(ranks[1:])
		slices.Sort(live)
		holders := rank(live, o)[:6]
		reader := uint64(1)
		for !slices.Contains(pick(holders, reader, o, 5)[:5], empty) {
			reader++
		}
		cl.client.reader = reader
		first := pick(holders, reader, o, 5)[:5]
		stalled := first[slices.IndexFunc(first, func(s int) bool { return s != empty })]
		cl.setMode(t, []int{stalled}, 0, true)
		cl.readExactly(t, hedged, fourPlusTwo, o, live, seen{outcome: hit, requests: 6})
		if got := cl.client.hedgers[fourPlusTwo].budget; got != 1 {
			t.Fatalf("the budget holds %d twentieths of a request, want the 1 the read earned", got)
		}
	})
}

// Sixty readers of one object each ask five of its six holders, the same five
// every time, and between them ask every holder.
func TestReadersSpreadOverEveryHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl := newCluster(t, 6)
		const o = 0
		asked := make([]int, 6)
		for reader := uint64(1); reader <= 60; reader++ {
			cl.client.reader = reader
			first := cl.askedBy(t, o)
			if again := cl.askedBy(t, o); !slices.Equal(first, again) {
				t.Fatalf("reader %d asked %v, then %v", reader, first, again)
			}
			for _, s := range first {
				asked[s]++
			}
		}
		// Each holder is left out by between 5 and 15 of the 60.
		if want := []int{46, 45, 55, 53, 51, 50}; !slices.Equal(asked, want) {
			t.Fatalf("the servers were asked by %v readers, want %v", asked, want)
		}
	})
}

// A percentile is within 1/32 of the true value, and a histogram survives
// its record.
func TestHistogramPercentiles(t *testing.T) {
	var h histogram
	for i := 1; i <= 10_000; i++ {
		h.observe(time.Duration(i) * time.Microsecond)
	}
	for _, q := range []float64{0.5, 0.9, 0.99, 0.999} {
		exact := time.Duration(q*10_000) * time.Microsecond
		got := h.quantile(q)
		if diff := got - exact; diff < -exact/32 || diff > exact/32 {
			t.Fatalf("p%v is %v, want within 1/32 of %v", q*100, got, exact)
		}
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var back histogram
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.total != h.total || back.max != h.max || back.quantile(0.999) != h.quantile(0.999) {
		t.Fatalf("the histogram came back as %d observations, max %v, p99.9 %v; want %d, %v, %v",
			back.total, back.max, back.quantile(0.999), h.total, h.max, h.quantile(0.999))
	}
}
