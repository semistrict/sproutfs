package sim_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// TestSimulatedClockElapsesOnlyWhenAdvanced: a virtual clock is what makes a
// deadline written in checkpoint intervals reachable at all. Nothing passes on
// its own, and the wait ends exactly at the instant it was armed for.
func TestSimulatedClockElapsesOnlyWhenAdvanced(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := sim.New(sim.Config{Seed: 3}).NewClock("host-0")
		slept := make(chan time.Time, 1)
		go func() {
			if err := clock.Sleep(t.Context(), time.Minute); err != nil {
				t.Error(err)
				return
			}
			slept <- clock.Now()
		}()
		synctest.Wait()
		if got := clock.Pending(); got != 1 {
			t.Fatalf("armed waits = %d, want 1", got)
		}
		if released := clock.Advance(59 * time.Second); released != 0 {
			t.Fatalf("released %d waits before the deadline", released)
		}
		synctest.Wait()
		select {
		case at := <-slept:
			t.Fatalf("the sleeper woke at %s, 59s into a one-minute wait", at)
		default:
		}
		if released := clock.Advance(time.Second); released != 1 {
			t.Fatalf("released %d waits at the deadline, want 1", released)
		}
		synctest.Wait()
		if got := (<-slept).Sub(sim.Epoch); got != time.Minute {
			t.Fatalf("the sleeper woke %s after the epoch, want 1m0s", got)
		}
		if got := clock.Pending(); got != 0 {
			t.Fatalf("a released wait stayed armed: %d", got)
		}
	})
}

// TestSimulatedClockSleepReportsCancellation: a cancelled sleep reports the
// cause and leaves nothing armed, so a retry loop cannot accumulate deadlines
// that a later advance would fire into a closed world.
func TestSimulatedClockSleepReportsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := sim.New(sim.Config{}).NewClock("host-0")
		cause := errors.New("the host is closing")
		ctx, cancel := context.WithCancelCause(t.Context())
		failed := make(chan error, 1)
		go func() { failed <- clock.Sleep(ctx, time.Hour) }()
		synctest.Wait()
		cancel(cause)
		if err := <-failed; !errors.Is(err, cause) {
			t.Fatalf("Sleep error = %v, want the cancellation cause", err)
		}
		synctest.Wait()
		if got := clock.Pending(); got != 0 {
			t.Fatalf("a cancelled sleep left %d waits armed", got)
		}
	})
}

// TestSimulatedClockAfterFuncRunsOnceAndStops: the two holds a host keeps are
// AfterFunc deadlines, and a release that beats one must actually disarm it.
func TestSimulatedClockAfterFuncRunsOnceAndStops(t *testing.T) {
	clock := sim.New(sim.Config{}).NewClock("host-0")
	fired := make(chan string, 4)
	expired := clock.AfterFunc(2*time.Minute, func() { fired <- "expired" })
	released := clock.AfterFunc(4*time.Minute, func() { fired <- "released" })
	if !released.Stop() {
		t.Fatal("stopping an armed deadline reported it was not armed")
	}
	if released.Stop() {
		t.Fatal("stopping a disarmed deadline reported it was armed")
	}
	clock.Advance(10 * time.Minute)
	clock.Settle()
	close(fired)
	var order []string
	for name := range fired {
		order = append(order, name)
	}
	if len(order) != 1 || order[0] != "expired" {
		t.Fatalf("callbacks ran %v, want only the deadline nothing stopped", order)
	}
	if expired.Stop() {
		t.Fatal("stopping a fired deadline reported it was still armed")
	}
}

// TestSimulatedClockTickerTicksEveryPeriod: the pager's verify interval is a
// ticker, so one advance across several periods must tick for each of them
// rather than collapse to one.
func TestSimulatedClockTickerTicksEveryPeriod(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := sim.New(sim.Config{}).NewClock("pager")
		ticker := clock.NewTicker(time.Second)
		defer ticker.Stop()
		var ticks []time.Duration
		for range 3 {
			clock.Advance(time.Second)
			synctest.Wait()
			ticks = append(ticks, (<-ticker.C()).Sub(sim.Epoch))
		}
		if got := len(ticks); got != 3 {
			t.Fatalf("delivered %d ticks over three periods, want 3", got)
		}
		if ticks[0] != time.Second || ticks[1] != 2*time.Second || ticks[2] != 3*time.Second {
			t.Fatalf("ticked at %v after the epoch, want 1s 2s 3s", ticks)
		}
	})
}

