package sim_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

func TestObjectStoreStreamsRangesAndEnforcesCAS(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		store := runtime.ObjectStore()
		key, err := platform.NewObjectKey("metadata/log-1")
		if err != nil {
			t.Fatal(err)
		}
		first := []byte("version one")
		created, err := store.Put(t.Context(), platform.PutRequest{
			Key:        key,
			Body:       bytes.NewReader(first),
			Size:       int64(len(first)),
			Attributes: map[string]string{"committed-lsn": "7"},
			Conditions: platform.PutConditions{IfNoneMatch: true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(t.Context(), platform.PutRequest{
			Key:        key,
			Body:       bytes.NewReader(first),
			Size:       int64(len(first)),
			Conditions: platform.PutConditions{IfNoneMatch: true},
		}); !errors.Is(err, platform.ErrPrecondition) {
			t.Fatalf("second create error = %v, want ErrPrecondition", err)
		}
		created.Metadata.Attributes["committed-lsn"] = "mutated"
		head, err := store.Head(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		if got := head.Attributes["committed-lsn"]; got != "7" {
			t.Fatalf("stored attribute = %q, want 7", got)
		}

		second := []byte("version two")
		updated, err := store.Put(t.Context(), platform.PutRequest{
			Key:        key,
			Body:       bytes.NewReader(second),
			Size:       int64(len(second)),
			Attributes: map[string]string{"committed-lsn": "9"},
			Conditions: platform.PutConditions{IfMatch: &created.Metadata.ETag},
		})
		if err != nil {
			t.Fatal(err)
		}
		if updated.Metadata.ETag == created.Metadata.ETag {
			t.Fatal("ETag did not change after content changed")
		}

		result, err := store.Get(t.Context(), platform.GetRequest{
			Key:   key,
			Range: &platform.ByteRange{Offset: 8, Length: 3},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer result.Body.Close()
		body, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(body), "two"; got != want {
			t.Fatalf("range body = %q, want %q", got, want)
		}
		if result.Metadata.Size != int64(len(second)) || result.ContentLength != 3 {
			t.Fatalf("metadata size/content length = %d/%d", result.Metadata.Size, result.ContentLength)
		}
		if got := result.Metadata.Attributes["committed-lsn"]; got != "9" {
			t.Fatalf("GET attribute = %q, want 9", got)
		}
	})
}

func TestObjectStoreRejectsOverflowingByteRange(t *testing.T) {
	key, err := platform.NewObjectKey("object")
	if err != nil {
		t.Fatal(err)
	}
	store := sim.New(sim.Config{}).ObjectStore()
	_, err = store.Get(t.Context(), platform.GetRequest{
		Key: key,
		Range: &platform.ByteRange{
			Offset: math.MaxInt64,
			Length: 2,
		},
	})
	if !errors.Is(err, platform.ErrInvalidRange) {
		t.Fatalf("Get error = %v, want ErrInvalidRange", err)
	}
}

func TestObjectStoreListsStablePages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := sim.New(sim.Config{}).ObjectStore()
		for _, value := range []string{"logs/c", "other/a", "logs/a", "logs/b"} {
			key, err := platform.NewObjectKey(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Put(t.Context(), platform.PutRequest{
				Key: key, Body: bytes.NewReader([]byte(value)), Size: int64(len(value)),
			}); err != nil {
				t.Fatal(err)
			}
		}
		prefix, err := platform.NewObjectPrefix("logs/")
		if err != nil {
			t.Fatal(err)
		}
		first, err := store.List(t.Context(), platform.ListRequest{Prefix: prefix, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		if got := []string{first.Objects[0].Key.String(), first.Objects[1].Key.String()}; !slices.Equal(got, []string{"logs/a", "logs/b"}) {
			t.Fatalf("first page = %v", got)
		}
		second, err := store.List(t.Context(), platform.ListRequest{Prefix: prefix, ContinuationToken: first.NextContinuationToken})
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Objects) != 1 || second.Objects[0].Key.String() != "logs/c" {
			t.Fatalf("second page = %#v", second.Objects)
		}
	})
}

func TestObjectStoreInjectsEveryOperationAndRecovers(t *testing.T) {
	tests := []struct {
		operation sim.ObjectOperation
		run       func(context.Context, *sim.ObjectStore, platform.ObjectKey) error
	}{
		{sim.ObjectHead, func(ctx context.Context, store *sim.ObjectStore, key platform.ObjectKey) error {
			_, err := store.Head(ctx, key)
			return err
		}},
		{sim.ObjectGet, func(ctx context.Context, store *sim.ObjectStore, key platform.ObjectKey) error {
			_, err := store.Get(ctx, platform.GetRequest{Key: key})
			return err
		}},
		{sim.ObjectPut, func(ctx context.Context, store *sim.ObjectStore, key platform.ObjectKey) error {
			_, err := store.Put(ctx, platform.PutRequest{Key: key, Body: bytes.NewReader([]byte("new")), Size: 3})
			return err
		}},
		{sim.ObjectDelete, func(ctx context.Context, store *sim.ObjectStore, key platform.ObjectKey) error {
			return store.Delete(ctx, platform.DeleteRequest{Key: key})
		}},
		{sim.ObjectList, func(ctx context.Context, store *sim.ObjectStore, _ platform.ObjectKey) error {
			_, err := store.List(ctx, platform.ListRequest{})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(string(test.operation), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := sim.New(sim.Config{})
				store := runtime.ObjectStore()
				key := mustObjectKey(t, "objects/value")
				if _, err := store.Put(t.Context(), platform.PutRequest{
					Key: key, Body: bytes.NewReader([]byte("value")), Size: 5,
				}); err != nil {
					t.Fatal(err)
				}
				runtime.Trace().Reset()
				store.FailNext(test.operation, 1)
				if err := test.run(t.Context(), store, key); !errors.Is(err, platform.ErrInjectedFault) {
					t.Fatalf("%s error = %v, want ErrInjectedFault", test.operation, err)
				}
				assertTraceEvent(t, runtime.Trace().Events(), "object_store", string(test.operation), "injected_fault")
			})
		})
	}

	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		store := runtime.ObjectStore()
		key := mustObjectKey(t, "objects/value")
		if _, err := store.Put(t.Context(), platform.PutRequest{
			Key: key, Body: bytes.NewReader([]byte("value")), Size: 5,
		}); err != nil {
			t.Fatal(err)
		}
		runtime.Trace().Reset()
		store.Fail()
		if _, err := store.Head(t.Context(), key); !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("Head while failed error = %v, want ErrUnavailable", err)
		}
		assertTraceEvent(t, runtime.Trace().Events(), "object_store", string(sim.ObjectHead), "unavailable")
		store.Recover()
		if _, err := store.Head(t.Context(), key); err != nil {
			t.Fatalf("Head after recovery: %v", err)
		}
	})
}

