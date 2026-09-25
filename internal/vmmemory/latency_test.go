package vmmemory_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmemory"
)

// The fault histogram decomposes the fault counter, so a record citing both can
// be checked against itself. One read of an eight-page memory region loads and maps the
// whole read-ahead run with one backing read and one mapping command; each of
// the eight stores that follow maps a private page over the page it copied from
// and resolves it, revoking nothing.
func TestFaultHistogramDecomposesFaultCount(t *testing.T) {
	f := newConfiguredFixture(t, vmmemory.Config{ResidentPages: 16, LogicalPages: 32, DirtyPages: 16, ReadAheadPages: 8})
	r, m, _ := f.memoryRegion(8)
	access(t, r, m, 0, false)
	for page := range uint64(8) {
		access(t, r, m, page, true)
	}
	stats, err := f.h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Faults != 9 {
		t.Fatalf("served %d faults, wanted 9", stats.Faults)
	}
	for _, item := range []struct {
		name  string
		value vmmemory.Latency
		want  uint64
	}{
		{"fault", stats.Fault, stats.Faults},
		{"mapping", stats.Mapping, 9},
		{"resolve", stats.Resolve, 9},
		{"revoke", stats.Revoke, 0},
		{"load", stats.Load, 1},
	} {
		var summed uint64
		for _, count := range item.value.Buckets {
			summed += count
		}
		if summed != item.value.Count {
			t.Errorf("%s histogram counts %d but its buckets sum to %d", item.name, item.value.Count, summed)
		}
		if item.value.Count != item.want {
			t.Errorf("%s histogram observed %d, wanted %d", item.name, item.value.Count, item.want)
		}
	}
	if stats.Fault.MeanNS() == 0 {
		t.Error("nine served faults summed to no time at all")
	}
}
