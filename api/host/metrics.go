package host

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Metrics writes one host's status as Prometheus text: what sproutfs-host
// serves on /metrics, and what an embedder that runs the host as a library
// serves on its own endpoint from the Status it reads. The format is four lines
// per metric and nothing else, so it is written by hand rather than linked: a
// client library would be a dependency, a registry and a set of collectors for
// numbers this host already has in one struct.
//
// Everything here is read from Status, which is the same report the API and the
// orchestrator's survey see. Counters end in _total and are monotonic for one
// process lifetime — a host restart is a host loss, so they start again at zero
// with everything else this process holds.
func Metrics(status Status) string {
	var out strings.Builder
	write := func(name, kind, help string, value any) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, value)
	}
	// What this host runs, as labels on a constant, which is how a dashboard
	// tells hosts apart during a rollout.
	fmt.Fprintf(&out, "# HELP sproutfs_build_info What this host is running.\n# TYPE sproutfs_build_info gauge\n"+
		"sproutfs_build_info{version=%q,api_revision=\"%d\",arena=%q} 1\n",
		status.Build.Version, status.Build.APIRevision, status.Build.Arena)
	write("sproutfs_vms_running", "gauge",
		"VMs this host runs.", len(status.Running))
	write("sproutfs_vms_serving", "gauge",
		"VMs this host has handed over and still serves the pages of.", len(status.Serving))
	// The widest window rather than one series per VM: what an operator alerts
	// on is the worst exposure this host carries, and a gauge per VM would put
	// the deployment's VM identities into the scraper's label space.
	widest, waiting := lossWindow(status.VMs)
	write("sproutfs_loss_window_seconds", "gauge",
		"How long the worst-off VM this host runs has held a write no checkpoint covers, which is what losing this host would cost it in time.",
		widest.Seconds())
	write("sproutfs_vms_waiting", "gauge",
		"VMs past their loss window, whose stores the pager is holding back until a checkpoint of them lands.", waiting)

	// A host runs one pager per kind of memory region, and one for ephemeral
	// disks, so every pager series carries the pager as its kind label: one
	// series each, and comparing RAM against PMEM is a query rather than twice
	// the metrics. Page counts have to be labelled — the pagers run their own
	// pages, so a sum of them would mean nothing — and the bytes are labelled
	// beside them for the same reason a deployment plans for each separately. A
	// host that runs no ephemeral pager reports zeroes for it.
	kinds := []struct {
		name  string
		pager PagerKind
	}{{"ram", status.Pager.RAM}, {"pmem", status.Pager.PMEM}, {"ephemeral", status.Pager.Ephemeral}}
	byKind := func(name, metric, help string, value func(PagerKind) any) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, metric)
		for _, kind := range kinds {
			fmt.Fprintf(&out, "%s{kind=%q} %v\n", name, kind.name, value(kind.pager))
		}
	}
	byKind("sproutfs_pager_page_bytes", "gauge",
		"The page each pager runs, which its page counts here are in.",
		func(p PagerKind) any { return p.PageBytes })
	byKind("sproutfs_pager_arena_pages", "gauge",
		"Pages each pager's arena holds.", func(p PagerKind) any { return p.ArenaPages })
	byKind("sproutfs_pager_resident_pages", "gauge",
		"Pages of each arena that are taken.", func(p PagerKind) any { return p.ResidentPages })
	byKind("sproutfs_pager_dirty_pages", "gauge",
		"Pages of volatile private state no checkpoint has published.",
		func(p PagerKind) any { return p.DirtyPages })
	byKind("sproutfs_pager_logical_pages", "gauge",
		"Pages each pager holds metadata for.", func(p PagerKind) any { return p.LogicalPages })
	write("sproutfs_pager_arena_bytes", "gauge",
		"What the two arenas hold together, which is the only unit their capacities can be added in.",
		status.Pager.ArenaBytes())
	write("sproutfs_guest_committed_bytes", "gauge",
		"Guest RAM the VMs this host runs have between them, resident or not, which is what a placement measures this host by.",
		status.Pager.CommittedBytes)
	byKind("sproutfs_pager_shared_pages_total", "counter",
		"Pages mapped to an already resident identity without a read, which is what a fork inherits.",
		func(p PagerKind) any { return p.SharedPages })
	// The sharing gauges are what the counter above is not — how much sharing is
	// still there, rather than how often it happened.
	byKind("sproutfs_pager_unique_resident_bytes", "gauge",
		"Host memory the pager's arena holds, one resident page counted once however many memory regions map it.",
		func(p PagerKind) any { return p.Sharing.UniqueBytes })
	byKind("sproutfs_pager_mapped_resident_bytes", "gauge",
		"Resident pages summed over the memory regions that map them, counting every alias, which is what this host would hold if nothing shared anything.",
		func(p PagerKind) any { return p.Sharing.MappedBytes })
	byKind("sproutfs_pager_shared_saved_bytes", "gauge",
		"Mapped less unique: the memory this host did not have to find because its guests are reading the same pages.",
		func(p PagerKind) any { return p.Sharing.SavedBytes })
	byKind("sproutfs_pager_faults_total", "counter", "Faults the pager has resolved.",
		func(p PagerKind) any { return p.Faults })
	byKind("sproutfs_pager_evictions_total", "counter", "Pages the pager has evicted.",
		func(p PagerKind) any { return p.Evictions })
	byKind("sproutfs_pager_spills_total", "counter", "Pages the pager has written to its spill file.",
		func(p PagerKind) any { return p.Spills })
	byKind("sproutfs_pager_idle_pages", "gauge",
		"Resident pages no memory region maps, kept for the next one that inherits them.",
		func(p PagerKind) any { return p.IdlePages })
	byKind("sproutfs_pager_loaded_pages_total", "counter", "Pages the pager read from its backing.",
		func(p PagerKind) any { return p.LoadedPages })
	byKind("sproutfs_pager_copy_on_writes_total", "counter", "Stores the pager gave a private copy of a page.",
		func(p PagerKind) any { return p.CopyOnWrites })
	byKind("sproutfs_pager_unmapped_copy_on_writes_total", "counter",
		"Copy-on-writes of a page the storing guest did not map.",
		func(p PagerKind) any { return p.UnmappedCopyOnWrites })
	byKind("sproutfs_pager_unchanged_pages_total", "counter",
		"Private copies a checkpoint found still holding the bytes they were copied from.",
		func(p PagerKind) any { return p.UnchangedPages })
	// What the kernel said of each page fault it reported.
	byKind("sproutfs_pager_read_traps_total", "counter", "Page faults the kernel reported as reads.",
		func(p PagerKind) any { return p.ReadTraps })
	byKind("sproutfs_pager_store_traps_total", "counter",
		"Page faults the kernel reported as stores into a page not in the page tables.",
		func(p PagerKind) any { return p.StoreTraps })
	byKind("sproutfs_pager_protect_traps_total", "counter",
		"Page faults the kernel reported as stores into a write-protected page.",
		func(p PagerKind) any { return p.ProtectTraps })
	byKind("sproutfs_pager_given_back_pages_total", "counter",
		"Unchanged copies given back to the page they were copied from without a checkpoint.",
		func(p PagerKind) any { return p.GivenBackPages })
	byKind("sproutfs_pager_revocations_total", "counter", "Commands that took mappings away from a VMM.",
		func(p PagerKind) any { return p.Revocations })
	byKind("sproutfs_pager_revoked_pages_total", "counter", "Pages those commands took away.",
		func(p PagerKind) any { return p.RevokedPages })
	// What an isolated arena copies between its files, which a shared arena
	// never does.
	byKind("sproutfs_pager_moved_pages_total", "counter",
		"Published pages copied into the tenant's shared file because another memory region inherited them.",
		func(p PagerKind) any { return p.MovedPages })
	byKind("sproutfs_pager_fork_copies_total", "counter",
		"Pages copied into a fork point's file for a child on this host.",
		func(p PagerKind) any { return p.ForkCopies })
	byKind("sproutfs_pager_tampered_total", "counter",
		"Moves whose copy did not hold the bytes the page's upload read.",
		func(p PagerKind) any { return p.Tampered })

	// Why a guest stops making progress: a store held back for the dirty
	// budget or the loss window, and a VMM out of mapping budget or faulting
	// in a loop.
	byKind("sproutfs_pager_dirty_waits_total", "counter", "Stores that waited for the dirty budget.",
		func(p PagerKind) any { return p.DirtyWaits })
	byKind("sproutfs_pager_checkpoint_requests_total", "counter",
		"Checkpoints a store waiting for the dirty budget asked for out of the interval's turn.",
		func(p PagerKind) any { return p.CheckpointRequests })
	byKind("sproutfs_pager_dirty_stalls_total", "counter",
		"Stores no checkpoint could admit, whose VM was stopped.",
		func(p PagerKind) any { return p.DirtyStalls })
	byKind("sproutfs_pager_window_waits_total", "counter",
		"Stores that waited because their VM had held a write no checkpoint covers for longer than the loss window.",
		func(p PagerKind) any { return p.WindowWaits })
	byKind("sproutfs_pager_window_stalls_total", "counter",
		"Stores past the loss window no checkpoint was ever going to cover, whose VM was stopped.",
		func(p PagerKind) any { return p.WindowStalls })
	byKind("sproutfs_pager_refused_mappings_total", "counter",
		"Faults a VMM refused a mapping command for, which is a VMM out of mapping budget.",
		func(p PagerKind) any { return p.RefusedMappings })
	byKind("sproutfs_pager_repeated_faults_total", "counter",
		"Faults a VMM took again on pages already mapped for it.",
		func(p PagerKind) any { return p.RepeatedFaults })
	byKind("sproutfs_pager_paced_faults_total", "counter",
		"Repeated faults that waited for their VMM's budget of them.",
		func(p PagerKind) any { return p.PacedFaults })
	histogramByKind := func(name, help string, value func(PagerKind) Latency) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
		for _, kind := range kinds {
			histogram(&out, name, fmt.Sprintf("kind=%q", kind.name), value(kind.pager))
		}
	}
	histogramByKind("sproutfs_pager_fault_seconds",
		"How long each fault took, from the kernel's report to the guest resuming.",
		func(p PagerKind) Latency { return p.Fault })
	histogramByKind("sproutfs_pager_load_seconds", "How long each read of pages from the backing took.",
		func(p PagerKind) Latency { return p.Load })
	histogramByKind("sproutfs_pager_seal_seconds",
		"How long each checkpoint's write-protection of one memory region took.",
		func(p PagerKind) Latency { return p.Seal })

	// What the interval checkpoints did, which is what makes a running
	// guest durable. The loss window says something is wrong; these say what.
	write("sproutfs_checkpoint_attempts_total", "counter", "Interval checkpoints this host began.",
		status.Checkpoints.Attempts)
	fmt.Fprintf(&out, "# HELP sproutfs_checkpoints_total Interval checkpoints that ended, by how.\n"+
		"# TYPE sproutfs_checkpoints_total counter\n")
	for _, outcome := range []struct {
		name  string
		count uint64
	}{
		{"published", status.Checkpoints.Published}, {"capture_failed", status.Checkpoints.CaptureFailed},
		{"publish_failed", status.Checkpoints.PublishFailed}, {"fenced", status.Checkpoints.Fenced},
	} {
		fmt.Fprintf(&out, "sproutfs_checkpoints_total{outcome=%q} %d\n", outcome.name, outcome.count)
	}
	write("sproutfs_checkpoint_uploaded_bytes_total", "counter",
		"Bytes the published interval checkpoints uploaded.", status.Checkpoints.UploadedBytes)
	fmt.Fprintf(&out, "# HELP sproutfs_checkpoint_pause_seconds How long each interval checkpoint paused its guest.\n"+
		"# TYPE sproutfs_checkpoint_pause_seconds histogram\n")
	histogram(&out, "sproutfs_checkpoint_pause_seconds", "", status.Checkpoints.Pause)
	fmt.Fprintf(&out, "# HELP sproutfs_checkpoint_upload_seconds How long each interval checkpoint took to publish, behind the running guest.\n"+
		"# TYPE sproutfs_checkpoint_upload_seconds histogram\n")
	histogram(&out, "sproutfs_checkpoint_upload_seconds", "", status.Checkpoints.Upload)

	// What the host did with its VMs. A drain's migrations happen just before
	// its host exits and are never scraped there: its destinations' receives
	// count them.
	for _, handovers := range []struct {
		name, help string
		outcomes   Outcomes
	}{
		{"sproutfs_migrations_total", "Migrations this host began as the source, by outcome.", status.Lifecycle.Migrations},
		{"sproutfs_forks_total", "Forks this host took of a VM it runs, by outcome.", status.Lifecycle.Forks},
		{"sproutfs_receives_total", "Migrated and forked VMs this host took in, by outcome.", status.Lifecycle.Receives},
	} {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s counter\n", handovers.name, handovers.help, handovers.name)
		fmt.Fprintf(&out, "%s{outcome=\"succeeded\"} %d\n%s{outcome=\"failed\"} %d\n", handovers.name,
			handovers.outcomes.Succeeded, handovers.name, handovers.outcomes.Failed)
	}
	fmt.Fprintf(&out, "# HELP sproutfs_received_pause_seconds What each guest this host took in paid, from the source's pause to its resume here.\n"+
		"# TYPE sproutfs_received_pause_seconds histogram\n")
	histogram(&out, "sproutfs_received_pause_seconds", `kind="migration"`, status.Lifecycle.MigrationPause)
	histogram(&out, "sproutfs_received_pause_seconds", `kind="fork"`, status.Lifecycle.ForkPause)
	fmt.Fprintf(&out, "# HELP sproutfs_vms_given_up_total VMs this host gave up, by why.\n"+
		"# TYPE sproutfs_vms_given_up_total counter\n"+
		"sproutfs_vms_given_up_total{reason=\"vmm_ended\"} %d\n"+
		"sproutfs_vms_given_up_total{reason=\"fenced\"} %d\n"+
		"sproutfs_vms_given_up_total{reason=\"stopped_for_a_bound\"} %d\n",
		status.Lifecycle.Deaths, status.Lifecycle.Fenced, status.Lifecycle.Stopped)

	// Template imports: the imports this host wrote, what each took, and every
	// byte of guest image it read, which is what a host start costs.
	fmt.Fprintf(&out, "# HELP sproutfs_template_imports_total Templates this host imported, by outcome.\n"+
		"# TYPE sproutfs_template_imports_total counter\n"+
		"sproutfs_template_imports_total{outcome=\"succeeded\"} %d\n"+
		"sproutfs_template_imports_total{outcome=\"failed\"} %d\n",
		status.Imports.Outcomes.Succeeded, status.Imports.Outcomes.Failed)
	fmt.Fprintf(&out, "# HELP sproutfs_template_import_seconds How long each import took, from its image's digest to its pin.\n"+
		"# TYPE sproutfs_template_import_seconds histogram\n")
	histogram(&out, "sproutfs_template_import_seconds", "", status.Imports.Latency)
	write("sproutfs_image_read_bytes_total", "counter",
		"Guest image bytes this host read, for a digest or an import.", status.Imports.ImageBytes)

	write("sproutfs_pages_requests_total", "counter",
		"Page requests this host's migration page server has answered.", status.Pages.Requests)
	write("sproutfs_pages_served_total", "counter", "Pages served to a peer.", status.Pages.Served)
	write("sproutfs_pages_absent_total", "counter", "Page requests for a page this host does not hold.", status.Pages.Absent)
	write("sproutfs_pages_refused_total", "counter", "Page requests refused, which is a peer at its budget.", status.Pages.Refused)

	write("sproutfs_memory_limit_bytes", "gauge", "The RAM allotment the pager takes its pages from.",
		status.Resources.MemoryLimit)
	write("sproutfs_memory_used_bytes", "gauge", "How much of that allotment is taken.", status.Resources.MemoryUsed)
	write("sproutfs_cache_limit_bytes", "gauge", "The page cache's own cap, which nothing else draws on.",
		status.Resources.CacheLimit)
	write("sproutfs_cache_used_bytes", "gauge", "How much of the page cache is resident.", status.Resources.CacheUsed)
	write("sproutfs_cache_disk_limit_bytes", "gauge", "The page cache's disk, which holds what pulls copy.",
		status.Resources.CacheDiskLimit)
	write("sproutfs_cache_disk_used_bytes", "gauge", "How much of the page cache's disk the pulls hold.",
		status.Resources.CacheDiskUsed)
	diskMetrics(&out, status.Disk)

	// The store counters carry the operation as a label: five operations, one
	// series each, which is what makes a rate by operation a query rather than
	// five metrics.
	operations := []struct {
		name  string
		count StoreCount
	}{
		{"head", status.Store.Head}, {"get", status.Store.Get}, {"put", status.Store.Put},
		{"delete", status.Store.Delete}, {"list", status.Store.List},
	}
	labelled := func(name, help string, value func(StoreCount) int64) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		for _, operation := range operations {
			fmt.Fprintf(&out, "%s{operation=%q} %d\n", name, operation.name, value(operation.count))
		}
	}
	labelled("sproutfs_store_calls_total", "Object store calls this host has made.",
		func(c StoreCount) int64 { return c.Calls })
	labelled("sproutfs_store_failures_total", "Object store calls that reported an error.",
		func(c StoreCount) int64 { return c.Failures })
	labelled("sproutfs_store_bytes_total", "Object bytes moved, which only get and put move.",
		func(c StoreCount) int64 { return c.Bytes })
	fmt.Fprintf(&out, "# HELP sproutfs_store_seconds How long each object store call took, failed ones included.\n"+
		"# TYPE sproutfs_store_seconds histogram\n")
	for _, operation := range operations {
		histogram(&out, "sproutfs_store_seconds", fmt.Sprintf("operation=%q", operation.name), operation.count.Latency)
	}
	return out.String()
}

