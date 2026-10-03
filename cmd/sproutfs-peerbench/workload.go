package main

// This file is the workload alone, with nothing of the peer server in it, so
// the GCE script can build the same workload against the release before as
// well (scripts/bench-peer-gce.sh).

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// benchClient is one destination's view of the source: a guest fault for one
// page, and one request of the post-copy stream. Busy is the source at its
// budget for this host, which both ask again after a backoff.
type benchClient interface {
	fault(ctx context.Context, region int, page uint64) (busy bool, err error)
	stream(ctx context.Context, region int, page uint64) (busy bool, err error)
	// streamConcurrency is how many stream requests each region has in flight,
	// as a received VM's memory region does.
	streamConcurrency() int
}

// stripeClient reads one stripe of a window, as a restore faulting from the
// cluster's cache does.
type stripeClient interface {
	readStripe(ctx context.Context, n uint64) error
}

type benchConfig struct {
	Case       string
	Regions    int
	Pages      uint64
	PageBytes  int
	Duration   time.Duration
	FaultEvery time.Duration
	Stream     bool
	// StripesPerSecond is the rate of stripe reads; zero reads none.
	StripesPerSecond int
}

// caseResult is one case's record. Latencies are in microseconds.
type caseResult struct {
	Case       string             `json:"case"`
	Faults     int                `json:"faults"`
	Fault      map[string]float64 `json:"fault_us"`
	FaultBusy  int64              `json:"fault_busy"`
	StreamMBps float64            `json:"stream_mbps"`
	StreamBusy int64              `json:"stream_busy"`
	// ClientCPU is the CPUs the destination's process kept busy while faults
	// were timed.
	ClientCPU float64            `json:"client_cpus"`
	Stripes   int                `json:"stripes"`
	Stripe    map[string]float64 `json:"stripe_us,omitempty"`
	// Errors is every failed ask, each asked again.
	Errors      int64    `json:"errors"`
	FirstErrors []string `json:"first_errors,omitempty"`
}

const (
	busyFirst = time.Millisecond
	busyMax   = 100 * time.Millisecond
	// warmUp lets the stream fill the link before any fault is timed.
	warmUp = 2 * time.Second
)

// recorder keeps a case's latencies and errors.
type recorder struct {
	mu          sync.Mutex
	faults      []time.Duration
	stripes     []time.Duration
	errors      atomic.Int64
	firstErrors []string
}

func (r *recorder) fail(what string, err error) {
	r.errors.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.firstErrors) < 5 {
		r.firstErrors = append(r.firstErrors, fmt.Sprintf("%s: %v", what, err))
	}
}

// retry asks until it is answered, backing off as a destination does for a
// page only the source holds: after a busy answer, which it counts, and after a
// failure, which it records.
func (r *recorder) retry(ctx context.Context, what string, busy *atomic.Int64, ask func() (bool, error)) error {
	delay := busyFirst
	for {
		wasBusy, err := ask()
		switch {
		case ctx.Err() != nil:
			return context.Cause(ctx)
		case err != nil:
			r.fail(what, err)
		case !wasBusy:
			return nil
		default:
			busy.Add(1)
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(delay):
		}
		delay = min(2*delay, busyMax)
	}
}

func runCase(ctx context.Context, client benchClient, stripes stripeClient, config benchConfig) (caseResult, error) {
	if config.StripesPerSecond > 0 && stripes == nil {
		return caseResult{}, errors.New("this build reads no stripes")
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	var record recorder
	var faultBusy, streamBusy, streamed atomic.Int64
	var background sync.WaitGroup
	if config.Stream {
		for region := range config.Regions {
			for worker := range client.streamConcurrency() {
				background.Go(func() {
					step := uint64(client.streamConcurrency())
					for page := uint64(worker); ctx.Err() == nil; page = (page + step) % config.Pages {
						if err := record.retry(ctx, "stream", &streamBusy, func() (bool, error) {
							return client.stream(ctx, region, page)
						}); err != nil {
							return
						}
						streamed.Add(int64(config.PageBytes))
					}
				})
			}
		}
	}
	if config.StripesPerSecond > 0 {
		background.Go(func() {
			ticker := time.NewTicker(time.Second / time.Duration(config.StripesPerSecond))
			defer ticker.Stop()
			var reads sync.WaitGroup
			defer reads.Wait()
			for n := uint64(0); ; n++ {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				reads.Go(func() {
					began := time.Now()
					if err := stripes.readStripe(ctx, n); err != nil {
						if ctx.Err() == nil {
							record.fail("stripe", err)
						}
						return
					}
					took := time.Since(began)
					record.mu.Lock()
					record.stripes = append(record.stripes, took)
					record.mu.Unlock()
				})
			}
		})
	}
	select {
	case <-ctx.Done():
		return caseResult{}, context.Cause(ctx)
	case <-time.After(warmUp):
	}
	began, before, cpuBefore := time.Now(), streamed.Load(), processCPU()
	var faulting sync.WaitGroup
	for region := range config.Regions {
		faulting.Go(func() {
			random := rand.New(rand.NewPCG(uint64(region), 7))
			ticker := time.NewTicker(config.FaultEvery)
			defer ticker.Stop()
			for time.Since(began) < config.Duration {
				<-ticker.C
				page := random.Uint64N(config.Pages)
				asked := time.Now()
				if err := record.retry(ctx, "fault", &faultBusy, func() (bool, error) {
					return client.fault(ctx, region, page)
				}); err != nil {
					return
				}
				took := time.Since(asked)
				record.mu.Lock()
				record.faults = append(record.faults, took)
				record.mu.Unlock()
			}
		})
	}
	faulting.Wait()
	elapsed := time.Since(began)
	rate := float64(streamed.Load()-before) / elapsed.Seconds() / 1e6
	cpus := (processCPU() - cpuBefore).Seconds() / elapsed.Seconds()
	stop()
	background.Wait()
	record.mu.Lock()
	defer record.mu.Unlock()
	result := caseResult{Case: config.Case, Faults: len(record.faults), Fault: percentiles(record.faults),
		FaultBusy: faultBusy.Load(), StreamMBps: rate, StreamBusy: streamBusy.Load(), ClientCPU: cpus,
		Errors: record.errors.Load(), FirstErrors: record.firstErrors}
	if config.StripesPerSecond > 0 {
		result.Stripes, result.Stripe = len(record.stripes), percentiles(record.stripes)
	}
	return result, nil
}

// percentiles is the median, the tail and the worst of latencies, in
// microseconds.
func percentiles(latencies []time.Duration) map[string]float64 {
	if len(latencies) == 0 {
		return nil
	}
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	at := func(q float64) float64 {
		return float64(sorted[min(len(sorted)-1, int(q*float64(len(sorted))))].Nanoseconds()) / 1e3
	}
	var total time.Duration
	for _, latency := range sorted {
		total += latency
	}
	return map[string]float64{"p50": at(0.50), "p90": at(0.90), "p99": at(0.99), "p999": at(0.999),
		"max":  float64(sorted[len(sorted)-1].Nanoseconds()) / 1e3,
		"mean": float64(total.Nanoseconds()) / float64(len(sorted)) / 1e3}
}

// processCPU is the user and system time this process has used.
func processCPU() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// noisePages is a pool of pages of seeded noise, which no compression shrinks,
// so what crosses the link is every byte of every page.
func noisePages(count, pageBytes int) [][]byte {
	pages := make([][]byte, count)
	for i := range pages {
		pages[i] = make([]byte, pageBytes)
		random := rand.NewChaCha8([32]byte{byte(i), 0x5a})
		_, _ = random.Read(pages[i])
	}
	return pages
}
