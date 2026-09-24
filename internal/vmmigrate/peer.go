package vmmigrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmmigrate/internal/peer"
	"github.com/semistrict/sproutfs/internal/vmmigrate/internal/wire"
	"github.com/semistrict/sproutfs/internal/volume"
)

// Dialer opens one connection to a peer's page source. A production dialer is
// plain TCP on the deployment's trusted network.
type Dialer = peer.Dialer

// Admitter orders a destination's decision to ask its source for pages against
// everything else a controlled run is running, named by the region it is for.
type Admitter = peer.Admitter

// WithAdmission installs admit for every request the regions of every VM
// received under ctx make to their source, including the ones the post-copy
// stream makes behind the running guest. A deployment installs none.
//
// A destination whose stream is cancelled between two requests may either send
// the next one and have it refused on the wire or abandon it before anything is
// sent. Both leave the guest correct — every page it did not fetch is in its own
// checkpoint — but they are different schedules, and a pooled connection is
// taken without dialing, so nothing an adapter admits lies between the
// cancellation and the send. This is that point.
func WithAdmission(ctx context.Context, admit Admitter) context.Context {
	return peer.WithAdmission(ctx, admit)
}

// PeerConfig binds one volume of a migrated VM to the host that still holds its
// pages.
type PeerConfig struct {
	// Volume is the destination's own handle on the volume, which answers
	// everything the peer cannot.
	Volume *volume.Volume
	// Peer is the source host's page source and VM the migrated VM's identity.
	Peer platform.Address
	VM   string
	// Unpublished names the pages the source holds that no checkpoint has, from
	// the handoff. They are the guest's writes since the source's last checkpoint:
	// the volume reports them as the checkpoint's bytes or as holes, and reading
	// them from it would silently rewind the guest, so this backing reports them
	// as bytes of its own that the pager must load and hold privately.
	Unpublished []PageRun
	// Selected is the sequence of this VM's own checkpoint the handoff was taken
	// against, which for a migration is the one the source's record selected
	// when it gave the VM up. Every checkpoint of this VM up to and including it
	// predates the pages Unpublished names; everything this VM publishes after
	// receiving is newer than them. It is zero for a fork, whose child has
	// published nothing and whose inherited pages name its parent instead.
	Selected uint64
	// PageSize is the page this region's numbers are counted in, which is the
	// volume's own and therefore the source's too: both hosts read it out of the
	// same durable geometry. Zero takes it from Volume, which is what every
	// caller but a scaled-model test wants.
	PageSize int
	Dial     Dialer
	// MaxConnections bounds this region's requests in flight, four by default.
	// The post-copy stream uses all but one, which is kept for guest faults.
	MaxConnections int
	// MaxPagesPerRequest bounds one request: one 2 MiB production page by
	// default, or at most 256 smaller model pages within 2 MiB.
	MaxPagesPerRequest int
	// Clock is what the wait between two asks of a busy source runs on. Nil is
	// the wall clock; a simulation passes its own so that a destination backing
	// off from a saturated source does so in simulated time.
	Clock platform.Clock
}

// PeerStats reports where one region's pages came from.
type PeerStats struct {
	// PeerPages is the pages the source host served and VolumePages the pages
	// read from this host's own volume, whether because the source did not hold
	// them or because it stopped serving.
	PeerPages, VolumePages int64
	// Requests is every request sent to the source.
	Requests int64
	// Refusals is every reply that said the source was at its budget for this
	// peer, each of which was asked again rather than read from the volume.
	Refusals int64
	// Stalls is every read of pages only the source has that went on waiting
	// for it past a few seconds, which is a guest thread stopped for that long.
	Stalls int64
	// Unfetched is how many pages no checkpoint holds are still only on the
	// source, and Fetched how many of them it has served.
	Unfetched int
	Fetched   int64
	// FellBack reports that this region will never ask the source again.
	FellBack bool
	// Latency is how long this region's requests to the source took, guest
	// faults and the post-copy stream apart.
	Latency RequestLatency
}

// RequestLatency is how long requests to a migration's source took: a guest
// fault's and the post-copy stream's, each in total and waiting for a
// connection.
type RequestLatency = peer.Latency

