package resource_test

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/resource"
)

// shareOf is what a limiter chose, in the words a status reports it.
type shareOf struct {
	Binding                 resource.DiskBinding
	Floor, Band, CacheShare int64
}

func chosen(l *resource.DiskLimiter) shareOf {
	s := l.Status()
	return shareOf{Binding: s.Binding, Floor: s.FloorBytes, Band: s.BandBytes, CacheShare: s.CacheShareBytes}
}

// Every goal and combination of goals, on a disk of 250 units with 50 held by
// other writers and a spill file promised 25. The room is 200 units: the hard
// share is the room less the floor and the promises, the band a fifth of what
// the cache has left of it, and the used goal caps the promises and the cache
// together. The strictest goal binds.
func TestEachGoalSetsTheCacheShare(t *testing.T) {
	for _, test := range []struct {
		name string
		goal resource.DiskGoal
		want shareOf
	}{
		// 200 - 25 - 25 = 150, less a band of 30.
		{"free bytes", resource.DiskGoal{FreeBytes: 25 * unit},
			shareOf{resource.BindingFreeBytes, 25 * unit, 30 * unit, 120 * unit}},
		// 20 % of 250 is 50: 200 - 50 - 25 = 125, less a band of 25.
		{"free percent", resource.DiskGoal{FreePercent: 20},
			shareOf{resource.BindingFreePercent, 50 * unit, 25 * unit, 100 * unit}},
		// 100 - 25. The filesystem alone would leave 175 less a band of 35.
		{"used bytes", resource.DiskGoal{UsedBytes: 100 * unit},
			shareOf{resource.BindingUsedBytes, 0, 35 * unit, 75 * unit}},
		{"free bytes under free percent", resource.DiskGoal{FreeBytes: 25 * unit, FreePercent: 20},
			shareOf{resource.BindingFreePercent, 50 * unit, 25 * unit, 100 * unit}},
		// 200 - 60 - 25 = 115, less a band of 23.
		{"free percent under free bytes", resource.DiskGoal{FreeBytes: 60 * unit, FreePercent: 20},
			shareOf{resource.BindingFreeBytes, 60 * unit, 23 * unit, 92 * unit}},
		{"free percent over used bytes", resource.DiskGoal{FreePercent: 20, UsedBytes: 100 * unit},
			shareOf{resource.BindingUsedBytes, 50 * unit, 25 * unit, 75 * unit}},
		// The two floors tie at 25, and the bytes goal is named.
		// The used goal leaves 145 - 25 = 120, as the free goal does, and the
		// free goal is named.
		{"a tie", resource.DiskGoal{FreeBytes: 25 * unit, UsedBytes: 145 * unit},
			shareOf{resource.BindingFreeBytes, 25 * unit, 30 * unit, 120 * unit}},
		{"all three", resource.DiskGoal{FreeBytes: 25 * unit, FreePercent: 10, UsedBytes: 200 * unit},
			shareOf{resource.BindingFreeBytes, 25 * unit, 30 * unit, 120 * unit}},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := defaultDisk(t)
				l := f.limiter(resource.DiskLimiterConfig{Goal: test.goal,
					Users: []resource.DiskUser{spill("spill", 25*unit, f.sparse("spill", 25*unit))}})
				defer l.Close()
				if got := chosen(l); got != test.want {
					t.Fatalf("the limiter chose %+v, want %+v", got, test.want)
				}
				if got := l.CacheShare(); got != test.want.CacheShare {
					t.Fatalf("the cache's share is %d, want %d", got, test.want.CacheShare)
				}
				if err := l.Ready(); err != nil {
					t.Fatal(err)
				}
				if got := f.runtime.Probes()[resource.ProbeDiskUnready]; got != 0 {
					t.Fatalf("a host whose promises fit marked itself unready %d times", got)
				}
			})
		})
	}
}

