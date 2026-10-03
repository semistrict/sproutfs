package peer_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/peer/internal/previous"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A request on a connection whose peer goes silent — a hold, which a real
// partition is, keeps every byte and says nothing — ends within a few seconds
// rather than when the kernel gives up, and the peer is marked down. Once the
// link carries bytes again, the probe finds the peer and the mark is cleared.
func TestASilentPeerIsFoundDeadAndProbedBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{})
		destination := s.table(t, "destination", peer.TableConfig{})
		// One request first, so the connection exists and its hello is done.
		if _, err := askPages(t.Context(), destination, 0, 1); err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		s.runtime.Network().Hold("source", "destination", began.Add(20*time.Second))
		_, err := disk(t.Context(), destination, 0, 1)
		if !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("a request to a silent peer = %v, want unavailable", err)
		}
		// The connection heard nothing for four seconds, checked once a second.
		if took := time.Since(began); took < 4*time.Second || took > 5*time.Second {
			t.Fatalf("the silent connection was found dead after %v, want 4 to 5 seconds", took)
		}
		if !destination.Down() {
			t.Fatal("a peer whose connection died is not marked down")
		}
		time.Sleep(time.Until(began.Add(20 * time.Second)))
		time.Sleep(10 * time.Second)
		if destination.Down() {
			t.Fatal("the probe did not find the peer back once its link carried bytes again")
		}
		probes := s.runtime.Probes()
		if probes[peer.ProbeDeadConnection] != 1 || probes[peer.ProbeMarkedDown] != 1 || probes[peer.ProbeProbed] == 0 {
			t.Fatalf("probes %v", probes)
		}
		close(s.gate.open)
	})
}

// A slow request is not a dead connection: while the server takes half a
// minute over a reply, it still answers the pings the quiet connection draws,
// and the request ends with its reply and no down mark.
func TestASlowRequestIsNotADeadPeer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{})
		destination := s.table(t, "destination", peer.TableConfig{})
		answered := make(chan error, 1)
		go func() {
			_, err := disk(t.Context(), destination, 0, 1)
			answered <- err
		}()
		<-s.gate.entered
		time.Sleep(25 * time.Second)
		close(s.gate.open)
		if err := <-answered; err != nil {
			t.Fatalf("a slow request: %v", err)
		}
		if destination.Down() || s.runtime.Probes()[peer.ProbeDeadConnection] != 0 {
			t.Fatalf("a slow request marked its peer down: probes %v", s.runtime.Probes())
		}
	})
}

// A request its caller gave up on says nothing about the peer: it is never a
// down mark, however long the peer had kept it waiting.
func TestACancelledRequestMarksNothingDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{})
		destination := s.table(t, "destination", peer.TableConfig{})
		// A dial its caller gives up on mid-connect...
		s.runtime.Network().Hold("destination", "source", time.Now().Add(time.Minute))
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		if _, err := askPages(ctx, destination, 0, 1); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a request whose caller gave up mid-dial = %v", err)
		}
		cancel()
		if destination.Down() {
			t.Fatal("a caller giving up on a dial marked its peer down")
		}
		s.runtime.Network().HealBoth("destination", "source")
		// ...and a request it gives up on mid-answer.
		ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if _, err := disk(ctx, destination, 0, 1); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a request whose caller gave up mid-answer = %v", err)
		}
		if destination.Down() {
			t.Fatal("a caller's own cancellation marked its peer down")
		}
		close(s.gate.open)
	})
}

// A dial that hangs, as one to a machine that is gone does, ends after the
// connect timeout, and that is a hard failure.
func TestAHangingDialIsAHardFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1, Network: sim.NetworkConfig{HangDeadDials: true}})
		var dials atomic.Int64
		table, err := peer.NewTable(sim.WithRuntime(t.Context(), runtime), peer.TableConfig{
			Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
				dials.Add(1)
				return runtime.Network().Dial(ctx, "destination", to)
			}})
		if err != nil {
			t.Fatal(err)
		}
		defer table.Close()
		gone := table.Peer("gone")
		began := time.Now()
		if _, err := askPages(t.Context(), gone, 0, 1); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("a request to a machine that is gone = %v, want unavailable", err)
		}
		if took := time.Since(began); took != 3*time.Second {
			t.Fatalf("the hanging dial ended after %v, want the 3s connect timeout", took)
		}
		if !gone.Down() {
			t.Fatal("a dial that hung is not a hard failure")
		}
		// The probe dials again about a second after the mark, and half as
		// long again each time after, up to ten seconds, and each of those
		// dials hangs for three seconds too: the request's dial and seven
		// probes by the minute's end.
		time.Sleep(time.Minute)
		if got := dials.Load(); got != 8 {
			t.Fatalf("a minute of probing dialed %d times in all, want 8", got)
		}
		status := table.Status()
		if len(status) != 1 || !status[0].Down || status[0].Cause == "" {
			t.Fatalf("the table reports %+v", status)
		}
	})
}

// A connection that carries nothing for thirty seconds is closed, and that says
// nothing about its peer.
func TestAnIdleConnectionIsClosedAndMarksNothingDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{})
		destination := s.table(t, "destination", peer.TableConfig{})
		if _, err := askPages(t.Context(), destination, 0, 1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(29 * time.Second)
		table := destination
		if status := table.Status(); status.Connections.Fault != 1 {
			t.Fatalf("before thirty idle seconds the table holds %+v", status.Connections)
		}
		time.Sleep(2 * time.Second)
		if status := table.Status(); status.Connections.Fault != 0 || status.Down {
			t.Fatalf("after thirty idle seconds the table holds %+v, down %v", status.Connections, status.Down)
		}
		close(s.gate.open)
	})
}

