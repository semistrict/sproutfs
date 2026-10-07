package bounded_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/bounded"
	"github.com/semistrict/sproutfs/platform/sim"
)

// The simulated store's latencies in these tests. Its bytes take no time, so
// what a request takes is its latency, the bound it waited out, and nothing
// else.
const (
	headLatency   = 5 * time.Millisecond
	getLatency    = 10 * time.Millisecond
	putLatency    = 20 * time.Millisecond
	deleteLatency = 7 * time.Millisecond
	listLatency   = 12 * time.Millisecond
)

// fixture is a simulated store, a log of the requests it was sent, and the
// same store under the default bounds.
type fixture struct {
	runtime *sim.Runtime
	sim     *sim.ObjectStore
	sent    *sent
	store   *bounded.Store
	ctx     context.Context
}

func newFixture(t *testing.T, bounds bounded.Bounds) *fixture {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 1, ObjectStore: sim.ObjectStoreConfig{HeadLatency: headLatency,
		GetLatency: getLatency, PutLatency: putLatency, DeleteLatency: deleteLatency, ListLatency: listLatency,
		BytesPerSecond: 1 << 62}})
	f := &fixture{runtime: runtime, sim: runtime.ObjectStore(), ctx: sim.WithRuntime(t.Context(), runtime)}
	f.sent = &sent{ObjectStore: f.sim}
	store, err := bounded.New(f.sent, bounds, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	return f
}

// sent records the ranges of the GETs it passes on.
type sent struct {
	platform.ObjectStore
	mu     sync.Mutex
	ranges []*platform.ByteRange
}

func (s *sent) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	s.mu.Lock()
	s.ranges = append(s.ranges, request.Range)
	s.mu.Unlock()
	return s.ObjectStore.Get(ctx, request)
}

func (s *sent) gets() []*platform.ByteRange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*platform.ByteRange(nil), s.ranges...)
}

func key(t *testing.T, name string) platform.ObjectKey {
	t.Helper()
	k, err := platform.NewObjectKey(name)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// pattern is size bytes that differ from one offset to the next, so a read
// of the wrong bytes reads as other bytes.
func pattern(size int, seed byte) []byte {
	data := make([]byte, size)
	for at := range data {
		data[at] = byte(at*7+at>>8) ^ seed
	}
	return data
}

// put writes an object straight into the simulated store, with no bound.
func (f *fixture) put(t *testing.T, k platform.ObjectKey, data []byte) {
	t.Helper()
	if _, err := f.sim.Put(t.Context(), platform.PutRequest{Key: k, Body: bytes.NewReader(data),
		Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
}

// stored is what the simulated store holds under k.
func (f *fixture) stored(t *testing.T, k platform.ObjectKey) []byte {
	t.Helper()
	data, _, err := platform.ReadObject(t.Context(), f.sim, k, 0, 1<<30, errors.New("corrupt"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (f *fixture) requireRecoveries(t *testing.T, want bounded.Recoveries) {
	t.Helper()
	if got := f.store.Recoveries(); got != want {
		t.Fatalf("recoveries = %+v, want %+v", got, want)
	}
}

func (f *fixture) requireProbes(t *testing.T, want map[string]uint64) {
	t.Helper()
	got := f.runtime.Probes()
	for _, name := range bounded.Probes {
		if got[name] != want[name] {
			t.Fatalf("probe %s reached %d times, want %d (all: %v)", name, got[name], want[name], got)
		}
	}
}

// A GET whose headers never come is given up at its first-byte bound and
// asked again, and the second answer is the one its caller reads: the call
// takes exactly the bound and one more round trip, where without the bound
// it takes the store's hour.
func TestAHungGetIsAbandonedAtItsFirstByteBoundAndTheRetrySucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		k := key(t, "part")
		data := pattern(8192, 1)
		f.put(t, k, data)
		f.sim.HangNext(sim.ObjectGet, 1)

		began := time.Now()
		result, err := f.store.Get(f.ctx, platform.GetRequest{Key: k,
			Range: &platform.ByteRange{Offset: 100, Length: 4000}})
		if err != nil {
			t.Fatal(err)
		}
		if took, want := time.Since(began), bounded.DefaultFirstByte+getLatency; took != want {
			t.Fatalf("the GET answered after %s, want its bound and one more round trip, %s", took, want)
		}
		got, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := result.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data[100:4100]) {
			t.Fatal("the retried GET read other bytes than were asked for")
		}
		f.requireRecoveries(t, bounded.Recoveries{Get: bounded.Recovery{FirstByteTimeouts: 1, Retries: 1}})
		f.requireProbes(t, map[string]uint64{bounded.ProbeFirstByte: 1, bounded.ProbeRetried: 1})
	})
}

// A GET whose body stops halfway is given up at its stall bound, measured
// from the read that waited, and the rest of its bytes are asked for from
// where it stopped: of a whole object, of a range and of a suffix alike.
func TestAStalledBodyIsAbandonedAtItsStallBoundAndReadOnFromWhereItStopped(t *testing.T) {
	data := pattern(8192, 2)
	for _, c := range []struct {
		name   string
		asked  *platform.ByteRange
		want   []byte
		resume platform.ByteRange
	}{
		{"whole", nil, data, platform.ByteRange{Offset: 4096, Length: 4096}},
		{"range", &platform.ByteRange{Offset: 1000, Length: 3000}, data[1000:4000],
			platform.ByteRange{Offset: 2500, Length: 1500}},
		{"suffix", &platform.ByteRange{Suffix: 6000}, data[2192:],
			platform.ByteRange{Offset: 5192, Length: 3000}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFixture(t, bounded.Bounds{})
				k := key(t, "part")
				f.put(t, k, data)
				f.sim.StallNextBody(sim.ObjectGet, 1)

				began := time.Now()
				result, err := f.store.Get(f.ctx, platform.GetRequest{Key: k, Range: c.asked})
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
				if took, want := time.Since(began), getLatency+bounded.DefaultStall+getLatency; took != want {
					t.Fatalf("the read took %s, want a round trip, the stall bound and a round trip, %s", took, want)
				}
				if !bytes.Equal(got, c.want) {
					t.Fatal("the resumed body read other bytes than were asked for")
				}
				if ranges := f.sent.gets(); len(ranges) != 2 || ranges[1] == nil || *ranges[1] != c.resume {
					t.Fatalf("the GETs sent asked for %v, want the second to ask for %+v", ranges, c.resume)
				}
				f.requireRecoveries(t, bounded.Recoveries{Get: bounded.Recovery{StallTimeouts: 1, Retries: 1}})
				f.requireProbes(t, map[string]uint64{bounded.ProbeStall: 1, bounded.ProbeRetried: 1,
					bounded.ProbeResumed: 1})
			})
		})
	}
}

