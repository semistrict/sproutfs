package vmmigrate_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/testbacking"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

const (
	pageSize = checkpoint.PageSize2MiB
	// stateBytes is the simulated VMM state: the guest's current value and how
	// many stores it has made.
	stateBytes = 9
)

// errInjected is the failure a test makes one migration phase report.
var errInjected = errors.New("injected migration failure")

// cluster is one simulated deployment: one object store holding every VM's
// control record and checkpoints, and the network both hosts of a migration dial
// over.
type cluster struct {
	runtime  *sim.Runtime
	prefix   platform.ObjectPrefix
	objects  *countingStore
	sequence int
	cleanup  func(func())
	// knobs are the tunables every store, manager and pager of this cluster is
	// built with. They are the harness baseline unless a campaign drew a seed's
	// own set, which is what campaignKnobs does under the opt-in.
	knobs knobs.Knobs
}

// harnessKnobs is what these tests are written around: the deployment's
// tunables with the thirty-two page arena and single-page write-ahead this
// harness counts against. A campaign varies them from here.
func harnessKnobs() knobs.Knobs {
	k := knobs.Defaults()
	// One page per store: these tests count the pages a source holds against the
	// pages its guest wrote, which read-ahead and write-ahead would round up to
	// their runs.
	k.ResidentPages, k.LogicalPages, k.DirtyPages = 32, 64, 32
	k.ReadAheadPages, k.WriteAheadPages = 1, 1
	return k
}

func newCluster(t *testing.T) *cluster {
	return newSeededCluster(t, 1)
}

func newSeededCluster(t *testing.T, seed uint64) *cluster {
	t.Helper()
	config := sim.Config{Seed: seed,
		Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond, ConnectLatency: time.Microsecond},
		ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
			PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
			BytesPerSecond: 1 << 40}}
	return newConfiguredCluster(t, config, t.Cleanup)
}

// Scheduled workloads finish fixture cleanup before stopping their controller.
func newConfiguredCluster(t *testing.T, config sim.Config, cleanup func(func())) *cluster {
	t.Helper()
	runtime := sim.New(config)
	prefix, err := platform.NewObjectPrefix("sproutfs/")
	if err != nil {
		t.Fatal(err)
	}
	return &cluster{runtime: runtime, prefix: prefix,
		objects: &countingStore{ObjectStore: runtime.ObjectStore()}, cleanup: cleanup,
		knobs: harnessKnobs()}
}

// manager builds one host's volume manager over the shared store. Nothing
// publishes on its own, so a test decides exactly when a checkpoint happens.
func (c *cluster) manager(t *testing.T, name string) *volume.Manager {
	t.Helper()
	c.sequence++
	records, err := control.NewClient(control.Config{ObjectStore: c.objects, ObjectPrefix: c.prefix,
		Entropy: c.runtime.NewEntropy(fmt.Sprintf("control/%d", c.sequence))})
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: c.objects, ObjectPrefix: c.prefix,
		PartBytes: c.knobs.PartBytes, MaxIndexBytes: c.knobs.MaxIndexBytes,
		Concurrency: c.knobs.UploadConcurrency, MaxBuilders: c.knobs.MaxBuilders})
	if err != nil {
		t.Fatal(err)
	}
	m, err := volume.NewManager(volume.Config{Control: records, Store: store,
		MaxWriteBytes: c.knobs.MaxWriteBytes, MaxOpenVMs: c.knobs.MaxOpenVMs})
	if err != nil {
		t.Fatal(err)
	}
	c.cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

// countingStore counts what a migration reads from object storage, so a test
// can prove that the pause window touched nothing but the checkpoint index the
// destination's open reads.
type countingStore struct {
	platform.ObjectStore

	mu   sync.Mutex
	gets []objectRead
}

type objectRead struct {
	at  time.Time
	key string
}

// between reports the keys read inside one window, which is how a test asks what
// object storage a pause touched.
func (s *countingStore) between(from, to time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for _, read := range s.gets {
		if !read.at.Before(from) && !read.at.After(to) {
			keys = append(keys, read.key)
		}
	}
	return keys
}

func (s *countingStore) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	s.mu.Lock()
	s.gets = append(s.gets, objectRead{at: time.Now(), key: request.Key.String()})
	s.mu.Unlock()
	return s.ObjectStore.Get(ctx, request)
}

// arena is the simulated shared page store: one byte slice per resident slot.
type arena struct {
	mu    sync.Mutex
	slots [][]byte
}

