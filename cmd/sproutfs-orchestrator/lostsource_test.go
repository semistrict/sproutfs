package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// A destination fetches the pages no checkpoint of a migrated VM holds from the
// host that handed it over, and asks for them until it has them: they exist
// nowhere else, and reading its own volume for one would rewind the guest past
// its own write. Nothing in the destination can end that wait, so the
// orchestrator is what ends it — it is the one thing that knows the source host
// is gone, and when it is, those pages are gone with it.
//
// The receive is discarded, which tears the half-received guest down on the
// destination, the VM is recovered from the checkpoint its control record
// selects, and the in-flight row goes with it.
func TestLosingTheSourceOfAMigrationEndsItAndRecoversTheVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	destination := d.hosts["host-1"]
	// The destination's post-copy never completes, because the pages it is
	// waiting for are on a host that stops existing while it waits.
	destination.holdReceive = true
	destination.onReceive = func() {
		d.hosts["host-0"].down = true
		if err := d.pods.Delete(t.Context(), "host-0"); err != nil {
			t.Error(err)
		}
	}
	_, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if !errors.Is(err, errLostSource) {
		t.Fatalf("a migration whose source was lost = %v, want errLostSource", err)
	}
	// The destination was told to give the half-received guest up rather than
	// left waiting for a host that is gone.
	if !slices.Contains(d.log, "host-1 receive-discarded vm-a") {
		t.Fatalf("the destination was never told to discard what it received: %v", d.log)
	}
	// The VM is back, at the checkpoint its record selects, on the host that is
	// left.
	if !slices.Contains(d.log, "host-1 open vm-a") {
		t.Fatalf("the VM whose source was lost was never recovered: %v", d.log)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if !found || row.State != stateRunning || row.Host != "host-1" {
		t.Fatalf("the table row after a lost source: %+v", row)
	}
	if row.From != "" || row.To != "" {
		t.Fatalf("the migration is still in flight in the table: %+v", row)
	}
}

// A source that is merely quiet is not a source that is gone: its pod is still
// listed, its guest's pages may be perfectly well where they were, and ending
// the migration would discard a guest that is about to be complete. The
// destination goes on waiting.
func TestAQuietSourceDoesNotEndAMigration(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	destination := d.hosts["host-1"]
	destination.holdReceive = true
	destination.onReceive = func() { d.hosts["host-0"].down = true }
	go func() {
		// Long enough for several watches of the source, and then the pages
		// arrive after all.
		time.Sleep(20 * d.orchestrator.sourceWatch)
		destination.release()
	}()
	result, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if err != nil {
		t.Fatalf("a migration whose source went quiet: %v", err)
	}
	if result.To != "host-1" {
		t.Fatalf("the VM went to %s", result.To)
	}
	for _, line := range d.log {
		if strings.Contains(line, "receive-discarded") {
			t.Fatalf("a quiet source ended the migration: %v", d.log)
		}
	}
}
