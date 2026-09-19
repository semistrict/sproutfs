package checkpoint_test

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/resource"
)

// The pre-mortem of the GCE soak's storage shape. The campaigns publish volumes
// of one to three pages, where a checkpoint is one part, a root is one segment
// and a read is one object. The soak's guest holds 256 MiB of memory and a
// 256 MiB file and rewrites scattered 4 KiB of every page of both every round,
// so each of its checkpoints is hundreds of pages across many parts, and each
// of its checks is hundreds of range reads through a page cache far smaller
// than what it walks. These are the assertions that shape has to meet.

// premortemPages is the volume this exercise publishes, in pages. It is large
// enough that the dirty set fills many parts and the read walks far more pages
// than the cache retains, and small enough that a unit test can hold it.
const premortemPages = 64

// premortemVolume is one soak-shaped volume: a page count that fills several
// segments' worth of table entries and many parts' worth of members.
func premortemVolume() map[string]uint64 {
	return map[string]uint64{
		"ram0": premortemPages * checkpoint.PageSize2MiB,
		"disk": premortemPages * checkpoint.PageSize2MiB,
	}
}

// scatter rewrites one 4 KiB sector of every page of every volume, which is
// what the witness's mutation does: every page of the guest is dirty and
// nothing about it is contiguous in what it changed.
func scatter(p *checkpoint.Publication, m *model, tag string, round int) {
	for _, name := range []string{"disk", "ram0"} {
		pages := m.sizes[name] / checkpoint.PageSize2MiB
		for page := range pages {
			sector := uint32((page + uint64(round)) % sectorsPerPage)
			m.dirty(p, name, page, sector, sectorData(tag+strconv.Itoa(round), page, sector))
		}
	}
}

// premortemStore is a store sized the way a soak's host is relative to its
// guests: parts that a round's dirty set overflows many times over, one builder
// slot, and a page cache that holds a fraction of what one check reads.
func premortemStore(t *testing.T, objects platform.ObjectStore, cache *checkpoint.Cache) *checkpoint.Store {
	t.Helper()
	return mustStore(t, checkpoint.Config{ObjectStore: objects, Cache: cache,
		Concurrency: 4, MaxBuilders: 1, PartBytes: 64 << 10})
}

// premortemCache is a page cache that holds a few pages of the many a check
// reads, so every round walks far more than it retains.
func premortemCache(t *testing.T) *checkpoint.Cache {
	t.Helper()
	budget, err := resource.New(8 * checkpoint.PageSize2MiB)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := checkpoint.NewCache(budget, checkpoint.CacheConfig{MaxConcurrentLoads: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.Close)
	return cache
}

// TestPremortemWholeVolumeCheckpointsReadBackThroughASmallCache publishes a
// round's whole-volume dirty set as many parts and then reads every byte back
// through a cache that holds a fraction of it, which is the soak's check: a
// range read per page, hundreds of them, against a budget that evicts under
// them. What each checkpoint says about itself must hold — the part count the
// last part carries, the member bytes the root records — and the bytes must be
// the ones the guest wrote.
func TestPremortemWholeVolumeCheckpointsReadBackThroughASmallCache(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{Seed: 3}).ObjectStore()
		cache := premortemCache(t)
		store := premortemStore(t, objects, cache)
		sizes := premortemVolume()
		index, err := store.Root(t.Context(), control.Ref{VM: "vm-soak", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(volumes2MiB(sizes))
		for round := 1; round <= 3; round++ {
			p := store.Begin(index, control.Ref{VM: "vm-soak", Sequence: uint64(round) + 1})
			scatter(p, m, "soak", round)
			published, err := p.Commit(t.Context(), m)
			if err != nil {
				t.Fatalf("round %d: publishing a whole-volume dirty set: %v", round, err)
			}
			// A dirty set this size does not fit one part, so the checkpoint is
			// several: the shape a one-part checkpoint never reaches.
			if parts := publishedParts(t, objects, "vm-soak", uint64(round)+1); parts < 2 {
				t.Fatalf("round %d: a whole-volume checkpoint wrote %d parts, want several",
					round, parts)
			}
			keys, violations := store.CheckIndex(t.Context(), published)
			if len(violations) != 0 {
				t.Fatalf("round %d: what the root says about its own parts is wrong: %v",
					round, violations)
			}
			if len(keys) == 0 {
				t.Fatalf("round %d: the root reaches no object at all", round)
			}
			checkRead(t, store, published, m)
			index = published
		}
		// The check is a read of every page, and the cache is far smaller than
		// what it walks: it must evict rather than refuse, and every byte must
		// still be the guest's.
		stats := cache.Stats()
		if stats.Evictions == 0 {
			t.Fatalf("a cache of 8 pages served %d pages of reads without evicting anything",
				stats.Hits+stats.Misses)
		}
		checkRead(t, store, index, m)
	})
}

