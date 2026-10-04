package checkpoint

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
)

// pages is the keys of pages [from, to) of vm's ram0.
func pages(vm string, from, to uint64) []diskKey {
	keys := make([]diskKey, 0, to-from)
	for page := from; page < to; page++ {
		keys = append(keys, keyOf(vm, page))
	}
	return keys
}

// fill writes a testItemBytes item under each key.
func (f *diskFixture) fill(t *testing.T, keys []diskKey) {
	t.Helper()
	for _, key := range keys {
		f.write(t, key, testItemBytes)
	}
}

// requireHeld requires the disk to hold exactly the keys of want among keys.
func (f *diskFixture) requireHeld(t *testing.T, keys []diskKey, want func(diskKey) bool) {
	t.Helper()
	for at, held := range f.held(f.ctx(t), keys) {
		if held != want(keys[at]) {
			t.Errorf("the disk holds page %d of %s: %v, want %v", keys[at].Page, keys[at].Ref.VM, held,
				want(keys[at]))
		}
	}
}

func among(keys []diskKey) func(diskKey) bool {
	return func(key diskKey) bool { return slices.Contains(keys, key) }
}

// Items are appended to the open region in the order they arrive, and a full
// region is closed: its items synced, its table written at its end, and the
// region synced again. The table names every item, where it lies and how long
// it is, and each item reads back as what was written under its own key.
func TestDiskRegionsFillInOrderAndCloseWithATable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		keys := pages("va", 0, testItemsPerRegion+1)
		f.fill(t, keys)
		stats := f.disk.stats()
		// One window: 21 items listed densely in the first region, one in the
		// second.
		want := DiskStats{UsedBytes: 2 * testRegionBytes, LimitBytes: 8 * testRegionBytes, Regions: 2, Entries: 22,
			IndexBytes: (windowEntryCharge + bitmapCharge + denseItemCharge*21) + (windowEntryCharge + listedItemCharge),
			Identity:   f.disk.identity}
		if stats != want {
			t.Fatalf("the disk reports %+v, want %+v", stats, want)
		}
		table, err := readRegionTable(t.Context(), f.file, regionBase(0), testRegionBytes)
		if err != nil {
			t.Fatal(err)
		}
		if table.sequence != 1 || table.generation != f.disk.generation || len(table.items) != testItemsPerRegion {
			t.Fatalf("the first region's table is region %d of generation %d with %d items, want region 1 of %d with %d",
				table.sequence, table.generation, len(table.items), f.disk.generation, testItemsPerRegion)
		}
		for at, item := range table.items {
			wantItem := tableItem{key: keys[at], code: wholeEnvelope, offset: uint32(at * 3050),
				length: testItemBytes}
			if item != wantItem {
				t.Fatalf("the table's item %d is %+v, want %+v", at, item, wantItem)
			}
			buffer := make([]byte, 3050)
			if err := readFull(t.Context(), f.file, buffer, regionBase(0)+int64(item.offset)); err != nil {
				t.Fatal(err)
			}
			parsed, err := parseItem(buffer, false)
			if err != nil || parsed.key != keys[at] || parsed.code != wholeEnvelope ||
				!bytes.Equal(parsed.data, f.model[keys[at]]) {
				t.Fatalf("item %d reads back as %+v, %v", at, parsed.key, err)
			}
		}
		if _, err := readRegionTable(t.Context(), f.file, regionBase(1), testRegionBytes); !errors.Is(err, errNoTable) {
			t.Fatalf("the open region's table reads back %v, want %v", err, errNoTable)
		}
		// The file's header is written and synced first, alone in the file's
		// first 64 KiB.
		wantOps := []string{fmt.Sprintf("write 0+%d", diskHeaderBytes(testDeployment)), "sync",
			"allocate 65536+65536"}
		for at := range testItemsPerRegion {
			wantOps = append(wantOps, fmt.Sprintf("write %d+3050", 65536+at*3050))
		}
		// The table is 21 entries of 40 bytes and the trailer of 40.
		wantOps = append(wantOps, "sync", "write 130192+880", "sync", "allocate 131072+65536", "write 131072+3050")
		if ops := f.file.operations(); !slices.Equal(ops[:len(wantOps)], wantOps) {
			t.Fatalf("the disk saw %q, want %q", ops, wantOps)
		}
		f.disk.checkInvariants(t)
	})
}

