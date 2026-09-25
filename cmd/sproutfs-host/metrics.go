package main

import (
	"fmt"
	"strings"
	"time"

	hostapi "github.com/semistrict/sproutfs/internal/api/host"
)

// metrics writes one host's status as Prometheus text. The format is four lines
// per metric and nothing else, so it is written by hand rather than linked: a
// client library would be a dependency, a registry and a set of collectors for
// numbers this host already has in one struct.
//
// Everything here is read from Status, which is the same report the API and the
// orchestrator's survey see. Counters end in _total and are monotonic for one
// process lifetime — a host restart is a host loss, so they start again at zero
// with everything else this process holds.
func metrics(status hostapi.Status) string {
	var out strings.Builder
	write := func(name, kind, help string, value any) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, value)
	}
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

	// A host runs one pager per kind of memory region, so every pager series carries
	// the kind as a label: two kinds, one series each, and comparing RAM against
	// PMEM is a query rather than twice the metrics. Page counts have to be
	// labelled — the two pagers run their own pages, so a sum of them would mean
	// nothing — and the bytes are labelled beside them for the same reason a
	// deployment plans for the two separately.
	kinds := []struct {
		name  string
		pager hostapi.PagerKind
	}{{"ram", status.Pager.RAM}, {"pmem", status.Pager.PMEM}}
	byKind := func(name, metric, help string, value func(hostapi.PagerKind) any) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, metric)
		for _, kind := range kinds {
			fmt.Fprintf(&out, "%s{kind=%q} %v\n", name, kind.name, value(kind.pager))
		}
	}
	byKind("sproutfs_pager_page_bytes", "gauge",
		"The page each pager runs, which its page counts here are in.",
		func(p hostapi.PagerKind) any { return p.PageBytes })
	byKind("sproutfs_pager_arena_pages", "gauge",
		"Pages each pager's arena holds.", func(p hostapi.PagerKind) any { return p.ArenaPages })
	byKind("sproutfs_pager_resident_pages", "gauge",
		"Pages of each arena that are taken.", func(p hostapi.PagerKind) any { return p.ResidentPages })
	byKind("sproutfs_pager_dirty_pages", "gauge",
		"Pages of volatile private state no checkpoint has published.",
		func(p hostapi.PagerKind) any { return p.DirtyPages })
	byKind("sproutfs_pager_logical_pages", "gauge",
		"Pages each pager holds metadata for.", func(p hostapi.PagerKind) any { return p.LogicalPages })
	write("sproutfs_pager_arena_bytes", "gauge",
		"What the two arenas hold together, which is the only unit their capacities can be added in.",
		status.Pager.ArenaBytes())
	write("sproutfs_guest_committed_bytes", "gauge",
		"Guest RAM the VMs this host runs have between them, resident or not, which is what a placement measures this host by.",
		status.Pager.CommittedBytes)
	byKind("sproutfs_pager_shared_pages_total", "counter",
		"Pages mapped to an already resident identity without a read, which is what a fork inherits.",
		func(p hostapi.PagerKind) any { return p.SharedPages })
	// The sharing gauges are what the counter above is not — how much sharing is
	// still there, rather than how often it happened.
	byKind("sproutfs_pager_unique_resident_bytes", "gauge",
		"Host memory the pager's arena holds, one resident page counted once however many memory regions map it.",
		func(p hostapi.PagerKind) any { return p.Sharing.UniqueBytes })
	byKind("sproutfs_pager_mapped_resident_bytes", "gauge",
		"Resident pages summed over the memory regions that map them, counting every alias, which is what this host would hold if nothing shared anything.",
		func(p hostapi.PagerKind) any { return p.Sharing.MappedBytes })
	byKind("sproutfs_pager_shared_saved_bytes", "gauge",
		"Mapped less unique: the memory this host did not have to find because its guests are reading the same pages.",
		func(p hostapi.PagerKind) any { return p.Sharing.SavedBytes })
	byKind("sproutfs_pager_faults_total", "counter", "Faults the pager has resolved.",
		func(p hostapi.PagerKind) any { return p.Faults })
	byKind("sproutfs_pager_evictions_total", "counter", "Pages the pager has evicted.",
		func(p hostapi.PagerKind) any { return p.Evictions })
	byKind("sproutfs_pager_spills_total", "counter", "Pages the pager has written to its spill file.",
		func(p hostapi.PagerKind) any { return p.Spills })

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

	// The store counters carry the operation as a label: five operations, one
	// series each, which is what makes a rate by operation a query rather than
	// five metrics.
	operations := []struct {
		name  string
		count hostapi.StoreCount
	}{
		{"head", status.Store.Head}, {"get", status.Store.Get}, {"put", status.Store.Put},
		{"delete", status.Store.Delete}, {"list", status.Store.List},
	}
	labelled := func(name, help string, value func(hostapi.StoreCount) int64) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		for _, operation := range operations {
			fmt.Fprintf(&out, "%s{operation=%q} %d\n", name, operation.name, value(operation.count))
		}
	}
	labelled("sproutfs_store_calls_total", "Object store calls this host has made.",
		func(c hostapi.StoreCount) int64 { return c.Calls })
	labelled("sproutfs_store_failures_total", "Object store calls that reported an error.",
		func(c hostapi.StoreCount) int64 { return c.Failures })
	labelled("sproutfs_store_bytes_total", "Object bytes moved, which only get and put move.",
		func(c hostapi.StoreCount) int64 { return c.Bytes })
	return out.String()
}

// lossWindow reduces the per-VM report to the two numbers a scrape carries: the
// widest window on this host, and how many VMs are past theirs.
func lossWindow(vms []hostapi.VM) (widest time.Duration, waiting int) {
	for _, vm := range vms {
		widest = max(widest, vm.LossWindow)
		if vm.Waiting {
			waiting++
		}
	}
	return widest, waiting
}