// PeerBacking is a pager backing whose loads ask the source host of a migration
// first and read the destination's own volume for everything it does not hold.
// Locating, writing and flushing are the volume's alone: the peer holds bytes,
// never authority.
//
// One rule decides how long it asks. A page only the source holds is asked for
// until it arrives or until something that knows says the source is gone, and
// two things know: the source itself, answering that it no longer serves this
// VM, which it does only after a release it agreed to; and the orchestrator,
// ending the migration by discarding the received VM, which closes this backing.
// Everything else — a reset connection, a timeout, a listener restarting, a
// connection dropped by a budget, a BUSY reply — is retried with the same
// backoff for as long as this backing lives. Nothing here can tell a source that
// stumbled from one that died, and the two answers are opposite: the volume
// holds the checkpoint's bytes, which the guest has already written past.
//
// A page a checkpoint does hold is a page this host can read either way, so a
// request for a run of them that failed on the wire is not worth waiting on: it
// is read from the volume this time and decides nothing for the next one.
//
// Two answers are neither a source that is gone nor one that stumbled: a source
// serving pages of another size, and a reply this host cannot read. They are a
// source this destination cannot use at all, so the fault fails with that cause
// and the received VM is torn, exactly as a torn post-copy is handled anyway.
//
// Falling back for good is silent after one line, and a source that says it no
// longer serves the VM is what reaches it — as is closing this backing, which
// says the same thing from this side. Everything still works then, from the
// volume, which is where the bytes are anyway — except for the pages no
// checkpoint holds. Those are not in the volume and never will be: a load of one
// the source never served fails rather than handing the guest the checkpoint it
// has already written past.
type PeerBacking struct {
	config PeerConfig
	// unpublished is the handoff's set as a lookup, fixed for this backing's
	// life: the guest was stopped when it was taken. What it decides is not
	// fixed — a page of it stops being reported as this region's own once a
	// checkpoint of this VM holds it, which Locate reads off the volume.
	unpublished map[uint64]bool

	// source is the host that still holds these pages, and the connections this
	// region asks it over.
	source *peer.Source

	// mu guards unfetched, which is the part of unpublished the source has not
	// served yet. It only ever shrinks, and empties when the source may stop
	// serving this region.
	mu        sync.Mutex
	unfetched map[uint64]struct{}

	fallen   atomic.Bool
	once     sync.Once
	served   atomic.Int64
	fromDisk atomic.Int64
	refusals atomic.Int64
	fetched  atomic.Int64
	stalls   atomic.Int64

	// life ends when this backing does, which is this region's half of the
	// migration ending: a fault waiting for a source that never answered ends
	// there, because nothing else would ever end it. Every request this backing
	// sends is made under it as well as under its caller's own context.
	life    context.Context
	endLife context.CancelCauseFunc
}

var _ vmmemory.UnpublishedInstaller = (*PeerBacking)(nil)

// ErrNotServed reports a source that does not serve this VM any more.
var ErrNotServed = peer.ErrNotServed

// ErrUnpublishedLost reports a page the handoff named as the source's own that
// the source has not served. No checkpoint holds it, so there is nothing to read
// instead: the destination's volume holds the bytes before the guest wrote them,
// and substituting those would silently rewind the guest.
var ErrUnpublishedLost = errors.New("vmmigrate: the source has not served a page no checkpoint holds")

const (
	// busyDelay is how long a destination waits before asking a busy source for
	// a page no checkpoint holds again, doubling up to busyDelayMax. The source
	// is at its budget for this peer, which a drain makes the normal state, so
	// this is a queue rather than a failure.
	busyDelay    = time.Millisecond
	busyDelayMax = 100 * time.Millisecond
	// stalledAsk is how long one ask may go on before it is worth a line. A
	// page no checkpoint holds is asked for until it arrives, so a guest fault
	// behind one of them waits exactly as long as the source takes; nothing
	// else on either host says that is happening, and a host whose log says
	// nothing about a guest that has stopped answering is the one thing this
	// package can be asked about and cannot answer.
	stalledAsk = 5 * time.Second
)

