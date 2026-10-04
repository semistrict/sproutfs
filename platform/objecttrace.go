package platform

import (
	"context"
	"sync"
	"time"
)

// ObjectCall is one object-store call an ObjectTrace saw: the operation, the
// key, when it began and how long it took.
type ObjectCall struct {
	Operation ObjectOperation
	Key       ObjectKey
	// Conditional reports a Put with a condition: a create if absent, or a
	// compare-and-set on the object's ETag.
	Conditional bool
	Began       time.Time
	Took        time.Duration
	Failed      bool
}

// objectTraceCalls bounds the calls one trace keeps. A host's start makes a
// few dozen. Work that inherits the context and outlives the start, such as a
// post-copy stream, may make many more, and those are counted in Dropped.
const objectTraceCalls = 1024

// ObjectTrace lists the object-store calls made under one context, in the
// order they ended. A host traces each VM start this way, so the start's log
// line says how much of it was the store. The zero value is ready and safe for
// concurrent use.
type ObjectTrace struct {
	mu      sync.Mutex
	calls   []ObjectCall
	dropped int
	closed  bool
}

// objectTraceKey carries the trace one unit of work's calls are listed in.
type objectTraceKey struct{}

// WithObjectTrace lists every object-store call made under the returned
// context in trace, as well as counting it in the store's totals. A nil trace
// returns ctx unchanged.
func WithObjectTrace(ctx context.Context, trace *ObjectTrace) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, objectTraceKey{}, trace)
}

func objectTraceOf(ctx context.Context) *ObjectTrace {
	trace, _ := ctx.Value(objectTraceKey{}).(*ObjectTrace)
	return trace
}

// add records one call, unless the trace is closed or full.
func (t *ObjectTrace) add(call ObjectCall) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if len(t.calls) == objectTraceCalls {
		t.dropped++
		return
	}
	t.calls = append(t.calls, call)
}

// Close ends the trace: calls made afterwards under its context are not
// listed. Work that inherited the context and runs on is then not mistaken
// for part of the work that was traced.
func (t *ObjectTrace) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
}

// Calls reports the calls listed so far, and how many more there were than
// the trace keeps.
func (t *ObjectTrace) Calls() (calls []ObjectCall, dropped int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]ObjectCall(nil), t.calls...), t.dropped
}
