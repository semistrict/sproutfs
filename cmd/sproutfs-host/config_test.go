package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/host"
)

// environ is one pod's environment.
func environ(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

// minimal is what the manifests always supply.
func minimal() map[string]string {
	return map[string]string{
		"SPROUTFS_BUCKET":   "sproutfs-demo",
		"SPROUTFS_PREFIX":   "demo",
		"SPROUTFS_POD_IP":   "10.0.0.7",
		"SPROUTFS_POD_NAME": "sproutfs-host-abc",
	}
}

func TestConfigTakesTheDocumentedDefaults(t *testing.T) {
	config, err := loadConfig(environ(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	if config.APIPort != 8080 || config.PagePort != 8081 {
		t.Fatalf("ports %d %d", config.APIPort, config.PagePort)
	}
	if config.HugepageDir != "/hugepages-2Mi" || config.ScratchDir != "/var/lib/sproutfs" {
		t.Fatalf("directories %s %s", config.HugepageDir, config.ScratchDir)
	}
	if config.Firecracker != "/usr/local/bin/firecracker" ||
		config.Seccomp != "/usr/share/sproutfs/seccomp.bpf" ||
		config.Kernel != "/usr/share/sproutfs/vmlinux" {
		t.Fatalf("VMM paths %s %s %s", config.Firecracker, config.Seccomp, config.Kernel)
	}
	if config.CheckpointInterval != 60*time.Second {
		t.Fatalf("checkpoint interval %s", config.CheckpointInterval)
	}
	if config.LossWindow != 5*time.Minute {
		t.Fatalf("loss window %s", config.LossWindow)
	}
	// The arena and the spill file are divided between the two pagers, three
	// quarters to RAM, and the two shares come to exactly what the deployment
	// gave this host.
	if config.ArenaBytes != (host.KindBytes{RAM: 3 << 29, PMEM: 1 << 29}) ||
		config.MemoryBytes != (2<<30)+(1<<30) || config.CacheBytes != 1<<30 ||
		config.SpillBytes != (host.KindBytes{RAM: 12 << 30, PMEM: 4 << 30}) {
		t.Fatalf("budgets %v %d %d %v", config.ArenaBytes, config.MemoryBytes, config.CacheBytes, config.SpillBytes)
	}
	if config.ArenaBytes.Total() != 2<<30 || config.SpillBytes.Total() != 16<<30 {
		t.Fatalf("the shares come to %d of arena and %d of spill", config.ArenaBytes.Total(), config.SpillBytes.Total())
	}
	// Each pager's resident pages are its own arena, and its other two bounds
	// are derived from that — each counted in that pager's own page, which is
	// why a RAM page other than the default would make them nothing alike: at
	// the default 2 MiB, 1.5 GiB of RAM arena is 768 pages and 512 MiB of PMEM
	// arena is 256.
	if config.LogicalPages != (host.KindPages{RAM: 768 * 32, PMEM: 8 * 1024}) ||
		config.DirtyPages != (host.KindPages{RAM: 768, PMEM: 256}) {
		t.Fatalf("pager bounds %v %v", config.LogicalPages, config.DirtyPages)
	}
	if config.VMMemoryBytes != 512<<20 || config.VCPUs != 1 {
		t.Fatalf("VM shape %d %d", config.VMMemoryBytes, config.VCPUs)
	}
	if config.BootArgs != defaultBootArgs {
		t.Fatalf("boot args %q", config.BootArgs)
	}
	if config.Orchestrator != "http://sproutfs-orchestrator.sproutfs.svc:8080" {
		t.Fatalf("orchestrator %q", config.Orchestrator)
	}
	if entry := config.Templates["alpine"]; entry.Path != "/usr/share/sproutfs/guest.ext4" ||
		entry.MemoryBytes != 512<<20 {
		t.Fatalf("templates %v", config.Templates)
	}
}

func TestConfigReportsEveryMissingVariableAtOnce(t *testing.T) {
	_, err := loadConfig(environ(map[string]string{}))
	if err == nil {
		t.Fatal("an empty environment configured a host")
	}
	for _, want := range []string{"SPROUTFS_BUCKET is required", "SPROUTFS_POD_IP is required",
		"SPROUTFS_POD_NAME is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not report %q", err, want)
		}
	}
}

func TestConfigRefusesAnArenaThatIsNotWholePages(t *testing.T) {
	values := minimal()
	values["SPROUTFS_ARENA_BYTES"] = "3000000"
	_, err := loadConfig(environ(values))
	if err == nil {
		t.Fatal("an arena of partial pages was accepted")
	}
	if !strings.Contains(err.Error(), "SPROUTFS_ARENA_BYTES is 3000000") {
		t.Fatalf("error %q", err)
	}
}

// Each pager is bounded in its own pages, so each is refused on its own.
func TestConfigRefusesADirtyBoundAboveTheLogicalOne(t *testing.T) {
	values := minimal()
	values["SPROUTFS_RAM_LOGICAL_PAGES"] = "2048"
	values["SPROUTFS_RAM_DIRTY_PAGES"] = "4096"
	_, err := loadConfig(environ(values))
	if err == nil {
		t.Fatal("a dirty bound above the logical one was accepted")
	}
	if !strings.Contains(err.Error(), "SPROUTFS_RAM_DIRTY_PAGES is 4096") {
		t.Fatalf("error %q", err)
	}
}

// A Firecracker guest has an i8042 controller only so that reboot=k can reset
// through it. Left to probe it for a keyboard and a mouse, the kernel stalls
// for half a second before it mounts the root — 0.215 s to 0.702 s in a guest's
// own log, measured on 2026-09-21 — and every cold boot pays it.
func TestTheDefaultBootArgsDoNotProbeTheKeyboardController(t *testing.T) {
	config, err := loadConfig(environ(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"i8042.noaux", "i8042.nomux", "i8042.nopnp", "i8042.dumbkbd"} {
		if !slices.Contains(strings.Fields(config.BootArgs), flag) {
			t.Fatalf("boot args %q do not carry %s", config.BootArgs, flag)
		}
	}
}

// A deployment written for one pager set SPROUTFS_DIRTY_PAGES and
// SPROUTFS_LOGICAL_PAGES. A host that went on starting with those set and read
// neither would run on its defaults while its manifest said otherwise, so each
// is refused with the two names that replaced it.
func TestConfigRefusesTheBudgetNamesOfASinglePager(t *testing.T) {
	for _, retired := range []struct{ name, ram, pmem string }{
		{"SPROUTFS_DIRTY_PAGES", "SPROUTFS_RAM_DIRTY_PAGES", "SPROUTFS_PMEM_DIRTY_PAGES"},
		{"SPROUTFS_LOGICAL_PAGES", "SPROUTFS_RAM_LOGICAL_PAGES", "SPROUTFS_PMEM_LOGICAL_PAGES"},
	} {
		values := minimal()
		values[retired.name] = "6144"
		_, err := loadConfig(environ(values))
		if err == nil {
			t.Fatalf("%s was accepted and ignored", retired.name)
		}
		want := retired.name + " is no longer read: set " + retired.ram + " and " + retired.pmem
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q, want it to say %q", err, want)
		}
	}
}

// The share a deployment sets divides the arena and the spill file between the
// two pagers, and whatever it is the two come to exactly what this host was
// given: an arena the share cannot divide into whole pages of both is refused
// rather than quietly run on less, and the shares themselves are whole pages.
func TestConfigDividesTheBudgetsByTheShare(t *testing.T) {
	for _, share := range []struct {
		percent    string
		ram, pmem  int64
		spillRAM   int64
		spillPMEM  int64
		ramDirty   int
		pmemDirty  int
		ramLogical int
	}{
		// Each pager's page counts are its own, here both 2 MiB.
		{"50", 1 << 30, 1 << 30, 8 << 30, 8 << 30, 512, 512, 512 * 32},
		{"25", 1 << 29, 3 << 29, 4 << 30, 12 << 30, 256, 768, 256 * 32},
	} {
		values := minimal()
		values["SPROUTFS_RAM_SHARE_PERCENT"] = share.percent
		config, err := loadConfig(environ(values))
		if err != nil {
			t.Fatalf("share %s%%: %v", share.percent, err)
		}
		if config.ArenaBytes != (host.KindBytes{RAM: share.ram, PMEM: share.pmem}) ||
			config.ArenaBytes.Total() != 2<<30 {
			t.Fatalf("share %s%% gave the arena %v", share.percent, config.ArenaBytes)
		}
		if config.SpillBytes != (host.KindBytes{RAM: share.spillRAM, PMEM: share.spillPMEM}) ||
			config.SpillBytes.Total() != 16<<30 {
			t.Fatalf("share %s%% gave the spill file %v", share.percent, config.SpillBytes)
		}
		if config.DirtyPages != (host.KindPages{RAM: share.ramDirty, PMEM: share.pmemDirty}) {
			t.Fatalf("share %s%% gave the dirty budgets %v", share.percent, config.DirtyPages)
		}
		if config.LogicalPages.RAM != share.ramLogical {
			t.Fatalf("share %s%% gave the RAM logical cap %d", share.percent, config.LogicalPages.RAM)
		}
	}
	// A share outside the range is a host with a pager of nothing.
	values := minimal()
	values["SPROUTFS_RAM_SHARE_PERCENT"] = "100"
	if _, err := loadConfig(environ(values)); err == nil ||
		!strings.Contains(err.Error(), "SPROUTFS_RAM_SHARE_PERCENT is 100") {
		t.Fatalf("a whole-arena share was accepted: %v", err)
	}
}

// A window of zero is how a deployment turns the bound off, which the host
// spells as a negative value: zero there is the default rather than nothing at
// all, and a pod that asked for no window must not be given five minutes of one.
func TestConfigDisablesTheLossWindowOnZero(t *testing.T) {
	values := minimal()
	values["SPROUTFS_LOSS_WINDOW"] = "0"
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatal(err)
	}
	if config.LossWindow >= 0 {
		t.Fatalf("a window of zero configured %s, want the bound disabled", config.LossWindow)
	}
}