// lossWindow reduces the per-VM report to the two numbers a scrape carries: the
// widest window on this host, and how many VMs are past theirs.
// diskBindings are the goals a disk limiter can report as binding, each a
// series of its own so that a dashboard can show which one sets the cache's
// share.
var diskBindings = []string{"free-bytes", "free-percent", "used-bytes", "filesystem"}

// diskMetrics writes what the disk limiter chose: the filesystem as it read
// it, the floor and band it keeps, what the host promised, the cache's share
// and the goal that set it, and the cache's write budget.
func diskMetrics(out *strings.Builder, disk Disk) {
	write := func(name, kind, help string, value any) {
		fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, value)
	}
	write("sproutfs_disk_total_bytes", "gauge", "The size of the filesystem the host writes to, as last read.",
		disk.TotalBytes)
	write("sproutfs_disk_available_bytes", "gauge", "What the filesystem had available, as last read.",
		disk.AvailableBytes)
	write("sproutfs_disk_smooth_free_bytes", "gauge", "What the filesystem has free, smoothed, as the limiter acts on it.",
		disk.SmoothFreeBytes)
	write("sproutfs_disk_floor_bytes", "gauge", "What the free-space goals keep free.", disk.FloorBytes)
	write("sproutfs_disk_reserve_bytes", "gauge", "What the cache leaves free above the floor for promises.",
		disk.ReserveBytes)
	write("sproutfs_disk_band_bytes", "gauge", "How far above the floor and the reserve the cache is kept.",
		disk.BandBytes)
	fmt.Fprintf(out, "# HELP sproutfs_disk_promised_bytes What each user that cannot give space back is promised.\n"+
		"# TYPE sproutfs_disk_promised_bytes gauge\n")
	for _, promise := range disk.Promises {
		fmt.Fprintf(out, "sproutfs_disk_promised_bytes{user=%q} %d\n", promise.Name, promise.PromisedBytes)
	}
	fmt.Fprintf(out, "# HELP sproutfs_disk_allocated_bytes What each user that cannot give space back holds.\n"+
		"# TYPE sproutfs_disk_allocated_bytes gauge\n")
	for _, promise := range disk.Promises {
		fmt.Fprintf(out, "sproutfs_disk_allocated_bytes{user=%q} %d\n", promise.Name, promise.AllocatedBytes)
	}
	write("sproutfs_disk_cache_share_bytes", "gauge",
		"What the cache may hold, below zero when the promises do not fit.", disk.CacheShareBytes)
	write("sproutfs_disk_cache_held_bytes", "gauge", "What the cache holds.", disk.CacheHeldBytes)
	fmt.Fprintf(out, "# HELP sproutfs_disk_binding The goal that sets the cache's share.\n"+
		"# TYPE sproutfs_disk_binding gauge\n")
	for _, binding := range diskBindings {
		value := 0
		if binding == disk.Binding {
			value = 1
		}
		fmt.Fprintf(out, "sproutfs_disk_binding{goal=%q} %d\n", binding, value)
	}
	ready := 1
	if disk.Unready != "" {
		ready = 0
	}
	write("sproutfs_disk_promises_fit", "gauge",
		"One while the host's promises fit under its goals with an empty cache, and zero when they do not.", ready)
	write("sproutfs_disk_device_written_bytes_total", "counter",
		"What the device wrote since the host started, by every writer.", disk.Writes.WrittenBytes)
	write("sproutfs_disk_cache_admitted_bytes_total", "counter",
		"What the cache was admitted to write.", disk.Writes.AdmittedBytes)
	write("sproutfs_disk_write_budget_left_bytes", "gauge",
		"What the cache's write budget has left, below zero when the device wrote past it.", disk.Writes.LeftBytes)
	fmt.Fprintf(out, "# HELP sproutfs_disk_cache_writes_refused_total The cache's writes the budget refused, by priority.\n"+
		"# TYPE sproutfs_disk_cache_writes_refused_total counter\n")
	for priority, refused := range disk.Writes.Refused {
		fmt.Fprintf(out, "sproutfs_disk_cache_writes_refused_total{priority=\"%d\"} %d\n", priority, refused)
	}
}

