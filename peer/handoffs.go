package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/internal/blob"
	migratev1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/migrate/v1"
	"google.golang.org/protobuf/proto"
)

// requestTimeout bounds the reading of one page request. Everything it reads is
// local, so this is generous by an order of magnitude and only bounds a source
// whose pages have stopped answering: a destination told its request failed
// retries or reads its own volume, where one left waiting holds a guest's fault
// open behind it.
const requestTimeout = 30 * time.Second

// Pages is one volume's worth of pages a host still holds for another: a
// migrated VM's memory region, whose volume has been given up but whose pages have
// not, or the sealed fork point a fork was taken at, which the parent goes on
// running behind. The protocol above does not know which it is answering from.
type Pages interface {
	// ReadResident copies one page's current bytes, reports false for a page
	// whose bytes this host does not hold, and reports separately whether the
	// page it served is state no checkpoint of the VM has. It never loads. A
	// page past the end of the volume is ErrPastEnd.
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

// handoffs is the book of what this host holds for VMs that run elsewhere, and
// what their destinations have fetched of it.
type handoffs struct {
	mu     sync.Mutex
	served map[string]map[string]Pages
	// outstanding is, per VM and volume, the pages this host holds that no
	// checkpoint has and that no destination has fetched yet. It is what makes
	// a release safe or not: those pages exist nowhere else, and this server is
	// the only thing that knows which of them it has actually answered for.
	outstanding map[string]map[string]map[uint64]struct{}
	// sending counts, per VM, the replies carrying pages no checkpoint holds
	// that this host has begun to send and not yet answered for. A release of
	// that VM waits for the count to reach zero, because the outcome of a reply
	// in flight is what decides whether those pages may be given up at all.
	sending map[string]int
	// settled is closed and replaced whenever the book moves, which is how a
	// release waiting on a reply in flight is woken.
	settled chan struct{}
	// unlisted is why a VM's volumes could not report what they still hold,
	// which is as good a reason to refuse a release as pages known to be
	// outstanding: an unlistable memory region may hold anything.
	unlisted map[string]error
	// claimed are the fork children whose destination has taken them in over
	// the hold this host keeps for them: see Claim.
	claimed map[string]bool
}

func newHandoffs() handoffs {
	return handoffs{served: make(map[string]map[string]Pages),
		outstanding: make(map[string]map[string]map[uint64]struct{}),
		sending:     make(map[string]int),
		settled:     make(chan struct{}),
		unlisted:    make(map[string]error),
		claimed:     make(map[string]bool)}
}

// Serve registers what this host holds for one VM, by volume name. The VM is
// the one that runs elsewhere: a migrated VM, or a fork's child.
// The guest is stopped by the time this is called, so the pages no checkpoint
// holds can no longer change: what each volume reports as unpublished here is
// exactly what the destination must fetch before this host may release them.
func (s *Server) Serve(vmID string, pages map[string]Pages) {
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
	h := &s.handoffs
	h.mu.Lock()
	defer h.mu.Unlock()
	h.served[vmID] = maps.Clone(pages)
	h.outstanding[vmID] = outstanding
	if unlisted != nil {
		h.unlisted[vmID] = unlisted
	} else {
		delete(h.unlisted, vmID)
	}
}

// Release stops serving one VM. Every later request for it is answered with the
// plain fact that this host does not serve it, which sends the destination to
// its own volume for good.
//
// It refuses while this host still holds pages of that VM no checkpoint has and
// no destination has fetched: those bytes exist nowhere else, and releasing
// them would lose the guest's writes since this host's last checkpoint. What a
// control plane's table says about the migration is not evidence — this server
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
// not. A server that closes while one is still in flight refuses, because a
// send that never returned is a send this host can say nothing about.
func (s *Server) Release(vmID string) error {
	h := &s.handoffs
	h.mu.Lock()
	defer h.mu.Unlock()
	for h.sending[vmID] > 0 {
		if _, serving := h.outstanding[vmID]; !serving {
			// The VM was given up while the reply was in flight — its hold
			// deadline passed, or this host lost it — so there is nothing left
			// for a release to decide about, and the deadline is what bounds
			// this wait.
			return nil
		}
		settled := h.settled
		h.mu.Unlock()
		select {
		case <-settled:
		case <-s.ctx.Done():
			h.mu.Lock()
			return fmt.Errorf("%w: a reply of %s's pages was still being sent when this peer server closed",
				ErrOutstanding, vmID)
		}
		h.mu.Lock()
	}
	if left := outstandingPages(h.outstanding[vmID]); left > 0 {
		return fmt.Errorf("%w: %s has %d the destination has not fetched", ErrOutstanding, vmID, left)
	}
	if err := h.unlisted[vmID]; err != nil {
		return fmt.Errorf("%w: what %s still holds could not be listed: %w", ErrOutstanding, vmID, err)
	}
	delete(h.served, vmID)
	delete(h.outstanding, vmID)
	delete(h.unlisted, vmID)
	delete(h.claimed, vmID)
	return nil
}

// Discard stops serving one VM whatever it still holds for it. It is for a VM
// this host is giving up rather than handing over — one whose fork hold
// outlived its deadline, one whose fan-out failed, one this host has lost —
// where the pages are going either way and refusing would only leave the
// parent sealed and the VM half-released. It reports a fork child whose
// destination had already claimed it: that child runs, whatever the give-up
// meant.
func (s *Server) Discard(vmID string) (claimed bool) {
	h := &s.handoffs
	h.mu.Lock()
	defer h.mu.Unlock()
	// A release waiting on a reply of this VM's pages has nothing left to
	// decide about, which is what bounds that wait by the hold's own deadline.
	defer h.wake()
	claimed = h.claimed[vmID]
	delete(h.served, vmID)
	delete(h.outstanding, vmID)
	delete(h.unlisted, vmID)
	delete(h.claimed, vmID)
	return claimed
}

// Claim is a fork child's destination taking the child in over the hold this
// host keeps for it, once it has every page the child inherited. It reports
// whether the hold still stood: one that did is marked claimed, and a later
// Discard says so; one that was given up, released or ran out is not, and the
// destination discards the child. Claim and Discard take the same lock, so of a
// claim and a give-up of one child exactly one comes first.
func (s *Server) Claim(vmID string) bool {
	h := &s.handoffs
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, served := h.served[vmID]; !served {
		return false
	}
	h.claimed[vmID] = true
	return true
}

// Serving reports the VMs whose pages this host still holds for another.
func (s *Server) Serving() []string {
	h := &s.handoffs
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Sorted(maps.Keys(h.served))
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
func (s *Server) Outstanding() map[string]int {
	h := &s.handoffs
	h.mu.Lock()
	defer h.mu.Unlock()
	left := make(map[string]int, len(h.served))
	for vmID := range h.served {
		if h.unlisted[vmID] != nil {
			left[vmID] = -1
			continue
		}
		left[vmID] = outstandingPages(h.outstanding[vmID])
	}
	return left
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
func (h *handoffs) sendingPages(vmID string, delta int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sending[vmID] += delta
	if h.sending[vmID] <= 0 {
		delete(h.sending, vmID)
	}
	h.wake()
}

// wake releases everything waiting on the book. Caller holds the lock.
func (h *handoffs) wake() {
	close(h.settled)
	h.settled = make(chan struct{})
}

// fetched records the unpublished pages one reply carried, which is the only
// evidence this host has that the destination holds them.
func (h *handoffs) fetched(vmID, name string, pages []uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	defer h.wake()
	set := h.outstanding[vmID][name]
	if set == nil {
		return
	}
	for _, page := range pages {
		delete(set, page)
	}
	if len(set) == 0 {
		delete(h.outstanding[vmID], name)
	}
}

// pagesOf resolves one request's volume, or reports why it cannot.
func (h *handoffs) pagesOf(vmID, name string) (Pages, migratev1.Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	volumes, found := h.served[vmID]
	if !found {
		return nil, migratev1.Status_STATUS_UNKNOWN_VM
	}
	pages, found := volumes[name]
	if !found {
		return nil, migratev1.Status_STATUS_UNKNOWN_VOLUME
	}
	return pages, migratev1.Status_STATUS_OK
}

// pagesPerRequest caps one reply of a volume whose page is pageSize. The bound
// is bytes, so a volume of small pages gets more of them in a reply rather than
// one page per round trip; a server told a cap of its own keeps it.
func (s *Server) pagesPerRequest(pageSize int) int {
	if s.config.MaxPagesPerRequest > 0 {
		return s.config.MaxPagesPerRequest
	}
	return max(1, min(DefaultMaxPages, RequestBytes/pageSize))
}

// answerPages answers one page request with the bytes this host holds. A page
// it does not hold is reported plainly, which sends the destination to its own
// volume for that page and nothing more. The pages no checkpoint holds that the
// reply carries are struck off the book once it has left, which is the only
// evidence this host ever gets that the destination holds them.
//
// A peer whose class is at its budget is answered BUSY, which a host draining
// many VMs at once makes the normal state. A destination must queue behind it
// for the pages no checkpoint holds and read its own volume for the rest.
func (s *Server) answerPages(session *session, request *migratev1.PageRequest) answer {
	s.requests.Add(1)
	// The reply is counted in the page of the volume it answers for, which is
	// only known once the request has named one; until then the budget's own
	// page is what an answer can carry.
	pageSize := s.config.PageSize
	status := func(status migratev1.Status) answer {
		return answer{message: migratev1.PageResponse_builder{Status: &status,
			PageSize: proto.Uint32(uint32(pageSize))}.Build()}
	}
	count := int(request.GetCount())
	if count <= 0 || request.GetPayloadFormat() != 1 {
		return status(migratev1.Status_STATUS_INVALID_REQUEST)
	}
	pages, found := s.handoffs.pagesOf(request.GetVm(), request.GetVolume())
	if found != migratev1.Status_STATUS_OK {
		return status(found)
	}
	if volumePage := int(pages.PageSize()); volumePage > 0 {
		pageSize = volumePage
	}
	count = min(count, s.pagesPerRequest(pageSize))
	release, busy := s.reserve(session, int64(count)*int64(pageSize))
	if busy != nil {
		if session.version < 2 {
			// The release before has no BUSY of its own: its page reply says it.
			return status(migratev1.Status_STATUS_BUSY)
		}
		return answer{message: busy}
	}

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
	found = migratev1.Status_STATUS_OK
	held := 0
	for index := range count {
		resident, unpublished, err := pages.ReadResident(ctx, request.GetFirstPage()+uint64(index), page)
		if errors.Is(err, ErrPastEnd) {
			// Every higher page is past the end too.
			break
		}
		if err != nil {
			slog.WarnContext(s.ctx, "peer: serving a page failed", "vm", request.GetVm(),
				"volume", request.GetVolume(), "page", request.GetFirstPage()+uint64(index), "error", err)
			release()
			return status(migratev1.Status_STATUS_INTERNAL)
		}
		if !resident {
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
		held++
	}
	encoded, err := blob.Encode(ctx, payload)
	if err != nil {
		release()
		return status(migratev1.Status_STATUS_INTERNAL)
	}
	s.servedPages.Add(int64(held))
	s.absentPages.Add(int64(count - held))
	// A reply carrying pages no checkpoint holds is in flight from here until
	// its outcome is recorded, and a release of this VM waits for that rather
	// than reading a book the reply has not been written into yet. The
	// destination acts on a reply the moment it arrives — it installs the
	// pages, its Done returns, and the release of this server follows from that
	// — and none of that is ordered after this goroutine's next statement.
	vm, volume := request.GetVm(), request.GetVolume()
	if len(served) > 0 {
		s.handoffs.sendingPages(vm, 1)
	}
	return answer{
		message: migratev1.PageResponse_builder{Status: &found, Present: present, Dirty: dirty,
			PageSize: proto.Uint32(uint32(pageSize)), Count: proto.Uint32(uint32(count)),
			PayloadFormat: proto.Uint32(1)}.Build(),
		payload: encoded,
		checked: true,
		sent: func(sent bool) {
			release()
			if len(served) == 0 {
				return
			}
			// Until the reply is on the wire this host has no evidence at all
			// that the destination holds these pages, and a reply that failed
			// to send carried nothing: recording them before it leaves would
			// let a release drop the only copy of the guest's writes.
			if sent {
				s.handoffs.fetched(vm, volume, served)
			}
			s.handoffs.sendingPages(vm, -1)
		},
	}
}

// answerResident lists what one memory region holds, from the requested page on,
// in runs.
func (s *Server) answerResident(request *migratev1.ResidentRequest) answer {
	s.listings.Add(1)
	pageSize := s.config.PageSize
	status := func(status migratev1.Status) answer {
		return answer{message: migratev1.ResidentResponse_builder{Status: &status,
			PageSize: proto.Uint32(uint32(pageSize))}.Build()}
	}
	pages, found := s.handoffs.pagesOf(request.GetVm(), request.GetVolume())
	if found != migratev1.Status_STATUS_OK {
		return status(found)
	}
	if volumePage := int(pages.PageSize()); volumePage > 0 {
		pageSize = volumePage
	}
	maxRuns := int(request.GetMaxRuns())
	if maxRuns <= 0 || maxRuns > DefaultMaxRuns {
		maxRuns = DefaultMaxRuns
	}
	resident, err := pages.Resident()
	if err != nil {
		// An empty listing is a host that holds nothing, which sends the
		// destination to its own volume for every page. A host that cannot
		// answer says so instead, and the destination asks again.
		slog.WarnContext(s.ctx, "peer: listing what this host holds failed",
			"vm", request.GetVm(), "volume", request.GetVolume(), "error", err)
		return status(migratev1.Status_STATUS_INTERNAL)
	}
	runs, more := pageRuns(resident, request.GetFirstPage(), maxRuns)
	found = migratev1.Status_STATUS_OK
	return answer{message: migratev1.ResidentResponse_builder{Status: &found, Runs: runs,
		PageSize: proto.Uint32(uint32(pageSize)), More: proto.Bool(more)}.Build()}
}

// answerClaim marks a fork child's hold claimed, if it still stands.
func (s *Server) answerClaim(request *migratev1.ClaimRequest) answer {
	status := migratev1.Status_STATUS_UNKNOWN_VM
	if s.Claim(request.GetVm()) {
		status = migratev1.Status_STATUS_OK
	}
	return answer{message: migratev1.ClaimResponse_builder{Status: &status}.Build()}
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
