package checkpoint

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// otherCache is a cache the stripe tests list beside the disk's own.
var otherCache = CacheIdentity{0xee, 0x01}

// stripeCodes are the codes of the table with parity, which the stripe tests
// list two caches under.
var stripeCodes = []rank.Code{{K: 1, M: 1}, {K: 2, M: 1}, {K: 2, M: 2}, {K: 4, M: 2}}

// windowKeys is the first page of each of count windows of vm's ram0.
func windowKeys(vm string, count int) []diskKey {
	keys := make([]diskKey, count)
	for at := range keys {
		keys[at] = keyOf(vm, uint64(at)*uint64(windowSpan(Geometry{PageSize: PageSize4KiB})))
	}
	return keys
}

// heldIndices is the indices of key's envelope under code the disk gives back
// by (page, index), each checked against the stripe the envelope splits into.
func (f *diskFixture) heldIndices(t *testing.T, key diskKey, code rank.Code) []int {
	t.Helper()
	stripes, err := stripe.Split(code, f.model[key])
	if err != nil {
		t.Fatal(err)
	}
	var held []int
	for index := range code.Width() {
		read, outcome := f.disk.readStripe(f.ctx(t), key, code, index)
		switch outcome {
		case diskHit:
			if read.Code != code || read.Index != index || read.Length != len(f.model[key]) ||
				!bytes.Equal(read.Bytes, stripes[index].Bytes) {
				t.Fatalf("stripe %d of %s of page %d reads back as %v", index, code, key.Page, read)
			}
			held = append(held, index)
		case diskAbsent:
		default:
			t.Fatalf("stripe %d of %s of page %d found %d", index, code, key.Page, outcome)
		}
	}
	return held
}

// A disk keeps of each envelope the stripes the list of caches it follows
// puts on its own cache, and no other: for each window, the indices whose
// holder is this cache, under the list's code. Each reads back by its page
// and its index as the stripe the envelope splits into. Where it holds k of
// them, the envelope rebuilds from its own disk; otherwise a read is a miss.
func TestDiskKeepsTheStripesItsCacheIsRankedFor(t *testing.T) {
	for _, code := range stripeCodes {
		t.Run(code.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDiskFixture(t, diskFixtureConfig{regions: 8, clusterPercent: 100, caches: func(self CacheIdentity) rank.List {
					return listOf(code, self, otherCache)
				}})
				held, _ := f.disk.following()
				list := held.List()
				several := uint64(0)
				for _, key := range windowKeys("va", 24) {
					f.write(t, key, testItemBytes)
					var want []int
					for index, holder := range list.Holders(rank.PageWindow(key.Identity, PageSize4KiB)) {
						if holder.Identity == f.disk.identity {
							want = append(want, index)
						}
					}
					if len(want) > 1 {
						several++
					}
					if held := f.heldIndices(t, key, code); !slices.Equal(held, want) || len(want) == 0 {
						t.Fatalf("page %d holds indices %v of %s, want %v", key.Page, held, code, want)
					}
					if !f.disk.has(f.ctx(t), key) {
						t.Fatalf("page %d is not held whole by its placement", key.Page)
					}
					data, outcome := f.disk.read(f.ctx(t), key, selfChecked)
					if len(want) >= code.K {
						if outcome != diskHit || !bytes.Equal(data, f.model[key]) {
							t.Fatalf("page %d with %v of %s read back %d and %d bytes", key.Page, want, code, outcome,
								len(data))
						}
					} else if outcome != diskAbsent {
						t.Fatalf("page %d with %v of %s found %d, want a miss", key.Page, want, code, outcome)
					}
				}
				if probes := f.runtime.Probes(); probes[ProbeDiskSeveralStripes] != several {
					t.Fatalf("the writes reached %v, want %d that kept several stripes", probes, several)
				}
				f.disk.checkInvariants(t)
			})
		})
	}
}