// When another writer fills the disk, each goal's share falls with it once the
// smoothed readings reach the disk as it is, and comes back when the writer
// lets go. A used goal stops binding when the disk itself leaves less.
func TestTheShareFollowsTheDiskFilledFromOutside(t *testing.T) {
	for _, test := range []struct {
		name         string
		goal         resource.DiskGoal
		before, full shareOf
	}{
		// The room falls from 200 to 100: 100 - 25 - 25 = 50, less 10.
		{"free bytes", resource.DiskGoal{FreeBytes: 25 * unit},
			shareOf{resource.BindingFreeBytes, 25 * unit, 30 * unit, 120 * unit},
			shareOf{resource.BindingFreeBytes, 25 * unit, 10 * unit, 40 * unit}},
		// 100 - 50 - 25 = 25, less 5.
		{"free percent", resource.DiskGoal{FreePercent: 20},
			shareOf{resource.BindingFreePercent, 50 * unit, 25 * unit, 100 * unit},
			shareOf{resource.BindingFreePercent, 50 * unit, 5 * unit, 20 * unit}},
		// 100 - 25 = 75, less a band of 15, is under the used goal's 75.
		{"used bytes", resource.DiskGoal{UsedBytes: 100 * unit},
			shareOf{resource.BindingUsedBytes, 0, 35 * unit, 75 * unit},
			shareOf{resource.BindingFilesystem, 0, 15 * unit, 60 * unit}},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := defaultDisk(t)
				l := f.limiter(resource.DiskLimiterConfig{Goal: test.goal,
					Users: []resource.DiskUser{spill("spill", 25*unit, f.sparse("spill", 25*unit))}})
				defer l.Close()
				if got := chosen(l); got != test.before {
					t.Fatalf("before the disk filled the limiter chose %+v, want %+v", got, test.before)
				}
				f.disk.SetOutsideBytes(150 * unit)
				f.converge()
				if got := chosen(l); got != test.full {
					t.Fatalf("with the disk filled from outside the limiter chose %+v, want %+v", got, test.full)
				}
				if status := l.Status(); status.AvailableBytes != 100*unit || status.SmoothFreeBytes != 100*unit {
					t.Fatalf("the limiter read %d available and smoothed %d free, want 100 units of each",
						status.AvailableBytes, status.SmoothFreeBytes)
				}
				f.disk.SetOutsideBytes(diskOutside)
				f.converge()
				if got := chosen(l); got != test.before {
					t.Fatalf("with the other writer gone the limiter chose %+v, want %+v", got, test.before)
				}
			})
		})
	}
}

// The capacity a host weighs its cache by is the filesystem less the floor
// and the promises, under the used goal: 250 - 25 - 25 under a floor of 25
// units, 250 - 50 - 25 under a fifth kept free, and 100 - 25 under a cap of
// 100 used. Another writer filling the disk moves the cache's share and not
// the capacity, because every change of a weight moves windows between hosts.
func TestTheCapacityDoesNotMoveAsTheDiskFills(t *testing.T) {
	for _, test := range []struct {
		name                  string
		goal                  resource.DiskGoal
		capacity, share, full int64
	}{
		{"free bytes", resource.DiskGoal{FreeBytes: 25 * unit}, 200 * unit, 120 * unit, 40 * unit},
		{"free percent", resource.DiskGoal{FreePercent: 20}, 175 * unit, 100 * unit, 20 * unit},
		{"used bytes", resource.DiskGoal{UsedBytes: 100 * unit}, 75 * unit, 75 * unit, 60 * unit},
		{"all three", resource.DiskGoal{FreeBytes: 25 * unit, FreePercent: 20, UsedBytes: 100 * unit},
			75 * unit, 75 * unit, 20 * unit},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := defaultDisk(t)
				l := f.limiter(resource.DiskLimiterConfig{Goal: test.goal,
					Users: []resource.DiskUser{spill("spill", 25*unit, f.sparse("spill", 25*unit))}})
				defer l.Close()
				if got := l.CacheShare(); got != test.share {
					t.Fatalf("the share is %d units, want %d", got/unit, test.share/unit)
				}
				if got := l.Capacity(); got != test.capacity {
					t.Fatalf("the capacity is %d units, want %d", got/unit, test.capacity/unit)
				}
				f.disk.SetOutsideBytes(150 * unit)
				f.converge()
				if got := l.CacheShare(); got != test.full {
					t.Fatalf("with the disk filled from outside the share is %d units, want %d",
						got/unit, test.full/unit)
				}
				if got := l.Capacity(); got != test.capacity {
					t.Fatalf("with the disk filled from outside the capacity is %d units, want %d",
						got/unit, test.capacity/unit)
				}
			})
		})
	}
}

