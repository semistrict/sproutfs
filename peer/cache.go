package peer

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/semistrict/sproutfs/control"
	peerv1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/peer/v1"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
	"google.golang.org/protobuf/proto"
)

// The requests of the cluster's disk cache (plans/disk-cache-2026-10-02.md):
// read the stripes a cache holds of a window, keep stripes, drop a stripe found
// wrong, ask which pages a cache holds, and probe it. This package carries them
// and answers them through a Cache; the checkpoint cache is what implements it.
//
// Every request names the cache it expects, by its identity in the list of
// caches. An address can come to belong to another cache — a pod that took
// the address of one that left — so a cache that is not the one named answers
// ErrNotMe, as does a host that keeps no cache. That is a stale list, never a
// down host.

var (
	// ErrNotMe reports a host that is not the cache a request named.
	ErrNotMe = errors.New("peer: the host is not the cache the request named")
	// ErrDropped reports a keep the cache did not write: it holds the stripe
	// already, or its budget for writes is spent, or this host's background
	// budget was. Nothing waits on a keep, so it is dropped, not queued.
	ErrDropped = errors.New("peer: the keep was dropped")
)

// StripeItem is one stripe a payload carries: the page of the window it belongs
// to, its index in the code, the length of the envelope it was cut from, and its
// size in the payload, where the items lie in order. Its bytes are stored as
// they go on the wire and carry a checksum of their own, so a frame of stripes
// carries no checksum of its payload.
type StripeItem struct {
	Page   uint32
	Index  int
	Length int
	Size   int
}

// StripeRead asks for every stripe a cache holds of some pages of one window,
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

