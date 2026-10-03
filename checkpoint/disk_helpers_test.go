package checkpoint

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/stripe"
)

// testRegionBytes is the region the disk tests use: room for 21 of their
// 3,000-byte items.
const testRegionBytes = 64 << 10

// testItemBytes is the envelope length most disk tests write.
const testItemBytes = 3000

// testItemsPerRegion is how many testItemBytes items, each with a header of
// 50 bytes and a table entry of 40 under keyOf's names, one region holds:
// (65,536 - 40) / (3,050 + 40).
const testItemsPerRegion = 21

// whole is the one stripe of envelope under 1+0: the envelope whole.
func whole(envelope []byte) stripe.Stripe {
	return stripe.Stripe{Code: wholeCode, Index: 0, Length: len(envelope), Bytes: envelope}
}

// checkedFile wraps the cache's file the way FoundationDB's
// AsyncFileWriteChecker does: it keeps a copy of every byte written, and on
// each read tells a disk that lied from one that returned what was written. A
// read under a context carrying a diskWitness marks the witness when the disk
// lied, so a test can require that every item the cache refused was one the
// disk damaged, and never one the cache misread.
//
// It also logs the operations it forwards, holds reads at a gate, and can lose
// power around a region's table write or after a number of operations.
type checkedFile struct {
	disk        *sim.Disk
	name        string
	regionBytes int64

	mu    sync.Mutex
	inner platform.File
	// shadow is every page written since the file was opened, and blind marks
	// a file whose bytes are no longer known after a power loss.
	shadow map[int64][]byte
	blind  bool
	ops    []string
	lies   int
	// gate holds a read whose offset falls in [gateFrom, gateTo).
	gate             *readGate
	gateFrom, gateTo int64
	// powerLoss names the moment around the next table write to lose power
	// at: "before-table" or "after-table".
	powerLoss string
	// armed loses the power once remaining more writes, syncs, allocations,
	// punches and truncations have been forwarded, and lost says it was.
	armed, lost bool
	remaining   int
}

// readGate is one read held until a test releases it.
type readGate struct {
	entered, release chan struct{}
}

const shadowPage = 4 << 10

func newCheckedFile(ctx context.Context, disk *sim.Disk, regionBytes int64) (*checkedFile, error) {
	inner, err := disk.Open(ctx, "cache", platform.OpenOptions{Create: true, Truncate: true})
	if err != nil {
		return nil, err
	}
	return &checkedFile{disk: disk, name: "cache", regionBytes: regionBytes, inner: inner,
		shadow: make(map[int64][]byte)}, nil
}

// reopen opens the file again over what the disk holds, as a host that
// restarts does. What was written is still known, unless the power was lost.
func (f *checkedFile) reopen(ctx context.Context) error {
	inner, err := f.disk.Open(ctx, f.name, platform.OpenOptions{Create: true})
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inner = inner
	f.ops = append(f.ops, "reopen")
	return nil
}

// losePower loses the disk's power now. The file's handle is gone with it,
// so every operation the cache tries after it fails, as it would in a host
// that died, until the file is opened again.
func (f *checkedFile) losePower(ctx context.Context) error {
	if err := f.disk.PowerLoss(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blind = true
	f.ops = append(f.ops, "power-loss")
	return nil
}

// regionBase is where the region in slot begins in a file of testRegionBytes
// regions, past the header's span.
func regionBase(slot int64) int64 { return (slot + 1) * testRegionBytes }

// testDeployment is the deployment the disk tests' files belong to.
var testDeployment = CacheDeployment{Store: "sim", Bucket: "test-bucket", Prefix: "cluster/"}

func (f *checkedFile) file() platform.File {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inner
}

func (f *checkedFile) log(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
}

// operations is the log of what was forwarded.
func (f *checkedFile) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ops)
}

// lied reports how many reads returned what was not written.
func (f *checkedFile) lied() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lies
}

// hold arms a gate for the next read that starts in [from, to).
func (f *checkedFile) hold(from, to int64) *readGate {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = &readGate{entered: make(chan struct{}), release: make(chan struct{})}
	f.gateFrom, f.gateTo = from, to
	return f.gate
}

func (f *checkedFile) ReadAt(ctx context.Context, destination []byte, offset int64) (int, error) {
	f.mu.Lock()
	gate := f.gate
	if gate != nil && offset >= f.gateFrom && offset < f.gateTo {
		f.gate = nil
	} else {
		gate = nil
	}
	f.mu.Unlock()
	if gate != nil {
		close(gate.entered)
		<-gate.release
	}
	n, err := f.file().ReadAt(ctx, destination, offset)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, fmt.Sprintf("read %d+%d", offset, len(destination)))
	if f.blind || !bytes.Equal(destination[:n], f.expectedLocked(offset, n)) {
		f.lies++
		if witness, ok := ctx.Value(diskWitnessKey{}).(*diskWitness); ok {
			witness.lied.Store(true)
		}
	}
	return n, err
}

