package host_test

import (
	"fmt"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/resource"
)

// listedPull has a controller write a membership under code before the
// pulling host starts: the host with its own disk, where withSelf says, and
// five others at addresses nobody answers at, with the cluster cache on for
// percent of windows. The host's identity is read from its cache file before
// it starts, which is the identity the host then reads back.
func listedPull(t *testing.T, code rank.Code, withSelf bool, percent int) func(h *hostHarness) {
	return func(h *hostHarness) {
		h.configs[1].Cache.ClusterPercent = percent
		budget, err := resource.New(4 << 10)
		if err != nil {
			t.Fatal(err)
		}
		cache, err := checkpoint.NewCache(t.Context(), budget, h.configs[1].Cache)
		if err != nil {
			t.Fatal(err)
		}
		self := cache.Stats().Disk.Identity
		cache.Close()
		want := membership.Want{Code: code}
		disk := func(id rank.Identity) []membership.Disk {
			return []membership.Disk{{ID: id, Volume: "cache-0", Weight: 1}}
		}
		if withSelf {
			want.Hosts = append(want.Hosts, membership.Host{ID: self, Address: "host-1-pages", Disks: disk(self)})
		}
		for other := range byte(5) {
			id := rank.Identity{0xee, other + 1}
			want.Hosts = append(want.Hosts, membership.Host{ID: id,
				Address: platform.Address(fmt.Sprintf("gone-%d-pages", other)), Disks: disk(id)})
		}
		store, err := membership.NewStore(membership.Config{ObjectStore: h.configs[1].ObjectStore,
			ObjectPrefix: h.configs[1].ObjectPrefix})
		if err != nil {
			t.Fatal(err)
		}
		settle(t, t.Context(), store, want)
	}
}

// With the cluster cache turned on for no window, which is what a deployment
// runs until it rolls it out, a host in a list of six caches of 4+2 keeps a
// pulled VM whole on its own disk, as it did before there were stripes: its
// faults, and the faults of pages its pager evicted since, make no request of
// the object store.
func TestOutsideTheClusterShareAPulledVMIsKeptWhole(t *testing.T) {
	counted, pagers, guest, _, h := pulledRunWith(t, 64<<20, listedPull(t, rank.Code{K: 4, M: 2}, true, 0))
	if held := h.hosts[1].Membership(); held.List().Len() != 6 || held.Code() != (rank.Code{K: 4, M: 2}) {
		t.Fatalf("the host holds %d disks under %s, want six under 4+2", held.List().Len(), held.Code())
	}
	stats, err := h.hosts[1].WaitPulled(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Done || stats.Err != nil || stats.Bytes == 0 || stats.Pulled != stats.Bytes {
		t.Fatalf("the pull ended at %+v, want the whole checkpoint on the disk", stats)
	}
	counted.reset()
	before, err := pagers.pmem().Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	readPulled(t, guest)
	// The second pass reads the spill file's versions of what the first
	// read, which its evictions kept there.
	readPulled(t, guest)
	if gets := counted.count(); gets != 0 {
		t.Fatalf("faulting a pulled VM's pages in twice made %d requests of the object store, want none", gets)
	}
	if disk := h.hosts[1].Status().Cache.Disk; disk.Hits != pullPages+1 || disk.Lost != 0 ||
		disk.Entries != pullPages+1 {
		t.Fatalf("the page cache's disk reports %+v, want every page and the segment whole", disk)
	}
	after, err := pagers.pmem().Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if loads := after.VersionLoads - before.VersionLoads; loads != pullPages {
		t.Fatalf("the second pass loaded %d pages from the spill file, want every page", loads)
	}
}

// With the cluster cache on for every window, the host's disk places a
// pulled VM by the membership the host reads: one that does not rank this
// host's disk keeps nothing of it here, and its faults read the store. The
// pull hands every window to the cluster's fills, which send each of the six
// stripes of the six pages and the segment to the disks the membership ranks
// for them; nobody answers at those five members' addresses, so all 42 are
// dropped.
func TestInsideTheClusterShareAPulledVMIsPlacedByTheHostsList(t *testing.T) {
	counted, _, guest, _, h := pulledRunWith(t, 64<<20, listedPull(t, rank.Code{K: 4, M: 2}, false, 100))
	stats, err := h.hosts[1].WaitPulled(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Done || stats.Err != nil || stats.Bytes == 0 || stats.Pulled != stats.Bytes {
		t.Fatalf("the pull ended at %+v, want it done with everything handed over", stats)
	}
	if err := h.hosts[1].SettleFills(t.Context()); err != nil {
		t.Fatal(err)
	}
	fill := h.hosts[1].Status().Cache.Fill
	dropped := uint64(0)
	for _, stripes := range fill.Dropped {
		dropped += stripes
	}
	if fill.FromPublications != pullPages+1 || fill.Kept != 0 || fill.Sent != 0 || dropped != 6*(pullPages+1) {
		t.Fatalf("the fills came to %+v, want seven windows and their 42 stripes dropped", fill)
	}
	counted.reset()
	readPulled(t, guest)
	if gets := counted.count(); gets == 0 {
		t.Fatal("a host that keeps nothing of a pulled VM read its pages with no request of the object store")
	}
	if disk := h.hosts[1].Status().Cache.Disk; disk.Entries != 0 || disk.Hits != 0 {
		t.Fatalf("the page cache's disk reports %+v, want nothing kept and nothing served", disk)
	}
}
