// Package peer is the peer server: the one channel hosts talk to each other
// over. A host's Server answers every other host. It serves the pages a host
// still holds for a VM it handed over — a migrated VM's memory, or the fork
// point a child on another host inherits — and, once the disk cache reads from
// its peers, the stripes of that cache.
//
// Migration is one user of this channel and the cluster's disk cache another,
// so neither owns it: vmmigrate asks it for pages, and the checkpoint cache will
// answer stripe requests through it.
package peer

import (
	"errors"

	"github.com/semistrict/sproutfs/peer/internal/wire"
)

var (
	// ErrInvalid reports a configuration or a request this package cannot use.
	ErrInvalid = errors.New("peer: invalid argument")
	// ErrClosed reports a server or a connection that has stopped.
	ErrClosed = errors.New("peer: closed")
	// ErrOutstanding reports a release refused because the destination has not
	// fetched every page this host holds that no checkpoint of the VM has.
	// Those pages exist nowhere else: releasing them would lose the guest's
	// writes since this host's last checkpoint. The server knows which of them
	// it has answered for, so this is the evidence, rather than whatever a
	// control plane's table believes about the migration.
	ErrOutstanding = errors.New("peer: unpublished pages are still outstanding")
	// ErrPastEnd is what Pages.ReadResident reports for a page past the end of
	// its volume. Every higher page is past the end too, so a request stops
	// there.
	ErrPastEnd = errors.New("peer: page past the end of the volume")
	// ErrMalformed reports a frame or a reply this host cannot read. A peer
	// that sends one is a peer this host cannot use, which is neither a peer
	// that is gone nor one that stumbled.
	ErrMalformed = wire.ErrMalformedFrame
)

// MaxPageSize is the largest page a volume may be published in, which is what a
// server's budgets are sized against when it is told no page of its own.
const MaxPageSize = 2 << 20

const (
	// RequestBytes is what one request of pages is sized against, whatever page
	// those are: a request of 4 KiB RAM pages carries as many of them as fit in
	// it, and one of 2 MiB PMEM pages carries a single page.
	RequestBytes = 2 << 20
	// DefaultMaxPages caps scaled-model requests. Production requests default
	// to one 2 MiB page, within the same bounded byte budget.
	DefaultMaxPages = 256
	// DefaultMaxRuns bounds one resident listing, so a destination walks a
	// large memory region in several bounded replies rather than one unbounded
	// one.
	DefaultMaxRuns = 1024
)
