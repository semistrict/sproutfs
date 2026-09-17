package platform_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/semistrict/sproutfs/internal/platform"
)

// countingStore answers every call the same way, so a test asserts what the
// meter recorded rather than what a real store would have done.
type countingStore struct {
	size int64
	fail error
}

func (s countingStore) Head(context.Context, platform.ObjectKey) (platform.ObjectMetadata, error) {
	return platform.ObjectMetadata{}, s.fail
}

func (s countingStore) Get(_ context.Context, request platform.GetRequest) (platform.GetResult, error) {
	if s.fail != nil {
		return platform.GetResult{}, s.fail
	}
	return platform.GetResult{
		Metadata:      platform.ObjectMetadata{Key: request.Key, Size: s.size},
		ContentLength: s.size,
		Body:          io.NopCloser(bytes.NewReader(make([]byte, s.size))),
	}, nil
}

func (s countingStore) Put(context.Context, platform.PutRequest) (platform.PutResult, error) {
	return platform.PutResult{}, s.fail
}

func (s countingStore) Delete(context.Context, platform.DeleteRequest) error { return s.fail }

func (s countingStore) List(context.Context, platform.ListRequest) (platform.ListResult, error) {
	return platform.ListResult{}, s.fail
}

func key(t *testing.T, value string) platform.ObjectKey {
	t.Helper()
	k, err := platform.NewObjectKey(value)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestAMeteredStoreCountsEveryOperation(t *testing.T) {
	store, err := platform.NewMeteredObjectStore(countingStore{size: 4096})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if _, err := store.Head(ctx, key(t, "a")); err != nil {
		t.Fatal(err)
	}
	result, err := store.Get(ctx, platform.GetRequest{Key: key(t, "a")})
	if err != nil {
		t.Fatal(err)
	}
	result.Body.Close()
	if _, err := store.Put(ctx, platform.PutRequest{Key: key(t, "a"),
		Body: bytes.NewReader(make([]byte, 300)), Size: 300}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, platform.DeleteRequest{Key: key(t, "a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx, platform.ListRequest{}); err != nil {
		t.Fatal(err)
	}

	traffic := store.Traffic()
	if traffic.Head != (platform.ObjectCount{Calls: 1}) {
		t.Fatalf("head %+v", traffic.Head)
	}
	if traffic.Get != (platform.ObjectCount{Calls: 1, Bytes: 4096}) {
		t.Fatalf("get %+v", traffic.Get)
	}
	if traffic.Put != (platform.ObjectCount{Calls: 1, Bytes: 300}) {
		t.Fatalf("put %+v", traffic.Put)
	}
	if traffic.Delete != (platform.ObjectCount{Calls: 1}) {
		t.Fatalf("delete %+v", traffic.Delete)
	}
	if traffic.List != (platform.ObjectCount{Calls: 1}) {
		t.Fatalf("list %+v", traffic.List)
	}
	if total := traffic.Total(); total != (platform.ObjectCount{Calls: 5, Bytes: 4396}) {
		t.Fatalf("total %+v", total)
	}
}

func TestAFailedCallCountsAsAFailureAndMovesNoBytes(t *testing.T) {
	store, err := platform.NewMeteredObjectStore(countingStore{size: 4096, fail: platform.ErrNotFound})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), platform.GetRequest{Key: key(t, "a")}); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("get returned %v", err)
	}
	if _, err := store.Put(t.Context(), platform.PutRequest{Key: key(t, "a"),
		Body: bytes.NewReader(nil), Size: 300}); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("put returned %v", err)
	}
	traffic := store.Traffic()
	if traffic.Get != (platform.ObjectCount{Calls: 1, Failures: 1}) {
		t.Fatalf("get %+v", traffic.Get)
	}
	if traffic.Put != (platform.ObjectCount{Calls: 1, Failures: 1}) {
		t.Fatalf("put %+v", traffic.Put)
	}
}

// A meter on the context is what tells one unit of work's traffic from another's
// when both go through the same store.
func TestAContextMeterCountsOnlyItsOwnCalls(t *testing.T) {
	store, err := platform.NewMeteredObjectStore(countingStore{size: 8})
	if err != nil {
		t.Fatal(err)
	}
	var mine, theirs platform.ObjectMeter
	put := func(ctx context.Context, size int64) {
		t.Helper()
		if _, err := store.Put(ctx, platform.PutRequest{Key: key(t, "a"),
			Body: bytes.NewReader(make([]byte, size)), Size: size}); err != nil {
			t.Error(err)
		}
	}
	put(platform.WithObjectMeter(t.Context(), &mine), 100)
	put(platform.WithObjectMeter(t.Context(), &mine), 200)
	put(platform.WithObjectMeter(t.Context(), &theirs), 900)
	put(t.Context(), 7)

	if got := mine.Traffic().Put; got != (platform.ObjectCount{Calls: 2, Bytes: 300}) {
		t.Fatalf("mine %+v", got)
	}
	if got := theirs.Traffic().Put; got != (platform.ObjectCount{Calls: 1, Bytes: 900}) {
		t.Fatalf("theirs %+v", got)
	}
	// The store's totals hold every call, attributed or not.
	if got := store.Traffic().Put; got != (platform.ObjectCount{Calls: 4, Bytes: 1207}) {
		t.Fatalf("totals %+v", got)
	}
}

// One checkpoint's uploads run on many goroutines at once, so its meter has to
// be safe for concurrent use.
func TestAMeterCountsConcurrentCalls(t *testing.T) {
	store, err := platform.NewMeteredObjectStore(countingStore{size: 8})
	if err != nil {
		t.Fatal(err)
	}
	var meter platform.ObjectMeter
	ctx := platform.WithObjectMeter(t.Context(), &meter)
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if _, err := store.Put(ctx, platform.PutRequest{Key: key(t, "a"),
				Body: bytes.NewReader(make([]byte, 16)), Size: 16}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := meter.Traffic().Put; got != (platform.ObjectCount{Calls: 64, Bytes: 1024}) {
		t.Fatalf("meter %+v", got)
	}
}

func TestAMeteredStoreNeedsAStore(t *testing.T) {
	if _, err := platform.NewMeteredObjectStore(nil); !errors.Is(err, platform.ErrNoObjectStore) {
		t.Fatalf("wrapping nothing returned %v", err)
	}
}