// publishedParts counts the part objects one checkpoint wrote.
func publishedParts(t *testing.T, objects platform.ObjectStore, vm string, sequence uint64) int {
	t.Helper()
	prefix, err := platform.NewObjectPrefix(
		"deployment/vm/" + vm + "/ckpt/" + strconv.FormatUint(sequence, 10) + "/part/")
	if err != nil {
		t.Fatal(err)
	}
	count, token := 0, ""
	for {
		page, err := objects.List(t.Context(), platform.ListRequest{Prefix: prefix, ContinuationToken: token})
		if err != nil {
			t.Fatal(err)
		}
		count += len(page.Objects)
		if page.NextContinuationToken == "" {
			return count
		}
		token = page.NextContinuationToken
	}
}

// TestPremortemConcurrentPublicationsShareOneBuilderSlot runs several VMs'
// whole-volume checkpoints at once through a store with one builder slot, which
// is a host whose guests all became dirty on the same interval. Publication's
// memory is what the bound exists for, so the publications have to serialize on
// it rather than each taking a builder — and every one of them still has to
// land and read back as what its own model holds.
func TestPremortemConcurrentPublicationsShareOneBuilderSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		objects := sim.New(sim.Config{Seed: 5}).ObjectStore()
		store := premortemStore(t, objects, premortemCache(t))
		sizes := map[string]uint64{"ram0": 8 * checkpoint.PageSize2MiB}
		const vms = 4
		roots := make([]*checkpoint.Index, vms)
		models := make([]*model, vms)
		for vm := range vms {
			id := fmt.Sprintf("vm-%d", vm)
			root, err := store.Root(t.Context(), control.Ref{VM: id, Sequence: 1}, volumes2MiB(sizes))
			if err != nil {
				t.Fatal(err)
			}
			roots[vm], models[vm] = root, newModel(volumes2MiB(sizes))
		}
		published := make([]*checkpoint.Index, vms)
		failures := make([]error, vms)
		var wg sync.WaitGroup
		for vm := range vms {
			id := fmt.Sprintf("vm-%d", vm)
			p := store.Begin(roots[vm], control.Ref{VM: id, Sequence: 2})
			scatter(p, models[vm], id, 1)
			wg.Go(func() {
				index, err := p.Commit(context.WithoutCancel(t.Context()), models[vm])
				published[vm], failures[vm] = index, err
			})
		}
		wg.Wait()
		for vm := range vms {
			if failures[vm] != nil {
				t.Fatalf("vm-%d could not publish beside the others: %v", vm, failures[vm])
			}
			if _, violations := store.CheckIndex(t.Context(), published[vm]); len(violations) != 0 {
				t.Fatalf("vm-%d: %v", vm, violations)
			}
			checkRead(t, store, published[vm], models[vm])
		}
	})
}

// scatterPart rewrites one sector of a drawn share of every volume's pages,
// which is what leaves an older checkpoint partly live: a page nothing rewrites
// keeps its checkpoint's parts alive after every other member of them is dead,
// and compaction is what bounds that. A drawn share rather than a strided one,
// because a stride rewrites a checkpoint's pages either all at once or not at
// all and so never leaves one half dead.
func scatterPart(p *checkpoint.Publication, m *model, tag string, round int, random *rand.Rand) {
	for _, name := range []string{"disk", "ram0"} {
		pages := m.sizes[name] / checkpoint.PageSize2MiB
		for page := range pages {
			if random.IntN(5) < 3 {
				continue
			}
			sector := uint32((page + uint64(round)) % sectorsPerPage)
			m.dirty(p, name, page, sector, sectorData(tag+strconv.Itoa(round), page, sector))
		}
	}
}

