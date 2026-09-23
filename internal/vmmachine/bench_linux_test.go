//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
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

// The measured sandbox: a 16 GiB guest with a 32 GiB PMEM root, on the pair of
// pagers a deployment runs — RAM's 4 KiB page over ordinary memory, PMEM's
// 2 MiB page over the node's HugeTLB pool — whose resident budgets are 40 GiB
// and 24 GiB. The shape is the workload's: the database scenario seeds 8 GiB of
// the guest's RAM and every fork of it copies what it updates, and the arenas
// hold that guest and the forks taken from it without the spill files becoming
// the measurement. Every one of the four is an environment override, because a
// qualification host and a laptop cannot run the same shape.
const (
	defaultBenchRAMBytes  = 16 << 30
	defaultBenchRootBytes = 32 << 30
	benchVCPUs            = 4
	// The resident budgets are stated per kind and apart: a guest's memory is
	// what the small page is for and takes the larger arena, while its root is
	// read far more than it is written and shares its pages across every fork.
	defaultBenchRAMResidentBytes  = 40 << 30
	defaultBenchPmemResidentBytes = 24 << 30
	// benchMaxWriteBytes is the volumes' write limit and so the pager's flush
	// batch. It is a size, so the pager's page does not change it; the read-ahead
	// run each pager loads is likewise a size, stated once in newHostPagers.
	benchMaxWriteBytes = checkpoint.PageSize2MiB
	// benchRAMWriteAheadBytes is the run a store into fresh zeros makes
	// private: the 8 MiB a host gives its RAM pager (internal/host/pager.go,
	// writeAheadBytes), which is 2048 pages at 4 KiB and 4 at 2 MiB.
	benchRAMWriteAheadBytes = 8 << 20
	// benchCacheBytes is what the shared budget holds above the two arenas, for
	// decoded objects. The budget itself is the sum: resident guest pages of
	// both pagers and the page cache are charged against one number, as they are
	// on a host, and a budget smaller than the arenas would evict pages the
	// arenas were sized to hold.
	benchCacheBytes = 4 << 30
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
	// benchQueuePages bounds the faults one region has pending. A machine clamps
	// it to the region's own size in its own pager's page, so this is a ceiling
	// over both geometries rather than a budget stated in either.
	benchQueuePages = 16384
	// benchRangeBytes is the 2 MiB-aligned range the page-geometry plan makes a
	// RAM region's unit of mapping: a private page lives at its own offset
	// within its range's extent, so what a range costs in mappings is how often
	// it alternates between shared and private. The fan-out reports the private
	// runs and the gaps between them within one of these.
	benchRangeBytes = checkpoint.PageSize2MiB
)

// The workloads, shared verbatim by the managed run and the baseline so the
// ratio between them is a property of the storage and nothing else.
//
// The image ships the openai/codex workspace at a pinned release with every
// crate vendored and the toolchain it pins, and nothing of it compiled: what
// the fan-out runs is one crate's tests, which builds what they need.
// `SPROUTFS_BENCH_TEST` selects another command; the configuration record names
// whichever the run actually used.
const (
	// The lockfile is frozen so the install is the work of linking a store into
	// a project, not a resolution the guest has no network for.
	workloadInstall = "cd /opt/app && pnpm install --offline --frozen-lockfile --reporter=append-only"
	// The two tests left out expect a write to be refused, and a guest runs as
	// root, which is refused nothing.
	defaultWorkloadTest = "cd /opt/codex/codex-rs && cargo test --offline -p codex-apply-patch -- " +
		"--skip test_apply_patch_fails_on_write_error --skip test_failed_move_returns_committed_destination_delta"
	workloadGrep = "cd /opt/codex && git grep -c fn | wc -l"
	workloadCat  = "cd /opt/codex && find . -path ./codex-rs/target -prune -o -type f -print0 | xargs -0 cat > /dev/null"
	workloadTrue = "true"
	// The recorded shape of the two concurrent scenarios. Both have environment
	// overrides for a quick run, and both record what they actually used.
	defaultForks        = 4
	defaultSteadyPeriod = 5 * time.Minute
	steadyGuests        = 8
)

func workloadTest() string { return cmpOr(os.Getenv("SPROUTFS_BENCH_TEST"), defaultWorkloadTest) }

// Bounds. These are deliberately generous on the first commit; tighten them
// from the numbers a recorded run writes to docs/measurements, keeping about a
// quarter of headroom over what was observed.
const (
	boundInstallRatio = 2.0
	boundWarmRestore  = 500 * time.Millisecond
	// A fork's first output is bounded against the plain side's clones running
	// the same command, not against a constant: the workload's own command
	// decides when it first prints — `cargo test` compiles for most of a minute
	// before it does — and what the pager owes is that a fork is not slower to
	// reach that point than a clone with its whole memory in place. The
	// 2026-09-23 run measured 50.9 s against 49.2 s.
	boundForkFirstOutputRatio = 1.25
)

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------

// benchRecord is one measured scenario. Every number the documentation cites
// comes from one of these, and the bounds this test asserts are checked against
// the same fields, so the table and the regression test cannot drift apart.
//
// A host runs one pager per kind of region, so what a scenario cost the pagers
// is two records and never their sum: MemoryRAM is the 4 KiB pager that holds
// guest memory and MemoryPMEM the 2 MiB pager that holds the disks. They
// replace the single `memory` field of records taken before the two geometries
// existed; each carries the page it counts in, so a reader that needs one
// number converts them to bytes rather than adding pages.
type benchRecord struct {
	Scenario   string          `json:"scenario"`
	Kind       string          `json:"kind"` // "sproutfs" or "baseline"
	WallNS     int64           `json:"wall_ns"`
	Guest      *guestTiming    `json:"guest,omitempty"`
	MemoryRAM  *memoryDelta    `json:"memory_ram,omitempty"`
	MemoryPMEM *memoryDelta    `json:"memory_pmem,omitempty"`
	Volumes    *volumeDelta    `json:"volumes,omitempty"`
	Objects    *objectCounters `json:"objects,omitempty"`
	Cache      *cacheDelta     `json:"cache,omitempty"`
	// Host is what the benchmark process itself holds when the scenario ends:
	// the pagers, the volumes and the object caches all live in it, so its heap
	// is the host's own memory beside the arenas.
	Host  *hostMemory    `json:"host,omitempty"`
	Extra map[string]any `json:"extra,omitempty"`
}

