package main

import (
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/api/orch"
)

// A nested VM cannot move live, because its RAM is never handed over (see
// host/nested.go). A migration of it, a drain's included, is a stop on the
// source, which checkpoints its disks, and a cold boot on the destination.
func TestAMigrationOfANestedVMRebootsItOnTheDestination(t *testing.T) {
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
	if !result.Rebooted || result.From != from || result.To != to {
		t.Fatalf("the migration reported %+v, want a reboot from %s to %s", result, from, to)
	}
	want := []string{from + " stop " + id, to + " open " + id + " cold"}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}