// A window shorter than the checkpoint interval is a window every VM is past
// before its first checkpoint is even due, so every guest waits at every
// interval. It is a misconfiguration rather than a tight bound, and a pod says
// so at startup instead of discovering it as stalled guests.
func TestConfigRefusesALossWindowBelowTheCheckpointInterval(t *testing.T) {
	values := minimal()
	values["SPROUTFS_CHECKPOINT_INTERVAL"] = "60s"
	values["SPROUTFS_LOSS_WINDOW"] = "10s"
	_, err := loadConfig(environ(values))
	if err == nil {
		t.Fatal("a loss window below the checkpoint interval was accepted")
	}
	if !strings.Contains(err.Error(), "SPROUTFS_LOSS_WINDOW is 10s") {
		t.Fatalf("error %q", err)
	}
}

// TestConfigRefusesBootArgsThatMountTheRootWithoutDAX: the root is a PMEM
// device so that the guest maps the host's resident pages directly and keeps no
// page cache of its own. A command line that mounts it any other way gives
// every guest a second copy of what the host already shares, and nothing else
// would say so — the guest boots and runs.
func TestConfigRefusesBootArgsThatMountTheRootWithoutDAX(t *testing.T) {
	values := minimal()
	values["SPROUTFS_BOOT_ARGS"] = "console=ttyS0 reboot=k panic=1 init=/init rootfstype=ext4"
	_, err := loadConfig(environ(values))
	if err == nil {
		t.Fatal("a command line that mounts the root without DAX was accepted")
	}
	if !strings.Contains(err.Error(), "SPROUTFS_BOOT_ARGS mounts the root without rootflags=dax=always") {
		t.Fatalf("error %q", err)
	}
}

