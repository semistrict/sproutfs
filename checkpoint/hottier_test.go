package checkpoint_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// countedStore is a bucket that counts the requests it is sent, and holds or
// fails its PUTs while the test says so. A held PUT tells entered as it
// begins and waits for release.
type countedStore struct {
	platform.ObjectStore
	gets, puts, heads atomic.Int64
	hold, fail        atomic.Bool
	entered, release  chan struct{}
}

func (s *countedStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	s.gets.Add(1)
	return s.ObjectStore.Get(ctx, request)
}

func (s *countedStore) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	s.heads.Add(1)
	return s.ObjectStore.Head(ctx, key)
}

func (s *countedStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	s.puts.Add(1)
	if s.hold.Load() {
		s.entered <- struct{}{}
		<-s.release
	}
	if s.fail.Load() {
		return platform.PutResult{}, platform.ErrInjectedFault
	}
	return s.ObjectStore.Put(ctx, request)
}

// hotFixture is a store that reads through a hot tier: the runtime's own
// bucket is the regional one, and a second bucket of the runtime is the hot
// tier. The store keeps no page cache, so every read reaches a bucket.
type hotFixture struct {
	runtime  *sim.Runtime
	simHot   *sim.ObjectStore
	regional *countedStore
	hot      *countedStore
	tier     *checkpoint.HotTier
	store    *checkpoint.Store
}

// hotLatency is what the hot tier's requests take in these tests: a tenth of
// the regional bucket's, as a zonal bucket's are.
var hotLatency = sim.ObjectStoreConfig{GetLatency: time.Millisecond, HeadLatency: time.Millisecond / 2,
	PutLatency: 2 * time.Millisecond}

func newHotFixture(t *testing.T, latency sim.ObjectStoreConfig, config checkpoint.HotTierConfig) *hotFixture {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 1})
	f := &hotFixture{runtime: runtime, simHot: runtime.NewObjectStore("hot", latency)}
	f.regional = &countedStore{ObjectStore: runtime.ObjectStore()}
	f.hot = &countedStore{ObjectStore: f.simHot}
	config.Store = f.hot
	tier, err := checkpoint.NewHotTier(f.ctx(t), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tier.Close)
	f.tier = tier
	f.store = mustStore(t, checkpoint.Config{ObjectStore: f.regional, HotTier: tier})
	return f
}

func (f *hotFixture) ctx(t *testing.T) context.Context {
	return sim.WithRuntime(t.Context(), f.runtime)
}

