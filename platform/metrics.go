package platform

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/internal/latency"
)

// ErrNoObjectStore reports a metered store asked to wrap nothing.
var ErrNoObjectStore = errors.New("platform: no object store to meter")

// ObjectOperation names one of the five calls an ObjectStore serves. It is the
// unit every count below is kept per.
type ObjectOperation int

const (
	HeadOperation ObjectOperation = iota
	GetOperation
	PutOperation
	DeleteOperation
	ListOperation
	// objectOperations bounds the array a meter keeps.
	objectOperations
)

func (o ObjectOperation) String() string {
	switch o {
	case HeadOperation:
		return "head"
	case GetOperation:
		return "get"
	case PutOperation:
		return "put"
	case DeleteOperation:
		return "delete"
	case ListOperation:
		return "list"
	default:
		return "unknown"
	}
}

// ObjectCount is what one operation did: every call made, the ones that
// reported an error, and the object bytes the call moved. Only Get and Put move
// bytes; the other three leave Bytes zero.
//
// A Get's bytes are what the store said it would return, which is the object or
// the requested range, counted when the call succeeds rather than when the
// caller finishes reading the body. A Put's are the size it declared.
type ObjectCount struct {
	Calls    int64 `json:"calls"`
	Failures int64 `json:"failures"`
	Bytes    int64 `json:"bytes"`
}

// Add returns the two counts summed.
func (c ObjectCount) Add(other ObjectCount) ObjectCount {
	return ObjectCount{Calls: c.Calls + other.Calls, Failures: c.Failures + other.Failures,
		Bytes: c.Bytes + other.Bytes}
}

// ObjectTraffic is one meter's whole tally, by operation.
type ObjectTraffic struct {
	Head   ObjectCount `json:"head"`
	Get    ObjectCount `json:"get"`
	Put    ObjectCount `json:"put"`
	Delete ObjectCount `json:"delete"`
	List   ObjectCount `json:"list"`
}

// Total sums every operation, which is the whole traffic one meter saw.
func (t ObjectTraffic) Total() ObjectCount {
	return t.Head.Add(t.Get).Add(t.Put).Add(t.Delete).Add(t.List)
}

// counter is one operation's three tallies.
type counter struct {
	calls    atomic.Int64
	failures atomic.Int64
	bytes    atomic.Int64
}

func (c *counter) count() ObjectCount {
	return ObjectCount{Calls: c.calls.Load(), Failures: c.failures.Load(), Bytes: c.bytes.Load()}
}

// ObjectMeter accumulates object-store calls. The zero value is ready and it is
// safe for concurrent use, which is what lets one meter belong to a checkpoint
// whose uploads run on many goroutines at once.
type ObjectMeter struct {
	counters [objectOperations]counter
}

// record tallies one completed call. bytes is ignored when the call failed:
// nothing moved.
func (m *ObjectMeter) record(operation ObjectOperation, bytes int64, err error) {
	if m == nil || operation < 0 || operation >= objectOperations {
		return
	}
	c := &m.counters[operation]
	c.calls.Add(1)
	if err != nil {
		c.failures.Add(1)
		return
	}
	if bytes > 0 {
		c.bytes.Add(bytes)
	}
}

// Traffic reports what this meter has seen. It is a snapshot: the counts are
// read one after another, so a tally taken while calls are in flight is
// self-consistent per operation rather than across all five.
func (m *ObjectMeter) Traffic() ObjectTraffic {
	if m == nil {
		return ObjectTraffic{}
	}
	return ObjectTraffic{
		Head:   m.counters[HeadOperation].count(),
		Get:    m.counters[GetOperation].count(),
		Put:    m.counters[PutOperation].count(),
		Delete: m.counters[DeleteOperation].count(),
		List:   m.counters[ListOperation].count(),
	}
}

// meterContextKey carries the meter one unit of work is attributed to.
type meterContextKey struct{}

// WithObjectMeter attributes every object-store call made under the returned
// context to meter, in addition to the store's own totals. It is how one
// checkpoint's traffic is told apart from the traffic of the checkpoints of
// other VMs running beside it: the calls inherit the context, wherever they are
// made from.
//
// A nil meter returns ctx unchanged.
func WithObjectMeter(ctx context.Context, meter *ObjectMeter) context.Context {
	if meter == nil {
		return ctx
	}
	return context.WithValue(ctx, meterContextKey{}, meter)
}

