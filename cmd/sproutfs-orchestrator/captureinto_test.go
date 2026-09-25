package main

import (
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/api/orch"
)

// TestCaptureIntoANewVMRecordsItStopped: a capture into a new VM allocates the
// new VM's identity, asks the host running the source for it, and records the
// new VM stopped, of the source's template and memory, with the source as its
// parent. The source is still running where it was.
func TestCaptureIntoANewVMRecordsItStopped(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", Host: "host-0", State: stateRunning,
		Template: "workload", Memory: 1 << 30})

	captured, err := d.orchestrator.Capture(t.Context(), "vm-a", orch.CaptureRequest{New: true})
	if err != nil {
		t.Fatal(err)
	}
	id := captured.Result.VM
	if captured.Host != "host-0" || id == "vm-a" || captured.Result.Checkpoint != 12 {
		t.Fatalf("the capture reports %+v, want a new VM's root captured on host-0", captured)
	}
	want := []string{"host-0 capture vm-a into " + id}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("the table has no row for the new VM: %v %v", found, err)
	}
	if row.State != stateStopped || row.Host != "" || row.Parent != "vm-a" || row.Template != "workload" ||
		row.Memory != 1<<30 {
		t.Fatalf("the table records %+v, want a stopped VM of vm-a with its template and memory", row)
	}
	source, found, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil || !found || source.State != stateRunning || source.Host != "host-0" {
		t.Fatalf("the source's row is %+v (%v, %v), want it running on host-0", source, found, err)
	}
}
