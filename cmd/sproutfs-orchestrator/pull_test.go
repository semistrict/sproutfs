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
}
