//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/adapters"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/resource"
	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// The measured sandbox: a 2 GiB guest with an 8 GiB PMEM root on a pager whose
// resident pages are a third of the guest's RAM plus root.
const (
	benchRAMBytes      = 2 << 30
	benchPmemBytes     = 8 << 30
	benchVCPUs         = 4
	benchResidentBytes = 3 << 30
	// benchMaxWriteBytes is the volumes' write limit and so the pager's flush
	// batch, and benchReadAheadBytes the window one read fault loads. Both are
	// sizes, so the pager's page changes neither.
	benchMaxWriteBytes  = vmmemory.PageSize
	benchReadAheadBytes = vmmemory.PageSize
	// benchMemoryBytes is shared by resident guest pages and decoded objects.
	benchMemoryBytes = 4 << 30
	// Both guests run with transparent huge pages off. Guest RAM here is host
	// pages served on demand, and khugepaged collapsing a 2 MiB range copies 512
	// of them through the fault path with preemption disabled, which soft-locks
	// the guest under any real build. The baseline carries the same setting so
	// the two differ only in where their memory and their root come from.
	benchBootArgs      = guestPmemBootArgs + " transparent_hugepage=never"
	benchPlainBootArgs = guestConsoleArgs + " reboot=k panic=1 init=/init root=/dev/vda rw transparent_hugepage=never"
	// benchMaxGuests sizes logical admission. Attaching a region admits metadata
	// for every one of its pages, so the bound is the largest number of guests
	// any scenario holds open at once, which is the twenty forks plus the
	// template and the siblings around them.
	benchMaxGuests = 26
)

// The workloads, shared verbatim by the managed run and the baseline so the
// ratio between them is a property of the storage and nothing else.
//
// The build is a real cold cargo build: the image ships the vendored ripgrep
// workspace with `grep-matcher`'s dependency graph already compiled and the
// crate itself cleaned, so what the guest builds is that crate's library and
// its three test targets, and what the fan-out then runs is those tests, from a
// target directory the forks inherit through the checkpoint.
//
// The whole vendored workspace is not a unit this host can measure. Its guests
// run under nested virtualization — Firecracker inside KVM inside the Lima VM —
// and are two orders of magnitude slower than the machine around them:
// compiling memchr and grep-matcher alone took 31 minutes of guest time in a
// trial run, and adding regex to that did not finish inside 85. The workspace's
// fifty-odd crates, cold and warm and then in twenty concurrent forks, is a day
// of compiling other people's code to measure a page fault.
// `SPROUTFS_BENCH_BUILD` and `SPROUTFS_BENCH_TEST` select other units,
// including much larger ones on a host that can afford them; the configuration
// record names whichever commands the run actually used.
const (
	// The lockfile is frozen so the install is the work of linking a store into
	// a project, not a resolution the guest has no network for.
	workloadInstall      = "cd /opt/app && pnpm install --offline --frozen-lockfile --reporter=append-only"
	defaultWorkloadBuild = "cd /opt/rust && cargo build --offline --all-targets -p grep-matcher"
	defaultWorkloadTest  = "cd /opt/rust && cargo test --offline -p grep-matcher"
	workloadGrep         = "cd /opt/repo && git grep -c fn | wc -l"
	workloadCat          = "cd /opt/repo && find . -type f | xargs cat > /dev/null"
	workloadTrue         = "true"
	// The recorded shape of the two concurrent scenarios. Both have environment
	// overrides for a quick run, and both record what they actually used.
	defaultForks        = 20
	defaultSteadyPeriod = 5 * time.Minute
	steadyGuests        = 8
)

func workloadBuild() string { return cmpOr(os.Getenv("SPROUTFS_BENCH_BUILD"), defaultWorkloadBuild) }
func workloadTest() string  { return cmpOr(os.Getenv("SPROUTFS_BENCH_TEST"), defaultWorkloadTest) }

// Bounds. These are deliberately generous on the first commit; tighten them
// from the numbers a recorded run writes to docs/measurements, keeping about a
// quarter of headroom over what was observed.
const (
	boundInstallRatio    = 2.0
	boundBuildRatio      = 2.0
	boundWarmRestore     = 500 * time.Millisecond
	boundForkFirstOutput = 2 * time.Second
)

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

// benchRecord is one measured scenario. Every number the documentation cites
// comes from one of these, and the bounds this test asserts are checked against
// the same fields, so the table and the regression test cannot drift apart.
type benchRecord struct {
	Scenario string          `json:"scenario"`
	Kind     string          `json:"kind"` // "sproutfs" or "baseline"
	WallNS   int64           `json:"wall_ns"`
	Guest    *guestTiming    `json:"guest,omitempty"`
	Memory   *memoryDelta    `json:"memory,omitempty"`
	Volumes  *volumeDelta    `json:"volumes,omitempty"`
	Objects  *objectCounters `json:"objects,omitempty"`
	Cache    *cacheDelta     `json:"cache,omitempty"`
	Extra    map[string]any  `json:"extra,omitempty"`
}

// cacheDelta is what one scenario asked of the host's shared page cache. A
// host serves every fork of one template from it, so its hit rate is what
// decides whether a cold read reaches object storage at all.
type cacheDelta struct {
	Hits           uint64 `json:"hits"`
	Misses         uint64 `json:"misses"`
	CoalescedLoads uint64 `json:"coalesced_loads"`
	Evictions      uint64 `json:"evictions"`
	ResidentBytes  int64  `json:"resident_bytes"`
	PeakLoads      int    `json:"peak_loads"`
}

// memoryDelta is what one scenario cost the pager. The counters are deltas; the
// page totals are the state left behind.
type memoryDelta struct {
	Faults            uint64 `json:"faults"`
	CopyOnWrites      uint64 `json:"copy_on_writes"`
	Evictions         uint64 `json:"evictions"`
	Spills            uint64 `json:"spills"`
	SpillRefaults     uint64 `json:"spill_refaults"`
	SpillWrites       uint64 `json:"spill_writes"`
	Loads             uint64 `json:"loads"`
	LoadedPages       uint64 `json:"loaded_pages"`
	IdentityHits      uint64 `json:"identity_hits"`
	Mappings          uint64 `json:"mappings"`
	MappedPages       uint64 `json:"mapped_pages"`
	MappingRuns       uint64 `json:"mapping_runs"`
	Revocations       uint64 `json:"revocations"`
	RevokedPages      uint64 `json:"revoked_pages"`
	Protections       uint64 `json:"protections"`
	ProtectedPages    uint64 `json:"protected_pages"`
	CheckpointPages   uint64 `json:"checkpoint_pages"`
	SpillWriteBytes   uint64 `json:"spill_write_bytes"`
	ResidentPages     int    `json:"resident_pages"`
	DirtyPages        int    `json:"dirty_pages"`
	LogicalPages      int    `json:"logical_pages"`
	PeakResidentPages int    `json:"peak_resident_pages"`
	PeakDirtyPages    int    `json:"peak_dirty_pages"`
	// WriteAheadPages counts the pages stores into fresh memory mapped
	// writable beyond the one each faulted on, and WriteAheadZeroPages those of
	// them the scenario's flushes and checkpoints wrote back still all zero.
	WriteAheadPages     uint64 `json:"write_ahead_pages"`
	WriteAheadZeroPages uint64 `json:"write_ahead_zero_pages"`
	// The pager's own latency histograms over this scenario. Fault decomposes
	// Faults exactly; the rest are the spans one fault contains, so a scenario
	// whose faults are slow can be attributed without another run.
	FaultQueue *latencyDelta `json:"fault_queue,omitempty"`
	Fault      *latencyDelta `json:"fault,omitempty"`
	Mapping    *latencyDelta `json:"mapping,omitempty"`
	Revoke     *latencyDelta `json:"revoke,omitempty"`
	Protect    *latencyDelta `json:"protect,omitempty"`
	Resolve    *latencyDelta `json:"resolve,omitempty"`
	Load       *latencyDelta `json:"load,omitempty"`
	Seal       *latencyDelta `json:"seal,omitempty"`
}

