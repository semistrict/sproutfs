package vmmigrate_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
)

// droppingListener hands out connections whose replies never leave this host.
// The destination therefore receives nothing, which is what a source that dies
// between reading a request and answering it looks like from the outside.
type droppingListener struct {
	platform.Listener
	err error
}

func (l droppingListener) Accept(ctx context.Context) (platform.Conn, error) {
	conn, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return droppingConn{Conn: conn, err: l.err}, nil
}

type droppingConn struct {
	platform.Conn
	err error
}

func (c droppingConn) Send(context.Context, platform.Frame) error { return c.err }

// TestAReplyThatNeverLeavesKeepsItsPagesOutstanding is the only evidence a
// source has that a destination holds a page: the reply that carried it. A
// reply this host could not send carried nothing, so the pages it named are
// still this host's alone and the release that would drop them must refuse.
func TestAReplyThatNeverLeavesKeepsItsPagesOutstanding(t *testing.T) {
	s := newServed(t, nil, 4)
	const address platform.Address = "source-pages-dropping"
	listener, err := s.migration.cluster.runtime.Network().Listen(address)
	if err != nil {
		t.Fatal(err)
	}
	source, err := vmmigrate.NewPageSource(t.Context(), vmmigrate.SourceConfig{PageSize: pageSize,
		MaxPagesPerRequest: 8, MaxBytesInFlightPerPeer: 32 << 20, Address: address,
		Listener: droppingListener{Listener: listener, err: errors.New("the reply never left the source")}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	source.Serve("vm-2", vmmigrate.RegionPages(s.machine.Regions()))

	backing := s.backing(t, source, "ram0")
	data := make([]byte, 4*pageSize)
	if err := backing.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	if stats := backing.Stats(); stats.PeerPages != 0 {
		t.Fatalf("a destination that received no reply held %d of the source's pages", stats.PeerPages)
	}
	if err := source.Release("vm-2"); !errors.Is(err, vmmigrate.ErrOutstanding) {
		t.Fatalf("releasing pages no destination received: %v", err)
	}
}

// unpublishedBacking binds a destination to a source holding runs the handoff
// named as its own, which is what a real migration's backing carries.
func (s *served) unpublishedBacking(t *testing.T, source *vmmigrate.PageSource, name string, runs []vmmigrate.PageRun) *vmmigrate.PeerBacking {
	t.Helper()
	address := sourceAddress
	if source != nil {
		address = source.Address()
	}
	return s.unpublishedAt(t, address, name, runs, s.migration.cluster.dialer("dest"))
}

// unpublishedDialing is unpublishedBacking over a dialer of the test's own,
// which is how a test decides what the source does to the replies it sends.
func (s *served) unpublishedDialing(t *testing.T, name string, runs []vmmigrate.PageRun, dial vmmigrate.Dialer) *vmmigrate.PeerBacking {
	t.Helper()
	return s.unpublishedAt(t, sourceAddress, name, runs, dial)
}

func (s *served) unpublishedAt(t *testing.T, address platform.Address, name string,
	runs []vmmigrate.PageRun, dial vmmigrate.Dialer) *vmmigrate.PeerBacking {
	t.Helper()
	backing, err := vmmigrate.NewPeerBacking(vmmigrate.PeerConfig{Volume: s.vm.Volume(name),
		Peer: address, VM: "vm-2", PageSize: pageSize, MaxPagesPerRequest: 8,
		Unpublished: runs, Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backing.Close() })
	return backing
}

// TestALoadIsNotAnInstall separates a load from an install. A page whose bytes
// reached this host's buffer is not a page this host holds: the pager can drop
// it — a full dirty budget on a read-ahead page is the ordinary way — and the
// bytes are then nowhere. Only the pager saying it installed the page may
// strike it off, because striking it off is what lets the source release its
// pages and lets a later read answer from a volume whose bytes predate the
// guest's write.
func TestALoadIsNotAnInstall(t *testing.T) {
	s := newServed(t, nil, 4)
	backing := s.unpublishedBacking(t, nil, "ram0", []vmmigrate.PageRun{{First: 0, Count: 4}})
	data := make([]byte, 4*pageSize)
	unpublished, err := backing.LoadUnpublished(t.Context(), 0, data)
	if err != nil {
		t.Fatal(err)
	}
	for page, private := range unpublished {
		if !private {
			t.Fatalf("page %d came back as the volume's own", page)
		}
	}
	if left := backing.Unfetched(); left != 4 {
		t.Fatalf("a load the pager has not installed left %d pages outstanding, want 4", left)
	}
	// The pager installed two of the four and dropped the rest.
	backing.InstalledUnpublished(0, []bool{true, true, false, false})
	if left := backing.Unfetched(); left != 2 {
		t.Fatalf("after installing two pages %d are outstanding, want 2", left)
	}
	// With the source gone the two it never installed have nowhere to come
	// from, and the volume's bytes predate the guest's write.
	s.migration.pages.Discard("vm-2")
	if err := backing.Load(t.Context(), 2*pageSize, data[:2*pageSize]); !errors.Is(err, vmmigrate.ErrUnpublishedLost) {
		t.Fatalf("reading pages the pager never installed: %v", err)
	}
}

// brokenOnce breaks the first reply it is asked for and then behaves, which is
// what a reset, a source host restarting its listener, or a connection the
// source dropped under its own budget looks like from the destination.
type brokenOnce struct {
	platform.Conn
	broken *bool
}

func (c brokenOnce) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	if !*c.broken {
		*c.broken = true
		return platform.ReceivedFrame{}, platform.ErrDisconnected
	}
	return c.Conn.Receive(ctx)
}

// TestOneBrokenReplyIsNotAPermanentFallback separates a source that is gone
// from one that stumbled, for the pages a checkpoint does hold. A reset
// connection costs this load its round trip and it reads its own volume, which
// has those bytes; what it must not do is decide anything about the source,
// which is still there and still holding the rest. The next load asks it again
// and gets its bytes. Only the source's own answer that it no longer serves the
// VM ends the asking.
func TestOneBrokenReplyIsNotAPermanentFallback(t *testing.T) {
	s := newServed(t, nil, 4)
	broken := false
	dial := s.migration.cluster.dialer("dest")
	backing := s.dialing(t, nil, "ram0", func(ctx context.Context, address platform.Address) (platform.Conn, error) {
		conn, err := dial(ctx, address)
		if err != nil {
			return nil, err
		}
		return brokenOnce{Conn: conn, broken: &broken}, nil
	})
	data := make([]byte, 4*pageSize)
	if err := backing.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, make([]byte, len(data))) {
		t.Fatal("a broken reply was answered with something other than the volume's own bytes")
	}
	if stats := backing.Stats(); stats.FellBack || stats.PeerPages != 0 || stats.VolumePages != 4 {
		t.Fatalf("one broken reply: %+v", stats)
	}
	if err := backing.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	want := s.machine.snapshot()["ram0"][:4*pageSize]
	if !bytes.Equal(data, want) {
		t.Fatal("one broken reply sent the destination to its volume for good")
	}
	if stats := backing.Stats(); stats.FellBack || stats.PeerPages != 4 {
		t.Fatalf("after a broken reply: %+v", stats)
	}
}
