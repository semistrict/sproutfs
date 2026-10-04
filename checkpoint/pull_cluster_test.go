package checkpoint_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
)

// A pull inside the cluster share is a prefetch into the cluster's cache
// (plans/disk-cache-2026-10-02.md, "Pull"): it asks a window's ranks what
// they hold, reads from the store only what the cluster lacks, and fills the
// cluster with it, as background work behind every fault.

// startPull has h open ref and pull it, and returns the pull and the store's
// count of requests once the open is made: the open reads the index object,
// and the rest is the pull's.
func (c *fillCluster) startPull(t *testing.T, h *fillHost, ref control.Ref) (*checkpoint.Pull, int64) {
	t.Helper()
	index, err := h.store.Open(c.ctx(t), ref)
	if err != nil {
		t.Fatal(err)
	}
	before := h.objects.gets.Load()
	pull, err := h.store.Pull(c.ctx(t), index)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pull.Close)
	return pull, before
}

// pullOn has h pull ref, and waits for the pull and for every host's fills.
// It returns how far the pull came and the requests of the store it made.
func (c *fillCluster) pullOn(t *testing.T, h *fillHost, ref control.Ref) (checkpoint.PullStats, int64) {
	t.Helper()
	pull, before := c.startPull(t, h, ref)
	if err := pull.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	c.settle(t)
	return pull.Stats(), h.objects.gets.Load() - before
}

// forget has every host drop the stripes of indices of page 0 of window it
// holds, as eviction or a lost host leaves the cluster short of them.
func (c *fillCluster) forget(t *testing.T, window rank.Window, indices ...int) {
	t.Helper()
	code := c.list.Load().Code()
	for at, held := range c.placed(window) {
		h := c.hosts[at]
		for _, index := range held {
			if !slices.Contains(indices, index) {
				continue
			}
			if err := h.cache.Drop(c.ctx(t), h.cache.Identity(),
				peer.Drop{Window: window, Page: 0, Index: index, Code: code}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A pull of a checkpoint the cluster holds reads nothing from the store: the
// open of its index object, before the pull, is the only request. It reads
// the segment through the cluster, finds every page held by asking the
// window's ranks, and keeps nothing more on its own disk than it held before.
// So under 1+1 on two hosts, 2+2 round three and 4+2 on six, on every host.
func TestAPullOfACheckpointTheClusterHoldsReadsNothingFromTheStore(t *testing.T) {
	for _, cluster := range clusterCodes {
		t.Run(cluster.code.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newFillCluster(t, fillConfig{hosts: cluster.hosts, code: cluster.code, share: 100})
				ref, _ := c.filled(t, 0, "vm", []uint64{0, 1, 2, 3})
				for _, h := range c.hosts {
					entries := h.cache.Stats().Disk.Entries
					stats, gets := c.pullOn(t, h, ref)
					if gets != 0 || stats.Err != nil || !stats.Done || stats.Fetched != 0 || stats.Held != stats.Bytes ||
						stats.Pulled != stats.Bytes || stats.Bytes == 0 {
						t.Fatalf("%s's pull came to %+v with %d requests of the store; want every byte found held "+
							"and no request", h.name, stats, gets)
					}
					if after := h.cache.Stats().Disk.Entries; after != entries {
						t.Fatalf("%s's disk went from %d stripes to %d; a pull of a checkpoint the cluster holds keeps "+
							"nothing more", h.name, entries, after)
					}
				}
			})
		})
	}
}

// A pull of a checkpoint the cluster holds part of reads from the store only
// the pages it lacks, and fills the cluster with them. Of eight pages of six
// hosts under 4+2, the cluster lost every stripe of pages 2 and 5, three of
// page 6, which leaves fewer than four, and two of page 7, which leaves four.
// The pull reads two ranges of the store, page 2 and pages 5 and 6, and after
// it every rank holds its stripes of the three, so a read of any page on
// another host makes no request of the store but the open of the index.
func TestAPullOfACheckpointTheClusterPartlyHoldsReadsOnlyWhatItLacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100})
		pages := []uint64{0, 1, 2, 3, 4, 5, 6, 7}
		ref, m := c.filled(t, 0, "vm", pages)
		c.forget(t, pageWindow(ref, 2), 0, 1, 2, 3, 4, 5)
		c.forget(t, pageWindow(ref, 5), 0, 1, 2, 3, 4, 5)
		c.forget(t, pageWindow(ref, 6), 0, 2, 4)
		c.forget(t, pageWindow(ref, 7), 1, 5)
		puller := c.hosts[4]
		gets := puller.partGets()
		stats, all := c.pullOn(t, puller, ref)
		if gets.Load() != 2 || all != 2 || stats.Err != nil || stats.Fetched == 0 || stats.Held == 0 ||
			stats.Pulled != stats.Bytes {
			t.Fatalf("the pull came to %+v with %d reads of parts and %d requests in all; want the two ranges "+
				"the cluster lacks and nothing else", stats, gets.Load(), all)
		}
		for _, page := range []uint64{2, 5, 6, 7} {
			window := pageWindow(ref, page)
			want := c.ranked(window)
			if page == 7 {
				// A page the cluster still held is left as it was.
				for at := range want {
					want[at] = slices.DeleteFunc(want[at], func(index int) bool { return index == 1 || index == 5 })
				}
			}
			if got := c.placed(window); !slices.EqualFunc(got, want, slices.Equal) {
				t.Fatalf("page %d's stripes are on %v after the pull, want %v", page, got, want)
			}
		}
		if requests := c.readEvery(t, c.hosts[1], ref, m, pages); requests != int64(len(pages)) {
			t.Fatalf("a read of every page after the pull made %d requests of the store, want only an open of "+
				"the index for each", requests)
		}
	})
}

