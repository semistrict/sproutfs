package peer_test

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/peer"
	migratev1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/migrate/v1"
	peerv1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/peer/v1"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/proto"
)

const pageSize = 4096

// script is a peer server that answers every request with what one test wrote
// for it, and counts the connections it was asked for and lost.
type script struct {
	answer  func(incoming wire.Incoming) (proto.Message, []byte, error)
	dials   atomic.Int64
	closes  atomic.Int64
	fail    atomic.Bool
	answers atomic.Int64
}

func (s *script) dial(context.Context, platform.Address) (platform.Conn, error) {
	s.dials.Add(1)
	return &scriptedConn{script: s, pending: make(chan platform.ReceivedFrame, 64), closed: make(chan struct{})}, nil
}

// peer is the host this script answers for, as a table of this end's own sees
// it. The table is closed when the test ends.
func (s *script) peer(t *testing.T) *peer.Peer {
	t.Helper()
	table, err := peer.NewTable(context.Background(), peer.TableConfig{Dial: s.dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = table.Close() })
	return table.Peer("source")
}

// askPages asks for count pages from first of the script's VM.
func askPages(ctx context.Context, p *peer.Peer, first uint64, count int) (peer.Answer, error) {
	return p.Pages(ctx, peer.PageRequest{VM: "vm", Volume: "ram0", First: first, Count: count, PageSize: pageSize})
}

type scriptedConn struct {
	script    *script
	pending   chan platform.ReceivedFrame
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *scriptedConn) Send(_ context.Context, frame platform.Frame) error {
	incoming, err := wire.Decode(platform.ReceivedFrame{Header: frame.Header,
		Payload: io.NopCloser(bytes.NewReader(nil))})
	if err != nil {
		return err
	}
	if err := incoming.Payload.Close(); err != nil {
		return err
	}
	if c.script.fail.Load() {
		return errors.New("the connection broke")
	}
	var message proto.Message
	var payload []byte
	if incoming.Message.MessageIs(new(peerv1.Hello)) {
		// The script is a server of this release: every connection opens with
		// a hello it answers at the newest version both speak.
		status := peerv1.Status_STATUS_OK
		message = peerv1.HelloReply_builder{Status: &status, Version: proto.Uint32(peer.NewestVersion),
			MinVersion: proto.Uint32(peer.OldestVersion), MaxVersion: proto.Uint32(peer.NewestVersion)}.Build()
	} else {
		c.script.answers.Add(1)
		if message, payload, err = c.script.answer(incoming); err != nil {
			return err
		}
	}
	outgoing := wire.Outgoing{Version: incoming.Version, RequestID: incoming.RequestID,
		InReplyTo: incoming.RequestID, Message: message}
	if len(payload) > 0 {
		outgoing.Payload = wire.Payload{Body: bytes.NewReader(payload), Size: int64(len(payload)),
			Algorithm: wire.ChecksumCRC32C,
			Checksum:  wire.EncodeCRC32C(crc32.Checksum(payload, crc32.MakeTable(crc32.Castagnoli)))}
	}
	reply, err := wire.Encode(outgoing)
	if err != nil {
		return err
	}
	c.pending <- platform.ReceivedFrame{Header: reply.Header,
		Payload: io.NopCloser(bytes.NewReader(payload)), PayloadSize: reply.PayloadSize}
	return nil
}

func (c *scriptedConn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	select {
	case frame := <-c.pending:
		return frame, nil
	case <-c.closed:
		return platform.ReceivedFrame{}, platform.ErrDisconnected
	case <-ctx.Done():
		return platform.ReceivedFrame{}, context.Cause(ctx)
	}
}

func (c *scriptedConn) LocalAddress() platform.Address  { return "destination" }
func (c *scriptedConn) RemoteAddress() platform.Address { return "source" }
func (c *scriptedConn) Close() error {
	c.closeOnce.Do(func() {
		c.script.closes.Add(1)
		close(c.closed)
	})
	return nil
}

func pageReply(t *testing.T, count int, present, dirty []byte, pages []byte) (proto.Message, []byte) {
	t.Helper()
	encoded, err := blob.Encode(t.Context(), pages)
	if err != nil {
		t.Fatal(err)
	}
	status := migratev1.Status_STATUS_OK
	return migratev1.PageResponse_builder{Status: &status, PageSize: proto.Uint32(pageSize),
		Count: proto.Uint32(uint32(count)), PayloadFormat: proto.Uint32(1),
		Present: present, Dirty: dirty}.Build(), encoded
}

