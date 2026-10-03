package checkpoint_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// The cluster's disk cache as reads see it: a page any host's cache holds is
// read from the hosts' disks, its own among them, before the store
// (plans/disk-cache-2026-10-02.md, "Reading a page").

// filled publishes pages of vm's root from host's store, whose cache fills the
// cluster with every part once its PUT succeeded and with the segments once
// the index object did, and waits for the fills. It returns the checkpoint
// and the model of what it holds.
func (c *fillCluster) filled(t *testing.T, host int, vm string, pages []uint64) (control.Ref, *model) {
	t.Helper()
	index, m := publish(t, c.hosts[host].store, vm, pages)
	c.settle(t)
	return index.Ref(), m
}

// partGets counts the reads of parts host makes of the store from now on.
func (h *fillHost) partGets() *atomic.Int64 {
	var gets atomic.Int64
	getting := func(key string) {
		if strings.Contains(key, "/part/") {
			gets.Add(1)
		}
	}
	h.objects.getting.Store(&getting)
	return &gets
}

// readEvery has host read every page of ref, and checks each against m. It
// returns what the reads cost in requests of the store.
func (c *fillCluster) readEvery(t *testing.T, h *fillHost, ref control.Ref, m *model, pages []uint64) int64 {
	t.Helper()
	before := h.objects.gets.Load()
	for _, page := range pages {
		c.read(t, h, ref, m, page)
	}
	return h.objects.gets.Load() - before
}

// picks is the ranks of window a reader asks first, as the reader picks them:
// k+1 of the first k+m, itself counted when it is ranked and holds a stripe,
// and the rest after.
func picks(list rank.List, reader rank.Identity, window rank.Window, ownHeld bool) (first, rest []rank.Cache) {
	var others []rank.Cache
	ranked := false
	for _, cache := range list.Ranks(window) {
		if cache.Identity == reader {
			ranked = true
			continue
		}
		others = append(others, cache)
	}
	want := list.Code().K + 1
	if ranked && ownHeld {
		want--
	}
	order := rank.Pick(others, reader, window, want)
	want = min(want, len(order))
	return order[:want], order[want:]
}

// hostOf is the host of the cluster whose cache is cache.
func (c *fillCluster) hostOf(cache rank.Cache) *fillHost {
	for _, h := range c.hosts {
		if h.cache.Identity() == cache.Identity {
			return h
		}
	}
	return nil
}

// A page the cluster's caches hold is read with no request of the store but
// the open of its checkpoint's index object: the segment that locates the page
// and the page itself both come from the hosts' disks, whichever host reads
// them, under 1+1 on two hosts, 2+2 round three and 4+2 on six.
func TestAPageInTheClusterIsReadWithNoStoreRead(t *testing.T) {
	for _, cluster := range clusterCodes {
		t.Run(cluster.code.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newFillCluster(t, fillConfig{hosts: cluster.hosts, code: cluster.code, share: 100})
				pages := []uint64{0, 1, 2}
				ref, m := c.filled(t, 0, "vm", pages)
				for _, h := range c.hosts {
					if gets := c.readEvery(t, h, ref, m, pages); gets != int64(len(pages)) {
						t.Fatalf("%s made %d requests of the store for %d pages, want only an open of the index for each",
							h.name, gets, len(pages))
					}
					if stats := h.cache.Stats().Read; stats.Hits != 2*uint64(len(pages)) || stats.Misses != 0 {
						t.Fatalf("%s read %+v, want a page and its segment from the cluster for each read", h.name, stats)
					}
				}
			})
		})
	}
}

