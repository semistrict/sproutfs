package main

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
)

// TestColdStartAsksTheHostToDiscardTheMemory: a cold start is the ordinary
// start with one thing added — the host discards the VM's memory and the VMM
// state with it before booting — so the orchestrator's part is the placement
// and carrying the word "cold" to the host it placed the VM on.
func TestColdStartAsksTheHostToDiscardTheMemory(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", State: stateStopped})

	result, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{To: "host-0", Cold: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-0" || !result.Result.Cold {
		t.Fatalf("result %+v, want a cold start on the host that was named", result)
	}
	want := []string{"host-0 open vm-a cold"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// TestColdStartCarriesTheNewShapeAndRecordsIt: a cold boot is the one moment a
// VM's shape can change, so the sizes go to the host with the request — and the
// table records the memory the VM now has, because from here its committed RAM
// is its own and not its template's.
func TestColdStartCarriesTheNewShapeAndRecordsIt(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	for _, h := range d.hosts {
		h.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
		h.arena(4096, 0)
		h.commit(0)
	}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", State: stateStopped, Template: "workload"})

	request := orch.StartRequest{To: "host-0", Cold: true, Memory: 1 << 30, Disk: 4 << 30}
	if _, err := d.orchestrator.Start(t.Context(), "vm-a", request); err != nil {
		t.Fatal(err)
	}
	want := []string{"host-0 open vm-a cold memory=1073741824 disk=4294967296"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil || !found {
		t.Fatalf("the table has no row for the cold-started VM: %v %v", found, err)
	}
	if row.Memory != 1<<30 {
		t.Fatalf("the table says the VM has %d bytes of memory, want the size the cold start gave it", row.Memory)
	}
}

// TestPlacementAdmitsAgainstTheMemoryTheVMHas: once a cold start has resized a
// VM, what it needs on a host is its own and not its template's, so the table's
// record of it is what a placement measures.
func TestPlacementAdmitsAgainstTheMemoryTheVMHas(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	for _, h := range d.hosts {
		h.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
		h.arena(1024, 0)
		h.commit(0)
	}
	// Room for the template's half a gigabyte and nowhere near the two the VM
	// was grown to.
	d.hosts["host-0"].arena(384, 0)
	d.hosts["host-0"].commit(0)
	d.hosts["host-1"].arena(384, 0)
	d.hosts["host-1"].commit(0)
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", State: stateStopped,
		Template: "workload", Memory: 2 << 30})

	if _, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{}); !errors.Is(err, errNoHost) {
		t.Fatalf("starting a VM grown past every host = %v, want no host", err)
	}
	if len(d.log) != 0 {
		t.Fatalf("the refused start did %v", d.log)
	}
}

// TestAWarmStartRefusesAShape: a VM that comes back where it was comes back at
// the shape its memory describes, so there is nothing to resize and asking is a
// mistake rather than a request that quietly does nothing.
func TestAWarmStartRefusesAShape(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", State: stateStopped})

	for _, request := range []orch.StartRequest{{Memory: 1 << 30}, {Disk: 1 << 30}} {
		if _, err := d.orchestrator.Start(t.Context(), "vm-a", request); !errors.Is(err, errRequest) {
			t.Fatalf("a warm start of %+v = %v, want a refused request", request, err)
		}
	}
	if len(d.log) != 0 {
		t.Fatalf("the refused starts did %v", d.log)
	}
}

// TestCreateRecordsTheTemplatesMemory: the memory a VM has starts as its
// template's, and it is written down when the VM is created so that everything
// after — a fork of it, a migration, a start — measures it without looking a
// template up again.
func TestCreateRecordsTheTemplatesMemory(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	for _, h := range d.hosts {
		h.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
		h.arena(1024, 0)
		h.commit(0)
	}
	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "workload"})
	if err != nil {
		t.Fatal(err)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), created.Result.VM.ID)
	if err != nil || !found {
		t.Fatalf("the table has no row for the created VM: %v %v", found, err)
	}
	if row.Memory != 512<<20 {
		t.Fatalf("the created VM was recorded with %d bytes of memory, want its template's", row.Memory)
	}
}

// A create's ephemeral disk is the host's to map, so the orchestrator hands it
// to the host with the rest of the request.
func TestACreateHandsItsEphemeralDiskToTheHost(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	host0 := d.hosts["host-0"]
	host0.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	host0.arena(1024, 0)
	host0.commit(0)
	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "workload",
		Ephemeral: 8 << 30})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("host-0 create %s workload ephemeral=%d", created.Result.VM.ID, 8<<30)
	if len(d.log) == 0 || d.log[len(d.log)-1] != want {
		t.Fatalf("the deployment did %v, want %q last", d.log, want)
	}
}

// TestACreateIsPlacedAndRecordedAtTheMemoryItAsksFor: a create that asks for
// more RAM than its template has costs a host that much, so it is placed by
// it, written down with it, and handed to the host with the rest of its shape.
func TestACreateIsPlacedAndRecordedAtTheMemoryItAsksFor(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	host0 := d.hosts["host-0"]
	host0.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	host0.arena(1024, 0)
	host0.commit(0)
	// The arena is 2 GiB: a 4 GiB guest fits nowhere, whatever its template.
	if _, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "workload",
		Memory: 4 << 30}); !errors.Is(err, errNoHost) {
		t.Fatalf("a create bigger than every host = %v, want errNoHost", err)
	}
	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "workload",
		Memory: 1 << 30, Disk: 4 << 30, VCPUs: 2})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Result.VM.ID
	row, found, err := d.orchestrator.table.VM(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("the table has no row for the created VM: %v %v", found, err)
	}
	if row.Memory != 1<<30 {
		t.Fatalf("the created VM was recorded with %d bytes of memory, want the 1 GiB it asked for", row.Memory)
	}
	want := fmt.Sprintf("host-0 create %s workload memory=%d disk=%d vcpus=2", id, 1<<30, 4<<30)
	if len(d.log) == 0 || d.log[len(d.log)-1] != want {
		t.Fatalf("the deployment did %v, want %q last", d.log, want)
	}
}

// TestAForkInheritsTheParentsMemory: a child is a guest of its own with RAM of
// its own, and what it has is what its parent has — the fork is of the parent,
// not of the template the parent started from, which may since have been resized
// out from under it.
func TestAForkInheritsTheParentsMemory(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	for _, h := range d.hosts {
		h.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
		h.arena(2048, 0)
		h.commit(0)
	}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", Host: "host-0", State: stateRunning,
		Template: "workload", Memory: 2 << 30})

	forked, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(forked.Children) != 1 {
		t.Fatalf("the fork made %v", forked.Children)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), forked.Children[0])
	if err != nil || !found {
		t.Fatalf("the table has no row for the child: %v %v", found, err)
	}
	if row.Memory != 2<<30 {
		t.Fatalf("the child was recorded with %d bytes of memory, want its parent's", row.Memory)
	}
}
