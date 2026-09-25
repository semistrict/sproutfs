package sim_test

import (
	"cmp"
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	simv1 "github.com/semistrict/sproutfs/platform/internal/gen/sproutfs/sim/v1"
	"github.com/semistrict/sproutfs/platform/sim"
)

// PROTOTYPE: can timing windows produce overlapping, seed-replayable races?
// Run: go test ./platform/sim -run '^TestOverlapPrototype' -v -count=1
// Deliberately limited to a declared batch of independent, nonblocking actions.
// No scheduler integration, recorded replay input, or production API changes.
type overlapPrototypeAction struct {
	id               string
	earliest, latest time.Duration
	apply            func() error
}

type overlapPrototypeEvent struct {
	id string
	at time.Duration
}

// Everyone registers before any release, making eligibility independent of Go
// scheduling. Each round chooses the earliest deadline and groups all windows
// containing it. A stable keyed priority orders actions at that moment.
func runOverlapPrototype(t *testing.T, random sim.Random, actions []overlapPrototypeAction, trace *overlapPrototypeTrace) []overlapPrototypeEvent {
	t.Helper()
	start := time.Now()
	ready := make(chan struct{}, len(actions))
	done := make(chan error)
	gates := make(map[string]chan struct{}, len(actions))
	for _, action := range actions {
		if action.id == "" || gates[action.id] != nil || action.earliest < 0 || action.latest < action.earliest {
			t.Fatal("invalid prototype action")
		}
		trace.recordWindow(action)
		gate := make(chan struct{})
		gates[action.id] = gate
		go func() {
			trace.record(simv1.EventKind_EVENT_KIND_READY, action.id, "", nil)
			ready <- struct{}{}
			<-gate
			done <- action.apply()
		}()
	}
	for range actions {
		<-ready
	}
	trace.record(simv1.EventKind_EVENT_KIND_BARRIER, "scheduler/ready", "", nil)
	pending := slices.Clone(actions)
	var events []overlapPrototypeEvent
	for len(pending) > 0 {
		at := pending[0].latest
		for _, action := range pending[1:] {
			at = min(at, action.latest)
		}
		var batch, remaining []overlapPrototypeAction
		for _, action := range pending {
			if action.earliest <= at {
				batch = append(batch, action)
			} else {
				remaining = append(remaining, action)
			}
		}
		slices.SortFunc(batch, func(a, b overlapPrototypeAction) int {
			pa, pb := random.Uint64(a.id), random.Uint64(b.id)
			if pa < pb {
				return -1
			}
			if pa > pb {
				return 1
			}
			return cmp.Compare(a.id, b.id)
		})
		time.Sleep(at - time.Since(start))
		for _, action := range batch {
			trace.record(simv1.EventKind_EVENT_KIND_RELEASED, action.id, "", nil)
			close(gates[action.id])
			err := <-done
			trace.record(simv1.EventKind_EVENT_KIND_COMPLETED, action.id, traceOutcome(err), nil)
			if err != nil {
				t.Fatal(err)
			}
			if time.Since(start) != at {
				t.Fatal("prototype actions must not advance time or wait for another action")
			}
			events = append(events, overlapPrototypeEvent{action.id, at})
		}
		pending = remaining
	}
	return events
}

func TestOverlapPrototypeReplaysListenerRaceFromSeed(t *testing.T) {
	seen := map[string]bool{}
	for seed := uint64(1); seed <= 32; seed++ {
		first := runOverlapListenerRace(t, seed, false, nil)
		second := runOverlapListenerRace(t, seed, true, nil)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("seed %d changed with reversed registration order: %v vs %v", seed, first, second)
		}
		if len(first) != 2 || first[0].at != 15*time.Millisecond || first[1].at != first[0].at {
			t.Fatalf("windows [5,15] and [10,20] did not overlap at 15ms: %v", first)
		}
		if !seen[first[0].id] {
			t.Logf("seed=%d windows: accept=[5,15]ms close=[10,20]ms; schedule=%v", seed, first)
		}
		seen[first[0].id] = true
	}
	if len(seen) != 2 {
		t.Fatal("the seeds did not explore both accept-before-close and close-before-accept")
	}
}

