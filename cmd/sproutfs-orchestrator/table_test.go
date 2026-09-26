package main

import (
	"path/filepath"
	"testing"
)

// TestTableRemembersWhatWasDoneToAVM is what the table is for: reading back
// which host a VM is on and what it was last asked to do.
func TestTableRemembersWhatWasDoneToAVM(t *testing.T) {
	catalog := testTable(t)
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", Host: "host-0",
		State: stateCreating, Template: "alpine"}); err != nil {
		t.Fatal(err)
	}
	row, found, err := catalog.VM(t.Context(), "vm-1")
	if err != nil || !found {
		t.Fatalf("reading vm-1 found %t: %v", found, err)
	}
	if row.Host != "host-0" || row.State != stateCreating || row.Template != "alpine" {
		t.Fatalf("row %+v", row)
	}
	if row.Updated.IsZero() {
		t.Fatal("the row has no time on it")
	}
}

// TestTableKeepsTheTemplateThroughAnUpdateThatDoesNotKnowIt: a migration says
// where a VM went, not what image it came from, and the listing still shows it.
func TestTableKeepsTheTemplateThroughAnUpdateThatDoesNotKnowIt(t *testing.T) {
	catalog := testTable(t)
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", Host: "host-0",
		State: stateRunning, Template: "alpine"}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", Host: "host-0",
		State: stateMigrating, From: "host-0", To: "host-1"}); err != nil {
		t.Fatal(err)
	}
	row, _, err := catalog.VM(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Template != "alpine" {
		t.Fatalf("the template became %q", row.Template)
	}
	if row.State != stateMigrating || row.From != "host-0" || row.To != "host-1" {
		t.Fatalf("row %+v, want a migration from host-0 to host-1", row)
	}
}

// TestTableForgetsADeletedVM.
func TestTableForgetsADeletedVM(t *testing.T) {
	catalog := testTable(t)
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", State: stateRunning}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Forget(t.Context(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := catalog.VM(t.Context(), "vm-1"); err != nil || found {
		t.Fatalf("vm-1 is still there: found %t, %v", found, err)
	}
}

// TestObserveMovesAVMToTheHostThatReportsIt, which is the only account of where
// a VM actually is.
func TestObserveMovesAVMToTheHostThatReportsIt(t *testing.T) {
	catalog := testTable(t)
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", Host: "host-0", State: stateRunning}); err != nil {
		t.Fatal(err)
	}
	err := catalog.Observe(t.Context(), surveyed{listed: []string{"host-0", "host-1"},
		answered: []string{"host-0", "host-1"}, running: map[string]string{"vm-1": "host-1"}})
	if err != nil {
		t.Fatal(err)
	}
	row, _, err := catalog.VM(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Host != "host-1" || row.State != stateRunning {
		t.Fatalf("row %+v, want vm-1 running on host-1", row)
	}
}

