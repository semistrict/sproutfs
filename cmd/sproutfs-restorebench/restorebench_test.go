package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// faithful reads g's memory as it was published.
func faithful(g *guest) reader {
	return func(ctx context.Context, offset uint64, dst []byte) error {
		for at := uint64(0); at < uint64(len(dst)); at += g.pageSize {
			if err := g.ReadPage(ctx, volume, (offset+at)/g.pageSize, dst[at:at+g.pageSize]); err != nil {
				return err
			}
		}
		return nil
	}
}

func mustGuest(t *testing.T, vm string, pageSize, pages uint64) *guest {
	t.Helper()
	g, err := newGuest(vm, pageSize, pages)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Following the links from any page, or from any fault run, visits every one
// of them once before it comes back.
func TestEachLinkIsOneCycleThroughTheGuest(t *testing.T) {
	g := mustGuest(t, "guest-4k", checkpoint.PageSize4KiB, 8192)
	for name, links := range map[string][]uint32{"pages": g.nextPage, "runs": g.nextRun} {
		seen := make([]bool, len(links))
		at := uint32(5 % len(links))
		for range links {
			if seen[at] {
				t.Fatalf("the %s' links come back to %d before visiting all %d", name, at, len(links))
			}
			seen[at] = true
			at = links[at]
		}
		if at != uint32(5%len(links)) {
			t.Fatalf("the %s' links end at %d after %d hops, not where they began", name, at, len(links))
		}
	}
	if len(g.nextPage) != 8192 || len(g.nextRun) != 4 {
		t.Fatalf("%d page links and %d run links, want 8192 and 4", len(g.nextPage), len(g.nextRun))
	}
}

// A chain reads next the unit the bytes it read last name, whatever those
// are, and a page whose bytes are not the guest's reads back wrong.
func TestAChainReadsWhereTheBytesItReadLink(t *testing.T) {
	g := mustGuest(t, "guest-4k", checkpoint.PageSize4KiB, 4096)
	// Every page links seven pages on, and is otherwise zeros.
	var offsets []uint64
	sevenOn := func(_ context.Context, offset uint64, dst []byte) error {
		offsets = append(offsets, offset)
		clear(dst)
		binary.LittleEndian.PutUint64(dst[pageLinkAt:], (offset/g.pageSize+7)%g.pages)
		return nil
	}
	got, err := walk(t.Context(), g, access{Pattern: patternChain, Unit: unitPage, Concurrency: 1, Reads: 5, Seed: 3},
		sevenOn, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := got.Units[0]
	want := []uint64{first, (first + 7) % 4096, (first + 14) % 4096, (first + 21) % 4096, (first + 28) % 4096}
	if !slices.Equal(got.Units, want) {
		t.Fatalf("the chain read units %v, want %v", got.Units, want)
	}
	for at, offset := range offsets {
		if offset != want[at]*checkpoint.PageSize4KiB {
			t.Fatalf("read %d was at %d, want %d", at, offset, want[at]*checkpoint.PageSize4KiB)
		}
	}
	if got.Wrong != 5 || got.Failed != 0 || len(got.Latencies) != 5 {
		t.Fatalf("wrong %d, failed %d and %d latencies, want 5, 0 and 5", got.Wrong, got.Failed, len(got.Latencies))
	}
}

// A chain of fault runs reads each whole run, and goes on to the run its first
// page links to.
func TestAChainOfRunsFollowsTheRunLinks(t *testing.T) {
	g := mustGuest(t, "guest-2m", checkpoint.PageSize2MiB, 64)
	var offsets []uint64
	read := faithful(g)
	got, err := walk(t.Context(), g, access{Pattern: patternChain, Unit: unitRun, Concurrency: 1, Reads: 16, Seed: 9},
		func(ctx context.Context, offset uint64, dst []byte) error {
			if len(dst) != faultRunBytes {
				return fmt.Errorf("a read of %d bytes, want a fault run's %d", len(dst), faultRunBytes)
			}
			offsets = append(offsets, offset)
			return read(ctx, offset, dst)
		}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for at := 1; at < len(got.Units); at++ {
		if want := uint64(g.nextRun[got.Units[at-1]]); got.Units[at] != want {
			t.Fatalf("hop %d read run %d after run %d, want %d", at, got.Units[at], got.Units[at-1], want)
		}
	}
	units := slices.Sorted(slices.Values(got.Units))
	if !slices.Equal(units, []uint64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}) {
		t.Fatalf("the chain read runs %v, want each of the 16 once", units)
	}
	if offsets[3] != got.Units[3]*faultRunBytes || got.Wrong != 0 || got.Failed != 0 {
		t.Fatalf("hop 3 at %d of run %d, wrong %d, failed %d", offsets[3], got.Units[3], got.Wrong, got.Failed)
	}
}

// A random read reads the units it is asked for, none twice, in an order its
// seed draws; a sequential read reads every unit in order.
func TestRandomAndSequentialReadsReadEachUnitOnce(t *testing.T) {
	g := mustGuest(t, "guest-4k", checkpoint.PageSize4KiB, 4096)
	random, err := walk(t.Context(), g, access{Pattern: patternRandom, Unit: unitPage, Concurrency: 4, Reads: 4096,
		Seed: 1}, faithful(g), nil)
	if err != nil {
		t.Fatal(err)
	}
	every := make([]uint64, 4096)
	for at := range every {
		every[at] = uint64(at)
	}
	if !slices.Equal(slices.Sorted(slices.Values(random.Units)), every) || slices.Equal(random.Units, every) {
		t.Fatalf("a random read of every page read %v..., want every page once, out of order", random.Units[:8])
	}
	again, err := walk(t.Context(), g, access{Pattern: patternRandom, Unit: unitPage, Concurrency: 1, Reads: 8,
		Seed: 1}, faithful(g), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again.Units, random.Units[:8]) {
		t.Fatalf("a seed's first eight reads were %v, then %v", random.Units[:8], again.Units)
	}
	sequential, err := walk(t.Context(), g, access{Pattern: patternSequential, Unit: unitRun, Concurrency: 1},
		faithful(g), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sequential.Units, []uint64{0, 1}) || sequential.Wrong != 0 || random.Wrong != 0 {
		t.Fatalf("a sequential read of runs read %v, wrong %d and %d", sequential.Units, sequential.Wrong, random.Wrong)
	}
	if _, err := walk(t.Context(), g, access{Pattern: patternRandom, Unit: unitRun, Concurrency: 1, Reads: 3},
		faithful(g), nil); err == nil || err.Error() != "3 reads of 2 units: a random reads each unit at most once" {
		t.Fatalf("three reads of two runs: %v", err)
	}
}

// A profiled run is a gzipped CPU profile.
func TestAProfiledRunIsAProfile(t *testing.T) {
	profile, err := profiled(true, func() error {
		_, err := calibrate(t.Context(), time.Millisecond)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(profile, []byte{0x1f, 0x8b}) {
		t.Fatalf("a profile beginning %x, want gzip's 1f8b", profile[:min(len(profile), 2)])
	}
	none, err := profiled(false, func() error { return nil })
	if err != nil || none != nil {
		t.Fatalf("an unprofiled run gave %d bytes and %v", len(none), err)
	}
}

// The cases parse, and only as their text says.
func TestCasesParse(t *testing.T) {
	specs, err := parseCases(defaultCases)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, spec := range specs {
		texts = append(texts, spec.String())
	}
	want := "2MiB/chain/page/1,2MiB/chain/run/1,2MiB/random/page/1,2MiB/random/page/4,2MiB/random/page/16," +
		"2MiB/sequential/page/16,4KiB/chain/page/1,4KiB/chain/run/1,4KiB/random/page/1,4KiB/random/page/4," +
		"4KiB/random/page/16,4KiB/sequential/run/16"
	if got := fmt.Sprint(texts); got != fmt.Sprint(strings.Split(want, ",")) {
		t.Fatalf("the default cases are %s, want %s", got, want)
	}
	for text, problem := range map[string]string{
		"2MiB/chain/page/4":   `a case "2MiB/chain/page/4": 4 reads at a time of a chain: want one or more, and one for a chain`,
		"8KiB/chain/page/1":   `a case "8KiB/chain/page/1": its page size is 2MiB or 4KiB`,
		"2MiB/strided/page/1": `a case "2MiB/strided/page/1": a pattern "strided": want sequential, random or chain`,
	} {
		if _, err := parseCase(text); err == nil || err.Error() != problem {
			t.Fatalf("%s: %v, want %s", text, err, problem)
		}
	}
}

func splitComma(text string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for len(text) > 0 {
			field, rest, _ := bytes.Cut([]byte(text), []byte(","))
			if !yield(string(field)) {
				return
			}
			text = string(rest)
		}
	}
}

// simNodes is six nodes over one simulated network, object store and set of
// disks, each reading through a hot tier in hot where it is not nil, closed
// with the test.
func simNodes(t *testing.T, ctx context.Context, runtime *sim.Runtime, hot platform.ObjectStore) []controller {
	t.Helper()
	var nodes []controller
	for index := range 6 {
		name := fmt.Sprintf("node-%d", index)
		disk := runtime.NewDisk(name, sim.DiskConfig{})
		file, err := disk.Open(ctx, "cache", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		address := platform.Address(name + "/pages")
		n, err := newNode(ctx, nodeConfig{address: address, listen: address, dialFrom: platform.Address(name),
			network: runtime.Network(), objects: runtime.ObjectStore(), file: file, cacheBytes: 512 << 20,
			deployment:  checkpoint.CacheDeployment{Store: "sim", Bucket: "bench", Prefix: "run"},
			memoryBytes: 64 << 20, fillQueueBytes: 1 << 30, fillBytesPerSecond: 4 << 30,
			serveRate:     500 << 20,
			dropPageCache: func() error { return nil }, hotObjects: hot, disk: disk})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			n.close()
			_ = file.Close()
		})
		nodes = append(nodes, n)
	}
	return nodes
}

// Six nodes under 4+2 publish a guest of each page size and read every case
// back, from the cluster and from the store. Every read reads the guest's
// bytes and none is served by the memory tier. A read of the cluster asks the
// store for the checkpoint's index, and for the fault runs of 4 KiB pages it
// hedges: 2,048 pages take past the bound on the simulated network, and each
// is three requests of at most 4 MiB. A read of the store asks it for the
// index, the page table and every page, a 2 MiB page with its header being
// past half of what one request may fetch.
func TestEveryCaseReadsTheGuestBack(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		nodes := simNodes(t, ctx, runtime, nil)
		specs, err := parseCases(defaultCases)
		if err != nil {
			t.Fatal(err)
		}
		result, profiles, err := drive(ctx, nodes, driveConfig{
			pages: map[uint64]uint64{checkpoint.PageSize2MiB: 16, checkpoint.PageSize4KiB: 4096},
			code:  "4+2", rounds: 1, cases: specs, sources: []string{sourceCluster, sourceStore}, reads: 8,
			runReads: 4, lost: 3, loseAfter: time.Second, cleared: 20 * time.Second, seed: 1,
			// Time stands still in the bubble, so the reader times one call of
			// each step.
			calibrate: 0})
		if err != nil {
			t.Fatal(err)
		}
		if len(profiles) != 0 || len(result.Cases) != 24 {
			t.Fatalf("%d profiles and %d cases, want none and 24", len(profiles), len(result.Cases))
		}
		var got []string
		for _, c := range result.Cases {
			got = append(got, fmt.Sprintf("%s %s: %d reads, %d gets, %d hedged, wrong %d, failed %d, %d memory hits",
				c.Case, c.Source, c.Reads, c.StoreGets, c.Read.StoreHedges, c.Wrong, c.Failed, c.MemoryHits))
		}
		slices.Sort(got)
		want := []string{
			"2MiB/chain/page/1 cluster: 8 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/chain/page/1 store: 8 reads, 10 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/chain/run/1 cluster: 4 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/chain/run/1 store: 4 reads, 18 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/random/page/1 cluster: 8 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/random/page/1 store: 8 reads, 10 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/random/page/16 cluster: 8 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/random/page/16 store: 8 reads, 10 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/random/page/4 cluster: 8 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/random/page/4 store: 8 reads, 10 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/sequential/page/16 cluster: 16 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"2MiB/sequential/page/16 store: 16 reads, 18 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/chain/page/1 cluster: 8 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/chain/page/1 store: 8 reads, 10 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/chain/run/1 cluster: 2 reads, 7 gets, 2 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/chain/run/1 store: 2 reads, 8 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/random/page/1 cluster: 8 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/random/page/1 store: 8 reads, 10 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/random/page/16 cluster: 8 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/random/page/16 store: 8 reads, 10 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/random/page/4 cluster: 8 reads, 1 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/random/page/4 store: 8 reads, 10 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/sequential/run/16 cluster: 2 reads, 7 gets, 2 hedged, wrong 0, failed 0, 0 memory hits",
			"4KiB/sequential/run/16 store: 2 reads, 8 gets, 0 hedged, wrong 0, failed 0, 0 memory hits",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("the cases read\n%s\nwant\n%s", fmt.Sprint(got), fmt.Sprint(want))
		}
		if len(result.Calibration.Steps["2MiB"]) != len(calibrationSteps) {
			t.Fatalf("the reader timed %v, want each of %v", result.Calibration.Steps["2MiB"], calibrationSteps)
		}
	})
}

