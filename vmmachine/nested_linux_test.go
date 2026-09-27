//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/guest"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/volume"
)

// newNestedGuestVM is newGuestVM made a nested VM by the cold boot's
// publication, as a create that asks for one makes it.
func newNestedGuestVM(t *testing.T, ctx context.Context, name string, nested bool) *volume.VM {
	t.Helper()
	return newNestedGuestVMIn(t, ctx, newMigrationCluster(t, ctx), name, nested)
}

// newNestedGuestVMIn is newNestedGuestVM on the source host of a cluster the
// caller holds.
func newNestedGuestVMIn(t *testing.T, ctx context.Context, c *migrationCluster, name string, nested bool) *volume.VM {
	t.Helper()
	vm := newGuestVMIn(t, ctx, c, name)
	if err := vm.DiscardMemory(ctx, vmmachine.RAMVolume, volume.Shape{Nested: &nested}); err != nil {
		t.Fatal(err)
	}
	return vm
}

// nestedVCPUs is how many processors these suites give a guest: two, so that
// one busy running an L2 of its own leaves the other to the guest's agent.
const nestedVCPUs = 2

// nestedPager is one host's pagers for these suites. A nested VM's RAM stays
// resident, so its RAM arena holds the whole of it whatever the suite's own
// resident budget is.
func nestedPager(t *testing.T, ctx context.Context) *hostPagers {
	t.Helper()
	return newSizedMigrationPager(t, ctx, 128<<20, 128<<20, 384<<20, 384<<20)
}

// nestedConfig is migrationConfig for a guest of these suites: booted on
// kernel, with its vsock, on nestedVCPUs processors.
func nestedConfig(t *testing.T, binaryPath, kernel string, pager *hostPagers, vm *volume.VM) vmmachine.Config {
	t.Helper()
	config := migrationConfig(t, binaryPath, pager, vm)
	starter := config.Starter.(*vmmachine.Firecracker)
	starter.VsockCID = guestVsockCID
	starter.Kernel = kernel
	config.VCPUs = nestedVCPUs
	return config
}

// bootNestedGuest starts config's VM and returns once its guest's agent is
// serving on the vsock.
func bootNestedGuest(t *testing.T, ctx context.Context, config vmmachine.Config) *vmmachine.Process {
	t.Helper()
	p, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitLine(t, ctx, p, fmt.Sprintf("sproutfs-guest-agent: serving on vsock port %d", guest.Port), 0)
	return p
}

// virtualisation is what a guest shows of hardware virtualisation.
type virtualisation struct {
	// flags is what its /proc/cpuinfo offers of VMX and SVM.
	flags string
	// kvm is how `witness kvm` exits and what it says: whether the guest can
	// open /dev/kvm and create a VM there, which is what a flag is for.
	kvm string
	// boot is what its kernel said of either as it booted, which is what
	// explains a flag or a device that is missing.
	boot string
}

// virtualisationOf boots vm on kernel and reports what its guest shows of
// hardware virtualisation.
func virtualisationOf(t *testing.T, ctx context.Context, binaryPath, kernel string, vm *volume.VM) virtualisation {
	t.Helper()
	p := bootNestedGuest(t, ctx, nestedConfig(t, binaryPath, kernel, nestedPager(t, ctx), vm))
	// The guest's root has busybox's cat and little else, so the flags are
	// read here.
	cpuinfo, err := guestExec(ctx, p, guest.ExecRequest{Cmd: "cat /proc/cpuinfo"})
	if err != nil || cpuinfo.Exit != 0 {
		t.Fatalf("reading the guest's /proc/cpuinfo: %v %+v\n%s", err, cpuinfo, consoleText(p))
	}
	var found []string
	for _, flag := range []string{"vmx", "svm"} {
		if slices.Contains(strings.Fields(cpuinfo.Stdout), flag) {
			found = append(found, flag)
		}
	}
	kvm, err := guestExec(ctx, p, guest.ExecRequest{Cmd: guestWitness + " kvm"})
	if err != nil {
		t.Fatalf("running `witness kvm` in the guest: %v\n%s", err, consoleText(p))
	}
	var said []string
	for _, line := range strings.Split(string(consoleText(p)), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "vmx") || strings.Contains(lower, "svm") ||
			strings.Contains(lower, "virtuali") || strings.Contains(lower, "kvm") {
			said = append(said, line)
		}
	}
	return virtualisation{
		flags: strings.Join(found, " "),
		kvm:   fmt.Sprintf("exit %d: %s", kvm.Exit, strings.TrimSpace(kvm.Stdout+kvm.Stderr)),
		boot:  strings.Join(said, "\n"),
	}
}

// requireNone fails the test unless a guest that is not nested shows no
// hardware virtualisation at all: neither flag, and no /dev/kvm to open.
func requireNone(t *testing.T, got virtualisation) {
	t.Helper()
	if got.flags != "" {
		t.Fatalf("a guest that is not nested sees %q, want neither VMX nor SVM; its kernel said:\n%s", got.flags, got.boot)
	}
	const none = "exit 1: sproutfs-guest-witness: open /dev/kvm: no such file or directory"
	if got.kvm != none {
		t.Fatalf("a guest that is not nested runs `witness kvm` to %q, want %q; its kernel said:\n%s", got.kvm, none, got.boot)
	}
}