func (f *hotFixture) settle(t *testing.T) {
	t.Helper()
	if err := f.tier.Settle(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// read opens ref and reads its one volume whole, which must be what the model
// holds, and reports how long the read took after the open.
func (f *hotFixture) read(t *testing.T, ref control.Ref, m *model) time.Duration {
	t.Helper()
	index, err := f.store.Open(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	f.settle(t)
	got := make([]byte, index.Size("root"))
	start := time.Now()
	if err := f.store.Read(t.Context(), index, "root", 0, got); err != nil {
		t.Fatal(err)
	}
	took := time.Since(start)
	if !bytes.Equal(got, m.contents["root"]) {
		t.Fatalf("%s read back as other bytes than were published", ref)
	}
	f.settle(t)
	return took
}

// requireCopies fails unless the hot tier holds exactly the regional bytes of
// every key, or, with none, nothing under them.
func (f *hotFixture) requireCopies(t *testing.T, held bool, keys ...platform.ObjectKey) {
	t.Helper()
	for _, key := range keys {
		hot, _, err := platform.ReadObject(t.Context(), f.simHot, key, 0, 1<<30, checkpoint.ErrCorrupt)
		if !held {
			if !errors.Is(err, platform.ErrNotFound) {
				t.Fatalf("the hot tier holds %s (%v), want nothing", key, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("the hot tier does not hold %s: %v", key, err)
		}
		regional, _, err := platform.ReadObject(t.Context(), f.runtime.ObjectStore(), key, 0, 1<<30, checkpoint.ErrCorrupt)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(hot, regional) {
			t.Fatalf("the hot tier's %s is not the regional bucket's", key)
		}
	}
}

// size is how many bytes the regional bucket holds under key.
func (f *hotFixture) size(t *testing.T, key platform.ObjectKey) uint64 {
	t.Helper()
	metadata, err := f.runtime.ObjectStore().Head(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(metadata.Size)
}

// publishedRef is the checkpoint publishFrom publishes.
var publishedRef = control.Ref{VM: "vm", Sequence: 2}

// A read misses the hot tier, is served by the regional bucket, and fills the
// hot tier with the object it missed behind it: the index object from the
// bytes its open read whole, and the part by a GET of the whole part. Once the
// fills have settled, the same reads hit the hot tier and send the regional
// bucket nothing.
func TestAMissIsFilledBehindTheReadAndTheNextReadHits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newHotFixture(t, hotLatency, checkpoint.HotTierConfig{SkipPublications: true})
		_, m, err := publishFrom(t, f.store, "vm", publishedPages)
		if err != nil {
			t.Fatal(err)
		}
		index, part := indexKey(t, "vm", 2), partKey(t, "vm", 2, 0)
		f.requireCopies(t, false, index, part)
		gets := f.regional.gets.Load()
		f.read(t, publishedRef, m)
		// The open's tail and the extent were read from the regional bucket,
		// and the part once more, whole, by its fill. The segment was read
		// from the index object the open's fill had put in the hot tier.
		if got := f.regional.gets.Load() - gets; got != 3 {
			t.Fatalf("the first read sent the regional bucket %d GETs, want 3", got)
		}
		f.requireCopies(t, true, index, part)
		want := checkpoint.HotTierStats{Hits: 1, Misses: 2, FromReads: 2, Sent: 2,
			SentBytes: f.size(t, index) + f.size(t, part), QueueBytes: checkpoint.DefaultHotTierQueueBytes}
		if got := f.tier.Stats(); got != want {
			t.Fatalf("after the first read the hot tier's stats are %+v, want %+v", got, want)
		}
		gets = f.regional.gets.Load()
		f.read(t, publishedRef, m)
		if got := f.regional.gets.Load() - gets; got != 0 {
			t.Fatalf("the second read sent the regional bucket %d GETs, want none", got)
		}
		want.Hits = 4
		if got := f.tier.Stats(); got != want {
			t.Fatalf("after the second read the hot tier's stats are %+v, want %+v", got, want)
		}
	})
}

// A publication writes each part to the hot tier once its regional PUT has
// succeeded, and its index object once that PUT has. While the part's PUT is
// in flight the hot tier holds nothing of it. A part the regional bucket
// refused reaches the hot tier never, and its publication tried again writes
// it then.
func TestAPublicationWritesTheHotTierOnlyOnceItsRegionalPutSucceeded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newHotFixture(t, hotLatency, checkpoint.HotTierConfig{})
		index, part := indexKey(t, "vm", 2), partKey(t, "vm", 2, 0)
		root, m, p := beginPublication(t, f.store, "vm", publishedPages)
		f.settle(t)
		f.requireCopies(t, true, indexKey(t, "vm", 1))
		f.regional.entered, f.regional.release = make(chan struct{}), make(chan struct{})
		f.regional.hold.Store(true)
		published := make(chan error, 1)
		go func() {
			_, err := p.Commit(t.Context(), m)
			published <- err
		}()
		<-f.regional.entered
		// Long enough for any fill of the hot tier to have landed.
		time.Sleep(time.Second)
		f.requireCopies(t, false, index, part)
		f.regional.hold.Store(false)
		close(f.regional.release)
		if err := <-published; err != nil {
			t.Fatal(err)
		}
		f.settle(t)
		f.requireCopies(t, true, index, part)
		if got := f.tier.Stats(); got.FromPublications != 3 || got.Sent != 3 || got.FromReads != 0 {
			t.Fatalf("the publications' fills came to %+v, want three objects sent", got)
		}

		// A part the regional bucket refuses reaches the hot tier never.
		next := control.Ref{VM: "vm", Sequence: 3}
		f.regional.fail.Store(true)
		retry := f.store.Begin(root, next)
		for _, page := range publishedPages {
			m.dirty(retry, "root", page, 0, sectorData("again", page, 0))
		}
		if _, err := retry.Commit(t.Context(), m); !errors.Is(err, platform.ErrInjectedFault) {
			t.Fatalf("a publication whose part the regional bucket refused = %v, want its failure", err)
		}
		f.settle(t)
		f.requireCopies(t, false, partKey(t, "vm", 3, 0), indexKey(t, "vm", 3))
		f.regional.fail.Store(false)
		again := f.store.Begin(root, next)
		for _, page := range publishedPages {
			m.dirty(again, "root", page, 0, sectorData("again", page, 0))
		}
		if _, err := again.Commit(t.Context(), m); err != nil {
			t.Fatal(err)
		}
		f.settle(t)
		f.requireCopies(t, true, partKey(t, "vm", 3, 0), indexKey(t, "vm", 3))
	})
}

