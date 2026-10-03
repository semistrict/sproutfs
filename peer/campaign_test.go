package peer_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/testsoak"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/peer/internal/previous"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// The peer server's campaign. One destination host asks a source of this
// release, a source of the release before, a cache, and a machine that is
// gone, for pages and stripes, over a network with every fault the simulator
// has: a heavy latency tail, pairs that stay slow, one link's bandwidth shared
// by every connection of a pair, bounded send buffers, holds longer than a
// connection's silence allowance, connections closed at random, flipped header
// bits, and, on the seeds that use byte streams, writes cut into pieces down to
// a byte and reassembled by the real framer. The peer server's own sites stall
// connections and answers and refuse requests as busy.
//
// None of it may make a reply wrong. A request may fail, and its caller asks
// again, but every answer that comes back is the bytes the source holds. Once
// the faults stop, every peer answers again without anything being restarted:
// no connection is left stuck and no peer left marked down.
//
// These seeds are the cheapest set that between them activate every site, the
// network's and the peer server's, and reach every probe. The soak runs
// sixty-four.
var campaignSeeds = []uint64{1, 6, 8}

const (
	campaignPages = 512
	campaignChaos = 30 * time.Second
	// campaignQuiet is long enough for a connection of version 1 left stuck
	// by the faults to be found dead and its peer probed back.
	campaignQuiet = time.Minute
)

func TestThePeerServerCampaignNeverAnswersWrong(t *testing.T) {
	seeds := campaignSeeds
	if testsoak.Enabled() {
		seeds = nil
		for seed := range uint64(64) {
			seeds = append(seeds, seed+1)
		}
	}
	reached := map[string]uint64{}
	sites := map[string]bool{}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := runPeerCampaign(t, seed)
				for name, count := range runtime.Probes() {
					reached[name] += count
				}
				for site, on := range runtime.BuggifySites() {
					sites[site] = sites[site] || on
				}
			})
		})
	}
	t.Logf("probes=%v sites=%v", reached, sites)
	for _, name := range peer.Probes {
		if reached[name] == 0 {
			t.Errorf("the campaign never reaches %s", name)
		}
	}
	for _, site := range slices.Concat(peer.Sites,
		[]string{sim.SiteRandomClose, sim.SiteHeaderBitFlip, sim.SiteStreamBitFlip}) {
		if !sites[site] {
			t.Errorf("no seed of the campaign activates the %s site", site)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(reached)) {
		if !slices.Contains(peer.Probes, name) {
			t.Errorf("%s is marked in the code but not in peer.Probes", name)
		}
	}
}

// campaign is one seed's world.
type campaign struct {
	t       *testing.T
	runtime *sim.Runtime
	random  sim.Random
	pages   memoryPages
	cache   *memoryCache
	// source is this release's server, which holds the cache too.
	source *peer.Server
	// destination and other are two tables of one destination host, as two
	// processes of a host would be: the source counts them against one budget,
	// which only it can see, so it answers some of their requests busy.
	destination, other *peer.Table
	// future is a destination two releases ahead, which shares no version
	// with anything here.
	future *peer.Table

	served, failed atomic.Int64
}

