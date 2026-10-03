package checkpoint

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A region fills to its last byte. Fifteen items of 4,054 bytes and one of
// 4,046, each with a table entry of 40, and the trailer of 40 are exactly
// 64 KiB, so the sixteenth shares the first region and the seventeenth opens
// the next. The second region's table names its items from its own start.
func TestDiskRegionFillsToItsLastByte(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		keys := pages("va", 0, 33)
		// Every sixteenth item is eight bytes shorter.
		length := func(at int) int { return 4008 - 8*(at%16/15) }
		for at, key := range keys[:16] {
			f.write(t, key, length(at))
		}
		if regions := f.disk.stats().Regions; regions != 1 {
			t.Fatalf("sixteen items that fill a region took %d regions", regions)
		}
		for at, key := range keys[16:] {
			f.write(t, key, length(16+at))
		}
		if regions := f.disk.stats().Regions; regions != 3 {
			t.Fatalf("33 items took %d regions, want 3", regions)
		}
		table, err := readRegionTable(t.Context(), f.file, regionBase(1), testRegionBytes)
		if err != nil || table.sequence != 2 || len(table.items) != 16 {
			t.Fatalf("the second region's table is region %d of %d items, %v", table.sequence, len(table.items), err)
		}
		for at, item := range table.items {
			want := tableItem{key: keys[16+at], code: wholeEnvelope, offset: uint32(at * 4054),
				length: uint32(length(at))}
			if item != want {
				t.Fatalf("the second region's item %d is %+v, want %+v", at, item, want)
			}
		}
		for _, key := range keys {
			f.read(t, key)
		}
		f.disk.checkInvariants(t)
	})
}

// An item that would reach into the room its region's table and trailer need
// goes to the next region, so the table never lies over an item. Fifteen items
// of 4,054 bytes leave room for a sixteenth of 4,054 and its table entry; one
// of 4,094 bytes would end 40 bytes into the table.
func TestDiskAnItemNeverReachesItsRegionsTable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		keys := pages("va", 0, 17)
		for _, key := range keys[:15] {
			f.write(t, key, 4008)
		}
		f.write(t, keys[15], 4048)
		if regions := f.disk.stats().Regions; regions != 2 {
			t.Fatalf("an item reaching into the table took %d regions, want it in the second", regions)
		}
		f.write(t, keys[16], 4008)
		f.disk.mu.Lock()
		open := f.disk.open
		f.disk.mu.Unlock()
		f.disk.close(f.ctx(t), open)
		for _, key := range keys {
			f.read(t, key)
		}
		f.disk.checkInvariants(t)
	})
}

// The largest item a region takes is the one that fills it with its table
// entry and the trailer; a byte more is refused. Past a region's size, the
// longest envelope is what an item's length carries.
func TestDiskRefusesAnItemLargerThanARegion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		f.write(t, keyOf("va", 0), testRegionBytes-46-40-diskTrailerSize)
		if stats := f.disk.stats(); stats.Regions != 1 || stats.Entries != 1 {
			t.Fatalf("an item that fills a region left %+v", stats)
		}
		key := keyOf("va", 1)
		err := f.disk.write(f.ctx(t), key, payloadOf(key, testRegionBytes-46-40-diskTrailerSize+1), WriteFillPublication)
		if !errors.Is(err, ErrDiskRefused) {
			t.Fatalf("an item a byte larger than a region returned %v, want %v", err, ErrDiskRefused)
		}
		f.read(t, keyOf("va", 0))
	})
	synctest.Test(t, func(t *testing.T) {
		const regionBytes = 32 << 20
		runtime := sim.New(sim.Config{})
		file, err := newCheckedFile(t.Context(), runtime.NewDisk("host", sim.DiskConfig{}), regionBytes)
		if err != nil {
			t.Fatal(err)
		}
		disk, err := openCacheDisk(t.Context(), file, newTestBudget(3*regionBytes/testRegionBytes),
			diskSettings{regionBytes: regionBytes, indexLimit: DefaultDiskIndexBytes, threshold: 1,
				deployment: testDeployment, entropy: runtime.NewEntropy("cache-disk")})
		if err != nil {
			t.Fatal(err)
		}
		largest := payloadOf(keyOf("va", 0), maximumDiskItem)
		if err := disk.write(t.Context(), keyOf("va", 0), largest, WriteFillPublication); err != nil {
			t.Fatal(err)
		}
		if data, outcome := disk.read(t.Context(), keyOf("va", 0)); outcome != diskHit || !bytes.Equal(data, largest) {
			t.Fatalf("the largest item read back as %d and %d bytes", outcome, len(data))
		}
		err = disk.write(t.Context(), keyOf("va", 1), make([]byte, maximumDiskItem+1), WriteFillPublication)
		if !errors.Is(err, ErrDiskRefused) {
			t.Fatalf("an item past what its length carries returned %v, want %v", err, ErrDiskRefused)
		}
	})
}

