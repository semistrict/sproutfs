package checkpoint

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform/sim"
)

// reopen opens a new cache disk over the fixture's file, as a host that
// restarts does.
func (f *diskFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.restart(f.ctx(t)); err != nil {
		t.Fatal(err)
	}
}

// order is the sequence numbers of the disk's closed regions, oldest first,
// and the slot each is in.
func (d *cacheDisk) order() (sequences, slots []int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, region := range d.closed {
		sequences = append(sequences, int64(region.sequence))
		slots = append(slots, region.slot)
	}
	return sequences, slots
}

// recovered is what a disk's open did with the regions it found.
type recovered struct {
	fromTables, scanned, givenBack uint64
	regions, entries               int
}

func (d *cacheDisk) recovered() recovered {
	stats := d.stats()
	return recovered{fromTables: stats.FromTables, scanned: stats.Scanned, givenBack: stats.GivenBackOnOpen,
		regions: stats.Regions, entries: stats.Entries}
}

// A cache disk closed and opened again serves every item it held, and keeps
// its identity and its order. A share of three fill regions is filled four
// times over, so the oldest region is given back and its slot taken by the
// newest: the slots no longer say the order, and the tables' sequence numbers
// must. After the restart, the next eviction takes the oldest region, not the
// one in the lowest slot.
func TestDiskReadsBackWhatItHeldInItsOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 4})
		keys := pages("va", 0, 85)
		f.fill(t, keys[:84])
		before := f.disk.stats()
		f.disk.shutdown(f.ctx(t))
		f.reopen(t)
		if got := f.disk.recovered(); got != (recovered{fromTables: 3, regions: 3, entries: 63}) {
			t.Fatalf("the restart read back %+v, want three regions of 63 items from their tables", got)
		}
		if f.disk.stats().Identity != before.Identity {
			t.Fatalf("the restart changed the identity from %v to %v", before.Identity, f.disk.stats().Identity)
		}
		sequences, slots := f.disk.order()
		if !slices.Equal(sequences, []int64{2, 3, 4}) || !slices.Equal(slots, []int64{1, 2, 0}) {
			t.Fatalf("the restart ordered regions %v in slots %v, want 2, 3, 4 in slots 1, 2, 0", sequences, slots)
		}
		f.requireHeld(t, keys, among(keys[21:84]))
		// The file is the header's span and three regions: three slots, all
		// held.
		if f.disk.next != 3 || len(f.disk.free) != 0 {
			t.Fatalf("the restart counted %d slots with %v free, want three and none free", f.disk.next, f.disk.free)
		}
		f.disk.checkInvariants(t)
		// The next write evicts region 2, the oldest, whose slot is the
		// one it opens in.
		f.fill(t, keys[84:])
		f.requireHeld(t, keys, among(keys[42:]))
		if sequences, slots := f.disk.order(); !slices.Equal(sequences, []int64{3, 4}) || !slices.Equal(slots, []int64{2, 0}) {
			t.Fatalf("after the eviction the closed regions are %v in slots %v, want 3 and 4 in slots 2 and 0",
				sequences, slots)
		}
		for _, key := range keys[42:] {
			f.read(t, key)
		}
		f.disk.checkInvariants(t)
	})
}

// The region open when the host stopped has no table, and the restart gives
// it back. The closed region before it is read back from its table, and the
// slot the open region was in is the one the next region opens in.
func TestDiskGivesBackTheRegionOpenAtTheRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		keys := pages("va", 0, 31)
		f.fill(t, keys[:30])
		f.reopen(t)
		if got := f.disk.recovered(); got != (recovered{fromTables: 1, givenBack: 1, regions: 1, entries: 21}) {
			t.Fatalf("the restart read back %+v, want the closed region and the open one given back", got)
		}
		probes := f.runtime.Probes()
		if probes[ProbeDiskRegionFromTable] != 1 || probes[ProbeDiskRegionGivenBackOnOpen] != 1 {
			t.Fatalf("the restart reached %v, want one region from its table and one given back", probes)
		}
		f.requireHeld(t, keys, among(keys[:21]))
		for _, key := range keys[:21] {
			f.read(t, key)
		}
		f.fill(t, keys[30:])
		ops := f.file.operations()
		ops = ops[slices.Index(ops, "reopen"):]
		if !slices.Contains(ops, fmt.Sprintf("punch %d+%d", regionBase(1), testRegionBytes)) ||
			!slices.Contains(ops, fmt.Sprintf("allocate %d+%d", regionBase(1), testRegionBytes)) {
			t.Fatalf("after the restart the disk saw %q, want the open region's slot punched and opened again", ops)
		}
		f.read(t, keys[30])
		f.disk.checkInvariants(t)
	})
}

