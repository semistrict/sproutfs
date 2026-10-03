package main

import (
	"slices"
	"sync"
	"time"
)

// caseLog is what one case's stripe requests and the servers' replies to them
// say: how many requests each server was sent, and the servers' time on every
// reply, including the replies that came after their read had what it needed.
// The client's read loops add the replies.
type caseLog struct {
	mu       sync.Mutex
	asked    []uint64
	sent     uint64
	answered uint64
	// settled is closed once every request is answered, when someone waits.
	settled chan struct{}

	queued histogram
	read   histogram
	waited histogram
}

func newCaseLog(servers int) *caseLog { return &caseLog{asked: make([]uint64, servers)} }

// ask counts a stripe request sent to server. A nil log counts nothing.
func (l *caseLog) ask(server int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked[server]++
	l.sent++
}

// answer records the server's time on one reply to a stripe request.
func (l *caseLog) answer(t serverTimes) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.answered++
	l.queued.observe(t.queued)
	l.read.observe(t.read)
	l.waited.observe(t.waited)
	if l.settled != nil && l.answered >= l.sent {
		close(l.settled)
		l.settled = nil
	}
}

// settle waits until every request sent has been answered, or for at most
// bound: a stalled server never answers.
func (l *caseLog) settle(bound time.Duration) {
	l.mu.Lock()
	if l.answered >= l.sent {
		l.mu.Unlock()
		return
	}
	settled := make(chan struct{})
	l.settled = settled
	l.mu.Unlock()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-settled:
	case <-timer.C:
	}
}

// path is where one hit's time went, from when it was due to the rebuilt
// object. Hedge to Deliver are the time of the stripe that completed the
// read, the k-th to arrive, and add up to the time from the read's first
// request to that stripe in the read's hands.
type path struct {
	// Issue runs from when the read was due to its first request.
	Issue time.Duration `json:"issue_ns"`
	// Hedge runs from the first request to the completing stripe's: the
	// hedge delay, or the replacement of a holder that held nothing.
	Hedge time.Duration `json:"hedge_ns"`
	// Queue, Read and Wait are the server's: before serving the request, a
	// slow server's delay included; reading the store; and waiting behind
	// the replies ahead of it on the connection.
	Queue time.Duration `json:"queue_ns"`
	Read  time.Duration `json:"read_ns"`
	Wait  time.Duration `json:"wait_ns"`
	// Network is the rest of the stripe's round trip: the request and the
	// reply on the wire, and the client reading the reply off the connection.
	Network time.Duration `json:"network_ns"`
	// Deliver runs from the reply read off the connection to the read
	// taking it.
	Deliver time.Duration `json:"deliver_ns"`
	// Decode is rebuilding the object from its k stripes: a copy, or a decode
	// when a data stripe is missing.
	Decode time.Duration `json:"decode_ns"`
}

func (p *path) add(o path) {
	p.Issue += o.Issue
	p.Hedge += o.Hedge
	p.Queue += o.Queue
	p.Read += o.Read
	p.Wait += o.Wait
	p.Network += o.Network
	p.Deliver += o.Deliver
	p.Decode += o.Decode
}

// parts names the parts of a path in order, for the report.
func (p path) parts() []time.Duration {
	return []time.Duration{p.Issue, p.Hedge, p.Queue, p.Read, p.Wait, p.Network, p.Deliver, p.Decode}
}

var pathParts = []string{"issue", "hedge", "queue", "read", "wait", "network", "deliver", "decode"}

// tailStats is where the time of the hits that took From or longer went:
// their paths summed, and which server sent the stripe that completed each.
type tailStats struct {
	From     time.Duration `json:"from_ns"`
	Reads    uint64        `json:"reads"`
	Sum      path          `json:"sum"`
	ByServer []uint64      `json:"by_server"`
}

func (t *tailStats) observe(p path, server int) {
	t.Reads++
	t.Sum.add(p)
	t.ByServer = grow(t.ByServer, server+1)
	t.ByServer[server]++
}

func (t *tailStats) add(o tailStats) {
	t.From = max(t.From, o.From)
	t.Reads += o.Reads
	t.Sum.add(o.Sum)
	t.ByServer = addCounts(t.ByServer, o.ByServer)
}

// grow extends a with zeros to at least n elements.
func grow(a []uint64, n int) []uint64 {
	if len(a) < n {
		a = append(slices.Clone(a), make([]uint64, n-len(a))...)
	}
	return a
}

// addCounts adds o to a, element by element, growing a as needed.
func addCounts(a, o []uint64) []uint64 {
	a = grow(a, len(o))
	for i, n := range o {
		a[i] += n
	}
	return a
}