func TestObjectStoreCanLoseAResponseAfterApplyingAMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		store := runtime.ObjectStore()
		key := mustObjectKey(t, "objects/ambiguous")
		store.FailNextAfterApply(sim.ObjectPut, 1)
		if _, err := store.Put(t.Context(), platform.PutRequest{
			Key: key, Body: bytes.NewReader([]byte("value")), Size: 5,
		}); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("ambiguous Put error = %v, want ErrInjectedFault", err)
		}
		result, err := store.Get(t.Context(), platform.GetRequest{Key: key})
		if err != nil {
			t.Fatal(err)
		}
		defer result.Body.Close()
		value, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(value) != "value" {
			t.Fatalf("value after ambiguous Put = %q, want value", value)
		}

		store.FailNextAfterApply(sim.ObjectDelete, 1)
		if err := store.Delete(t.Context(), platform.DeleteRequest{Key: key}); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("ambiguous Delete error = %v, want ErrInjectedFault", err)
		}
		if _, err := store.Get(t.Context(), platform.GetRequest{Key: key}); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("Get after ambiguous Delete error = %v, want ErrNotFound", err)
		}
	})
}

func TestObjectStoreBodyReadHonorsContextWhileThrottled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{BytesPerSecond: 1}})
		store := runtime.ObjectStore()
		key := mustObjectKey(t, "objects/slow")
		body := []byte("slow body")
		if _, err := store.Put(t.Context(), platform.PutRequest{
			Key: key, Body: bytes.NewReader(body), Size: int64(len(body)),
		}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		result, err := store.Get(ctx, platform.GetRequest{Key: key})
		if err != nil {
			t.Fatal(err)
		}
		defer result.Body.Close()
		start := time.Now()
		if _, err := io.ReadAll(result.Body); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ReadAll error = %v, want DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > time.Second {
			t.Fatalf("throttled body read stopped after %v, want the remaining one-second deadline", elapsed)
		}
	})
}

// A suffix range reads the tail of an object without its reader knowing how
// long the object is, and one longer than the object reads the whole of it, as
// an HTTP suffix range does.
func TestObjectStoreReadsASuffixRange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := sim.New(sim.Config{}).ObjectStore()
		key := mustObjectKey(t, "part/0")
		object := []byte("0123456789")
		if _, err := store.Put(t.Context(), platform.PutRequest{
			Key: key, Body: bytes.NewReader(object), Size: int64(len(object)),
		}); err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name   string
			suffix int64
			want   string
		}{
			{name: "a tail", suffix: 4, want: "6789"},
			{name: "the whole object", suffix: int64(len(object)), want: "0123456789"},
			{name: "one longer than the object", suffix: 64, want: "0123456789"},
		} {
			result, err := store.Get(t.Context(), platform.GetRequest{
				Key: key, Range: &platform.ByteRange{Suffix: test.suffix}})
			if err != nil {
				t.Fatalf("%s suffix of %d bytes: %v", test.name, test.suffix, err)
			}
			body, err := io.ReadAll(result.Body)
			result.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != test.want {
				t.Fatalf("%s suffix of %d bytes read %q, want %q", test.name, test.suffix, body, test.want)
			}
			if result.ContentLength != int64(len(test.want)) || result.Metadata.Size != int64(len(object)) {
				t.Fatalf("%s suffix reported length/size %d/%d, want %d/%d",
					test.name, result.ContentLength, result.Metadata.Size, len(test.want), len(object))
			}
		}
	})
}

// A suffix is exclusive with an offset and a length: a range carrying both says
// two different things about what it wants.
func TestObjectStoreRejectsASuffixWithAnOffset(t *testing.T) {
	store := sim.New(sim.Config{}).ObjectStore()
	_, err := store.Get(t.Context(), platform.GetRequest{
		Key:   mustObjectKey(t, "part/0"),
		Range: &platform.ByteRange{Offset: 2, Length: 3, Suffix: 4}})
	if !errors.Is(err, platform.ErrInvalidRange) {
		t.Fatalf("Get error = %v, want ErrInvalidRange", err)
	}
}

func mustObjectKey(t *testing.T, value string) platform.ObjectKey {
	t.Helper()
	key, err := platform.NewObjectKey(value)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
