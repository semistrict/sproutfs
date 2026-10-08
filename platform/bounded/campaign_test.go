package bounded_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/bounded"
	"github.com/semistrict/sproutfs/platform/sim"
)

// campaignSeeds are the seeds the bounds' campaign runs, which between them
// fire every request chaos site and reach every probe.
var campaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

// campaignBounds are short, so a hold costs a seed a few simulated seconds.
var campaignBounds = bounded.Bounds{FirstByte: 2 * time.Second, Stall: time.Second}

const (
	campaignWorkers    = 4
	campaignOperations = 30
	campaignCounters   = 2
)

// TestTheBoundsSurviveTheirFaultsAndReachTheirProbes runs workers over a
// bounded store whose requests hang before their reply, hang after their
// write is applied, and stall halfway through their body, every completion
// released by the seed's scheduler. The workers create immutable objects and
// read them back whole, by range and by suffix, list and head them, add to
// counters by compare-and-set, and write and delete objects of their own
// without a condition.
//
// Every read must be the bytes written, whatever hung. A create refused by
// its own landed write must find its own bytes, and a compare-and-set
// refused the same way must find its own change, which is how every caller
// of a conditional write settles one. Each counter must have taken every
// worker's additions exactly once. An unconditional write that timed out may
// or may not have landed, and the next read must find one of the two. Every
// site must fire and every probe be reached across the seeds, and the
// store's counts must agree with the probes.
func TestTheBoundsSurviveTheirFaultsAndReachTheirProbes(t *testing.T) {
	fired, probes := map[string]uint64{}, map[string]uint64{}
	for _, seed := range campaignSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := runBoundsCampaign(t, seed)
				for site, count := range runtime.FiredSites() {
					fired[site] += count
				}
				for name, count := range runtime.Probes() {
					probes[name] += count
				}
			})
		})
	}
	t.Logf("fired=%v probes=%v", fired, probes)
	for _, site := range []string{sim.SiteObjectHang, sim.SiteObjectHangAfterApply, sim.SiteObjectStallBody} {
		if fired[site] == 0 {
			t.Errorf("no seed fired the %s site", site)
		}
	}
	for _, name := range bounded.Probes {
		if probes[name] == 0 {
			t.Errorf("no seed reached the %s probe", name)
		}
	}
}

// boundsCampaign is one seed's store and what its workers know of it.
type boundsCampaign struct {
	runtime *sim.Runtime
	draw    sim.Random
	store   *bounded.Store

	mu sync.Mutex
	// created is every immutable object a worker created, with its bytes.
	created map[platform.ObjectKey][]byte
	// added is every addition to a counter that returned, by counter.
	added map[platform.ObjectKey][]string
}