// NewPeerBacking binds one volume to the source host that still holds its pages.
func NewPeerBacking(config PeerConfig) (*PeerBacking, error) {
	if config.Volume == nil || config.Peer == "" || config.VM == "" || config.Dial == nil {
		return nil, fmt.Errorf("%w: a peer backing needs a volume, a peer, a VM and a dialer", ErrInvalid)
	}
	if config.PageSize == 0 {
		config.PageSize = int(config.Volume.PageSize())
	}
	if config.MaxConnections == 0 {
		config.MaxConnections = 4
	}
	if config.MaxPagesPerRequest == 0 {
		config.MaxPagesPerRequest = max(1, min(defaultMaxPages, requestBytes/config.PageSize))
	}
	config.Clock = platform.ClockOr(config.Clock)
	if config.PageSize < 512 || config.PageSize > blob.MaxSize || config.MaxConnections < 1 || config.MaxPagesPerRequest < 1 || config.MaxPagesPerRequest > blob.MaxSize/config.PageSize {
		return nil, fmt.Errorf("%w: invalid peer backing budgets", ErrInvalid)
	}
	life, endLife := context.WithCancelCause(context.Background())
	b := &PeerBacking{config: config,
		unpublished: make(map[uint64]bool),
		unfetched:   make(map[uint64]struct{}),
		life:        life, endLife: endLife,
		source: peer.New(peer.Config{Peer: config.Peer, VM: config.VM,
			Volume: config.Volume.Name(), PageSize: config.PageSize,
			MaxConnections: config.MaxConnections, MaxRuns: defaultMaxRuns, Dial: config.Dial,
			Clock: config.Clock})}
	for _, run := range config.Unpublished {
		for page := run.First; page < run.First+uint64(run.Count); page++ {
			b.unpublished[page] = true
			b.unfetched[page] = struct{}{}
		}
	}
	return b, nil
}

func (b *PeerBacking) Size() uint64 { return b.config.Volume.Size() }

// PageSize is the volume's own, which is what this backing's page numbers are
// in: it stands in front of the volume rather than changing its geometry, so a
// pager whose page is a different size refuses it exactly as it refuses the
// volume.
func (b *PeerBacking) PageSize() uint64                 { return b.config.Volume.PageSize() }
func (b *PeerBacking) Verify(ctx context.Context) error { return b.config.Volume.Verify(ctx) }

// Locate reports the volume's own identities everywhere except the pages whose
// bytes no checkpoint of this VM holds. Those it reports as bytes of this region
// alone — no reference, so no page of them is ever shared and none of them is
// taken for a hole — which is what makes the pager load them through Load, where
// the source answers, rather than resolve them against a checkpoint that does
// not have them.
//
// One rule decides it, and it is about which checkpoint rather than about time
// or about whose VM it is: a page of the handoff's set is stripped for exactly
// as long as the checkpoint the volume names for it predates the handoff. That
// is any checkpoint of another VM — what a fork child inherits from its parent —
// and any checkpoint of this VM's own up to the one the handoff selected, which
// is what a migration's unpublished pages were written past.
//
// Both halves are load-bearing, in opposite directions. A migration keeps the
// same VM, and its volume names that VM's own pre-handoff checkpoint for exactly
// these pages: resolving one would hand the guest bytes from before its own
// write. A fork child publishes its inherited pages itself within a second of
// starting, under a sequence of its own above the selected one: going on
// stripping those tells the pager a page it has just published has no object,
// and the pager's retire then gives up the guest's only copy of those bytes.
func (b *PeerBacking) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	extents, err := b.config.Volume.Locate(ctx, offset, length)
	if err != nil || len(b.unpublished) == 0 {
		return extents, err
	}
	size := uint64(b.config.PageSize)
	var result []control.Extent
	add := func(next control.Extent) {
		if n := len(result); n > 0 && result[n-1].Identity == next.Identity && result[n-1].Offset+result[n-1].Length == next.Offset {
			result[n-1].Length += next.Length
			return
		}
		result = append(result, next)
	}
	for _, extent := range extents {
		end := extent.Offset + extent.Length
		for cursor := extent.Offset; cursor < end; {
			page := cursor / size
			stop := min(end, (page+1)*size)
			if b.unpublished[page] && b.predatesHandoff(extent.Identity.Ref) {
				add(control.Extent{Offset: cursor, Length: stop - cursor})
			} else {
				add(control.Extent{Offset: cursor, Length: stop - cursor, Identity: extent.Identity})
			}
			cursor = stop
		}
	}
	return result, nil
}

