//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"os"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/volume"
)

// TestFirecrackerOwnerRereadsMovedPages is the first
// inheritance of the isolated arena over a real guest. A parent writes a working
// set and is captured, and a child of that checkpoint on the same host reads
// all of it. Each published page the child reaches is moved out of the parent's
// private file into the tenant's shared file. The parent then reads its working
// set again. Its mapping of each moved page is the shared copy, so that read
// faults on none of them and copies none: a move saves the page's memory. The
// guest's kernel goes on running meanwhile, and a store it makes into a moved
// page traps on the write protection and copies it, as a store into any shared
// page does; those are the only faults the reread may see.
func TestFirecrackerOwnerRereadsMovedPages(t *testing.T) {
	binary := os.Getenv("SPROUTFS_FIRECRACKER")
	if binary == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx := t.Context()
	cluster := newMigrationCluster(t, ctx)
	parent, err := cluster.source.Create(ctx, "owner", []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: 128 << 20, PageSize: ramPageBytes(t)},
		{Name: "root", Size: 64 << 20, PageSize: checkpoint.PageSize2MiB},
	})
	if err != nil {
		t.Fatal(err)
	}
	loadRootImage(t, ctx, parent.Volume("root"))
	if err := parent.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	// Arenas that hold both guests whole, so that no page is evicted: every
	// fault counted below is one a move caused.
	pagers := newConfiguredHostPagers(t, ctx, hostPagersConfig{
		RAM:      hostPagerBudgets{Arena: 256 << 20, Logical: 384 << 20, Dirty: 256 << 20},
		PMEM:     hostPagerBudgets{Arena: 128 << 20, Logical: 192 << 20, Dirty: 128 << 20},
		Isolated: true,
	})
	ram := pagers.pagers.Ram
	config := migrationConfig(t, binary, pagers, parent)
	p, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
	command(t, ctx, p, "pressure 48\n", "SPROUTFS_PRESSURE bytes=50331648")
	ckpt, err := parent.Snapshot(ctx, prepareAndResume(p), volume.Terms{})
	if err != nil {
		t.Fatalf("capture: %v\n%s", err, consoleText(p))
	}
	if err := ckpt.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	point, err := cluster.source.Inherit(ctx, ckpt.Ref())
	if err != nil {
		t.Fatal(err)
	}
	fork, err := cluster.source.Fork(ctx, "child", point)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fork.Close(t.Context()) })
	if err := fork.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	beforeChild, err := ram.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restoring := config
	restoring.VM = fork
	restoring.RestoreState = ckpt.State()
	child, err := vmmachine.Start(ctx, restoring)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Close() })
	if err := child.Release(ctx); err != nil {
		t.Fatal(err)
	}
	command(t, ctx, child, "checkpressure\n", "SPROUTFS_PRESSURE_OK bytes=50331648")
	afterChild, err := ram.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	moved := afterChild.MovedPages - beforeChild.MovedPages
	if moved == 0 {
		t.Fatal("the child moved none of the pages it inherited")
	}

	// The parent's first command after the child ran refaults whatever of the
	// guest's own the command path touches and the child moved. The one after
	// it reads the working set and nothing else the first did not.
	command(t, ctx, p, "read\n", "SPROUTFS_VALUE ram=7 disk=0")
	before, err := ram.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	command(t, ctx, p, "checkpressure\n", "SPROUTFS_PRESSURE_OK bytes=50331648")
	after, err := ram.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sharing, err := ram.Sharing(ctx)
	if err != nil {
		t.Fatal(err)
	}
	faults, copies := after.Faults-before.Faults, after.CopyOnWrites-before.CopyOnWrites
	stores := after.ProtectTraps - before.ProtectTraps
	t.Logf("moved=%d owner reread: faults=%d copy_on_writes=%d unmapped_copy_on_writes=%d read_traps=%d store_traps=%d protect_traps=%d revoked_pages=%d; saved=%d MiB",
		moved, faults, copies, after.UnmappedCopyOnWrites-before.UnmappedCopyOnWrites,
		after.ReadTraps-before.ReadTraps, after.StoreTraps-before.StoreTraps, after.ProtectTraps-before.ProtectTraps,
		afterChild.RevokedPages-beforeChild.RevokedPages, sharing.Ram.SavedBytes>>20)
	if faults != stores || copies != stores {
		t.Fatalf("the parent's reread of %d moved pages faulted %d times and copied %d pages, want only the %d stores it made",
			moved, faults, copies, stores)
	}
}