// TestSimulatedClockTimerResetRearms: a checkpoint loop rearms its interval
// timer on every turn, so a reset must move the deadline rather than add one.
func TestSimulatedClockTimerResetRearms(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := sim.New(sim.Config{}).NewClock("host-0")
		timer := clock.NewTimer(time.Minute)
		if !timer.Stop() {
			t.Fatal("stopping a pending timer reported it was not pending")
		}
		if timer.Reset(time.Second) {
			t.Fatal("resetting a stopped timer reported it was pending")
		}
		if got := clock.Pending(); got != 1 {
			t.Fatalf("a reset timer left %d waits armed, want 1", got)
		}
		clock.Advance(time.Second)
		synctest.Wait()
		if got := (<-timer.C()).Sub(sim.Epoch); got != time.Second {
			t.Fatalf("the reset timer fired %s after the epoch, want 1s", got)
		}
	})
}

// TestSimulatedClockReleasesSimultaneousDeadlinesInSeededOrder: two holds that
// expire at the same instant are released in an order the seed chooses, not the
// order they were armed in. A host that retires its parent's fork holds in
// arming order every time never meets the run where the last child's release
// arrives first.
func TestSimulatedClockReleasesSimultaneousDeadlinesInSeededOrder(t *testing.T) {
	order := func(seed uint64) string {
		clock := sim.New(sim.Config{Seed: seed}).NewClock("host-0")
		released := make(chan byte, 3)
		for _, name := range []byte("abc") {
			clock.AfterFunc(time.Minute, func() { released <- name })
		}
		clock.Advance(time.Minute)
		clock.Settle()
		close(released)
		var got []byte
		for name := range released {
			got = append(got, name)
		}
		return string(got)
	}
	// One advance runs the callbacks it released in the order it released them,
	// which is the order this asserts. Arming order is "abc" in every run.
	if got := order(1); got != "bca" {
		t.Fatalf("seed 1 released %q", got)
	}
	if got := order(2); got != "bac" {
		t.Fatalf("seed 2 released %q", got)
	}
	if got := order(1); got != "bca" {
		t.Fatalf("seed 1 released %q on a second run", got)
	}
}

// TestSimulatedEntropyIsReproducibleAndDistinct: a writer nonce must be
// unpredictable in a deployment and reproducible in a simulation, and two
// draws of one stream must still differ — two writers sharing a nonce would
// each read the other's lost write back as their own.
func TestSimulatedEntropyIsReproducibleAndDistinct(t *testing.T) {
	draw := func(seed uint64, id string, count int) [][]byte {
		entropy := sim.New(sim.Config{Seed: seed}).NewEntropy(id)
		var drawn [][]byte
		for range count {
			nonce := make([]byte, 16)
			entropy.Fill(nonce)
			drawn = append(drawn, nonce)
		}
		return drawn
	}
	first, again := draw(7, "control", 3), draw(7, "control", 3)
	for i := range first {
		if !bytes.Equal(first[i], again[i]) {
			t.Fatalf("draw %d differed between runs of one seed: %x %x", i, first[i], again[i])
		}
	}
	if bytes.Equal(first[0], first[1]) {
		t.Fatalf("two draws of one stream are the same nonce: %x", first[0])
	}
	if other := draw(7, "host", 1); bytes.Equal(first[0], other[0]) {
		t.Fatalf("two streams of one seed drew the same nonce: %x", first[0])
	}
	if other := draw(8, "control", 1); bytes.Equal(first[0], other[0]) {
		t.Fatalf("two seeds drew the same nonce: %x", first[0])
	}
}