// predatesHandoff reports a checkpoint reference older than the pages this
// backing's handoff named: another VM's, or this VM's own from at or before the
// checkpoint the handoff selected. A page the volume names by one of those is a
// page the guest has already written past. See Locate.
func (b *PeerBacking) predatesHandoff(ref control.Ref) bool {
	return ref.VM != b.config.VM || ref.Sequence <= b.config.Selected
}

func (b *PeerBacking) Stats() PeerStats {
	return PeerStats{PeerPages: b.served.Load(), VolumePages: b.fromDisk.Load(),
		Requests: b.source.Requests(), Refusals: b.refusals.Load(), Stalls: b.stalls.Load(),
		Unfetched: b.Unfetched(), Fetched: b.fetched.Load(), FellBack: b.gone(),
		Latency: b.source.Latency()}
}

// Unfetched reports how many pages no checkpoint holds are still only on the
// source. The source may stop serving this region when it reaches zero.
func (b *PeerBacking) Unfetched() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.unfetched)
}

// Concurrency is how many requests the post-copy stream may have in flight for
// this region at once: every connection but the one kept for guest faults.
func (b *PeerBacking) Concurrency() int { return b.source.Concurrency() }

// onlyOnSource reports the first page of a run whose bytes are still only on the
// source, which is a page no fallback may answer for.
func (b *PeerBacking) onlyOnSource(first uint64, count int) (uint64, bool) {
	if len(b.unpublished) == 0 {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.unfetched) == 0 {
		return 0, false
	}
	for page := first; page < first+uint64(count); page++ {
		if _, only := b.unfetched[page]; only {
			return page, true
		}
	}
	return 0, false
}

// InstalledUnpublished records the pages of one load this host went on to hold
// as its own dirty state. Those are the pages whose only copy was the source's
// and now is not: they stop holding a fallback up and the source comes closer
// to being releasable.
//
// A load is not an install. The pager fills a buffer and may still drop the
// page — a read-ahead page on a full dirty budget is the ordinary way — and
// those bytes are then nowhere: not in a page here, not in the volume, whose
// bytes predate the guest's write. Such a page stays the source's alone, so it
// is asked for again and a read that cannot get it fails loudly.
func (b *PeerBacking) InstalledUnpublished(offset uint64, installed []bool) {
	b.took(offset/uint64(b.config.PageSize), installed)
}

// took records the pages of one run this host now holds, so they stop holding a
// fallback up and the source comes closer to being releasable.
func (b *PeerBacking) took(first uint64, served []bool) {
	if len(b.unpublished) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for index, arrived := range served {
		page := first + uint64(index)
		if _, only := b.unfetched[page]; !arrived || !only {
			continue
		}
		delete(b.unfetched, page)
		b.fetched.Add(1)
	}
}

// lost names a page the source holds alone and has not served.
func (b *PeerBacking) lost(page uint64) error {
	return fmt.Errorf("%w: %s page %d", ErrUnpublishedLost, b.config.Volume.Name(), page)
}

// fromVolume reads a range this backing will not ask the source for, refusing
// any page whose only copy is still the source's.
func (b *PeerBacking) fromVolume(ctx context.Context, offset uint64, dst []byte) error {
	size := uint64(b.config.PageSize)
	first := offset / size
	count := int((offset%size + uint64(len(dst)) + size - 1) / size)
	if page, only := b.onlyOnSource(first, count); only {
		return b.lost(page)
	}
	b.fromDisk.Add(int64(count))
	return b.config.Volume.Load(ctx, offset, dst)
}

// Load fills dst from the source host where it can and from the volume where it
// cannot. A whole-page range is what the pager asks for; anything else is the
// volume's alone.
func (b *PeerBacking) Load(ctx context.Context, offset uint64, dst []byte) error {
	_, err := b.LoadUnpublished(ctx, offset, dst)
	return err
}

