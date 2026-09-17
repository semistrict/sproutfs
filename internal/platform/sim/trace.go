package sim

import (
	"fmt"
	"hash/fnv"
	"io"
	"sync"
	"time"
)

type Event struct {
	Sequence  uint64
	At        time.Time
	Kind      string
	Resource  string
	Operation string
	Outcome   string
	Bytes     int
	LocalID   uint64
}

type Trace struct {
	mu     sync.Mutex
	next   uint64
	events []Event
}

func newTrace() *Trace { return &Trace{} }

func (t *Trace) record(event Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	event.Sequence = t.next
	event.At = time.Now()
	t.events = append(t.events, event)
}

// Record adds one event of a harness's own to the trace, so that what a
// campaign did to the world appears in the same sequence as what the adapters
// did about it: a fault that begins is an event between the operations it
// perturbs, and a run that fails prints both halves of the story. Sequence and
// At are assigned here, as they are for an adapter's own events.
//
// Nothing in production code records: every event a recording compares comes
// from an adapter, so a campaign that traces its faults changes no recording
// but its own.
func (t *Trace) Record(event Event) { t.record(event) }

func (t *Trace) Events() []Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Event(nil), t.events...)
}

func (t *Trace) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next = 0
	t.events = nil
}

// Fingerprint is the digest of everything the simulated dependencies of a run
// did: FoundationDB's unseed, printed after every test and compared against a
// sample of repeated seeds. Two runs of one seed whose adapters did different
// work, in a different order within one resource, at different simulated
// instants, or with different fault-injection sites firing, have different
// fingerprints.
//
// Events are combined by addition rather than concatenation, so the digest does
// not depend on the order two independent resources happened to record in; the
// per-resource ordinal keeps it sensitive to the order one resource saw its own
// operations. It holds for a harness that controls completion order — the
// scheduled scenarios — and is the strongest thing a seed can promise.
//
// It sees exactly what an event carries, which is not everything an operation
// did: a trace event names the resource, the operation, its outcome and its
// byte count, so two writes of one size to different offsets of one file are
// the same event and digest alike. Bytes are compared against the models, not
// here.
func (t *Trace) Fingerprint() uint64 {
	return t.fingerprint(nil, func(h io.Writer, event Event, ordinal uint64) {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d",
			event.Kind, event.Resource, event.Operation, event.Outcome,
			event.Bytes, event.LocalID, event.At.UnixNano(), ordinal)
	})
}

// WorkFingerprint digests which resource did which operation, to what outcome
// and over how many bytes, and nothing else: not the order, not the simulated
// instant, not the adapter's own operation numbering. It is what a campaign
// that does not control completion order can promise — an uncontrolled race
// between two callers of one dependency decides which of them is numbered
// first, and how much simulated time their attempts cost, and the seed does not.
//
// keep selects the events to digest, for a campaign that has to name a class
// its own concurrency decides rather than its seed. Excluding a class is a
// statement about the campaign: the caller that excludes one owes a reason and
// a bound on what it hides. nil keeps every event.
func (t *Trace) WorkFingerprint(keep func(Event) bool) uint64 {
	return t.fingerprint(keep, func(h io.Writer, event Event, _ uint64) {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%d",
			event.Kind, event.Resource, event.Operation, event.Outcome, event.Bytes)
	})
}

func (t *Trace) fingerprint(keep func(Event) bool, write func(io.Writer, Event, uint64)) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var sum uint64
	ordinals := make(map[string]uint64, len(t.events))
	for _, event := range t.events {
		if keep != nil && !keep(event) {
			continue
		}
		ordinals[event.Resource]++
		h := fnv.New64a()
		write(h, event, ordinals[event.Resource])
		sum += mix64(h.Sum64())
	}
	return mix64(sum)
}

// Fingerprint digests this runtime's whole trace. See Trace.Fingerprint.
func (r *Runtime) Fingerprint() uint64 { return r.trace.Fingerprint() }

// WorkFingerprint digests the work in this runtime's trace. See
// Trace.WorkFingerprint.
func (r *Runtime) WorkFingerprint(keep func(Event) bool) uint64 {
	return r.trace.WorkFingerprint(keep)
}
