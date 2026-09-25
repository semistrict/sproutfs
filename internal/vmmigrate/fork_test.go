package vmmigrate_test

import (
	"bytes"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

// forked seals a running parent and hands the child to the other host, which is
// what the orchestrator drives: the parent pauses for its state capture and the
// seal and is running again when this returns.
func (m *migration) forked(t *testing.T, child string) (*volume.ForkPoint, vmmigrate.Handoff) {
	t.Helper()
	point, err := host.Seal(t.Context(), m.vm, m.machine)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := vmmigrate.Fork(t.Context(), child, point, m.pages, vmmigrate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return point, handoff
}

// A fork onto another host pulls exactly the pages no checkpoint of the parent
// holds and nothing else: everything before the parent's last checkpoint is in
// object storage, where the child reads it from. The child's own first
// checkpoint publishes what it pulled, and it reads back byte for byte.
func TestForkAcrossHostsPullsExactlyTheUnpublishedPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		// The parent's own checkpoint publishes what its pager holds; everything from
		// here is the parent's alone until one side publishes it.
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(3) {
			m.machine.write("ram0", page)
			m.machine.write("disk", page)
		}
		at := m.machine.snapshot()

		point, handoff := m.forked(t, "vm-2")
		if !handoff.IsFork() || handoff.VMID != "vm-2" || handoff.Parent != "vm-1" {
			t.Fatalf("handoff: %+v", handoff)
		}
		if handoff.ParentCheckpoint != point.Parent().Sequence {
			t.Fatalf("the handoff inherits %d, want the pinned %d",
				handoff.ParentCheckpoint, point.Parent().Sequence)
		}
		unpublished := 0
		for _, memoryRegion := range handoff.MemoryRegions {
			for _, run := range memoryRegion.Unpublished {
				unpublished += run.Count
			}
		}
		if unpublished != 6 {
			t.Fatalf("the handoff named %d unpublished pages, want the 6 written since the parent's checkpoint", unpublished)
		}
		// The parent keeps its VM and its pages: nothing was published to take
		// the fork, and nothing may seal it again until the child has the pages.
		if status := m.vm.Status(); !status.Sealed || status.HandedOff {
			t.Fatalf("the parent gave something up to be forked: %+v", status)
		}

		received, child := m.receive(t, handoff)
		if err := received.Done(t.Context()); err != nil {
			t.Fatal(err)
		}
		stats := received.Stats()
		if stats.Unpublished != 6 || stats.Fetched != 6 || stats.PeerPages != 6 {
			t.Fatalf("the child fetched %+v, want exactly the 6 unpublished pages and nothing else", stats)
		}
		child.adopt(at)
		if err := child.verify(t.Context(), at); err != nil {
			t.Fatalf("the child did not start as its parent's point: %v", err)
		}

		// The child's first checkpoint is its own root index, and it publishes
		// the pages it inherited as its own.
		if err := child.checkpoint(t.Context(), received.VM()); err != nil {
			t.Fatal(err)
		}
		root := control.Ref{VM: "vm-2", Sequence: control.Sequence(received.VM().Epoch(), 1)}
		if got := received.VM().Status().Checkpoint; got != root {
			t.Fatalf("the child's first checkpoint is %v, want its root %v", got, root)
		}

		// The parent takes its pages back, which is what the release means, and
		// is checkpointed again from there.
		if err := m.pages.Release("vm-2"); err != nil {
			t.Fatalf("releasing a child that published its own root: %v", err)
		}
		if err := point.Retire(t.Context()); err != nil {
			t.Fatal(err)
		}
		if status := m.vm.Status(); status.Sealed {
			t.Fatalf("the parent kept its seal after the child published: %+v", status)
		}
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatalf("the parent could not checkpoint after the fork: %v", err)
		}
	})
}

