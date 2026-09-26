package vmmemory_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory"
)

// ephemeralDisk is the backing of an ephemeral disk, which is what a volume
// no checkpoint holds looks like to a pager: every page reads as zeroes, and
// it says which kind of pager it belongs to.
type ephemeralDisk struct {
	size      uint64
	ephemeral bool
}

func (d ephemeralDisk) Size() uint64 { return d.size }
func (d ephemeralDisk) Load(_ context.Context, _ uint64, dst []byte) error {
	clear(dst)
	return nil
}
func (ephemeralDisk) Verify(context.Context) error { return nil }
func (d ephemeralDisk) Locate(_ context.Context, offset, length uint64) ([]control.Extent, error) {
	return []control.Extent{{Offset: offset, Length: length, Identity: control.ZeroIdentity}}, nil
}
func (d ephemeralDisk) Ephemeral() bool { return d.ephemeral }

var _ vmmemory.EphemeralBacking = ephemeralDisk{}

// An ephemeral pager holds every page of a disk no checkpoint holds: a store
// into each of them goes through without waiting though the arena holds a
// quarter of them, the ones evicted come back from the spill file, and a seal
// takes nothing, so no capture can publish one.
func TestAnEphemeralPagerHoldsEveryStoreAndSealsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 8
		f := newConfiguredFixture(t, vmmemory.Config{Ephemeral: true,
			ResidentPages: pages / 4, LogicalPages: pages, DirtyPages: pages})
		r, m := f.attachKind(vmmemory.Pmem, ephemeralDisk{size: pages * uint64(pageSize), ephemeral: true})
		if !r.Ephemeral() || r.OnInterval() {
			t.Fatalf("the memory region is ephemeral %v and on the interval %v, want ephemeral and off it",
				r.Ephemeral(), r.OnInterval())
		}
		for page := range uint64(pages) {
			value := byte(40 + page)
			if _, err := memoryByte(t.Context(), r, m, page, &value); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if r.Checkpoint() != nil {
			t.Fatal("the seal of an ephemeral disk recorded a checkpoint")
		}
		for page := range uint64(pages) {
			got, err := memoryByte(t.Context(), r, m, page, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != byte(40+page) {
				t.Fatalf("page %d reads %d, want %d", page, got, 40+page)
			}
		}
		stats, err := f.h.Stats(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if stats.DirtyWaits != 0 || stats.Spills == 0 {
			t.Fatalf("the ephemeral pager waited %d times and spilled %d pages, want no waits and some spills",
				stats.DirtyWaits, stats.Spills)
		}
	})
}

// An ephemeral pager is built for ephemeral disks alone: its budget must hold
// every page it admits, it keeps no loss window, it maps no RAM, and a volume
// that says which pager it belongs to attaches only to that one.
func TestAnEphemeralPagerServesOnlyEphemeralDisks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, cfg := range []vmmemory.Config{
			{Ephemeral: true, ResidentPages: 2, LogicalPages: 8, DirtyPages: 4},
			{Ephemeral: true, ResidentPages: 2, LogicalPages: 8, DirtyPages: 8, LossWindow: 1},
		} {
			cfg.PageSize = uint64(pageSize)
			if _, err := newBrokenFixture(t, cfg); !errors.Is(err, vmmemory.ErrConfig) {
				t.Fatalf("an ephemeral pager of %+v was built with %v, want ErrConfig", cfg, err)
			}
		}
		ephemeral := newConfiguredFixture(t, vmmemory.Config{Ephemeral: true,
			ResidentPages: 2, LogicalPages: 8, DirtyPages: 8})
		plain := newFixture(t, 2, 8, 8)
		size := 2 * uint64(pageSize)
		for _, refused := range []struct {
			f       *fixture
			backing vmmemory.MemoryRegionBacking
		}{
			{ephemeral, vmmemory.MemoryRegionBacking{Kind: vmmemory.Ram, Backing: ephemeralDisk{size: size, ephemeral: true}}},
			{ephemeral, vmmemory.MemoryRegionBacking{Kind: vmmemory.Pmem, Backing: ephemeralDisk{size: size}}},
			{plain, vmmemory.MemoryRegionBacking{Kind: vmmemory.Pmem, Backing: ephemeralDisk{size: size, ephemeral: true}}},
		} {
			m := &mapping{arena: refused.f.a, pages: make(map[uint64]mapped)}
			if _, err := refused.f.h.Attach(t.Context(), refused.backing, m); !errors.Is(err, vmmemory.ErrConfig) {
				t.Fatalf("attaching %+v returned %v, want ErrConfig", refused.backing, err)
			}
		}
	})
}
