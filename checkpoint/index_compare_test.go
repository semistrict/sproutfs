package checkpoint_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/resource"
)

// The index comparison (TASK-66, TASK-81) runs a store that writes an index
// object at every checkpoint beside stores that defer it, over the same page
// workloads, and reports what each costs in object-store requests, bytes and
// modelled time. It runs in a synctest bubble over the
// simulated store, so its times are the store's latency model and nothing of
// this machine's speed: 10 ms a GET or LIST, 20 ms a PUT, 500 MiB/s.
//
// It publishes gigabytes of pages, so it is skipped unless
// SPROUTFS_COMPARE_INDEX is set. `just compare-index` runs it.

// comparedCadences are what the comparison runs: an index object at every
// checkpoint, and at every 16th and 64th.
var comparedCadences = []cadence{
	{name: "index"},
	{name: "deferred/16", every: 16},
	{name: "deferred/64", every: 64},
}

// objectKind is what a key holds, which is what the ledger splits traffic by.
type objectKind int

const (
	partObject objectKind = iota
	indexObject
	listing
	objectKinds
)

func kindOf(key string) objectKind {
	switch {
	case strings.HasSuffix(key, "/index"):
		return indexObject
	case strings.Contains(key, "/part/"):
		return partObject
	}
	return listing
}

// tally is one kind of request to one kind of object: how many, and the bytes
// they moved.
type tally struct {
	calls int64
	bytes int64
}

// traffic is every tally, by operation and object kind.
type traffic [platform.ListOperation + 1][objectKinds]tally

func (t traffic) minus(other traffic) traffic {
	for operation := range t {
		for kind := range t[operation] {
			t[operation][kind].calls -= other[operation][kind].calls
			t[operation][kind].bytes -= other[operation][kind].bytes
		}
	}
	return t
}

// sum is one operation's tally over the given kinds, or every kind.
func (t traffic) sum(operation platform.ObjectOperation, kinds ...objectKind) tally {
	if len(kinds) == 0 {
		kinds = []objectKind{partObject, indexObject, listing}
	}
	var total tally
	for _, kind := range kinds {
		total.calls += t[operation][kind].calls
		total.bytes += t[operation][kind].bytes
	}
	return total
}

// requests is every request, of every operation.
func (t traffic) requests() int64 {
	var calls int64
	for operation := range t {
		calls += t.sum(platform.ObjectOperation(operation)).calls
	}
	return calls
}

// ledger counts what passes through an object store, by operation and object
// kind.
type ledger struct {
	platform.ObjectStore
	mu   sync.Mutex
	seen traffic
}

func (l *ledger) record(operation platform.ObjectOperation, key string, bytes int64, err error) {
	if err != nil {
		bytes = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[operation][kindOf(key)].calls++
	l.seen[operation][kindOf(key)].bytes += bytes
}

func (l *ledger) snapshot() traffic {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen
}

func (l *ledger) Head(ctx context.Context, key platform.ObjectKey) (platform.ObjectMetadata, error) {
	metadata, err := l.ObjectStore.Head(ctx, key)
	l.record(platform.HeadOperation, key.String(), 0, err)
	return metadata, err
}

func (l *ledger) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	result, err := l.ObjectStore.Get(ctx, request)
	l.record(platform.GetOperation, request.Key.String(), result.ContentLength, err)
	return result, err
}

func (l *ledger) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	result, err := l.ObjectStore.Put(ctx, request)
	l.record(platform.PutOperation, request.Key.String(), request.Size, err)
	return result, err
}

func (l *ledger) Delete(ctx context.Context, request platform.DeleteRequest) error {
	err := l.ObjectStore.Delete(ctx, request)
	l.record(platform.DeleteOperation, request.Key.String(), 0, err)
	return err
}

func (l *ledger) List(ctx context.Context, request platform.ListRequest) (platform.ListResult, error) {
	result, err := l.ObjectStore.List(ctx, request)
	l.record(platform.ListOperation, "", 0, err)
	return result, err
}