// A hot tier that fails never fails a read: down, slower than its bound,
// holding other bytes under a name, or holding only the first half of a
// part. Each read is what was published, the regional bucket serves what the
// hot tier did not, and each failure is counted by why. Three in a row mark
// the hot tier down.
func TestAHotTierThatFailsNeverFailsARead(t *testing.T) {
	for _, test := range []struct {
		name    string
		latency sim.ObjectStoreConfig
		break_  func(t *testing.T, f *hotFixture)
		reason  checkpoint.HotFailure
		hits    uint64
		failed  uint64
		down    uint64
	}{
		{name: "down", latency: hotLatency, reason: checkpoint.HotFailError, failed: 3, down: 1,
			break_: func(t *testing.T, f *hotFixture) { f.simHot.Fail() }},
		{name: "slow", latency: sim.ObjectStoreConfig{GetLatency: time.Second, PutLatency: time.Millisecond},
			reason: checkpoint.HotFailSlow, failed: 3, down: 1},
		{name: "other-bytes", latency: hotLatency, reason: checkpoint.HotFailCorrupt, failed: 3, down: 1,
			break_: func(t *testing.T, f *hotFixture) {
				f.putHot(t, indexKey(t, "vm", 2), func(data []byte) []byte { return bytes.Repeat([]byte{7}, len(data)) })
				f.putHot(t, partKey(t, "vm", 2, 0), func(data []byte) []byte { return bytes.Repeat([]byte{7}, len(data)) })
			}},
		{name: "half-a-part", latency: hotLatency, reason: checkpoint.HotFailCorrupt, hits: 2, failed: 1,
			break_: func(t *testing.T, f *hotFixture) {
				f.putHot(t, indexKey(t, "vm", 2), func(data []byte) []byte { return data })
				f.putHot(t, partKey(t, "vm", 2, 0), func(data []byte) []byte { return data[:len(data)/2] })
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newHotFixture(t, test.latency, checkpoint.HotTierConfig{SkipPublications: true,
					Bound: 100 * time.Millisecond})
				_, m, err := publishFrom(t, f.store, "vm", publishedPages)
				if err != nil {
					t.Fatal(err)
				}
				if test.break_ != nil {
					test.break_(t, f)
				}
				f.read(t, publishedRef, m)
				got := f.tier.Stats()
				if got.Hits != test.hits || got.Failed[test.reason] != test.failed || got.MarkedDown != test.down ||
					got.Misses != 0 {
					t.Fatalf("the hot tier's stats are %+v, want %d hits, %d failures for %s and %d marked down",
						got, test.hits, test.failed, test.reason, test.down)
				}
			})
		})
	}
}

// putHot writes into the hot tier, under key, what change makes of the
// regional bytes.
func (f *hotFixture) putHot(t *testing.T, key platform.ObjectKey, change func([]byte) []byte) {
	t.Helper()
	data, _, err := platform.ReadObject(t.Context(), f.runtime.ObjectStore(), key, 0, 1<<30, checkpoint.ErrCorrupt)
	if err != nil {
		t.Fatal(err)
	}
	data = change(data)
	if _, err := f.simHot.Put(t.Context(), platform.PutRequest{Key: key, Body: bytes.NewReader(data),
		Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
}

// A hot tier marked down is skipped by every read for ten seconds, and the
// first read after that tries it again.
func TestAHotTierMarkedDownIsSkippedAndTriedAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newHotFixture(t, hotLatency, checkpoint.HotTierConfig{SkipPublications: true})
		_, m, err := publishFrom(t, f.store, "vm", publishedPages)
		if err != nil {
			t.Fatal(err)
		}
		f.simHot.Fail()
		f.read(t, publishedRef, m)
		hotGets := f.hot.gets.Load()
		f.read(t, publishedRef, m)
		if got := f.hot.gets.Load() - hotGets; got != 0 {
			t.Fatalf("a read while the hot tier is marked down asked it %d times, want none", got)
		}
		if got := f.tier.Stats(); got.Skipped != 3 || !got.Down || got.Failed[checkpoint.HotFailError] != 3 {
			t.Fatalf("the hot tier's stats are %+v, want three reads skipped while it is down", got)
		}
		time.Sleep(10 * time.Second)
		f.simHot.Recover()
		f.read(t, publishedRef, m)
		if got := f.tier.Stats(); got.Skipped != 3 || got.Down || got.Misses != 2 || got.Hits != 1 {
			t.Fatalf("the hot tier's stats are %+v, want it tried again and filled", got)
		}
	})
}

