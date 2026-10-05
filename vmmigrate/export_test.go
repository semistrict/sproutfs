package vmmigrate

import (
	"context"
	"testing"
)

// SetReplySeam installs what every request a peer backing makes runs once it
// has come back from the source and before the backing decides whether to take
// its reply, given the request's context, so a test can close the backing in
// that moment. It is restored when the test ends.
func SetReplySeam(t *testing.T, seam func(ctx context.Context)) {
	previous := replySeam
	replySeam = seam
	t.Cleanup(func() { replySeam = previous })
}
