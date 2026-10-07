package host

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MetricKind is what a family's samples are: a counter, a gauge or a histogram.
type MetricKind int

const (
	Counter MetricKind = iota
	Gauge
	Histogram
)

// String is the kind as the Prometheus text format names it.
func (k MetricKind) String() string {
	switch k {
	case Counter:
		return "counter"
	case Gauge:
		return "gauge"
	case Histogram:
		return "histogram"
	}
	return "untyped"
}

// MetricFamily is one metric: its name, its help, its kind and its samples, one
// for each set of labels. A family of a host with nothing to report under a
// label, such as a disk limiter with no promises, has no samples.
type MetricFamily struct {
	Name    string
	Help    string
	Kind    MetricKind
	Samples []Sample
}

// Label is one label of a sample. A sample's labels are in the order the text
// writes them.
type Label struct {
	Name  string
	Value string
}

// Sample is one series of a family. Value is a counter's or a gauge's value.
// Buckets, Sum and Count are a histogram's: the cumulative count of
// observations at each bucket's upper bound in seconds, in ascending order and
// without +Inf, the sum of the observations in seconds, and their count.
type Sample struct {
	Labels  []Label
	Value   float64
	Buckets []Bucket
	Sum     float64
	Count   uint64
}

// Bucket is one bucket of a histogram: how many observations were at most
// UpperBound seconds.
type Bucket struct {
	UpperBound float64
	Count      uint64
}

// Metrics writes one host's status as Prometheus text: what sproutfs-host
// serves on /metrics. It is MetricFamilies written out, so the text and the
// families cannot differ.
func Metrics(status Status) string {
	var out strings.Builder
	for _, family := range MetricFamilies(status) {
		family.write(&out)
	}
	return out.String()
}

