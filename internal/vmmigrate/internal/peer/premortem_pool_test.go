package peer_test

import (
	"sync"
	"testing"

	migratev1 "github.com/semistrict/sproutfs/internal/vmmigrate/internal/gen/sproutfs/migrate/v1"
	"github.com/semistrict/sproutfs/internal/vmmigrate/internal/wire"
	"google.golang.org/protobuf/proto"
)

// The pre-mortem of the GCE soak's fan-out. A source bounds the connections one
// destination host may hold at once, over every memory region of every VM it is
// serving that host. A memory region that pooled the connections of a burst it has
// finished holds that bound against the memory regions still asking — and what they
// ask for is the pages no checkpoint holds, which exist nowhere else, so they
// ask for ever.

// TestPremortemABurstGivesItsConnectionsBackWhenItIsOver: a memory region's pool exists
// so that concurrent requests pipeline. When the burst is over the memory region keeps
// one connection and gives the rest back, so the source's per-peer bound is a
// queue the next memory region gets to the front of rather than one this memory region holds
// for the life of its receive.
func TestPremortemABurstGivesItsConnectionsBackWhenItIsOver(t *testing.T) {
	// Every request waits until the whole burst has arrived, so all four are in
	// flight at once and the memory region really does open four connections.
	var mu sync.Mutex
	arrived := 0
	all := make(chan struct{})
	s := &script{answer: func(wire.Incoming) (proto.Message, []byte, error) {
		mu.Lock()
		arrived++
		if arrived == 4 {
			close(all)
		}
		mu.Unlock()
		<-all
		status := migratev1.Status_STATUS_BUSY
		return migratev1.PageResponse_builder{Status: &status,
			PageSize: proto.Uint32(pageSize)}.Build(), nil, nil
	}}
	source := s.source(4)
	var wg sync.WaitGroup
	for page := range uint64(4) {
		wg.Go(func() {
			if _, err := source.Pages(t.Context(), page, 1); err != nil {
				t.Errorf("page %d: %v", page, err)
			}
		})
	}
	wg.Wait()
	if s.dials.Load() != 4 {
		t.Fatalf("four concurrent requests opened %d connections, want one each", s.dials.Load())
	}
	// The burst is over. Three of the four go back to the source, which is what
	// lets another memory region of this host be served at all.
	if dropped := s.closes.Load(); dropped != 3 {
		t.Fatalf("a finished burst gave %d of its 4 connections back, want all but the one it keeps", dropped)
	}
	// And the one it kept is reused, so a memory region asking one page at a time
	// still pays one socket rather than one per page.
	if _, err := source.Pages(t.Context(), 0, 1); err != nil {
		t.Fatal(err)
	}
	if s.dials.Load() != 4 {
		t.Fatalf("the request after the burst dialled again: %d connections", s.dials.Load())
	}
	source.Close()
	if open := s.dials.Load() - s.closes.Load(); open != 0 {
		t.Fatalf("closing the source left %d connections open", open)
	}
}