// A spill file is counted at its promise while it is sparse, and writing into
// it changes nothing: what it takes from the free space it adds to what the
// host holds.
func TestASpillFileCountsAtItsPromiseWhileSparse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		file := f.sparse("spill", 25*unit)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Users: []resource.DiskUser{spill("spill", 25*unit, file)}})
		defer l.Close()
		if got := l.CacheShare(); got != 120*unit {
			t.Fatalf("with the spill file sparse the cache's share is %d units, want 120", got/unit)
		}
		if _, err := file.WriteAt(f.ctx, region, 3*unit); err != nil {
			t.Fatal(err)
		}
		f.tick(1)
		if got := l.CacheShare(); got != 120*unit {
			t.Fatalf("with a unit spilled the cache's share is %d units, want 120", got/unit)
		}
		if got := l.Status().SmoothFreeBytes; got != 199*unit {
			t.Fatalf("with a unit spilled the limiter counts %d units free, want 199", got/unit)
		}
		want := []resource.DiskPromise{{Name: "spill", PromisedBytes: 25 * unit, AllocatedBytes: unit}}
		if got := l.Status().Promises; !slices.Equal(got, want) {
			t.Fatalf("the limiter counts %+v, want %+v", got, want)
		}
	})
}

// Promises the disk cannot keep make the host unready, with the reason, and
// refuse every further promise. The limiter takes nothing from a spill file to
// make them fit: the promises stay counted whole. When the disk frees, the host
// is ready again.
func TestPromisesThatDoNotFitMakeTheHostUnready(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Users: []resource.DiskUser{
				spill("spill-ram", 25*unit, f.sparse("spill-ram", 25*unit)),
				spill("spill-ephemeral", 100*unit, f.sparse("spill-ephemeral", 100*unit)),
			}})
		defer l.Close()
		// 200 - 25 - 125 = 50.
		if err := l.Fits(f.ctx, 50*unit); err != nil {
			t.Fatalf("a promise of the 50 units left was refused: %v", err)
		}
		if err := l.Fits(f.ctx, 50*unit+1); !errors.Is(err, resource.ErrDiskPromises) {
			t.Fatalf("a promise of a byte more than is left returned %v, want %v", err, resource.ErrDiskPromises)
		}
		f.disk.SetOutsideBytes(150 * unit)
		f.converge()
		// 100 - 25 - 125 = -50.
		err := l.Ready()
		want := "the disk cannot keep the host's promises: the host promises 51200000 bytes to spill-ram, " +
			"spill-ephemeral, and the free-bytes goal (free 10240000 bytes) leaves it 30720000"
		if err == nil || err.Error() != want {
			t.Fatalf("the host with its promises over the disk is ready with %v, want %q", err, want)
		}
		if err := l.Fits(f.ctx, 0); !errors.Is(err, resource.ErrDiskPromises) {
			t.Fatalf("an unready host's promise returned %v, want %v", err, resource.ErrDiskPromises)
		}
		if status := l.Status(); status.PromisedBytes != 125*unit || l.CacheShare() != 0 {
			t.Fatalf("the unready host counts %d units promised and gives the cache %d, want 125 and none",
				status.PromisedBytes/unit, l.CacheShare())
		}
		f.disk.SetOutsideBytes(diskOutside)
		f.converge()
		if err := l.Ready(); err != nil {
			t.Fatalf("with the disk freed the host is still unready: %v", err)
		}
	})
}

// Promises are feasible while the goals would keep them on this filesystem
// with nothing else on it, whatever other writers hold now: a host that is
// unready because another writer filled the disk can wait for it, and one
// whose promises the filesystem could never keep cannot.
func TestPromisesAreFeasibleWhileOnlyOtherWritersLeaveNoRoom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Users: []resource.DiskUser{spill("spill", 125*unit, f.sparse("spill", 125*unit))}})
		defer l.Close()
		f.disk.SetOutsideBytes(150 * unit)
		f.converge()
		if err := l.Ready(); !errors.Is(err, resource.ErrDiskPromises) {
			t.Fatalf("a host with the disk filled from outside is ready with %v, want %v", err, resource.ErrDiskPromises)
		}
		// 250 - 25 - 125 = 100.
		if err := l.Feasible(); err != nil {
			t.Fatalf("promises the empty filesystem keeps are not feasible: %v", err)
		}
		for _, test := range []struct {
			name string
			goal resource.DiskGoal
			want string
		}{
			// 250 - 50 - 225 = -25.
			{"free percent", resource.DiskGoal{FreePercent: 20},
				"the disk cannot keep the host's promises: the host promises 92160000 bytes, " +
					"and the goals (free 20%) leave it 81920000 of a filesystem of 102400000"},
			// 200 - 225 = -25.
			{"used bytes", resource.DiskGoal{UsedBytes: 200 * unit},
				"the disk cannot keep the host's promises: the host promises 92160000 bytes, " +
					"and the goals (use 81920000 bytes) leave it 81920000 of a filesystem of 102400000"},
		} {
			other := f.limiter(resource.DiskLimiterConfig{Goal: test.goal,
				Users: []resource.DiskUser{{Name: "staging", Promised: func() int64 { return 225 * unit }}}})
			err := other.Feasible()
			other.Close()
			if err == nil || err.Error() != test.want || !errors.Is(err, resource.ErrDiskPromises) {
				t.Fatalf("%s: promises of 225 units were judged %v, want %q", test.name, err, test.want)
			}
		}
	})
}

