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

	write("sproutfs_pager_page_bytes", "gauge",
		"The pager's page, which every page count here is in.", status.Pager.PageBytes)
	write("sproutfs_pager_arena_pages", "gauge",
		"Pages the pager's arena holds.", status.Pager.ArenaPages)
	write("sproutfs_pager_resident_pages", "gauge",
		"Pages of the arena that are taken.", status.Pager.ResidentPages)
	write("sproutfs_guest_committed_bytes", "gauge",
		"Guest RAM the VMs this host runs have between them, resident or not, which is what a placement measures this host by.",
		status.Pager.CommittedBytes)
	write("sproutfs_pager_dirty_pages", "gauge",
		"Pages of volatile private state no checkpoint has published.", status.Pager.DirtyPages)
	write("sproutfs_pager_logical_pages", "gauge",
		"Pages the pager holds metadata for.", status.Pager.LogicalPages)
	write("sproutfs_pager_shared_pages_total", "counter",
		"Pages mapped to an already resident identity without a read, which is what a fork inherits.",
		status.Pager.SharedPages)
	// The sharing gauges carry the kind of region as a label: two kinds, one
	// series each, so comparing RAM against PMEM is a query rather than six
	// metrics. They are what the counter above is not — how much sharing is
	// still there, rather than how often it happened.
	kinds := []struct {
		name    string
		sharing hostapi.Sharing
	}{{"ram", status.Pager.RAM}, {"pmem", status.Pager.PMEM}}
	byKind := func(name, help string, value func(hostapi.Sharing) uint64) {
		fmt.Fprintf(&out, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, kind := range kinds {
			fmt.Fprintf(&out, "%s{kind=%q} %d\n", name, kind.name, value(kind.sharing))
		}
	}
	byKind("sproutfs_pager_unique_resident_bytes",
		"Host memory the pager's arena holds, one resident page counted once however many regions map it.",
		func(s hostapi.Sharing) uint64 { return s.UniqueBytes })
	byKind("sproutfs_pager_mapped_resident_bytes",
		"Resident pages summed over the regions that map them, counting every alias, which is what this host would hold if nothing shared anything.",
		func(s hostapi.Sharing) uint64 { return s.MappedBytes })
	byKind("sproutfs_pager_shared_saved_bytes",
		"Mapped less unique: the memory this host did not have to find because its guests are reading the same pages.",
		func(s hostapi.Sharing) uint64 { return s.SavedBytes })
	write("sproutfs_pager_faults_total", "counter", "Faults the pager has resolved.", status.Pager.Faults)
	write("sproutfs_pager_evictions_total", "counter", "Pages the pager has evicted.", status.Pager.Evictions)
	write("sproutfs_pager_spills_total", "counter", "Pages the pager has written to its spill file.", status.Pager.Spills)

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
