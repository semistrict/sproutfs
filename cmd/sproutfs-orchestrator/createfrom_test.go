package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
)

// TestACreateFromAStoppedVMCarriesItsCheckpoint: a create from another VM's
// checkpoint is placed like any create and carried to the host as it was
// asked. The new VM has the memory of the VM it starts from, and the table
// records where it came from.
func TestACreateFromAStoppedVMCarriesItsCheckpoint(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	host0 := d.hosts["host-0"]
	host0.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	host0.arena(1024, 0)
	host0.commit(0)
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", State: stateStopped, Template: "workload",
		Memory: 1 << 30})

	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{
		From: &host.CheckpointRef{VM: "vm-a", Checkpoint: 7}})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Result.VM.ID
	want := []string{"host-0 create " + id + " from vm-a@7 memory=0"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("the table has no row for the created VM: %v %v", found, err)
	}
	if row.State != stateRunning || row.Parent != "vm-a" || row.Template != "workload" || row.Memory != 1<<30 {
		t.Fatalf("the table records %+v, want a running VM of vm-a with its template and memory", row)
	}
}

// TestACreateNamesATemplateOrACheckpoint: a create starts from one thing, so a
// request that names both, or a checkpoint of no VM, reaches no host.
func TestACreateNamesATemplateOrACheckpoint(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	for _, request := range []orch.CreateRequest{
		{Template: "workload", From: &host.CheckpointRef{VM: "vm-a"}},
		{From: &host.CheckpointRef{Checkpoint: 7}},
	} {
		if _, err := d.orchestrator.Create(t.Context(), request); !errors.Is(err, errRequest) {
			t.Fatalf("creating from %+v = %v, want errRequest", request, err)
		}
	}
	if len(d.log) != 0 {
		t.Fatalf("a refused create reached a host: %v", d.log)
	}
}