// tearTable writes zeros over the first half of the table that closes the
// region in slot, as a write the device kept only the end of leaves it.
func (f *diskFixture) tearTable(t *testing.T, slot int64) {
	t.Helper()
	table, err := readRegionTable(t.Context(), f.file, regionBase(slot), testRegionBytes)
	if err != nil {
		t.Fatal(err)
	}
	length := int64(diskTrailerSize)
	for _, item := range table.items {
		length += tableEntryBytes(item.key)
	}
	if _, err := f.file.WriteAt(t.Context(), make([]byte, length/2), regionBase(slot+1)-length); err != nil {
		t.Fatal(err)
	}
}

// damageItem flips one byte of the bytes of the at-th item of a region of
// testItemBytes items in slot.
func (f *diskFixture) damageItem(t *testing.T, slot int64, at int) {
	t.Helper()
	offset := regionBase(slot) + int64(at)*(46+testItemBytes) + 46 + 7
	buffer := make([]byte, 1)
	if err := readFull(t.Context(), f.file, buffer, offset); err != nil {
		t.Fatal(err)
	}
	if _, err := f.file.WriteAt(t.Context(), []byte{buffer[0] ^ 0xff}, offset); err != nil {
		t.Fatal(err)
	}
}

// A region whose table is torn is read back by scanning its items' headers.
// The intact items are indexed, and a damaged one is stepped over by the
// length its header gives. The region has lost its place in the order, so it
// is the oldest. A region whose scan finds nothing intact is given back.
func TestDiskScansARegionWhoseTableIsTorn(t *testing.T) {
	t.Run("a torn table", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiskFixture(t, diskFixtureConfig{regions: 8})
			keys := pages("va", 0, 63)
			f.fill(t, keys)
			f.disk.shutdown(f.ctx(t))
			// The second region's table is torn, and its sixth item damaged.
			f.tearTable(t, 1)
			f.damageItem(t, 1, 5)
			f.reopen(t)
			if got := f.disk.recovered(); got != (recovered{fromTables: 2, scanned: 1, regions: 3, entries: 62}) {
				t.Fatalf("the restart read back %+v, want two regions from their tables and one scanned", got)
			}
			if probes := f.runtime.Probes(); probes[ProbeDiskRegionScanned] != 1 {
				t.Fatalf("the restart reached %v, want one region scanned", probes)
			}
			sequences, slots := f.disk.order()
			if !slices.Equal(sequences, []int64{0, 1, 3}) || !slices.Equal(slots, []int64{1, 0, 2}) {
				t.Fatalf("the restart ordered regions %v in slots %v, want the scanned one first", sequences, slots)
			}
			f.requireHeld(t, keys, func(key diskKey) bool { return key != keys[26] })
			for _, key := range keys {
				if key != keys[26] {
					f.read(t, key)
				}
			}
			f.disk.checkInvariants(t)
		})
	})
	t.Run("nothing intact", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiskFixture(t, diskFixtureConfig{regions: 8})
			keys := pages("va", 0, 42)
			f.fill(t, keys)
			f.disk.shutdown(f.ctx(t))
			f.tearTable(t, 0)
			for at := range testItemsPerRegion {
				f.damageItem(t, 0, at)
			}
			f.reopen(t)
			if got := f.disk.recovered(); got != (recovered{fromTables: 1, givenBack: 1, regions: 1, entries: 21}) {
				t.Fatalf("the restart read back %+v, want the region with nothing intact given back", got)
			}
			f.requireHeld(t, keys, among(keys[21:]))
			f.disk.checkInvariants(t)
		})
	})
}