// A pull's reads are background work. Its links to every other host are a
// second slower than the reader's bound, but it reads the store for nothing:
// its read of the segment through the cluster asks no second request and
// reads the store as a hedge never, and leaves the delays where they were.
// Every request it sends goes over the bulk class: a connection of the bulk
// class to each host it asked, the segment's first ranks and every rank of a
// page's window, and none of the fault, stripe or write classes.
func TestAPullsReadsOfTheClusterAreBulkWorkThatNeverHedges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100, runtime: latencyRuntime,
			cache: func(_ int, cache *checkpoint.CacheConfig) {
				cache.ClusterHedgeFloor = 50 * time.Millisecond
				cache.ClusterBound = 10 * time.Millisecond
				cache.ClusterStripeTimeout = time.Hour
			}})
		pages := []uint64{0, 1, 2, 3, 4, 5}
		ref, _ := c.filled(t, 1, "vm", pages)
		puller := c.hosts[0]
		for _, h := range c.hosts[1:] {
			c.runtime.Network().SetLink(platform.Address(puller.name), h.address, simLink(time.Second))
			c.runtime.Network().SetLink(h.address, platform.Address(puller.name), simLink(time.Second))
		}
		stats, gets := c.pullOn(t, puller, ref)
		if gets != 0 || stats.Err != nil || stats.Held != stats.Bytes {
			t.Fatalf("the pull came to %+v with %d requests of the store; want everything found in the cluster",
				stats, gets)
		}
		list := *c.list.Load()
		self := puller.cache.Identity()
		asked := map[rank.Identity]bool{}
		segment := segmentWindow(ref)
		ownHeld := len(puller.cache.HeldIndices(segment, 0, list.Code())) > 0
		first, _ := picks(list, self, segment, ownHeld)
		for _, cache := range first {
			asked[cache.Identity] = true
		}
		// checked is the ranks of the pages' windows, each asked once for
		// every window it ranks for.
		checked := map[rank.Identity]bool{}
		for _, page := range pages {
			for _, cache := range list.Ranks(pageWindow(ref, page)) {
				if cache.Identity != self {
					checked[cache.Identity], asked[cache.Identity] = true, true
				}
			}
		}
		read := puller.cache.Stats().Read
		moved := func(class checkpoint.ReadClass) bool {
			return class.Reads != 0 || class.Delay != 50*time.Millisecond || class.Bound != 200*time.Millisecond
		}
		if read.StoreHedges != 0 || read.StoreHedgesRefused != 0 || read.SecondRequests != 0 || read.Refused != 0 ||
			read.Prefetches != 1 || slices.ContainsFunc(read.Classes, moved) {
			t.Fatalf("the pull's reads came to %+v; want one read of the segment as a prefetch, no hedge, no "+
				"second request and every delay at its floor", read)
		}
		for _, h := range c.hosts[1:] {
			want := peer.Connections{}
			if asked[h.cache.Identity()] {
				want.BulkRead = 1
			}
			if got := puller.table.Peer(h.address).Status().Connections; got != want {
				t.Fatalf("the puller holds %+v connections to %s, want %+v", got, h.name, want)
			}
		}
		if int(read.Presences) != len(checked) {
			t.Fatalf("the pull sent %d presence checks, want one to each of the %d ranks of its pages' windows",
				read.Presences, len(checked))
		}
	})
}

