package peer_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/peer"
	migratev1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/migrate/v1"
	peerv1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/peer/v1"
	"github.com/semistrict/sproutfs/peer/internal/previous"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"google.golang.org/protobuf/proto"
)

// memoryPages is one volume a test serves: pages 0 to count-1, each filled
// with its own number, every even page no checkpoint's.
type memoryPages struct {
	count    uint64
	pageSize int
}

func (p memoryPages) page(number uint64) []byte {
	return bytes.Repeat([]byte{byte(number + 1)}, p.pageSize)
}

func (p memoryPages) ReadResident(_ context.Context, number uint64, dst []byte) (bool, bool, error) {
	if number >= p.count {
		return false, false, peer.ErrPastEnd
	}
	copy(dst, p.page(number))
	return true, number%2 == 0, nil
}

func (p memoryPages) Resident() ([]uint64, error) {
	pages := make([]uint64, p.count)
	for i := range pages {
		pages[i] = uint64(i)
	}
	return pages, nil
}

func (p memoryPages) Unpublished() ([]uint64, error) {
	var pages []uint64
	for i := uint64(0); i < p.count; i += 2 {
		pages = append(pages, i)
	}
	return pages, nil
}

func (p memoryPages) PageSize() uint64 { return uint64(p.pageSize) }

// countingDialer dials over the simulated network from "destination" and
// counts the connections it opened.
type countingDialer struct {
	network *sim.Network
	dials   atomic.Int64
}

func (d *countingDialer) dial(ctx context.Context, to platform.Address) (platform.Conn, error) {
	d.dials.Add(1)
	return d.network.Dial(ctx, "destination", to)
}

// A host of this release fetches pages from a host of the release before,
// whose server cannot read a hello and closes the connection it came on: the
// destination dials again without one and speaks version 1, one request at a
// time, which is all the previous release speaks.
func TestThisReleaseFetchesPagesFromThePreviousRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1})
		listener, err := runtime.Network().Listen("previous")
		if err != nil {
			t.Fatal(err)
		}
		pages := memoryPages{count: 3, pageSize: pageSize}
		server := &previous.Server{Served: map[string]map[string]previous.Pages{"vm": {"ram0": pages}}}
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		go server.Serve(ctx, listener)
		defer listener.Close()

		dialer := &countingDialer{network: runtime.Network()}
		source := peer.NewSource(peer.SourceConfig{Peer: "previous", VM: "vm", Volume: "ram0", PageSize: pageSize,
			MaxConnections: 1, MaxRuns: 16, Dial: dialer.dial})
		defer source.Close()
		answer, err := source.Pages(t.Context(), 0, 4)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(answer.Present, []byte{0b0111}) || !bytes.Equal(answer.Dirty, []byte{0b0101}) ||
			!bytes.Equal(answer.Payload, slices.Concat(pages.page(0), pages.page(1), pages.page(2))) {
			t.Fatalf("pages from the previous release: present %08b dirty %08b, %d bytes",
				answer.Present, answer.Dirty, len(answer.Payload))
		}
		runs, err := source.Resident(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(runs, []peer.Run{{First: 0, Count: 3}}) {
			t.Fatalf("the previous release listed %v, want one run of three pages", runs)
		}
		if err := source.Claim(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !server.Claimed("vm") {
			t.Fatal("the claim did not reach the previous release")
		}
		// The hello's connection, closed unanswered, and the one the
		// destination spoke version 1 over, which it kept for the rest.
		if dials := dialer.dials.Load(); dials != 2 {
			t.Fatalf("the destination dialed %d times, want 2", dials)
		}
	})
}

// A host of the release before fetches pages from a host of this one: it
// sends no hello, and this release's server answers it in version 1, the only
// version it reads.
func TestThePreviousReleaseFetchesPagesFromThisRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1})
		server, err := peer.NewServer(t.Context(), peer.ServerConfig{Network: runtime.Network(), Address: "current",
			PageSize: pageSize})
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		pages := memoryPages{count: 3, pageSize: pageSize}
		server.Serve("vm", map[string]peer.Pages{"ram0": pages})
		conn, err := runtime.Network().Dial(t.Context(), "previous", "current")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		client := &previous.Client{Conn: conn}
		response, payload, err := client.Pages(t.Context(), "vm", "ram0", 1, 2, pageSize)
		if err != nil {
			t.Fatal(err)
		}
		if response.GetStatus() != migratev1.Status_STATUS_OK || !bytes.Equal(response.GetPresent(), []byte{0b11}) ||
			!bytes.Equal(response.GetDirty(), []byte{0b10}) || !bytes.Equal(payload, slices.Concat(pages.page(1), pages.page(2))) {
			t.Fatalf("this release answered the previous one %v with %d bytes", response, len(payload))
		}
		listing, err := client.Resident(t.Context(), "vm", "ram0", 0)
		if err != nil {
			t.Fatal(err)
		}
		if listing.GetStatus() != migratev1.Status_STATUS_OK || len(listing.GetRuns()) != 1 || listing.GetRuns()[0].GetCount() != 3 {
			t.Fatalf("this release listed %v for the previous one", listing)
		}
		claim, err := client.Claim(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		if claim.GetStatus() != migratev1.Status_STATUS_OK {
			t.Fatalf("this release answered the previous one's claim %v", claim.GetStatus())
		}
		if stats := server.Stats(); stats.Requests != 1 || stats.Served != 2 || stats.Listings != 1 {
			t.Fatalf("the server counted %+v", stats)
		}
	})
}

