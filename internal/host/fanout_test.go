package host_test

import (
	"slices"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// TestAFanOutNothingReceivesExpiresEveryHold: a fan-out is one instant and one
// hold per child, and a request that fails part way through taking its children
// in leaves every one of those holds behind. Each of them carries the deadline
// a handover gets, so the parent takes its frames back within it and is
// checkpointed again — which is the whole reason the deadline exists.
//
// The releases the orchestrator carries are refused first, exactly as they are
// for a child nothing ever fetched the pages of: what is under test is that the
// deadline is what ends a hold no release can.
func TestAFanOutNothingReceivesExpiresEveryHold(t *testing.T) {
	h, pagers, arenas := startMigrationHosts(t)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("source")
	h.configs[0].Clock = clock
	// The epoch watch sleeps on the same clock. Disabling it leaves the holds'
	// deadlines as the only thing an advance can reach.
	h.configs[0].EpochInterval = -1
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], arenas[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 7)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	children := []string{"child-a", "child-b", "child-c"}
	if _, err := h.hosts[0].Fork(t.Context(), "parent", children, h.pages[1]); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; !slices.Equal(serving, children) {
		t.Fatalf("the fan-out serves %v, want every child", serving)
	}
	// Nothing fetched what the instant holds, so the release the orchestrator
	// carries is refused for every one of them: the pages exist nowhere else.
	for _, child := range children {
		if err := h.hosts[0].ReleaseMigrated(child); err == nil {
			t.Fatalf("releasing %s with its inherited pages outstanding was allowed", child)
		}
	}
	if released := clock.Advance(4 * time.Minute); released != len(children) {
		t.Fatalf("released %d deadlines at the holds', want %d", released, len(children))
	}
	clock.Settle()
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("expired fork holds still serve %v", serving)
	}
	if vm.Status().Sealed {
		t.Fatal("a fan-out nothing received left the parent sealed")
	}
	if err := vm.Checkpoint(t.Context()); err != nil {
		t.Fatalf("checkpointing the parent every expired hold released: %v", err)
	}
}

// TestAHostReportsTheHoldsOfAFanOutOntoItself: a child taken in on its parent's
// own host is served nothing — it maps the frames the seal froze — so the page
// server knows nothing about it. That does not make the hold any less a
// handover: it holds the parent sealed, so the parent cannot be checkpointed,
// fenced, migrated or stopped while it stands, and a host that exits with one
// outstanding loses the parent's writes since its last checkpoint.
//
// A host that reported only what its page server holds reported such a parent
// as holding nothing at all: the orchestrator's survey — the one thing that
// releases a hold nothing is waiting on — could not see it, a drain called
// itself finished with one standing, and the deadline was the only thing left
// that could ever end it.
func TestAHostReportsTheHoldsOfAFanOutOntoItself(t *testing.T) {
	h, pagers, arenas := startMigrationHosts(t)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("source")
	h.configs[0].Clock = clock
	h.configs[0].EpochInterval = -1
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], arenas[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 7)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	children := []string{"child-a", "child-b"}
	// No destination: the children are taken in here, over the instant itself.
	if _, err := h.hosts[0].Fork(t.Context(), "parent", children, ""); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; !slices.Equal(serving, children) {
		t.Fatalf("a host holding a fan-out onto itself reports serving %v, want %v",
			serving, children)
	}
	if err := h.hosts[0].ReleaseMigrated("child-a"); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; !slices.Equal(serving, []string{"child-b"}) {
		t.Fatalf("after one release the host reports serving %v, want the other child", serving)
	}
	if err := h.hosts[0].ReleaseMigrated("child-b"); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("every hold was released and the host still reports serving %v", serving)
	}
	if vm.Status().Sealed {
		t.Fatal("the parent is still sealed after every child of the fan-out was released")
	}
}
