package main

import (
	"errors"
	"testing"
)

// TestRecoverIsRefusedWhileAHostStillServesTheVM: a source that has handed a VM
// over goes on holding the pages no checkpoint of it has until the destination
// reports that it has them. No host reports running such a VM — the source gave
// it up and the destination has not finished taking it — so a recovery took it
// for a VM whose host was gone, opened it, and fenced the destination that was
// halfway through a post-copy. A host that still serves the VM is positive
// evidence that its migration is not over, so the recovery is refused.
func TestRecoverIsRefusedWhileAHostStillServesTheVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	d.hosts["host-0"].serving = []string{"vm-a"}
	_, err := d.orchestrator.Recover(t.Context(), "vm-a", false)
	if !errors.Is(err, errRunning) {
		t.Fatalf("recovering a VM a host still serves = %v, want errRunning", err)
	}
}

// TestRecoverIsRefusedWhileTheTableRowIsInFlight: an operation the orchestrator
// itself started — a create, a fork, a recovery, a migration — is the other way
// no host reports a VM that is perfectly fine. The row says what the
// orchestrator last did with it, and while that is still in flight a recovery
// would take the epoch out from under the host the operation is about to hand
// it to. Such a row ages off, so a row whose operation really did die stops
// refusing recoveries on its own.
func TestRecoverIsRefusedWhileTheTableRowIsInFlight(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-a", Host: "host-0",
		State: stateMigrating, From: "host-0", To: "host-1"}); err != nil {
		t.Fatal(err)
	}
	_, err := d.orchestrator.Recover(t.Context(), "vm-a", false)
	if !errors.Is(err, errRunning) {
		t.Fatalf("recovering a VM the table has in flight = %v, want errRunning", err)
	}
	// Force is an operator's evidence that a host's process is gone. It says
	// nothing about an operation in flight, so it does not get past this either.
	if _, err := d.orchestrator.Recover(t.Context(), "vm-a", true); !errors.Is(err, errRunning) {
		t.Fatalf("recovering by force while an operation is in flight = %v, want errRunning", err)
	}
	// Once nothing is in flight the recovery goes through.
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-a", State: stateStopped}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Recover(t.Context(), "vm-a", false); err != nil {
		t.Fatalf("recovering a VM nothing is doing anything with: %v", err)
	}
}