// TestConfigAcceptsBootArgsThatKeepDAXAmongOtherRootFlags.
func TestConfigAcceptsBootArgsThatKeepDAXAmongOtherRootFlags(t *testing.T) {
	values := minimal()
	values["SPROUTFS_BOOT_ARGS"] = "console=ttyS0 quiet init=/init rootfstype=ext4 rootflags=commit=30,dax=always"
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatal(err)
	}
	if config.BootArgs != values["SPROUTFS_BOOT_ARGS"] {
		t.Fatalf("boot args %q", config.BootArgs)
	}
}

func TestConfigReadsEveryTemplate(t *testing.T) {
	values := minimal()
	values["SPROUTFS_TEMPLATES"] = "alpine=/images/alpine.ext4, debian=/images/debian.ext4"
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatal(err)
	}
	if config.Templates["alpine"].Path != "/images/alpine.ext4" ||
		config.Templates["debian"].Path != "/images/debian.ext4" {
		t.Fatalf("templates %v", config.Templates)
	}
	name, chosen, err := config.Templates.Resolve("debian")
	if err != nil {
		t.Fatal(err)
	}
	if name != "debian" || chosen.Path != "/images/debian.ext4" {
		t.Fatalf("resolved %s %v", name, chosen)
	}
	// With more than one image a request must say which it wants.
	if _, _, err := config.Templates.Resolve(""); err == nil {
		t.Fatal("an unnamed template chose between two images")
	}
	if _, _, err := config.Templates.Resolve("windows"); err == nil {
		t.Fatal("an unconfigured template was resolved")
	}
}

