package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A store that loses the read of the page it copies to another holder of the
// page, again and again, still stores: each loss is someone else's progress,
// counted as a load counts its lost races, not as a decision whether it needs
// a page of its own, of which a fault has only eight. Eight forks of one
// checkpoint on two vCPUs each lost eight running in the unscheduled soak, and
// the guest's session ended with ErrContended.
func TestAStoreThatLosesItsReadTenTimesRunningStillStores(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, 8, 32, 8)
		a, am := f.attach(f.newBacking(8))
		b, bm := f.attach(f.newBacking(8))
		accessUnder(f.ctx, t, a, am, 3, false)
		losses := 0
		vmmemory.SetReadInSeam(t, func(index uint64) {
			if index != 3 || losses == 10 {
				return
			}
			losses++
			release, err := vmmemory.HoldPage(f.ctx, a, 3)
			if err != nil {
				t.Errorf("holding the other fork's page: %v", err)
				return
			}
			go func() {
				synctest.Wait()
				release()
			}()
		})
		f.storeAt(b, bm, 3, 0, 0x77)
		if losses != 10 {
			t.Fatalf("the store lost its read %d times, want 10", losses)
		}
		if got := accessUnder(f.ctx, t, b, bm, 3, false)[0]; got != 0x77 {
			t.Fatalf("the store's page reads %#x, want 0x77", got)
		}
		if got := accessUnder(f.ctx, t, a, am, 3, false)[0]; got != 4 {
			t.Fatalf("the other fork's page reads %d, want 4, its volume's", got)
		}
	})
}
