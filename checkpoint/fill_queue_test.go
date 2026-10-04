package checkpoint

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// aloneFixture is a cache alone in its list under 1+0, so every stripe of a
// fill is its own and goes through its queue, on a disk whose writes take a
// second, with a queue of queueBytes.
func aloneFixture(t *testing.T, queueBytes int64) *keepFixture {
	t.Helper()
	return aloneUnder(t, rank.CodeFor(1), queueBytes)
}

// aloneUnder is aloneFixture under code, which a list of one cache takes
// round it: the cache holds every index.
func aloneUnder(t *testing.T, code rank.Code, queueBytes int64) *keepFixture {
	t.Helper()
	f := newKeepFixture(t, 100, sim.DiskConfig{WriteLatency: time.Second}, func(self CacheIdentity) rank.List {
		return listOf(code, self)
	})
	f.cache.filler.queueBytes = queueBytes
	return f
}

// envelopesOf is one envelope of length bytes for each of count windows of
// vm.
func envelopesOf(vm string, count, length int) []envelope {
	envelopes := make([]envelope, count)
	for at := range envelopes {
		key := keyOf(vm, uint64(at)*512)
		envelopes[at] = envelope{key: key, data: payloadOf(key, length)}
	}
	return envelopes
}

// The queue of writes to a host's disk takes a fill while what it holds and
// the fill fit in it, and drops the rest at once: with room for exactly two
// windows, a fill of four keeps the first two and drops the last two. A fill
// of windows the disk holds already writes nothing, and counts each a
// duplicate.
func TestTheQueueTakesWhatFitsAndDropsTheRest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := aloneFixture(t, 2000)
		envelopes := envelopesOf("queued", 4, 1000)
		f.cache.fill(WriteFillPublication, envelopes)
		if fill := f.cache.Stats().Fill; fill.Queued != 2000 || fill.Dropped[DropQueue] != 2 {
			t.Fatalf("a fill of four windows into a queue of two came to %+v, want two queued and two dropped", fill)
		}
		if err := f.cache.SettleFills(t.Context()); err != nil {
			t.Fatal(err)
		}
		for at, e := range envelopes {
			want := 0
			if at < 2 {
				want = 1
			}
			if held := len(f.held(e.key, wholeCode)); held != want {
				t.Fatalf("window %d holds %d stripes, want %d", at, held, want)
			}
		}
		f.cache.fill(WriteFillPublication, envelopes[:2])
		if err := f.cache.SettleFills(t.Context()); err != nil {
			t.Fatal(err)
		}
		if fill := f.cache.Stats().Fill; fill.Kept != 2 || fill.Duplicates != 2 || fill.FromPublications != 4 ||
			fill.Queued != 0 {
			t.Fatalf("filling two windows the disk holds again came to %+v, want two duplicates and nothing more kept",
				fill)
		}
	})
}

// A fill of a window of 4 KiB pages carries an envelope for each page it
// read, and the cache keeps each page's stripes under that page's own key.
func TestAFillKeepsEachPageOfAWindowUnderItsOwnKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := aloneUnder(t, rank.Code{K: 1, M: 1}, 1<<20)
		var envelopes []envelope
		for page := range uint64(3) {
			key := keyOf("pages", page)
			envelopes = append(envelopes, envelope{key: key, data: payloadOf(key, 600)})
		}
		f.cache.fill(WriteFillRead, envelopes)
		if err := f.cache.SettleFills(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, e := range envelopes {
			if held := f.held(e.key, rank.Code{K: 1, M: 1}); len(held) != 2 {
				t.Fatalf("page %d of the window holds stripes %v, want both", e.key.Page, held)
			}
			data, outcome := f.cache.disk.read(t.Context(), e.key, selfChecked)
			if outcome != diskHit || string(data) != string(e.data) {
				t.Fatalf("page %d read back as %v", e.key.Page, outcome)
			}
		}
		if fill := f.cache.Stats().Fill; fill.FromReads != 1 || fill.RightsGranted != 1 || fill.Kept != 6 {
			t.Fatalf("a read's fill of three pages of one window came to %+v, want one window and its six stripes", fill)
		}
	})
}

// A cache that closes drops what its fills had not done: the write in
// flight, a disk write that takes a second, and the two windows queued behind
// it.
// The queue empties, and nothing waits for it.
func TestClosingTheCacheDropsWhatItsFillsHadNotDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Under 1+1 alone, the cache holds both stripes of every window.
		f := aloneUnder(t, rank.Code{K: 1, M: 1}, 1<<20)
		// A first window opens the region the next ones are written into.
		f.cache.fill(WriteFillPublication, envelopesOf("opening", 1, 1000))
		if err := f.cache.SettleFills(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.cache.fill(WriteFillPublication, envelopesOf("closing", 3, 1000))
		synctest.Wait()
		f.cache.Close()
		if fill := f.cache.Stats().Fill; fill.Queued != 0 || fill.Kept != 2 || fill.Dropped[DropFailed] != 6 ||
			dropped(fill) != 6 {
			t.Fatalf("a cache closed under three fills came to %+v, want the queue empty and their six stripes dropped",
				fill)
		}
		if err := f.cache.SettleFills(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// A host's rate of keeps holds a second of its bytes at most, refills as its
// clock moves, and takes a keep only while it holds all of its bytes.
func TestTheRateOfKeepsRefillsWithItsClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := sim.New(sim.Config{Seed: 1}).NewClock("host")
		bucket := newTokenBucket(clock, 1000)
		if !bucket.take(1000) || bucket.take(1) {
			t.Fatal("a full bucket of 1000 bytes did not take exactly 1000")
		}
		clock.Advance(500 * time.Millisecond)
		if !bucket.take(500) || bucket.take(1) {
			t.Fatal("half a second did not refill exactly 500 bytes")
		}
		clock.Advance(time.Hour)
		if !bucket.take(1000) || bucket.take(1) {
			t.Fatal("an hour refilled more or less than one second of the rate")
		}
	})
}

// A cache refuses a keep whose items do not fill its payload exactly: one
// that runs past it, and bytes past the last.
func TestACacheRefusesAKeepWhoseItemsDoNotFillItsPayload(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 1, M: 1}
		f := newKeepFixture(t, 100, sim.DiskConfig{}, func(self CacheIdentity) rank.List {
			return listOf(code, self, otherCache)
		})
		key := keyOf("vm", 0)
		past := keepOf(t, key, code, []int{0})
		past.Payload = slices.Clip(past.Payload)
		past.Items[0].Size = len(past.Payload) + 1
		trailing := keepOf(t, key, code, []int{0})
		trailing.Payload = append(trailing.Payload, 0)
		for name, keep := range map[string]peer.Keep{"an item past the payload": past, "bytes past the items": trailing} {
			if err := f.cache.Keep(t.Context(), f.m, f.cache.Identity(), keep); !errors.Is(err, peer.ErrDropped) {
				t.Fatalf("a keep with %s = %v, want it dropped", name, err)
			}
		}
		if fill := f.cache.Stats().Fill; fill.Refused != 2 || fill.Kept != 0 {
			t.Fatalf("the malformed keeps came to %+v, want both refused", fill)
		}
	})
}

// dropped is every stripe fills dropped, for any reason.
func dropped(stats FillStats) uint64 {
	total := uint64(0)
	for _, stripes := range stats.Dropped {
		total += stripes
	}
	return total
}
