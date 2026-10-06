package main

import (
	"context"
	"fmt"
	"hash/crc32"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// The ways a read walks a guest's memory.
const (
	// patternSequential reads every unit in order, as a restore's post-copy
	// stream does.
	patternSequential = "sequential"
	// patternRandom reads units in a random order, none twice, each read
	// independent of the others: faults whose addresses the guest knew
	// beforehand.
	patternRandom = "random"
	// patternChain reads one unit at a time, the next named by the bytes of
	// the one before: a guest following pointers, each fault known only once
	// the one before it is served.
	patternChain = "chain"
)

// The units a read takes.
const (
	// unitPage is one page.
	unitPage = "page"
	// unitRun is the fault run the page is in, as a pager's fault reads it.
	unitRun = "run"
	// unitFault is one page, faulted in through a real pager that reads the
	// faulting page first and the rest of its run behind it (pager.go).
	unitFault = "fault"
	// unitRunFirst is one page, faulted in through the same pager with every
	// fault reading its whole run before the page is installed, as faults did
	// before 2026-10-04.
	unitRunFirst = "runfirst"
)

// pagerUnit reports a unit read through a pager.
func pagerUnit(unit string) bool { return unit == unitFault || unit == unitRunFirst }

// access is how one case reads a guest's memory.
type access struct {
	Pattern string `json:"pattern"`
	Unit    string `json:"unit"`
	// Concurrency is how many reads are in flight at once, always one for a
	// chain.
	Concurrency int `json:"concurrency"`
	// Reads is how many units a random read or a chain reads. A sequential
	// read reads every unit.
	Reads int `json:"reads"`
	// Seed orders a random read and chooses where a chain starts, unless
	// Start names the unit it starts at.
	Seed  uint64  `json:"seed"`
	Start *uint64 `json:"start,omitempty"`
	// Pace is how long a chain waits before each hop, as a guest computes
	// between faults. The wait is not part of the hop's latency.
	Pace time.Duration `json:"pace_ns,omitempty"`
}

func (a access) check() error {
	switch {
	case a.Pattern != patternSequential && a.Pattern != patternRandom && a.Pattern != patternChain:
		return fmt.Errorf("a pattern %q: want %s, %s or %s", a.Pattern, patternSequential, patternRandom, patternChain)
	case a.Unit != unitPage && a.Unit != unitRun && !pagerUnit(a.Unit):
		return fmt.Errorf("a unit %q: want %s, %s, %s or %s", a.Unit, unitPage, unitRun, unitFault, unitRunFirst)
	case a.Concurrency < 1 || a.Pattern == patternChain && a.Concurrency != 1:
		return fmt.Errorf("%d reads at a time of a %s: want one or more, and one for a chain", a.Concurrency, a.Pattern)
	case a.Pattern != patternSequential && a.Reads < 1:
		return fmt.Errorf("%d reads of a %s: want one or more", a.Reads, a.Pattern)
	}
	return nil
}

// walked is what a walk read: the unit of each read in the order they began,
// how long each took, and the pages that read back wrong.
type walked struct {
	Units     []uint64 `json:"units"`
	Latencies []int64  `json:"latencies_ns"`
	Seconds   float64  `json:"seconds"`
	// Wrong is the pages whose bytes were not the guest's, and Failed the
	// reads that returned an error.
	Wrong  int `json:"wrong"`
	Failed int `json:"failed"`
	// Next is the unit a chain would have read next.
	Next uint64 `json:"next"`
}

// readBound is the longest one read of a walk may take.
const readBound = 2 * time.Minute

// reader reads dst from a guest's memory at offset.
type reader func(ctx context.Context, offset uint64, dst []byte) error

// walk reads g's memory through read as a says. It checks every page it read
// against the guest's, after the reads, so what a read is timed by is the
// read and a CRC-32C of what it read. A chain also ends early once stop is
// closed, with the hops it took; a nil stop never closes.
func walk(ctx context.Context, g *guest, a access, read reader, stop <-chan struct{}) (walked, error) {
	if err := a.check(); err != nil {
		return walked{}, err
	}
	units := g.units(a.Unit)
	unitBytes := g.unitPages(a.Unit) * g.pageSize
	reads := uint64(a.Reads)
	if a.Pattern == patternSequential {
		reads = units
	}
	if reads > units {
		return walked{}, fmt.Errorf("%d reads of %d units: a %s reads each unit at most once", reads, units, a.Pattern)
	}
	// Each read's slot is its own, so workers never write the same entry.
	out := walked{Units: make([]uint64, reads), Latencies: make([]int64, reads)}
	sums := make([]uint32, g.pages)
	readPages := make([]bool, g.pages)
	var failed atomic.Int64
	one := func(at, unit uint64, buffer []byte) error {
		out.Units[at] = unit
		start := time.Now()
		// A read past the bound fails the walk, rather than holding a run of
		// hosts for hours: on 2026-10-04 one GET of GCS waited 52 minutes for
		// a response its HTTP/2 stream never got.
		bounded, cancel := context.WithTimeout(ctx, readBound)
		err := read(bounded, unit*unitBytes, buffer)
		cancel()
		out.Latencies[at] = int64(time.Since(start))
		if err != nil {
			failed.Add(1)
			return err
		}
		first := unit * g.unitPages(a.Unit)
		for page := range g.unitPages(a.Unit) {
			sums[first+page] = crc32.Checksum(buffer[page*g.pageSize:][:g.pageSize], crc32c)
			readPages[first+page] = true
		}
		return nil
	}
	began := time.Now()
	switch a.Pattern {
	case patternChain:
		random := rand.New(rand.NewPCG(a.Seed, units))
		unit := random.Uint64N(units)
		if a.Start != nil {
			if unit = *a.Start; unit >= units {
				return walked{}, fmt.Errorf("a chain from unit %d of %d", unit, units)
			}
		}
		buffer := make([]byte, unitBytes)
		for at := range reads {
			stopped, err := beforeHop(ctx, stop, a.Pace)
			if err != nil {
				return walked{}, err
			}
			if stopped {
				out.Units, out.Latencies = out.Units[:at], out.Latencies[:at]
				break
			}
			if err := one(at, unit, buffer); err != nil {
				return walked{}, fmt.Errorf("hop %d of the chain, at unit %d: %w", at, unit, err)
			}
			unit = link(a.Unit, buffer)
			if unit >= units {
				return walked{}, fmt.Errorf("hop %d of the chain links to unit %d of %d", at, unit, units)
			}
		}
		out.Next = unit
	default:
		order := make([]uint64, units)
		for at := range order {
			order[at] = uint64(at)
		}
		if a.Pattern == patternRandom {
			random := rand.New(rand.NewPCG(a.Seed, units))
			random.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		}
		var next atomic.Uint64
		var workers sync.WaitGroup
		for range a.Concurrency {
			workers.Go(func() {
				buffer := make([]byte, unitBytes)
				for {
					at := next.Add(1) - 1
					if at >= reads {
						return
					}
					if err := one(at, order[at], buffer); err != nil {
						slog.WarnContext(ctx, "walk: a read failed", "unit", order[at], "error", err)
					}
				}
			})
		}
		workers.Wait()
	}
	out.Seconds = time.Since(began).Seconds()
	out.Failed = int(failed.Load())
	for page, read := range readPages {
		if read && sums[page] != g.sum(uint64(page)) {
			out.Wrong++
		}
	}
	return out, nil
}

// beforeHop waits pace before a chain's next hop, and reports whether stop
// closed first or is closed once the wait is over.
func beforeHop(ctx context.Context, stop <-chan struct{}, pace time.Duration) (bool, error) {
	if pace > 0 {
		timer := time.NewTimer(pace)
		defer timer.Stop()
		select {
		case <-stop:
			return true, nil
		case <-ctx.Done():
			return false, context.Cause(ctx)
		case <-timer.C:
		}
	}
	select {
	case <-stop:
		return true, nil
	default:
		return false, nil
	}
}