// A body that stalls while its object is replaced is not finished from the
// new object: the read fails with ErrChanged, and its caller never holds
// bytes of two objects.
func TestAStalledBodyWhoseObjectChangedIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		k := key(t, "control/vm")
		first, second := pattern(8192, 3), pattern(8192, 4)
		f.put(t, k, first)
		f.sim.StallNextBody(sim.ObjectGet, 1)
		replaced := make(chan struct{})
		go func() {
			defer close(replaced)
			time.Sleep(getLatency + bounded.DefaultStall/2)
			f.put(t, k, second)
		}()

		began := time.Now()
		result, err := f.store.Get(f.ctx, platform.GetRequest{Key: k})
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(result.Body)
		<-replaced
		if !errors.Is(err, bounded.ErrChanged) || !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("reading a body whose object changed under its stall = %v, want ErrChanged", err)
		}
		if err := result.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, first[:4096]) {
			t.Fatalf("the refused body handed over %d bytes, want the first object's first half", len(got))
		}
		if took, want := time.Since(began), getLatency+bounded.DefaultStall+getLatency; took != want {
			t.Fatalf("the read took %s, want %s", took, want)
		}
		f.requireRecoveries(t, bounded.Recoveries{Get: bounded.Recovery{StallTimeouts: 1, Retries: 1}})
		f.requireProbes(t, map[string]uint64{bounded.ProbeStall: 1, bounded.ProbeRetried: 1,
			bounded.ProbeChanged: 1})
	})
}

