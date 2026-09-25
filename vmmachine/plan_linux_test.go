//go:build linux && (amd64 || arm64)

package vmmachine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// TestPlanBindsMemoryRegionsToTheirVolumes is the layout every start binds: the one
// RAM volume, one memory region per PMEM device, and each memory region attached with the
// volume it maps. A VM's other volumes, ram1 among them, are not memory regions.
func TestPlanBindsMemoryRegionsToTheirVolumes(t *testing.T) {
	vm := planVM(t)
	layout, err := planConfig(t, vm).plan()
	if err != nil {
		t.Fatal(err)
	}
	if layout.ram.name != RAMVolume || layout.ram.volume != vm.Volume(RAMVolume) {
		t.Fatalf("the plan maps %+v as RAM, want ram0", layout.ram)
	}
	if layout.ramBytes != 4<<20 {
		t.Fatalf("the plan boots with %d bytes of RAM, want %d", layout.ramBytes, 4<<20)
	}
	if layout.ram.backing.Kind != vmmemory.Ram || layout.ram.backing.Backing != vmmemory.Backing(layout.ram.volume) {
		t.Fatalf("RAM attaches with %#v rather than its own volume", layout.ram.backing)
	}
	if len(layout.pmem) != 1 || layout.pmem[0].name != "root" {
		t.Fatalf("the plan maps %d PMEM memory regions, want one named root: %+v", len(layout.pmem), layout.pmem)
	}
	if layout.pmem[0].backing.Kind != vmmemory.Pmem ||
		layout.pmem[0].backing.Backing != vmmemory.Backing(vm.Volume("root")) {
		t.Fatalf("the PMEM memory region attaches with %#v rather than its own volume", layout.pmem[0].backing)
	}
}

// TestPlanAttachesTheSuppliedBackings is what a migration's destination needs:
// the named memory regions read through the backing the receive supplied — the host
// that still holds their pages — while the volume stays their identity, and
// every memory region the configuration did not name keeps reading its own volume.
func TestPlanAttachesTheSuppliedBackings(t *testing.T) {
	vm := planVM(t)
	c := planConfig(t, vm)
	ram := &fakeBacking{size: 4 << 20}
	root := &fakeBacking{size: 4 << 20}
	c.Backings = map[string]vmmemory.Backing{RAMVolume: ram, "root": root}
	layout, err := c.plan()
	if err != nil {
		t.Fatal(err)
	}
	if layout.ram.backing.Backing != vmmemory.Backing(ram) || layout.ram.volume != vm.Volume(RAMVolume) {
		t.Fatalf("ram0 did not attach with the supplied backing over its own volume: %#v", layout.ram)
	}
	if layout.pmem[0].backing.Backing != vmmemory.Backing(root) || layout.pmem[0].volume != vm.Volume("root") {
		t.Fatalf("root did not attach with the supplied backing over its own volume: %#v", layout.pmem[0])
	}
	if layout.ramBytes != 4<<20 {
		t.Fatalf("a supplied backing changed the machine's RAM to %d bytes", layout.ramBytes)
	}
}

// TestPlanRefusesABackingItWouldNotUse covers every override a machine cannot
// honour. Each one would otherwise start a destination that faults from its own
// volumes without ever asking the host holding its pages.
func TestPlanRefusesABackingItWouldNotUse(t *testing.T) {
	vm := planVM(t)
	for _, test := range []struct {
		name     string
		backings map[string]vmmemory.Backing
		want     string
	}{
		{"a volume this machine does not map", map[string]vmmemory.Backing{"scratch": &fakeBacking{size: 4 << 20}},
			`vmmachine: planned maps no memory region named "scratch"`},
		{"no volume at all", map[string]vmmemory.Backing{"ram7": &fakeBacking{size: 4 << 20}},
			`vmmachine: planned maps no memory region named "ram7"`},
		{"a second RAM volume", map[string]vmmemory.Backing{"ram1": &fakeBacking{size: 4 << 20}},
			`vmmachine: planned maps no memory region named "ram1"`},
		{"a nil backing", map[string]vmmemory.Backing{RAMVolume: nil},
			`vmmachine: the backing of "ram0" is nil`},
		{"a backing of another size", map[string]vmmemory.Backing{RAMVolume: &fakeBacking{size: 2 << 20}},
			`vmmachine: the backing of "ram0" is 2097152 bytes, its volume is 4194304`},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := planConfig(t, vm)
			c.Backings = test.backings
			_, err := c.plan()
			if err == nil || err.Error() != test.want {
				t.Fatalf("plan reported %v, want %s", err, test.want)
			}
			if _, err := Start(t.Context(), c); err == nil || err.Error() != test.want {
				t.Fatalf("start reported %v, want %s", err, test.want)
			}
		})
	}
}

