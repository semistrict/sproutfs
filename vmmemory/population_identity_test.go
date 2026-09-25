package vmmemory_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A related image can inherit adjacent pages from different volumes and
// checkpoint generations. Bytes remain indexed by logical page; identities are
// supplied through the same Locate boundary as a real volume.
type identifiedBacking struct {
	*backing
	identities []control.Identity
}

// Locate gives each page the identity the test chose, under that page's own
// number: a page is published whole, so its number is its object's.
func (b *identifiedBacking) Locate(_ context.Context, offset, length uint64) ([]control.Extent, error) {
	return extentsOf(offset, length, uint64(b.pageSize), func(cursor uint64) control.Identity {
		page := cursor / uint64(b.pageSize)
		id := b.identities[page]
		id.Page = page
		return id
	}), nil
}

func TestPopulationOrdersRelatedIdentitiesWithoutBlockingOtherPagers(t *testing.T) {
	identity := func(vm string, sequence uint64, volume string) control.Identity {
		return control.Identity{Ref: control.Ref{VM: vm, Sequence: sequence}, Volume: volume}
	}
	// Related populations can also cross storage pages while sharing the
	// same volume and generation. The lock order must stay consistent there.
	perPage := checkpoint.PageSize2MiB / pageSize
	pages := make([]control.Identity, perPage+1)
	for page := range pages {
		pages[page] = identity("ancestor", 1, "a")
		pages[page].Page = uint64(page / perPage)
	}
	for _, test := range []struct {
		name       string
		identities []control.Identity
		held       [2]uint64
	}{
		{"sequence", []control.Identity{identity("ancestor", 1, "b"), identity("ancestor", 1, "a"), identity("ancestor", 2, "a")}, [2]uint64{0, 2}},
		{"page", pages, [2]uint64{0, uint64(perPage)}},
		// Thirteen residents cross the sorter's small-input boundary. These
		// inherited volumes and generations expose an inconsistent ordering
		// that a two-page population alone cannot distinguish.
		{"volume", []control.Identity{
			identity("0", 2, "0"), identity("0", 1, "2"), identity("1", 2, "1"),
			identity("1", 2, "0"), identity("1", 3, "0"), identity("1", 1, "0"),
			identity("0", 3, "2"), identity("0", 2, "1"), identity("1", 1, "1"),
			identity("1", 3, "2"), identity("1", 1, "0"), identity("0", 2, "1"),
			identity("1", 1, "0"),
		}, [2]uint64{8, 12}},
	} {
		t.Run(test.name, func(t *testing.T) { populationOrder(t, test.identities, test.held) })
	}
}

func populationOrder(t *testing.T, identities []control.Identity, held [2]uint64) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		count := len(identities)
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: count + 4, LogicalPages: count * 4, DirtyPages: 4, ReadAheadPages: 1})
		cold := control.Identity{Ref: control.Ref{VM: "unrelated", Sequence: 1}, Volume: "a"}
		full := func() *identifiedBacking { return &identifiedBacking{f.newBacking(count), identities} }
		seedBacking := full()
		seed, seedMap := f.attach(seedBacking)
		for page := range uint64(count) {
			access(t, seed, seedMap, page, false)
		}
		otherFixture := newConfiguredFixture(t, vmmemory.Config{ResidentPages: count + 4, LogicalPages: count + 4, DirtyPages: 4, ReadAheadPages: 1})
		otherBacking := &identifiedBacking{otherFixture.newBacking(count), identities}
		other, otherMap := otherFixture.attach(otherBacking)
		if len(otherMap.pages) != 0 {
			t.Fatal("population reused pages from a different pager")
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		entered := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		release := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		released := [2]bool{}
		defer func() {
			for i := range release {
				if !released[i] {
					close(release[i])
				}
			}
		}()
		seedMap.onResolve = func(page uint64) {
			index := 0
			if page == held[1] {
				index = 1
			} else if page != held[0] {
				panic("unexpected held fault")
			}
			close(entered[index])
			<-release[index]
		}
		faults := make(chan error, 2)
		for _, page := range held {
			go func() { faults <- seed.Fault(ctx, page, false) }()
		}
		<-entered[0]
		<-entered[1]

		attach := func(b vmmemory.Backing) (*mapping, <-chan error) {
			m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
			f.a.mu.Lock()
			f.a.mappings = append(f.a.mappings, m)
			f.a.mu.Unlock()
			result := make(chan error, 1)
			done := make(chan struct{})
			var memoryRegion *vmmemory.MemoryRegion
			go func() {
				var err error
				memoryRegion, err = f.h.Attach(ctx, ram(b), m)
				result <- err
				close(done)
			}()
			t.Cleanup(func() {
				<-done
				f.a.mu.Lock()
				clear(m.pages)
				f.a.mu.Unlock()
				if memoryRegion != nil {
					if err := memoryRegion.Detach(context.Background()); err != nil {
						t.Error(err)
					}
				}
			})
			return m, result
		}
		partialIdentities := make([]control.Identity, count)
		for page := range partialIdentities {
			partialIdentities[page] = cold
		}
		for _, page := range held {
			partialIdentities[page] = identities[page]
		}
		partialBacking := &identifiedBacking{f.newBacking(count), partialIdentities}
		partial, partialDone := attach(partialBacking)
		synctest.Wait()
		completeBacking := full()
		complete, completeDone := attach(completeBacking)
		synctest.Wait()

		// The same identities in another pager own different resident locks.
		foreignDone := make(chan error, 1)
		go func() { foreignDone <- other.Fault(ctx, 0, false) }()
		synctest.Wait()
		select {
		case err := <-foreignDone:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("another pager waited behind held resident pages")
		}
		if got := access(t, other, otherMap, 0, false)[0]; got != 1 {
			t.Fatalf("independent pager bytes = %d", got)
		}

		// The partial plan and the complete plan share X and Y. Release Y while
		// X stays held, then X. Both must finish regardless of the additional
		// identities; waiting with different orders would leave a lock cycle.
		close(release[1])
		released[1] = true
		synctest.Wait()
		close(release[0])
		released[0] = true
		synctest.Wait()
		for _, done := range []<-chan error{partialDone, completeDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("related populations acquired resident locks in incompatible orders")
			}
		}
		for range 2 {
			if err := <-faults; err != nil {
				t.Fatal(err)
			}
		}
		for page := range uint64(count) {
			if got, ok := complete.pages[page]; !ok || got.slot != seedMap.pages[page].slot {
				t.Fatalf("complete population lost shared page %d", page)
			}
			if page != held[0] && page != held[1] {
				if _, ok := partial.pages[page]; ok {
					t.Fatal("population loaded or invented a cold identity")
				}
			} else if got, ok := partial.pages[page]; !ok || got.slot != seedMap.pages[page].slot {
				t.Fatalf("partial population lost shared page %d", page)
			}
		}
		if partialBacking.loads != 0 || completeBacking.loads != 0 {
			t.Fatal("population performed cold loads")
		}
	})
}
