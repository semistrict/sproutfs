package peer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/membership"
	peerv1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/peer/v1"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
	"google.golang.org/protobuf/proto"
)

// The requests of the cluster's disk cache (plans/disk-cache-2026-10-02.md):
// read the stripes a disk holds of a window, keep stripes, drop a stripe found
// wrong, ask which pages a disk holds, and probe it. This package carries them
// and answers them through a Cache; the checkpoint cache is what implements it.
//
// Every request is routed by the membership (package membership). It names
// the disk it expects to reach and the generation its sender holds. A host
// answers only under that generation: one behind reads the membership first,
// and one ahead, or one that cannot read it, answers stale with its own, which
// the sender reads the membership for and asks again. A host the membership
// at that generation does not have serve the disk named answers ErrNotMe: an
// address can come to belong to another host, and a disk to another member.
// That is a stale route, never a down host. Every answer names the generation
// that assigned the disk to the host answering, so a host that lost the disk
// can never be taken for its server again.

var (
	// ErrNotMe reports a host that does not serve the disk a request named
	// under the request's generation, or that names another assignment of it
	// than the sender's.
	ErrNotMe = errors.New("peer: the host does not serve the disk the request named")
	// ErrStale reports a request answered under another generation of the
	// membership than the one it was made under. Its error is a StaleError.
	ErrStale = errors.New("peer: the request was made under another generation of the membership")
	// ErrDropped reports a keep the cache did not write: it holds the stripe
	// already, or its budget for writes is spent, or this host's background
	// budget was. Nothing waits on a keep, so it is dropped, not queued.
	ErrDropped = errors.New("peer: the keep was dropped")
	// ErrNoRoom reports a keep dropped before it was sent, because this
	// host's background budget had no room for it. It is an ErrDropped too.
	ErrNoRoom = errors.New("peer: the background budget has no room")
)

// StaleError is a holder's answer that it holds another generation of the
// membership than the request named. A sender behind it reads the membership
// and asks again; a holder behind it could not read the membership, and is
// asked nothing more under this request's.
type StaleError struct {
	// Generation is the holder's.
	Generation uint64
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("%v: the holder holds generation %d", ErrStale, e.Generation)
}

// Is makes a StaleError an ErrStale.
func (e *StaleError) Is(target error) bool { return target == ErrStale }

// StripeItem is one stripe a payload carries: the page of the window it belongs
// to, counted from the window's first, its index in the code, the length of
// the envelope it was split from, and its size in the payload, where the items
// lie in order. Its bytes are stored as they go on the wire and carry a
// checksum of their own, so a frame of stripes carries no checksum of its
// payload.
type StripeItem struct {
	Page   uint32
	Index  int
	Length int
	Size   int
}

// StripeRead asks for every stripe a disk holds of some pages of one window,
// of any index of the code. Pages nil asks for every page.
type StripeRead struct {
	Window   rank.Window
	Pages    []uint32
	Code     rank.Code
	MaxBytes int64
}

// Stripes is what a cache holds of a StripeRead. Payload is Size bytes holding
// the items in order: in memory, or a range of the cache's file, which a TCP
// adapter on Linux sends with sendfile. Release, when set, runs once the reply
// has been sent: what a read of a cache file holds — a region it must not give
// back under the send — it holds until then. FillRight gives the reader the
// right to fill a window this cache ranks first for and holds nothing of.
type Stripes struct {
	Items     []StripeItem
	Payload   io.ReaderAt
	Size      int64
	FillRight bool
	Release   func()
}

// Keep asks a cache to write stripes. Payload holds the items in order. A
// repair is the lowest of every write, and a fill from a publication the
// highest: a cache whose write budget runs low drops the rest first.
type Keep struct {
	Window      rank.Window
	Code        rank.Code
	Items       []StripeItem
	Payload     []byte
	Repair      bool
	Publication bool
}

// Drop tells a cache to forget one stripe a reader found wrong.
type Drop struct {
	Window rank.Window
	Page   uint32
	Index  int
	Code   rank.Code
}

