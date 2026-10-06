package host_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/platform/sim"
)

// A host given a hot tier and the cluster cache refuses to start, and names
// both: they are alternatives.
func TestAHostRefusesAHotTierBesideTheClusterCache(t *testing.T) {
	h := newHostHarness(t)
	config := h.configs[0]
	config.HotTier = checkpoint.HotTierConfig{Store: h.runtime.NewObjectStore("hot", sim.ObjectStoreConfig{})}
	config.Cache.ClusterPercent = 25
	started, err := host.StartHost(t.Context(), config)
	want := "host: invalid configuration: a hot tier beside the cluster cache: a hot tier (HotTier) and the " +
		"cluster cache (Cache.ClusterPercent 25) are alternatives; configure one of them"
	if started != nil || !errors.Is(err, host.ErrHotTierBesideClusterCache) || err.Error() != want {
		t.Fatalf("a host with a hot tier beside the cluster cache started with %v, want %q", err, want)
	}
}

// The hot tier is off by default. Hosts given no hot tier run without one: a
// VM written and closed on one opens on the other from the regional bucket,
// and neither host reports a hot tier.
func TestAHostGivenNoHotTierRunsWithoutOne(t *testing.T) {
	h := newHostHarness(t)
	h.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	vm, err := h.hosts[0].Volumes().Create(ctx, "cold", rootVolume)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ctx, vm, 0, "read from the regional bucket")
	if err := vm.Close(ctx); err != nil {
		t.Fatal(err)
	}
	opened, err := h.hosts[1].Volumes().Open(ctx, "cold")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, ctx, opened, 0, len("read from the regional bucket")); got != "read from the regional bucket" {
		t.Fatalf("the VM read back %q", got)
	}
	if err := opened.Close(ctx); err != nil {
		t.Fatal(err)
	}
	for n, started := range h.hosts {
		if got := started.Status().HotTier; got != nil {
			t.Fatalf("host %d, given no hot tier, reports one that did %+v", n, *got)
		}
	}
}

// Two hosts share a hot tier. A VM written and closed on one publishes its
// checkpoint to the hot tier behind the regional bucket, and opened on the
// other reads every checkpoint object it needs from the hot tier: no read
// misses it, and the regional bucket serves it no part.
func TestAVMOpenedOnAnotherHostReadsItsCheckpointFromTheHotTier(t *testing.T) {
	h := newHostHarness(t)
	hot := h.runtime.NewObjectStore("hot", sim.ObjectStoreConfig{GetLatency: time.Nanosecond,
		PutLatency: time.Nanosecond, BytesPerSecond: 1 << 60})
	for n := range h.configs {
		h.configs[n].HotTier = checkpoint.HotTierConfig{Store: hot}
	}
	h.start(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	vm, err := h.hosts[0].Volumes().Create(ctx, "hot", rootVolume)
	if err != nil {
		t.Fatal(err)
	}
	write(t, ctx, vm, 0, "read from the hot tier")
	if err := vm.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].SettleFills(ctx); err != nil {
		t.Fatal(err)
	}
	// The create's root, and the close's part and index object.
	if got := h.hosts[0].Status().HotTier; got.FromPublications != 3 || got.Sent != 3 || got.FromReads != 0 {
		t.Fatalf("the writing host's hot tier did %+v, want three objects published to it", *got)
	}
	opened, err := h.hosts[1].Volumes().Open(ctx, "hot")
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, ctx, opened, 0, len("read from the hot tier")); got != "read from the hot tier" {
		t.Fatalf("the VM read back %q", got)
	}
	if err := opened.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// The open's index object, its segment, and the page.
	got := h.hosts[1].Status().HotTier
	if got.Hits != 3 || got.Misses != 0 || got.Failed != [3]uint64{} {
		t.Fatalf("the reading host's hot tier did %+v, want three hits and nothing else", *got)
	}
}

// /status reports what a host's hot tier did under every reason a read of it
// fails and a fill of it is dropped, by the names /metrics lists, and nothing
// for a host with no hot tier.
func TestTheHotTierIsReportedByEveryReason(t *testing.T) {
	var stats checkpoint.HotTierStats
	for _, reason := range checkpoint.HotFailures() {
		stats.Failed[reason] = uint64(reason) + 1
	}
	for _, reason := range checkpoint.HotDrops() {
		stats.Dropped[reason] = uint64(reason) + 10
	}
	stats.Hits, stats.Misses, stats.Sent, stats.QueueBytes, stats.Down = 1, 2, 3, 4, true
	report := host.HotTierReport(&stats)
	if names := slices.Sorted(maps.Keys(report.Failed)); !slices.Equal(names, slices.Sorted(slices.Values(hostapi.HotTierFailures))) {
		t.Fatalf("the hot tier's failures are reported under %v, and /metrics lists %v", names, hostapi.HotTierFailures)
	}
	if names := slices.Sorted(maps.Keys(report.Dropped)); !slices.Equal(names, slices.Sorted(slices.Values(hostapi.HotTierDropReasons))) {
		t.Fatalf("the hot tier's drops are reported under %v, and /metrics lists %v", names, hostapi.HotTierDropReasons)
	}
	for at, name := range hostapi.HotTierFailures {
		if report.Failed[name] != uint64(at)+1 {
			t.Fatalf("the hot tier reports %d reads failed for %s, want %d", report.Failed[name], name, at+1)
		}
	}
	for at, name := range hostapi.HotTierDropReasons {
		if report.Dropped[name] != uint64(at)+10 {
			t.Fatalf("the hot tier reports %d fills dropped for %s, want %d", report.Dropped[name], name, at+10)
		}
	}
	if report.Hits != 1 || report.Misses != 2 || report.Sent != 3 || report.QueueBytes != 4 || !report.Down {
		t.Fatalf("the hot tier is reported as %+v, want 1 hit, 2 misses, 3 sent, a queue of 4 and down", *report)
	}
	if report := host.HotTierReport(nil); report != nil {
		t.Fatalf("a host with no hot tier reports %+v", *report)
	}
}