// A cached page survives losing a host. Any one host of six under 4+2, or of
// two under 1+1, is lost, drained from the list, or restarted over its cache
// file, and every host still reads every page from the cluster, with no
// request of the store but the open of the checkpoint's index object.
func TestAPageSurvivesLosingDrainingOrRestartingAnyOneHost(t *testing.T) {
	type event struct {
		name string
		do   func(t *testing.T, c *fillCluster, host int)
		// readers is whether the host the event befell reads too.
		reads bool
	}
	events := []event{
		{"lost", func(t *testing.T, c *fillCluster, host int) { c.hosts[host].shut() }, false},
		{"drained", func(t *testing.T, c *fillCluster, host int) {
			c.hold(c.list.Load().Without(c.hosts[host].cache.Identity()))
		}, true},
		{"restarted", func(t *testing.T, c *fillCluster, host int) { c.restart(t, host) }, true},
	}
	for _, cluster := range []struct {
		hosts int
		code  rank.Code
	}{{2, rank.Code{K: 1, M: 1}}, {6, rank.Code{K: 4, M: 2}}} {
		for _, e := range events {
			for befallen := range cluster.hosts {
				t.Run(fmt.Sprintf("%s/%s/host-%d", cluster.code, e.name, befallen), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						c := newFillCluster(t, fillConfig{hosts: cluster.hosts, code: cluster.code, share: 100})
						pages := []uint64{0, 1, 2, 3}
						ref, m := c.filled(t, (befallen+1)%cluster.hosts, "vm", pages)
						e.do(t, c, befallen)
						for at, h := range c.hosts {
							if at == befallen && !e.reads {
								continue
							}
							if gets := c.readEvery(t, h, ref, m, pages); gets != int64(len(pages)) {
								t.Fatalf("%s made %d requests of the store for %d pages with host %d %s, want only the opens",
									h.name, gets, len(pages), befallen, e.name)
							}
						}
					})
				})
			}
		}
	}
}

// A hot page spreads its load. All six hosts of a 4+2 cluster read one page at
// once. Each reader asks k holders besides itself for each window, chosen by a
// hash of the reader and the window, so the readers of one window ask every
// one of its holders between them; each holder sends each reader one stripe,
// a quarter of the envelope, and none sends a whole one.
func TestAHotPageSpreadsItsLoadOverEveryHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		counters := make([]*countingCache, 6)
		// A floor well past a round trip: no read asks a second time.
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100,
			cache: func(_ int, cache *checkpoint.CacheConfig) { cache.ClusterHedgeFloor = time.Second },
			server: func(host int, config *peer.ServerConfig) {
				counters[host] = &countingCache{Cache: config.Cache, reads: make(map[rank.Window]int)}
				config.Cache = counters[host]
			}})
		ref, m := c.filled(t, 0, "vm", []uint64{0})
		var readers sync.WaitGroup
		for _, h := range c.hosts {
			readers.Go(func() { c.read(t, h, ref, m, 0) })
		}
		readers.Wait()
		// A read has its page once k stripes are in; the request it no
		// longer needs is still answered, a moment later.
		time.Sleep(time.Second)
		for _, window := range []rank.Window{pageWindow(ref, 0), segmentWindow(ref)} {
			total := 0
			for at, counter := range counters {
				asked := counter.asked(window)
				if asked == 0 {
					t.Fatalf("no reader asked host %d for %+v: the readers do not spread over its holders", at, window)
				}
				total += asked
			}
			if total != 6*4 {
				t.Fatalf("six readers asked %d times for %+v, want four holders each besides themselves", total, window)
			}
		}
		for at, h := range c.hosts {
			stats := h.server.Stats()
			if stats.Stripes != stats.StripeReads {
				t.Fatalf("host %d sent %d stripes in %d replies, want one a reply", at, stats.Stripes, stats.StripeReads)
			}
			if limit := stats.StripeReads * (checkpoint.PageSize2MiB/4 + 4096); stats.StripeBytes > limit {
				t.Fatalf("host %d sent %d bytes in %d replies, more than a quarter of a page each", at,
					stats.StripeBytes, stats.StripeReads)
			}
		}
	})
}

// countingCache counts the reads of stripes a server answers, by window.
type countingCache struct {
	peer.Cache
	mu    sync.Mutex
	reads map[rank.Window]int
}

func (c *countingCache) ReadStripes(ctx context.Context, read peer.StripeRead) (peer.Stripes, error) {
	if read.MaxBytes > 0 {
		c.mu.Lock()
		c.reads[read.Window]++
		c.mu.Unlock()
	}
	return c.Cache.ReadStripes(ctx, read)
}

