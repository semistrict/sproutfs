package main

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// config is this process's environment: the host it assembles, and the names of
// the things only this command builds from them — the object store the
// deployment's VMs live in, and the port its own API listens on.
type config struct {
	host.SupervisorConfig
	// Bucket and Prefix are the deployment's object namespace, and Endpoint
	// selects a GCS emulator instead of the ambient Google credentials.
	Bucket, Prefix, Endpoint string
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

// defaultTemplates is the one guest image the demo image carries.
const defaultTemplates = "alpine=/usr/share/sproutfs/guest.ext4"

// ramPageSize and pmemPageSize are the pages this build's two pagers run, which
// a deployment does not choose: they are what vmwire, the Rust adapter and the
// Firecracker integration map, and what a HugeTLB arena is made of. They are
// here so that the byte budgets this command divides are checked against the
// pages they will actually be counted in.
const (
	ramPageSize  = checkpoint.PageSize2MiB
	pmemPageSize = checkpoint.PageSize2MiB
)

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

	c := config{
		Bucket:   required("SPROUTFS_BUCKET"),
		Prefix:   text("SPROUTFS_PREFIX", ""),
		Endpoint: text("SPROUTFS_GCS_ENDPOINT", ""),
		APIPort:  port("SPROUTFS_API_PORT", 8080),
		SupervisorConfig: host.SupervisorConfig{
			PagePort:    port("SPROUTFS_PAGE_SERVER_PORT", 8081),
			PodIP:       required("SPROUTFS_POD_IP"),
			PodName:     required("SPROUTFS_POD_NAME"),
			Namespace:   text("SPROUTFS_NAMESPACE", "sproutfs"),
			HugepageDir: text("SPROUTFS_HUGEPAGE_DIR", "/hugepages-2Mi"),
			ScratchDir:  text("SPROUTFS_SCRATCH_DIR", "/var/lib/sproutfs"),
			Firecracker: text("SPROUTFS_FIRECRACKER", "/usr/local/bin/firecracker"),
			Seccomp:     text("SPROUTFS_SECCOMP", "/usr/share/sproutfs/seccomp.bpf"),
			Kernel:      text("SPROUTFS_KERNEL", "/usr/share/sproutfs/vmlinux"),
			BootArgs:    text("SPROUTFS_BOOT_ARGS", defaultBootArgs),
			VCPUs:       int(number("SPROUTFS_VM_VCPUS", 1)),
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
	c.MemoryBytes = number("SPROUTFS_MEMORY_BYTES", arenaBytes+(1<<30))
	c.CacheBytes = number("SPROUTFS_CACHE_BYTES", 1<<30)
	spillBytes := number("SPROUTFS_SPILL_BYTES", 16<<30)
	c.VMMemoryBytes = uint64(number("SPROUTFS_VM_MEMORY_BYTES", 512<<20))

	// A host runs one pager per kind of region, each with an arena and a spill
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
	if c.SpillBytes.Total() != spillBytes {
		fail("SPROUTFS_SPILL_BYTES is %d, which %d%% cannot divide into whole %d-byte RAM pages and whole %d-byte PMEM pages",
			spillBytes, ramShare, ramPageSize, pmemPageSize)
	}
	if c.VMMemoryBytes%ramPageSize != 0 {
		fail("SPROUTFS_VM_MEMORY_BYTES is %d, want a multiple of the RAM pager's %d-byte page",
			c.VMMemoryBytes, ramPageSize)
	}
	if !mountsRootWithDAX(c.BootArgs) {
		fail("SPROUTFS_BOOT_ARGS mounts the root without rootflags=dax=always: %q", c.BootArgs)
	}
	if c.VCPUs < 1 || c.VCPUs > 32 {
		fail("SPROUTFS_VM_VCPUS is %d, want 1 to 32", c.VCPUs)
	}
	resident := host.KindPages{RAM: int(c.ArenaBytes.RAM / ramPageSize), PMEM: int(c.ArenaBytes.PMEM / pmemPageSize)}
	spillable := host.KindPages{RAM: int(c.SpillBytes.RAM / ramPageSize), PMEM: int(c.SpillBytes.PMEM / pmemPageSize)}
	// The logical cap bounds per-region metadata, which is the only thing it
	// costs: it reserves nothing, and a page that is never touched has no
	// metadata to bound. What it does decide is which VMs a host will run at
	// all, because every region of every VM is charged against the pager of its
	// kind, so it has to be sized by the VMs a host holds rather than by the
	// arena they share. Thirty-two arenas is twenty-two of the deployment's
	// workload VMs, whose regions are 2 GiB of RAM over a 5 GiB root; the arenas
	// and the placement are what actually bound a host, and this is the backstop
	// that catches a region absurd next to them. Each pager is capped in its own
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
// name=path pairs separated by commas. A pair may name the RAM its VMs get,
// after a colon — name=path:bytes — which is what an image that needs more than
// the deployment's default uses; a path with a colon in it therefore cannot
// carry one, and the default applies. defaultMemory is that default.
func parseTemplates(value string, defaultMemory uint64) (host.Templates, error) {
	templates := host.Templates{}
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
		return nil, errors.New("no template is configured")
	}
	return templates, nil
}

// environment reads the process environment, which is what main configures from.
func environment(name string) string { return os.Getenv(name) }