// busyReply is what a server of this release answers a request over its
// peer's budget with.
func busyReply() proto.Message {
	class := peerv1.Class_CLASS_FAULT
	return peerv1.Busy_builder{Class: &class, HeldBytes: proto.Uint64(6 << 20), BudgetBytes: proto.Uint64(8 << 20),
		AskedBytes: proto.Uint64(4 << 20)}.Build()
}

// A served page comes back with its bytes, the bit that says the peer served
// it, and the bit that says no checkpoint holds it.
func TestPagesReportWhatThePeerServed(t *testing.T) {
	first := bytes.Repeat([]byte{0xa5}, pageSize)
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		message, payload := pageReply(t, 2, []byte{0b01}, []byte{0b01}, first)
		return message, payload, nil
	}}
	answer, err := askPages(t.Context(), s.peer(t), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if answer.Busy != nil {
		t.Fatal("a peer that served a page reported itself busy")
	}
	if len(answer.Present) != 1 || answer.Present[0] != 0b01 || answer.Dirty[0] != 0b01 {
		t.Fatalf("bitmaps present=%v dirty=%v", answer.Present, answer.Dirty)
	}
	if !bytes.Equal(answer.Payload, first) {
		t.Fatalf("the payload is %d bytes, want the page's %d", len(answer.Payload), len(first))
	}
}

// A peer at its budget for this host served nothing and said how busy it is,
// which is an answer rather than an error: the caller decides whether to wait.
func TestPagesReportABusyPeerAsAnAnswerThatSaysHowBusy(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		return busyReply(), nil, nil
	}}
	answer, err := askPages(t.Context(), s.peer(t), 0, 9)
	if err != nil {
		t.Fatal(err)
	}
	want := peer.BusyError{Class: peer.Fault, Held: 6 << 20, Budget: 8 << 20, Asked: 4 << 20}
	if answer.Busy == nil || *answer.Busy != want || len(answer.Present) != 2 || len(answer.Dirty) != 2 {
		t.Fatalf("a busy reply came back as %+v, busy %+v", answer, answer.Busy)
	}
	if answer.Present[0]|answer.Present[1]|answer.Dirty[0]|answer.Dirty[1] != 0 {
		t.Fatal("a busy peer was recorded as having served a page")
	}
}

// A listing a busy peer refuses is ErrBusy, with how busy.
func TestAListingABusyPeerRefusesSaysHowBusy(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		return busyReply(), nil, nil
	}}
	_, err := s.peer(t).Resident(t.Context(), "vm", "ram0", pageSize, 2)
	var busy *peer.BusyError
	if !errors.As(err, &busy) || !errors.Is(err, peer.ErrBusy) || busy.Held != 6<<20 {
		t.Fatalf("a listing a busy peer refused: %v", err)
	}
}

// A peer that says it does not serve this VM is not going to serve it again.
func TestAnUnknownVMIsNotServed(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		status := migratev1.Status_STATUS_UNKNOWN_VM
		return migratev1.PageResponse_builder{Status: &status}.Build(), nil, nil
	}}
	if _, err := askPages(t.Context(), s.peer(t), 0, 1); !errors.Is(err, peer.ErrNotServed) {
		t.Fatalf("a request for a VM the peer dropped: %v", err)
	}
}

// A claim of a VM the peer no longer holds is ErrNotServed.
func TestAClaimOfAnUnknownVMIsNotServed(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		status := migratev1.Status_STATUS_UNKNOWN_VM
		return migratev1.ClaimResponse_builder{Status: &status}.Build(), nil, nil
	}}
	if err := s.peer(t).Claim(t.Context(), "vm"); !errors.Is(err, peer.ErrNotServed) {
		t.Fatalf("a claim of a VM the peer dropped: %v", err)
	}
}

// A peer that answers for fewer pages than were asked, as one whose replies
// are capped does, leaves the rest clear: the caller reads them elsewhere or
// asks again.
func TestAnAnswerForFewerPagesLeavesTheRestClear(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{MaxPagesPerRequest: 2})
		destination := s.table(t, "destination", peer.TableConfig{})
		answer, err := askPages(t.Context(), destination, 0, 4)
		if err != nil {
			t.Fatal(err)
		}
		pages := memoryPages{count: 64, pageSize: pageSize}
		if !bytes.Equal(answer.Present, []byte{0b0011}) || !bytes.Equal(answer.Dirty, []byte{0b0001}) ||
			!bytes.Equal(answer.Payload, append(pages.page(0), pages.page(1)...)) {
			t.Fatalf("a reply capped at two pages of four: present %08b dirty %08b, %d bytes",
				answer.Present, answer.Dirty, len(answer.Payload))
		}
		close(s.gate.open)
	})
}

