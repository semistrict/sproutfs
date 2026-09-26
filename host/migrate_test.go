package host_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

const migrationPageSize = checkpoint.PageSize2MiB

// migrationVolumes is the VM every migration test runs: one RAM volume, which is
// all the host wiring needs to move.
var migrationVolumes = []volume.VolumeSpec{{Name: "ram0", Size: 8 * migrationPageSize, PageSize: migrationPageSize}}

// pageArena is the simulated page store of one host's pager.
type pageArena struct {
	mu    sync.Mutex
	slots [][]byte
}

// File is the one file this arena is. A pager makes exactly one.
func (a *pageArena) File(context.Context, int) (vmmemory.ArenaFile, error) { return a, nil }

func (a *pageArena) Read(_ context.Context, slot int, dst []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copy(dst, a.slots[slot])
	return nil
}

func (a *pageArena) Write(_ context.Context, slot int, src []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.slots[slot] != nil {
		return fmt.Errorf("write into allocated slot %d", slot)
	}
	a.slots[slot] = bytes.Clone(src)
	return nil
}

func (a *pageArena) Release(_ context.Context, slot int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.slots[slot] = nil
	return nil
}

type pageMapping struct {
	arena *pageArena
	mu    sync.Mutex
	pages map[uint64]int // page to slot, -1 for an explicit zero
	write map[uint64]bool
}

func newPageMapping(a *pageArena) *pageMapping {
	return &pageMapping{arena: a, pages: map[uint64]int{}, write: map[uint64]bool{}}
}

func (m *pageMapping) Map(_ context.Context, page uint64, file, slot, count int, writable bool) error {
	if file != 0 {
		return fmt.Errorf("map of file %d, and this arena has only file 0", file)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		m.pages[page+uint64(i)], m.write[page+uint64(i)] = slot+i, writable
	}
	return nil
}

func (m *pageMapping) MapZero(_ context.Context, page uint64, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		m.pages[page+uint64(i)], m.write[page+uint64(i)] = -1, false
	}
	return nil
}

func (m *pageMapping) Protect(_ context.Context, page uint64, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		if _, ok := m.pages[page+uint64(i)]; !ok {
			return fmt.Errorf("protect of unmapped page %d", page+uint64(i))
		}
		m.write[page+uint64(i)] = false
	}
	return nil
}

func (m *pageMapping) Revoke(_ context.Context, page uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pages, page)
	delete(m.write, page)
	return nil
}

func (m *pageMapping) Resolve(_ context.Context, page uint64, count int, writable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		if _, ok := m.pages[page+uint64(i)]; !ok || m.write[page+uint64(i)] != writable {
			return errors.New("invalid resolution")
		}
	}
	return nil
}

// machine is the VMM process one host runs for a VM: the migration Runtime over
// one memory region, and the stores a test makes through its mapping.
type machine struct {
	t             *testing.T
	memoryRegions map[string]*vmmemory.MemoryRegion
	maps          map[string]*pageMapping
	// closed says this machine's memory regions have been detached, and closing admits
	// the first Close: a hold this host expires closes the machine on its own
	// goroutine and the test's cleanup closes it too, so a test reads the flag
	// while another goroutine may be setting it.
	closed  atomic.Bool
	closing sync.Once
	// exit ends Wait, which is how this fake's process dies. It has no process
	// to lose on its own, so only a test that kills it through die closes it.
	exit    chan struct{}
	exitErr error
}

// hostPagers is one host's two pagers and the arena each of them owns, which is
// what a test builds because it is what a host builds: the two run their own
// pages over their own arenas and share nothing, so a memory region attaches to the
// pager of its kind or to none.
type hostPagers struct {
	pagers vmmemory.Pagers
	arenas map[vmmemory.MemoryRegionKind]*pageArena
}

func (p *hostPagers) ram() *vmmemory.Host  { return p.pagers.Ram }
func (p *hostPagers) pmem() *vmmemory.Host { return p.pagers.Pmem }

// close releases both pagers, which is what a test that has taken a host's
// memory away does before it looks at what is left.
func (p *hostPagers) close(ctx context.Context) error {
	return errors.Join(p.pagers.Ram.Close(ctx), p.pagers.Pmem.Close(ctx))
}

func newPager(t *testing.T, resources *resource.Budget) *hostPagers {
	t.Helper()
	return newPagerWithWriteAhead(t, resources, 0)
}

