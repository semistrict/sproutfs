package main

import (
	"context"
	"errors"
	"fmt"
	"hash/maphash"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"time"
)

// controlTimeout bounds a mode, stats or hello request. A stalled server
// still answers these.
const controlTimeout = 10 * time.Second

type outcome int

const (
	// hit: k stripes arrived and the object was rebuilt.
	hit outcome = iota
	// miss: every server asked answered, and fewer than k held a stripe. The
	// deployment would read the object store.
	miss
	// timedOut: k stripes had not arrived by the client's bound. The
	// deployment would read the object store as well.
	timedOut
)

type readResult struct {
	outcome outcome
	latency time.Duration
	object  []byte // from the client's buffers, on a hit
	decoded bool   // a data stripe was missing and parity rebuilt it

	requests int  // the stripe requests the read sent
	second   bool // it asked the rest of the holders after the delay
	refused  bool // it would have, but the budget held less than a request
}

// client reads objects from the servers.
type client struct {
	set     objectSet
	layouts []*layout
	peers   []*peer
	bufs    *buffers
	timeout time.Duration
	// reader is this reader's identity. It chooses the holders a hedged read
	// asks first.
	reader uint64
	// hedgers holds one hedger per code, since stripes of different sizes
	// take different times.
	hedgers []*hedger
}

func newClient(set objectSet, reader uint64, hedgeMin time.Duration) (*client, error) {
	layouts, err := set.layouts()
	if err != nil {
		return nil, err
	}
	c := &client{set: set, layouts: layouts, bufs: &buffers{}, reader: reader, hedgers: make([]*hedger, len(layouts))}
	for i := range c.hedgers {
		c.hedgers[i] = newHedger(hedgeMin)
	}
	return c, nil
}

// read reads one object under one code from the servers in live, which are
// the servers not drained. The object's holders are its first k+m ranks among
// them. Under askAll the read asks them all at once. Under hedged it asks
// k+1 of them, and the rest only if k stripes have not arrived after the
// hedger's delay and the budget holds a request. Either way, a holder that
// answers a miss is replaced at once by the next holder not yet asked. The
// read rebuilds the object from the first k stripes, whatever their indices.
// Its latency runs from scheduled to the rebuilt object.
func (c *client) read(mode readMode, codeIndex int, object uint32, live []int, scheduled time.Time) (readResult, error) {
	l := c.layouts[codeIndex]
	holders := rank(live, object)
	holders = holders[:min(l.n(), len(holders))]
	first := len(holders)
	if mode == hedged {
		first = min(l.k+1, len(holders))
		holders = pick(holders, c.reader, object, first)
	}
	ch := make(chan reply, len(holders))
	type sent struct {
		peer *peer
		id   uint64
	}
	asked := make([]sent, 0, len(holders))
	stripes := make([][]byte, l.n())
	defer func() {
		for _, a := range asked {
			a.peer.forget(a.id)
		}
		// Replies that came after the read had what it needed.
		for empty := false; !empty; {
			select {
			case r := <-ch:
				c.bufs.put(r.data)
			default:
				empty = true
			}
		}
		for _, s := range stripes {
			c.bufs.put(s)
		}
	}()
	next := 0
	// ask asks the next n holders not yet asked.
	ask := func(n int) error {
		for _, s := range holders[next : next+n] {
			id, err := c.peers[s].send(request{op: opRead, code: uint8(codeIndex), object: object}, ch)
			if err != nil {
				return err
			}
			asked = append(asked, sent{c.peers[s], id})
		}
		next += n
		return nil
	}
	h := c.hedgers[codeIndex]
	began := time.Now()
	if err := ask(first); err != nil {
		return readResult{}, err
	}
	timer := time.NewTimer(c.timeout)
	defer timer.Stop()
	// The delay fires once, and only when some holder is left to ask.
	var delay <-chan time.Time
	if next < len(holders) {
		t := time.NewTimer(h.delay())
		defer t.Stop()
		delay = t.C
	}
	var res readResult
	waited := false
	hits := 0
	for answered := 0; hits < l.k && answered < len(asked); {
		select {
		case r := <-ch:
			answered++
			if r.err != nil {
				return readResult{}, fmt.Errorf("server %d: %w", r.server, r.err)
			}
			switch r.status {
			case statusMiss:
				if next < len(holders) {
					if err := ask(1); err != nil {
						return readResult{}, err
					}
				}
			case statusHit:
				if r.stripe >= l.n() || len(r.data) != l.stripeBytes || stripes[r.stripe] != nil {
					c.bufs.put(r.data)
					return readResult{}, fmt.Errorf("server %d sent stripe %d of %d bytes of object %d under %v",
						r.server, r.stripe, len(r.data), object, l.code)
				}
				stripes[r.stripe] = r.data
				hits++
			case statusRefused:
				return readResult{}, fmt.Errorf("server %d refused a read: %s", r.server, r.data)
			default:
				return readResult{}, fmt.Errorf("server %d answered a read with status %d", r.server, r.status)
			}
		case <-delay:
			delay, waited = nil, true
			switch {
			case next == len(holders):
				// Misses have already asked every holder.
			case h.take():
				res.second = true
				if err := ask(len(holders) - next); err != nil {
					return readResult{}, err
				}
			default:
				res.refused = true
			}
		case <-timer.C:
			res.outcome, res.latency, res.requests = timedOut, time.Since(scheduled), len(asked)
			return res, nil
		}
	}
	res.requests = len(asked)
	if hits < l.k {
		res.outcome, res.latency = miss, time.Since(scheduled)
		return res, nil
	}
	if mode == hedged {
		h.done(time.Since(began), waited)
	}
	out := c.bufs.get(l.objectBytes)
	decoded, err := l.join(stripes, out)
	if err != nil {
		c.bufs.put(out)
		return readResult{}, err
	}
	res.outcome, res.latency, res.object, res.decoded = hit, time.Since(scheduled), out, decoded
	return res, nil
}