// MetricFamilies is one host's status as metrics: what an embedder that runs
// the host as a library registers with its own metrics library, from the Status
// it reads. sproutfs links no metrics library: the families are plain data, and
// Metrics writes them as text.
//
// Everything here is read from Status, which is the same report the API and the
// orchestrator's survey see. Counters end in _total and are monotonic for one
// process lifetime — a host restart is a host loss, so they start again at zero
// with everything else this process holds.
func MetricFamilies(status Status) []MetricFamily {
	var e exposition
	// What this host runs, as labels on a constant, which is how a dashboard
	// tells hosts apart during a rollout.
	e.family("sproutfs_build_info", Gauge, "What this host is running.", sample(1,
		Label{"version", status.Build.Version}, Label{"api_revision", strconv.Itoa(status.Build.APIRevision)},
		Label{"arena", status.Build.Arena}))
	e.one("sproutfs_vms_running", Gauge,
		"VMs this host runs.", float(len(status.Running)))
	e.one("sproutfs_vms_serving", Gauge,
		"VMs this host has handed over and still serves the pages of.", float(len(status.Serving)))
	// The widest window rather than one series per VM: what an operator alerts
	// on is the worst exposure this host carries, and a gauge per VM would put
	// the deployment's VM identities into the scraper's label space.
	widest, waiting := lossWindow(status.VMs)
	e.one("sproutfs_loss_window_seconds", Gauge,
		"How long the worst-off VM this host runs has held a write no checkpoint covers, which is what losing this host would cost it in time.",
		widest.Seconds())
	e.one("sproutfs_vms_waiting", Gauge,
		"VMs past their loss window, whose stores the pager is holding back until a checkpoint of them lands.", float(waiting))

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
	byKind := func(name string, kind MetricKind, help string, value func(PagerKind) float64) {
		samples := make([]Sample, 0, len(kinds))
		for _, k := range kinds {
			samples = append(samples, sample(value(k.pager), Label{"kind", k.name}))
		}
		e.family(name, kind, help, samples...)
	}
	byKind("sproutfs_pager_page_bytes", Gauge,
		"The page each pager runs, which its page counts here are in.",
		func(p PagerKind) float64 { return float(p.PageBytes) })
	byKind("sproutfs_pager_arena_pages", Gauge,
		"Pages each pager's arena holds.", func(p PagerKind) float64 { return float(p.ArenaPages) })
	byKind("sproutfs_pager_resident_pages", Gauge,
		"Pages of each arena that are taken.", func(p PagerKind) float64 { return float(p.ResidentPages) })
	byKind("sproutfs_pager_dirty_pages", Gauge,
		"Pages of volatile private state no checkpoint has published.",
		func(p PagerKind) float64 { return float(p.DirtyPages) })
	byKind("sproutfs_pager_logical_pages", Gauge,
		"Pages each pager holds metadata for.", func(p PagerKind) float64 { return float(p.LogicalPages) })
	e.one("sproutfs_pager_arena_bytes", Gauge,
		"What the two arenas hold together, which is the only unit their capacities can be added in.",
		float(status.Pager.ArenaBytes()))
	e.one("sproutfs_guest_committed_bytes", Gauge,
		"Guest RAM the VMs this host runs have between them, resident or not, which is what a placement measures this host by.",
		float(status.Pager.CommittedBytes))
	byKind("sproutfs_pager_shared_pages_total", Counter,
		"Pages mapped to an already resident identity without a read, which is what a fork inherits.",
		func(p PagerKind) float64 { return float(p.SharedPages) })
	// The sharing gauges are what the counter above is not — how much sharing is
	// still there, rather than how often it happened.
	byKind("sproutfs_pager_unique_resident_bytes", Gauge,
		"Host memory the pager's arena holds, one resident page counted once however many memory regions map it.",
		func(p PagerKind) float64 { return float(p.Sharing.UniqueBytes) })
	byKind("sproutfs_pager_mapped_resident_bytes", Gauge,
		"Resident pages summed over the memory regions that map them, counting every alias, which is what this host would hold if nothing shared anything.",
		func(p PagerKind) float64 { return float(p.Sharing.MappedBytes) })
	byKind("sproutfs_pager_shared_saved_bytes", Gauge,
		"Mapped less unique: the memory this host did not have to find because its guests are reading the same pages.",
		func(p PagerKind) float64 { return float(p.Sharing.SavedBytes) })
	byKind("sproutfs_pager_faults_total", Counter, "Faults the pager has resolved.",
		func(p PagerKind) float64 { return float(p.Faults) })
	byKind("sproutfs_pager_evictions_total", Counter, "Pages the pager has evicted.",
		func(p PagerKind) float64 { return float(p.Evictions) })
	byKind("sproutfs_pager_spills_total", Counter, "Pages the pager has written to its spill file.",
		func(p PagerKind) float64 { return float(p.Spills) })
	byKind("sproutfs_pager_idle_pages", Gauge,
		"Resident pages no memory region maps, kept for the next one that inherits them.",
		func(p PagerKind) float64 { return float(p.IdlePages) })
	byKind("sproutfs_pager_loaded_pages_total", Counter, "Pages the pager read from its backing.",
		func(p PagerKind) float64 { return float(p.LoadedPages) })
	byKind("sproutfs_pager_copy_on_writes_total", Counter, "Stores the pager gave a private copy of a page.",
		func(p PagerKind) float64 { return float(p.CopyOnWrites) })
	byKind("sproutfs_pager_unmapped_copy_on_writes_total", Counter,
		"Copy-on-writes of a page the storing guest did not map.",
		func(p PagerKind) float64 { return float(p.UnmappedCopyOnWrites) })
	byKind("sproutfs_pager_unchanged_pages_total", Counter,
		"Private copies a checkpoint found still holding the bytes they were copied from.",
		func(p PagerKind) float64 { return float(p.UnchangedPages) })
	// What the kernel said of each page fault it reported.
	byKind("sproutfs_pager_read_traps_total", Counter, "Page faults the kernel reported as reads.",
		func(p PagerKind) float64 { return float(p.ReadTraps) })
	byKind("sproutfs_pager_store_traps_total", Counter,
		"Page faults the kernel reported as stores into a page not in the page tables.",
		func(p PagerKind) float64 { return float(p.StoreTraps) })
	byKind("sproutfs_pager_protect_traps_total", Counter,
		"Page faults the kernel reported as stores into a write-protected page.",
		func(p PagerKind) float64 { return float(p.ProtectTraps) })
	byKind("sproutfs_pager_given_back_pages_total", Counter,
		"Unchanged copies given back to the page they were copied from without a checkpoint.",
		func(p PagerKind) float64 { return float(p.GivenBackPages) })
	byKind("sproutfs_pager_revocations_total", Counter, "Commands that took mappings away from a VMM.",
		func(p PagerKind) float64 { return float(p.Revocations) })
	byKind("sproutfs_pager_revoked_pages_total", Counter, "Pages those commands took away.",
		func(p PagerKind) float64 { return float(p.RevokedPages) })
	// What an isolated arena copies between its files, which a shared arena
	// never does.
	byKind("sproutfs_pager_moved_pages_total", Counter,
		"Published pages copied into the tenant's shared file because another memory region inherited them.",
		func(p PagerKind) float64 { return float(p.MovedPages) })
	byKind("sproutfs_pager_fork_copies_total", Counter,
		"Pages copied into a fork point's file for a child on this host.",
		func(p PagerKind) float64 { return float(p.ForkCopies) })
	byKind("sproutfs_pager_tampered_total", Counter,
		"Moves whose copy did not hold the bytes the page's upload read.",
		func(p PagerKind) float64 { return float(p.Tampered) })

	// Why a guest stops making progress: a store held back for the dirty
	// budget or the loss window, and a VMM out of mapping budget or faulting
	// in a loop.
	byKind("sproutfs_pager_dirty_waits_total", Counter, "Stores that waited for the dirty budget.",
		func(p PagerKind) float64 { return float(p.DirtyWaits) })
	byKind("sproutfs_pager_checkpoint_requests_total", Counter,
		"Checkpoints a store waiting for the dirty budget asked for out of the interval's turn.",
		func(p PagerKind) float64 { return float(p.CheckpointRequests) })
	byKind("sproutfs_pager_dirty_stalls_total", Counter,
		"Stores no checkpoint could admit, whose VM was stopped.",
		func(p PagerKind) float64 { return float(p.DirtyStalls) })
	byKind("sproutfs_pager_window_waits_total", Counter,
		"Stores that waited because their VM had held a write no checkpoint covers for longer than the loss window.",
		func(p PagerKind) float64 { return float(p.WindowWaits) })
	byKind("sproutfs_pager_window_stalls_total", Counter,
		"Stores past the loss window no checkpoint was ever going to cover, whose VM was stopped.",
		func(p PagerKind) float64 { return float(p.WindowStalls) })
	byKind("sproutfs_pager_refused_mappings_total", Counter,
		"Faults a VMM refused a mapping command for, which is a VMM out of mapping budget.",
		func(p PagerKind) float64 { return float(p.RefusedMappings) })
	byKind("sproutfs_pager_repeated_faults_total", Counter,
		"Faults a VMM took again on pages already mapped for it.",
		func(p PagerKind) float64 { return float(p.RepeatedFaults) })
	byKind("sproutfs_pager_paced_faults_total", Counter,
		"Repeated faults that waited for their VMM's budget of them.",
		func(p PagerKind) float64 { return float(p.PacedFaults) })
	byKind("sproutfs_pager_prefetched_pages_total", Counter,
		"Pages the prefetches behind faults landed: the rest of each fault's run, read behind its own page.",
		func(p PagerKind) float64 { return float(p.PrefetchedPages) })
	byKind("sproutfs_pager_prefetch_waits_total", Counter,
		"Faults that waited for a prefetch already reading their page.",
		func(p PagerKind) float64 { return float(p.PrefetchWaits) })
	byKind("sproutfs_pager_prefetch_cancelled_total", Counter,
		"Prefetches an allocation cancelled to take their slots rather than evict a page a guest maps.",
		func(p PagerKind) float64 { return float(p.PrefetchCancelled) })
	histogramByKind := func(name, help string, value func(PagerKind) Latency) {
		samples := make([]Sample, 0, len(kinds))
		for _, k := range kinds {
			samples = append(samples, latencySample(value(k.pager), Label{"kind", k.name}))
		}
		e.family(name, Histogram, help, samples...)
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
	e.one("sproutfs_checkpoint_attempts_total", Counter, "Interval checkpoints this host began.",
		float(status.Checkpoints.Attempts))
	e.family("sproutfs_checkpoints_total", Counter, "Interval checkpoints that ended, by how.",
		sample(float(status.Checkpoints.Published), Label{"outcome", "published"}),
		sample(float(status.Checkpoints.CaptureFailed), Label{"outcome", "capture_failed"}),
		sample(float(status.Checkpoints.PublishFailed), Label{"outcome", "publish_failed"}),
		sample(float(status.Checkpoints.Fenced), Label{"outcome", "fenced"}))
	e.one("sproutfs_checkpoint_uploaded_bytes_total", Counter,
		"Bytes the published interval checkpoints uploaded.", float(status.Checkpoints.UploadedBytes))
	e.family("sproutfs_checkpoint_pause_seconds", Histogram, "How long each interval checkpoint paused its guest.",
		latencySample(status.Checkpoints.Pause))
	e.family("sproutfs_checkpoint_upload_seconds", Histogram,
		"How long each interval checkpoint took to publish, behind the running guest.",
		latencySample(status.Checkpoints.Upload))

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
		e.outcomes(handovers.name, handovers.help, handovers.outcomes)
	}
	e.family("sproutfs_received_pause_seconds", Histogram,
		"What each guest this host took in paid, from the source's pause to its resume here.",
		latencySample(status.Lifecycle.MigrationPause, Label{"kind", "migration"}),
		latencySample(status.Lifecycle.ForkPause, Label{"kind", "fork"}))
	e.family("sproutfs_vms_given_up_total", Counter, "VMs this host gave up, by why.",
		sample(float(status.Lifecycle.Deaths), Label{"reason", "vmm_ended"}),
		sample(float(status.Lifecycle.Fenced), Label{"reason", "fenced"}),
		sample(float(status.Lifecycle.Stopped), Label{"reason", "stopped_for_a_bound"}))
	e.journal(status.Journal)

	// Template imports: the imports this host wrote, what each took, and every
	// byte of guest image it read, which is what a host start costs.
	e.outcomes("sproutfs_template_imports_total", "Templates this host imported, by outcome.", status.Imports.Outcomes)
	e.family("sproutfs_template_import_seconds", Histogram,
		"How long each import took, from its image's digest to its pin.", latencySample(status.Imports.Latency))
	e.one("sproutfs_image_read_bytes_total", Counter,
		"Guest image bytes this host read, for a digest or an import.", float(status.Imports.ImageBytes))

	e.one("sproutfs_pages_requests_total", Counter,
		"Page requests this host's peer server has answered.", float(status.Pages.Requests))
	e.one("sproutfs_pages_served_total", Counter, "Pages served to a peer.", float(status.Pages.Served))
	e.one("sproutfs_pages_absent_total", Counter, "Page requests for a page this host does not hold.",
		float(status.Pages.Absent))
	e.one("sproutfs_pages_refused_total", Counter, "Page requests refused, which is a peer at its budget.",
		float(status.Pages.Refused))
	up, down, incompatible := 0, 0, 0
	for _, peer := range status.Peers {
		switch {
		case peer.Incompatible != "":
			incompatible++
		case peer.Down:
			down++
		default:
			up++
		}
	}
	e.family("sproutfs_peers", Gauge, "Hosts this host has asked anything of, by what its table of peers knows of them.",
		sample(float(up), Label{"state", "up"}), sample(float(down), Label{"state", "down"}),
		sample(float(incompatible), Label{"state", "incompatible"}))

	e.one("sproutfs_memory_limit_bytes", Gauge, "The RAM allotment the pager takes its pages from.",
		float(status.Resources.MemoryLimit))
	e.one("sproutfs_memory_used_bytes", Gauge, "How much of that allotment is taken.",
		float(status.Resources.MemoryUsed))
	e.one("sproutfs_cache_limit_bytes", Gauge, "The page cache's own cap, which nothing else draws on.",
		float(status.Resources.CacheLimit))
	e.one("sproutfs_cache_used_bytes", Gauge, "How much of the page cache is resident.",
		float(status.Resources.CacheUsed))
	e.one("sproutfs_cache_disk_limit_bytes", Gauge, "The page cache's disk, which holds what pulls copy.",
		float(status.Resources.CacheDiskLimit))
	e.one("sproutfs_cache_disk_used_bytes", Gauge, "How much of the page cache's disk the pulls hold.",
		float(status.Resources.CacheDiskUsed))
	e.cacheMemory(status.CacheMemory)
	e.cacheDisk(status.CacheDisk)
	e.cacheFill(status.CacheFill)
	e.cacheRead(status.CacheRead)
	e.hotTier(status.HotTier)
	e.disk(status.Disk)

	e.store("sproutfs_store", "Object store", status.Store)
	if status.HotTierStore != nil {
		e.store("sproutfs_hot_tier_store", "Hot tier bucket", *status.HotTierStore)
	}
	return e.families
}

