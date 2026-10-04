package checkpoint

import (
	"context"

	"github.com/semistrict/sproutfs/peer"
)

// A prefetch is a read nothing waits on yet: the rest of a fault's read-ahead
// run, which a pager reads behind the fault once the faulting page is in
// (vmmemory/prefetch.go). It is worth having only if it never makes a read
// something does wait on slower, so every place a read can wait or spend
// something treats it as background work:
//
//   - its requests of peers, of the cluster and of a migration's source, go
//     over the bulk class, on connections of their own and within the host's
//     background budget, so no fault's reply waits behind a prefetch's on a
//     connection and bulk work gives the links to faults;
//   - the page cache gives its loads slots of their own, so a fault never
//     waits for a load slot behind one;
//   - a read of the cluster for it asks no second requests after the delay and
//     never reads the store as a hedge, and its times are left out of the
//     delay, which is an estimate of what a fault's read takes.
//
// A fault that wants a page a prefetch is already reading waits for that read
// rather than reading the page again: the page cache keeps one load per page.

type prefetchKey struct{}

// WithPrefetch marks the reads made under ctx as a prefetch.
func WithPrefetch(ctx context.Context) context.Context {
	return peer.WithClass(context.WithValue(ctx, prefetchKey{}, true), peer.BulkRead)
}

// Prefetching reports whether the reads made under ctx are a prefetch.
func Prefetching(ctx context.Context) bool {
	prefetch, _ := ctx.Value(prefetchKey{}).(bool)
	return prefetch
}
