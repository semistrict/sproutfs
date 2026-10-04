package peer_test

import (
	"bytes"
	"context"
	"net"
	"testing"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// A keep carries its stripes as its request's payload, and over real TCP a
// payload is read under the deadline of the receive that found its frame. A
// keep of half a megabyte, more than a socket's buffer holds, is kept, and
// read back, over a loopback socket. The GCE run of 2026-10-03 found every
// keep reset by its holder: the server had ended the receive's context before
// it read the payload.
func TestAKeepIsKeptOverTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := platform.Address(listener.Addr().String())
	_ = listener.Close()
	runtime := sim.New(sim.Config{Seed: 1})
	ctx := sim.WithRuntime(t.Context(), runtime)
	network := adapters.NewNetwork()
	cache := newMemoryCache(1)
	server, err := peer.NewServer(ctx, peer.ServerConfig{Network: network, Address: address, PageSize: pageSize,
		Cache: cache, Membership: cache.source, Member: cache.member})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	table, err := peer.NewTable(ctx, peer.TableConfig{Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
		return network.Dial(ctx, "", to)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer table.Close()
	holder := table.Peer(address)
	stripe := bytes.Repeat([]byte("stripe"), 100_000)
	code := rank.Code{K: 1, M: 1}
	if err := holder.Keep(t.Context(), cache.route, peer.Keep{Window: window, Code: code,
		Items: []peer.StripeItem{{Page: 0, Index: 0, Length: len(stripe), Size: len(stripe)}}, Payload: stripe}); err != nil {
		t.Fatalf("a keep of %d bytes over TCP: %v", len(stripe), err)
	}
	reply, err := holder.ReadStripes(t.Context(), cache.route, peer.StripeRead{Window: window, Code: code,
		MaxBytes: int64(len(stripe))})
	if err != nil {
		t.Fatal(err)
	}
	defer reply.Release()
	if !bytes.Equal(reply.Payload, stripe) {
		t.Fatal("the stripe kept over TCP reads back other bytes")
	}
}
