package vmmemory

import (
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/vmmemory/internal/zirconvm"
)

// Every backing read of a fault or a prefetch is the answer to a READ
// request to a page source, as a Zircon VMO's missing pages are
// (internal/zirconvm, pagesource.go and pagerproxy.go). The source batches
// requests that overlap and wakes every request waiting on one when its range
// is supplied or failed. The pager is in this process, so the fault or
// prefetch that sends a request answers it: it reads the pages on a goroutine,
// its own or one it starts, and then supplies the range, or fails it where the
// read failed.
//
// There are two kinds of source:
//
//   - An identity root's. A published checkpoint's pages of one volume are an
//     identity root, and a page's identity names its root and its place in it,
//     which is the page's own (plan.identity). A prefetch sends its READ
//     requests to the roots of its pages, so that two prefetches of one
//     identity batch: the later leaves those pages to the earlier. A fault
//     that meets a page a prefetch is reading sends a READ request that waits
//     on the prefetch's, and plans again once the prefetch supplies or fails
//     it. A root's source exists while a request of it is in use
//     (Host.roots).
//   - A memory region's own. A fault sends the READ requests of its own reads
//     there. Only the faults of one window read its pages, one at a time, so
//     they never batch. A prefetch does not see them, as before the requests:
//     a fault's read of its own page is not in flight for prefetches to see,
//     and a prefetch that reads the same page lands it second and drops it
//     (TestAPrefetchedPageAnotherLoadMadeResidentFirstIsDropped).
//
// Which pages a fault reads, which faults prefetch and how many prefetches
// read at once stay the pager's own (faultfirst.go, prefetch.go).

// rootKey names an identity root: the pages one published checkpoint holds of
// one volume.
type rootKey struct {
	ref    control.Ref
	volume string
}

func rootOf(key pageKey) rootKey { return rootKey{ref: key.id.Ref, volume: key.id.Volume} }

// requestSource is a page source and the proxy its requests go to.
type requestSource struct {
	source *zirconvm.PageSource
	proxy  *zirconvm.PagerProxy
}

func newRequestSource() *requestSource {
	proxy := zirconvm.NewPagerProxy(false)
	return &requestSource{source: zirconvm.NewPageSource(proxy), proxy: proxy}
}

// newRequest is a READ request not in use, from the host's pool.
func (h *Host) newRequest() *zirconvm.PageRequest {
	return h.requests.Get().(*zirconvm.PageRequest)
}

// sentRead is a READ request a fault sent to its memory region's source, which
// it answers.
type sentRead struct {
	request        *zirconvm.PageRequest
	offset, length uint64
}

// sendRead sends the READ request of the pages [first, first+count) to this
// memory region's own source, for the caller to answer once it has read them.
// Only the faults of one window read its pages, one at a time, and no fault
// reads after its memory region is detached, so the request is always sent.
func (r *MemoryRegion) sendRead(first, count uint64) sentRead {
	h := r.host
	read := sentRead{request: h.newRequest(), offset: first * h.pageSize, length: count * h.pageSize}
	sent, sentLen, err := r.reads.source.SendPages(read.offset, read.length, read.request)
	if err != zirconvm.ErrShouldWait {
		panic("vmmemory: a fault's read request was refused: " + err.Error())
	}
	// The request was sent, and the length says nothing cut it short.
	if !sent || sentLen != read.length {
		panic("vmmemory: a fault's read request met another read of its memory region's pages")
	}
	return read
}

// answer resolves a read sendRead sent: its pages are supplied, or failed
// where err says the read failed. The request is not waited on after.
func (r *MemoryRegion) answer(read sentRead, err error) {
	if err != nil {
		r.reads.source.OnPagesFailed(read.offset, read.length, zirconvm.ErrIO)
	} else {
		r.reads.source.OnPagesSupplied(read.offset, read.length)
	}
}

// requested runs a fault's backing read of the pages [first, first+count) as
// the answer to a READ request of its own.
func (r *MemoryRegion) requested(first, count uint64, read func() error) error {
	sent := r.sendRead(first, count)
	err := read()
	r.answer(sent, err)
	r.host.requests.Put(sent.request)
	return err
}
