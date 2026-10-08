package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
)

// TestAPartialForkTakesBackTheChildrenItStarted: one request forks one parent
// into a set of children, and a set that did not happen leaves nothing behind.
// Whichever of them did start are guests nobody asked for, holding a host's
// memory under identities only the failed request ever knew. The fan-out is
// rolled back the same way wherever the children landed: this one fails while
// the parent's host is still building the handoffs.
func TestAPartialForkTakesBackTheChildrenItStarted(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	d.hosts["host-0"].arena(1024, 100)
	d.hosts["host-0"].templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	// The host hands the first child over and then cannot hand over the second.
	d.hosts["host-0"].forks = 1
	if _, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 2}); err == nil {
		t.Fatal("a fan-out whose host could not hand every child over reported success")
	}
	var deleted []string
	for _, line := range d.log {
		if after, found := strings.CutPrefix(line, "host-0 delete "); found {
			deleted = append(deleted, strings.Fields(after)[0])
		}
	}
	if len(deleted) != 2 {
		t.Fatalf("the failed fan-out deleted %v, want both of the children it named: %v", deleted, d.log)
	}
	if started := d.hosts["host-0"].running; len(started) != 1 || started[0] != "vm-a" {
		t.Fatalf("host-0 still runs %v, want the parent alone", started)
	}
}

// TestAPartialForkOnTheParentsOwnHostTakesItsChildrenBack: a fork is always a
// handoff, so a fan-out onto the parent's own host is rolled back by exactly
// what rolls one onto another host back — the children that were taken in are
// deleted, every child is released so the parent takes its pages back, and
// nothing of the request is left running.
func TestAPartialForkOnTheParentsOwnHostTakesItsChildrenBack(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	// The host takes the first child in and refuses the second.
	d.hosts["host-0"].receives = 1
	if _, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 2}); err == nil {
		t.Fatal("a fan-out whose host refused a child reported success")
	}
	started, deleted := "", ""
	var released, abandoned []string
	for _, line := range d.log {
		if after, found := strings.CutPrefix(line, "host-0 receive "); found && started == "" {
			started = strings.Fields(after)[0]
		}
		if after, found := strings.CutPrefix(line, "host-0 delete "); found {
			deleted = strings.Fields(after)[0]
		}
		if after, found := strings.CutPrefix(line, "host-0 released "); found {
			released = append(released, strings.Fields(after)[0])
		}
		if after, found := strings.CutPrefix(line, "host-0 abandoned "); found {
			abandoned = append(abandoned, strings.Fields(after)[0])
		}
	}
	if started == "" {
		t.Fatalf("no child was started at all: %v", d.log)
	}
	if deleted != started {
		t.Fatalf("the fork started %s and deleted %q: %v", started, deleted, d.log)
	}
	// The child that was taken in has every page it inherited, so its hold is
	// released. The one that never started has none of them and never will:
	// releasing it is a request the source can only refuse, for ever, because
	// the pages it holds exist nowhere else. It is given up instead, which is
	// what takes the parent's seal off it.
	if len(released) != 1 || released[0] != started {
		t.Fatalf("the fan-out released %v, want the child it started: %v", released, d.log)
	}
	if len(abandoned) != 1 || abandoned[0] == started {
		t.Fatalf("the fan-out gave up %v, want the child that never started: %v", abandoned, d.log)
	}
	// And the parent's host holds nothing for either of them, so nothing is
	// left for a survey to keep asking about.
	if serving := d.hosts["host-0"].serving; len(serving) != 0 {
		t.Fatalf("host-0 still holds %v after the fan-out was rolled back: %v", serving, d.log)
	}
	if running := d.hosts["host-0"].running; len(running) != 1 || running[0] != "vm-a" {
		t.Fatalf("host-0 still runs %v, want the parent alone", running)
	}
}