func (c *countingCache) asked(window rank.Window) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[window]
}

// stalled holds every byte between a reader and each host of its first picks
// of window that stall names, for an hour, and slows the link to each one slow
// names to a tenth of a second each way.
func (c *fillCluster) degrade(reader *fillHost, stall, slow []*fillHost) {
	for _, h := range stall {
		c.runtime.Network().HoldBoth(platform.Address(reader.name), h.address, time.Now().Add(time.Hour))
	}
	for _, h := range slow {
		c.runtime.Network().SetLink(platform.Address(reader.name), h.address, simLink(100*time.Millisecond))
		c.runtime.Network().SetLink(h.address, platform.Address(reader.name), simLink(100*time.Millisecond))
	}
}

// simLink is a link of latency each way and no jitter.
func simLink(latency time.Duration) sim.LinkConfig { return sim.LinkConfig{Latency: latency} }

// steadyRuntime is a simulation whose every hop takes a millisecond, to the
// nanosecond, so two reads that wait on the same hops take as long.
var steadyRuntime = sim.Config{Seed: 1,
	Network: sim.NetworkConfig{Latency: time.Millisecond, Jitter: time.Nanosecond, ConnectLatency: time.Millisecond},
	ObjectStore: sim.ObjectStoreConfig{GetLatency: 5 * time.Millisecond, HeadLatency: time.Millisecond,
		PutLatency: 5 * time.Millisecond, BytesPerSecond: 1 << 40}}

// within reports two durations a few nanoseconds of jitter apart.
func within(a, b time.Duration) bool { return max(a-b, b-a) <= time.Microsecond }

// A slow or stalled holder does not slow reads beyond the hedge delay. In a
// 4+2 cluster with no jitter, a reader's page read takes exactly as long with
// one of the holders it asks first stalled, since the k+1 it asks hold a
// stripe to spare. With one of them stalled and another slow, a window whose
// first picks hold both takes exactly the hedge delay longer: the reader asks
// the rest once the delay passes, and the spare answers as quickly as any
// holder.
func TestAStalledOrSlowHolderSlowsAReadByTheHedgeDelayAtMost(t *testing.T) {
	const delay = 3 * time.Millisecond
	config := fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100, runtime: steadyRuntime,
		cache: func(_ int, cache *checkpoint.CacheConfig) {
			cache.ClusterHedgeFloor = delay
			cache.ClusterBound = time.Hour
		}}
	timed := func(t *testing.T, c *fillCluster, reader *fillHost, ref control.Ref, m *model) time.Duration {
		index, err := reader.store.Open(c.ctx(t), ref)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		readCachedPage(t, reader.store, index, m, 0)
		return time.Since(start)
	}
	for readerAt := range 6 {
		for _, slowed := range []bool{false, true} {
			t.Run(fmt.Sprintf("reader-%d/slowed-%v", readerAt, slowed), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					c := newFillCluster(t, config)
					ref, m := c.filled(t, (readerAt+1)%6, "vm", []uint64{0})
					reader := c.hosts[readerAt]
					// The first read dials every connection it uses.
					timed(t, c, reader, ref, m)
					healthy := timed(t, c, reader, ref, m)
					// The holders stalled and slowed are two of the page
					// window's first picks; how many of the two windows pick
					// both is what the delay is paid for.
					var first [][]rank.Cache
					for _, window := range []rank.Window{segmentWindow(ref), pageWindow(ref, 0)} {
						picked, _ := picks(*c.list.Load(), reader.cache.Identity(), window, true)
						first = append(first, picked)
					}
					stall := []*fillHost{c.hostOf(first[1][0])}
					var slow []*fillHost
					if slowed {
						slow = []*fillHost{c.hostOf(first[1][1])}
					}
					c.degrade(reader, stall, slow)
					affected := 0
					for _, picked := range first {
						if slowed && slices.ContainsFunc(picked, func(cache rank.Cache) bool { return c.hostOf(cache) == stall[0] }) &&
							slices.ContainsFunc(picked, func(cache rank.Cache) bool { return c.hostOf(cache) == slow[0] }) {
							affected++
						}
					}
					before := reader.cache.Stats().Read.SecondRequests
					if took, want := timed(t, c, reader, ref, m), healthy+time.Duration(affected)*delay; !within(took, want) {
						t.Fatalf("with %d of its first picks stalled or slow, a read took %v, want %v: %v healthy and the delay for each of %d windows",
							1+len(slow), took, want, healthy, affected)
					}
					if second := reader.cache.Stats().Read.SecondRequests - before; second != uint64(affected) {
						t.Fatalf("the reader made %d second requests, want %d", second, affected)
					}
				})
			})
		}
	}
}

