package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAnInFlightRowAgesOff: a row says what the orchestrator last did with a VM,
// and while it says an operation is in flight nothing touches that VM — the
// source's pages are left served, the reconcile leaves the row alone, and a
// recovery is refused. An operation that died with the process driving it
// therefore held all three open for as long as the deployment ran: the source
// pinned its arena for ever, and the VM could be neither recovered nor released.
// A row ages out of flight instead, well inside the deadline the source's own
// host gives that handover.
func TestAnInFlightRowAgesOff(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	d.hosts["host-0"].serving = []string{"vm-a"}
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-a", Host: "host-0",
		State: stateMigrating, From: "host-0", To: "host-1",
		Updated: time.Now().Add(-inFlightFor - time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := d.orchestrator.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if serving := d.hosts["host-0"].serving; len(serving) != 0 {
		t.Fatalf("a migration nothing is driving any more still pins %v on its source", serving)
	}
	row, found, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if !found || row.State != stateStopped {
		t.Fatalf("the aged row is %+v, want a VM no host runs", row)
	}
	if _, err := d.orchestrator.Recover(t.Context(), "vm-a", false); err != nil {
		t.Fatalf("recovering a VM whose migration died: %v", err)
	}
}

// TestAHandoverNothingWillEverReceiveIsGivenUp: the survey is what releases a
// handover nothing is waiting on, and for a VM that exists somewhere that is
// exactly right — the destination has the pages, and the release is the word
// that says so. For a child of a fan-out that never started it is a request the
// source can only refuse, because the pages it holds are the only copy and
// nothing will ever fetch them: the survey asked every few seconds, was refused
// every time, and the parent stayed sealed — never checkpointed, never fenced,
// never migratable — until the host's own deadline got there minutes later.
//
// A handover of a VM no host runs and the table has never heard of is one
// nothing will ever receive. The survey gives it up instead of asking again.
func TestAHandoverNothingWillEverReceiveIsGivenUp(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.records.ids = []string{"vm-a"}
	// The child of a fan-out that failed: host-0 holds the point for it, no
	// host runs it, and the request that named it is gone with its row.
	d.hosts["host-0"].serving = []string{"vm-child"}
	d.hosts["host-0"].outstanding["vm-child"] = true
	if err := d.orchestrator.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	var released, abandoned []string
	for _, line := range d.log {
		if after, found := strings.CutPrefix(line, "host-0 released "); found {
			released = append(released, strings.Fields(after)[0])
		}
		if after, found := strings.CutPrefix(line, "host-0 abandoned "); found {
			abandoned = append(abandoned, strings.Fields(after)[0])
		}
	}
	if len(released) != 0 {
		t.Fatalf("the survey asked host-0 to release %v, which nothing will ever fetch: %v",
			released, d.log)
	}
	if len(abandoned) != 1 || abandoned[0] != "vm-child" {
		t.Fatalf("the survey gave up %v, want the child nothing will ever receive: %v",
			abandoned, d.log)
	}
	if serving := d.hosts["host-0"].serving; len(serving) != 0 {
		t.Fatalf("host-0 still holds %v after the survey gave it up", serving)
	}
	// And a second survey has nothing left to say about it, so nothing retries
	// for the rest of the deployment's life.
	before := len(d.log)
	if err := d.orchestrator.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, line := range d.log[before:] {
		if strings.Contains(line, "vm-child") {
			t.Fatalf("a survey after the give-up still asks about it: %q", line)
		}
	}
}

// TestDeletingAVMNoHostRunsGoesToAReadyHost: a VM's authority is its control
// record and its data is the objects that record selects, both in the bucket,
// so deleting one is work any host can do. Routing a delete to the host running
// the VM meant a VM whose host was gone could never be deleted at all: there
// was no host to route to, and its record and objects stayed in the bucket for
// good.
func TestDeletingAVMNoHostRunsGoesToAReadyHost(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.records.ids = []string{"vm-gone"}
	if err := d.orchestrator.Delete(t.Context(), "vm-gone"); err != nil {
		t.Fatalf("deleting a VM no host runs: %v", err)
	}
	deleted := false
	for _, line := range d.log {
		if strings.HasSuffix(line, " delete vm-gone") {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("no host was asked to delete it: %v", d.log)
	}
	// With no host to ask at all, the delete says so rather than reporting
	// success over a record nothing removed.
	d.pods.pods = nil
	if err := d.orchestrator.Delete(t.Context(), "vm-gone"); !errors.Is(err, errNoHost) {
		t.Fatalf("deleting with no host to ask = %v, want errNoHost", err)
	}
}