// What a pull may copy at once is the share less the region kept free, before
// headers and tables: a checkpoint of exactly that fits and a byte more does
// not. A share of one region holds nothing, and refuses every write.
func TestDiskHoldsTheShareLessTheFreeRegion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		capacity := int64(7 * (testRegionBytes - diskTrailerSize))
		if got := f.disk.capacity(); got != capacity || !f.disk.holds(capacity) || f.disk.holds(capacity+1) {
			t.Fatalf("a share of eight regions holds %d bytes, want %d and not a byte more", got, capacity)
		}
		f.budget.share.Store(testRegionBytes)
		if got := f.disk.capacity(); got != 0 || !f.disk.holds(0) || f.disk.holds(1) {
			t.Fatalf("a share of one region holds %d bytes, want none", got)
		}
		f.budget.share.Store(0)
		if got := f.disk.capacity(); got != 0 {
			t.Fatalf("an empty share holds %d bytes, want none", got)
		}
		key := keyOf("va", 0)
		f.budget.share.Store(testRegionBytes)
		if err := f.disk.write(f.ctx(t), key, payloadOf(key, 100), WriteFillPublication); !errors.Is(err, ErrDiskRefused) {
			t.Fatalf("a write to a share of one region returned %v, want %v", err, ErrDiskRefused)
		}
		if stats := f.disk.stats(); stats.Regions != 0 || stats.Refused != 1 {
			t.Fatalf("the refused write left %+v", stats)
		}
	})
}

// A run of a window's items is extended only by a higher page. A window that
// keeps a bitmap of eighteen pages, up to page 100, takes page 101 into the
// same entry and page 50 into a new one, and each reads back.
func TestDiskIndexExtendsARunOnlyUpward(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		for _, key := range append(pages("va", 0, 17), keyOf("va", 100)) {
			f.write(t, key, 100)
		}
		dense := int64(windowEntryCharge + bitmapCharge)
		for _, step := range []struct {
			page uint64
			cost int64
		}{
			{101, dense + 19*denseItemCharge},
			{50, dense + 19*denseItemCharge + windowEntryCharge + listedItemCharge},
		} {
			f.write(t, keyOf("va", step.page), 100)
			if used := f.disk.stats().IndexBytes; used != step.cost {
				t.Fatalf("after page %d the index costs %d bytes, want %d", step.page, used, step.cost)
			}
		}
		for _, key := range keysOf(f.model) {
			f.read(t, key)
		}
		f.disk.checkInvariants(t)
	})
}

// A second chance writes up to exactly half a region. Every item of the victim
// was read, and eight of 4,096 bytes are exactly its half.
func TestDiskSecondChanceFillsHalfARegionExactly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 4})
		// Fifteen items of 4,096 bytes and their table entries fill a region.
		keys := pages("va", 0, 46)
		for _, key := range keys[:45] {
			f.write(t, key, 4050)
		}
		for _, key := range keys[:15] {
			f.read(t, key)
		}
		f.write(t, keys[45], 4050)
		if stats := f.disk.stats(); stats.Rewritten != 8 || stats.Evicted != 1 {
			t.Fatalf("the eviction left %+v, want eight items written again", stats)
		}
		f.requireHeld(t, keys, among(append(pages("va", 0, 8), keys[15:]...)))
		f.disk.checkInvariants(t)
	})
}

// Fitting a fallen share closes the open region when nothing else is left,
// and gives it back too.
func TestDiskFitGivesBackTheOpenRegion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 4})
		keys := pages("va", 0, 5)
		f.fill(t, keys)
		f.budget.share.Store(testRegionBytes)
		if err := f.disk.fit(f.ctx(t)); err != nil {
			t.Fatal(err)
		}
		if stats := f.disk.stats(); stats.Regions != 0 || stats.Evicted != 1 || stats.Entries != 0 {
			t.Fatalf("fitting a share of one region left %+v, want nothing", stats)
		}
		if table, err := readRegionTable(t.Context(), f.file, regionBase(0), testRegionBytes); !errors.Is(err, errNoTable) {
			t.Fatalf("the region given back still reads a table of %d items, %v", len(table.items), err)
		}
		f.disk.checkInvariants(t)
	})
}