// A run that only publishes publishes the guest of each page size its cases
// name, once a round as a VM of its own, and counts what each publication's
// fills did on every node. Under 4+2 on six nodes each of the sixteen pages'
// windows and the segment's puts one stripe on each node: the publisher keeps
// one of each and sends the other five.
func TestAPublishingRunCountsEachPublicationsFills(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		nodes := simNodes(t, ctx, runtime, nil)
		specs, err := parseCases("2MiB/sequential/page/16")
		if err != nil {
			t.Fatal(err)
		}
		result, profiles, err := drive(ctx, nodes, driveConfig{pages: map[uint64]uint64{checkpoint.PageSize2MiB: 16},
			code: "4+2", cases: specs, lost: 3, publishes: 2})
		if err != nil {
			t.Fatal(err)
		}
		if len(profiles) != 0 || len(result.Cases) != 0 || len(result.Publications) != 2 {
			t.Fatalf("%d profiles, %d cases and %d publications, want none, none and 2", len(profiles),
				len(result.Cases), len(result.Publications))
		}
		for round, one := range result.Publications {
			if want := fmt.Sprintf("guest-2mib-%d", round); one.VM != want || one.Round != round {
				t.Fatalf("publication %d is %s of round %d, want %s", round, one.VM, one.Round, want)
			}
			publisher := one.Fills[0]
			kept := uint64(0)
			for _, fills := range one.Fills[1:] {
				kept += fills.Kept
			}
			if publisher.FromPublications != 17 || publisher.Kept != 17 || publisher.Sent != 85 ||
				publisher.Dropped != ([len(publisher.Dropped)]uint64{}) || kept != 85 {
				t.Fatalf("publication %d's fills came to %+v on the publisher, and the others kept %d; want 17 "+
					"windows, one stripe of each kept and five sent and kept", round, publisher, kept)
			}
		}
	})
}

