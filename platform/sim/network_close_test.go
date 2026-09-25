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

func TestListenerCloseDisconnectsUnacceptedClients(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		listener, accepted, server := connectedPair(t, runtime)
		defer accepted.Close()
		defer server.Close()
		queued, err := runtime.Network().Dial(t.Context(), "queued", listener.Address())
		if err != nil {
			t.Fatal(err)
		}
		defer queued.Close()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := queued.Receive(ctx); !errors.Is(err, platform.ErrDisconnected) {
			t.Fatalf("unaccepted client outlived its listener: %v", err)
		}
		if _, err := listener.Accept(ctx); !errors.Is(err, platform.ErrClosed) {
			t.Fatalf("closed listener accepted a queued connection: %v", err)
		}
		// An accepted connection belongs to its server, not to the listener.
		if err := accepted.Send(ctx, platform.Frame{Header: []byte("still connected")}); err != nil {
			t.Fatal(err)
		}
		assertReceivedHeader(t, server, "still connected")
	})
}

func TestDialCannotJoinAListenerClosedDuringConnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Network: sim.NetworkConfig{ConnectLatency: time.Second}})
		listener, err := runtime.Network().Listen("server")
		if err != nil {
			t.Fatal(err)
		}
		connected := make(chan error, 1)
		go func() {
			conn, err := runtime.Network().Dial(t.Context(), "client", "server")
			if conn != nil {
				_ = conn.Close()
			}
			connected <- err
		}()
		synctest.Wait() // Dial is sleeping in its modeled connection latency.
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		// Reusing the address must not redirect the in-flight dial to a new server.
		replacement, err := runtime.Network().Listen("server")
		if err != nil {
			t.Fatal(err)
		}
		defer replacement.Close()
		if err := <-connected; !errors.Is(err, platform.ErrClosed) {
			t.Fatalf("dial joined the retired listener: %v", err)
		}
	})
}

func TestSendRejectsAConnectionClosedBeforeDelivery(t *testing.T) {
	for attempt := range 20 {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := sim.New(sim.Config{})
				listener, client, server := connectedPair(t, runtime)
				defer listener.Close()
				defer client.Close()
				result := make(chan error, 1)
				go func() { result <- client.Send(t.Context(), platform.Frame{Header: []byte("late frame")}) }()
				synctest.Wait() // Send is waiting for its transmission latency.
				if err := server.Close(); err != nil {
					t.Fatal(err)
				}
				if err := <-result; !errors.Is(err, platform.ErrDisconnected) {
					t.Fatalf("Send after disconnection = %v, want ErrDisconnected", err)
				}
			})
		})
	}
}

// A canceled caller has not begun a connection attempt. In particular, whether
// a connection-pool select chose ctx.Done or a free slot must not consume a
// different simulator ID or fault before returning the same cancellation.
func TestCanceledDialDoesNotEnterTheNetwork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		waits := 0
		runtime := sim.New(sim.Config{Wait: func(context.Context, string, time.Duration, time.Duration) error { waits++; return nil }})
		listener, err := runtime.Network().Listen("server")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		conn, err := runtime.Network().Dial(ctx, "client", "server")
		if conn != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled dial: %v %v", conn, err)
		}
		if waits != 0 {
			t.Fatalf("canceled dial entered %d scheduling points", waits)
		}
		for _, event := range runtime.Trace().Events() {
			if event.Operation == "dial" {
				t.Fatal("canceled dial consumed a network attempt")
			}
		}
	})
}
