package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
)

// TestACaptureAndAStopCarryTheKeep: a capture or a stop that keeps its
// checkpoint reaches the host running the VM with the keep, and a capture into
// a new VM that asks to keep reaches no host at all, because the new VM's root
// is its selected checkpoint already.
func TestACaptureAndAStopCarryTheKeep(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a", "vm-b"}})
	if _, err := d.orchestrator.Capture(t.Context(), "vm-a", orch.CaptureRequest{Keep: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Capture(t.Context(), "vm-a", orch.CaptureRequest{New: true, Keep: true}); !errors.Is(err, errRequest) {
		t.Fatalf("capturing into a new VM and keeping = %v, want errRequest", err)
	}
	if _, err := d.orchestrator.Stop(t.Context(), "vm-a", orch.StopRequest{Keep: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Stop(t.Context(), "vm-b", orch.StopRequest{Suspend: true, Keep: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{"host-0 capture vm-a kept", "host-0 stop vm-a kept", "host-0 suspend vm-b kept"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// TestKeptCheckpointsAreListedAndReleasedByAnyHost: a VM's kept checkpoints are
// its control record's, so a VM nothing runs is listed and released through a
// ready host all the same.
func TestKeptCheckpointsAreListedAndReleasedByAnyHost(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", State: stateStopped, Template: "workload"})
	kept, err := d.orchestrator.Kept(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if want := []host.Kept{{Checkpoint: 7, State: true}}; kept.VM != "vm-a" || !slices.Equal(kept.Kept, want) {
		t.Fatalf("the listing is %+v, want vm-a keeping %+v", kept, want)
	}
	if err := d.orchestrator.Release(t.Context(), "vm-a", 7); err != nil {
		t.Fatal(err)
	}
	want := []string{"host-0 kept vm-a", "host-0 release vm-a@7"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}