// Presence asks which pages of some windows a cache holds a stripe of.
type Presence struct {
	Windows []rank.Window
	Code    rank.Code
}

// Cache is what a host's disk cache answers its peers with. A host may keep
// several disks: its own, or the shards the membership assigns it. Every
// method is asked only for a disk the cache keeps, named by its identity, and
// only under a membership that has this host serve it at the generation the
// request named, which the methods that place a window by its ranks are
// given.
type Cache interface {
	// Disks is the identity of every disk the cache keeps now, in order,
	// and Keeps whether it keeps one.
	Disks() []rank.Identity
	Keeps(disk rank.Identity) bool
	ReadStripes(ctx context.Context, m membership.Membership, disk rank.Identity, read StripeRead) (Stripes, error)
	// Keep writes stripes, or reports ErrDropped when it does not.
	Keep(ctx context.Context, m membership.Membership, disk rank.Identity, keep Keep) error
	Drop(ctx context.Context, disk rank.Identity, drop Drop) error
	// Presence reports, per window asked, the pages it holds a stripe of.
	Presence(ctx context.Context, disk rank.Identity, presence Presence) ([][]uint32, error)
}

func windowToWire(window rank.Window) *peerv1.Window {
	return peerv1.Window_builder{Vm: proto.String(window.Ref.VM), Sequence: proto.Uint64(window.Ref.Sequence),
		Volume: proto.String(window.Volume), Segment: proto.Bool(window.Segment),
		Number: proto.Uint64(window.Number), Pages: proto.Uint32(window.Pages)}.Build()
}

func windowFromWire(window *peerv1.Window) rank.Window {
	return rank.Window{Ref: control.Ref{VM: window.GetVm(), Sequence: window.GetSequence()},
		Volume: window.GetVolume(), Segment: window.GetSegment(), Number: window.GetNumber(), Pages: window.GetPages()}
}

func itemsToWire(items []StripeItem) []*peerv1.StripeItem {
	encoded := make([]*peerv1.StripeItem, 0, len(items))
	for _, item := range items {
		encoded = append(encoded, peerv1.StripeItem_builder{Page: proto.Uint32(item.Page),
			Index: proto.Uint32(uint32(item.Index)), Length: proto.Uint32(uint32(item.Length)),
			Size: proto.Uint32(uint32(item.Size))}.Build())
	}
	return encoded
}

// itemsFromWire reads items and checks they fill size bytes exactly.
func itemsFromWire(items []*peerv1.StripeItem, size int64) ([]StripeItem, error) {
	decoded := make([]StripeItem, 0, len(items))
	total := int64(0)
	for _, item := range items {
		decoded = append(decoded, StripeItem{Page: item.GetPage(), Index: int(item.GetIndex()),
			Length: int(item.GetLength()), Size: int(item.GetSize())})
		total += int64(item.GetSize())
	}
	if total != size {
		return nil, fmt.Errorf("%w: stripes of %d bytes in a payload of %d", wire.ErrMalformedFrame, total, size)
	}
	return decoded, nil
}

// pageBitmap is pages as a bitmap, least significant bit of byte 0 first.
func pageBitmap(pages []uint32) []byte {
	var bitmap []byte
	for _, page := range pages {
		for int(page/8) >= len(bitmap) {
			bitmap = append(bitmap, 0)
		}
		bitmap[page/8] |= 1 << (page % 8)
	}
	return bitmap
}

func bitmapPages(bitmap []byte) []uint32 {
	var pages []uint32
	for index, bits := range bitmap {
		for bit := range 8 {
			if bits&(1<<bit) != 0 {
				pages = append(pages, uint32(index*8+bit))
			}
		}
	}
	return pages
}

func codeFromWire(k, m uint32) rank.Code { return rank.Code{K: int(k), M: int(m)} }

func cacheStatus(status peerv1.CacheStatus) *peerv1.CacheStatus { return &status }

