package main

import (
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/api/orch"
)

// A VM marked to pull its whole memory carries the mark to the host that runs
// it however it starts there: a create, a start, and each child of a fork,
// whose handoff the destination receives it in.
func TestThePullMarkReachesTheHostOnEveryStart(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	d.records.ids = []string{"vm-a", "vm-b"}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-b", State: stateStopped, Template: "alpine"})

	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "alpine", Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Start(t.Context(), "vm-b", orch.StartRequest{Pull: true}); err != nil {
		t.Fatal(err)
	}
	forked, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 1, Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	child := forked.Children[0]
	want := []string{
		"host-0 create " + created.Result.VM.ID + " alpine pull",
		"host-0 open vm-b pull",
		"host-0 fork vm-a " + child,
		"host-0 receive " + child + "  pull", "host-0 released " + child,
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	// The child's row keeps the mark through its flight and its landing, so a
	// later open of the child pulls too.
	row, _, err := d.orchestrator.table.VM(t.Context(), child)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateRunning || !row.Pull {
		t.Fatalf("the child's row is %+v, want it running and marked to pull", row)
	}
}

// A marked VM whose host is lost is recovered pulling. The recovery asks for
// nothing but the VM, so the mark is the one the orchestrator recorded when
// the VM was created.
func TestAMarkedVMIsRecoveredPulling(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "alpine", Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Result.VM.ID
	if _, err := d.orchestrator.Kill(t.Context(), created.Host); err != nil {
		t.Fatal(err)
	}
	recovered, err := d.orchestrator.Recover(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"host-0 create " + id + " alpine pull",
		"host-1 open " + id + " pull",
	}
	if recovered.Host != "host-1" || !slices.Equal(d.log, want) {
		t.Fatalf("recovered on %s after %v, want host-1 after %v", recovered.Host, d.log, want)
	}
}

// Every open the orchestrator drives carries a VM's mark: a migration's
// receive, though its source did not report the mark, and a start that did not
// ask for it again.
func TestEveryOpenCarriesThePullMark(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", Host: "host-0", State: stateRunning, Pull: true})
	if _, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Stop(t.Context(), "vm-a", orch.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{To: "host-0"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081 pull",
		"host-0 released vm-a",
		"host-1 stop vm-a",
		"host-0 open vm-a pull",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// A table that lost its rows learns a running VM's mark again from the host
// that runs it, so the recovery after that host is lost still pulls.
func TestASurveyRelearnsThePullMarkOfARunningVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "alpine", Pull: true})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Result.VM.ID
	d.records.ids = []string{id}
	d.orchestrator.table = testTable(t)
	if err := d.orchestrator.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Kill(t.Context(), created.Host); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Recover(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	if last := d.log[len(d.log)-1]; last != "host-1 open "+id+" pull" {
		t.Fatalf("the recovery was %q, want it to pull: %v", last, d.log)
	}
}
