//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/guest"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// These suites run a VM inside a nested VM's guest: L0 is this host's KVM, L1
// the nested VM's guest and L2 the VM that guest runs, through `witness kvm`
// (cmd/sproutfs-guest-witness/l2.go). They are TASK-57's proof that a nested
// VM can be what any other VM is: L2 runs; L1 is not offered the VMX controls
// with which it could have L0 write its memory behind the host page tables;
// and an L2 left running goes on running across a capture and restore, a
// fork and a live migration of L1, with L1's memory intact. See vmmachine's
// nested.go for why that holds.

// l2Log is where the resident witness running the counting L2 says why it
// stopped: the guest's devtmpfs, which is guest memory.
const l2Log = "/dev/sproutfs-l2.log"

// TestANestedGuestRunsAVMOfItsOwn: a nested VM's guest runs an L2 to its halt,
// and the word L2 stored is in the memory L1 gave it.
func TestANestedGuestRunsAVMOfItsOwn(t *testing.T) {
	binaryPath, kernel := nestedFirecracker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	vm := newNestedGuestVM(t, ctx, "nested", true)
	p := bootNestedGuest(t, ctx, nestedConfig(t, binaryPath, kernel, nestedPager(t, ctx), vm))
	result, err := guestExec(ctx, p, guest.ExecRequest{Cmd: guestWitness + " kvm run", Timeout: 120})
	if err != nil {
		t.Fatalf("running `witness kvm run` in the guest: %v\n%s", err, consoleText(p))
	}
	const want = "exit 0: L2 ran to its stop and stored 0x4b4f324c at 0x2000"
	if got := fmt.Sprintf("exit %d: %s", result.Exit, strings.TrimSpace(result.Stdout+result.Stderr)); got != want {
		t.Fatalf("a nested guest runs `witness kvm run` to %q, want %q (%s)\n%s",
			got, want, hostNested(t), consoleText(p))
	}
}

// TestANestedGuestIsNotOfferedTheControlsThatPinItsMemory: L1 is offered
// neither TPR shadow, nor virtualize APIC accesses, nor posted interrupts.
// With any of them L1 names a page of its own in its VMCS that L0 maps for as
// long as L2 runs and writes behind the host page tables, which a seal, an
// eviction or a move of that page would miss. That is what keeps a nested
// VM's RAM fixed today, and the Firecracker fork takes the three out of the
// VMX capabilities it gives a nested VM's vCPUs (TASK-57).
func TestANestedGuestIsNotOfferedTheControlsThatPinItsMemory(t *testing.T) {
	binaryPath, kernel := nestedFirecracker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	vm := newNestedGuestVM(t, ctx, "nested", true)
	p := bootNestedGuest(t, ctx, nestedConfig(t, binaryPath, kernel, nestedPager(t, ctx), vm))
	result, err := guestExec(ctx, p, guest.ExecRequest{Cmd: guestWitness + " kvm controls", Timeout: 120})
	if err != nil {
		t.Fatalf("running `witness kvm controls` in the guest: %v\n%s", err, consoleText(p))
	}
	const want = "exit 0: L1 is offered TPR shadow: no, virtualize APIC accesses: no, posted interrupts: no"
	if got := fmt.Sprintf("exit %d: %s", result.Exit, strings.TrimSpace(result.Stdout)); got != want {
		t.Fatalf("a nested guest runs `witness kvm controls` to %q, want %q\n"+
			"its KVM's VMX capability MSRs:\n%s\nits kernel's %s",
			got, want, result.Stderr, vmxFlags(t, ctx, p))
	}
}

