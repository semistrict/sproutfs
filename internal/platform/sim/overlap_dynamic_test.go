package sim_test

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/platform/sim"
)

func dynamicPoint(s *sim.Scheduler, id string) error {
	return s.Wait(context.Background(), "task/"+id, 0, 0)
}

func TestDynamicOverlapDiscoversNewWorkBeforeAdvancingTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		s := sim.NewScheduler(1)
		done := make(chan struct{})
		var arrivals []time.Duration
		go func() {
			defer close(done)
			for i := range 3 {
				if err := s.Wait(t.Context(), fmt.Sprintf("chain/%d", i), time.Millisecond, 2*time.Millisecond); err != nil {
					t.Error(err)
					return
				}
				arrivals = append(arrivals, time.Since(start))
			}
		}()
		if err := s.Run(done); err != nil {
			t.Fatal(err)
		}
		for i, at := range arrivals {
			if at != time.Duration(i+1)*2*time.Millisecond {
				t.Fatalf("new work ran at %s", at)
			}
		}
		if len(arrivals) != 3 {
			t.Fatal("lost dynamically registered work")
		}
	})
}

func TestDynamicOverlapHonorsCancellationBeforeTheWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		s := sim.NewScheduler(1)
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			err := s.Wait(ctx, "canceled/io", 10*time.Millisecond, 20*time.Millisecond)
			if err != context.DeadlineExceeded {
				t.Errorf("canceled operation returned %v", err)
			}
			if at := time.Since(start); at != time.Millisecond {
				t.Errorf("cancellation waited until %s", at)
			}
		}()
		if err := s.Run(done); err != nil {
			t.Fatal(err)
		}
	})
}
