// Package latency is latency instrumentation: the live histograms
// the fault path writes without taking the host lock, and the snapshots a
// stats record carries. Nothing the pager does depends on what they hold.
package latency

import (
	"math/bits"
	"sync/atomic"
	"time"
)

// BucketCount is how many fixed log-scale buckets every latency histogram
// carries. Bucket 0 counts observations under one microsecond, and bucket i
// counts the half-open range from 2^(i-1) to 2^i microseconds, so the last
// bucket holds everything from about 4.2 seconds upwards. The scale is fixed
// rather than configurable so records written by different runs are comparable
// without carrying their boundaries.
const BucketCount = 24

// Snapshot is one histogram's snapshot. Count is exactly the sum of Buckets, so
// a record can be checked against the counter it decomposes; TotalNS is the
// summed duration, which is what a mean needs and buckets cannot give.
type Snapshot struct {
	Count   uint64              `json:"count"`
	TotalNS uint64              `json:"total_ns"`
	MaxNS   uint64              `json:"max_ns"`
	Buckets [BucketCount]uint64 `json:"buckets"`
}

// MeanNS is the average observation in nanoseconds, or zero for no observations.
func (l Snapshot) MeanNS() uint64 {
	if l.Count == 0 {
		return 0
	}
	return l.TotalNS / l.Count
}

// BucketUpperNS is the exclusive upper bound of bucket i in nanoseconds. The
// last bucket is unbounded; it reports the largest value the scale
// distinguishes, which is where that bucket begins.
func BucketUpperNS(i int) uint64 {
	if i <= 0 {
		return uint64(time.Microsecond) - 1
	}
	return uint64(time.Microsecond) << uint(min(i, BucketCount-1))
}

// Histogram is the live histogram. It is written on the fault path, so it uses
// atomics rather than the host lock: an idle counter must never contend with
// page transitions. Nothing here changes what the pager does; a snapshot taken
// while faults are in flight simply misses the ones that have not finished.
type Histogram struct {
	totalNS atomic.Uint64
	maxNS   atomic.Uint64
	buckets [BucketCount]atomic.Uint64
}

func (l *Histogram) Observe(d time.Duration) {
	if d < 0 {
		// A non-monotonic reading is charged to the smallest bucket rather than
		// wrapping the unsigned total.
		d = 0
	}
	l.totalNS.Add(uint64(d))
	for highest := l.maxNS.Load(); uint64(d) > highest; highest = l.maxNS.Load() {
		if l.maxNS.CompareAndSwap(highest, uint64(d)) {
			break
		}
	}
	l.buckets[BucketOf(d)].Add(1)
}

func (l *Histogram) Snapshot() Snapshot {
	out := Snapshot{TotalNS: l.totalNS.Load(), MaxNS: l.maxNS.Load()}
	for i := range l.buckets {
		count := l.buckets[i].Load()
		out.Buckets[i] = count
		out.Count += count
	}
	return out
}

// BucketOf is the bucket one duration falls in, which is what a reader
// rendering a histogram needs to label its columns.
func BucketOf(d time.Duration) int {
	micros := uint64(d / time.Microsecond)
	if micros == 0 {
		return 0
	}
	return min(bits.Len64(micros), BucketCount-1)
}

// Merge adds another snapshot's observations to this one, which is how
// histograms of several memory regions become one record.
func (l Snapshot) Merge(other Snapshot) Snapshot {
	l.Count += other.Count
	l.TotalNS += other.TotalNS
	l.MaxNS = max(l.MaxNS, other.MaxNS)
	for i := range l.Buckets {
		l.Buckets[i] += other.Buckets[i]
	}
	return l
}

// QuantileUpperNS is the upper bound of the bucket the quantile q falls in, or
// zero for no observations. A histogram cannot report a quantile more precisely
// than its buckets.
func (l Snapshot) QuantileUpperNS(q float64) uint64 {
	if l.Count == 0 {
		return 0
	}
	want := uint64(float64(l.Count) * q)
	var seen uint64
	for i, count := range l.Buckets {
		seen += count
		if seen > want {
			return BucketUpperNS(i)
		}
	}
	return BucketUpperNS(BucketCount - 1)
}
