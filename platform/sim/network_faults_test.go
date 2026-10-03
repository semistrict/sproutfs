package sim_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// quiet is a network whose every hop takes exactly a millisecond, so a test
// can say when a frame arrives.
func quiet(config sim.NetworkConfig) sim.NetworkConfig {
	config.Latency, config.Jitter, config.ConnectLatency = time.Millisecond, time.Nanosecond, time.Millisecond
	return config
}

// sendFor sends one frame and reports how long its send took, to the
// microsecond: a hop's jitter of a nanosecond is below what a test means.
func sendFor(t *testing.T, conn platform.Conn, header string, payload []byte) time.Duration {
	t.Helper()
	began := time.Now()
	if err := conn.Send(t.Context(), platform.Frame{Header: []byte(header), Payload: platform.Bytes(payload),
		PayloadSize: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	return time.Since(began).Round(time.Microsecond)
}

// drain receives and drops every frame conn gets until it closes.
func drain(conn platform.Conn) {
	for {
		frame, err := conn.Receive(context.Background())
		if err != nil {
			return
		}
		_ = frame.Payload.Close()
	}
}

// A hold delays what is sent over a link rather than refusing it: the sender
// is told nothing, the bytes arrive once the hold ends, and a dial over the
// link waits for the end too.
func TestAHoldDelaysWhatItCarriesAndRefusesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Network: quiet(sim.NetworkConfig{})})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()

		began := time.Now()
		runtime.Network().Hold("client", "server", began.Add(30*time.Second))
		arrived := make(chan time.Duration, 1)
		go func() {
			_, _ = receiveFrame(t, server)
			arrived <- time.Since(began).Round(time.Microsecond)
		}()
		if took := sendFor(t, client, "held", nil); took != 30*time.Second+time.Millisecond {
			t.Fatalf("a send over a held link took %v, want the hold and a hop", took)
		}
		if got := <-arrived; got != 30*time.Second+time.Millisecond {
			t.Fatalf("the held frame arrived after %v, want 30.001s", got)
		}
		// The other direction is not held.
		if took := sendFor(t, server, "back", nil); took != time.Millisecond {
			t.Fatalf("a send over the unheld reverse link took %v", took)
		}

		runtime.Network().Hold("client", "server", time.Now().Add(10*time.Second))
		dialed := time.Now()
		second, err := runtime.Network().Dial(t.Context(), "client", "server")
		if err != nil {
			t.Fatalf("a dial over a held link: %v", err)
		}
		defer second.Close()
		if took := time.Since(dialed).Round(time.Microsecond); took != 10*time.Second+time.Millisecond {
			t.Fatalf("a dial over a held link took %v, want the hold and the connect", took)
		}
	})
}

// A dial to an address nobody listens at is refused at once, or, where the
// network says dials to the dead hang, waits for its caller to give up.
func TestADialToNobodyIsRefusedOrHangs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		refusing := sim.New(sim.Config{})
		if _, err := refusing.Network().Dial(t.Context(), "client", "nobody"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("a dial to nobody = %v, want ErrNotFound", err)
		}
		hanging := sim.New(sim.Config{Network: sim.NetworkConfig{HangDeadDials: true}})
		ctx, cancel := context.WithTimeout(t.Context(), 7*time.Second)
		defer cancel()
		began := time.Now()
		if _, err := hanging.Network().Dial(ctx, "client", "nobody"); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a hanging dial = %v, want its caller's deadline", err)
		}
		if took := time.Since(began); took != 7*time.Second {
			t.Fatalf("the hanging dial gave up after %v, want 7s", took)
		}
	})
}

// A heavy tail: about one hop in TailEvery takes up to TailLatency longer, and
// which hops those are is the seed's.
func TestAHeavyTailMakesAFewHopsLong(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 7, Network: quiet(sim.NetworkConfig{TailEvery: 100,
			TailLatency: 50 * time.Millisecond})})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()
		go drain(server)
		long, longest := 0, time.Duration(0)
		for i := range 2000 {
			took := sendFor(t, client, fmt.Sprint(i), nil)
			if took > time.Millisecond {
				long++
				longest = max(longest, took)
			}
		}
		if long != 22 || longest > 51*time.Millisecond {
			t.Fatalf("%d of 2000 hops were long, the longest %v; want 22 under 51ms", long, longest)
		}
	})
}