func (a *arena) Read(_ context.Context, slot int, dst []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	copy(dst, a.slots[slot])
	return nil
}

func (a *arena) Write(_ context.Context, slot int, src []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.slots[slot] != nil {
		return fmt.Errorf("write into allocated slot %d", slot)
	}
	a.slots[slot] = bytes.Clone(src)
	return nil
}

func (a *arena) Release(_ context.Context, slot int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.slots[slot] = nil
	return nil
}

type mapped struct {
	slot     int
	writable bool
}

// mapping is one simulated process region. Every lookup takes the arena lock,
// because a guest storing into a page races the seal that write-protects it:
// the store and the writability check must be one step, exactly as the hardware
// makes them.
type mapping struct {
	arena *arena
	mu    sync.Mutex
	pages map[uint64]mapped
}

func newMapping(a *arena) *mapping { return &mapping{arena: a, pages: make(map[uint64]mapped)} }

func (m *mapping) Map(_ context.Context, page uint64, slot, count int, writable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		m.pages[page+uint64(i)] = mapped{slot + i, writable}
	}
	return nil
}

func (m *mapping) MapZero(_ context.Context, page uint64, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		m.pages[page+uint64(i)] = mapped{-1, false}
	}
	return nil
}

func (m *mapping) Protect(_ context.Context, page uint64, count int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		p, ok := m.pages[page+uint64(i)]
		if !ok {
			return fmt.Errorf("protect of unmapped page %d", page+uint64(i))
		}
		p.writable = false
		m.pages[page+uint64(i)] = p
	}
	return nil
}

func (m *mapping) Revoke(_ context.Context, page uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pages, page)
	return nil
}

func (m *mapping) Resolve(_ context.Context, page uint64, count int, writable bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range count {
		p, ok := m.pages[page+uint64(i)]
		if !ok || p.writable != writable {
			return errors.New("invalid resolution")
		}
	}
	return nil
}

// store writes one page's bytes the way a guest does: it takes the mapping lock,
// and only a mapping that is writable right now accepts the store. A page a seal
// has write-protected reports false, which is the trap the caller answers with a
// write fault.
func (m *mapping) store(page uint64, value byte) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[page]
	if !ok || !p.writable || p.slot < 0 {
		return false
	}
	m.arena.mu.Lock()
	defer m.arena.mu.Unlock()
	slot := m.arena.slots[p.slot]
	for i := range slot {
		slot[i] = value
	}
	return true
}

// pager is one host's shared page store and its pager Host.
type pager struct {
	host    *vmmemory.Host
	arena   *arena
	cleanup func(func())
	runtime *sim.Runtime
}

