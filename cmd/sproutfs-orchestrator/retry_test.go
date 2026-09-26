package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/orch"
)

// holdForAMinute is what a source in these tests says it holds a handover for:
// far longer than any of them runs, so only the tests about the hold reach it.
const holdForAMinute = 60

// A receive that fails leaves the handoff as good as it was: the destination
// published nothing, and the source still serves every page no checkpoint has.
// So it is tried again while the source holds those pages, and the guest keeps
// its writes.
func TestAFailedReceiveIsRetriedWhileTheSourceHoldsThePages(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-0"].hold = holdForAMinute
	d.hosts["host-1"].refusedReceives = 1
	result, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.To != "host-1" || result.Unpublished != 6 {
		t.Fatalf("result %+v, want the VM on host-1 with every page it held", result)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-0 released vm-a",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	row, _, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.Host != "host-1" || row.State != stateRunning {
		t.Fatalf("row %+v, want vm-a running on host-1", row)
	}
}

// A destination gets two attempts in a row. When it keeps failing, the handoff
// goes to another host with room for the guest, which is the one a host that
// has none is passed over for.
func TestADestinationThatKeepsFailingIsLeftForAnotherWithRoom(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}, "host-2": {}, "host-3": {}})
	d.hosts["host-0"].hold = holdForAMinute
	d.hosts["host-1"].refusesEveryReceive = true
	for _, name := range []string{"host-1", "host-2", "host-3"} {
		d.hosts[name].arena(8, 0)
	}
	// host-2 has no room for a guest of two pages, and host-3 has.
	d.hosts["host-2"].commit(7 * (2 << 20))
	d.hosts["host-3"].commit(4 * (2 << 20))
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-a", Host: "host-0",
		State: stateRunning, Memory: 2 * (2 << 20)}); err != nil {
		t.Fatal(err)
	}
	result, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.To != "host-3" {
		t.Fatalf("the VM went to %s, want host-3, the other host with room", result.To)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-3 receive vm-a 10.0.0.1:8081",
		"host-0 released vm-a",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// The source's hold is what a handoff is good for. Once it is over the source
// has given the pages up, so the retries stop: the last look, at the end of the
// hold, finds the pages gone, nothing is released, and the VM is recovered from
// its checkpoint, as when the source is lost.
func TestRetriesStopWhenTheSourcesHoldIsOver(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	// A fifth of a second is fifty looks at the policy of these tests.
	const hold = 0.2
	d.hosts["host-0"].hold = hold
	d.hosts["host-1"].refusesEveryReceive = true
	began := time.Now()
	_, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if !errors.Is(err, errLostSource) {
		t.Fatalf("a migration to a destination that refuses every receive = %v, want errLostSource", err)
	}
	if waited := time.Since(began); waited < time.Duration(hold*float64(time.Second)) {
		t.Fatalf("the handoff was given up %s in, inside the source's hold of %gs", waited, hold)
	}
	receives := 0
	for _, line := range d.log {
		if line == "host-1 receive vm-a 10.0.0.1:8081" {
			receives++
		}
		if line == "host-0 released vm-a" {
			t.Fatalf("the source was released with no destination holding the pages: %v", d.log)
		}
	}
	if receives < 3 {
		t.Fatalf("the handoff was received %d times inside its hold, want it tried again: %v", receives, d.log)
	}
	if last := d.log[len(d.log)-1]; last != "host-0 open vm-a" {
		t.Fatalf("the deployment ended with %q, want the VM recovered: %v", last, d.log)
	}
	row, _, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateRunning || row.Host != "host-0" {
		t.Fatalf("row %+v, want vm-a running on host-0", row)
	}
}

// A source that answers and no longer serves the VM has given its pages up,
// whether its hold ran out or it came back without them. Nothing is left to
// try again with, so the VM is recovered from its checkpoint.
func TestRetriesStopWhenTheSourceNoLongerServesTheVM(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	source := d.hosts["host-0"]
	source.hold = holdForAMinute
	d.hosts["host-1"].refusedReceives = 1
	// The fake shares one lock across the deployment, and the refusal runs
	// outside it.
	d.hosts["host-1"].onRefusal = func() {
		source.mu.Lock()
		source.serving = nil
		source.mu.Unlock()
	}
	_, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if !errors.Is(err, errLostSource) {
		t.Fatalf("a migration whose source gave its pages up = %v, want errLostSource", err)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-0 open vm-a",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// A receive whose answer was lost may still have taken the VM. Asking any
// host again would fence that guest, so a host that runs the VM ends the
// handover there, and the source is released to it.
func TestAReceiveWhoseAnswerWasLostEndsWhereItLanded(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}, "host-2": {}})
	d.hosts["host-0"].hold = holdForAMinute
	d.hosts["host-1"].loseAnswer = true
	result, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.To != "host-1" {
		t.Fatalf("the VM went to %s, want host-1, which took it", result.To)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		// The survey that found the VM on host-1 releases the handover it no
		// longer sees in flight, and the migration then releases it too. A
		// release is idempotent, and a source refuses one while any page it
		// holds is still unfetched.
		"host-0 released vm-a",
		"host-0 released vm-a",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// A receive whose caller hung up goes on where it was sent, and its host
// reports it in flight. No receive goes anywhere while it does, the same host
// included, and when it takes the VM in the handover ends there with one guest.
func TestNoReceiveIsSentWhileAnEarlierOneIsInFlight(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}, "host-2": {}})
	d.hosts["host-0"].hold = holdForAMinute
	d.hosts["host-1"].outlives = 4
	result, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.To != "host-1" {
		t.Fatalf("the VM went to %s, want host-1, where the receive went on", result.To)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-1 took vm-a in",
		// The survey that found the VM on host-1 releases the handover, and
		// the migration then releases it too.
		"host-0 released vm-a",
		"host-0 released vm-a",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// A receive in flight that ends without the VM holds the retries back only
// until it ends. The handoff then goes on under the policy, and the destination
// gets its second attempt.
func TestTheRetriesGoOnOnceAReceiveInFlightGivesTheVMUp(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}, "host-2": {}})
	d.hosts["host-0"].hold = holdForAMinute
	d.hosts["host-1"].outlives = 4
	d.hosts["host-1"].outlivedFails = true
	result, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.To != "host-1" {
		t.Fatalf("the VM went to %s, want host-1's second attempt", result.To)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-1 gave vm-a up",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-0 released vm-a",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// A destination that failed and went quiet may be finishing that receive, so
// no other host is asked until it answers again. When it does, and runs
// nothing of the VM, the handoff goes on.
func TestAQuietFailedDestinationHoldsTheRetryBack(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}, "host-2": {}})
	d.hosts["host-0"].hold = holdForAMinute
	d.hosts["host-1"].refusedReceives = 1
	d.hosts["host-1"].quietAfterRefusal = 3
	result, err := d.orchestrator.Migrate(t.Context(), "vm-a", "host-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.To != "host-1" {
		t.Fatalf("the VM went to %s, want host-1 once it answered again", result.To)
	}
	want := []string{
		"host-0 migrate vm-a 10.0.0.2:8081",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-1 answers again",
		"host-1 receive vm-a 10.0.0.1:8081",
		"host-0 released vm-a",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}