// expectedLocked is what the file holds at [offset, offset+n) by what was
// written to it.
func (f *checkedFile) expectedLocked(offset int64, n int) []byte {
	expected := make([]byte, n)
	for at := int64(0); at < int64(n); {
		page, within := (offset+at)/shadowPage, (offset+at)%shadowPage
		take := min(int64(n)-at, shadowPage-within)
		if data, ok := f.shadow[page]; ok {
			copy(expected[at:at+take], data[within:])
		}
		at += take
	}
	return expected
}

func (f *checkedFile) recordLocked(offset int64, data []byte) {
	for at := int64(0); at < int64(len(data)); {
		page, within := (offset+at)/shadowPage, (offset+at)%shadowPage
		take := min(int64(len(data))-at, shadowPage-within)
		stored := make([]byte, shadowPage)
		copy(stored, f.shadow[page])
		copy(stored[within:], data[at:at+take])
		f.shadow[page] = stored
		at += take
	}
}

// loseAfter arms the file to lose the power once n more operations that
// change it have been forwarded, before the next one.
func (f *checkedFile) loseAfter(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.remaining = true, n
}

// powerLost reports whether the file armed by loseAfter has lost the power.
func (f *checkedFile) powerLost() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lost
}

// change counts one operation that changes the file, and loses the power
// before it where the file was armed to.
func (f *checkedFile) change(ctx context.Context) {
	f.mu.Lock()
	lose := f.armed && f.remaining == 0
	if lose {
		f.armed, f.lost = false, true
	} else if f.armed {
		f.remaining--
	}
	f.mu.Unlock()
	if lose {
		if err := f.losePower(ctx); err != nil {
			panic(err)
		}
	}
}

func (f *checkedFile) WriteAt(ctx context.Context, source []byte, offset int64) (int, error) {
	f.change(ctx)
	table := (offset+int64(len(source)))%f.regionBytes == 0
	if table {
		f.maybeLosePower(ctx, "before-table")
	}
	n, err := f.file().WriteAt(ctx, source, offset)
	f.mu.Lock()
	f.recordLocked(offset, source[:n])
	f.ops = append(f.ops, fmt.Sprintf("write %d+%d", offset, len(source)))
	f.mu.Unlock()
	if table {
		f.maybeLosePower(ctx, "after-table")
	}
	return n, err
}

// maybeLosePower loses power at moment if the test asked for it there, and
// opens the file again on what the disk kept.
func (f *checkedFile) maybeLosePower(ctx context.Context, moment string) {
	f.mu.Lock()
	lose := f.powerLoss == moment
	if lose {
		f.powerLoss = ""
	}
	f.mu.Unlock()
	if !lose {
		return
	}
	if err := f.disk.PowerLoss(ctx); err != nil {
		panic(err)
	}
	inner, err := f.disk.Open(ctx, f.name, platform.OpenOptions{Create: true})
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inner, f.blind = inner, true
	f.ops = append(f.ops, "power-loss "+moment)
}

func (f *checkedFile) Truncate(ctx context.Context, size int64) error {
	f.change(ctx)
	err := f.file().Truncate(ctx, size)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "truncate")
	if err == nil {
		for page := range f.shadow {
			if page*shadowPage >= size {
				delete(f.shadow, page)
			}
		}
		if size%shadowPage != 0 {
			f.recordLocked(size, make([]byte, shadowPage-size%shadowPage))
		}
	}
	return err
}

func (f *checkedFile) Sync(ctx context.Context) error {
	f.change(ctx)
	f.log("sync")
	return f.file().Sync(ctx)
}

func (f *checkedFile) Size(ctx context.Context) (int64, error) { return f.file().Size(ctx) }
func (f *checkedFile) Close() error                            { return f.file().Close() }

func (f *checkedFile) PunchHole(ctx context.Context, offset, length int64) error {
	f.change(ctx)
	err := f.file().(platform.SparseFile).PunchHole(ctx, offset, length)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, fmt.Sprintf("punch %d+%d", offset, length))
	if err == nil {
		f.recordLocked(offset, make([]byte, length))
	}
	return err
}

func (f *checkedFile) Allocate(ctx context.Context, offset, length int64) error {
	f.change(ctx)
	f.log(fmt.Sprintf("allocate %d+%d", offset, length))
	return f.file().(platform.AllocatingFile).Allocate(ctx, offset, length)
}

// diskWitness is carried by a read's context, and marked when the disk lied
// to that read.
type diskWitness struct{ lied atomic.Bool }

type diskWitnessKey struct{}

func witnessed(ctx context.Context) (context.Context, *diskWitness) {
	witness := &diskWitness{}
	return context.WithValue(ctx, diskWitnessKey{}, witness), witness
}

