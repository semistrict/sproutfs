package peer

import (
	"context"
	"fmt"
)

// Class is the kind of traffic a request is, which decides the connections it
// goes over and the budget it is counted against at the server. A guest waiting
// on a page must never queue behind a stream that fetches pages nothing waits
// on yet, so the two never share a connection or a budget.
type Class uint8

const (
	// Fault is a request something waits on now: a guest's page fault, a claim,
	// a probe, a presence check, a stripe read for a fault.
	Fault Class = iota
	// BulkRead is a read nothing waits on at once: the post-copy stream, a
	// resident listing, a pull or a read-ahead.
	BulkRead
	// BulkWrite is a write of data: a keep that fills the cluster's cache, or a
	// repair.
	BulkWrite
	// classes is how many there are.
	classes
)

func (c Class) String() string {
	switch c {
	case Fault:
		return "fault"
	case BulkRead:
		return "bulk-read"
	case BulkWrite:
		return "bulk-write"
	}
	return fmt.Sprintf("class-%d", c)
}

type classKey struct{}

// WithClass marks the requests made under ctx as class. A request whose
// context names none is a Fault: something is waiting on it.
func WithClass(ctx context.Context, class Class) context.Context {
	return context.WithValue(ctx, classKey{}, class)
}

// WithStream marks the requests made under ctx as the post-copy stream's,
// which fetches pages behind a running guest: BulkRead.
func WithStream(ctx context.Context) context.Context { return WithClass(ctx, BulkRead) }

// ClassOf reports the class of the requests made under ctx.
func ClassOf(ctx context.Context) Class {
	class, ok := ctx.Value(classKey{}).(Class)
	if !ok || class >= classes {
		return Fault
	}
	return class
}

// Budgets is bytes per class: what one remote host's requests of each class
// may hold at once. A server bounds what each peer holds of it, and a peer
// asks for no more than the server said it may.
type Budgets struct {
	Fault, BulkRead, BulkWrite int64
}

// DefaultBudgets is what a server holds for each peer by default: a few 2 MiB
// faults at once, and room for the stream and the cache to keep a link busy.
var DefaultBudgets = Budgets{Fault: 8 << 20, BulkRead: 16 << 20, BulkWrite: 16 << 20}

// Of is the budget of class.
func (b Budgets) Of(class Class) int64 {
	switch class {
	case BulkRead:
		return b.BulkRead
	case BulkWrite:
		return b.BulkWrite
	default:
		return b.Fault
	}
}

func (b Budgets) orDefault() Budgets {
	if b == (Budgets{}) {
		return DefaultBudgets
	}
	return b
}

func (b Budgets) valid(largest int64) bool {
	for class := range classes {
		if b.Of(class) < largest {
			return false
		}
	}
	return true
}
