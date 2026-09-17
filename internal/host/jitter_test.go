package host

import (
	"slices"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// A jittered wait is within an eighth of the interval either side of it, so the
// interval is what a deployment gets on average — only shortening would make it
// checkpoint more often than it asked to — and the spread is enough to break
// the lockstep of loops that started together. Across many draws the waits
// differ.
func TestJitteredWaitIsSymmetricWithinAnEighthOfTheInterval(t *testing.T) {
	const interval = 60 * time.Second
	const spread = interval / 8
	seen := make(map[time.Duration]bool)
	for range 1000 {
		wait := jittered(platform.SystemEntropy(), interval)
		if wait < interval-spread || wait >= interval+spread {
			t.Fatalf("jittered wait %s, want within [%s, %s)", wait, interval-spread, interval+spread)
		}
		seen[wait] = true
	}
	if len(seen) < 2 {
		t.Fatalf("1000 jittered waits were all %v", seen)
	}
}

// An interval too short to divide is waited out as it is, rather than asking
// for a random number below one, which is what a test driving its loops at ten
// milliseconds does to this.
func TestAnIntervalTooShortToDivideIsNotJittered(t *testing.T) {
	for _, interval := range []time.Duration{time.Nanosecond, 4 * time.Nanosecond, 7 * time.Nanosecond} {
		if wait := jittered(platform.SystemEntropy(), interval); wait != interval {
			t.Fatalf("jittered(%s) = %s, want the interval itself", interval, wait)
		}
	}
}

// A simulation must be able to reproduce which VM checkpointed first: the
// spread is what decides it, and math/rand decided it from a source no seed
// reaches. Drawn from the seeded entropy instead, one seed produces one
// sequence of waits and another seed produces a different one — still spread,
// still inside the interval's eighth.
func TestJitterIsReproducibleFromSeededEntropy(t *testing.T) {
	const interval = 60 * time.Second
	const spread = interval / 8
	waits := func(seed uint64) []time.Duration {
		entropy := sim.New(sim.Config{Seed: seed}).NewEntropy("host/interval")
		var drawn []time.Duration
		for range 8 {
			wait := jittered(entropy, interval)
			if wait < interval-spread || wait >= interval+spread {
				t.Fatalf("jittered wait %s, want within [%s, %s)", wait, interval-spread, interval+spread)
			}
			drawn = append(drawn, wait)
		}
		return drawn
	}
	if first, again := waits(5), waits(5); !slices.Equal(first, again) {
		t.Fatalf("one seed drew %v then %v", first, again)
	}
	if first, other := waits(5), waits(6); slices.Equal(first, other) {
		t.Fatalf("two seeds drew the same waits: %v", first)
	}
	if drawn := waits(5); drawn[0] == drawn[1] {
		t.Fatalf("eight draws of one seed never spread: %v", drawn)
	}
}
