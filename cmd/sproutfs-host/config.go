package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
	"github.com/semistrict/sproutfs/internal/vmmemory"
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
// the command line names only the filesystem and DAX.
const defaultBootArgs = "console=ttyS0 reboot=k panic=1 init=/init rootfstype=ext4 rootflags=dax=always"

// minimumCheckpointInterval is the shortest interval a deployment may configure.
// A checkpoint pauses every VM this host runs for its state capture and seal and
// then uploads its dirty set, so an interval below this is a host spending its
// guests' time on the object store rather than a durability choice. A negative
// value disables the loop, which is what a host driving its own captures wants.
const minimumCheckpointInterval = time.Second

// defaultTemplates is the one guest image the demo image carries.
const defaultTemplates = "alpine=/usr/share/sproutfs/guest.ext4"

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
			ArenaBytes:  number("SPROUTFS_ARENA_BYTES", 2<<30),
		},
	}
	// SPROUTFS_ORCHESTRATOR_URL is what the manifests set, from the
	// orchestrator's Service; the shorter name is kept for a host started by
	// hand. Without either, the Service's own DNS name in this namespace.
	c.Orchestrator = text("SPROUTFS_ORCHESTRATOR_URL", text("SPROUTFS_ORCHESTRATOR",
		fmt.Sprintf("http://sproutfs-orchestrator.%s.svc:8080", c.Namespace)))
	// One token admits the whole control plane, so a host reads the same name
	// the orchestrator and the CLI do. It is not required: a host run by hand
	// outside a cluster serves an API that admits anyone, and says so.
	c.APIToken = jsonhttp.Token(text(jsonhttp.TokenEnv, ""))
	c.MemoryBytes = number("SPROUTFS_MEMORY_BYTES", c.ArenaBytes+(1<<30))
	c.CacheBytes = number("SPROUTFS_CACHE_BYTES", 1<<30)
	c.SpillBytes = number("SPROUTFS_SPILL_BYTES", 16<<30)
	c.VMMemoryBytes = uint64(number("SPROUTFS_VM_MEMORY_BYTES", 512<<20))

	if c.ArenaBytes%vmmemory.PageSize != 0 {
		fail("SPROUTFS_ARENA_BYTES is %d, want a multiple of the pager's %d-byte page",
			c.ArenaBytes, vmmemory.PageSize)
	}
	if c.VMMemoryBytes%vmmemory.PageSize != 0 {
		fail("SPROUTFS_VM_MEMORY_BYTES is %d, want a multiple of the pager's %d-byte page",
			c.VMMemoryBytes, vmmemory.PageSize)
	}
	if c.VCPUs < 1 || c.VCPUs > 32 {
		fail("SPROUTFS_VM_VCPUS is %d, want 1 to 32", c.VCPUs)
	}
	resident := c.ArenaBytes / vmmemory.PageSize
	spillable := c.SpillBytes / vmmemory.PageSize
	// The logical cap bounds per-region metadata, which is the only thing it
	// costs: it reserves nothing, and a page that is never touched has no
	// metadata to bound. What it does decide is which VMs a host will run at
	// all, because every region of every VM is charged against it, so it has to
	// be sized by the VMs a host holds rather than by the arena they share.
	// Thirty-two arenas is twenty-two of the deployment's workload VMs, whose
	// regions are 2 GiB of RAM over a 5 GiB root; the arena and the placement
	// are what actually bound a host, and this is the backstop that catches a
	// region absurd next to them.
	c.LogicalPages = int(number("SPROUTFS_LOGICAL_PAGES", resident*32))
	c.DirtyPages = int(number("SPROUTFS_DIRTY_PAGES", min(resident, spillable)))
	if int64(c.LogicalPages) < resident {
		fail("SPROUTFS_LOGICAL_PAGES is %d, want at least the arena's %d pages", c.LogicalPages, resident)
	}
	if c.DirtyPages > c.LogicalPages {
		fail("SPROUTFS_DIRTY_PAGES is %d, want at most SPROUTFS_LOGICAL_PAGES, %d", c.DirtyPages, c.LogicalPages)
	}
	// Every dirty page must have somewhere to spill, so the spill cap is what
	// bounds the dirty budget rather than something checked as it fills.
	if int64(c.DirtyPages) > spillable {
		fail("SPROUTFS_DIRTY_PAGES is %d, and SPROUTFS_SPILL_BYTES holds %d pages", c.DirtyPages, spillable)
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

	c.Templates, err = parseTemplates(text("SPROUTFS_TEMPLATES", defaultTemplates), c.VMMemoryBytes)
	if err != nil {
		fail("SPROUTFS_TEMPLATES: %v", err)
	}
	for name, entry := range c.Templates {
		if entry.MemoryBytes%vmmemory.PageSize != 0 {
			fail("template %s asks for %d bytes of RAM, want a multiple of the pager's %d-byte page",
				name, entry.MemoryBytes, vmmemory.PageSize)
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