// TestANestedGuestKeepsItsVMAcrossACaptureAndRestore: a nested VM whose guest
// is running an L2 is captured while it runs, goes on running, and is closed.
// A VM restored from the capture resumes the guest with the memory it had at
// the pause, and its L2 goes on counting from where the capture left it.
func TestANestedGuestKeepsItsVMAcrossACaptureAndRestore(t *testing.T) {
	binaryPath, kernel := nestedFirecracker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	c := newMigrationCluster(t, ctx)
	vm := newNestedGuestVMIn(t, ctx, c, "nested-captured", true)
	pager := nestedPager(t, ctx)
	p := bootNestedGuest(t, ctx, nestedConfig(t, binaryPath, kernel, pager, vm))
	startL2(t, ctx, p)
	writeMarker(t, ctx, p, "captured")
	before := awaitL2Past(t, ctx, p, 0)

	ckpt, err := vm.Snapshot(ctx, prepareAndResume(p), volume.Terms{})
	if err != nil {
		t.Fatalf("capturing the nested VM %s while its guest runs an L2: %v\n%s", vm.ID(), err, consoleText(p))
	}
	if err := ckpt.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// The captured guest runs on, and its L2 with it.
	awaitL2Past(t, ctx, p, before)
	writeMarker(t, ctx, p, "after the capture")
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	point, err := c.source.Inherit(ctx, ckpt.Ref())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := c.source.Fork(ctx, "nested-restored", point)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close(context.WithoutCancel(ctx)) })
	if !restored.Nested() {
		t.Fatal("the VM restored from a capture of a nested VM is not nested")
	}
	if err := restored.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	config := nestedConfig(t, binaryPath, kernel, pager, restored)
	config.RestoreState = ckpt.State()
	rp, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatalf("restoring the capture of a nested VM: %v", err)
	}
	t.Cleanup(func() { _ = rp.Close() })
	if err := rp.Release(ctx); err != nil {
		t.Fatalf("resuming the restored nested VM: %v\n%s", err, consoleText(rp))
	}
	if got := readMarker(t, ctx, rp); got != "captured\n" {
		t.Fatalf("the restored guest reads %q, want the capture's %q\n%s", got, "captured\n", consoleText(rp))
	}
	resumed := awaitL2Past(t, ctx, rp, before)
	awaitL2Past(t, ctx, rp, resumed)
}

