package main

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
	"github.com/semistrict/sproutfs/platform/adapters"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
)

// guestCID is the context id every guest has on its own virtio-vsock device.
const guestCID = 3

// config is this process's environment: the host it assembles, and the names of
// the things only this command builds from them — the object store the
// deployment's VMs live in, and the port its own API listens on.
type config struct {
	host.SupervisorConfig
	// Firecracker is how this command runs every VMM: Firecracker directly,
	// as the host's Starter.
	Firecracker vmmachine.Firecracker
	// Store is the object store the deployment's VMs live in.
	Store adapters.ObjectStoreConfig
	// HotTier is the bucket reads of checkpoint objects try first, nil for
	// none: SPROUTFS_HOT_TIER, a URL. It and the cluster cache are
	// alternatives.
	HotTier *adapters.ObjectStoreConfig
	// CacheDir is the directory the page cache's disk is kept in, on the
	// filesystem the scratch directory is on. Empty keeps it in the scratch
	// directory.
	CacheDir string
	// Shards is the cloud whose network disks this host serves as shards
	// instead of keeping a cache disk of its own: SPROUTFS_SHARDS, gce, or
	// empty for none. Machine is the node the host runs on,
	// SPROUTFS_NODE_NAME, which the controller attaches its shards to, and
	// ShardDevices the directory the node's disks are named in,
	// SPROUTFS_SHARD_DEVICE_DIR, /dev/disk/by-id when unset.
	Shards, Machine, ShardDevices string
	// APIPort serves this process's HTTP API.
	APIPort int
}

// defaultBootArgs boots from the PMEM root with the serial console the demo
// drives the guest over. The root device is the PMEM device declared root, so
// the command line names only the filesystem and DAX. The i8042 flags are
// Firecracker's own: the guest has that controller only so that reboot=k can
// reset through it, and a kernel left to probe it for a keyboard and a mouse
// stalls for half a second before it mounts the root.
const defaultBootArgs = "console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd " +
	"init=/init rootfstype=ext4 rootflags=dax=always"

// mountsRootWithDAX reports whether a guest command line mounts the root with
// dax=always. The root is a PMEM device so that the guest maps the host's
// resident pages directly and keeps no page cache of its own; mounted any other
// way the guest boots and runs, and holds a second copy of everything the host
// already shares, which nothing else would report. With the flag, ext4 refuses
// the mount where DAX is not to be had and the guest does not boot at all.
func mountsRootWithDAX(args string) bool {
	for _, field := range strings.Fields(args) {
		flags, found := strings.CutPrefix(field, "rootflags=")
		if !found {
			continue
		}
		if slices.Contains(strings.Split(flags, ","), "dax=always") {
			return true
		}
	}
	return false
}

// minimumCheckpointInterval is the shortest interval a deployment may configure.
// A checkpoint pauses every VM this host runs for its state capture and seal and
// then uploads its dirty set, so an interval below this is a host spending its
// guests' time on the object store rather than a durability choice. A negative
// value disables the loop, which is what a host driving its own captures wants.
const minimumCheckpointInterval = time.Second

// defaultEphemeralArenaBytes is the HugeTLB share the ephemeral pager takes
// when a deployment gives it a disk and names no arena: enough to keep a
// working set of an ephemeral disk resident, and small beside the disks it may
// spill.
const defaultEphemeralArenaBytes = 256 << 20

// defaultDiskFreePercent is the share of the filesystem a host keeps free when
// a deployment names no disk goal.
const defaultDiskFreePercent = 10

// defaultDiskReserveBytes is what the page cache leaves free above the floor
// for promises not yet made: sixteen VMs' staging, or an image's staging
// sixteen regions at a time, before any cache on the filesystem has to give
// space back.
const defaultDiskReserveBytes = 1 << 30

// defaultCacheServeBytesPerSecond is a host's serving bandwidth for stripes
// of the cluster's cache: 500 MiB a second, about 40 % of a 10 Gb/s NIC.
const defaultCacheServeBytesPerSecond = 500 << 20

// defaultTemplates is the one guest image the demo image carries.
const defaultTemplates = "alpine=/usr/share/sproutfs/guest.ext4"

