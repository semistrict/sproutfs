package sim_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// networkFailures is every site at which the network refuses or ends what a
// caller asked of it.
var networkFailures = []string{sim.SiteListenFails, sim.SiteDialRefused, sim.SiteAcceptFails, sim.SiteSendTimedOut,
	sim.SiteReceiveTimedOut}

// Under Buggify the network refuses a listen, refuses a dial to an address
// that listens, fails an accept with a connection waiting, and ends a
// connection as the kernel ends one that timed out, over framed links and over
// the byte streams a host's TCP adapter frames. What each leaves is what a
// real one leaves: a refused listen listens nowhere, a refused dial connects
// nothing, a failed accept leaves its connection for the next accept, and a
// connection that failed under one end fails at the other. Across the seeds
// every site fires.
func TestTheNetworkRefusesAndEndsWhatItIsAskedAtRandom(t *testing.T) {
	fired := map[string]bool{}
	for seed := uint64(1); seed <= 48; seed++ {
		for _, framed := range []bool{false, true} {
			synctest.Test(t, func(t *testing.T) {
				runtime := sim.New(sim.Config{Seed: seed, Buggify: true, Network: quiet(sim.NetworkConfig{})})
				var network platform.Network = runtime.Network()
				if framed {
					network = runtime.Network().Framed()
				}
				exchangeUnderFailures(t, runtime, network)
				for site := range runtime.FiredSites() {
					fired[site] = true
				}
			})
		}
	}
	for _, site := range networkFailures {
		if !fired[site] {
			t.Errorf("%s never fired", site)
		}
	}
}

// exchangeUnderFailures listens, dials, accepts and exchanges frames, at
// address after address, checking what each failure left.
func exchangeUnderFailures(t *testing.T, runtime *sim.Runtime, network platform.Network) {
	ctx := t.Context()
	for round := range 40 {
		address := platform.Address(fmt.Sprintf("server-%d", round))
		listener, err := network.Listen(address)
		for err != nil {
			// Nothing listens at an address whose listen was refused.
			if _, dialed := network.Dial(ctx, "client", address); !errors.Is(dialed, platform.ErrNotFound) {
				t.Fatalf("a dial to an address whose listen was refused (%v): %v, want not found", err, dialed)
			}
			listener, err = network.Listen(address)
		}
		client, err := network.Dial(ctx, "client", address)
		if err != nil {
			// A refused dial left no connection to accept.
			waited, cancel := context.WithTimeout(ctx, time.Second)
			conn, accepted := listener.Accept(waited)
			cancel()
			if !errors.Is(accepted, context.DeadlineExceeded) {
				t.Fatalf("round %d: a dial refused with %v left a connection to accept: %v, %v", round, err, conn,
					accepted)
			}
			_ = listener.Close()
			continue
		}
		server, err := listener.Accept(ctx)
		for err != nil {
			// The connection waits for the next accept.
			server, err = listener.Accept(ctx)
		}
		exchange(t, runtime, round, client, server)
		_ = client.Close()
		_ = server.Close()
		_ = listener.Close()
	}
}

// exchange sends frames both ways until twenty have crossed or the connection
// fails, and checks that a connection that failed under one end fails at the
// other. A frame whose prefix a flipped bit lengthened leaves its reader
// waiting for bytes that never come, which the peer server's pings end: that
// is not this test's, so a receive waits a minute at most where one did.
func exchange(t *testing.T, runtime *sim.Runtime, round int, client, server platform.Conn) {
	ctx := t.Context()
	for i := range 20 {
		from, to := client, server
		if i%2 == 1 {
			from, to = server, client
		}
		header := []byte(fmt.Sprintf("round %d frame %d", round, i))
		if err := from.Send(ctx, platform.Frame{Header: header}); err != nil {
			endsAtTheOtherEnd(t, round, err, to)
			return
		}
		waited, cancel := context.WithTimeout(ctx, time.Minute)
		frame, err := to.Receive(waited)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && runtime.FiredSites()[sim.SiteStreamBitFlip] > 0 {
			return
		}
		if err != nil {
			endsAtTheOtherEnd(t, round, err, from)
			return
		}
		_ = frame.Payload.Close()
	}
}

// endsAtTheOtherEnd checks that a connection one end saw fail fails at the
// other end too, rather than leaving it waiting.
func endsAtTheOtherEnd(t *testing.T, round int, err error, other platform.Conn) {
	t.Helper()
	if !errors.Is(err, platform.ErrDisconnected) && !errors.Is(err, platform.ErrUnavailable) &&
		!errors.Is(err, platform.ErrMessageTooLarge) {
		t.Fatalf("round %d: a connection failed with %v, which no connection's end is", round, err)
	}
	waited, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for {
		frame, received := other.Receive(waited)
		if errors.Is(received, context.DeadlineExceeded) {
			t.Fatalf("round %d: one end failed with %v and the other waits on", round, err)
		}
		if received != nil {
			return
		}
		// A frame sent before the failure may still arrive.
		_ = frame.Payload.Close()
	}
}
