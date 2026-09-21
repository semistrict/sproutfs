//go:build linux && (amd64 || arm64)

package host

import (
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// The pagers' own bounds, which a deployment does not set: they follow from the
// arena each was given, the node's processors and the page each runs. Everything
// a deployment does choose — the arenas, the logical and dirty budgets — is in
// SupervisorConfig.
const (
	// ramPageSize and pmemPageSize are the pages this build's two pagers run.
	// Both are 2 MiB: vmwire, the Rust adapter and the Firecracker integration
	// map that page and nothing else, and the arenas are HugeTLB, which is the
	// same statement made of the memory behind them. Step 4 of the page-geometry
	// plan changes the RAM half of that, together with the wire.
	ramPageSize  = checkpoint.PageSize2MiB
	pmemPageSize = checkpoint.PageSize2MiB
	// readAheadBytes is the aligned run one fault loads and maps, stated in
	// bytes because it is a buffer: each pager admits that many of its own
	// pages, four at 2 MiB. A boot, a restore and a guest's own working set all
	// walk memory forwards, so the run is served by the fault that would
	// otherwise be the first of four. It must come to a power of two pages and
	// at most 16 MiB, which is the largest buffer one fault may hold.
	readAheadBytes = 8 << 20
	// writeAheadBytes is the run one store into fresh zeros — a hole, or a page
	// the guest has never touched — gives private pages in a single mapping
	// command, likewise in bytes. Every page of it is charged a dirty
	// reservation and written back whether the guest uses it or not, so it is
	// kept to the read-ahead run rather than larger, and a pager whose dirty
	// budget cannot afford runs of that size keeps one page.
	writeAheadBytes = 8 << 20
	// writeAheadDirtyShare is the fraction of the dirty budget one write-ahead
	// run may take before the run is not worth its reservations.
	writeAheadDirtyShare = 64
	// concurrentIOPerCPU is how many page reads and spill writes one processor
	// is given in flight, and the bounds the result is held between. Each
	// permit can hold one read-ahead run, so the budget is both the parallelism
	// a node can use and a bound on the buffers it costs.
	concurrentIOPerCPU  = 4
	minimumConcurrentIO = 16
	maximumConcurrentIO = 256
	// maximumSettleWorkers bounds the workers one settle divides a sealed set
	// between. A settle is a comparison of resident pages and no I/O at all, so
	// the node's processors are what it can use; past a few dozen it is memory
	// bandwidth that bounds it and more workers buy nothing.
	maximumSettleWorkers = 64
	// faultWorkersPerCPU bounds faults served concurrently, and the bounds the
	// result is held between. A fault spends most of its life in a store read,
	// so a node serves more of them than it has processors; the I/O budget is
	// what actually bounds the reads.
	faultWorkersPerCPU  = 2
	minimumFaultWorkers = 8
	maximumFaultWorkers = 64
	// vmaHeadroom is the share of the node's mapping-count limit this host
	// admits a VMM's replacements against. The pager's mappings are not the only
	// ones a VMM process has, and the limit is the kernel's for the whole
	// address space, so half of it is the budget and the rest is headroom.
	vmaHeadroom = 2
	// minimumVMAs is the smallest budget worth admitting against; below it the
	// budget is disabled instead, which is also what a client without /proc gets.
	minimumVMAs = 128
	maximumVMAs = 1 << 20
	// maxMapCountPath is where Linux reports that limit.
	maxMapCountPath = "/proc/sys/vm/max_map_count"
)

// pagerConfig is the configuration of one of a supervisor's two pagers: the
// share of the budgets the deployment gave that kind, the page it runs, and the
// read-ahead, write-ahead and I/O bounds that follow from the two. The
// read-ahead and write-ahead runs are stated in bytes and converted here, so a
// pager of small pages gets a run of the same size rather than the same number
// of pages.
func pagerConfig(config SupervisorConfig, kind vmmemory.RegionKind) vmmemory.Config {
	pageSize, arenaBytes, logical, dirty := uint64(pmemPageSize), config.ArenaBytes.PMEM, config.LogicalPages.PMEM, config.DirtyPages.PMEM
	if kind == vmmemory.Ram {
		pageSize, arenaBytes, logical, dirty = ramPageSize, config.ArenaBytes.RAM, config.LogicalPages.RAM, config.DirtyPages.RAM
	}
	resident := int(uint64(arenaBytes) / pageSize)
	readAhead := int(max(readAheadBytes/pageSize, 1))
	writeAhead := int(max(writeAheadBytes/pageSize, 1))
	if dirty < writeAhead*writeAheadDirtyShare {
		// A pager this small would spend a whole store's turn of the dirty
		// budget on pages the guest may never touch.
		writeAhead = 1
	}
	return vmmemory.Config{
		PageSize:        pageSize,
		ResidentPages:   resident,
		LogicalPages:    logical,
		DirtyPages:      dirty,
		ConcurrentIO:    concurrentIO(resident, readAhead),
		ReadAheadPages:  readAhead,
		WriteAheadPages: writeAhead,
		SettleWorkers:   min(max(runtime.NumCPU(), 1), maximumSettleWorkers),
		// The pager is what holds a guest back past the window, so it carries
		// the same bound the host reports and schedules its retries by.
		LossWindow: lossWindowOf(config.LossWindow),
	}
}

// concurrentIO bounds page reads and spill writes in flight. It is the node's
// processors scaled up, held between a floor worth having and a ceiling, and
// never more read-ahead runs than the arena has room for: a permit that cannot
// put its run anywhere only queues for a page.
func concurrentIO(resident, readAhead int) int {
	permits := min(max(concurrentIOPerCPU*runtime.NumCPU(), minimumConcurrentIO), maximumConcurrentIO)
	return max(1, min(permits, resident/max(readAhead, 1)))
}

// faultWorkers bounds the faults one session serves at a time.
func faultWorkers() int {
	return min(max(faultWorkersPerCPU*runtime.NumCPU(), minimumFaultWorkers), maximumFaultWorkers)
}

// vmaBudget is the mapping-count budget a VMM's replacements are admitted
// against, read from the node's own limit with headroom. Zero disables the
// budget, which is what a client that cannot read /proc gets anyway, and is
// what an unreadable or implausibly small limit is answered with.
func vmaBudget() int {
	raw, err := os.ReadFile(maxMapCountPath)
	if err != nil {
		slog.Warn("host: no mapping-count limit to admit against", "path", maxMapCountPath, "error", err)
		return 0
	}
	limit, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || limit <= 0 {
		slog.Warn("host: unreadable mapping-count limit", "path", maxMapCountPath, "value", strings.TrimSpace(string(raw)))
		return 0
	}
	budget := limit / vmaHeadroom
	if budget < minimumVMAs {
		return 0
	}
	return min(budget, maximumVMAs)
}
