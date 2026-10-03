package host

import "context"

// RefreshCaches reads the list of caches now, so a test can start from the
// list a host read instead of waiting for its first read.
func (h *Host) RefreshCaches(ctx context.Context) error { return h.caches.Refresh(ctx) }

// CacheDiskReport is the page cache's disk as /status reports it.
var CacheDiskReport = cacheDiskReport
