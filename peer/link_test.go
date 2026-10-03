package peer_test

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// The small-behind-large problem. A guest fault's one page crosses the same
// link as the post-copy stream's megabytes, and a link carries bytes in the
// order they were sent: the fault's reply leaves behind every stream byte the
// source sent before it. What bounds that wait is what bounds the stream's
// bytes in flight, which is the destination's background budget. The source
// here would let the stream hold sixty-four megabytes, a quarter of a second of
// this link; the budget holds it to four, and a fault waits no longer than the
// link takes to carry them.
//
// The stream still has the link: it is paced by the budget, not stopped by it.
func TestAGuestFaultIsAnsweredWhileTheStreamSaturatesTheLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			link       = 256 << 20 // bytes a second, each way
			background = 4 << 20
			request    = 256 // pages, a megabyte
			volume     = 1 << 16
			streams    = 16
		)
		runtime := sim.New(sim.Config{Seed: 1, Network: sim.NetworkConfig{Latency: 100 * time.Microsecond,
			LinkBytesPerSecond: link}})
		pages := noisyPages{memoryPages{count: volume, pageSize: pageSize}}
		server, err := peer.NewServer(sim.WithRuntime(t.Context(), runtime), peer.ServerConfig{
			Network: runtime.Network(), Address: "source", PageSize: pageSize, MaxPagesPerRequest: request,
			MaxInFlight: 64, Budgets: peer.Budgets{Fault: 8 << 20, BulkRead: 64 << 20, BulkWrite: 16 << 20}})
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		server.Serve("vm", map[string]peer.Pages{"ram0": pages})
		table, err := peer.NewTable(sim.WithRuntime(t.Context(), runtime), peer.TableConfig{
			Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
				return runtime.Network().Dial(ctx, "destination", to)
			},
			// A simulated connection has one frame on the link at a time, as
			// a socket's send buffer holds about one: sixteen bulk connections
			// can put sixteen megabytes ahead of a fault.
			Connections: peer.Connections{Fault: 1, BulkRead: streams, BulkWrite: 1}, InFlight: 4,
			BackgroundBytes: background})
		if err != nil {
			t.Fatal(err)
		}
		defer table.Close()
		source := table.Peer("source")

		ctx, stop := context.WithCancel(t.Context())
		stream := peer.WithPriority(peer.WithStream(ctx), peer.Resident)
		var streamed atomic.Int64
		var wg sync.WaitGroup
		for worker := range uint64(streams) {
			wg.Go(func() {
				for first := worker * request; ; first = (first + streams*request) % volume {
					answer, err := askPages(stream, source, first, request)
					if errors.Is(err, context.Canceled) {
						return
					}
					if err != nil {
						t.Errorf("the stream: %v", err)
						return
					}
					if !bytes.Equal(answer.Payload[:pageSize], pages.page(first)) {
						t.Errorf("the stream read page %d wrong", first)
						return
					}
					streamed.Add(int64(len(answer.Payload)))
				}
			})
		}
		// The stream fills the link first.
		time.Sleep(200 * time.Millisecond)
		began, before := time.Now(), streamed.Load()
		bound := time.Duration(background)*time.Second/link + time.Millisecond
		var slowest time.Duration
		for fault := range uint64(20) {
			asked := time.Now()
			answer, err := askPages(t.Context(), source, fault*101%volume, 1)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(answer.Payload, pages.page(fault*101%volume)) {
				t.Fatalf("fault %d read the wrong page", fault)
			}
			slowest = max(slowest, time.Since(asked))
			time.Sleep(50 * time.Millisecond)
		}
		rate := float64(streamed.Load()-before) / time.Since(began).Seconds()
		stop()
		wg.Wait()
		t.Logf("slowest fault %v, bound %v; the stream ran at %.0f MiB/s of a %d MiB/s link",
			slowest, bound, rate/(1<<20), link>>20)
		if slowest > bound {
			t.Errorf("a guest fault waited %v behind the stream, more than the %v the link takes to carry the background budget",
				slowest, bound)
		}
		if rate < 0.9*link {
			t.Errorf("the stream ran at %.0f MiB/s, under nine tenths of the link's %d MiB/s", rate/(1<<20), link>>20)
		}
	})
}

// noisyPages is memoryPages whose pages are seeded noise, which no compression
// shrinks: what crosses the link is every byte of every page.
type noisyPages struct{ memoryPages }

func (p noisyPages) page(number uint64) []byte {
	page := make([]byte, p.pageSize)
	random := rand.NewChaCha8([32]byte{byte(number), byte(number >> 8), byte(number >> 16), byte(number >> 24)})
	_, _ = random.Read(page)
	return page
}

func (p noisyPages) ReadResident(_ context.Context, number uint64, dst []byte) (bool, bool, error) {
	if number >= p.count {
		return false, false, peer.ErrPastEnd
	}
	copy(dst, p.page(number))
	return true, number%2 == 0, nil
}
