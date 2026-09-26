package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/orch"
)

// A child forked onto its parent's own host is served nothing, but its host
// holds the fork point for it until it is released, and that hold keeps the
// parent sealed. The host reports the hold in Serving like any other handover,
// so the survey sees it and ends it once it is stale.

// TestASurveyReleasesALocalForkHoldItsChildWasTakenIn: the orchestrator that
// forked the child restarted before it released the hold. The child runs on the
// parent's host, so it was taken in over the point and has every page it
// inherited. The survey releases the hold, and the host accepts.
func TestASurveyReleasesALocalForkHoldItsChildWasTakenIn(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a", "vm-a-child"}})
	source := d.hosts["host-0"]
	source.serving = []string{"vm-a-child"}
	source.outstanding["vm-a-child"] = true
	source.fetched["vm-a-child"] = true
	if _, err := d.orchestrator.survey(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(d.log, "host-0 released vm-a-child") {
		t.Fatalf("the survey did not release the child's hold: %v", d.log)
	}
	if len(source.serving) != 0 {
		t.Fatalf("host-0 still holds %v for a child it took in", source.serving)
	}
}

// TestAReconcileGivesUpALocalForkHoldWhoseChildWasNeverTakenIn: the
// orchestrator that forked the child died after the parent's host took the
// fork point and before it took the child in. Nothing will take it in now, and
// the hold keeps the parent sealed until the host's own deadline.
//
// A survey alone cannot prove the child is gone: its row says only that no host
// runs it, which is also true of a child between two steps of its fork. So it
// asks for the release, and the host refuses, because the child has none of
// the pages it inherited. The reconcile lists the bucket, which has no record
// of the child. That is the evidence: the child does not exist and nothing is
// creating it, so the hold is given up.
func TestAReconcileGivesUpALocalForkHoldWhoseChildWasNeverTakenIn(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	source := d.hosts["host-0"]
	source.serving = []string{"vm-a-child"}
	source.outstanding["vm-a-child"] = true
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-a-child", Host: "host-0",
		State: stateCreating, Parent: "vm-a",
		Updated: time.Now().Add(-inFlightFor - time.Minute)}); err != nil {
		t.Fatal(err)
	}

	if _, err := d.orchestrator.survey(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(d.log, "host-0 released vm-a-child") {
		t.Fatalf("the survey did not ask for the release: %v", d.log)
	}
	if !slices.Equal(source.serving, []string{"vm-a-child"}) {
		t.Fatalf("host-0 holds %v, want the refused release to leave the hold", source.serving)
	}

	if err := d.orchestrator.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(d.log, "host-0 abandoned vm-a-child") {
		t.Fatalf("the reconcile did not give up the hold of a child that does not exist: %v", d.log)
	}
	if len(source.serving) != 0 {
		t.Fatalf("host-0 still holds %v for a child that does not exist", source.serving)
	}
}

// TestAReconcileLeavesALocalForkHoldWhoseForkIsInFlight: a fork still running
// has noted its child and not yet taken it in. The child has no record and no
// host runs it, as with a fork that died, but its row is in flight. Giving the
// hold up would leave the fork nothing to take the child in over, so the
// reconcile leaves it alone.
func TestAReconcileLeavesALocalForkHoldWhoseForkIsInFlight(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	source := d.hosts["host-0"]
	source.serving = []string{"vm-a-child"}
	source.outstanding["vm-a-child"] = true
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-a-child", Host: "host-0",
		State: stateCreating, Parent: "vm-a"}); err != nil {
		t.Fatal(err)
	}
	if err := d.orchestrator.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(d.log) != 0 {
		t.Fatalf("the reconcile asked host-0 %v while the fork was in flight", d.log)
	}
	if !slices.Equal(source.serving, []string{"vm-a-child"}) {
		t.Fatalf("host-0 holds %v, want the in-flight fork's hold left alone", source.serving)
	}
}

// TestAFanOutKeepsItsChildrenInFlightWhileTheyAreReceived: a fan-out takes its
// children in one after another, so the last of them can begin long after the
// fork wrote its row. The table takes a row in flight at its word only for its
// aging bound, and a reconcile gives up the hold of a child whose row aged out
// and that does not exist yet. That is right for a fork whose orchestrator
// died, and wrong for one still running: the child's hold would be given up
// under it and the fork would fail for nothing. The fork writes each child's
// row again until the child's own receive has finished, so the last child is
// still in flight when its turn comes, and is taken in.
func TestAFanOutKeepsItsChildrenInFlightWhileTheyAreReceived(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	const aging = 200 * time.Millisecond
	d.orchestrator.table.aging = aging
	source := d.hosts["host-0"]
	// The reconcile runs on its own timer while the fan-out runs, as in a
	// deployment, and until the last child's receive begins.
	reconciling, stop := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		d.orchestrator.Reconciling(reconciling, aging/10)
	}()
	// The first child's receive takes three times as long as the table
	// believes a row.
	source.holdReceive = true
	began := 0
	source.onReceive = func() {
		began++
		if began == 1 {
			time.AfterFunc(3*aging, source.release)
			return
		}
		stop()
		<-stopped
	}
	result, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 2})
	if err != nil {
		t.Fatalf("a fan-out whose last child was received after the aging bound: %v", err)
	}
	for _, line := range d.log {
		if strings.Contains(line, "abandoned") {
			t.Fatalf("a hold was given up while its fan-out was running: %v", d.log)
		}
	}
	if running := source.running; !slices.Equal(running, []string{"vm-a", "vm-new-1", "vm-new-2"}) {
		t.Fatalf("host-0 runs %v, want the parent and both children", running)
	}
	for _, child := range result.Children {
		row, found, err := d.orchestrator.table.VM(t.Context(), child)
		if err != nil {
			t.Fatal(err)
		}
		if !found || row.State != stateRunning || row.Host != "host-0" || row.Parent != "vm-a" {
			t.Fatalf("the row of %s after the fan-out: %+v", child, row)
		}
	}
}