// defaultRAMSharePercent is how much of this host's arena, spill file and page
// budgets goes to the RAM pager when a deployment names no share. Three
// quarters, because RAM is where a guest's memory diverges and the root is
// mostly read: the 2026-09-19 fan-out measured a fork holding about 114 MB of
// RAM privately against 10 MB of root, and the deployment's workload VM keeps
// one shared root behind every fork's own RAM. A deployment whose guests write
// their disks instead sets the share.
const defaultRAMSharePercent = 75

// splitBytes divides one byte budget between the two pagers at the given
// percentage, rounding each share down to a whole page of the pager that will
// hold it. The two shares therefore need not come to the whole, which the caller
// refuses rather than quietly running a host on less than it was given.
func splitBytes(total, ramPercent int64, ramPage, pmemPage uint64) host.KindBytes {
	ram := total * ramPercent / 100 / int64(ramPage) * int64(ramPage)
	pmem := (total - ram) / int64(pmemPage) * int64(pmemPage)
	return host.KindBytes{RAM: ram, PMEM: pmem}
}

// loadConfig reads the whole environment, reporting every problem it found
// rather than the first: a pod that is misconfigured in three places should say
// so once.
func loadConfig(lookup func(string) string) (config, error) {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	text := func(name, fallback string) string {
		if value := strings.TrimSpace(lookup(name)); value != "" {
			return value
		}
		return fallback
	}
	required := func(name string) string {
		value := text(name, "")
		if value == "" {
			fail("%s is required", name)
		}
		return value
	}
	number := func(name string, fallback int64) int64 {
		value := text(name, "")
		if value == "" {
			return fallback
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			fail("%s is %q, want a positive number", name, value)
			return fallback
		}
		return parsed
	}
	port := func(name string, fallback int) int {
		value := number(name, int64(fallback))
		if value > 65535 {
			fail("%s is %d, want a port", name, value)
			return fallback
		}
		return int(value)
	}

	store, err := adapters.ObjectStoreFromEnvironment(lookup)
	if err != nil {
		errs = append(errs, err)
	}
	c := config{
		Store:   store,
		APIPort: port("SPROUTFS_API_PORT", 8080),
		SupervisorConfig: host.SupervisorConfig{
			Version:     version,
			PagePort:    port("SPROUTFS_PAGE_SERVER_PORT", 8081),
			PodIP:       required("SPROUTFS_POD_IP"),
			PodName:     required("SPROUTFS_POD_NAME"),
			Namespace:   text("SPROUTFS_NAMESPACE", "sproutfs"),
			HugepageDir: text("SPROUTFS_HUGEPAGE_DIR", "/hugepages-2Mi"),
			ScratchDir:  text("SPROUTFS_SCRATCH_DIR", "/var/lib/sproutfs"),
		},
		Firecracker: vmmachine.Firecracker{
			Binary:        text("SPROUTFS_FIRECRACKER", "/usr/local/bin/firecracker"),
			SeccompFilter: text("SPROUTFS_SECCOMP", "/usr/share/sproutfs/seccomp.bpf"),
			Kernel:        text("SPROUTFS_KERNEL", "/usr/share/sproutfs/vmlinux"),
			BootArgs:      text("SPROUTFS_BOOT_ARGS", defaultBootArgs),
			VCPUs:         int(number("SPROUTFS_VM_VCPUS", 1)),
			// Every guest knows itself by the same context id on its own
			// vsock: the device is a private channel to the process running
			// it, so the name is the guest's alone.
			VsockCID: guestCID,
		},
	}
	arenaBytes := number("SPROUTFS_ARENA_BYTES", 2<<30)
	// SPROUTFS_ORCHESTRATOR_URL is what the manifests set, from the
	// orchestrator's Service; the shorter name is kept for a host started by
	// hand. Without either, the Service's own DNS name in this namespace.
	c.Orchestrator = text("SPROUTFS_ORCHESTRATOR_URL", text("SPROUTFS_ORCHESTRATOR",
		fmt.Sprintf("http://sproutfs-orchestrator.%s.svc:8080", c.Namespace)))
	// One token admits the whole control plane, so a host reads the same name
	// the orchestrator and the CLI do. It is not required: a host run by hand
	// outside a cluster serves an API that admits anyone, and says so.
	c.APIToken = jsonhttp.Token(text(jsonhttp.TokenEnv, ""))
	// An ephemeral disk's pages are its only copy, so the pager that holds them
	// is budgeted apart from the other two. SPROUTFS_EPHEMERAL_BYTES is the disk
	// its spill file may take, which is every ephemeral disk this host admits,
	// and SPROUTFS_EPHEMERAL_ARENA_BYTES its share of the HugeTLB pool. A host
	// without the first runs no ephemeral pager and refuses an ephemeral disk.
	// PMEM's page is 2 MiB on the HugeTLB pool unless the deployment runs it at
	// 4 KiB on ordinary memory, and every PMEM and ephemeral budget below is
	// counted in it.
	pmemPageSize, err := host.PMEMPage(uint64(number("SPROUTFS_PMEM_PAGE_BYTES", int64(host.DefaultPMEMPageSize))))
	if err != nil {
		fail("SPROUTFS_PMEM_PAGE_BYTES: %v", err)
		pmemPageSize = host.DefaultPMEMPageSize
	}
	c.PMEMPageSize = pmemPageSize
	c.Ephemeral = host.EphemeralBudget{DiskBytes: number("SPROUTFS_EPHEMERAL_BYTES", 0)}
	if c.Ephemeral.DiskBytes > 0 {
		c.Ephemeral.ArenaBytes = number("SPROUTFS_EPHEMERAL_ARENA_BYTES", defaultEphemeralArenaBytes)
		for _, budget := range []struct {
			name  string
			bytes int64
		}{{"SPROUTFS_EPHEMERAL_BYTES", c.Ephemeral.DiskBytes}, {"SPROUTFS_EPHEMERAL_ARENA_BYTES", c.Ephemeral.ArenaBytes}} {
			if budget.bytes%int64(pmemPageSize) != 0 {
				fail("%s is %d, want whole %d-byte PMEM pages", budget.name, budget.bytes, pmemPageSize)
			}
		}
	}
	c.MemoryBytes = number("SPROUTFS_MEMORY_BYTES", arenaBytes+c.Ephemeral.ArenaBytes+(1<<30))
	c.CacheBytes = number("SPROUTFS_CACHE_BYTES", 1<<30)
	spillBytes := number("SPROUTFS_SPILL_BYTES", 16<<30)
	// The page cache's disk outlives the pod where the scratch does not, so
	// a deployment keeps it in a directory of the node's own, on the same
	// filesystem. Its size is the disk limiter's alone: the cap it once had
	// is refused by name, so a manifest that still sets it is not run on the
	// limiter's goals while it says otherwise.
	c.CacheDir = text("SPROUTFS_CACHE_DIR", "")
	if c.CacheDir != "" && !filepath.IsAbs(c.CacheDir) {
		fail("SPROUTFS_CACHE_DIR is %q, want an absolute directory", c.CacheDir)
	}
	// A host may serve shards, network disks the membership assigns it,
	// instead of keeping a cache disk on its node.
	// SPROUTFS_CACHE_DIR is then unused, so one manifest serves either way.
	c.Shards, c.Machine = text("SPROUTFS_SHARDS", ""), text("SPROUTFS_NODE_NAME", "")
	c.ShardDevices = text("SPROUTFS_SHARD_DEVICE_DIR", "")
	switch {
	case c.Shards == "":
	case c.Shards != "gce":
		fail("SPROUTFS_SHARDS is %q, want gce or nothing", c.Shards)
	case c.Machine == "":
		fail("SPROUTFS_SHARDS needs SPROUTFS_NODE_NAME, the node the host runs on, which its shards are attached to")
	case c.ShardDevices != "" && !filepath.IsAbs(c.ShardDevices):
		fail("SPROUTFS_SHARD_DEVICE_DIR is %q, want an absolute directory", c.ShardDevices)
	}
	if text("SPROUTFS_CACHE_DISK_BYTES", "") != "" {
		fail("SPROUTFS_CACHE_DISK_BYTES is no longer read: the disk limiter alone sets the page cache's disk; " +
			"set SPROUTFS_DISK_FREE_PERCENT, SPROUTFS_DISK_FREE_BYTES or SPROUTFS_DISK_USED_BYTES")
	}
	// The cluster cache is rolled out a share of windows at a time. Outside
	// the share, a window is kept whole on this host, whatever the list of
	// caches says. Unset, no window is in it.
	if value := text("SPROUTFS_CACHE_CLUSTER_PERCENT", ""); value != "" {
		percent, err := strconv.Atoi(value)
		if err != nil || percent < 0 || percent > 100 {
			fail("SPROUTFS_CACHE_CLUSTER_PERCENT is %q, want 0 to 100", value)
		} else {
			c.CacheClusterPercent = percent
		}
	}
	// A hot tier is a second bucket, named by a URL, that reads try before
	// the deployment's own and that reads and publications fill. It is an
	// alternative to the cluster cache, and a host given both refuses to
	// start.
	if value := text("SPROUTFS_HOT_TIER", ""); value != "" {
		hot, err := adapters.ObjectStoreFromURL(value)
		switch {
		case err != nil:
			fail("SPROUTFS_HOT_TIER: %v", err)
		case c.CacheClusterPercent > 0:
			fail("SPROUTFS_HOT_TIER and SPROUTFS_CACHE_CLUSTER_PERCENT=%d are both set: the hot tier and the "+
				"cluster cache are alternatives; unset one of them", c.CacheClusterPercent)
		default:
			// Its requests wait within the deployment's bounds, which the
			// URL does not name.
			hot.Bounds = c.Store.Bounds
			c.HotTier = &hot
		}
	}
	// A host serves stripes of the cluster's cache within a bandwidth, and
	// answers a read BUSY past it. The tail of reads from the cluster follows
	// the bytes each host serves well before its NIC's rate, so the default is
	// about 40 % of a 10 Gb/s NIC until the machine type is measured
	// (docs/measurements/gce-stripes-tail-2026-10-03.md).
	c.CacheServeBytesPerSecond = number("SPROUTFS_CACHE_SERVE_BYTES_PER_SECOND", defaultCacheServeBytesPerSecond)
	c.VMMemoryBytes = uint64(number("SPROUTFS_VM_MEMORY_BYTES", 512<<20))

	// The disk limiter keeps every goal it is given: a floor of free bytes, a
	// floor of the filesystem's share kept free, and a cap on what the host
	// holds. Each is optional, and a host given none keeps a tenth of the
	// filesystem free. Its promises are checked against them at startup.
	c.DiskGoal = resource.DiskGoal{FreeBytes: number("SPROUTFS_DISK_FREE_BYTES", 0),
		FreePercent: number("SPROUTFS_DISK_FREE_PERCENT", 0), UsedBytes: number("SPROUTFS_DISK_USED_BYTES", 0)}
	if c.DiskGoal == (resource.DiskGoal{}) {
		c.DiskGoal.FreePercent = defaultDiskFreePercent
	}
	c.DiskBandBytes = number("SPROUTFS_DISK_BAND_BYTES", 0)
	// The cache leaves a reserve free above the floor, so that a host whose
	// own cache is empty can still promise a VM's staging or receive one
	// while another host's cache on the same filesystem has filled it.
	c.DiskReserveBytes = number("SPROUTFS_DISK_RESERVE_BYTES", defaultDiskReserveBytes)
	// The disk cache's write budget is an average a day and a burst ahead of
	// it, a share of the device's rated endurance that the deployment works
	// out. Unset, the cache's writes are not limited. The burst defaults to an
	// hour's average.
	perDay := number("SPROUTFS_CACHE_WRITE_BYTES_PER_DAY", 0)
	burst := number("SPROUTFS_CACHE_WRITE_BURST_BYTES", 0)
	switch {
	case perDay == 0 && burst > 0:
		fail("SPROUTFS_CACHE_WRITE_BURST_BYTES is set without SPROUTFS_CACHE_WRITE_BYTES_PER_DAY")
	case perDay > 0:
		c.DiskWrites = resource.WriteBudget{BytesPerDay: perDay, BurstBytes: cmp.Or(burst, max(perDay/24, 1))}
	}
	if c.DiskGoal.FreePercent > 99 {
		fail("SPROUTFS_DISK_FREE_PERCENT is %d, want 1 to 99", c.DiskGoal.FreePercent)
	} else if err := c.DiskGoal.Validate(); err != nil {
		fail("the disk goal: %v", err)
	}
	if err := c.DiskWrites.Validate(); err != nil {
		fail("the cache's write budget: %v", err)
	}

	// A host runs one pager per kind of memory region, each with an arena and a spill
	// file of its own, so the byte budgets the deployment gives this host are
	// divided between them. One share decides all of them, because a deployment
	// that gives RAM three quarters of the arena wants RAM to have three
	// quarters of the spill and the budgets that fill it too, and because the
	// two halves must add up to exactly what was given however the division
	// falls: PMEM takes the remainder rather than a second rounding.
	// The names one pager's budgets had. A host that started with them set and
	// read neither would run on its defaults while its manifest said otherwise.
	for _, retired := range []struct{ name, ram, pmem string }{
		{"SPROUTFS_DIRTY_PAGES", "SPROUTFS_RAM_DIRTY_PAGES", "SPROUTFS_PMEM_DIRTY_PAGES"},
		{"SPROUTFS_LOGICAL_PAGES", "SPROUTFS_RAM_LOGICAL_PAGES", "SPROUTFS_PMEM_LOGICAL_PAGES"},
	} {
		if text(retired.name, "") != "" {
			fail("%s is no longer read: set %s and %s, each in its own pager's pages",
				retired.name, retired.ram, retired.pmem)
		}
	}
	// RAM's page is 4 KiB on ordinary memory unless the deployment runs it at
	// 2 MiB on the HugeTLB pool, and every RAM budget below is counted in it.
	ramPageSize, err := host.RAMPage(uint64(number("SPROUTFS_RAM_PAGE_BYTES", int64(host.DefaultRAMPageSize))))
	if err != nil {
		fail("SPROUTFS_RAM_PAGE_BYTES: %v", err)
		ramPageSize = host.DefaultRAMPageSize
	}
	c.RAMPageSize = ramPageSize
	ramShare := number("SPROUTFS_RAM_SHARE_PERCENT", defaultRAMSharePercent)
	if ramShare < 1 || ramShare > 99 {
		fail("SPROUTFS_RAM_SHARE_PERCENT is %d, want 1 to 99", ramShare)
	} else {
		c.ArenaBytes = splitBytes(arenaBytes, ramShare, ramPageSize, pmemPageSize)
		c.SpillBytes = splitBytes(spillBytes, ramShare, ramPageSize, pmemPageSize)
	}
	if c.ArenaBytes.Total() != arenaBytes {
		fail("SPROUTFS_ARENA_BYTES is %d, which %d%% cannot divide into whole %d-byte RAM pages and whole %d-byte PMEM pages",
			arenaBytes, ramShare, ramPageSize, pmemPageSize)
	}
	// The arena mode is how each pager divides its resident pages between the
	// files of its arena: one shared file, or a private file per memory region
	// beside read-only files of what another region may map.
	arena := text("SPROUTFS_ARENA", vmmemory.ArenaIsolated.String())
	if c.Arena, err = vmmemory.ParseArenaMode(arena); err != nil {
		fail("SPROUTFS_ARENA is %q, want shared or isolated", arena)
	}
	// The pager core is the fault and checkpoint code every pager runs: the
	// pager's own, or the port of Zircon's page layer, which runs beside it
	// until it has been measured (plans/zircon-pager-port-2026-10-05.md).
	core := text("SPROUTFS_PAGER_CORE", vmmemory.CoreZircon.String())
	if c.PagerCore, err = vmmemory.ParseCore(core); err != nil {
		fail("SPROUTFS_PAGER_CORE is %q, want current or zircon", core)
	}
	if c.SpillBytes.Total() != spillBytes {
		fail("SPROUTFS_SPILL_BYTES is %d, which %d%% cannot divide into whole %d-byte RAM pages and whole %d-byte PMEM pages",
			spillBytes, ramShare, ramPageSize, pmemPageSize)
	}
	if c.VMMemoryBytes%ramPageSize != 0 {
		fail("SPROUTFS_VM_MEMORY_BYTES is %d, want a multiple of the RAM pager's %d-byte page",
			c.VMMemoryBytes, ramPageSize)
	}
	if !mountsRootWithDAX(c.Firecracker.BootArgs) {
		fail("SPROUTFS_BOOT_ARGS mounts the root without rootflags=dax=always: %q", c.Firecracker.BootArgs)
	}
	if c.Firecracker.VCPUs < 1 || c.Firecracker.VCPUs > 32 {
		fail("SPROUTFS_VM_VCPUS is %d, want 1 to 32", c.Firecracker.VCPUs)
	}
	// Every VMM runs jailed, as a user of its own, unless the deployment says
	// none: the isolated arena holds a VMM to its own VM's memory only while it
	// runs as neither root nor this process's user.
	if jail := text("SPROUTFS_VMM_JAIL", filepath.Join(c.ScratchDir, "jail")); jail != "none" {
		c.Firecracker.Jail = &vmmachine.Jail{Root: jail,
			FirstUID: int(number("SPROUTFS_VMM_FIRST_UID", 100000)),
			UIDs:     int(number("SPROUTFS_VMM_USERS", 1024)),
			GID:      int(number("SPROUTFS_VMM_GID", 100000))}
		if !filepath.IsAbs(jail) {
			fail("SPROUTFS_VMM_JAIL is %q, want an absolute directory or none", jail)
		}
	}
	resident := host.KindPages{RAM: int(c.ArenaBytes.RAM / int64(ramPageSize)), PMEM: int(c.ArenaBytes.PMEM / int64(pmemPageSize))}
	spillable := host.KindPages{RAM: int(c.SpillBytes.RAM / int64(ramPageSize)), PMEM: int(c.SpillBytes.PMEM / int64(pmemPageSize))}
	// The logical cap bounds per-memory-region metadata, which is the only thing it
	// costs: it reserves nothing, and a page that is never touched has no
	// metadata to bound. What it does decide is which VMs a host will run at
	// all, because every memory region of every VM is charged against the pager of its
	// kind, so it has to be sized by the VMs a host holds rather than by the
	// arena they share. Thirty-two arenas is twenty-two of the deployment's
	// workload VMs, whose memory regions are 2 GiB of RAM over a 5 GiB root; the arenas
	// and the placement are what actually bound a host, and this is the backstop
	// that catches a memory region absurd next to them. Each pager is capped in its own
	// pages, which is why the two numbers are set apart.
	c.LogicalPages = host.KindPages{
		RAM:  int(number("SPROUTFS_RAM_LOGICAL_PAGES", int64(resident.RAM)*32)),
		PMEM: int(number("SPROUTFS_PMEM_LOGICAL_PAGES", int64(resident.PMEM)*32)),
	}
	c.DirtyPages = host.KindPages{
		RAM:  int(number("SPROUTFS_RAM_DIRTY_PAGES", int64(min(resident.RAM, spillable.RAM)))),
		PMEM: int(number("SPROUTFS_PMEM_DIRTY_PAGES", int64(min(resident.PMEM, spillable.PMEM)))),
	}
	for _, budget := range []struct {
		kind                                    string
		logical, dirty, resident, spillable     int
		logicalName, dirtyName, spillName, page string
	}{
		{"RAM", c.LogicalPages.RAM, c.DirtyPages.RAM, resident.RAM, spillable.RAM,
			"SPROUTFS_RAM_LOGICAL_PAGES", "SPROUTFS_RAM_DIRTY_PAGES", "SPROUTFS_SPILL_BYTES", "RAM"},
		{"PMEM", c.LogicalPages.PMEM, c.DirtyPages.PMEM, resident.PMEM, spillable.PMEM,
			"SPROUTFS_PMEM_LOGICAL_PAGES", "SPROUTFS_PMEM_DIRTY_PAGES", "SPROUTFS_SPILL_BYTES", "PMEM"},
	} {
		if budget.logical < budget.resident {
			fail("%s is %d, want at least the %s arena's %d pages", budget.logicalName, budget.logical, budget.kind, budget.resident)
		}
		if budget.dirty > budget.logical {
			fail("%s is %d, want at most %s, %d", budget.dirtyName, budget.dirty, budget.logicalName, budget.logical)
		}
		// Every dirty page must have somewhere to spill, so the spill cap is
		// what bounds the dirty budget rather than something checked as it
		// fills.
		if budget.dirty > budget.spillable {
			fail("%s is %d, and the %s share of %s holds %d pages", budget.dirtyName, budget.dirty,
				budget.kind, budget.spillName, budget.spillable)
		}
	}

	interval := text("SPROUTFS_CHECKPOINT_INTERVAL", "60s")
	parsed, err := time.ParseDuration(interval)
	switch {
	case err != nil:
		fail("SPROUTFS_CHECKPOINT_INTERVAL is %q, want a duration such as 60s", interval)
	case parsed > 0 && parsed < minimumCheckpointInterval:
		// A checkpoint pauses every VM this host runs and uploads its dirty
		// set. Under a second is not a configuration: it is a host that spends
		// its guests' time on the object store.
		fail("SPROUTFS_CHECKPOINT_INTERVAL is %s, want at least %s or a negative value to disable it",
			parsed, minimumCheckpointInterval)
	}
	c.CheckpointInterval = parsed

	// The window is what bounds a host loss in time, where the interval bounds
	// it when everything works. Zero here is the deployment turning it off,
	// which the host spells as a negative value — zero there is the default.
	window := text("SPROUTFS_LOSS_WINDOW", "5m")
	held, windowErr := time.ParseDuration(window)
	switch {
	case windowErr != nil || held < 0:
		fail("SPROUTFS_LOSS_WINDOW is %q, want a duration such as 5m, or 0 to disable it", window)
	case held == 0:
		c.LossWindow = -1
	case parsed > 0 && held < parsed:
		// Every VM would be past the window before its first checkpoint was
		// due, so every guest would wait at every interval. That is not a tight
		// bound, it is a host that cannot keep the one it was given.
		fail("SPROUTFS_LOSS_WINDOW is %s, want at least SPROUTFS_CHECKPOINT_INTERVAL, %s", held, parsed)
	default:
		c.LossWindow = held
	}

	// A flush waits while the VM's disks hold a write older than the bound.
	// Unset leaves the host's default, twice the checkpoint interval. Zero here
	// means a guest never waits, which the host represents as a negative value,
	// because zero there selects the default.
	if flush := text("SPROUTFS_FLUSH_BOUND", ""); flush != "" {
		bound, flushErr := time.ParseDuration(flush)
		switch {
		case flushErr != nil || bound < 0:
			fail("SPROUTFS_FLUSH_BOUND is %q, want a duration such as 120s, or 0 to disable it", flush)
		case bound == 0:
			c.FlushBound = -1
		default:
			c.FlushBound = bound
		}
	}

	c.Templates, err = parseTemplates(text("SPROUTFS_TEMPLATES", defaultTemplates), c.VMMemoryBytes)
	if err != nil {
		fail("SPROUTFS_TEMPLATES: %v", err)
	}
	for name, entry := range c.Templates {
		if entry.MemoryBytes%ramPageSize != 0 {
			fail("template %s asks for %d bytes of RAM, want a multiple of the RAM pager's %d-byte page",
				name, entry.MemoryBytes, ramPageSize)
		}
	}
	if len(errs) > 0 {
		return config{}, errors.Join(errs...)
	}
	return c, nil
}

