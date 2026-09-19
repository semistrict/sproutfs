//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// countedVolume reports how much of a restore the pager had to read back. It
// forwards every call to the volume unchanged.
type countedVolume struct {
	*volume.Volume
	loads, loadedPages atomic.Uint64
}

func (v *countedVolume) Load(ctx context.Context, offset uint64, dst []byte) error {
	v.loads.Add(1)
	v.loadedPages.Add(uint64(len(dst)) / uint64(vmmemory.PageSize))
	return v.Volume.Load(ctx, offset, dst)
}

// Two machines restored from one checkpoint share every page the first brought
// in. The second reads nothing from its volumes, takes no fault on those pages,
// and installs them with a number of mapping commands far below the page count.
func TestRestoreFromSharedSnapshotLoadsNothingOnTheSecondMachine(t *testing.T) {
	const pages = 32
	size := vmmemory.PageSize
	h := kernelHost(t, 4*pages, 12*pages)
	c := newPagerCluster(t)
	source, err := c.manager.Create(t.Context(), "source", []volume.VolumeSpec{
		{Name: "pmem0", Size: uint64(pages * size), PageSize: vmmemory.PageSize},
		{Name: "ram0", Size: uint64(pages * size), PageSize: vmmemory.PageSize},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	for region, name := range []string{"pmem0", "ram0"} {
		data := make([]byte, pages*size)
		for i := range data {
			data[i] = byte(1 + region*32 + i/size)
		}
		for offset := 0; offset < len(data); offset += 1 << 20 {
			if err := source.Volume(name).Write(t.Context(), uint64(offset), data[offset:offset+(1<<20)]); err != nil {
				t.Fatal(err)
			}
			if err := source.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	point, err := source.ForkPoint(t.Context(), volume.Prepared([]byte("vmm state"), nil))
	if err != nil {
		t.Fatal(err)
	}
	restore := func(id string) ([]vmmemory.Backing, []*countedVolume) {
		vm, err := c.manager.Fork(t.Context(), id, point)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = vm.Close(context.Background()) })
		var backing []vmmemory.Backing
		var counted []*countedVolume
		for _, name := range []string{"pmem0", "ram0"} {
			v := &countedVolume{Volume: vm.Volume(name)}
			counted = append(counted, v)
			backing = append(backing, v)
		}
		return backing, counted
	}
	touch := func(p *nativeProcess) {
		for region := range 2 {
			for page := range pages {
				p.request(fmt.Sprintf("kvmread %d %d", region, page*size), fmt.Sprintf("kvm %d", byte(1+region*32+page)))
			}
		}
	}
	firstBacking, firstCounted := restore("a")
	first := startNative(t, h, pages, firstBacking...)
	touch(first)
	warm, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	secondBacking, secondCounted := restore("b")
	before, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second := startNative(t, h, pages, secondBacking...)
	attached, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	touch(second)
	after, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range secondCounted {
		if loads := v.loads.Load(); loads != 0 {
			t.Errorf("region %d of the second machine issued %d backing loads", i, loads)
		}
	}
	mapped := attached.MappedPages - before.MappedPages
	commands := attached.Mappings - before.Mappings
	if mapped != 2*pages || attached.IdentityHits-before.IdentityHits != 2*pages {
		t.Fatalf("restore did not inherit every resident page: mapped=%d hits=%d want %d",
			mapped, attached.IdentityHits-before.IdentityHits, 2*pages)
	}
	if commands*8 >= mapped {
		t.Fatalf("restore used %d mapping commands for %d pages; want far fewer", commands, mapped)
	}
	if after.Faults != attached.Faults {
		t.Fatalf("the restored machine faulted %d times on inherited pages", after.Faults-attached.Faults)
	}
	if after.Loads != attached.Loads {
		t.Fatalf("the restored machine loaded %d times from its volumes", after.Loads-attached.Loads)
	}
	raw, err := json.Marshal(map[string]any{
		"pages_per_region": pages, "regions": 2, "page_size": size,
		"first_machine_loaded_pages":   firstCounted[0].loadedPages.Load() + firstCounted[1].loadedPages.Load(),
		"first_machine_backing_loads":  firstCounted[0].loads.Load() + firstCounted[1].loads.Load(),
		"second_machine_backing_loads": secondCounted[0].loads.Load() + secondCounted[1].loads.Load(),
		"second_machine_faults":        after.Faults - attached.Faults,
		"restore_mapped_pages":         mapped, "restore_map_commands": commands,
		"restore_mapping_runs": attached.MappingRuns - before.MappingRuns,
		"warm_resident_pages":  warm.ResidentPages,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SHARED_RESTORE_MEASUREMENT %s", raw)
	// The identities the two machines report for an untouched page are equal,
	// which is the only reason their pages are shared.
	for _, page := range []uint64{0, uint64(pages) / 2, uint64(pages) - 1} {
		var identities [2]control.Identity
		for side, v := range [][]*countedVolume{firstCounted, secondCounted} {
			extents, err := v[1].Locate(t.Context(), page*uint64(size), uint64(size))
			if err != nil || len(extents) != 1 {
				t.Fatalf("locate page %d: %v %v", page, extents, err)
			}
			identities[side] = extents[0].Identity
		}
		if identities[0] != identities[1] || identities[0].Ref.IsZero() {
			t.Fatalf("page %d does not inherit the whole page identity: %v", page, identities)
		}
	}
}

// A machine restored from a checkpoint, which then writes and checkpoints
// again, hands its own descendants the pages it inherited and the ones it
// changed.
func TestKVMNestedForkMapsResidentPagesBeforeItRuns(t *testing.T) {
	const pages = 32
	size := vmmemory.PageSize
	h := kernelHost(t, 6*pages, 12*pages)
	c := newPagerCluster(t)
	specs := []volume.VolumeSpec{
		{Name: "pmem0", Size: uint64(pages * size), PageSize: vmmemory.PageSize},
		{Name: "ram0", Size: uint64(pages * size), PageSize: vmmemory.PageSize},
	}
	names := []string{"pmem0", "ram0"}
	source, err := c.manager.Create(t.Context(), "source", specs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close(context.Background()) })
	for region, name := range names {
		data := make([]byte, pages*size)
		for i := range data {
			data[i] = byte(1 + region*32 + i/size)
		}
		for offset := 0; offset < len(data); offset += size {
			if err := source.Volume(name).Write(t.Context(), uint64(offset), data[offset:offset+size]); err != nil {
				t.Fatal(err)
			}
			if err := source.Checkpoint(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	point, err := source.ForkPoint(t.Context(), volume.Prepared(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	fork := func(id string, from *volume.ForkPoint) (*volume.VM, []vmmemory.Backing) {
		vm, err := c.manager.Fork(t.Context(), id, from)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = vm.Close(context.Background()) })
		var backing []vmmemory.Backing
		for _, name := range names {
			backing = append(backing, vm.Volume(name))
		}
		return vm, backing
	}
	touch := func(p *nativeProcess, written bool) {
		for region := range 2 {
			for page := range pages {
				value := 1 + region*32 + page
				if written && page == 0 {
					value = 91
				}
				p.request(fmt.Sprintf("kvmread %d %d", region, page*size), fmt.Sprintf("kvm %d", value))
			}
		}
	}
	_, ab := fork("a", point)
	a := startNative(t, h, pages, ab...)
	touch(a, false)
	check := func(backing []vmmemory.Backing, written bool) *nativeProcess {
		before, _ := h.Stats(t.Context())
		p := startNative(t, h, pages, backing...)
		attached, _ := h.Stats(t.Context())
		if attached.Loads != before.Loads || attached.MappedPages-before.MappedPages != 2*pages ||
			attached.IdentityHits-before.IdentityHits != 2*pages || attached.Mappings-before.Mappings > 2 {
			t.Fatalf("restore attach: before=%+v after=%+v", before, attached)
		}
		touch(p, written)
		after, _ := h.Stats(t.Context())
		if after.Loads != attached.Loads || after.Faults != attached.Faults {
			t.Fatalf("restored machine faulted on its first run: before=%+v after=%+v", attached, after)
		}
		return p
	}
	bv, bb := fork("b", point)
	b := check(bb, false)
	// What the guest stored reaches the nested fork through the seal: it freezes
	// each region's dirty set and the child reads those pages straight out of
	// the pager. The fork's own root index has to exist before it can be forked
	// again, so it publishes first.
	if err := bv.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	sources := make(map[string]volume.DirtySource, len(names))
	for region, name := range names {
		b.request(fmt.Sprintf("kvmwrite %d 0 91", region), "kvm 91")
		b.seal(region)
		sources[name] = b.region(region).Checkpoint()
	}
	nested, err := bv.ForkPoint(t.Context(), volume.Prepared(nil, sources))
	if err != nil {
		t.Fatal(err)
	}
	// Offering the pages the seal froze is what the host taking this child in
	// does before its regions attach: the child maps them rather than reading
	// the pages back.
	if err := nested.Share(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, cb := fork("c", nested)
	check(cb, true)
	touch(a, false)
}