// Two hosts share a node's disk, each with the same goals: a floor of 30 units
// and a reserve of 10 above it. The first host's cache has filled the disk to
// the reserve when the second restarts. Its spill file fits in the floor and
// the reserve, so it is allocated whole; the second host is then unready, and
// the first host's cache gives back what its goals now ask for, a few regions
// at each reading, until the second host is ready. The reserve is then free
// again, so the second host, whose cache is empty, can still promise a VM's
// staging. Neither spill file loses a byte.
func TestARestartedHostFindsRoomAnotherHostsCacheGivesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		host := func(name string) resource.DiskLimiterConfig {
			t.Helper()
			file, err := f.disk.Open(f.ctx, name, platform.OpenOptions{Create: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := file.(platform.AllocatingFile).Allocate(f.ctx, 0, 25*unit); err != nil {
				t.Fatal(err)
			}
			return resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 30 * unit}, ReserveBytes: 10 * unit,
				Users: []resource.DiskUser{spill(name, 25*unit, file)}}
		}
		firstConfig := host("spill-a")
		first := f.limiter(firstConfig)
		defer first.Close()
		// The cache could hold 250 - 30 - 10 - 25 if nothing else wrote to
		// the disk: the reserve is never the cache's.
		if got := first.Capacity(); got != 185*unit {
			t.Fatalf("the first host's capacity is %d units, want 185", got/unit)
		}
		cache := f.cache()
		unregister, err := first.RegisterCache(cache)
		if err != nil {
			t.Fatal(err)
		}
		defer unregister()
		// 250 - 50 - 25 - 30 - 10 leaves the cache 135; the band takes a
		// fifth of what is left of it, so the cache stops at 134 with 41 free.
		cache.fill(t, first)
		if got, free := cache.Held(), f.disk.Usage().FreeBytes(); got != 134*unit || free != 41*unit {
			t.Fatalf("the first host's cache filled to %d units with %d free, want 134 and 41", got/unit, free/unit)
		}

		secondConfig := host("spill-b")
		second := f.limiter(secondConfig)
		defer second.Close()
		if err := second.Feasible(); err != nil {
			t.Fatalf("the restarted host's promises are not feasible: %v", err)
		}
		if err := second.Ready(); !errors.Is(err, resource.ErrDiskPromises) {
			t.Fatalf("the restarted host is ready with %v while the first host's cache holds its room", err)
		}
		readings := 0
		for second.Ready() != nil && readings < 1000 {
			f.tick(1)
			readings++
		}
		// The spill file took 25 of the 41 free. The first host's smoothed
		// room falls towards it over its smoothing time, and its cache gives
		// back as its share falls: 22 units by the 14th reading, which leaves
		// the restarted host 38 free, 8 over its floor.
		if got, free := cache.Held(), f.disk.Usage().FreeBytes(); readings != 14 || got != 112*unit || free != 38*unit {
			t.Fatalf("the restarted host was ready after %d readings, with the cache at %d units and %d free, "+
				"want 14, 112 and 38", readings, got/unit, free/unit)
		}
		f.converge()
		// 250 - 50 - 25 - 25 - 30 - 10 leaves the first cache 110, and the
		// floor and the reserve, 40, free.
		if got, free := cache.Held(), f.disk.Usage().FreeBytes(); got != 110*unit || free != 40*unit {
			t.Fatalf("the cache settled at %d units with %d free, want 110 and 40", got/unit, free/unit)
		}
		if err := second.Fits(f.ctx, 10*unit); err != nil {
			t.Fatalf("the restarted host cannot promise the reserve: %v", err)
		}
		if err := cache.failures(); err != nil {
			t.Fatal(err)
		}
		for _, config := range []resource.DiskLimiterConfig{firstConfig, secondConfig} {
			if held, err := config.Users[0].Allocated(f.ctx); err != nil || held != 25*unit {
				t.Fatalf("%s holds %d units (%v), want its whole 25", config.Users[0].Name, held/unit, err)
			}
		}
	})
}