// A slow pair stays slow: every hop between the two hosts pays the same extra
// latency, both ways, for the whole run, and the hosts are named by HostOf.
func TestASlowPairStaysSlowBothWays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 3, Network: quiet(sim.NetworkConfig{SlowPairPerMille: 1000,
			SlowPairLatency: 100 * time.Millisecond,
			HostOf:          func(address platform.Address) string { return strings.TrimSuffix(string(address), "/pages") }})})
		listener, err := runtime.Network().Listen("server/pages")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		client, err := runtime.Network().Dial(t.Context(), "client", "server/pages")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		server, err := listener.Accept(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		go drain(client)
		go drain(server)
		first := sendFor(t, server, "a", nil)
		if first <= time.Millisecond || first > 101*time.Millisecond {
			t.Fatalf("a hop of a slow pair took %v", first)
		}
		for range 5 {
			if again := sendFor(t, server, "b", nil); again != first {
				t.Fatalf("the pair's hops took %v and then %v: a slow pair is slow by the same amount", first, again)
			}
		}
		if back := sendFor(t, client, "c", nil); back != first {
			t.Fatalf("the other way took %v, want %v", back, first)
		}
	})
}

// Bandwidth is shared by every connection between two hosts: two megabytes
// sent at once over two connections of one pair take twice as long as one, and
// a pair of other hosts is not slowed at all.
func TestBandwidthIsSharedPerHostPair(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Network: quiet(sim.NetworkConfig{LinkBytesPerSecond: 1 << 20,
			HostOf: func(address platform.Address) string { return strings.SplitN(string(address), "#", 2)[0] }})})
		listener, err := runtime.Network().Listen("server")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		go func() {
			for {
				conn, err := listener.Accept(context.Background())
				if err != nil {
					return
				}
				go func() {
					for {
						frame, err := conn.Receive(context.Background())
						if err != nil {
							return
						}
						_ = frame.Payload.Close()
					}
				}()
			}
		}()
		dial := func(from platform.Address) platform.Conn {
			conn, err := runtime.Network().Dial(t.Context(), from, "server")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			return conn
		}
		a, b, other := dial("client#a"), dial("client#b"), dial("elsewhere")
		megabyte := make([]byte, 1<<20)
		took := make(chan time.Duration, 3)
		for _, conn := range []platform.Conn{a, b, other} {
			go func() { took <- sendFor(t, conn, "bulk", megabyte) }()
		}
		times := []time.Duration{<-took, <-took, <-took}
		// One frame is its four-byte header and its megabyte, transmitted at a
		// megabyte a second, and a hop of a millisecond behind it.
		transmit := time.Duration(int64(1<<20+len("bulk")) * int64(time.Second) / (1 << 20))
		want := map[time.Duration]int{(transmit + time.Millisecond).Round(time.Microsecond): 2,
			(2*transmit + time.Millisecond).Round(time.Microsecond): 1}
		got := map[time.Duration]int{}
		for _, d := range times {
			got[d]++
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("three megabytes took %v, want one pair's two to share it and the other pair's alone", times)
		}
	})
}

// A bounded send buffer stalls a sender whose reader does not read, rather than
// growing: the frame that does not fit is sent once the reader takes one.
func TestABoundedSendBufferStallsTheSender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Network: quiet(sim.NetworkConfig{SendBufferBytes: 1000})})
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()
		frame := make([]byte, 600)
		sendFor(t, client, "first", frame)
		sent := make(chan struct{})
		go func() {
			sendFor(t, client, "second", frame)
			close(sent)
		}()
		time.Sleep(time.Hour)
		select {
		case <-sent:
			t.Fatal("a sender whose reader holds a full buffer was not stalled")
		default:
		}
		_, _ = receiveFrame(t, server)
		<-sent
		if header, _ := receiveFrame(t, server); string(header) != "second" {
			t.Fatalf("the stalled frame arrived as %q", header)
		}
	})
}

// seedsWithSite runs the sends of one connection under each seed with buggify on
// and reports, per seed, whether the site fired and what the run observed.
func seedsWithSite(t *testing.T, site string, observe func(t *testing.T, runtime *sim.Runtime) bool) {
	t.Helper()
	fired, observed := 0, 0
	for seed := uint64(1); seed <= 24; seed++ {
		synctest.Test(t, func(t *testing.T) {
			runtime := sim.New(sim.Config{Seed: seed, Buggify: true, Network: quiet(sim.NetworkConfig{})})
			saw := observe(t, runtime)
			did := runtime.FiredSites()[site] > 0
			if saw != did {
				t.Fatalf("seed %d: the site fired %v and the run saw it %v", seed, did, saw)
			}
			if did {
				fired++
			}
			if saw {
				observed++
			}
		})
	}
	if fired == 0 {
		t.Fatalf("%s fired under none of 24 seeds", site)
	}
}