// admission is how a host answers one cache request: under which generation,
// for which of its disks, naming which assignment of it, and with what status
// when it does not answer it.
type admission struct {
	m          membership.Membership
	disk       rank.Identity
	generation uint64
	assigned   uint64
	// refused is the status of a request the host does not answer, nil for
	// one it does.
	refused *peerv1.CacheStatus
}

// admitCache decides how this host answers a request naming disk at generation. A
// host behind the request reads the membership first. It answers only under
// the request's generation, and only for a disk it keeps while that
// generation has it serve it. Every answer names the generation that
// assigned it the disk, as the last membership that had it serve the disk
// said.
func (s *Server) admitCache(disk []byte, generation uint64) admission {
	cache, source := s.config.Cache, s.config.Membership
	if cache == nil || source == nil {
		return admission{refused: notMe}
	}
	// The guard serves under whatever generation the host holds: it neither
	// reads a newer one nor tells the sender it is stale.
	stale := s.bug("membership-serve-stale-generation")
	m := source.Current()
	if m.Generation() < generation && !stale {
		caught, err := source.Catch(s.ctx, generation)
		m = caught
		if err != nil {
			slog.DebugContext(s.ctx, "peer: the membership could not be read for a request ahead of it",
				"generation", m.Generation(), "request", generation, "error", err)
		} else if m.Generation() >= generation {
			s.probe(membership.ProbeHolderCaughtUp)
		}
	}
	answered := admission{m: m, generation: m.Generation()}
	if m.Generation() != generation && !stale {
		s.probe(membership.ProbeStaleAnswered)
		answered.refused = cacheStatus(peerv1.CacheStatus_CACHE_STATUS_STALE)
		return answered
	}
	identity, named := identityOf(disk)
	if !named || !cache.Keeps(identity) {
		if !s.bug("peer-answer-for-another-cache") {
			s.probe(membership.ProbeNotServed)
			answered.refused = notMe
			return answered
		}
		// The guard answers for whichever disk the host keeps.
		if kept := cache.Disks(); len(kept) > 0 {
			identity = kept[0]
		}
	}
	answered.disk = identity
	answered.assigned = s.assignedOf(m, identity)
	if !m.Serves(s.config.Member, identity) && !s.bug("membership-serve-stale-assignment") {
		s.probe(membership.ProbeNotServed)
		answered.refused = notMe
	}
	return answered
}

// identityOf reads a disk's identity off the wire, and whether it names one.
func identityOf(disk []byte) (rank.Identity, bool) {
	var identity rank.Identity
	if len(disk) != len(identity) {
		return identity, false
	}
	copy(identity[:], disk)
	return identity, !identity.IsZero()
}

// assignedOf is the generation that assigned this host disk, as the last
// membership that had it serve the disk said, which m updates when it does:
// what every answer for the disk names. A host that lost the disk names it
// still, which no sender that knows of the loss accepts.
func (s *Server) assignedOf(m membership.Membership, disk rank.Identity) uint64 {
	s.assignedMu.Lock()
	defer s.assignedMu.Unlock()
	if m.Serves(s.config.Member, disk) {
		found, _ := m.Disk(disk)
		s.assigned[disk] = found.Assigned
	}
	return s.assigned[disk]
}

var notMe = cacheStatus(peerv1.CacheStatus_CACHE_STATUS_NOT_ME)

