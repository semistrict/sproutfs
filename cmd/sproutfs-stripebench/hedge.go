package main

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// readMode is how a read chooses which holders to ask.
type readMode int

const (
	// askAll asks the object's first k+m ranks at once.
	askAll readMode = iota
	// hedged asks k+1 of them, chosen by the reader, and the rest only if k
	// stripes have not arrived after the hedger's delay, under its budget.
	// This is the plan's read (plans/disk-cache-2026-10-02.md, "Reading a
	// page").
	hedged
)

var readModeNames = []string{"ask-all", "hedged"}

func (m readMode) String() string { return readModeNames[m] }

func parseReadModes(s string) ([]readMode, error) {
	var out []readMode
	for _, name := range splitList(s) {
		i := slices.Index(readModeNames, name)
		if i < 0 {
			return nil, fmt.Errorf("read mode %q: want ask-all or hedged", name)
		}
		if slices.Contains(out, readMode(i)) {
			return nil, fmt.Errorf("read mode %q is given twice", name)
		}
		out = append(out, readMode(i))
	}
	if len(out) == 0 {
		return nil, errors.New("no read modes")
	}
	return out, nil
}

// pick orders an object's holders for a reader: the want holders that score
// highest for this reader and object first, then the others, each part in
// rank order. So the readers of one object spread over all its holders, and
// one reader always picks the same ones.
func pick(holders []int, reader uint64, object uint32, want int) []int {
	if want >= len(holders) {
		return holders
	}
	byScore := slices.Clone(holders)
	slices.SortStableFunc(byScore, func(a, b int) int {
		return cmp.Compare(readerScore(reader, b, object), readerScore(reader, a, object))
	})
	chosen := byScore[:want]
	out := make([]int, 0, len(holders))
	for _, s := range holders {
		if slices.Contains(chosen, s) {
			out = append(out, s)
		}
	}
	for _, s := range holders {
		if !slices.Contains(chosen, s) {
			out = append(out, s)
		}
	}
	return out
}

const (
	// hedgeWindow is how many recent reads the delay is drawn from.
	hedgeWindow = 256
	// hedgeEvery is how many reads pass between updates of the delay.
	hedgeEvery = 32
	// hedgeEarn: a read that has its stripes within the delay earns
	// 1/hedgeEarn of a second request.
	hedgeEarn = 20
	// hedgeMax is the most second requests the budget holds.
	hedgeMax = 5
)

// hedger is one reader's delay and budget for second requests, kept as
// FoundationDB's load balancer keeps them. The delay is the 95th percentile
// of the time the reader's recent reads took to get k stripes, and no less
// than a floor. The budget counts twentieths of a request: a read that had
// its stripes within the delay adds one, and a second request takes twenty.
// So when every holder is slow nothing refills it, and the reader stops
// asking for more.
type hedger struct {
	floor time.Duration

	mu     sync.Mutex
	wait   time.Duration
	budget int
	seen   int
	recent [hedgeWindow]time.Duration
	sorted [hedgeWindow]time.Duration
}

func newHedger(floor time.Duration) *hedger { return &hedger{floor: floor, wait: floor} }

func (h *hedger) delay() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wait
}

// take spends one second request, if the budget holds one.
func (h *hedger) take() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.budget < hedgeEarn {
		return false
	}
	h.budget -= hedgeEarn
	return true
}

// done records a read that got k stripes in took. waited says the delay
// passed before they came.
func (h *hedger) done(took time.Duration, waited bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !waited {
		h.budget = min(h.budget+1, hedgeMax*hedgeEarn)
	}
	h.recent[h.seen%hedgeWindow] = took
	h.seen++
	if h.seen%hedgeEvery != 0 {
		return
	}
	n := min(h.seen, hedgeWindow)
	s := h.sorted[:n]
	copy(s, h.recent[:n])
	slices.Sort(s)
	h.wait = max(s[(n*95+99)/100-1], h.floor)
}