// A create-if-absent whose write landed and whose reply never came is made
// again at its bound, measured from the last byte of the body the store
// took, and the object its first attempt wrote refuses the second. The
// caller is handed that refusal, which every caller of a conditional write
// settles against what the store holds.
func TestAHungCreateWhoseWriteLandedIsMadeAgainAndRefusedByItsOwnObject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		k := key(t, "part")
		data := pattern(4096, 5)
		f.sim.HangNextAfterApply(sim.ObjectPut, 1)

		began := time.Now()
		_, err := f.store.Put(f.ctx, platform.PutRequest{Key: k, Body: bytes.NewReader(data), Size: int64(len(data)),
			Conditions: platform.PutConditions{IfNoneMatch: true}})
		if !errors.Is(err, platform.ErrPrecondition) {
			t.Fatalf("a create made again over its own landed write = %v, want ErrPrecondition", err)
		}
		if took, want := time.Since(began), putLatency+bounded.DefaultFirstByte+putLatency; took != want {
			t.Fatalf("the create answered after %s, want %s", took, want)
		}
		if !bytes.Equal(f.stored(t, k), data) {
			t.Fatal("the store does not hold what the create wrote")
		}
		f.requireRecoveries(t, bounded.Recoveries{Put: bounded.Recovery{FirstByteTimeouts: 1, Retries: 1}})
		f.requireProbes(t, map[string]uint64{bounded.ProbeFirstByte: 1, bounded.ProbeRetried: 1,
			bounded.ProbeWriteRetried: 1})
	})
}

// A control record created across a reply that hung, and then changed across
// another, is settled the way a lost reply always was: the record read back
// carries the writer's nonce, so the create and the selection both succeed.
func TestAControlRecordWrittenAcrossHungRepliesIsReconciled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		client, err := control.NewClient(control.Config{ObjectStore: f.store,
			Entropy: f.runtime.NewEntropy("control")})
		if err != nil {
			t.Fatal(err)
		}
		first := control.Sequence(7, 1)
		f.sim.HangNextAfterApply(sim.ObjectPut, 1)
		handle, err := client.Create(f.ctx, "vm", first, true)
		if err != nil {
			t.Fatalf("creating a record across a hung reply: %v", err)
		}
		f.sim.HangNextAfterApply(sim.ObjectPut, 1)
		record, err := handle.Select(f.ctx, control.Sequence(7, 2), nil)
		if err != nil {
			t.Fatalf("selecting across a hung reply: %v", err)
		}
		if record.Selected != control.Sequence(7, 2) {
			t.Fatalf("the record selects %d, want %d", record.Selected, control.Sequence(7, 2))
		}
		read, err := client.Read(f.ctx, "vm")
		if err != nil {
			t.Fatal(err)
		}
		if read.Selected != control.Sequence(7, 2) || read.Epoch != 7 {
			t.Fatalf("the store's record is %+v, want epoch 7 selecting %d", read, control.Sequence(7, 2))
		}
		if got := f.runtime.Probes()[control.ProbeReplyReconciled]; got != 2 {
			t.Fatalf("reconciled %d lost replies, want both writes", got)
		}
		f.requireRecoveries(t, bounded.Recoveries{Put: bounded.Recovery{FirstByteTimeouts: 2, Retries: 2}})
	})
}

// pages is a guest whose pages each read as bytes drawn from the page and the
// offset, so two pages differ.
type pages struct{}

func (pages) ReadPage(_ context.Context, _ string, page uint64, dst []byte) error {
	for at := range dst {
		dst[at] = byte(page*31 + uint64(at)*7)
	}
	return nil
}

// A publication whose part landed and whose reply hung is made again; the
// part it finds is settled by its digest, as a retried publication's own
// part always was, and the checkpoint reads back as it was written.
func TestAPublicationWhosePartsReplyHungIsSettledByItsDigest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: f.store})
		if err != nil {
			t.Fatal(err)
		}
		const size = 3 * checkpoint.PageSize2MiB
		root, err := store.Root(f.ctx, control.Ref{VM: "vm", Sequence: 1},
			map[string]checkpoint.VolumeSpec{"root": {Size: size, PageSize: checkpoint.PageSize2MiB}})
		if err != nil {
			t.Fatal(err)
		}
		publication := store.Begin(root, control.Ref{VM: "vm", Sequence: 2})
		for page := range uint64(3) {
			publication.Dirty("root", page)
		}
		f.sim.HangNextAfterApply(sim.ObjectPut, 1)
		if _, err := publication.Commit(f.ctx, pages{}); err != nil {
			t.Fatalf("committing across a hung reply: %v", err)
		}
		index, err := store.Open(f.ctx, control.Ref{VM: "vm", Sequence: 2})
		if err != nil {
			t.Fatal(err)
		}
		got, want := make([]byte, size), make([]byte, size)
		for page := range uint64(3) {
			_ = pages{}.ReadPage(f.ctx, "root", page, want[page*checkpoint.PageSize2MiB:][:checkpoint.PageSize2MiB])
		}
		if err := store.Read(f.ctx, index, "root", 0, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("the checkpoint read back as other bytes than were published")
		}
		f.requireRecoveries(t, bounded.Recoveries{Put: bounded.Recovery{FirstByteTimeouts: 1, Retries: 1}})
	})
}

