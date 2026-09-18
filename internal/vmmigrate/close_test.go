package vmmigrate_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
)

// heldListener hands out connections whose replies wait for release, so a test
// can have a request on the wire and no answer to it at a moment of its
// choosing. sending reports the first reply reaching that point.
type heldListener struct {
	platform.Listener
	sending chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHeldListener(listener platform.Listener) *heldListener {
	return &heldListener{Listener: listener,
		sending: make(chan struct{}), release: make(chan struct{})}
}

func (l *heldListener) Accept(ctx context.Context) (platform.Conn, error) {
	conn, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return heldConn{Conn: conn, listener: l}, nil
}

type heldConn struct {
	platform.Conn
	listener *heldListener
}

func (c heldConn) Send(ctx context.Context, frame platform.Frame) error {
	c.listener.once.Do(func() { close(c.listener.sending) })
	select {
	case <-c.listener.release:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	return c.Conn.Send(ctx, frame)
}

// heldSource is a page source serving this VM whose every reply waits for the
// listener to be released.
func (s *served) heldSource(t *testing.T, address platform.Address) *heldListener {
	t.Helper()
	listener, err := s.migration.cluster.runtime.Network().Listen(address)
	if err != nil {
		t.Fatal(err)
	}
	held := newHeldListener(listener)
	source, err := vmmigrate.NewPageSource(t.Context(), vmmigrate.SourceConfig{PageSize: pageSize,
		MaxPagesPerRequest: 8, MaxBytesInFlightPerPeer: 32 << 20, Address: address, Listener: held})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A close that is still holding a reply would wait for one forever.
		held.let()
		_ = source.Close()
	})
	source.Serve("vm-2", vmmigrate.RegionPages(s.machine.Regions()))
	s.source = source
	return held
}

func (l *heldListener) let() {
	select {
	case <-l.release:
	default:
		close(l.release)
	}
}

// TestClosingAPostCopyReadsTheVolumeForTheRequestInFlight. Closing a receive is
// what a destination does the moment the post-copy is over, and its guest is
// running and faulting all the while: one of those faults is on the wire to the
// source when the close lands. It must come back with the checkpoint's bytes —
// which is where every page of it is by then — rather than with this host's own
// end as a failure, because a fault that fails ends the memory session, and
// ending that session kills a guest whose pages are all here.
func TestClosingAPostCopyReadsTheVolumeForTheRequestInFlight(t *testing.T) {
	s := newServed(t, nil, 4)
	held := s.heldSource(t, "source-pages-held")
	backing := s.backing(t, s.source, "ram0")

	data := make([]byte, 4*pageSize)
	loaded := make(chan error, 1)
	go func() { loaded <- backing.Load(t.Context(), 0, data) }()
	<-held.sending
	// The request is on the wire and the source is about to answer it. This is
	// the moment a receive that has finished streaming closes its backings.
	if err := backing.Close(); err != nil {
		t.Fatal(err)
	}
	held.let()
	if err := <-loaded; err != nil {
		t.Fatalf("closing a finished post-copy failed the read it interrupted: %v", err)
	}
	// Every page of this run is one the volume holds, so the volume is what
	// answered: the pages the source had were never published, which is exactly
	// why they read as zeroes here.
	if want := make([]byte, len(data)); !bytes.Equal(data, want) {
		t.Fatal("the interrupted read did not come from this host's own volume")
	}
	if stats := backing.Stats(); stats.VolumePages == 0 || !stats.FellBack {
		t.Fatalf("peer backing: %+v", stats)
	}
}

// TestClosingAPostCopyStillRefusesAPageOnlyTheSourceHad is the other half of
// it: a page the handoff named as the source's own and that never arrived is
// not the volume's to answer, whatever interrupted the read. The volume holds
// the bytes from before the guest wrote them, and handing those to a guest
// would rewind it silently. The read ends with the close that stopped it and
// fills nothing, which is what asking_test.go's discard pins from the other
// side; what this adds is that the volume is not read in its place.
func TestClosingAPostCopyStillRefusesAPageOnlyTheSourceHad(t *testing.T) {
	s := newServed(t, nil, 4)
	held := s.heldSource(t, "source-pages-held-unpublished")
	backing, err := vmmigrate.NewPeerBacking(vmmigrate.PeerConfig{Volume: s.vm.Volume("ram0"),
		Peer: s.source.Address(), VM: "vm-2", PageSize: pageSize, MaxPagesPerRequest: 8,
		Unpublished: []vmmigrate.PageRun{{First: 0, Count: 4}},
		Dial:        s.migration.cluster.dialer("dest")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backing.Close() })

	data := make([]byte, 4*pageSize)
	loaded := make(chan error, 1)
	go func() { loaded <- backing.Load(t.Context(), 0, data) }()
	<-held.sending
	if err := backing.Close(); err != nil {
		t.Fatal(err)
	}
	held.let()
	err = <-loaded
	if !errors.Is(err, vmmigrate.ErrClosed) {
		t.Fatalf("a read of a page only the source ever had reported %v, want ErrClosed", err)
	}
	if !bytes.Equal(data, make([]byte, len(data))) {
		t.Fatal("the read was answered out of a volume that never held those pages")
	}
	if stats := backing.Stats(); stats.VolumePages != 0 {
		t.Fatalf("the volume answered for a page only the source ever had: %+v", stats)
	}
}
