//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/api/guest"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/volume"
)

// The channel a deployment reaches a guest on is the VM's vsock: the host
// connects to the socket the VMM listens on, asks for the agent's port, and runs
// a command over what comes back. Everything a host does to a running VM happens
// under commands taking that route, so these two suites are about what one of
// them sees when it does.
const (
	// guestVsockCID is this machine's own name for its vsock device. It never
	// leaves the guest, so every VM of this suite may use the same one.
	guestVsockCID = 3
	// guestExecTimeout is what the host allows one command, and is deliberately
	// far longer than anything here waits for: a failure must be the exec being
	// answered late or not at all, never this bound firing and hiding it.
	guestExecTimeout = 2 * time.Minute
	// guestCommandSeconds is how long the command these suites interrupt runs
	// for, which is long enough that what interrupts it lands in the middle
	// rather than near either end.
	guestCommandSeconds = 20
	// handoffAnswer is how long a host may still be waiting on a command in a
	// guest that has been stopped for good. It is a bound on being told, not on
	// the answer: the guest is gone and no answer is coming, so anything past
	// this is a host that will wait out its own timeout for nothing.
	handoffAnswer = 15 * time.Second
	// startedMarker lives on the guest's devtmpfs, which is writable whatever
	// its root filesystem was mounted as.
	startedMarker = "/dev/the-command-started"
)

// TestACheckpointDoesNotInterruptACommandRunningInTheGuest: a host checkpoints a
// running VM on an interval, and a checkpoint is a pause, a state capture and a
// resume of that same guest. Nothing about it invalidates the connection a
// command is running over, so the command must run to its end and be answered
// with everything it did — its output, both streams, and its exit status.
//
// The VMM used to tell the guest's vsock driver to reset its transport whenever
// a snapshot was created, which killed every connection the guest had the moment
// it resumed. The command died with the connection, and the host was told
// nothing: it sat waiting for a reply until a timeout of its own ended it,
// minutes later, having lost the command's answer entirely.
func TestACheckpointDoesNotInterruptACommandRunningInTheGuest(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	vm, p := startGuestWithAgent(t, ctx, binaryPath, "checkpointed")

	answers := execInGuest(ctx, p, guest.ExecRequest{
		Cmd: fmt.Sprintf("touch %s; sleep %d; echo finished; echo bothered >&2; exit 7",
			startedMarker, guestCommandSeconds),
		Timeout: 120,
	})
	awaitGuestMarker(t, ctx, p)

	// The checkpoint the interval takes, on the guest that is running it.
	published, err := vm.Snapshot(ctx, prepareAndResume(p))
	if err != nil {
		t.Fatalf("checkpointing %s while it ran a command: %v\n%s", vm.ID(), err, consoleText(p))
	}
	if err := published.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	answered := <-answers
	if answered.err != nil {
		t.Fatalf("the command the checkpoint ran over was not answered: %v\n%s",
			answered.err, consoleText(p))
	}
	result := answered.result
	if result.Exit != 7 {
		t.Fatalf("the command exited %d, want 7: %+v", result.Exit, result)
	}
	if result.Stdout != "finished\n" {
		t.Fatalf("stdout is %q, want \"finished\\n\"", result.Stdout)
	}
	if result.Stderr != "bothered\n" {
		t.Fatalf("stderr is %q, want \"bothered\\n\"", result.Stderr)
	}
	if result.Seconds < guestCommandSeconds {
		t.Fatalf("the command reported %.1fs, want at least %ds: it did not run to its end",
			result.Seconds, guestCommandSeconds)
	}

	// And the channel is still the channel: the guest goes on answering.
	after, err := guestExec(ctx, p, guest.ExecRequest{Cmd: "echo still here"})
	if err != nil {
		t.Fatalf("the guest stopped answering after the checkpoint: %v\n%s", err, consoleText(p))
	}
	if after.Stdout != "still here\n" {
		t.Fatalf("a command after the checkpoint printed %q", after.Stdout)
	}
}

