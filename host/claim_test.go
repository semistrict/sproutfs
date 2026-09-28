package host_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/vmmigrate"
)

// TestAForkChildWhoseHoldWasGivenUpIsDiscarded: the fan-out gave the child's
// hold up before the child's destination had taken it in. The child needs
// nothing from its parent's host — the parent wrote nothing no checkpoint has —
// so its post-copy completes, and it is the claim that finds the hold gone. The
// destination discards the child rather than run it, and no host can open it.
func TestAForkChildWhoseHoldWasGivenUpIsDiscarded(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var child *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &child)
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
	defer h.hosts[0].RemoveMachine("parent")
	handoffs, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := h.hosts[0].GiveUp("child"); err != nil || claimed {
		t.Fatalf("giving up an untaken child = claimed %v, %v; want unclaimed", claimed, err)
	}
	if _, err := h.hosts[1].Receive(t.Context(), handoffs[0]); !errors.Is(err, vmmigrate.ErrGivenUp) {
		t.Fatalf("receiving a child whose hold was given up = %v, want %v", err, vmmigrate.ErrGivenUp)
	}
	if running := h.hosts[1].Machines(); len(running) != 0 {
		t.Fatalf("the destination runs %v after discarding the child", running)
	}
	if _, err := h.hosts[2].Volumes().Open(t.Context(), "child"); err == nil {
		t.Fatal("another host opened a child its destination discarded")
	}
	if status := vm.Status(); status.Sealed {
		t.Fatalf("the parent is still sealed after its only child was given up: %+v", status)
	}
}

// TestAGiveUpAfterTheClaimSaysTheChildRuns: the destination took the child in
// and claimed its hold before the give-up arrived. The give-up still takes the
// parent's seal off, and says the child was claimed: the child runs, and the
// control plane that gave it up is the one that deletes it.
func TestAGiveUpAfterTheClaimSaysTheChildRuns(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var child *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &child)
	h.start(t)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(3) {
		guest.store("ram0", page, byte(page+1))
	}
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("parent")
	handoffs, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	received, err := h.hosts[1].Receive(t.Context(), handoffs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer received.Close()
	if claimed, err := h.hosts[0].GiveUp("child"); err != nil || !claimed {
		t.Fatalf("giving up a child its destination took in = claimed %v, %v; want claimed", claimed, err)
	}
	if running := h.hosts[1].Machines(); !slices.Equal(running, []string{"child"}) {
		t.Fatalf("the destination runs %v, want the child it claimed", running)
	}
	if status := vm.Status(); status.Sealed {
		t.Fatalf("the parent is still sealed after the give-up: %+v", status)
	}
}

// TestALocalChildClaimsItsHoldWhenTakenIn: a child on its parent's own host
// claims its hold as it is bound to the fork point, and a give-up after that
// says so, as one from another host's claim does.
func TestALocalChildClaimsItsHoldWhenTakenIn(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var child *machine
	h.configs[0].Migration.StartVM = starter(t, pagers[0], &child)
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
	defer h.hosts[0].RemoveMachine("parent")
	handoffs, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, "")
	if err != nil {
		t.Fatal(err)
	}
	received, err := h.hosts[0].Receive(t.Context(), handoffs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer received.Close()
	defer h.hosts[0].RemoveMachine("child")
	if claimed, err := h.hosts[0].GiveUp("child"); err != nil || !claimed {
		t.Fatalf("giving up a child this host took in = claimed %v, %v; want claimed", claimed, err)
	}
}
