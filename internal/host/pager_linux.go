//go:build linux && (amd64 || arm64)

package host

import (
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// The pager's own bounds, which a deployment does not set: they follow from the
// arena it was given, the node's processors and the 2 MiB page. Everything a
// deployment does choose — the arena, the logical and dirty budgets — is in
// SupervisorConfig.
const (
	// readAheadPages is the aligned run one fault loads and maps: four 2 MiB
	// pages, 8 MiB. A boot, a restore and a guest's own working set all walk
	// memory forwards, so the run is served by the fault that would otherwise
	// be the first of four. It must be a power of two and at most 16 MiB, which
	// is the largest buffer one fault may hold.
	readAheadPages = 4
	// writeAheadPages is the run one store into fresh zeros — a hole, or a page
	// the guest has never touched — gives private pages in a single mapping
	// command. Every page of it is charged a dirty reservation and written back
	// whether the guest uses it or not, so it is kept to the read-ahead run
	// rather than larger, and a host whose dirty budget cannot afford runs of
	// that size keeps one page.
	writeAheadPages = 4
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

// pagerConfig is the pager configuration one supervisor starts its host with:
// the budgets the deployment chose, and the read-ahead, write-ahead and I/O
// bounds that follow from them.
func pagerConfig(config SupervisorConfig) vmmemory.Config {
	resident := int(config.ArenaBytes / vmmemory.PageSize)
	writeAhead := writeAheadPages
	if config.DirtyPages < writeAhead*writeAheadDirtyShare {
		// A host this small would spend a whole store's turn of the dirty budget
		// on pages the guest may never touch.
		writeAhead = 1
	}
	return vmmemory.Config{
		ResidentPages:   resident,
		LogicalPages:    config.LogicalPages,
		DirtyPages:      config.DirtyPages,
		ConcurrentIO:    concurrentIO(resident),
		ReadAheadPages:  readAheadPages,
		WriteAheadPages: writeAhead,
		// The pager is what holds a guest back past the window, so it carries
		// the same bound the host reports and schedules its retries by.
		LossWindow: lossWindowOf(config.LossWindow),
	}
}

// concurrentIO bounds page reads and spill writes in flight. It is the node's
// processors scaled up, held between a floor worth having and a ceiling, and
// never more read-ahead runs than the arena has room for: a permit that cannot
// put its run anywhere only queues for a page.
func concurrentIO(resident int) int {
	permits := min(max(concurrentIOPerCPU*runtime.NumCPU(), minimumConcurrentIO), maximumConcurrentIO)
	return max(1, min(permits, resident/readAheadPages))
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
