package checkpoint_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// publishedPages is what the publication tests publish: three pages, which
// one part holds.
var publishedPages = []uint64{0, 1, 2}

// requireNothingPlaced fails unless no host holds a stripe of any window of
// ref's pages or of its segment.
func (c *fillCluster) requireNothingPlaced(t *testing.T, ref control.Ref, when string) {
	t.Helper()
	nothing := make([][]int, len(c.hosts))
	windows := []rank.Window{segmentWindow(ref)}
	for _, page := range publishedPages {
		windows = append(windows, pageWindow(ref, page))
	}
	for _, window := range windows {
		if got := c.placed(window); !slices.EqualFunc(got, nothing, slices.Equal) {
			t.Fatalf("%s, the stripes of %+v are on %v already", when, window, got)
		}
	}
}

// requirePlaced fails unless every window of ref's pages and its segment is on
// exactly the hosts the list ranks for it.
func (c *fillCluster) requirePlaced(t *testing.T, ref control.Ref) {
	t.Helper()
	windows := []rank.Window{segmentWindow(ref)}
	for _, page := range publishedPages {
		windows = append(windows, pageWindow(ref, page))
	}
	for _, window := range windows {
		if got, want := c.placed(window), c.ranked(window); !slices.EqualFunc(got, want, slices.Equal) {
			t.Fatalf("the stripes of %+v are on %v, want %v", window, got, want)
		}
	}
}

// A publication sends each part's stripes only once the part's PUT has
// succeeded, and the segments' once the index object's has. While the part's
// PUT is in flight no host holds any of it, and nothing has been filled; once
// it lands, every window is on exactly its ranks. A PUT that fails fills
// nothing, so no cache ever holds the bytes of a part the store refused, and
// the publication retried under the same reference fills them then.
func TestAPublicationFillsNothingBeforeItsPartIsDurable(t *testing.T) {
	for _, cluster := range clusterCodes {
		t.Run(cluster.code.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := newFillCluster(t, fillConfig{hosts: cluster.hosts, code: cluster.code, share: 100})
				publisher := c.hosts[0]
				ref := control.Ref{VM: "vm", Sequence: 2}
				c.puts.entered, c.puts.release = make(chan struct{}), make(chan struct{})
				c.puts.hold.Store(true)
				published := make(chan error, 1)
				go func() {
					_, _, err := publishFrom(t, publisher.store, "vm", publishedPages)
					published <- err
				}()
				<-c.puts.entered
				c.settle(t)
				c.requireNothingPlaced(t, ref, "while the part's PUT is in flight")
				if fills := c.fills(); fills.FromPublications != 0 || fills.Kept != 0 || fills.Sent != 0 {
					t.Fatalf("while the part's PUT is in flight the fills came to %+v, want nothing", fills)
				}
				c.puts.hold.Store(false)
				close(c.puts.release)
				if err := <-published; err != nil {
					t.Fatal(err)
				}
				c.settle(t)
				c.requirePlaced(t, ref)
				windows := uint64(len(publishedPages) + 1)
				if fills := c.fills(); fills.FromPublications != windows ||
					fills.Kept != windows*uint64(cluster.code.Width()) || dropped(fills) != 0 || fills.FromReads != 0 {
					t.Fatalf("the publication's fills came to %+v, want %d windows on their ranks", fills, windows)
				}
			})
		})
	}
}

// A part whose PUT fails is filled nowhere, and its publication fails. The
// publication tried again under the same reference lands, and fills then.
func TestAPartTheStoreRefusedReachesNoCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, share: 100})
		publisher := c.hosts[0]
		ref := control.Ref{VM: "vm", Sequence: 2}
		c.puts.fail.Store(true)
		root, m, p := beginPublication(t, publisher.store, "vm", publishedPages)
		if _, err := p.Commit(t.Context(), m); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("a publication whose part the store refused = %v, want the store's failure", err)
		}
		c.settle(t)
		c.requireNothingPlaced(t, ref, "after the store refused the part")
		if fills := c.fills(); fills.FromPublications != 0 || fills.Kept != 0 || fills.Sent != 0 {
			t.Fatalf("after the store refused the part the fills came to %+v, want nothing", fills)
		}
		c.puts.fail.Store(false)
		retry := publisher.store.Begin(root, ref)
		for _, page := range publishedPages {
			for sector := range uint32(sectorsPerPage) {
				m.dirty(retry, "root", page, sector, sectorData("vm", page, sector))
			}
		}
		if _, err := retry.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		c.settle(t)
		c.requirePlaced(t, ref)
	})
}

// beginPublication is a publication of pages of vm's one volume, root, from
// store, begun over the root checkpoint and with every page dirty in the
// model, before it is committed.
func beginPublication(t *testing.T, store *checkpoint.Store, vm string, pages []uint64) (*checkpoint.Index, *model,
	*checkpoint.Publication) {
	t.Helper()
	return beginPublicationOf(t, store, vm, pages, sectorData)
}

// beginPublicationOf is beginPublication of sectors data makes.
func beginPublicationOf(t *testing.T, store *checkpoint.Store, vm string, pages []uint64,
	data func(tag string, page uint64, sector uint32) []byte) (*checkpoint.Index, *model, *checkpoint.Publication) {
	t.Helper()
	sizes := map[string]uint64{"root": (slices.Max(pages) + 1) * checkpoint.PageSize2MiB}
	root, err := store.Root(t.Context(), control.Ref{VM: vm, Sequence: 1}, volumes2MiB(sizes))
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(volumes2MiB(sizes))
	p := store.Begin(root, control.Ref{VM: vm, Sequence: 2})
	for _, page := range pages {
		for sector := range uint32(sectorsPerPage) {
			m.dirty(p, "root", page, sector, data(vm, page, sector))
		}
	}
	return root, m, p
}