// A reply for more pages than were asked for is not this protocol's.
func TestPagesRejectAReplyThatOverrunsTheRequest(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		message, payload := pageReply(t, 4, []byte{0}, []byte{0}, nil)
		return message, payload, nil
	}}
	if _, err := askPages(t.Context(), s.peer(t), 0, 2); !errors.Is(err, wire.ErrMalformedFrame) {
		t.Fatalf("a reply naming more pages than were asked for: %v", err)
	}
}

// A listing is walked a bounded number of runs at a time, and every reply's
// runs join the one the caller gets.
func TestResidentWalksTheWholeListing(t *testing.T) {
	var replies atomic.Int64
	var mu sync.Mutex
	var asked []uint64
	s := &script{answer: func(incoming wire.Incoming) (proto.Message, []byte, error) {
		request := new(migratev1.ResidentRequest)
		if err := incoming.UnmarshalTo(request); err != nil {
			return nil, nil, err
		}
		mu.Lock()
		asked = append(asked, request.GetFirstPage())
		mu.Unlock()
		status := migratev1.Status_STATUS_OK
		more := replies.Add(1) == 1
		run := migratev1.PageRun_builder{FirstPage: proto.Uint64(uint64(replies.Load()-1) * 4),
			Count: proto.Uint32(4)}.Build()
		return migratev1.ResidentResponse_builder{Status: &status, PageSize: proto.Uint32(pageSize),
			Runs: []*migratev1.PageRun{run}, More: proto.Bool(more)}.Build(), nil, nil
	}}
	runs, err := s.peer(t).Resident(t.Context(), "vm", "ram0", pageSize, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []peer.Run{{First: 0, Count: 4}, {First: 4, Count: 4}}
	if !slices.Equal(runs, want) {
		t.Fatalf("the listing is %+v, want %+v", runs, want)
	}
	// Each request after the first starts past the last run listed.
	if !slices.Equal(asked, []uint64{0, 4}) {
		t.Fatalf("the listing was asked from pages %v, want 0 then 4", asked)
	}
}

// A peer whose pages are another size cannot be read at all, however
// well-formed its reply is.
func TestResidentRefusesAPeerOfAnotherPageSize(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		status := migratev1.Status_STATUS_OK
		return migratev1.ResidentResponse_builder{Status: &status,
			PageSize: proto.Uint32(pageSize * 2)}.Build(), nil, nil
	}}
	if _, err := s.peer(t).Resident(t.Context(), "vm", "ram0", pageSize, 2); !errors.Is(err, peer.ErrPageSize) {
		t.Fatalf("a peer serving pages of another size: %v", err)
	}
}

// A connection that answered is reused, one that failed is dropped and the
// next request dials again, and closing the table drops what is left — so a
// host's requests to one peer cost a socket per class rather than one per page.
func TestConnectionsAreReusedUntilOneFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
			return busyReply(), nil, nil
		}}
		table, err := peer.NewTable(t.Context(), peer.TableConfig{Dial: s.dial})
		if err != nil {
			t.Fatal(err)
		}
		source := table.Peer("source")
		for range 3 {
			if _, err := askPages(t.Context(), source, 0, 1); err != nil {
				t.Fatal(err)
			}
		}
		if s.dials.Load() != 1 || s.closes.Load() != 0 {
			t.Fatalf("three requests over %d connections, %d of them dropped", s.dials.Load(), s.closes.Load())
		}
		s.fail.Store(true)
		if _, err := askPages(t.Context(), source, 0, 1); err == nil {
			t.Fatal("a broken connection answered")
		}
		synctest.Wait()
		if s.dials.Load() != 1 || s.closes.Load() != 1 {
			t.Fatalf("after a failure: %d connections, %d dropped", s.dials.Load(), s.closes.Load())
		}
		s.fail.Store(false)
		if _, err := askPages(t.Context(), source, 0, 1); err != nil {
			t.Fatal(err)
		}
		if s.dials.Load() != 2 {
			t.Fatalf("a failed connection was not replaced: %d dials", s.dials.Load())
		}
		if err := table.Close(); err != nil {
			t.Fatal(err)
		}
		if s.closes.Load() != 2 {
			t.Fatalf("closing the table left %d connections open", s.dials.Load()-s.closes.Load())
		}
		if got := s.answers.Load(); got != 4 {
			t.Fatalf("the peer answered %d requests, want 4", got)
		}
	})
}