// LoadUnpublished is Load, reporting which of the pages it filled the source
// host served out of its own dirty pages. Those bytes are the guest's state
// since the source's last checkpoint and no checkpoint has them, so the pager
// holds each of them privately until this host's next checkpoint publishes it.
// Everything read from this host's own volume, and every page the source served
// out of a checkpoint it shares with this host, is reported clean.
func (b *PeerBacking) LoadUnpublished(ctx context.Context, offset uint64, dst []byte) ([]bool, error) {
	size := uint64(b.config.PageSize)
	if b.gone() || offset%size != 0 || uint64(len(dst))%size != 0 || len(dst) == 0 {
		return nil, b.fromVolume(ctx, offset, dst)
	}
	first := offset / size
	pages := uint64(len(dst)) / size
	unpublished := make([]bool, pages)
	for done := uint64(0); done < pages; {
		count := min(pages-done, uint64(b.config.MaxPagesPerRequest))
		window := dst[done*size : (done+count)*size]
		reply, err := b.ask(ctx, first+done, int(count))
		if err != nil {
			if cancelled(ctx, err) || unusable(err) {
				// A source this destination cannot use at all is a torn
				// post-copy rather than a source that is gone: the fault fails
				// with that cause and the received VM is given up, which is
				// what a torn post-copy takes anyway.
				return nil, err
			}
			if errors.Is(err, peer.ErrNotServed) || stopped(err) {
				// The source is gone, or this host has stopped asking it, which
				// is the same thing from this side and is what a receive does
				// the moment its post-copy is over.
				b.fallBack(ctx, err)
			}
			if _, only := b.onlyOnSource(first+done, int(pages-done)); only && stopped(err) {
				// This backing's own end, on a read that still holds a page only
				// the source ever had. Those bytes are nowhere: not here, and
				// not in a volume that holds the checkpoint the guest wrote
				// past. The read ends with the end that stopped it — the
				// orchestrator discarding a post-copy that never completed —
				// rather than as a page lost, which would say this guest's
				// memory is torn when what happened is that it was given up.
				return nil, err
			}
			// Everything the source did serve is in dst and is the pager's to
			// install; the rest comes from the volume, which is where the
			// checkpoint's bytes are — unless it is a page no checkpoint has,
			// which fromVolume refuses, failing this fault rather than handing
			// the guest bytes from before its own write.
			if sim.Bug(ctx, "migration-corrupt-fallback") {
				clear(dst[done*size:])
			} else if err := b.fromVolume(ctx, offset+done*size, dst[done*size:]); err != nil {
				return nil, err
			}
			return unpublished, nil
		}
		if err := b.fill(ctx, first+done, window, reply.Present, reply.Payload); err != nil {
			return nil, err
		}
		for index := range int(count) {
			bit := byte(1) << (index % 8)
			unpublished[int(done)+index] = reply.Dirty[index/8]&bit != 0
		}
		done += count
	}
	return unpublished, nil
}

// fill copies the pages the source served into dst and reads every run it did
// not serve from the volume, one read per run rather than one per page. A run it
// did not serve that holds a page no checkpoint has is not the volume's to
// answer: those bytes are only on the source, so the load fails instead.
func (b *PeerBacking) fill(ctx context.Context, first uint64, dst []byte, present []byte, payload []byte) error {
	size := int(b.config.PageSize)
	count := len(dst) / size
	served := 0
	// The pages already copied out of the payload came from the source whether
	// or not the rest of this window does.
	defer func() { b.served.Add(int64(served)) }()
	for index := 0; index < count; {
		if present[index/8]&(1<<(index%8)) != 0 {
			if (served+1)*size > len(payload) {
				return fmt.Errorf("%w: the source served fewer pages than it reported", wire.ErrMalformedFrame)
			}
			if sim.Bug(ctx, "migration-corrupt-peer-page") {
				clear(dst[index*size : (index+1)*size])
			} else {
				copy(dst[index*size:(index+1)*size], payload[served*size:(served+1)*size])
			}
			served++
			index++
			continue
		}
		run := index
		for run < count && present[run/8]&(1<<(run%8)) == 0 {
			run++
		}
		if page, only := b.onlyOnSource(first+uint64(index), run-index); only {
			return b.lost(page)
		}
		if err := b.config.Volume.Load(ctx, (first+uint64(index))*uint64(size), dst[index*size:run*size]); err != nil {
			return err
		}
		b.fromDisk.Add(int64(run - index))
		index = run
	}
	return nil
}