// A read checks the key the item names and its checksum. An item that fails
// either is a miss, and the index forgets it, so it is not read again.
func TestDiskReadChecksKeyAndChecksum(t *testing.T) {
	t.Run("a damaged item", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiskFixture(t, diskFixtureConfig{regions: 8})
			f.fill(t, pages("va", 0, 2))
			// One byte of the second item's envelope, past its 50-byte header.
			if _, err := f.file.file().WriteAt(t.Context(), []byte{0xff}, regionBase(0)+3050+50+10); err != nil {
				t.Fatal(err)
			}
			if data, outcome := f.disk.read(f.ctx(t), keyOf("va", 1), nil); outcome != diskDamaged || data != nil {
				t.Fatalf("reading the damaged item found %d and %d bytes, want it damaged", outcome, len(data))
			}
			if stats := f.disk.stats(); stats.Lost != 1 || stats.Entries != 1 {
				t.Fatalf("the disk reports %+v, want one copy lost and one held", stats)
			}
			if _, outcome := f.disk.read(f.ctx(t), keyOf("va", 1), nil); outcome != diskAbsent {
				t.Fatalf("reading the forgotten item again found %d, want it absent", outcome)
			}
			f.read(t, keyOf("va", 0))
			f.disk.checkInvariants(t)
		})
	})
	t.Run("a misdirected index", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiskFixture(t, diskFixtureConfig{regions: 8})
			f.fill(t, []diskKey{keyOf("va", 0), keyOf("vb", 0)})
			// The index sends a read of va's page to vb's item, of the same
			// length under a header of the same length.
			f.disk.mu.Lock()
			misdirected, _ := f.disk.index.lookup(keyOf("va", 0), wholeEnvelope, false, false)
			other, _ := f.disk.index.lookup(keyOf("vb", 0), wholeEnvelope, false, false)
			misdirected.entry.first = other.offset
			f.disk.mu.Unlock()
			if data, outcome := f.disk.read(f.ctx(t), keyOf("va", 0), nil); outcome != diskKeyMismatch || data != nil {
				t.Fatalf("a misdirected read found %d and %d bytes, want another key's item refused", outcome,
					len(data))
			}
			if stats := f.disk.stats(); stats.Lost != 1 || stats.Entries != 1 {
				t.Fatalf("the disk reports %+v, want one copy lost and one held", stats)
			}
			if f.disk.has(f.ctx(t), keyOf("va", 0)) {
				t.Fatal("the disk still names the misdirected item")
			}
			f.read(t, keyOf("vb", 0))
			f.disk.checkInvariants(t)
		})
	})
}

// The victim is always the oldest closed region. A share of four regions
// fills three, and each fill that needs a fourth gives the oldest back first.
func TestDiskEvictsTheOldestRegionFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 4})
		keys := pages("va", 0, 85)
		f.fill(t, keys[:63])
		if stats := f.disk.stats(); stats.Regions != 3 || stats.Evicted != 0 {
			t.Fatalf("three full regions report %+v, want three regions and nothing evicted", stats)
		}
		f.fill(t, keys[63:64])
		if stats := f.disk.stats(); stats.Regions != 3 || stats.Evicted != 1 || stats.Entries != 43 {
			t.Fatalf("the first eviction left %+v, want three regions, one evicted, 43 items", stats)
		}
		f.requireHeld(t, keys[:64], among(keys[21:64]))
		f.fill(t, keys[64:85])
		if stats := f.disk.stats(); stats.Regions != 3 || stats.Evicted != 2 || stats.Entries != 43 {
			t.Fatalf("the second eviction left %+v, want three regions, two evicted, 43 items", stats)
		}
		f.requireHeld(t, keys, among(keys[42:85]))
		f.disk.checkInvariants(t)
	})
}