// latencyDelta is one histogram over one scenario. Buckets are the fixed
// log-scale ones the pager exports, so records of different runs are directly
// comparable; the boundaries are named once here rather than in every record.
type latencyDelta struct {
	Count   uint64 `json:"count"`
	TotalNS uint64 `json:"total_ns"`
	MeanNS  uint64 `json:"mean_ns"`
	// MaxToDateNS is the largest single observation the host has ever made, not
	// this scenario's: a maximum cannot be subtracted.
	MaxToDateNS uint64                          `json:"max_to_date_ns"`
	MedianNS    uint64                          `json:"median_upper_ns"`
	P99NS       uint64                          `json:"p99_upper_ns"`
	Buckets     [vmmemory.LatencyBuckets]uint64 `json:"buckets"`
	BucketUpNS  []uint64                        `json:"bucket_upper_ns"`
}

func latencyBetween(before, after vmmemory.Latency) *latencyDelta {
	d := &latencyDelta{TotalNS: after.TotalNS - before.TotalNS, MaxToDateNS: after.MaxNS}
	for i := range d.Buckets {
		d.Buckets[i] = after.Buckets[i] - before.Buckets[i]
		d.Count += d.Buckets[i]
		d.BucketUpNS = append(d.BucketUpNS, vmmemory.LatencyBucketUpperNS(i))
	}
	if d.Count == 0 {
		return d
	}
	d.MeanNS = d.TotalNS / d.Count
	d.MedianNS, d.P99NS = d.quantileUpperNS(0.5), d.quantileUpperNS(0.99)
	return d
}

// quantileUpperNS is the upper bound of the bucket the quantile falls in. A
// histogram cannot report a quantile more precisely than its buckets, and
// saying so in the field name keeps the documentation honest.
func (d *latencyDelta) quantileUpperNS(q float64) uint64 {
	want := uint64(float64(d.Count) * q)
	var seen uint64
	for i, count := range d.Buckets {
		seen += count
		if seen > want {
			return vmmemory.LatencyBucketUpperNS(i)
		}
	}
	return vmmemory.LatencyBucketUpperNS(vmmemory.LatencyBuckets - 1)
}

func memoryBetween(before, after vmmemory.Stats) *memoryDelta {
	return &memoryDelta{
		Faults: after.Faults - before.Faults, CopyOnWrites: after.CopyOnWrites - before.CopyOnWrites,
		Evictions: after.Evictions - before.Evictions, Spills: after.Spills - before.Spills,
		SpillRefaults: after.SpillRefaults - before.SpillRefaults, SpillWrites: after.SpillWrites - before.SpillWrites,
		Loads: after.Loads - before.Loads, LoadedPages: after.LoadedPages - before.LoadedPages,
		IdentityHits: after.IdentityHits - before.IdentityHits, Mappings: after.Mappings - before.Mappings,
		MappedPages: after.MappedPages - before.MappedPages, MappingRuns: after.MappingRuns - before.MappingRuns,
		Revocations: after.Revocations - before.Revocations, RevokedPages: after.RevokedPages - before.RevokedPages,
		Protections: after.Protections - before.Protections, ProtectedPages: after.ProtectedPages - before.ProtectedPages,
		CheckpointPages: after.CheckpointPages - before.CheckpointPages,
		SpillWriteBytes: after.SpillWriteBytes - before.SpillWriteBytes,
		ResidentPages:   after.ResidentPages, DirtyPages: after.DirtyPages, LogicalPages: after.LogicalPages,
		PeakResidentPages: after.PeakResidentPages, PeakDirtyPages: after.PeakDirtyPages,
		WriteAheadPages:     after.WriteAheadPages - before.WriteAheadPages,
		WriteAheadZeroPages: after.WriteAheadZeroPages - before.WriteAheadZeroPages,
		FaultQueue:          latencyBetween(before.FaultQueue, after.FaultQueue),
		Fault:               latencyBetween(before.Fault, after.Fault),
		Mapping:             latencyBetween(before.Mapping, after.Mapping),
		Revoke:              latencyBetween(before.Revoke, after.Revoke),
		Protect:             latencyBetween(before.Protect, after.Protect),
		Resolve:             latencyBetween(before.Resolve, after.Resolve),
		Load:                latencyBetween(before.Load, after.Load),
		Seal:                latencyBetween(before.Seal, after.Seal),
	}
}