// Unpublished reports the pages the handoff named as the source's own, which is
// what the destination must fetch before the source may stop serving.
func (b *PeerBacking) Unpublished() []PageRun { return b.config.Unpublished }

// Resident asks the source which pages it still holds, so the destination can
// stream them in behind its running guest. A source that will not answer has
// nothing to stream, which is not an error the destination can do anything
// about: it reports it and reads its volume.
//
// A listing that does not arrive is decided exactly as a load that does not:
// the caller of this one is the stream, which the destination stops itself and
// cancels with a cause of its own, and neither that nor a busy source nor a
// source that stumbled is this source's last word. Ending the asking here would
// send the region to a volume that does not hold the pages no checkpoint has,
// and every later fault on one of them would fail with the source still there.
// Only the source's own answer that it does not serve this VM does that.
func (b *PeerBacking) Resident(ctx context.Context) ([]PageRun, error) {
	if b.gone() {
		return nil, nil
	}
	runs, err := b.source.Resident(ctx)
	if err != nil {
		if errors.Is(err, peer.ErrNotServed) {
			b.fallBack(ctx, err)
		}
		return nil, err
	}
	return runs, nil
}

// PageRun is a run of consecutive pages the source holds.
type PageRun = peer.Run

// ask asks for one run of pages until it has an answer, for as long as the run
// holds a page only the source has. There is no attempt count: those pages exist
// nowhere else, so every alternative to asking again either waits — which is
// this — or hands the guest the checkpoint's bytes, which it has written past.
// A busy source is the same wait for the same reason: it is at its budget for
// this peer, which a drain makes the normal state.
//
// A run the volume can answer for is not worth waiting on, busy or broken: it is
// read from there this time and nothing is decided for the next one.
//
// Three answers stop the asking whatever the run holds. The source saying it no
// longer serves this VM is the one that says the source is gone; a source
// serving pages of another size and a reply this host cannot read say the
// destination cannot use this source at all; and the caller's own cancellation
// is not this source's business at all.
func (b *PeerBacking) ask(caller context.Context, first uint64, count int) (peer.Answer, error) {
	ctx, release := b.bounded(caller)
	defer release()
	began := b.config.Clock.Now()
	stalled := false
	defer func() {
		if stalled {
			b.stalls.Add(1)
			slog.WarnContext(ctx, "vmmigrate: a read of pages only the migration source has stopped waiting for it",
				"vm", b.config.VM, "volume", b.config.Volume.Name(), "source", b.config.Peer,
				"first_page", first, "pages", count,
				"waited_seconds", b.config.Clock.Since(began).Seconds(),
				"refusals", b.refusals.Load(), "unfetched", b.Unfetched())
		}
	}()
	delay := busyDelay
	for {
		reply, err := b.source.Pages(ctx, first, count)
		_, only := b.onlyOnSource(first, count)
		switch {
		case err == nil && !reply.Busy:
			return reply, nil
		case err == nil:
			// The source is at its budget for this peer and served nothing.
			b.refusals.Add(1)
			if !only {
				return reply, nil
			}
		case cancelled(ctx, err) || unusable(err) || errors.Is(err, peer.ErrNotServed):
			return peer.Answer{}, b.ended(caller, err)
		case !only:
			// The volume holds every page of this run, so the round trip this
			// one would cost is not worth its bytes.
			return peer.Answer{}, err
		}
		if !stalled && b.config.Clock.Since(began) >= stalledAsk {
			// The guest thread behind this read has been stopped for as long as
			// this ask has run, and will be until the source answers.
			stalled = true
			slog.WarnContext(ctx, "vmmigrate: a read of pages only the migration source has is still waiting for it",
				"vm", b.config.VM, "volume", b.config.Volume.Name(), "source", b.config.Peer,
				"first_page", first, "pages", count,
				"waited_seconds", b.config.Clock.Since(began).Seconds(),
				"refusals", b.refusals.Load(), "unfetched", b.Unfetched(), "last_error", err)
		}
		if err := b.wait(ctx, delay); err != nil {
			return peer.Answer{}, b.ended(caller, err)
		}
		delay = min(2*delay, busyDelayMax)
	}
}