// testBudget is a disk limiter a test drives: a share it sets, and a write
// budget that refuses the kinds it names.
type testBudget struct {
	share  atomic.Int64
	mu     sync.Mutex
	refuse map[WriteKind]bool
	// asked counts the writes of each kind the disk asked to make, and
	// refused the ones refused.
	asked, refused map[WriteKind]int
}

func newTestBudget(regions int64) *testBudget {
	budget := &testBudget{refuse: make(map[WriteKind]bool), asked: make(map[WriteKind]int),
		refused: make(map[WriteKind]int)}
	budget.share.Store(regions * testRegionBytes)
	return budget
}

func (b *testBudget) Share() int64 { return b.share.Load() }

func (b *testBudget) Admit(_ int64, kind WriteKind) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked[kind]++
	if b.refuse[kind] {
		b.refused[kind]++
		return false
	}
	return true
}

func (b *testBudget) refusing(kind WriteKind) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuse[kind] = true
}

// diskFixture is a cache disk over a checked file on a simulated disk.
type diskFixture struct {
	runtime  *sim.Runtime
	simDisk  *sim.Disk
	file     *checkedFile
	budget   *testBudget
	settings diskSettings
	disk     *cacheDisk
	model    map[diskKey][]byte
	// caches is the list of caches the disk follows, made from its own
	// identity; nil follows none.
	caches func(self CacheIdentity) rank.List
}

type diskFixtureConfig struct {
	seed       uint64
	regions    int64
	indexLimit int64
	disk       sim.DiskConfig
	// caches is the list of caches the disk follows, made from its own
	// identity once it opens; nil follows none, and keeps envelopes whole.
	caches func(self CacheIdentity) rank.List
	// clusterPercent is the share of windows the disk places by that list.
	clusterPercent int
}

// follow has the fixture's disk follow its list of caches, if it has one.
func (f *diskFixture) follow() {
	if f.caches == nil {
		return
	}
	list := f.caches(f.disk.identity)
	f.disk.follow(func() rank.List { return list })
}

// listOf is the list of the cache self and caches others, of weight one each,
// under code.
func listOf(code rank.Code, self CacheIdentity, others ...CacheIdentity) rank.List {
	caches := []rank.Cache{{Identity: self, Weight: 1}}
	for _, other := range others {
		caches = append(caches, rank.Cache{Identity: other, Weight: 1})
	}
	list, err := rank.NewList(code, caches)
	if err != nil {
		panic(err)
	}
	return list
}

// newDiskFixture builds a fixture on a runtime of its own.
func newDiskFixture(t *testing.T, config diskFixtureConfig) *diskFixture {
	t.Helper()
	f, err := openDiskFixture(t.Context(), sim.New(sim.Config{Seed: config.seed}), config)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// openDiskFixture builds a fixture on runtime. A runtime whose operations a
// scheduler releases is opened from the workload's own goroutine.
func openDiskFixture(ctx context.Context, runtime *sim.Runtime, config diskFixtureConfig) (*diskFixture, error) {
	simDisk := runtime.NewDisk("host", config.disk)
	file, err := newCheckedFile(ctx, simDisk, testRegionBytes)
	if err != nil {
		return nil, err
	}
	budget := newTestBudget(config.regions)
	limit := config.indexLimit
	if limit == 0 {
		limit = DefaultDiskIndexBytes
	}
	f := &diskFixture{runtime: runtime, simDisk: simDisk, file: file, budget: budget,
		settings: diskSettings{regionBytes: testRegionBytes, indexLimit: limit, threshold: 1,
			deployment: testDeployment, entropy: runtime.NewEntropy("cache-disk"),
			clusterPercent: config.clusterPercent},
		model: make(map[diskKey][]byte), caches: config.caches}
	if f.disk, err = openCacheDisk(ctx, file, budget, f.settings); err != nil {
		return nil, err
	}
	f.follow()
	return f, nil
}

// restart opens a new cache disk over the fixture's file, as a host that
// restarts does, and leaves the old one behind without closing it. What it
// wrote stays the model.
func (f *diskFixture) restart(ctx context.Context) error {
	if err := f.file.reopen(ctx); err != nil {
		return err
	}
	disk, err := openCacheDisk(ctx, f.file, f.budget, f.settings)
	if err != nil {
		return err
	}
	f.disk = disk
	f.follow()
	return nil
}

// ctx is the test's context carrying the fixture's runtime.
func (f *diskFixture) ctx(t *testing.T) context.Context {
	return sim.WithRuntime(t.Context(), f.runtime)
}

// keyOf is the key of one 4 KiB page of volume ram0 of a checkpoint of vm.
func keyOf(vm string, page uint64) diskKey {
	return pageDiskKey(control.Identity{Ref: control.Ref{VM: vm, Sequence: 1}, Volume: "ram0", Page: page},
		Geometry{PageSize: PageSize4KiB, SegmentPages: segmentPages4KiB})
}

// payloadOf is the envelope a test writes under key: length bytes no other key
// shares. Past 32 bytes, it ends in the SHA-256 of what comes before, so it
// checks itself as an envelope does (selfChecked), and another key's payload
// passes that check too, as another page's envelope would.
func payloadOf(key diskKey, length int) []byte {
	data := make([]byte, length)
	seed := fmt.Sprintf("%s/%d/%s/%d", key.Ref.VM, key.Ref.Sequence, key.Volume, key.Page)
	for at := range data {
		data[at] = seed[at%len(seed)] ^ byte(at*31)
	}
	if length >= sha256.Size {
		sum := sha256.Sum256(data[:length-sha256.Size])
		copy(data[length-sha256.Size:], sum[:])
	}
	return data
}

// errNotSelfChecked reports a payload whose last 32 bytes are not the SHA-256
// of the rest.
var errNotSelfChecked = errors.New("the payload fails its own SHA-256")

// selfChecked is the check a payload makes of itself, as an envelope's SHA-256
// does: it knows nothing of the key it was read under.
func selfChecked(payload []byte) error {
	if len(payload) < sha256.Size {
		return errNotSelfChecked
	}
	body := payload[:len(payload)-sha256.Size]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], payload[len(body):]) {
		return errNotSelfChecked
	}
	return nil
}

