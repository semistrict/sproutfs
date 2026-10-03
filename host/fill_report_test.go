package host_test

import (
	"maps"
	"slices"
	"testing"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/rank"
)

// /status reports what a host's fills did under every reason a fill drops
// stripes for, by the names /metrics lists, and nothing for a host that keeps
// no cache disk.
func TestTheFillsAreReportedByEveryReasonTheyDropFor(t *testing.T) {
	var fill checkpoint.FillStats
	for _, reason := range checkpoint.DropReasons() {
		fill.Dropped[reason] = uint64(reason) + 1
	}
	fill.FromReads, fill.Sent, fill.Kept, fill.Refused, fill.QueueBytes = 1, 2, 3, 4, 5
	report := host.CacheFillReport(checkpoint.DiskStats{Identity: rank.Identity{1}}, fill)
	if report == nil {
		t.Fatal("a host that keeps a cache disk reports no fills")
	}
	if names := slices.Sorted(maps.Keys(report.Dropped)); !slices.Equal(names, slices.Sorted(slices.Values(hostapi.FillDropReasons))) {
		t.Fatalf("the fills are reported under %v, and /metrics lists %v", names, hostapi.FillDropReasons)
	}
	for at, name := range hostapi.FillDropReasons {
		if report.Dropped[name] != uint64(at)+1 {
			t.Fatalf("the fills report %d stripes dropped for %s, want %d", report.Dropped[name], name, at+1)
		}
	}
	if report.FromReads != 1 || report.Sent != 2 || report.Kept != 3 || report.Refused != 4 || report.QueueBytes != 5 {
		t.Fatalf("the fills are reported as %+v, want 1 from reads, 2 sent, 3 kept, 4 refused and a queue of 5",
			*report)
	}
	if report := host.CacheFillReport(checkpoint.DiskStats{}, fill); report != nil {
		t.Fatalf("a host that keeps no cache disk reports fills %+v", *report)
	}
}