// A fault during a pull is never slowed by it. Each host takes one load at a
// time. While the pull's read of the pages the cluster lacks is held at the
// store, a fault on a page the cluster holds takes exactly as long as it did
// before the pull, and so does a fault on a page of the pulled checkpoint
// that the pull has not reached, which the fault reads from the store itself.
func TestAFaultDuringAPullIsNeverSlowedByIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100, runtime: latencyRuntime,
			cache: func(_ int, cache *checkpoint.CacheConfig) {
				cache.MaxConcurrentLoads = 1
				cache.ClusterStripeTimeout = time.Hour
			}})
		// Links of one latency and no jitter, so two reads down one path take
		// one time whenever they are made.
		for _, from := range c.hosts {
			for _, to := range c.hosts {
				c.runtime.Network().SetLink(platform.Address(from.name), to.address, simLink(time.Millisecond))
				c.runtime.Network().SetLink(to.address, platform.Address(from.name), simLink(time.Millisecond))
			}
		}
		held, heldModel := c.filled(t, 0, "held", []uint64{0, 1})
		pulledIndex, pulledModel := publish(t, c.publisher, "pulled", []uint64{0, 1, 2, 3})
		pulled := pulledIndex.Ref()
		faulter := c.hosts[5]
		timed := func(ref control.Ref, m *model, page uint64) time.Duration {
			index, err := faulter.store.Open(c.ctx(t), ref)
			if err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			readCachedPage(t, faulter.store, index, m, page)
			return time.Since(began)
		}
		// The first reads dial the connections the rest use.
		timed(held, heldModel, 0)
		fromCluster := timed(held, heldModel, 0)
		// A fault on a page the cluster lacks reads the store, and its fill
		// puts the page, and the segment, in the cluster. The next such
		// fault finds the segment in the cluster and its page only in the
		// store, as a fault during the pull does.
		timed(pulled, pulledModel, 3)
		c.settle(t)
		fromStore := timed(pulled, pulledModel, 2)
		c.settle(t)
		objects := faulter.objects
		objects.entered, objects.release = make(chan struct{}), make(chan struct{})
		objects.prefetches.Store(true)
		objects.block.Store(true)
		pull, _ := c.startPull(t, faulter, pulled)
		<-objects.entered
		faults := make(chan [2]time.Duration, 1)
		go func() {
			faults <- [2]time.Duration{timed(held, heldModel, 0), timed(pulled, pulledModel, 1)}
		}()
		time.Sleep(time.Second)
		select {
		case got := <-faults:
			if got != [2]time.Duration{fromCluster, fromStore} {
				t.Fatalf("faults during the pull took %v, want %v and %v as before it", got, fromCluster, fromStore)
			}
		default:
			t.Fatal("a fault waited for the pull's read of the store")
		}
		objects.block.Store(false)
		close(objects.release)
		if err := pull.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// Pressure cancels a pull's reads. With the pull's read of the store held,
// the host's memory budget refuses a reservation no cache can make room for,
// or the disk limiter shrinks the cache: the read in flight is cancelled, the
// pull ends with ErrPressure, and it reads nothing more.
func TestPressureCancelsAPullsReads(t *testing.T) {
	for _, pressure := range []struct {
		name  string
		press func(t *testing.T, c *fillCluster, h *fillHost)
	}{
		{"memory", func(t *testing.T, c *fillCluster, h *fillHost) {
			// The first takes the whole budget, the caches giving back what
			// they hold, and the second finds nothing left to give back.
			lease, err := h.budget.TryAcquire(c.ctx(t), 4<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Close()
			if _, err := h.budget.TryAcquire(c.ctx(t), 1); err == nil {
				t.Fatal("a full budget took more")
			}
		}},
		{"disk", func(t *testing.T, c *fillCluster, h *fillHost) {
			if err := h.cache.FitDisk(c.ctx(t)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(pressure.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100,
					memory: 4 << 20})
				index, _ := publish(t, c.publisher, "pulled", []uint64{0, 1, 2, 3})
				puller := c.hosts[2]
				objects := puller.objects
				objects.entered, objects.release = make(chan struct{}), make(chan struct{})
				objects.prefetches.Store(true)
				objects.block.Store(true)
				pull, before := c.startPull(t, puller, index.Ref())
				<-objects.entered
				pressure.press(t, c, puller)
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				err := pull.Wait(ctx)
				stats := pull.Stats()
				close(objects.release)
				if !errors.Is(err, checkpoint.ErrPressure) || !stats.Done || !errors.Is(stats.Err, checkpoint.ErrPressure) ||
					objects.canceled.Load() != 1 || stats.Fetched != 0 {
					t.Fatalf("under %s pressure the pull ended with %v at %+v, %d reads cancelled; want it stopped "+
						"short with its read cancelled", pressure.name, err, stats, objects.canceled.Load())
				}
				time.Sleep(time.Minute)
				if gets := objects.gets.Load() - before; gets != 1 {
					t.Fatalf("the pull made %d requests of the store, want only the one cancelled", gets)
				}
			})
		})
	}
}
