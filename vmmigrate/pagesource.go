package vmmigrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"maps"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
	migratev1 "github.com/semistrict/sproutfs/vmmigrate/internal/gen/sproutfs/migrate/v1"
	"github.com/semistrict/sproutfs/vmmigrate/internal/wire"
	"github.com/semistrict/sproutfs/volume"
	"google.golang.org/protobuf/proto"
)

var pageCRCTable = crc32.MakeTable(crc32.Castagnoli)

const (
	// requestBytes is what one request of pages is sized against, whatever page
	// those are: a request of 4 KiB RAM pages carries as many of them as fit in
	// it, and one of 2 MiB PMEM pages carries a single page.
	requestBytes = 2 << 20
	// defaultMaxPages caps scaled-model requests. Production requests default
	// to one 2 MiB page, within the same bounded byte budget.
	defaultMaxPages = 256
	// defaultMaxRuns bounds one resident listing, so a destination walks a large
	// memory region in several bounded replies rather than one unbounded one.
	defaultMaxRuns = 1024
	// defaultConnectionsPerPeer bounds how many requests one peer can have in
	// flight here, since each connection serves one request at a time.
	defaultConnectionsPerPeer = 8
	// defaultBytesInFlight bounds the page bytes one peer's requests may hold at
	// once, whatever their connections would otherwise allow.
	defaultBytesInFlight = 8 << 20
	// requestTimeout bounds one page request. Everything it reads is local, so
	// this is generous by an order of magnitude and only bounds a source whose
	// pages have stopped answering: a destination told its request failed
	// retries or reads its own volume, where one left waiting holds a guest's
	// fault open behind it.
	requestTimeout = 30 * time.Second
)

// SourceConfig supplies the network one host serves migration pages on. Every
// peer that reaches the listener is served: hosts share a trusted cluster
// network, and restricting this port to them is the network policy's job. One
// peer is one remote host, which is what the budgets below are counted per.
type SourceConfig struct {
	// Network opens the listener at Address. A caller that has already opened
	// one supplies it as Listener instead.
	Network  platform.Network
	Listener platform.Listener
	Address  platform.Address
	// PageSize is the largest page this source serves, which is what its
	// per-peer byte budgets are sized against. What a reply is actually counted
	// in is the page of the volume it answers for — a host's two kinds of memory region
	// need not agree — so this bounds the budgets and names nothing else. Zero
	// selects the largest page a volume may be published in.
	PageSize int
	// MaxPagesPerRequest caps one reply whatever volume it answers for. Zero
	// takes the cap from the volume's own page instead, so that a reply of small
	// pages carries as many of them as one of a large page carries bytes.
	// MaxConnectionsPerPeer and MaxBytesInFlightPerPeer take the documented
	// defaults when zero.
	MaxPagesPerRequest      int
	MaxConnectionsPerPeer   int
	MaxBytesInFlightPerPeer int64
}

// SourceStats reports what this host has served.
type SourceStats struct {
	// Requests is every page request answered, Served and Absent the pages they
	// found and did not find.
	Requests, Served, Absent int64
	// Refused counts the requests turned away by the per-peer budget and the
	// connections closed for exceeding it, which a destination answers by
	// reading its own volume.
	Refused int64
	// Listings is every resident listing answered.
	Listings int64
}

// Pages is one volume's worth of pages a host still holds for another: a
// migrated VM's memory region, whose volume has been given up but whose pages have
// not, or the sealed fork point a fork was taken at, which the parent goes on
// running behind. The protocol above does not know which it is answering from.
type Pages interface {
	// ReadResident copies one page's current bytes, reports false for a page
	// whose bytes this host does not hold, and reports separately whether the
	// page it served is state no checkpoint of the VM has. It never loads.
	ReadResident(ctx context.Context, page uint64, dst []byte) (held, unpublished bool, err error)
	// Resident lists the pages this source can serve, in ascending order. A
	// source that cannot answer reports why: an empty listing is one the
	// destination acts on by reading every page from its own volume.
	Resident() ([]uint64, error)
	// Unpublished lists the pages this source holds that no checkpoint of the
	// VM has, in ascending order. They exist nowhere else, so the destination
	// must fetch every one of them and this source may not stop serving until
	// it has; the rest of Resident it may read from its own volume instead. A
	// source that cannot answer reports why, because the empty set is one the
	// destination acts on by fetching nothing.
	Unpublished() ([]uint64, error)
	// PageSize is the page this volume's numbers are counted in. It is the
	// volume's own, which both hosts read out of the same durable geometry, so
	// the two kinds of memory region a VM maps may answer differently.
	PageSize() uint64
}