// answerReadStripes answers a read with the stripes the cache holds. The reply
// holds what it carries of its peer's budget until it has been sent.
func (s *Server) answerReadStripes(session *session, request *peerv1.ReadStripes) answer {
	admitted := s.admitCache(request.GetCache(), request.GetGeneration())
	refuse := func(status *peerv1.CacheStatus) answer {
		return answer{message: peerv1.Stripes_builder{Status: status, Generation: proto.Uint64(admitted.generation),
			Assigned: proto.Uint64(admitted.assigned)}.Build()}
	}
	if admitted.refused != nil {
		return refuse(admitted.refused)
	}
	maximum := int64(min(request.GetMaxBytes(), uint64(platform.MaxFrameBytes)))
	if maximum > 0 {
		// A read that wants bytes waits for none here: past this host's
		// serving bandwidth its reader is better served by another holder.
		if admitted, left := s.serving.admit(); !admitted && !s.bug("peer-serve-past-budget") {
			s.stripesBusy.Add(1)
			return answer{message: s.busy(session, maximum, -left)}
		}
	}
	release, busy := s.reserve(session, maximum)
	if busy != nil {
		return answer{message: busy}
	}
	read := StripeRead{Window: windowFromWire(request.GetWindow()), Code: codeFromWire(request.GetK(), request.GetM()),
		MaxBytes: maximum}
	if len(request.GetPages()) > 0 {
		read.Pages = bitmapPages(request.GetPages())
	}
	stripes, err := s.config.Cache.ReadStripes(s.ctx, admitted.m, admitted.disk, read)
	if err == nil && stripes.Size > maximum {
		err = fmt.Errorf("the cache answered %d bytes of a read of at most %d", stripes.Size, maximum)
	}
	if err != nil {
		release()
		if stripes.Release != nil {
			stripes.Release()
		}
		slog.WarnContext(s.ctx, "peer: the cache could not answer a read", "peer", session.peer, "error", err)
		return refuse(cacheStatus(peerv1.CacheStatus_CACHE_STATUS_UNSPECIFIED))
	}
	if len(stripes.Items) > 0 {
		s.serving.spend(stripes.Size)
		s.stripeReads.Add(1)
		s.stripes.Add(int64(len(stripes.Items)))
		s.stripeBytes.Add(stripes.Size)
	}
	return answer{
		message: peerv1.Stripes_builder{Status: cacheStatus(peerv1.CacheStatus_CACHE_STATUS_OK),
			Items: itemsToWire(stripes.Items), FillRight: proto.Bool(stripes.FillRight),
			Generation: proto.Uint64(admitted.generation), Assigned: proto.Uint64(admitted.assigned)}.Build(),
		body: stripes.Payload, size: stripes.Size,
		sent: func(bool) {
			release()
			if stripes.Release != nil {
				stripes.Release()
			}
		},
	}
}

// answerKeep writes the stripes a keep carries, or says it dropped them.
func (s *Server) answerKeep(session *session, request *peerv1.Keep, payload *payloadBuffer) answer {
	defer payload.release()
	admitted := s.admitCache(request.GetCache(), request.GetGeneration())
	kept := func(status *peerv1.CacheStatus) answer {
		return answer{message: peerv1.Kept_builder{Status: status, Generation: proto.Uint64(admitted.generation),
			Assigned: proto.Uint64(admitted.assigned)}.Build()}
	}
	if admitted.refused != nil {
		return kept(admitted.refused)
	}
	items, err := itemsFromWire(request.GetItems(), int64(len(payload.bytes)))
	if err != nil {
		return kept(cacheStatus(peerv1.CacheStatus_CACHE_STATUS_UNSPECIFIED))
	}
	// A keep holds its bytes of the peer's write budget while the cache
	// writes them; one over it is told so, and its sender drops it.
	release, busy := s.reserve(session, int64(len(payload.bytes)))
	if busy != nil {
		return answer{message: busy}
	}
	defer release()
	err = s.config.Cache.Keep(s.ctx, admitted.m, admitted.disk, Keep{Window: windowFromWire(request.GetWindow()),
		Code: codeFromWire(request.GetK(), request.GetM()), Items: items, Payload: payload.bytes,
		Repair: request.GetRepair(), Publication: request.GetPublication()})
	if err != nil {
		return kept(cacheStatus(peerv1.CacheStatus_CACHE_STATUS_DROPPED))
	}
	return kept(cacheStatus(peerv1.CacheStatus_CACHE_STATUS_OK))
}

