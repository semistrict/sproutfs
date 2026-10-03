package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// testRegionBytes is the region the disk tests use: room for 21 of their
// 3,000-byte items.
const testRegionBytes = 64 << 10

// testItemBytes is the envelope length most disk tests write.
const testItemBytes = 3000

// testItemsPerRegion is how many testItemBytes items, each with a header of
// 46 bytes and a table entry of 40 under keyOf's names, one region holds:
// (65,536 - 32) / (3,046 + 40).
const testItemsPerRegion = 21

// checkedFile wraps the cache's file the way FoundationDB's
// AsyncFileWriteChecker does: it keeps a copy of every byte written, and on
// each read tells a disk that lied from one that returned what was written. A
// read under a context carrying a diskWitness marks the witness when the disk
// lied, so a test can require that every item the cache refused was one the
// disk damaged, and never one the cache misread.
//
// It also logs the operations it forwards, holds reads at a gate, and can lose
// power around a region's table write.
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

func (f *checkedFile) WriteAt(ctx context.Context, source []byte, offset int64) (int, error) {
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
	f.log("truncate")
	return f.file().Truncate(ctx, size)
}

func (f *checkedFile) Sync(ctx context.Context) error {
	f.log("sync")
	return f.file().Sync(ctx)
}

func (f *checkedFile) Size(ctx context.Context) (int64, error) { return f.file().Size(ctx) }
func (f *checkedFile) Close() error                            { return f.file().Close() }

func (f *checkedFile) PunchHole(ctx context.Context, offset, length int64) error {
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
	runtime *sim.Runtime
	simDisk *sim.Disk
	file    *checkedFile
	budget  *testBudget
	disk    *cacheDisk
	model   map[diskKey][]byte
}

type diskFixtureConfig struct {
	seed       uint64
	regions    int64
	indexLimit int64
	disk       sim.DiskConfig
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
	return &diskFixture{runtime: runtime, simDisk: simDisk, file: file, budget: budget,
		disk: newCacheDisk(file, budget, testRegionBytes, limit, 1), model: make(map[diskKey][]byte)}, nil
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
// shares.
func payloadOf(key diskKey, length int) []byte {
	data := make([]byte, length)
	seed := fmt.Sprintf("%s/%d/%s/%d", key.Ref.VM, key.Ref.Sequence, key.Volume, key.Page)
	for at := range data {
		data[at] = seed[at%len(seed)] ^ byte(at*31)
	}
	return data
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
	data, outcome := f.disk.read(f.ctx(t), key)
	if outcome != diskHit || !bytes.Equal(data, f.model[key]) {
		t.Fatalf("reading %v found %d and %d bytes, want a hit of %d", key, outcome, len(data), len(f.model[key]))
	}
	f.disk.served(key)
}

// held reports which of keys the disk holds.
func (f *diskFixture) held(keys []diskKey) []bool {
	found := make([]bool, len(keys))
	for at, key := range keys {
		found[at] = f.disk.has(key)
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
			entry.each(func(uint16, diskLocation) { counted++ })
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