// MemoryRegionPages presents the memory regions of a migrated VM as what its page source
// serves, by volume name. The memory regions keep their pages after their volumes
// were handed off, which is exactly what this serves.
func MemoryRegionPages(memoryRegions map[string]*vmmemory.MemoryRegion) map[string]Pages {
	pages := make(map[string]Pages, len(memoryRegions))
	for name, memoryRegion := range memoryRegions {
		pages[name] = memoryRegion
	}
	return pages
}

// forkPages presents one volume of a fork point as what its parent's page
// source serves. Only the pages no checkpoint of the parent holds are served:
// everything else is in object storage, where the child reads it from, and
// serving it would only copy what both sides already share by identity.
type forkPages struct {
	point  *volume.ForkPoint
	volume string
	held   map[uint64]bool
}

// ForkPages presents a fork point as what the parent's page source serves the
// child, by volume name.
func ForkPages(point *volume.ForkPoint) map[string]Pages {
	names := point.Volumes()
	pages := make(map[string]Pages, len(names))
	for _, name := range names {
		held := make(map[uint64]bool)
		for _, page := range point.Pages(name) {
			held[page] = true
		}
		pages[name] = forkPages{point: point, volume: name, held: held}
	}
	return pages
}

func (f forkPages) Resident() ([]uint64, error) { return f.point.Pages(f.volume), nil }

func (f forkPages) PageSize() uint64 { return f.point.PageSize(f.volume) }

// Unpublished is everything a fork point serves: the pages it names are exactly
// the ones no checkpoint of the parent holds, which is why the child has to
// fetch them and why nothing else is offered.
func (f forkPages) Unpublished() ([]uint64, error) { return f.point.Pages(f.volume), nil }

func (f forkPages) ReadResident(ctx context.Context, page uint64, dst []byte) (bool, bool, error) {
	if !f.held[page] {
		return false, false, nil
	}
	if err := f.point.ReadPage(ctx, f.volume, page, dst); err != nil {
		return false, false, err
	}
	// Every page a fork point serves is one no checkpoint holds, so the child's
	// pager keeps it privately until its own first checkpoint publishes it.
	return true, true, nil
}

// PageSource serves the pages of the VMs this host holds memory for on another
// host's behalf: the ones it has migrated away, and the children it has forked
// onto another host. A VM registers its pages when it is handed over and gives
// them up when the destination reports that it has them all.
type PageSource struct {
	config   SourceConfig
	listener platform.Listener
	ctx      context.Context
	cancel   context.CancelCauseFunc
	wg       sync.WaitGroup

	mu     sync.Mutex
	served map[string]map[string]Pages
	// outstanding is, per VM and volume, the pages this host holds that no
	// checkpoint has and that no destination has fetched yet. It is what makes
	// a release safe or not: those pages exist nowhere else, and this source is
	// the only thing that knows which of them it has actually answered for.
	outstanding map[string]map[string]map[uint64]struct{}
	// sending counts, per VM, the replies carrying pages no checkpoint holds
	// that this host has begun to send and not yet answered for. A release of
	// that VM waits for the count to reach zero, because the outcome of a reply
	// in flight is what decides whether those pages may be given up at all.
	sending map[string]int
	// settled is closed and replaced whenever this source's bookkeeping moves,
	// which is how a release waiting on a reply in flight is woken.
	settled chan struct{}
	// unlisted is why a VM's volumes could not report what they still hold,
	// which is as good a reason to refuse a release as pages known to be
	// outstanding: an unlistable memory region may hold anything.
	unlisted map[string]error
	peers    map[string]*peerBudget

	requests, servedPages, absentPages, refused, listings atomic.Int64
	closeOnce                                             sync.Once
	closeErr                                              error
}

// peerBudget is what one peer may hold here at once.
type peerBudget struct {
	connections int
	bytes       int64
}

