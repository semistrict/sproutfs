package vmmigrate_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	goruntime "runtime"
	"sync"
	"testing"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/peer/peertest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// heldListener hands out connections whose replies of one kind wait for
// release, so a test can have a request on the wire and no answer to it at a
// moment of its choosing. sending reports the first such reply reaching that
// point.
type heldListener struct {
	platform.Listener
	// holds names the replies that wait: a page request's, unless a test
	// holds another kind.
	holds   func(platform.Frame) bool
	sending chan struct{}
	release chan struct{}
	once    sync.Once
	// deliver hands the frame to the destination before the hold rather than
	// after it, so a test can have a reply the destination has already acted on
	// and a send that has not returned — which is the order a real socket puts
	// the two in.
	deliver bool
	// err is what the held send finally reports. A send that fails delivers
	// nothing, whatever deliver says.
	err error
}

func newHeldListener(listener platform.Listener, holds func(platform.Frame) bool) *heldListener {
	return &heldListener{Listener: listener, holds: holds,
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
	if !c.listener.holds(frame) {
		return c.Conn.Send(ctx, frame)
	}
	c.listener.once.Do(func() { close(c.listener.sending) })
	deliver := c.listener.deliver && c.listener.err == nil
	if deliver {
		if err := c.Conn.Send(ctx, frame); err != nil {
			return err
		}
	}
	select {
	case <-c.listener.release:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	if c.listener.err != nil {
		return c.listener.err
	}
	if deliver {
		return nil
	}
	return c.Conn.Send(ctx, frame)
}

// heldSource is a peer server serving this VM whose every page reply waits for
// the listener to be released.
func (s *served) heldSource(t *testing.T, address platform.Address) *heldListener {
	t.Helper()
	return s.heldSourceHolding(t, address, peertest.IsPageReply, false, nil)
}

// heldSourceHolding is heldSource holding the replies holds names, with what
// the hold does to the reply it is holding: deliver says the reply reaches the
// destination before the send that carried it returns, and sendErr is what
// that send finally reports.
func (s *served) heldSourceHolding(t *testing.T, address platform.Address, holds func(platform.Frame) bool,
	deliver bool, sendErr error) *heldListener {
	t.Helper()
	source, held := s.migration.cluster.heldServer(t, address, holds)
	held.deliver, held.err = deliver, sendErr
	source.Serve("vm-2", vmmigrate.MemoryRegionPages(s.machine.MemoryRegions()))
	s.source = source
	return held
}

// heldServer is a peer server at address, serving nothing yet, whose replies
// that holds names wait for the listener to be released.
func (c *cluster) heldServer(t *testing.T, address platform.Address,
	holds func(platform.Frame) bool) (*peer.Server, *heldListener) {
	t.Helper()
	listener, err := c.runtime.Network().Listen(address)
	if err != nil {
		t.Fatal(err)
	}
	held := newHeldListener(listener, holds)
	source, err := peer.NewServer(t.Context(), peer.ServerConfig{PageSize: pageSize,
		MaxPagesPerRequest: 8, Budgets: budgets(32 << 20), Address: address, Listener: held})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// A close that is still holding a reply would wait for one forever.
		held.let()
		_ = source.Close()
	})
	return source, held
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
	onOneProcessorAndAll(t, func(t *testing.T) {
		s := newServed(t, nil, 4)
		held := s.heldSource(t, "source-pages-held")
		backing := s.backing(t, s.source, "ram0")

		data := make([]byte, 4*pageSize)
		loaded := make(chan error, 1)
		go func() { loaded <- backing.Load(s.ctx(t), 0, data) }()
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
	})
}

// TestClosingAPostCopyStillRefusesAPageOnlyTheSourceHad is the other half of
// it: a page the handoff named as the source's own and that never arrived is
// not the volume's to answer, whatever interrupted the read. The volume holds
// the bytes from before the guest wrote them, and handing those to a guest
// would rewind it silently. The read ends with the close that stopped it and
// fills nothing, which is what asking_test.go's discard pins from the other
// side; what this adds is that the volume is not read in its place.
func TestClosingAPostCopyStillRefusesAPageOnlyTheSourceHad(t *testing.T) {
	onOneProcessorAndAll(t, func(t *testing.T) {
		s := newServed(t, nil, 4)
		held := s.heldSource(t, "source-pages-held-unpublished")
		backing, err := vmmigrate.NewPeerBacking(vmmigrate.PeerConfig{Volume: s.vm.Volume("ram0"),
			Peer: s.source.Address(), VM: "vm-2", PageSize: pageSize, MaxPagesPerRequest: 8,
			Unpublished: []vmmigrate.PageRun{{First: 0, Count: 4}},
			Peers:       s.migration.cluster.peers(t, s.migration.cluster.dialer("dest"))})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = backing.Close() })

		data := make([]byte, 4*pageSize)
		loaded := make(chan error, 1)
		go func() { loaded <- backing.Load(s.ctx(t), 0, data) }()
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
	})
}

