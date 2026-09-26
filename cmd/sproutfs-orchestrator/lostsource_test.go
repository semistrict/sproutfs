package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/handover"
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
// the migration would discard a guest that is about to be complete. Inside its
// hold the destination goes on waiting.
func TestAQuietSourceDoesNotEndAMigration(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-0"].hold = holdForAMinute
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

// A source whose pod is still listed and that nothing can reach says nothing
// either way, until its hold is over. It promised the pages for that long and
// no longer, so from then on they are gone whether the host is alive or not.
// The migration ends then, on that evidence: the destination gives up what it
// received, and the VM is recovered from its checkpoint. The source's silence
// does not hold the recovery back, because the source handed the VM over and
// cannot be running it.
func TestAListedSourceNothingCanReachEndsAMigrationAtItsHold(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	const hold = 0.2
	d.hosts["host-0"].hold = hold
	destination := d.hosts["host-1"]
	destination.holdReceive = true
	destination.onReceive = func() { d.hosts["host-0"].down = true }
	began := time.Now()
	_, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if !errors.Is(err, errLostSource) || !errors.Is(err, handover.ErrHoldOver) {
		t.Fatalf("a migration whose listed source went quiet = %v, want errLostSource at the end of its hold", err)
	}
	if waited := time.Since(began); waited < time.Duration(hold*float64(time.Second)) {
		t.Fatalf("the migration ended %s in, inside the source's hold of %gs", waited, hold)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-1 receive-discarded vm-a",
		"host-1 open vm-a",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	row, _, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateRunning || row.Host != "host-1" || row.From != "" || row.To != "" {
		t.Fatalf("the table row after the source's hold ended: %+v", row)
	}
}

// Only the source's silence is excused. Any other host that does not answer
// could be running the VM, so the recovery after the hold is refused while one
// is quiet, exactly as an operator's would be.
func TestAnotherQuietHostHoldsTheRecoveryBack(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}, "host-2": {}})
	d.hosts["host-0"].hold = 0.2
	destination := d.hosts["host-1"]
	destination.holdReceive = true
	destination.onReceive = func() {
		d.hosts["host-0"].down = true
		d.hosts["host-2"].down = true
	}
	_, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if !errors.Is(err, errLostSource) || !errors.Is(err, errRunning) {
		t.Fatalf("a migration ended while another host was quiet = %v, want errLostSource and a refused recovery", err)
	}
	if !strings.Contains(err.Error(), "host-2 did not answer") {
		t.Fatalf("the recovery was refused for %v, want host-2's silence", err)
	}
	if !slices.Contains(d.log, "host-1 receive-discarded vm-a") {
		t.Fatalf("the destination was never told to discard what it received: %v", d.log)
	}
	for _, line := range d.log {
		if strings.Contains(line, "open vm-a") {
			t.Fatalf("the VM was recovered while host-2 was quiet: %v", d.log)
		}
	}
	row, _, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateStopped {
		t.Fatalf("row %+v, want vm-a stopped", row)
	}
}
