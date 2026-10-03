package host_test

import (
	"strings"
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
)

// An embedder runs the host as a library, so it reads the host's Status and
// serves the exposition itself. The numbers it has to alert on are there: what
// losing the host would cost, pages spilled to disk, and object-store failures.
func TestMetricsAreWhatAnEmbedderServes(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{
		VMs: []hostapi.VM{{ID: "vm-1", LossWindow: 90 * time.Second, Waiting: true}},
		Pager: hostapi.Pager{
			RAM: hostapi.PagerKind{Spills: 5},
		},
		Store: hostapi.Store{Put: hostapi.StoreCount{Calls: 4, Failures: 2}},
	})
	for _, want := range []string{
		"sproutfs_loss_window_seconds 90",
		"sproutfs_vms_waiting 1",
		`sproutfs_pager_spills_total{kind="ram"} 5`,
		`sproutfs_store_failures_total{operation="put"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}

// The disk limiter's choice: the goal that binds, what each user is promised
// and holds, the cache's share, whether the promises fit, and the write budget.
func TestMetricsExposeTheDiskLimiter(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{Disk: hostapi.Disk{
		Binding: "free-percent", TotalBytes: 1000, AvailableBytes: 600, SmoothFreeBytes: 610,
		FloorBytes: 100, BandBytes: 20,
		Promises:       []hostapi.DiskPromise{{Name: "spill-ram", PromisedBytes: 300, AllocatedBytes: 40}},
		CacheHeldBytes: 50, CacheShareBytes: 400, Unready: "too much promised",
		Writes: hostapi.DiskWrites{WrittenBytes: 7000, AdmittedBytes: 3000, LeftBytes: -5,
			Refused: []uint64{4, 3, 0, 0}},
	}})
	for _, want := range []string{
		"sproutfs_disk_total_bytes 1000",
		"sproutfs_disk_available_bytes 600",
		"sproutfs_disk_smooth_free_bytes 610",
		"sproutfs_disk_floor_bytes 100",
		"sproutfs_disk_band_bytes 20",
		`sproutfs_disk_promised_bytes{user="spill-ram"} 300`,
		`sproutfs_disk_allocated_bytes{user="spill-ram"} 40`,
		"sproutfs_disk_cache_share_bytes 400",
		"sproutfs_disk_cache_held_bytes 50",
		`sproutfs_disk_binding{goal="free-percent"} 1`,
		`sproutfs_disk_binding{goal="free-bytes"} 0`,
		"sproutfs_disk_promises_fit 0",
		"sproutfs_disk_device_written_bytes_total 7000",
		"sproutfs_disk_cache_admitted_bytes_total 3000",
		"sproutfs_disk_write_budget_left_bytes -5",
		`sproutfs_disk_cache_writes_refused_total{priority="0"} 4`,
		`sproutfs_disk_cache_writes_refused_total{priority="1"} 3`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}

// The page cache's disk: what it holds, the reads it served without the
// object store, and what the host read back from its file when it started. A
// host that keeps no cache disk reports zeroes.
func TestMetricsExposeThePageCachesDisk(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{CacheDisk: &hostapi.CacheDisk{File: "cache-1", Regions: 12, Entries: 900,
		Hits: 4321, Lost: 2, Evicted: 5, Refused: 7, Opened: hostapi.CacheDiskOpened{FromTables: 11, Scanned: 1,
			GivenBack: 3}}})
	for _, want := range []string{
		"sproutfs_cache_disk_regions 12",
		"sproutfs_cache_disk_entries 900",
		"sproutfs_cache_disk_hits_total 4321",
		"sproutfs_cache_disk_lost_total 2",
		"sproutfs_cache_disk_evicted_regions_total 5",
		"sproutfs_cache_disk_writes_refused_total 7",
		`sproutfs_cache_disk_opened_regions{how="tables"} 11`,
		`sproutfs_cache_disk_opened_regions{how="scanned"} 1`,
		`sproutfs_cache_disk_opened_regions{how="given-back"} 3`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
	body = hostapi.Metrics(hostapi.Status{})
	for _, want := range []string{"sproutfs_cache_disk_hits_total 0", `sproutfs_cache_disk_opened_regions{how="tables"} 0`} {
		if !strings.Contains(body, want+"\n") {
			t.Fatalf("a host with no cache disk has no %q in its exposition:\n%s", want, body)
		}
	}
}

// Why a guest stopped making progress, and how long faults take, per pager.
// A histogram is cumulative, in seconds, and ends with +Inf, its sum and its
// count, which is what a Prometheus histogram_quantile reads.
func TestMetricsExposeStallsAndFaultLatency(t *testing.T) {
	buckets := make([]uint64, hostapi.LatencyBuckets)
	buckets[0], buckets[3], buckets[hostapi.LatencyBuckets-1] = 2, 1, 1
	body := hostapi.Metrics(hostapi.Status{
		Build: hostapi.Build{Version: "v1.2.3", APIRevision: 1, Arena: "isolated"},
		Pager: hostapi.Pager{RAM: hostapi.PagerKind{DirtyWaits: 4, CheckpointRequests: 3, DirtyStalls: 1,
			WindowWaits: 6, WindowStalls: 2, RefusedMappings: 5, RepeatedFaults: 9, PacedFaults: 7,
			Fault: hostapi.Latency{Count: 4, TotalNS: 9_000_000_000, Buckets: buckets}}},
	})
	for _, want := range []string{
		`sproutfs_build_info{version="v1.2.3",api_revision="1",arena="isolated"} 1`,
		`sproutfs_pager_dirty_waits_total{kind="ram"} 4`,
		`sproutfs_pager_checkpoint_requests_total{kind="ram"} 3`,
		`sproutfs_pager_dirty_stalls_total{kind="ram"} 1`,
		`sproutfs_pager_window_waits_total{kind="ram"} 6`,
		`sproutfs_pager_window_stalls_total{kind="ram"} 2`,
		`sproutfs_pager_refused_mappings_total{kind="ram"} 5`,
		`sproutfs_pager_repeated_faults_total{kind="ram"} 9`,
		`sproutfs_pager_paced_faults_total{kind="ram"} 7`,
		"# TYPE sproutfs_pager_fault_seconds histogram",
		`sproutfs_pager_fault_seconds_bucket{kind="ram",le="1e-06"} 2`,
		`sproutfs_pager_fault_seconds_bucket{kind="ram",le="4e-06"} 2`,
		`sproutfs_pager_fault_seconds_bucket{kind="ram",le="8e-06"} 3`,
		`sproutfs_pager_fault_seconds_bucket{kind="ram",le="4.194304"} 3`,
		`sproutfs_pager_fault_seconds_bucket{kind="ram",le="+Inf"} 4`,
		`sproutfs_pager_fault_seconds_sum{kind="ram"} 9`,
		`sproutfs_pager_fault_seconds_count{kind="ram"} 4`,
		`sproutfs_pager_fault_seconds_count{kind="pmem"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}

// No series names a VM or a tenant: a host runs VMs of many tenants, and a
// label per VM would put their identities into the scraper's label space and
// a series per VM into its storage.
func TestMetricsNameNoVMOrTenant(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{
		Running: []string{"acme/vm-secret"}, Serving: []string{"zeta/vm-moving"},
		Outstanding: map[string]int{"zeta/vm-moving": 3}, Receiving: []string{"zeta/vm-coming"},
		VMs: []hostapi.VM{{ID: "acme/vm-secret", LossWindow: time.Minute, CheckpointInterval: time.Second}},
	})
	for _, identity := range []string{"acme", "zeta", "vm-secret", "vm-moving", "vm-coming"} {
		if strings.Contains(body, identity) {
			t.Fatalf("the exposition names %q:\n%s", identity, body)
		}
	}
}