// A cache over its share is told to stop one region below it, and a cache
// within that region of its share is left alone, so a share that hovers at a
// region's edge does not evict and refill.
func TestTheCacheStopsOneRegionBelowItsShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Users: []resource.DiskUser{spill("spill", 25*unit, f.sparse("spill", 25*unit))}})
		defer l.Close()
		cache := f.cache()
		unregister, err := l.RegisterCache(cache)
		if err != nil {
			t.Fatal(err)
		}
		defer unregister()
		// The band shrinks as the cache fills, so it fills to within a region
		// and a quarter of the hard share of 150.
		cache.fill(t, l)
		if got := cache.Held(); got != 149*unit {
			t.Fatalf("the cache filled to %d units, want 149", got/unit)
		}
		if got := l.Status().SmoothFreeBytes; got != 51*unit {
			t.Fatalf("with the cache full the limiter counts %d units free, want 51", got/unit)
		}
		// Two more units taken from outside bring the hard share to 148.
		f.disk.SetOutsideBytes(diskOutside + 2*unit)
		f.converge()
		if err := cache.failures(); err != nil {
			t.Fatal(err)
		}
		if got := cache.Held(); got != 147*unit {
			t.Fatalf("the cache holds %d units, want 147: one region below its share", got/unit)
		}
		// A hard share of 148, less a fifth of the one unit the cache has left.
		if got := l.CacheShare(); got != 148*unit-unit/5 {
			t.Fatalf("the cache's share is %d, want %d", got, 148*unit-unit/5)
		}
		probes := f.runtime.Probes()
		if probes[resource.ProbeDiskShrink] != 1 || probes[resource.ProbeDiskHysteresisHeld] == 0 {
			t.Fatalf("the probes are %v, want one shrink and the hysteresis held after it", probes)
		}
	})
}

// As the disk fills from outside a unit every reading, the cache gives its
// regions back a few at a time, not all at once at the floor.
func TestTheBandGivesRegionsBackGradually(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Users: []resource.DiskUser{spill("spill", 25*unit, f.sparse("spill", 25*unit))}})
		defer l.Close()
		cache := f.cache()
		unregister, err := l.RegisterCache(cache)
		if err != nil {
			t.Fatal(err)
		}
		defer unregister()
		cache.fill(t, l)
		for step := range 50 {
			f.disk.SetOutsideBytes(diskOutside + int64(step+1)*unit)
			f.tick(1)
		}
		f.converge()
		if err := cache.failures(); err != nil {
			t.Fatal(err)
		}
		// The hard share is now 100 units, and the cache stops a region below it.
		if got := cache.Held(); got != 99*unit {
			t.Fatalf("the cache holds %d units, want 99", got/unit)
		}
		cache.mu.Lock()
		freed := slices.Clone(cache.freed)
		cache.mu.Unlock()
		given := 0
		for _, regions := range freed {
			given += regions
			if regions > 3 {
				t.Fatalf("one reading gave back %d regions, want at most 3: %v", regions, freed)
			}
		}
		if given != 50 || len(freed) < 17 {
			t.Fatalf("the cache gave back %d regions in %d steps, want 50 in at least 17: %v", given, len(freed), freed)
		}
	})
}

