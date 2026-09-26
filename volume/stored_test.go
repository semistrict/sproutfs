package volume_test

import (
	"errors"
	"fmt"
	"maps"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// billSpecs is one volume of four whole pages, so a checkpoint whose other
// three pages are rewritten is a quarter live and compaction takes the fourth.
var billSpecs = []volume.VolumeSpec{{Name: "root", Size: 4 * checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize2MiB}}

// listedBytes sums what the store holds under one key prefix, listed directly
// rather than through the report under test.
func listedBytes(t *testing.T, h *harness, prefix string) uint64 {
	t.Helper()
	listed, err := platform.NewObjectPrefix(h.prefix.String() + prefix)
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	if err := platform.ListAll(t.Context(), h.objects, listed, func(object platform.ObjectMetadata) error {
		total += uint64(object.Size)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return total
}

// checkpointBytes is what one checkpoint's objects hold: its index object and
// its parts. They are immutable, so it is what they hold for as long as they
// are there.
func checkpointBytes(t *testing.T, h *harness, ref control.Ref) uint64 {
	t.Helper()
	_, name := control.SplitID(ref.VM)
	held := listedBytes(t, h, fmt.Sprintf("%svm/%s/ckpt/%d/", control.Namespace(ref.VM), name, ref.Sequence))
	if held == 0 {
		t.Fatalf("checkpoint %v holds nothing", ref)
	}
	return held
}

// recordBytes is the size of one VM's control record as it stands.
func recordBytes(t *testing.T, h *harness, id string) uint64 {
	t.Helper()
	key, err := platform.NewObjectKey(h.prefix.String() + control.RecordName(id))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := h.objects.Head(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(metadata.Size)
}

// billed is the report under test for one tenant.
func billed(t *testing.T, h *harness, tenant string) map[string]uint64 {
	t.Helper()
	report, err := volume.StoredBytes(t.Context(), h.objects, h.prefix, tenant)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// requireBill requires one tenant's report to be exactly want.
func requireBill(t *testing.T, h *harness, tenant, what string, want map[string]uint64) {
	t.Helper()
	if got := billed(t, h, tenant); !maps.Equal(got, want) {
		t.Fatalf("%s: tenant %q is billed %v, want %v", what, tenant, got, want)
	}
}

// Each page is billed to the VM that published it, for as long as the store
// holds it. A fork is billed only for what it published itself, and reading its
// parent's pages leaves them the parent's. Compaction moves a page's bytes into
// a later checkpoint of the VM that published it, so that VM pays for both
// copies until the next checkpoint's sweep reclaims the old one, and no other
// VM's bill moves. A deleted parent's pinned checkpoint stays billed to it,
// with no record, while its child reads through it. And every byte the store
// holds is billed once.
//
// The deployment check at the end is also what kills the in-tree guard
// checkpoint-compact-another-vm: a child that compacted its parent's
// checkpoint into its own parts would pay for a page it never published.
func TestEachPageIsBilledToTheVMThatPublishedIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		ctx := sim.WithRuntime(t.Context(), h.runtime)
		manager := h.manager(t, h.config())
		defer manager.Close(ctx)
		checkpointOf := func(vm *volume.VM) control.Ref {
			t.Helper()
			if err := vm.Checkpoint(ctx); err != nil {
				t.Fatal(err)
			}
			return vm.Status().Checkpoint
		}

		parent, err := manager.Create(ctx, "acme/parent", billSpecs)
		if err != nil {
			t.Fatal(err)
		}
		for page := range uint64(4) {
			writePage(t, parent, page, byte(page+1))
		}
		p1 := checkpointOf(parent)
		requireBill(t, h, "acme", "the parent's first checkpoint", map[string]uint64{
			"acme/parent": recordBytes(t, h, "acme/parent") + checkpointBytes(t, h, p1)})

		// The child's root publishes no page, and it reads all four of the
		// parent's where the parent published them.
		point, err := parent.ForkPoint(ctx, volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		child, err := manager.Fork(ctx, "acme/child", point)
		if err != nil {
			t.Fatal(err)
		}
		root := checkpointOf(child)
		parentBill := recordBytes(t, h, "acme/parent") + checkpointBytes(t, h, p1)
		requireBill(t, h, "acme", "the child's root", map[string]uint64{
			"acme/parent": parentBill,
			"acme/child":  recordBytes(t, h, "acme/child") + checkpointBytes(t, h, root)})

		// Rewriting three of the four leaves the parent's checkpoint a quarter
		// live in the child's view. It is the parent's, so the child does not
		// compact it, and the child's sweep reclaims its own root.
		for page := range uint64(3) {
			writePage(t, child, page, byte(page+11))
		}
		c1 := checkpointOf(child)
		requireBill(t, h, "acme", "the child's first checkpoint of its own", map[string]uint64{
			"acme/parent": parentBill,
			"acme/child":  recordBytes(t, h, "acme/child") + checkpointBytes(t, h, c1)})

		// The parent's own history: the pin keeps its first checkpoint.
		for page := range uint64(4) {
			writePage(t, parent, page, byte(page+21))
		}
		p2 := checkpointOf(parent)
		for page := range uint64(3) {
			writePage(t, parent, page, byte(page+31))
		}
		compactions := h.runtime.Probes()[checkpoint.ProbeCompactionRewrite]
		p3 := checkpointOf(parent)
		if h.runtime.Probes()[checkpoint.ProbeCompactionRewrite] != compactions+1 {
			t.Fatal("the parent's third checkpoint compacted nothing")
		}
		childBill := recordBytes(t, h, "acme/child") + checkpointBytes(t, h, c1)
		// The page compaction moved is in the third checkpoint's parts, and the
		// second checkpoint is spared for one checkpoint, so the parent pays for
		// it twice and the child not at all.
		compacted := recordBytes(t, h, "acme/parent") + checkpointBytes(t, h, p1) +
			checkpointBytes(t, h, p2) + checkpointBytes(t, h, p3)
		requireBill(t, h, "acme", "the compacting checkpoint", map[string]uint64{
			"acme/parent": compacted, "acme/child": childBill})

		// The next checkpoint's sweep reclaims the emptied one, and the bill
		// goes down by exactly what it held.
		p2Bytes := checkpointBytes(t, h, p2)
		writePage(t, parent, 0, 41)
		p4 := checkpointOf(parent)
		reclaimed := compacted + checkpointBytes(t, h, p4) - p2Bytes
		requireBill(t, h, "acme", "the checkpoint after it", map[string]uint64{
			"acme/parent": reclaimed, "acme/child": childBill})
		if got := recordBytes(t, h, "acme/parent") + checkpointBytes(t, h, p1) +
			checkpointBytes(t, h, p3) + checkpointBytes(t, h, p4); got != reclaimed {
			t.Fatalf("the parent holds %d bytes in its record and its first, third and fourth checkpoints, "+
				"and is billed %d", got, reclaimed)
		}

		// A VM of no tenant is billed in the report of no tenant, and in no
		// tenant's.
		solo, err := manager.Create(ctx, "solo", billSpecs)
		if err != nil {
			t.Fatal(err)
		}
		writePage(t, solo, 0, 51)
		s1 := checkpointOf(solo)
		requireBill(t, h, "", "a VM of no tenant", map[string]uint64{
			"solo": recordBytes(t, h, "solo") + checkpointBytes(t, h, s1)})

		// Deleting the parent removes its record and every checkpoint no pin
		// keeps. The one its child reads through stays, billed to the parent.
		p1Bytes := checkpointBytes(t, h, p1)
		for _, vm := range []*volume.VM{parent, child, solo} {
			if err := vm.Close(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := manager.Delete(ctx, "acme/parent"); err != nil {
			t.Fatal(err)
		}
		requireBill(t, h, "acme", "the parent deleted", map[string]uint64{
			"acme/parent": p1Bytes, "acme/child": childBill})
		readsPage(t, h, "acme/child", 3*checkpoint.PageSize2MiB, 4)

		var total uint64
		for _, tenant := range []string{"", "acme"} {
			for _, bytes := range billed(t, h, tenant) {
				total += bytes
			}
		}
		if held := listedBytes(t, h, ""); total != held {
			t.Fatalf("the VMs are billed %d bytes and the store holds %d", total, held)
		}
		checkStore(t, h, volume.AllowUnrecordedVM)
	})
}

// A tenant's name is a path element of every key it has, so a report for a
// name no tenant can have is refused rather than listed.
func TestStoredBytesRefusesATenantThatCannotExist(t *testing.T) {
	h := newHarness(t)
	if _, err := volume.StoredBytes(t.Context(), h.objects, h.prefix, "Not/A-Tenant"); !errors.Is(err, volume.ErrInvalidConfig) {
		t.Fatalf("a report for an invalid tenant gave %v, want ErrInvalidConfig", err)
	}
}
