// Copyright 2016 The Fuchsia Authors
// Copyright (c) 2014 Travis Geiselbrecht
// Copyright 2017 The Fuchsia Authors
// Ported from zircon/kernel/vm/include/vm/page.h and vm/include/vm/pmm.h
// at fuchsia 90e54e09. MIT licence; see LICENSE in this directory.

package zirconvm

// VmPage is a page of memory that an object holds, Zircon's vm_page_t in its
// OBJECT state: the fields VmCowPages and the page queues keep in it.
//
// Zircon reaches a page's bytes through the physmap at its physical address.
// The pager's pages are slots of memfds (plan: Arena and residency), so here
// a page carries its bytes, which the Pmm that made it hands out.
type VmPage struct {
	// PageQueueNode is the page's queue_node, its backlink (object and
	// pageOffset) and its queue (pageQueue). See pagequeues.go.
	PageQueueNode[*VmPage, *CowPages]

	data []byte

	// shareCount is how many objects other than its owner can reach the page.
	// Only a hidden node shares its pages, and the port has none, so it is 0
	// here except while a compression carries it as metadata.
	shareCount uint32

	// alwaysNeed is the always_need hint: do not reclaim the page.
	alwaysNeed bool

	// dirtyState is the page's VmCowPages::DirtyState.
	dirtyState DirtyState

	// Zircon's pin_count and loaned state are not kept. Pinning is for DMA and
	// loaned pages are for contiguous memory; neither ports (plan: Which
	// Zircon tests port).
}

// QueueNode is the page's part the page queues own.
func (p *VmPage) QueueNode() *PageQueueNode[*VmPage, *CowPages] { return &p.PageQueueNode }

// NewPage is a page over data, which is the page's bytes. A Pmm makes pages
// with it.
func NewPage(data []byte) *VmPage { return &VmPage{data: data} }

// Data is the page's bytes.
func (p *VmPage) Data() []byte { return p.data }

// DirtyState is the dirty state of a page, VmCowPages::DirtyState.
//
// The transitions between the three tracked states can roughly be summarized
// as follows:
//  1. A page starts off as Clean when supplied.
//  2. A write transitions the page from Clean to Dirty.
//  3. A writeback begin moves the Dirty page to AwaitingClean.
//  4. A writeback end moves the AwaitingClean page to Clean.
//  5. A write that comes in while the writeback is in progress does not move
//     the AwaitingClean page back to Dirty, as Zircon's does: the
//     AwaitingClean page stays with the checkpoint and the write gets a Dirty
//     copy (D1, see dirty.go).
type DirtyState uint8

const (
	// Untracked is a page that does not track dirty state, a page of an
	// object no user pager backs.
	Untracked DirtyState = iota
	// Clean is a page whose contents are what was supplied.
	Clean
	// Dirty is a page modified since it was supplied, to be written back.
	Dirty
	// AwaitingClean is a page a writeback is writing back.
	AwaitingClean
	numDirtyStates
)

// dirtyStateBits is VM_PAGE_OBJECT_DIRTY_STATE_BITS.
const dirtyStateMask = 1<<dirtyStateBits - 1

// Zircon's static_assert that a dirty state fits the bits a page keeps it in.
const _ = uint(1<<dirtyStateBits - numDirtyStates)

func (s DirtyState) String() string {
	switch s {
	case Untracked:
		return "Untracked"
	case Clean:
		return "Clean"
	case Dirty:
		return "Dirty"
	case AwaitingClean:
		return "AwaitingClean"
	}
	return "DirtyState(?)"
}

// Pmm is the physical memory manager an object allocates its pages from and
// frees them to, Zircon's pmm_alloc_page, pmm_free_page and
// vm_get_zero_page. The pager's arena takes its place (plan: Arena and
// residency), so here it is an interface.
type Pmm interface {
	// AllocPage is a new page of the pmm's page size, whose bytes are any.
	// Zircon's can fail with ZX_ERR_NO_MEMORY or ask the caller to wait with
	// ZX_ERR_SHOULD_WAIT; an error here fails the operation that needed it.
	AllocPage() (*VmPage, error)
	// FreePage gives a page back. It is in no page queue and no object.
	FreePage(*VmPage)
	// ZeroPage is the one page of zeros every object reads where it holds
	// zeros. It is never written and never freed.
	ZeroPage() *VmPage
}

// Node is what Zircon's VM reaches through globals: Pmm::Node(), its page
// queues and its page compression. Go has no such globals here, so every
// object carries the Node it was made on.
type Node struct {
	pmm         Pmm
	pageSize    uint64
	queues      *VmPageQueues
	compression *Compression
}

// NewNode is a node over pmm, whose pages are pageSize bytes, with its own
// page queues. compression may be nil: Zircon's Pmm::Node().
// GetPageCompression() is null when no compression is configured.
func NewNode(pmm Pmm, pageSize uint64, compression *Compression) *Node {
	assert(pageSize != 0 && pageSize&(pageSize-1) == 0, "the page size is a power of two")
	return &Node{pmm: pmm, pageSize: pageSize, queues: NewPageQueues[*VmPage, *CowPages](pageSize), compression: compression}
}

// FreePage gives a page back to the node's pmm. With FreeReference it makes
// the node the Freer a splice list of its pages frees through.
func (n *Node) FreePage(p *VmPage) {
	assert(p.queue == nil, "a freed page is in no queue")
	initializeVmPage(p)
	n.pmm.FreePage(p)
}

// FreeReference frees a compressed reference to the node's compression.
func (n *Node) FreeReference(ref ReferenceValue) {
	assert(n.compression != nil, "a reference has a compression to free it to")
	n.compression.Free(ref)
}

// PageQueues are the node's page queues.
func (n *Node) PageQueues() *VmPageQueues { return n.queues }

// VmPageQueues are the page queues of a Node's pages.
type VmPageQueues = PageQueues[*VmPage, *CowPages]

// PageSize is the size of the node's pages.
func (n *Node) PageSize() uint64 { return n.pageSize }

// Compression is the node's page compression, or nil.
func (n *Node) Compression() *Compression { return n.compression }