// One odd reading moves the smoothed readings only part of the way, so a
// cache inside its share is not told to give anything back for it.
func TestOneOddReadingDoesNotShrinkTheCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Users: []resource.DiskUser{spill("spill", 25*unit, f.sparse("spill", 25*unit))}})
		defer l.Close()
		cache := f.cache()
		unregister, err := l.RegisterCache(cache)
		if err != nil {
			t.Fatal(err)
		}
		defer unregister()
		// The cache fills, gives back what another writer takes for a while,
		// and is left with room inside its share when the writer lets go.
		cache.fill(t, l)
		f.disk.SetOutsideBytes(diskOutside + 20*unit)
		f.converge()
		f.disk.SetOutsideBytes(diskOutside)
		f.converge()
		held := cache.Held()
		if held != 129*unit || l.CacheShare() <= held+unit {
			t.Fatalf("the cache holds %d units of a share of %d, want 129 with room above", held/unit, l.CacheShare()/unit)
		}
		// For one reading the disk looks forty units fuller than it is.
		f.disk.SetOutsideBytes(diskOutside + 40*unit)
		f.tick(1)
		f.disk.SetOutsideBytes(diskOutside)
		f.tick(1)
		if status := l.Status(); status.AvailableBytes != 200*unit-held {
			t.Fatalf("the last raw reading is %d available, want %d", status.AvailableBytes, 200*unit-held)
		}
		f.converge()
		if got := cache.Held(); got != held {
			t.Fatalf("one odd reading took the cache from %d units to %d", held/unit, got/unit)
		}
	})
}

// The limiter reads the disk on its own every interval of its clock, and not
// before.
func TestTheLimiterReadsTheDiskEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit}})
		defer l.Close()
		f.disk.SetOutsideBytes(100 * unit)
		f.clock.Advance(resource.DefaultDiskInterval - 1)
		f.settle()
		if got := l.Status().AvailableBytes; got != 200*unit {
			t.Fatalf("before an interval passed the limiter read %d units available, want the first 200", got/unit)
		}
		f.clock.Advance(1)
		f.settle()
		if got := l.Status().AvailableBytes; got != 150*unit {
			t.Fatalf("after an interval the limiter read %d units available, want 150", got/unit)
		}
	})
}

// A reading that cannot be true, or that fails, changes nothing the limiter
// acts on, and the limiter says why.
func TestAReadingThatFailsChangesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit}})
		defer l.Close()
		before := chosen(l)
		f.disk.Fail()
		if err := l.Refresh(f.ctx); err == nil {
			t.Fatal("a reading of a failed disk succeeded")
		}
		f.disk.Recover()
		f.clock.Advance(10 * resource.DefaultDiskSmoothing)
		if got := chosen(l); got != before {
			t.Fatalf("a failed reading changed the share from %+v to %+v", before, got)
		}
		if l.Status().ReadError == "" {
			t.Fatal("a failed reading left no reason in the status")
		}
		if err := l.Refresh(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := l.Status().ReadError; got != "" {
			t.Fatalf("a good reading left %q as the reason", got)
		}
	})
}

// The budget keeps a reserve for every priority above the lowest, so as it
// runs down it refuses repairs first and fills from publications last. It
// refills at its daily average on the clock, up to a burst.
func TestTheWriteBudgetRefusesLowPrioritiesFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Writes: resource.WriteBudget{BytesPerDay: 100 * unit, BurstBytes: 40 * unit}, Device: &device{}})
		defer l.Close()
		// A burst of 40 keeps a reserve of 10 for each priority above.
		for i, step := range []struct {
			bytes    int64
			priority int
			admitted bool
		}{
			{10 * unit, 0, true},  // 40 - 10 keeps the 30 a repair leaves
			{1, 0, false},         // a byte more would not
			{1, 3, true},          // a publication needs only that it fits
			{10 * unit, 1, false}, // a second chance keeps 20
			{10*unit - 1, 1, true},
			{10 * unit, 2, true},  // a fill from the store keeps 10
			{10 * unit, 0, false}, // a repair would leave nothing of its 30
			{10 * unit, 3, true},  // to nothing
			{1, 3, false},
		} {
			if got := l.Admit(step.bytes, step.priority); got != step.admitted {
				t.Fatalf("step %d: a write of %d bytes at priority %d was admitted %v, want %v",
					i, step.bytes, step.priority, got, step.admitted)
			}
		}
		writes := l.Status().Writes
		if writes.LeftBytes != 0 || writes.AdmittedBytes != 40*unit || writes.Refused != [4]uint64{2, 1, 0, 1} {
			t.Fatalf("the budget is %+v, want nothing left, 40 units admitted and refusals 2 1 0 1", writes)
		}
		// A quarter of a day credits a quarter of a day's budget.
		f.clock.Advance(6 * time.Hour)
		if got := l.Status().Writes.LeftBytes; got != 25*unit {
			t.Fatalf("six hours left %d units, want 25", got/unit)
		}
		f.clock.Advance(30 * time.Hour)
		if got := l.Status().Writes.LeftBytes; got != 40*unit {
			t.Fatalf("a day and a half left %d units, want the burst of 40", got/unit)
		}
		if probes := f.runtime.Probes(); probes[resource.ProbeDiskLowRefused] != 3 {
			t.Fatalf("the probes are %v, want three writes refused that a publication would have been admitted for", probes)
		}
	})
}