// Every request is offered to the run's admitter before it takes a connection,
// so a controlled run orders the decision to ask the peer against the
// cancellation that would stop it. An open connection is taken without
// dialing, so without this there is nothing between the two.
func TestEveryRequestIsAdmittedBeforeItTakesAConnection(t *testing.T) {
	var sends atomic.Int64
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		sends.Add(1)
		return busyReply(), nil, nil
	}}
	refused := errors.New("the stream was closed")
	var admitted []string
	ctx := peer.WithAdmission(t.Context(), func(_ context.Context, memoryRegion string) error {
		admitted = append(admitted, memoryRegion)
		if len(admitted) == 1 {
			return nil
		}
		return refused
	})
	source := s.peer(t)
	if _, err := askPages(ctx, source, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := askPages(ctx, source, 0, 1); !errors.Is(err, refused) {
		t.Fatalf("the second request was not refused by the admitter: %v", err)
	}
	if want := []string{"vm/ram0", "vm/ram0"}; !slices.Equal(admitted, want) {
		t.Fatalf("the admitter saw %v, want %v", admitted, want)
	}
	// The connection the first request left open is the one a refused request
	// would have reached the wire over.
	if s.dials.Load() != 1 || sends.Load() != 1 {
		t.Fatalf("a refused request reached the wire: %d dials, %d sends", s.dials.Load(), sends.Load())
	}
}

// An already-canceled caller still reaches the admitter. That is what makes the
// decision the controller's rather than the Go runtime's: the request is
// refused because the run said so at a point it chose, not because a select
// happened to see a canceled context first.
func TestAnAlreadyCanceledRequestIsStillAdmitted(t *testing.T) {
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		t.Error("a canceled request reached the peer")
		return nil, nil, errors.New("unreachable")
	}}
	closed := errors.New("the stream was closed")
	var admitted int
	ctx, cancel := context.WithCancelCause(t.Context())
	ctx = peer.WithAdmission(ctx, func(inner context.Context, _ string) error {
		admitted++
		return context.Cause(inner)
	})
	cancel(closed)
	if _, err := s.peer(t).Resident(ctx, "vm", "ram0", pageSize, 2); !errors.Is(err, closed) {
		t.Fatalf("a canceled listing returned %v", err)
	}
	if admitted != 1 {
		t.Fatalf("the admitter saw %d requests, want 1", admitted)
	}
	if s.dials.Load() != 0 {
		t.Fatalf("a canceled listing dialed %d connections", s.dials.Load())
	}
}

