package vmmigrate_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// portedListener gives every accepted connection the address a real TCP peer
// has: one host, and the ephemeral port that connection happens to have been
// given. A simulated listener reports the peer's node name alone, so without
// this nothing here would ever see the port that a production source counts its
// budgets against.
type portedListener struct {
	platform.Listener
	next atomic.Int64
}

func (l *portedListener) Accept(ctx context.Context) (platform.Conn, error) {
	conn, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return portedConn{Conn: conn,
		remote: platform.Address(fmt.Sprintf("10.0.0.7:%d", 40000+l.next.Add(1)))}, nil
}

type portedConn struct {
	platform.Conn
	remote platform.Address
}

func (c portedConn) RemoteAddress() platform.Address { return c.remote }

// TestPeerBudgetsCountOneDestinationHostOnce. A destination opens a connection
// per memory region and dials again whenever one breaks, and every one of those gets an
// ephemeral port of its own. Counting them as separate peers counts nothing: the
// connection budget never binds, the bytes budget never binds, and the table of
// peers grows with every reconnect for as long as the source serves.
func TestPeerBudgetsCountOneDestinationHostOnce(t *testing.T) {
	s := newServed(t, nil, 4)
	const address platform.Address = "source-pages-ported"
	listener, err := s.migration.cluster.runtime.Network().Listen(address)
	if err != nil {
		t.Fatal(err)
	}
	source, err := vmmigrate.NewPageSource(t.Context(), vmmigrate.SourceConfig{PageSize: pageSize,
		MaxPagesPerRequest: 8, MaxBytesInFlightPerPeer: 32 << 20, MaxConnectionsPerPeer: 1,
		Address: address, Listener: &portedListener{Listener: listener}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	source.Serve("vm-2", vmmigrate.MemoryRegionPages(s.machine.MemoryRegions()))

	first, second := s.backing(t, source, "ram0"), s.backing(t, source, "ram0")
	data := make([]byte, 4*pageSize)
	if err := first.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	if stats := first.Stats(); stats.PeerPages != 4 {
		t.Fatalf("the first connection was not served: %+v", stats)
	}
	// The first backing keeps its connection, so the second one's is this same
	// host's second and over a budget of one.
	if err := second.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	if stats := second.Stats(); stats.PeerPages != 0 {
		t.Fatalf("a connection over the budget was served: %+v", stats)
	}
	if refused := source.Stats().Refused; refused == 0 {
		t.Fatal("a second connection from one destination host was counted as a second peer")
	}
}