// A dial that hangs past the connect timeout is unavailable, and marks its peer
// down, whatever the dialer says when it is cut short: a real dialer says only
// that it was cancelled, which is what a caller giving up looks like too.
func TestADialCutShortByTheConnectTimeoutIsUnavailable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		table := newTable(t, sim.New(sim.Config{Seed: 1}), peer.TableConfig{
			Dial: func(ctx context.Context, _ platform.Address) (platform.Conn, error) {
				<-ctx.Done()
				return nil, errors.Join(context.Canceled, errors.New("dial tcp: operation was canceled"))
			}})
		gone := table.Peer("gone")
		if _, err := askPages(t.Context(), gone, 0, 1); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("a dial cut short by the connect timeout = %v, want unavailable", err)
		}
		if !gone.Down() {
			t.Fatal("a dial cut short by the connect timeout did not mark its peer down")
		}
	})
}

// A reply that takes longer than the dead allowance to arrive is not a dead
// connection: its bytes are heard as they come, though the pongs queue behind
// them on the link.
func TestAReplyLongerThanTheDeadAllowanceIsHeardAsItComes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1, Network: sim.NetworkConfig{LinkBytesPerSecond: 32 << 10}})
		network := runtime.Network().Framed()
		pages := noisyPages{memoryPages{count: 64, pageSize: pageSize}}
		server, err := peer.NewServer(sim.WithRuntime(t.Context(), runtime), peer.ServerConfig{Network: network,
			Address: "source", PageSize: pageSize})
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		server.Serve("vm", map[string]peer.Pages{"ram0": pages})
		table := newTable(t, runtime, peer.TableConfig{Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
			return network.Dial(ctx, "destination", to)
		}})
		source := table.Peer("source")
		began := time.Now()
		answer, err := askPages(peer.WithStream(t.Context()), source, 0, 64)
		if err != nil {
			t.Fatalf("a reply of a quarter of a megabyte at 32 KiB/s: %v", err)
		}
		if took := time.Since(began); took < 8*time.Second {
			t.Fatalf("the reply took %v, want the eight seconds the link takes to carry it", took)
		}
		if len(answer.Payload) != 64*pageSize || !bytes.Equal(answer.Payload[63*pageSize:], pages.page(63)) {
			t.Fatalf("the reply carried %d bytes", len(answer.Payload))
		}
		if source.Down() || runtime.Probes()[peer.ProbeDeadConnection] != 0 {
			t.Fatalf("a slow reply was taken for a dead connection: probes %v", runtime.Probes())
		}
	})
}

// A connection of version 1 cannot be pinged: the release before closes a
// connection on any frame it does not know. It is found dead when it has owed
// a reply and heard nothing for longer than that release takes to give up on a
// request, thirty seconds, and the dead allowance after that. Silence while it
// owes nothing is not death.
func TestASilentPreviousReleaseIsFoundDeadAfterItsRequestTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1})
		listener, err := runtime.Network().Listen("previous")
		if err != nil {
			t.Fatal(err)
		}
		server := &previous.Server{Served: map[string]map[string]previous.Pages{
			"vm": {"ram0": memoryPages{count: 4, pageSize: pageSize}}}}
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		go server.Serve(ctx, listener)
		defer listener.Close()
		dialer := &countingDialer{network: runtime.Network()}
		source := newTable(t, runtime, peer.TableConfig{Dial: dialer.dial}).Peer("previous")
		if _, err := askPages(t.Context(), source, 0, 1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Second)
		began := time.Now()
		runtime.Network().Hold("previous", "destination", began.Add(2*time.Minute))
		if _, err := askPages(t.Context(), source, 1, 1); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("a request to a silent previous release = %v, want unavailable", err)
		}
		if took := time.Since(began); took < 34*time.Second || took > 35*time.Second {
			t.Fatalf("the silent connection was found dead after %v, want 34 to 35 seconds", took)
		}
		if !source.Down() {
			t.Fatal("a previous release whose connection died is not marked down")
		}
	})
}

// A down peer whose probe is answered INCOMPATIBLE is up: it is there, of a
// release this host cannot speak to, and probing it again would change nothing.
func TestAProbeAnsweredIncompatibleEndsTheProbing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1})
		server, err := peer.NewServer(sim.WithRuntime(t.Context(), runtime), peer.ServerConfig{
			Network: runtime.Network(), Address: "current", PageSize: pageSize})
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		dialer := &countingDialer{network: runtime.Network()}
		source := newTable(t, runtime, peer.TableConfig{Dial: dialer.dial, Versions: peer.Versions{Min: 3, Max: 4}}).Peer("current")
		// The first dial waits out its connect timeout behind a hold, which
		// marks the peer down.
		runtime.Network().HoldBoth("destination", "current", time.Now().Add(5*time.Second))
		if _, err := askPages(t.Context(), source, 0, 1); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("a request behind a hold = %v, want unavailable", err)
		}
		if !source.Down() {
			t.Fatal("a dial that timed out did not mark its peer down")
		}
		time.Sleep(time.Minute)
		status := source.Status()
		want := peer.IncompatibleError{Min: 1, Max: 2}
		if status.Down || status.Incompatible == nil || *status.Incompatible != want {
			t.Fatalf("a peer whose probe was answered incompatible reports %+v", status)
		}
		// The request's dial, and the probe's a second after it, which
		// waited out the rest of the hold and was answered.
		if dials := dialer.dials.Load(); dials != 2 {
			t.Fatalf("a minute of probing dialed %d times, want 2", dials)
		}
	})
}