// Before the oldest region is given back, the items in it that were read since
// they were written are written again into the region kept free for that, and
// stay readable. The rest go. The pages are past the first 64 of their
// window, so the victim's bitmap names them from its second word.
func TestDiskSecondChanceKeepsWhatWasRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 4})
		keys := pages("va", 100, 164)
		f.fill(t, keys[:63])
		read := []diskKey{keys[3], keys[7], keys[11]}
		for _, key := range read {
			f.read(t, key)
		}
		f.fill(t, keys[63:])
		if stats := f.disk.stats(); stats.Rewritten != 3 || stats.Evicted != 1 || stats.Regions != 3 ||
			stats.Entries != 46 {
			t.Fatalf("the eviction left %+v, want three items written again, one region evicted", stats)
		}
		f.requireHeld(t, keys, among(append(slices.Clone(read), keys[21:]...)))
		for _, key := range read {
			f.read(t, key)
		}
		probes := f.runtime.Probes()
		if probes[ProbeDiskSecondChance] != 3 || probes[ProbeDiskFreeRegion] != 1 ||
			probes[ProbeDiskSecondChanceBounded] != 0 {
			t.Fatalf("the eviction reached %v, want three second chances in the free region", probes)
		}
		f.disk.checkInvariants(t)
	})
}

// A second chance writes at most half a region again, so an eviction always
// gives space back. Every item of the victim was read; the first ten fit in
// half a region and the rest go.
func TestDiskSecondChanceIsBoundedAtHalfARegion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 4})
		keys := pages("va", 0, 64)
		f.fill(t, keys[:63])
		for _, key := range keys[:testItemsPerRegion] {
			f.read(t, key)
		}
		f.fill(t, keys[63:])
		// Ten items of 3,050 bytes are 30,500 of the half region's 32,768.
		if stats := f.disk.stats(); stats.Rewritten != 10 || stats.Evicted != 1 || stats.Regions != 3 {
			t.Fatalf("the eviction left %+v, want ten items written again and one region evicted", stats)
		}
		f.requireHeld(t, keys, among(append(slices.Clone(keys[:10]), keys[21:]...)))
		if probes := f.runtime.Probes(); probes[ProbeDiskSecondChanceBounded] != 1 {
			t.Fatalf("the eviction reached %v, want the second chance stopped at its bound once", probes)
		}
		f.disk.checkInvariants(t)
	})
}

// A second chance is skipped when the write budget refuses it or the cache is
// over its share, and it stops where the region kept free would not be enough.
// The victim goes all the same.
func TestDiskSecondChanceGivesWay(t *testing.T) {
	t.Run("the write budget refuses it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiskFixture(t, diskFixtureConfig{regions: 4})
			keys := pages("va", 0, 64)
			f.fill(t, keys[:63])
			for _, key := range keys[:3] {
				f.read(t, key)
			}
			f.budget.refusing(WriteSecondChance)
			f.fill(t, keys[63:])
			if stats := f.disk.stats(); stats.Rewritten != 0 || stats.Evicted != 1 || stats.Regions != 3 {
				t.Fatalf("the eviction left %+v, want nothing written again and one region evicted", stats)
			}
			if refused := f.budget.refused[WriteSecondChance]; refused != 1 {
				t.Fatalf("the budget refused %d second chances, want the first, after which none was asked", refused)
			}
			f.requireHeld(t, keys, among(keys[21:]))
			f.disk.checkInvariants(t)
		})
	})
	t.Run("the cache is over its share", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiskFixture(t, diskFixtureConfig{regions: 4})
			keys := pages("va", 0, 64)
			f.fill(t, keys[:63])
			for _, key := range keys[:3] {
				f.read(t, key)
			}
			f.budget.share.Store(2 * testRegionBytes)
			f.fill(t, keys[63:])
			if asked := f.budget.asked[WriteSecondChance]; asked != 0 {
				t.Fatalf("over its share the disk asked for %d second chances, want none", asked)
			}
			// Three regions over a share of two: all three go, with no second
			// chance, and the new item opens the one region fills may use.
			if stats := f.disk.stats(); stats.Rewritten != 0 || stats.Evicted != 3 || stats.Regions != 1 ||
				stats.Entries != 1 {
				t.Fatalf("the eviction left %+v, want three regions evicted and only the new item", stats)
			}
			f.requireHeld(t, keys, among(keys[63:]))
			f.disk.checkInvariants(t)
		})
	})
	t.Run("the free region is taken", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiskFixture(t, diskFixtureConfig{regions: 4})
			keys := pages("va", 0, 64)
			f.fill(t, keys[:63])
			for _, key := range keys[:3] {
				f.read(t, key)
			}
			// A share of three regions holds the three it has and no more: the
			// second chance has no region to write into and stops, and the
			// fill takes two regions back.
			f.budget.share.Store(3 * testRegionBytes)
			f.fill(t, keys[63:])
			// It asked for the first, and found no region to write it in.
			if asked := f.budget.asked[WriteSecondChance]; asked != 1 {
				t.Fatalf("the disk asked for %d second chances, want the first alone", asked)
			}
			if stats := f.disk.stats(); stats.Rewritten != 0 || stats.Evicted != 2 || stats.Regions != 2 {
				t.Fatalf("the eviction left %+v, want nothing written again and two regions evicted", stats)
			}
			if probes := f.runtime.Probes(); probes[ProbeDiskFreeRegion] != 0 {
				t.Fatalf("the eviction reached %v, want no free region opened", probes)
			}
			f.requireHeld(t, keys, among(keys[42:]))
			f.disk.checkInvariants(t)
		})
	})
}