// A request that waited for room is offered to the admitter again when it is
// woken, before it looks for a connection. The room one request gives back —
// the hello its dial was waiting on, or the background budget its reply frees —
// wakes every request waiting for it beside the one that gave it back, and
// without a point a controlled run orders, the Go scheduler chose which of
// them took which connection and went first on it: one run in a few hundred of
// the scheduled world sent a destination's guest fault and its stream's
// listing of resident pages in either order.
//
// The second request is held at its second admission while the first, whose
// dial woke it or whose reply freed its room, goes on alone.
func TestARequestWokenFromAWaitForRoomIsAdmittedAgain(t *testing.T) {
	for _, wait := range []struct {
		name string
		// class is the class both requests are made in, and background the
		// host's background budget.
		class      func(context.Context) context.Context
		background int64
	}{
		{name: "for the dial another request began", class: func(ctx context.Context) context.Context { return ctx }},
		{name: "for the background budget", class: peer.WithStream, background: pageSize},
	} {
		t.Run(wait.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newServing(t, peer.ServerConfig{})
				destination := s.table(t, "destination", peer.TableConfig{BackgroundBytes: wait.background})
				// Nothing the source sends arrives for a second: the first
				// request's dial is heard, and its reply arrives, only then.
				heard := time.Now().Add(time.Second)
				s.runtime.Network().Hold("source", "destination", heard)
				var mu sync.Mutex
				var admitted, sent []string
				held := make(chan struct{})
				request := func(name string, page uint64) <-chan error {
					ctx := peer.WithAdmission(wait.class(t.Context()), func(ctx context.Context, _ string) error {
						mu.Lock()
						admitted = append(admitted, name)
						again := slices.Index(admitted, name) != len(admitted)-1
						mu.Unlock()
						if name == "second" && again {
							<-held
						}
						return context.Cause(ctx)
					})
					ctx = peer.WithSent(ctx, func() {
						mu.Lock()
						defer mu.Unlock()
						sent = append(sent, name)
					})
					done := make(chan error, 1)
					go func() {
						_, err := askPages(ctx, destination, page, 1)
						done <- err
					}()
					synctest.Wait()
					return done
				}
				first := request("first", 0)
				second := request("second", 1)
				mu.Lock()
				if want := []string{"first", "second"}; !slices.Equal(admitted, want) || len(sent) != 0 {
					t.Fatalf("before the dial was heard: admitted %v and sent %v, want %v and nothing", admitted, sent, want)
				}
				mu.Unlock()
				time.Sleep(time.Until(heard) + 10*time.Millisecond)
				synctest.Wait()
				mu.Lock()
				if want := []string{"first", "second", "second"}; !slices.Equal(admitted, want) ||
					!slices.Equal(sent, []string{"first"}) {
					t.Fatalf("once the first request went on: admitted %v and sent %v, want %v and only the first",
						admitted, sent, want)
				}
				mu.Unlock()
				if err := <-first; err != nil {
					t.Fatal(err)
				}
				close(held)
				if err := <-second; err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(sent, []string{"first", "second"}) {
					t.Fatalf("sent %v, want the first and then the second", sent)
				}
			})
		})
	}
}

// A guest fault goes over connections the post-copy stream never uses. With
// every bulk connection's requests stuck at the peer, a fault still gets its
// answer at once, over a fault connection of its own.
func TestAGuestFaultNeverWaitsBehindTheStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		page := bytes.Repeat([]byte{0x5a}, pageSize)
		gate := make(chan struct{})
		var entered atomic.Int64
		s := &script{answer: func(incoming wire.Incoming) (proto.Message, []byte, error) {
			request := new(migratev1.PageRequest)
			if err := incoming.UnmarshalTo(request); err != nil {
				return nil, nil, err
			}
			if request.GetFirstPage() >= 100 {
				entered.Add(1)
				<-gate
			}
			message, payload := pageReply(t, 1, []byte{0b1}, []byte{0}, page)
			return message, payload, nil
		}}
		// One request a connection, so each of the stream's requests holds a bulk
		// connection of its own.
		table, err := peer.NewTable(t.Context(), peer.TableConfig{Dial: s.dial, InFlight: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer table.Close()
		source := table.Peer("source")
		stream := peer.WithStream(t.Context())
		bulk := peer.DefaultConnections.BulkRead
		streamed := make(chan error, bulk)
		var once sync.Once
		release := func() { once.Do(func() { close(gate) }) }
		defer release()
		for first := range uint64(bulk) {
			go func() {
				_, err := askPages(stream, source, 100+first, 1)
				streamed <- err
			}()
		}
		synctest.Wait()
		if got := entered.Load(); got != int64(bulk) {
			t.Fatalf("%d stream requests reached the peer, want one held on each of the %d bulk connections", got, bulk)
		}
		faulted := make(chan error, 1)
		go func() {
			_, err := askPages(t.Context(), source, 0, 1)
			faulted <- err
		}()
		synctest.Wait()
		select {
		case err := <-faulted:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("a guest fault waited behind the stream")
		}
		release()
		for range bulk {
			if err := <-streamed; err != nil {
				t.Fatal(err)
			}
		}
	})
}

// A class's requests to one peer never hold more connections than the class
// may, however many are made at once: the rest wait for a slot rather than
// dial. A peer host is one budget at the server, so connections beyond it would
// buy nothing.
func TestABurstHoldsNoMoreConnectionsThanItsClassMay(t *testing.T) {
	t.Run("answered", testABurstThatIsAnswered)
	t.Run("while dials are slow", testABurstWhileDialsAreSlow)
	t.Run("two a connection", testABurstOfTwoAConnection)
}

