package vmmemory

import "fmt"

// SetFlushed installs what this host does with a guest's flush of one of its
// memory regions. The guest is waiting: its flush completes when done is called, with
// nil once what it flushed is durable — which may take a disk checkpoint of the
// memory region's VM first — or with the error that kept it from being made so, which
// the guest reads as an I/O error.
//
// flushed runs on a goroutine of the memory region's session, never on the one reading
// its socket, and should return at once: a checkpoint's seal needs that reader,
// and a flush that has to wait for one keeps done and calls it later. done may
// be called from any goroutine, at most once; a second call is logged and
// ignored. It may also never be called — a host drops the flushes of a VM that
// leaves it, and the guest that made them sends them again from wherever it is
// restored — and nothing waits for it when the session ends.
//
// With nothing installed, every flush is answered at once with success. A
// supervisor sets it once, before any memory region is attached, and clears it by
// installing nil before it stops answering.
func (h *Host) SetFlushed(flushed func(r *MemoryRegion, done func(error))) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushed = flushed
}

// Flush is a guest's flush of this memory region reaching the pager: what a session's
// reader does with a FLUSH request, and what an attacher in this process — the
// simulation, a test's machine — does for its guest's. done completes the
// guest's flush, under the same contract SetFlushed gives the host. A flush is a
// disk's, so a flush of a RAM memory region is answered with an error at once.
func (r *MemoryRegion) Flush(done func(error)) {
	if r.kind != Pmem {
		done(fmt.Errorf("vmmemory: a flush of a %s memory region", r.kind))
		return
	}
	r.host.countFlush()
	r.deliverFlush(done)
}

// deliverFlush hands one counted flush to the host's callback, or answers it at
// once when nothing is installed.
func (r *MemoryRegion) deliverFlush(done func(error)) {
	if flushed := r.host.flushedCallback(); flushed != nil {
		flushed(r, done)
		return
	}
	done(nil)
}

// countFlush counts one flush request as the session's reader takes it.
func (h *Host) countFlush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stats.Flushes++
}

// flushedCallback is what SetFlushed last installed.
func (h *Host) flushedCallback() func(*MemoryRegion, func(error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.flushed
}
