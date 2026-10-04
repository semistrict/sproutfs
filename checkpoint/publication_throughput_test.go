package checkpoint_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// throughput is one publication of pages of noise, timed in simulated time:
// how the store is sized, what an encode costs, and what a PUT costs.
type throughput struct {
	pages    uint64
	encoders int
	uploads  int
	// encodeRate prices an encode in bytes a second; zero makes it free.
	encodeRate int64
	putLatency time.Duration
	shake      uint64
}

// putBytesPerSecond is the simulated store's bandwidth for each PUT, high
// enough that a part's PUT takes mostly its request's latency.
const putBytesPerSecond = 1 << 40

// throughputPartBytes is what a part fills to: four pages of noise, whose
// envelopes are a little larger than the pages.
const throughputPartBytes = 4 * checkpoint.PageSize2MiB

// tenMillisecondPages prices an encode so that a 2 MiB page takes exactly
// 10 ms.
const tenMillisecondPages = checkpoint.PageSize2MiB * 100

// throughputResult is what one publication took and did.
type throughputResult struct {
	took time.Duration
	// objects is each object's PUT, by key.
	objects map[string]put
	// encodes is what the encodes did, and puts the most PUTs at once.
	encodes     sim.WorkStats
	puts        int64
	fingerprint uint64
}

// put is one object's PUT: its size, and when it began and ended, from the
// start of the commit.
type put struct {
	size       int64
	began, end time.Duration
}

// putTime is what the simulated store takes to PUT an object of size bytes.
func (c throughput) putTime(size int64) time.Duration {
	return c.putLatency + time.Duration(size*int64(time.Second)/putBytesPerSecond)
}