// A wrong stripe is never returned, and its holder is told to drop it. A
// holder the reader asks first holds a stripe of the page whose checksum
// holds and whose bytes are wrong. The reader reads the page right, finds the
// stripe among the k+1 it has, and the holder forgets it. With another of its
// first picks stalled, the reader has only k stripes, which rebuild nothing,
// and asks one more holder at once to tell which is wrong.
func TestAWrongStripeIsNeverReturnedAndItsHolderIsTold(t *testing.T) {
	code := rank.Code{K: 4, M: 2}
	for _, stalled := range []bool{false, true} {
		t.Run(fmt.Sprintf("stalled-%v", stalled), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newFillCluster(t, fillConfig{hosts: 6, code: code, share: 100,
					cache: func(_ int, cache *checkpoint.CacheConfig) { cache.ClusterBound = time.Hour }})
				ref, m := c.filled(t, 1, "vm", []uint64{0})
				reader := c.hosts[0]
				window := pageWindow(ref, 0)
				first, _ := picks(*c.list.Load(), reader.cache.Identity(), window, true)
				wrong := c.hostOf(first[0])
				index := wrong.cache.HeldIndices(window, 0, code)[0]
				if err := wrong.cache.SpoilStripe(c.ctx(t), window, 0, code, index); err != nil {
					t.Fatal(err)
				}
				if stalled {
					c.degrade(reader, []*fillHost{c.hostOf(first[1])}, nil)
				}
				c.read(t, reader, ref, m, 0)
				c.settle(t)
				if held := wrong.cache.HeldIndices(window, 0, code); slices.Contains(held, index) {
					t.Fatalf("the holder of the wrong stripe %d still holds %v", index, held)
				}
				if stats := reader.cache.Stats().Read; stats.WrongStripes != 1 || stats.DropsSent != 1 || stats.Hits != 2 {
					t.Fatalf("the reader's reads came to %+v, want one wrong stripe found and dropped and two hits", stats)
				}
			})
		})
	}
}

// The store is read only when fewer than k stripes of a page exist. Under 4+2
// on six hosts, stripes are dropped until n remain anywhere; the reader reads
// the page from the cluster with no read of a part while n is at least four,
// and reads its part from the store once n is below.
func TestTheStoreIsReadOnlyWhenFewerThanKStripesExist(t *testing.T) {
	code := rank.Code{K: 4, M: 2}
	for remaining := code.Width(); remaining >= 0; remaining-- {
		t.Run(fmt.Sprintf("remaining-%d", remaining), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newFillCluster(t, fillConfig{hosts: 6, code: code, share: 100})
				ref, m := c.filled(t, 1, "vm", []uint64{0})
				window := pageWindow(ref, 0)
				dropped := 0
				for _, h := range c.hosts {
					for _, index := range h.cache.HeldIndices(window, 0, code) {
						if dropped == code.Width()-remaining {
							break
						}
						if err := h.cache.Drop(c.ctx(t), peer.Drop{Window: window, Page: 0, Index: index, Code: code}); err != nil {
							t.Fatal(err)
						}
						dropped++
					}
				}
				reader := c.hosts[0]
				gets := reader.partGets()
				c.read(t, reader, ref, m, 0)
				want := int64(0)
				if remaining < code.K {
					want = 1
				}
				if got := gets.Load(); got != want {
					t.Fatalf("with %d stripes left the reader read %d parts from the store, want %d", remaining, got, want)
				}
			})
		})
	}
}