// A file that is not this cache's is emptied, given back whole, and made
// again under a new identity: one of another deployment, of another format,
// of another region size, or with a damaged header. A file of the same
// deployment keeps its identity and its generation across restarts. A table
// of another generation is one an older file left behind, and its region is
// given back.
func TestDiskEmptiesAFileThatIsNotItsOwn(t *testing.T) {
	for _, refused := range []struct {
		name   string
		change func(t *testing.T, f *diskFixture)
		// next says whether the generation is the old one's next, which
		// it is where the old header could be read.
		next bool
	}{
		{"another deployment", func(_ *testing.T, f *diskFixture) {
			f.settings.deployment = CacheDeployment{Store: "sim", Bucket: "test-bucket", Prefix: "other/"}
		}, true},
		{"another region size", func(_ *testing.T, f *diskFixture) {
			f.settings.regionBytes = 2 * testRegionBytes
		}, true},
		{"another format", func(t *testing.T, f *diskFixture) {
			if _, err := f.file.WriteAt(t.Context(), []byte{diskFormatVersion + 1}, 4); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"a damaged header", func(t *testing.T, f *diskFixture) {
			if _, err := f.file.WriteAt(t.Context(), []byte{0xff}, 20); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(refused.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDiskFixture(t, diskFixtureConfig{regions: 8})
				keys := pages("va", 0, 42)
				f.fill(t, keys)
				f.disk.shutdown(f.ctx(t))
				before := f.disk.stats().Identity
				generation := f.disk.generation
				refused.change(t, f)
				f.reopen(t)
				if got := f.disk.recovered(); got != (recovered{}) {
					t.Fatalf("the restart over a file not its own read back %+v, want nothing", got)
				}
				if f.disk.stats().Identity == before {
					t.Fatalf("the restart kept the identity %v of a file not its own", before)
				}
				if (f.disk.generation == generation+1) != refused.next {
					t.Fatalf("the restart took generation %d after %d, want the next one: %v", f.disk.generation,
						generation, refused.next)
				}
				if probes := f.runtime.Probes(); probes[ProbeDiskHeaderRefused] != 1 {
					t.Fatalf("the restart reached %v, want the header refused once", probes)
				}
				size, err := f.file.Size(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if size != diskHeaderBytes(f.settings.deployment) {
					t.Fatalf("the emptied file is %d bytes, want only its header's %d", size,
						diskHeaderBytes(f.settings.deployment))
				}
				f.requireHeld(t, keys, among(nil))
				f.model = make(map[diskKey][]byte)
				f.fill(t, keys[:3])
				for _, key := range keys[:3] {
					f.read(t, key)
				}
				f.disk.checkInvariants(t)
			})
		})
	}
	t.Run("the same deployment", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiskFixture(t, diskFixtureConfig{regions: 8})
			keys := pages("va", 0, 42)
			f.fill(t, keys)
			f.disk.shutdown(f.ctx(t))
			before, generation := f.disk.stats().Identity, f.disk.generation
			for range 2 {
				f.reopen(t)
				f.disk.shutdown(f.ctx(t))
			}
			f.reopen(t)
			if got := f.disk.recovered(); got != (recovered{fromTables: 2, regions: 2, entries: 42}) {
				t.Fatalf("the third restart read back %+v, want both regions", got)
			}
			if f.disk.stats().Identity != before || f.disk.generation != generation {
				t.Fatalf("the restarts went from identity %v and generation %d to %v and %d", before, generation,
					f.disk.stats().Identity, f.disk.generation)
			}
			if probes := f.runtime.Probes(); probes[ProbeDiskHeaderRefused] != 0 {
				t.Fatalf("the restarts reached %v, want no header refused", probes)
			}
			for _, key := range keys {
				f.read(t, key)
			}
		})
	})
	t.Run("a table of another generation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newDiskFixture(t, diskFixtureConfig{regions: 8})
			keys := pages("va", 0, 42)
			f.fill(t, keys)
			f.disk.shutdown(f.ctx(t))
			table, err := readRegionTable(t.Context(), f.file, regionBase(0), testRegionBytes)
			if err != nil {
				t.Fatal(err)
			}
			old := encodeTable(table.sequence, f.disk.generation-1, table.items)
			if _, err := f.file.WriteAt(t.Context(), old, regionBase(1)-int64(len(old))); err != nil {
				t.Fatal(err)
			}
			f.reopen(t)
			if got := f.disk.recovered(); got != (recovered{fromTables: 1, givenBack: 1, regions: 1, entries: 21}) {
				t.Fatalf("the restart read back %+v, want the region of the old generation given back", got)
			}
			f.requireHeld(t, keys, among(keys[21:]))
		})
	})
}

