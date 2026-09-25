package vmmigrate_test

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// brokenListener hands out connections whose sends fail, which is what a
// destination that went away between its request and the answer looks like from
// the source.
type brokenListener struct {
	platform.Listener
}

func (l brokenListener) Accept(ctx context.Context) (platform.Conn, error) {
	conn, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return brokenConn{Conn: conn}, nil
}

type brokenConn struct {
	platform.Conn
}

var errSendFailed = errors.New("the connection went away")

func (c brokenConn) Send(context.Context, platform.Frame) error { return errSendFailed }

// TestAPageIsFetchedOnlyOnceItsReplyIsSent: the source strikes a page off its
// outstanding set when it answers for it, and that set is the only evidence
// anything has that the destination holds the pages no checkpoint has. It struck
// them off as soon as the reply was built, so a send that never reached the
// destination — the connection dropped, the pod deleted — left the source
// believing it had handed over bytes that exist nowhere else, and the next
// release took them with it. The page counts as fetched once its reply is on the
// wire and not before.
func TestAPageIsFetchedOnlyOnceItsReplyIsSent(t *testing.T) {
	m := newMigration(t)
	listener, err := m.cluster.runtime.Network().Listen("broken-source")
	if err != nil {
		t.Fatal(err)
	}
	source, err := vmmigrate.NewPageSource(t.Context(), vmmigrate.SourceConfig{
		Listener: brokenListener{Listener: listener}, Address: "broken-source",
		PageSize: pageSize, MaxPagesPerRequest: 8, MaxBytesInFlightPerPeer: 32 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	s := newServed(t, source, 3)

	// The destination asks, and every reply is dropped on the way out. What it
	// makes of that is its own business — it retries, and reads its own volume
	// for anything a checkpoint holds — but the source has handed over nothing.
	backing := s.dialing(t, source, "ram0", m.cluster.dialer("dest"))
	data := make([]byte, 3*pageSize)
	_ = backing.Load(t.Context(), 0, data)
	if err := source.Release("vm-2"); !errors.Is(err, vmmigrate.ErrOutstanding) {
		t.Fatalf("releasing after replies that never left = %v, want ErrOutstanding", err)
	}
}
