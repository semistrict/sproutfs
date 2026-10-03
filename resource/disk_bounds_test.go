package resource_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// The largest byte count a goal or a budget may name.
const largest = 1 << 60

// A goal and a budget are valid up to their bounds and not past them.
func TestGoalsAndBudgetsAreValidAtTheirBounds(t *testing.T) {
	for _, goal := range []resource.DiskGoal{{FreeBytes: largest}, {FreePercent: 99}, {UsedBytes: largest}} {
		if err := goal.Validate(); err != nil {
			t.Fatalf("%+v was refused: %v", goal, err)
		}
	}
	for _, goal := range []resource.DiskGoal{{FreeBytes: largest + 1}, {FreePercent: 100}, {UsedBytes: largest + 1}} {
		if err := goal.Validate(); !errors.Is(err, resource.ErrInvalid) {
			t.Fatalf("%+v returned %v, want %v", goal, err, resource.ErrInvalid)
		}
	}
	if err := (resource.WriteBudget{BytesPerDay: largest, BurstBytes: largest}).Validate(); err != nil {
		t.Fatalf("the largest budget was refused: %v", err)
	}
	for _, budget := range []resource.WriteBudget{{BytesPerDay: largest + 1, BurstBytes: 1},
		{BytesPerDay: 1, BurstBytes: largest + 1}, {BurstBytes: 1}, {BytesPerDay: 1}} {
		if err := budget.Validate(); !errors.Is(err, resource.ErrInvalid) {
			t.Fatalf("%+v returned %v, want %v", budget, err, resource.ErrInvalid)
		}
	}
}

// A goal names each thing it keeps, and only those.
func TestAGoalNamesWhatItKeeps(t *testing.T) {
	for _, test := range []struct {
		goal resource.DiskGoal
		want string
	}{
		{resource.DiskGoal{UsedBytes: 3}, "use 3 bytes"},
		{resource.DiskGoal{FreePercent: 2}, "free 2%"},
		{resource.DiskGoal{FreeBytes: 1, FreePercent: 2, UsedBytes: 3}, "free 1 bytes, free 2%, use 3 bytes"},
	} {
		if got := test.goal.String(); got != test.want {
			t.Fatalf("%+v is %q, want %q", test.goal, got, test.want)
		}
	}
}

// space is a filesystem reading a test sets by hand.
type space struct{ reading platform.FilesystemSpace }

func (s space) Space(context.Context) (platform.FilesystemSpace, error) { return s.reading, nil }

// A filesystem as large as a limiter counts, and wholly free, is a reading it
// takes. One larger than that is refused.
func TestTheLargestFilesystemIsRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l, err := resource.NewDiskLimiter(f.ctx, resource.DiskLimiterConfig{Clock: f.clock,
			Space: space{platform.FilesystemSpace{Total: largest, Available: largest}},
			Goal:  resource.DiskGoal{FreeBytes: 1}})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if status := l.Status(); status.TotalBytes != largest || status.AvailableBytes != largest {
			t.Fatalf("the limiter read %d of %d, want all of %d", status.AvailableBytes, status.TotalBytes, largest)
		}
		_, err = resource.NewDiskLimiter(f.ctx, resource.DiskLimiterConfig{Clock: f.clock,
			Space: space{platform.FilesystemSpace{Total: largest + 1, Available: 1}},
			Goal:  resource.DiskGoal{FreeBytes: 1}})
		if !errors.Is(err, resource.ErrDiskReading) {
			t.Fatalf("a filesystem past the largest was read with %v, want %v", err, resource.ErrDiskReading)
		}
	})
}

// A limiter takes its defaults for what its configuration leaves at zero,
// and a band of the whole headroom keeps the cache at nothing.
func TestALimiterTakesItsDefaultsAndItsWidestBand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l, err := resource.NewDiskLimiter(f.ctx, resource.DiskLimiterConfig{Space: f.disk, Clock: f.clock,
			Goal: resource.DiskGoal{FreeBytes: 25 * unit}, BandPercent: 100})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if status := l.Status(); status.BandBytes != 175*unit || status.CacheShareBytes != 0 {
			t.Fatalf("a band of all the headroom is %d units and leaves the cache %d, want 175 and none",
				status.BandBytes/unit, status.CacheShareBytes)
		}
	})
}

// A user whose holding cannot be read is counted as holding nothing, and the
// reading says why.
func TestAUserWhoseHoldingCannotBeReadIsReported(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		failing := false
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Users: []resource.DiskUser{{Name: "spill", Promised: func() int64 { return 25 * unit },
				Allocated: func(context.Context) (int64, error) {
					if failing {
						return 0, platform.ErrDiskFailed
					}
					return unit, nil
				}}}})
		defer l.Close()
		failing = true
		if err := l.Refresh(f.ctx); !errors.Is(err, platform.ErrDiskFailed) {
			t.Fatalf("a reading of a user that cannot say what it holds returned %v, want %v", err, platform.ErrDiskFailed)
		}
		status := l.Status()
		want := resource.DiskPromise{Name: "spill", PromisedBytes: 25 * unit}
		if status.ReadError != "what spill holds: disk failed" || len(status.Promises) != 1 || status.Promises[0] != want {
			t.Fatalf("the limiter reports %q and %+v, want the reason and the promise counted whole",
				status.ReadError, status.Promises)
		}
	})
}