// stored is what the store holds, by object kind, in bytes.
func (l *ledger) stored(t *testing.T) [objectKinds]int64 {
	t.Helper()
	var bytes [objectKinds]int64
	err := platform.ListAll(t.Context(), l.ObjectStore, platform.ObjectPrefix{}, func(object platform.ObjectMetadata) error {
		bytes[kindOf(object.Key.String())] += object.Size
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return bytes
}

// guest is the contents of one VM's volumes as a function of how often each
// page has been written, so nothing holds a copy of a volume. An eighth of a
// written page is random and the rest zeroes, which compresses about as well as
// the guest pages the workload report measured.
type guest struct {
	versions map[string]map[uint64]uint32
}

func newGuest() *guest {
	return &guest{versions: map[string]map[uint64]uint32{"ram0": {}, "root": {}}}
}

func (g *guest) clone() *guest {
	next := newGuest()
	for volume, versions := range g.versions {
		for page, version := range versions {
			next.versions[volume][page] = version
		}
	}
	return next
}

func (g *guest) ReadPage(_ context.Context, volume string, page uint64, dst []byte) error {
	clear(dst)
	version := g.versions[volume][page]
	if version == 0 {
		return nil
	}
	name := fnv.New64a()
	name.Write([]byte(volume))
	random := rand.New(rand.NewPCG(name.Sum64()^page, uint64(version)))
	fill := dst[:len(dst)/8]
	for at := 0; at+8 <= len(fill); at += 8 {
		binary.LittleEndian.PutUint64(fill[at:], random.Uint64())
	}
	return nil
}

// comparedShape is the VM every comparison runs: 2 GiB of RAM in 4 KiB pages
// and an 8 GiB root disk in 2 MiB pages, which is how a host creates them.
var comparedShape = map[string]checkpoint.VolumeSpec{
	"ram0": {Size: 2 << 30, PageSize: checkpoint.PageSize4KiB},
	"root": {Size: 8 << 30, PageSize: checkpoint.PageSize2MiB},
}

// sample is what one operation cost: its modelled time and its traffic.
type sample struct {
	elapsed time.Duration
	traffic traffic
}

// bench is one VM in one cadence. Its store publishes through a page cache, as
// a host's does, so a publication finds the segments it rewrites where the
// last one left them.
type bench struct {
	cadence  cadence
	objects  *ledger
	store    *checkpoint.Store
	cache    *checkpoint.Cache
	guest    *guest
	index    *checkpoint.Index
	vm       string
	sequence uint64
	// populated is the RAM pages the boot wrote, which the steady workloads
	// write again.
	populated []uint64
	random    *rand.Rand
	// stored is what the store held after each checkpoint and its reclamation.
	stored [][objectKinds]int64
}

func measure(objects *ledger, do func()) sample {
	before := objects.snapshot()
	start := time.Now()
	do()
	return sample{elapsed: time.Since(start), traffic: objects.snapshot().minus(before)}
}

// boot creates a VM and publishes its first checkpoint of contents: half its
// RAM, scattered, and the first GiB of its root disk, which is the image.
func boot(t *testing.T, l cadence) *bench {
	t.Helper()
	objects := &ledger{ObjectStore: sim.New(sim.Config{}).ObjectStore()}
	cache := newCache(t)
	store := mustStore(t, checkpoint.Config{ObjectStore: objects, Cache: cache, IndexEvery: l.every})
	b := &bench{cadence: l, objects: objects, store: store, cache: cache, guest: newGuest(), vm: "vm-a", sequence: 1,
		random: rand.New(rand.NewPCG(0xc0ffee, 0x66))}
	root, err := store.Root(t.Context(), control.Ref{VM: b.vm, Sequence: 1}, comparedShape)
	if err != nil {
		t.Fatal(err)
	}
	b.index = root
	ram := comparedShape["ram0"].Size / checkpoint.PageSize4KiB
	for page := range ram {
		if b.random.IntN(2) == 0 {
			b.populated = append(b.populated, page)
		}
	}
	image := make([]uint64, 512)
	for page := range image {
		image[page] = uint64(page)
	}
	b.publish(t, map[string][]uint64{"ram0": b.populated, "root": image})
	return b
}

// publish writes pages, publishes a checkpoint of them, and reclaims what it
// replaced. It reports the commit and the reclamation separately: only the
// commit is on the path to durability.
func (b *bench) publish(t *testing.T, writes map[string][]uint64) (commit, reclaim sample) {
	t.Helper()
	b.sequence++
	p := b.store.Begin(b.index, control.Ref{VM: b.vm, Sequence: b.sequence})
	for _, volume := range slices.Sorted(maps.Keys(writes)) {
		for _, page := range writes[volume] {
			b.guest.versions[volume][page]++
			p.Dirty(volume, page)
		}
	}
	var index *checkpoint.Index
	var err error
	commit = measure(b.objects, func() { index, err = p.Commit(t.Context(), b.guest) })
	if err != nil {
		t.Fatalf("%s: publish %s/%d: %v", b.cadence.name, b.vm, b.sequence, err)
	}
	if b.index.Ref().VM == b.vm {
		reclaim = measure(b.objects, func() { err = b.store.Reclaim(t.Context(), b.index, index, nil) })
		if err != nil {
			t.Fatalf("%s: reclaim %s/%d: %v", b.cadence.name, b.vm, b.sequence, err)
		}
	}
	b.index = index
	b.stored = append(b.stored, b.objects.stored(t))
	return commit, reclaim
}

// scattered draws count distinct pages of RAM the boot populated, uniformly.
func (b *bench) scattered(count int) []uint64 {
	chosen := make(map[uint64]bool, count)
	for len(chosen) < count {
		chosen[b.populated[b.random.IntN(len(b.populated))]] = true
	}
	return slices.Sorted(maps.Keys(chosen))
}

// stream is the distinct RAM pages a guest writing rate pages a second
// dirties in seconds: most of its writes land in a hot set it keeps writing,
// and the rest anywhere in what it has populated.
func (b *bench) stream(rate, seconds int) []uint64 {
	const hot = 8192
	chosen := make(map[uint64]bool)
	for range rate * seconds {
		if b.random.IntN(5) != 0 {
			chosen[b.populated[b.random.IntN(hot)]] = true
			continue
		}
		chosen[b.populated[b.random.IntN(len(b.populated))]] = true
	}
	return slices.Sorted(maps.Keys(chosen))
}

// newCache is a host's page cache, a GiB of it.
func newCache(t *testing.T) *checkpoint.Cache {
	t.Helper()
	budget, err := resource.New(1 << 30)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(budget, checkpoint.CacheConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	return cache
}

// cold is another host: a store over the same objects with an empty cache of
// its own, which is where a migrated or restarted VM opens.
type cold struct {
	*bench
	store *checkpoint.Store
}

// open reads the VM's latest checkpoint back from storage alone, on another
// host.
func (b *bench) open(t *testing.T) (cold, *checkpoint.Index, sample) {
	t.Helper()
	host := cold{bench: b, store: mustStore(t, checkpoint.Config{ObjectStore: b.objects, Cache: newCache(t), IndexEvery: b.cadence.every})}
	var index *checkpoint.Index
	var err error
	cost := measure(b.objects, func() { index, err = host.store.Open(t.Context(), b.index.Ref()) })
	if err != nil {
		t.Fatalf("%s: open %s: %v", b.cadence.name, b.index.Ref(), err)
	}
	return host, index, cost
}

// fault picks a RAM page the guest has written, which a fault must fetch.
func (b *bench) fault() uint64 {
	return b.populated[b.random.IntN(len(b.populated))] * checkpoint.PageSize4KiB
}

// read is one read of an index this host opened, as a pager's fault makes one.
func (c cold) read(t *testing.T, index *checkpoint.Index, volume string, offset, length uint64) sample {
	t.Helper()
	dst := make([]byte, length)
	var err error
	cost := measure(c.objects, func() { err = c.store.Read(t.Context(), index, volume, offset, dst) })
	if err != nil {
		t.Fatalf("%s: read %s [%d,+%d): %v", c.cadence.name, volume, offset, length, err)
	}
	return cost
}

// fork starts a child of the VM's latest checkpoint in the same store.
func (b *bench) fork(vm string) *bench {
	return &bench{cadence: b.cadence, objects: b.objects, store: b.store, cache: b.cache, guest: b.guest.clone(),
		index: b.index, vm: vm, populated: b.populated, random: rand.New(rand.NewPCG(0xf0, 0x4c))}
}

// results is every figure the comparison reports, by cadence.
type results struct {
	// publish is each dirty-set size's commits and reclamations.
	publish map[int][]sample
	reclaim map[int][]sample
	// interval is a minute of one write stream at each checkpoint interval.
	interval map[int][]sample
	// opens is an open after each checkpoint of a long run.
	opens []sample
	// first is the first page fault after the last open, and runs the
	// read-ahead runs after it; loaded is the page entries in memory at open and
	// after the reads.
	first, runs    sample
	loadedAtOpen   int
	loadedAfterRun int
	// fork is a fork's first checkpoint, its open and its first fault.
	fork, forkOpen, forkFault sample
	// stored is the bytes in the store after every checkpoint of the VM.
	stored [][objectKinds]int64
}

var dirtySizes = []int{64, 1024, 8192, 32768}

var intervals = []int{1, 5, 15, 60}

const (
	checkpointsPerSize = 16
	openRun            = 64
	streamRate         = 2000
)

func TestCompareIndexCadences(t *testing.T) {
	if os.Getenv("SPROUTFS_COMPARE_INDEX") == "" {
		t.Skip("set SPROUTFS_COMPARE_INDEX=1 to run the index comparison")
	}
	all := make(map[string]*results)
	for _, l := range comparedCadences {
		synctest.Test(t, func(t *testing.T) {
			all[l.name] = compareOne(t, l)
		})
	}
	report(t, all)
}

func compareOne(t *testing.T, l cadence) *results {
	r := &results{publish: map[int][]sample{}, reclaim: map[int][]sample{}, interval: map[int][]sample{}}
	b := boot(t, l)
	for _, size := range dirtySizes {
		for range checkpointsPerSize {
			commit, reclaim := b.publish(t, map[string][]uint64{"ram0": b.scattered(size), "root": {uint64(b.random.IntN(512))}})
			r.publish[size] = append(r.publish[size], commit)
			r.reclaim[size] = append(r.reclaim[size], reclaim)
		}
	}
	for _, seconds := range intervals {
		for range 60 / seconds {
			commit, reclaim := b.publish(t, map[string][]uint64{"ram0": b.stream(streamRate, seconds)})
			r.interval[seconds] = append(r.interval[seconds], commit, reclaim)
		}
	}
	for range openRun {
		b.publish(t, map[string][]uint64{"ram0": b.scattered(1024)})
		_, _, cost := b.open(t)
		r.opens = append(r.opens, cost)
	}
	host, index, _ := b.open(t)
	r.loadedAtOpen = checkpoint.LoadedPages(index)
	ram := comparedShape["ram0"].Size
	r.first = host.read(t, index, "ram0", b.fault(), checkpoint.PageSize4KiB)
	for range 64 {
		run := uint64(b.random.IntN(int(ram/checkpoint.PageSize2MiB))) * checkpoint.PageSize2MiB
		cost := host.read(t, index, "ram0", run, checkpoint.PageSize2MiB)
		r.runs.elapsed += cost.elapsed
		r.runs.traffic = addTraffic(r.runs.traffic, cost.traffic)
	}
	r.loadedAfterRun = checkpoint.LoadedPages(index)
	r.stored = b.stored
	child := b.fork("vm-b")
	r.fork, _ = child.publish(t, map[string][]uint64{"ram0": child.scattered(64)})
	childHost, childIndex, cost := child.open(t)
	r.forkOpen = cost
	r.forkFault = childHost.read(t, childIndex, "ram0", child.fault(), checkpoint.PageSize4KiB)
	return r
}

func addTraffic(a, b traffic) traffic {
	for operation := range a {
		for kind := range a[operation] {
			a[operation][kind].calls += b[operation][kind].calls
			a[operation][kind].bytes += b[operation][kind].bytes
		}
	}
	return a
}

// report logs every table the measurement report quotes, as Markdown.
func report(t *testing.T, all map[string]*results) {
	var out strings.Builder
	names := make([]string, 0, len(comparedCadences))
	for _, l := range comparedCadences {
		names = append(names, l.name)
	}
	mib := func(bytes int64) string { return fmt.Sprintf("%.2f", float64(bytes)/(1<<20)) }
	kib := func(bytes int64) string { return fmt.Sprintf("%.1f", float64(bytes)/(1<<10)) }
	ms := func(d time.Duration) string { return fmt.Sprintf("%.0f", float64(d)/float64(time.Millisecond)) }
	mean := func(samples []sample) (total sample, worst time.Duration) {
		for _, s := range samples {
			total.elapsed += s.elapsed
			total.traffic = addTraffic(total.traffic, s.traffic)
			worst = max(worst, s.elapsed)
		}
		return total, worst
	}
	per := func(total int64, count int) string { return fmt.Sprintf("%.1f", float64(total)/float64(count)) }

	fmt.Fprintf(&out, "\n### Publishing a checkpoint\n\n")
	fmt.Fprintf(&out, "Mean per checkpoint over %d checkpoints of each size, plus one root-disk page each.\n\n", checkpointsPerSize)
	fmt.Fprintf(&out, "| RAM pages | cadence | PUTs | part MiB | index KiB | index GETs | compaction GETs | mean commit ms | worst commit ms | reclaim requests |\n")
	fmt.Fprintf(&out, "| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, size := range dirtySizes {
		for _, name := range names {
			commits, worst := mean(all[name].publish[size])
			reclaims, _ := mean(all[name].reclaim[size])
			count := len(all[name].publish[size])
			fmt.Fprintf(&out, "| %d | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", size, name,
				per(commits.traffic.sum(platform.PutOperation).calls, count),
				mib(commits.traffic.sum(platform.PutOperation, partObject).bytes/int64(count)),
				kib(commits.traffic.sum(platform.PutOperation, indexObject).bytes/int64(count)),
				per(commits.traffic.sum(platform.GetOperation, indexObject).calls, count),
				per(commits.traffic.sum(platform.GetOperation, partObject).calls, count),
				ms(commits.elapsed/time.Duration(count)), ms(worst),
				per(reclaims.traffic.requests(), count))
		}
	}

	fmt.Fprintf(&out, "\n### A minute of writes at each checkpoint interval\n\n")
	fmt.Fprintf(&out, "The guest writes %d RAM pages a second, four in five of them into a hot set of 8,192 pages.\n\n", streamRate)
	fmt.Fprintf(&out, "| interval s | cadence | checkpoints | PUTs | part MiB | index MiB | index share |\n")
	fmt.Fprintf(&out, "| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, seconds := range intervals {
		for _, name := range names {
			samples := all[name].interval[seconds]
			var commits []sample
			for at := 0; at < len(samples); at += 2 {
				commits = append(commits, samples[at])
			}
			total, _ := mean(samples)
			parts := total.traffic.sum(platform.PutOperation, partObject).bytes
			metadata := total.traffic.sum(platform.PutOperation, indexObject).bytes
			fmt.Fprintf(&out, "| %d | %s | %d | %d | %s | %s | %.0f%% |\n", seconds, name, len(commits),
				total.traffic.sum(platform.PutOperation).calls, mib(parts), mib(metadata),
				100*float64(metadata)/float64(parts+metadata))
		}
	}

	fmt.Fprintf(&out, "\n### Opening a checkpoint\n\n")
	fmt.Fprintf(&out, "An open after each of %d checkpoints of 1,024 RAM pages.\n\n", openRun)
	fmt.Fprintf(&out, "| cadence | mean requests | worst requests | mean KiB read | mean ms | worst ms |\n")
	fmt.Fprintf(&out, "| --- | --- | --- | --- | --- | --- |\n")
	for _, name := range names {
		opens := all[name].opens
		total, worst := mean(opens)
		var worstRequests int64
		for _, s := range opens {
			worstRequests = max(worstRequests, s.traffic.requests())
		}
		fmt.Fprintf(&out, "| %s | %s | %d | %s | %s | %s |\n", name, per(total.traffic.requests(), len(opens)), worstRequests,
			kib(total.traffic.sum(platform.GetOperation).bytes/int64(len(opens))),
			ms(total.elapsed/time.Duration(len(opens))), ms(worst))
	}

	fmt.Fprintf(&out, "\n### Reading after an open\n\n")
	fmt.Fprintf(&out, "One 4 KiB fault, then 64 read-ahead runs of 2 MiB of RAM at random, one after another.\n\n")
	fmt.Fprintf(&out, "| cadence | first fault requests | first fault ms | runs requests | runs ms | page entries at open | after runs |\n")
	fmt.Fprintf(&out, "| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, name := range names {
		r := all[name]
		fmt.Fprintf(&out, "| %s | %d | %s | %d | %s | %d | %d |\n", name, r.first.traffic.requests(), ms(r.first.elapsed),
			r.runs.traffic.requests(), ms(r.runs.elapsed), r.loadedAtOpen, r.loadedAfterRun)
	}

	fmt.Fprintf(&out, "\n### Forking\n\n")
	fmt.Fprintf(&out, "A fork's first checkpoint of 64 RAM pages, an open of it, and its first fault.\n\n")
	fmt.Fprintf(&out, "| cadence | first checkpoint PUTs | PUT KiB | commit ms | open requests | open KiB | open ms | first fault ms |\n")
	fmt.Fprintf(&out, "| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, name := range names {
		r := all[name]
		fmt.Fprintf(&out, "| %s | %d | %s | %s | %d | %s | %s | %s |\n", name,
			r.fork.traffic.sum(platform.PutOperation).calls, kib(r.fork.traffic.sum(platform.PutOperation).bytes), ms(r.fork.elapsed),
			r.forkOpen.traffic.requests(), kib(r.forkOpen.traffic.sum(platform.GetOperation).bytes), ms(r.forkOpen.elapsed),
			ms(r.forkFault.elapsed))
	}

	fmt.Fprintf(&out, "\n### What the store holds\n\n")
	fmt.Fprintf(&out, "Sampled after every checkpoint of the whole run and its reclamation.\n\n")
	fmt.Fprintf(&out, "| cadence | checkpoints | mean part MiB | mean metadata MiB | mean total MiB | peak total MiB | final total MiB |\n")
	fmt.Fprintf(&out, "| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, name := range names {
		samples := all[name].stored
		var parts, metadata, peak int64
		for _, held := range samples {
			parts += held[partObject]
			metadata += held[indexObject]
			peak = max(peak, held[partObject]+held[indexObject])
		}
		count := int64(len(samples))
		final := samples[len(samples)-1]
		fmt.Fprintf(&out, "| %s | %d | %s | %s | %s | %s | %s |\n", name, count, mib(parts/count), mib(metadata/count),
			mib((parts+metadata)/count), mib(peak), mib(final[partObject]+final[indexObject]))
	}
	t.Log(out.String())
}