// What the interval checkpoints did: every attempt, each ending by outcome,
// what they uploaded and what they cost.
func TestMetricsExposeCheckpointOutcomes(t *testing.T) {
	buckets := make([]uint64, hostapi.LatencyBuckets)
	buckets[10] = 3
	body := hostapi.Metrics(hostapi.Status{Checkpoints: hostapi.Checkpoints{Attempts: 9, Published: 3,
		CaptureFailed: 1, PublishFailed: 4, Fenced: 1, UploadedBytes: 4096,
		Pause: hostapi.Latency{Count: 3, TotalNS: 3_000_000, Buckets: buckets}}})
	for _, want := range []string{
		"sproutfs_checkpoint_attempts_total 9",
		`sproutfs_checkpoints_total{outcome="published"} 3`,
		`sproutfs_checkpoints_total{outcome="capture_failed"} 1`,
		`sproutfs_checkpoints_total{outcome="publish_failed"} 4`,
		`sproutfs_checkpoints_total{outcome="fenced"} 1`,
		"sproutfs_checkpoint_uploaded_bytes_total 4096",
		`sproutfs_checkpoint_pause_seconds_bucket{le="0.001024"} 3`,
		"sproutfs_checkpoint_pause_seconds_sum 0.003",
		"sproutfs_checkpoint_pause_seconds_count 3",
		"sproutfs_checkpoint_upload_seconds_count 0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}

// What a host did with its VMs: handovers by outcome, what each received
// guest paid, and the VMs it gave up, by why.
func TestMetricsExposeTheLifecycle(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{Lifecycle: hostapi.Lifecycle{
		Migrations: hostapi.Outcomes{Succeeded: 2, Failed: 1}, Forks: hostapi.Outcomes{Succeeded: 3},
		Receives:  hostapi.Outcomes{Succeeded: 4, Failed: 2},
		ForkPause: hostapi.Latency{Count: 1, TotalNS: 5_000_000, Buckets: bucketAt(13)},
		Deaths:    1, Fenced: 2, Stopped: 3}})
	for _, want := range []string{
		`sproutfs_migrations_total{outcome="succeeded"} 2`,
		`sproutfs_migrations_total{outcome="failed"} 1`,
		`sproutfs_forks_total{outcome="succeeded"} 3`,
		`sproutfs_receives_total{outcome="failed"} 2`,
		`sproutfs_received_pause_seconds_count{kind="migration"} 0`,
		`sproutfs_received_pause_seconds_bucket{kind="fork",le="0.008192"} 1`,
		`sproutfs_received_pause_seconds_sum{kind="fork"} 0.005`,
		`sproutfs_vms_given_up_total{reason="vmm_ended"} 1`,
		`sproutfs_vms_given_up_total{reason="fenced"} 2`,
		`sproutfs_vms_given_up_total{reason="stopped_for_a_bound"} 3`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}

// bucketAt is a histogram's buckets with one observation in bucket i.
func bucketAt(i int) []uint64 {
	buckets := make([]uint64, hostapi.LatencyBuckets)
	buckets[i] = 1
	return buckets
}

// How long the object store takes, per operation, failed calls included.
func TestMetricsExposeStoreLatency(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{Store: hostapi.Store{
		Put: hostapi.StoreCount{Calls: 1, Latency: hostapi.Latency{Count: 1, TotalNS: 20_000_000, Buckets: bucketAt(15)}}}})
	for _, want := range []string{
		"# TYPE sproutfs_store_seconds histogram",
		`sproutfs_store_seconds_bucket{operation="put",le="0.032768"} 1`,
		`sproutfs_store_seconds_sum{operation="put"} 0.02`,
		`sproutfs_store_seconds_count{operation="get"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}

// What a host start costs: the templates a host imported, what each took, and
// every byte of guest image it read.
func TestMetricsExposeTemplateImports(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{Imports: hostapi.Imports{Outcomes: hostapi.Outcomes{Succeeded: 1, Failed: 2},
		Latency:    hostapi.Latency{Count: 1, TotalNS: 156_000_000_000, Buckets: bucketAt(hostapi.LatencyBuckets - 1)},
		ImageBytes: 1 << 30}})
	for _, want := range []string{
		`sproutfs_template_imports_total{outcome="succeeded"} 1`,
		`sproutfs_template_imports_total{outcome="failed"} 2`,
		`sproutfs_template_import_seconds_bucket{le="4.194304"} 0`,
		`sproutfs_template_import_seconds_bucket{le="+Inf"} 1`,
		"sproutfs_template_import_seconds_sum 156",
		"sproutfs_image_read_bytes_total 1073741824",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}