// A connection carries as many requests as its peer allows and no more: three
// held requests where each connection may carry two take two connections.
func testABurstOfTwoAConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hold := make(chan struct{})
		s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
			<-hold
			message, payload := pageReply(t, 1, []byte{0b1}, []byte{0}, bytes.Repeat([]byte{1}, pageSize))
			return message, payload, nil
		}}
		table, err := peer.NewTable(t.Context(), peer.TableConfig{Dial: s.dial, InFlight: 2})
		if err != nil {
			t.Fatal(err)
		}
		defer table.Close()
		source := table.Peer("source")
		var wg sync.WaitGroup
		for page := range uint64(3) {
			wg.Go(func() {
				if _, err := askPages(t.Context(), source, page, 1); err != nil {
					t.Errorf("page %d: %v", page, err)
				}
			})
			synctest.Wait()
		}
		if dials := s.dials.Load(); dials != 2 {
			t.Fatalf("three requests two a connection dialed %d connections, want 2", dials)
		}
		close(hold)
		wg.Wait()
	})
}

// A dial in progress counts against the class as a connection does: a burst
// that finds every connection busy and one dial under way waits for it rather
// than dialing past the class.
func testABurstWhileDialsAreSlow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hold := make(chan struct{})
		s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
			<-hold
			message, payload := pageReply(t, 1, []byte{0b1}, []byte{0}, bytes.Repeat([]byte{1}, pageSize))
			return message, payload, nil
		}}
		slow := make(chan struct{})
		var dials atomic.Int64
		table, err := peer.NewTable(t.Context(), peer.TableConfig{InFlight: 1,
			Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
				if dials.Add(1) > 1 {
					<-slow
				}
				return s.dial(ctx, to)
			}})
		if err != nil {
			t.Fatal(err)
		}
		defer table.Close()
		source := table.Peer("source")
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, err := askPages(t.Context(), source, 0, 1); err != nil {
				t.Error(err)
			}
		})
		synctest.Wait()
		for page := range uint64(4) {
			wg.Go(func() {
				if _, err := askPages(t.Context(), source, 1+page, 1); err != nil {
					t.Errorf("page %d: %v", 1+page, err)
				}
			})
		}
		synctest.Wait()
		if got := dials.Load(); got != int64(peer.DefaultConnections.Fault) {
			t.Fatalf("a burst behind a slow dial dialed %d times, want the class's %d", got, peer.DefaultConnections.Fault)
		}
		close(slow)
		close(hold)
		wg.Wait()
	})
}

func testABurstThatIsAnswered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		arrived := 0
		all := make(chan struct{})
		s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
			mu.Lock()
			arrived++
			if arrived == 2 {
				close(all)
			}
			mu.Unlock()
			<-all
			return busyReply(), nil, nil
		}}
		table, err := peer.NewTable(t.Context(), peer.TableConfig{Dial: s.dial, InFlight: 1})
		if err != nil {
			t.Fatal(err)
		}
		defer table.Close()
		source := table.Peer("source")
		var wg sync.WaitGroup
		for page := range uint64(6) {
			wg.Go(func() {
				if _, err := askPages(t.Context(), source, page, 1); err != nil {
					t.Errorf("page %d: %v", page, err)
				}
			})
		}
		wg.Wait()
		if dials := s.dials.Load(); dials != int64(peer.DefaultConnections.Fault) {
			t.Fatalf("six concurrent faults opened %d connections, want the class's %d", dials, peer.DefaultConnections.Fault)
		}
		if status := table.Status(); len(status) != 1 || status[0].Connections != (peer.Connections{Fault: 2}) ||
			status[0].Version != peer.NewestVersion {
			t.Fatalf("the table reports %+v", status)
		}
	})
}

// An answer's bytes are its own. A page no encoder shrinks goes as a raw
// envelope, which decodes to a slice of the reply's buffer; that buffer goes
// back to the pool as the answer is returned, and the next reply is read into
// it. Released buffers are poisoned in this package's tests, so an answer that
// shares its reply's buffer holds the poison on every run, whichever buffer the
// pool hands the next reply.
func TestAnAnswersPagesOutliveItsReplysBuffer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pages := noisyPages{memoryPages{count: 4, pageSize: pageSize}}
		s := newServing(t, peer.ServerConfig{})
		s.server.Serve("noise", map[string]peer.Pages{"ram0": pages})
		destination := s.table(t, "destination", peer.TableConfig{})
		answer, err := destination.Pages(t.Context(), peer.PageRequest{VM: "noise", Volume: "ram0", First: 0,
			Count: 1, PageSize: pageSize})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(answer.Payload, pages.page(0)) {
			t.Fatal("an answer's page changed under it once its reply's buffer was released")
		}
	})
}