func TestASingleTemplateIsWhatAnUnnamedRequestMeans(t *testing.T) {
	config, err := loadConfig(environ(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	name, chosen, err := config.Templates.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if name != "alpine" || chosen.Path != "/usr/share/sproutfs/guest.ext4" {
		t.Fatalf("resolved %s %v", name, chosen)
	}
}

// A template may name the RAM its VMs get, which is what an image needing more
// than the deployment's default is configured with.
func TestATemplateNamesTheMemoryItsVMsGet(t *testing.T) {
	values := minimal()
	values["SPROUTFS_TEMPLATES"] = "alpine=/images/alpine.ext4,workload=/images/workload.ext4:2147483648"
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatal(err)
	}
	if entry := config.Templates["workload"]; entry.Path != "/images/workload.ext4" ||
		entry.MemoryBytes != 2<<30 {
		t.Fatalf("workload template %v", config.Templates["workload"])
	}
	// The one that names no size keeps the deployment's default.
	if entry := config.Templates["alpine"]; entry.MemoryBytes != 512<<20 {
		t.Fatalf("alpine template %v", entry)
	}
}

func TestATemplateMemoryMustBeWholePages(t *testing.T) {
	values := minimal()
	values["SPROUTFS_TEMPLATES"] = "workload=/images/workload.ext4:3000000"
	_, err := loadConfig(environ(values))
	if err == nil {
		t.Fatal("a template of partial pages was accepted")
	}
	if !strings.Contains(err.Error(), "template workload asks for 3000000 bytes of RAM") {
		t.Fatalf("error %q", err)
	}
}

func TestConfigRefusesMalformedTemplates(t *testing.T) {
	values := minimal()
	values["SPROUTFS_TEMPLATES"] = "alpine"
	if _, err := loadConfig(environ(values)); err == nil {
		t.Fatal("a template with no path was accepted")
	}
	values["SPROUTFS_TEMPLATES"] = "alpine=/a,alpine=/b"
	if _, err := loadConfig(environ(values)); err == nil {
		t.Fatal("a template declared twice was accepted")
	}
	values["SPROUTFS_TEMPLATES"] = "alpine=/a:not-a-size"
	if _, err := loadConfig(environ(values)); err == nil {
		t.Fatal("a template whose RAM is not a number was accepted")
	}
}

// TestTheDefaultLogicalCapAdmitsTheDeployment. The logical cap is what admits
// a VM: it bounds per-region metadata and the pager checks it one attachment at
// a time, so a host that runs out of it kills a guest part way through starting
// one. The deployment the manifests describe gives a host a 5 GiB arena and its
// workload template gives every VM 2 GiB of RAM over the image's 5 GiB root,
// which is 3,584 pager pages a VM, and every fork lands on its parent's host, so
// the workload run's five VMs are all on one. Eight arenas is 20,480 pages,
// which admits those five and leaves room for no sixth: that VM's RAM was
// admitted and its root refused, killing the guest part way through a restore.
func TestTheDefaultLogicalCapAdmitsTheDeployment(t *testing.T) {
	values := minimal()
	values["SPROUTFS_ARENA_BYTES"] = "5368709120"
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatal(err)
	}
	// Each pager holds one kind of region, so each cap is asked about the
	// regions it would hold: six VMs' RAM against the RAM pager's, six roots
	// against the PMEM pager's.
	const ramPagesPerVM = (2 << 30) / (2 << 20)
	const rootPagesPerVM = (5 << 30) / (2 << 20)
	const vms = 6
	if config.LogicalPages.RAM < ramPagesPerVM*vms {
		t.Fatalf("the default RAM logical cap is %d pages, and the deployment's %d VMs need %d",
			config.LogicalPages.RAM, vms, ramPagesPerVM*vms)
	}
	if config.LogicalPages.PMEM < rootPagesPerVM*vms {
		t.Fatalf("the default PMEM logical cap is %d pages, and the deployment's %d VMs need %d",
			config.LogicalPages.PMEM, vms, rootPagesPerVM*vms)
	}
}

// A deployment may run RAM at 4 KiB on ordinary memory, and then the RAM share
// is counted in 4 KiB pages: the same bytes are 512 times as many pages as
// PMEM's. Any page but the two arenas there are is refused.
func TestConfigRunsRAMAtFourKiBWhenAsked(t *testing.T) {
	values := minimal()
	values["SPROUTFS_RAM_PAGE_BYTES"] = "4096"
	values["SPROUTFS_RAM_SHARE_PERCENT"] = "50"
	config, err := loadConfig(environ(values))
	if err != nil {
		t.Fatal(err)
	}
	if config.RAMPageSize != 4<<10 || config.DirtyPages != (host.KindPages{RAM: 1 << 18, PMEM: 512}) ||
		config.LogicalPages.RAM != (1<<18)*32 {
		t.Fatalf("a 4 KiB RAM page gave page %d, dirty budgets %v and a RAM logical cap of %d",
			config.RAMPageSize, config.DirtyPages, config.LogicalPages.RAM)
	}
	values["SPROUTFS_RAM_PAGE_BYTES"] = "8192"
	if _, err := loadConfig(environ(values)); err == nil ||
		!strings.Contains(err.Error(), "SPROUTFS_RAM_PAGE_BYTES: a RAM page of 8192 bytes: want 4096 or 2097152") {
		t.Fatalf("an 8 KiB RAM page was accepted: %v", err)
	}
}
