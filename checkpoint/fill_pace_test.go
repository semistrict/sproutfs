package checkpoint_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// pacedCluster is two hosts under 1+1 whose second host's disk takes write
// over each write: a slow holder. Each host's disk is one region, which the
// first write to it opens. The first host publishes, and its fills of the
// cluster wait on the clock its table of peers runs on, which is the
// bubble's, as its rate does. Its queue holds queueBytes.
func pacedCluster(queueBytes int64, shake uint64, write time.Duration,
	cache func(config *checkpoint.CacheConfig)) fillConfig {
	runtime := latencyRuntime
	runtime.Shake = shake
	return fillConfig{hosts: 2, code: rank.Code{K: 1, M: 1}, share: 100, runtime: runtime,
		diskOf: func(host int) sim.DiskConfig {
			if host == 1 {
				return sim.DiskConfig{WriteLatency: write}
			}
			return sim.DiskConfig{}
		},
		// No connection is pinged while a keep is answered: a ping on a link
		// takes the sequence number, and so the drawn latency, a later frame
		// would have taken, and a run behind a slow holder would differ from
		// one behind a quick one by more than the holder's writes.
		table: func(config *peer.TableConfig) {
			config.PingInterval, config.DeadAfter = time.Minute, 2*time.Minute
		},
		cache: func(host int, config *checkpoint.CacheConfig) {
			config.DiskRegionBytes = checkpoint.DefaultDiskRegionBytes
			if host == 0 {
				config.Clock, config.FillQueueBytes = nil, queueBytes
				if cache != nil {
					cache(config)
				}
			}
		}}
}

// noisyWindow is what a window of one page of noise costs the queue: its
// envelope, which holds the page as it is, being noise, behind the envelope's
// header. noisySegment is what the window of the segment naming eight such
// pages costs: its envelope, which holds the segment's 144 bytes as they are.
const (
	noisyWindow  = checkpoint.PageSize2MiB + blob.HeaderSize
	noisySegment = 144 + blob.HeaderSize
)

// pacedRun is what one publication of noisy pages from the first host of a
// cluster did: how long its commit took, how long until its fills had
// settled, what the publisher's fills came to, the most parts the
// publication held that its fills had not finished with, where the stripes
// of its pages' windows and then its segment's were and where the list ranks
// them, and the simulation's fingerprint.
type pacedRun struct {
	ref            control.Ref
	took, settled  time.Duration
	fills          checkpoint.FillStats
	held           int
	placed, ranked [][][]int
	fingerprint    uint64
}

// pacedPublication is one publication of pages of noise from the first host
// of a cluster of config, a part a page through uploads slots. before is
// done to the cluster before the publication begins, and beside on a
// goroutine of its own while it runs, which the run waits for before its
// fills settle.
type pacedPublication struct {
	config         fillConfig
	pages, uploads int
	before         func(t *testing.T, c *fillCluster)
	beside         func(t *testing.T, c *fillCluster)
}

// countedPuts is an object store that counts the parts whose PUT has begun,
// and at each records how many of them the publisher's fills had not
// finished with: the parts the publication holds.
type countedPuts struct {
	platform.ObjectStore
	publisher *checkpoint.Cache
	begun     atomic.Int64
	mu        sync.Mutex
	most      int
}

func (s *countedPuts) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	if strings.Contains(request.Key.String(), "/part/") {
		begun := s.begun.Add(1)
		// Under 1+1 each window is one keep to the other host, answered once
		// the window's last stripe is placed.
		sent := int64(s.publisher.Stats().Fill.Sent)
		s.mu.Lock()
		s.most = max(s.most, int(begun-sent))
		s.mu.Unlock()
	}
	return s.ObjectStore.Put(ctx, request)
}

// run publishes, and reports what the publication did.
func (p pacedPublication) run(t *testing.T) pacedRun {
	t.Helper()
	var run pacedRun
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, p.config)
		publisher := c.hosts[0]
		counted := &countedPuts{ObjectStore: publisher.objects, publisher: publisher.cache}
		store := mustStore(t, checkpoint.Config{ObjectStore: counted, Cache: publisher.cache, PartBytes: 1,
			Concurrency: p.uploads})
		numbers := make([]uint64, p.pages)
		for at := range numbers {
			numbers[at] = uint64(at)
		}
		_, m, publication := beginPublicationOf(t, store, "vm", numbers, noisySector)
		if p.before != nil {
			p.before(t, c)
		}
		var beside sync.WaitGroup
		if p.beside != nil {
			beside.Go(func() { p.beside(t, c) })
		}
		start := time.Now()
		index, err := publication.Commit(c.ctx(t), m)
		if err != nil {
			t.Fatal(err)
		}
		run.took = time.Since(start)
		beside.Wait()
		c.settle(t)
		run.settled = time.Since(start)
		run.fills, run.held = publisher.cache.Stats().Fill, counted.most
		run.fingerprint = c.runtime.Fingerprint()
		ref := index.Ref()
		run.ref = ref
		for _, window := range append(windowsOf(ref, numbers), segmentWindow(ref)) {
			run.placed, run.ranked = append(run.placed, c.placed(window)), append(run.ranked, c.ranked(window))
		}
	})
	return run
}

