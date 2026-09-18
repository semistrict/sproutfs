package host_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/volume"
)

// gatedMachine is a VMM process whose migration pause can be held open, which is
// what lets a test have a second caller arrive while the first handover is
// halfway through it.
type gatedMachine struct {
	*machine
	entered chan struct{}
	release chan struct{}
}

func newGatedMachine(m *machine) *gatedMachine {
	return &gatedMachine{machine: m, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *gatedMachine) Stop(ctx context.Context) ([]byte, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	return g.machine.Stop(ctx)
}

// TestOneHandoverOfAVMAtATime: a migration stops the guest, gives every region's
// volume up and registers the pages with the page server. Two callers that
// found the same registration each did all of that to one VMM process: the
// second stopped a guest the first had already handed over, failed, and gave the
// VM up — closing the process whose pages the winner's destination was about to
// fault out of. A host admits one handover of a VM at a time and tells the
// second caller so, leaving the first alone.
func TestOneHandoverOfAVMAtATime(t *testing.T) {
	h, pagers, arenas := startMigrationHosts(t)
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], arenas[1], &received)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "contended", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	source, err := newMachine(t, pagers[0], arenas[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	source.store("ram0", 0, 5)
	gated := newGatedMachine(source)
	if err := h.hosts[0].AddMachine("contended", gated); err != nil {
		t.Fatal(err)
	}

	first := make(chan error, 1)
	go func() {
		_, err := h.hosts[0].Migrate(context.WithoutCancel(t.Context()), "contended", h.pages[1])
		first <- err
	}()
	select {
	case <-gated.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first migration never reached the pause")
	}
	// The second caller is answered without waiting for the first to finish: it
	// is refused, rather than let into a pause of a guest the first has already
	// handed over.
	second := make(chan error, 1)
	go func() {
		_, err := h.hosts[0].Migrate(context.WithoutCancel(t.Context()), "contended", h.pages[2])
		second <- err
	}()
	var secondErr error
	answered := false
	select {
	case secondErr = <-second:
		answered = true
	case <-time.After(10 * time.Second):
	}
	close(gated.release)
	if !answered {
		t.Fatal("the second handover was not refused: it is inside the pause of a guest the first has handed over")
	}
	if !errors.Is(secondErr, host.ErrNotMigratable) {
		t.Fatalf("a second handover of one VM = %v, want ErrNotMigratable", secondErr)
	}
	if err := <-first; err != nil {
		t.Fatalf("the handover that got there first: %v", err)
	}
	if source.closed.Load() {
		t.Fatal("the second caller closed the process the winner's destination faults out of")
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 1 || serving[0] != "contended" {
		t.Fatalf("the source serves %v after the handover, want the VM it handed over", serving)
	}
}

// TestMigratingAForkBeforeItsRootIsPublishedIsRefused: a fork reads its parent's
// sealed pages until it publishes a root index of its own, and only that
// publication retires the point. Handing such a child to another host releases
// the one handle that could ever publish it, and does not retire the point: the
// parent stays sealed for good — never checkpointed, never fenced, never
// migratable — and the child is an identity nothing can open anywhere. The
// handover is refused before the guest is stopped for it.
func TestMigratingAForkBeforeItsRootIsPublishedIsRefused(t *testing.T) {
	h, pagers, arenas := startMigrationHosts(t)
	h.start(t)

	parent, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], arenas[0], parent, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 3)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	point, err := host.Seal(t.Context(), parent, guest)
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := host.CreateFork(t.Context(), h.hosts[0].Volumes(), "child", point)
	if err != nil {
		t.Fatal(err)
	}
	childGuest, err := newMachine(t, pagers[0], arenas[0], child, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("child", childGuest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Migrate(t.Context(), "child", h.pages[1]); !errors.Is(err, volume.ErrForkPending) {
		t.Fatalf("migrating a fork with no root of its own = %v, want ErrForkPending", err)
	}
	// The child is untouched: it still runs here and still reads the point.
	if childGuest.closed.Load() {
		t.Fatal("the refused handover stopped the child's guest")
	}
	if status := child.Status(); status.Err != nil || !status.Root {
		t.Fatalf("the refused handover left the child at %+v", status)
	}
	// And so is the parent, which the child publishing its root releases.
	if err := child.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := point.Retire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if status := parent.Status(); status.Sealed {
		t.Fatalf("the parent is still sealed after its child published: %+v", status)
	}
}
