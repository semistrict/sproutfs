package host_test

import (
	"testing"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// termsHost is one host on a simulated clock whose own interval is an hour and
// whose loss window is ten minutes, in the host and in its pagers. configure
// adjusts its configuration before it starts.
func termsHost(t *testing.T, configure ...func(*host.Config)) (*hostHarness, *sim.Clock, *hostPagers) {
	t.Helper()
	h := newSizedHostHarness(t, 1)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("host")
	h.configs[0].Clock = clock
	h.configs[0].EpochInterval = -1
	h.configs[0].CheckpointInterval = time.Hour
	h.configs[0].LossWindow = 10 * time.Minute
	pagers := newMixedPagers(t, h.configs[0].Resources, func(cfg *vmmemory.Config) {
		cfg.Clock = clock
		cfg.LossWindow = 10 * time.Minute
	})
	h.configs[0].Pagers = pagers.pagers
	for _, adjust := range configure {
		adjust(&h.configs[0])
	}
	h.start(t)
	return h, clock, pagers
}

// termsVM runs one VM on terms of its own and counts its checkpoints.
func termsVM(t *testing.T, h *hostHarness, pagers *hostPagers, id string, terms host.MachineTerms) (*volume.VM, *machine, *countingMachine) {
	t.Helper()
	vm, err := h.hosts[0].Volumes().Create(t.Context(), id, mixedVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	counting := newCountingMachine(guest)
	if err := h.hosts[0].AddMachineWith(id, counting, terms); err != nil {
		t.Fatal(err)
	}
	return vm, guest, counting
}

// A VM that asks for an interval of its own is checkpointed on it, clamped to
// between the host's minimum and the host's own interval: it may ask for a
// tighter bound on what a host loss costs it, never a looser one. A VM that
// asks for nothing takes the host's.
func TestAVMIsCheckpointedOnAnIntervalOfItsOwn(t *testing.T) {
	h, clock, pagers := termsHost(t)
	_, _, tight := termsVM(t, h, pagers, "tight", host.MachineTerms{CheckpointInterval: 2 * time.Minute})
	_, _, plain := termsVM(t, h, pagers, "plain", host.MachineTerms{})
	for id, want := range map[string]time.Duration{"tight": 2 * time.Minute, "plain": time.Hour} {
		if got := h.hosts[0].CheckpointInterval(id); got != want {
			t.Fatalf("%s is checkpointed every %s, want %s", id, got, want)
		}
	}
	for _, clamped := range []struct {
		id    string
		asked time.Duration
		want  time.Duration
	}{
		{"too-tight", time.Millisecond, host.DefaultMinimumCheckpointInterval},
		{"too-loose", 5 * time.Hour, time.Hour},
	} {
		termsVM(t, h, pagers, clamped.id, host.MachineTerms{CheckpointInterval: clamped.asked})
		if got := h.hosts[0].CheckpointInterval(clamped.id); got != clamped.want {
			t.Fatalf("a VM asking for %s is checkpointed every %s, want %s", clamped.asked, got, clamped.want)
		}
	}
	// Each loop arms its timer on its own goroutine, so the clock moves a
	// second at a time until the VM on two minutes has its turn. Two minutes
	// and an eighth of jitter is well inside five, and the host's hour less an
	// eighth is well past it.
	for step := 0; tight.count() == 0; step++ {
		if step == 300 {
			t.Fatal("the VM on a two-minute interval had no turn in five minutes")
		}
		clock.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	if n := plain.count(); n != 0 {
		t.Fatalf("the VM on the host's hour-long interval was checkpointed %d times in five minutes", n)
	}
}

// A VM that asks for no interval takes no interval turns, is held to no loss
// window, and has every flush complete at once, however old its writes get. A
// VM beside it on the host's terms is held to the window.
func TestAVMThatAsksForNoIntervalIsHeldToNoWindow(t *testing.T) {
	h, clock, pagers := termsHost(t)
	_, loose, looseCount := termsVM(t, h, pagers, "loose", host.MachineTerms{CheckpointInterval: -1})
	_, plain, _ := termsVM(t, h, pagers, "plain", host.MachineTerms{})
	if got := h.hosts[0].CheckpointInterval("loose"); got != 0 {
		t.Fatalf("a VM that asked for no interval is checkpointed every %s", got)
	}
	loose.store("disk", 0, 7)
	plain.store("disk", 0, 7)
	// The store fails, so the plain VM's window turns can publish nothing and
	// its write keeps its age.
	h.runtime.ObjectStore().Fail()
	defer h.runtime.ObjectStore().Recover()
	clock.Advance(20 * time.Minute)
	if age, waiting := h.hosts[0].LossWindow("loose"); age < 20*time.Minute || waiting {
		t.Fatalf("the VM with no interval reports a %s window and waiting=%t, want its age and not waiting", age, waiting)
	}
	if _, waiting := h.hosts[0].LossWindow("plain"); !waiting {
		t.Fatal("the VM on the host's terms is past its window and not waiting")
	}
	select {
	case err := <-flush(loose):
		if err != nil {
			t.Fatalf("the flush failed: %v", err)
		}
	default:
		t.Fatal("a flush of the VM with no interval waited")
	}
	// Its stores are admitted past the window.
	loose.store("disk", 1, 8)
	if n := looseCount.count(); n != 0 {
		t.Fatalf("the VM with no interval was checkpointed %d times", n)
	}
}

// A VM's flush bound follows its own interval, two of them, where the host
// configures no bound of its own.
func TestAVMsFlushBoundFollowsItsInterval(t *testing.T) {
	h, clock, pagers := termsHost(t)
	_, guest, _ := termsVM(t, h, pagers, "tight", host.MachineTerms{CheckpointInterval: 5 * time.Minute})
	guest.store("disk", 0, 7)
	h.runtime.ObjectStore().Fail()
	defer h.runtime.ObjectStore().Recover()
	clock.Advance(8 * time.Minute)
	select {
	case err := <-flush(guest):
		if err != nil {
			t.Fatalf("the flush failed: %v", err)
		}
	default:
		t.Fatal("a flush waited on a write 8 minutes old, inside the VM's bound of two 5-minute intervals")
	}
	clock.Advance(4 * time.Minute)
	select {
	case err := <-flush(guest):
		t.Fatalf("a flush of a write 12 minutes old was answered while nothing could be published: %v", err)
	default:
	}
}

// The terms travel with the VM: a migration's handoff carries the interval it
// asked for, and the destination checkpoints it on that interval.
func TestAMigrationCarriesTheVMsInterval(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &received)
	// The destination runs its loop, an hour apart, so it has an interval to
	// resolve the VM's against.
	h.configs[1].CheckpointInterval = time.Hour
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachineWith("vm-1", source, host.MachineTerms{CheckpointInterval: 7 * time.Second}); err != nil {
		t.Fatal(err)
	}
	handoff, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	if handoff.CheckpointInterval != 7*time.Second {
		t.Fatalf("the handoff carries interval %s, want the 7s the VM asked for", handoff.CheckpointInterval)
	}
	taken, err := h.hosts[1].Receive(t.Context(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if got := h.hosts[1].CheckpointInterval("vm-1"); got != 7*time.Second {
		t.Fatalf("the destination checkpoints the VM every %s, want 7s", got)
	}
	if err := h.hosts[0].ReleaseMigrated("vm-1"); err != nil {
		t.Fatal(err)
	}
}