// TestAHandoffFailsACommandRunningInTheGuest: a migration's stop is the other
// half of the same question. It pauses this guest for good — a destination
// starts from the state it captures — so the command running over the vsock is
// over, and the host must be told so at once. What it must not do is wait: the
// VMM's process goes on running to serve the destination its pages, so a host
// left holding that connection waits out its own timeout, ten minutes later,
// for a guest that stopped in the first second.
func TestAHandoffFailsACommandRunningInTheGuest(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	_, p := startGuestWithAgent(t, ctx, binaryPath, "handed-off")

	answers := execInGuest(ctx, p, guest.ExecRequest{
		Cmd: fmt.Sprintf("touch %s; sleep %d; echo finished",
			startedMarker, guestCommandSeconds),
		Timeout: 120,
	})
	awaitGuestMarker(t, ctx, p)

	// The handoff's stop phase, which is what a migration does to the source.
	began := time.Now()
	if _, err := p.Stop(ctx); err != nil {
		t.Fatalf("stopping %s while it ran a command: %v\n%s", p.Directory(), err, consoleText(p))
	}
	select {
	case answered := <-answers:
		took := time.Since(began)
		if answered.err == nil {
			t.Fatalf("the command survived the handoff that stopped its guest: %+v", answered.result)
		}
		if took > handoffAnswer {
			t.Fatalf("the exec was told %s after the handoff, want within %s: %v",
				took.Round(time.Millisecond), handoffAnswer, answered.err)
		}
		if !strings.Contains(answered.err.Error(), "/exec") {
			t.Fatalf("the failure reads %q, want it to name the request that could not be answered",
				answered.err)
		}
	case <-time.After(handoffAnswer):
		t.Fatalf("the exec was still waiting %s after the handoff stopped the guest\n%s",
			handoffAnswer, consoleText(p))
	}
}

// guestRootBytes is the size of this suite's qualification root image, which is
// therefore the size of the volume it is loaded into.
const guestRootBytes = 64 << 20

// startGuestWithAgent boots one VM of this suite's qualification image with a
// vsock, and returns once the agent inside it is serving on that vsock.
func startGuestWithAgent(t *testing.T, ctx context.Context, binaryPath, name string) (*volume.VM, *vmmachine.Process) {
	t.Helper()
	vm := newGuestVM(t, ctx, name)
	return vm, bootGuestWithAgent(t, ctx, binaryPath, vm)
}

// newGuestVM creates one VM of this suite's qualification image, loaded and
// checkpointed, with nothing running over it yet.
func newGuestVM(t *testing.T, ctx context.Context, name string) *volume.VM {
	t.Helper()
	c := newMigrationCluster(t, ctx)
	vm, err := c.source.Create(ctx, name, []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: 128 << 20},
		{Name: "root", Size: guestRootBytes},
	})
	if err != nil {
		t.Fatal(err)
	}
	loadRootImage(t, ctx, vm.Volume("root"))
	if err := vm.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	return vm
}

// bootGuestWithAgent boots one prepared VM cold and returns once the agent
// inside it is serving on its vsock.
func bootGuestWithAgent(t *testing.T, ctx context.Context, binaryPath string, vm *volume.VM) *vmmachine.Process {
	t.Helper()
	pager, _ := newMigrationPager(t, ctx)
	config := migrationConfig(t, binaryPath, pager, vm)
	config.VsockCID = guestVsockCID
	p, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
	// The agent's own first line on the console, so nothing here polls for a
	// guest that is still starting.
	waitLine(t, ctx, p, fmt.Sprintf("sproutfs-guest-agent: serving on vsock port %d", guest.Port), 0)
	return p
}

// guestExec runs one command in a guest over its vsock: the socket the VMM
// listens on, the connection forwarding request, and the agent's own API. It is
// the path internal/host takes, built from the same client.
func guestExec(ctx context.Context, p *vmmachine.Process, request guest.ExecRequest) (guest.ExecResult, error) {
	client := guest.NewClient(p.VsockPath(), guestExecTimeout)
	return jsonhttp.Call[guest.ExecResult](ctx, client, http.MethodPost, guest.URL("/exec"), request)
}

// execution is one exec's answer, whichever kind it turned out to be.
type execution struct {
	result guest.ExecResult
	err    error
}

// execInGuest starts one command and reports what the host is eventually told
// about it, so that the caller can do something to the VM while it runs.
func execInGuest(ctx context.Context, p *vmmachine.Process, request guest.ExecRequest) <-chan execution {
	answers := make(chan execution, 1)
	go func() {
		result, err := guestExec(ctx, p, request)
		answers <- execution{result: result, err: err}
	}()
	return answers
}

// awaitGuestMarker returns once the command started above is certainly running,
// so that what happens next happens in the middle of it. The marker is asked for
// over a second connection of its own, which is also what says the guest admits
// more than the one command.
func awaitGuestMarker(t *testing.T, ctx context.Context, p *vmmachine.Process) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		result, err := guestExec(ctx, p, guest.ExecRequest{
			Cmd: fmt.Sprintf("test -e %s && echo running", startedMarker), Timeout: 10})
		if err != nil {
			t.Fatalf("asking the guest whether its command had started: %v\n%s", err, consoleText(p))
		}
		if strings.Contains(result.Stdout, "running") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the command never started in the guest: %+v\n%s", result, consoleText(p))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