// Second requests stay within their budget. A reader of a 4+2 cluster has two
// of the holders it asks first for many windows slow. Each such read waits
// the delay and asks the rest, which is a second request, while its budget
// holds one: it starts with five and earns a twentieth of one with each read
// that had its stripes within the delay. Past it, a read waits for the slow
// holders instead.
func TestSecondRequestsStayWithinTheirBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100, runtime: latencyRuntime,
			cache: func(_ int, cache *checkpoint.CacheConfig) {
				cache.ClusterHedgeFloor = 3 * time.Millisecond
				cache.ClusterBound = time.Hour
			}})
		var pages []uint64
		for page := range uint64(40) {
			pages = append(pages, page)
		}
		ref, m := c.filled(t, 1, "vm", pages)
		reader := c.hosts[0]
		list := *c.list.Load()
		// The two slow are the pair the most page windows pick first.
		pairs := make(map[[2]rank.Identity]int)
		for _, page := range pages {
			first, _ := picks(list, reader.cache.Identity(), pageWindow(ref, page), true)
			for a := range first {
				for b := a + 1; b < len(first); b++ {
					pairs[[2]rank.Identity{first[a].Identity, first[b].Identity}]++
				}
			}
		}
		var slowest [2]rank.Identity
		for pair, count := range pairs {
			if count > pairs[slowest] || count == pairs[slowest] && slices.Compare(pair[0][:], slowest[0][:]) < 0 {
				slowest = pair
			}
		}
		var slow []*fillHost
		for _, identity := range slowest {
			slow = append(slow, c.hostOf(rank.Cache{Identity: identity}))
		}
		c.degrade(reader, nil, slow)
		c.readEvery(t, reader, ref, m, pages)
		stats := reader.cache.Stats().Read
		fast := 2*uint64(len(pages)) - stats.SecondRequests - stats.Refused
		if limit := 5 + fast/20; stats.SecondRequests > limit || stats.Refused == 0 {
			t.Fatalf("the reader made %d second requests and was refused %d after %d fast reads, want at most %d and some refused",
				stats.SecondRequests, stats.Refused, fast, limit)
		}
	})
}

// The reads of the store a read past its bound starts stay within their token
// bucket. Every peer of a 4+2 reader is slow, so every read of a page waits
// past its bound. It reads the store as well while the bucket holds a read:
// five at first, and a twentieth of one for each read of a window that asked
// the cluster. Past it, the read waits for the stripes.
func TestStoreReadsPastTheBoundStayWithinTheirBucket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100, runtime: latencyRuntime,
			cache: func(_ int, cache *checkpoint.CacheConfig) {
				// The rest, which a read asks after the delay, are as slow,
				// and the bound is four delays. Nothing times out.
				cache.ClusterHedgeFloor = 50 * time.Millisecond
				cache.ClusterBound = 10 * time.Millisecond
				cache.ClusterStripeTimeout = time.Hour
			}})
		var pages []uint64
		for page := range uint64(40) {
			pages = append(pages, page)
		}
		ref, m := c.filled(t, 1, "vm", pages)
		reader := c.hosts[0]
		for _, h := range c.hosts[1:] {
			c.runtime.Network().SetLink(platform.Address(reader.name), h.address, simLink(time.Second))
			c.runtime.Network().SetLink(h.address, platform.Address(reader.name), simLink(time.Second))
		}
		gets := reader.partGets()
		c.readEvery(t, reader, ref, m, pages)
		stats := reader.cache.Stats().Read
		earned := uint64(2*len(pages)) / 20
		if stats.StoreHedges > 5+earned || stats.StoreHedgesRefused == 0 || stats.StoreHedgesWon != stats.StoreHedges ||
			gets.Load() != int64(stats.StoreHedges) {
			t.Fatalf("past the bound the reader read the store %d times (%+v), want at most %d and the rest refused",
				gets.Load(), stats, 5+earned)
		}
	})
}