// The cluster cache is turned on for a share of windows. A window inside it
// is kept as the stripes the list puts on this cache, under the list's code;
// every other window is kept whole under 1+0, whatever the list says, and
// reads back from this disk alone. At 0 that is every window, so a host
// keeps everything as it did before there were stripes.
func TestDiskStripesOnlyTheWindowsInItsShare(t *testing.T) {
	code := rank.Code{K: 2, M: 2}
	for _, percent := range []int{0, 50} {
		t.Run(fmt.Sprintf("%d-percent", percent), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDiskFixture(t, diskFixtureConfig{regions: 8, clusterPercent: percent,
					caches: func(self CacheIdentity) rank.List { return listOf(code, self, otherCache) }})
				held, _ := f.disk.following()
				list := held.List()
				inside := 0
				for _, key := range windowKeys("va", 40) {
					f.write(t, key, testItemBytes)
					window := rank.PageWindow(key.Identity, PageSize4KiB)
					var wantWhole, wantStriped []int
					if window.InShare(percent) {
						inside++
						for index, holder := range list.Holders(window) {
							if holder.Identity == f.disk.identity {
								wantStriped = append(wantStriped, index)
							}
						}
					} else {
						wantWhole = []int{0}
					}
					whole, striped := f.heldIndices(t, key, wholeCode), f.heldIndices(t, key, code)
					if !slices.Equal(whole, wantWhole) || !slices.Equal(striped, wantStriped) {
						t.Fatalf("page %d holds %v whole and %v of %s, want %v and %v", key.Page, whole, striped, code,
							wantWhole, wantStriped)
					}
					f.read(t, key)
				}
				if want := map[int]int{0: 0, 50: 24}[percent]; inside != want {
					t.Fatalf("%d of 40 windows are inside a share of %d percent, want %d", inside, percent, want)
				}
				f.disk.checkInvariants(t)
			})
		})
	}
}

// A disk that follows no list keeps each envelope whole, as stripe 0 of 1+0,
// which is what it held before there were stripes.
func TestDiskThatFollowsNoListKeepsEnvelopesWhole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8})
		key := keyOf("va", 0)
		f.write(t, key, testItemBytes)
		if held := f.heldIndices(t, key, wholeCode); !slices.Equal(held, []int{0}) {
			t.Fatalf("the disk holds indices %v of 1+0, want the envelope whole", held)
		}
		read, _ := f.disk.readStripe(f.ctx(t), key, wholeCode, 0)
		if !bytes.Equal(read.Bytes, f.model[key]) {
			t.Fatal("the one stripe of 1+0 is not the envelope")
		}
		f.read(t, key)
	})
}

// A list that does not rank this cache for a window, as one that has not
// heard of it yet, keeps nothing of the window here.
func TestDiskKeepsNothingOfAWindowItIsNotRankedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newDiskFixture(t, diskFixtureConfig{regions: 8, clusterPercent: 100, caches: func(CacheIdentity) rank.List {
			return listOf(rank.Code{K: 1, M: 1}, otherCache)
		}})
		key := keyOf("va", 0)
		if err := f.disk.write(f.ctx(t), key, payloadOf(key, testItemBytes), WriteFillPublication); err != nil {
			t.Fatal(err)
		}
		if stats := f.disk.stats(); stats.Entries != 0 || stats.Regions != 0 {
			t.Fatalf("a disk not ranked for the window reports %+v, want nothing kept", stats)
		}
		if f.disk.has(f.ctx(t), key) {
			t.Fatal("a disk that keeps nothing of a window says it has it")
		}
	})
}