// A region kept for a read in flight is not counted as kept when the disk
// fits a fallen share: the fit gives back the regions after it until the rest
// fit, and the read's region goes when the read is done.
func TestDiskFitCountsARegionWaitingForItsReaderAsGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 6})
		keys := pages("va", 0, 5*testItemsPerRegion)
		f.fill(t, keys)
		gate := f.file.hold(regionBase(0), regionBase(1))
		done := make(chan diskReadOutcome, 1)
		go func() {
			_, outcome := f.disk.read(f.ctx(t), keys[0])
			done <- outcome
		}()
		<-gate.entered
		f.budget.share.Store(3 * testRegionBytes)
		if err := f.disk.fit(f.ctx(t)); err != nil {
			t.Fatal(err)
		}
		// The read's region waits; the second and third are given back; the
		// fourth and fifth are the two a share of three keeps.
		if stats := f.disk.stats(); stats.Regions != 3 || stats.Evicted != 2 {
			t.Fatalf("fitting with a read in flight left %+v, want the read's region and two kept", stats)
		}
		close(gate.release)
		if outcome := <-done; outcome != diskHit {
			t.Fatalf("the read in flight found %d, want a hit", outcome)
		}
		if stats := f.disk.stats(); stats.Regions != 2 || stats.Evicted != 3 {
			t.Fatalf("once the read finished the disk reports %+v", stats)
		}
		f.requireHeld(t, keys, among(keys[3*testItemsPerRegion:]))
		f.disk.checkInvariants(t)
	})
}

// A cache refuses a disk it cannot lay out: a region that is not whole
// filesystem blocks, smaller than any envelope needs, or larger than a table's
// offsets reach, a second-chance threshold past what the read counter holds,
// and a deployment whose names do not fit its header's length fields or whose
// header does not fit in a region. A disk without a share keeps nothing, and
// one with a budget keeps its share.
func TestCacheRefusesADiskItCannotLayOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		file := diskTableFile(t, nil)
		for _, config := range []CacheConfig{
			{Disk: file, DiskBytes: 1 << 30, DiskRegionBytes: testRegionBytes + 1},
			{Disk: file, DiskBytes: 1 << 30, DiskRegionBytes: minimumDiskRegionBytes - diskBlock},
			{Disk: file, DiskBytes: 1 << 30, DiskRegionBytes: maximumDiskRegionBytes + diskBlock},
			{Disk: file, DiskBytes: 1 << 30, DiskSecondChanceReads: wordReadsMax + 1},
			{Disk: file, DiskBytes: 1 << 30, DiskIndexBytes: -1},
			{Disk: file, DiskBytes: 1 << 30, Deployment: CacheDeployment{Bucket: strings.Repeat("b", 0x10000)}},
			// 48 bytes, three names of 21,830 and a checksum are 65,542 bytes,
			// six more than a region of 64 KiB.
			{Disk: file, DiskBytes: 1 << 30, DiskRegionBytes: testRegionBytes, Deployment: CacheDeployment{
				Store: strings.Repeat("s", 21830), Bucket: strings.Repeat("b", 21830), Prefix: strings.Repeat("p", 21830)}},
		} {
			if _, err := NewCache(t.Context(), testresource.New(), config); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("a cache of %+v returned %v, want %v", config, err, ErrInvalidConfig)
			}
		}
		for _, test := range []struct {
			config CacheConfig
			share  int64
		}{
			{CacheConfig{Disk: file}, -1},
			{CacheConfig{Disk: file, DiskBytes: 1 << 30}, 1 << 30},
			{CacheConfig{Disk: file, Budget: newTestBudget(5)}, 5 * testRegionBytes},
			// Names as long as their length fields carry are storable.
			{CacheConfig{Disk: file, DiskBytes: 1 << 30, DiskRegionBytes: 4 * testRegionBytes, Deployment: CacheDeployment{
				Store: strings.Repeat("s", 0xffff), Bucket: strings.Repeat("b", 0xffff), Prefix: strings.Repeat("p", 0xffff)}},
				1 << 30},
			// A header of exactly a region fits.
			{CacheConfig{Disk: file, DiskBytes: 1 << 30, DiskRegionBytes: testRegionBytes, Deployment: CacheDeployment{
				Store: strings.Repeat("s", 21828), Bucket: strings.Repeat("b", 21828), Prefix: strings.Repeat("p", 21828)}},
				1 << 30},
		} {
			cache, err := NewCache(t.Context(), testresource.New(), test.config)
			if err != nil {
				t.Fatal(err)
			}
			share := int64(-1)
			if cache.disk != nil {
				share = cache.Stats().Disk.LimitBytes
			}
			if share != test.share {
				t.Fatalf("a cache of %+v keeps a share of %d, want %d", test.config, share, test.share)
			}
			cache.Close()
		}
	})
}