// Once the source has stopped the guest, the handover is the orchestrator's to
// finish. A drain's request gives up on its own deadline, and a handover that
// ended with it would lose the guest's writes, so the retries go on without it.
func TestAHandoverOutlivesTheRequestThatAskedForIt(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-0"].hold = holdForAMinute
	d.hosts["host-1"].refusedReceives = 1
	ctx, cancel := context.WithCancel(t.Context())
	d.hosts["host-1"].onRefusal = cancel
	result, err := d.orchestrator.Migrate(ctx, "vm-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if result.To != "host-1" {
		t.Fatalf("the VM went to %s", result.To)
	}
	if !slices.Contains(d.log, "host-0 released vm-a") {
		t.Fatalf("the source was never released: %v", d.log)
	}
}

// A drain's report that it could not hand a VM over comes after its own
// deadline, while the orchestrator may still be carrying the handoff to a
// destination. The row the migration wrote stays as it is.
func TestADrainReportDoesNotOverwriteAHandoverStillUnderWay(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	if err := d.orchestrator.table.Record(t.Context(), vmRecord{ID: "vm-a", Host: "host-0",
		State: stateMigrating, From: "host-0", To: "host-1"}); err != nil {
		t.Fatal(err)
	}
	failed := orch.DrainReport{Host: "host-0", VM: "vm-a", Phase: orch.DrainFinished,
		Error: "context deadline exceeded"}
	if err := d.orchestrator.Drained(t.Context(), failed); err != nil {
		t.Fatal(err)
	}
	row, _, err := d.orchestrator.table.VM(t.Context(), "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if row.State != stateMigrating || row.To != "host-1" {
		t.Fatalf("row %+v, want the handover to host-1 still under way", row)
	}
}