func runOverlapListenerRace(t *testing.T, seed uint64, reverse bool, trace *overlapPrototypeTrace) []overlapPrototypeEvent {
	t.Helper()
	var events []overlapPrototypeEvent
	synctest.Test(t, func(t *testing.T) {
		if trace != nil {
			trace.start = time.Now()
		}
		runtime := sim.New(sim.Config{Seed: seed})
		listener, err := runtime.Network().Listen("source")
		trace.record(simv1.EventKind_EVENT_KIND_LISTEN, "source/listen/1", traceOutcome(err), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			trace.record(simv1.EventKind_EVENT_KIND_CLOSE, "source/listener/cleanup", traceOutcome(listener.Close()), nil)
		}()
		client, err := runtime.Network().Dial(t.Context(), "destination", "source")
		trace.record(simv1.EventKind_EVENT_KIND_DIAL, "destination/dial/1", traceOutcome(err), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			trace.record(simv1.EventKind_EVENT_KIND_CLOSE, "destination/client/cleanup", traceOutcome(client.Close()), nil)
		}()
		var accepted platform.Conn
		var acceptErr error
		actions := []overlapPrototypeAction{
			{id: "source/accept/1", earliest: 5 * time.Millisecond, latest: 15 * time.Millisecond, apply: func() error {
				accepted, acceptErr = listener.Accept(t.Context())
				trace.record(simv1.EventKind_EVENT_KIND_ACCEPT, "source/accept/1", traceOutcome(acceptErr), nil)
				return nil // Either outcome is checked against the chosen order below.
			}},
			{id: "source/close/1", earliest: 10 * time.Millisecond, latest: 20 * time.Millisecond, apply: func() error {
				err := listener.Close()
				trace.record(simv1.EventKind_EVENT_KIND_CLOSE, "source/close/1", traceOutcome(err), nil)
				return err
			}},
		}
		if reverse {
			slices.Reverse(actions)
		}
		events = runOverlapPrototype(t, runtime.Random("overlap-prototype"), actions, trace)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if events[0].id == "source/accept/1" {
			if acceptErr != nil || accepted == nil {
				t.Fatalf("accept before close failed: %v", acceptErr)
			}
			defer func() {
				trace.record(simv1.EventKind_EVENT_KIND_CLOSE, "source/accepted/cleanup", traceOutcome(accepted.Close()), nil)
			}()
			payload := []byte("accepted connection survives")
			err := client.Send(ctx, platform.Frame{Header: payload})
			trace.record(simv1.EventKind_EVENT_KIND_SEND, "destination/send/1", traceOutcome(err), payload)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := accepted.Receive(ctx)
			trace.record(simv1.EventKind_EVENT_KIND_RECEIVE, "source/receive/1", traceOutcome(err), frame.Header)
			if err != nil || string(frame.Header) != string(payload) {
				t.Fatalf("accepted connection receive: header=%q err=%v", frame.Header, err)
			}
			if err := frame.Payload.Close(); err != nil {
				t.Fatal(err)
			}
		} else {
			if !errors.Is(acceptErr, platform.ErrClosed) || accepted != nil {
				t.Fatalf("close before accept retained the queued connection: %v", acceptErr)
			}
			_, err := client.Receive(ctx)
			trace.record(simv1.EventKind_EVENT_KIND_RECEIVE, "destination/receive/1", traceOutcome(err), nil)
			if !errors.Is(err, platform.ErrDisconnected) {
				t.Fatalf("unaccepted client did not disconnect: %v", err)
			}
		}
	})
	return events
}

func TestOverlapPrototypeKeepsDisjointWindowsApart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1})
		nothing := func() error { return nil }
		events := runOverlapPrototype(t, runtime.Random("overlap-prototype"), []overlapPrototypeAction{
			{id: "late", earliest: 8 * time.Millisecond, latest: 10 * time.Millisecond, apply: nothing},
			{id: "early", earliest: time.Millisecond, latest: 4 * time.Millisecond, apply: nothing},
			{id: "overlap", earliest: 3 * time.Millisecond, latest: 6 * time.Millisecond, apply: nothing},
		}, nil)
		if len(events) != 3 || events[0].at != 4*time.Millisecond || events[1].at != 4*time.Millisecond ||
			events[2] != (overlapPrototypeEvent{"late", 10 * time.Millisecond}) {
			t.Fatalf("scheduler forced incompatible windows together: %v", events)
		}
		t.Logf("separate windows produce two release moments: %v", events)
	})
}