// windowsOf is the windows of pages of ref's volume.
func windowsOf(ref control.Ref, pages []uint64) []rank.Window {
	windows := make([]rank.Window, 0, len(pages))
	for _, page := range pages {
		windows = append(windows, pageWindow(ref, page))
	}
	return windows
}

// samePlaces reports whether two placements of windows, by window and host,
// are the same.
func samePlaces(a, b [][][]int) bool {
	return slices.EqualFunc(a, b, func(x, y [][]int) bool { return slices.EqualFunc(x, y, slices.Equal) })
}

// The holders' writes the tests run behind: a quick disk's, and a slow one's,
// a second longer.
const (
	quickWrite = time.Millisecond
	slowWrite  = time.Second + quickWrite
)

// A publication goes at the pace its slowest holder keeps, and every stripe
// of it lands. Eight pages of noise, a part each, go to a holder, through a
// queue with room below its high-water mark for two windows. Every part's PUT
// ends 5 ms in. The first two windows are taken at once, and each after
// waits for a window's keep to be answered, so the eighth part is handed over
// once the sixth keep is, and the commit returns when its index object's PUT
// has landed. With a holder whose disk takes a second longer over each write,
// the commit takes exactly six seconds longer than with a quick one, one for
// each keep it waited for. The segment's window, which fits beside the last
// two, and those two are kept three seconds later still. Dropped at a full
// queue, as fills were before, the publication took no longer than with a
// quick holder, and six of its nine windows reached no host.
func TestAPublicationGoesAtThePaceOfItsSlowestHolder(t *testing.T) {
	quick := pacedPublication{config: pacedCluster(6<<20, 0, quickWrite, nil), pages: 8, uploads: 8}.run(t)
	slow := pacedPublication{config: pacedCluster(6<<20, 0, slowWrite, nil), pages: 8, uploads: 8}.run(t)
	if slow.took-quick.took != 6*time.Second || slow.settled-quick.settled != 9*time.Second {
		t.Fatalf("behind a slow holder the publication took %v and settled after %v; behind a quick one %v and %v. "+
			"Want six and nine writes of a second longer", slow.took, slow.settled, quick.took, quick.settled)
	}
	for name, run := range map[string]pacedRun{"quick": quick, "slow": slow} {
		if !samePlaces(run.placed, run.ranked) {
			t.Fatalf("behind the %s holder the windows' stripes are on %v, want %v", name, run.placed, run.ranked)
		}
		fills := run.fills
		if fills.FromPublications != 9 || fills.Sent != 9 || fills.Kept != 9 || dropped(fills) != 0 ||
			fills.GaveUp != 0 || fills.Waits != 6 {
			t.Fatalf("behind the %s holder the publisher's fills came to %+v; want its eight pages and its segment "+
				"on both hosts, the six parts after the first two having waited for room", name, fills)
		}
		if most := int64(2*noisyWindow + noisySegment); fills.QueuedPeak != most {
			t.Fatalf("behind the %s holder the queue held %d bytes at most, want two windows' and the segment's %d", name,
				fills.QueuedPeak, most)
		}
	}
}

// A publication holds no more parts than its upload slots and its queue have
// room for, however far behind its fills are. Twelve pages of noise, a part
// each, go through two upload slots to a slow holder, through a queue with
// room for two windows. A part keeps its slot until it is handed over, so
// when each part's PUT begins, the publication holds at most four parts its
// fills have not finished with: two in the queue and two under the slots,
// uploading or waiting for room. Handed over as their PUTs ended, as before,
// every part would be held at once.
func TestAPublicationHoldsNoMorePartsThanItsSlotsAndItsQueue(t *testing.T) {
	run := pacedPublication{config: pacedCluster(6<<20, 0, slowWrite, nil), pages: 12, uploads: 2}.run(t)
	if run.held != 4 {
		t.Fatalf("the publication held as many as %d parts its fills had not finished with, want 4", run.held)
	}
	if !samePlaces(run.placed, run.ranked) {
		t.Fatalf("the windows' stripes are on %v, want %v", run.placed, run.ranked)
	}
	if fills := run.fills; fills.QueuedPeak > (6<<20)*3/4 || fills.Sent != 13 || dropped(fills) != 0 {
		t.Fatalf("the publisher's fills came to %+v, want every window kept through a queue held below its "+
			"high-water mark", fills)
	}
}

