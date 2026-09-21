package host_test

import (
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// TestForkHoldExpiresOnTheSimulatedClock: a fork's child is served the parent's
// sealed pages until the orchestrator says it has them all, and the deadline
// that retires the hold when that word never comes is four checkpoint intervals
// — four minutes of a deployment's time. Nothing could reach it: the only test
// of an expiring handover set a fifty-millisecond timeout of its own and then
// polled the wall clock for it, so the deadline a deployment actually runs was
// never the one under test, and a parent left sealed for good is exactly the
// failure this deadline exists for.
//
// With the host's clock injected the deployment's own bound is reached by
// advancing to it. No wall-clock time passes and nothing is polled.
func TestForkHoldExpiresOnTheSimulatedClock(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("source")
	h.configs[0].Clock = clock
	// The epoch watch sleeps on the same clock. Disabling it leaves the hold's
	// deadline as the only thing an advance can reach, which is what makes the
	// counts below assertions about the hold.
	h.configs[0].EpochInterval = -1
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 7)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1]); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 1 || serving[0] != "child" {
		t.Fatalf("the fork point is served for %v, want the child", serving)
	}
	if !vm.Status().Sealed {
		t.Fatal("a fork point does not hold the parent's pages")
	}

	// A minute short of the deadline changes nothing: a healthy destination is
	// still fetching the pages it inherited, and cutting it off would cost it
	// every write since the parent's last checkpoint.
	if released := clock.Advance(3 * time.Minute); released != 0 {
		t.Fatalf("released %d deadlines three minutes into a four-minute hold", released)
	}
	clock.Settle()
	if serving := h.hosts[0].Status().Serving; len(serving) != 1 {
		t.Fatalf("the hold was given up early: serving %v", serving)
	}

	// The deployment's own bound, reached without waiting for it.
	if released := clock.Advance(time.Minute); released != 1 {
		t.Fatalf("released %d deadlines at the hold's, want 1", released)
	}
	clock.Settle()
	if elapsed := clock.Since(sim.Epoch); elapsed != 4*time.Minute {
		t.Fatalf("the hold expired after %s of simulated time, want the four checkpoint intervals", elapsed)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("an expired fork hold still serves %v", serving)
	}
	// This is what the deadline exists for: a parent nothing can checkpoint,
	// fence or migrate takes its pages back and is durable again.
	if vm.Status().Sealed {
		t.Fatal("an expired fork hold left the parent sealed")
	}
	if err := vm.Checkpoint(t.Context()); err != nil {
		t.Fatalf("checkpointing the parent a retired point released: %v", err)
	}
}

// TestReleasingAForkHoldDisarmsItsDeadline: the release is the ordinary end of
// a hold, and a deadline left armed behind it would later fire into a host that
// had already given those pages up. Advancing past it must find nothing to do.
func TestReleasingAForkHoldDisarmsItsDeadline(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("source")
	h.configs[0].Clock = clock
	h.configs[0].EpochInterval = -1
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1]); err != nil {
		t.Fatal(err)
	}
	// The parent wrote nothing since the checkpoint its record selects, so the
	// point holds no page the child has to fetch and the release is the
	// ordinary word the orchestrator carries.
	if err := h.hosts[0].ReleaseMigrated("child"); err != nil {
		t.Fatal(err)
	}
	if got := clock.Pending(); got != 0 {
		t.Fatalf("a released hold left %d deadlines armed", got)
	}
	if released := clock.Advance(time.Hour); released != 0 {
		t.Fatalf("an hour past a released hold fired %d deadlines", released)
	}
	clock.Settle()
	if vm.Status().Sealed {
		t.Fatal("the released point still holds the parent's pages")
	}
}