// mixedVolumes is a VM whose two memory regions are two geometries: its RAM in 4 KiB
// pages and its disk in 2 MiB pages, which is what a host's two pagers are for.
// The page counts are the same either way, so nothing about the VM is larger —
// only the bytes behind one of them.
var mixedVolumes = []volume.VolumeSpec{
	{Name: "ram0", Size: 8 * checkpoint.PageSize4KiB, PageSize: checkpoint.PageSize4KiB},
	{Name: "disk", Size: 8 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB},
}

// newMixedPagers is a host whose two pagers run different pages, which is the
// arrangement every byte this host reports across them has to survive: a page
// count of one says nothing about the other.
func newMixedPagers(t *testing.T, resources *resource.Budget, budget func(*vmmemory.Config)) *hostPagers {
	t.Helper()
	disk, err := adapters.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	built := &hostPagers{arenas: map[vmmemory.MemoryRegionKind]*pageArena{}}
	for _, kind := range []vmmemory.MemoryRegionKind{vmmemory.Ram, vmmemory.Pmem} {
		cfg := vmmemory.Config{PageSize: checkpoint.PageSize2MiB,
			ResidentPages: 32, LogicalPages: 64, DirtyPages: 32, ReadAheadPages: 1}
		if kind == vmmemory.Ram {
			cfg.PageSize = checkpoint.PageSize4KiB
		}
		if budget != nil {
			budget(&cfg)
		}
		spill, err := disk.Open(t.Context(), "spill-"+kind.String(), platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = spill.Close() })
		arena := &pageArena{slots: make([][]byte, cfg.ResidentPages)}
		built.arenas[kind] = arena
		pager, err := vmmemory.New(t.Context(), resources, cfg, arena, spill)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := pager.Close(context.Background()); err != nil {
				t.Errorf("close %s pager: %v", kind, err)
			}
		})
		if kind == vmmemory.Ram {
			built.pagers.Ram = pager
		} else {
			built.pagers.Pmem = pager
		}
	}
	return built
}

func newPagerWithWriteAhead(t *testing.T, resources *resource.Budget, writeAheadPages int) *hostPagers {
	t.Helper()
	return newPagerWithConfig(t, resources, vmmemory.Config{
		ResidentPages: 32, LogicalPages: 64, DirtyPages: 32, ReadAheadPages: 8, WriteAheadPages: writeAheadPages})
}

// newPagerWithConfig builds a host's pagers exactly as configured, which is how
// a test gives a guest a budget small enough to run into. Both run the
// production page, as a real host's two do, and each is given the whole of the
// configured budget: a test that wants a tight arena wants a tight arena of
// each kind.
func newPagerWithConfig(t *testing.T, resources *resource.Budget, cfg vmmemory.Config) *hostPagers {
	t.Helper()
	disk, err := adapters.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PageSize == 0 {
		cfg.PageSize = migrationPageSize
	}
	built := &hostPagers{arenas: map[vmmemory.MemoryRegionKind]*pageArena{}}
	for _, kind := range []vmmemory.MemoryRegionKind{vmmemory.Ram, vmmemory.Pmem} {
		spill, err := disk.Open(t.Context(), "spill-"+kind.String(), platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = spill.Close() })
		arena := &pageArena{slots: make([][]byte, cfg.ResidentPages)}
		built.arenas[kind] = arena
		pager, err := vmmemory.New(t.Context(), resources, cfg, arena, spill)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := pager.Close(context.Background()); err != nil {
				t.Errorf("close %s pager: %v", kind, err)
			}
		})
		if kind == vmmemory.Ram {
			built.pagers.Ram = pager
		} else {
			built.pagers.Pmem = pager
		}
	}
	return built
}

func newMachine(t *testing.T, p *hostPagers, vm *volume.VM,
	backings map[string]vmmemory.Backing) (*machine, error) {
	m := &machine{t: t, memoryRegions: map[string]*vmmemory.MemoryRegion{}, maps: map[string]*pageMapping{},
		exit: make(chan struct{})}
	for _, v := range vm.Volumes() {
		var backing vmmemory.Backing = v
		if supplied, ok := backings[v.Name()]; ok {
			backing = supplied
		}
		// One RAM volume and PMEM for the rest, which is the shape a real
		// machine binds; the kind is the attacher's to state, and it decides
		// which pager and which arena the memory region belongs to.
		kind := vmmemory.Pmem
		if v.Name() == "ram0" {
			kind = vmmemory.Ram
		}
		mapping := newPageMapping(p.arenas[kind])
		memoryRegion, err := p.pagers.For(kind).Attach(t.Context(),
			vmmemory.MemoryRegionBacking{Kind: kind, Backing: backing}, mapping)
		if err != nil {
			return nil, err
		}
		m.memoryRegions[v.Name()], m.maps[v.Name()] = memoryRegion, mapping
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, nil
}

func (m *machine) MemoryRegions() map[string]*vmmemory.MemoryRegion { return m.memoryRegions }

// Wait reports the end of this machine's VMM process, which for a fake with no
// process is the death a test stages through die.
func (m *machine) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-m.exit:
		return m.exitErr
	}
}

