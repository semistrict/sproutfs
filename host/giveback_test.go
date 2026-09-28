package host_test

import (
	"testing"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// giveBackHost is a started host whose checkpoint loop and epoch watch are
// off, on a simulated clock, so the give-back is all the clock drives.
func giveBackHost(t *testing.T) (*hostHarness, *sim.Clock) {
	t.Helper()
	h := newSizedHostHarness(t, 1)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("host")
	h.configs[0].Clock = clock
	h.configs[0].CheckpointInterval = -1
	h.configs[0].EpochInterval = -1
	h.start(t)
	return h, clock
}

// giveBackVM is a VM on h whose guest has published pages [0, published) and
// runs on the host. Its RAM copies are made by the caller.
func giveBackVM(t *testing.T, h *hostHarness, published uint64) (*hostPagers, *machine) {
	t.Helper()
	pagers := newPager(t, h.configs[0].Resources)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The pages are published, so their resident pages hold an identity a
	// copy can be compared with.
	for page := range published {
		guest.store("ram0", page, 7)
	}
	capture(t, vm, guest)
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.hosts[0].RemoveMachine("vm-1") })
	return pagers, guest
}

// copyPages has the guest take pages [0, pages) writable and store nothing,
// which is what a cold read is where KVM asks for every page writable.
func copyPages(t *testing.T, guest *machine, pages uint64) {
	t.Helper()
	for page := range pages {
		if err := guest.memoryRegions["ram0"].Fault(t.Context(), page, true); err != nil {
			t.Fatal(err)
		}
	}
}

// advance moves the host's clock on by d and waits for the passes it started.
func advance(clock *sim.Clock, d time.Duration) {
	clock.Advance(d)
	clock.Settle()
}

func ramStats(t *testing.T, pagers *hostPagers) vmmemory.Stats {
	t.Helper()
	stats, err := pagers.ram().Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return stats
}

// RAM is never checkpointed on the interval, so a copy a write fault made of a
// page the guest never changed would stay private for the VM's life. The
// give-back interval gives it back instead, with the guest running, on a
// schedule of its own: a host that checkpoints nothing still does it.
func TestTheGiveBackIntervalGivesBackARAMCopyWithCheckpointsOff(t *testing.T) {
	h, clock := giveBackHost(t)
	pagers, guest := giveBackVM(t, h, 1)
	copyPages(t, guest, 1)
	// Short of the earliest the jittered interval can end, nothing happens.
	advance(clock, host.DefaultGiveBackInterval*7/8-time.Millisecond)
	if stats := ramStats(t, pagers); stats.GiveBackPasses != 0 {
		t.Fatalf("a pass ran before the interval: %d passes", stats.GiveBackPasses)
	}
	advance(clock, host.DefaultGiveBackInterval/4)
	if stats := ramStats(t, pagers); stats.GiveBackPasses != 1 || stats.GivenBackPages != 1 {
		t.Fatalf("the interval ran %d passes and gave back %d pages, want 1 and 1",
			stats.GiveBackPasses, stats.GivenBackPages)
	}
	if got := guest.load("ram0", 0)[0]; got != 7 {
		t.Fatalf("the page given back reads %d, want the 7 the guest stored", got)
	}
	region, err := guest.memoryRegions["ram0"].Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if region.PrivatePages != 0 {
		t.Fatalf("the guest still holds %d private RAM pages, want none", region.PrivatePages)
	}
}

// A VM that has made a pass's worth of copies is given back at once rather
// than holding them all until the interval.
func TestAPassWorthOfCopiesIsGivenBackBeforeTheInterval(t *testing.T) {
	defer host.SetGiveBackPages(2)()
	h, clock := giveBackHost(t)
	pagers, guest := giveBackVM(t, h, 2)
	// The guest copies both pages while it runs, and no time passes: the pass
	// they ask for runs at once.
	copyPages(t, guest, 2)
	advance(clock, 0)
	if stats := ramStats(t, pagers); stats.GiveBackPasses != 1 || stats.GivenBackPages != 2 {
		t.Fatalf("a pass's worth of copies ran %d passes and gave back %d pages with no time passing, want 1 and 2",
			stats.GiveBackPasses, stats.GivenBackPages)
	}
}

// A VM with no copy to give back costs no pass, however many intervals pass.
func TestAnIdleVMCostsNoGiveBackPass(t *testing.T) {
	h, clock := giveBackHost(t)
	pagers, _ := giveBackVM(t, h, 1)
	for range 5 {
		advance(clock, host.DefaultGiveBackInterval*9/8)
	}
	if stats := ramStats(t, pagers); stats.GiveBackPasses != 0 {
		t.Fatalf("an idle VM cost %d give-back passes over five intervals, want none", stats.GiveBackPasses)
	}
}
