package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

// The delay of a size class stays at its floor until the reader has heard 32
// reads of the class, and is then the 95th percentile of its last 256, never less
// than the floor. The budget starts with five requests, a read within the
// delay earns a twentieth of one up to five, and a read that waited earns
// nothing.
func TestTheHedgerFollowsItsReadsWithinItsBudget(t *testing.T) {
	const class = 5
	h := newHedger(time.Millisecond)
	for range hedgeEvery - 1 {
		h.done(class, 10*time.Millisecond, false)
	}
	if delay := h.delay(class); delay != time.Millisecond {
		t.Fatalf("after %d reads the delay is %v, want the floor", hedgeEvery-1, delay)
	}
	h.done(class, 10*time.Millisecond, false)
	if delay := h.delay(class); delay != 10*time.Millisecond {
		t.Fatalf("after %d reads of 10ms the delay is %v, want 10ms", hedgeEvery, delay)
	}
	// The last 256 reads take 1 to 256 ms; their 95th percentile is the
	// 244th smallest.
	for at := range hedgeWindow {
		h.done(class, time.Duration(at+1)*time.Millisecond, false)
	}
	if delay := h.delay(class); delay != 244*time.Millisecond {
		t.Fatalf("over reads of 1 to 256ms the delay is %v, want the 95th percentile, 244ms", delay)
	}
	for range hedgeEvery {
		h.done(class, time.Microsecond, false)
	}
	if delay := h.delay(class); delay != 244*time.Millisecond {
		t.Fatalf("after 32 reads of 1µs the delay is %v, want the 95th percentile of the last 256, 244ms", delay)
	}
	low := newHedger(time.Millisecond)
	for range hedgeEvery {
		low.done(class, time.Microsecond, false)
	}
	if delay := low.delay(class); delay != time.Millisecond {
		t.Fatalf("over reads of 1µs the delay is %v, want the floor", delay)
	}
	for at := range hedgeMax {
		if !h.take() {
			t.Fatalf("a full budget refused second request %d", at+1)
		}
	}
	if h.take() {
		t.Fatal("an empty budget gave a second request")
	}
	for range hedgeEarn - 1 {
		h.done(class, time.Millisecond, false)
	}
	h.done(class, time.Millisecond, true)
	if h.take() {
		t.Fatal("nineteen reads within the delay and one that waited earned a second request")
	}
	h.done(class, time.Millisecond, false)
	if !h.take() || h.take() {
		t.Fatal("twenty reads within the delay did not earn exactly one second request")
	}
}

// A read's size class is the bytes it asks for: up to 4 KiB, then each four
// times the last, up to 16 MiB and past it. A 4 KiB page, a segment, a 2 MiB
// page and a run of four of them are each of a class of their own.
func TestAReadIsOfTheClassOfTheBytesItAsksFor(t *testing.T) {
	for _, c := range []struct {
		bytes int64
		class int
	}{{0, 0}, {1, 0}, {4 << 10, 0}, {4<<10 + 1, 1}, {16 << 10, 1}, {64 << 10, 2}, {256 << 10, 3},
		{maximumSegmentSize, 4}, {PageSize2MiB, 5}, {4 << 20, 5}, {4 * PageSize2MiB, 6}, {16 << 20, 6},
		{1 << 30, 6}} {
		if class := readClass(c.bytes); class != c.class {
			t.Fatalf("a read of %d bytes is of class %d, want %d", c.bytes, class, c.class)
		}
	}
	for class, bytes := range []int64{4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20, 16 << 20} {
		if got := readClassBytes(class); got != bytes {
			t.Fatalf("class %d holds reads of up to %d bytes, want %d", class, got, bytes)
		}
	}
}