// unusable reports a source this destination cannot read at all: one serving
// pages of another size, and a reply this host cannot decode. Neither is a
// source that is gone and neither is one that stumbled, so neither the volume
// nor another attempt answers it: the fault fails with it and the received VM
// is torn.
func unusable(err error) bool {
	return errors.Is(err, peer.ErrPageSize) || errors.Is(err, wire.ErrMalformedFrame)
}

// stopped reports this backing's own end, which is the migration's: the
// orchestrator discarded the received VM, or the destination closed the stream.
func stopped(err error) bool { return errors.Is(err, ErrClosed) }

// cancelled reports the caller having given up. It asks the context, because a
// cancellation carries whatever cause the canceller installed — a stream this
// host stopped cancels with ErrClosed — and the error alone therefore does not
// say what it is. Reading one as the source's failure retries it, counts it
// against the source and ends by answering a page only the source has from a
// volume that never held it; none of that is about the source at all.
func cancelled(ctx context.Context, err error) bool {
	return ctx.Err() != nil ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// wait sleeps for delay unless the caller gives up first, or this backing does.
// A fault waiting for a source that never answers ends only one of those two
// ways: the asking itself has no end, which is the point of it.
func (b *PeerBacking) wait(ctx context.Context, delay time.Duration) error {
	timer := b.config.Clock.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C():
		return nil
	}
}

// bounded is one request's context: the caller's, ending also when this backing
// does. A request already on the wire when the migration ends is what a discard
// has to reach — the reply it is waiting for is never coming — and a context is
// the only thing that reaches inside a connection's read.
func (b *PeerBacking) bounded(caller context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(caller)
	stop := context.AfterFunc(b.life, func() { cancel(context.Cause(b.life)) })
	return ctx, func() { stop(); cancel(nil) }
}

// ended names this backing's own end as the cause of an ask that stopped for it,
// rather than whatever the interrupted request happened to report.
func (b *PeerBacking) ended(caller context.Context, err error) error {
	if caller.Err() == nil && b.life.Err() != nil {
		return context.Cause(b.life)
	}
	return err
}

// fallBack is the transition to "the source is gone", and the only thing that
// makes it is the source's own answer that it no longer serves this VM — or
// this backing being closed, which says the same from this side. From here a
// page a checkpoint holds reads from the volume and a page only the source held
// fails with ErrUnpublishedLost. It says so exactly once.
func (b *PeerBacking) fallBack(ctx context.Context, cause error) {
	if b.fallen.Swap(true) {
		return
	}
	// Every page from here comes out of this host's own volume, which holds
	// the checkpoint's bytes and nothing the source published since. Whether
	// that is correct depends entirely on which pages no checkpoint holds.
	sim.Probe(ctx, ProbeVolumeFallback)
	b.once.Do(func() {
		slog.InfoContext(ctx, "vmmigrate: reading the rest of this volume from the log rather than the migration source",
			"vm", b.config.VM, "volume", b.config.Volume.Name(), "source", b.config.Peer, "reason", cause)
	})
	b.source.Close()
}

// Close ends this region's half of the migration: every connection it holds is
// dropped, every fault waiting for a source that never answered is given that
// end as its cause, and nothing is asked of the source again. The volume keeps
// working, which is every page a checkpoint holds; a page only the source held
// is lost from here, exactly as it is when the source says it has gone.
//
// It is what a destination that stopped its own stream does, and what the
// orchestrator's discard of a received VM comes to.
func (b *PeerBacking) Close() error {
	b.endLife(fmt.Errorf("%w: %s stopped asking %s for %s", ErrClosed,
		b.config.VM, b.config.Peer, b.config.Volume.Name()))
	b.source.Close()
	return nil
}

// gone reports a region that will not ask its source again: one the source
// answered out of, and one this host closed.
func (b *PeerBacking) gone() bool { return b.fallen.Load() || b.life.Err() != nil }