// Three timeouts in a row mark a host down, and only a probe clears the mark.
// A holder stalls for a reader, whose requests to it time out; after the third
// the reader marks it down and asks it nothing more, and reads without it. The
// holder answers again a second later, but the mark stays until the probe
// about ten seconds after it, which clears it.
func TestThreeTimeoutsMarkAHostDownAndOnlyAProbeClearsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100,
			cache: func(_ int, cache *checkpoint.CacheConfig) {
				cache.ClusterStripeTimeout = 100 * time.Millisecond
				cache.ClusterBound = time.Hour
			}})
		var pages []uint64
		for page := range uint64(24) {
			pages = append(pages, page)
		}
		ref, m := c.filled(t, 1, "vm", pages)
		reader := c.hosts[0]
		first, _ := picks(*c.list.Load(), reader.cache.Identity(), pageWindow(ref, 0), true)
		stalled := c.hostOf(first[0])
		until := time.Now().Add(time.Hour)
		c.runtime.Network().HoldBoth(platform.Address(reader.name), stalled.address, until)
		marked := time.Time{}
		for _, page := range pages {
			c.read(t, reader, ref, m, page)
			// The timeouts land a tenth of a second after their requests.
			time.Sleep(200 * time.Millisecond)
			stats := reader.cache.Stats().Read
			if (stats.MarkedDown == 1) != (stats.Timeouts >= 3) {
				t.Fatalf("after %d timeouts the reader had marked %d hosts down, want one from the third on",
					stats.Timeouts, stats.MarkedDown)
			}
			if stats.MarkedDown == 1 && marked.IsZero() {
				marked = time.Now()
			}
		}
		if marked.IsZero() {
			t.Fatal("a holder whose requests all timed out was never marked down")
		}
		// Still stalled, it is asked nothing: a request to it would time out.
		timeouts := reader.cache.Stats().Read.Timeouts
		for _, page := range pages[:4] {
			c.read(t, reader, ref, m, page)
		}
		time.Sleep(200 * time.Millisecond)
		if after := reader.cache.Stats().Read; after.Timeouts != timeouts || after.Down != 1 {
			t.Fatalf("after the mark the reads came to %+v, want no more timeouts and one host down", after)
		}
		// Nor is it sent fills: the reader's publication drops at once the
		// stripes the list puts on it.
		published, _ := publish(t, reader.store, "vm-2", []uint64{0})
		c.settle(t)
		ranked := 0
		for _, window := range []rank.Window{pageWindow(published.Ref(), 0), segmentWindow(published.Ref())} {
			for _, holder := range c.list.Load().Holders(window) {
				if holder.Identity == stalled.cache.Identity() {
					ranked++
				}
			}
		}
		if down := reader.cache.Stats().Fill.Dropped[checkpoint.DropDown]; ranked == 0 || down != uint64(ranked) {
			t.Fatalf("a publication dropped %d stripes for a host marked down, want the %d the list puts on it",
				down, ranked)
		}
		// Released now, it answers, but the mark stays until the probe.
		c.runtime.Network().HoldBoth(platform.Address(reader.name), stalled.address, time.Now())
		time.Sleep(time.Until(marked.Add(8 * time.Second)))
		if stats := reader.cache.Stats().Read; stats.Down != 1 || stats.Cleared != 0 {
			t.Fatalf("eight seconds after the mark the reads came to %+v, want the host still down", stats)
		}
		time.Sleep(time.Until(marked.Add(12 * time.Second)))
		if stats := reader.cache.Stats().Read; stats.Down != 0 || stats.Cleared != 1 {
			t.Fatalf("twelve seconds after the mark the reads came to %+v, want the probe to have cleared it", stats)
		}
	})
}