// publishFrom is publish that reports a failure rather than failing the test.
func publishFrom(t *testing.T, store *checkpoint.Store, vm string, pages []uint64) (*checkpoint.Index, *model, error) {
	t.Helper()
	return publishFromOf(t, store, vm, pages, sectorData)
}

// publishFromOf is publishFrom of sectors data makes.
func publishFromOf(t *testing.T, store *checkpoint.Store, vm string, pages []uint64,
	data func(tag string, page uint64, sector uint32) []byte) (*checkpoint.Index, *model, error) {
	t.Helper()
	_, m, p := beginPublicationOf(t, store, vm, pages, data)
	published, err := p.Commit(t.Context(), m)
	return published, m, err
}

// commitLatency is how long the first host of a cluster of config takes to
// publish three pages, and what the cluster's fills came to once they settled.
func commitLatency(t *testing.T, config fillConfig) (time.Duration, checkpoint.FillStats) {
	t.Helper()
	var took time.Duration
	var fills checkpoint.FillStats
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, config)
		_, m, p := beginPublication(t, c.hosts[0].store, "vm", publishedPages)
		start := time.Now()
		if _, err := p.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		took = time.Since(start)
		c.settle(t)
		fills = c.fills()
	})
	return took, fills
}

// Nothing waits on a fill, a publication's no more than a fault's. With the
// publisher's queue of writes to its own disk holding one window at a time and
// its disk taking a second over each write, the publication takes exactly as
// long as with the cluster cache off: the windows its queue has no room for
// are dropped, not waited for.
func TestAPublicationNeverWaitsForItsFill(t *testing.T) {
	config := fillConfig{hosts: 6, code: rank.Code{K: 4, M: 2}, runtime: latencyRuntime,
		disk: sim.DiskConfig{WriteLatency: time.Second},
		cache: func(host int, cache *checkpoint.CacheConfig) {
			if host == 0 {
				cache.FillQueueBytes = 1
			}
		}}
	off, offFills := commitLatency(t, config)
	if offFills.FromPublications != 0 || offFills.Kept != 0 {
		t.Fatalf("with the cluster cache off the fills came to %+v, want none", offFills)
	}
	config.share = 100
	on, onFills := commitLatency(t, config)
	if on != off {
		t.Fatalf("a publication took %v with the cluster cache off and %v with its fills behind a full queue; want the same",
			off, on)
	}
	if onFills.FromPublications == 0 || onFills.Dropped[checkpoint.DropQueue] == 0 {
		t.Fatalf("with a queue of one window the fills came to %+v, want some filled and some dropped for the queue",
			onFills)
	}
}

// heldPart is an object store that holds the PUT of the first part of a
// checkpoint until release is closed, and tells landed of each other part's
// PUT once it has succeeded.
type heldPart struct {
	platform.ObjectStore
	first   string
	release chan struct{}
	landed  chan struct{}
}

func (s *heldPart) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	key := request.Key.String()
	if strings.HasSuffix(key, s.first) {
		select {
		case <-s.release:
		case <-ctx.Done():
			return platform.PutResult{}, context.Cause(ctx)
		}
	}
	result, err := s.ObjectStore.Put(ctx, request)
	if err == nil && strings.Contains(key, "/part/") && !strings.HasSuffix(key, s.first) {
		s.landed <- struct{}{}
	}
	return result, err
}

// A publication hands its parts to the cluster in their own order, whatever
// order their PUTs end in: a part whose PUT has succeeded waits for the part
// before it to be handed over. Each page is a part of its own here, and the
// first part's PUT is held: while it is, the other two parts land and
// nothing is filled. Once it lands, all three pages and the segment are.
//
// A publication's uploads run beside each other and end at one instant when
// their latencies are equal. Handed over as they ended, a host's one worker of
// fills took them in the order the Go scheduler ran the uploads, and which of
// them a full queue or a spent rate dropped was not the seed's choice.
func TestAPublicationHandsItsPartsOverInTheirOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newFillCluster(t, fillConfig{hosts: 2, code: rank.Code{K: 1, M: 1}, share: 100})
		publisher := c.hosts[0]
		held := &heldPart{ObjectStore: publisher.objects, first: "/ckpt/2/part/0",
			release: make(chan struct{}), landed: make(chan struct{}, len(publishedPages))}
		store := mustStore(t, checkpoint.Config{ObjectStore: held, Cache: publisher.cache, PartBytes: 1})
		published := make(chan error, 1)
		go func() {
			_, _, err := publishFrom(t, store, "vm", publishedPages)
			published <- err
		}()
		for range len(publishedPages) - 1 {
			<-held.landed
		}
		synctest.Wait()
		early := c.fills()
		close(held.release)
		if err := <-published; err != nil {
			t.Fatal(err)
		}
		c.settle(t)
		if early.FromPublications != 0 {
			t.Fatalf("with the first part's PUT held, %d windows of the parts after it were filled, want none",
				early.FromPublications)
		}
		c.requirePlaced(t, control.Ref{VM: "vm", Sequence: 2})
		if fills := c.fills(); fills.FromPublications != uint64(len(publishedPages)+1) || dropped(fills) != 0 {
			t.Fatalf("the publication's fills came to %+v, want its three pages and its segment", fills)
		}
	})
}