// condition is the state of the servers during a case.
type condition struct {
	name    string
	drained bool // one server is out of the list
	slow    bool // one other server answers every read late
	stall   bool // one other server never answers a read
}

var conditions = []condition{
	{name: "healthy"},
	{name: "slow", slow: true},
	{name: "stall", stall: true},
	{name: "drained", drained: true},
	{name: "drained-slow", drained: true, slow: true},
	{name: "drained-stall", drained: true, stall: true},
}

func parseConditions(s string) ([]condition, error) {
	var out []condition
	for _, name := range splitList(s) {
		i := slices.IndexFunc(conditions, func(c condition) bool { return c.name == name })
		if i < 0 {
			return nil, fmt.Errorf("condition %q: want one of healthy, slow, stall, drained, drained-slow or drained-stall", name)
		}
		out = append(out, conditions[i])
	}
	if len(out) == 0 {
		return nil, errors.New("no conditions")
	}
	return out, nil
}

// schedule is how a client runs its cases.
type schedule struct {
	load        string // a label: idle or full
	disk        bool   // servers drop each stripe from the page cache once read
	rate        float64
	concurrency int
	duration    time.Duration
	drained     int
	slow        int
	slowDelay   time.Duration
	seed        uint64
}

// setModes tells every server how to answer during a case.
func (c *client) setModes(s schedule, cond condition) error {
	for i, p := range c.peers {
		var req request
		req.op = opMode
		if s.disk {
			req.flags |= modeCold
		}
		if i == s.slow && cond.slow {
			req.arg = uint64(s.slowDelay)
		}
		if i == s.slow && cond.stall {
			req.flags |= modeStall
		}
		if _, err := p.call(req, controlTimeout); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) serverStats() ([]serverStats, error) {
	out := make([]serverStats, len(c.peers))
	for i, p := range c.peers {
		b, err := p.call(request{op: opStats}, controlTimeout)
		if err != nil {
			return nil, err
		}
		if out[i], err = parseStats(b); err != nil {
			return nil, fmt.Errorf("server %d: %w", i, err)
		}
	}
	return out, nil
}

// expected is each object's hash, which every rebuilt object is checked
// against.
type expected struct {
	seed   maphash.Seed
	hashes []uint64
}

func newExpected(set objectSet) (*expected, error) {
	e := &expected{seed: maphash.MakeSeed(), hashes: make([]uint64, set.objects)}
	err := parallel(set.objects, func(i int) error {
		b := make([]byte, set.objectBytes)
		set.fill(uint32(i), b)
		e.hashes[i] = maphash.Bytes(e.seed, b)
		return nil
	})
	return e, err
}

// runCase reads at the schedule's rate from start for its duration, under
// one condition, one code and one read mode, and records what it saw.
func (c *client) runCase(ctx context.Context, s schedule, cond condition, codeIndex int, mode readMode,
	start time.Time, want *expected) (caseResult, error) {
	medium := "memory"
	if s.disk {
		medium = "disk"
	}
	code := c.layouts[codeIndex].code.String()
	res := caseResult{
		Name: fmt.Sprintf("%s/%s/%s/%s/%s", s.load, medium, cond.name, code, mode),
		Load: s.load, Medium: medium, Condition: cond.name, Code: code, Read: mode.String(),
	}
	if err := c.setModes(s, cond); err != nil {
		return res, err
	}
	live := everyServer(len(c.peers))
	if cond.drained {
		live = slices.DeleteFunc(live, func(i int) bool { return i == s.drained })
	}
	before, err := c.serverStats()
	if err != nil {
		return res, err
	}
	cpuBefore, err := processCPU()
	if err != nil {
		return res, err
	}
	if wait := time.Until(start); wait > 0 {
		time.Sleep(wait)
	}
	res.Started = time.Now()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var mu sync.Mutex
	record := func(r readResult, object uint32) {
		if r.outcome == hit {
			if got := maphash.Bytes(want.seed, r.object); got != want.hashes[object] {
				cancel(fmt.Errorf("object %d under %s rebuilt wrong", object, code))
			}
			c.bufs.put(r.object)
		}
		mu.Lock()
		defer mu.Unlock()
		res.Issued++
		res.Requests += uint64(r.requests)
		if r.second {
			res.Second++
		}
		if r.refused {
			res.Refused++
		}
		switch r.outcome {
		case hit:
			res.Hits++
			res.Latency.observe(r.latency)
			if r.decoded {
				res.Decoded++
			}
		case miss:
			res.Misses++
		case timedOut:
			res.TimedOut++
		}
	}
	rng := rand.New(rand.NewPCG(s.seed, uint64(start.UnixNano())))
	sem := make(chan struct{}, s.concurrency)
	var wg sync.WaitGroup
	issue := func(object uint32, scheduled time.Time) {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		wg.Go(func() {
			defer func() { <-sem }()
			r, err := c.read(mode, codeIndex, object, live, scheduled)
			if err != nil {
				cancel(err)
				return
			}
			record(r, object)
		})
	}
	end := start.Add(s.duration)
	if s.rate > 0 {
		// Open loop: reads arrive as a Poisson process whatever the servers
		// do, and a read's latency counts from when it was due.
		for next := start; next.Before(end) && ctx.Err() == nil; next = next.Add(time.Duration(rng.ExpFloat64() / s.rate * float64(time.Second))) {
			if wait := time.Until(next); wait > 0 {
				time.Sleep(wait)
			}
			issue(uint32(rng.IntN(c.set.objects)), next)
		}
	} else {
		// Closed loop: each of concurrency readers reads as fast as it can.
		var readers sync.WaitGroup
		for r := range s.concurrency {
			own := rand.New(rand.NewPCG(s.seed, uint64(r)))
			readers.Go(func() {
				for ctx.Err() == nil && time.Now().Before(end) {
					object := uint32(own.IntN(c.set.objects))
					scheduled := time.Now()
					got, err := c.read(mode, codeIndex, object, live, scheduled)
					if err != nil {
						cancel(err)
						return
					}
					record(got, object)
				}
			})
		}
		readers.Wait()
	}
	wg.Wait()
	res.Elapsed = time.Since(res.Started)
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return res, err
	}
	after, err := c.serverStats()
	if err != nil {
		return res, err
	}
	cpuAfter, err := processCPU()
	if err != nil {
		return res, err
	}
	res.ClientCPU = cpuAfter - cpuBefore
	for i := range after {
		d := after[i].minus(before[i])
		res.Servers.CPU += d.CPU
		res.Servers.Sent += d.Sent
		res.Servers.Replies += d.Replies
		res.Servers.Reads += d.Reads
	}
	res.summarize()
	slog.Info("case done", "case", res.Name, "reads", res.Issued, "misses", res.Misses,
		"timed_out", res.TimedOut, "second", res.Second, "refused", res.Refused,
		"p50", res.P50, "p99", res.P99, "p999", res.P999)
	return res, nil
}