// NewPageSource starts serving on address until Close. It serves nothing until a
// migration registers a VM's memory regions.
func NewPageSource(ctx context.Context, config SourceConfig) (*PageSource, error) {
	if (config.Network == nil && config.Listener == nil) || config.Address == "" {
		return nil, fmt.Errorf("%w: a page source needs a listener or a network, and an address", ErrInvalid)
	}
	if config.PageSize == 0 {
		config.PageSize = checkpoint.PageSize2MiB
	}
	if config.MaxConnectionsPerPeer == 0 {
		config.MaxConnectionsPerPeer = defaultConnectionsPerPeer
	}
	if config.MaxBytesInFlightPerPeer == 0 {
		config.MaxBytesInFlightPerPeer = defaultBytesInFlight
	}
	if config.PageSize < 512 || config.PageSize > blob.MaxSize || config.MaxPagesPerRequest < 0 || config.MaxPagesPerRequest > blob.MaxSize/config.PageSize || config.MaxConnectionsPerPeer < 1 ||
		config.MaxBytesInFlightPerPeer < int64(config.PageSize) {
		return nil, fmt.Errorf("%w: invalid page source budgets", ErrInvalid)
	}
	listener := config.Listener
	if listener == nil {
		opened, err := config.Network.Listen(config.Address)
		if err != nil {
			return nil, err
		}
		listener = opened
	}
	sourceCtx, cancel := context.WithCancelCause(ctx)
	s := &PageSource{config: config, listener: listener, ctx: sourceCtx, cancel: cancel,
		served:      make(map[string]map[string]Pages),
		outstanding: make(map[string]map[string]map[uint64]struct{}),
		sending:     make(map[string]int),
		settled:     make(chan struct{}),
		unlisted:    make(map[string]error),
		peers:       make(map[string]*peerBudget)}
	s.wg.Go(s.accept)
	return s, nil
}

// Address is where peers reach this page source.
func (s *PageSource) Address() platform.Address { return s.config.Address }

// PageSize is the largest page this source serves, which is what its budgets
// are sized against. A reply is counted in the page of the volume it answers
// for, which both hosts read out of that volume's durable geometry.
func (s *PageSource) PageSize() int { return s.config.PageSize }

// pagesPerRequest caps one reply of a volume whose page is pageSize. The bound
// is bytes, so a volume of small pages gets more of them in a reply rather than
// one page per round trip; a source told a cap of its own keeps it.
func (s *PageSource) pagesPerRequest(pageSize int) int {
	if s.config.MaxPagesPerRequest > 0 {
		return s.config.MaxPagesPerRequest
	}
	return max(1, min(defaultMaxPages, requestBytes/pageSize))
}

