package vmmigrate_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/peer"
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

// TestPeerBudgetsCountOneDestinationHostOnce. A destination opens several
// connections and dials again whenever one breaks, and every one of them gets
// an ephemeral port of its own. Counting them as separate peers counts nothing:
// the budget never binds, and the table of budgets grows with every reconnect
// for as long as the server serves. So a request of one destination host's that
// would take its class past the budget is answered BUSY whichever connection it
// came on.
func TestPeerBudgetsCountOneDestinationHostOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServed(t, nil, 4)
		const address platform.Address = "source-pages-ported"
		listener, err := s.migration.cluster.runtime.Network().Listen(address)
		if err != nil {
			t.Fatal(err)
		}
		source, err := peer.NewServer(t.Context(), peer.ServerConfig{PageSize: pageSize,
			MaxPagesPerRequest: 8, Budgets: budgets(4 * pageSize),
			Address: address, Listener: &portedListener{Listener: listener}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = source.Close() })
		gate := newPageGate()
		source.Serve("vm-2", gated(vmmigrate.MemoryRegionPages(s.machine.MemoryRegions()), gate))

		// Two backings, each with a table and so a connection of its own: the
		// first's request holds the whole budget while it is answered.
		first, second := s.backing(t, source, "ram0"), s.backing(t, source, "ram0")
		held := make(chan error, 1)
		go func() { held <- first.Load(t.Context(), 0, make([]byte, 4*pageSize)) }()
		<-gate.entered
		if err := second.Load(t.Context(), 0, make([]byte, 4*pageSize)); err != nil {
			t.Fatal(err)
		}
		if stats := second.Stats(); stats.PeerPages != 0 || stats.Refusals != 1 {
			t.Fatalf("a second connection of one destination host was served past its budget: %+v", stats)
		}
		if refused := source.Stats().Refused; refused != 1 {
			t.Fatalf("a second connection from one destination host was counted as a second peer: %d refusals", refused)
		}
		close(gate.open)
		if err := <-held; err != nil {
			t.Fatal(err)
		}
		if stats := first.Stats(); stats.PeerPages != 4 {
			t.Fatalf("the request that held the budget: %+v", stats)
		}
	})
}