// Nothing waits on a fill. A read that misses the hot tier takes exactly as
// long behind a hot tier whose PUTs take ten seconds as behind one whose PUTs
// take a millisecond: the miss's round trip and the regional bucket's.
func TestAReadIsNotSlowedByItsHotTierFill(t *testing.T) {
	took := func(put time.Duration) time.Duration {
		var took time.Duration
		synctest.Test(t, func(t *testing.T) {
			latency := hotLatency
			latency.PutLatency = put
			f := newHotFixture(t, latency, checkpoint.HotTierConfig{SkipPublications: true})
			_, m, err := publishFrom(t, f.store, "vm", publishedPages)
			if err != nil {
				t.Fatal(err)
			}
			index, err := f.store.Open(t.Context(), publishedRef)
			if err != nil {
				t.Fatal(err)
			}
			f.settle(t)
			got := make([]byte, index.Size("root"))
			start := time.Now()
			if err := f.store.Read(t.Context(), index, "root", 0, got); err != nil {
				t.Fatal(err)
			}
			took = time.Since(start)
			if !bytes.Equal(got, m.contents["root"]) {
				t.Fatal("the read is not what was published")
			}
			f.settle(t)
		})
		return took
	}
	fast, slow := took(time.Millisecond), took(10*time.Second)
	if fast != slow {
		t.Fatalf("a read behind a fill whose PUT takes ten seconds took %v, and %v behind a fast one", slow, fast)
	}
}

// The queue holds what its bytes allow, and the rate what it has taken in
// the last second: a fill past either is dropped, never waited for, and the
// read it came from is unchanged. Two VMs of three incompressible 2 MiB pages
// are read while the hot tier's PUTs are held. Each read misses the index
// object, which its open fills, then misses it again while that fill is held,
// and misses the part once for each of its three pages, which are an extent
// each. A queue of 1 MiB holds both index objects and neither part, so all
// six misses of a part are dropped. A rate of 7 MiB a second has room for the
// first VM's index object and part and the second's index object, and not its
// part: the first VM's later misses of its part find its fill held, and the
// second's are dropped.
func TestTheHotTierDropsFillsPastItsQueueOrItsRate(t *testing.T) {
	for _, test := range []struct {
		name       string
		config     checkpoint.HotTierConfig
		reason     checkpoint.HotDrop
		fills      uint64
		duplicates uint64
		dropped    uint64
		queueSize  int64
	}{
		{name: "queue", config: checkpoint.HotTierConfig{SkipPublications: true, QueueBytes: 1 << 20},
			reason: checkpoint.HotDropQueue, fills: 2, duplicates: 2, dropped: 6, queueSize: 1 << 20},
		{name: "rate", config: checkpoint.HotTierConfig{SkipPublications: true, BytesPerSecond: 7 << 20},
			reason: checkpoint.HotDropRate, fills: 3, duplicates: 4, dropped: 3,
			queueSize: checkpoint.DefaultHotTierQueueBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newHotFixture(t, hotLatency, test.config)
				var models []*model
				for _, vm := range []string{"a", "b"} {
					_, m, err := publishFromOf(t, f.store, vm, publishedPages, noisySector)
					if err != nil {
						t.Fatal(err)
					}
					models = append(models, m)
				}
				f.hot.entered, f.hot.release = make(chan struct{}, 8), make(chan struct{})
				f.hot.hold.Store(true)
				released := false
				release := func() {
					if !released {
						released = true
						f.hot.hold.Store(false)
						close(f.hot.release)
					}
				}
				t.Cleanup(release)
				for at, vm := range []string{"a", "b"} {
					ref := control.Ref{VM: vm, Sequence: 2}
					index, err := f.store.Open(t.Context(), ref)
					if err != nil {
						t.Fatal(err)
					}
					got := make([]byte, index.Size("root"))
					if err := f.store.Read(t.Context(), index, "root", 0, got); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, models[at].contents["root"]) {
						t.Fatalf("%s read back as other bytes than were published", vm)
					}
				}
				synctest.Wait()
				got := f.tier.Stats()
				if got.Misses != 10 || got.Duplicates != test.duplicates || got.FromReads != test.fills ||
					got.Dropped[test.reason] != test.dropped || got.Sent != 0 || got.QueueBytes != test.queueSize {
					t.Fatalf("while the PUTs are held the hot tier's stats are %+v, want 10 misses, %d duplicates, "+
						"%d fills held and %d dropped for its %s", got, test.duplicates, test.fills, test.dropped,
						test.reason)
				}
				release()
				f.settle(t)
				if got := f.tier.Stats(); got.Sent != test.fills || got.Queued != 0 {
					t.Fatalf("once the PUTs went the hot tier's stats are %+v, want %d sent and nothing queued", got,
						test.fills)
				}
			})
		})
	}
}