func runPeerCampaign(t *testing.T, seed uint64) *sim.Runtime {
	runtime := sim.New(sim.Config{Seed: seed, Buggify: true, Network: sim.NetworkConfig{
		Latency: 200 * time.Microsecond, Jitter: 50 * time.Microsecond, ConnectLatency: 200 * time.Microsecond,
		TailEvery: 200, TailLatency: 2 * time.Second,
		SlowPairPerMille: 300, SlowPairLatency: 20 * time.Millisecond,
		LinkBytesPerSecond: 128 << 20, SendBufferBytes: 1 << 20,
		HangDeadDials: true,
	}})
	// Half the seeds run over the byte streams a host's TCP adapter frames;
	// the other half over framed links, whose header bits flip on their own.
	var network platform.Network = runtime.Network()
	if seed%2 == 1 {
		network = runtime.Network().Framed()
	}
	ctx := sim.WithRuntime(t.Context(), runtime)
	c := &campaign{t: t, runtime: runtime, random: runtime.Random("peer-campaign"),
		pages: memoryPages{count: campaignPages, pageSize: pageSize}, cache: newMemoryCache(7)}
	source, err := peer.NewServer(ctx, peer.ServerConfig{Network: network, Address: "source", PageSize: pageSize,
		MaxPagesPerRequest: 64, Cache: c.cache,
		Budgets: peer.Budgets{Fault: 8 * pageSize, BulkRead: 128 * pageSize, BulkWrite: 64 << 10}})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	source.Serve("vm", map[string]peer.Pages{"ram0": c.pages})
	c.source = source

	listener, err := network.Listen("previous")
	if err != nil {
		t.Fatal(err)
	}
	old := &previous.Server{Served: map[string]map[string]previous.Pages{"vm": {"ram0": c.pages}}}
	serving, stopPrevious := context.WithCancel(t.Context())
	var previousDone sync.WaitGroup
	previousDone.Go(func() { old.Serve(serving, listener) })
	defer func() {
		stopPrevious()
		_ = listener.Close()
		previousDone.Wait()
	}()

	dial := func(ctx context.Context, to platform.Address) (platform.Conn, error) {
		return network.Dial(ctx, "destination", to)
	}
	c.destination = c.table(ctx, peer.TableConfig{Dial: dial})
	c.other = c.table(ctx, peer.TableConfig{Dial: dial})
	c.future = c.table(ctx, peer.TableConfig{Dial: dial, Versions: peer.Versions{Min: 3, Max: 4}})

	c.chaos(ctx)
	runtime.SetBuggify(false)
	time.Sleep(campaignQuiet)
	c.quiet(ctx)
	t.Logf("seed=%d served=%d failed=%d probes=%v fired=%v", seed, c.served.Load(), c.failed.Load(),
		runtime.Probes(), runtime.FiredSites())
	return runtime
}

func (c *campaign) table(ctx context.Context, config peer.TableConfig) *peer.Table {
	table, err := peer.NewTable(ctx, config)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { _ = table.Close() })
	return table
}

// chaos runs every kind of request at once for campaignChaos, while holds come
// and go between the destination and the source.
func (c *campaign) chaos(ctx context.Context) {
	ctx, stop := context.WithTimeout(ctx, campaignChaos)
	defer stop()
	var workers sync.WaitGroup
	for worker := range 3 {
		workers.Go(func() { c.faults(ctx, c.destination.Peer("source"), fmt.Sprintf("fault-%d", worker)) })
	}
	workers.Go(func() { c.faults(ctx, c.other.Peer("source"), "other-fault") })
	for worker, priority := range []peer.Priority{peer.Unpublished, peer.Resident} {
		workers.Go(func() {
			c.stream(peer.WithPriority(peer.WithStream(ctx), priority), c.destination.Peer("source"),
				fmt.Sprintf("stream-%d", worker))
		})
	}
	workers.Go(func() { c.stream(peer.WithStream(ctx), c.other.Peer("source"), "other-stream") })
	workers.Go(func() { c.faults(ctx, c.destination.Peer("previous"), "previous") })
	workers.Go(func() { c.incompatible(ctx) })
	workers.Go(func() { c.gone(ctx) })
	workers.Go(func() { c.stripes(ctx) })
	workers.Go(func() { c.holds(ctx) })
	workers.Wait()
}

// check fails the campaign on an error no fault here may cause: one that calls
// a peer of this release broken, which only a frame it meant can show.
func (c *campaign) check(worker string, err error) {
	c.failed.Add(1)
	if errors.Is(err, peer.ErrMalformed) || errors.Is(err, peer.ErrPageSize) || errors.Is(err, peer.ErrIncompatible) {
		c.t.Errorf("%s: a fault on the way made a sound peer look broken: %v", worker, err)
	}
}

// faults asks for a few pages at a time, giving up on each after a second, as
// a guest fault that has somewhere else to ask does.
func (c *campaign) faults(ctx context.Context, source *peer.Peer, worker string) {
	for attempt := 0; ctx.Err() == nil; attempt++ {
		id := fmt.Sprintf("%s/%d", worker, attempt)
		count := 1 + c.random.Intn(id+"/count", 4)
		first := uint64(c.random.Intn(id+"/first", campaignPages-count))
		asked, cancel := context.WithTimeout(ctx, time.Second)
		answer, err := askPages(asked, source, first, count)
		cancel()
		c.answered(ctx, worker, answer, err, first, count)
		time.Sleep(time.Duration(5+c.random.Intn(id+"/pause", 20)) * time.Millisecond)
	}
}

