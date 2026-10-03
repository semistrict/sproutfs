package peer

import (
	"context"
	"sync"
)

type sentKey struct{}

// WithSent has sent called once for every request made to a Peer under ctx,
// as soon as that request is on its connection, behind every request sent on
// it before, or has ended without getting there. A caller that keeps several
// requests in flight in an order of its own goes on to the next one there,
// rather than when the reply arrives: the post-copy stream asks for its pages
// one after another and keeps the link full while it does.
func WithSent(ctx context.Context, sent func()) context.Context {
	if sent == nil {
		panic("nil peer sent hook")
	}
	return context.WithValue(ctx, sentKey{}, sent)
}

// sentHook is the hook one request under ctx calls, at most once, and a
// function that does nothing where ctx carries none.
func sentHook(ctx context.Context) func() {
	sent, ok := ctx.Value(sentKey{}).(func())
	if !ok {
		return func() {}
	}
	return sync.OnceFunc(sent)
}