// A list shorter than the code is wide takes the stripes round its caches. A
// cache alone in a list of 2+2 holds all four indices of every page of a
// window, next to each other in index order in one run, so the window costs
// one entry. Each reads back by its page and its index, before a restart and
// after, from the region's table and from its items when the table is torn.
func TestDiskHoldsEveryIndexOfAPageRoundAShortList(t *testing.T) {
	code := rank.Code{K: 2, M: 2}
	for _, torn := range []bool{false, true} {
		t.Run(fmt.Sprintf("torn-%v", torn), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDiskFixture(t, diskFixtureConfig{regions: 8, clusterPercent: 100, caches: func(self CacheIdentity) rank.List {
					return listOf(code, self)
				}})
				keys := pages("va", 0, 20)
				for _, key := range keys {
					f.write(t, key, 1000)
				}
				// Twenty pages of four stripes, one entry: listed to sixteen
				// items, then a bitmap.
				if stats := f.disk.stats(); stats.Entries != 80 ||
					stats.IndexBytes != windowEntryCharge+bitmapCharge+80*denseItemCharge {
					t.Fatalf("the disk reports %+v, want 80 stripes of one window in one entry", stats)
				}
				if probes := f.runtime.Probes(); probes[ProbeDiskSeveralStripes] != 20 {
					t.Fatalf("the writes reached %v, want each to keep several stripes", probes)
				}
				check := func(when string) {
					for _, key := range keys {
						if held := f.heldIndices(t, key, code); !slices.Equal(held, []int{0, 1, 2, 3}) {
							t.Fatalf("%s page %d holds indices %v, want all four", when, key.Page, held)
						}
						f.read(t, key)
					}
				}
				check("before a restart")
				f.disk.shutdown(f.ctx(t))
				if torn {
					f.tearTable(t, 0)
				}
				f.reopen(t)
				check("after a restart")
				f.disk.checkInvariants(t)
			})
		})
	}
}

// A read rebuilds an envelope from any k indices it holds: the data stripes
// alone copy it, and with one or both of the data stripes of 2+2 forgotten,
// the parity stripes decode it.
func TestDiskRebuildsAnEnvelopeFromItsParity(t *testing.T) {
	code := rank.Code{K: 2, M: 2}
	for _, c := range []struct {
		forgotten []int
		decoded   uint64
	}{{[]int{2, 3}, 0}, {[]int{1, 3}, 1}, {[]int{0, 1}, 1}} {
		t.Run(fmt.Sprint(c.forgotten), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDiskFixture(t, diskFixtureConfig{regions: 8, clusterPercent: 100, caches: func(self CacheIdentity) rank.List {
					return listOf(code, self)
				}})
				key := keyOf("va", 0)
				f.write(t, key, testItemBytes)
				f.disk.mu.Lock()
				for _, index := range c.forgotten {
					location, _ := f.disk.index.lookup(key, indexOf(code, index), false, false)
					f.disk.index.forget(location)
				}
				f.disk.mu.Unlock()
				f.read(t, key)
				if probes := f.runtime.Probes(); probes[ProbeDiskStripesDecoded] != c.decoded {
					t.Fatalf("the read reached %v, want %d decodes from parity", probes, c.decoded)
				}
			})
		})
	}
}

// An empty envelope is kept and read back, whole and as the empty stripes of
// 2+2.
func TestDiskKeepsAnEmptyEnvelope(t *testing.T) {
	for _, code := range []rank.Code{wholeCode, {K: 2, M: 2}} {
		t.Run(code.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newDiskFixture(t, diskFixtureConfig{regions: 8, clusterPercent: 100, caches: func(self CacheIdentity) rank.List {
					return listOf(code, self)
				}})
				key := keyOf("va", 0)
				if err := f.disk.write(f.ctx(t), key, nil, WriteFillPublication); err != nil {
					t.Fatal(err)
				}
				data, outcome := f.disk.read(f.ctx(t), key, nil)
				if outcome != diskHit || len(data) != 0 {
					t.Fatalf("an empty envelope read back as %d and %d bytes", outcome, len(data))
				}
				if stats := f.disk.stats(); stats.Entries != code.Width() {
					t.Fatalf("the disk reports %+v, want %d empty stripes", stats, code.Width())
				}
			})
		})
	}
}