// write keeps one item of length bytes under key, and remembers it.
func (f *diskFixture) write(t *testing.T, key diskKey, length int) {
	t.Helper()
	data := payloadOf(key, length)
	if err := f.disk.write(f.ctx(t), key, data, WriteFillPublication); err != nil {
		t.Fatal(err)
	}
	f.model[key] = data
}

// read reads key and requires a hit with what was written.
func (f *diskFixture) read(t *testing.T, key diskKey) {
	t.Helper()
	data, outcome := f.disk.read(f.ctx(t), key, selfChecked)
	if outcome != diskHit || !bytes.Equal(data, f.model[key]) {
		t.Fatalf("reading %v found %d and %d bytes, want a hit of %d", key, outcome, len(data), len(f.model[key]))
	}
	f.disk.served(key)
}

// held reports which of keys the disk holds.
func (f *diskFixture) held(ctx context.Context, keys []diskKey) []bool {
	found := make([]bool, len(keys))
	for at, key := range keys {
		found[at] = f.disk.has(ctx, key)
	}
	return found
}

// checkInvariants checks what the disk's bookkeeping must say whenever no
// operation is in flight: the index's charge and count are its entries', the
// regions it holds are the open one, the closed ones and none waiting, and no
// region is read.
func (d *cacheDisk) checkInvariants(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	var used int64
	live := 0
	for hash, entries := range d.index.windows {
		for _, entry := range entries {
			if entry.removed || entry.hash != hash || entry.region.evicted {
				t.Errorf("the index names an entry that is removed, misfiled or in an evicted region: %+v", entry)
			}
			used += entry.charge()
			counted := 0
			entry.each(func(uint16, uint8, diskLocation) { counted++ })
			if counted != entry.live {
				t.Errorf("an entry counts %d live items and holds %d", entry.live, counted)
			}
			live += entry.live
		}
	}
	if used != d.index.used || live != d.index.live {
		t.Errorf("the index charges %d bytes for %d items, and its entries come to %d for %d",
			d.index.used, d.index.live, used, live)
	}
	open := 0
	if d.open != nil {
		open = 1
	}
	if d.pending != 0 || d.held != open+len(d.closed) {
		t.Errorf("the disk holds %d regions with %d waiting, and has %d open and %d closed",
			d.held, d.pending, open, len(d.closed))
	}
	for _, region := range append(slices.Clone(d.closed), d.open) {
		if region != nil && (region.readers != 0 || region.evicted) {
			t.Errorf("region %d has %d readers and evicted %v at rest", region.sequence, region.readers,
				region.evicted)
		}
	}
	slots := slices.Clone(d.free)
	for _, region := range append(slices.Clone(d.closed), d.open) {
		if region != nil {
			slots = append(slots, region.slot)
		}
	}
	slices.Sort(slots)
	if int64(len(slots)) != d.next || len(slots) != len(slices.Compact(slices.Clone(slots))) {
		t.Errorf("the disk's slots %v are not each used once below %d", slots, d.next)
	}
}

// keysOf is the keys of a model, sorted.
func keysOf(model map[diskKey][]byte) []diskKey {
	return slices.SortedFunc(maps.Keys(model), func(a, b diskKey) int {
		if a.Ref.VM != b.Ref.VM {
			return compareStrings(a.Ref.VM, b.Ref.VM)
		}
		return int(a.Page) - int(b.Page)
	})
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