func runBoundsCampaign(t *testing.T, seed uint64) *sim.Runtime {
	scheduler := sim.NewScheduler(seed)
	runtime := sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: true,
		ObjectStore: sim.ObjectStoreConfig{HeadLatency: 3 * time.Millisecond, GetLatency: 8 * time.Millisecond,
			PutLatency: 15 * time.Millisecond, DeleteLatency: 6 * time.Millisecond, ListLatency: 9 * time.Millisecond,
			BytesPerSecond: 64 << 20, RequestChaos: true}})
	store, err := bounded.New(runtime.ObjectStore(), campaignBounds, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &boundsCampaign{runtime: runtime, draw: runtime.Random("bounds-campaign"), store: store,
		created: map[platform.ObjectKey][]byte{}, added: map[platform.ObjectKey][]string{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := sim.WithRuntime(t.Context(), runtime)
		for counter := range campaignCounters {
			k := key(t, fmt.Sprintf("counter/%d", counter))
			if _, err := retried(func() (platform.PutResult, error) {
				return store.Put(ctx, platform.PutRequest{Key: k, Body: strings.NewReader(""),
					Conditions: platform.PutConditions{IfNoneMatch: true}})
			}); err != nil && !errors.Is(err, platform.ErrPrecondition) {
				t.Error(err)
				return
			}
		}
		var workers sync.WaitGroup
		for worker := range campaignWorkers {
			workers.Go(func() {
				c.work(t, sim.WithTask(ctx, fmt.Sprintf("worker-%d", worker)), worker)
			})
		}
		workers.Wait()
		c.check(t, sim.WithTask(ctx, "check"))
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	return runtime
}

// scratch is what a worker knows of its own unconditional object: the
// contents it may hold, nil among them for none. A write or a delete that
// timed out leaves two.
type scratch struct {
	key      platform.ObjectKey
	possible [][]byte
}

func (c *boundsCampaign) work(t *testing.T, ctx context.Context, worker int) {
	own := &scratch{key: key(t, fmt.Sprintf("scratch/%d", worker)), possible: [][]byte{nil}}
	var mine []platform.ObjectKey
	for operation := range campaignOperations {
		id := fmt.Sprintf("%d/%d", worker, operation)
		switch kind := c.draw.Intn(id+"/kind", 8); {
		case kind < 2 || len(mine) == 0:
			k := key(t, fmt.Sprintf("object/%d/%d", worker, operation))
			if c.create(t, ctx, id, k) {
				mine = append(mine, k)
			}
		case kind < 4:
			c.read(t, ctx, id, mine[c.draw.Intn(id+"/object", len(mine))])
		case kind == 4:
			c.list(t, ctx, worker, mine)
		case kind == 5:
			c.add(t, ctx, id, key(t, fmt.Sprintf("counter/%d", c.draw.Intn(id+"/counter", campaignCounters))))
		case kind == 6:
			c.write(t, ctx, id, own)
		default:
			c.readScratch(t, ctx, own)
		}
	}
}

// retried makes a request again while the store answers it unavailable, as
// every caller of a store does. Only a request that is safe to make again is
// made through it: a read, or a write under a condition.
func retried[T any](call func() (T, error)) (T, error) {
	for {
		value, err := call()
		if !errors.Is(err, platform.ErrUnavailable) {
			return value, err
		}
	}
}

// create writes one immutable object create-if-absent, again while the store
// answers unavailable. A refusal is the object its own earlier attempt wrote,
// which must hold its bytes.
func (c *boundsCampaign) create(t *testing.T, ctx context.Context, id string, k platform.ObjectKey) bool {
	data := pattern(c.draw.Intn(id+"/size", 48<<10), byte(c.draw.Intn(id+"/seed", 256)))
	_, err := retried(func() (platform.PutResult, error) {
		return c.store.Put(ctx, platform.PutRequest{Key: k, Body: bytes.NewReader(data), Size: int64(len(data)),
			Conditions: platform.PutConditions{IfNoneMatch: true}})
	})
	switch {
	case errors.Is(err, platform.ErrPrecondition):
		if got, err := c.whole(ctx, k); err != nil || !bytes.Equal(got, data) {
			t.Errorf("a create of %s refused by its own write found %d other bytes (%v)", k, len(got), err)
			return false
		}
	case err != nil:
		t.Errorf("creating %s: %v", k, err)
		return false
	}
	c.mu.Lock()
	c.created[k] = data
	c.mu.Unlock()
	return true
}

// read reads one created object whole, by a range or by a suffix, and
// heads it.
func (c *boundsCampaign) read(t *testing.T, ctx context.Context, id string, k platform.ObjectKey) {
	c.mu.Lock()
	data := c.created[k]
	c.mu.Unlock()
	request, want := platform.GetRequest{Key: k}, data
	if size := int64(len(data)); size > 0 {
		switch c.draw.Intn(id+"/form", 3) {
		case 1:
			offset := c.draw.Uint64(id+"/offset") % uint64(size)
			length := 1 + c.draw.Uint64(id+"/length")%uint64(size-int64(offset))
			request.Range = &platform.ByteRange{Offset: int64(offset), Length: int64(length)}
			want = data[offset : offset+length]
		case 2:
			suffix := 1 + int64(c.draw.Uint64(id+"/suffix")%uint64(2*size))
			request.Range = &platform.ByteRange{Suffix: suffix}
			want = data[max(size-suffix, 0):]
		}
	}
	got, err := retried(func() ([]byte, error) {
		result, err := c.store.Get(ctx, request)
		if err != nil {
			return nil, err
		}
		got, err := io.ReadAll(result.Body)
		if closeErr := result.Body.Close(); closeErr != nil {
			t.Errorf("closing %s: %v", k, closeErr)
		}
		return got, err
	})
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("reading %s %+v read %d other bytes (%v)", k, request.Range, len(got), err)
	}
	metadata, err := retried(func() (platform.ObjectMetadata, error) { return c.store.Head(ctx, k) })
	if err != nil || metadata.Size != int64(len(data)) {
		t.Errorf("heading %s: %d bytes (%v), want %d", k, metadata.Size, err, len(data))
	}
}

// list lists a worker's own objects, which must be exactly what it created.
func (c *boundsCampaign) list(t *testing.T, ctx context.Context, worker int, mine []platform.ObjectKey) {
	prefix, err := platform.NewObjectPrefix(fmt.Sprintf("object/%d/", worker))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := retried(func() ([]platform.ObjectKey, error) {
		var listed []platform.ObjectKey
		return listed, platform.ListAll(ctx, c.store, prefix, func(object platform.ObjectMetadata) error {
			listed = append(listed, object.Key)
			return nil
		})
	})
	if err != nil {
		t.Errorf("listing %s: %v", prefix, err)
		return
	}
	want := slices.SortedFunc(slices.Values(mine), func(a, b platform.ObjectKey) int {
		return strings.Compare(a.String(), b.String())
	})
	if !slices.Equal(listed, want) {
		t.Errorf("listing %s found %v, want %v", prefix, listed, want)
	}
}

// add appends this addition's name to a counter by compare-and-set. A
// refusal whose counter already holds the name is its own landed write, and
// so is a write whose reply was lost. A read whose body stalled while another
// worker changed the counter is read again, as any read the store did not
// answer is, and so is a write the store answered unavailable.
func (c *boundsCampaign) add(t *testing.T, ctx context.Context, id string, k platform.ObjectKey) {
	for {
		result, err := c.store.Get(ctx, platform.GetRequest{Key: k})
		if errors.Is(err, platform.ErrUnavailable) {
			continue
		}
		if err != nil {
			t.Errorf("reading %s: %v", k, err)
			return
		}
		current, err := io.ReadAll(result.Body)
		if closeErr := result.Body.Close(); closeErr != nil {
			t.Errorf("closing %s: %v", k, closeErr)
			return
		}
		if errors.Is(err, platform.ErrUnavailable) {
			continue
		}
		if err != nil {
			t.Errorf("reading %s: %v", k, err)
			return
		}
		if slices.Contains(names(current), id) {
			break
		}
		next := []byte(strings.Join(append(names(current), id), ","))
		_, err = c.store.Put(ctx, platform.PutRequest{Key: k, Body: bytes.NewReader(next), Size: int64(len(next)),
			Conditions: platform.PutConditions{IfMatch: &result.Metadata.ETag}})
		if err == nil {
			break
		}
		if !errors.Is(err, platform.ErrPrecondition) && !errors.Is(err, platform.ErrUnavailable) {
			t.Errorf("adding %s to %s: %v", id, k, err)
			return
		}
	}
	c.mu.Lock()
	c.added[k] = append(c.added[k], id)
	c.mu.Unlock()
}

func names(counter []byte) []string {
	if len(counter) == 0 {
		return nil
	}
	return strings.Split(string(counter), ",")
}

// write writes or deletes the worker's own object without a condition. One
// that timed out, or that the store answered unavailable, may or may not have
// landed.
func (c *boundsCampaign) write(t *testing.T, ctx context.Context, id string, own *scratch) {
	var next []byte
	var err error
	if c.draw.Chance(id+"/delete", 0.3) {
		err = c.store.Delete(ctx, platform.DeleteRequest{Key: own.key})
	} else {
		next = pattern(1+c.draw.Intn(id+"/size", 16<<10), byte(c.draw.Intn(id+"/seed", 256)))
		_, err = c.store.Put(ctx, platform.PutRequest{Key: own.key, Body: bytes.NewReader(next),
			Size: int64(len(next))})
	}
	switch {
	case err == nil:
		own.possible = [][]byte{next}
	case errors.Is(err, platform.ErrUnavailable):
		own.possible = append(own.possible, next)
	default:
		t.Errorf("writing %s: %v", own.key, err)
	}
}

// readScratch reads the worker's own object, which must hold one of what it
// may, and is then known to hold that.
func (c *boundsCampaign) readScratch(t *testing.T, ctx context.Context, own *scratch) {
	got, err := c.whole(ctx, own.key)
	if errors.Is(err, platform.ErrNotFound) {
		got, err = nil, nil
	}
	if err != nil {
		t.Errorf("reading %s: %v", own.key, err)
		return
	}
	if !slices.ContainsFunc(own.possible, func(p []byte) bool { return (p == nil) == (got == nil) && bytes.Equal(p, got) }) {
		t.Errorf("%s holds %d bytes, which none of its %d possible writes left", own.key, len(got), len(own.possible))
		return
	}
	own.possible = [][]byte{got}
}

func (c *boundsCampaign) whole(ctx context.Context, k platform.ObjectKey) ([]byte, error) {
	return retried(func() ([]byte, error) {
		data, _, err := platform.ReadObject(ctx, c.store, k, 0, 1<<30, errors.New("corrupt"))
		return data, err
	})
}

// check reads everything back once the workers are done, and holds the
// store's counts to the probes.
func (c *boundsCampaign) check(t *testing.T, ctx context.Context) {
	for _, k := range slices.SortedFunc(maps.Keys(c.created), func(a, b platform.ObjectKey) int {
		return strings.Compare(a.String(), b.String())
	}) {
		if got, err := c.whole(ctx, k); err != nil || !bytes.Equal(got, c.created[k]) {
			t.Errorf("%s read back as %d other bytes (%v)", k, len(got), err)
		}
	}
	for counter := range campaignCounters {
		k := key(t, fmt.Sprintf("counter/%d", counter))
		got, err := c.whole(ctx, k)
		if err != nil {
			t.Errorf("reading %s: %v", k, err)
			continue
		}
		if held, want := names(got), c.added[k]; !slices.Equal(slices.Sorted(slices.Values(held)), slices.Sorted(slices.Values(want))) {
			t.Errorf("%s holds the additions %v, want each of %v once", k, held, want)
		}
	}
	recoveries := c.store.Recoveries()
	var firstByte, stall, retries int64
	for _, r := range []bounded.Recovery{recoveries.Head, recoveries.Get, recoveries.Put, recoveries.Delete, recoveries.List} {
		firstByte, stall, retries = firstByte+r.FirstByteTimeouts, stall+r.StallTimeouts, retries+r.Retries
	}
	probes := c.runtime.Probes()
	if uint64(firstByte) != probes[bounded.ProbeFirstByte] || uint64(stall) != probes[bounded.ProbeStall] ||
		uint64(retries) != probes[bounded.ProbeRetried] {
		t.Errorf("the store counted %d first-byte timeouts, %d stalls and %d retries, but the probes were reached %v",
			firstByte, stall, retries, probes)
	}
	t.Logf("recoveries=%+v", recoveries)
}