// A used goal smaller than the promises makes the host unready, and names
// itself as the goal that binds.
func TestAUsedGoalUnderThePromisesIsUnready(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{UsedBytes: 20 * unit},
			Users: []resource.DiskUser{spill("spill", 25*unit, f.sparse("spill", 25*unit))}})
		defer l.Close()
		if err := l.Ready(); !errors.Is(err, resource.ErrDiskPromises) {
			t.Fatalf("25 units promised under a goal of 20 is ready with %v, want %v", err, resource.ErrDiskPromises)
		}
		if got := chosen(l); got.Binding != resource.BindingUsedBytes || got.CacheShare != -5*unit {
			t.Fatalf("the limiter chose %+v, want the used goal binding at five units short", got)
		}
	})
}

// Promises that fill the room to the floor exactly still fit, with nothing
// left for the cache or a further promise.
func TestPromisesThatFitExactlyAreReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, 1, diskSpaceOf(100*unit))
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Users: []resource.DiskUser{
				spill("spill-ram", 25*unit, f.sparse("spill-ram", 25*unit)),
				spill("spill-ephemeral", 100*unit, f.sparse("spill-ephemeral", 100*unit)),
			}})
		defer l.Close()
		// 150 - 25 - 125 = 0.
		if err := l.Ready(); err != nil {
			t.Fatalf("promises that fit exactly are unready: %v", err)
		}
		if err := l.Fits(f.ctx, 0); err != nil {
			t.Fatalf("a promise of nothing was refused: %v", err)
		}
		if err := l.Fits(f.ctx, 1); !errors.Is(err, resource.ErrDiskPromises) {
			t.Fatalf("a promise of a byte returned %v, want %v", err, resource.ErrDiskPromises)
		}
		if got := l.CacheShare(); got != 0 {
			t.Fatalf("the cache's share is %d, want none", got)
		}
	})
}

// diskSpaceOf is the default disk with other writers holding outside bytes.
func diskSpaceOf(outside int64) sim.SpaceConfig {
	return sim.SpaceConfig{TotalBytes: diskTotal, OutsideBytes: outside}
}

// A write of nothing is admitted, and a priority out of range is the nearest
// one in range.
func TestTheWriteBudgetTakesEveryPriority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Writes: resource.WriteBudget{BytesPerDay: 100 * unit, BurstBytes: 40 * unit}, Device: &device{}})
		defer l.Close()
		if !l.Admit(0, 0) || !l.Admit(35*unit, 3) || !l.Admit(1, 9) || l.Admit(1, -2) {
			t.Fatal("the budget did not take a write of nothing, or read a priority out of range as the nearest")
		}
		if got := l.Status().Writes; got.Refused != [4]uint64{1, 0, 0, 0} || got.LeftBytes != 5*unit-1 {
			t.Fatalf("the budget is %+v, want one repair refused and five units less a byte left", got)
		}
	})
}

// A burst of several days refills at a day's budget a day, and a budget of
// nearly the largest refills whole after a long quiet.
func TestTheWriteBudgetRefillsOverDays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Writes: resource.WriteBudget{BytesPerDay: 10 * unit, BurstBytes: 40 * unit}, Device: &device{}})
		defer l.Close()
		if !l.Admit(40*unit, 3) {
			t.Fatal("a burst was refused")
		}
		f.clock.Advance(2 * 24 * time.Hour)
		if got := l.Status().Writes.LeftBytes; got != 20*unit {
			t.Fatalf("two days left %d units, want 20", got/unit)
		}

		widest := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Writes: resource.WriteBudget{BytesPerDay: largest - 9, BurstBytes: largest}, Device: &device{}})
		defer widest.Close()
		if !widest.Admit(largest, 3) {
			t.Fatal("the largest burst was refused")
		}
		f.clock.Advance(1001 * 24 * time.Hour)
		if got := widest.Status().Writes.LeftBytes; got != largest {
			t.Fatalf("a thousand and one days left %d, want %d", got, largest)
		}
	})
}

// A device whose counter starts at zero is charged from its first count.
func TestADeviceCountedFromZeroIsCharged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		counter := &device{}
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Writes: resource.WriteBudget{BytesPerDay: 100 * unit, BurstBytes: 40 * unit}, Device: counter})
		defer l.Close()
		counter.set(15 * unit)
		if err := l.Refresh(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := l.Status().Writes; got.LeftBytes != 25*unit || got.WrittenBytes != 15*unit {
			t.Fatalf("15 units written left %+v, want 25 units left of 15 written", got)
		}
	})
}
