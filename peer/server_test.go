package peer_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// gatedPages holds every read until its gate opens, which is a request whose
// reply the server has not built yet: what it reserved of its peer's budget is
// held all that time.
type gatedPages struct {
	memoryPages
	open    chan struct{}
	entered chan uint64
}

func (g gatedPages) ReadResident(ctx context.Context, number uint64, dst []byte) (bool, bool, error) {
	select {
	case g.entered <- number:
	default:
	}
	select {
	case <-g.open:
	case <-ctx.Done():
		return false, false, context.Cause(ctx)
	}
	return g.memoryPages.ReadResident(ctx, number, dst)
}

// serving is one server of this release on a simulated network, serving a VM
// "vm" whose "ram0" is pages and whose "disk" is held behind a gate.
type serving struct {
	runtime *sim.Runtime
	server  *peer.Server
	gate    gatedPages
}

func newServing(t *testing.T, config peer.ServerConfig) *serving {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 1})
	config.Network, config.Address = runtime.Network(), "source"
	if config.PageSize == 0 {
		config.PageSize = pageSize
	}
	server, err := peer.NewServer(sim.WithRuntime(t.Context(), runtime), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	gate := gatedPages{memoryPages: memoryPages{count: 64, pageSize: pageSize}, open: make(chan struct{}),
		entered: make(chan uint64, 64)}
	server.Serve("vm", map[string]peer.Pages{"ram0": memoryPages{count: 64, pageSize: pageSize}, "disk": gate})
	return &serving{runtime: runtime, server: server, gate: gate}
}

// table is a destination host's table of peers that reaches this server from
// host, closed when the test ends.
func (s *serving) table(t *testing.T, host platform.Address, config peer.TableConfig) *peer.Peer {
	t.Helper()
	config.Dial = func(ctx context.Context, to platform.Address) (platform.Conn, error) {
		return s.runtime.Network().Dial(ctx, host, to)
	}
	table, err := peer.NewTable(sim.WithRuntime(t.Context(), s.runtime), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = table.Close() })
	return table.Peer("source")
}

func disk(ctx context.Context, p *peer.Peer, first uint64, count int) (peer.Answer, error) {
	return p.Pages(ctx, peer.PageRequest{VM: "vm", Volume: "disk", First: first, Count: count, PageSize: pageSize})
}

// A peer's bulk class at its budget at the server takes nothing from its fault
// class: a fault is answered at once while the stream's requests hold every
// byte the stream may.
func TestAFaultIsAnsweredWhileTheBulkClassIsAtItsBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{Budgets: peer.Budgets{Fault: 4 * pageSize, BulkRead: 4 * pageSize,
			BulkWrite: 4 * pageSize}})
		destination := s.table(t, "destination", peer.TableConfig{})
		stream := peer.WithStream(t.Context())
		streamed := make(chan error, 1)
		go func() {
			_, err := disk(stream, destination, 0, 4)
			streamed <- err
		}()
		<-s.gate.entered
		answer, err := askPages(t.Context(), destination, 0, 4)
		if err != nil {
			t.Fatal(err)
		}
		if answer.Busy != nil || len(answer.Payload) != 4*pageSize {
			t.Fatalf("a fault beside a stream at its budget came back busy %+v with %d bytes", answer.Busy, len(answer.Payload))
		}
		close(s.gate.open)
		if err := <-streamed; err != nil {
			t.Fatal(err)
		}
		if refused := s.server.Stats().Refused; refused != 0 {
			t.Fatalf("the server refused %d requests", refused)
		}
	})
}

// Requests that come to exactly a class's budget all go at once: neither end
// holds back the one that fills it.
func TestRequestsThatFillTheBudgetExactlyAllGo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{Budgets: peer.Budgets{Fault: 4 * pageSize, BulkRead: 4 * pageSize,
			BulkWrite: 4 * pageSize}})
		destination := s.table(t, "destination", peer.TableConfig{})
		answered := make(chan peer.Answer, 2)
		for first := range uint64(2) {
			go func() {
				answer, err := disk(t.Context(), destination, 2*first, 2)
				if err != nil {
					t.Error(err)
				}
				answered <- answer
			}()
		}
		<-s.gate.entered
		<-s.gate.entered
		close(s.gate.open)
		for range 2 {
			if answer := <-answered; answer.Busy != nil {
				t.Fatalf("a request that filled the budget exactly came back busy %+v", answer.Busy)
			}
		}
		if waited := s.runtime.Probes()[peer.ProbeWaitedForBudget]; waited != 0 {
			t.Fatalf("%d requests waited for a budget they fitted in", waited)
		}
	})
}

