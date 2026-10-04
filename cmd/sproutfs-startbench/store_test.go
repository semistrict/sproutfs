package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

// conditionalStore keeps objects in memory, honours a Put's conditions, and
// takes a fixed time per call of each operation. It records each call as
// "<operation> <key> <condition>".
type conditionalStore struct {
	objects map[string]platform.ETag
	written int
	takes   map[platform.ObjectOperation]time.Duration
	calls   []string
}

func (s *conditionalStore) Head(context.Context, platform.ObjectKey) (platform.ObjectMetadata, error) {
	return platform.ObjectMetadata{}, platform.ErrUnavailable
}

func (s *conditionalStore) Get(_ context.Context, request platform.GetRequest) (platform.GetResult, error) {
	time.Sleep(s.takes[platform.GetOperation])
	s.calls = append(s.calls, "get "+request.Key.String())
	if _, found := s.objects[request.Key.String()]; !found {
		return platform.GetResult{}, platform.ErrNotFound
	}
	return platform.GetResult{Body: io.NopCloser(bytes.NewReader(make([]byte, recordBytes)))}, nil
}

func (s *conditionalStore) Put(_ context.Context, request platform.PutRequest) (platform.PutResult, error) {
	time.Sleep(s.takes[platform.PutOperation])
	key := request.Key.String()
	held, found := s.objects[key]
	switch {
	case request.Conditions.IfNoneMatch:
		s.calls = append(s.calls, "put "+key+" if absent")
		if found {
			return platform.PutResult{}, platform.ErrPrecondition
		}
	case request.Conditions.IfMatch != nil:
		s.calls = append(s.calls, fmt.Sprintf("put %s if %s", key, *request.Conditions.IfMatch))
		if !found || held != *request.Conditions.IfMatch {
			return platform.PutResult{}, platform.ErrPrecondition
		}
	default:
		s.calls = append(s.calls, "put "+key)
	}
	s.written++
	etag := platform.ETag(fmt.Sprintf("e%d", s.written))
	s.objects[key] = etag
	return platform.PutResult{Metadata: platform.ObjectMetadata{ETag: etag}}, nil
}

func (s *conditionalStore) Delete(_ context.Context, request platform.DeleteRequest) error {
	time.Sleep(s.takes[platform.DeleteOperation])
	s.calls = append(s.calls, "delete "+request.Key.String())
	delete(s.objects, request.Key.String())
	return nil
}

func (s *conditionalStore) List(context.Context, platform.ListRequest) (platform.ListResult, error) {
	return platform.ListResult{}, platform.ErrUnavailable
}

// The store bench creates each object if absent, reads it, sets it again on
// the ETag the create returned, deletes it, and times each call.
func TestTheStoreBenchTimesACreateAReadACompareAndSetAndADelete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &conditionalStore{objects: map[string]platform.ETag{}, takes: map[platform.ObjectOperation]time.Duration{
			platform.PutOperation: 30 * time.Millisecond, platform.GetOperation: 10 * time.Millisecond,
			platform.DeleteOperation: 20 * time.Millisecond}}
		times, err := timeStore(t.Context(), platform.WallClock(), store, "bench/", 2)
		if err != nil {
			t.Fatal(err)
		}
		want := storeTimes{Create: []float64{30, 30}, Get: []float64{10, 10}, Update: []float64{30, 30},
			Delete: []float64{20, 20}}
		if !slices.Equal(times.Create, want.Create) || !slices.Equal(times.Get, want.Get) ||
			!slices.Equal(times.Update, want.Update) || !slices.Equal(times.Delete, want.Delete) {
			t.Fatalf("the bench timed %+v, want %+v", times, want)
		}
		calls := []string{"put bench/record-0 if absent", "get bench/record-0", "put bench/record-0 if e1",
			"delete bench/record-0", "put bench/record-1 if absent", "get bench/record-1",
			"put bench/record-1 if e3", "delete bench/record-1"}
		if !slices.Equal(store.calls, calls) {
			t.Fatalf("the bench called %q, want %q", store.calls, calls)
		}
		if len(store.objects) != 0 {
			t.Fatalf("the bench left %v behind", store.objects)
		}
	})
}
