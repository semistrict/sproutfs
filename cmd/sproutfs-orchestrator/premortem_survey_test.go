package main

import (
	"testing"
)

// TestASurveyStopsAttributingAVMToAPodTheClusterNoLongerHas: a host that did
// not answer is a host that may be running its guests perfectly well behind one
// dropped request, and its rows are left alone for exactly that reason. A pod
// the Kubernetes API no longer lists is not that: nothing runs there, because
// there is no there. Every survey lists the pods, so this is known at every
// survey rather than at the next reconcile of the whole bucket.
//
// Until it was, the deployment went on reporting the VMs of a killed host as
// running on it — and the soak's host loss waits for no host to report the VM
// before it recovers it, so the wait was on a timer rather than on the kill.
func TestASurveyStopsAttributingAVMToAPodTheClusterNoLongerHas(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {"vm-b"}})
	if _, err := d.orchestrator.survey(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.Kill(t.Context(), "host-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.orchestrator.survey(t.Context()); err != nil {
		t.Fatal(err)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), "vm-b")
	if err != nil || !found {
		t.Fatalf("the table has no row for the VM of the killed host: %v %v", found, err)
	}
	if row.State != stateStopped || row.Host != "" {
		t.Fatalf("the table says %+v, want a VM no host runs", row)
	}
	vms, err := d.orchestrator.VMs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, vm := range vms {
		if vm.ID != "vm-b" {
			continue
		}
		if vm.Host != "" {
			t.Fatalf("the deployment still lists vm-b on %s, which it no longer has", vm.Host)
		}
	}
}

// TestAQuietHostKeepsItsRows: the other half of the same rule. A pod the API
// still lists that did not answer this survey says nothing about what it runs,
// and a row cleared on that would offer a recovery of a guest that is fine.
func TestAQuietHostKeepsItsRows(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {"vm-b"}})
	if _, err := d.orchestrator.survey(t.Context()); err != nil {
		t.Fatal(err)
	}
	d.hosts["host-1"].down = true
	if _, err := d.orchestrator.survey(t.Context()); err != nil {
		t.Fatal(err)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), "vm-b")
	if err != nil || !found {
		t.Fatalf("the table has no row for the quiet host's VM: %v %v", found, err)
	}
	if row.State != stateRunning || row.Host != "host-1" {
		t.Fatalf("the table says %+v, want the VM still on the host that went quiet", row)
	}
}