// A read's fill goes ahead of a publication's. Three hosts under 1+2 put a
// copy of each window on each host, so each window is two keeps, both to
// slow holders, which go side by side. Two and a half seconds into a
// publication of twelve pages, the publisher reads a page from the store,
// which fills two windows: the page's and its segment's. Each holder's lane
// finishes the keep it has on the wire, the publication's third, and carries
// the read's keeps before the publication's fourth, which waits behind it,
// and before the publication's windows in the queue and those waiting for
// room: the page's window is on its ranks half a second, less than one keep
// of a slow holder, later than when nothing else is filled. By then the
// publication has begun six of its thirteen windows: three kept, two decided
// behind the read's keeps, and the sixth waiting for room on the lanes. The
// queue's last quarter is left to reads, so neither of the read's fills is
// dropped. Carried after the publication's keeps queued on the lanes, or
// after every publication's still to do, the read's fills would wait a keep
// more, or many.
func TestAReadsFillGoesAheadOfAPublications(t *testing.T) {
	alone, aloneStarted := readBeside(t, 1, 10*time.Second)
	behind, behindStarted := readBeside(t, 12, 2500*time.Millisecond)
	if alone.delay <= 0 || behind.delay-alone.delay >= slowWrite+5*time.Millisecond {
		t.Fatalf("a read's window was filled %v after the read with nothing else to fill, and %v after it beside "+
			"a publication; want it later by less than one keep of a slow holder", alone.delay, behind.delay)
	}
	if aloneStarted != 2 || behindStarted != 6 {
		t.Fatalf("the publisher had begun %d and %d windows of its publications when the read's window was "+
			"filled, want 2 and 6", aloneStarted, behindStarted)
	}
	if fills := behind.fills; fills.FromReads != 2 || dropped(fills) != 0 || fills.Sent != 30 {
		t.Fatalf("the publisher's fills came to %+v, want the read's two windows and the publication's thirteen "+
			"kept on both holders", fills)
	}
}

// readFill is what became of a read's fill beside a publication: how long
// after the read its window was on its ranks, and what the publisher's fills
// came to once they settled.
type readFill struct {
	delay time.Duration
	fills checkpoint.FillStats
}

// readBeside reads a page the store holds on the first of three hosts under
// 1+2, at into a publication of pages of its own behind two slow holders, and
// reports how long after the read its window was on its ranks and how many
// windows of the publication's the publisher had begun to place by then.
func readBeside(t *testing.T, pages int, at time.Duration) (readFill, uint64) {
	t.Helper()
	var got readFill
	var started uint64
	var ref control.Ref
	var m *model
	config := pacedCluster(16<<20, 0, slowWrite, nil)
	config.hosts, config.code = 3, rank.Code{K: 1, M: 2}
	config.diskOf = func(host int) sim.DiskConfig {
		if host == 0 {
			return sim.DiskConfig{}
		}
		return sim.DiskConfig{WriteLatency: slowWrite}
	}
	run := pacedPublication{config: config, pages: pages, uploads: 8,
		before: func(t *testing.T, c *fillCluster) {
			var index *checkpoint.Index
			index, m = publish(t, c.publisher, "read", []uint64{0})
			ref = index.Ref()
		},
		beside: func(t *testing.T, c *fillCluster) {
			time.Sleep(at)
			reader := c.hosts[0]
			start := time.Now()
			c.read(t, reader, ref, m, 0)
			window := pageWindow(ref, 0)
			for !slices.EqualFunc(c.placed(window), c.ranked(window), slices.Equal) {
				time.Sleep(time.Millisecond)
			}
			got.delay, started = time.Since(start), reader.cache.Stats().Fill.FromPublications
		}}.run(t)
	got.fills = run.fills
	return got, started
}