// A reader marks down at most a fifth of its list, and always at least one.
// Two holders of six stall for it, and every request to either times out, but
// it marks one of them down and no more.
func TestAReaderMarksDownAtMostAFifthOfItsList(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100,
			cache: func(_ int, cache *checkpoint.CacheConfig) {
				cache.ClusterStripeTimeout = 100 * time.Millisecond
				cache.ClusterBound = time.Hour
			}})
		var pages []uint64
		for page := range uint64(24) {
			pages = append(pages, page)
		}
		ref, m := c.filled(t, 1, "vm", pages)
		reader := c.hosts[0]
		c.degrade(reader, c.hosts[1:3], nil)
		for _, page := range pages {
			c.read(t, reader, ref, m, page)
			time.Sleep(200 * time.Millisecond)
		}
		if stats := reader.cache.Stats().Read; stats.Down != 1 || stats.MarkedDown != 1 || stats.Capped == 0 {
			t.Fatalf("with two of six hosts stalled the reads came to %+v, want one marked down and the other refused a mark",
				stats)
		}
	})
}

// A miss is not a failure of the host. A holder that holds nothing of any page
// answers every request it is asked with nothing, and is never marked down.
func TestAMissIsNotAFailureOfTheHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 4, M: 2}
		// The reader's repairs are dropped for its rate, so the holder stays
		// empty for every read.
		c := newFillCluster(t, fillConfig{hosts: 6, code: code, share: 100,
			cache: func(host int, cache *checkpoint.CacheConfig) {
				if host == 0 {
					cache.FillBytesPerSecond = 1
				}
			}})
		var pages []uint64
		for page := range uint64(16) {
			pages = append(pages, page)
		}
		ref, m := c.filled(t, 1, "vm", pages)
		empty := c.hosts[2]
		windows := []rank.Window{segmentWindow(ref)}
		for _, page := range pages {
			windows = append(windows, pageWindow(ref, page))
		}
		for _, window := range windows {
			for _, index := range empty.cache.HeldIndices(window, 0, code) {
				if err := empty.cache.Drop(c.ctx(t), peer.Drop{Window: window, Index: index, Code: code}); err != nil {
					t.Fatal(err)
				}
			}
		}
		reader := c.hosts[0]
		for _, page := range pages {
			c.read(t, reader, ref, m, page)
		}
		if stats := reader.cache.Stats().Read; stats.Replaced == 0 || stats.MarkedDown != 0 {
			t.Fatalf("a holder that missed came to %+v, want it replaced and never marked down", stats)
		}
	})
}

// One refused connection marks a host down at once: a holder whose process is
// gone refuses its reader's dial, and the reader marks it down on that one
// request and reads from the rest.
func TestARefusedConnectionMarksAHostDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100})
		ref, m := c.filled(t, 1, "vm", []uint64{0})
		reader := c.hosts[0]
		first, _ := picks(*c.list.Load(), reader.cache.Identity(), pageWindow(ref, 0), true)
		c.hostOf(first[0]).shut()
		c.read(t, reader, ref, m, 0)
		if stats := reader.cache.Stats().Read; stats.MarkedDown != 1 || stats.Timeouts != 0 || stats.Hits != 2 {
			t.Fatalf("a read that met a refused connection came to %+v, want its host marked down at once", stats)
		}
	})
}

// Repair sends only an index no rank holds, to a rank that holds fewer than the
// code puts on it. One holder of a 4+2 window has lost its stripe. A reader
// that asks it hears nothing, asks the rest at once, and so hears from every
// rank: it rebuilds the page and sends the holder the one index nobody holds,
// which is the index the list puts on it. Every index is then on exactly the
// rank the list puts it on.
func TestRepairSendsOnlyAnIndexNoRankHolds(t *testing.T) {
	code := rank.Code{K: 4, M: 2}
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: code, share: 100})
		ref, m := c.filled(t, 1, "vm", []uint64{0})
		reader := c.hosts[0]
		window := pageWindow(ref, 0)
		first, _ := picks(*c.list.Load(), reader.cache.Identity(), window, true)
		// The lost stripe is not index 0, so a repair that sent the lowest
		// index the holder lacks would send one another rank holds.
		var lost *fillHost
		for _, cache := range first {
			if h := c.hostOf(cache); h.cache.HeldIndices(window, 0, code)[0] != 0 {
				lost = h
				break
			}
		}
		index := lost.cache.HeldIndices(window, 0, code)[0]
		if err := lost.cache.Drop(c.ctx(t), peer.Drop{Window: window, Index: index, Code: code}); err != nil {
			t.Fatal(err)
		}
		c.read(t, reader, ref, m, 0)
		c.settle(t)
		if got, want := c.placed(window), c.ranked(window); !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("after the repair the stripes are on %v, want %v", got, want)
		}
		if stats := reader.cache.Stats().Read; stats.Repairs != 1 {
			t.Fatalf("the reader's reads came to %+v, want one stripe repaired", stats)
		}
	})
}

