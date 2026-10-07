package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A prefetch reads what the source did with its request as it sends it, under
// the source's lock. A fault of another region of the root supplies pages
// under the root's lock alone, so it may complete the request the moment the
// source lets it go. A prefetch that looked after found its request taken back
// and panicked that it met another, which is TASK-105's panic by another path.
// The unscheduled soak found the unlocked look as a data race.
func TestAPrefetchWhoseRequestASupplyCompletesAtOnceReadsItsRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newConfiguredFixture(t, prefetchConfig())
		b := f.slowBacking(16)
		r, m := f.attach(b)
		supplied := 0
		vmmemory.SetPrefetchSentSeam(t, func(supply func()) {
			supplied++
			supply()
		})
		for page := range uint64(2) {
			if err := r.Fault(f.ctx, page, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.SettlePrefetches(f.ctx); err != nil {
			t.Fatal(err)
		}
		if supplied == 0 {
			t.Fatal("no prefetch sent a request")
		}
		for page := range uint64(8) {
			if got := accessUnder(f.ctx, t, r, m, page, false)[0]; got != byte(page+1) {
				t.Fatalf("page %d reads %d, want %d", page, got, page+1)
			}
		}
	})
}
