package volume_test

import (
	"bytes"
	"fmt"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/volume"
)

// The pre-mortem of what the GCE soak's rounds do to one parent and its
// descendants. The soak
// forks one parent twice a round for six rounds, forks the children again, and
// deletes a seeded share between rounds — so a parent's record accumulates a
// pin a round while its guest rewrites every page between them, and the
// deployment ends with every VM deleted and `sproutfsctl check` over what is
// left.

// premortemAllowances is what cmd/sproutfs-orchestrator gives its /check: every
// leftover a live deployment always holds. A pre-mortem run loses no host and
// is refused nothing, so a violation outside these is durable state disagreeing
// with itself.
var premortemAllowances = []volume.Allowance{
	volume.AllowUnpublishedIndex, volume.AllowUnrecordedVM,
	volume.AllowUnreferencedCheckpoint, volume.AllowSupersededEpoch,
}

// premortemCheck runs the deployment check the soak ends with.
func premortemCheck(t *testing.T, h *harness, what string) {
	t.Helper()
	if err := volume.CheckDeployment(t.Context(), h.objects, h.prefix, premortemAllowances...); err != nil {
		t.Fatalf("%s: the deployment disagrees with itself: %v", what, err)
	}
}

// writePage puts one value over the first sector of one page of a VM.
func writePage(t *testing.T, vm *volume.VM, page uint64, value byte) {
	t.Helper()
	if err := vm.Volume("root").Write(t.Context(), page*checkpoint.PageSize,
		bytes.Repeat([]byte{value}, checkpoint.SectorSize)); err != nil {
		t.Fatal(err)
	}
}

// TestPremortemRoundsOfForksOffOneParentLeaveEveryDescendantWhole is the soak's
// parent: forked twice a round for several rounds, with its guest rewriting its
// pages between them, and every child forked again. Each fork pins a checkpoint
// of the parent that nothing ever gives back, each publication of the parent
// sweeps what it replaced and compacts what has gone mostly dead, and every
// descendant goes on reading the point it was taken at.
//
// The pins accumulate, so what the parent's own sweep may take shrinks every
// round; what must never shrink is what any descendant reads.
func TestPremortemRoundsOfForksOffOneParentLeaveEveryDescendantWhole(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := rewritingManager(t, h)
		defer manager.Close(t.Context())
		parent, write := forkedParent(t, h, manager, "vm")
		write(0, 1)
		write(checkpoint.PageSize, 2)
		if err := parent.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}

		// Every VM that exists, in the order it came into existence, and the
		// page values it must read back: page zero and page one of its root
		// volume, as its ancestors left them. The handles stay open and
		// are read through, because opening one elsewhere advances its epoch
		// and fences the writer that holds it.
		expected := map[string][2]byte{"vm": {1, 2}}
		open := []*volume.VM{parent}
		value := byte(2)
		const rounds = 4
		for round := 1; round <= rounds; round++ {
			// 2. the parent fans out. Both children of one round come from one
			// point, which is one pin: the same shape as the soak's two
			// fan-outs, taken as two points so the record gains a pin a round.
			point, err := parent.ForkPoint(t.Context(), volume.Prepared(nil, nil))
			if err != nil {
				t.Fatalf("round %d: taking the fork point: %v", round, err)
			}
			inherited := expected["vm"]
			var children []*volume.VM
			for side := range 2 {
				id := fmt.Sprintf("child-%d-%d", round, side)
				child, err := manager.Fork(t.Context(), id, point)
				if err != nil {
					t.Fatalf("round %d: forking %s: %v", round, id, err)
				}
				// What a child inherited is exactly what its parent held at the
				// seal, before anything of its own is written.
				checkPages(t, child, id+" inherited", inherited)
				expected[id] = inherited
				children = append(children, child)
			}
			// Each child diverges under a value of its own and publishes its
			// root, which is what ends the parent's seal.
			for index, child := range children {
				value++
				writePage(t, child, 0, value)
				expected[child.ID()] = [2]byte{value, inherited[1]}
				if err := child.Checkpoint(t.Context()); err != nil {
					t.Fatalf("round %d: publishing the root of %s: %v", round, child.ID(), err)
				}
				if index == 0 {
					// The grandchild: a root that names its grandparent's
					// checkpoints directly, because the child never rewrote the
					// page it reads through them.
					grandPoint, err := child.ForkPoint(t.Context(), volume.Prepared(nil, nil))
					if err != nil {
						t.Fatalf("round %d: taking the grandchild's point: %v", round, err)
					}
					id := fmt.Sprintf("grandchild-%d", round)
					grandchild, err := manager.Fork(t.Context(), id, grandPoint)
					if err != nil {
						t.Fatalf("round %d: forking %s: %v", round, id, err)
					}
					checkPages(t, grandchild, id+" inherited", expected[child.ID()])
					expected[id] = expected[child.ID()]
					if err := grandchild.Checkpoint(t.Context()); err != nil {
						t.Fatalf("round %d: publishing the root of %s: %v", round, id, err)
					}
					open = append(open, grandchild)
				}
			}
			open = append(open, children...)
			if status := parent.Status(); status.Sealed {
				t.Fatalf("round %d: the parent is still sealed after every child published", round)
			}
			// 1. the parent's own guest does some work and publishes it, which
			// is what makes the checkpoints its earlier forks pinned go dead
			// for everything but those pins.
			value++
			writePage(t, parent, 0, value)
			writePage(t, parent, 1, value)
			expected["vm"] = [2]byte{value, value}
			if err := parent.Checkpoint(t.Context()); err != nil {
				t.Fatalf("round %d: the parent's checkpoint: %v", round, err)
			}
			// Every VM that exists still reads what it and its ancestors published.
			for _, held := range open {
				checkPages(t, held, fmt.Sprintf("round %d: %s", round, held.ID()), expected[held.ID()])
			}
			premortemCheck(t, h, fmt.Sprintf("round %d", round))
		}

		// A pin a round, and nothing ever gives one back.
		if record := pins(t, h, "vm"); len(record.Pinned) != rounds {
			t.Fatalf("the parent pins %v after %d rounds of forks, want one a round",
				record.Pinned, rounds)
		}
		for _, held := range open {
			if err := held.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		premortemCheck(t, h, "every writer closed")
		// And every one of them still reads the same pages from a host that
		// never held it, which is what a start on the other host is.
		for id, want := range expected {
			readsPages(t, h, id, want)
		}
	})
}