// objectMeterOf is the meter a context attributes its calls to, nil for one
// that attributes them to nothing but the store's totals.
func objectMeterOf(ctx context.Context) *ObjectMeter {
	meter, _ := ctx.Value(meterContextKey{}).(*ObjectMeter)
	return meter
}

// MeteredObjectStore counts what passes through an ObjectStore without changing
// what it does: every call is tallied against the store's own totals and
// against the meter its context carries, and then handed to the store beneath.
//
// It is the whole of the host's object-store accounting: wrapping the store
// once, where it is built, counts the control records, the indexes and the
// checkpoint pages alike, because all three go through the same seam.
type MeteredObjectStore struct {
	store  ObjectStore
	totals ObjectMeter
	// clock times every call, and latencies holds how long each operation's
	// calls took, failed ones included: a slow store is slow whether or not
	// it then refuses.
	clock     Clock
	latencies [objectOperations]latency.Histogram
}

var _ ObjectStore = (*MeteredObjectStore)(nil)

// NewMeteredObjectStore wraps store, timing its calls on clock; nil is the
// wall clock. A nil store is a programming error and is reported rather than
// deferred to the first call.
func NewMeteredObjectStore(store ObjectStore, clock Clock) (*MeteredObjectStore, error) {
	if store == nil {
		return nil, ErrNoObjectStore
	}
	return &MeteredObjectStore{store: store, clock: ClockOr(clock)}, nil
}

// ObjectLatency is how long each operation's calls took, as histograms.
type ObjectLatency struct {
	Head, Get, Put, Delete, List latency.Snapshot
}

// Latency reports how long every call this store has served took, by
// operation.
func (s *MeteredObjectStore) Latency() ObjectLatency {
	return ObjectLatency{
		Head: s.latencies[HeadOperation].Snapshot(), Get: s.latencies[GetOperation].Snapshot(),
		Put: s.latencies[PutOperation].Snapshot(), Delete: s.latencies[DeleteOperation].Snapshot(),
		List: s.latencies[ListOperation].Snapshot(),
	}
}

// Traffic reports everything this store has served since it was built.
func (s *MeteredObjectStore) Traffic() ObjectTraffic { return s.totals.Traffic() }

// record tallies one call against the totals and against the context's meter.
func (s *MeteredObjectStore) record(ctx context.Context, operation ObjectOperation, began time.Time, bytes int64, err error) {
	s.totals.record(operation, bytes, err)
	objectMeterOf(ctx).record(operation, bytes, err)
	s.latencies[operation].Observe(s.clock.Since(began))
}

func (s *MeteredObjectStore) Head(ctx context.Context, key ObjectKey) (ObjectMetadata, error) {
	began := s.clock.Now()
	metadata, err := s.store.Head(ctx, key)
	s.record(ctx, HeadOperation, began, 0, err)
	return metadata, err
}

func (s *MeteredObjectStore) Get(ctx context.Context, request GetRequest) (GetResult, error) {
	began := s.clock.Now()
	result, err := s.store.Get(ctx, request)
	s.record(ctx, GetOperation, began, result.ContentLength, err)
	return result, err
}

func (s *MeteredObjectStore) Put(ctx context.Context, request PutRequest) (PutResult, error) {
	began := s.clock.Now()
	result, err := s.store.Put(ctx, request)
	s.record(ctx, PutOperation, began, request.Size, err)
	return result, err
}

func (s *MeteredObjectStore) Delete(ctx context.Context, request DeleteRequest) error {
	began := s.clock.Now()
	err := s.store.Delete(ctx, request)
	s.record(ctx, DeleteOperation, began, 0, err)
	return err
}

func (s *MeteredObjectStore) List(ctx context.Context, request ListRequest) (ListResult, error) {
	began := s.clock.Now()
	result, err := s.store.List(ctx, request)
	s.record(ctx, ListOperation, began, 0, err)
	return result, err
}
