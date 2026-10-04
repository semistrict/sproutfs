package bounded_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/bounded"
	"github.com/semistrict/sproutfs/platform/sim"
)

// An unconditional write whose body stalls is not made again either, and the
// error says which bound it waited out, and for how long.
func TestAStalledUnconditionalWriteSaysWhichBoundItWaitedOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{FirstByte: 4 * time.Second, Stall: 3 * time.Second})
		data := pattern(4096, 12)
		f.sim.StallNextBody(sim.ObjectPut, 1)
		began := time.Now()
		_, err := f.store.Put(f.ctx, platform.PutRequest{Key: key(t, "scratch"), Body: bytes.NewReader(data),
			Size: int64(len(data))})
		if !errors.Is(err, bounded.ErrTimedOut) {
			t.Fatalf("a stalled unconditional PUT = %v, want ErrTimedOut", err)
		}
		if want := "put scratch waited 3s for stall"; !strings.Contains(err.Error(), want) {
			t.Fatalf("the error says %q, want it to say %q", err, want)
		}
		if took, want := time.Since(began), putLatency+3*time.Second; took != want {
			t.Fatalf("the PUT gave up after %s, want %s", took, want)
		}
		f.requireRecoveries(t, bounded.Recoveries{Put: bounded.Recovery{StallTimeouts: 1}})
	})
}

// cutShort is a store whose next GET's body, once its reader waits on it,
// hands over the bytes it has together with the error its cancellation
// brings, as an io.Reader may.
type cutShort struct {
	platform.ObjectStore
	armed bool
}

func (s *cutShort) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	result, err := s.ObjectStore.Get(ctx, request)
	if err != nil || !s.armed {
		return result, err
	}
	s.armed = false
	data, err := io.ReadAll(result.Body)
	if closeErr := result.Body.Close(); err != nil || closeErr != nil {
		return platform.GetResult{}, errors.Join(err, closeErr)
	}
	result.Body = io.NopCloser(&withError{ctx: ctx, data: data[:len(data)/2]})
	return result, nil
}

// withError waits for its context to end, then hands over all its data with
// the context's error.
type withError struct {
	ctx  context.Context
	data []byte
}

func (w *withError) Read(p []byte) (int, error) {
	<-w.ctx.Done()
	n := copy(p, w.data)
	w.data = w.data[n:]
	return n, context.Cause(w.ctx)
}

// Bytes a stalled reply hands over with its error are the caller's, and the
// rest is asked for after them.
func TestBytesHandedOverWithTheStallAreKept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1, ObjectStore: sim.ObjectStoreConfig{GetLatency: getLatency,
			PutLatency: putLatency, BytesPerSecond: 1 << 62}})
		ctx := sim.WithRuntime(t.Context(), runtime)
		data := pattern(8192, 13)
		k := key(t, "part")
		if _, err := runtime.ObjectStore().Put(ctx, platform.PutRequest{Key: k, Body: bytes.NewReader(data),
			Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		store, err := bounded.New(&cutShort{ObjectStore: runtime.ObjectStore(), armed: true}, bounded.Bounds{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.Get(ctx, platform.GetRequest{Key: k})
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := result.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("read %d bytes that are not the object's %d", len(got), len(data))
		}
		want := bounded.Recoveries{Get: bounded.Recovery{StallTimeouts: 1, Retries: 1}}
		if got := store.Recoveries(); got != want {
			t.Fatalf("recoveries = %+v, want %+v", got, want)
		}
	})
}