func newPager(t *testing.T, c *cluster, name string) *pager {
	t.Helper()
	disk := c.runtime.NewDisk(name+"-pager", sim.DiskConfig{})
	spill, err := disk.Open(t.Context(), "spill", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	c.cleanup(func() { _ = spill.Close() })
	a := &arena{slots: make([][]byte, c.knobs.ResidentPages)}
	host, err := vmmemory.New(t.Context(), testresource.New(), vmmemory.Config{
		PageSize:      pageSize,
		ResidentPages: c.knobs.ResidentPages, LogicalPages: c.knobs.LogicalPages,
		DirtyPages: c.knobs.DirtyPages, ReadAheadPages: c.knobs.ReadAheadPages,
		WriteAheadPages: c.knobs.WriteAheadPages, ConcurrentIO: c.knobs.ConcurrentIO}, a, spill)
	if err != nil {
		t.Fatal(err)
	}
	return &pager{host: host, arena: a, cleanup: c.cleanup, runtime: c.runtime}
}

// countingBacking counts what one region's volume was asked to do, which is how
// a test observes what a pause window cost. The admission the count hangs off
// is the shared wrapper's, so this harness and the host's order their regions
// against the scheduler the same way.
type countingBacking struct {
	*testbacking.Admitting
	loads, verifies atomic.Int64
}

func newCountingBacking(backing vmmemory.Backing, runtime *sim.Runtime, task string) *countingBacking {
	counted := &countingBacking{Admitting: testbacking.New(backing, runtime, task)}
	counted.Admitted = func(call string) {
		switch call {
		case testbacking.Load:
			counted.loads.Add(1)
		case testbacking.Verify:
			counted.verifies.Add(1)
		}
	}
	return counted
}

// machine is the simulated VMM process of one VM: one region per volume, a
// guest that stores into them, and the migration Runtime the coordinator drives.
type machine struct {
	t        *testing.T
	pager    *pager
	regions  map[string]*vmmemory.Region
	mappings map[string]*mapping
	backings map[string]*countingBacking
	names    []string
	pages    map[string]int

	// model is what the guest has stored into every page of every volume, which
	// is what the destination must read back byte for byte.
	modelMu sync.Mutex
	model   map[string][]byte
	writes  int64
	value   byte

	guestMu    sync.Mutex
	guestStop  chan struct{}
	guestDone  chan struct{}
	running    bool
	workingSet int

	failStop error
	// onStopped runs once the guest has stopped and its state is captured, which
	// is where a test fails the phases that come after the pause has already
	// begun.
	onStopped func()
	// stopLoads and stopVerifies are what the regions' volumes had been asked
	// for when the pause began, so a test can attribute the work of the pause
	// window exactly.
	stopLoads, stopVerifies int64
	closed                  bool
}

// ctx is the context the guest's own faults run under: the pager's fault paths
// carry buggified sites and probes of their own, and a fault driven from a bare
// test context reaches none of them.
func (m *machine) ctx() context.Context {
	return sim.WithRuntime(m.t.Context(), m.pager.runtime)
}

// volumeWork reports how many loads and authority checks this machine's regions
// have sent to their volumes.
func (m *machine) volumeWork() (loads, verifies int64) {
	for _, name := range m.names {
		loads += m.backings[name].loads.Load()
		verifies += m.backings[name].verifies.Load()
	}
	return loads, verifies
}

// regionKind is what a volume of this harness's machines is to its guest: the
// one volume named ram0 is its RAM and everything else is a PMEM disk, which is
// the shape a real machine binds.
func regionKind(volume string) vmmemory.RegionKind {
	if volume == "ram0" {
		return vmmemory.Ram
	}
	return vmmemory.Pmem
}

// newMachine attaches one region per volume of vm through backing, which is the
// volume itself on a source and a peer-backed volume on a destination.
func newMachine(t *testing.T, p *pager, vm *volume.VM, backings map[string]vmmemory.Backing, state []byte) (*machine, error) {
	m := &machine{t: t, pager: p, regions: map[string]*vmmemory.Region{}, mappings: map[string]*mapping{},
		backings: map[string]*countingBacking{}, pages: map[string]int{}, model: map[string][]byte{}}
	for _, v := range vm.Volumes() {
		name := v.Name()
		var backing vmmemory.Backing = v
		if supplied, ok := backings[name]; ok {
			backing = supplied
		}
		counted := newCountingBacking(backing, p.runtime, vm.ID()+"/"+name)
		mp := newMapping(p.arena)
		region, err := p.host.Attach(sim.WithRuntime(t.Context(), p.runtime),
			vmmemory.RegionBacking{Kind: regionKind(name), Backing: counted}, mp)
		if err != nil {
			return nil, err
		}
		m.names = append(m.names, name)
		m.regions[name] = region
		m.mappings[name] = mp
		m.backings[name] = counted
		m.pages[name] = int(v.Size() / pageSize)
		m.model[name] = make([]byte, v.Size())
	}
	if len(state) == stateBytes {
		// The VMM state carries the guest's own counters, so a destination that
		// restored it continues exactly where the source stopped.
		m.value = state[0]
		m.writes = int64(binary.LittleEndian.Uint64(state[1:]))
	} else if len(state) != 0 {
		return nil, fmt.Errorf("restored %d state bytes, want %d", len(state), stateBytes)
	}
	p.cleanup(func() { m.close() })
	return m, nil
}

// partialMachine is a machine that maps every region but one, which is what a
// supervisor misconfigured for the VM it received starts: whatever state it
// restored, it is not that VM, and the region it lacks would fault from nowhere.
type partialMachine struct {
	*machine
	missing string
	closed  atomic.Bool
}

func (p *partialMachine) Regions() map[string]*vmmemory.Region {
	regions := p.machine.Regions()
	delete(regions, p.missing)
	return regions
}

func (p *partialMachine) Close() error {
	p.closed.Store(true)
	return p.machine.Close()
}

func (m *machine) Regions() map[string]*vmmemory.Region {
	result := make(map[string]*vmmemory.Region, len(m.regions))
	for name, region := range m.regions {
		result[name] = region
	}
	return result
}

// checkpoint is this machine's interval checkpoint: the guest pauses, every
// region seals, the guest resumes, and the checkpoint publishes the sealed
// pages. It is the only thing that makes a running VM's memory durable.
func (m *machine) checkpoint(ctx context.Context, vm *volume.VM) error {
	ckpt, err := host.Capture(ctx, vm, m, nil)
	if err != nil {
		return err
	}
	return ckpt.Wait(ctx)
}

// Prepare is the capture's pause: the guest stops storing, its state is
// captured, and every region seals the pages the checkpoint will publish.
func (m *machine) Prepare(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
	state, err := m.Stop(ctx)
	if err != nil {
		return nil, nil, err
	}
	sources := make(map[string]volume.DirtySource, len(m.regions))
	for _, name := range m.names {
		if err := m.regions[name].Seal(ctx); err != nil {
			return nil, nil, err
		}
		sources[name] = m.regions[name].Checkpoint()
	}
	return state, sources, nil
}

// Stop is the migration's pause: the guest stops storing and its state is
// captured. Nothing is sealed and nothing is uploaded — the pages this machine
// keeps are what the destination fetches.
func (m *machine) Stop(ctx context.Context) ([]byte, error) {
	m.pause()
	// The pause begins here, so this is what the work of the pause window is
	// measured against.
	m.stopLoads, m.stopVerifies = m.volumeWork()
	if m.failStop != nil {
		return nil, m.failStop
	}
	if m.onStopped != nil {
		m.onStopped()
	}
	m.modelMu.Lock()
	defer m.modelMu.Unlock()
	state := make([]byte, stateBytes)
	state[0] = m.value
	binary.LittleEndian.PutUint64(state[1:], uint64(m.writes))
	return state, nil
}

// Resume restarts the vCPUs, which for this machine means letting the guest that
// was running before the pause store again.
func (m *machine) Resume(context.Context) error {
	m.guestMu.Lock()
	working := m.workingSet
	m.running = true
	m.guestMu.Unlock()
	if working > 0 {
		m.start(working)
	}
	return nil
}

func (m *machine) Release(ctx context.Context) error {
	for _, name := range m.names {
		if err := m.regions[name].Unseal(ctx); err != nil {
			return err
		}
	}
	return m.Resume(ctx)
}

func (m *machine) Close() error {
	m.close()
	return nil
}

// Wait completes host.Machine, which a capture of this fake is driven through.
// This process has no life of its own to end: only its caller stops it.
func (m *machine) Wait(ctx context.Context) error { <-ctx.Done(); return context.Cause(ctx) }

func (m *machine) close() {
	m.pause()
	if m.closed {
		return
	}
	m.closed = true
	for _, name := range m.names {
		clear(m.mappings[name].pages)
		if err := m.regions[name].Detach(context.Background()); err != nil {
			m.t.Error(err)
		}
	}
}

// start runs the guest: it stores into a fixed working set of every region,
// round after round, until pause stops it. That is the load a pre-copy has to
// converge against.
func (m *machine) start(workingSet int) {
	m.guestMu.Lock()
	defer m.guestMu.Unlock()
	if m.guestStop != nil {
		return
	}
	m.running, m.workingSet = true, workingSet
	stop, done := make(chan struct{}), make(chan struct{})
	m.guestStop, m.guestDone = stop, done
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, name := range m.names {
				count := min(workingSet, m.pages[name])
				for page := range uint64(count) {
					select {
					case <-stop:
						return
					default:
					}
					m.write(name, page)
				}
			}
		}
	}()
}