// die is the VMM process ending on its own: the host killed it because its
// pager session failed, or the kernel did, and err is why.
func (m *machine) die(err error) {
	m.exitErr = err
	close(m.exit)
}

// Prepare is a capture's pause: the VMM state is captured and every memory region
// seals the pages the checkpoint will publish.
func (m *machine) Prepare(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
	sources := make(map[string]volume.DirtySource, len(m.memoryRegions))
	for _, name := range slices.Sorted(maps.Keys(m.memoryRegions)) {
		if err := m.memoryRegions[name].Seal(ctx); err != nil {
			return nil, nil, err
		}
		sources[name] = m.memoryRegions[name].Checkpoint()
	}
	return []byte("vmm-state"), sources, nil
}

// SealDisks is a disk checkpoint's pause: the memory regions of the machine's disks
// seal, and its RAM and its state are left alone.
func (m *machine) SealDisks(ctx context.Context) (map[string]volume.DirtySource, error) {
	sources := map[string]volume.DirtySource{}
	for _, name := range slices.Sorted(maps.Keys(m.memoryRegions)) {
		if m.memoryRegions[name].Kind() != vmmemory.Pmem {
			continue
		}
		if err := m.memoryRegions[name].Seal(ctx); err != nil {
			return nil, err
		}
		sources[name] = m.memoryRegions[name].Checkpoint()
	}
	return sources, nil
}

// Stop is a migration's pause: it seals nothing, because the pages it leaves
// behind are what the destination fetches.
func (m *machine) Stop(context.Context) ([]byte, error) { return []byte("vmm-state"), nil }

// checkpoint is this machine's interval checkpoint: the pause, the seal and the
// publication of the sealed pages, which is the only thing that makes its
// guest's memory durable.
func (m *machine) checkpoint(ctx context.Context, vm *volume.VM) error {
	checkpoint, err := host.Capture(ctx, vm, m, nil)
	if err != nil {
		return err
	}
	return checkpoint.Wait(ctx)
}

func (m *machine) Resume(context.Context) error { return nil }

