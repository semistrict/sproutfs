package peer

import (
	"cmp"
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
	// Stripe is a read of the cluster's disk cache that a fault waits on. Its
	// replies are a few stripes each, and a reply leaves a connection in the
	// order its request came, so a stripe on a connection that also carries
	// 2 MiB pages waits for each page ahead of it. Stripe reads have
	// connections and a budget of their own for that reason.
	Stripe
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
	case Stripe:
		return "stripe"
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
func ClassOf(ctx context.Context) Class { return classOr(ctx, Fault) }

// classOr is the class ctx names, or otherwise when it names none.
func classOr(ctx context.Context, otherwise Class) Class {
	class, ok := ctx.Value(classKey{}).(Class)
	if !ok || class >= classes {
		return otherwise
	}
	return class
}

// waitedOn reports a class something waits on now: a fault, or a read of
// stripes for one. While one is in flight the background budget shrinks.
func (c Class) waitedOn() bool { return c == Fault || c == Stripe }

// Budgets is bytes per class: what one remote host's requests of each class
// may hold at once. A server bounds what each peer holds of it, and a peer
// asks for no more than the server said it may.
type Budgets struct {
	Fault, BulkRead, BulkWrite, Stripe int64
}

// DefaultBudgets is what a server holds for each peer by default: a few 2 MiB
// faults at once, and room for the stream, the cache's fills and its reads to
// keep a link busy.
var DefaultBudgets = Budgets{Fault: 8 << 20, BulkRead: 16 << 20, BulkWrite: 16 << 20, Stripe: 16 << 20}

// Of is the budget of class.
func (b Budgets) Of(class Class) int64 {
	switch class {
	case BulkRead:
		return b.BulkRead
	case BulkWrite:
		return b.BulkWrite
	case Stripe:
		return b.Stripe
	default:
		return b.Fault
	}
}

// orDefault is the default budgets for none, and otherwise b with a stripe
// budget it leaves at zero taken from its fault budget: stripe reads were
// faults before they had a class of their own.
func (b Budgets) orDefault() Budgets {
	if b == (Budgets{}) {
		return DefaultBudgets
	}
	b.Stripe = cmp.Or(b.Stripe, b.Fault)
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