// An unconditional write that hangs is not made again: a second attempt
// could undo a change another writer made after the first. Its caller is
// told the outcome is unknown, at exactly the bound.
func TestAHungUnconditionalWriteIsNotMadeAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		k := key(t, "scratch")
		data := pattern(4096, 6)
		f.sim.HangNext(sim.ObjectPut, 1)
		began := time.Now()
		_, err := f.store.Put(f.ctx, platform.PutRequest{Key: k, Body: bytes.NewReader(data), Size: int64(len(data))})
		if !errors.Is(err, bounded.ErrTimedOut) || !errors.Is(err, platform.ErrUnavailable) {
			t.Fatalf("a hung unconditional PUT = %v, want ErrTimedOut", err)
		}
		if took := time.Since(began); took != bounded.DefaultFirstByte {
			t.Fatalf("the PUT gave up after %s, want its bound, %s", took, bounded.DefaultFirstByte)
		}
		if _, err := f.sim.Head(t.Context(), k); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("the store holds the PUT it never applied: %v", err)
		}

		f.put(t, k, data)
		f.sim.HangNextAfterApply(sim.ObjectDelete, 1)
		began = time.Now()
		if err := f.store.Delete(f.ctx, platform.DeleteRequest{Key: k}); !errors.Is(err, bounded.ErrTimedOut) {
			t.Fatalf("a hung DELETE = %v, want ErrTimedOut", err)
		}
		if took := time.Since(began); took != bounded.DefaultFirstByte {
			t.Fatalf("the DELETE gave up after %s, want its bound, %s", took, bounded.DefaultFirstByte)
		}
		if _, err := f.sim.Head(t.Context(), k); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("the store still holds what the DELETE removed: %v", err)
		}
		f.requireRecoveries(t, bounded.Recoveries{Put: bounded.Recovery{FirstByteTimeouts: 1},
			Delete: bounded.Recovery{FirstByteTimeouts: 1}})
		f.requireProbes(t, map[string]uint64{bounded.ProbeFirstByte: 2, bounded.ProbeNotRepeated: 2})
	})
}

// An upload the store stops taking halfway is given up at its stall bound,
// measured from the last byte it took, and a conditional one is made again.
func TestAStalledUploadIsAbandonedAtItsStallBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		k := key(t, "part")
		data := pattern(8192, 7)
		f.sim.StallNextBody(sim.ObjectPut, 1)
		began := time.Now()
		if _, err := f.store.Put(f.ctx, platform.PutRequest{Key: k, Body: bytes.NewReader(data),
			Size: int64(len(data)), Conditions: platform.PutConditions{IfNoneMatch: true}}); err != nil {
			t.Fatal(err)
		}
		if took, want := time.Since(began), putLatency+bounded.DefaultStall+putLatency; took != want {
			t.Fatalf("the PUT took %s, want %s", took, want)
		}
		if !bytes.Equal(f.stored(t, k), data) {
			t.Fatal("the store does not hold what the PUT wrote")
		}
		f.requireRecoveries(t, bounded.Recoveries{Put: bounded.Recovery{StallTimeouts: 1, Retries: 1}})
	})
}

