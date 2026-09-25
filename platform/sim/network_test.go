package sim_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/bits"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

func TestNetworkTransfersFramedPayloadUsingVirtualTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		runtime.Network().SetLink("client", "server", sim.LinkConfig{Latency: 2 * time.Second})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()

		payload := []byte("raw payload bytes")
		start := time.Now()
		err := client.Send(t.Context(), platform.Frame{
			Header:      []byte("protobuf header"),
			Payload:     bytes.NewReader(payload),
			PayloadSize: int64(len(payload)),
		})
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < 2*time.Second {
			t.Fatalf("Send advanced virtual time by %v, want at least 2s", elapsed)
		}
		payload[0] = 'X'

		_, body := receiveFrame(t, server)
		if got, want := string(body), "raw payload bytes"; got != want {
			t.Fatalf("payload = %q, want %q", got, want)
		}
	})
}

func TestNetworkFaultControls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()

		runtime.Network().DropNext("client", "server", 1)
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("lost")}); err != nil {
			t.Fatal(err)
		}
		receiveCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if _, err := server.Receive(receiveCtx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Receive after drop error = %v, want DeadlineExceeded", err)
		}
		assertTraceEvent(t, runtime.Trace().Events(), "network", "send", "dropped")

		runtime.Network().DuplicateNext("client", "server", 1)
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("twice")}); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			assertReceivedHeader(t, server, "twice")
		}
		assertTraceEvent(t, runtime.Trace().Events(), "network", "send", "duplicated")

		runtime.Network().Partition("client", "server")
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("blocked")}); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("Send while partitioned error = %v, want ErrUnavailable", err)
		}
		assertTraceEvent(t, runtime.Trace().Events(), "network", "send", "blocked")
	})
}

func TestNetworkDirectionalPartitionAndHealing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()

		runtime.Network().Partition("client", "server")
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("blocked")}); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("forward Send error = %v, want ErrUnavailable", err)
		}
		if err := server.Send(t.Context(), platform.Frame{Header: []byte("reverse")}); err != nil {
			t.Fatal(err)
		}
		assertReceivedHeader(t, client, "reverse")

		runtime.Network().Heal("client", "server")
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("healed")}); err != nil {
			t.Fatal(err)
		}
		assertReceivedHeader(t, server, "healed")

		runtime.Network().PartitionBoth("client", "server")
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("a")}); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("forward Send after PartitionBoth error = %v, want ErrUnavailable", err)
		}
		if err := server.Send(t.Context(), platform.Frame{Header: []byte("b")}); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("reverse Send after PartitionBoth error = %v, want ErrUnavailable", err)
		}
		runtime.Network().HealBoth("client", "server")
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("a")}); err != nil {
			t.Fatal(err)
		}
		if err := server.Send(t.Context(), platform.Frame{Header: []byte("b")}); err != nil {
			t.Fatal(err)
		}
		assertReceivedHeader(t, server, "a")
		assertReceivedHeader(t, client, "b")
	})
}

func TestNetworkCorruptsExactlyOneBitOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 77})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()

		header := []byte("protobuf-header")
		payload := []byte("raw-payload")
		frame := platform.Frame{Header: header, Payload: bytes.NewReader(payload), PayloadSize: int64(len(payload))}
		runtime.Trace().Reset()
		runtime.Network().CorruptNext("client", "server", 1)
		if err := client.Send(t.Context(), frame); err != nil {
			t.Fatal(err)
		}
		gotHeader, gotPayload := receiveFrame(t, server)
		if difference := bitDifference(append(slicesClone(header), payload...), append(gotHeader, gotPayload...)); difference != 1 {
			t.Fatalf("corrupted frame differs by %d bits, want 1", difference)
		}
		assertTraceEvent(t, runtime.Trace().Events(), "network", "send", "corrupted")

		if err := client.Send(t.Context(), frame); err != nil {
			t.Fatal(err)
		}
		gotHeader, gotPayload = receiveFrame(t, server)
		if !bytes.Equal(gotHeader, header) || !bytes.Equal(gotPayload, payload) {
			t.Fatalf("second frame = (%q, %q), want clean frame", gotHeader, gotPayload)
		}
	})
}

func TestNetworkOneShotDelayPreservesConcurrentSendFIFO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()

		runtime.Network().DelayNext("client", "server", 10*time.Second)
		firstEntered := make(chan struct{})
		firstResult := make(chan error, 1)
		start := time.Now()
		go func() {
			close(firstEntered)
			firstResult <- client.Send(t.Context(), platform.Frame{Header: []byte("first")})
		}()
		receiveWithin(t, firstEntered)
		synctest.Wait()
		secondResult := make(chan error, 1)
		go func() {
			secondResult <- client.Send(t.Context(), platform.Frame{Header: []byte("second")})
		}()
		if err := receiveWithin(t, firstResult); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed < 10*time.Second {
			t.Fatalf("first Send elapsed = %v, want at least 10s virtual time", elapsed)
		}
		if err := receiveWithin(t, secondResult); err != nil {
			t.Fatal(err)
		}
		assertReceivedHeader(t, server, "first")
		assertReceivedHeader(t, server, "second")
	})
}

func TestNetworkSendCancelsUnderInboxBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Network: sim.NetworkConfig{InboxSize: 1}})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()

		if err := client.Send(t.Context(), platform.Frame{Header: []byte("fills inbox")}); err != nil {
			t.Fatal(err)
		}
		runtime.Trace().Reset()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := client.Send(ctx, platform.Frame{Header: []byte("blocked")}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("backpressured Send error = %v, want DeadlineExceeded", err)
		}
		assertTraceEvent(t, runtime.Trace().Events(), "network", "send", "canceled")
		assertReceivedHeader(t, server, "fills inbox")
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("after drain")}); err != nil {
			t.Fatal(err)
		}
		assertReceivedHeader(t, server, "after drain")
	})
}

func connectedPair(t *testing.T, runtime *sim.Runtime) (platform.Listener, platform.Conn, platform.Conn) {
	t.Helper()
	listener, err := runtime.Network().Listen("server")
	if err != nil {
		t.Fatal(err)
	}
	client, err := runtime.Network().Dial(t.Context(), "client", "server")
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	server, err := listener.Accept(ctx)
	if err != nil {
		_ = client.Close()
		_ = listener.Close()
		t.Fatal(err)
	}
	return listener, client, server
}

func assertReceivedHeader(t *testing.T, conn platform.Conn, want string) {
	t.Helper()
	header, _ := receiveFrame(t, conn)
	if string(header) != want {
		t.Fatalf("received header = %q, want %q", header, want)
	}
}

func receiveFrame(t *testing.T, conn platform.Conn) ([]byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	frame, err := conn.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frame.Payload.Close()
	payload, err := io.ReadAll(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), frame.Header...), payload
}

func slicesClone(value []byte) []byte { return append([]byte(nil), value...) }

func bitDifference(left, right []byte) int {
	if len(left) != len(right) {
		return -1
	}
	difference := 0
	for index := range left {
		difference += bits.OnesCount8(left[index] ^ right[index])
	}
	return difference
}
