package volume_test

import (
	"bytes"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/resource"
	"github.com/semistrict/sproutfs/volume"
)

// cachedStore is a checkpoint store with a page cache, which is what a host
// runs: the pages a VM reads stay resident, keyed by the page identity
// (VM, sequence, volume, page) the checkpoint gave them. A test that reuses a
// VM identity needs one, because two VMs whose checkpoints share a sequence
// share those keys.
func cachedStore(t *testing.T, h *harness) *checkpoint.Store {
	t.Helper()
	budget, err := resource.New(64 << 20)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(budget, checkpoint.CacheConfig{MaxConcurrentLoads: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	store, err := checkpoint.NewStore(checkpoint.Config{ObjectStore: h.objects, ObjectPrefix: h.prefix,
		Cache: cache, PartBytes: h.knobs.PartBytes, MaxIndexBytes: h.knobs.MaxIndexBytes})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// sector is one sector of one repeated byte, which is what these tests write so
// that bytes from the wrong VM are recognisable on sight.
func sector(value byte) []byte { return bytes.Repeat([]byte{value}, checkpoint.SectorSize) }

// readSector reads one sector of a VM's volume.
func readSector(t *testing.T, vm *volume.VM, name string, offset uint64) []byte {
	t.Helper()
	got := make([]byte, checkpoint.SectorSize)
	if err := vm.Volume(name).Read(t.Context(), offset, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// A VM created under a deleted VM's identity must read its own bytes. Nothing
// stops an identity from being handed out again — the orchestrator allocates
// them and a deleted one is gone from the deployment — so the two VMs must not
// share a checkpoint sequence: the page identity a page cache keys a
// resident page by is (VM, sequence, volume, page), and two VMs that agree on
// all four are one VM as far as every reader is concerned.
func TestARecreatedIdentityReadsItsOwnBytesRatherThanTheDeadVMs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		config := h.config()
		config.Store = cachedStore(t, h)
		manager := h.manager(t, config)
		defer manager.Close(t.Context())

		dead, _ := createVM(t, manager, "vm")
		if err := dead.Volume("root").Write(t.Context(), 0, sector(0xdd)); err != nil {
			t.Fatal(err)
		}
		if err := dead.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Reading it back through the cache is what leaves the dead VM's page
		// resident under its page identity.
		if got := readSector(t, dead, "root", 0); !bytes.Equal(got, sector(0xdd)) {
			t.Fatalf("the first VM reads back as %#x...", got[:8])
		}
		if err := dead.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatal(err)
		}

		reborn, _ := createVM(t, manager, "vm")
		defer reborn.Close(t.Context())
		// A VM created under a free identity holds nothing: every page of it
		// reads as zeroes, whatever the identity held before.
		if got := readSector(t, reborn, "root", 0); !bytes.Equal(got, make([]byte, checkpoint.SectorSize)) {
			t.Fatalf("a new VM under a used identity reads %#x..., want zeroes", got[:8])
		}
		if err := reborn.Volume("root").Write(t.Context(), 0, sector(0xaa)); err != nil {
			t.Fatal(err)
		}
		if err := reborn.Checkpoint(t.Context()); err != nil {
			t.Fatalf("the recreated VM's checkpoint: %v", err)
		}
		// The page it just published is its own. A checkpoint of this VM that
		// took a sequence the dead one had used would serve the dead VM's bytes
		// out of the cache here, and every host that ever ran that VM would.
		if got := readSector(t, reborn, "root", 0); !bytes.Equal(got, sector(0xaa)) {
			t.Fatalf("the recreated VM reads %#x..., want its own %#x...", got[:8], sector(0xaa)[:8])
		}
		reopened, err := manager.Open(t.Context(), "vm")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		if got := readSector(t, reopened, "root", 0); !bytes.Equal(got, sector(0xaa)) {
			t.Fatalf("the recreated VM reopened reads %#x..., want its own %#x...", got[:8], sector(0xaa)[:8])
		}
	})
}

// A deleted VM that was ever forked leaves the checkpoints its record pinned:
// forks this deployment cannot enumerate read through them, so the delete
// frees the identity and leaves the objects. Creating a VM under that identity
// would publish into the leftovers — the very keys they hold, create-if-absent
// — so it is refused, and the refusal says the identity was used before.
func TestCreatingAVMUnderAnIdentityWithLeftoversIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		config := h.config()
		config.Store = cachedStore(t, h)
		manager := h.manager(t, config)
		defer manager.Close(t.Context())

		parent, write := forkedParent(t, h, manager, "vm")
		write(0, 0xdd)
		if err := parent.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		point, err := parent.ForkPoint(t.Context(), volume.Prepared(nil, nil))
		if err != nil {
			t.Fatal(err)
		}
		child, err := manager.Fork(t.Context(), "child", point)
		if err != nil {
			t.Fatal(err)
		}
		if err := child.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := child.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := parent.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		pinned := pins(t, h, "vm").Pinned
		if len(pinned) == 0 {
			t.Fatal("the fixture expected the fork to pin the parent")
		}
		if err := manager.Delete(t.Context(), "vm"); err != nil {
			t.Fatal(err)
		}
		// The pinned checkpoint is still there, which is what the child reads
		// through.
		if got := objectsUnder(t, h, h.objects, control.Ref{VM: "vm", Sequence: pinned[0]}); len(got) == 0 {
			t.Fatal("the delete took the pinned checkpoint the child reads through")
		}

		again, err := manager.Create(t.Context(), "vm", reclaimSpecs)
		if !errors.Is(err, volume.ErrIdentityUsed) {
			if err == nil {
				defer again.Close(t.Context())
			}
			t.Fatalf("creating a VM under an identity with leftovers = %v, want it refused", err)
		}
		if got := err.Error(); !bytes.Contains([]byte(got), []byte("used before")) {
			t.Fatalf("the refusal reads %q, want it to say the identity was used before", got)
		}
		// The child still reads what it inherited, which is the whole reason
		// those objects are there.
		reopened, err := manager.Open(t.Context(), "child")
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close(t.Context())
		if got := readSector(t, reopened, "root", 0); !bytes.Equal(got, sector(0xdd)) {
			t.Fatalf("the child reads %#x..., want what it inherited", got[:8])
		}
	})
}
