package host

import (
	"fmt"
	"runtime"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// The pagers' own bounds, which a deployment does not set: they follow from the
// arena each was given, the node's processors and the page each runs. Everything
// a deployment does choose — the arenas, the logical and dirty budgets — is in
// SupervisorConfig. They are here rather than beside the supervisor that starts
// the VMs so that what a node chooses for each pager is checked wherever the
// suite runs, and not only on the one platform that can start a guest.
const (
	// readAheadBytes is the aligned run one fault loads and maps, stated in
	// bytes because it is a buffer: each pager admits that many of its own
	// pages, four at 2 MiB and two thousand and forty-eight at 4 KiB. A boot, a
	// restore and a guest's own working set all walk memory forwards, so the run
	// is served by the fault that would otherwise be the first of them, and the
	// pages of it land in consecutive arena slots so that one mapping command
	// installs the whole run. It must come to a power of two pages and at most
	// 16 MiB, which is the largest buffer one fault may hold.
	readAheadBytes = 8 << 20
	// writeAheadBytes is the run one store into fresh zeros — a hole, or a page
	// the guest has never touched — gives private pages in a single mapping
	// command, likewise in bytes.
	//
	// Fresh zeros are the whole of what write-ahead serves, and they are shared
	// with nobody: a hole and a zero mapping have no resident page and no page
	// identity, so making a store's neighbours private gives back no sharing at
	// all. What it costs is an arena slot and a dirty reservation each, and a
	// run takes only slots and reservations that are already free. The pages of
	// it the guest never stores into then cost nothing past the next
	// checkpoint: they read back as zeroes, so the publication gives them no
	// object and the retire hands each one back as the hole it was, its slot and
	// its reservation with it.
	//
	// What it buys is one fault where a guest writing fresh memory forwards
	// would take a run of them. A 16 GiB guest's boot at a 4 KiB page makes
	// about 103,000 RAM pages private on GCE, and two contiguous runs are 80 %
	// of them: 65,280 pages of the kernel's memmap, 256 MiB written page by page
	// as it is initialised, and 16,384 pages of swiotlb's 64 MiB bounce buffer,
	// memset to zero in one block. Those are real stores into fresh memory, in
	// order, and at 8 MiB a run the boot faults 32 times for the two of them
	// instead of 81,664.
	//
	// It is kept to the read-ahead run rather than larger because every page of
	// it is charged a dirty reservation until the next checkpoint, and a pager
	// whose dirty budget cannot afford runs of that size keeps one page.
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
)

// pagerConfig is the configuration of one of a supervisor's two pagers: the
// share of the budgets the deployment gave that kind, the page it runs, and the
// read-ahead, write-ahead and I/O bounds that follow from the two. The
// read-ahead and write-ahead runs are stated in bytes and converted here, so a
// pager of small pages gets a run of the same size rather than the same number
// of pages.
func pagerConfig(config SupervisorConfig, kind vmmemory.RegionKind) vmmemory.Config {
	pageSize, arenaBytes, logical, dirty := uint64(PMEMPageSize), config.ArenaBytes.PMEM, config.LogicalPages.PMEM, config.DirtyPages.PMEM
	if kind == vmmemory.Ram {
		pageSize, arenaBytes, logical, dirty = ramPage(config), config.ArenaBytes.RAM, config.LogicalPages.RAM, config.DirtyPages.RAM
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
		ArenaOffsets:    arenaOffsets(pageSize, resident, logical),
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

// arenaOffsets is how many addresses a pager's arena has, which is not how many
// pages it may hold: the arena is a sparse file, so an offset costs nothing
// until a page is put there.
//
// A pager of pages smaller than a range puts a private page at the offset it has
// within its 2 MiB range, so every range a region may have written into owns a
// run of consecutive offsets however few of its pages are private. At 4 KiB a
// range is 512 pages and an extent 512 offsets, so the extents come to exactly
// the logical pages this pager admits — every page of every region it may map —
// and the read-ahead runs, which take consecutive offsets of their own, are
// bounded by what the arena can hold at once. A pager whose page is the whole
// range places nothing, so its offsets and its pages are one number.
func arenaOffsets(pageSize uint64, resident, logical int) int {
	if pageSize >= PMEMPageSize {
		return resident
	}
	return logical + resident
}

// ramPage is the RAM page a validated configuration names.
func ramPage(config SupervisorConfig) uint64 {
	page, err := RAMPage(config.RAMPageSize)
	if err != nil {
		panic(fmt.Sprintf("host: pagerConfig of an unvalidated configuration: %v", err))
	}
	return page
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