// Eviction always gives space back. Over a seeded mix of writes and reads that
// keeps the disk under pressure, every write that found the open region full
// and the share's fill regions taken gave at least one region back and left
// no more regions than it found, and no write ever left more than the fill
// regions held.
func TestDiskEvictionAlwaysGivesSpaceBack(t *testing.T) {
	for _, seed := range []uint64{1, 2, 3, 4} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDiskFixture(t, diskFixtureConfig{seed: seed, regions: 5})
				random := f.runtime.Random("disk-pressure")
				var written []diskKey
				pressured := 0
				for op := range 1500 {
					if len(written) > 0 && random.Chance(fmt.Sprintf("read/%d", op), 0.3) {
						key := written[random.Intn(fmt.Sprintf("which/%d", op), len(written))]
						if data, code, outcome := f.disk.readCode(f.ctx(t), key, nil); outcome == diskHit {
							if !bytes.Equal(data, f.model[key]) {
								t.Fatalf("page %d read back other bytes", key.Page)
							}
							f.disk.served(key, code)
						}
						continue
					}
					key := keyOf("va", uint64(random.Intn(fmt.Sprintf("page/%d", op), 4096)))
					length := 500 + int(key.Page*37%5500)
					size := itemHeaderBytes(key) + int64(length)
					f.disk.mu.Lock()
					full := f.disk.open == nil || !f.disk.open.fits(size, tableEntryBytes(key), testRegionBytes)
					_, present := f.disk.index.lookup(key, wholeEnvelope, false, false)
					f.disk.mu.Unlock()
					before := f.disk.stats()
					f.write(t, key, length)
					written = append(written, key)
					after := f.disk.stats()
					if after.Regions > 4 {
						t.Fatalf("a write left %d regions, more than the share's four fill regions", after.Regions)
					}
					if full && !present && before.Regions == 4 {
						pressured++
						if after.Evicted == before.Evicted || after.Regions > before.Regions {
							t.Fatalf("a write under pressure went from %+v to %+v, want a region given back", before,
								after)
						}
					}
				}
				// Each write under pressure gave back exactly the one region
				// its second chance left room for.
				want := map[uint64]int{1: 54, 2: 53, 3: 55, 4: 57}[seed]
				if evicted := f.disk.stats().Evicted; pressured != want || evicted != uint64(want) {
					t.Fatalf("%d writes found the disk under pressure and %d regions went, want %d of each",
						pressured, evicted, want)
				}
				f.disk.checkInvariants(t)
			})
		})
	}
}

