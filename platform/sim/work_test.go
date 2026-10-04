package sim_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
)

// Priced work takes its bytes over its rate of simulated time, three pieces
// side by side take as long as one, and the runtime counts them and the most
// at once. Work of a kind nothing prices, and work outside a simulation, takes
// no time and is not counted.
func TestWorkTakesTheTimeItsPriceSays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Compute: map[string]int64{"encode": 1 << 20}})
		ctx := sim.WithRuntime(t.Context(), runtime)
		started := time.Now()
		began := started
		if err := sim.Work(sim.WithTask(ctx, "first"), "encode", 512<<10); err != nil {
			t.Fatal(err)
		}
		if got, want := time.Since(started), 500*time.Millisecond; got != want {
			t.Fatalf("half a MiB at 1 MiB a second took %v, want %v", got, want)
		}
		started = time.Now()
		var side sync.WaitGroup
		for range 3 {
			side.Go(func() {
				if err := sim.Work(ctx, "encode", 1<<20); err != nil {
					t.Error(err)
				}
			})
		}
		side.Wait()
		if got, want := time.Since(started), time.Second; got != want {
			t.Fatalf("three pieces side by side took %v, want %v", got, want)
		}
		started = time.Now()
		if err := sim.Work(ctx, "unpriced", 1<<30); err != nil {
			t.Fatal(err)
		}
		if err := sim.Work(context.Background(), "encode", 1<<30); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(started); got != 0 {
			t.Fatalf("unpriced work took %v, want none", got)
		}
		if got, want := runtime.Work("encode"), (sim.WorkStats{Pieces: 4, Peak: 3}); got != want {
			t.Fatalf("encode work did %+v, want %+v", got, want)
		}
		if got := runtime.Work("unpriced"); got != (sim.WorkStats{}) {
			t.Fatalf("unpriced work was counted: %+v", got)
		}
		// The first piece ran under a task of its own, at the start, and the
		// three after it under none, half a second in, each priced by its
		// bytes.
		pieces := runtime.WorkPieces("encode")
		if len(pieces) != 4 || pieces[0].Task != `/"first"` || !pieces[0].Began.Equal(began) ||
			pieces[0].Bytes != 512<<10 {
			t.Fatalf("the pieces were %+v, want the first under its task at %v, of half a MiB", pieces, began)
		}
		for _, piece := range pieces[1:] {
			if piece.Task != "" || piece.Began.Sub(began) != 500*time.Millisecond || piece.Bytes != 1<<20 {
				t.Fatalf("a piece side by side was %+v, want no task half a second in, of a MiB", piece)
			}
		}
	})
}

// Work ends early, with the cause, when its context does.
func TestWorkEndsWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Compute: map[string]int64{"encode": 1}})
		ctx, cancel := context.WithTimeout(sim.WithRuntime(t.Context(), runtime), time.Second)
		defer cancel()
		started := time.Now()
		if err := sim.Work(ctx, "encode", 1<<20); err != context.DeadlineExceeded {
			t.Fatalf("work under a deadline ended with %v, want %v", err, context.DeadlineExceeded)
		}
		if got, want := time.Since(started), time.Second; got != want {
			t.Fatalf("work under a deadline of a second took %v, want %v", got, want)
		}
	})
}