// A publishing run that times faults publishes the guest they read from the
// third node and publishes each guest it names alone. It then times a chain of
// faults on the publisher and on the reader with nothing publishing, which
// takes all its hops, 25 ms apart, and follows the guest's links across the
// pager it takes every 128 hops. Then it times a chain beside one more
// publication of each guest, which ends with the publication.
func TestAPublishingRunTimesFaultsBesideItsPublications(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		nodes := simNodes(t, ctx, runtime, nil)
		result, _, err := drive(ctx, nodes, driveConfig{pages: map[uint64]uint64{}, code: "4+2", lost: 3,
			publishes: 1, publishPages: []uint64{16, 32}, faultPages: 256, faultHops: 200, seed: 1})
		if err != nil {
			t.Fatal(err)
		}
		if result.Victim == nil || result.Victim.Fill.FromPublications != 257 {
			t.Fatalf("the faults' guest was published as %+v, want its 256 pages' windows and its segment", result.Victim)
		}
		victim := mustGuest(t, "victim-2mib", checkpoint.PageSize2MiB, 256)
		var got []string
		for _, c := range result.IdleFaults {
			linked := true
			for at := 1; at < len(c.Units); at++ {
				linked = linked && c.Units[at] == uint64(victim.nextPage[c.Units[at-1]])
			}
			got = append(got, fmt.Sprintf("idle on %d: %d hops, linked %v, paced %v, wrong %d, failed %d", c.Node,
				c.Hops, linked, c.Seconds >= 5, c.Wrong, c.Failed))
		}
		for _, one := range result.Publications {
			got = append(got, fmt.Sprintf("%s: %d windows, dropped %v", one.VM, one.Fills[0].FromPublications,
				one.Fills[0].Dropped))
			for _, c := range one.Faults {
				got = append(got, fmt.Sprintf("beside on %d: ended early %v, wrong %d, failed %d", c.Node,
					c.Hops < 256, c.Wrong, c.Failed))
			}
		}
		want := []string{
			"idle on 0: 200 hops, linked true, paced true, wrong 0, failed 0",
			"idle on 1: 200 hops, linked true, paced true, wrong 0, failed 0",
			"guest-2mib-16-0: 17 windows, dropped [0 0 0 0 0 0 0 0 0]",
			"guest-2mib-32-0: 33 windows, dropped [0 0 0 0 0 0 0 0 0]",
			"guest-2mib-16-faults: 17 windows, dropped [0 0 0 0 0 0 0 0 0]",
			"beside on 0: ended early true, wrong 0, failed 0",
			"beside on 1: ended early true, wrong 0, failed 0",
			"guest-2mib-32-faults: 33 windows, dropped [0 0 0 0 0 0 0 0 0]",
			"beside on 0: ended early true, wrong 0, failed 0",
			"beside on 1: ended early true, wrong 0, failed 0",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("the run did\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}

// A stop that comes before its read ends the read's chain before its first
// hop.
func TestAStopBeforeItsReadEndsTheChainAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		nodes := simNodes(t, ctx, runtime, nil)
		if err := followAll(ctx, nodes, "4+2"); err != nil {
			t.Fatal(err)
		}
		g := guestRequest{VM: "guest", PageSize: checkpoint.PageSize2MiB, Pages: 16}
		published, err := nodes[0].publish(ctx, g)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := nodes[1].stop(ctx, stopRequest{Name: "early"}); err != nil {
			t.Fatal(err)
		}
		got, err := nodes[1].read(ctx, readRequest{Guest: g, Sequence: published.Sequence, Source: sourceCluster,
			Access: access{Pattern: patternChain, Unit: unitFault, Concurrency: 1, Reads: 16, Seed: 1}, Stop: "early"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Latencies) != 0 || len(got.Units) != 0 {
			t.Fatalf("the stopped chain took %d hops, want none", len(got.Latencies))
		}
		// A second stop of the same read is no error.
		if _, err := nodes[1].stop(ctx, stopRequest{Name: "early"}); err != nil {
			t.Fatal(err)
		}
	})
}

// A stop part way through a chain ends it before its next hop, with the hops
// it took.
func TestAStopEndsAChainAtItsNextHop(t *testing.T) {
	g := mustGuest(t, "guest-2m", checkpoint.PageSize2MiB, 64)
	stop := make(chan struct{})
	hops := 0
	read := faithful(g)
	got, err := walk(t.Context(), g, access{Pattern: patternChain, Unit: unitPage, Concurrency: 1, Reads: 16, Seed: 2},
		func(ctx context.Context, offset uint64, dst []byte) error {
			if hops++; hops == 3 {
				close(stop)
			}
			return read(ctx, offset, dst)
		}, stop)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Units) != 3 || len(got.Latencies) != 3 || got.Wrong != 0 {
		t.Fatalf("the chain took %d hops, timed %d and read %d wrong, want 3, 3 and none", len(got.Units),
			len(got.Latencies), got.Wrong)
	}
}