// stream walks the volume a run at a time, as the post-copy stream does.
func (c *campaign) stream(ctx context.Context, source *peer.Peer, worker string) {
	for first := uint64(0); ctx.Err() == nil; first = (first + 16) % campaignPages {
		answer, err := askPages(ctx, source, first, 16)
		c.answered(ctx, worker, answer, err, first, 16)
		time.Sleep(20 * time.Millisecond)
	}
}

// answered checks one answer to a page request: busy, failed, or exactly the
// pages asked for.
func (c *campaign) answered(ctx context.Context, worker string, answer peer.Answer, err error, first uint64, count int) {
	switch {
	case ctx.Err() != nil:
	case err != nil:
		if worker == "previous" && errors.Is(err, peer.ErrMalformed) {
			// The release before checks no header: a flipped bit in one
			// is what it reads as malformed.
			c.failed.Add(1)
			return
		}
		c.check(worker, err)
	case answer.Busy != nil:
		c.failed.Add(1)
	default:
		c.served.Add(1)
		if worker == "previous" {
			// The release before checks no header. A flipped bit in one can
			// change what it says of a page, its payload's format among it,
			// and nothing finds it: its answers are checked once the faults
			// stop. A header checksum is what version 2 adds for this.
			return
		}
		if err := c.samePages(answer, first, count); err != nil {
			c.t.Errorf("%s: pages %d+%d: %v", worker, first, count, err)
		}
	}
}

// unpublished is the bitmap of the pages from first that no checkpoint holds:
// every even one.
func (c *campaign) unpublished(first uint64, count int) []byte {
	dirty := make([]byte, (count+7)/8)
	for i := range count {
		if (first+uint64(i))%2 == 0 {
			dirty[i/8] |= 1 << (i % 8)
		}
	}
	return dirty
}

// samePages compares an answer with what the source holds: every page
// present, every even one unpublished, and the bytes of each.
func (c *campaign) samePages(answer peer.Answer, first uint64, count int) error {
	var want []byte
	present, dirty := make([]byte, (count+7)/8), c.unpublished(first, count)
	for i := range count {
		want = append(want, c.pages.page(first+uint64(i))...)
		present[i/8] |= 1 << (i % 8)
	}
	if !bytes.Equal(answer.Present, present) || !bytes.Equal(answer.Dirty, dirty) || !bytes.Equal(answer.Payload, want) {
		return fmt.Errorf("present %08b dirty %08b and %d bytes, want %08b %08b and %d bytes",
			answer.Present, answer.Dirty, len(answer.Payload), present, dirty, len(want))
	}
	return nil
}

// incompatible asks the source from a release two ahead, which no fault can
// make it answer.
func (c *campaign) incompatible(ctx context.Context) {
	source := c.future.Peer("source")
	for ctx.Err() == nil {
		_, err := askPages(ctx, source, 0, 1)
		if err == nil {
			c.t.Error("a source answered a destination it shares no version with")
		}
		time.Sleep(2 * time.Second)
	}
}

