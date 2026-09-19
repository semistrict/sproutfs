package checkpoint_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// heldSource fills every page and blocks after the first one of each
// publication, so a publication that has started writing stays started.
type heldSource struct {
	m       *model
	release chan struct{}
	mu      sync.Mutex
	seen    map[string]int
}

func (s *heldSource) ReadPage(ctx context.Context, volume string, page uint64, dst []byte) error {
	if err := s.m.ReadPage(ctx, volume, page, dst); err != nil {
		return err
	}
	s.mu.Lock()
	s.seen[volume]++
	first := s.seen[volume] == 1
	s.mu.Unlock()
	if first {
		return nil
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// A publication holds a part builder of up to PartBytes while it writes. The
// budget for them is the host's, not the checkpoint's: a manager whose VMs all
// become dirty at once must not multiply one publication's memory by the number
// of them.
func TestLivePartBuildersAreBoundedHostWide(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const publications = 6
		const bound = 2
		objects := sim.New(sim.Config{}).ObjectStore()
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, PartBytes: 1,
			Concurrency: 4, MaxBuilders: bound})
		sizes := map[string]uint64{}
		names := make([]string, publications)
		for index := range publications {
			names[index] = "volume" + string(rune('a'+index))
			sizes[names[index]] = 2 * checkpoint.PageSize2MiB
		}
		root, err := store.Root(t.Context(), control.Ref{VM: "budget", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(volumes2MiB(sizes))
		source := &heldSource{m: m, release: make(chan struct{}), seen: make(map[string]int)}

		var wait sync.WaitGroup
		errs := make([]error, publications)
		for index := range publications {
			p := store.Begin(root, control.Ref{VM: "budget", Sequence: uint64(index) + 2})
			for page := range uint64(2) {
				for sector := range uint32(sectorsPerPage) {
					m.dirty(p, names[index], page, sector, sectorData(names[index], page, sector))
				}
			}
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, errs[index] = p.Commit(t.Context(), source)
			}()
		}
		synctest.Wait()
		if live := checkpoint.LiveBuilders(store); live != bound {
			t.Fatalf("%d publications hold %d live part builders, want the budget of %d",
				publications, live, bound)
		}
		close(source.release)
		wait.Wait()
		for index, err := range errs {
			if err != nil {
				t.Fatalf("publication %d: %v", index, err)
			}
		}
		if live := checkpoint.LiveBuilders(store); live != 0 {
			t.Fatalf("%d part builders outlived their publications", live)
		}
	})
}