// A walk reads the same chain of pages from the regional bucket, the cluster
// and a hot tier its publication filled, and reads a cold hot tier, which
// fills it.
func TestAWalkReadsEachSourceAndFillsAColdHotTier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		hot := runtime.NewObjectStore("hot", sim.ObjectStoreConfig{GetLatency: time.Millisecond,
			HeadLatency: time.Millisecond / 2, PutLatency: 2 * time.Millisecond})
		nodes := simNodes(t, ctx, runtime, hot)
		result, err := walkRun(ctx, nodes, walkConfig{pages: 16, pages4K: 4096, reads: 8, rounds: 1, code: "4+2",
			seed: 1})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range slices.Concat(result.Cold, result.Cases) {
			got = append(got, fmt.Sprintf("%d %s: %d gets, %d hot gets, %d hot puts, %d hits, %d misses, wrong %d",
				c.PageBytes, c.Source, c.StoreGets, c.HotGets, c.HotPuts, c.Hot.Hits, c.Hot.Misses, c.Wrong))
		}
		// A cold hot tier misses the index, the page table and each of the
		// eight pages, reads them from the regional bucket, and puts three
		// objects in the hot tier behind them, which takes two more reads of
		// the regional bucket. A warm one serves all ten. The cluster asks the
		// regional bucket for the index alone.
		want := []string{
			"2097152 hot: 12 gets, 10 hot gets, 3 hot puts, 0 hits, 10 misses, wrong 0",
			"4096 hot: 12 gets, 10 hot gets, 3 hot puts, 0 hits, 10 misses, wrong 0",
			"2097152 cluster: 1 gets, 0 hot gets, 0 hot puts, 0 hits, 0 misses, wrong 0",
			"4096 cluster: 1 gets, 0 hot gets, 0 hot puts, 0 hits, 0 misses, wrong 0",
			"2097152 hot: 0 gets, 10 hot gets, 0 hot puts, 10 hits, 0 misses, wrong 0",
			"4096 hot: 0 gets, 10 hot gets, 0 hot puts, 10 hits, 0 misses, wrong 0",
			"4096 regional: 10 gets, 0 hot gets, 0 hot puts, 0 hits, 0 misses, wrong 0",
			"2097152 regional: 10 gets, 0 hot gets, 0 hot puts, 0 hits, 0 misses, wrong 0",
		}
		if !slices.Equal(got, want) {
			t.Fatalf("the walks read\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}
