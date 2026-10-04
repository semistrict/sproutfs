package checkpoint_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// sideBySide is holders+1 hosts under 1+holders, so every host keeps a whole
// copy of each window: each window the first host publishes is a write to
// its own disk and a keep to every other host, whose disks take write over
// each write. The first host's queue holds queueBytes, and the run is shaken
// by shake.
func sideBySide(holders int, queueBytes int64, shake uint64, write time.Duration,
	cache func(config *checkpoint.CacheConfig)) fillConfig {
	config := pacedCluster(queueBytes, shake, write, cache)
	config.hosts, config.code = holders+1, rank.Code{K: 1, M: holders}
	config.diskOf = func(host int) sim.DiskConfig {
		if host == 0 {
			return sim.DiskConfig{}
		}
		return sim.DiskConfig{WriteLatency: write}
	}
	return config
}

// A publication's keeps go to its windows' holders side by side, so its pace
// is one holder's, however many holders a window has. Eight pages of noise, a
// part each, go through a queue with room below its high-water mark for two
// windows, to one holder or to three, each a second slower over each write
// than a quick one. Each holder takes one keep at a time, a second each, and
// has the next behind it: the eighth part is handed over once each holder has
// kept the sixth window, and the commit takes exactly six seconds longer than
// behind quick holders, with one holder or three. The segment's window and
// the last two are kept three seconds later still. Sent one keep at a time,
// as a host did before, behind three holders it would take three seconds a
// window: eighteen seconds longer.
func TestAPublicationsKeepsGoToItsHoldersSideBySide(t *testing.T) {
	for _, holders := range []int{1, 3} {
		quick := pacedPublication{config: sideBySide(holders, 6<<20, 0, quickWrite, nil), pages: 8, uploads: 8}.run(t)
		slow := pacedPublication{config: sideBySide(holders, 6<<20, 0, slowWrite, nil), pages: 8, uploads: 8}.run(t)
		if slow.took-quick.took != 6*time.Second || slow.settled-quick.settled != 9*time.Second {
			t.Fatalf("behind %d slow holders the publication took %v and settled after %v; behind quick ones %v "+
				"and %v. Want six and nine writes of a second longer", holders, slow.took, slow.settled, quick.took,
				quick.settled)
		}
		for name, run := range map[string]pacedRun{"quick": quick, "slow": slow} {
			if !samePlaces(run.placed, run.ranked) {
				t.Fatalf("behind %d %s holders the windows' stripes are on %v, want %v", holders, name, run.placed,
					run.ranked)
			}
			if fills := run.fills; fills.FromPublications != 9 || fills.Sent != uint64(9*holders) || fills.Kept != 9 ||
				dropped(fills) != 0 || fills.GaveUp != 0 {
				t.Fatalf("behind %d %s holders the publisher's fills came to %+v; want its eight pages and its "+
					"segment kept on every host", holders, name, fills)
			}
		}
	}
}

// The publications' keeps a host has on their way at once are bounded. With
// one at a time, the bound's least, a publication behind three holders a
// second slower than quick ones each takes three seconds a window: the
// commit, which waits for the sixth window as above, takes exactly eighteen
// seconds longer than behind quick ones.
func TestAPublicationsKeepsInFlightAreBounded(t *testing.T) {
	one := func(config *checkpoint.CacheConfig) { config.FillKeepsInFlight = 1 }
	quick := pacedPublication{config: sideBySide(3, 6<<20, 0, quickWrite, one), pages: 8, uploads: 8}.run(t)
	slow := pacedPublication{config: sideBySide(3, 6<<20, 0, slowWrite, one), pages: 8, uploads: 8}.run(t)
	if slow.took-quick.took != 18*time.Second {
		t.Fatalf("with one keep in flight behind three slow holders the publication took %v, and %v behind quick "+
			"ones; want eighteen writes of a second longer", slow.took, quick.took)
	}
	if fills := slow.fills; !samePlaces(slow.placed, slow.ranked) || fills.Sent != 27 || dropped(fills) != 0 {
		t.Fatalf("the windows' stripes are on %v, want %v, and the publisher's fills came to %+v; want every "+
			"window kept on every host", slow.placed, slow.ranked, fills)
	}
}

// keepOrder is a peer server's cache that notes the window of every keep it
// is asked to write, in the order the server asks.
type keepOrder struct {
	peer.Cache
	mu      sync.Mutex
	windows []rank.Window
}

func (c *keepOrder) Keep(ctx context.Context, m membership.Membership, disk rank.Identity, keep peer.Keep) error {
	c.mu.Lock()
	c.windows = append(c.windows, keep.Window)
	c.mu.Unlock()
	return c.Cache.Keep(ctx, m, disk, keep)
}

// orderedRun is a publication behind holders slow holders and what each
// holder was asked to keep, in order.
type orderedRun struct {
	pacedRun
	kept [][]rank.Window
}

// orderedPublication publishes pages of noise, a part each, from the first of
// holders+1 hosts under 1+holders whose other hosts are slow holders, under
// shake, and notes the keeps each holder was asked to write.
func orderedPublication(t *testing.T, holders, pages int, shake uint64) orderedRun {
	t.Helper()
	config := sideBySide(holders, 16<<20, shake, slowWrite, nil)
	orders := make([]*keepOrder, holders+1)
	config.server = func(host int, config *peer.ServerConfig) {
		orders[host] = &keepOrder{Cache: config.Cache}
		config.Cache = orders[host]
	}
	run := orderedRun{pacedRun: pacedPublication{config: config, pages: pages, uploads: 4}.run(t)}
	for _, order := range orders[1:] {
		run.kept = append(run.kept, order.windows)
	}
	return run
}