// TestAForkWhoseFirstChildIsRefusedGivesUpTheRestOnTheSource: a fan-out is one
// fork point, and the children after the one that failed are never even offered
// to a destination. Their holds are on the parent all the same — the point was
// taken for all of them at once — and nothing will ever fetch what those holds
// keep, so the only word that ends them is the source's own give-up. Asking it
// to release them instead is asking for something it must refuse: the pages it
// holds are the only copy, and a release that took them would lose the guest's
// writes since the parent's last checkpoint.
func TestAForkWhoseFirstChildIsRefusedGivesUpTheRestOnTheSource(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-1"].arena(1024, 100)
	// The destination refuses every child, starting with the first.
	d.hosts["host-1"].refusesEveryReceive = true
	if _, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 3, To: "host-1"}); err == nil {
		t.Fatal("a fan-out whose destination refused every child reported success")
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
		t.Fatalf("the fan-out asked the source to release %v, none of which was ever received: %v",
			released, d.log)
	}
	if len(abandoned) != 3 {
		t.Fatalf("the fan-out gave up %v on the source, want all three children: %v", abandoned, d.log)
	}
	if serving := d.hosts["host-0"].serving; len(serving) != 0 {
		t.Fatalf("the parent's host still holds %v for children that never started", serving)
	}
}

// TestAForkChildWhoseAnswerWasLostIsDeleted: the destination took the first
// child in, claiming its hold, and the answer never came back, so the fan-out
// failed there. The give-up on the parent's host comes after the claim, and
// says so: the child runs on the destination under an identity only the failed
// fork knew, and it is deleted with the children that started. The second
// child is never offered, and its hold is given up unclaimed.
func TestAForkChildWhoseAnswerWasLostIsDeleted(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-1"].arena(1024, 100)
	d.hosts["host-1"].loseAnswer = "Receive"
	if _, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 2, To: "host-1"}); err == nil {
		t.Fatal("a fan-out whose first child's answer was lost reported success")
	}
	want := []string{
		"host-0 fork vm-a vm-new-1,vm-new-2",
		"host-1 receive vm-new-1 10.0.0.1:8081",
		"host-0 abandoned vm-new-1 claimed",
		"host-0 abandoned vm-new-2",
		"host-1 delete vm-new-1",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	if running := d.hosts["host-1"].running; len(running) != 0 {
		t.Fatalf("host-1 still runs %v", running)
	}
	if running := d.hosts["host-0"].running; !slices.Equal(running, []string{"vm-a"}) {
		t.Fatalf("host-0 runs %v, want the parent alone", running)
	}
}

// TestAChildWhoseReceiveOutlivesItsFanOutNeverRuns: the first child's receive
// fails for its caller and goes on on its host, so the fan-out gives both
// holds up. When that receive has the child's pages it claims the hold, which
// is no longer kept, and the destination gives the child up rather than run
// it. Nothing is left for anyone to delete.
func TestAChildWhoseReceiveOutlivesItsFanOutNeverRuns(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-1"].arena(1024, 100)
	d.hosts["host-1"].outlives = 2
	if _, err := d.orchestrator.Fork(t.Context(), "vm-a", orch.ForkRequest{Count: 2, To: "host-1"}); err == nil {
		t.Fatal("a fan-out whose first child's receive failed reported success")
	}
	for range 2 {
		if err := d.orchestrator.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"host-0 fork vm-a vm-new-1,vm-new-2",
		"host-1 receive vm-new-1 10.0.0.1:8081",
		"host-0 abandoned vm-new-1",
		"host-0 abandoned vm-new-2",
		"host-1 gave vm-new-1 up",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	if running := d.hosts["host-1"].running; len(running) != 0 {
		t.Fatalf("host-1 runs %v", running)
	}
}

// TestAForkWhoseCallerHangsUpIsStillRolledBack: the caller of a fan-out hangs
// up while the first child is being received. The receive ends with it, and
// the rollback runs all the same: both holds are given up on the parent's
// host, which is what unseals the parent, rather than left to the host's own
// deadline.
func TestAForkWhoseCallerHangsUpIsStillRolledBack(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-1"].arena(1024, 100)
	ctx, hangUp := context.WithCancel(t.Context())
	d.hosts["host-1"].holdReceive = true
	d.hosts["host-1"].onReceive = hangUp
	if _, err := d.orchestrator.Fork(ctx, "vm-a", orch.ForkRequest{Count: 2, To: "host-1"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a fan-out whose caller hung up = %v, want it cancelled", err)
	}
	want := []string{
		"host-0 fork vm-a vm-new-1,vm-new-2",
		"host-1 receive vm-new-1 10.0.0.1:8081",
		"host-1 receive-discarded vm-new-1",
		"host-0 abandoned vm-new-1",
		"host-0 abandoned vm-new-2",
	}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
	if serving := d.hosts["host-0"].serving; len(serving) != 0 {
		t.Fatalf("the parent's host still holds %v", serving)
	}
}
