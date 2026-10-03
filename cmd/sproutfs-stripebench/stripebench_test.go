package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"slices"
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
		c.peers = append(c.peers, newPeer(i, clientEnd, c.bufs))
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
				res, err := cl.client.runCase(t.Context(), s, cond, c.codeIndex, mode, time.Now(), want)
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
// and, on a hit, the bytes.
func (cl *cluster) readExactly(t *testing.T, mode readMode, codeIndex int, o uint32, live []int, want seen) {
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
// hedge delay, which twenty reads within the delay have paid for.
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
		cl.readExactly(t, hedged, fourPlusTwo, o, all,
			seen{outcome: hit, latency: testHedgeMin, requests: 6, second: true})
	})
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