// Serve registers what this host holds for one VM, by volume name. The VM is
// the one that runs elsewhere: a migrated VM, or a fork's child.
// The guest is stopped by the time this is called, so the pages no checkpoint
// holds can no longer change: what each volume reports as unpublished here is
// exactly what the destination must fetch before this host may release them.
func (s *PageSource) Serve(vmID string, pages map[string]Pages) {
	outstanding := make(map[string]map[uint64]struct{}, len(pages))
	var unlisted error
	for name, volume := range pages {
		unpublished, err := volume.Unpublished()
		if err != nil {
			// What this volume still holds cannot be listed, so nothing can
			// establish that a destination has it: the VM goes on being served
			// and only Discard gives it up.
			unlisted = errors.Join(unlisted, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if len(unpublished) == 0 {
			continue
		}
		set := make(map[uint64]struct{}, len(unpublished))
		for _, page := range unpublished {
			set[page] = struct{}{}
		}
		outstanding[name] = set
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.served[vmID] = maps.Clone(pages)
	s.outstanding[vmID] = outstanding
	if unlisted != nil {
		s.unlisted[vmID] = unlisted
	} else {
		delete(s.unlisted, vmID)
	}
}

// Release stops serving one VM. Every later request for it is answered with the
// plain fact that this host does not serve it, which sends the destination to
// its own volume for good.
//
// It refuses while this host still holds pages of that VM no checkpoint has and
// no destination has fetched: those bytes exist nowhere else, and releasing
// them would lose the guest's writes since this host's last checkpoint. What a
// control plane's table says about the migration is not evidence — this source
// answered the fetches, so it is the one thing that knows. A caller giving the
// VM up altogether uses Discard.
//
// A reply of that VM's pages that is still being sent is waited for rather than
// read past. This host's record of a reply is written once the reply has left,
// because a reply that failed to send carried nothing; the destination, though,
// acts on one the moment it arrives, so the release that its Done permits can
// arrive here while the send that earned it has not returned. Reading the book
// then refuses a release for pages the destination is already running on —
// which is what a drain sees as a VM it can never let go of — and no ordering
// of the record against the send can fix it, because the two events are
// concurrent. So the release waits for the reply's outcome and then reads a
// book that answers for it: struck off if it left, still outstanding if it did
// not. A source that closes while one is still in flight refuses, because a
// send that never returned is a send this host can say nothing about.
func (s *PageSource) Release(vmID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.sending[vmID] > 0 {
		if _, serving := s.outstanding[vmID]; !serving {
			// The VM was given up while the reply was in flight — its hold
			// deadline passed, or this host lost it — so there is nothing left
			// for a release to decide about, and the deadline is what bounds
			// this wait.
			return nil
		}
		settled := s.settled
		s.mu.Unlock()
		select {
		case <-settled:
		case <-s.ctx.Done():
			s.mu.Lock()
			return fmt.Errorf("%w: a reply of %s's pages was still being sent when this page source closed",
				ErrOutstanding, vmID)
		}
		s.mu.Lock()
	}
	if left := outstandingPages(s.outstanding[vmID]); left > 0 {
		return fmt.Errorf("%w: %s has %d the destination has not fetched", ErrOutstanding, vmID, left)
	}
	if err := s.unlisted[vmID]; err != nil {
		return fmt.Errorf("%w: what %s still holds could not be listed: %w", ErrOutstanding, vmID, err)
	}
	delete(s.served, vmID)
	delete(s.outstanding, vmID)
	delete(s.unlisted, vmID)
	return nil
}

// Discard stops serving one VM whatever it still holds for it. It is for a VM
// this host is giving up rather than handing over — one whose fork hold
// outlived its deadline, one whose fan-out failed, one this host has lost —
// where the pages are going either way and refusing would only leave the
// parent sealed and the VM half-released.
func (s *PageSource) Discard(vmID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// A release waiting on a reply of this VM's pages has nothing left to
	// decide about, which is what bounds that wait by the hold's own deadline.
	defer s.wake()
	delete(s.served, vmID)
	delete(s.outstanding, vmID)
	delete(s.unlisted, vmID)
}

// outstandingPages counts what one VM's volumes still owe a destination.
func outstandingPages(volumes map[string]map[uint64]struct{}) int {
	total := 0
	for _, pages := range volumes {
		total += len(pages)
	}
	return total
}

// sendingPages counts a reply carrying pages no checkpoint holds in or out of
// flight for one VM, and wakes whatever is waiting on the count.
func (s *PageSource) sendingPages(vmID string, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sending[vmID] += delta
	if s.sending[vmID] <= 0 {
		delete(s.sending, vmID)
	}
	s.wake()
}

// wake releases everything waiting on this source's bookkeeping. Caller holds
// the lock.
func (s *PageSource) wake() {
	close(s.settled)
	s.settled = make(chan struct{})
}

// fetched records the unpublished pages one reply carried, which is the only
// evidence this host has that the destination holds them.
func (s *PageSource) fetched(vmID, name string, pages []uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.wake()
	set := s.outstanding[vmID][name]
	if set == nil {
		return
	}
	for _, page := range pages {
		delete(set, page)
	}
	if len(set) == 0 {
		delete(s.outstanding[vmID], name)
	}
}

// Serving reports the VMs whose pages this host still holds for another.
func (s *PageSource) Serving() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.served))
}

// Outstanding reports, per VM this host still serves, how many pages no
// checkpoint has that no destination has fetched. A VM at zero is one a release
// would accept; every other number is a destination still fetching, or one that
// stopped. It is what a host being drained is actually waiting on, which
// Serving alone does not say: a VM can sit in that list for either reason.
//
// A VM whose volumes could not be listed reports -1 rather than a count: what it
// still holds is unknown, which is as good a reason to refuse a release as pages
// known to be outstanding.
func (s *PageSource) Outstanding() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	left := make(map[string]int, len(s.served))
	for vmID := range s.served {
		if s.unlisted[vmID] != nil {
			left[vmID] = -1
			continue
		}
		left[vmID] = outstandingPages(s.outstanding[vmID])
	}
	return left
}