// TestObserveLosesAVMItsOwnHostStoppedReporting, which is what a VM looks like
// once the host that ran it has gone and been replaced by a live one.
func TestObserveLosesAVMItsOwnHostStoppedReporting(t *testing.T) {
	catalog := testTable(t)
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", Host: "host-0", State: stateRunning}); err != nil {
		t.Fatal(err)
	}
	err := catalog.Observe(t.Context(), surveyed{listed: []string{"host-0"},
		answered: []string{"host-0"}, running: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	row, _, err := catalog.VM(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateStopped || row.Host != "" {
		t.Fatalf("row %+v, want vm-1 stopped and on no host", row)
	}
}

// TestObserveKeepsAVMWhoseHostDidNotAnswer: a quiet host is not a gone one, and
// recovering a VM that is perfectly fine would cost its guest everything since
// its last checkpoint.
func TestObserveKeepsAVMWhoseHostDidNotAnswer(t *testing.T) {
	catalog := testTable(t)
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", Host: "host-0", State: stateRunning}); err != nil {
		t.Fatal(err)
	}
	// host-0 is listed and did not answer, so it accounts for nothing.
	err := catalog.Observe(t.Context(), surveyed{listed: []string{"host-0"},
		running: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	row, _, err := catalog.VM(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateRunning || row.Host != "host-0" {
		t.Fatalf("row %+v, want vm-1 left where it was", row)
	}
}

// TestObserveLeavesAVMThatIsBeingCreated: no host reports a VM in the moment
// between its identity being allocated and its guest starting.
func TestObserveLeavesAVMThatIsBeingCreated(t *testing.T) {
	catalog := testTable(t)
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", Host: "host-0", State: stateCreating}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Observe(t.Context(), surveyed{listed: []string{"host-0"},
		answered: []string{"host-0"}, running: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	row, _, err := catalog.VM(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateCreating {
		t.Fatalf("row %+v, want the creation left alone", row)
	}
}

// TestReconcileTellsADeletedVMFromOneWhoseHostIsGone. Only the bucket can:
// a VM with a control record and no host is stopped, and one with neither was
// deleted.
func TestReconcileTellsADeletedVMFromOneWhoseHostIsGone(t *testing.T) {
	catalog := testTable(t)
	for _, id := range []string{"vm-stopped", "vm-deleted"} {
		if err := catalog.Record(t.Context(), vmRecord{ID: id, Host: "host-0", State: stateRunning}); err != nil {
			t.Fatal(err)
		}
	}
	err := catalog.Reconcile(t.Context(), surveyed{listed: []string{"host-0", "host-1"},
		answered: []string{"host-1"}, running: map[string]string{}}, []string{"vm-stopped"})
	if err != nil {
		t.Fatal(err)
	}
	row, found, err := catalog.VM(t.Context(), "vm-stopped")
	if err != nil || !found {
		t.Fatalf("vm-stopped found %t: %v", found, err)
	}
	if row.State != stateStopped {
		t.Fatalf("vm-stopped is %s, want stopped", row.State)
	}
	if _, found, err := catalog.VM(t.Context(), "vm-deleted"); err != nil || found {
		t.Fatalf("vm-deleted is still in the table: found %t, %v", found, err)
	}
}

// TestReconcileAddsAVMTheTableNeverSaw, which is every VM after an orchestrator
// restart: the file is gone or stale and the bucket is what rebuilds it.
func TestReconcileAddsAVMTheTableNeverSaw(t *testing.T) {
	catalog := testTable(t)
	err := catalog.Reconcile(t.Context(), surveyed{listed: []string{"host-0"},
		answered: []string{"host-0"}, running: map[string]string{"vm-running": "host-0"}},
		[]string{"vm-running", "vm-idle"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := catalog.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("the table holds %+v, want two VMs", rows)
	}
	if rows[0].ID != "vm-idle" || rows[0].State != stateStopped {
		t.Fatalf("row %+v, want vm-idle stopped", rows[0])
	}
	if rows[1].ID != "vm-running" || rows[1].Host != "host-0" || rows[1].State != stateRunning {
		t.Fatalf("row %+v, want vm-running on host-0", rows[1])
	}
}

// TestTableSurvivesTheProcessThatWroteIt, which is what putting it on a volume
// that outlives the pod is for.
func TestTableSurvivesTheProcessThatWroteIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orchestrator.db")
	first, err := openTable(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Record(t.Context(), vmRecord{ID: "vm-1", Host: "host-0", State: stateRunning}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := openTable(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	})
	row, found, err := second.VM(t.Context(), "vm-1")
	if err != nil || !found {
		t.Fatalf("reopening found vm-1 %t: %v", found, err)
	}
	if row.Host != "host-0" {
		t.Fatalf("row %+v", row)
	}
}

// TestTableKeepsThePullMark: a VM marked to pull keeps the mark through every
// row written after it, a stop and a survey among them, because nothing but
// the table remembers it while the VM runs nowhere. A survey also writes it
// down again for a running VM whose host reports it.
func TestTableKeepsThePullMark(t *testing.T) {
	catalog := testTable(t)
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", Host: "host-0",
		State: stateRunning, Pull: true}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Record(t.Context(), vmRecord{ID: "vm-1", State: stateStopped}); err != nil {
		t.Fatal(err)
	}
	err := catalog.Observe(t.Context(), surveyed{listed: []string{"host-0"}, answered: []string{"host-0"},
		running: map[string]string{"vm-2": "host-0"}, pulling: map[string]bool{"vm-2": true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"vm-1", "vm-2"} {
		row, _, err := catalog.VM(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !row.Pull {
			t.Fatalf("row %+v lost the pull mark", row)
		}
	}
}