// Each class's delay is drawn from its own reads alone, and the budget is the
// reader's: 256 reads of 4 KiB at 1 ms leave the delay of 2 MiB reads at the
// 20 ms their own reads took, and a second request the small reads earned is
// spent on a large one. A class with fewer than 32 reads of its own takes the
// delay of the nearest class that has them, the larger first, and with none,
// the floor.
func TestEachSizeClassKeepsItsOwnDelay(t *testing.T) {
	small, large := readClass(4<<10), readClass(PageSize2MiB)
	h := newHedger(500 * time.Microsecond)
	if delay := h.delay(large); delay != 500*time.Microsecond {
		t.Fatalf("with no class learned the delay of a 2 MiB read is %v, want the floor", delay)
	}
	for range hedgeEvery {
		h.done(large, 20*time.Millisecond, false)
	}
	for h.take() {
	}
	for range hedgeWindow {
		h.done(small, time.Millisecond, false)
	}
	if delay := h.delay(large); delay != 20*time.Millisecond {
		t.Fatalf("after 256 reads of 4 KiB the delay of a 2 MiB read is %v, want its own 20ms", delay)
	}
	if reads, delay := h.state(small); reads != hedgeWindow || delay != time.Millisecond {
		t.Fatalf("after 256 reads of 4 KiB at 1ms their class holds %d reads and a delay of %v, want 256 and 1ms",
			reads, delay)
	}
	for _, c := range []struct {
		bytes int64
		delay time.Duration
	}{{16 << 10, time.Millisecond}, {64 << 10, time.Millisecond}, {256 << 10, 20 * time.Millisecond},
		{maximumSegmentSize, 20 * time.Millisecond}, {4 * PageSize2MiB, 20 * time.Millisecond}} {
		if reads, delay := h.state(readClass(c.bytes)); reads != 0 || delay != c.delay {
			t.Fatalf("a class of %d bytes no read was of holds %d reads and a delay of %v, want none and %v",
				c.bytes, reads, delay, c.delay)
		}
	}
	tie := newHedger(500 * time.Microsecond)
	for range hedgeEvery {
		tie.done(readClass(4<<10), time.Millisecond, false)
		tie.done(readClass(maximumSegmentSize), 7*time.Millisecond, false)
	}
	if delay := tie.delay(readClass(64 << 10)); delay != 7*time.Millisecond {
		t.Fatalf("a class two from a learned class either way has a delay of %v, want the larger's 7ms", delay)
	}
	if !h.take() {
		t.Fatal("the budget small reads earned refused a large read's second request")
	}
}

// A probe goes half as long again after each one that failed, up to a minute,
// each moved by up to a tenth either way by a hash of the host and the
// attempt, so the readers probing one host do not arrive together.
func TestProbesWaitHalfAsLongAgainUpToAMinute(t *testing.T) {
	wait := DefaultProbeFirst
	var waits []time.Duration
	for range 7 {
		waits = append(waits, wait)
		wait = nextProbeWait(wait, DefaultProbeMax)
	}
	want := []time.Duration{10 * time.Second, 15 * time.Second, 22500 * time.Millisecond, 33750 * time.Millisecond,
		50625 * time.Millisecond, time.Minute, time.Minute}
	if !slices.Equal(waits, want) {
		t.Fatalf("probes wait %v, want %v", waits, want)
	}
	seen := map[time.Duration]bool{}
	for host := range 1000 {
		identity := rank.Identity{byte(host), byte(host >> 8), 7}
		for attempt := 1; attempt <= 3; attempt++ {
			spread := spreadProbe(identity, attempt, 10*time.Second)
			if spread < 9*time.Second || spread > 11*time.Second {
				t.Fatalf("host %d's probe %d waits %v, more than a tenth from 10s", host, attempt, spread)
			}
			if again := spreadProbe(identity, attempt, 10*time.Second); again != spread {
				t.Fatalf("host %d's probe %d waits %v and then %v", host, attempt, spread, again)
			}
			seen[spread] = true
		}
	}
	if len(seen) < 2900 {
		t.Fatalf("3000 probes of 1000 hosts wait only %d different times", len(seen))
	}
	if short := spreadProbe(rank.Identity{1}, 1, 5*time.Nanosecond); short != 5*time.Nanosecond {
		t.Fatalf("a wait too short to spread waits %v", short)
	}
}