func (m *machine) Release(ctx context.Context) error {
	for _, name := range slices.Sorted(maps.Keys(m.memoryRegions)) {
		memoryRegion := m.memoryRegions[name]
		if err := memoryRegion.Unseal(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (m *machine) Close() error {
	var errs []error
	m.closing.Do(func() {
		defer m.closed.Store(true)
		for _, name := range slices.Sorted(maps.Keys(m.memoryRegions)) {
			memoryRegion := m.memoryRegions[name]
			// The memory region gives its pages up first. Until it has, the pager may
			// still be mapping, protecting and revoking through this mapping —
			// a capture sealing it, a fault installing a window — and emptying
			// the mapping under that is a write beside their reads. Emptying it
			// afterwards is what a VMM process's address space going away is,
			// and it is done under the mapping's own lock for the same reason.
			errs = append(errs, memoryRegion.Detach(context.Background()))
			mapping := m.maps[name]
			mapping.mu.Lock()
			clear(mapping.pages)
			clear(mapping.write)
			mapping.mu.Unlock()
		}
	})
	return errors.Join(errs...)
}

// store writes one page the way a guest does, faulting for write first.
func (m *machine) store(name string, page uint64, value byte) {
	m.t.Helper()
	mapping := m.maps[name]
	for range 8 {
		mapping.mu.Lock()
		slot, mapped := mapping.pages[page]
		writable := mapping.write[page]
		if mapped && writable && slot >= 0 {
			mapping.arena.mu.Lock()
			for i := range mapping.arena.slots[slot] {
				mapping.arena.slots[slot][i] = value
			}
			mapping.arena.mu.Unlock()
			mapping.mu.Unlock()
			return
		}
		mapping.mu.Unlock()
		if err := m.memoryRegions[name].Fault(m.t.Context(), page, true); err != nil {
			m.t.Fatal(err)
		}
	}
	m.t.Fatalf("store on %s page %d never resolved", name, page)
}

// load reads one page through the memory region, which is what a destination's guest
// faults on.
func (m *machine) load(name string, page uint64) []byte {
	m.t.Helper()
	mapping := m.maps[name]
	mapping.mu.Lock()
	slot, mapped := mapping.pages[page]
	mapping.mu.Unlock()
	if !mapped {
		if err := m.memoryRegions[name].Fault(m.t.Context(), page, false); err != nil {
			m.t.Fatal(err)
		}
		mapping.mu.Lock()
		slot, mapped = mapping.pages[page]
		mapping.mu.Unlock()
		if !mapped {
			m.t.Fatalf("%s page %d unmapped after a fault", name, page)
		}
	}
	if slot < 0 {
		return make([]byte, migrationPageSize)
	}
	mapping.arena.mu.Lock()
	defer mapping.arena.mu.Unlock()
	return bytes.Clone(mapping.arena.slots[slot])
}

// startMigrationHosts starts two hosts that can migrate to one another, each
// with pagers of its own.
func startMigrationHosts(t *testing.T) (*hostHarness, []*hostPagers) {
	t.Helper()
	return startMigrationHostsWithWriteAhead(t, 0)
}

func startMigrationHostsWithWriteAhead(t *testing.T, sourceWriteAheadPages int) (*hostHarness, []*hostPagers) {
	t.Helper()
	// A VM's log is placed on three hosts other than the one that writes it, so
	// a migration test needs four.
	h := newSizedHostHarness(t, 4)
	pagers := make([]*hostPagers, len(h.configs))
	for i := range h.configs {
		writeAheadPages := 0
		if i == 0 {
			writeAheadPages = sourceWriteAheadPages
		}
		pagers[i] = newPagerWithWriteAhead(t, h.configs[i].Resources, writeAheadPages)
		h.configs[i].Migration = host.MigrationConfig{Address: h.pages[i], PageSize: migrationPageSize}
	}
	return h, pagers
}

// starter is the destination's StartVM: it attaches every memory region of the received
// VM through the backings the migration supplies and returns the running machine.
func starter(t *testing.T, pagers *hostPagers, out **machine) host.StartFunc {
	return func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (host.Machine, error) {
		if string(state) != "vmm-state" {
			return nil, fmt.Errorf("the destination restored %q", state)
		}
		built, err := newMachine(t, pagers, vm, backings)
		if err != nil {
			return nil, err
		}
		*out = built
		return built, nil
	}
}

// starters is starter for a host that takes more than one VM in — a fan-out of
// forks it is the destination of — recording every machine it starts by the VM
// it started it for.
func starters(t *testing.T, pagers *hostPagers, out map[string]*machine) host.StartFunc {
	return func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (host.Machine, error) {
		if string(state) != "vmm-state" {
			return nil, fmt.Errorf("the destination restored %q", state)
		}
		built, err := newMachine(t, pagers, vm, backings)
		if err != nil {
			return nil, err
		}
		out[vm.ID()] = built
		return built, nil
	}
}

// TestMigrationWaitsForTheIntervalCheckpointItInterrupts: a checkpoint that is
// still publishing owns the guest's sealed memory regions, and a handoff that found
// one sealed would have to give the migration up and resume the guest. Stopping
// the checkpoint loop has to mean the checkpoint it had in flight has landed,
// not merely that the loop noticed it should stop.
//
// The store publishes slowly and the interval is short, and the migration below
// begins on the store's word that a publication has started, so it always
// begins with a checkpoint in flight.
func TestMigrationWaitsForTheIntervalCheckpointItInterrupts(t *testing.T) {
	const publish = 200 * time.Millisecond
	h := newSizedHostHarnessOn(t, 4, sim.ObjectStoreConfig{GetLatency: time.Nanosecond,
		PutLatency: publish, ListLatency: time.Nanosecond, BytesPerSecond: 1 << 60})
	publishing := &putWatcher{ObjectStore: h.configs[0].ObjectStore}
	h.configs[0].ObjectStore = publishing
	pagers := make([]*hostPagers, len(h.configs))
	for i := range h.configs {
		pagers[i] = newPagerWithWriteAhead(t, h.configs[i].Resources, 0)
		// The handover is held for as long as a real one is: the destination
		// fetches the guest's RAM from here, which no checkpoint publishes.
		h.configs[i].Migration = host.MigrationConfig{Address: h.pages[i], PageSize: migrationPageSize,
			HoldTimeout: 4 * host.DefaultCheckpointInterval}
		h.configs[i].CheckpointInterval = 5 * time.Millisecond
	}
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &received)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(4) {
		source.store("ram0", page, byte(page+1))
		source.store("disk", page, byte(page+11))
	}
	started := publishing.next()
	if err := h.hosts[0].AddMachine("vm-1", source); err != nil {
		t.Fatal(err)
	}
	// The next write this host makes is the interval checkpoint's, and it takes
	// the store a publication to answer. Migrating the moment it begins puts
	// the handoff inside that publication rather than a measured distance into
	// it.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the interval checkpoint never began publishing")
	}

	handoff, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
	if err != nil {
		t.Fatalf("migrating over an interval checkpoint: %v", err)
	}
	if handoff.VMID != "vm-1" {
		t.Fatalf("handoff: %+v", handoff)
	}
	taken, err := h.hosts[1].Receive(t.Context(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if err := taken.Done(t.Context()); err != nil {
		t.Fatal(err)
	}
	if machines := h.hosts[1].Machines(); len(machines) != 1 || machines[0] != "vm-1" {
		t.Fatalf("the destination does not run the VM: %v", machines)
	}
	if got := received.load("ram0", 3); got[0] != 4 {
		t.Fatalf("the destination reads RAM page 3 as %d, want the guest's 4", got[0])
	}
	if got := received.load("disk", 3); got[0] != 14 {
		t.Fatalf("the destination reads disk page 3 as %d, want the guest's 14", got[0])
	}
}

// TestHostMigratesAVMToAnotherHost is the host wiring end to end: two admitted
// hosts over real TCP, one VM's pages fetched from the source's page server
// under the deployment's own mutual authentication.
func TestHostMigratesAVMToAnotherHost(t *testing.T) {
	for _, tc := range []struct {
		name            string
		writeAheadPages int
		heldPages       int
	}{
		{name: "one_page", writeAheadPages: 1, heldPages: 4},
		{name: "six_pages", writeAheadPages: 6, heldPages: 6},
		// A zero WriteAheadPages selects one page, so the default holds only
		// the pages the guest stored into.
		{name: "default", heldPages: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pagers := startMigrationHostsWithWriteAhead(t, tc.writeAheadPages)
			var received *machine
			h.configs[1].Migration.StartVM = starter(t, pagers[1], &received)
			h.start(t)

			vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
			if err != nil {
				t.Fatal(err)
			}
			source, err := newMachine(t, pagers[0], vm, nil)
			if err != nil {
				t.Fatal(err)
			}
			for page := range uint64(4) {
				source.store("ram0", page, byte(page+1))
			}
			// Write-ahead pages are held even when the guest has not stored into
			// them. Assert the independently expected set before migration starts.
			wantResident := make([]uint64, tc.heldPages)
			for page := range wantResident {
				wantResident[page] = uint64(page)
			}
			if got, err := source.memoryRegions["ram0"].Resident(); err != nil || !slices.Equal(got, wantResident) {
				t.Fatalf("source resident pages: got %v, %v, want %v", got, err, wantResident)
			}
			if err := h.hosts[0].AddMachine("vm-1", source); err != nil {
				t.Fatal(err)
			}
			if machines := h.hosts[0].Machines(); len(machines) != 1 || machines[0] != "vm-1" {
				t.Fatalf("host machines: %v", machines)
			}

			handoff, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
			if err != nil {
				t.Fatal(err)
			}
			if handoff.Source != h.pages[0] || handoff.PageSize != migrationPageSize {
				t.Fatalf("handoff: %+v", handoff)
			}
			if machines := h.hosts[0].Machines(); len(machines) != 0 {
				t.Fatalf("a migrated VM is still running here: %v", machines)
			}

			taken, err := h.hosts[1].Receive(t.Context(), handoff)
			if err != nil {
				t.Fatal(err)
			}
			defer taken.Close()
			if err := taken.Done(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Done is only about the pages no checkpoint holds. What the source
			// served behind them is what the counts below are measured over.
			if err := taken.Streamed(t.Context()); err != nil {
				t.Fatal(err)
			}
			if machines := h.hosts[1].Machines(); len(machines) != 1 || machines[0] != "vm-1" {
				t.Fatalf("the destination does not run the VM: %v", machines)
			}
			// The handoff published nothing, so the destination ends up holding
			// exactly the pages the source held and nothing else: the rest of the
			// volume is a hole its own reads resolve.
			if got, err := received.memoryRegions["ram0"].Resident(); err != nil || !slices.Equal(got, wantResident) {
				t.Fatalf("destination resident pages: got %v, %v, want %v", got, err, wantResident)
			}
			// Every one of them is this host's own state now, which its next checkpoint
			// publishes: the source published none of them.
			if got, err := received.memoryRegions["ram0"].Unpublished(); err != nil || !slices.Equal(got, wantResident) {
				t.Fatalf("destination unpublished pages: got %v, %v, want %v", got, err, wantResident)
			}
			if stats := taken.Stats(); stats.Unpublished != int64(tc.heldPages) || stats.Fetched != stats.Unpublished {
				t.Fatalf("post-copy fetched %d of %d unpublished pages, want %d of %d",
					stats.Fetched, stats.Unpublished, tc.heldPages, tc.heldPages)
			}
			// All held pages, including speculative zeros, arrive exactly once over
			// the network; the rest of the published page the
			// destination reads from its own volume.
			checkFetches := func() {
				t.Helper()
				if stats := taken.Stats(); stats.PeerPages != int64(tc.heldPages) ||
					stats.Streamed != int64(tc.heldPages) || !stats.Complete || stats.StreamError != nil {
					t.Fatalf("post-copy stats: %+v; want the %d pages the source held, fetched once", stats, tc.heldPages)
				}
				if status := h.hosts[0].Status(); status.Pages.Served != int64(tc.heldPages) || len(status.Serving) != 1 {
					t.Fatalf("the source served %+v for %v", status.Pages, status.Serving)
				}
			}
			checkFetches()
			for page := range uint64(8) {
				value := byte(0)
				if page < 4 {
					value = byte(page + 1)
				}
				want := bytes.Repeat([]byte{value}, migrationPageSize)
				if got := received.load("ram0", page); !bytes.Equal(got, want) {
					t.Fatalf("page %d: got %d..., want %d...", page, got[0], want[0])
				}
			}
			checkFetches()
			// The destination's own checkpoint is what makes the pages it fetched
			// durable; only then does its volume hold the migrated guest.
			if err := received.checkpoint(t.Context(), taken.VM()); err != nil {
				t.Fatal(err)
			}
			for page := range uint64(8) {
				value := byte(0)
				if page < 4 {
					value = byte(page + 1)
				}
				want := bytes.Repeat([]byte{value}, migrationPageSize)
				got := make([]byte, migrationPageSize)
				if err := taken.VM().Volume("ram0").Read(t.Context(), page*migrationPageSize, got); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("durable page %d: got %d..., want %d...", page, got[0], want[0])
				}
			}
			if err := h.hosts[0].ReleaseMigrated("vm-1"); err != nil {
				t.Fatal(err)
			}
			if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
				t.Fatalf("a released source still serves %v", serving)
			}
		})
	}
}

// TestHostDrainMovesEveryVM is what the preStop hook calls: every VM this host
// runs moves, and the host is left running none.
func TestHostDrainMovesEveryVM(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	h.configs[0].Migration.DrainConcurrency = 1
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &received)
	h.start(t)

	for _, id := range []string{"vm-a", "vm-b"} {
		vm, err := h.hosts[0].Volumes().Create(t.Context(), id, migrationVolumes)
		if err != nil {
			t.Fatal(err)
		}
		source, err := newMachine(t, pagers[0], vm, nil)
		if err != nil {
			t.Fatal(err)
		}
		source.store("ram0", 0, byte(len(id)))
		if err := h.hosts[0].AddMachine(id, source); err != nil {
			t.Fatal(err)
		}
	}
	destination := h.pages[1]
	handoffs, err := h.hosts[0].Drain(t.Context(), func(string) platform.Address { return destination })
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 2 || handoffs[0].VMID != "vm-a" || handoffs[1].VMID != "vm-b" {
		t.Fatalf("drain moved %+v", handoffs)
	}
	if machines := h.hosts[0].Machines(); len(machines) != 0 {
		t.Fatalf("a drained host still runs %v", machines)
	}
	for _, handoff := range handoffs {
		taken, err := h.hosts[1].Receive(t.Context(), handoff)
		if err != nil {
			t.Fatal(err)
		}
		if err := taken.Done(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := h.hosts[0].ReleaseMigrated(handoff.VMID); err != nil {
			t.Fatal(err)
		}
	}
	if machines := h.hosts[1].Machines(); len(machines) != 2 {
		t.Fatalf("the destination runs %v", machines)
	}
}

func TestHostRejectsMachineUsingAnotherResourceBudget(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "resource-owner", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	wrong, err := newMachine(t, pagers[1], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine(vm.ID(), wrong); !errors.Is(err, host.ErrInvalidConfig) {
		t.Fatalf("registered a machine outside host accounting: %v", err)
	}
	if wrong.closed.Load() || len(h.hosts[0].Machines()) != 0 {
		t.Fatal("registration changed ownership of a rejected machine")
	}
	if err := wrong.Close(); err != nil {
		t.Fatal(err)
	}
	correct, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine(vm.ID(), correct); err != nil {
		t.Fatal(err)
	}
	before := h.hosts[0].Status().Resources.Used
	correct.store("ram0", 0, 17)
	if h.hosts[0].Resources() != pagers[0].ram().Resources() || h.hosts[0].Status().Resources.Used <= before {
		t.Fatal("guest pages did not enter the storage host's resource accounting")
	}
	if err := correct.Close(); err != nil {
		t.Fatal(err)
	}
	h.hosts[0].RemoveMachine(vm.ID())
	if after := h.hosts[0].Status().Resources.Used; after != before {
		t.Fatalf("detached machine retained memory: %d -> %d", before, after)
	}
}

func TestReceiveClosesMachineWithMismatchedResourceBudget(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var rejected *machine
	// The callback accidentally selects a third host's pager.
	h.configs[1].Migration.StartVM = starter(t, pagers[2], &rejected)
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "wrong-destination-budget", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	source.store("ram0", 0, 23)
	if err := h.hosts[0].AddMachine(vm.ID(), source); err != nil {
		t.Fatal(err)
	}
	handoff, err := h.hosts[0].Migrate(t.Context(), vm.ID(), h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].ReleaseMigrated(vm.ID())
	before := h.hosts[0].Status().Pages.Served
	if received, err := h.hosts[1].Receive(t.Context(), handoff); received != nil || !errors.Is(err, host.ErrInvalidConfig) {
		t.Fatalf("received a machine with a different budget: %v %v", received, err)
	}
	if rejected == nil || !rejected.closed.Load() || len(h.hosts[1].Machines()) != 0 || len(h.hosts[1].Volumes().VMs()) != 0 {
		t.Fatal("rejected receive leaked a process, registration or volume handle")
	}
	if got := h.hosts[0].Status().Pages.Served; got != before {
		t.Fatalf("rejected receive streamed source pages: %d -> %d", before, got)
	}
	if err := pagers[2].close(t.Context()); err != nil {
		t.Fatalf("rejected runtime kept memory regions attached: %v", err)
	}
}

// TestMigratedPagesAreReleasedAfterTheirDeadline: a handover is in flight
// until something reports the destination has every page no checkpoint holds,
// and that word comes from the orchestrator. An orchestrator that restarted
// mid-migration never says it, and before the deadline below the source served
// those pages — and held the VMM process that owns them — for as long as it
// ran. A fork hold has had a deadline all along; a migration's did not.
func TestMigratedPagesAreReleasedAfterTheirDeadline(t *testing.T) {
	// The receive below has to finish inside the deadline, so the deadline
	// leaves it room on a loaded machine; the release after it is waited for.
	const holdTimeout = 500 * time.Millisecond
	h, pagers := startMigrationHosts(t)
	h.configs[0].Migration.HoldTimeout = holdTimeout
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &received)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "abandoned", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	source.store("ram0", 0, 29)
	if err := h.hosts[0].AddMachine("abandoned", source); err != nil {
		t.Fatal(err)
	}
	handoff, err := h.hosts[0].Migrate(t.Context(), "abandoned", h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	taken, err := h.hosts[1].Receive(t.Context(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if err := taken.Done(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Nothing releases it: this is the orchestrator that went away.
	awaitReleased(t, h.hosts[0], source)
}

// awaitReleased waits for a host to be serving nothing and for the processes
// named to be closed, which together are what a released handover leaves
// behind: the page server is given up first and the VMM process last, so a
// wait on the serving set alone can return before the close lands.
func awaitReleased(t *testing.T, host *host.Host, closed ...*machine) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		serving := host.Status().Serving
		open := 0
		for _, m := range closed {
			if !m.closed.Load() {
				open++
			}
		}
		if len(serving) == 0 && open == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the source still serves %v with %d processes open and nothing waiting on it", serving, open)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestReceivedGuestIsDiscardedWhenItsPostCopyFails: the pages no checkpoint
// holds exist only on the source, so a post-copy that cannot fetch them leaves
// a guest whose memory is part this host's and part missing — a torn image.
// Registering it started its checkpoint loop, so the next interval published
// that image over the checkpoint the control record selects, and every later
// reader of the VM read it. Nothing here is publishable: the VM is given up
// without publishing, and its last checkpoint is what a recovery opens.
func TestReceivedGuestIsDiscardedWhenItsPostCopyFails(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &received)
	forgotten := make(chan string, 1)
	h.configs[1].MachineClosed = func(id string) { forgotten <- id }
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "torn", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Page 0 is durable and page 1 is not, so the destination has to fetch one
	// page from the source to be the guest that was handed over.
	source.store("ram0", 0, 7)
	if err := source.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	source.store("ram0", 1, 9)
	if err := h.hosts[0].AddMachine("torn", source); err != nil {
		t.Fatal(err)
	}
	handoff, err := h.hosts[0].Migrate(t.Context(), "torn", h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	// The source gives the pages up before the destination has them, which is
	// every way a post-copy loses its pages at once.
	if err := h.hosts[0].Abandon("torn"); err != nil {
		t.Fatal(err)
	}
	taken, err := h.hosts[1].Receive(t.Context(), handoff)
	if err == nil {
		taken.Close()
		t.Fatal("the destination took a guest whose unpublished pages it never got")
	}
	if taken != nil {
		t.Fatalf("a failed receive returned %v", taken)
	}
	if machines := h.hosts[1].Machines(); len(machines) != 0 {
		t.Fatalf("the destination still runs %v, which its checkpoint loop would publish", machines)
	}
	if vms := h.hosts[1].Volumes().VMs(); len(vms) != 0 {
		t.Fatalf("the destination still holds %d handles on a torn guest", len(vms))
	}
	if received == nil || !received.closed.Load() {
		t.Fatal("the destination left the torn guest's VMM process running")
	}
	select {
	case id := <-forgotten:
		if id != "torn" {
			t.Fatalf("the destination forgot %q", id)
		}
	default:
		t.Fatal("the destination never told its supervisor to forget the torn guest")
	}
	// A recovery opens the checkpoint the record selects, which is the guest as
	// its source last published it and nothing of the torn image.
	recovered, err := h.hosts[2].Volumes().Open(t.Context(), "torn")
	if err != nil {
		t.Fatal(err)
	}
	page := make([]byte, migrationPageSize)
	if err := recovered.Volume("ram0").Read(t.Context(), 0, page); err != nil {
		t.Fatal(err)
	}
	if page[0] != 7 {
		t.Fatalf("the recovered page 0 reads %d, want the durable 7", page[0])
	}
	if err := recovered.Volume("ram0").Read(t.Context(), migrationPageSize, page); err != nil {
		t.Fatal(err)
	}
	if page[0] != 0 {
		t.Fatalf("the recovered page 1 reads %d, want the zeroes of a page no checkpoint holds", page[0])
	}
	if err := recovered.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestDrainReturnsWithinItsOwnDeadline: a drain hands its VMs to the
// orchestrator, which drives both halves of every migration. An orchestrator
// that is wedged — restarting, or waiting on a host that no longer answers —
// answers none of those calls, and the drain has no bound of its own: it is a
// preStop hook, so what is waiting on the other end is a termination grace
// period after which the pod is killed with every page it still holds. The
// bound has to be the drain's, because the caller's context carries none.
func TestDrainReturnsWithinItsOwnDeadline(t *testing.T) {
	const (
		whole = 200 * time.Millisecond
		perVM = 50 * time.Millisecond
	)
	h, pagers := startMigrationHosts(t)
	h.start(t)
	for _, id := range []string{"vm-a", "vm-b", "vm-c"} {
		vm, err := h.hosts[0].Volumes().Create(t.Context(), id, migrationVolumes)
		if err != nil {
			t.Fatal(err)
		}
		guest, err := newMachine(t, pagers[0], vm, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.hosts[0].AddMachine(id, guest); err != nil {
			t.Fatal(err)
		}
	}
	// The wedged orchestrator: every call waits, and nothing but the drain's own
	// deadline ends the wait. It is released at the end of the test so that a
	// drain which never returned leaves no goroutine behind.
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	move := func(ctx context.Context, vmID string) error {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-released:
			return errors.New("the test released the wedged orchestrator")
		}
	}

	type outcome struct {
		moved, remaining []string
		err              error
	}
	done := make(chan outcome, 1)
	began := time.Now()
	go func() {
		moved, remaining, err := h.hosts[0].DrainVia(t.Context(),
			host.DrainBudget{Timeout: whole, PerVM: perVM, Concurrency: 2}, move)
		done <- outcome{moved, remaining, err}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("the drain never returned: a wedged orchestrator holds it past its %s deadline", whole)
	}
	if took := time.Since(began); took > 5*whole {
		t.Fatalf("the drain took %s, want its own %s deadline", took, whole)
	}
	if len(got.moved) != 0 {
		t.Fatalf("a wedged orchestrator moved %v", got.moved)
	}
	if !slices.Equal(got.remaining, []string{"vm-a", "vm-b", "vm-c"}) {
		t.Fatalf("the drain left %v behind, want every VM", got.remaining)
	}
	if got.err == nil {
		t.Fatal("a drain that moved nothing reported no failure")
	}
	// Every VM that did not move is still running here, still checkpointed.
	if machines := h.hosts[0].Machines(); len(machines) != 3 {
		t.Fatalf("a failed drain left %v running", machines)
	}
}
