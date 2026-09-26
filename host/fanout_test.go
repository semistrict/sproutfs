package host_test

import (
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmigrate"
)

// TestAFanOutNothingReceivesExpiresEveryHold: a fan-out is one pause and one
// hold per child, and a request that fails part way through taking its children
// in leaves every one of those holds behind. Each of them carries the deadline
// a handover gets, so the parent takes its pages back within it and is
// checkpointed again — which is the whole reason the deadline exists.
//
// The releases the orchestrator carries are refused first, exactly as they are
// for a child nothing ever fetched the pages of: what is under test is that the
// deadline is what ends a hold no release can.
func TestAFanOutNothingReceivesExpiresEveryHold(t *testing.T) {
	h, pagers := startMigrationHosts(t)
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
	guest, err := newMachine(t, pagers[0], vm, nil)
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
	// Nothing fetched what the point holds, so the release the orchestrator
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
// own host is served nothing — it maps the pages the seal froze — so the page
// server knows nothing about it. That does not make the hold any less a
// handover: it holds the parent sealed, so the parent cannot be checkpointed,
// fenced, migrated or stopped while it stands, and a host that exits with one
// outstanding loses the parent's writes since its last checkpoint.
//
// So it is reported as a served hold is. It is in Serving, it owes the pages
// the point holds for it until the child is taken in, and its release is
// refused until then, as the page server refuses one for a child elsewhere that
// has not fetched them. A release that went through first would leave the child
// nothing to be taken in over.
func TestAHostReportsTheHoldsOfAFanOutOntoItself(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	started := map[string]*machine{}
	h.configs[0].Migration.StartVM = starters(t, pagers[0], started)
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
	guest.store("ram0", 0, 7)
	guest.store("ram0", 1, 8)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	children := []string{"child-a", "child-b"}
	// No destination: the children are taken in here, over the point itself.
	handoffs, err := h.hosts[0].Fork(t.Context(), "parent", children, "")
	if err != nil {
		t.Fatal(err)
	}
	status := h.hosts[0].Status()
	if !slices.Equal(status.Serving, children) {
		t.Fatalf("a host holding a fan-out onto itself reports serving %v, want %v",
			status.Serving, children)
	}
	// The parent wrote two pages since its checkpoint, and neither child has
	// them yet.
	if want := map[string]int{"child-a": 2, "child-b": 2}; !maps.Equal(status.Outstanding, want) {
		t.Fatalf("the holds owe %v, want %v", status.Outstanding, want)
	}
	if err := h.hosts[0].ReleaseMigrated("child-a"); !errors.Is(err, vmmigrate.ErrOutstanding) {
		t.Fatalf("releasing a child not yet taken in = %v, want ErrOutstanding", err)
	}
	if serving := h.hosts[0].Status().Serving; !slices.Equal(serving, children) {
		t.Fatalf("a refused release left the host serving %v, want %v", serving, children)
	}

	received, err := h.hosts[0].Receive(t.Context(), handoffs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer received.Close()
	defer h.hosts[0].RemoveMachine("child-a")
	if want := map[string]int{"child-a": 0, "child-b": 2}; !maps.Equal(h.hosts[0].Status().Outstanding, want) {
		t.Fatalf("after child-a was taken in the holds owe %v, want %v",
			h.hosts[0].Status().Outstanding, want)
	}
	if err := h.hosts[0].ReleaseMigrated("child-a"); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; !slices.Equal(serving, []string{"child-b"}) {
		t.Fatalf("after one release the host reports serving %v, want the other child", serving)
	}
	if !vm.Status().Sealed {
		t.Fatal("the parent lost its seal while child-b still holds the point")
	}
	// Nothing will take child-b in, so it is given up rather than released.
	if err := h.hosts[0].Abandon("child-b"); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("every hold was released and the host still reports serving %v", serving)
	}
	if vm.Status().Sealed {
		t.Fatal("the parent is still sealed after every child of the fan-out was released")
	}
	if pending := clock.Pending(); pending != 0 {
		t.Fatalf("the ended holds left %d deadlines armed", pending)
	}
}