func (s *Server) answerDrop(request *peerv1.Drop) answer {
	admitted := s.admitCache(request.GetCache(), request.GetGeneration())
	status := admitted.refused
	if status == nil {
		status = cacheStatus(peerv1.CacheStatus_CACHE_STATUS_OK)
		if err := s.config.Cache.Drop(s.ctx, admitted.disk, Drop{Window: windowFromWire(request.GetWindow()), Page: request.GetPage(),
			Index: int(request.GetIndex()), Code: codeFromWire(request.GetK(), request.GetM())}); err != nil {
			status = cacheStatus(peerv1.CacheStatus_CACHE_STATUS_UNSPECIFIED)
		}
	}
	return answer{message: peerv1.Dropped_builder{Status: status, Generation: proto.Uint64(admitted.generation),
		Assigned: proto.Uint64(admitted.assigned)}.Build()}
}

func (s *Server) answerPresence(request *peerv1.Presence) answer {
	admitted := s.admitCache(request.GetCache(), request.GetGeneration())
	present := func(status *peerv1.CacheStatus, bitmaps [][]byte) answer {
		return answer{message: peerv1.Present_builder{Status: status, Pages: bitmaps,
			Generation: proto.Uint64(admitted.generation), Assigned: proto.Uint64(admitted.assigned)}.Build()}
	}
	if admitted.refused != nil {
		return present(admitted.refused, nil)
	}
	windows := make([]rank.Window, 0, len(request.GetWindows()))
	for _, window := range request.GetWindows() {
		windows = append(windows, windowFromWire(window))
	}
	held, err := s.config.Cache.Presence(s.ctx, admitted.disk, Presence{Windows: windows, Code: codeFromWire(request.GetK(), request.GetM())})
	if err != nil || len(held) != len(windows) {
		return present(cacheStatus(peerv1.CacheStatus_CACHE_STATUS_UNSPECIFIED), nil)
	}
	bitmaps := make([][]byte, 0, len(held))
	for _, pages := range held {
		bitmaps = append(bitmaps, pageBitmap(pages))
	}
	return present(cacheStatus(peerv1.CacheStatus_CACHE_STATUS_OK), bitmaps)
}

// answerProbe says whether this host keeps the disk named, and names it; an
// empty name asks only whether the host is there, and is answered with the
// first disk the host keeps. A probe asks whether a host is there to be asked
// at all, under no generation, so it is answered by the disks alone.
func (s *Server) answerProbe(request *peerv1.Probe) answer {
	var kept []rank.Identity
	if s.config.Cache != nil {
		kept = s.config.Cache.Disks()
	}
	var identity []byte
	if len(kept) > 0 {
		identity = kept[0][:]
	}
	status := peerv1.CacheStatus_CACHE_STATUS_OK
	if named := request.GetCache(); len(named) > 0 {
		disk, ok := identityOf(named)
		switch {
		case ok && slices.Contains(kept, disk):
			identity = disk[:]
		case len(kept) > 0 && s.bug("peer-answer-for-another-cache"):
		default:
			status = peerv1.CacheStatus_CACHE_STATUS_NOT_ME
		}
	}
	return answer{message: peerv1.Probed_builder{Status: &status, Cache: identity}.Build()}
}

// replied is the error a cache's answer to a request routed by route means.
// An answer under another generation is stale whatever its status says, and
// one that names another assignment of the disk than the route's comes from a
// host that does not serve it under the route's membership.
func replied(route membership.Route, status peerv1.CacheStatus, generation, assigned uint64) error {
	switch status {
	case peerv1.CacheStatus_CACHE_STATUS_STALE:
		return &StaleError{Generation: generation}
	case peerv1.CacheStatus_CACHE_STATUS_NOT_ME:
		return ErrNotMe
	}
	if generation != route.Generation {
		return &StaleError{Generation: generation}
	}
	if assigned != route.Assigned {
		return fmt.Errorf("%w: it names the assignment of disk %s at generation %d, not %d", ErrNotMe, route.Disk,
			assigned, route.Assigned)
	}
	switch status {
	case peerv1.CacheStatus_CACHE_STATUS_OK:
		return nil
	case peerv1.CacheStatus_CACHE_STATUS_DROPPED:
		return ErrDropped
	default:
		return fmt.Errorf("peer: the cache could not answer: %s", status)
	}
}

