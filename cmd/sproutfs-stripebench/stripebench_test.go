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

// cluster is six servers and a client over in-memory connections, so it runs
// inside a synctest bubble on the bubble's clock.
type cluster struct {
	set    objectSet
	client *client
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	set := objectSet{servers: 6, objects: 48, objectBytes: 4099, seed: 3, codes: benchCodes}
	layouts, err := set.layouts()
	if err != nil {
		t.Fatal(err)
	}
	c := &client{set: set, layouts: layouts, bufs: &buffers{}, timeout: time.Second}
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
	r, err := cl.client.read(codeIndex, o, live, time.Now())
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
				cl := newCluster(t)
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
		cl := newCluster(t)
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

// A case at a fixed rate under a drained and a slow server: every read is a
// hit, every 4+2 read takes no time, the slowest 4+1 read takes the slow
// server's delay, and each read asked the five live servers.
func TestACaseRecordsEveryRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cl := newCluster(t)
		want, err := newExpected(cl.set)
		if err != nil {
			t.Fatal(err)
		}
		s := schedule{load: "idle", rate: 200, concurrency: 64, duration: time.Second,
			drained: 5, slow: 4, slowDelay: 20 * time.Millisecond, seed: 1}
		cond := condition{name: "drained-slow", drained: true, slow: true}
		for _, c := range []struct {
			codeIndex int
			max       time.Duration
		}{{fourPlusTwo, 0}, {fourPlusOne, 20 * time.Millisecond}} {
			res, err := cl.client.runCase(t.Context(), s, cond, c.codeIndex, time.Now(), want)
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
			if res.Servers.Reads != 5*res.Issued {
				t.Fatalf("%s: servers got %d reads for %d reads, want five each", res.Name, res.Servers.Reads, res.Issued)
			}
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
