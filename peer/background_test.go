package peer

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

// Unpublished post-copy pages go before the rest of the stream whatever order
// they arrived in, and each priority keeps its own arrival order.
func TestTheBackgroundBudgetAdmitsUnpublishedPagesFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewBackground(4)
		if err := budget.Acquire(t.Context(), Resident, 4); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var order []string
		var wg sync.WaitGroup
		for _, waiter := range []struct {
			name     string
			priority Priority
		}{{"resident-1", Resident}, {"unpublished-1", Unpublished}, {"resident-2", Resident}, {"unpublished-2", Unpublished}} {
			wg.Go(func() {
				if err := budget.Acquire(t.Context(), waiter.priority, 4); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				order = append(order, waiter.name)
				mu.Unlock()
				budget.Release(4)
			})
			synctest.Wait()
		}
		budget.Release(4)
		wg.Wait()
		if want := []string{"unpublished-1", "unpublished-2", "resident-1", "resident-2"}; !slices.Equal(order, want) {
			t.Fatalf("admitted %v, want %v", order, want)
		}
	})
}

// Work that would fit does not pass work already waiting: a small request
// behind a large one waits its turn, or the large one would wait for ever.
func TestWorkThatFitsWaitsBehindWorkThatWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewBackground(4)
		if err := budget.Acquire(t.Context(), Resident, 3); err != nil {
			t.Fatal(err)
		}
		large := make(chan error, 1)
		go func() { large <- budget.Acquire(t.Context(), Resident, 2) }()
		synctest.Wait()
		small := make(chan error, 1)
		go func() { small <- budget.Acquire(t.Context(), Resident, 1) }()
		synctest.Wait()
		select {
		case <-small:
			t.Fatal("a request that fit passed one waiting in front of it")
		default:
		}
		budget.Release(3)
		for _, admitted := range []chan error{large, small} {
			if err := <-admitted; err != nil {
				t.Fatal(err)
			}
		}
		if status := budget.Status(); status.Held != 3 || status.Waiting != 0 {
			t.Fatalf("the budget is %+v", status)
		}
	})
}

// Fills and repairs over the budget are dropped, never queued; a repair has
// half the budget, so fills keep the rest; and neither takes room while work
// that waits is waiting.
func TestFillsAndRepairsOverTheBudgetAreDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewBackground(8)
		if !budget.TryAcquire(Repair, 4) {
			t.Fatal("a repair within half the budget was dropped")
		}
		if budget.TryAcquire(Repair, 1) {
			t.Fatal("a repair past half the budget was taken")
		}
		if !budget.TryAcquire(Fill, 4) {
			t.Fatal("a fill within the budget was dropped")
		}
		if budget.TryAcquire(Fill, 1) {
			t.Fatal("a fill past the budget was taken")
		}
		waiting := make(chan error, 1)
		go func() { waiting <- budget.Acquire(t.Context(), Unpublished, 4) }()
		synctest.Wait()
		budget.Release(4)
		if err := <-waiting; err != nil {
			t.Fatal(err)
		}
		budget.Release(4)
		go func() { waiting <- budget.Acquire(t.Context(), Resident, 8) }()
		synctest.Wait()
		if budget.TryAcquire(Fill, 1) {
			t.Fatal("a fill was taken while the stream waited")
		}
		budget.Release(4)
		if err := <-waiting; err != nil {
			t.Fatal(err)
		}
		if status := budget.Status(); status.Held != 8 || status.Waiting != 0 {
			t.Fatalf("the budget is %+v", status)
		}
	})
}

// A guest fault waiting anywhere shrinks the budget to a quarter: the stream
// waits for the fault, then takes its share again.
func TestAFaultShrinksTheBackgroundBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewBackground(8)
		if err := budget.Acquire(t.Context(), Resident, 2); err != nil {
			t.Fatal(err)
		}
		budget.faultStarted()
		admitted := make(chan error, 1)
		go func() { admitted <- budget.Acquire(t.Context(), Resident, 2) }()
		synctest.Wait()
		select {
		case <-admitted:
			t.Fatal("the stream took more than a quarter of the budget while a fault waited")
		default:
		}
		budget.faultEnded()
		if err := <-admitted; err != nil {
			t.Fatal(err)
		}
	})
}

// A waiter whose caller gives up leaves nothing held and nothing queued.
func TestAWaiterThatGivesUpHoldsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := NewBackground(4)
		if err := budget.Acquire(t.Context(), Resident, 4); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		gaveUp := make(chan error, 1)
		go func() { gaveUp <- budget.Acquire(ctx, Unpublished, 4) }()
		synctest.Wait()
		cancel()
		if err := <-gaveUp; !errors.Is(err, context.Canceled) {
			t.Fatalf("a waiter that gave up: %v", err)
		}
		budget.Release(4)
		if status := budget.Status(); status.Held != 0 || status.Waiting != 0 {
			t.Fatalf("the budget is %+v", status)
		}
	})
}