func (c throughput) run(t *testing.T) throughputResult {
	t.Helper()
	var result throughputResult
	synctest.Test(t, func(t *testing.T) {
		config := sim.Config{Seed: 1, Shake: c.shake, ObjectStore: sim.ObjectStoreConfig{
			PutLatency: c.putLatency, BytesPerSecond: putBytesPerSecond}}
		if c.encodeRate != 0 {
			config.Compute = map[string]int64{blob.WorkEncode: c.encodeRate}
		}
		runtime := sim.New(config)
		ctx := sim.WithRuntime(t.Context(), runtime)
		codecs, err := blob.NewCodecs(c.encoders, 1)
		if err != nil {
			t.Fatal(err)
		}
		objects := &timedStore{concurrencyStore: concurrencyStore{ObjectStore: runtime.ObjectStore()}}
		store := mustStore(t, checkpoint.Config{ObjectStore: objects, Codecs: codecs, Concurrency: c.uploads,
			PartBytes: throughputPartBytes})
		sizes := map[string]uint64{"ram0": c.pages * checkpoint.PageSize2MiB}
		root, err := store.Root(ctx, control.Ref{VM: "wide", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		publication := store.Begin(root, control.Ref{VM: "wide", Sequence: 2})
		for page := range c.pages {
			publication.Dirty("ram0", page)
		}
		objects.peak.Store(0)
		started := time.Now()
		objects.reset(started)
		index, err := publication.Commit(ctx, noiseSource{})
		if err != nil {
			t.Fatal(err)
		}
		result.took = time.Since(started)
		result.objects, result.puts = objects.puts, objects.peak.Load()
		result.encodes, result.fingerprint = runtime.Work(blob.WorkEncode), runtime.Fingerprint()
		// Every page reads back as it was published, whatever order the
		// encodes ended in.
		got, want := make([]byte, checkpoint.PageSize2MiB), make([]byte, checkpoint.PageSize2MiB)
		for page := range c.pages {
			if err := store.Read(ctx, index, "ram0", page*checkpoint.PageSize2MiB, got); err != nil {
				t.Fatal(err)
			}
			if err := (noiseSource{}).ReadPage(ctx, "ram0", page, want); err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Fatalf("page %d reads back as other bytes than it was published with", page)
			}
		}
	})
	return result
}

// timedStore is a concurrencyStore that also keeps each PUT's size and when
// it began and ended, from an instant the test sets.
type timedStore struct {
	concurrencyStore
	mu   sync.Mutex
	from time.Time
	puts map[string]put
}

func (s *timedStore) reset(from time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.from, s.puts = from, map[string]put{}
}

func (s *timedStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	began := time.Now()
	result, err := s.concurrencyStore.Put(ctx, request)
	s.mu.Lock()
	if s.puts != nil {
		s.puts[request.Key.String()] = put{size: request.Size, began: began.Sub(s.from), end: time.Since(s.from)}
	}
	s.mu.Unlock()
	return result, err
}

// object is the PUT of the object whose key ends in suffix.
func (r throughputResult) object(t *testing.T, suffix string) put {
	t.Helper()
	for key, object := range r.objects {
		if strings.HasSuffix(key, suffix) {
			return object
		}
	}
	t.Fatalf("no object ends in %s: %v", suffix, r.objects)
	return put{}
}

// partsLanded is when the last of a publication's parts landed. It checks
// that the publication PUT those parts and its index object, and that the
// index object's PUT began only once every part had landed.
func (r throughputResult) partsLanded(t *testing.T, parts int) time.Duration {
	t.Helper()
	if len(r.objects) != parts+1 {
		t.Fatalf("PUT %d objects, want %d parts and the index object", len(r.objects), parts)
	}
	var landed time.Duration
	for number := range parts {
		landed = max(landed, r.object(t, fmt.Sprintf("/part/%d", number)).end)
	}
	if index := r.object(t, "/2/index"); index.began < landed {
		t.Fatalf("the index object's PUT began at %v, before the last part landed at %v", index.began, landed)
	}
	return landed
}

// A publication encodes as many pages at once as its store has encoders, so
// where encoding is what bounds it, its parts land after the pages' encoding
// time over the encoders and then the PUT of its last part: 64 pages at 10 ms
// each on four encoders take 160 ms of encoding, not 640. Its index object is
// PUT only once every part has landed.
func TestAPublicationEncodesAsManyPagesAtOnceAsItHasEncoders(t *testing.T) {
	c := throughput{pages: 64, encoders: 4, uploads: 8, putLatency: 20 * time.Millisecond,
		encodeRate: tenMillisecondPages}
	got := c.run(t)
	// The pages, and the three small envelopes of the two roots and the
	// segment.
	if want := (sim.WorkStats{Pieces: 64 + 3, Peak: 4}); got.encodes != want {
		t.Fatalf("the encodes did %+v, want %+v", got.encodes, want)
	}
	parts := int(c.pages / 4)
	encoding := time.Duration(c.pages) * 10 * time.Millisecond / time.Duration(c.encoders)
	// The last two parts are sealed as the last encode ends.
	last := max(c.putTime(got.object(t, fmt.Sprintf("/part/%d", parts-2)).size),
		c.putTime(got.object(t, fmt.Sprintf("/part/%d", parts-1)).size))
	if landed, want := got.partsLanded(t, parts), encoding+last; landed != want {
		t.Fatalf("the parts of %d pages landed at %v, want %v: %v encoding on %d encoders and %v for the last part",
			c.pages, landed, want, encoding, c.encoders, last)
	}
}

// Where the store is what bounds a publication, it keeps every upload slot
// busy: 16 parts through four slots land after four rounds of a part's PUT,
// never one part's PUT at a time.
func TestAPublicationKeepsEveryUploadSlotBusy(t *testing.T) {
	c := throughput{pages: 64, encoders: 4, uploads: 4, putLatency: 100 * time.Millisecond}
	got := c.run(t)
	if got.puts != int64(c.uploads) {
		t.Fatalf("%d PUTs at once at most, want %d", got.puts, c.uploads)
	}
	part := c.putTime(got.object(t, "/part/0").size)
	for number := range 16 {
		if size := got.object(t, fmt.Sprintf("/part/%d", number)).size; c.putTime(size) != part {
			t.Fatalf("part %d is %d bytes, which takes another time to PUT than part 0's", number, size)
		}
	}
	if landed, want := got.partsLanded(t, 16), 4*part; landed != want {
		t.Fatalf("16 parts through %d slots landed at %v, want %v: four rounds of %v", c.uploads, landed, want, part)
	}
}

// A shake changes nothing a publication does: the same objects at the same
// simulated instants, in the same time, whatever order its encodes and
// uploads reach the processor in.
func TestAPublicationDoesTheSameWorkUnderAShake(t *testing.T) {
	c := throughput{pages: 32, encoders: 4, uploads: 3, putLatency: 20 * time.Millisecond,
		encodeRate: tenMillisecondPages}
	want := c.run(t)
	for _, shake := range []uint64{1, 2, 0x9e3779b97f4a7c15} {
		c.shake = shake
		got := c.run(t)
		if got.fingerprint != want.fingerprint || got.took != want.took {
			t.Fatalf("under shake %#x the publication took %v and digested as %#x, want %v and %#x",
				shake, got.took, got.fingerprint, want.took, want.fingerprint)
		}
	}
}
