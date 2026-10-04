package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/platform"
)

// fakeHost takes a fixed time for each call and runs a VM's agent from a
// fixed time after the call that starts the VM began. Every call is recorded
// as "<host> <call> <vm>".
type fakeHost struct {
	name  string
	calls *[]string
	mu    *sync.Mutex
	// takes is each call's time, by name; agent is when an agent answers,
	// from the start of the call that started its VM.
	takes        map[string]time.Duration
	agent        time.Duration
	failReceive  bool
	answersAfter map[string]time.Time
}

func (h *fakeHost) call(name, vm string) {
	h.mu.Lock()
	*h.calls = append(*h.calls, h.name+" "+name+" "+vm)
	h.mu.Unlock()
	time.Sleep(h.takes[name])
}

func (h *fakeHost) starting(vm string) {
	h.mu.Lock()
	h.answersAfter[vm] = time.Now().Add(h.agent)
	h.mu.Unlock()
}

func (h *fakeHost) Status(context.Context) (hostapi.Status, error) {
	return hostapi.Status{Host: h.name, PageAddress: h.name + "-pages"}, nil
}

func (h *fakeHost) Create(_ context.Context, request hostapi.CreateRequest) (hostapi.CreateResult, error) {
	h.starting(request.ID)
	h.call("create", request.ID)
	return hostapi.CreateResult{}, nil
}

func (h *fakeHost) Open(_ context.Context, id string, _ hostapi.OpenRequest) (hostapi.OpenResult, error) {
	h.starting(id)
	h.call("open", id)
	return hostapi.OpenResult{}, nil
}

func (h *fakeHost) Fork(_ context.Context, parent string, request hostapi.ForkRequest) (hostapi.ForkResult, error) {
	h.call("fork", parent)
	return hostapi.ForkResult{Handoffs: []hostapi.Handoff{{VMID: request.IDs[0], Parent: parent,
		Source: request.Destination}}}, nil
}

func (h *fakeHost) Receive(_ context.Context, handoff hostapi.Handoff) (hostapi.ReceiveResult, error) {
	h.starting(handoff.VMID)
	h.call("receive", handoff.VMID)
	if h.failReceive {
		return hostapi.ReceiveResult{}, errors.New("the receive failed")
	}
	return hostapi.ReceiveResult{}, nil
}

func (h *fakeHost) Released(_ context.Context, id string) error {
	h.call("released", id)
	return nil
}

func (h *fakeHost) Abandoned(_ context.Context, id string) (hostapi.AbandonedResult, error) {
	h.call("abandoned", id)
	return hostapi.AbandonedResult{}, nil
}

func (h *fakeHost) Exec(_ context.Context, id string, _ hostapi.ExecRequest) (hostapi.ExecResult, error) {
	h.mu.Lock()
	after, found := h.answersAfter[id]
	h.mu.Unlock()
	if !found || time.Now().Before(after) {
		return hostapi.ExecResult{}, fmt.Errorf("the guest of %s is not answering", id)
	}
	return hostapi.ExecResult{}, nil
}

func (h *fakeHost) Stop(_ context.Context, id string, _ hostapi.StopRequest) (hostapi.StopResult, error) {
	h.call("stop", id)
	h.mu.Lock()
	delete(h.answersAfter, id)
	h.mu.Unlock()
	return hostapi.StopResult{}, nil
}

func (h *fakeHost) Delete(_ context.Context, id string) error {
	h.call("delete", id)
	return nil
}

// world is two fake hosts, a driver over them, and what it wrote and asked.
type world struct {
	a, b    *fakeHost
	driver  *driver
	samples []sample
	calls   []string
}

func newWorld() *world {
	w := &world{}
	var mu sync.Mutex
	takes := map[string]time.Duration{"create": 30 * time.Millisecond, "open": 20 * time.Millisecond,
		"fork": 10 * time.Millisecond, "receive": 40 * time.Millisecond, "stop": 50 * time.Millisecond}
	w.a = &fakeHost{name: "a", calls: &w.calls, mu: &mu, takes: takes, agent: 100 * time.Millisecond,
		answersAfter: map[string]time.Time{}}
	w.b = &fakeHost{name: "b", calls: &w.calls, mu: &mu, takes: takes, agent: 100 * time.Millisecond,
		answersAfter: map[string]time.Time{}}
	w.driver = &driver{clock: platform.WallClock(), hosts: []target{{"a", "a-pages", w.a}, {"b", "b-pages", w.b}},
		template: "alpine", tag: "t", interval: time.Second, poll: 10 * time.Millisecond, patience: time.Minute,
		write: func(s sample) error {
			w.samples = append(w.samples, s)
			return nil
		}}
	return w
}