// A window whose ranks shifted still reads from the cluster: a seventh cache
// joins a 4+2 cluster, which moves it into the first six ranks of some windows
// and every holder below it down by one. Each of those holders now holds an
// index its new rank would not be given, and every host still reads every page
// with no read of the store, because a reader rebuilds from any k indices it
// is sent (B5 in spec/bugs.md).
func TestAReaderRebuildsFromAnyIndicesAfterTheRanksShift(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 7, code: rank.Code{K: 4, M: 2}, share: 100})
		joining := c.hosts[6]
		c.hold(c.list.Load().Without(joining.cache.Identity()))
		var pages []uint64
		for page := range uint64(12) {
			pages = append(pages, page)
		}
		ref, m := c.filled(t, 1, "vm", pages)
		var caches []rank.Cache
		for _, h := range c.hosts {
			caches = append(caches, rank.Cache{Identity: h.cache.Identity(), Weight: 1, Address: h.address})
		}
		joined, err := rank.NewList(rank.Code{K: 4, M: 2}, caches)
		if err != nil {
			t.Fatal(err)
		}
		shifted := 0
		for _, page := range pages {
			if slices.ContainsFunc(joined.Ranks(pageWindow(ref, page)), func(cache rank.Cache) bool {
				return cache.Identity == joining.cache.Identity()
			}) {
				shifted++
			}
		}
		if shifted == 0 {
			t.Fatal("the join shifted the ranks of no window")
		}
		c.hold(joined)
		for _, h := range c.hosts[:6] {
			if gets := c.readEvery(t, h, ref, m, pages); gets != int64(len(pages)) {
				t.Fatalf("%s made %d requests of the store for %d pages after %d windows' ranks shifted, want only the opens",
					h.name, gets, len(pages), shifted)
			}
		}
	})
}

// One hit of the disk tier in HeadCheckEvery has the part it was served from
// checked with a HEAD. With every hit checked, a read finds its page's part
// there; once the part is deleted behind the cache's back, as a reclamation
// bug would, the next hit reports it missing.
func TestASampledHitChecksItsPartStillExists(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 3, code: rank.Code{K: 2, M: 1}, share: 100,
			cache: func(_ int, cache *checkpoint.CacheConfig) { cache.HeadCheckEvery = 1 }})
		ref, m := c.filled(t, 1, "vm", []uint64{0, 1})
		reader := c.hosts[0]
		c.read(t, reader, ref, m, 0)
		c.settle(t)
		if stats := reader.cache.Stats().Read; stats.HeadChecks != 2 || stats.HeadMissing != 0 {
			t.Fatalf("a read of a page and its segment checked %+v, want two checks and nothing missing", stats)
		}
		deleted := 0
		if err := platform.ListAll(c.ctx(t), c.runtime.ObjectStore(), platform.ObjectPrefix{}, func(object platform.ObjectMetadata) error {
			if !strings.Contains(object.Key.String(), "/part/") {
				return nil
			}
			deleted++
			return c.runtime.ObjectStore().Delete(c.ctx(t), platform.DeleteRequest{Key: object.Key})
		}); err != nil {
			t.Fatal(err)
		}
		if deleted == 0 {
			t.Fatal("the checkpoint has no part to delete")
		}
		c.read(t, reader, ref, m, 1)
		c.settle(t)
		if stats := reader.cache.Stats().Read; stats.HeadChecks != 4 || stats.HeadMissing != 1 {
			t.Fatalf("a hit whose part is gone checked %+v, want it reported missing", stats)
		}
	})
}
