package vmmemory_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

type delayedLocate struct {
	vmmemory.Backing
	entered chan struct{}
	release chan struct{}
}

type unavailableLocate struct {
	vmmemory.Backing
	ready bool
}

func (b *unavailableLocate) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	if !b.ready {
		return nil, errInjected
	}
	return b.Backing.Locate(ctx, offset, length)
}

func TestEmptyResidencyAttachesWithoutReadingColdMetadata(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 4, 4)
		backing := &unavailableLocate{Backing: f.newBacking(4)}
		r, m := f.attach(backing)
		if len(m.pages) != 0 {
			t.Fatal("empty residency invented eager data mappings")
		}
		backing.ready = true
		if got := access(t, r, m, 0, false)[0]; got != 1 {
			t.Fatalf("cold first fault lost contents: %d", got)
		}
	})
}

// Two attachments inherit the same frames while both are held by in-flight
// faults. Population acquires resident locks in one global identity order and
// never waits for a lock from inside a plan that already holds another, so the
// release order of the faults cannot leave either attachment stuck.
func TestConcurrentPopulationsOfHeldFramesDoNotDeadlock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2, LogicalPages: 6, DirtyPages: 2, ReadAheadPages: 1})
		source, sm, _ := f.region(2)
		access(t, source, sm, 0, false)
		access(t, source, sm, 1, false)
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
		sm.onResolve = func(page uint64) { close(entered[page]); <-release[page] }
		faults := make(chan error, 2)
		for page := range uint64(2) {
			go func() { faults <- source.Fault(t.Context(), page, false) }()
		}
		<-entered[0]
		<-entered[1]
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		attach := func(backing vmmemory.Backing) (*mapping, <-chan error) {
			m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
			f.a.mu.Lock()
			f.a.mappings = append(f.a.mappings, m)
			f.a.mu.Unlock()
			done := make(chan struct{})
			result := make(chan error, 1)
			var r *vmmemory.Region
			go func() {
				var err error
				r, err = f.h.Attach(ctx, backing, m)
				result <- err
				close(done)
			}()
			t.Cleanup(func() {
				<-done
				clear(m.pages)
				if r != nil {
					if err := r.Detach(context.Background()); err != nil {
						t.Error(err)
					}
				}
			})
			return m, result
		}
		first, firstDone := attach(f.newBacking(2))
		synctest.Wait()
		second, secondDone := attach(f.newBacking(2))
		synctest.Wait()
		// Release the second page first. An attachment that waited on a held
		// frame while holding another would never see either fault complete.
		close(release[1])
		released[1] = true
		synctest.Wait()
		close(release[0])
		released[0] = true
		synctest.Wait()
		for _, done := range []<-chan error{firstDone, secondDone} {
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("concurrent populations of held frames deadlocked")
			}
		}
		for range 2 {
			if err := <-faults; err != nil {
				t.Fatal(err)
			}
		}
		for page := range uint64(2) {
			firstPage, firstOK := first.pages[page]
			secondPage, secondOK := second.pages[page]
			if !firstOK || !secondOK || firstPage.slot != sm.pages[page].slot || secondPage.slot != sm.pages[page].slot {
				t.Fatalf("page %d did not inherit the source frame", page)
			}
		}
	})
}

func (b *delayedLocate) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	close(b.entered)
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-b.release:
	}
	return b.Backing.Locate(ctx, offset, length)
}

func TestStalledMetadataDoesNotDelayUnrelatedWarmAttachment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 4, 12, 4)
		source, sm, _ := f.region(4)
		for page := range uint64(4) {
			access(t, source, sm, page, false)
		}
		slow := &delayedLocate{Backing: f.newUnrelatedBacking(4), entered: make(chan struct{}), release: make(chan struct{})}
		defer close(slow.release)
		attach := func(backing vmmemory.Backing) (*mapping, <-chan struct{}, *error) {
			m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
			f.a.mu.Lock()
			f.a.mappings = append(f.a.mappings, m)
			f.a.mu.Unlock()
			done := make(chan struct{})
			var r *vmmemory.Region
			var err error
			go func() { r, err = f.h.Attach(t.Context(), backing, m); close(done) }()
			t.Cleanup(func() {
				<-done
				f.a.mu.Lock()
				clear(m.pages)
				f.a.mu.Unlock()
				if r != nil {
					if err := r.Detach(context.Background()); err != nil {
						t.Error(err)
					}
				}
			})
			return m, done, &err
		}
		_, _, _ = attach(slow)
		<-slow.entered
		warm, done, err := attach(f.newBacking(4))
		synctest.Wait()
		select {
		case <-done:
			if *err != nil {
				t.Fatal(*err)
			}
		default:
			t.Fatal("warm attachment waited for unrelated metadata")
		}
		for page := range uint64(4) {
			got, ok := warm.pages[page]
			if !ok || got.slot != sm.pages[page].slot {
				t.Fatalf("resident page %d missing at attachment completion", page)
			}
		}
	})
}

func TestAttachmentIncludesFramesLoadedDuringMetadataLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 6, 9, 4)
		source, sm, _ := f.region(4)
		for page := range uint64(3) {
			access(t, source, sm, page, false)
		}
		seed, seedMapping := f.attach(f.newUnrelatedBacking(1))
		access(t, seed, seedMapping, 0, false)
		slow := &delayedLocate{Backing: f.newBacking(4), entered: make(chan struct{}), release: make(chan struct{})}
		m := &mapping{arena: f.a, pages: make(map[uint64]mapped)}
		f.a.mappings = append(f.a.mappings, m)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		var r *vmmemory.Region
		var err error
		go func() { r, err = f.h.Attach(ctx, slow, m); close(done) }()
		t.Cleanup(func() {
			<-done
			clear(m.pages)
			if r != nil {
				if err := r.Detach(context.Background()); err != nil {
					t.Error(err)
				}
			}
		})
		<-slow.entered
		access(t, source, sm, 3, false)
		close(slow.release)
		<-done
		if err != nil {
			t.Fatal(err)
		}
		for page := range uint64(4) {
			got, ok := m.pages[page]
			if !ok || got.slot != sm.pages[page].slot {
				t.Fatalf("page %d was resident before metadata returned but was not populated", page)
			}
		}
	})
}