// The device's own counter charges the budget with what other writers wrote,
// and does not charge the cache's own writes twice. A counter that goes
// backwards is a new device, and a jump charges at most a burst into debt.
func TestTheDeviceCounterChargesTheBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		counter := &device{written: 1000 * unit}
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Writes: resource.WriteBudget{BytesPerDay: 100 * unit, BurstBytes: 40 * unit}, Device: counter})
		defer l.Close()
		refresh := func() resource.DiskWritesStatus {
			t.Helper()
			if err := l.Refresh(f.ctx); err != nil {
				t.Fatal(err)
			}
			return l.Status().Writes
		}
		counter.set(1015 * unit)
		if got := refresh(); got.LeftBytes != 25*unit || got.WrittenBytes != 15*unit {
			t.Fatalf("15 units written by others left %+v, want 25 units left of 15 written", got)
		}
		if !l.Admit(5*unit, 3) {
			t.Fatal("a write within the budget was refused")
		}
		counter.set(1020 * unit)
		if got := refresh(); got.LeftBytes != 20*unit || got.WrittenBytes != 20*unit {
			t.Fatalf("the cache's own 5 units counted by the device left %+v, want 20 left of 20 written", got)
		}
		counter.set(3 * unit)
		if got := refresh(); got.LeftBytes != 20*unit || got.WrittenBytes != 20*unit {
			t.Fatalf("a replaced device left %+v, want nothing charged", got)
		}
		counter.set(4 * unit)
		if got := refresh(); got.LeftBytes != 19*unit || got.WrittenBytes != 21*unit {
			t.Fatalf("a unit written on the new device left %+v, want 19 left of 21 written", got)
		}
		counter.set(1 << 50)
		if got := refresh(); got.LeftBytes != -40*unit {
			t.Fatalf("a counter that jumped left %d, want a burst of debt", got.LeftBytes)
		}
		if l.Admit(1, 3) {
			t.Fatal("a budget in debt admitted a write")
		}
	})
}

// A limiter refuses a configuration it could not keep, and a second cache.
func TestALimiterRefusesWhatItCannotKeep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		for _, config := range []resource.DiskLimiterConfig{
			{},
			{Goal: resource.DiskGoal{FreePercent: 100}},
			{Goal: resource.DiskGoal{FreeBytes: -1}},
			{Goal: resource.DiskGoal{UsedBytes: -1, FreeBytes: 1}},
			{Goal: resource.DiskGoal{FreeBytes: 1}, Writes: resource.WriteBudget{BytesPerDay: 1}, Device: &device{}},
			{Goal: resource.DiskGoal{FreeBytes: 1}, Writes: resource.WriteBudget{BurstBytes: 1}, Device: &device{}},
			{Goal: resource.DiskGoal{FreeBytes: 1}, Writes: resource.WriteBudget{BytesPerDay: 1, BurstBytes: 1}},
			{Goal: resource.DiskGoal{FreeBytes: 1}, Users: []resource.DiskUser{{Name: "nothing"}}},
			{Goal: resource.DiskGoal{FreeBytes: 1}, BandPercent: 101},
		} {
			config.Space, config.Clock = f.disk, f.clock
			if _, err := resource.NewDiskLimiter(f.ctx, config); !errors.Is(err, resource.ErrInvalid) {
				t.Fatalf("%+v was built with %v, want %v", config, err, resource.ErrInvalid)
			}
		}
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 1}})
		defer l.Close()
		// Without a budget every write is admitted that a byte count can be.
		if l.Admit(-1, 3) || l.Admit(1<<61, 3) || !l.Admit(5, 0) {
			t.Fatal("a limiter without a budget admitted a write of no size or refused one of five bytes")
		}
		if got := l.Status().Writes.AdmittedBytes; got != 5 {
			t.Fatalf("the limiter counts %d bytes admitted, want 5", got)
		}
		if err := l.Fits(f.ctx, -1); !errors.Is(err, resource.ErrInvalid) {
			t.Fatalf("a promise of -1 bytes returned %v, want %v", err, resource.ErrInvalid)
		}
		if _, err := l.RegisterCache(nil); !errors.Is(err, resource.ErrInvalid) {
			t.Fatalf("no cache was registered with %v, want %v", err, resource.ErrInvalid)
		}
		unregister, err := l.RegisterCache(f.cache())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.RegisterCache(f.cache()); !errors.Is(err, resource.ErrInvalid) {
			t.Fatalf("a second cache was registered with %v, want %v", err, resource.ErrInvalid)
		}
		unregister()
		if _, err := l.RegisterCache(f.cache()); err != nil {
			t.Fatalf("a cache after the first was unregistered: %v", err)
		}
	})
}