// hostMemory is the Go runtime's account of the benchmark process: the heap
// its live objects occupy, what the runtime has taken from the operating system
// in all, and the resident set the kernel charges it, anonymous and shared.
type hostMemory struct {
	HeapInuseBytes uint64 `json:"heap_inuse_bytes"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	SysBytes       uint64 `json:"sys_bytes"`
	RSSAnonBytes   int64  `json:"rss_anon_bytes"`
	RSSShmemBytes  int64  `json:"rss_shmem_bytes"`
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

// memoryDelta is what one scenario cost one of the two pagers. The counters are
// deltas; the page totals are the state left behind. Every page count here is
// in PageSize, which is the only page it means: the other pager's record counts
// its own, and the two are never added.
type memoryDelta struct {
	PageSize      uint64 `json:"page_size"`
	Faults        uint64 `json:"faults"`
	CopyOnWrites  uint64 `json:"copy_on_writes"`
	Evictions     uint64 `json:"evictions"`
	Spills        uint64 `json:"spills"`
	SpillRefaults uint64 `json:"spill_refaults"`
	SpillWrites   uint64 `json:"spill_writes"`
	Loads         uint64 `json:"loads"`
	LoadedPages   uint64 `json:"loaded_pages"`
	IdentityHits  uint64 `json:"identity_hits"`
	Mappings      uint64 `json:"mappings"`
	MappedPages   uint64 `json:"mapped_pages"`
	MappingRuns   uint64 `json:"mapping_runs"`
	Revocations   uint64 `json:"revocations"`
	RevokeRuns    uint64 `json:"revoke_runs"`
	RevokedPages  uint64 `json:"revoked_pages"`
	// RuleCopies is the pages the two mapping rules made private beside the ones
	// the guest stored into, which is the whole of what a store copies past its
	// faulting page: with it beside CopyOnWrites a record says whether a fork's
	// first pass over its memory copies one page a window or more than one.
	// MappingMerges says the backstop behind those rules acted, and
	// RefusedMappings counts the faults a client refused a command for.
	RuleCopies      uint64 `json:"rule_copies"`
	MappingMerges   uint64 `json:"mapping_merges"`
	RefusedMappings uint64 `json:"refused_mappings"`
	// UnchangedPages is the pages a settle found to hold exactly their origin's
	// bytes: write faults the guest never stored through, which no checkpoint
	// publishes and which go straight back to sharing. PrivateExtents is how many
	// 2 MiB ranges of this pager's regions hold a private page.
	UnchangedPages    uint64 `json:"unchanged_pages"`
	PrivateExtents    int    `json:"private_extents"`
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

func memoryBetween(pageSize uint64, before, after vmmemory.Stats) *memoryDelta {
	return &memoryDelta{
		PageSize: pageSize,
		Faults:   after.Faults - before.Faults, CopyOnWrites: after.CopyOnWrites - before.CopyOnWrites,
		Evictions: after.Evictions - before.Evictions, Spills: after.Spills - before.Spills,
		SpillRefaults: after.SpillRefaults - before.SpillRefaults, SpillWrites: after.SpillWrites - before.SpillWrites,
		Loads: after.Loads - before.Loads, LoadedPages: after.LoadedPages - before.LoadedPages,
		IdentityHits: after.IdentityHits - before.IdentityHits, Mappings: after.Mappings - before.Mappings,
		MappedPages: after.MappedPages - before.MappedPages, MappingRuns: after.MappingRuns - before.MappingRuns,
		Revocations: after.Revocations - before.Revocations, RevokeRuns: after.RevokeRuns - before.RevokeRuns,
		RevokedPages:    after.RevokedPages - before.RevokedPages,
		RuleCopies:      after.RuleCopies - before.RuleCopies,
		MappingMerges:   after.MappingMerges - before.MappingMerges,
		RefusedMappings: after.RefusedMappings - before.RefusedMappings,
		UnchangedPages:  after.UnchangedPages - before.UnchangedPages,
		PrivateExtents:  after.PrivateExtents,
		Protections:     after.Protections - before.Protections, ProtectedPages: after.ProtectedPages - before.ProtectedPages,
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
	// pagers is the production pair — RAM at 4 KiB over ordinary memory, PMEM
	// at 2 MiB over the HugeTLB pool — and resources the budget their resident
	// pages and the decoded objects of the cache are charged against together.
	pagers    *hostPagers
	resources *resource.Budget
	scenarios scenarioSet

	// vmm is the machine whose address space the sequential scenarios run in,
	// so every one of them can report how many mappings it holds.
	vmm *vmmachine.Process

	binary, seccomp, kernel, imagePath string
	// The guest's shape and the budget behind each kind of its memory, in bytes
	// because the two pagers count them in different pages.
	ramBytes, rootBytes                 uint64
	ramResidentBytes, pmemResidentBytes uint64
	ramDirtyBytes, pmemDirtyBytes       uint64
	ramLogicalBytes, pmemLogicalBytes   uint64
	objectStoreKind                     string
	objectLatency                       objectLatency

	mu      sync.Mutex
	records []benchRecord
	// lastGuest is the previous scenario's guest record per kind, which is what
	// the harness checks a new one against.
	lastGuest map[string]guestTiming
	output    string
}

// sample is everything a scenario is measured against, taken at one moment.
// The two pagers are read apart and stay apart: their event counters would add,
// but their page counts are in different pages and their histograms describe
// different work.
type sample struct {
	at        time.Time
	ram, pmem vmmemory.Stats
	objects   objectCounters
	cache     checkpoint.CacheStats
	volumes   volume.Stats
}

func (b *benchmark) sample(ctx context.Context) sample {
	b.t.Helper()
	ram, err := b.pagers.pagers.Ram.Stats(ctx)
	if err != nil {
		b.t.Fatal(err)
	}
	pmem, err := b.pagers.pagers.Pmem.Stats(ctx)
	if err != nil {
		b.t.Fatal(err)
	}
	return sample{at: time.Now(), ram: ram, pmem: pmem, objects: b.objects.counters(),
		cache: b.cache.Stats(), volumes: b.manager.Stats()}
}

// pagerStats reads both pagers at one moment, for the spans inside a scenario
// that a full sample would be too much for.
func (b *benchmark) pagerStats(ctx context.Context) (ram, pmem vmmemory.Stats) {
	b.t.Helper()
	ram, err := b.pagers.pagers.Ram.Stats(ctx)
	if err != nil {
		b.t.Fatal(err)
	}
	pmem, err = b.pagers.pagers.Pmem.Stats(ctx)
	if err != nil {
		b.t.Fatal(err)
	}
	return ram, pmem
}

// ramPageSize and pmemPageSize are the pages the records' counts are in.
func (b *benchmark) ramPageSize() uint64  { return b.pagers.pagers.Ram.PageSize() }
func (b *benchmark) pmemPageSize() uint64 { return b.pagers.pagers.Pmem.PageSize() }

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
		rec.MemoryRAM = memoryBetween(b.ramPageSize(), start.ram, end.ram)
		rec.MemoryPMEM = memoryBetween(b.pmemPageSize(), start.pmem, end.pmem)
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
		rec.Host = b.hostMemory(name)
	}
	b.appendRecord(rec)
	return rec
}

// hostMemory reads what this process holds once a scenario has ended, and
// where SPROUTFS_BENCH_HEAP_DIR names a directory writes the scenario's heap
// profile there, which is what says whose objects that heap is.
func (b *benchmark) hostMemory(scenario string) *hostMemory {
	b.t.Helper()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	host := &hostMemory{HeapInuseBytes: stats.HeapInuse, HeapAllocBytes: stats.HeapAlloc, SysBytes: stats.Sys}
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		b.t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "kB" {
			continue
		}
		kib, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			b.t.Fatal(err)
		}
		switch fields[0] {
		case "RssAnon:":
			host.RSSAnonBytes = kib << 10
		case "RssShmem:":
			host.RSSShmemBytes = kib << 10
		}
	}
	if dir := os.Getenv("SPROUTFS_BENCH_HEAP_DIR"); dir != "" {
		file, err := os.Create(filepath.Join(dir, scenario+".heap"))
		if err != nil {
			b.t.Fatal(err)
		}
		defer func() {
			if err := file.Close(); err != nil {
				b.t.Error(err)
			}
		}()
		if err := pprof.Lookup("heap").WriteTo(file, 0); err != nil {
			b.t.Fatal(err)
		}
	}
	return host
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
	b := &benchmark{t: t, work: work, scenarios: selectedScenarios(t),
		binary:    requireEnv(t, "SPROUTFS_FIRECRACKER"),
		seccomp:   requireEnv(t, "SPROUTFS_FIRECRACKER_SECCOMP"),
		kernel:    requireEnv(t, "SPROUTFS_FIRECRACKER_KERNEL"),
		imagePath: requireEnv(t, "SPROUTFS_BENCH_IMAGE"),
		output:    os.Getenv("SPROUTFS_BENCH_OUTPUT")}
	b.shape()
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
	memoryBytes := int64(b.ramResidentBytes + b.pmemResidentBytes + benchCacheBytes)
	if value := os.Getenv("SPROUTFS_BENCH_MEMORY_MIB"); value != "" {
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed <= 0 || int64(parsed) > (1<<63-1)>>20 {
			t.Fatal("invalid SPROUTFS_BENCH_MEMORY_MIB")
		}
		memoryBytes = int64(parsed) << 20
	}
	// One local RAM budget is shared by decoded objects and the resident pages
	// of both pagers.
	resources, err := resource.New(memoryBytes)
	if err != nil {
		t.Fatal(err)
	}
	b.resources = resources
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

	// The two pagers, as a host assembles them. Every budget is in bytes and
	// converted by the pager it belongs to, because the same number of pages is
	// a different amount of memory in each.
	b.pagers = newConfiguredHostPagers(t, ctx, hostPagersConfig{
		RAM: hostPagerBudgets{Arena: b.ramResidentBytes, Logical: b.ramLogicalBytes,
			Dirty: b.ramDirtyBytes, WriteAhead: int(benchRAMWriteAheadBytes / ramPageBytes(t))},
		PMEM: hostPagerBudgets{Arena: b.pmemResidentBytes, Logical: b.pmemLogicalBytes,
			Dirty: b.pmemDirtyBytes, WriteAhead: benchWriteAheadPages()},
		Resources: resources,
		SpillDir:  work,
	})
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
		if err := b.pagers.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return b
}

// shape reads the guest's geometry and the budget behind each kind of its
// memory from the environment, with the recorded defaults above where a
// variable is unset, and checks each against the pager it belongs to. A budget
// that is not a whole number of that pager's pages would be silently rounded
// down, and a root smaller than the image it is ingested from cannot hold it,
// so both fail the run rather than measuring something other than what was
// asked for.
func (b *benchmark) shape() {
	b.t.Helper()
	// The RAM pager's page is SPROUTFS_RAM_PAGE_BYTES: 4 KiB on ordinary memory
	// by default, or 2 MiB on the HugeTLB pool, which is the pager RAM ran
	// before it had a page of its own and what a comparison of the two runs.
	b.ramBytes = benchBytesEnv(b.t, "SPROUTFS_BENCH_RAM_BYTES", defaultBenchRAMBytes, ramPageBytes(b.t))
	b.rootBytes = benchBytesEnv(b.t, "SPROUTFS_BENCH_ROOT_BYTES", defaultBenchRootBytes, checkpoint.PageSize2MiB)
	b.ramResidentBytes = benchBytesEnv(b.t, "SPROUTFS_BENCH_RAM_RESIDENT_BYTES",
		defaultBenchRAMResidentBytes, ramPageBytes(b.t))
	b.pmemResidentBytes = benchBytesEnv(b.t, "SPROUTFS_BENCH_PMEM_RESIDENT_BYTES",
		defaultBenchPmemResidentBytes, checkpoint.PageSize2MiB)
	info, err := os.Stat(b.imagePath)
	if err != nil {
		b.t.Fatal(err)
	}
	if uint64(info.Size()) > b.rootBytes {
		b.t.Fatalf("the guest image is %d bytes and the root volume %d: SPROUTFS_BENCH_ROOT_BYTES cannot be smaller than the image",
			info.Size(), b.rootBytes)
	}
	// The dirty budget is not the resident budget. A private page becomes clean
	// only through a coordinated capture, so what a running guest has dirtied
	// stays dirty for as long as it runs; the resident budget bounds physical
	// pages and the spill file absorbs the rest. A host therefore has to
	// provision dirty capacity for everything every guest it runs at once could
	// have written, which for either kind is the whole of what that kind maps —
	// so each pager's dirty budget is its logical one, and both are the guests'
	// own bytes rather than a share of an arena.
	b.ramLogicalBytes = benchMaxGuests * b.ramBytes
	b.pmemLogicalBytes = benchMaxGuests * b.rootBytes
	b.ramDirtyBytes, b.pmemDirtyBytes = b.ramLogicalBytes, b.pmemLogicalBytes
}

// benchBytesEnv reads one byte-valued variable, defaulting where it is unset
// and requiring a whole, positive number of the given page.
func benchBytesEnv(t *testing.T, name string, fallback, page uint64) uint64 {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 || parsed%page != 0 {
		t.Fatalf("%s is %q; want a positive whole multiple of the %d-byte page it is stated in", name, value, page)
	}
	return parsed
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
		// Every benchmark of a run shares the run's prefix, and each is a
		// deployment of its own, so each takes a prefix of its own under it:
		// two of them naming a VM "template" must not be one VM.
		prefix := path.Join(os.Getenv("SPROUTFS_GCS_PREFIX"), fmt.Sprintf("benchmark-%d", time.Now().UnixNano()))
		store, closer, err := adapters.NewGCS(t.Context(), endpoint, bucket, prefix)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if closeErr := closer.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		})
		where := cmpOr(endpoint, "storage.googleapis.com")
		t.Logf("object store: Google Cloud Storage %s bucket %s prefix %s", where, bucket, prefix)
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
		Scratch: b.managedScratch, Pagers: b.pagers.pagers, VM: vm,
		Pmem: []vmmachine.Pmem{{ID: "root", Root: true}}, VCPUs: benchGuestVCPUs(), RestoreState: restore,
		// The queue is a ceiling: a machine clamps it to each region's own size
		// in that region's pager's page, so nothing here is stated in a page.
		Connection: vmmemory.ConnectionConfig{QueuePages: benchQueuePages, FaultWorkers: 16,
			CommandTimeout: 5 * time.Minute, VerifyInterval: 5 * time.Second},
	}
}

func (b *benchmark) createVM(ctx context.Context, id string) *volume.VM {
	b.t.Helper()
	// Each volume is created in the page of the pager that will map it, which is
	// what a host does: the guest's memory at 4 KiB and its root at 2 MiB.
	vm, err := b.manager.Create(ctx, id, []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: b.ramBytes, PageSize: ramPageBytes(b.t)},
		{Name: "root", Size: b.rootBytes, PageSize: checkpoint.PageSize2MiB},
	})
	if err != nil {
		b.t.Fatal(err)
	}
	return vm
}

// ingest writes the guest image into the template's root volume. Only the
// allocated extents of the sparse image are written, in whole page-sized
// batches; every hole is discarded, which costs one bounded record however
// large it is. A root larger than the image is the rest of it discarded, so a
// run can give the guest more disk than the image was built with.
func (b *benchmark) ingest(ctx context.Context, vm *volume.VM) (elapsed time.Duration, written, imageBytes uint64) {
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
	if uint64(info.Size()) > root.Size() || uint64(info.Size())%checkpoint.PageSize2MiB != 0 {
		b.t.Fatalf("guest image is %d bytes, root volume is %d: the image must be a whole number of 2 MiB pages and fit",
			info.Size(), root.Size())
	}
	pages := int(root.Size() / checkpoint.PageSize2MiB)
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
		for page := start / checkpoint.PageSize2MiB; page <= (end-1)/checkpoint.PageSize2MiB && int(page) < pages; page++ {
			data[page] = true
		}
		offset = end
	}
	started := time.Now()
	buffer := make([]byte, checkpoint.PageSize2MiB)
	for page := 0; page < pages; page++ {
		if !data[page] {
			hole := page
			for hole < pages && !data[hole] {
				hole++
			}
			if err := root.Discard(ctx, uint64(page)*checkpoint.PageSize2MiB, uint64(hole-page)*checkpoint.PageSize2MiB); err != nil {
				b.t.Fatal(err)
			}
			page = hole - 1
			continue
		}
		if _, err := file.ReadAt(buffer, int64(page)*checkpoint.PageSize2MiB); err != nil {
			b.t.Fatal(err)
		}
		if err := root.WriteBatch(ctx, []volume.WriteExtent{{Offset: uint64(page) * checkpoint.PageSize2MiB, Data: buffer}}); err != nil {
			b.t.Fatal(err)
		}
		written += checkpoint.PageSize2MiB
	}
	if err := vm.Checkpoint(ctx); err != nil {
		b.t.Fatal(err)
	}
	return time.Since(started), written, uint64(info.Size())
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
//
// Phases is what VMMStartNS divides into, so a restore of seconds says which
// part of it was the VMM's own start, which was the snapshot load, and which was
// the pager attaching and populating each region — and, per region, what that
// populate installed. A managed fan-out's restore is the number to explain
// before its first output is, and none of it is explainable from one duration.
type startTiming struct {
	VMMStartNS  int64 `json:"vmm_start_ns"`
	ToReadyNS   int64 `json:"to_ready_ns"`
	KernelNS    int64 `json:"kernel_ns,omitempty"`
	InitNS      int64 `json:"init_ns,omitempty"`
	PreKernelNS int64 `json:"pre_kernel_ns,omitempty"`
	Phases      vmmachine.StartPhases
}

func (s startTiming) extra(hostSetup time.Duration) map[string]any {
	extra := map[string]any{"host_setup_ns": hostSetup.Nanoseconds(), "vmm_start_ns": s.VMMStartNS,
		"to_ready_ns": s.ToReadyNS, "kernel_ns": s.KernelNS, "init_ns": s.InitNS,
		"pre_kernel_ns": s.PreKernelNS}
	for key, value := range s.phases() {
		extra[key] = value
	}
	return extra
}

// phases is the start's own split as a record carries it: the four phases of
// vmmachine.Start, and per region the attach and the populate inside it.
func (s startTiming) phases() map[string]any {
	attach := map[string]map[string]any{}
	for name, stats := range s.Phases.Attachments {
		attach[name] = map[string]any{"attach_ns": stats.DurationNS,
			"populate_ns": stats.Populate.DurationNS, "populate_commands": stats.Populate.Commands,
			"populate_runs": stats.Populate.Runs, "populate_pages": stats.Populate.Pages}
	}
	return map[string]any{"vmm_process_ns": s.Phases.ProcessNS,
		"state_load_ns": s.Phases.StateLoadNS, "sessions_ns": s.Phases.SessionsNS,
		"vmm_ready_ns": s.Phases.ReadyNS, "region_attach": attach}
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
	timing := startTiming{VMMStartNS: time.Since(launched).Nanoseconds(), Phases: p.StartPhases()}
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
// A capture seals both of the machine's regions, so it reads both pagers: what
// the two spent and how many commands they issued adds, because nanoseconds and
// commands are the same unit in either; what they sealed and protected does
// not, so those are reported per kind and in bytes.
func (b *benchmark) capture(ctx context.Context, p *vmmachine.Process, vm *volume.VM) (*volume.Checkpoint, map[string]any) {
	b.t.Helper()
	beforeRAM, beforePMEM := b.pagerStats(ctx)
	// How many mappings the VMM's address space holds when the seal runs. A
	// range write-protect is applied to every registered mapping the range
	// covers, so the kernel walks them; the seal microbenchmark's guest has a
	// handful and a guest that has run a build has one per private page.
	vmas := countMappings(p.PID())
	paused := time.Now()
	var state []byte
	var prepared, resumed time.Time
	var atResumeRAM, atResumePMEM vmmemory.Stats
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
		atResumeRAM, atResumePMEM = b.pagerStats(ctx)
		return captured, sources, nil
	})
	if err != nil {
		b.t.Fatalf("capture: %v", err)
	}
	protectedRAM := atResumeRAM.ProtectedPages - beforeRAM.ProtectedPages
	protectedPMEM := atResumePMEM.ProtectedPages - beforePMEM.ProtectedPages
	published := time.Now()
	if err := ckpt.Wait(ctx); err != nil {
		b.t.Fatal(err)
	}
	// The pages a seal freezes are moved into the checkpoint by the walk behind
	// its pause, with the guest already running, so they are counted once that
	// walk has run rather than at the resume — which is also where the walk's
	// own duration is, the number the pause used to carry.
	afterRAM, afterPMEM := b.pagerStats(ctx)
	sealedRAM := afterRAM.CheckpointPages - beforeRAM.CheckpointPages
	sealedPMEM := afterPMEM.CheckpointPages - beforePMEM.CheckpointPages
	sealedBytes := sealedRAM*b.ramPageSize() + sealedPMEM*b.pmemPageSize()
	pause := resumed.Sub(paused)
	// Prepare is one API call pair: Firecracker pauses its vCPUs, saves its device
	// and KVM state, and only then asks the pager for the memory checkpoint. The
	// pager times its own half, so what is left of Prepare is Firecracker's pause
	// and state save together, which is the number a fix would have to target if
	// the seal turns out not to be the cost.
	seals := int64(atResumeRAM.Seal.TotalNS-beforeRAM.Seal.TotalNS) +
		int64(atResumePMEM.Seal.TotalNS-beforePMEM.Seal.TotalNS)
	protects := int64(atResumeRAM.Protect.TotalNS-beforeRAM.Protect.TotalNS) +
		int64(atResumePMEM.Protect.TotalNS-beforePMEM.Protect.TotalNS)
	prepare := prepared.Sub(paused).Nanoseconds()
	extra := map[string]any{
		"sealed_pages_ram":     sealedRAM,
		"sealed_pages_pmem":    sealedPMEM,
		"sealed_bytes":         sealedBytes,
		"pause_ns":             pause.Nanoseconds(),
		"prepare_ns":           prepare,
		"seal_ns":              seals,
		"seal_calls":           atResumeRAM.Seal.Count - beforeRAM.Seal.Count + atResumePMEM.Seal.Count - beforePMEM.Seal.Count,
		"longest_seal_ns":      max(atResumeRAM.Seal.MaxNS, atResumePMEM.Seal.MaxNS),
		"protect_ns":           protects,
		"protect_commands":     atResumeRAM.Protect.Count - beforeRAM.Protect.Count + atResumePMEM.Protect.Count - beforePMEM.Protect.Count,
		"protected_pages_ram":  protectedRAM,
		"protected_pages_pmem": protectedPMEM,
		"protected_bytes":      protectedRAM*b.ramPageSize() + protectedPMEM*b.pmemPageSize(),
		"seal_bookkeeping_ns":  seals - protects,
		// The walk behind the pause: moving each sealed page into the
		// checkpoint, which runs with the guest running and holding its region,
		// so a fault of that region waits for it and the VM does not.
		"seal_walk_ns": int64(afterRAM.SealWalk.TotalNS-beforeRAM.SealWalk.TotalNS) +
			int64(afterPMEM.SealWalk.TotalNS-beforePMEM.SealWalk.TotalNS),
		"vmm_pause_save_ns": prepare - seals,
		"resume_ns":         resumed.Sub(prepared).Nanoseconds(),
		"publish_ns":        time.Since(published).Nanoseconds(),
		"state_bytes":       len(state),
		"vmm_mappings":      vmas,
		// Per MiB rather than per page: a pause covers pages of both geometries
		// and there is no page the two of them share.
		"pause_ns_per_mib": 0,
		"seal_ns_per_mib":  0,
	}
	if mib := int64(sealedBytes >> 20); mib > 0 {
		extra["pause_ns_per_mib"] = pause.Nanoseconds() / mib
		extra["seal_ns_per_mib"] = seals / mib
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
	for _, kind := range []vmmemory.RegionKind{vmmemory.Ram, vmmemory.Pmem} {
		cfg := b.pagers.configs[kind]
		t.Logf("%s pager: page=%d resident=%d pages (%d MiB) dirty=%d logical=%d read-ahead=%d write-ahead=%d",
			kind, cfg.PageSize, cfg.ResidentPages, uint64(cfg.ResidentPages)*cfg.PageSize>>20,
			cfg.DirtyPages, cfg.LogicalPages, cfg.ReadAheadPages, cfg.WriteAheadPages)
	}
	t.Logf("guest: ram=%d MiB root=%d MiB scenarios=%v", b.ramBytes>>20, b.rootBytes>>20, b.scenarios.names())

	b.appendRecord(benchRecord{Scenario: "configuration", Kind: "config", Extra: b.configuration()})

	template := b.createVM(ctx, "template")
	t.Cleanup(func() { _ = template.Close(context.Background()) })
	start := b.sample(ctx)
	elapsed, written, imageBytes := b.ingest(ctx, template)
	b.record(ctx, "ingest-template", "sproutfs", start, nil,
		map[string]any{"ingest_ns": elapsed.Nanoseconds(), "written_bytes": written,
			"image_bytes": imageBytes, "root_bytes": b.rootBytes, "image_path": b.imagePath})

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
		// Every fork of this run comes off the one point, taken and given up
		// one after another, so the benchmark holds it for its whole life as a
		// host holds a fan-out's: a point its last child retired refuses the
		// next.
		if err := point.Hold(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := point.Retire(context.Background()); err != nil {
				t.Error(err)
			}
		})
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
		sibling, siblingVM, siblingConsole, _, _ := b.fork(ctx, origin, "sibling")
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

	// Scenario 8: a seeded database, forked per test run, each fork updating it
	// at random.
	if b.scenarios["db-fork"] {
		b.dbFork(ctx, origin)
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
// starts from the capture's checkpoint. "repository" is the cold and the warm
// search and read of the repository,
// and "baseline" the plain-Firecracker record of each other selected scenario.
var benchScenarios = []struct {
	name, needs string
	// optIn marks a scenario a run takes only when it names it: one that
	// finds things out rather than times them, at a cost no timed run should
	// carry.
	optIn bool
}{
	{"boot", "", false},
	{"pnpm-install", "boot", false},
	{"repository", "boot", false},
	{"capture", "boot", false},
	{"restore-cold", "capture", false},
	{"restore-warm", "capture", false},
	{"fork-fanout", "capture", false},
	{"fork-diagnostics", "fork-fanout", true},
	{"steady-state", "capture", false},
	{"db-fork", "capture", false},
	{"baseline", "", false},
}

// scenarioSet is the scenarios one run measures.
type scenarioSet map[string]bool

// selectedScenarios reads SPROUTFS_BENCH_SCENARIOS, a comma-separated list of
// scenario names; unset, a run takes every one but those that are opt-in. A
// name it does not know, or a scenario without the one it needs, fails the run
// rather than measuring something other than what was asked for.
func selectedScenarios(t *testing.T) scenarioSet {
	t.Helper()
	selected := scenarioSet{}
	value := os.Getenv("SPROUTFS_BENCH_SCENARIOS")
	for _, scenario := range benchScenarios {
		selected[scenario.name] = value == "" && !scenario.optIn
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
	config := map[string]any{
		"date":                    time.Now().UTC().Format(time.RFC3339),
		"host_kernel":             strings.TrimSpace(string(release)),
		"revision":                os.Getenv("SPROUTFS_BENCH_REVISION"),
		"guest_ram_bytes":         b.ramBytes,
		"guest_pmem_bytes":        b.rootBytes,
		"vcpus":                   benchGuestVCPUs(),
		"host_page_size":          os.Getpagesize(),
		"scenarios":               b.scenarios.names(),
		"shared_memory_limit":     b.resources.Stats().Limit,
		"max_write_bytes":         benchMaxWriteBytes,
		"object_store":            b.objectStoreKind,
		"object_get_latency_ns":   b.objectLatency.Get.Nanoseconds(),
		"object_put_latency_ns":   b.objectLatency.Put.Nanoseconds(),
		"object_bytes_per_second": b.objectLatency.BytesPerSecond,
		"kernel":                  b.kernel,
		"firecracker":             b.binary,
		"guest_image":             b.imagePath,
		"workload_install":        workloadInstall,
		"workload_test":           workloadTest(),
		"workload_grep":           workloadGrep,
		"workload_read_tree":      workloadCat,
		"workload_restore":        workloadTrue,
		"forks":                   b.forks(),
		"steady_guests":           steadyGuests,
		"steady_period_ns":        b.steadyPeriod().Nanoseconds(),
		"db_keys":                 b.dbKeys(),
		"db_value_bytes":          dbValueBytes,
		"db_update_steps":         dbUpdateSteps,
		"fault_workers":           16,
		"queue_pages":             benchQueuePages,
		"boot_args":               bootArgs(benchBootArgs),
		"baseline_boot_args":      bootArgs(benchPlainBootArgs),
		"baseline_huge_pages":     os.Getenv("SPROUTFS_BENCH_PLAIN_HUGE_PAGES"),
	}
	// Each pager's own budgets, named for its kind and given in its own pages
	// and in bytes: the pages say what the pager admits against, the bytes are
	// what a reader may compare between the two or against another run's.
	for _, kind := range []vmmemory.RegionKind{vmmemory.Ram, vmmemory.Pmem} {
		cfg := b.pagers.configs[kind]
		for name, pages := range map[string]int{"resident": cfg.ResidentPages, "dirty": cfg.DirtyPages,
			"logical": cfg.LogicalPages, "read_ahead": cfg.ReadAheadPages, "write_ahead": cfg.WriteAheadPages} {
			config[kind.String()+"_"+name+"_pages"] = pages
			config[kind.String()+"_"+name+"_bytes"] = uint64(pages) * cfg.PageSize
		}
		config[kind.String()+"_page_size"] = cfg.PageSize
	}
	return config
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
// the qualification host's guests are: a fork's test build there is tens of
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

// fork restores one child of the origin and returns it ready to take commands,
// with what its restore divided into: taking the handle off the parent's point,
// and then every phase of the start itself.
func (b *benchmark) fork(ctx context.Context, origin *forkOrigin, id string) (*vmmachine.Process, *volume.VM, *console, startTiming, time.Duration) {
	b.t.Helper()
	handle := time.Now()
	vm, err := b.manager.Fork(ctx, id, origin.point)
	if err != nil {
		b.t.Fatalf("fork %s: %v", id, err)
	}
	forked := time.Since(handle)
	p, c, timing := b.start(ctx, vm, origin.state)
	return p, vm, c, timing, forked
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
	// What each of those restores was made of. A managed fork's restore is the
	// larger half of its first output, so it is recorded in its phases rather
	// than as one duration: the handle on the parent's point, the VMM process,
	// the snapshot load, the pager's attach of each region and the populate
	// inside it.
	restorePhases := make([]map[string]any, count)
	for index := range children {
		at := time.Now()
		p, vm, c, timing, forked := b.fork(ctx, origin, "fanout-"+strconv.Itoa(index))
		restoreEach[index] = time.Since(at).Nanoseconds()
		phases := timing.phases()
		phases["fork_ns"] = forked.Nanoseconds()
		phases["vmm_start_ns"] = timing.VMMStartNS
		phases["to_ready_ns"] = timing.ToReadyNS
		restorePhases[index] = phases
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
			timing, err := c.runObserved(ctx, workloadTest(), func() {
				firstOutput[index] = time.Since(issued).Nanoseconds()
			})
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
	// pages it maps, the pages whose bytes are its own, and the resident pages
	// another region still maps, which is the sharing this fork has kept. A
	// larger page copies more of what a fork writes into pages of its own, and
	// the shared count is what that costs on the other side. The same three in
	// bytes, because page counts of different geometries cannot be compared and
	// the whole point of the record is to compare them — which is also why the
	// fork's own totals across its two regions are bytes and nothing else.
	privateBytes := make([]uint64, count)
	residentBytes := make([]uint64, count)
	regionPages := make([]map[string]map[string]uint64, count)
	// How many mappings each fork's VMM holds, which is what a region's runs
	// and gaps cost in the address space.
	forkMappings := make([]int, count)
	for index, item := range children {
		regionPages[index] = map[string]map[string]uint64{}
		forkMappings[index] = countMappings(item.process.PID())
		for name, region := range item.process.Regions() {
			stats, err := region.Stats(ctx)
			if err != nil {
				b.t.Fatal(err)
			}
			regionPages[index][name] = map[string]uint64{
				"page_size":      region.PageSize(),
				"resident_pages": uint64(stats.ResidentPages), "private_pages": uint64(stats.PrivatePages),
				"shared_pages":   uint64(stats.SharedPages),
				"resident_bytes": stats.ResidentBytes(), "private_bytes": stats.PrivateBytes(),
				"shared_bytes": stats.SharedBytes()}
			privateBytes[index] += stats.PrivateBytes()
			residentBytes[index] += stats.ResidentBytes()
		}
	}
	b.record(ctx, "fork-fanout", "sproutfs", start, nil, map[string]any{
		"forks":               count,
		"command":             workloadTest(),
		"restore_all_ns":      restored.Sub(start.at).Nanoseconds(),
		"restore_each_ns":     restoreEach,
		"restore_phases":      restorePhases,
		"max_restore_ns":      maxOf(restoreEach),
		"first_output_ns":     firstOutput,
		"total_ns":            total,
		"max_first_output":    maxOf(firstOutput),
		"max_total_ns":        maxOf(total),
		"fork_private_bytes":  privateBytes,
		"fork_resident_bytes": residentBytes,
		"fork_region_pages":   regionPages,
		"fork_vmm_mappings":   forkMappings,
	})
	if b.scenarios["fork-diagnostics"] {
		forks := make([]forkedVM, count)
		for index, item := range children {
			forks[index] = forkedVM{item.process, item.vm}
		}
		b.forkDiagnostics(ctx, origin, forks)
	}
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

// forkedVM is one running fork: its VMM and its volumes.
type forkedVM struct {
	process *vmmachine.Process
	vm      *volume.VM
}

// forkDiagnostics is what a fan-out's forks are made of once their command has
// finished, which is a scenario of its own because finding out costs far more
// than the fan-out: every page each fork owns, and how those pages lie in its
// RAM; which 4 KiB blocks of them it changed, against a fork of the same point
// that never ran; and a checkpoint of each fork, which uploads everything it
// owns. On an 8-processor host these took 25 minutes and 15 GB of heap beside a
// fan-out of 19, so a run that times the fan-out leaves them out.
func (b *benchmark) forkDiagnostics(ctx context.Context, origin *forkOrigin, children []forkedVM) {
	b.t.Helper()
	count := len(children)
	start := b.sample(ctx)
	// Which pages those are, so that what a fork wrote can be looked up in the
	// image: a page number times that region's page size is an offset into it.
	ownPages := make([]map[string][]uint64, count)
	// How the private pages of the fork's RAM lie: the runs they form, the gaps
	// between them and the 2 MiB ranges they fall in. The page-geometry plan
	// sets the gap a store closes from these, so they are measured here rather
	// than chosen.
	ramGeometry := make([]*privateGeometry, count)
	for index, item := range children {
		ownPages[index] = map[string][]uint64{}
		for name, region := range item.process.Regions() {
			unpublished, err := region.Unpublished()
			if err != nil {
				b.t.Fatal(err)
			}
			slices.Sort(unpublished)
			ownPages[index][name] = unpublished
			if region.Kind() == vmmemory.Ram {
				ramGeometry[index] = newPrivateGeometry(unpublished, region.PageSize())
			}
		}
	}
	// Which 4 KiB blocks of those pages a fork changed, against a fork of the
	// same point that never ran. A page a fork owns with no block changed took a
	// write fault and no write; at a 4 KiB RAM page a page is one block, and at
	// the root's 2 MiB page it is 512 of them.
	pristine, err := b.manager.Fork(ctx, "fanout-pristine", origin.point)
	if err != nil {
		b.t.Fatal(err)
	}
	const block = 4096
	changedBlocks := make([]map[string][]uint64, count)
	// How many blocks of each page a fork owns it changed, in every region: a
	// root page's blocks are too many to list, and the count is what says
	// whether the page was written at all.
	changedCounts := make([]map[string]map[string]int, count)
	for index, item := range children {
		changedBlocks[index] = map[string][]uint64{}
		changedCounts[index] = map[string]map[string]int{}
		for name, region := range item.process.Regions() {
			changedCounts[index][name] = map[string]int{}
			pageSize := region.PageSize()
			after := make([]byte, pageSize)
			// The pristine bytes are read a 2 MiB window at a time, the unit the
			// store serves them in: a page at a time is a store read per page
			// wherever nothing warmed the host's cache first, which is what a
			// fork that mapped idle pages instead of loading them leaves, and
			// those reads are this record's own cost and not the fan-out's.
			const window = 2 << 20
			span := max(uint64(window), pageSize)
			size := pristine.Volume(name).Size()
			var windowStart uint64
			var windowBytes []byte
			for _, page := range ownPages[index][name] {
				held, _, err := region.ReadResident(ctx, page, after)
				if err != nil {
					b.t.Fatal(err)
				}
				if !held {
					b.t.Fatalf("fork %d no longer holds its own %s page %d", index, name, page)
				}
				at := page * pageSize
				if windowBytes == nil || at < windowStart || at >= windowStart+uint64(len(windowBytes)) {
					windowStart = at / span * span
					windowBytes = make([]byte, min(span, size-windowStart))
					if err := pristine.Volume(name).Read(ctx, windowStart, windowBytes); err != nil {
						b.t.Fatal(err)
					}
				}
				before := windowBytes[at-windowStart : at-windowStart+pageSize]
				changed := []uint64{}
				for offset := 0; offset < len(after); offset += block {
					if !bytes.Equal(before[offset:offset+block], after[offset:offset+block]) {
						changed = append(changed, (page*pageSize+uint64(offset))/block)
					}
				}
				changedCounts[index][name][strconv.FormatUint(page, 10)] = len(changed)
				if name == "root" {
					changedBlocks[index][strconv.FormatUint(page, 10)] = changed
				}
			}
		}
	}
	if err := pristine.Close(ctx); err != nil {
		b.t.Fatal(err)
	}
	// A checkpoint of each fork, which is where a page the fork never stored
	// into stops being its own: what the settle dropped, what the checkpoint
	// published, and what each region holds and shares once it landed.
	forkCheckpoints := make([]map[string]any, count)
	for index, item := range children {
		ckpt, _ := b.capture(ctx, item.process, item.vm)
		published, publishedBytes := ckpt.Sealed()
		after := map[string]map[string]uint64{}
		for name, region := range item.process.Regions() {
			stats, err := region.Stats(ctx)
			if err != nil {
				b.t.Fatal(err)
			}
			after[name] = map[string]uint64{"page_size": region.PageSize(),
				"resident_pages": uint64(stats.ResidentPages), "private_pages": uint64(stats.PrivatePages),
				"shared_pages":   uint64(stats.SharedPages),
				"resident_bytes": stats.ResidentBytes(), "private_bytes": stats.PrivateBytes(),
				"shared_bytes": stats.SharedBytes()}
		}
		forkCheckpoints[index] = map[string]any{
			"unchanged_pages": ckpt.Unchanged(),
			"published_pages": published,
			"published_bytes": publishedBytes,
			"region_pages":    after,
		}
	}
	b.record(ctx, "fork-diagnostics", "sproutfs", start, nil, map[string]any{
		"forks":               count,
		"fork_checkpoints":    forkCheckpoints,
		"fork_changed_blocks": changedBlocks,
		"fork_changed_counts": changedCounts,
		"fork_own_pages":      ownPages,
		"fork_ram_geometry":   ramGeometry,
	})
}

// runBuckets and runBucketUpper are the fixed buckets a private run's length
// and a gap between two of them are counted in: a page alone, a handful, and so
// on up to a whole 2 MiB range of 4 KiB pages. The last bucket holds anything
// longer, which a range of 512 pages cannot produce but a coarser page could.
const runBuckets = 6

var runBucketUpper = [runBuckets]int{1, 4, 16, 64, 256, 512}

// runHistogram counts runs or gaps by length in those buckets, so the records
// of two runs are directly comparable; the boundaries travel with the histogram
// rather than being named once in whatever reads it.
type runHistogram struct {
	Buckets [runBuckets]int `json:"buckets"`
	Upper   []int           `json:"bucket_upper"`
}

func newRunHistogram() runHistogram { return runHistogram{Upper: runBucketUpper[:]} }

func (h *runHistogram) add(length int) {
	for i, upper := range runBucketUpper {
		if length <= upper || i == runBuckets-1 {
			h.Buckets[i]++
			return
		}
	}
}

// privateGeometry is how one region's private pages lie in it: the runs of
// consecutive pages they form, the gaps between consecutive runs of one
// 2 MiB-aligned range, and how many ranges hold any private page at all. A run
// is what one mapping covers under the page-geometry plan — a private page
// lives at its own offset within its range's extent, so private pages adjacent
// in the guest are adjacent in the arena and are one mapping, and a run never
// crosses a range boundary — and the gaps are what the plan sets the distance a
// store closes from. A range that is at least half private is one the plan
// would make whole, so it is counted as well as the ranges touched at all.
type privateGeometry struct {
	PageSize   uint64       `json:"page_size"`
	RangeBytes uint64       `json:"range_bytes"`
	Pages      int          `json:"private_pages"`
	Runs       int          `json:"runs"`
	RunLengths runHistogram `json:"run_lengths"`
	Gaps       runHistogram `json:"gaps"`
	Ranges     int          `json:"ranges"`
	HalfRanges int          `json:"half_private_ranges"`
}

// newPrivateGeometry reads that geometry off one region's own page numbers,
// which must be sorted. The pages are the region's own, not the volume's: a
// page a checkpoint has published is shared again and is no longer a mapping of
// this fork's alone.
func newPrivateGeometry(pages []uint64, pageSize uint64) *privateGeometry {
	perRange := max(benchRangeBytes/pageSize, 1)
	g := &privateGeometry{PageSize: pageSize, RangeBytes: benchRangeBytes, Pages: len(pages),
		RunLengths: newRunHistogram(), Gaps: newRunHistogram()}
	for start := 0; start < len(pages); {
		// The private pages of one range, which a sorted list holds together.
		which := pages[start] / perRange
		end := start
		for end < len(pages) && pages[end]/perRange == which {
			end++
		}
		g.Ranges++
		if uint64(end-start)*2 >= perRange {
			g.HalfRanges++
		}
		runStart := start
		for i := start + 1; i <= end; i++ {
			if i < end && pages[i] == pages[i-1]+1 {
				continue
			}
			g.Runs++
			g.RunLengths.add(i - runStart)
			if i < end {
				g.Gaps.add(int(pages[i] - pages[i-1] - 1))
			}
			runStart = i
		}
		start = end
	}
	return g
}

// TestPrivateGeometryCountsRunsWithinRanges. A run is what one mapping covers,
// so it stops at a 2 MiB range's boundary however consecutive the page numbers
// are, and a gap is only ever between two runs of one range.
func TestPrivateGeometryCountsRunsWithinRanges(t *testing.T) {
	// Range 0 holds 0-2 and 10-11, which is two runs seven pages apart; range 1
	// holds two pages alone, 422 apart; range 2 holds one.
	g := newPrivateGeometry([]uint64{0, 1, 2, 10, 11, 600, 1023, 1024}, checkpoint.PageSize4KiB)
	if g.Pages != 8 || g.Runs != 5 || g.Ranges != 3 || g.HalfRanges != 0 {
		t.Fatalf("pages=%d runs=%d ranges=%d half=%d; want 8, 5, 3 and 0",
			g.Pages, g.Runs, g.Ranges, g.HalfRanges)
	}
	if g.RunLengths.Buckets != [runBuckets]int{3, 2, 0, 0, 0, 0} {
		t.Fatalf("run lengths %v; want three runs of one page and two of two to four", g.RunLengths.Buckets)
	}
	if g.Gaps.Buckets != [runBuckets]int{0, 0, 1, 0, 0, 1} {
		t.Fatalf("gaps %v; want one of five to sixteen pages and one of 257 to 512", g.Gaps.Buckets)
	}

	// Page 511 and page 512 are consecutive and in different ranges, so they are
	// two runs and the distance between them is not a gap.
	across := newPrivateGeometry([]uint64{511, 512}, checkpoint.PageSize4KiB)
	if across.Runs != 2 || across.Ranges != 2 || across.Gaps.Buckets != [runBuckets]int{} {
		t.Fatalf("a run crossed a range boundary: %+v", across)
	}

	// Half of a range's 512 pages is where the plan would make the range whole.
	half := make([]uint64, 256)
	for i := range half {
		half[i] = uint64(2 * i)
	}
	whole := newPrivateGeometry(half, checkpoint.PageSize4KiB)
	if whole.Ranges != 1 || whole.HalfRanges != 1 || whole.Runs != 256 {
		t.Fatalf("ranges=%d half=%d runs=%d; want one range, half private, in 256 runs",
			whole.Ranges, whole.HalfRanges, whole.Runs)
	}
	if whole.Gaps.Buckets != [runBuckets]int{255, 0, 0, 0, 0, 0} {
		t.Fatalf("gaps %v; want 255 gaps of a single page", whole.Gaps.Buckets)
	}

	// A 2 MiB page is one page to a range, so nothing it holds has a gap.
	coarse := newPrivateGeometry([]uint64{3, 4, 9}, checkpoint.PageSize2MiB)
	if coarse.Ranges != 3 || coarse.Runs != 3 || coarse.HalfRanges != 3 {
		t.Fatalf("at a 2 MiB page every private page is its own whole range: %+v", coarse)
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
		p, vm, c, _, _ := b.fork(ctx, origin, "steady-"+strconv.Itoa(index))
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
		MemoryMiB: int(b.ramBytes >> 20), VCPUs: benchGuestVCPUs(),
		// SPROUTFS_BENCH_PLAIN_HUGE_PAGES backs the plain guest's memory with
		// huge pages, which measures what the size of a guest's translations
		// costs with no pager involved at all.
		HugePages: os.Getenv("SPROUTFS_BENCH_PLAIN_HUGE_PAGES")}

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
	// The root as it was when the memory was saved. A restore runs on the root
	// it is given, so the clones below are cut from this copy rather than from a
	// root the cold restore has since written to.
	atSnapshot := filepath.Join(b.work, "baseline-root.snapshot.ext4")
	if b.scenarios["fork-fanout"] {
		if err := copySparse(root, atSnapshot); err != nil {
			b.t.Fatal(err)
		}
		defer os.Remove(atSnapshot)
	}
	if b.scenarios["restore-cold"] {
		b.baselineRestore(ctx, config, statePath, memoryPath, memoryInfo.Size())
	}
	if b.scenarios["fork-fanout"] {
		b.baselineFanOut(ctx, config, statePath, memoryPath, atSnapshot)
	}
	// Last, once every other plain VM is gone: the database takes a guest of
	// its own and as many clones of it as the fan-out had.
	if b.scenarios["db-fork"] {
		b.baselineDB(ctx, config)
	}
}

// baselineRestore is the plain restore: one VM from the memory file, on the
// root the snapshot was taken over.
func (b *benchmark) baselineRestore(ctx context.Context, config plainConfig, statePath, memoryPath string, memoryBytes int64) {
	b.t.Helper()

	restoreConfig := config
	restoreConfig.SnapshotPath = statePath
	restoreConfig.MemoryPath = memoryPath
	at := time.Now()
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
		Extra: map[string]any{"memory_file_bytes": memoryBytes}})
}

// baselineFanOut is what plain Firecracker offers in place of a fork: as many
// VMs as the managed fan-out has forks, each restored from the one memory file
// — which Firecracker maps privately, so the clones share its clean pages
// through the host's page cache and copy what they write — and each over a copy
// of the root of its own, because a block device cannot be shared by writers.
// They run the fan-out's command together. What is recorded beside the managed
// fan-out's numbers is what a clone costs before it runs — the copy of its root
// — how long each took to its first output and to finish, and what the kernel
// accounts to each VMM once it has: the memory that is its alone and its share
// of what the clones hold in common.
func (b *benchmark) baselineFanOut(ctx context.Context, config plainConfig, statePath, memoryPath, atSnapshot string) {
	b.t.Helper()
	count := b.forks()
	type clone struct {
		vm      *plainVM
		console *console
		root    string
	}
	clones := make([]clone, count)
	copyEach := make([]int64, count)
	restoreEach := make([]int64, count)
	start := time.Now()
	for index := range clones {
		root := filepath.Join(b.work, "baseline-clone-"+strconv.Itoa(index)+".ext4")
		at := time.Now()
		if err := copySparse(atSnapshot, root); err != nil {
			b.t.Fatal(err)
		}
		defer os.Remove(root)
		copyEach[index] = time.Since(at).Nanoseconds()
		cloneConfig := config
		cloneConfig.SnapshotPath, cloneConfig.MemoryPath, cloneConfig.CloneRoot = statePath, memoryPath, root
		at = time.Now()
		vm, err := startPlainVM(ctx, cloneConfig)
		if err != nil {
			b.t.Fatalf("baseline clone %d: %v", index, err)
		}
		defer vm.Close()
		restoreEach[index] = time.Since(at).Nanoseconds()
		c := newConsole(vm)
		if err := c.skipExisting(); err != nil {
			b.t.Fatal(err)
		}
		clones[index] = clone{vm, c, root}
	}
	restored := time.Now()
	firstOutput := make([]int64, count)
	total := make([]int64, count)
	errs := make([]error, count)
	exits := make([]int, count)
	var wg sync.WaitGroup
	for index := range clones {
		wg.Go(func() {
			issued := time.Now()
			timing, err := clones[index].console.runObserved(ctx, workloadTest(), func() {
				firstOutput[index] = time.Since(issued).Nanoseconds()
			})
			errs[index], exits[index] = err, timing.Exit
			total[index] = time.Since(issued).Nanoseconds()
		})
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			b.t.Fatalf("baseline clone %d: %v", index, err)
		}
		if exits[index] != 0 {
			b.t.Fatalf("baseline clone %d exited %d", index, exits[index])
		}
	}
	memory := make([]map[string]int64, count)
	for index, item := range clones {
		rollup, err := item.vm.memoryRollup()
		if err != nil {
			b.t.Fatal(err)
		}
		memory[index] = rollup
	}
	b.appendRecord(benchRecord{Scenario: "fork-fanout", Kind: "baseline", WallNS: time.Since(start).Nanoseconds(),
		Extra: map[string]any{
			"forks":            count,
			"command":          workloadTest(),
			"root_copy_ns":     copyEach,
			"restore_each_ns":  restoreEach,
			"restore_all_ns":   restored.Sub(start).Nanoseconds(),
			"first_output_ns":  firstOutput,
			"total_ns":         total,
			"max_first_output": maxOf(firstOutput),
			"max_total_ns":     maxOf(total),
			"clone_memory":     memory,
		}})
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
	b.t.Logf("%s/%s wall=%s guest=%+v ram=%+v pmem=%+v objects=%+v extra=%v", rec.Kind, rec.Scenario,
		time.Duration(rec.WallNS), rec.Guest, rec.MemoryRAM, rec.MemoryPMEM, rec.Objects, rec.Extra)
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
	}{{"pnpm-install", "pnpm-install", boundInstallRatio}} {
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

	if b.scenarios["fork-fanout"] && b.scenarios["baseline"] {
		fanout := b.find("fork-fanout", "sproutfs")
		highest, ok := fanout.Extra["max_first_output"].(int64)
		if !ok {
			t.Fatalf("the fan-out recorded no first output: %v", fanout.Extra["max_first_output"])
		}
		plain, ok := b.find("fork-fanout", "baseline").Extra["max_first_output"].(int64)
		if !ok || plain <= 0 {
			t.Fatalf("the plain fan-out recorded no first output")
		}
		got := float64(highest) / float64(plain)
		t.Logf("fork first output: managed %s, baseline %s, ratio %.2f (bound %.2f)",
			time.Duration(highest), time.Duration(plain), got, boundForkFirstOutputRatio)
		if got > boundForkFirstOutputRatio {
			t.Errorf("slowest fork produced its first output after %s, %.2fx the plain clone's %s, above the %.2fx bound",
				time.Duration(highest), got, time.Duration(plain), boundForkFirstOutputRatio)
		}
	}

	if b.scenarios["capture"] {
		capture := b.find("capture", "sproutfs")
		t.Logf("capture: %v", capture.Extra)
	}
}