// A request that would take its peer's class past the budget is answered BUSY,
// with what the class holds, what it may, and what was asked; the connection it
// came on stays open and serves the next request.
func TestARequestOverItsBudgetIsAnsweredBusyAndTheConnectionStays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{Budgets: peer.Budgets{Fault: 4 * pageSize, BulkRead: 4 * pageSize,
			BulkWrite: 4 * pageSize}})
		// Two hosts' worth of tables from one address: the first holds the
		// budget, and the second does not know it, so only the server can say.
		holder := s.table(t, "destination", peer.TableConfig{})
		held := make(chan error, 1)
		go func() {
			_, err := disk(t.Context(), holder, 0, 4)
			held <- err
		}()
		<-s.gate.entered
		other := s.table(t, "destination", peer.TableConfig{})
		answer, err := askPages(t.Context(), other, 0, 2)
		if err != nil {
			t.Fatal(err)
		}
		want := peer.BusyError{Class: peer.Fault, Held: 4 * pageSize, Budget: 4 * pageSize, Asked: 2 * pageSize}
		if answer.Busy == nil || *answer.Busy != want {
			t.Fatalf("a request over the budget came back busy %+v, want %+v", answer.Busy, want)
		}
		close(s.gate.open)
		if err := <-held; err != nil {
			t.Fatal(err)
		}
		answer, err = askPages(t.Context(), other, 0, 2)
		if err != nil || answer.Busy != nil || len(answer.Payload) != 2*pageSize {
			t.Fatalf("after the budget came back: busy %+v, %d bytes, %v", answer.Busy, len(answer.Payload), err)
		}
		if refused := s.server.Stats().Refused; refused != 1 {
			t.Fatalf("the server refused %d requests, want 1", refused)
		}
	})
}

// This end asks a peer for no more than the budget the peer's hello gave it:
// a request past it waits here for room, and is never answered BUSY.
func TestARequestWaitsHereForTheBudgetThePeerGave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{Budgets: peer.Budgets{Fault: 4 * pageSize, BulkRead: 4 * pageSize,
			BulkWrite: 4 * pageSize}})
		destination := s.table(t, "destination", peer.TableConfig{})
		ctx := sim.WithRuntime(t.Context(), s.runtime)
		var wg sync.WaitGroup
		results := make(chan peer.Answer, 2)
		for first := range uint64(2) {
			wg.Go(func() {
				answer, err := disk(ctx, destination, first*4, 4)
				if err != nil {
					t.Error(err)
				}
				results <- answer
			})
		}
		<-s.gate.entered
		synctest.Wait()
		close(s.gate.open)
		wg.Wait()
		close(results)
		for answer := range results {
			if answer.Busy != nil || len(answer.Payload) != 4*pageSize {
				t.Fatalf("a request this end held back came back busy %+v with %d bytes", answer.Busy, len(answer.Payload))
			}
		}
		if refused := s.server.Stats().Refused; refused != 0 {
			t.Fatalf("the server refused %d requests", refused)
		}
		if s.runtime.Probes()[peer.ProbeWaitedForBudget] == 0 {
			t.Fatal("no request waited here for the budget")
		}
	})
}

// A connection carries several requests at once, answered concurrently, and
// their replies leave in the order the requests came: a fast request behind a
// slow one on the same connection is read and answered while the slow one is
// still being served, and its reply follows the slow one's.
func TestRepliesLeaveInTheOrderTheirRequestsCame(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{})
		destination := s.table(t, "destination", peer.TableConfig{Connections: peer.Connections{Fault: 1, BulkRead: 1,
			BulkWrite: 1}})
		order := make(chan string, 2)
		slow := make(chan error, 1)
		go func() {
			_, err := disk(t.Context(), destination, 0, 1)
			order <- "slow"
			slow <- err
		}()
		<-s.gate.entered
		fast := make(chan error, 1)
		go func() {
			_, err := askPages(t.Context(), destination, 0, 1)
			order <- "fast"
			fast <- err
		}()
		// A second of simulated time is a thousand round trips: the fast
		// request has been read and answered, and its reply waits for the slow
		// one's.
		time.Sleep(time.Second)
		if requests := s.server.Stats().Requests; requests != 2 {
			t.Fatalf("the server had read %d requests, want both", requests)
		}
		select {
		case <-fast:
			t.Fatal("a reply left before the reply to the request in front of it")
		default:
		}
		close(s.gate.open)
		if err := <-slow; err != nil {
			t.Fatal(err)
		}
		if err := <-fast; err != nil {
			t.Fatal(err)
		}
		if first, second := <-order, <-order; first != "slow" || second != "fast" {
			t.Fatalf("the replies left %s then %s", first, second)
		}
	})
}

// A request whose caller gave up holds its place until its reply arrives,
// because the server holds the request's bytes until then; the late reply is
// read, dropped, and gives the place back.
func TestALateReplyGivesBackWhatItsRequestHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newServing(t, peer.ServerConfig{Budgets: peer.Budgets{Fault: 4 * pageSize, BulkRead: 4 * pageSize,
			BulkWrite: 4 * pageSize}})
		destination := s.table(t, "destination", peer.TableConfig{})
		ctx, cancel := context.WithCancel(sim.WithRuntime(t.Context(), s.runtime))
		gaveUp := make(chan error, 1)
		go func() {
			_, err := disk(ctx, destination, 0, 4)
			gaveUp <- err
		}()
		<-s.gate.entered
		cancel()
		if err := <-gaveUp; !errors.Is(err, context.Canceled) {
			t.Fatalf("a request whose caller gave up: %v", err)
		}
		close(s.gate.open)
		time.Sleep(time.Second)
		if s.runtime.Probes()[peer.ProbeLateReply] != 1 {
			t.Fatalf("the late reply was not read: probes %v", s.runtime.Probes())
		}
		answer, err := disk(t.Context(), destination, 0, 4)
		if err != nil || answer.Busy != nil || len(answer.Payload) != 4*pageSize {
			t.Fatalf("after a late reply: busy %+v, %d bytes, %v", answer.Busy, len(answer.Payload), err)
		}
	})
}