// hostNested is what this host's KVM says of nested virtualisation.
func hostNested(t *testing.T) string {
	t.Helper()
	var said []string
	for _, module := range []string{"kvm_intel", "kvm_amd"} {
		value, err := os.ReadFile("/sys/module/" + module + "/parameters/nested")
		if err == nil {
			said = append(said, module+".nested="+strings.TrimSpace(string(value)))
		}
	}
	return strings.Join(said, " ")
}

// hostVirtualisation is the one of VMX and SVM this host's processors offer.
func hostVirtualisation(t *testing.T) string {
	t.Helper()
	cpuinfo, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range strings.Fields(string(cpuinfo)) {
		if field == "vmx" || field == "svm" {
			return field
		}
	}
	t.Fatal("this host's processors offer neither VMX nor SVM, so no guest of it can run a VM")
	return ""
}

// nestedFirecracker is the VMM and the guest kernel a suite that boots a
// nested VM's guest runs, or skips it where there are none. Every such suite
// has NestedGuest in its name: that is how the GCE qualification
// (scripts/lib/bench-memory-linux.sh) knows a run needs the kernel built.
// TestOnlyANestedGuestIsOfferedHardwareVirtualisation says why the guest needs
// a kernel of its own.
func nestedFirecracker(t *testing.T) (binaryPath, kernel string) {
	t.Helper()
	binaryPath = os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker qualification script")
	}
	if runtime.GOARCH != "amd64" {
		t.Skip("only an x86_64 host runs a nested VM; TestANestedVMIsRefusedOffX86 covers the rest")
	}
	kernel = os.Getenv("SPROUTFS_FIRECRACKER_NESTED_KERNEL")
	if kernel == "" {
		t.Skip("SPROUTFS_FIRECRACKER_NESTED_KERNEL names no guest kernel with KVM built in; " +
			"the GCE qualification (scripts/bench-memory-gce.sh, SPROUTFS_GCE_QUALIFY=1) builds one")
	}
	return binaryPath, kernel
}

// TestOnlyANestedGuestIsOfferedHardwareVirtualisation: a nested VM's guest sees
// the host's VMX or SVM and can create a VM of its own, and a guest that is not
// nested sees neither and has no /dev/kvm, so it cannot start one. See
// vmmachine's nested.go for why.
//
// Both guests boot SPROUTFS_FIRECRACKER_NESTED_KERNEL: the CI kernel's
// configuration with KVM built in, which the GCE qualification builds. The CI
// kernel itself cannot answer: built without KVM, it never enables VMX in
// IA32_FEATURE_CONTROL and clears the vmx flag it was offered (Linux's
// arch/x86/kernel/cpu/feat_ctl.c), so its guest shows no VMX whether it was
// offered or not. Only a kernel that would take VMX if it were offered says,
// by not taking it, that it was not.
func TestOnlyANestedGuestIsOfferedHardwareVirtualisation(t *testing.T) {
	binaryPath, kernel := nestedFirecracker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	want := hostVirtualisation(t)
	requireNone(t, virtualisationOf(t, ctx, binaryPath, kernel, newNestedGuestVM(t, ctx, "plain", false)))
	got := virtualisationOf(t, ctx, binaryPath, kernel, newNestedGuestVM(t, ctx, "nested", true))
	if got.flags != want {
		t.Fatalf("a nested guest sees %q, want the host's %q (%s); its kernel said:\n%s",
			got.flags, want, hostNested(t), got.boot)
	}
	const created = "exit 0: created a VM on KVM API version 12"
	if got.kvm != created {
		t.Fatalf("a nested guest runs `witness kvm` to %q, want %q (%s); its kernel said:\n%s",
			got.kvm, created, hostNested(t), got.boot)
	}
}

// TestANestedVMIsRefusedOffX86: only x86_64 offers a guest hardware
// virtualisation here, so a nested VM is refused anywhere else before its VMM
// starts.
func TestANestedVMIsRefusedOffX86(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker qualification script")
	}
	if runtime.GOARCH == "amd64" {
		t.Skip("an x86_64 host runs a nested VM; TestOnlyANestedGuestIsOfferedHardwareVirtualisation covers it")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	requireNone(t, virtualisationOf(t, ctx, binaryPath, os.Getenv("SPROUTFS_FIRECRACKER_KERNEL"),
		newNestedGuestVM(t, ctx, "plain", false)))
	vm := newNestedGuestVM(t, ctx, "nested", true)
	config := migrationConfig(t, binaryPath, newMigrationPager(t, ctx), vm)
	p, err := vmmachine.Start(ctx, config)
	if err == nil {
		_ = p.Close()
		t.Fatal("a nested VM started off x86_64")
	}
	if !strings.Contains(err.Error(), "only x86_64 hosts run") {
		t.Fatalf("starting a nested VM off x86_64 = %v, want the refusal that says only x86_64 runs one", err)
	}
}
