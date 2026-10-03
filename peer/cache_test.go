package peer_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/peer/peertest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// stripeKey names one stored stripe.
type stripeKey struct {
	window rank.Window
	page   uint32
	index  int
}

// memoryCache is a disk cache in memory, as a test needs one: the stripes it
// holds, keeps, drops, and whether it drops every keep. Its reads answer from a
// file range of a file it writes the stripes into, as the checkpoint cache's
// will.
type memoryCache struct {
	identity  rank.Identity
	dropKeeps bool
	// reads, when set, is told of every read, which then waits for hold.
	reads chan struct{}
	hold  chan struct{}

	mu      sync.Mutex
	stripes map[stripeKey][]byte
	dropped []peer.Drop
}

func newMemoryCache(identity byte) *memoryCache {
	return &memoryCache{identity: rank.Identity{identity}, stripes: make(map[stripeKey][]byte)}
}

func (c *memoryCache) Identity() rank.Identity { return c.identity }

func (c *memoryCache) ReadStripes(ctx context.Context, read peer.StripeRead) (peer.Stripes, error) {
	if c.reads != nil {
		c.reads <- struct{}{}
		select {
		case <-c.hold:
		case <-ctx.Done():
			return peer.Stripes{}, context.Cause(ctx)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var keys []stripeKey
	for key := range c.stripes {
		if key.window == read.Window && (read.Pages == nil || slices.Contains(read.Pages, key.page)) {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b stripeKey) int {
		if a.page != b.page {
			return int(a.page) - int(b.page)
		}
		return a.index - b.index
	})
	file := &byteFile{}
	var items []peer.StripeItem
	for _, key := range keys {
		stripe := c.stripes[key]
		items = append(items, peer.StripeItem{Page: key.page, Index: key.index, Length: 4 * len(stripe), Size: len(stripe)})
		file.data = append(file.data, stripe...)
	}
	return peer.Stripes{Items: items, Payload: platform.FileRange{File: file}, Size: int64(len(file.data)),
		FillRight: len(items) == 0}, nil
}

func (c *memoryCache) Keep(_ context.Context, keep peer.Keep) error {
	if c.dropKeeps {
		return peer.ErrDropped
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	offset := 0
	for _, item := range keep.Items {
		c.stripes[stripeKey{keep.Window, item.Page, item.Index}] = bytes.Clone(keep.Payload[offset : offset+item.Size])
		offset += item.Size
	}
	return nil
}

func (c *memoryCache) Drop(_ context.Context, drop peer.Drop) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.stripes, stripeKey{drop.Window, drop.Page, drop.Index})
	c.dropped = append(c.dropped, drop)
	return nil
}

func (c *memoryCache) Presence(_ context.Context, presence peer.Presence) ([][]uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := make([][]uint32, len(presence.Windows))
	for key := range c.stripes {
		for i, window := range presence.Windows {
			if key.window == window && !slices.Contains(held[i], key.page) {
				held[i] = append(held[i], key.page)
			}
		}
	}
	for i := range held {
		slices.Sort(held[i])
	}
	return held, nil
}

var window = rank.Window{Ref: control.Ref{VM: "vm", Sequence: 3}, Volume: "ram0", Number: 7}

// cacheServing is a server of this release whose host keeps cache, and a
// destination's view of it, with every frame the destination receives read.
func cacheServing(t *testing.T, cache peer.Cache) (*peer.Peer, *[]*peertest.Frame) {
	t.Helper()
	return cacheServingWith(t, cache, peer.TableConfig{})
}

func cacheServingWith(t *testing.T, cache peer.Cache, config peer.TableConfig) (*peer.Peer, *[]*peertest.Frame) {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 1})
	server, err := peer.NewServer(t.Context(), peer.ServerConfig{Network: runtime.Network(), Address: "holder",
		PageSize: pageSize, Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	var mu sync.Mutex
	frames := new([]*peertest.Frame)
	config.Dial = func(ctx context.Context, to platform.Address) (platform.Conn, error) {
		conn, err := runtime.Network().Dial(ctx, "reader", to)
		if err != nil {
			return nil, err
		}
		return &recordingConn{Conn: conn, record: func(frame *peertest.Frame) {
			mu.Lock()
			defer mu.Unlock()
			*frames = append(*frames, frame)
		}}, nil
	}
	table := newTable(t, config)
	return table.Peer("holder"), frames
}

// recordingConn reads every frame it receives and hands it on whole.
type recordingConn struct {
	platform.Conn
	record func(*peertest.Frame)
}

func (c *recordingConn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	received, err := c.Conn.Receive(ctx)
	if err != nil {
		return received, err
	}
	frame, err := peertest.Read(received)
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	c.record(frame)
	return frame.Pass(), nil
}

// Keep writes stripes to the holder's cache, a read returns every stripe it
// holds of the pages asked for in page and index order, and the reply carries
// no checksum of its payload: each stripe carries its own.
func TestStripesAreKeptAndReadBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMemoryCache(1)
		holder, frames := cacheServing(t, cache)
		code := rank.Code{K: 2, M: 1}
		if err := holder.Keep(t.Context(), cache.identity, peer.Keep{Window: window, Code: code,
			Items:   []peer.StripeItem{{Page: 3, Index: 0, Length: 12, Size: 3}, {Page: 3, Index: 2, Length: 12, Size: 4}, {Page: 9, Index: 1, Length: 8, Size: 2}},
			Payload: []byte("abcdefghi")}); err != nil {
			t.Fatal(err)
		}
		reply, err := holder.ReadStripes(t.Context(), cache.identity, peer.StripeRead{Window: window, Pages: []uint32{3},
			Code: code, MaxBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		defer reply.Release()
		want := []peer.StripeItem{{Page: 3, Index: 0, Length: 12, Size: 3}, {Page: 3, Index: 2, Length: 16, Size: 4}}
		if !slices.Equal(reply.Items, want) || string(reply.Payload) != "abcdefg" || reply.FillRight {
			t.Fatalf("read back %+v %q, fill right %v", reply.Items, reply.Payload, reply.FillRight)
		}
		last := (*frames)[len(*frames)-1]
		if last.Checksummed() {
			t.Fatal("a reply of stripes carried a checksum of its payload")
		}
		empty, err := holder.ReadStripes(t.Context(), cache.identity, peer.StripeRead{Window: rank.Window{Number: 1},
			Code: code, MaxBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		if len(empty.Items) != 0 || !empty.FillRight {
			t.Fatalf("a window the holder lacks came back %+v", empty)
		}
	})
}

// Every cache request names the cache it expects: a host whose cache is
// another — a pod that took the address of one that left — answers not me, as
// a host that keeps no cache does. Neither marks the peer down.
func TestAReusedAddressAnswersNotMe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		code := rank.Code{K: 1, M: 1}
		for _, cache := range []peer.Cache{newMemoryCache(2), nil} {
			holder, _ := cacheServing(t, cache)
			stale := rank.Identity{1}
			if _, err := holder.ReadStripes(t.Context(), stale, peer.StripeRead{Window: window, Code: code,
				MaxBytes: 1024}); !errors.Is(err, peer.ErrNotMe) {
				t.Fatalf("a read naming another cache = %v, want ErrNotMe", err)
			}
			if err := holder.Keep(t.Context(), stale, peer.Keep{Window: window, Code: code,
				Items: []peer.StripeItem{{Size: 1}}, Payload: []byte("x")}); !errors.Is(err, peer.ErrNotMe) {
				t.Fatalf("a keep naming another cache = %v, want ErrNotMe", err)
			}
			if err := holder.Drop(t.Context(), stale, peer.Drop{Window: window, Code: code}); !errors.Is(err, peer.ErrNotMe) {
				t.Fatalf("a drop naming another cache = %v, want ErrNotMe", err)
			}
			if _, err := holder.Presence(t.Context(), stale, peer.Presence{Windows: []rank.Window{window}, Code: code}); !errors.Is(err, peer.ErrNotMe) {
				t.Fatalf("a presence naming another cache = %v, want ErrNotMe", err)
			}
			held, err := holder.Probe(t.Context(), stale)
			if !errors.Is(err, peer.ErrNotMe) {
				t.Fatalf("a probe naming another cache = %v, want ErrNotMe", err)
			}
			if want := (rank.Identity{}); cache != nil {
				want = cache.Identity()
				if held != want {
					t.Fatalf("the probe reported cache %v, want %v", held, want)
				}
			}
			if holder.Down() {
				t.Fatal("a host that is not the cache named was marked down")
			}
		}
	})
}

