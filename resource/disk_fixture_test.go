package resource_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// unit is the size every amount in these tests is a multiple of: a hundred
// device pages, so a cache region is whole pages, and a hundredth of it is
// whole bytes, so every percentage of it is exact.
const unit = 409600

// The disk every test starts from: 250 units, of which other writers hold 50.
const (
	diskTotal   = 250 * unit
	diskOutside = 50 * unit
)

// diskFixture is one simulated host's disk and the clock its limiter runs on.
// It lives inside a synctest bubble: the disk's latencies pass on the bubble's
// clock, and the limiter's readings and budget on the simulated one.
type diskFixture struct {
	t       *testing.T
	ctx     context.Context
	runtime *sim.Runtime
	clock   *sim.Clock
	disk    *sim.Disk
}

func newDiskFixture(t *testing.T, seed uint64, space sim.SpaceConfig) *diskFixture {
	t.Helper()
	f := &diskFixture{t: t}
	f.runtime = sim.New(sim.Config{Seed: seed, Now: func() time.Time { return f.clock.Now() }})
	f.clock = f.runtime.NewClock("limiter")
	f.disk = f.runtime.NewDisk("node", sim.DiskConfig{Space: space})
	f.ctx = sim.WithRuntime(t.Context(), f.runtime)
	return f
}

func defaultDisk(t *testing.T) *diskFixture {
	return newDiskFixture(t, 1, sim.SpaceConfig{TotalBytes: diskTotal, OutsideBytes: diskOutside})
}

// limiter starts a limiter over this disk. The caller closes it before its
// bubble ends.
func (f *diskFixture) limiter(config resource.DiskLimiterConfig) *resource.DiskLimiter {
	f.t.Helper()
	config.Space = f.disk
	config.Clock = f.clock
	if config.Region == 0 {
		config.Region = unit
	}
	l, err := resource.NewDiskLimiter(f.ctx, config)
	if err != nil {
		f.t.Fatal(err)
	}
	return l
}

// tick lets n intervals pass on the simulated clock, one at a time, so that
// the limiter reads the disk at each of them.
func (f *diskFixture) tick(n int) {
	for range n {
		f.clock.Advance(resource.DefaultDiskInterval)
		f.settle()
	}
}

// settle lets whatever the clock released run to its end. The disk's own
// latencies pass on the bubble's clock while the test sleeps on it.
func (f *diskFixture) settle() {
	time.Sleep(time.Second)
	synctest.Wait()
}

// converge lets the smoothed readings reach the disk as it now is: forty
// minutes is forty smoothing times, which leaves no byte of difference.
func (f *diskFixture) converge() { f.tick(240) }

// sparse opens a file and sizes it to bytes without writing any of it: a user
// that holds less than it promised. A pager allocates its spill file whole, so
// this tests the limiter's rule on its own: a promise counts whole, whatever
// the file holds.
func (f *diskFixture) sparse(name string, bytes int64) platform.File {
	f.t.Helper()
	file, err := f.disk.Open(f.ctx, name, platform.OpenOptions{Create: true})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := file.Truncate(f.ctx, bytes); err != nil {
		f.t.Fatal(err)
	}
	return file
}

// spill is a user of the disk that cannot give space back: a sparse file
// counted at its promise.
func spill(name string, promise int64, file platform.File) resource.DiskUser {
	return resource.DiskUser{Name: name, Promised: func() int64 { return promise },
		Allocated: file.(platform.FileAllocation).Allocated}
}

// diskCache is a cache of whole regions on the simulated disk, as the page
// cache's disk will be: it writes a region at a time and gives the oldest
// back first.
type diskCache struct {
	ctx  context.Context
	file platform.File

	mu      sync.Mutex
	regions []int64
	next    int64
	targets []int64
	freed   []int
	errs    []error
}

func (f *diskFixture) cache() *diskCache {
	return &diskCache{ctx: f.ctx, file: f.sparse("cache", 0)}
}

func (c *diskCache) Held() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(len(c.regions)) * unit
}

func (c *diskCache) Shrink(target int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets = append(c.targets, target)
	freed := 0
	for int64(len(c.regions))*unit > target {
		// A punch the device fails gives the region back all the same, as the
		// cache gives it back: the filesystem keeps its blocks until the slot
		// is written again. Any other failure is the fixture's own.
		if err := c.file.(platform.SparseFile).PunchHole(c.ctx, c.regions[0], unit); err != nil &&
			!errors.Is(err, platform.ErrInjectedFault) {
			c.errs = append(c.errs, err)
			return
		}
		c.regions = c.regions[1:]
		freed++
	}
	c.freed = append(c.freed, freed)
}

// fill writes regions while the limiter's share has room for one more,
// reading the disk before each, as the cache will before each region it opens.
func (c *diskCache) fill(t *testing.T, l *resource.DiskLimiter) {
	t.Helper()
	for {
		if err := l.Refresh(c.ctx); err != nil {
			t.Fatal(err)
		}
		if c.Held()+unit > l.CacheShare() {
			return
		}
		c.mu.Lock()
		at := c.next
		c.next += unit
		c.mu.Unlock()
		if _, err := c.file.WriteAt(c.ctx, region, at); err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		c.regions = append(c.regions, at)
		c.mu.Unlock()
	}
}

func (c *diskCache) failures() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.errs) > 0 {
		return fmt.Errorf("the cache could not give regions back: %v", c.errs)
	}
	return nil
}

// region is the bytes of one cache region: not zeroes, so every page of it is
// stored.
var region = func() []byte {
	b := make([]byte, unit)
	for i := range b {
		b[i] = byte(i%251 + 1)
	}
	return b
}()

// device is a write counter a test sets by hand.
type device struct {
	mu      sync.Mutex
	written uint64
	err     error
}

func (d *device) BytesWritten(context.Context) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.written, d.err
}

func (d *device) set(written uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.written = written
}