func (s *PageSource) Stats() SourceStats {
	return SourceStats{Requests: s.requests.Load(), Served: s.servedPages.Load(),
		Absent: s.absentPages.Load(), Refused: s.refused.Load(), Listings: s.listings.Load()}
}

// Close stops accepting and drops every connection. It does not release the
// memory regions: their pages belong to the VMM process that owns them.
func (s *PageSource) Close() error {
	s.closeOnce.Do(func() {
		s.cancel(ErrClosed)
		s.closeErr = s.listener.Close()
		s.wg.Wait()
	})
	return s.closeErr
}

func (s *PageSource) accept() {
	for {
		conn, err := s.listener.Accept(s.ctx)
		if err != nil {
			if context.Cause(s.ctx) == nil {
				slog.WarnContext(s.ctx, "vmmigrate: page source stopped accepting", "address", s.config.Address, "error", err)
			}
			return
		}
		s.wg.Go(func() { s.serveConn(conn) })
	}
}

// peerKey is the identity a budget is counted against: the host a connection
// came from, without the ephemeral port it happens to have been given. A
// destination opens a connection per memory region and dials again whenever one
// breaks, so counting connections by their full address counts each of them as
// a peer of its own: neither budget ever binds, and the table of peers grows
// with every reconnect for as long as this host serves. A simulated address is
// a logical name with no port and is its own key.
func peerKey(address platform.Address) string {
	host, _, err := net.SplitHostPort(string(address))
	if err != nil {
		return string(address)
	}
	return host
}

func (s *PageSource) serveConn(conn platform.Conn) {
	defer conn.Close()
	peer := peerKey(conn.RemoteAddress())
	if !s.acquireConnection(peer) {
		s.refused.Add(1)
		slog.WarnContext(s.ctx, "vmmigrate: refused a page connection over the per-peer budget", "peer", peer)
		return
	}
	defer s.releaseConnection(peer)
	// The connection stays open for as many requests as the destination sends;
	// each is answered before the next is read, which is what makes one
	// connection one request in flight.
	for {
		received, err := conn.Receive(s.ctx)
		if err != nil {
			return
		}
		incoming, err := wire.Decode(received)
		if err != nil {
			return
		}
		if err := s.dispatch(conn, peer, incoming); err != nil {
			return
		}
	}
}

func (s *PageSource) acquireConnection(peer string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	budget := s.peers[peer]
	if budget == nil {
		budget = &peerBudget{}
		s.peers[peer] = budget
	}
	if budget.connections >= s.config.MaxConnectionsPerPeer {
		return false
	}
	budget.connections++
	return true
}

func (s *PageSource) releaseConnection(peer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	budget := s.peers[peer]
	if budget == nil {
		return
	}
	budget.connections--
	if budget.connections <= 0 && budget.bytes == 0 {
		delete(s.peers, peer)
	}
}

// reserve takes one request's worst-case page bytes from a peer's budget, and
// reports false when that peer already holds all of it.
func (s *PageSource) reserve(peer string, bytes int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	budget := s.peers[peer]
	if budget == nil || budget.bytes+bytes > s.config.MaxBytesInFlightPerPeer {
		return false
	}
	budget.bytes += bytes
	return true
}

func (s *PageSource) release(peer string, bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if budget := s.peers[peer]; budget != nil {
		budget.bytes -= bytes
	}
}

// pagesOf resolves one request's volume, or reports why it cannot.
func (s *PageSource) pagesOf(vmID, name string) (Pages, migratev1.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	volumes, found := s.served[vmID]
	if !found {
		return nil, migratev1.Status_STATUS_UNKNOWN_VM
	}
	pages, found := volumes[name]
	if !found {
		return nil, migratev1.Status_STATUS_UNKNOWN_VOLUME
	}
	return pages, migratev1.Status_STATUS_OK
}