// A disk's key names the window the list of caches ranks: the span of its
// pages, or its segment.
func TestADiskKeysWindowIsTheOneTheListRanks(t *testing.T) {
	ref := control.Ref{VM: "va", Sequence: 3}
	page := control.Identity{Ref: ref, Volume: "ram0", Page: 1029}
	for _, c := range []struct {
		key  diskKey
		want rank.Window
	}{
		{pageDiskKey(page, Geometry{PageSize: PageSize4KiB}), rank.PageWindow(page, PageSize4KiB)},
		{pageDiskKey(page, Geometry{PageSize: PageSize2MiB}), rank.PageWindow(page, PageSize2MiB)},
		{segmentDiskKey("ram0", 7, ref), rank.SegmentWindow(ref, "ram0", 7)},
	} {
		if got := c.key.rankWindow(); got != c.want {
			t.Fatalf("the key of %+v is in window %+v, want %+v", c.key, got, c.want)
		}
	}
}

// A run's set of indices is fixed from its second page, and a page begins
// only once the one before holds every index of the set. In a run of pages
// holding indices 0 and 1, a page that holds only 0 ends the run: the
// index finds its 0 and not its 1, and the next page starts a run of its
// own. Twenty items are past the point where the run keeps a bitmap.
func TestDiskIndexKeepsARunsSetOfIndices(t *testing.T) {
	region := &diskRegion{}
	index := newDiskIndex()
	code := rank.Code{K: 1, M: 1}
	offset := int64(0)
	insert := func(page uint64, stripe int) {
		key := keyOf("va", page)
		index.insert(key, indexOf(code, stripe), region, offset, 10)
		offset += itemHeaderBytes(key) + 10
	}
	for page := range uint64(10) {
		insert(page, 0)
		insert(page, 1)
	}
	insert(10, 0)
	insert(11, 0)
	if len(region.entries) != 2 {
		t.Fatalf("the items made %d runs, want the eleventh page to end the first", len(region.entries))
	}
	run := region.entries[0]
	items := 0
	run.each(func(uint16, uint8, diskLocation) { items++ })
	if run.present == nil || items != 21 || run.stripes != 0b11 {
		t.Fatalf("the first run holds %d items of indices %b, dense %v; want 21 of 0 and 1 in a bitmap", items,
			run.stripes, run.present != nil)
	}
	if _, found := index.lookup(keyOf("va", 10), indexOf(code, 0), false, false); !found {
		t.Fatal("the index does not find index 0 of the run's last page")
	}
	if _, found := index.lookup(keyOf("va", 10), indexOf(code, 1), false, false); found {
		t.Fatal("the index finds an index the run's last page does not hold")
	}
	if location, found := index.lookup(keyOf("va", 11), indexOf(code, 0), false, false); !found ||
		location.entry != region.entries[1] {
		t.Fatal("the page after a short one is not in a run of its own")
	}
}

// rewriteStripe writes stripe index of key's envelope under code again in
// place, with one byte of it changed and its checksum made to hold: a stripe
// whose own checks pass and whose bytes are not the envelope's.
func (f *diskFixture) rewriteStripe(t *testing.T, key diskKey, code rank.Code, index int) {
	t.Helper()
	read, outcome := f.disk.readStripe(f.ctx(t), key, code, index)
	if outcome != diskHit {
		t.Fatalf("stripe %d of page %d found %d", index, key.Page, outcome)
	}
	read.Bytes = slices.Clone(read.Bytes)
	read.Bytes[len(read.Bytes)/2] ^= 0x40
	f.disk.mu.Lock()
	location, _ := f.disk.index.lookup(key, indexOf(code, index), false, false)
	f.disk.mu.Unlock()
	if _, err := f.file.WriteAt(t.Context(), encodeItem(key, read), location.offset); err != nil {
		t.Fatal(err)
	}
}

