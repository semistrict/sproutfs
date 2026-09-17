package latency_test

import (
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/vmmemory/internal/latency"
)

// The scale is fixed, so a record written by one run is read by another without
// carrying its boundaries. Bucket 0 is under a microsecond and bucket i is
// [2^(i-1), 2^i) microseconds, with the last bucket unbounded above.
func TestBucketBoundaries(t *testing.T) {
	for _, item := range []struct {
		of   time.Duration
		want int
	}{
		{0, 0},
		{999 * time.Nanosecond, 0},
		{time.Microsecond, 1},
		{2 * time.Microsecond, 2},
		{3 * time.Microsecond, 2},
		{4 * time.Microsecond, 3},
		{time.Millisecond, 10},
		{2 * time.Millisecond, 11},
		{time.Second, 20},
		{time.Hour, latency.BucketCount - 1},
	} {
		if got := latency.BucketOf(item.of); got != item.want {
			t.Errorf("%s fell in bucket %d, wanted %d", item.of, got, item.want)
		}
	}
	if got := latency.BucketUpperNS(0); got != uint64(time.Microsecond)-1 {
		t.Errorf("bucket 0 ends at %d ns, wanted just under a microsecond", got)
	}
	if got := latency.BucketUpperNS(10); got != 1024*uint64(time.Microsecond) {
		t.Errorf("bucket 10 ends at %d ns, wanted 1,024 microseconds", got)
	}
}

// A snapshot decomposes the observations it summarizes: its count is the sum
// of its buckets, its total is the summed duration, and its maximum is the
// largest single observation.
func TestSnapshotDecomposesObservations(t *testing.T) {
	var h latency.Histogram
	for _, d := range []time.Duration{500 * time.Nanosecond, time.Microsecond, time.Millisecond, 2 * time.Millisecond} {
		h.Observe(d)
	}
	got := h.Snapshot()
	if got.Count != 4 {
		t.Fatalf("observed %d, wanted 4", got.Count)
	}
	var summed uint64
	for _, count := range got.Buckets {
		summed += count
	}
	if summed != got.Count {
		t.Errorf("the buckets sum to %d but the count is %d", summed, got.Count)
	}
	want := uint64(500*time.Nanosecond + time.Microsecond + time.Millisecond + 2*time.Millisecond)
	if got.TotalNS != want {
		t.Errorf("totalled %d ns, wanted %d", got.TotalNS, want)
	}
	if got.MaxNS != uint64(2*time.Millisecond) {
		t.Errorf("the largest observation is %d ns, wanted %d", got.MaxNS, 2*time.Millisecond)
	}
	if got.MeanNS() != want/4 {
		t.Errorf("the mean is %d ns, wanted %d", got.MeanNS(), want/4)
	}
	if got.Buckets[0] != 1 || got.Buckets[1] != 1 || got.Buckets[10] != 1 || got.Buckets[11] != 1 {
		t.Errorf("buckets 0, 1, 10 and 11 hold %d, %d, %d and %d, wanted one each",
			got.Buckets[0], got.Buckets[1], got.Buckets[10], got.Buckets[11])
	}
}

// A non-monotonic reading is charged to the smallest bucket rather than
// wrapping the unsigned total.
func TestNegativeObservationIsCharacterizedAsZero(t *testing.T) {
	var h latency.Histogram
	h.Observe(-time.Second)
	got := h.Snapshot()
	if got.Count != 1 || got.Buckets[0] != 1 || got.TotalNS != 0 || got.MaxNS != 0 {
		t.Fatalf("a negative observation gave %+v, wanted one observation of nothing in bucket 0", got)
	}
}