func lossWindow(vms []VM) (widest time.Duration, waiting int) {
	for _, vm := range vms {
		widest = max(widest, vm.LossWindow)
		if vm.Waiting {
			waiting++
		}
	}
	return widest, waiting
}

// histogram writes one Prometheus histogram series set: the cumulative count
// at each bucket's upper bound, in seconds, then +Inf, the sum and the count.
// labels are the series' own labels, empty for none.
func histogram(out *strings.Builder, name, labels string, l Latency) {
	with := func(label string) string {
		if labels == "" {
			return "{" + label + "}"
		}
		return "{" + labels + "," + label + "}"
	}
	plain := ""
	if labels != "" {
		plain = "{" + labels + "}"
	}
	var cumulative uint64
	for i := range LatencyBuckets - 1 {
		if i < len(l.Buckets) {
			cumulative += l.Buckets[i]
		}
		upper := LatencyBucketUpperNS(i)
		if i == 0 {
			upper++ // the first bucket is every observation under a microsecond
		}
		fmt.Fprintf(out, "%s_bucket%s %d\n", name, with(fmt.Sprintf("le=%q", seconds(upper))), cumulative)
	}
	fmt.Fprintf(out, "%s_bucket%s %d\n", name, with(`le="+Inf"`), l.Count)
	fmt.Fprintf(out, "%s_sum%s %s\n", name, plain, seconds(l.TotalNS))
	fmt.Fprintf(out, "%s_count%s %d\n", name, plain, l.Count)
}

// seconds is a nanosecond count as Prometheus writes a duration.
func seconds(ns uint64) string {
	return strconv.FormatFloat(float64(ns)/1e9, 'g', -1, 64)
}