// A read in flight holds its region. The region is evicted while the read is
// held: the index stops naming it at once, but its space is kept until the
// read finishes, and the read returns what was written.
func TestDiskReadInFlightKeepsItsRegion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 4})
		keys := pages("va", 0, 64)
		f.fill(t, keys[:63])
		gate := f.file.hold(regionBase(0), regionBase(1))
		type result struct {
			data    []byte
			outcome diskReadOutcome
		}
		done := make(chan result, 1)
		go func() {
			data, outcome := f.disk.read(f.ctx(t), keys[5], nil)
			done <- result{data, outcome}
		}()
		<-gate.entered
		f.fill(t, keys[63:])
		// The read's region still takes its space, so the fill gave the next
		// region back too: the read's region is held and unnamed, the second
		// is given back, and the third and the new one hold 22 items.
		if stats := f.disk.stats(); stats.Evicted != 1 || stats.Regions != 3 || stats.Entries != 22 {
			t.Fatalf("with a read in flight the eviction left %+v, want the region held and unnamed", stats)
		}
		if f.disk.has(f.ctx(t), keys[5]) {
			t.Fatal("the index still names an evicted region's item")
		}
		if probes := f.runtime.Probes(); probes[ProbeDiskEvictionWaitsForReader] != 1 {
			t.Fatalf("the eviction reached %v, want it waiting for the reader once", probes)
		}
		close(gate.release)
		got := <-done
		if got.outcome != diskHit || !bytes.Equal(got.data, f.model[keys[5]]) {
			t.Fatalf("the read in flight found %d and %d bytes, want what was written", got.outcome, len(got.data))
		}
		if stats := f.disk.stats(); stats.Evicted != 2 || stats.Regions != 2 {
			t.Fatalf("once the read finished the disk reports %+v, want the region given back", stats)
		}
		f.disk.checkInvariants(t)
	})
}

// The index costs an entry per window, not per page: a window lists its first
// sixteen pages and then keeps a bitmap, and scattered pages each cost an
// entry with one listed page.
func TestDiskIndexCostsAWindowNotAPage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 64})
		for _, step := range []struct {
			keys []diskKey
			cost int64
		}{
			{pages("va", 0, 16), windowEntryCharge + 16*listedItemCharge},
			{pages("va", 16, 17), windowEntryCharge + bitmapCharge + 17*denseItemCharge},
			{pages("va", 17, 300), windowEntryCharge + bitmapCharge + 300*denseItemCharge},
			{[]diskKey{keyOf("va", 512), keyOf("va", 1024), keyOf("va", 1536)},
				windowEntryCharge + bitmapCharge + 300*denseItemCharge +
					3*(windowEntryCharge+listedItemCharge)},
		} {
			for _, key := range step.keys {
				f.write(t, key, 100)
			}
			if used := f.disk.stats().IndexBytes; used != step.cost {
				t.Fatalf("the index costs %d bytes, want %d", used, step.cost)
			}
		}
		for _, key := range keysOf(f.model) {
			f.read(t, key)
		}
		f.disk.checkInvariants(t)
	})
}

// The index refuses writes rather than grow past its bound. Its bound is what
// three windows cost and the most one insert can add, so the fourth window
// reaches the bound exactly and the fifth is refused.
func TestDiskIndexRefusesWritesPastItsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		window := int64(windowEntryCharge + listedItemCharge)
		f := newDiskFixture(t, diskFixtureConfig{regions: 8, indexLimit: 3*window + maximumInsertCharge})
		for _, page := range []uint64{0, 512, 1024, 1536} {
			f.write(t, keyOf("va", page), 1000)
		}
		err := f.disk.write(f.ctx(t), keyOf("va", 2048), payloadOf(keyOf("va", 2048), 1000), WriteFillPublication)
		if !errors.Is(err, ErrDiskRefused) {
			t.Fatalf("a write past the index's bound returned %v, want %v", err, ErrDiskRefused)
		}
		if stats := f.disk.stats(); stats.IndexBytes != 4*window || stats.Refused != 1 || stats.Entries != 4 {
			t.Fatalf("the disk reports %+v, want four windows indexed and one write refused", stats)
		}
		f.disk.checkInvariants(t)
	})
}

// When its share falls, the disk gives regions back, oldest first and with no
// second chance, until it holds one region less than its share.
func TestDiskFitsAFallenShare(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 6})
		keys := pages("va", 0, 5*testItemsPerRegion)
		f.fill(t, keys)
		for _, key := range keys[:5] {
			f.read(t, key)
		}
		if stats := f.disk.stats(); stats.Regions != 5 {
			t.Fatalf("five full regions report %+v", stats)
		}
		f.budget.share.Store(3 * testRegionBytes)
		if err := f.disk.fit(f.ctx(t)); err != nil {
			t.Fatal(err)
		}
		if stats := f.disk.stats(); stats.Regions != 2 || stats.Evicted != 3 || stats.Rewritten != 0 {
			t.Fatalf("fitting a share of three regions left %+v, want two regions and none written again", stats)
		}
		f.requireHeld(t, keys, among(keys[3*testItemsPerRegion:]))
		f.disk.checkInvariants(t)
	})
}