// A cache whose share fell while the host was down gives regions back, oldest
// first, before it serves anything: it holds its share less one region.
func TestDiskOverItsShareGivesRegionsBackOnOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		keys := pages("va", 0, 5*testItemsPerRegion)
		f.fill(t, keys)
		f.disk.shutdown(f.ctx(t))
		f.budget.share.Store(3 * testRegionBytes)
		f.reopen(t)
		stats := f.disk.stats()
		if stats.FromTables != 5 || stats.Regions != 2 || stats.Evicted != 3 || stats.Entries != 42 {
			t.Fatalf("the restart over a share of three regions left %+v, want five read back and three given back",
				stats)
		}
		f.requireHeld(t, keys, among(keys[3*testItemsPerRegion:]))
		f.disk.checkInvariants(t)
	})
}

// The index rebuilt on a restart keeps to its memory bound: the newest
// regions are indexed first, and the region that would take the index past
// its bound is given back with every one older. Each region here holds 21
// pages of one window, an entry of 244 bytes, and the bound holds two.
func TestDiskRestartKeepsTheIndexToItsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		keys := pages("va", 0, 4*testItemsPerRegion)
		f.fill(t, keys)
		f.disk.shutdown(f.ctx(t))
		entry := int64(windowEntryCharge + bitmapCharge + denseItemCharge*testItemsPerRegion)
		f.settings.indexLimit = 2 * entry
		f.reopen(t)
		if got := f.disk.recovered(); got != (recovered{fromTables: 2, givenBack: 2, regions: 2, entries: 42}) {
			t.Fatalf("the restart read back %+v, want the two newest regions", got)
		}
		if used := f.disk.stats().IndexBytes; used != 2*entry {
			t.Fatalf("the index costs %d bytes, want %d", used, 2*entry)
		}
		f.requireHeld(t, keys, among(keys[2*testItemsPerRegion:]))
		f.disk.checkInvariants(t)
	})
}

// A second chance can leave one item in two regions when the host stops
// between the write and the eviction. The restart keeps the newer copy, in
// the newer region, so the item outlives the older region's eviction.
func TestDiskRestartKeepsTheNewerCopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		keys := pages("va", 0, 42)
		f.fill(t, keys)
		// The first region's first item is written again into the third,
		// as a second chance does, and the host stops before the first is
		// given back.
		if _, err := f.disk.append(f.ctx(t), keys[0], f.model[keys[0]], WriteSecondChance); err != nil {
			t.Fatal(err)
		}
		f.disk.shutdown(f.ctx(t))
		f.reopen(t)
		if got := f.disk.recovered(); got != (recovered{fromTables: 3, regions: 3, entries: 42}) {
			t.Fatalf("the restart read back %+v, want three regions and each item once", got)
		}
		f.disk.mu.Lock()
		location, _ := f.disk.index.lookup(keys[0])
		slot := location.entry.region.slot
		f.disk.mu.Unlock()
		if slot != 2 {
			t.Fatalf("the index names the item in slot %d, want the newer copy in slot 2", slot)
		}
		f.read(t, keys[0])
		f.disk.checkInvariants(t)
	})
}