// StripesReply is what a peer's cache sent of a StripeRead. Payload holds the
// items in order; Release gives its buffer back.
type StripesReply struct {
	Items     []StripeItem
	Payload   []byte
	FillRight bool
	buffer    *payloadBuffer
}

// Release gives the reply's buffer back. Its payload must not be used after.
func (r StripesReply) Release() { r.buffer.release() }

// ReadStripes asks the peer for the stripes the disk route names holds of
// read, under the route's generation. It is of the Stripe class unless ctx
// says otherwise, so it goes over connections no page reply holds up, and
// within that class's budget at the peer, which bounds the stripe bytes this
// host has in flight there. A peer marked down is not asked: another holder
// has the stripes, or the store does. Every read of this host's also waits
// for room under its bound on stripe bytes in flight at all its peers.
func (p *Peer) ReadStripes(ctx context.Context, route membership.Route, read StripeRead) (StripesReply, error) {
	if p.Down() {
		p.table.probe(ProbeSkippedDown)
		return StripesReply{}, ErrDown
	}
	class := Stripe
	if p.table.bug("peer-stripes-in-fault-class") {
		class = Fault
	}
	ctx = WithClass(ctx, classOr(ctx, class))
	request := peerv1.ReadStripes_builder{Cache: route.Disk[:], Window: windowToWire(read.Window),
		Pages: pageBitmap(read.Pages), K: proto.Uint32(uint32(read.Code.K)), M: proto.Uint32(uint32(read.Code.M)),
		MaxBytes: proto.Uint64(uint64(read.MaxBytes)), Generation: proto.Uint64(route.Generation)}.Build()
	response := new(peerv1.Stripes)
	if !p.table.bug("peer-unbounded-stripes") {
		if err := p.table.stripes.Acquire(ctx, Resident, read.MaxBytes); err != nil {
			return StripesReply{}, err
		}
		defer p.table.stripes.Release(read.MaxBytes)
	}
	got, _, err := p.call(ctx, "", request, response, read.MaxBytes, read.MaxBytes, nil)
	if err != nil {
		return StripesReply{}, err
	}
	if err := replied(route, response.GetStatus(), response.GetGeneration(), response.GetAssigned()); err != nil {
		got.payload.release()
		return StripesReply{}, err
	}
	items, err := itemsFromWire(response.GetItems(), int64(len(got.payload.bytes)))
	if err != nil {
		got.payload.release()
		return StripesReply{}, err
	}
	return StripesReply{Items: items, Payload: got.payload.bytes, FillRight: response.GetFillRight(),
		buffer: got.payload}, nil
}

// Keep asks the peer to write stripes to the disk route names, under the
// route's generation. A keep is bulk write, and nothing waits on it: one over
// this host's background budget, or one the cache does not write, is
// ErrDropped, never queued.
func (p *Peer) Keep(ctx context.Context, route membership.Route, keep Keep) error {
	if p.Down() {
		p.table.probe(ProbeSkippedDown)
		return ErrDown
	}
	size := int64(len(keep.Payload))
	priority := Fill
	if keep.Repair {
		priority = Repair
	}
	if p.table.bug("peer-queue-keeps") {
		if err := p.table.background.Acquire(ctx, Resident, size); err != nil {
			return err
		}
	} else if !p.table.background.TryAcquire(priority, size) {
		return fmt.Errorf("%w: %w", ErrDropped, ErrNoRoom)
	}
	defer p.table.background.Release(size)
	request := peerv1.Keep_builder{Cache: route.Disk[:], Window: windowToWire(keep.Window),
		K: proto.Uint32(uint32(keep.Code.K)), M: proto.Uint32(uint32(keep.Code.M)), Items: itemsToWire(keep.Items),
		Repair: proto.Bool(keep.Repair), Publication: proto.Bool(keep.Publication),
		Generation: proto.Uint64(route.Generation)}.Build()
	response := new(peerv1.Kept)
	got, _, err := p.call(WithClass(ctx, BulkWrite), "", request, response, size, 0, keep.Payload)
	if errors.Is(err, ErrBusy) {
		// A cache at its write budget for this host drops the keep, as one
		// whose own budget is spent does.
		return errors.Join(ErrDropped, err)
	}
	if err != nil {
		return err
	}
	got.payload.release()
	return replied(route, response.GetStatus(), response.GetGeneration(), response.GetAssigned())
}

