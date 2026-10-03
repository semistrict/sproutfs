package main

import (
	"encoding/json"
	"fmt"
	"math/bits"
	"slices"
	"time"
)

// subBuckets is how many buckets each doubling of latency is split into.
// A percentile read from the histogram is within 1/32 of the true value.
const subBuckets = 32

// histogram counts latencies in log-linear buckets. Histograms of clients
// that ran the same case at once add up.
type histogram struct {
	counts map[int]uint64
	total  uint64
	sum    time.Duration
	max    time.Duration
}

func bucketOf(d time.Duration) int {
	v := uint64(max(d, 0))
	if v < 2*subBuckets {
		return int(v)
	}
	shift := bits.Len64(v) - 6 // v>>shift is in [32, 64)
	return shift*subBuckets + int(v>>shift)
}

// bucketLower is the smallest latency bucket i holds.
func bucketLower(i int) time.Duration {
	if i < 2*subBuckets {
		return time.Duration(i)
	}
	shift := i/subBuckets - 1
	return time.Duration(uint64(i%subBuckets+subBuckets) << shift)
}

func (h *histogram) observe(d time.Duration) { h.observeN(d, 1) }

// observeN counts n observations of d.
func (h *histogram) observeN(d time.Duration, n uint64) {
	if h.counts == nil {
		h.counts = make(map[int]uint64)
	}
	h.counts[bucketOf(d)] += n
	h.total += n
	h.sum += d * time.Duration(n)
	h.max = max(h.max, d)
}

func (h *histogram) add(o histogram) {
	if h.counts == nil {
		h.counts = make(map[int]uint64)
	}
	for i, n := range o.counts {
		h.counts[i] += n
	}
	h.total += o.total
	h.sum += o.sum
	h.max = max(h.max, o.max)
}

// quantile is the latency at or below which a fraction q of the
// observations fall: the middle of the bucket holding that rank, at most
// the largest observation.
func (h *histogram) quantile(q float64) time.Duration {
	if h.total == 0 {
		return 0
	}
	want := uint64(q*float64(h.total) + 0.999999)
	want = min(max(want, 1), h.total)
	var seen uint64
	for _, i := range h.sorted() {
		seen += h.counts[i]
		if seen >= want {
			lo, hi := bucketLower(i), bucketLower(i+1)
			return min(lo+(hi-lo)/2, h.max)
		}
	}
	return h.max
}

func (h *histogram) sorted() []int {
	out := make([]int, 0, len(h.counts))
	for i := range h.counts {
		out = append(out, i)
	}
	slices.Sort(out)
	return out
}

// shapeBucket is one doubling of latency: the observations from From up to
// To.
type shapeBucket struct {
	From  time.Duration `json:"from_ns"`
	To    time.Duration `json:"to_ns"`
	Count uint64        `json:"count"`
}

// shape is the histogram coarsened to one bucket per doubling, from the
// first doubling that holds anything to the last.
func (h *histogram) shape() []shapeBucket {
	var out []shapeBucket
	for _, i := range h.sorted() {
		var from time.Duration
		if lo := bucketLower(i); lo > 0 {
			from = time.Duration(1) << (bits.Len64(uint64(lo)) - 1)
		}
		if len(out) > 0 && out[len(out)-1].From == from {
			out[len(out)-1].Count += h.counts[i]
			continue
		}
		// Keep the doublings between two that hold something, so the shape
		// shows its gaps.
		for len(out) > 0 && out[len(out)-1].To < from {
			last := out[len(out)-1].To
			out = append(out, shapeBucket{From: last, To: 2 * last})
		}
		to := max(2*from, 1)
		out = append(out, shapeBucket{From: from, To: to, Count: h.counts[i]})
	}
	return out
}

// histogramJSON is the histogram as a record keeps it: its buckets as pairs
// of the bucket's lower bound in nanoseconds and its count.
type histogramJSON struct {
	Count   uint64      `json:"count"`
	SumNS   int64       `json:"sum_ns"`
	MaxNS   int64       `json:"max_ns"`
	Buckets [][2]uint64 `json:"buckets"`
}

func (h histogram) MarshalJSON() ([]byte, error) {
	out := histogramJSON{Count: h.total, SumNS: int64(h.sum), MaxNS: int64(h.max), Buckets: [][2]uint64{}}
	for _, i := range h.sorted() {
		out.Buckets = append(out.Buckets, [2]uint64{uint64(bucketLower(i)), h.counts[i]})
	}
	return json.Marshal(out)
}

func (h *histogram) UnmarshalJSON(b []byte) error {
	var in histogramJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	*h = histogram{counts: make(map[int]uint64), sum: time.Duration(in.SumNS), max: time.Duration(in.MaxNS)}
	for _, pair := range in.Buckets {
		i := bucketOf(time.Duration(pair[0]))
		if bucketLower(i) != time.Duration(pair[0]) {
			return fmt.Errorf("histogram bucket at %d ns is not a bucket boundary", pair[0])
		}
		h.counts[i] += pair[1]
		h.total += pair[1]
	}
	if h.total != in.Count {
		return fmt.Errorf("histogram buckets hold %d observations, and its count says %d", h.total, in.Count)
	}
	return nil
}
