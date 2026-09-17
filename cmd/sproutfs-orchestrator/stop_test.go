package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/api/orch"
)

// TestStopAsksTheHostRunningTheVMAndRecordsIt: a stop is the host's work —
// only the host running a guest can publish what it holds and close it — so the
// orchestrator's part is finding that host and writing down that the VM is no
// longer anywhere.
func TestStopAsksTheHostRunningTheVMAndRecordsIt(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	result, err := d.orchestrator.Stop(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if result.VM != "vm-a" || result.Host != "host-0" || result.Checkpoint != 13 {
		t.Fatalf("result %+v, want the VM, its host and the checkpoint it comes back at", result)
	}
	want := []string{"host-0 stop vm-a"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil || !found {
		t.Fatalf("the table has no row for the stopped VM: %v %v", found, err)
	}
	if row.State != stateStopped || row.Host != "" {
		t.Fatalf("the table says %+v, want it stopped on no host", row)
	}
}

// TestStopOfAVMNoHostRunsIsNotFound. A VM nothing runs has nothing to stop: it
// is already only its control record and its objects.
func TestStopOfAVMNoHostRunsIsNotFound(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	if _, err := d.orchestrator.Stop(t.Context(), "vm-z"); !errors.Is(err, errNotFound) {
		t.Fatalf("stopping a VM no host runs = %v, want not found", err)
	}
}

// TestStartOpensAStoppedVMOnTheHostWithTheMostMemoryFree: a start is a
// placement and an open. It is a recovery without the evidence of a loss — the
// VM was stopped deliberately and its last host closed it — so what it needs is
// somewhere the guest fits.
func TestStartOpensAStoppedVMOnTheHostWithTheMostMemoryFree(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	d.hosts["host-0"].arena(1024, 100)
	d.hosts["host-0"].commit(900 << 21)
	d.hosts["host-1"].arena(1024, 900)
	d.hosts["host-1"].commit(100 << 21)
	for _, h := range d.hosts {
		h.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", State: stateStopped, Template: "workload"})

	result, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-1" {
		t.Fatalf("the VM started on %s, want the host whose guests have promised the least", result.Host)
	}
	want := []string{"host-1 open vm-a"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil || !found {
		t.Fatalf("the table has no row for the started VM: %v %v", found, err)
	}
	if row.State != stateRunning || row.Host != "host-1" {
		t.Fatalf("the table says %+v, want it running on host-1", row)
	}
}

// TestStartOnANamedHostGoesThere, which is what a soak spreading its VMs over
// the cluster asks for.
func TestStartOnANamedHostGoesThere(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	result, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{To: "host-0"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Host != "host-0" {
		t.Fatalf("the VM started on %s, want the host that was named", result.Host)
	}
}

// TestStartOfAVMAHostAlreadyRunsIsRefused: opening a VM takes its control
// record's epoch, which fences whatever held it. A host that says it runs the
// VM is a host whose guest is fine, and starting a second one would leave a
// writer whose stores can never be published.
func TestStartOfAVMAHostAlreadyRunsIsRefused(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	if _, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{}); !errors.Is(err, errRunning) {
		t.Fatalf("starting a VM a host runs = %v, want errRunning", err)
	}
	if len(d.log) != 0 {
		t.Fatalf("the refused start did %v", d.log)
	}
}

// TestStartIsRefusedWhileAHostStillServesTheVM: a source that has handed a VM
// over holds the pages no checkpoint of it has until the destination reports
// having them, and no host reports running such a VM. It is between hosts
// rather than stopped, and starting it would fence the host about to run it.
func TestStartIsRefusedWhileAHostStillServesTheVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	d.hosts["host-0"].serving = []string{"vm-a"}
	if _, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{}); !errors.Is(err, errRunning) {
		t.Fatalf("starting a VM a host still serves = %v, want errRunning", err)
	}
}

// TestStartIsRefusedWhileTheTableRowIsInFlight, for the reason a recovery is:
// an operation this orchestrator started is why no host reports the VM, and the
// host it is about to be handed to would be fenced by a start.
func TestStartIsRefusedWhileTheTableRowIsInFlight(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-a", Host: "host-0",
		State: stateMigrating, From: "host-0", To: "host-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{}); !errors.Is(err, errRunning) {
		t.Fatalf("starting a VM the table has in flight = %v, want errRunning", err)
	}
}

// TestStartOnAHostWithoutRoomIsRefused before anything opens the VM: a host
// admitted past its arena runs a guest that faults against the spill file, or
// one whose memory will not attach at all.
func TestStartOnAHostWithoutRoomIsRefused(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	for _, h := range d.hosts {
		h.arena(1024, 10)
		h.commit(1024 << 21)
		h.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", State: stateStopped, Template: "workload"})
	if _, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{To: "host-0"}); !errors.Is(err, errNoHost) {
		t.Fatalf("starting a VM on a full host = %v, want errNoHost", err)
	}
	if len(d.log) != 0 {
		t.Fatalf("the refused start did %v", d.log)
	}
}

// TestStoppingAndStartingReturnsTheVMAtItsCheckpoint end to end over the fakes,
// which is the pair the soak drives.
func TestStoppingAndStartingReturnsTheVMAtItsCheckpoint(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	if _, err := d.orchestrator.Stop(t.Context(), "vm-a"); err != nil {
		t.Fatal(err)
	}
	if running := d.hosts["host-0"].running; slices.Contains(running, "vm-a") {
		t.Fatalf("host-0 still runs %v after the stop", running)
	}
	started, err := d.orchestrator.Start(t.Context(), "vm-a", orch.StartRequest{To: "host-1"})
	if err != nil {
		t.Fatal(err)
	}
	if started.Result.VM.Checkpoint == 0 {
		t.Fatalf("the started VM came back at checkpoint %d", started.Result.VM.Checkpoint)
	}
	want := []string{"host-0 stop vm-a", "host-1 open vm-a"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}
