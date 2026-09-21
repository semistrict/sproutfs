package simtest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
	"testing/synctest"
	"time"

	hostapi "github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/volume"
)

// templateVolumes are what a template has: the RAM every VM forked from it
// gets, which reads as the zeroes a cold boot starts from, and the root volume
// the guest image is written into.
var templateVolumes = []volume.VolumeSpec{
	{Name: simtest.MemoryVolume, Size: simtest.RAMPage, PageSize: simtest.RAMPage},
	{Name: "root", Size: simtest.PMEMPage, PageSize: simtest.PMEMPage},
}

// guestImage is a guest image's bytes: a page no untouched volume could read as.
func guestImage(value byte) []byte { return bytes.Repeat([]byte{value}, simtest.PMEMPage) }

// templateImport is one host's import of one guest image. Nothing of the host
// is in it: the image's bytes are the whole of what names the template.
func templateImport(image string, value byte) host.TemplateImport {
	return host.TemplateImport{Image: image, Volumes: templateVolumes, Root: "root",
		Source: bytes.NewReader(guestImage(value))}
}

// templateIDOf is the identity a guest image's bytes name.
func templateIDOf(image []byte) string {
	sum := sha256.Sum256(image)
	return hostapi.TemplateID(sum)
}

// TestAHostKilledMidImportImportsItAgainWhenItComesBack: the rollout that
// wedged the first GCE cluster of the day, under the identity a template has
// now. The host is taken away while it is reading the second guest image, so
// what it leaves is a template whose record is there and whose selected
// checkpoint is not the image and is pinned by nothing.
//
// The replacement finds it. It cannot choose another name — a template is named
// by the image's bytes and that has not changed — so it waits for the import to
// publish, and then recovers the record as any VM is recovered: it takes the
// epoch, which fences a writer that turns out to be alive, and imports again
// under it. The first image, which was imported whole, it does not import at
// all.
//
// What that leaves in the store is a collector's and nothing else, which is
// what the check at the end says.
func TestAHostKilledMidImportImportsItAgainWhenItComesBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCrashRuntime(1)
		ctx := sim.WithRuntime(t.Context(), runtime)
		prefix := newPrefix(t, "template-by-digest/")
		topology := simtest.Topology{Hosts: []string{"host-0"}}
		world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
			Knobs: campaignKnobs(t, runtime, topology), Prefix: prefix, Log: t.Logf})

		// The first image is imported whole.
		alpine, err := world.Host(0).TemplateOf(ctx, templateImport("alpine", 0xa5))
		if err != nil {
			t.Fatal(err)
		}
		if want := templateIDOf(guestImage(0xa5)); alpine.ID() != want {
			t.Fatalf("the first import landed at %s, want %s", alpine.ID(), want)
		}

		// And the pod is taken away in the middle of the next one.
		cut, err := world.KillDuring(ctx, 0, sim.CrashProcess, 500*time.Microsecond,
			func(ctx context.Context) error {
				_, err := world.Host(0).TemplateOf(ctx, templateImport("workload", 0x5a))
				return err
			})
		if !cut {
			t.Fatal("the kill landed after the import had finished: this scenario is about one it interrupts")
		}
		if err == nil {
			t.Fatal("an import interrupted by the loss of its host reported success")
		}
		t.Logf("the interrupted import ended as: %v", err)

		// The replacement pod comes back — under a name of its own, which
		// nothing about a template is any more — and asks for both templates.
		if err := world.Restart(ctx, 0); err != nil {
			t.Fatal(err)
		}
		again, err := world.Host(0).TemplateOf(ctx, templateImport("alpine", 0xa5))
		if err != nil {
			t.Fatalf("the replacement could not have the alpine template: %v", err)
		}
		if again.ID() != alpine.ID() {
			t.Fatalf("the replacement's alpine template is %s, want the %s already imported",
				again.ID(), alpine.ID())
		}
		// What the kill left at the workload's identity is the state this
		// scenario is about: a record, and a selected checkpoint that is not the
		// image and that nothing pinned.
		unfinished := templateIDOf(guestImage(0x5a))
		left, err := world.Host(0).Control().Read(ctx, unfinished)
		if err != nil {
			t.Fatalf("the interrupted import left no record at %s: %v", unfinished, err)
		}
		if len(left.Pinned) != 0 {
			t.Fatalf("the interrupted import pinned %v at %s: it is not unfinished", left.Pinned, unfinished)
		}

		request := templateImport("workload", 0x5a)
		// The host that wrote the half-finished record is gone, and the wait is
		// the whole of what separates that from an import still running.
		request.Wait = -1
		workload, err := world.Host(0).TemplateOf(ctx, request)
		if err != nil {
			t.Fatalf("the replacement could not recover the workload template: %v", err)
		}
		if workload.ID() != unfinished {
			t.Fatalf("the recovered workload template is %s, want %s", workload.ID(), unfinished)
		}
		recovered, err := world.Host(0).Control().Read(ctx, unfinished)
		if err != nil {
			t.Fatal(err)
		}
		if recovered.Epoch <= left.Epoch {
			t.Fatalf("the recovery did not take the epoch: it was %d and is %d", left.Epoch, recovered.Epoch)
		}
		if !recovered.IsPinned(recovered.Selected) {
			t.Fatalf("the recovery left %s selecting %d unpinned", unfinished, recovered.Selected)
		}

		// A template is only a template if something can be created from it,
		// which is a fork of the checkpoint it pinned.
		volumes := world.Host(0).Volumes()
		child, err := volumes.Fork(ctx, "vm-created", workload.Point)
		if err != nil {
			t.Fatalf("creating a VM from the recovered template: %v", err)
		}
		if err := child.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		page := make([]byte, simtest.PMEMPage)
		if err := child.Volume("root").Read(ctx, 0, page); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(page, guestImage(0x5a)) {
			t.Fatalf("the VM created from the recovered template reads %#x..., want the image it forked",
				page[:8])
		}
		if err := child.Close(ctx); err != nil {
			t.Fatal(err)
		}

		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
		// What the interrupted import left is a collector's: the parts that
		// never reached an index, the roots and intermediate checkpoints an
		// import publishes on its way, and the epoch the recovery superseded.
		if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), prefix,
			volume.AllowSupersededEpoch, volume.AllowUnpublishedIndex,
			volume.AllowUnreferencedCheckpoint, volume.AllowUnrecordedVM); err != nil {
			t.Error(err)
		}
		if t.Failed() {
			reportTrace(t, runtime)
		}
	})
}