// parseTemplates reads the guest images a VM can be created from, written as
// name=path pairs separated by commas. "none" configures no image at all: a
// deployment whose images a builder produces imports each of them on request,
// and creates from it by its identity. A pair may name the RAM its VMs get,
// after a colon — name=path:bytes — which is what an image that needs more than
// the deployment's default uses; a path with a colon in it therefore cannot
// carry one, and the default applies. defaultMemory is that default.
func parseTemplates(value string, defaultMemory uint64) (host.Templates, error) {
	templates := host.Templates{}
	if value == "none" {
		return templates, nil
	}
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, path, found := strings.Cut(entry, "=")
		name, path = strings.TrimSpace(name), strings.TrimSpace(path)
		if !found || name == "" || path == "" {
			return nil, fmt.Errorf("%q is not name=path", entry)
		}
		if _, exists := templates[name]; exists {
			return nil, fmt.Errorf("%q is declared twice", name)
		}
		memory := defaultMemory
		if head, tail, split := strings.Cut(path, ":"); split {
			size, err := strconv.ParseUint(strings.TrimSpace(tail), 10, 64)
			if err != nil || size == 0 {
				return nil, fmt.Errorf("%q names %q as the RAM of %s, want a positive number of bytes",
					entry, tail, name)
			}
			path, memory = strings.TrimSpace(head), size
		}
		if path == "" {
			return nil, fmt.Errorf("%q is not name=path", entry)
		}
		templates[name] = host.Template{Path: path, MemoryBytes: memory}
	}
	if len(templates) == 0 {
		return nil, errors.New(`no template is configured; "none" says so on purpose`)
	}
	return templates, nil
}

// environment reads the process environment, which is what main configures from.
func environment(name string) string { return os.Getenv(name) }