func (s *PageSource) dispatch(conn platform.Conn, peer string, incoming wire.Incoming) error {
	if err := drain(incoming); err != nil {
		return err
	}
	pageRequest, residentRequest := new(migratev1.PageRequest), new(migratev1.ResidentRequest)
	switch {
	case incoming.Message.MessageIs(pageRequest):
		if err := incoming.UnmarshalTo(pageRequest); err != nil {
			return err
		}
		response, payload, served := s.pages(peer, pageRequest)
		// A reply carrying pages no checkpoint holds is in flight from here
		// until its outcome is recorded, and a release of this VM waits for
		// that rather than reading a book the reply has not been written into
		// yet. The destination acts on a reply the moment it arrives — it
		// installs the pages, its Done returns, and the release of this source
		// follows from that — and none of that is ordered after this
		// goroutine's next statement.
		if len(served) > 0 {
			s.sendingPages(pageRequest.GetVm(), 1)
			defer s.sendingPages(pageRequest.GetVm(), -1)
		}
		if err := s.reply(conn, incoming.RequestID, response, payload); err != nil {
			return err
		}
		// The reply is on the wire. Until it is, this host has no evidence at
		// all that the destination holds these pages, and a reply that failed
		// to send carried nothing: recording them before it leaves would let a
		// release drop the only copy of the guest's writes.
		if len(served) > 0 {
			s.fetched(pageRequest.GetVm(), pageRequest.GetVolume(), served)
		}
		return nil
	case incoming.Message.MessageIs(residentRequest):
		if err := incoming.UnmarshalTo(residentRequest); err != nil {
			return err
		}
		return s.reply(conn, incoming.RequestID, s.resident(residentRequest), nil)
	default:
		return wire.ErrMalformedFrame
	}
}

// drain consumes a request's payload. A page request carries none, so anything
// here is a malformed frame and the connection is dropped.
func drain(incoming wire.Incoming) error {
	defer incoming.Payload.Close()
	if incoming.PayloadSize != 0 {
		return platform.ErrMessageTooLarge
	}
	_, err := io.Copy(io.Discard, incoming.Payload)
	return err
}

// pages answers one page request with the bytes this host holds. A page it does
// not hold is reported plainly, which sends the destination to its own volume
// for that page and nothing more. The pages it reports as served are the ones
// no checkpoint holds that this reply carries; the caller records them once the
// reply has left, which is the only evidence this host ever gets.
func (s *PageSource) pages(peer string, request *migratev1.PageRequest) (*migratev1.PageResponse, []byte, []uint64) {
	s.requests.Add(1)
	// The reply is counted in the page of the volume it answers for, which is
	// only known once the request has named one; until then the budget's own
	// page is what an answer can carry.
	pageSize := s.config.PageSize
	answer := func(status migratev1.Status) *migratev1.PageResponse {
		return migratev1.PageResponse_builder{Status: &status,
			PageSize: proto.Uint32(uint32(pageSize))}.Build()
	}
	count := int(request.GetCount())
	if count <= 0 || request.GetPayloadFormat() != 1 {
		return answer(migratev1.Status_STATUS_INVALID_REQUEST), nil, nil
	}
	pages, status := s.pagesOf(request.GetVm(), request.GetVolume())
	if status != migratev1.Status_STATUS_OK {
		return answer(status), nil, nil
	}
	if volumePage := int(pages.PageSize()); volumePage > 0 {
		pageSize = volumePage
	}
	count = min(count, s.pagesPerRequest(pageSize))
	reserved := int64(count) * int64(pageSize)
	// A source at its per-peer budget answers BUSY, which a host draining many
	// VMs at once makes the normal state. A destination must queue behind it
	// for the pages no checkpoint holds and read its own volume for the rest,
	// and a campaign with one migration at a time never makes a source busy.
	if sim.Buggify(s.ctx, "vmmigrate/source-busy", 0.5) || !s.reserve(peer, reserved) {
		s.refused.Add(1)
		return answer(migratev1.Status_STATUS_BUSY), nil, nil
	}
	defer s.release(peer, reserved)

	// One request gets a deadline of its own. Everything it touches is local —
	// pages this host already holds — so a read that does not finish inside it
	// is a page this connection is never going to get, and the destination is
	// better told than left holding a request while its guest waits on the
	// fault behind it.
	ctx, cancel := context.WithTimeout(s.ctx, requestTimeout)
	defer cancel()

	present := make([]byte, (count+7)/8)
	dirty := make([]byte, (count+7)/8)
	payload := make([]byte, 0, count*pageSize)
	page := make([]byte, pageSize)
	var served []uint64
	found := 0
	for index := range count {
		held, unpublished, err := pages.ReadResident(ctx, request.GetFirstPage()+uint64(index), page)
		if errors.Is(err, vmmemory.ErrRange) {
			// Every higher page is out of range too.
			break
		}
		if err != nil {
			slog.WarnContext(s.ctx, "vmmigrate: serving a page failed", "vm", request.GetVm(),
				"volume", request.GetVolume(), "page", request.GetFirstPage()+uint64(index), "error", err)
			return answer(migratev1.Status_STATUS_INTERNAL), nil, nil
		}
		if !held {
			continue
		}
		present[index/8] |= 1 << (index % 8)
		if unpublished {
			// These bytes are this host's own: no checkpoint of the VM holds
			// them, so the destination has to keep the page dirty.
			dirty[index/8] |= 1 << (index % 8)
			served = append(served, request.GetFirstPage()+uint64(index))
		}
		payload = append(payload, page...)
		found++
	}
	encoded, err := blob.Encode(ctx, payload)
	if err != nil {
		return answer(migratev1.Status_STATUS_INTERNAL), nil, nil
	}
	s.servedPages.Add(int64(found))
	s.absentPages.Add(int64(count - found))
	status = migratev1.Status_STATUS_OK
	return migratev1.PageResponse_builder{Status: &status, Present: present, Dirty: dirty,
		PageSize: proto.Uint32(uint32(pageSize)), Count: proto.Uint32(uint32(count)), PayloadFormat: proto.Uint32(1)}.Build(), encoded, served
}

