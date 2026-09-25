package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/testresource"
)

func TestCacheNewReaderDoesNotJoinAnAbandonedLoad(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, err := NewCache(testresource.New(), CacheConfig{MaxConcurrentLoads: 2})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cache.Close)
		entered := make(chan struct{})
		canceled := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		first := make(chan error, 1)
		go func() {
			_, _, err := cache.get(ctx, cacheKeyOf("page"), func(ctx context.Context) ([]byte, error) {
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release // Transport cleanup can outlive the canceled reader.
				return nil, context.Cause(ctx)
			})
			first <- err
		}()
		<-entered
		cancel()
		if err := <-first; !errors.Is(err, context.Canceled) {
			t.Fatalf("first reader: %v", err)
		}
		<-canceled
		type result struct {
			data []byte
			err  error
		}
		fresh := make(chan result, 1)
		go func() {
			data, unpin, err := cache.get(t.Context(), cacheKeyOf("page"), func(context.Context) ([]byte, error) {
				return []byte("fresh"), nil
			})
			if unpin != nil {
				defer unpin()
			}
			fresh <- result{data, err}
		}()
		synctest.Wait()
		select {
		case got := <-fresh:
			if got.err != nil || !bytes.Equal(got.data, []byte("fresh")) {
				t.Fatalf("fresh reader: %q, %v", got.data, got.err)
			}
		default:
			t.Fatal("fresh reader joined a canceled load instead of starting its own")
		}
		if stats := cache.Stats(); stats.ActiveLoads != 1 || stats.PeakLoads != 2 {
			t.Fatalf("abandoned transport must retain its slot until cleanup: %+v", stats)
		}
	})
}
func TestCacheOwnsOnlyTheLoadedBytes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cache, err := NewCache(testresource.New(), CacheConfig{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(cache.Close)
		buffer := make([]byte, 3, 1<<20)
		copy(buffer, "abc")
		data, unpin, err := cache.get(t.Context(), cacheKeyOf("page"), func(context.Context) ([]byte, error) {
			return buffer, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		defer unpin()
		if cap(data) != len(data) {
			t.Fatalf("retained %d bytes of allocation for %d loaded bytes", cap(data), len(data))
		}
		copy(buffer, "xyz")
		if !bytes.Equal(data, []byte("abc")) {
			t.Fatalf("cache retained the loader's mutable buffer: %q", data)
		}
	})
}

// Close is what a host calls on shutdown, from wherever it unwinds: a second
// call must be a no-op rather than a panic on an already closed channel or a
// second unregistration of the evictor.
func TestCacheCloseIsIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		budget := testresource.New()
		cache, err := NewCache(budget, CacheConfig{})
		if err != nil {
			t.Fatal(err)
		}
		data, release, err := cache.get(t.Context(), cacheKeyOf("page"), func(context.Context) ([]byte, error) {
			return []byte("bytes"), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		release()
		if !bytes.Equal(data, []byte("bytes")) {
			t.Fatalf("read %q", data)
		}
		cache.Close()
		cache.Close()
		if stats := cache.Stats(); stats.Entries != 0 {
			t.Fatalf("a closed cache retains %d entries", stats.Entries)
		}
		if used := budget.Stats().Used; used != 0 {
			t.Fatalf("a closed cache retains %d bytes", used)
		}
	})
}