// A holder that is gone costs a publication the bound at most, and then its
// fills are dropped. The publisher's link to its one peer is cut, so the
// first keep waits out the dial's three seconds. The first two windows take
// the queue's room; the third waits the bound, a second, and is dropped, and
// the publication waits no more: every window after it finds the queue full
// and is dropped at once. The commit takes exactly the bound longer than
// with the cluster cache off. The two windows queued and the segment's keep
// the publisher's own stripes, and their keeps fail: the first at the dial,
// and the rest at once, the peer being marked down. Waiting for room without
// a bound, the publication would wait for the dial too.
func TestADeadHolderCostsAPublicationTheBoundAtMost(t *testing.T) {
	bound := func(config *checkpoint.CacheConfig) { config.FillWaitBound = time.Second }
	offConfig := pacedCluster(6<<20, 0, quickWrite, bound)
	offConfig.share = 0
	off := pacedPublication{config: offConfig, pages: 8, uploads: 8}.run(t)
	dead := pacedPublication{config: pacedCluster(6<<20, 0, quickWrite, bound), pages: 8, uploads: 8,
		before: func(t *testing.T, c *fillCluster) {
			c.runtime.Network().HoldBoth(platform.Address(c.hosts[0].name), c.hosts[1].address,
				time.Now().Add(time.Hour))
		}}.run(t)
	if dead.took-off.took != time.Second {
		t.Fatalf("with its holder gone the publication took %v, and %v with the cluster cache off; want the bound, "+
			"a second, longer", dead.took, off.took)
	}
	fills := dead.fills
	var want [len(fills.Dropped)]uint64
	want[checkpoint.DropQueue], want[checkpoint.DropFailed], want[checkpoint.DropDown] = 12, 1, 2
	if fills.Dropped != want || fills.Kept != 3 || fills.Sent != 0 || fills.GaveUp != 1 || fills.Waits != 1 {
		t.Fatalf("with its holder gone the publisher's fills came to %+v; want six windows dropped at the queue, "+
			"three kept on its own disk alone, and the publication given up once", fills)
	}
}

// A keep of a publication's that finds the publisher's background budget
// full is tried again, not dropped, until the bound. Bulk work holds the
// whole budget for the first three seconds of a publication of two pages, as
// a migration's stream may. The first keep is refused, and tried again after
// 10 ms and twice as long each time after, up to half a second: it goes at
// 3.13 s, the first try after the budget came free, and every window lands.
// With a bound of a second, the keep is dropped for the budget a second after
// its first try, the publication waits no more, and the next two keeps,
// which find the budget full too, are dropped at once.
func TestAPublicationsKeepWaitsForTheBackgroundBudget(t *testing.T) {
	held := func(t *testing.T, c *fillCluster) {
		budget := c.hosts[0].table.Background()
		limit := budget.Status().Limit
		if err := budget.Acquire(c.ctx(t), peer.Resident, limit); err != nil {
			t.Error(err)
			return
		}
		time.AfterFunc(3*time.Second, func() { budget.Release(limit) })
	}
	waited := pacedPublication{config: pacedCluster(6<<20, 0, quickWrite, nil), pages: 2, uploads: 8,
		before: held}.run(t)
	if !samePlaces(waited.placed, waited.ranked) || dropped(waited.fills) != 0 || waited.fills.Sent != 3 ||
		waited.fills.Waits != 1 || waited.fills.GaveUp != 0 {
		t.Fatalf("behind a full budget the windows are on %v, want %v, and the fills came to %+v; want every "+
			"window kept after one wait", waited.placed, waited.ranked, waited.fills)
	}
	// The first try 5 ms in, after the parts' PUTs, and the tries after it at
	// 10, 30, 70, 150, 310, 630 ms, 1.13, 1.63, 2.13, 2.63 and 3.13 s after.
	if first := waited.fills.Waited; first != 3130*time.Millisecond {
		t.Fatalf("the first keep waited %v for the budget, want 3.13 s", first)
	}
	bound := func(config *checkpoint.CacheConfig) { config.FillWaitBound = time.Second }
	gaveUp := pacedPublication{config: pacedCluster(6<<20, 0, quickWrite, bound), pages: 2, uploads: 8,
		before: held}.run(t)
	fills := gaveUp.fills
	if fills.Dropped[checkpoint.DropBudget] != 3 || dropped(fills) != 3 || fills.Sent != 0 || fills.Kept != 3 ||
		fills.GaveUp != 1 || fills.Waited != time.Second {
		t.Fatalf("behind a budget full for longer than the bound the fills came to %+v; want three keeps dropped "+
			"for the budget after one wait of the bound", fills)
	}
}

// A publication whose fills wait for room does the same work at the same
// moments under a shake: the windows wait for room, and are given it, in the
// order the parts were handed over in, whatever order the goroutines run in.
func TestAPacedPublicationDoesTheSameWorkUnderAShake(t *testing.T) {
	want := pacedPublication{config: pacedCluster(6<<20, 0, slowWrite, nil), pages: 8, uploads: 3}.run(t)
	for _, shake := range []uint64{1, 2, 0x9e3779b97f4a7c15} {
		got := pacedPublication{config: pacedCluster(6<<20, shake, slowWrite, nil), pages: 8, uploads: 3}.run(t)
		if got.fingerprint != want.fingerprint || got.took != want.took || got.settled != want.settled {
			t.Fatalf("under shake %#x the publication took %v, settled after %v and digested as %#x; want %v, %v "+
				"and %#x", shake, got.took, got.settled, got.fingerprint, want.took, want.settled, want.fingerprint)
		}
	}
}