// TestPlanRefusesAMachineItCannotBind keeps the layout rules a start has always
// applied, now that one function decides them.
func TestPlanRefusesAMachineItCannotBind(t *testing.T) {
	vm := planVM(t)
	for _, test := range []struct {
		name   string
		modify func(*Config)
		want   string
	}{
		{"no pagers", func(c *Config) { c.Pagers = vmmemory.Pagers{} }, "vmmachine: invalid configuration"},
		{"only one pager", func(c *Config) { c.Pagers.Pmem = nil }, "vmmachine: invalid configuration"},
		{"no kernel to boot", func(c *Config) { c.KernelPath = "" }, "vmmachine: cold boot needs a kernel"},
		{"a PMEM device with no volume", func(c *Config) { c.Pmem = []Pmem{{ID: "missing"}} },
			`vmmachine: invalid PMEM device "missing"`},
		{"a PMEM device over a RAM memory region", func(c *Config) { c.Pmem = []Pmem{{ID: RAMVolume}} },
			`vmmachine: invalid PMEM device "ram0"`},
		{"two PMEM roots", func(c *Config) { c.Pmem = []Pmem{{ID: "root", Root: true}, {ID: "scratch", Root: true}} },
			"vmmachine: multiple PMEM roots"},
		{"a reserved vsock CID", func(c *Config) { c.VsockCID = 2 }, "vmmachine: vsock CID 2 is reserved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := planConfig(t, vm)
			test.modify(&c)
			_, err := c.plan()
			if err == nil || err.Error() != test.want {
				t.Fatalf("plan reported %v, want %s", err, test.want)
			}
		})
	}
}

// TestPlanRefusesAVMWithoutRAM is the one layout error a VM rather than a
// configuration causes.
func TestPlanRefusesAVMWithoutRAM(t *testing.T) {
	manager := planManager(t)
	vm, err := manager.Create(t.Context(), "diskless", []volume.VolumeSpec{{Name: "root", Size: 4 << 20, PageSize: checkpoint.PageSize2MiB}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Close(context.WithoutCancel(t.Context())) })
	c := planConfig(t, vm)
	if _, err := c.plan(); err == nil || err.Error() != "vmmachine: diskless has no volume named ram0" {
		t.Fatalf("plan reported %v, want a VM with no ram0", err)
	}
}

// planConfig is a configuration that would start planVM's machine, with a
// binary that does not exist: nothing here reaches an exec.
func planConfig(t *testing.T, vm *volume.VM) Config {
	return Config{Binary: "/nonexistent-sproutfs-vmm", SeccompFilter: "/nonexistent-sproutfs-seccomp",
		KernelPath: "/nonexistent-sproutfs-kernel", Pagers: planPagers(t), VM: vm,
		Pmem: []Pmem{{ID: "root", Root: true}}, VCPUs: 1}
}

// planArena is the shared page store a planning test's pagers are built over.
// Nothing here is ever read or written: a layout is decided before any memory
// is touched.
type planArena struct{}

func (planArena) Read(context.Context, int, []byte) error  { return nil }
func (planArena) Write(context.Context, int, []byte) error { return nil }
func (planArena) Release(context.Context, int) error       { return nil }

// planPagers is the pair of pagers a machine takes, each with the page this
// build's transport maps. A layout reads nothing from them but their pages,
// which is what a memory region's size has to be a whole number of.
func planPagers(t *testing.T) vmmemory.Pagers {
	t.Helper()
	disk := sim.New(sim.Config{}).NewDisk("pager", sim.DiskConfig{})
	build := func(name string) *vmmemory.Host {
		spill, err := disk.Open(t.Context(), name, platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = spill.Close() })
		h, err := vmmemory.New(t.Context(), testresource.New(), vmmemory.Config{
			PageSize: checkpoint.PageSize2MiB, ResidentPages: 1, LogicalPages: 64, DirtyPages: 1},
			planArena{}, spill)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	return vmmemory.Pagers{Ram: build("spill-ram"), Pmem: build("spill-pmem")}
}

// planVM is a VM with its RAM volume, a PMEM volume and two volumes no machine
// maps — "ram1" among them, since a machine has one RAM memory region — which is what
// an override must not name.
func planVM(t *testing.T) *volume.VM {
	t.Helper()
	vm, err := planManager(t).Create(t.Context(), "planned", []volume.VolumeSpec{
		{Name: RAMVolume, Size: 4 << 20, PageSize: checkpoint.PageSize2MiB},
		{Name: "ram1", Size: 4 << 20, PageSize: checkpoint.PageSize2MiB},
		{Name: "root", Size: 4 << 20, PageSize: checkpoint.PageSize2MiB},
		{Name: "scratch", Size: 4 << 20, PageSize: checkpoint.PageSize2MiB},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vm.Close(context.WithoutCancel(t.Context())) })
	return vm
}

// planManager is one volume manager over a simulated object store, which is all
// a layout needs: no memory region is ever attached here.
func planManager(t *testing.T) *volume.Manager {
	t.Helper()
	ctx := t.Context()
	runtime := sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Nanosecond,
		GetLatency: time.Nanosecond, PutLatency: time.Nanosecond, ListLatency: time.Nanosecond,
		DeleteLatency: time.Nanosecond, BytesPerSecond: 1 << 50}})
	client, err := control.NewClient(control.Config{ObjectStore: runtime.ObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: runtime.ObjectStore()})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := volume.NewManager(volume.Config{Control: client, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.WithoutCancel(ctx)) })
	return manager
}

// fakeBacking stands in for the peer backing a migration supplies: a plan only
// ever asks it how large it is.
type fakeBacking struct{ size uint64 }

var errFakeBacking = errors.New("vmmachine: the test backing was used")

func (b *fakeBacking) Size() uint64 { return b.size }

func (b *fakeBacking) Load(context.Context, uint64, []byte) error { return errFakeBacking }

func (b *fakeBacking) Verify(context.Context) error { return errFakeBacking }

func (b *fakeBacking) Locate(context.Context, uint64, uint64) ([]control.Extent, error) {
	return nil, errFakeBacking
}