// exposition collects the families in the order Metrics writes them.
type exposition struct {
	families []MetricFamily
}

func (e *exposition) family(name string, kind MetricKind, help string, samples ...Sample) {
	e.families = append(e.families, MetricFamily{Name: name, Help: help, Kind: kind, Samples: samples})
}

// one adds a family of one sample with no labels.
func (e *exposition) one(name string, kind MetricKind, help string, value float64) {
	e.family(name, kind, help, sample(value))
}

// outcomes adds a counter of handovers or imports by outcome.
func (e *exposition) outcomes(name, help string, outcomes Outcomes) {
	e.family(name, Counter, help,
		sample(float(outcomes.Succeeded), Label{"outcome", "succeeded"}),
		sample(float(outcomes.Failed), Label{"outcome", "failed"}))
}

func sample(value float64, labels ...Label) Sample {
	return Sample{Labels: labels, Value: value}
}

// number is every type a Status field counts in.
type number interface {
	~int | ~int32 | ~int64 | ~uint | ~uint32 | ~uint64 | ~float64
}

// float is a Status field as a sample's value. A count past 2^53 loses its low
// digits, as it would in any Prometheus client.
func float[N number](n N) float64 { return float64(n) }

// latencySample is a Latency as a histogram sample: the cumulative count at
// each bucket's upper bound, in seconds, and the sum and the count.
func latencySample(l Latency, labels ...Label) Sample {
	s := Sample{Labels: labels, Sum: float64(l.TotalNS) / 1e9, Count: l.Count,
		Buckets: make([]Bucket, 0, LatencyBuckets-1)}
	var cumulative uint64
	for i := range LatencyBuckets - 1 {
		if i < len(l.Buckets) {
			cumulative += l.Buckets[i]
		}
		upper := LatencyBucketUpperNS(i)
		if i == 0 {
			upper++ // the first bucket is every observation under a microsecond
		}
		s.Buckets = append(s.Buckets, Bucket{UpperBound: float64(upper) / 1e9, Count: cumulative})
	}
	return s
}