// sayHello sends one hello stating a range over a fresh connection and returns
// the answer.
func sayHello(t *testing.T, conn platform.Conn, minimum, maximum uint32) *peerv1.HelloReply {
	t.Helper()
	frame, err := wire.Encode(wire.Outgoing{Version: 2, Message: peerv1.Hello_builder{
		MinVersion: proto.Uint32(minimum), MaxVersion: proto.Uint32(maximum)}.Build()})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(t.Context(), frame); err != nil {
		t.Fatal(err)
	}
	received, err := conn.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	incoming, err := wire.Decode(received)
	if err != nil {
		t.Fatal(err)
	}
	defer incoming.Payload.Close()
	reply := new(peerv1.HelloReply)
	if err := incoming.UnmarshalTo(reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

// A hello is answered with the newest version both ends speak, and a range
// this host shares nothing with is answered INCOMPATIBLE with this host's own
// range, after which the connection closes. Neither is a server that is down.
func TestAHelloIsAnsweredWithTheNewestSharedVersionOrIncompatible(t *testing.T) {
	for _, test := range []struct {
		name              string
		minimum, maximum  uint32
		status            peerv1.Status
		version           uint32
		serverMin, serMax uint32
	}{
		{"a later release that still speaks this one", 2, 5, peerv1.Status_STATUS_OK, 2, 1, 2},
		{"this release", 1, 2, peerv1.Status_STATUS_OK, 2, 1, 2},
		{"a release two ahead", 3, 4, peerv1.Status_STATUS_INCOMPATIBLE, 0, 1, 2},
		{"a range with nothing in it", 2, 1, peerv1.Status_STATUS_INCOMPATIBLE, 0, 1, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := sim.New(sim.Config{Seed: 1})
				server, err := peer.NewServer(t.Context(), peer.ServerConfig{Network: runtime.Network(),
					Address: "current", PageSize: pageSize})
				if err != nil {
					t.Fatal(err)
				}
				defer server.Close()
				conn, err := runtime.Network().Dial(t.Context(), "other", "current")
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				reply := sayHello(t, conn, test.minimum, test.maximum)
				if reply.GetStatus() != test.status || reply.GetVersion() != test.version ||
					reply.GetMinVersion() != test.serverMin || reply.GetMaxVersion() != test.serMax {
					t.Fatalf("a hello for %d to %d was answered %v", test.minimum, test.maximum, reply)
				}
				incompatible := int64(0)
				if test.status == peerv1.Status_STATUS_INCOMPATIBLE {
					incompatible = 1
					ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
					defer cancel()
					if _, err := conn.Receive(ctx); !errors.Is(err, platform.ErrDisconnected) {
						t.Fatalf("after INCOMPATIBLE the connection gave %v, want it closed", err)
					}
				}
				if got := server.Stats().Incompatible; got != incompatible {
					t.Fatalf("the server counted %d incompatible peers, want %d", got, incompatible)
				}
			})
		})
	}
}

// A destination of a release two ahead is told INCOMPATIBLE and the range this
// host speaks, and asks nothing further.
func TestADestinationTwoReleasesAheadIsToldItIsIncompatible(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1})
		server, err := peer.NewServer(t.Context(), peer.ServerConfig{Network: runtime.Network(), Address: "current",
			PageSize: pageSize})
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()
		server.Serve("vm", map[string]peer.Pages{"ram0": memoryPages{count: 1, pageSize: pageSize}})
		dialer := &countingDialer{network: runtime.Network()}
		source := peer.NewSource(peer.SourceConfig{Peer: "current", VM: "vm", Volume: "ram0", PageSize: pageSize,
			MaxConnections: 1, MaxRuns: 16, Dial: dialer.dial, Versions: peer.Versions{Min: 3, Max: 4}})
		defer source.Close()
		_, err = source.Pages(t.Context(), 0, 1)
		var incompatible *peer.IncompatibleError
		if !errors.As(err, &incompatible) || *incompatible != (peer.IncompatibleError{Min: 1, Max: 2}) {
			t.Fatalf("a request from two releases ahead = %v, want INCOMPATIBLE naming 1 to 2", err)
		}
		if dials, requests := dialer.dials.Load(), server.Stats().Requests; dials != 1 || requests != 0 {
			t.Fatalf("an incompatible destination dialed %d times and made %d requests, want 1 and 0", dials, requests)
		}
	})
}

// A destination that no longer speaks version 1 does not fall back to it: a
// server that closes its hello unanswered is one it cannot use.
func TestADestinationPastVersionOneDoesNotFallBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1})
		listener, err := runtime.Network().Listen("previous")
		if err != nil {
			t.Fatal(err)
		}
		server := &previous.Server{Served: map[string]map[string]previous.Pages{}}
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		go server.Serve(ctx, listener)
		defer listener.Close()
		dialer := &countingDialer{network: runtime.Network()}
		source := peer.NewSource(peer.SourceConfig{Peer: "previous", VM: "vm", Volume: "ram0", PageSize: pageSize,
			MaxConnections: 1, MaxRuns: 16, Dial: dialer.dial, Versions: peer.Versions{Min: 2, Max: 3}})
		defer source.Close()
		if _, err := source.Pages(t.Context(), 0, 1); !errors.Is(err, platform.ErrDisconnected) {
			t.Fatalf("a destination past version 1 asking the previous release = %v, want the closed hello", err)
		}
		if dials := dialer.dials.Load(); dials != 1 {
			t.Fatalf("it dialed %d times, want 1", dials)
		}
	})
}