// Drop tells the peer to forget a stripe of the disk route names, under the
// route's generation.
func (p *Peer) Drop(ctx context.Context, route membership.Route, drop Drop) error {
	request := peerv1.Drop_builder{Cache: route.Disk[:], Window: windowToWire(drop.Window), Page: proto.Uint32(drop.Page),
		Index: proto.Uint32(uint32(drop.Index)), K: proto.Uint32(uint32(drop.Code.K)),
		M: proto.Uint32(uint32(drop.Code.M)), Generation: proto.Uint64(route.Generation)}.Build()
	response := new(peerv1.Dropped)
	got, _, err := p.call(WithClass(ctx, Fault), "", request, response, 0, 0, nil)
	if err != nil {
		return err
	}
	got.payload.release()
	return replied(route, response.GetStatus(), response.GetGeneration(), response.GetAssigned())
}

// Presence asks the peer which pages of some windows the disk route names
// holds a stripe of, under the route's generation.
func (p *Peer) Presence(ctx context.Context, route membership.Route, presence Presence) ([][]uint32, error) {
	windows := make([]*peerv1.Window, 0, len(presence.Windows))
	for _, window := range presence.Windows {
		windows = append(windows, windowToWire(window))
	}
	request := peerv1.Presence_builder{Cache: route.Disk[:], Windows: windows, K: proto.Uint32(uint32(presence.Code.K)),
		M: proto.Uint32(uint32(presence.Code.M)), Generation: proto.Uint64(route.Generation)}.Build()
	response := new(peerv1.Present)
	got, _, err := p.call(WithClass(ctx, Fault), "", request, response, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	got.payload.release()
	if err := replied(route, response.GetStatus(), response.GetGeneration(), response.GetAssigned()); err != nil {
		return nil, err
	}
	if len(response.GetPages()) != len(presence.Windows) {
		return nil, fmt.Errorf("%w: presence of %d windows for %d asked", wire.ErrMalformedFrame,
			len(response.GetPages()), len(presence.Windows))
	}
	held := make([][]uint32, 0, len(response.GetPages()))
	for _, bitmap := range response.GetPages() {
		held = append(held, bitmapPages(bitmap))
	}
	return held, nil
}

// Probe asks whether the peer is there and keeps disk: ErrNotMe when it keeps
// another at its address. The zero identity asks only whether the host is
// there. It reports the identity of the disk the peer keeps.
func (p *Peer) Probe(ctx context.Context, disk rank.Identity) (rank.Identity, error) {
	request := peerv1.Probe_builder{}.Build()
	if !disk.IsZero() {
		request.SetCache(disk[:])
	}
	response := new(peerv1.Probed)
	got, _, err := p.call(WithClass(ctx, Fault), "", request, response, 0, 0, nil)
	if err != nil {
		return rank.Identity{}, err
	}
	got.payload.release()
	var held rank.Identity
	copy(held[:], response.GetCache())
	switch response.GetStatus() {
	case peerv1.CacheStatus_CACHE_STATUS_OK:
		return held, nil
	case peerv1.CacheStatus_CACHE_STATUS_NOT_ME:
		return held, ErrNotMe
	default:
		return held, fmt.Errorf("peer: the cache could not answer: %s", response.GetStatus())
	}
}