// A stripe whose own checks pass and whose bytes are wrong is found when a
// read holds more than k: the first k fail the envelope's check, another set
// passes, and the wrong one is named and forgotten. The read returns what was
// written, and a later read rebuilds without the wrong stripe.
func TestDiskFindsAndForgetsAWrongStripe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 2, M: 2}
		f := newDiskFixture(t, diskFixtureConfig{regions: 8, clusterPercent: 100, caches: func(self CacheIdentity) rank.List {
			return listOf(code, self)
		}})
		key := keyOf("va", 0)
		f.write(t, key, testItemBytes)
		f.rewriteStripe(t, key, code, 0)
		f.read(t, key)
		if probes := f.runtime.Probes(); probes[ProbeDiskWrongStripe] != 1 {
			t.Fatalf("the read reached %v, want the wrong stripe found", probes)
		}
		if _, outcome := f.disk.readStripe(f.ctx(t), key, code, 0); outcome != diskAbsent {
			t.Fatalf("the wrong stripe reads back as %d, want it forgotten", outcome)
		}
		if held := f.heldIndices(t, key, code); !slices.Equal(held, []int{1, 2, 3}) {
			t.Fatalf("the disk holds indices %v, want the three right ones", held)
		}
		if stats := f.disk.stats(); stats.Lost != 1 || stats.Entries != 3 {
			t.Fatalf("the disk reports %+v, want one stripe lost and three held", stats)
		}
		f.read(t, key)
		f.disk.checkInvariants(t)
	})
}

// With exactly k stripes and one wrong, which one is wrong cannot be told: the
// read is a miss, and neither is kept.
func TestDiskForgetsKStripesThatRebuildNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 2, M: 2}
		f := newDiskFixture(t, diskFixtureConfig{regions: 8, clusterPercent: 100, caches: func(self CacheIdentity) rank.List {
			return listOf(code, self)
		}})
		key := keyOf("va", 0)
		f.write(t, key, testItemBytes)
		f.disk.mu.Lock()
		for _, index := range []int{2, 3} {
			location, _ := f.disk.index.lookup(key, indexOf(code, index), false, false)
			f.disk.index.forget(location)
		}
		f.disk.mu.Unlock()
		f.rewriteStripe(t, key, code, 1)
		if data, outcome := f.disk.read(f.ctx(t), key, selfChecked); outcome != diskWrongStripe || data != nil {
			t.Fatalf("k stripes with one wrong found %d and %d bytes, want a miss", outcome, len(data))
		}
		if held := f.heldIndices(t, key, code); len(held) != 0 {
			t.Fatalf("the disk still holds indices %v", held)
		}
		f.disk.checkInvariants(t)
	})
}

// A stripe of another code is a miss, never part of an envelope of this one.
// A disk that kept a page under 2+1 and now follows a list of 2+2 finds
// nothing for it, keeps the old stripes for a list of 2+1 again, and keeps
// the page again under 2+2 beside them.
func TestDiskReadsNoStripeOfAnotherCode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 2, M: 1}
		f := newDiskFixture(t, diskFixtureConfig{regions: 8, clusterPercent: 100, caches: func(self CacheIdentity) rank.List {
			return listOf(code, self)
		}})
		key := keyOf("va", 0)
		f.write(t, key, testItemBytes)
		self := f.disk.identity
		f.disk.follow(membership.NewFixed(servingOf(listOf(rank.Code{K: 2, M: 2}, self))), self)
		if data, outcome := f.disk.read(f.ctx(t), key, selfChecked); outcome != diskAbsent || data != nil {
			t.Fatalf("a page kept under 2+1 read under 2+2 found %d and %d bytes, want a miss", outcome, len(data))
		}
		if probes := f.runtime.Probes(); probes[ProbeDiskStripeOfAnotherCode] != 1 {
			t.Fatalf("the read reached %v, want a stripe of another code found", probes)
		}
		if held := f.heldIndices(t, key, code); !slices.Equal(held, []int{0, 1, 2}) {
			t.Fatalf("the disk holds indices %v of 2+1, want all three still", held)
		}
		if f.disk.has(f.ctx(t), key) {
			t.Fatal("the disk says it holds under 2+2 a page it kept under 2+1")
		}
		f.write(t, key, testItemBytes)
		f.read(t, key)
		f.disk.follow(membership.NewFixed(servingOf(listOf(code, self))), self)
		f.read(t, key)
		f.disk.checkInvariants(t)
	})
}
