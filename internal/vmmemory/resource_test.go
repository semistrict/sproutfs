package vmmemory_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/resource"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

func pageBudget(t *testing.T, pages int) *resource.Budget {
	t.Helper()
	b, err := resource.New(int64(pages * pageSize))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPagerEvictsWithinSharedRAMAllowance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := pageBudget(t, 2)
		other, err := b.TryAcquire(t.Context(), int64(pageSize))
		if err != nil {
			t.Fatal(err)
		}
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2, LogicalPages: 4, DirtyPages: 4, ReadAheadPages: 1}, b)
		r, m, _ := f.region(2)
		for _, page := range []uint64{0, 1, 0} {
			if got := access(t, r, m, page, false)[0]; got != byte(page+1) {
				t.Fatalf("page %d lost data: %d", page, got)
			}
			if got := b.Stats().Used; got != int64(2*pageSize) {
				t.Fatalf("shared RAM accounting = %d", got)
			}
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil || stats.ResidentPages != 1 || stats.Evictions == 0 {
			t.Fatalf("pager exceeded shared allowance instead of evicting: %+v %v", stats, err)
		}
		other.Close()
		access(t, r, m, 1, false)
		stats, err = f.h.Stats(t.Context())
		if err != nil || stats.ResidentPages != 2 {
			t.Fatalf("released RAM was not available to pager: %+v %v", stats, err)
		}
		clear(m.pages)
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := b.Stats().Used; got != 0 {
			t.Fatalf("detached pages retain %d bytes", got)
		}
	})
}

func TestSharedPageIsChargedOnceUntilLastAliasDetaches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := pageBudget(t, 1)
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 2, LogicalPages: 2, DirtyPages: 2, ReadAheadPages: 1}, b)
		first, fm, _ := f.region(1)
		access(t, first, fm, 0, false)
		second, sm, _ := f.region(1)
		if got := access(t, second, sm, 0, false)[0]; got != 1 {
			t.Fatal(got)
		}
		if fm.pages[0].slot != sm.pages[0].slot || b.Stats().Used != int64(pageSize) {
			t.Fatal("shared page was duplicated or charged twice")
		}
		clear(fm.pages)
		if err := first.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if b.Stats().Used != int64(pageSize) {
			t.Fatal("first detach released a page still held by a sibling")
		}
		clear(sm.pages)
		if err := second.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if b.Stats().Used != 0 {
			t.Fatal("last detach retained a punched page")
		}
	})
}

func TestFaultWaitsForOtherConsumerAndCancellationReleasesReservations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := pageBudget(t, 1)
		other, err := b.TryAcquire(t.Context(), int64(pageSize))
		if err != nil {
			t.Fatal(err)
		}
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 1, LogicalPages: 1, DirtyPages: 1, ReadAheadPages: 1}, b)
		r, m, _ := f.region(1)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Fault(ctx, 0, false) }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("fault did not wait for shared RAM: %v", err)
		default:
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if b.Stats().Used != int64(pageSize) {
			t.Fatal("canceled fault changed another consumer's ownership")
		}
		other.Close()
		if got := access(t, r, m, 0, false)[0]; got != 1 {
			t.Fatal(got)
		}
		clear(m.pages)
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if b.Stats().Used != 0 {
			t.Fatal("fault/teardown leaked RAM")
		}
	})
}

// Allocate before failing, as a partial physical allocation can precede a
// failed response. A failed punch must not turn those bytes into free capacity.
type failedResourceArena struct {
	*arena
	failPunch bool
}

func (a *failedResourceArena) Write(ctx context.Context, slot int, data []byte) error {
	if err := a.arena.Write(ctx, slot, data); err != nil {
		return err
	}
	return errInjected
}
func (a *failedResourceArena) Release(ctx context.Context, slot int) error {
	if a.failPunch {
		return errInjected
	}
	return a.arena.Release(ctx, slot)
}

func TestFailedPhysicalCleanupRetainsRAMUntilRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := pageBudget(t, 1)
		f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 1, LogicalPages: 1, DirtyPages: 1}, b)
		spill, err := f.disk.Open(t.Context(), "failed-spill", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		defer spill.Close()
		a := &failedResourceArena{arena: f.a, failPunch: true}
		f.h, err = vmmemory.New(t.Context(), b, vmmemory.Config{PageSize: uint64(pageSize), ResidentPages: 1, LogicalPages: 1, DirtyPages: 1}, a, spill)
		if err != nil {
			t.Fatal(err)
		}
		r, m, _ := f.region(1)
		if err := r.Fault(t.Context(), 0, false); !errors.Is(err, errInjected) {
			t.Fatal(err)
		}
		clear(m.pages)
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := f.h.Close(t.Context()); !errors.Is(err, errInjected) {
			t.Fatalf("failed punch was accepted: %v", err)
		}
		if b.Stats().Used != int64(pageSize) {
			t.Fatal("unreleased physical memory became available to another owner")
		}
		a.failPunch = false
		if err := f.h.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if b.Stats().Used != 0 || f.a.slots[0] != nil {
			t.Fatal("successful cleanup did not return physical memory and reservation")
		}
	})
}

func TestHugePageFaultReclaimsCacheBeforeEvictingGuestPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const huge = 2 << 20
		b, err := resource.New(2 * huge)
		if err != nil {
			t.Fatal(err)
		}
		cached, err := b.TryAcquireCache(t.Context(), huge)
		if err != nil {
			t.Fatal(err)
		}
		defer cached.Close()
		data := make([]byte, huge)
		defer b.RegisterCache(func(ctx context.Context, requested int64) (bool, error) {
			if requested == 0 || data == nil {
				return false, nil
			}
			data = nil
			cached.Close()
			return true, nil
		})()
		// The budget here is two of the large page, so this pager runs that page
		// whichever one the suite is exercising: what is being asked is how a
		// 2 MiB admission behaves against a cache holding the other half.
		f := newConfiguredFixture(t, vmmemory.Config{PageSize: huge, ResidentPages: 2, LogicalPages: 2,
			DirtyPages: 2, ReadAheadPages: 1}, b)
		r, m, _ := f.region(2)
		access(t, r, m, 0, false)
		if data == nil {
			t.Fatal("cache was discarded while the first huge page still fit")
		}
		access(t, r, m, 1, false)
		stats, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if data != nil || stats.ResidentPages != 2 || stats.Evictions != 0 || b.Stats().Used != 2*huge {
			t.Fatalf("huge-page admission failed to prefer cache eviction: pager=%+v budget=%+v", stats, b.Stats())
		}
		clear(m.pages)
		if err := r.Detach(t.Context()); err != nil {
			t.Fatal(err)
		}
		if b.Stats().Used != 0 {
			t.Fatal("huge-page teardown retained its reservation")
		}
	})
}