// gone asks a machine that is gone for stripes: its dials hang, it is marked
// down, and the reads after that skip it.
func (c *campaign) gone(ctx context.Context) {
	gone := c.destination.Peer("gone")
	read := peer.StripeRead{Window: window, Code: rank.Code{K: 1, M: 1}, MaxBytes: 64 << 10}
	for ctx.Err() == nil {
		if reply, err := gone.ReadStripes(ctx, c.cache.identity, read); err == nil {
			reply.Release()
			c.t.Error("a machine that is gone answered a read")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// stripeOf is what stripe index of page holds in this campaign.
func stripeOf(page uint32, index int) []byte {
	return bytes.Repeat([]byte{byte(page), byte(index), 0x5a}, 100)
}

// stripes keeps stripes in the source's cache and reads them back; every
// stripe read is the one that was kept.
func (c *campaign) stripes(ctx context.Context) {
	source := c.destination.Peer("source")
	code := rank.Code{K: 2, M: 1}
	for attempt := 0; ctx.Err() == nil; attempt++ {
		id := fmt.Sprintf("stripes/%d", attempt)
		page := uint32(c.random.Intn(id+"/page", 64))
		var items []peer.StripeItem
		var payload []byte
		for index := range 3 {
			stripe := stripeOf(page, index)
			items = append(items, peer.StripeItem{Page: page, Index: index, Length: 2 * len(stripe), Size: len(stripe)})
			payload = append(payload, stripe...)
		}
		err := source.Keep(ctx, c.cache.identity, peer.Keep{Window: window, Code: code, Items: items, Payload: payload,
			Repair: attempt%5 == 0})
		if err != nil && !errors.Is(err, peer.ErrDropped) && ctx.Err() == nil {
			c.check("keep", err)
		}
		reply, err := source.ReadStripes(ctx, c.cache.identity, peer.StripeRead{Window: window, Pages: []uint32{page},
			Code: code, MaxBytes: 64 << 10})
		switch {
		case ctx.Err() != nil:
		case err != nil:
			c.check("stripes", err)
		default:
			c.served.Add(1)
			offset := 0
			for _, item := range reply.Items {
				if got := reply.Payload[offset : offset+item.Size]; !bytes.Equal(got, stripeOf(item.Page, item.Index)) {
					c.t.Errorf("stripe %d of page %d read back wrong", item.Index, item.Page)
				}
				offset += item.Size
			}
			reply.Release()
		}
		time.Sleep(time.Duration(5+c.random.Intn(id+"/pause", 50)) * time.Millisecond)
	}
}

// holds holds the link between the destination and the source both ways now
// and then, for up to six seconds: past four, a connection is found dead.
func (c *campaign) holds(ctx context.Context) {
	for hold := 0; ; hold++ {
		id := fmt.Sprintf("hold/%d", hold)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(2000+c.random.Intn(id+"/gap", 8000)) * time.Millisecond):
		}
		until := time.Now().Add(time.Duration(500+c.random.Intn(id+"/length", 5500)) * time.Millisecond)
		c.runtime.Network().HoldBoth("destination", "source", until)
	}
}

// quiet is the campaign's last word: with every fault over, each peer answers
// at once, and answers right.
func (c *campaign) quiet(ctx context.Context) {
	for _, table := range []*peer.Table{c.destination, c.other} {
		for _, status := range table.Status() {
			if status.Down && status.Address != "gone" {
				c.t.Errorf("%s is still marked down once every fault is over: %s", status.Address, status.Cause)
			}
		}
	}
	for first := uint64(0); first < campaignPages; first += 64 {
		for _, asked := range []context.Context{ctx, peer.WithStream(ctx)} {
			answer, err := askPages(asked, c.destination.Peer("source"), first, 64)
			if err != nil {
				c.t.Fatalf("once every fault is over, the source: %v", err)
			}
			if err := c.samePages(answer, first, 64); err != nil {
				c.t.Fatalf("once every fault is over, the source's pages %d+64: %v", first, err)
			}
		}
		answer, err := askPages(ctx, c.destination.Peer("previous"), first, 64)
		if err != nil {
			c.t.Fatalf("once every fault is over, the previous release: %v", err)
		}
		if err := c.samePages(answer, first, 64); err != nil {
			c.t.Fatalf("once every fault is over, the previous release's pages %d+64: %v", first, err)
		}
	}
	_, err := askPages(ctx, c.future.Peer("source"), 0, 1)
	var incompatible *peer.IncompatibleError
	if !errors.As(err, &incompatible) || *incompatible != (peer.IncompatibleError{Min: 1, Max: 2}) {
		c.t.Errorf("a destination two releases ahead = %v, want incompatible with versions 1 to 2", err)
	}
	_, err = c.destination.Peer("gone").ReadStripes(ctx, c.cache.identity,
		peer.StripeRead{Window: window, Code: rank.Code{K: 1, M: 1}, MaxBytes: 1 << 10})
	if !errors.Is(err, peer.ErrDown) {
		c.t.Errorf("a read from a machine that is gone = %v, want ErrDown", err)
	}
}
