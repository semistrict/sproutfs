package vmmemory

// SetFlushed installs what this host does with a guest's flush of one of its
// regions. The guest is waiting: its flush completes when done is called, with
// nil once what it flushed is durable — which may take a disk checkpoint of the
// region's VM first — or with the error that kept it from being made so, which
// the guest reads as an I/O error.
//
// flushed runs on a goroutine of the region's session, never on the one reading
// its socket, and should return at once: a checkpoint's seal needs that reader,
// and a flush that has to wait for one keeps done and calls it later. done may
// be called from any goroutine, at most once; a second call is logged and
// ignored. It may also never be called — a host drops the flushes of a VM that
// leaves it, and the guest that made them sends them again from wherever it is
// restored — and nothing waits for it when the session ends.
//
// With nothing installed, every flush is answered at once with success. A
// supervisor sets it once, before any region is attached, and clears it by
// installing nil before it stops answering.
func (h *Host) SetFlushed(flushed func(r *Region, done func(error))) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushed = flushed
}

// countFlush counts one flush request as the session's reader takes it.
func (h *Host) countFlush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stats.Flushes++
}

// flushedCallback is what SetFlushed last installed.
func (h *Host) flushedCallback() func(*Region, func(error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.flushed
}
