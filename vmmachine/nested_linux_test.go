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
	vm := newGuestVM(t, ctx, name)
	if err := vm.DiscardMemory(ctx, vmmachine.RAMVolume, volume.Shape{Nested: &nested}); err != nil {
		t.Fatal(err)
	}
	return vm
}

// virtualisationFlags is what a guest's /proc/cpuinfo offers of VMX and SVM,
// and what its kernel said about either as it booted, which is what explains a
// flag that is missing.
func virtualisationFlags(t *testing.T, ctx context.Context, binaryPath string, vm *volume.VM) (flags, boot string) {
	t.Helper()
	// A nested VM's RAM stays resident, so its RAM arena holds the whole of it
	// whatever the suite's own resident budget is.
	pager := newSizedMigrationPager(t, ctx, 128<<20, 128<<20, 384<<20, 384<<20)
	config := migrationConfig(t, binaryPath, pager, vm)
	config.Starter.(*vmmachine.Firecracker).VsockCID = guestVsockCID
	p, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitLine(t, ctx, p, fmt.Sprintf("sproutfs-guest-agent: serving on vsock port %d", guest.Port), 0)
	// The guest's root has busybox's cat and little else, so the flags are
	// read here.
	result, err := guestExec(ctx, p, guest.ExecRequest{Cmd: "cat /proc/cpuinfo"})
	if err != nil || result.Exit != 0 {
		t.Fatalf("reading the guest's /proc/cpuinfo: %v %+v\n%s", err, result, consoleText(p))
	}
	var found []string
	for _, flag := range []string{"vmx", "svm"} {
		if slices.Contains(strings.Fields(result.Stdout), flag) {
			found = append(found, flag)
		}
	}
	var said []string
	for _, line := range strings.Split(string(consoleText(p)), "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "vmx") || strings.Contains(lower, "svm") || strings.Contains(lower, "virtuali") {
			said = append(said, line)
		}
	}
	return strings.Join(found, " "), strings.Join(said, "\n")
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

// TestOnlyANestedGuestIsOfferedHardwareVirtualisation: a nested VM's guest sees
// the host's VMX or SVM, and a guest that is not nested sees neither, so it
// cannot start a VM of its own. See vmmachine's nested.go for why.
func TestOnlyANestedGuestIsOfferedHardwareVirtualisation(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker qualification script")
	}
	if runtime.GOARCH != "amd64" {
		t.Skip("only an x86_64 host runs a nested VM; TestANestedVMIsRefusedOffX86 covers the rest")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	want := hostVirtualisation(t)
	if got, boot := virtualisationFlags(t, ctx, binaryPath, newNestedGuestVM(t, ctx, "plain", false)); got != "" {
		t.Fatalf("a guest that is not nested sees %q, want neither VMX nor SVM; its kernel said:\n%s", got, boot)
	}
	if got, boot := virtualisationFlags(t, ctx, binaryPath, newNestedGuestVM(t, ctx, "nested", true)); got != want {
		t.Fatalf("a nested guest sees %q, want the host's %q (%s); its kernel said:\n%s",
			got, want, hostNested(t), boot)
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
	if got, boot := virtualisationFlags(t, ctx, binaryPath, newNestedGuestVM(t, ctx, "plain", false)); got != "" {
		t.Fatalf("a guest that is not nested sees %q, want neither VMX nor SVM; its kernel said:\n%s", got, boot)
	}
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
