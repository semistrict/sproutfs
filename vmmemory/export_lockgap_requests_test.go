package vmmemory

import "testing"

// SetPrefetchSentSeam runs seam in every prefetch once it has sent a request,
// with the host's lock held, handing it a supply of that request's pages as a
// fault of another region makes one, until the test ends.
func SetPrefetchSentSeam(t *testing.T, seam func(supply func())) {
	previous := prefetchSentSeam
	prefetchSentSeam = seam
	t.Cleanup(func() { prefetchSentSeam = previous })
}