// A probe that names no cache asks only whether the host is there, and says
// which cache it keeps.
func TestAProbeNamingNoCacheReportsTheCacheKept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMemoryCache(5)
		holder, _ := cacheServing(t, cache)
		held, err := holder.Probe(t.Context(), rank.Identity{})
		if err != nil || held != cache.identity {
			t.Fatalf("a probe naming no cache = %v, %v; want %v", held, err, cache.identity)
		}
	})
}

// A keep the cache does not write is dropped, not queued, and says so.
func TestAKeepTheCacheDoesNotWriteIsDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMemoryCache(1)
		cache.dropKeeps = true
		holder, _ := cacheServing(t, cache)
		err := holder.Keep(t.Context(), cache.identity, peer.Keep{Window: window, Code: rank.Code{K: 1, M: 1},
			Items: []peer.StripeItem{{Size: 2}}, Payload: []byte("ab")})
		if !errors.Is(err, peer.ErrDropped) {
			t.Fatalf("a keep the cache dropped = %v, want ErrDropped", err)
		}
	})
}

// A drop reaches the cache with the stripe it names, and a presence check says
// which pages of each window the cache holds a stripe of.
func TestDropAndPresenceReachTheCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMemoryCache(1)
		holder, _ := cacheServing(t, cache)
		code := rank.Code{K: 1, M: 1}
		other := rank.Window{Ref: control.Ref{VM: "vm", Sequence: 3}, Volume: "disk"}
		if err := holder.Keep(t.Context(), cache.identity, peer.Keep{Window: window, Code: code,
			Items: []peer.StripeItem{{Page: 0, Size: 1}, {Page: 4, Size: 1}, {Page: 4, Index: 1, Size: 1}}, Payload: []byte("abc")}); err != nil {
			t.Fatal(err)
		}
		if err := holder.Drop(t.Context(), cache.identity, peer.Drop{Window: window, Page: 0, Index: 0, Code: code}); err != nil {
			t.Fatal(err)
		}
		if want := []peer.Drop{{Window: window, Page: 0, Index: 0, Code: code}}; !slices.Equal(cache.dropped, want) {
			t.Fatalf("the cache was told to drop %+v", cache.dropped)
		}
		held, err := holder.Presence(t.Context(), cache.identity, peer.Presence{Windows: []rank.Window{window, other}, Code: code})
		if err != nil {
			t.Fatal(err)
		}
		if len(held) != 2 || !slices.Equal(held[0], []uint32{4}) || len(held[1]) != 0 {
			t.Fatalf("presence %v, want page 4 of the first window and nothing of the second", held)
		}
	})
}

// A reader bounds the stripe bytes its reads have in flight at all its peers:
// a read past the bound waits for one in flight to come back rather than
// adding to what the reader's memory must hold.
func TestAReaderBoundsItsStripeBytesInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache := newMemoryCache(1)
		cache.reads, cache.hold = make(chan struct{}, 4), make(chan struct{})
		holder, _ := cacheServingWith(t, cache, peer.TableConfig{StripeBytes: 64 << 10})
		read := peer.StripeRead{Window: window, Code: rank.Code{K: 1, M: 1}, MaxBytes: 48 << 10}
		done := make(chan error, 2)
		for range 2 {
			go func() {
				reply, err := holder.ReadStripes(t.Context(), cache.identity, read)
				if err == nil {
					reply.Release()
				}
				done <- err
			}()
		}
		<-cache.reads
		synctest.Wait()
		if len(cache.reads) != 0 {
			t.Fatal("a read past the bound on stripe bytes in flight reached the holder")
		}
		close(cache.hold)
		for range 2 {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		<-cache.reads
	})
}