// markedCache is a cache that follows a list of itself and five others, and
// reaches no peer.
func markedCache(t *testing.T) (*Cache, []rank.Cache) {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 1})
	ctx := sim.WithRuntime(t.Context(), runtime)
	file, err := runtime.NewDisk("host", sim.DiskConfig{}).Open(ctx, "cache", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	budget, err := resource.New(4 << 10)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := NewCache(ctx, budget, CacheConfig{Disk: file, DiskBytes: 64 << 20, DiskRegionBytes: 8 << 20,
		ClusterPercent: 100, Entropy: runtime.NewEntropy("host")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	caches := []rank.Cache{{Identity: cache.Identity(), Weight: 1, Address: "self"}}
	for at := range 5 {
		caches = append(caches, rank.Cache{Identity: rank.Identity{byte(at + 1), 0xee}, Weight: 1,
			Address: platform.Address(fmt.Sprintf("host-%d", at))})
	}
	list, err := rank.NewList(rank.Code{K: 4, M: 2}, caches)
	if err != nil {
		t.Fatal(err)
	}
	cache.FollowMembership(membership.NewFixed(servingOf(list)), cache.Identity())
	return cache, caches[1:]
}

// A host is marked down on its third stripe request in a row that timed out,
// and an answer between them starts the count again. A refused connection
// marks a host at once. A miss, BUSY, an answer for another cache and a
// request this host gave up on mark nothing. Past one host of six, a fifth of
// the list, no host is marked.
func TestAHostIsMarkedDownOnItsThirdTimeoutInARow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, others := markedCache(t)
		marks := &cache.reader.marks
		ctx := t.Context()
		first, second, third := others[0], others[1], others[2]
		marks.observe(ctx, first, errStripeTimeout, false)
		marks.observe(ctx, first, errStripeTimeout, false)
		marks.observe(ctx, first, nil, true)
		marks.observe(ctx, first, errStripeTimeout, false)
		marks.observe(ctx, first, errStripeTimeout, false)
		if marks.isDown(first.Identity) {
			t.Fatal("a host was marked down after two timeouts in a row")
		}
		for _, err := range []error{peer.ErrBusy, peer.ErrNotMe, context.Canceled, errAnswerLost} {
			marks.observe(ctx, second, err, false)
		}
		if marks.isDown(second.Identity) {
			t.Fatal("a host was marked down for a request that failed for no fault of its own")
		}
		marks.observe(ctx, first, errStripeTimeout, false)
		if !marks.isDown(first.Identity) {
			t.Fatal("a host was not marked down on its third timeout in a row")
		}
		marks.observe(ctx, third, fmt.Errorf("dialing: %w", peer.ErrDown), false)
		if marks.isDown(third.Identity) {
			t.Fatal("a second host of six was marked down")
		}
		stats := cache.reader.statistics()
		if stats.Timeouts != 5 || stats.MarkedDown != 1 || stats.Capped != 1 || stats.Down != 1 {
			t.Fatalf("the marks came to %+v, want five timeouts, one host down and one mark refused", stats)
		}
	})
}

// One refused connection marks a host down at once, while there is room under
// the fifth.
func TestOneRefusedConnectionMarksAHostDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, others := markedCache(t)
		cache.reader.marks.observe(t.Context(), others[0], fmt.Errorf("dialing: %w", peer.ErrDown), false)
		if !cache.reader.marks.isDown(others[0].Identity) {
			t.Fatal("a refused connection did not mark its host down")
		}
		if errors.Is(errStripeTimeout, peer.ErrDown) || refused(errStripeTimeout) {
			t.Fatal("a timeout reads as a refused connection")
		}
	})
}