// TestPremortemDeletingAParentThenAChildLeavesACheckableDeployment is the end
// of the soak: a parent with children on two hosts is deleted, then one of the
// children, and then every VM. What is left under each prefix is the checkpoints
// the deleted VMs pinned, which is what a collector owns — and the deployment
// check has to say so rather than report it as a deployment that disagrees with
// itself.
func TestPremortemDeletingAParentThenAChildLeavesACheckableDeployment(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		manager, _ := rewritingManager(t, h)
		defer manager.Close(t.Context())
		parent, write := forkedParent(t, h, manager, "vm")
		write(0, 1)
		write(checkpoint.PageSize, 2)
		if err := parent.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Two children of one fork point, as a round's fan-out onto both hosts is,
		// and a grandchild of one of them.
		point, err := parent.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		here, err := manager.Fork(t.Context(), "child-here", point)
		if err != nil {
			t.Fatal(err)
		}
		away, err := manager.Fork(t.Context(), "child-away", point)
		if err != nil {
			t.Fatal(err)
		}
		writePage(t, here, 0, 3)
		if err := here.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := away.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		grandPoint, err := here.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		grandchild, err := manager.Fork(t.Context(), "grandchild", grandPoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := grandchild.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, held := range []*volume.VM{grandchild, away, here, parent} {
			if err := held.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		premortemCheck(t, h, "before any delete")

		// The parent goes first, with children on both sides still reading it.
		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatal(err)
		}
		premortemCheck(t, h, "the parent deleted")
		readsPages(t, h, "child-here", [2]byte{3, 2})
		readsPages(t, h, "child-away", [2]byte{1, 2})
		readsPages(t, h, "grandchild", [2]byte{3, 2})

		// Then one of the children, whose own grandchild still reads through it.
		if err := manager.Delete(t.Context(), "child-here"); err != nil {
			t.Fatal(err)
		}
		premortemCheck(t, h, "a child deleted")
		readsPages(t, h, "child-away", [2]byte{1, 2})
		readsPages(t, h, "grandchild", [2]byte{3, 2})

		// And then the rest, which is what the soak does before it checks.
		for _, id := range []string{"child-away", "grandchild"} {
			if err := manager.Delete(t.Context(), id); err != nil {
				t.Fatalf("deleting %s: %v", id, err)
			}
		}
		premortemCheck(t, h, "every VM deleted")
		// What is left is the pinned checkpoints of deleted VMs and nothing else:
		// no control record, and no object of a VM nothing was forked from.
		for _, key := range h.objectKeys(t) {
			if !bytes.HasPrefix([]byte(key), []byte("vm/")) {
				t.Fatalf("the deployment kept %s, which is not an object of a pinned checkpoint", key)
			}
		}
	})
}

// checkPages requires one open VM to read the two page values its ancestors left it.
func checkPages(t *testing.T, vm *volume.VM, what string, want [2]byte) {
	t.Helper()
	for page, value := range want {
		got := make([]byte, checkpoint.SectorSize)
		if err := vm.Volume("root").Read(t.Context(), uint64(page)*checkpoint.PageSize, got); err != nil {
			t.Fatalf("%s: reading page %d: %v", what, page, err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{value}, checkpoint.SectorSize)) {
			t.Fatalf("%s: page %d reads %d..., want %d...", what, page, got[0], value)
		}
	}
}

// readsPages requires one VM to read the two page values its ancestors left it,
// opening it as a host that never held it would.
func readsPages(t *testing.T, h *harness, id string, want [2]byte) {
	t.Helper()
	readsPage(t, h, id, 0, want[0])
	readsPage(t, h, id, checkpoint.PageSize, want[1])
}
