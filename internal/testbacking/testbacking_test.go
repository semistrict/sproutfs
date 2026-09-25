package testbacking_test

import (
	"context"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testbacking"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// plainBacking answers the four calls every backing answers and nothing else.
type plainBacking struct{}

func (plainBacking) Size() uint64                               { return 1 << 20 }
func (plainBacking) Load(context.Context, uint64, []byte) error { return nil }
func (plainBacking) Verify(context.Context) error               { return nil }
func (plainBacking) Locate(context.Context, uint64, uint64) ([]control.Extent, error) {
	return nil, nil
}

// sparseBacking is a volume: a fault can ask it for part of a window.
type sparseBacking struct{ plainBacking }

func (sparseBacking) LoadPages(context.Context, uint64, []byte, []bool) error { return nil }

// peerBacking is a migration destination's: its loads can return bytes the
// volume does not hold.
type peerBacking struct{ plainBacking }

func (peerBacking) LoadUnpublished(context.Context, uint64, []byte) ([]bool, error) {
	return nil, nil
}

// The wrapper claims exactly what the backing it wraps claims. A pager reads
// what a backing can do from the methods it has, so a wrapper that claimed more
// would make every simulated volume look like a migration destination's, and
// one that claimed less would hide the masked window read a fault depends on.
func TestTheWrapperClaimsWhatTheBackingClaims(t *testing.T) {
	runtime := sim.New(sim.Config{})
	for _, item := range []struct {
		name          string
		backing       vmmemory.Backing
		sparse, unpub bool
	}{
		{name: "plain", backing: plainBacking{}},
		{name: "volume", backing: sparseBacking{}, sparse: true},
		{name: "peer", backing: peerBacking{}, unpub: true},
	} {
		attached, admitting := testbacking.New(item.backing, runtime, item.name)
		if admitting == nil {
			t.Fatalf("%s: the wrapper reported no admission to count through", item.name)
		}
		if _, ok := attached.(vmmemory.SparseLoader); ok != item.sparse {
			t.Fatalf("%s: asked for part of a window = %t, want %t", item.name, ok, item.sparse)
		}
		if _, ok := attached.(vmmemory.UnpublishedLoader); ok != item.unpub {
			t.Fatalf("%s: reports unpublished pages = %t, want %t", item.name, ok, item.unpub)
		}
	}
}