// write stores one page the way a guest does: it stores through the mapping
// when the mapping is writable, and takes a write fault when a seal took that
// access away. The model is updated under the mapping lock the store took, so a
// checkpoint can never contain bytes the model does not.
func (m *machine) write(name string, page uint64) {
	mp := m.mappings[name]
	m.modelMu.Lock()
	m.value++
	if m.value == 0 {
		m.value = 1
	}
	value := m.value
	m.modelMu.Unlock()
	for range 8 {
		if m.storeModel(name, mp, page, value) {
			return
		}
		if err := m.regions[name].Fault(m.ctx(), page, true); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			m.t.Errorf("guest store fault on %s page %d: %v", name, page, err)
			return
		}
	}
	m.t.Errorf("guest store on %s page %d never resolved", name, page)
}

func (m *machine) storeModel(name string, mp *mapping, page uint64, value byte) bool {
	m.modelMu.Lock()
	defer m.modelMu.Unlock()
	if !mp.store(page, value) {
		return false
	}
	for i := range pageSize {
		m.model[name][int(page)*pageSize+i] = value
	}
	m.writes++
	return true
}

// pause stops the guest and waits for its last store to finish, which is what
// makes the model exact.
func (m *machine) pause() {
	m.guestMu.Lock()
	stop, done := m.guestStop, m.guestDone
	m.guestStop, m.guestDone = nil, nil
	m.running = false
	m.guestMu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// storedMore waits for the guest to make one more store than it had, which is
// how a test proves the guest is running rather than merely not stopped.
func (m *machine) storedMore(t *testing.T, than int64) {
	t.Helper()
	for m.stored() <= than {
		runtime.Gosched()
		if err := t.Context().Err(); err != nil {
			t.Fatalf("the guest stored nothing more than %d: %v", than, err)
		}
	}
}

func (m *machine) stored() int64 {
	m.modelMu.Lock()
	defer m.modelMu.Unlock()
	return m.writes
}

// adopt takes the bytes a migration handed this machine as its own model, which
// is what a destination inherits: memory it did not write itself.
func (m *machine) adopt(model map[string][]byte) {
	m.modelMu.Lock()
	defer m.modelMu.Unlock()
	for name, data := range model {
		m.model[name] = bytes.Clone(data)
	}
}

func (m *machine) snapshot() map[string][]byte {
	m.modelMu.Lock()
	defer m.modelMu.Unlock()
	result := make(map[string][]byte, len(m.model))
	for name, data := range m.model {
		result[name] = bytes.Clone(data)
	}
	return result
}

// read faults one page in for reading and returns its bytes, which is what a
// destination's guest would see.
func (m *machine) read(ctx context.Context, name string, page uint64) ([]byte, error) {
	mp := m.mappings[name]
	mp.mu.Lock()
	p, ok := mp.pages[page]
	mp.mu.Unlock()
	if !ok {
		if err := m.regions[name].Fault(ctx, page, false); err != nil {
			return nil, err
		}
		mp.mu.Lock()
		p, ok = mp.pages[page]
		mp.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("%s page %d unmapped after a fault", name, page)
		}
	}
	if p.slot < 0 {
		return make([]byte, pageSize), nil
	}
	mp.arena.mu.Lock()
	defer mp.arena.mu.Unlock()
	return bytes.Clone(mp.arena.slots[p.slot]), nil
}