// Two hosts that miss one object at once both fill it, with no version to
// check: their PUTs are of the same bytes under the same name. The hot tier
// takes the first, and the second finds the object there and writes nothing.
func TestTwoHostsFillingOneObjectWriteItOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newHotFixture(t, hotLatency, checkpoint.HotTierConfig{SkipPublications: true})
		if _, _, err := publishFrom(t, f.store, "vm", publishedPages); err != nil {
			t.Fatal(err)
		}
		other, err := checkpoint.NewHotTier(f.ctx(t), checkpoint.HotTierConfig{Store: f.hot, SkipPublications: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(other.Close)
		second := mustStore(t, checkpoint.Config{ObjectStore: f.regional, HotTier: other})
		// Both opens miss the index object, and both fills reach the hot
		// tier's PUT before either is let go.
		f.hot.entered, f.hot.release = make(chan struct{}, 2), make(chan struct{})
		f.hot.hold.Store(true)
		for _, store := range []*checkpoint.Store{f.store, second} {
			if _, err := store.Open(t.Context(), publishedRef); err != nil {
				t.Fatal(err)
			}
			<-f.hot.entered
		}
		f.hot.hold.Store(false)
		close(f.hot.release)
		f.settle(t)
		if err := other.Settle(t.Context()); err != nil {
			t.Fatal(err)
		}
		first, next := f.tier.Stats(), other.Stats()
		if first.Misses != 1 || next.Misses != 1 || first.Sent+next.Sent != 1 || first.Present+next.Present != 1 {
			t.Fatalf("the hosts' fills came to %+v and %+v, want one miss each, the index object sent once and "+
				"found there once", first, next)
		}
		f.requireCopies(t, true, indexKey(t, "vm", 2))
	})
}

// One hit in HeadCheckEvery has its regional object checked with a HEAD, so a
// warm hot tier does not hide an object the regional bucket lost.
func TestASampledHotHitChecksItsRegionalObject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newHotFixture(t, hotLatency, checkpoint.HotTierConfig{HeadCheckEvery: 3})
		_, m, err := publishFrom(t, f.store, "vm", publishedPages)
		if err != nil {
			t.Fatal(err)
		}
		f.settle(t)
		if err := f.runtime.ObjectStore().Delete(t.Context(),
			platform.DeleteRequest{Key: partKey(t, "vm", 2, 0)}); err != nil {
			t.Fatal(err)
		}
		heads := f.regional.heads.Load()
		f.read(t, publishedRef, m)
		if got := f.regional.heads.Load() - heads; got != 1 {
			t.Fatalf("three hits sent the regional bucket %d HEADs, want 1", got)
		}
		if got := f.tier.Stats(); got.Hits != 3 || got.HeadChecks != 1 || got.HeadMissing != 1 {
			t.Fatalf("the hot tier's stats are %+v, want one check of three hits finding the part gone", got)
		}
	})
}

// A hot tier and the cluster cache are alternatives: a store is refused one
// beside a cache whose disk fills the cluster, and given one beside a cache
// whose disk keeps every window whole.
func TestAStoreRefusesAHotTierBesideTheClusterCache(t *testing.T) {
	for _, test := range []struct {
		percent int
		want    error
	}{{percent: 0}, {percent: 1, want: checkpoint.ErrHotTierBesideClusterCache},
		{percent: 100, want: checkpoint.ErrHotTierBesideClusterCache}} {
		t.Run(fmt.Sprintf("share-%d", test.percent), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newHotFixture(t, hotLatency, checkpoint.HotTierConfig{})
				ctx := f.ctx(t)
				file, err := f.runtime.NewDisk("host", sim.DiskConfig{}).Open(ctx, "cache",
					platform.OpenOptions{Create: true})
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				budget, err := resource.New(4 << 10)
				if err != nil {
					t.Fatal(err)
				}
				cache, err := checkpoint.NewCache(ctx, budget, checkpoint.CacheConfig{Disk: file, DiskBytes: 64 << 20,
					DiskRegionBytes: pullRegionBytes, ClusterPercent: test.percent,
					Entropy: f.runtime.NewEntropy("host")})
				if err != nil {
					t.Fatal(err)
				}
				defer cache.Close()
				_, err = checkpoint.NewStore(checkpoint.Config{ObjectStore: f.regional, ObjectPrefix: testPrefix(t),
					Cache: cache, HotTier: f.tier})
				if !errors.Is(err, test.want) {
					t.Fatalf("a store with a hot tier beside a cluster share of %d%% = %v, want %v", test.percent,
						err, test.want)
				}
			})
		})
	}
}