// resident lists what one memory region holds, from the requested page on, in runs.
func (s *PageSource) resident(request *migratev1.ResidentRequest) *migratev1.ResidentResponse {
	s.listings.Add(1)
	pageSize := s.config.PageSize
	answer := func(status migratev1.Status) *migratev1.ResidentResponse {
		return migratev1.ResidentResponse_builder{Status: &status,
			PageSize: proto.Uint32(uint32(pageSize))}.Build()
	}
	pages, status := s.pagesOf(request.GetVm(), request.GetVolume())
	if status != migratev1.Status_STATUS_OK {
		return answer(status)
	}
	if volumePage := int(pages.PageSize()); volumePage > 0 {
		pageSize = volumePage
	}
	maxRuns := int(request.GetMaxRuns())
	if maxRuns <= 0 || maxRuns > defaultMaxRuns {
		maxRuns = defaultMaxRuns
	}
	resident, err := pages.Resident()
	if err != nil {
		// An empty listing is a host that holds nothing, which sends the
		// destination to its own volume for every page. A host that cannot
		// answer says so instead, and the destination asks again.
		slog.WarnContext(s.ctx, "vmmigrate: listing what this host holds failed",
			"vm", request.GetVm(), "volume", request.GetVolume(), "error", err)
		return answer(migratev1.Status_STATUS_INTERNAL)
	}
	runs, more := pageRuns(resident, request.GetFirstPage(), maxRuns)
	status = migratev1.Status_STATUS_OK
	return migratev1.ResidentResponse_builder{Status: &status, Runs: runs,
		PageSize: proto.Uint32(uint32(pageSize)), More: proto.Bool(more)}.Build()
}

// pageRuns groups ascending page numbers at or above first into at most maxRuns
// runs, reporting whether it stopped early.
func pageRuns(pages []uint64, first uint64, maxRuns int) (runs []*migratev1.PageRun, more bool) {
	for _, page := range pages {
		if page < first {
			continue
		}
		if n := len(runs); n > 0 && runs[n-1].GetFirstPage()+uint64(runs[n-1].GetCount()) == page {
			runs[n-1].SetCount(runs[n-1].GetCount() + 1)
			continue
		}
		if len(runs) == maxRuns {
			return runs, true
		}
		runs = append(runs, migratev1.PageRun_builder{FirstPage: proto.Uint64(page), Count: proto.Uint32(1)}.Build())
	}
	return runs, false
}

func (s *PageSource) reply(conn platform.Conn, requestID uint64, message proto.Message, payload []byte) error {
	frame, err := wire.Encode(wire.Outgoing{
		InReplyTo: requestID,
		RequestID: requestID,
		Message:   message,
		Payload: wire.Payload{Body: bytes.NewReader(payload), Size: int64(len(payload)),
			Algorithm: wire.ChecksumCRC32C, Checksum: wire.EncodeCRC32C(crc32.Checksum(payload, pageCRCTable))},
	})
	if err != nil {
		return err
	}
	return conn.Send(s.ctx, frame)
}