// A connection closes at random under a frame, at the random-close site: the
// sender or the receiver learns of it, and nothing else does.
func TestAConnectionClosesAtRandomUnderAFrame(t *testing.T) {
	seedsWithSite(t, sim.SiteRandomClose, func(t *testing.T, runtime *sim.Runtime) bool {
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer server.Close()
		received := make(chan error, 1)
		go func() {
			for {
				frame, err := server.Receive(context.Background())
				if err != nil {
					received <- err
					return
				}
				_ = frame.Payload.Close()
			}
		}()
		for i := range 400 {
			if err := client.Send(t.Context(), platform.Frame{Header: []byte(fmt.Sprint(i))}); err != nil {
				if !errors.Is(err, platform.ErrDisconnected) {
					t.Fatalf("a send closed under it = %v, want ErrDisconnected", err)
				}
				return true
			}
		}
		_ = client.Close()
		err := <-received
		return !errors.Is(err, platform.ErrDisconnected) || runtime.FiredSites()[sim.SiteRandomClose] > 0
	})
}

// One bit of a frame's header flips on the way at the header bit-flip site, and
// nothing else of the frame changes.
func TestAHeaderBitFlipsOnTheWay(t *testing.T) {
	seedsWithSite(t, sim.SiteHeaderBitFlip, func(t *testing.T, runtime *sim.Runtime) bool {
		listener, client, server := connectedPair(t, runtime)
		defer listener.Close()
		defer client.Close()
		defer server.Close()
		flipped := false
		for i := range 400 {
			header := []byte(fmt.Sprintf("header %04d", i))
			// The random-close site may be on in the same run: a closed
			// connection ends the sends, and what was flipped before stands.
			if err := client.Send(t.Context(), platform.Frame{Header: header, Payload: platform.Bytes("payload"),
				PayloadSize: 7}); err != nil {
				break
			}
			frame, err := server.Receive(t.Context())
			if err != nil {
				break
			}
			got := frame.Header
			payload, err := io.ReadAll(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if string(payload) != "payload" {
				t.Fatal("a payload changed on a link that flips header bits")
			}
			switch difference := bitDifference(got, header); difference {
			case 0:
			case 1:
				flipped = true
			default:
				t.Fatalf("a header arrived %d bits from what was sent", difference)
			}
		}
		return flipped
	})
}

// Frames cross the framed network whole and in order though every write is cut
// into pieces and every read returns part of what has arrived: the real framer
// reassembles them.
func TestTheRealFramerReassemblesFragmentedStreams(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 5, Network: quiet(sim.NetworkConfig{})})
		network := runtime.Network().Framed()
		listener, err := network.Listen("server")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		client, err := network.Dial(t.Context(), "client", "server")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		server, err := listener.Accept(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		payloads := make([][]byte, 40)
		for i := range payloads {
			payloads[i] = bytes.Repeat([]byte{byte(i)}, i*i*97)
		}
		go func() {
			for i, payload := range payloads {
				sendFor(t, client, fmt.Sprintf("frame %d", i), payload)
			}
		}()
		for i, want := range payloads {
			frame, err := server.Receive(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(frame.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if string(frame.Header) != fmt.Sprintf("frame %d", i) || !bytes.Equal(got, want) {
				t.Fatalf("frame %d arrived as %q with %d bytes, want %d", i, frame.Header, len(got), len(want))
			}
		}
		pieces := 0
		for _, event := range runtime.Trace().Events() {
			if event.Operation == "write" && event.Outcome == "delivered" {
				pieces++
			}
		}
		if pieces != len(payloads) {
			t.Fatalf("the trace has %d writes, want one a frame", pieces)
		}
	})
}

// A stream that stops being read stalls its writer at its buffer, and a write
// deadline ends the wait, as the framer's cancelled send sets one.
func TestAStreamWriteStallsAtItsBufferUntilItsDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Network: quiet(sim.NetworkConfig{SendBufferBytes: 4096})})
		network := runtime.Network().Framed()
		listener, err := network.Listen("server")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		client, err := network.Dial(t.Context(), "client", "server")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		began := time.Now()
		err = client.Send(ctx, platform.Frame{Header: []byte("big"), Payload: platform.Bytes(make([]byte, 1<<20)),
			PayloadSize: 1 << 20})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a send nobody reads = %v, want its deadline", err)
		}
		if took := time.Since(began); took != 5*time.Second {
			t.Fatalf("the stalled send gave up after %v, want 5s", took)
		}
	})
}