// A cache at its share is inside it, and a cache one region below its share
// is at its stop mark: neither is told to shrink, and the second is not held
// by the hysteresis either. The used goal has no band, so the share is exact.
func TestACacheAtItsShareOrItsStopMarkIsLeftAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		promise := int64(25 * unit)
		l := f.limiter(resource.DiskLimiterConfig{Goal: resource.DiskGoal{UsedBytes: 125 * unit},
			Users: []resource.DiskUser{{Name: "staging", Promised: func() int64 { return promise }}}})
		defer l.Close()
		cache := f.cache()
		unregister, err := l.RegisterCache(cache)
		if err != nil {
			t.Fatal(err)
		}
		defer unregister()
		probes := func() (shrinks, held uint64) {
			got := f.runtime.Probes()
			return got[resource.ProbeDiskShrink], got[resource.ProbeDiskHysteresisHeld]
		}
		cache.fill(t, l)
		if err := l.Refresh(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got, share := cache.Held(), l.CacheShare(); got != 100*unit || share != 100*unit {
			t.Fatalf("the cache holds %d units of a share of %d, want all 100", got/unit, share/unit)
		}
		if shrinks, held := probes(); shrinks != 0 || held != 2 {
			t.Fatalf("a cache at its share was told to shrink %d times and held %d, want none and 2", shrinks, held)
		}
		promise += unit
		if err := l.Refresh(f.ctx); err != nil {
			t.Fatal(err)
		}
		if got := cache.Held(); got != 98*unit {
			t.Fatalf("a share of 99 left the cache at %d units, want 98", got/unit)
		}
		if err := l.Refresh(f.ctx); err != nil {
			t.Fatal(err)
		}
		if shrinks, held := probes(); shrinks != 1 || held != 2 {
			t.Fatalf("the cache at its stop mark was told to shrink %d times and held %d, want 1 and 2", shrinks, held)
		}
	})
}

// A cache's file read back after a restart holds its regions before any cache
// registers. The limiter counts what the file holds as the cache's, so the
// hard share is the 150 units of an empty disk, not 40 fewer, as it would be
// were the file another writer's.
func TestACacheFileHeldBeforeItsCacheCountsAsTheCaches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := defaultDisk(t)
		file, err := f.disk.Open(f.ctx, "cache", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := file.(platform.AllocatingFile).Allocate(f.ctx, 0, 40*unit); err != nil {
			t.Fatal(err)
		}
		config := resource.DiskLimiterConfig{Goal: resource.DiskGoal{FreeBytes: 25 * unit},
			Users: []resource.DiskUser{spill("spill", 25*unit, f.sparse("spill", 25*unit))}}
		other := f.limiter(config)
		other.Close()
		config.CacheFile = file.(platform.FileAllocation).Allocated
		l := f.limiter(config)
		defer l.Close()
		// Counting the file: 200 - 25 - 25 = 150, less a band of a fifth of
		// the 110 the cache has left of it. Without: 200 - 40 - 25 - 25 =
		// 110, less a band of 22.
		if got, without := l.CacheShare(), other.CacheShare(); got != 128*unit || without != 88*unit {
			t.Fatalf("the share is %d units counting the file and %d without, want 128 and 88", got/unit, without/unit)
		}
		if held := l.Status().CacheHeldBytes; held != 40*unit {
			t.Fatalf("the limiter counts %d units as the cache's, want the file's 40", held/unit)
		}
	})
}
