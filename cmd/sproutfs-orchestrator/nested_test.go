package main

import (
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/api/orch"
)

// A nested VM migrates live like any other VM, a drain's migration included:
// the Firecracker fork never offers its guest the VMX controls that make KVM
// write its RAM behind the page tables (see vmmachine's nested.go), so its RAM
// is handed over page by page and its guest keeps running.
func TestANestedVMMigratesLiveLikeAnyOther(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "alpine", Nested: true})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Result.VM.ID
	from := created.Host
	to := "host-1"
	if from == to {
		to = "host-0"
	}
	d.log = nil
	result, err := d.orchestrator.Migrate(t.Context(), id, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.From != from || result.To != to {
		t.Fatalf("the migration reported %+v, want %s to %s", result, from, to)
	}
	want := []string{from + " migrate " + id + " " + d.hosts[to].page, to + " receive " + id + " " + d.hosts[from].page,
		from + " released " + id}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}