// TestClosingAForkChildEndsTheClaimInFlight is the same close reaching a fork
// child's claim of its hold. A receive closed while that claim is on the wire
// has given the child up: its host discards it. The claim must end with the
// close rather than report the hold claimed, even though the parent's host
// marked it claimed and says so after the close. A claim that took that answer
// would tell the host to run a child whose receive it has just closed.
func TestClosingAForkChildEndsTheClaimInFlight(t *testing.T) {
	onOneProcessorAndAll(t, func(t *testing.T) {
		m := newMigration(t)
		parent, held := m.cluster.heldServer(t, "parent-claims-held", peertest.IsClaimReply)
		_, handoff := m.forkedFrom(t, "vm-2", parent)
		received, _ := m.receive(t, handoff)
		if err := received.Done(t.Context()); err != nil {
			t.Fatal(err)
		}

		claimed := make(chan error, 1)
		go func() { claimed <- received.Claim(m.ctx(t)) }()
		<-held.sending
		// The parent's host has marked the hold claimed and is about to say
		// so. This is the moment a host that gave the child up closes it.
		received.Close()
		held.let()
		if err := <-claimed; !errors.Is(err, vmmigrate.ErrClosed) {
			t.Fatalf("a claim the close interrupted reported %v, want ErrClosed", err)
		}
		if !parent.Discard("vm-2") {
			t.Fatal("the parent's host never marked the hold claimed, so no answer came after the close")
		}
	})
}

// TestClosingAPostCopyEndsTheListingInFlight is the same close reaching the
// listing of what the source holds, which the stream asks for once the pages
// no checkpoint has are here. A listing is made under the backing's life like
// every other request, so the close ends it, and the answer the source sends
// after the close is not taken.
func TestClosingAPostCopyEndsTheListingInFlight(t *testing.T) {
	onOneProcessorAndAll(t, func(t *testing.T) {
		s := newServed(t, nil, 4)
		held := s.heldSourceHolding(t, "source-listing-held", peertest.IsListingReply, false, nil)
		backing := s.backing(t, s.source, "ram0")

		listed := make(chan error, 1)
		go func() {
			_, err := backing.Resident(s.ctx(t))
			listed <- err
		}()
		<-held.sending
		if err := backing.Close(); err != nil {
			t.Fatal(err)
		}
		held.let()
		if err := <-listed; !errors.Is(err, vmmigrate.ErrClosed) {
			t.Fatalf("a listing the close interrupted reported %v, want ErrClosed", err)
		}
	})
}

// onOneProcessorAndAll runs test three times on one processor and then on as
// many as the test binary has. A close ends a request in flight from a
// goroutine of its own, and on one processor the Go scheduler runs the
// delivery of a reply the source sends right after the close before that
// goroutine in about nine runs of ten: the reply is there to be taken, and
// only the backing's own check of its end refuses it.
func onOneProcessorAndAll(t *testing.T, test func(t *testing.T)) {
	t.Helper()
	all := goruntime.GOMAXPROCS(0)
	for run, processors := range []int{1, 1, 1, all} {
		t.Run(fmt.Sprintf("%d-processors-%d", processors, run), func(t *testing.T) {
			goruntime.GOMAXPROCS(processors)
			t.Cleanup(func() { goruntime.GOMAXPROCS(all) })
			test(t)
		})
	}
}

// ctx is the test's context carrying the cluster's runtime, so the in-tree bug
// guards a negative test enables reach the backings' requests.
func (m *migration) ctx(t *testing.T) context.Context {
	return sim.WithRuntime(t.Context(), m.cluster.runtime)
}

// ctx is the served VM's migration's context: see migration.ctx.
func (s *served) ctx(t *testing.T) context.Context { return s.migration.ctx(t) }