// write writes the family as Prometheus text: its help and type, then each
// sample, a histogram's as its buckets, +Inf, its sum and its count.
func (f MetricFamily) write(out *strings.Builder) {
	fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s %s\n", f.Name, helpEscaper.Replace(f.Help), f.Name, f.Kind)
	for _, s := range f.Samples {
		if f.Kind != Histogram {
			fmt.Fprintf(out, "%s%s %s\n", f.Name, labelText(s.Labels), value(s.Value))
			continue
		}
		with := func(le string) string { return labelText(append(slices.Clip(s.Labels), Label{"le", le})) }
		for _, bucket := range s.Buckets {
			fmt.Fprintf(out, "%s_bucket%s %d\n", f.Name, with(seconds(bucket.UpperBound)), bucket.Count)
		}
		fmt.Fprintf(out, "%s_bucket%s %d\n", f.Name, with("+Inf"), s.Count)
		fmt.Fprintf(out, "%s_sum%s %s\n", f.Name, labelText(s.Labels), seconds(s.Sum))
		fmt.Fprintf(out, "%s_count%s %d\n", f.Name, labelText(s.Labels), s.Count)
	}
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

// labelText is a sample's labels as the text writes them, empty for none.
func labelText(labels []Label) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, len(labels))
	for i, label := range labels {
		parts[i] = label.Name + `="` + labelEscaper.Replace(label.Value) + `"`
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// value is a counter's or a gauge's value as the text writes it: a whole number
// without an exponent, anything else in Go's shortest form.
func value(v float64) string {
	if v == 0 {
		return "0"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

// seconds is a duration in seconds as Prometheus writes it.
func seconds(s float64) string {
	return strconv.FormatFloat(s, 'g', -1, 64)
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

// journal adds what durable flush did: whether it is on and served, its
// flushes by outcome, what they and their captures took, and the ring.
func (e *exposition) journal(j Journal) {
	e.one("sproutfs_durable_flush", Gauge,
		"One where a disk's flush is answered from the host's journal, zero where the flush bound answers it.",
		flag(j.DurableFlush))
	e.one("sproutfs_journal_served", Gauge, "One while a journal disk is served on this host.", flag(j.Served))
	e.outcomes("sproutfs_journal_flushes_total",
		"Flushes the journal answered, by outcome: a failed one is an I/O error in the guest.", j.Flushes)
	e.family("sproutfs_journal_flush_seconds", Histogram,
		"How long each durable flush took, from its arrival to its answer.", latencySample(j.Flush))
	e.family("sproutfs_journal_capture_seconds", Histogram,
		"How long each capture of a disk's changed blocks took: protecting, reading and hashing its pages.",
		latencySample(j.Capture))
	e.one("sproutfs_journal_ring_bytes", Gauge, "The journal disk's ring.", float(j.RingBytes))
	e.one("sproutfs_journal_live_bytes", Gauge, "What trimming has not freed of the journal's ring.",
		float(j.LiveBytes))
	e.one("sproutfs_journal_written_bytes_total", Counter,
		"The journal's next position, which grows by every byte the journal writes, pads included.",
		float(j.Position))
}

// diskBindings are the goals a disk limiter can report as binding, each a
// series of its own so that a dashboard can show which one sets the cache's
// share.
var diskBindings = []string{"free-bytes", "free-percent", "used-bytes", "filesystem"}

// disk adds what the disk limiter chose: the filesystem as it read it, the
// floor and band it keeps, what the host promised, the cache's share and the
// goal that set it, and the cache's write budget.
func (e *exposition) disk(disk Disk) {
	e.one("sproutfs_disk_total_bytes", Gauge, "The size of the filesystem the host writes to, as last read.",
		float(disk.TotalBytes))
	e.one("sproutfs_disk_available_bytes", Gauge, "What the filesystem had available, as last read.",
		float(disk.AvailableBytes))
	e.one("sproutfs_disk_smooth_free_bytes", Gauge, "What the filesystem has free, smoothed, as the limiter acts on it.",
		float(disk.SmoothFreeBytes))
	e.one("sproutfs_disk_floor_bytes", Gauge, "What the free-space goals keep free.", float(disk.FloorBytes))
	e.one("sproutfs_disk_reserve_bytes", Gauge, "What the cache leaves free above the floor for promises.",
		float(disk.ReserveBytes))
	e.one("sproutfs_disk_band_bytes", Gauge, "How far above the floor and the reserve the cache is kept.",
		float(disk.BandBytes))
	promised := make([]Sample, 0, len(disk.Promises))
	allocated := make([]Sample, 0, len(disk.Promises))
	for _, promise := range disk.Promises {
		promised = append(promised, sample(float(promise.PromisedBytes), Label{"user", promise.Name}))
		allocated = append(allocated, sample(float(promise.AllocatedBytes), Label{"user", promise.Name}))
	}
	e.family("sproutfs_disk_promised_bytes", Gauge, "What each user that cannot give space back is promised.",
		promised...)
	e.family("sproutfs_disk_allocated_bytes", Gauge, "What each user that cannot give space back holds.",
		allocated...)
	e.one("sproutfs_disk_cache_share_bytes", Gauge,
		"What the cache may hold, below zero when the promises do not fit.", float(disk.CacheShareBytes))
	e.one("sproutfs_disk_cache_held_bytes", Gauge, "What the cache holds.", float(disk.CacheHeldBytes))
	bindings := make([]Sample, 0, len(diskBindings))
	for _, binding := range diskBindings {
		bindings = append(bindings, sample(flag(binding == disk.Binding), Label{"goal", binding}))
	}
	e.family("sproutfs_disk_binding", Gauge, "The goal that sets the cache's share.", bindings...)
	e.one("sproutfs_disk_promises_fit", Gauge,
		"One while the host's promises fit under its goals with an empty cache, and zero when they do not.",
		flag(disk.Unready == ""))
	e.one("sproutfs_disk_device_written_bytes_total", Counter,
		"What the device wrote since the host started, by every writer.", float(disk.Writes.WrittenBytes))
	e.one("sproutfs_disk_cache_admitted_bytes_total", Counter,
		"What the cache was admitted to write.", float(disk.Writes.AdmittedBytes))
	e.one("sproutfs_disk_write_budget_left_bytes", Gauge,
		"What the cache's write budget has left, below zero when the device wrote past it.", float(disk.Writes.LeftBytes))
	refused := make([]Sample, 0, len(disk.Writes.Refused))
	for priority, count := range disk.Writes.Refused {
		refused = append(refused, sample(float(count), Label{"priority", strconv.Itoa(priority)}))
	}
	e.family("sproutfs_disk_cache_writes_refused_total", Counter, "The cache's writes the budget refused, by priority.",
		refused...)
}

// flag is a condition as a gauge: one when it holds, zero when it does not.
func flag(holds bool) float64 {
	if holds {
		return 1
	}
	return 0
}

// cacheMemory adds what the page cache's memory tier holds and what it served,
// in pages and page tables.
func (e *exposition) cacheMemory(memory CacheMemory) {
	e.one("sproutfs_cache_memory_entries", Gauge, "The pages and page tables the page cache holds in memory.",
		float(memory.Entries))
	e.one("sproutfs_cache_memory_page_tables", Gauge, "The segments' page tables the page cache holds decoded in memory.",
		float(memory.Tables))
	e.one("sproutfs_cache_memory_page_table_bytes", Gauge, "What the page tables the page cache holds are charged.",
		float(memory.TableBytes))
	e.family("sproutfs_cache_memory_page_table_lookups_total", Counter,
		"Lookups of segments' page tables, by outcome: answered by a held table, a fetch and decode of the segment, or a table a publication kept.",
		sample(float(memory.TableHits), Label{"outcome", "hit"}),
		sample(float(memory.TableLoads), Label{"outcome", "load"}),
		sample(float(memory.TableKept), Label{"outcome", "kept"}))
	e.family("sproutfs_cache_memory_reads_total", Counter,
		"Reads of pages from the page cache's memory, by outcome: served from it, fetched from the disk, the cluster or the store, or joined to a fetch in flight.",
		sample(float(memory.Hits), Label{"outcome", "hit"}),
		sample(float(memory.Misses), Label{"outcome", "miss"}),
		sample(float(memory.Coalesced), Label{"outcome", "coalesced"}))
	e.one("sproutfs_cache_memory_evictions_total", Counter, "Entries the page cache's memory gave up.",
		float(memory.Evictions))
}

// cacheDisk adds what the page cache's disk holds, what it served and what the
// host read back from it when it started. A host that keeps no cache disk
// reports zeroes.
func (e *exposition) cacheDisk(disk *CacheDisk) {
	var held CacheDisk
	if disk != nil {
		held = *disk
	}
	e.one("sproutfs_cache_disk_regions", Gauge, "The regions the page cache's disk holds.", float(held.Regions))
	e.one("sproutfs_cache_disk_entries", Gauge, "The stripes of pages and segments the page cache's disk holds.",
		float(held.Entries))
	e.one("sproutfs_cache_disk_hits_total", Counter,
		"Reads the page cache's disk served, which made no request of the object store.", float(held.Hits))
	e.one("sproutfs_cache_disk_lost_total", Counter,
		"Copies the page cache's disk could not give back intact, which the object store served instead.",
		float(held.Lost))
	e.one("sproutfs_cache_disk_evicted_regions_total", Counter, "Regions the page cache's disk gave back.",
		float(held.Evicted))
	e.one("sproutfs_cache_disk_writes_refused_total", Counter, "Writes the page cache's disk refused.",
		float(held.Refused))
	e.family("sproutfs_cache_disk_opened_regions", Gauge,
		"What the host did with the regions it found in its cache's file when it started.",
		sample(float(held.Opened.FromTables), Label{"how", "tables"}),
		sample(float(held.Opened.Scanned), Label{"how", "scanned"}),
		sample(float(held.Opened.GivenBack), Label{"how", "given-back"}))
}

// FillDropReasons is every reason a fill drops stripes for, in the order
// /metrics lists them.
var FillDropReasons = []string{"queue", "rate", "budget", "busy", "down", "stale", "peer", "disk", "failed"}

// byReason is a counter's samples for every reason in reasons, zero for a
// reason counts does not hold.
func byReason(reasons []string, counts map[string]uint64) []Sample {
	samples := make([]Sample, 0, len(reasons))
	for _, reason := range reasons {
		samples = append(samples, sample(float(counts[reason]), Label{"reason", reason}))
	}
	return samples
}

// cacheFill adds what this host's fills of the cluster's disk cache did. A host
// that keeps no cache disk reports zeroes.
func (e *exposition) cacheFill(fill *CacheFill) {
	var did CacheFill
	if fill != nil {
		did = *fill
	}
	e.family("sproutfs_cache_fills_total", Counter,
		"Windows this host filled the cluster's cache with, by what it read them for.",
		sample(float(did.FromReads), Label{"from", "read"}),
		sample(float(did.FromPublications), Label{"from", "publication"}))
	e.one("sproutfs_cache_fills_without_right_total", Counter,
		"Windows this host read from the store and filled nothing of, for want of the fill right.",
		float(did.WithoutRight))
	e.one("sproutfs_cache_fill_rights_granted_total", Counter,
		"Fill rights this host's cache gave out as a window's rank 1.", float(did.RightsGranted))
	e.one("sproutfs_cache_fill_stripes_sent_total", Counter,
		"Stripes this host's keeps carried that their holders kept.", float(did.Sent))
	e.one("sproutfs_cache_fill_bytes_sent_total", Counter, "Bytes of the keeps their holders kept.",
		float(did.SentBytes))
	e.one("sproutfs_cache_fill_stripes_kept_total", Counter,
		"Stripes fills wrote to this host's disk, its own and its peers' keeps.", float(did.Kept))
	e.family("sproutfs_cache_fill_stripes_dropped_total", Counter, "Stripes fills dropped, by why.",
		byReason(FillDropReasons, did.Dropped)...)
	e.one("sproutfs_cache_fill_stripes_duplicate_total", Counter,
		"Stripes a fill or a keep carried that this host's cache held or was writing already.", float(did.Duplicates))
	e.one("sproutfs_cache_keep_stripes_refused_total", Counter,
		"Stripes of keeps refused: for a window this host's list does not rank its cache for, or that did not hold together.",
		float(did.Refused))
	e.one("sproutfs_cache_fill_queued_bytes", Gauge,
		"What the queue of writes to this host's disk holds now.", float(did.QueuedBytes))
	e.one("sproutfs_cache_fill_queue_limit_bytes", Gauge,
		"The bound of the queue of writes to this host's disk.", float(did.QueueBytes))
	e.one("sproutfs_cache_fill_queued_peak_bytes", Gauge,
		"The most the queue of writes to this host's disk has held.", float(did.QueuedPeakBytes))
	e.one("sproutfs_cache_fill_publication_waits_total", Counter,
		"Waits of publications' fills: for room in the queue, the rate, the background budget or a busy holder.",
		float(did.PublicationWaits))
	e.one("sproutfs_cache_fill_publication_waited_seconds_total", Counter,
		"How long publications' fills waited in all.", did.PublicationWaitedSeconds)
	e.one("sproutfs_cache_fill_publications_gave_up_total", Counter,
		"Publications that waited out the bound for their fills and waited no more.", float(did.PublicationsGaveUp))
}

// HotTierFailures is every reason a read of the hot tier fails for, and
// HotTierDropReasons every reason a fill of it is dropped for, in the order
// /metrics lists them.
var (
	HotTierFailures    = []string{"error", "slow", "corrupt"}
	HotTierDropReasons = []string{"queue", "rate", "read", "write", "closed"}
)

// hotTier adds what this host's reads through the hot tier and its fills of it
// did. A host with no hot tier reports zeroes.
func (e *exposition) hotTier(hot *HotTier) {
	var did HotTier
	if hot != nil {
		did = *hot
	}
	e.family("sproutfs_hot_tier_reads_total", Counter,
		"Reads of checkpoint objects through the hot tier, by how they ended.",
		sample(float(did.Hits), Label{"result", "hit"}),
		sample(float(did.Misses), Label{"result", "miss"}),
		sample(float(did.Skipped), Label{"result", "skipped"}))
	e.family("sproutfs_hot_tier_failures_total", Counter,
		"Reads the hot tier failed, which the regional bucket served, by why.",
		byReason(HotTierFailures, did.Failed)...)
	e.one("sproutfs_hot_tier_marked_down_total", Counter,
		"Times the hot tier failed three reads in a row and reads skipped it for a while.", float(did.MarkedDown))
	e.one("sproutfs_hot_tier_down", Gauge, "Whether reads skip the hot tier now.", flag(did.Down))
	e.family("sproutfs_hot_tier_fills_total", Counter,
		"Fills of the hot tier handed over and held, by what handed them over.",
		sample(float(did.FromReads), Label{"from", "read"}),
		sample(float(did.FromPublications), Label{"from", "publication"}))
	e.one("sproutfs_hot_tier_fills_sent_total", Counter, "Fills the hot tier took.", float(did.Sent))
	e.one("sproutfs_hot_tier_fill_bytes_sent_total", Counter, "Bytes of the fills the hot tier took.",
		float(did.SentBytes))
	e.one("sproutfs_hot_tier_fills_present_total", Counter,
		"Fills that found the object in the hot tier already.", float(did.Present))
	e.one("sproutfs_hot_tier_fills_duplicate_total", Counter,
		"Misses of an object a fill was already held for.", float(did.Duplicates))
	e.family("sproutfs_hot_tier_fills_dropped_total", Counter, "Fills of the hot tier dropped, by why.",
		byReason(HotTierDropReasons, did.Dropped)...)
	e.one("sproutfs_hot_tier_head_checks_total", Counter,
		"Sampled hits whose regional object was checked with a HEAD.", float(did.HeadChecks))
	e.one("sproutfs_hot_tier_head_missing_total", Counter,
		"Sampled hits whose regional object the regional bucket no longer held.", float(did.HeadMissing))
	e.one("sproutfs_hot_tier_fill_queued_bytes", Gauge, "What the fills of the hot tier held now come to.",
		float(did.QueuedBytes))
	e.one("sproutfs_hot_tier_fill_queue_limit_bytes", Gauge, "The bound of the fills of the hot tier held.",
		float(did.QueueBytes))
}

// cacheRead adds what this host's reads of the cluster's disk cache did, and
// what its peer server served of its cache. A host that keeps no cache disk
// reports zeroes.
func (e *exposition) cacheRead(read *CacheRead) {
	var did CacheRead
	if read != nil {
		did = *read
	}
	e.family("sproutfs_cache_reads_total", Counter, "Envelopes this host read from the cluster's cache, by outcome.",
		sample(float(did.Hits), Label{"outcome", "hit"}),
		sample(float(did.Misses), Label{"outcome", "miss"}))
	e.one("sproutfs_cache_read_own_hits_total", Counter,
		"Envelopes this host's own stripes rebuilt alone, with no request.", float(did.OwnHits))
	e.one("sproutfs_cache_read_earlier_hits_total", Counter,
		"Envelopes rebuilt from stripes of a code the deployment used before its own.", float(did.EarlierHits))
	e.one("sproutfs_cache_read_requests_total", Counter, "Stripe requests this host sent its peers.",
		float(did.Requests))
	e.one("sproutfs_cache_read_replaced_total", Counter,
		"Holders replaced at once for answering with nothing, BUSY or an error.", float(did.Replaced))
	e.one("sproutfs_cache_read_second_requests_total", Counter,
		"Reads that asked the rest of a window's ranks after the delay.", float(did.SecondRequests))
	e.one("sproutfs_cache_read_refused_by_budget_total", Counter,
		"Reads whose second request the budget refused.", float(did.RefusedByBudget))
	e.family("sproutfs_cache_read_store_hedges_total", Counter,
		"Reads past the bound that read the store as well, by outcome.",
		sample(float(did.StoreHedgesWon), Label{"outcome", "won"}),
		sample(float(did.StoreHedges-did.StoreHedgesWon), Label{"outcome", "lost"}),
		sample(float(did.StoreHedgesRefused), Label{"outcome", "refused"}))
	e.one("sproutfs_cache_read_wrong_stripes_total", Counter, "Stripes found wrong.", float(did.WrongStripes))
	e.one("sproutfs_cache_read_drops_sent_total", Counter,
		"Drops sent to the holders of stripes found wrong.", float(did.DropsSent))
	e.one("sproutfs_cache_read_repairs_total", Counter,
		"Stripes sent to ranks that lacked them, of indices no rank held.", float(did.Repairs))
	e.one("sproutfs_cache_read_timeouts_total", Counter, "Stripe requests that timed out.", float(did.Timeouts))
	e.one("sproutfs_cache_read_marked_down_total", Counter, "Hosts this host's reads marked down.",
		float(did.MarkedDown))
	e.one("sproutfs_cache_read_mark_capped_total", Counter,
		"Marks refused because a fifth of the list was marked down already.", float(did.MarkCapped))
	e.one("sproutfs_cache_read_mark_cleared_total", Counter, "Marks a probe cleared.", float(did.MarkCleared))
	e.one("sproutfs_cache_read_down_hosts", Gauge, "Hosts this host's reads have marked down now.", float(did.Down))
	e.one("sproutfs_cache_read_head_checks_total", Counter,
		"Sampled hits whose part was checked with a HEAD.", float(did.HeadChecks))
	e.one("sproutfs_cache_read_head_missing_total", Counter,
		"Sampled hits whose part the store no longer had.", float(did.HeadMissing))
	// Each size class of read is a series of its own, labelled by the most
	// bytes a read of it asks for.
	classes := func(name string, kind MetricKind, help string, value func(CacheReadClass) float64) {
		samples := make([]Sample, 0, len(did.Classes))
		for _, class := range did.Classes {
			samples = append(samples, sample(value(class), Label{"up_to_bytes", strconv.FormatInt(class.UpToBytes, 10)}))
		}
		e.family(name, kind, help, samples...)
	}
	classes("sproutfs_cache_read_delay_seconds", Gauge,
		"The delay before a read of a size class asks the rest of a window's ranks.",
		func(class CacheReadClass) float64 { return class.Delay.Seconds() })
	classes("sproutfs_cache_read_bound_seconds", Gauge,
		"The bound before a read of a size class reads the store too.",
		func(class CacheReadClass) float64 { return class.Bound.Seconds() })
	classes("sproutfs_cache_read_class_reads_total", Counter,
		"Reads of a size class that had their stripes, which its delay is drawn from.",
		func(class CacheReadClass) float64 { return float(class.Reads) })
	e.one("sproutfs_cache_serve_reads_total", Counter,
		"Reads of this host's stripes its peer server answered with them.", float(did.Served))
	e.one("sproutfs_cache_serve_stripes_total", Counter, "Stripes this host served its peers.",
		float(did.ServedStripes))
	e.one("sproutfs_cache_serve_bytes_total", Counter, "Bytes of stripes this host served its peers.",
		float(did.ServedBytes))
	e.one("sproutfs_cache_serve_busy_total", Counter,
		"Reads this host answered BUSY because its serving bandwidth was spent.", float(did.ServeBusy))
}

// store adds what one bucket served, under prefix. The counters carry the
// operation as a label: five operations, one series each, which is what makes a
// rate by operation a query rather than five metrics. The timeouts carry the
// bound that cancelled the attempt as a second.
func (e *exposition) store(prefix, what string, store Store) {
	operations := []struct {
		name  string
		count StoreCount
	}{
		{"head", store.Head}, {"get", store.Get}, {"put", store.Put}, {"delete", store.Delete}, {"list", store.List},
	}
	labelled := func(name, help string, value func(StoreCount) int64) {
		samples := make([]Sample, 0, len(operations))
		for _, operation := range operations {
			samples = append(samples, sample(float(value(operation.count)), Label{"operation", operation.name}))
		}
		e.family(prefix+name, Counter, help, samples...)
	}
	labelled("_calls_total", what+" calls this host has made.",
		func(c StoreCount) int64 { return c.Calls })
	labelled("_failures_total", what+" calls that reported an error.",
		func(c StoreCount) int64 { return c.Failures })
	labelled("_bytes_total", "Object bytes moved, which only get and put move.",
		func(c StoreCount) int64 { return c.Bytes })
	labelled("_retries_total", what+" attempts made again after one outlived its bound.",
		func(c StoreCount) int64 { return c.Retries })
	timeouts := make([]Sample, 0, 2*len(operations))
	latencies := make([]Sample, 0, len(operations))
	for _, operation := range operations {
		timeouts = append(timeouts,
			sample(float(operation.count.FirstByteTimeouts), Label{"operation", operation.name}, Label{"bound", "first_byte"}),
			sample(float(operation.count.StallTimeouts), Label{"operation", operation.name}, Label{"bound", "stall"}))
		latencies = append(latencies, latencySample(operation.count.Latency, Label{"operation", operation.name}))
	}
	e.family(prefix+"_timeouts_total", Counter,
		what+" attempts cancelled at a bound: waiting for the first byte, or stalled between two.", timeouts...)
	e.family(prefix+"_seconds", Histogram,
		"How long each "+strings.ToLower(what)+" call took, failed ones and every attempt included.", latencies...)
}