// volumeDelta is what one scenario left unpublished, which is what losing the
// host would cost.
type volumeDelta struct {
	OpenVMs    int    `json:"open_vms"`
	DirtyBytes uint64 `json:"dirty_bytes"`
	MaxOpenVMs int    `json:"max_open_vms"`
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type benchmark struct {
	t              *testing.T
	work           string
	scratch        string
	managedScratch *vmmachine.Scratch
	objects        *countingObjectStore
	cache          *checkpoint.Cache
	store          *checkpoint.Store
	manager        *volume.Manager
	host           *vmmemory.Host
	arena          *vmmemory.LinuxArena
	// pageSize is the pager's page, and readAheadPages the fixed byte budget
	// above in those pages.
	pageSize, readAheadPages int
	scenarios                scenarioSet

	// vmm is the machine whose address space the sequential scenarios run in,
	// so every one of them can report how many mappings it holds.
	vmm *vmmachine.Process

	binary, seccomp, kernel, imagePath string
	residentPages, logicalPages        int
	dirtyPages                         int
	objectStoreKind                    string
	objectLatency                      objectLatency

	mu      sync.Mutex
	records []benchRecord
	// lastGuest is the previous scenario's guest record per kind, which is what
	// the harness checks a new one against.
	lastGuest map[string]guestTiming
	output    string
}

// sample is everything a scenario is measured against, taken at one moment.
type sample struct {
	at      time.Time
	memory  vmmemory.Stats
	objects objectCounters
	cache   checkpoint.CacheStats
	volumes volume.Stats
}

func (b *benchmark) sample(ctx context.Context) sample {
	b.t.Helper()
	stats, err := b.host.Stats(ctx)
	if err != nil {
		b.t.Fatal(err)
	}
	return sample{at: time.Now(), memory: stats, objects: b.objects.counters(),
		cache: b.cache.Stats(), volumes: b.manager.Stats()}
}

// record folds one scenario's measurements into a record and appends it, and
// rewrites the output file so an interrupted run still leaves what it proved.
func (b *benchmark) record(ctx context.Context, name, kind string, start sample, guest *guestTiming, extra map[string]any) benchRecord {
	b.t.Helper()
	end := b.sample(ctx)
	rec := benchRecord{Scenario: name, Kind: kind, WallNS: end.at.Sub(start.at).Nanoseconds(), Guest: guest, Extra: extra}
	if kind == "sproutfs" && b.vmm != nil {
		if rec.Extra == nil {
			rec.Extra = map[string]any{}
		}
		rec.Extra["vmm_mappings"] = countMappings(b.vmm.PID())
	}
	if kind == "sproutfs" {
		rec.Memory = memoryBetween(start.memory, end.memory)
		objects := end.objects.sub(start.objects)
		rec.Objects = &objects
		rec.Cache = &cacheDelta{Hits: end.cache.Hits - start.cache.Hits,
			Misses:         end.cache.Misses - start.cache.Misses,
			CoalescedLoads: end.cache.CoalescedLoads - start.cache.CoalescedLoads,
			Evictions:      end.cache.Evictions - start.cache.Evictions,
			ResidentBytes:  end.cache.ResidentBytes,
			PeakLoads:      end.cache.PeakLoads}
		rec.Volumes = &volumeDelta{OpenVMs: end.volumes.OpenVMs,
			DirtyBytes: end.volumes.DirtyBytes, MaxOpenVMs: end.volumes.MaxOpenVMs}
	}
	b.appendRecord(rec)
	return rec
}

func (b *benchmark) writeOutput(records []benchRecord) {
	if b.output == "" {
		return
	}
	raw, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(b.output, append(raw, '\n'), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// find returns the record one bound is asserted against.
func (b *benchmark) find(scenario, kind string) benchRecord {
	b.t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, rec := range b.records {
		if rec.Scenario == scenario && rec.Kind == kind {
			return rec
		}
	}
	b.t.Fatalf("no %s record for %q", kind, scenario)
	return benchRecord{}
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required by the workload benchmark", name)
	}
	return value
}

func newBenchmark(ctx context.Context, t *testing.T) *benchmark {
	t.Helper()
	work := os.Getenv("SPROUTFS_BENCH_WORK")
	if work == "" {
		work = t.TempDir()
	} else if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &benchmark{t: t, work: work, pageSize: pagerPageBytes(t), scenarios: selectedScenarios(t),
		binary:    requireEnv(t, "SPROUTFS_FIRECRACKER"),
		seccomp:   requireEnv(t, "SPROUTFS_FIRECRACKER_SECCOMP"),
		kernel:    requireEnv(t, "SPROUTFS_FIRECRACKER_KERNEL"),
		imagePath: requireEnv(t, "SPROUTFS_BENCH_IMAGE"),
		output:    os.Getenv("SPROUTFS_BENCH_OUTPUT")}
	b.scratch = filepath.Join(work, "scratch")
	if err := os.MkdirAll(b.scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	inner, kind, latency := newBenchObjectStore(t, work)
	b.objects = &countingObjectStore{inner: inner}
	b.objectStoreKind, b.objectLatency = kind, latency

	client, err := control.NewClient(control.Config{ObjectStore: b.objects})
	if err != nil {
		t.Fatal(err)
	}
	// A host shares one page cache across every VM and fork it serves; without
	// it every read of any part of a page fetches that whole object again.
	memoryBytes := int64(benchMemoryBytes)
	if value := os.Getenv("SPROUTFS_BENCH_MEMORY_MIB"); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed <= 0 || int64(parsed) > (1<<63-1)>>20 {
			t.Fatal("invalid SPROUTFS_BENCH_MEMORY_MIB")
		}
		memoryBytes = int64(parsed) << 20
	}
	// One local RAM budget is shared by decoded objects and guest pages.
	resources, err := resource.New(memoryBytes)
	if err != nil {
		t.Fatal(err)
	}
	b.cache, err = checkpoint.NewCache(resources, checkpoint.CacheConfig{MaxConcurrentLoads: 32})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.cache.Close)
	b.store, err = checkpoint.NewStore(checkpoint.Config{ObjectStore: b.objects, Cache: b.cache, Concurrency: 16})
	if err != nil {
		t.Fatal(err)
	}
	b.manager, err = volume.NewManager(volume.Config{Control: client, Store: b.store,
		MaxWriteBytes: benchMaxWriteBytes, MaxOpenVMs: 256})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.manager.Close(context.Background()) })

	b.residentPages = benchResidentBytes / b.pageSize
	// The dirty budget is not the resident budget. A private RAM page becomes
	// clean only through a coordinated capture, so what a running guest has
	// dirtied stays dirty for as long as it runs; the resident budget bounds
	// physical pages and the spill file absorbs the rest. A host therefore has
	// to provision dirty capacity for the RAM of every guest it runs at once,
	// which is what this derives.
	b.dirtyPages = benchMaxGuests * (benchRAMBytes / b.pageSize)
	b.logicalPages = benchMaxGuests * ((benchRAMBytes + benchPmemBytes) / b.pageSize)
	b.readAheadPages = benchReadAheadBytes / b.pageSize
	b.arena, err = vmmemory.NewLinuxArena(b.residentPages)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.arena.Close() })
	disk, err := adapters.NewDisk(work)
	if err != nil {
		t.Fatal(err)
	}
	spill, err := disk.Open(ctx, "spill", platform.OpenOptions{Create: true, Exclusive: true, Permissions: 0o600})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spill.Close() })
	b.host, err = vmmemory.New(ctx, resources, vmmemory.Config{ResidentPages: b.residentPages,
		LogicalPages: b.logicalPages, DirtyPages: b.dirtyPages,
		ReadAheadPages: b.readAheadPages, WriteAheadPages: benchWriteAheadPages()}, b.arena, spill)
	if err != nil {
		t.Fatal(err)
	}
	b.managedScratch, err = vmmachine.NewScratch(t.Context(), filepath.Join(b.scratch, "managed"), adapters.NewDisk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.managedScratch.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() {
		if err := b.host.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return b
}

// newBenchObjectStore selects the object storage this run measures against: a
// Google Cloud Storage endpoint when one is configured — the deployment's own
// store, or the emulator standing in for it — the simulated store by default,
// and the same model kept on disk when the host cannot hold the checkpoints of
// a multi-gigabyte workload in memory.
func newBenchObjectStore(t *testing.T, work string) (platform.ObjectStore, string, objectLatency) {
	t.Helper()
	latency := objectLatency{Head: 10 * time.Millisecond, Get: 10 * time.Millisecond,
		Put: 10 * time.Millisecond, Delete: 10 * time.Millisecond, List: 10 * time.Millisecond,
		BytesPerSecond: 200 << 20}
	if bucket := os.Getenv("SPROUTFS_GCS_BUCKET"); bucket != "" {
		endpoint := os.Getenv("SPROUTFS_GCS_ENDPOINT")
		store, closer, err := adapters.NewGCS(t.Context(), endpoint, bucket, os.Getenv("SPROUTFS_GCS_PREFIX"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := closer.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
		where := cmpOr(endpoint, "storage.googleapis.com")
		t.Logf("object store: Google Cloud Storage %s bucket %s", where, bucket)
		return store, "gcs:" + where, objectLatency{}
	}
	if directory := os.Getenv("SPROUTFS_BENCH_OBJECT_DIR"); directory != "" {
		store, err := newDiskObjectStore(directory, latency)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("object store: on-disk under %s, %s GET/PUT latency, %d MiB/s",
			directory, latency.Get, latency.BytesPerSecond>>20)
		return store, "disk", latency
	}
	runtime := sim.New(sim.Config{ObjectStore: sim.ObjectStoreConfig{
		HeadLatency: latency.Head, GetLatency: latency.Get, PutLatency: latency.Put,
		DeleteLatency: latency.Delete, ListLatency: latency.List, BytesPerSecond: latency.BytesPerSecond,
		MaxObjectSize: 64 << 20}})
	t.Logf("object store: simulated, %s GET/PUT latency, %d MiB/s", latency.Get, latency.BytesPerSecond>>20)
	return runtime.ObjectStore(), "sim", latency
}

func cmpOr[T comparable](values ...T) T {
	var zero T
	for _, value := range values {
		if value != zero {
			return value
		}
	}
	return zero
}

// bootArgs appends SPROUTFS_BENCH_BOOT_ARGS_EXTRA to a guest's kernel command
// line, which is how a diagnostic run adds initcall_debug or a larger log buffer
// without changing what a recorded run boots with.
func bootArgs(base string) string {
	if extra := strings.TrimSpace(os.Getenv("SPROUTFS_BENCH_BOOT_ARGS_EXTRA")); extra != "" {
		return base + " " + extra
	}
	return base
}

// machineConfig is the managed VM every scenario runs: the guest's RAM and root
// are volumes of one VM, and the pager is their only mutator.
func (b *benchmark) machineConfig(vm *volume.VM, restore []byte) vmmachine.Config {
	return vmmachine.Config{
		Binary: b.binary, SeccompFilter: b.seccomp, KernelPath: b.kernel, BootArgs: bootArgs(benchBootArgs),
		Scratch: b.managedScratch, Host: b.host, VM: vm,
		Pmem: []vmmachine.Pmem{{ID: "root", Root: true}}, VCPUs: benchGuestVCPUs(), RestoreState: restore,
		Connection: vmmemory.ConnectionConfig{QueuePages: min(16384, b.logicalPages), FaultWorkers: 16,
			CommandTimeout: 5 * time.Minute, VerifyInterval: 5 * time.Second},
	}
}

func (b *benchmark) createVM(ctx context.Context, id string) *volume.VM {
	b.t.Helper()
	vm, err := b.manager.Create(ctx, id, []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: benchRAMBytes},
		{Name: "root", Size: benchPmemBytes},
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return vm
}

// ingest writes the guest image into the template's root volume. Only the
// allocated extents of the sparse image are written, in whole page-sized
// batches; every hole is discarded, which costs one bounded record however
// large it is.
func (b *benchmark) ingest(ctx context.Context, vm *volume.VM) (time.Duration, uint64) {
	b.t.Helper()
	root := vm.Volume("root")
	file, err := os.Open(b.imagePath)
	if err != nil {
		b.t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		b.t.Fatal(err)
	}
	if uint64(info.Size()) != root.Size() {
		b.t.Fatalf("guest image is %d bytes, root volume is %d", info.Size(), root.Size())
	}
	pages := int(root.Size() / checkpoint.PageSize)
	data := make([]bool, pages)
	for offset := int64(0); offset < info.Size(); {
		start, err := file.Seek(offset, seekData)
		if errors.Is(err, syscall.ENXIO) {
			break
		}
		if err != nil {
			b.t.Fatal(err)
		}
		end, err := file.Seek(start, seekHole)
		if err != nil {
			b.t.Fatal(err)
		}
		for page := start / checkpoint.PageSize; page <= (end-1)/checkpoint.PageSize && int(page) < pages; page++ {
			data[page] = true
		}
		offset = end
	}
	started := time.Now()
	buffer := make([]byte, checkpoint.PageSize)
	var written uint64
	for page := 0; page < pages; page++ {
		if !data[page] {
			hole := page
			for hole < pages && !data[hole] {
				hole++
			}
			if err := root.Discard(ctx, uint64(page)*checkpoint.PageSize, uint64(hole-page)*checkpoint.PageSize); err != nil {
				b.t.Fatal(err)
			}
			page = hole - 1
			continue
		}
		if _, err := file.ReadAt(buffer, int64(page)*checkpoint.PageSize); err != nil {
			b.t.Fatal(err)
		}
		if err := root.WriteBatch(ctx, []volume.WriteExtent{{Offset: uint64(page) * checkpoint.PageSize, Data: buffer}}); err != nil {
			b.t.Fatal(err)
		}
		written += checkpoint.PageSize
	}
	if err := vm.Checkpoint(ctx); err != nil {
		b.t.Fatal(err)
	}
	return time.Since(started), written
}

// Linux sparse-file seeks. A file with no data past offset reports ENXIO.
const (
	seekData = 3
	seekHole = 4
)

// startTiming splits what a start actually cost, so a boot is explainable
// rather than a single number. VMMStartNS is everything `vmmachine.Start` does:
// launching the VMM, exchanging descriptors, admitting and populating every
// region, and the API round trip that proves the machine is up. ToReadyNS is
// what follows on the console. KernelNS and InitNS come from the guest's own
// monotonic clock, so the two of them together say how much of the wait was the
// guest at all; PreKernelNS is the remainder, which is the VMM's own setup
// before the kernel ran plus the console poll that observed readiness.
//
// A restore reports only VMMStartNS and ToReadyNS: it resumes a guest that
// booted long ago and runs no init.
type startTiming struct {
	VMMStartNS  int64 `json:"vmm_start_ns"`
	ToReadyNS   int64 `json:"to_ready_ns"`
	KernelNS    int64 `json:"kernel_ns,omitempty"`
	InitNS      int64 `json:"init_ns,omitempty"`
	PreKernelNS int64 `json:"pre_kernel_ns,omitempty"`
}

func (s startTiming) extra(hostSetup time.Duration) map[string]any {
	return map[string]any{"host_setup_ns": hostSetup.Nanoseconds(), "vmm_start_ns": s.VMMStartNS,
		"to_ready_ns": s.ToReadyNS, "kernel_ns": s.KernelNS, "init_ns": s.InitNS,
		"pre_kernel_ns": s.PreKernelNS}
}

// start launches one managed machine and waits for the guest to be ready. A
// restore returns paused, so it is released before the console is used.
func (b *benchmark) start(ctx context.Context, vm *volume.VM, restore []byte) (*vmmachine.Process, *console, startTiming) {
	b.t.Helper()
	launched := time.Now()
	p, err := vmmachine.Start(ctx, b.machineConfig(vm, restore))
	if err != nil {
		b.t.Fatalf("start %s: %v", vm.ID(), err)
	}
	timing := startTiming{VMMStartNS: time.Since(launched).Nanoseconds()}
	running := time.Now()
	c := newConsole(p)
	if len(restore) > 0 {
		if err := p.Release(ctx); err != nil {
			b.t.Fatalf("release %s: %v", vm.ID(), err)
		}
		// A restored guest resumes inside its command loop; the VMM's own restore
		// output is already on this console and is not the scenario's.
		if err := c.skipExisting(); err != nil {
			b.t.Fatal(err)
		}
		timing.ToReadyNS = time.Since(running).Nanoseconds()
		return p, c, timing
	}
	line, err := c.wait(ctx, "SPROUTFS_READY")
	if err != nil {
		b.t.Fatalf("boot %s: %v", vm.ID(), err)
	}
	timing.ToReadyNS = time.Since(running).Nanoseconds()
	b.splitBoot(&timing, line)
	return p, c, timing
}

// splitBoot charges the guest's own phases against the wall time the host
// measured. The guest reports the monotonic reading at which its init started
// and the one at which it was ready; that clock starts with the kernel, so the
// first is the kernel's whole boot.
func (b *benchmark) splitBoot(timing *startTiming, ready string) {
	b.t.Helper()
	entry, at, err := parseGuestReady(ready)
	if err != nil {
		b.t.Fatal(err)
	}
	timing.KernelNS, timing.InitNS = int64(entry), int64(at-entry)
	timing.PreKernelNS = timing.VMMStartNS + timing.ToReadyNS - timing.KernelNS - timing.InitNS
}

// capture runs a coordinated capture and reports what the guest's pause bought.
func (b *benchmark) capture(ctx context.Context, p *vmmachine.Process, vm *volume.VM) (*volume.Checkpoint, map[string]any) {
	b.t.Helper()
	before, err := b.host.Stats(ctx)
	if err != nil {
		b.t.Fatal(err)
	}
	// How many mappings the VMM's address space holds when the seal runs. A
	// range write-protect is applied to every registered mapping the range
	// covers, so the kernel walks them; the seal microbenchmark's guest has a
	// handful and a guest that has run a build has one per private page.
	vmas := countMappings(p.PID())
	paused := time.Now()
	var state []byte
	var prepared, resumed time.Time
	var atResume vmmemory.Stats
	ckpt, err := vm.Snapshot(ctx, func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		captured, sources, err := p.Prepare(ctx)
		if err != nil {
			return nil, nil, err
		}
		state, prepared = captured, time.Now()
		if err := p.Resume(ctx); err != nil {
			return nil, nil, err
		}
		resumed = time.Now()
		if atResume, err = b.host.Stats(ctx); err != nil {
			return nil, nil, err
		}
		return captured, sources, nil
	})
	if err != nil {
		b.t.Fatalf("capture: %v", err)
	}
	sealed := atResume.CheckpointPages - before.CheckpointPages
	published := time.Now()
	if err := ckpt.Wait(ctx); err != nil {
		b.t.Fatal(err)
	}
	pause := resumed.Sub(paused)
	// Prepare is one API call pair: Firecracker pauses its vCPUs, saves its device
	// and KVM state, and only then asks the pager for the memory checkpoint. The
	// pager times its own half, so what is left of Prepare is Firecracker's pause
	// and state save together, which is the number a fix would have to target if
	// the seal turns out not to be the cost.
	seals := int64(atResume.Seal.TotalNS - before.Seal.TotalNS)
	protects := int64(atResume.Protect.TotalNS - before.Protect.TotalNS)
	prepare := prepared.Sub(paused).Nanoseconds()
	extra := map[string]any{
		"sealed_pages":        sealed,
		"pause_ns":            pause.Nanoseconds(),
		"prepare_ns":          prepare,
		"seal_ns":             seals,
		"seal_calls":          atResume.Seal.Count - before.Seal.Count,
		"longest_seal_ns":     atResume.Seal.MaxNS,
		"protect_ns":          protects,
		"protect_commands":    atResume.Protect.Count - before.Protect.Count,
		"protected_pages":     atResume.ProtectedPages - before.ProtectedPages,
		"seal_bookkeeping_ns": seals - protects,
		"vmm_pause_save_ns":   prepare - seals,
		"resume_ns":           resumed.Sub(prepared).Nanoseconds(),
		"publish_ns":          time.Since(published).Nanoseconds(),
		"state_bytes":         len(state),
		"vmm_mappings":        vmas,
		"pause_ns_per_page":   0,
		"seal_ns_per_page":    0,
	}
	if sealed > 0 {
		extra["pause_ns_per_page"] = pause.Nanoseconds() / int64(sealed)
		extra["seal_ns_per_page"] = seals / int64(sealed)
	}
	return ckpt, extra
}

// ---------------------------------------------------------------------------
// The benchmark
// ---------------------------------------------------------------------------

func TestGuestWorkloadBenchmark(t *testing.T) {
	if os.Getenv("SPROUTFS_FIRECRACKER_BENCH") != "1" {
		t.Skip("run scripts/bench-guest-lima.sh")
	}
	ctx := t.Context()
	b := newBenchmark(ctx, t)
	t.Logf("pager: resident=%d pages (%d MiB) dirty=%d logical=%d read-ahead=%d page=%d scenarios=%v",
		b.residentPages, benchResidentBytes>>20, b.dirtyPages, b.logicalPages, b.readAheadPages,
		b.pageSize, b.scenarios.names())

	b.appendRecord(benchRecord{Scenario: "configuration", Kind: "config", Extra: b.configuration()})

	template := b.createVM(ctx, "template")
	t.Cleanup(func() { _ = template.Close(context.Background()) })
	start := b.sample(ctx)
	elapsed, written := b.ingest(ctx, template)
	b.record(ctx, "ingest-template", "sproutfs", start, nil,
		map[string]any{"ingest_ns": elapsed.Nanoseconds(), "written_bytes": written,
			"image_bytes": benchPmemBytes, "image_path": b.imagePath})

	// Scenario 1: cold boot from the template. Every guest workload up to the
	// capture runs in this machine.
	var source *vmmachine.Process
	var sourceConsole *console
	if b.scenarios["boot"] {
		stopProfile := b.profile("boot")
		start = b.sample(ctx)
		var phases startTiming
		source, sourceConsole, phases = b.start(ctx, template, nil)
		b.vmm = source
		b.record(ctx, "boot", "sproutfs", start, nil, phases.extra(0))
		stopProfile()
		b.inspect(ctx, sourceConsole, "sproutfs", template)
	}

	// Scenario 3: the offline pnpm install.
	if b.scenarios["pnpm-install"] {
		stopProfile := b.profile("pnpm-install")
		start = b.sample(ctx)
		timing := b.run(ctx, sourceConsole, workloadInstall)
		b.record(ctx, "pnpm-install", "sproutfs", start, &timing, nil)
		stopProfile()
		b.sync(ctx, sourceConsole)
	}

	// Scenario 4: a cold cargo build, then the same build as a warm no-op.
	if b.scenarios["cargo-build"] {
		start = b.sample(ctx)
		timing := b.run(ctx, sourceConsole, workloadBuild())
		b.record(ctx, "cargo-build-cold", "sproutfs", start, &timing, nil)
		b.sync(ctx, sourceConsole)
		start = b.sample(ctx)
		timing = b.run(ctx, sourceConsole, workloadBuild())
		b.record(ctx, "cargo-build-warm", "sproutfs", start, &timing, nil)
	}

	// Scenario 5: repository search and a full read of every file, cold then warm.
	if b.scenarios["repository"] {
		for _, phase := range []string{"cold", "warm"} {
			start = b.sample(ctx)
			timing := b.run(ctx, sourceConsole, workloadGrep)
			b.record(ctx, "git-grep-"+phase, "sproutfs", start, &timing, nil)
			start = b.sample(ctx)
			timing = b.run(ctx, sourceConsole, workloadCat)
			b.record(ctx, "read-tree-"+phase, "sproutfs", start, &timing, nil)
		}
		b.sync(ctx, sourceConsole)
	}

	// The capture whose checkpoint every restore and fork below starts from,
	// rebuilt as the pause a fork inherits: a published checkpoint with
	// nothing held back, which is what a template is.
	var ckpt *volume.Checkpoint
	var origin *forkOrigin
	if b.scenarios["capture"] {
		stopProfile := b.profile("capture")
		start = b.sample(ctx)
		var captureExtra map[string]any
		ckpt, captureExtra = b.capture(ctx, source, template)
		b.record(ctx, "capture", "sproutfs", start, nil, captureExtra)
		stopProfile()
		point, err := b.manager.Inherit(ctx, ckpt.Ref())
		if err != nil {
			t.Fatal(err)
		}
		origin = &forkOrigin{point: point, state: ckpt.State()}
	}
	if source != nil {
		b.vmm = nil
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Scenario 2, cold: a restore with no sibling holding the checkpoint's pages.
	if b.scenarios["restore-cold"] {
		b.restoreScenario(ctx, origin, "restore-cold", "no-sibling")
	}

	// Scenario 2, warm: a sibling that has already run the warm repository
	// search keeps the pages resident, so the restore inherits them.
	if b.scenarios["restore-warm"] {
		sibling, siblingVM, siblingConsole := b.fork(ctx, origin, "sibling")
		b.run(ctx, siblingConsole, workloadGrep)
		b.run(ctx, siblingConsole, workloadCat)
		b.restoreScenario(ctx, origin, "restore-warm", "sibling-warm")
		if err := sibling.Close(); err != nil {
			t.Fatal(err)
		}
		if err := siblingVM.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// Scenario 6: twenty forks of one checkpoint, each running the same test
	// build.
	if b.scenarios["fork-fanout"] {
		b.forkFanOut(ctx, origin)
	}

	// Scenario 7: the steady state.
	if b.scenarios["steady-state"] {
		b.steadyState(ctx, origin)
	}

	// Baseline: the same guest, kernel and workloads on plain Firecracker.
	if b.scenarios["baseline"] {
		b.baseline(ctx)
	}

	b.assertBounds(t)
}

// benchScenarios are what SPROUTFS_BENCH_SCENARIOS selects from, in the order a
// run takes them, each with the scenario it cannot run without: every guest
// workload runs in the machine the boot started, and every restore and fork
// starts from the capture's checkpoint. "cargo-build" is the cold and the warm
// build, "repository" the cold and the warm search and read of the repository,
// and "baseline" the plain-Firecracker record of each other selected scenario.
var benchScenarios = []struct{ name, needs string }{
	{"boot", ""},
	{"pnpm-install", "boot"},
	{"cargo-build", "boot"},
	{"repository", "boot"},
	{"capture", "boot"},
	{"restore-cold", "capture"},
	{"restore-warm", "capture"},
	{"fork-fanout", "capture"},
	{"steady-state", "capture"},
	{"baseline", ""},
}

// scenarioSet is the scenarios one run measures.
type scenarioSet map[string]bool

// selectedScenarios reads SPROUTFS_BENCH_SCENARIOS, a comma-separated list of
// scenario names; unset, a run takes every one. A name it does not know, or a
// scenario without the one it needs, fails the run rather than measuring
// something other than what was asked for.
func selectedScenarios(t *testing.T) scenarioSet {
	t.Helper()
	selected := scenarioSet{}
	value := os.Getenv("SPROUTFS_BENCH_SCENARIOS")
	for _, scenario := range benchScenarios {
		selected[scenario.name] = value == ""
	}
	if value == "" {
		return selected
	}
	for name := range strings.SplitSeq(value, ",") {
		name = strings.TrimSpace(name)
		if _, known := selected[name]; !known {
			t.Fatalf("unknown benchmark scenario %q", name)
		}
		selected[name] = true
	}
	for _, scenario := range benchScenarios {
		if selected[scenario.name] && scenario.needs != "" && !selected[scenario.needs] {
			t.Fatalf("benchmark scenario %s needs %s", scenario.name, scenario.needs)
		}
	}
	return selected
}

// names lists the selected scenarios in the order they run.
func (s scenarioSet) names() []string {
	var names []string
	for _, scenario := range benchScenarios {
		if s[scenario.name] {
			names = append(names, scenario.name)
		}
	}
	return names
}

// configuration is the exact setup every number below was measured under, so
// the documentation can cite it from the same file as the numbers.
func (b *benchmark) configuration() map[string]any {
	release, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return map[string]any{
		"date":                    time.Now().UTC().Format(time.RFC3339),
		"host_kernel":             strings.TrimSpace(string(release)),
		"revision":                os.Getenv("SPROUTFS_BENCH_REVISION"),
		"guest_ram_bytes":         benchRAMBytes,
		"guest_pmem_bytes":        benchPmemBytes,
		"vcpus":                   benchGuestVCPUs(),
		"page_size":               b.pageSize,
		"host_page_size":          os.Getpagesize(),
		"scenarios":               b.scenarios.names(),
		"resident_pages":          b.residentPages,
		"dirty_pages":             b.dirtyPages,
		"logical_pages":           b.logicalPages,
		"read_ahead_pages":        b.readAheadPages,
		"write_ahead_pages":       benchWriteAheadPages(), // zero is the pager's default
		"shared_memory_limit":     b.host.Resources().Stats().Limit,
		"max_write_bytes":         benchMaxWriteBytes,
		"object_store":            b.objectStoreKind,
		"object_get_latency_ns":   b.objectLatency.Get.Nanoseconds(),
		"object_put_latency_ns":   b.objectLatency.Put.Nanoseconds(),
		"object_bytes_per_second": b.objectLatency.BytesPerSecond,
		"kernel":                  b.kernel,
		"firecracker":             b.binary,
		"guest_image":             b.imagePath,
		"workload_install":        workloadInstall,
		"workload_build":          workloadBuild(),
		"workload_test":           workloadTest(),
		"workload_grep":           workloadGrep,
		"workload_read_tree":      workloadCat,
		"workload_restore":        workloadTrue,
		"forks":                   b.forks(),
		"steady_guests":           steadyGuests,
		"steady_period_ns":        b.steadyPeriod().Nanoseconds(),
		"fault_workers":           16,
		"queue_pages":             16384,
		"boot_args":               bootArgs(benchBootArgs),
		"baseline_boot_args":      bootArgs(benchPlainBootArgs),
	}
}

// forks and steadyPeriod are the recorded shape of the two concurrent
// scenarios, read once so the configuration record and the scenarios cannot
// disagree about what was run.
func (b *benchmark) forks() int {
	b.t.Helper()
	value := os.Getenv("SPROUTFS_BENCH_FORKS")
	if value == "" {
		return defaultForks
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		b.t.Fatal("invalid SPROUTFS_BENCH_FORKS")
	}
	return parsed
}
func (b *benchmark) steadyPeriod() time.Duration {
	b.t.Helper()
	value := os.Getenv("SPROUTFS_BENCH_STEADY")
	if value == "" {
		return defaultSteadyPeriod
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		b.t.Fatal("invalid SPROUTFS_BENCH_STEADY")
	}
	return parsed
}

// profile writes a CPU profile of the test process over one scenario when
// SPROUTFS_BENCH_PROFILE names a file and SPROUTFS_BENCH_PROFILE_SCENARIO names
// that scenario. It is the test binary profiling itself through runtime/pprof,
// so nothing outside the process signals it. The profile covers the host: the
// pager's fault workers, the control transport and the object store, which is
// where a fault's milliseconds have to be if they are not in the kernel.
//
// Both variables must be set, and a recorded run sets neither. Go's profiler
// interrupts every thread of the process a hundred times a second, and a
// workload whose progress is a stream of page faults resolved by those threads
// pays for it heavily: the same install measured 11 minutes profiled against
// under 3 unprofiled. A profiled run is a separate diagnostic run, and its own
// durations are not comparable with a recorded one.
func (b *benchmark) profile(scenario string) func() {
	b.t.Helper()
	path, want := os.Getenv("SPROUTFS_BENCH_PROFILE"), os.Getenv("SPROUTFS_BENCH_PROFILE_SCENARIO")
	if path == "" || want == "" || want != scenario {
		return func() {}
	}
	file, err := os.Create(path)
	if err != nil {
		b.t.Fatal(err)
	}
	if err := pprof.StartCPUProfile(file); err != nil {
		b.t.Fatal(err)
	}
	b.t.Logf("CPU profile of %s writing to %s", scenario, path)
	return func() {
		pprof.StopCPUProfile()
		if err := file.Close(); err != nil {
			b.t.Fatal(err)
		}
	}
}

// benchCommandTimeout bounds one guest command. A restored guest that resumes
// into a state where its vCPUs spin without producing output must fail the
// scenario with its console attached, not hang the run. It is generous because
// the qualification host's guests are: a cold cargo build there is tens of
// minutes, and twenty of them share four host cores in the fan-out.
const benchCommandTimeout = 2 * time.Hour

// runWithin is run without the test-fatal behaviour, for the concurrent
// scenarios that must collect their own errors.
func (b *benchmark) runWithin(ctx context.Context, c *console, command string) (guestTiming, error) {
	ctx, cancel := context.WithTimeout(ctx, benchCommandTimeout)
	defer cancel()
	return c.run(ctx, command)
}

func (b *benchmark) run(ctx context.Context, c *console, command string) guestTiming {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(ctx, benchCommandTimeout)
	defer cancel()
	timing, err := c.run(ctx, command)
	if err != nil {
		b.t.Fatalf("%q: %v", command, err)
	}
	if timing.Exit != 0 {
		b.t.Fatalf("%q exited %d", command, timing.Exit)
	}
	return timing
}

// inspect is what a diagnostic run asks of a freshly booted guest, of either
// kind, before any workload touches it.
//
// SPROUTFS_BENCH_DMESG names a file that receives the guest kernel's log,
// suffixed with the machine's kind. The kernel keeps every message in its log
// buffer even when quiet keeps them off the slow serial console, so this is how
// a run reads the boot's own timestamps.
//
// SPROUTFS_BENCH_RUN is a shell command, run in the guest, whose output becomes
// a "run" record of the machine's kind. Run in both guests of one benchmark, it
// measures the same thing on managed and plain guest memory.
func (b *benchmark) inspect(ctx context.Context, c *console, kind string, vm *volume.VM) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(ctx, benchCommandTimeout)
	defer cancel()
	if path := os.Getenv("SPROUTFS_BENCH_DMESG"); path != "" {
		output, err := c.capture(ctx, "dmesg")
		if err != nil {
			b.t.Fatalf("dmesg: %v", err)
		}
		if err := os.WriteFile(path+"."+kind, []byte(output), 0o644); err != nil {
			b.t.Fatal(err)
		}
	}
	if command := os.Getenv("SPROUTFS_BENCH_RUN"); command != "" {
		extra := map[string]any{"command": command}
		if vm != nil {
			extra["checkpoint_before"] = vm.Status().Checkpoint.Sequence
		}
		start := b.sample(ctx)
		output, err := c.capture(ctx, command)
		if err != nil {
			b.t.Fatalf("%s: %v", command, err)
		}
		b.t.Logf("%s run: %s", kind, strings.TrimSpace(output))
		extra["output"] = output
		if vm != nil {
			extra["checkpoint_after"] = vm.Status().Checkpoint.Sequence
		}
		b.record(ctx, "run", kind, start, nil, extra)
	}
}

// sync makes the guest's dirty DAX pages reach its volume, so a scenario's
// writes are charged to that scenario rather than to the next one.
func (b *benchmark) sync(ctx context.Context, c *console) {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(ctx, benchCommandTimeout)
	defer cancel()
	if err := c.send(ctx, "sync"); err != nil {
		b.t.Fatal(err)
	}
	if _, err := c.wait(ctx, "SPROUTFS_SYNC"); err != nil {
		b.t.Fatal(err)
	}
}

// forkOrigin is what every fork in this benchmark starts from: the pause a
// child inherits, and the VMM state it restores.
type forkOrigin struct {
	point *volume.ForkPoint
	state []byte
}

// fork restores one child of the origin and returns it ready to take commands.
func (b *benchmark) fork(ctx context.Context, origin *forkOrigin, id string) (*vmmachine.Process, *volume.VM, *console) {
	b.t.Helper()
	vm, err := b.manager.Fork(ctx, id, origin.point)
	if err != nil {
		b.t.Fatalf("fork %s: %v", id, err)
	}
	p, c, _ := b.start(ctx, vm, origin.state)
	return p, vm, c
}

// restoreScenario measures a restore from the checkpoint to the first trivial
// command completing inside the restored guest.
func (b *benchmark) restoreScenario(ctx context.Context, origin *forkOrigin, name, note string) {
	b.t.Helper()
	// Taking the handle is bookkeeping over the parent's checkpoint metadata,
	// not the restore. What this scenario measures is the restore itself and the
	// first command the guest completes after it.
	forkStarted := time.Now()
	vm, err := b.manager.Fork(ctx, name, origin.point)
	if err != nil {
		b.t.Fatalf("fork %s: %v", name, err)
	}
	forked := time.Now()
	start := b.sample(ctx)
	p, c, phases := b.start(ctx, vm, origin.state)
	restored := time.Now()
	timing := b.run(ctx, c, workloadTrue)
	extra := phases.extra(forked.Sub(forkStarted))
	extra["note"] = note
	extra["fork_ns"] = forked.Sub(forkStarted).Nanoseconds()
	extra["restore_ns"] = restored.Sub(start.at).Nanoseconds()
	b.record(ctx, name, "sproutfs", start, &timing, extra)
	c.close()
	if err := p.Close(); err != nil {
		b.t.Fatal(err)
	}
	if err := vm.Close(ctx); err != nil {
		b.t.Fatal(err)
	}
}

// forkFanOut restores twenty children of one checkpoint and runs the same test
// build in all of them at once. What it measures is how long each fork takes to
// produce its first output while nineteen siblings compete for the same pager.
func (b *benchmark) forkFanOut(ctx context.Context, origin *forkOrigin) {
	b.t.Helper()
	count := b.forks()
	start := b.sample(ctx)
	type child struct {
		process *vmmachine.Process
		vm      *volume.VM
		console *console
	}
	children := make([]child, count)
	restoreEach := make([]int64, count)
	for index := range children {
		at := time.Now()
		p, vm, c := b.fork(ctx, origin, "fanout-"+strconv.Itoa(index))
		restoreEach[index] = time.Since(at).Nanoseconds()
		children[index] = child{p, vm, c}
	}
	restored := time.Now()
	firstOutput := make([]int64, count)
	total := make([]int64, count)
	exits := make([]int, count)
	var wg sync.WaitGroup
	errs := make([]error, count)
	for index := range children {
		wg.Go(func() {
			c := children[index].console
			ctx, cancel := context.WithTimeout(ctx, benchCommandTimeout)
			defer cancel()
			if err := c.discard(); err != nil {
				errs[index] = err
				return
			}
			issued := time.Now()
			if err := c.send(ctx, "run "+workloadTest()); err != nil {
				errs[index] = err
				return
			}
			for {
				grown, err := c.grown()
				if err != nil {
					errs[index] = err
					return
				}
				if grown > 0 {
					break
				}
				select {
				case <-ctx.Done():
					errs[index] = context.Cause(ctx)
					return
				case <-time.After(5 * time.Millisecond):
				}
			}
			firstOutput[index] = time.Since(issued).Nanoseconds()
			line, err := c.wait(ctx, "SPROUTFS_RUN")
			if err != nil {
				errs[index] = err
				return
			}
			timing, err := parseGuestTiming(line)
			if err != nil {
				errs[index] = err
				return
			}
			total[index] = time.Since(issued).Nanoseconds()
			exits[index] = timing.Exit
		})
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			b.t.Fatalf("fork %d: %v", index, err)
		}
		if exits[index] != 0 {
			b.t.Fatalf("fork %d exited %d", index, exits[index])
		}
	}
	// What each fork holds once its command has finished, per volume: the
	// pages it maps and the pages whose bytes are its own. A larger page
	// copies more of what a fork writes into pages of its own.
	privatePages := make([]int, count)
	residentPages := make([]int, count)
	regionPages := make([]map[string]map[string]int, count)
	for index, item := range children {
		regionPages[index] = map[string]map[string]int{}
		for name, region := range item.process.Regions() {
			stats, err := region.Stats(ctx)
			if err != nil {
				b.t.Fatal(err)
			}
			regionPages[index][name] = map[string]int{"resident_pages": stats.ResidentPages, "private_pages": stats.PrivatePages}
			privatePages[index] += stats.PrivatePages
			residentPages[index] += stats.ResidentPages
		}
	}
	b.record(ctx, "fork-fanout", "sproutfs", start, nil, map[string]any{
		"forks":               count,
		"command":             workloadTest(),
		"restore_all_ns":      restored.Sub(start.at).Nanoseconds(),
		"restore_each_ns":     restoreEach,
		"max_restore_ns":      maxOf(restoreEach),
		"first_output_ns":     firstOutput,
		"total_ns":            total,
		"max_first_output":    maxOf(firstOutput),
		"max_total_ns":        maxOf(total),
		"fork_private_pages":  privatePages,
		"fork_resident_pages": residentPages,
		"fork_region_pages":   regionPages,
	})
	for _, item := range children {
		item.console.close()
		if err := item.process.Close(); err != nil {
			b.t.Fatal(err)
		}
		if err := item.vm.Close(ctx); err != nil {
			b.t.Fatal(err)
		}
	}
}

// steadyState runs eight guests through the repository workload for a fixed
// period and reports what the log and the checkpoint worker did under it.
func (b *benchmark) steadyState(ctx context.Context, origin *forkOrigin) {
	b.t.Helper()
	guests := steadyGuests
	period := b.steadyPeriod()
	start := b.sample(ctx)
	type child struct {
		process *vmmachine.Process
		vm      *volume.VM
		console *console
	}
	children := make([]child, guests)
	for index := range children {
		p, vm, c := b.fork(ctx, origin, "steady-"+strconv.Itoa(index))
		children[index] = child{p, vm, c}
	}
	deadline := time.Now().Add(period)
	iterations := make([]int, guests)
	errs := make([]error, guests)
	var wg sync.WaitGroup
	for index := range children {
		wg.Go(func() {
			c := children[index].console
			for time.Now().Before(deadline) {
				for _, command := range []string{workloadGrep, workloadCat} {
					timing, err := b.runWithin(ctx, c, command)
					if err != nil {
						errs[index] = err
						return
					}
					if timing.Exit != 0 {
						errs[index] = fmt.Errorf("%q exited %d", command, timing.Exit)
						return
					}
				}
				iterations[index]++
			}
		})
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			b.t.Fatalf("steady guest %d: %v", index, err)
		}
	}
	checkpoints := uint64(0)
	for _, item := range children {
		checkpoints += item.vm.Status().Checkpoint.Sequence
	}
	b.record(ctx, "steady-state", "sproutfs", start, nil, map[string]any{
		"guests":            guests,
		"period_ns":         period.Nanoseconds(),
		"iterations":        iterations,
		"checkpoint_totals": checkpoints,
	})
	for _, item := range children {
		item.console.close()
		if err := item.process.Close(); err != nil {
			b.t.Fatal(err)
		}
		if err := item.vm.Close(ctx); err != nil {
			b.t.Fatal(err)
		}
	}
}

func maxOf(values []int64) int64 {
	var highest int64
	for _, value := range values {
		if value > highest {
			highest = value
		}
	}
	return highest
}

// ---------------------------------------------------------------------------
// Baseline
// ---------------------------------------------------------------------------

// baseline runs the same workloads on plain Firecracker: a virtio-block root
// from a copy of the same image and ordinary anonymous guest RAM, built by the
// same script from the same kernel.
func (b *benchmark) baseline(ctx context.Context) {
	b.t.Helper()
	root := filepath.Join(b.work, "baseline-root.ext4")
	// Copying the image is the baseline's own host setup: the managed side
	// ingests it into a volume once, and this is what the plain VMM needs
	// instead, so it is reported rather than hidden inside the boot.
	copyStarted := time.Now()
	if err := copySparse(b.imagePath, root); err != nil {
		b.t.Fatal(err)
	}
	copyElapsed := time.Since(copyStarted)
	defer os.Remove(root)
	config := plainConfig{Binary: b.binary, Kernel: b.kernel,
		BootArgs: bootArgs(benchPlainBootArgs), RootPath: root, Directory: b.scratch,
		MemoryMiB: benchRAMBytes >> 20, VCPUs: benchGuestVCPUs()}

	copied := time.Now()
	p, err := startPlainVM(ctx, config)
	if err != nil {
		b.t.Fatal(err)
	}
	phases := startTiming{VMMStartNS: time.Since(copied).Nanoseconds()}
	running := time.Now()
	c := newConsole(p)
	line, err := c.wait(ctx, "SPROUTFS_READY")
	if err != nil {
		b.t.Fatalf("baseline boot: %v", err)
	}
	phases.ToReadyNS = time.Since(running).Nanoseconds()
	b.splitBoot(&phases, line)
	b.appendRecord(benchRecord{Scenario: "boot", Kind: "baseline",
		WallNS: time.Since(copied).Nanoseconds(), Extra: phases.extra(copyElapsed)})
	b.inspect(ctx, c, "baseline", nil)

	// The plain machine takes the scenarios the run selected for the managed
	// one, so a run that measures only the boot boots each kind once and stops.
	for _, item := range []struct {
		name      string
		selection string
		command   string
	}{
		{"pnpm-install", "pnpm-install", workloadInstall},
		{"cargo-build-cold", "cargo-build", workloadBuild()},
		{"cargo-build-warm", "cargo-build", workloadBuild()},
		{"git-grep-cold", "repository", workloadGrep},
		{"read-tree-cold", "repository", workloadCat},
		{"git-grep-warm", "repository", workloadGrep},
		{"read-tree-warm", "repository", workloadCat},
	} {
		if !b.scenarios[item.selection] {
			continue
		}
		at := time.Now()
		timing, err := c.run(ctx, item.command)
		if err != nil {
			b.t.Fatalf("baseline %s: %v", item.name, err)
		}
		if timing.Exit != 0 {
			b.t.Fatalf("baseline %s exited %d", item.name, timing.Exit)
		}
		b.appendRecord(benchRecord{Scenario: item.name, Kind: "baseline",
			WallNS: time.Since(at).Nanoseconds(), Guest: &timing})
	}

	if !b.scenarios["capture"] {
		if err := p.Close(); err != nil {
			b.t.Fatal(err)
		}
		return
	}

	// The baseline restore: an ordinary memory-file snapshot, which writes the
	// whole of guest RAM and reads it back.
	statePath := filepath.Join(b.work, "baseline.state")
	memoryPath := filepath.Join(b.work, "baseline.mem")
	defer os.Remove(statePath)
	defer os.Remove(memoryPath)
	at := time.Now()
	if err := p.Snapshot(ctx, statePath, memoryPath); err != nil {
		b.t.Fatalf("baseline snapshot: %v", err)
	}
	snapshotElapsed := time.Since(at)
	memoryInfo, err := os.Stat(memoryPath)
	if err != nil {
		b.t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		b.t.Fatal(err)
	}
	b.appendRecord(benchRecord{Scenario: "capture", Kind: "baseline", WallNS: snapshotElapsed.Nanoseconds(),
		Extra: map[string]any{"memory_file_bytes": memoryInfo.Size()}})
	if !b.scenarios["restore-cold"] {
		return
	}

	restoreConfig := config
	restoreConfig.SnapshotPath = statePath
	restoreConfig.MemoryPath = memoryPath
	at = time.Now()
	restored, err := startPlainVM(ctx, restoreConfig)
	if err != nil {
		b.t.Fatalf("baseline restore: %v", err)
	}
	defer restored.Close()
	rc := newConsole(restored)
	if err := rc.skipExisting(); err != nil {
		b.t.Fatal(err)
	}
	timing, err := rc.run(ctx, workloadTrue)
	if err != nil {
		b.t.Fatalf("baseline restore command: %v", err)
	}
	b.appendRecord(benchRecord{Scenario: "restore-cold", Kind: "baseline",
		WallNS: time.Since(at).Nanoseconds(), Guest: &timing,
		Extra: map[string]any{"memory_file_bytes": memoryInfo.Size()}})
}

// appendRecord stores one finished record and rewrites the output file, so an
// interrupted run still leaves what it proved.
//
// The guest timing is copied rather than referenced. A caller that reuses one
// variable across scenarios would otherwise leave every record pointing at the
// same value, and since the file is rewritten after every scenario the finished
// file would report the last command's numbers for all of them. That is exactly
// what the first recorded run did, which is why the copy is checked as well as
// made: two consecutive scenarios of one kind reporting a byte-identical guest
// record means one of them measured the other's command, and no real pair of
// commands agrees to the nanosecond.
func (b *benchmark) appendRecord(rec benchRecord) {
	b.t.Helper()
	if rec.Guest != nil {
		guest := *rec.Guest
		rec.Guest = &guest
	}
	b.mu.Lock()
	if b.lastGuest == nil {
		b.lastGuest = make(map[string]guestTiming)
	}
	previous, seen := b.lastGuest[rec.Kind]
	if rec.Guest != nil {
		b.lastGuest[rec.Kind] = *rec.Guest
	}
	b.records = append(b.records, rec)
	snapshot := make([]benchRecord, len(b.records))
	copy(snapshot, b.records)
	b.mu.Unlock()
	if rec.Guest != nil && seen && previous == *rec.Guest {
		b.t.Fatalf("%s/%s reports the same guest record as the previous %s scenario (%+v): one of them measured the other's command",
			rec.Kind, rec.Scenario, rec.Kind, previous)
	}
	b.writeOutput(snapshot)
	b.t.Logf("%s/%s wall=%s guest=%+v memory=%+v objects=%+v extra=%v", rec.Kind, rec.Scenario,
		time.Duration(rec.WallNS), rec.Guest, rec.Memory, rec.Objects, rec.Extra)
}

// copySparse copies the guest image without materializing its holes; the
// baseline must not write through to the cached image.
func copySparse(from, to string) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	destination, err := os.OpenFile(to, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer destination.Close()
	if err := destination.Truncate(info.Size()); err != nil {
		return err
	}
	buffer := make([]byte, 1<<20)
	for offset := int64(0); offset < info.Size(); {
		start, err := source.Seek(offset, seekData)
		if errors.Is(err, syscall.ENXIO) {
			break
		}
		if err != nil {
			return err
		}
		end, err := source.Seek(start, seekHole)
		if err != nil {
			return err
		}
		for cursor := start; cursor < end; {
			length := min(int64(len(buffer)), end-cursor)
			if _, err := source.ReadAt(buffer[:length], cursor); err != nil {
				return err
			}
			if _, err := destination.WriteAt(buffer[:length], cursor); err != nil {
				return err
			}
			cursor += length
		}
		offset = end
	}
	return destination.Sync()
}

// ---------------------------------------------------------------------------
// Bounds
// ---------------------------------------------------------------------------

// assertBounds checks every bound whose scenarios the run measured; a ratio to
// the baseline needs both of its sides.
func (b *benchmark) assertBounds(t *testing.T) {
	t.Helper()
	for _, item := range []struct {
		scenario, selection string
		ratio               float64
	}{{"pnpm-install", "pnpm-install", boundInstallRatio}, {"cargo-build-cold", "cargo-build", boundBuildRatio}} {
		if !b.scenarios[item.selection] || !b.scenarios["baseline"] {
			continue
		}
		managed := b.find(item.scenario, "sproutfs")
		plain := b.find(item.scenario, "baseline")
		if plain.WallNS <= 0 {
			t.Fatalf("%s has no baseline duration", item.scenario)
		}
		got := float64(managed.WallNS) / float64(plain.WallNS)
		t.Logf("%s: managed %s, baseline %s, ratio %.2f (bound %.2f)", item.scenario,
			time.Duration(managed.WallNS), time.Duration(plain.WallNS), got, item.ratio)
		if got > item.ratio {
			t.Errorf("%s took %.2fx the baseline, above the %.2fx bound", item.scenario, got, item.ratio)
		}
	}

	if b.scenarios["restore-warm"] {
		warm := b.find("restore-warm", "sproutfs")
		if time.Duration(warm.WallNS) > boundWarmRestore {
			t.Errorf("warm restore to first output took %s, above %s", time.Duration(warm.WallNS), boundWarmRestore)
		}
		if warm.Objects == nil || warm.Objects.Gets != 0 {
			t.Errorf("warm restore issued %v object-store GETs, which a resident sibling makes unnecessary", warm.Objects)
		}
	}

	if b.scenarios["fork-fanout"] {
		fanout := b.find("fork-fanout", "sproutfs")
		if highest, ok := fanout.Extra["max_first_output"].(int64); !ok || time.Duration(highest) > boundForkFirstOutput {
			t.Errorf("slowest fork produced its first output after %v, above %s", fanout.Extra["max_first_output"], boundForkFirstOutput)
		}
	}

	if b.scenarios["capture"] {
		capture := b.find("capture", "sproutfs")
		t.Logf("capture: %v", capture.Extra)
	}
}
