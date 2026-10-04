package platform_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// putStore takes a fixed time on its clock for every Put, as slowStore does
// for every Get and Head.
type putStore struct {
	slowStore
}

func (s putStore) Put(context.Context, platform.PutRequest) (platform.PutResult, error) {
	s.clock.Advance(s.takes)
	return platform.PutResult{}, nil
}

// A trace lists the calls made under its context, each with its key, whether
// it was conditional, when it began and how long it took, and nothing made
// under another context or after it was closed.
func TestATraceListsItsOwnCallsAndStopsWhenClosed(t *testing.T) {
	clock := sim.New(sim.Config{Seed: 1}).NewClock("store")
	store, err := platform.NewMeteredObjectStore(putStore{slowStore{countingStore: countingStore{size: 8},
		clock: clock, takes: 5 * time.Millisecond}}, clock)
	if err != nil {
		t.Fatal(err)
	}
	var trace platform.ObjectTrace
	traced := platform.WithObjectTrace(t.Context(), &trace)
	start := clock.Now()
	if _, err := store.Put(traced, platform.PutRequest{Key: key(t, "control/a"), Body: bytes.NewReader(nil),
		Conditions: platform.PutConditions{IfNoneMatch: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), platform.PutRequest{Key: key(t, "elsewhere"),
		Body: bytes.NewReader(nil)}); err != nil {
		t.Fatal(err)
	}
	result, err := store.Get(traced, platform.GetRequest{Key: key(t, "vm/a/ckpt/1/index")})
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	if _, err := store.Head(traced, key(t, "vm/a/ckpt/1/part/0")); !errors.Is(err, platform.ErrUnavailable) {
		t.Fatalf("head = %v, want the store's failure", err)
	}
	trace.Close()
	if _, err := store.Put(traced, platform.PutRequest{Key: key(t, "control/a"),
		Body: bytes.NewReader(nil)}); err != nil {
		t.Fatal(err)
	}

	calls, dropped := trace.Calls()
	want := []platform.ObjectCall{
		{Operation: platform.PutOperation, Key: key(t, "control/a"), Conditional: true, Began: start,
			Took: 5 * time.Millisecond},
		{Operation: platform.GetOperation, Key: key(t, "vm/a/ckpt/1/index"), Began: start.Add(10 * time.Millisecond),
			Took: 5 * time.Millisecond},
		{Operation: platform.HeadOperation, Key: key(t, "vm/a/ckpt/1/part/0"),
			Began: start.Add(15 * time.Millisecond), Took: 5 * time.Millisecond, Failed: true},
	}
	if dropped != 0 || len(calls) != len(want) {
		t.Fatalf("the trace listed %d calls and dropped %d: %+v; want %+v", len(calls), dropped, calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d is %+v, want %+v", i, calls[i], want[i])
		}
	}
}

// A trace keeps a bounded number of calls and counts the rest, because work
// that outlives what was traced can inherit its context.
func TestATraceKeepsABoundedNumberOfCalls(t *testing.T) {
	store, err := platform.NewMeteredObjectStore(countingStore{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var trace platform.ObjectTrace
	ctx := platform.WithObjectTrace(t.Context(), &trace)
	for range 1030 {
		if err := store.Delete(ctx, platform.DeleteRequest{Key: key(t, "a")}); err != nil {
			t.Fatal(err)
		}
	}
	calls, dropped := trace.Calls()
	if len(calls) != 1024 || dropped != 6 {
		t.Fatalf("the trace kept %d calls and dropped %d; want 1024 and 6", len(calls), dropped)
	}
}