// powerLossRestart fills a disk with 30 items, the first region's 21 and nine
// of the second's, on a device that may tear and garble what was not synced,
// and loses the power once moment operations have reached the file: writes 2
// to 22 fill the first region, 23 syncs it, 24 writes its table, 25 syncs
// that, 26 opens the second region and 27 on fill it. The disk is then opened
// again. Every read is the bytes written or a miss, nothing of the region open
// at the loss is held, and every item named by a table that reads back intact
// is served. It reports what the open did.
func powerLossRestart(t *testing.T, seed uint64, moment func(*sim.Runtime) int) (int, recovered) {
	t.Helper()
	f := newDiskFixture(t, diskFixtureConfig{seed: seed, regions: 8,
		disk: sim.DiskConfig{PowerLossFaults: true, PowerLossKillMode: sim.FullCorruption}})
	lostAfter := moment(f.runtime)
	f.file.loseAfter(lostAfter)
	var written []diskKey
	for _, key := range pages("va", 0, 30) {
		data := payloadOf(key, testItemBytes)
		err := f.disk.write(f.ctx(t), key, data, WriteFillPublication)
		if f.file.powerLost() {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		f.model[key] = data
		written = append(written, key)
	}
	// What the region open at the loss held.
	var open []diskKey
	f.disk.mu.Lock()
	for _, key := range written {
		if location, found := f.disk.index.lookup(key); found && location.entry.region == f.disk.open {
			open = append(open, key)
		}
	}
	f.disk.mu.Unlock()
	f.reopen(t)
	for _, key := range open {
		if f.disk.has(key) {
			t.Errorf("page %d of the region open at the loss is still held", key.Page)
		}
	}
	for slot := range int64(2) {
		table, err := readRegionTable(t.Context(), f.file, regionBase(slot), testRegionBytes)
		if err != nil {
			continue
		}
		for _, item := range table.items {
			if data, outcome := f.disk.read(f.ctx(t), item.key); outcome != diskHit ||
				!bytes.Equal(data, f.model[item.key]) {
				t.Errorf("page %d, named by an intact table, read back as %d", item.key.Page, outcome)
			}
		}
	}
	for _, key := range written {
		if data, outcome := f.disk.read(f.ctx(t), key); outcome == diskHit && !bytes.Equal(data, f.model[key]) {
			t.Errorf("page %d read back other bytes", key.Page)
		}
	}
	f.disk.checkInvariants(t)
	return lostAfter, f.disk.recovered()
}

// lossOutcome is what one power loss's restart is expected to find: after how
// many operations the power was lost, and what the open did.
type lossOutcome struct {
	lostAfter int
	recovered
}

// Losing power at a seeded moment while a region fills, as it closes, or while
// the next one fills, leaves a disk that reads back safely. Where the power is
// lost decides what is read back: nothing before the first region's table is
// synced, that region after it, and never the region open at the loss. A
// region given back is counted only where its first item survived the loss;
// one whose first item did not looks never written.
func TestDiskRestartsAfterPowerLoss(t *testing.T) {
	one := recovered{fromTables: 1, regions: 1, entries: 21}
	openGone := recovered{fromTables: 1, givenBack: 1, regions: 1, entries: 21}
	gone := recovered{givenBack: 1}
	want := map[uint64]lossOutcome{
		1: {24, gone}, 2: {29, one}, 3: {25, one}, 4: {33, openGone}, 5: {21, gone}, 6: {23, gone},
		7: {27, one}, 8: {32, openGone}, 9: {26, one}, 10: {26, one}, 11: {29, one}, 12: {22, recovered{}},
		13: {33, one}, 14: {31, one}, 15: {26, one}, 16: {33, one},
	}
	for seed := uint64(1); seed <= 16; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				lostAfter, got := powerLossRestart(t, seed, func(runtime *sim.Runtime) int {
					return 20 + runtime.Random("power-loss").Intn("moment", 14)
				})
				if (lossOutcome{lostAfter, got}) != want[seed] {
					t.Fatalf("the power was lost after operation %d and the restart read back %+v, want %+v",
						lostAfter, got, want[seed])
				}
			})
		})
	}
}