// TestANestedGuestKeepsItsVMAcrossAFork: a nested VM whose guest is running an
// L2 is forked while it runs, and the child is taken in on another host as a
// deployment takes one. Both guests go on with their L2s: the child's resumes
// from the fork point, with the memory its parent had there, and the parent's
// is its own.
func TestANestedGuestKeepsItsVMAcrossAFork(t *testing.T) {
	binaryPath, kernel := nestedFirecracker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	c := newMigrationCluster(t, ctx)
	vm := newNestedGuestVMIn(t, ctx, c, "nested-parent", true)
	p := bootNestedGuest(t, ctx, nestedConfig(t, binaryPath, kernel, nestedPager(t, ctx), vm))
	startL2(t, ctx, p)
	writeMarker(t, ctx, p, "forked")
	before := awaitL2Past(t, ctx, p, 0)

	pages, err := peer.NewServer(ctx, peer.ServerConfig{Network: c.network,
		Address: "source-pages", PageSize: pagerPageBytes(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pages.Close() })
	point, err := vm.ForkPoint(ctx, prepareAndResume(p))
	if err != nil {
		t.Fatalf("sealing the point the nested VM %s is forked at: %v\n%s", vm.ID(), err, consoleText(p))
	}
	// The fork's own hold keeps the point while the child is described, and
	// the child's is the second, as forkPointFixture takes them.
	if err := point.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := point.Pin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := point.Hold(); err != nil {
		t.Fatal(err)
	}
	handoff, err := vmmigrate.Fork(ctx, "nested-child", point, pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := point.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	// The parent runs on from the fork point, and its L2 with it.
	awaitL2Past(t, ctx, p, before)
	writeMarker(t, ctx, p, "the parent after the fork")

	childPager := nestedPager(t, ctx)
	child := receiveChild(t, ctx, c, handoff, pages, func(vm *volume.VM) vmmachine.Config {
		return nestedConfig(t, binaryPath, kernel, childPager, vm)
	})
	if !child.vm.Nested() {
		t.Fatal("the child of a nested VM is not nested")
	}
	if got := readMarker(t, ctx, child.process); got != "forked\n" {
		t.Fatalf("the child's guest reads %q, want the fork point's %q\n%s", got, "forked\n", consoleText(child.process))
	}
	resumed := awaitL2Past(t, ctx, child.process, before)
	awaitL2Past(t, ctx, child.process, resumed)
	// And the parent is still itself, with an L2 still counting.
	if got := readMarker(t, ctx, p); got != "the parent after the fork\n" {
		t.Fatalf("the parent's guest reads %q after the fork, want its own %q", got, "the parent after the fork\n")
	}
	awaitL2Past(t, ctx, p, l2Count(t, ctx, p))
}

// TestANestedGuestKeepsItsVMAcrossALiveMigration: a nested VM whose guest is
// running an L2 is migrated to another host while it runs, as
// TestFirecrackerLiveMigration migrates a guest. The destination resumes the
// guest with its memory and its L2, and the L2 goes on counting once every
// page the source held has been fetched and the source has let the VM go.
func TestANestedGuestKeepsItsVMAcrossALiveMigration(t *testing.T) {
	binaryPath, kernel := nestedFirecracker(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	c := newMigrationCluster(t, ctx)
	source := newNestedGuestVMIn(t, ctx, c, "nested-migrant", true)
	p := bootNestedGuest(t, ctx, nestedConfig(t, binaryPath, kernel, nestedPager(t, ctx), source))
	startL2(t, ctx, p)
	writeMarker(t, ctx, p, "migrated")
	before := awaitL2Past(t, ctx, p, 0)

	pages, err := peer.NewServer(ctx, peer.ServerConfig{Network: c.network,
		Address: "source-pages", PageSize: pagerPageBytes(t),
		MaxConnectionsPerPeer: 32, MaxBytesInFlightPerPeer: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pages.Close() })
	handoff, err := vmmigrate.Migrate(ctx, source, p, pages, vmmigrate.Options{})
	if err != nil {
		t.Fatalf("migrating the nested VM %s while its guest runs an L2: %v\n%s", source.ID(), err, consoleText(p))
	}

	destination, err := c.destination.Open(ctx, "nested-migrant")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destination.Close(context.WithoutCancel(ctx)) })
	if !destination.Nested() {
		t.Fatal("the migrated nested VM is not nested on its destination")
	}
	peers := make(map[string]*vmmigrate.PeerBacking, len(handoff.MemoryRegions))
	config := nestedConfig(t, binaryPath, kernel, nestedPager(t, ctx), destination)
	config.RestoreState = handoff.State
	config.Backings = make(map[string]vmmemory.Backing, len(handoff.MemoryRegions))
	for _, memoryRegion := range handoff.MemoryRegions {
		v := destination.Volume(memoryRegion.Name)
		if v == nil {
			t.Fatalf("the destination opened no volume named %s", memoryRegion.Name)
		}
		peers[memoryRegion.Name] = peerBacking(t, c, handoff, v)
		config.Backings[memoryRegion.Name] = peers[memoryRegion.Name]
	}
	fp, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatalf("restoring the migrated nested VM: %v", err)
	}
	t.Cleanup(func() { _ = fp.Close() })
	if err := fp.Release(ctx); err != nil {
		t.Fatalf("resuming the migrated nested VM: %v\n%s", err, consoleText(fp))
	}
	if got := readMarker(t, ctx, fp); got != "migrated\n" {
		t.Fatalf("the migrated guest reads %q, want its %q\n%s", got, "migrated\n", consoleText(fp))
	}
	resumed := awaitL2Past(t, ctx, fp, before)

	// The post-copy's own pass: every page only the source held is fetched,
	// and the source lets the VM go. The L2 runs on what this host holds.
	for _, memoryRegion := range handoff.MemoryRegions {
		mapped := fp.MemoryRegions()[memoryRegion.Name]
		if mapped == nil {
			t.Fatalf("the destination maps no memory region for %s", memoryRegion.Name)
		}
		for _, run := range memoryRegion.Unpublished {
			for page := run.First; page < run.First+uint64(run.Count); page++ {
				if err := mapped.Fault(ctx, page, false); err != nil {
					t.Fatalf("fetching %s page %d, which no checkpoint holds: %v", memoryRegion.Name, page, err)
				}
			}
		}
		if left := peers[memoryRegion.Name].Unfetched(); left != 0 {
			t.Fatalf("%s still owes the source %d pages", memoryRegion.Name, left)
		}
	}
	if err := pages.Release(handoff.VMID); err != nil {
		t.Fatalf("the source refused to release a VM it had served every page of: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	awaitL2Past(t, ctx, fp, resumed)
	if got := readMarker(t, ctx, fp); got != "migrated\n" {
		t.Fatalf("the migrated guest reads %q once the source is gone, want %q", got, "migrated\n")
	}
}

// startL2 leaves an L2 counting in the guest, and returns once it has begun.
func startL2(t *testing.T, ctx context.Context, p *vmmachine.Process) {
	t.Helper()
	result := runInGuest(t, ctx, p, guestWitness+" kvm loop")
	if want := "L2 is counting at 0x2000 of /dev/sproutfs-l2\n"; result.Stdout != want {
		t.Fatalf("`witness kvm loop` said %q, want %q", result.Stdout, want)
	}
}

// l2Count is what the guest's counting L2 has counted to.
func l2Count(t *testing.T, ctx context.Context, p *vmmachine.Process) uint64 {
	t.Helper()
	result := runInGuest(t, ctx, p, guestWitness+" kvm count")
	var count uint64
	if _, err := fmt.Sscanf(result.Stdout, "L2 has counted to %d\n", &count); err != nil {
		t.Fatalf("`witness kvm count` said %q: %v", result.Stdout, err)
	}
	return count
}

// l2Wait bounds how long a count may stay put before the L2 is taken for
// stopped. A running L2 counts thousands of times a second; this separates
// one that has stopped from one whose guest was slow to answer.
const l2Wait = time.Minute

// awaitL2Past returns the guest's L2 count once it is past since, which says
// the L2 is running. A count below since is an L2 that lost what it had
// counted: it counts in a register and only copies the count to memory, so it
// is an L2 whose registers came back older than its memory.
func awaitL2Past(t *testing.T, ctx context.Context, p *vmmachine.Process, since uint64) uint64 {
	t.Helper()
	deadline := time.Now().Add(l2Wait)
	for {
		count := l2Count(t, ctx, p)
		if count > since {
			return count
		}
		if count < since {
			t.Fatalf("the L2 counted back from %d to %d\nits log: %s\n%s", since, count, l2Said(ctx, p), consoleText(p))
		}
		if time.Now().After(deadline) {
			t.Fatalf("the L2 stayed at %d for %s\nits log: %s\n%s", count, l2Wait, l2Said(ctx, p), consoleText(p))
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for the L2 to count past %d: %v", since, context.Cause(ctx))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// l2Said is what the resident witness running the L2 logged, for a failure.
func l2Said(ctx context.Context, p *vmmachine.Process) string {
	result, err := guestExec(ctx, p, guest.ExecRequest{Cmd: "cat " + l2Log, Timeout: 30})
	if err != nil {
		return err.Error()
	}
	return result.Stdout + result.Stderr
}

// vmxFlags is the guest kernel's "vmx flags" line of /proc/cpuinfo: the VMX
// controls it read from the capability MSRs L0 offers it, named, which says
// what `witness kvm controls` reads through KVM a second way.
func vmxFlags(t *testing.T, ctx context.Context, p *vmmachine.Process) string {
	t.Helper()
	cpuinfo := runInGuest(t, ctx, p, "cat /proc/cpuinfo").Stdout
	for _, line := range strings.Split(cpuinfo, "\n") {
		if strings.HasPrefix(line, "vmx flags") {
			return line
		}
	}
	return "/proc/cpuinfo has no vmx flags line"
}