// verify reads every page of every region and requires it to equal the model
// the source's guest left behind.
func (m *machine) verify(ctx context.Context, model map[string][]byte) error {
	for _, name := range m.names {
		want, found := model[name]
		if !found {
			return fmt.Errorf("model has no volume %s", name)
		}
		for page := range uint64(m.pages[name]) {
			got, err := m.read(ctx, name, page)
			if err != nil {
				return fmt.Errorf("%s page %d: %w", name, page, err)
			}
			if !bytes.Equal(got, want[int(page)*pageSize:(int(page)+1)*pageSize]) {
				return fmt.Errorf("%s page %d: got %d..., want %d...", name, page, got[0], want[int(page)*pageSize])
			}
		}
	}
	return nil
}

// residentPages is how many pages this machine's regions hold, which is exactly
// what a destination must fetch from the peer rather than from its volume.
func (m *machine) residentPages() int {
	total := 0
	for _, name := range m.names {
		resident, err := m.regions[name].Resident()
		if err != nil {
			m.t.Fatalf("listing what %s holds: %v", name, err)
		}
		total += len(resident)
	}
	return total
}

// vmSpec is the VM every migration test runs: one RAM volume and one PMEM
// volume, so a migration has to name and move more than one region.
var vmSpec = []volume.VolumeSpec{{Name: "ram0", Size: 8 * pageSize, PageSize: checkpoint.PageSize2MiB}, {Name: "disk", Size: 4 * pageSize, PageSize: checkpoint.PageSize2MiB}}

// vmSpecPages is every page of that VM. One hop can dirty all of them, so it is
// the floor under any budget a campaign draws.
const vmSpecPages = 8 + 4

// classify sorts observed object reads into the log's control record, the
// checkpoint index objects — which hold their roots — and the parts that hold a
// VM's actual bytes. Only the last of those would put object storage on a
// migration's critical path.
func classify(keys []string) (records, indexes, pages int) {
	for _, key := range keys {
		switch {
		case strings.Contains(key, control.RecordPrefix):
			records++
		case strings.HasSuffix(key, "/index"):
			indexes++
		default:
			pages++
		}
	}
	return records, indexes, pages
}

// dialer dials the page source over the simulated network from one host.
func (c *cluster) dialer(from platform.Address) vmmigrate.Dialer {
	return func(ctx context.Context, peer platform.Address) (platform.Conn, error) {
		return c.runtime.Network().Dial(ctx, from, peer)
	}
}