// Keep asks a cache to write stripes. Payload holds the items in order.
type Keep struct {
	Window  rank.Window
	Code    rank.Code
	Items   []StripeItem
	Payload []byte
	Repair  bool
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

// Cache is what a host's disk cache answers its peers with. Every method is
// asked for the cache the request named, and only then.
type Cache interface {
	// Identity is the cache's identity in the list of caches.
	Identity() rank.Identity
	ReadStripes(ctx context.Context, read StripeRead) (Stripes, error)
	// Keep writes stripes, or reports ErrDropped when it does not.
	Keep(ctx context.Context, keep Keep) error
	Drop(ctx context.Context, drop Drop) error
	// Presence reports, per window asked, the pages it holds a stripe of.
	Presence(ctx context.Context, presence Presence) ([][]uint32, error)
}

func windowToWire(window rank.Window) *peerv1.Window {
	return peerv1.Window_builder{Vm: proto.String(window.Ref.VM), Sequence: proto.Uint64(window.Ref.Sequence),
		Volume: proto.String(window.Volume), Segment: proto.Bool(window.Segment),
		Number: proto.Uint64(window.Number)}.Build()
}

func windowFromWire(window *peerv1.Window) rank.Window {
	return rank.Window{Ref: control.Ref{VM: window.GetVm(), Sequence: window.GetSequence()},
		Volume: window.GetVolume(), Segment: window.GetSegment(), Number: window.GetNumber()}
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

// named reports whether the request named this host's cache.
func (s *Server) named(cache []byte) bool {
	if s.config.Cache == nil {
		return false
	}
	identity := s.config.Cache.Identity()
	return string(cache) == string(identity[:]) || s.bug("peer-answer-for-another-cache")
}

var notMe = cacheStatus(peerv1.CacheStatus_CACHE_STATUS_NOT_ME)

// answerReadStripes answers a read with the stripes the cache holds. The reply
// holds what it carries of its peer's budget until it has been sent.
func (s *Server) answerReadStripes(session *session, request *peerv1.ReadStripes) answer {
	if !s.named(request.GetCache()) {
		return answer{message: peerv1.Stripes_builder{Status: notMe}.Build()}
	}
	maximum := int64(min(request.GetMaxBytes(), uint64(platform.MaxFrameBytes)))
	release, busy := s.reserve(session, maximum)
	if busy != nil {
		return answer{message: busy}
	}
	read := StripeRead{Window: windowFromWire(request.GetWindow()), Code: codeFromWire(request.GetK(), request.GetM()),
		MaxBytes: maximum}
	if len(request.GetPages()) > 0 {
		read.Pages = bitmapPages(request.GetPages())
	}
	stripes, err := s.config.Cache.ReadStripes(s.ctx, read)
	if err == nil && stripes.Size > maximum {
		err = fmt.Errorf("the cache answered %d bytes of a read of at most %d", stripes.Size, maximum)
	}
	if err != nil {
		release()
		if stripes.Release != nil {
			stripes.Release()
		}
		return answer{message: peerv1.Stripes_builder{Status: cacheStatus(peerv1.CacheStatus_CACHE_STATUS_UNSPECIFIED)}.Build(),
			sent: nil}
	}
	return answer{
		message: peerv1.Stripes_builder{Status: cacheStatus(peerv1.CacheStatus_CACHE_STATUS_OK),
			Items: itemsToWire(stripes.Items), FillRight: proto.Bool(stripes.FillRight)}.Build(),
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
	if !s.named(request.GetCache()) {
		return answer{message: peerv1.Kept_builder{Status: notMe}.Build()}
	}
	items, err := itemsFromWire(request.GetItems(), int64(len(payload.bytes)))
	if err != nil {
		return answer{message: peerv1.Kept_builder{Status: cacheStatus(peerv1.CacheStatus_CACHE_STATUS_UNSPECIFIED)}.Build()}
	}
	// A keep holds its bytes of the peer's write budget while the cache
	// writes them; one over it is told so, and its sender drops it.
	release, busy := s.reserve(session, int64(len(payload.bytes)))
	if busy != nil {
		return answer{message: busy}
	}
	defer release()
	err = s.config.Cache.Keep(s.ctx, Keep{Window: windowFromWire(request.GetWindow()),
		Code: codeFromWire(request.GetK(), request.GetM()), Items: items, Payload: payload.bytes,
		Repair: request.GetRepair()})
	status := peerv1.CacheStatus_CACHE_STATUS_OK
	if err != nil {
		status = peerv1.CacheStatus_CACHE_STATUS_DROPPED
	}
	return answer{message: peerv1.Kept_builder{Status: &status}.Build()}
}

func (s *Server) answerDrop(request *peerv1.Drop) answer {
	if !s.named(request.GetCache()) {
		return answer{message: peerv1.Dropped_builder{Status: notMe}.Build()}
	}
	status := peerv1.CacheStatus_CACHE_STATUS_OK
	if err := s.config.Cache.Drop(s.ctx, Drop{Window: windowFromWire(request.GetWindow()), Page: request.GetPage(),
		Index: int(request.GetIndex()), Code: codeFromWire(request.GetK(), request.GetM())}); err != nil {
		status = peerv1.CacheStatus_CACHE_STATUS_UNSPECIFIED
	}
	return answer{message: peerv1.Dropped_builder{Status: &status}.Build()}
}

func (s *Server) answerPresence(request *peerv1.Presence) answer {
	if !s.named(request.GetCache()) {
		return answer{message: peerv1.Present_builder{Status: notMe}.Build()}
	}
	windows := make([]rank.Window, 0, len(request.GetWindows()))
	for _, window := range request.GetWindows() {
		windows = append(windows, windowFromWire(window))
	}
	held, err := s.config.Cache.Presence(s.ctx, Presence{Windows: windows, Code: codeFromWire(request.GetK(), request.GetM())})
	if err != nil || len(held) != len(windows) {
		return answer{message: peerv1.Present_builder{Status: cacheStatus(peerv1.CacheStatus_CACHE_STATUS_UNSPECIFIED)}.Build()}
	}
	bitmaps := make([][]byte, 0, len(held))
	for _, pages := range held {
		bitmaps = append(bitmaps, pageBitmap(pages))
	}
	return answer{message: peerv1.Present_builder{Status: cacheStatus(peerv1.CacheStatus_CACHE_STATUS_OK),
		Pages: bitmaps}.Build()}
}

// answerProbe says whether this host keeps the cache named; an empty name
// asks only whether the host is there.
func (s *Server) answerProbe(request *peerv1.Probe) answer {
	var identity []byte
	if s.config.Cache != nil {
		own := s.config.Cache.Identity()
		identity = own[:]
	}
	status := peerv1.CacheStatus_CACHE_STATUS_OK
	if len(request.GetCache()) > 0 && !s.named(request.GetCache()) {
		status = peerv1.CacheStatus_CACHE_STATUS_NOT_ME
	}
	return answer{message: peerv1.Probed_builder{Status: &status, Cache: identity}.Build()}
}

// cacheError is the error a cache's status means.
func cacheError(status peerv1.CacheStatus) error {
	switch status {
	case peerv1.CacheStatus_CACHE_STATUS_OK:
		return nil
	case peerv1.CacheStatus_CACHE_STATUS_NOT_ME:
		return ErrNotMe
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

// ReadStripes asks the peer's cache, which must be cache, for the stripes it
// holds of read. It is a Fault unless ctx says otherwise. A peer marked down is
// not asked: another holder has the stripes, or the store does. Every read of
// this host's waits for room under its bound on stripe bytes in flight.
func (p *Peer) ReadStripes(ctx context.Context, cache rank.Identity, read StripeRead) (StripesReply, error) {
	if p.Down() {
		p.table.probe(ProbeSkippedDown)
		return StripesReply{}, ErrDown
	}
	request := peerv1.ReadStripes_builder{Cache: cache[:], Window: windowToWire(read.Window),
		Pages: pageBitmap(read.Pages), K: proto.Uint32(uint32(read.Code.K)), M: proto.Uint32(uint32(read.Code.M)),
		MaxBytes: proto.Uint64(uint64(read.MaxBytes))}.Build()
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
	if err := cacheError(response.GetStatus()); err != nil {
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

// Keep asks the peer's cache, which must be cache, to write stripes. A keep is
// bulk write, and nothing waits on it: one over this host's background budget,
// or one the cache does not write, is ErrDropped, never queued.
func (p *Peer) Keep(ctx context.Context, cache rank.Identity, keep Keep) error {
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
		return ErrDropped
	}
	defer p.table.background.Release(size)
	request := peerv1.Keep_builder{Cache: cache[:], Window: windowToWire(keep.Window),
		K: proto.Uint32(uint32(keep.Code.K)), M: proto.Uint32(uint32(keep.Code.M)), Items: itemsToWire(keep.Items),
		Repair: proto.Bool(keep.Repair)}.Build()
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
	return cacheError(response.GetStatus())
}

// Drop tells the peer's cache, which must be cache, to forget a stripe.
func (p *Peer) Drop(ctx context.Context, cache rank.Identity, drop Drop) error {
	request := peerv1.Drop_builder{Cache: cache[:], Window: windowToWire(drop.Window), Page: proto.Uint32(drop.Page),
		Index: proto.Uint32(uint32(drop.Index)), K: proto.Uint32(uint32(drop.Code.K)),
		M: proto.Uint32(uint32(drop.Code.M))}.Build()
	response := new(peerv1.Dropped)
	got, _, err := p.call(WithClass(ctx, Fault), "", request, response, 0, 0, nil)
	if err != nil {
		return err
	}
	got.payload.release()
	return cacheError(response.GetStatus())
}

// Presence asks the peer's cache, which must be cache, which pages of some
// windows it holds a stripe of.
func (p *Peer) Presence(ctx context.Context, cache rank.Identity, presence Presence) ([][]uint32, error) {
	windows := make([]*peerv1.Window, 0, len(presence.Windows))
	for _, window := range presence.Windows {
		windows = append(windows, windowToWire(window))
	}
	request := peerv1.Presence_builder{Cache: cache[:], Windows: windows, K: proto.Uint32(uint32(presence.Code.K)),
		M: proto.Uint32(uint32(presence.Code.M))}.Build()
	response := new(peerv1.Present)
	got, _, err := p.call(WithClass(ctx, Fault), "", request, response, 0, 0, nil)
	if err != nil {
		return nil, err
	}
	got.payload.release()
	if err := cacheError(response.GetStatus()); err != nil {
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

// Probe asks whether the peer is there and keeps cache: ErrNotMe when another
// cache answers at its address. The zero identity asks only whether the host
// is there. It reports the identity the peer keeps.
func (p *Peer) Probe(ctx context.Context, cache rank.Identity) (rank.Identity, error) {
	request := peerv1.Probe_builder{}.Build()
	if !cache.IsZero() {
		request.SetCache(cache[:])
	}
	response := new(peerv1.Probed)
	got, _, err := p.call(WithClass(ctx, Fault), "", request, response, 0, 0, nil)
	if err != nil {
		return rank.Identity{}, err
	}
	got.payload.release()
	var held rank.Identity
	copy(held[:], response.GetCache())
	return held, cacheError(response.GetStatus())
}