// Neither side of a fork can see the other's writes. The parent goes on storing
// into copies of the pages the child inherited, and the child stores into its
// own.
func TestForkAcrossHostsDivergesFromItsParent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(2) {
			m.machine.write("ram0", page)
			m.machine.write("disk", page)
		}
		at := m.machine.snapshot()

		point, handoff := m.forked(t, "vm-2")
		received, child := m.receive(t, handoff)
		if err := received.Done(t.Context()); err != nil {
			t.Fatal(err)
		}
		child.adopt(at)

		// The parent moves on. The child keeps the bytes of the point.
		m.machine.write("ram0", 0)
		inherited := bytes.Clone(at["ram0"][:pageSize])
		got, err := child.read(t.Context(), "ram0", 0)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, inherited) {
			t.Fatalf("the child read the parent's later store: %d..., want %d...", got[0], inherited[0])
		}
		// And the child's own stores stay the child's.
		child.write("disk", 0)
		childDisk, err := child.read(t.Context(), "disk", 0)
		if err != nil {
			t.Fatal(err)
		}
		parentDisk, err := m.machine.read(t.Context(), "disk", 0)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(childDisk, parentDisk) {
			t.Fatalf("the parent read the child's store: both hold %d...", childDisk[0])
		}

		// Both publish their own, and neither publication changes the other.
		if err := child.checkpoint(t.Context(), received.VM()); err != nil {
			t.Fatal(err)
		}
		if err := m.pages.Release("vm-2"); err != nil {
			t.Fatalf("releasing a child that published its own root: %v", err)
		}
		if err := point.Retire(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		if err := child.verify(t.Context(), child.snapshot()); err != nil {
			t.Fatal(err)
		}
		if err := m.machine.verify(t.Context(), m.machine.snapshot()); err != nil {
			t.Fatal(err)
		}
	})
}

// Losing the parent's host after a cross-host fork costs the child nothing once
// it has published: its root index and the objects the pin protects are the
// whole of what it reads, and no page of it is only on the parent any more.
func TestForkSurvivesTheParentHostOnceItHasPublished(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := newMigration(t)
		if err := m.machine.checkpoint(t.Context(), m.vm); err != nil {
			t.Fatal(err)
		}
		for page := range uint64(3) {
			m.machine.write("ram0", page)
			m.machine.write("disk", page)
		}
		at := m.machine.snapshot()

		_, handoff := m.forked(t, "vm-2")
		received, child := m.receive(t, handoff)
		if err := received.Done(t.Context()); err != nil {
			t.Fatal(err)
		}
		child.adopt(at)

		// Before the child publishes, nothing else can open it: the pages it
		// holds are nowhere in object storage.
		elsewhere := m.cluster.manager(t, "recovery")
		if _, err := elsewhere.Open(t.Context(), "vm-2"); !errors.Is(err, volume.ErrForkPending) {
			t.Fatalf("opening the child before it published = %v, want ErrForkPending", err)
		}
		if err := child.checkpoint(t.Context(), received.VM()); err != nil {
			t.Fatal(err)
		}

		// The parent's host is lost: its page server, its pages and its VM
		// handle all go with it, and the fork point is never retired.
		if err := m.pages.Release("vm-2"); err != nil {
			t.Fatalf("releasing a child that published its own root: %v", err)
		}
		if err := m.pages.Close(); err != nil {
			t.Fatal(err)
		}
		m.machine.close()
		child.close()
		if err := received.VM().Close(t.Context()); err != nil {
			t.Fatal(err)
		}

		reopened, err := elsewhere.Open(t.Context(), "vm-2")
		if err != nil {
			t.Fatalf("the child was not recoverable after its parent's host was lost: %v", err)
		}
		defer reopened.Close(t.Context())
		recovered, err := newMachine(t, m.destPager, reopened, nil, handoff.State)
		if err != nil {
			t.Fatal(err)
		}
		if err := recovered.verify(t.Context(), at); err != nil {
			t.Fatalf("the recovered child lost the point it was forked at: %v", err)
		}
	})
}
