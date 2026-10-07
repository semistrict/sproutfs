package vmmemory_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/vmmemory"
)

// A child's store into the copy a fork point lent it takes its alias off the
// copy before its command replaces the child's mapping of it. A seal's end
// that drops the copy in between finds no alias and revokes nothing, so the
// copy goes back only once the store's command has landed: given back at
// once, the child mapped a slot the arena had taken back. The unscheduled soak
// found it.
func TestTheEndOfASealLeavesACopyAChildsStoreStillReplaces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPinnedFixture(t, vmmemory.Config{Arena: vmmemory.ArenaIsolated, ResidentPages: 16,
			LogicalPages: 32, DirtyPages: 8, ReadAheadPages: 1})
		parent, _, _, cb := lendingParent(t, f)
		child, cm := f.attach(cb)
		share(t, f, parent, cb)
		if got := access(t, child, cm, 0, false)[0]; got != 44 {
			t.Fatalf("the child reads %d at page 0, want the 44 the point lent it", got)
		}
		var unsealed error
		ended := false
		done := make(chan struct{})
		cm.onMap = func(page uint64, _ int) {
			if page != 0 || ended {
				return
			}
			// The store has copied the page and taken its alias off the copy;
			// its command has not landed. The unseal goes as far as it can
			// before the command does.
			ended = true
			go func() {
				defer close(done)
				unsealed = parent.Unseal(f.ctx)
			}()
			synctest.Wait()
		}
		accessUnder(f.ctx, t, child, cm, 0, true)[0] = 45
		<-done
		if !ended || unsealed != nil {
			t.Fatalf("the unseal under the child's store ran %t and returned %v, want it run and done", ended, unsealed)
		}
		if got := access(t, child, cm, 0, false)[0]; got != 45 {
			t.Fatalf("the child reads %d at page 0 after its store, want 45", got)
		}
		if got := access(t, child, cm, 1, false)[0]; got != 55 {
			t.Fatalf("the child reads %d at page 1, want the 55 its volume holds", got)
		}
	})
}