// Each holder is asked to keep a publication's windows in the order the
// publication handed them over, whatever order the goroutines run in. Twelve
// pages of noise, a part each, and the segment that locates them go to three
// slow holders, so each holder's lane holds a keep on the wire and the next
// behind it. Every holder is asked for every window once, page by page and
// the segment last, and the run does the same work at the same moments under
// three shakes. Carried newest first, the keep behind would go before the
// one decided before it.
func TestEachHolderKeepsAPublicationsWindowsInTheirOrder(t *testing.T) {
	want := orderedPublication(t, 3, 12, 0)
	var ordered []rank.Window
	for page := range uint64(12) {
		ordered = append(ordered, pageWindow(want.ref, page))
	}
	ordered = append(ordered, segmentWindow(want.ref))
	for holder, kept := range want.kept {
		if !slices.Equal(kept, ordered) {
			t.Fatalf("holder %d was asked to keep %v, want %v", holder+1, kept, ordered)
		}
	}
	for _, shake := range []uint64{1, 2, 0x9e3779b97f4a7c15} {
		got := orderedPublication(t, 3, 12, shake)
		if got.fingerprint != want.fingerprint || got.took != want.took || got.settled != want.settled ||
			!slices.EqualFunc(got.kept, want.kept, slices.Equal) {
			t.Fatalf("under shake %#x the publication took %v, settled after %v and digested as %#x; want %v, %v "+
				"and %#x", shake, got.took, got.settled, got.fingerprint, want.took, want.settled, want.fingerprint)
		}
	}
}

// A publication's windows in the queue hold what the queue counts of them and
// no more, so the queue's bytes bound what the fills hold: besides them, a
// publication holds only the parts under its upload slots. Twelve pages of
// noise, a part each, go to three slow holders through a queue with room
// below its high-water mark for five windows. Every tenth of a second, the
// windows queued behind the one the worker is on, as many as three, hold
// exactly the bytes the queue counts of them, and the queue never holds more
// than the mark. A window holding a view of
// its part, as before, would hold the whole part until its last keep was
// answered: the queue would count the window and the host hold the part.
func TestAPublicationsQueuedWindowsHoldWhatTheQueueCounts(t *testing.T) {
	const queueBytes = 16 << 20
	var counted, held []int64
	config := sideBySide(3, queueBytes, 0, slowWrite, nil)
	run := pacedPublication{config: config, pages: 12, uploads: 4,
		beside: func(t *testing.T, c *fillCluster) {
			for range 100 {
				time.Sleep(100 * time.Millisecond)
				queued, holds := c.hosts[0].cache.QueuedWindows()
				counted, held = append(counted, queued), append(held, holds)
			}
		}}.run(t)
	if !slices.Equal(held, counted) {
		t.Fatalf("the queued windows held %v bytes, and the queue counted %v of them; want what it counted", held,
			counted)
	}
	if most := slices.Max(counted); most <= 2*noisyWindow || most > 3*noisyWindow ||
		run.fills.QueuedPeak > queueBytes*3/4 {
		t.Fatalf("the queue counted at most %d bytes of the windows behind the worker's and held %d at most; "+
			"want three windows', and no more than its high-water mark", most, run.fills.QueuedPeak)
	}
	if fills := run.fills; !samePlaces(run.placed, run.ranked) || fills.Sent != 39 || dropped(fills) != 0 {
		t.Fatalf("the windows' stripes are on %v, want %v, and the publisher's fills came to %+v; want every "+
			"window kept on every host", run.placed, run.ranked, fills)
	}
}

// A holder's lane holds two of a publication's keeps: one on the wire, and
// one behind it. Eight pages of noise, a part each, go to one slow holder
// through a queue with room below its high-water mark for all nine windows.
// Half a second in, the publication has begun three windows: the first's
// keep on the wire, the second's behind it, and the third waiting for room
// on the lane, with the other six queued behind it. Once the first keep is
// answered, a second later, it has begun a fourth.
func TestAHoldersLaneHoldsTwoOfAPublicationsKeeps(t *testing.T) {
	var begun []uint64
	run := pacedPublication{config: sideBySide(1, 32<<20, 0, slowWrite, nil), pages: 8, uploads: 8,
		beside: func(t *testing.T, c *fillCluster) {
			for _, after := range []time.Duration{500 * time.Millisecond, time.Second} {
				time.Sleep(after)
				begun = append(begun, c.hosts[0].cache.Stats().Fill.FromPublications)
			}
		}}.run(t)
	if !slices.Equal(begun, []uint64{3, 4}) {
		t.Fatalf("half a second and a second and a half in, the publication had begun %v windows; want 3 and 4",
			begun)
	}
	if fills := run.fills; !samePlaces(run.placed, run.ranked) || fills.Sent != 9 || dropped(fills) != 0 {
		t.Fatalf("the windows' stripes are on %v, want %v, and the publisher's fills came to %+v; want every "+
			"window kept on both hosts", run.placed, run.ranked, fills)
	}
}