// A table written and not yet synced when the power goes may survive whole,
// torn with its trailer, or without it. A whole one is read back, a torn one
// is scanned, and a region left with no trailer is given back.
func TestDiskRestartsAfterPowerLossBeforeATablesSync(t *testing.T) {
	want := map[uint64]recovered{
		1: {givenBack: 1},
		2: {scanned: 1, regions: 1, entries: 21},
		3: {fromTables: 1, regions: 1, entries: 21},
		4: {givenBack: 1},
	}
	for seed := uint64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, got := powerLossRestart(t, seed, func(*sim.Runtime) int { return 24 })
				if got != want[seed] {
					t.Fatalf("the restart read back %+v, want %+v", got, want[seed])
				}
			})
		})
	}
}

// A scan reads items up to the region's trailer and no further: an item that
// ends at the trailer is found, an item of a header alone that ends there too,
// and an item that runs one byte into the trailer is not.
func TestDiskScanStopsAtTheTrailer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limit := int64(testRegionBytes - diskTrailerSize)
		empty := encodeItem(diskKey{}, wholeEnvelope, nil)
		key := keyOf("va", 0)
		for _, test := range []struct {
			name  string
			items [][]byte
			want  []tableItem
		}{
			{"two items that end at the trailer", [][]byte{
				encodeItem(key, wholeEnvelope, payloadOf(key, int(limit-diskItemFixed-46))), empty},
				[]tableItem{{key: key, code: wholeEnvelope, offset: 0, length: uint32(limit - diskItemFixed - 46)},
					{code: wholeEnvelope, offset: uint32(limit - diskItemFixed)}}},
			{"an item one byte into the trailer", [][]byte{
				encodeItem(key, wholeEnvelope, payloadOf(key, int(limit-46+1)))}, nil},
		} {
			file := diskTableFile(t, nil)
			at := int64(0)
			for _, item := range test.items {
				if _, err := file.WriteAt(t.Context(), item, at); err != nil {
					t.Fatal(err)
				}
				at += int64(len(item))
			}
			got, err := scanRegion(t.Context(), file, 0, testRegionBytes)
			if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("scanning %s found %+v, %v, want %+v", test.name, got, err, test.want)
			}
		}
	})
}

// A region whose allocation fails is never opened: its slot is free again, and
// the write is refused.
func TestDiskARegionThatCannotBeAllocatedIsNotOpened(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		f.simDisk.FailNext(sim.DiskAllocate, 1)
		key := keyOf("va", 0)
		if err := f.disk.write(f.ctx(t), key, payloadOf(key, testItemBytes), WriteFillPublication); !errors.Is(err, ErrDiskRefused) {
			t.Fatalf("a write whose region could not be allocated returned %v, want %v", err, ErrDiskRefused)
		}
		if stats := f.disk.stats(); stats.Regions != 0 || stats.Refused != 1 {
			t.Fatalf("the refused write left %+v, want no region", stats)
		}
		f.disk.checkInvariants(t)
		f.fill(t, []diskKey{key})
		f.read(t, key)
		f.disk.checkInvariants(t)
	})
}

// A region given back whose punch failed keeps its old table at its end. When
// its slot is opened again, that trailer is cleared, so a restart with the new
// region open gives it back rather than read the old table, which names items
// the new region has written over.
func TestDiskReusedSlotForgetsItsOldTable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 4})
		keys := pages("va", 0, 64)
		f.fill(t, keys[:63])
		f.simDisk.FailNext(sim.DiskPunchHole, 1)
		f.fill(t, keys[63:])
		f.reopen(t)
		if got := f.disk.recovered(); got != (recovered{fromTables: 2, givenBack: 1, regions: 2, entries: 42}) {
			t.Fatalf("the restart read back %+v, want the reused slot's open region given back", got)
		}
		f.requireHeld(t, keys, among(keys[21:63]))
		for _, key := range keys[21:63] {
			f.read(t, key)
		}
		if lost := f.disk.stats().Lost; lost != 0 || f.file.lied() != 0 {
			t.Fatalf("the disk lost %d copies and lied %d times, want neither", lost, f.file.lied())
		}
		f.disk.checkInvariants(t)
	})
}