func (w *world) check(t *testing.T, samples []sample, calls []string) {
	t.Helper()
	if !slices.Equal(w.samples, samples) {
		t.Errorf("the driver wrote\n%+v\nwant\n%+v", w.samples, samples)
	}
	if !slices.Equal(w.calls, calls) {
		t.Errorf("the driver called\n%q\nwant\n%q", w.calls, calls)
	}
}

// A cold start's sample is the create's round trip and the agent's first
// answer, which the driver asks for from the create's start, every poll.
func TestAColdStartIsTimedToItsAgentsAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld()
		if err := w.driver.run(t.Context(), []string{caseCold}, 2); err != nil {
			t.Fatal(err)
		}
		w.check(t, []sample{
			{Case: caseCold, Round: 0, VM: "startbench-t-cold-0", Host: "a", RequestMS: 30, Started: true,
				AgentMS: 100, Execs: 11},
			{Case: caseCold, Round: 1, VM: "startbench-t-cold-1", Host: "a", RequestMS: 30, Started: true,
				AgentMS: 100, Execs: 11},
		}, []string{"a create startbench-t-cold-0", "a delete startbench-t-cold-0",
			"a create startbench-t-cold-1", "a delete startbench-t-cold-1"})
	})
}

// A restore opens two rounds on each host in turn, and suspends the VM after
// each, so half the restores are on the host that suspended it.
func TestRestoresTakeTurnsOnTheHosts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld()
		if err := w.driver.run(t.Context(), []string{caseRestore}, 3); err != nil {
			t.Fatal(err)
		}
		id := "startbench-t-restore-0"
		w.check(t, []sample{
			{Case: caseRestore, Round: 0, VM: id, Host: "a", From: "a", RequestMS: 20, Started: true, AgentMS: 100,
				Execs: 11, StopMS: 50},
			{Case: caseRestore, Round: 1, VM: id, Host: "a", From: "a", RequestMS: 20, Started: true, AgentMS: 100,
				Execs: 11, StopMS: 50},
			{Case: caseRestore, Round: 2, VM: id, Host: "b", From: "a", RequestMS: 20, Started: true, AgentMS: 100,
				Execs: 11, StopMS: 50},
		}, []string{"a create " + id, "a stop " + id, "a open " + id, "a stop " + id, "a open " + id, "a stop " + id,
			"b open " + id, "b stop " + id, "a delete " + id})
	})
}

// A fork to another host is the fork on the parent's host, the receive on the
// child's and the release on the parent's, and its agent is asked from the
// receive's start.
func TestAForkToAnotherHostIsCarriedAsTheOrchestratorCarriesOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld()
		if err := w.driver.run(t.Context(), []string{caseForkRemote}, 1); err != nil {
			t.Fatal(err)
		}
		parent, child := "startbench-t-fork-remote-parent-0", "startbench-t-fork-remote-0"
		w.check(t, []sample{
			{Case: caseForkRemote, Round: 0, VM: child, Parent: parent, Host: "b", From: "a", ForkMS: 10,
				RequestAtMS: 10, RequestMS: 40, Started: true, AgentMS: 110, Execs: 11},
		}, []string{"a create " + parent, "a fork " + parent, "b receive " + child, "a released " + child,
			"b delete " + child, "a delete " + parent})
	})
}

// A receive that fails gives the parent's hold up, as the orchestrator does,
// and the next round goes on.
func TestAFailedReceiveGivesTheHoldUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newWorld()
		w.a.failReceive = true
		if err := w.driver.run(t.Context(), []string{caseForkLocal}, 1); err != nil {
			t.Fatal(err)
		}
		parent, child := "startbench-t-fork-local-parent-0", "startbench-t-fork-local-0"
		w.check(t, []sample{
			{Case: caseForkLocal, Round: 0, VM: child, Parent: parent, Host: "a", From: "a", ForkMS: 10,
				RequestAtMS: 10, RequestMS: 40, Error: "receive: the receive failed"},
		}, []string{"a create " + parent, "a fork " + parent, "a receive " + child, "a abandoned " + child,
			"a delete " + child, "a delete " + parent})
	})
}