// TestPremortemCompactionSparesEveryPinnedRoundAfterRound is the parent of the
// soak's fan-out: forked at some rounds and not others, so its record
// accumulates pins while its own guest keeps rewriting a share of its pages
// between them. Compaction is what bounds the dead bytes those rewrites leave,
// and what a pin protects is what it may not touch: every checkpoint a pinned root
// names has to read back exactly as the child that inherited it sees it,
// however many rounds of compaction and reclamation have run over the parent
// since.
func TestPremortemCompactionSparesEveryPinnedRoundAfterRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 9})
		ctx := sim.WithRuntime(t.Context(), runtime)
		store := premortemStore(t, runtime.ObjectStore(), premortemCache(t))
		sizes := map[string]uint64{"ram0": 16 * checkpoint.PageSize2MiB, "disk": 16 * checkpoint.PageSize2MiB}
		index, err := store.Root(ctx, control.Ref{VM: "parent", Sequence: 1}, volumes2MiB(sizes))
		if err != nil {
			t.Fatal(err)
		}
		m := newModel(volumes2MiB(sizes))
		random := rand.New(rand.NewPCG(0x50a4, 0x9))
		// The rounds a fork was taken at, whose checkpoints the record pins and
		// whose bytes are what that fork inherited.
		forkedAt := map[int]bool{2: true, 5: true}
		var pinned []uint64
		inherited := map[uint64]*model{}
		previous := index
		for round := 1; round <= 9; round++ {
			sequence := uint64(round) + 1
			p := store.Begin(index, control.Ref{VM: "parent", Sequence: sequence})
			p.Protect(pinned)
			scatterPart(p, m, "parent", round, random)
			next, err := p.Commit(ctx, m)
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			if forkedAt[round] {
				pinned = append(pinned, sequence)
				inherited[sequence] = m.clone()
			}
			if err := store.Reclaim(ctx, previous, next, pinned); err != nil {
				t.Fatalf("round %d: the sweep behind the checkpoint: %v", round, err)
			}
			previous, index = next, next
			// Everything a fork of an earlier round reads through has to be whole
			// and hold exactly what that fork inherited.
			for _, at := range pinned {
				held, err := store.Open(ctx, control.Ref{VM: "parent", Sequence: at})
				if err != nil {
					t.Fatalf("round %d: the pinned checkpoint %d does not open: %v", round, at, err)
				}
				if _, violations := store.CheckIndex(ctx, held); len(violations) != 0 {
					t.Fatalf("round %d: the pinned checkpoint %d is not whole: %v", round, at, violations)
				}
				checkPinnedRead(t, store, held, inherited[at])
			}
			checkRead(t, store, index, m)
		}
		// A parent whose pages a fraction of a round rewrites is the shape
		// compaction exists for: a run that never compacted would be asserting
		// nothing about what it spares.
		if runtime.Probes()[checkpoint.ProbeCompactionRewrite] == 0 {
			t.Fatal("no publication compacted, so nothing here asserted what compaction spares")
		}
	})
}

// checkPinnedRead requires one pinned checkpoint to hold exactly the bytes the
// fork taken at it inherited.
func checkPinnedRead(t *testing.T, store *checkpoint.Store, index *checkpoint.Index, m *model) {
	t.Helper()
	for _, name := range index.Volumes() {
		got := make([]byte, index.Size(name))
		if err := store.Read(t.Context(), index, name, 0, got); err != nil {
			t.Fatalf("reading %s of the pinned %s: %v", name, index.Ref(), err)
		}
		if !bytes.Equal(got, m.contents[name]) {
			t.Fatalf("the pinned %s no longer holds what the fork taken at it inherited of %s",
				index.Ref(), name)
		}
	}
}
