package vmmemory_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// A pager's core is named as a deployment names it, and a configuration that
// names none runs the current core.
func TestAPagerCoreIsNamedAsADeploymentNamesIt(t *testing.T) {
	if got := (vmmemory.Config{}).Core; got != vmmemory.CoreCurrent {
		t.Fatalf("a configuration naming no core runs %s, want current", got)
	}
	for _, core := range []vmmemory.Core{vmmemory.CoreCurrent, vmmemory.CoreZircon} {
		parsed, err := vmmemory.ParseCore(core.String())
		if err != nil || parsed != core {
			t.Fatalf("ParseCore(%q) = %s, %v, want %s", core.String(), parsed, err, core)
		}
	}
	if got := vmmemory.CoreZircon.String(); got != "zircon" {
		t.Fatalf("the zircon core is named %q, want zircon", got)
	}
	_, err := vmmemory.ParseCore("freebsd")
	if !errors.Is(err, vmmemory.ErrConfig) || err.Error() !=
		`invalid managed-memory configuration: pager core "freebsd", want current or zircon` {
		t.Fatalf("ParseCore of an unknown core = %v, want it refused as a configuration", err)
	}
}

// newCorePager builds a pager in core, whatever core the suite runs in.
func newCorePager(t *testing.T, ctx context.Context, core vmmemory.Core) (*vmmemory.Host, *arena, error) {
	t.Helper()
	runtime := sim.New(sim.Config{})
	ctx = sim.WithRuntime(ctx, runtime)
	spill, err := runtime.NewDisk("pager", sim.DiskConfig{}).Open(ctx, "spill", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spill.Close() })
	a := newArena(pageSize)
	h, err := vmmemory.New(ctx, testresource.New(), vmmemory.Config{PageSize: uint64(pageSize),
		ResidentPages: 2, LogicalPages: 4, DirtyPages: 2, Core: core}, a, spill)
	return h, a, err
}

// A core this build does not have is refused when the pager is built, rather
// than run as another.
func TestAPagerRefusesACoreItDoesNotHave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _, err := newCorePager(t, t.Context(), vmmemory.Core(7))
		if h != nil || !errors.Is(err, vmmemory.ErrConfig) ||
			err.Error() != "invalid managed-memory configuration: pager core Core(7)" {
			t.Fatalf("a pager of core 7 = %v, %v, want it refused as a configuration", h, err)
		}
	})
}

// The zircon core attaches, reads and detaches a memory region, and refuses,
// naming it, each operation it does not serve yet.
func TestTheZirconCoreRefusesWhatItDoesNotServeYet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		h, a, err := newCorePager(t, ctx, vmmemory.CoreZircon)
		if err != nil {
			t.Fatal(err)
		}
		f := &fixture{t: t, ctx: ctx, h: h, a: a, pageSize: pageSize}
		backing := f.newBacking(4)
		m := newMapping(a)
		r, err := h.Attach(ctx, vmmemory.MemoryRegionBacking{Kind: vmmemory.Pmem, Backing: backing}, m)
		if err != nil {
			t.Fatal(err)
		}
		if got := access(t, r, m, 2, false)[0]; got != 3 {
			t.Fatalf("page 2 reads %d under the zircon core, want 3", got)
		}
		_, err = r.Unpublished()
		wantRefused(t, "unpublished", err, "the zircon core does not list unpublished pages yet")
		_, err = r.Handoff(ctx)
		wantRefused(t, "handoff", err, "the zircon core does not hand a memory region off yet")
		_, err = r.GiveBackColdCopies(ctx)
		wantRefused(t, "give back", err, "the zircon core does not give cold copies back yet")
		clear(m.pages)
		if err := r.Detach(ctx); err != nil {
			t.Fatal(err)
		}
		if got := h.LogicalHeadroom(); got != 4 {
			t.Fatalf("a zircon pager that detached its region has %d logical pages of headroom, want 4", got)
		}
		if err := h.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// wantRefused fails unless err is the zircon core's refusal of an operation.
func wantRefused(t *testing.T, operation string, err error, what string) {
	t.Helper()
	if !errors.Is(err, vmmemory.ErrCoreUnsupported) ||
		err.Error() != "managed-memory operation not served by this pager's core: "+what {
		t.Fatalf("%s under the zircon core = %v, want it refused: %s", operation, err, what)
	}
}