// A HEAD and a LIST are reads, and are made again at their bound.
func TestAHungHeadOrListIsMadeAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		k := key(t, "vm/a/part")
		data := pattern(100, 8)
		f.put(t, k, data)

		f.sim.HangNext(sim.ObjectHead, 1)
		began := time.Now()
		metadata, err := f.store.Head(f.ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		if took, want := time.Since(began), bounded.DefaultFirstByte+headLatency; took != want {
			t.Fatalf("the HEAD took %s, want %s", took, want)
		}
		if metadata.Size != int64(len(data)) {
			t.Fatalf("the HEAD reported %d bytes, want %d", metadata.Size, len(data))
		}

		f.sim.HangNext(sim.ObjectList, 1)
		prefix, err := platform.NewObjectPrefix("vm/")
		if err != nil {
			t.Fatal(err)
		}
		began = time.Now()
		listed, err := f.store.List(f.ctx, platform.ListRequest{Prefix: prefix})
		if err != nil {
			t.Fatal(err)
		}
		if took, want := time.Since(began), bounded.DefaultFirstByte+listLatency; took != want {
			t.Fatalf("the LIST took %s, want %s", took, want)
		}
		if len(listed.Objects) != 1 || listed.Objects[0].Key != k {
			t.Fatalf("the LIST found %v, want %s", listed.Objects, k)
		}
		f.requireRecoveries(t, bounded.Recoveries{Head: bounded.Recovery{FirstByteTimeouts: 1, Retries: 1},
			List: bounded.Recovery{FirstByteTimeouts: 1, Retries: 1}})
	})
}

// A caller that gives up while its request hangs gets its own cancellation
// at the moment it gave up, and nothing is counted or made again.
func TestACallerThatGivesUpIsNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		k := key(t, "part")
		f.put(t, k, pattern(100, 9))
		f.sim.HangNext(sim.ObjectGet, 1)
		ctx, cancel := context.WithCancel(f.ctx)
		time.AfterFunc(3*time.Second, cancel)
		began := time.Now()
		if _, err := f.store.Get(ctx, platform.GetRequest{Key: k}); !errors.Is(err, context.Canceled) {
			t.Fatalf("a GET its caller gave up on = %v, want context.Canceled", err)
		}
		if took := time.Since(began); took != 3*time.Second {
			t.Fatalf("the GET returned after %s, want when its caller gave up, 3s", took)
		}
		f.requireRecoveries(t, bounded.Recoveries{})
	})
}

// A caller holding a body without reading it keeps the store waiting, not
// the other way round: nothing is cancelled however long it holds it.
func TestABodyHeldUnreadIsNotAStall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{})
		k := key(t, "part")
		data := pattern(8192, 10)
		f.put(t, k, data)
		result, err := f.store.Get(f.ctx, platform.GetRequest{Key: k})
		if err != nil {
			t.Fatal(err)
		}
		half := make([]byte, 4096)
		if _, err := io.ReadFull(result.Body, half); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * bounded.DefaultStall)
		rest, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := result.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(append(half, rest...), data) {
			t.Fatal("a body held unread read back as other bytes")
		}
		f.requireRecoveries(t, bounded.Recoveries{})
	})
}

// Bounds of a deployment's own are kept, a zero bound is the default, and a
// negative one is refused.
func TestBoundsAreTheirDefaultsAtZeroAndRefusedBelow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t, bounded.Bounds{FirstByte: 2 * time.Second, Stall: 3 * time.Second})
		k := key(t, "part")
		data := pattern(8192, 11)
		f.put(t, k, data)
		f.sim.HangNext(sim.ObjectGet, 1)
		f.sim.StallNextBody(sim.ObjectGet, 1)
		began := time.Now()
		result, err := f.store.Get(f.ctx, platform.GetRequest{Key: k})
		if err != nil {
			t.Fatal(err)
		}
		if took, want := time.Since(began), 2*time.Second+getLatency; took != want {
			t.Fatalf("the GET answered after %s, want %s", took, want)
		}
		got, err := io.ReadAll(result.Body)
		if err != nil {
			t.Fatal(err)
		}
		if took, want := time.Since(began), 2*time.Second+getLatency+3*time.Second+getLatency; took != want {
			t.Fatalf("the read took %s, want %s", took, want)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("the read returned other bytes")
		}
	})
	if got := (bounded.Bounds{}).WithDefaults(); got != (bounded.Bounds{FirstByte: bounded.DefaultFirstByte,
		Stall: bounded.DefaultStall}) {
		t.Fatalf("zero bounds = %+v, want the defaults", got)
	}
	for _, b := range []bounded.Bounds{{FirstByte: -1}, {Stall: -time.Second}} {
		if _, err := bounded.New(&sent{}, b, nil); !errors.Is(err, bounded.ErrInvalidBounds) {
			t.Fatalf("bounds %+v = %v, want ErrInvalidBounds", b, err)
		}
	}
	if _, err := bounded.New(nil, bounded.Bounds{}, nil); !errors.Is(err, bounded.ErrNoStore) {
		t.Fatalf("bounding no store = %v, want ErrNoStore", err)
	}
}
