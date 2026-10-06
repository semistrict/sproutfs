package checkpoint

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/checkpoint/internal/part"
	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/blob"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// partWriter fills parts and uploads each one as it is sealed, so a large
// checkpoint costs a few PUTs and holds a bounded number of parts in memory.
//
// Its members are encoded side by side, as many at once as the store has
// encoders, and taken into parts in the order they were submitted, on the
// publication's own goroutine. Encoding a page — its digest and its
// Zstandard — is what a publication spends its processor on, and a
// publication that encoded one page at a time ran at one processor's pace
// however many encoders the host gave it
// (docs/measurements/gce-publication-throughput-2026-10-04.md). An envelope
// is the same bytes whichever encode ends first, and the order a part takes
// them in is the order they were submitted, so the parts are the same bytes
// as when they were encoded one at a time.
type partWriter struct {
	store  *Store
	ref    control.Ref
	cancel context.CancelFunc
	part   *part.Builder
	// admitted is whether this writer holds a slot of the store's host-wide
	// builder budget, which it takes before it submits anything.
	admitted bool
	next     uint32
	bytes    uint64
	// keep is the pull that keeps each part's pages once the part is durable,
	// and members the pages of the part in hand, where they lie in it.
	// geometry is each volume's, which says what window a page is in.
	keep     *Pull
	members  []keptMember
	geometry map[string]Geometry
	// handed is closed once the part before the next one has been handed to
	// the pull and the cluster, or never will be: see handOver. It is nil
	// before the first part.
	handed chan struct{}
	// pace is the publication's waits for its fills of the cluster, which
	// its parts and its segments share.
	pace    fillPace
	wait    sync.WaitGroup
	once    sync.Once
	failure error

	// encoding is the batches of members being encoded, oldest first, and
	// open the batch being filled, which is not encoding yet. encodes counts
	// the goroutines encoding them.
	encoding []*batch
	open     *batch
	encodes  sync.WaitGroup
	// batches counts the batches dispatched.
	batches int
	// planned is the bytes of members the publication has said are still to
	// come, each counted with its envelope's header, which is what a new
	// part's body is made room for.
	planned uint64
	// spare is the buffers batches are read into and encoded into, given back
	// once their members are in a part. Only the publication's goroutine
	// takes and gives them.
	spare [][]byte
}

// batch is members encoded one after another on one goroutine: a page of
// 2 MiB alone, and small pages up to about batchBytes together, so a
// publication of small pages does not pay a goroutine and a wait a page.
type batch struct {
	members []*pending
	// in is the buffer the batch's pages were read into, where they were,
	// and out the one their envelopes are encoded into, one after another.
	in, out []byte
	// bytes is the decoded bytes of the batch's members.
	bytes int
	done  chan struct{}
	err   error
}

// batchBytes is what a batch holds before it is encoded.
const batchBytes = 1 << 20

// pending is one member submitted and not yet in a part.
type pending struct {
	member part.Member
	kind   memberKind
	origin control.Ref
	data   []byte
	// envelope is where its envelope lies in its batch's out, once encoded.
	envelope [2]int
	// placed is told where the member landed, on the publication's goroutine,
	// in the order members were submitted.
	placed func(location)
}

// memberKind says what one member holds: a page of a volume's contents or the
// VMM state. Those are the two kinds a part holds.
type memberKind int

const (
	memberPage memberKind = iota
	memberState
)

// admit takes this writer's slot of the store's builder budget, once. A
// publication that writes nothing never takes one.
func (w *partWriter) admit(ctx context.Context) error {
	if w.admitted {
		return nil
	}
	if err := w.store.acquireBuilder(ctx); err != nil {
		return err
	}
	w.admitted = true
	return nil
}

// discharge gives the builder slot back, once, when the writer is done with it.
func (w *partWriter) discharge() {
	if w.admitted {
		w.admitted = false
		w.store.releaseBuilder()
	}
}

// plan says that count members of size bytes each are to come, so the parts
// that will hold them are made room for once rather than grown.
func (w *partWriter) plan(count int, size uint64) {
	w.planned += uint64(count) * (size + blob.HeaderSize)
}

// room is where the next page of size bytes a publication reads goes: in the
// open batch's buffer, after the pages it already holds. The page is the
// batch's only once submit takes it; until then the next room is the same
// bytes, so a page that is not submitted costs nothing.
func (w *partWriter) room(ctx context.Context, size int) ([]byte, error) {
	if w.open != nil && cap(w.open.in)-len(w.open.in) < size {
		if err := w.dispatch(ctx); err != nil {
			return nil, err
		}
	}
	if w.open == nil {
		w.open = &batch{in: w.take(max(size, batchBytes))[:0], done: make(chan struct{})}
	}
	return w.open.in[len(w.open.in):][:size], nil
}

// submitRead submits the page room last gave, which the publication has read
// into it. placed is told where it landed.
func (w *partWriter) submitRead(ctx context.Context, volume string, page uint64, data []byte,
	placed func(location)) error {
	w.open.in = w.open.in[:len(w.open.in)+len(data)]
	return w.submit(ctx, volume, page, memberPage, w.ref, data, placed)
}

// submit hands one member to be encoded beside the others and taken into a
// part in the order members were submitted. origin is the checkpoint the page
// was first published under: this one for a page the publication wrote, and
// the page's existing origin for one compaction moved. data is read until the
// member is in a part, so a caller lending it waits for drain before it takes
// it back. placed, if not nil, is told where the member landed.
func (w *partWriter) submit(ctx context.Context, volume string, page uint64, kind memberKind, origin control.Ref,
	data []byte, placed func(location)) error {
	if err := w.admit(ctx); err != nil {
		return err
	}
	member := part.Member{Volume: volume, Page: page, State: kind == memberState}
	if origin != w.ref {
		member.OriginVM, member.OriginSequence = origin.VM, origin.Sequence
	}
	if w.open == nil {
		w.open = &batch{done: make(chan struct{})}
	}
	w.open.members = append(w.open.members, &pending{member: member, kind: kind, origin: origin, data: data,
		placed: placed})
	w.open.bytes += len(data)
	if w.open.bytes >= batchBytes {
		return w.dispatch(ctx)
	}
	return nil
}

// window is how many batches a publication has encoding at once: one for
// each encoder and one more, so an encoder that finishes finds the next batch
// ready while the publication reads pages and fills parts. It is what a
// publication holds of its pages besides its parts: a batch and its
// envelopes, each, for one more than the encoders.
func (w *partWriter) window(ctx context.Context) int {
	if sim.Bug(ctx, "checkpoint-encode-one-batch-at-a-time") {
		return 1
	}
	return w.store.codecs.Encoders() + 1
}

// dispatch starts encoding the open batch, first taking the oldest batches
// into parts while the window is full.
//
// The batch's encoder is taken here, on the publication's goroutine, before
// the batch's own goroutine starts: batches are admitted to the encoders in
// the order they were filled. Were each batch's goroutine to take its own,
// a later batch could take the encoder an earlier one was waiting for,
// whenever the Go scheduler ran it first, and the publication, which takes
// batches into parts oldest first, would wait a whole encode for the earlier
// one with an encoder idle.
func (w *partWriter) dispatch(ctx context.Context) error {
	if w.open == nil || len(w.open.members) == 0 {
		return nil
	}
	for len(w.encoding) >= w.window(ctx) {
		if err := w.place(ctx); err != nil {
			return err
		}
	}
	var encoder *blob.Encoder
	if !sim.Bug(ctx, "checkpoint-encode-admitted-in-any-order") {
		var err error
		if encoder, err = w.store.codecs.Encoder(ctx); err != nil {
			return err
		}
	}
	b := w.open
	w.open = nil
	b.out = w.take(b.bytes + len(b.members)*blob.HeaderSize + memberSlack)[:0]
	w.encoding = append(w.encoding, b)
	w.encodes.Add(1)
	ctx = w.batchTask(ctx)
	go func() {
		defer w.encodes.Done()
		defer close(b.done)
		if encoder == nil {
			if encoder, b.err = w.store.codecs.Encoder(ctx); b.err != nil {
				return
			}
		}
		defer encoder.Release()
		for _, m := range b.members {
			start := len(b.out)
			if b.out, b.err = encoder.AppendEncode(ctx, b.out, m.data); b.err != nil {
				return
			}
			m.envelope = [2]int{start, len(b.out)}
		}
	}()
	return nil
}

// batchTask names the next batch's encoding as a task of its own in a
// simulation, by the order the batches were filled, so a run can see the
// order its encodes began in (sim.Runtime.WorkOrder). Outside a simulation it
// is ctx.
func (w *partWriter) batchTask(ctx context.Context) context.Context {
	w.batches++
	if sim.RuntimeFrom(ctx) == nil {
		return ctx
	}
	return sim.WithTask(ctx, fmt.Sprintf("encode-%d", w.batches))
}

// memberSlack is what encoding a member may write beyond its envelope's size
// before it falls back to raw bytes.
const memberSlack = 64 << 10

// drain takes every member submitted into a part: once it returns, the
// writer holds no member's bytes but the parts'.
func (w *partWriter) drain(ctx context.Context) error {
	if err := w.dispatch(ctx); err != nil {
		return err
	}
	for len(w.encoding) > 0 {
		if err := w.place(ctx); err != nil {
			return err
		}
	}
	return nil
}

// place waits for the oldest batch to be encoded and takes its members into
// parts, in order, sealing a part and starting its upload whenever the next
// member no longer fits it.
func (w *partWriter) place(ctx context.Context) error {
	b := w.encoding[0]
	w.encoding = w.encoding[1:]
	<-b.done
	w.give(b.in)
	defer w.give(b.out)
	if b.err != nil {
		return b.err
	}
	for _, m := range b.members {
		builder, err := w.builderFor(ctx, m.member)
		if err != nil {
			return err
		}
		offset, length := builder.Append(ctx, m.member, b.out[m.envelope[0]:m.envelope[1]])
		at := location{ref: w.ref, origin: m.origin, part: w.next, offset: offset, length: length}
		w.bytes += at.length
		w.planned -= min(w.planned, uint64(len(m.data))+blob.HeaderSize)
		if (w.keep != nil || w.store.cache.fills()) && m.kind == memberPage {
			w.members = append(w.members, keptMember{volume: m.member.Volume, page: m.member.Page, at: at})
		}
		if m.placed != nil {
			m.placed(at)
		}
	}
	return nil
}

// take is a spare buffer of at least size bytes, or a new one.
func (w *partWriter) take(size int) []byte {
	for at, buffer := range slices.Backward(w.spare) {
		if cap(buffer) >= size {
			w.spare = slices.Delete(w.spare, at, at+1)
			return buffer[:size]
		}
	}
	return make([]byte, size)
}

// give keeps a buffer for a later batch.
func (w *partWriter) give(buffer []byte) {
	if cap(buffer) > 0 {
		w.spare = append(w.spare, buffer[:0])
	}
}

// keptMember is one page of a part in hand and where it lies in the part.
type keptMember struct {
	volume string
	page   uint64
	at     location
}

// kept is what a durable part's pages are for the pull that keeps them and
// the cluster they fill: each member's envelope, named by the page's identity.
func (w *partWriter) kept(data []byte, members []keptMember) []envelope {
	envelopes := make([]envelope, 0, len(members))
	for _, m := range members {
		envelopes = append(envelopes, envelope{key: pageDiskKey(identityOf(m.volume, m.page, m.at), w.geometry[m.volume]),
			data: data[m.at.offset:][:m.at.length]})
	}
	return envelopes
}

// partBytes is the encoded member size a part fills to before it is sealed and
// uploaded. It is the store's configured size, except where a campaign has
// buggified it down to a single page: a deployment whose pages are large
// relative to the part size writes a part per page, and a multi-part checkpoint
// is the shape a one-part checkpoint never reaches — a member table per part, a
// part count carried by the last one, and a root that must name which part each
// page is in.
func (w *partWriter) partBytes(ctx context.Context) int {
	if sim.Buggify(ctx, "checkpoint/one-page-parts", 1) {
		return 1
	}
	return w.store.partBytes
}

// builderFor returns the part builder member goes into, sealing the part in
// hand first when member no longer fits it — in body bytes or in table. A full
// part is sealed when the next member arrives rather than as soon as it fills,
// so the part this publication holds last is always the one finish closes: that
// is the part carrying the checkpoint's part count.
//
// A new part's body is made room for at once: the members a part fills to and
// the one that fills it, or what the publication has still to write when that
// is less.
func (w *partWriter) builderFor(ctx context.Context, member part.Member) (*part.Builder, error) {
	if w.part != nil && w.part.Full(member, w.partBytes(ctx), maximumTableSize) {
		if err := w.flush(ctx); err != nil {
			return nil, err
		}
	}
	if w.part == nil {
		w.part = part.NewBuilder(w.store.codecs)
		w.part.Reserve(int(min(w.planned, uint64(w.partBytes(ctx))+PageSize2MiB+blob.HeaderSize)))
	}
	return w.part, nil
}

// flush seals the part in hand as one the checkpoint goes on past and starts its
// upload, which runs under one slot of the store's shared budget so parts in
// flight are bounded. The part keeps its slot until it has been handed to the
// cluster, so a publication whose fills wait for room waits for a slot for its
// next part, and holds no more parts than the store has slots. Only the last
// part carries the part count, and finish writes that one.
func (w *partWriter) flush(ctx context.Context) error {
	if w.part == nil {
		return nil
	}
	data, err := w.part.Seal(0)
	if err != nil {
		return err
	}
	if len(data) > maximumPartSize {
		return ErrInvalidRange
	}
	key, err := w.store.partKey(w.ref, w.next)
	if err != nil {
		return err
	}
	members := w.members
	w.part, w.next, w.members = nil, w.next+1, nil
	if err := w.store.acquire(ctx); err != nil {
		return err
	}
	before, handed := w.handed, make(chan struct{})
	w.handed = handed
	w.wait.Add(1)
	upload := func() {
		defer w.wait.Done()
		defer close(handed)
		defer w.store.release()
		sealed := sealedPart{key: key, data: data, members: members}
		w.fillEarly(ctx, &sealed)
		if err := w.put(ctx, key, data); err != nil {
			w.record(err)
			return
		}
		w.handOver(ctx, before, sealed)
	}
	if sim.Bug(ctx, "checkpoint-upload-one-part-at-a-time") {
		// The publication waits for each part's PUT before it fills the next.
		upload()
		return nil
	}
	go upload()
	return nil
}

// sealedPart is one part a publication uploads: its key and bytes, the
// members it holds, and whether a bug handed it to the cluster or to the hot
// tier before its PUT.
type sealedPart struct {
	key                    platform.ObjectKey
	data                   []byte
	members                []keptMember
	earlyCluster, earlyHot bool
}

// handOver hands a durable part over once the part before it has been, so
// the parts reach the pull and the cluster's one worker of fills in their own
// order. Uploads that end at one instant would otherwise hand theirs over in
// the order the Go scheduler runs them, and the window a fill waits for room
// for, or the one it drops, would be a different one on every run of a seed.
// before is the part before's, nil for the first part. The caller holds the
// part's upload slot until handOver returns: a part waiting for room for its
// fills, or for the part before it, is a part the publication holds.
func (w *partWriter) handOver(ctx context.Context, before <-chan struct{}, sealed sealedPart) {
	if before != nil && !w.store.cache.bug("fill-parts-in-any-order") {
		<-before
	}
	w.durable(ctx, sealed)
}

// durable hands a part whose PUT has succeeded to the hot tier, to the pull
// that keeps its pages and to the cluster, which takes only the windows
// inside its share. Nothing is filled before then: a part the store refused
// must reach no cache. It returns once the cluster has taken every window of
// the part or dropped it, which is when the queue had room for it.
func (w *partWriter) durable(ctx context.Context, sealed sealedPart) {
	if !sealed.earlyHot {
		w.store.hot.published(ctx, sealed.key, sealed.data)
	}
	if len(sealed.members) == 0 {
		return
	}
	envelopes := w.kept(sealed.data, sealed.members)
	if w.keep != nil {
		w.keep.keep(ctx, envelopes)
	}
	if !sealed.earlyCluster {
		w.store.cache.publish(ctx, &w.pace, envelopes)
	}
}

// fillEarly is the bugs that fill the cluster, or the hot tier, with a part
// before its PUT has succeeded. It notes which did.
func (w *partWriter) fillEarly(ctx context.Context, sealed *sealedPart) {
	if len(sealed.members) > 0 && w.store.cache.bug("fill-before-durable") {
		w.store.cache.fill(WriteFillPublication, w.kept(sealed.data, sealed.members))
		sealed.earlyCluster = true
	}
	if w.store.hot.bug("hot-tier-fill-before-durable") {
		w.store.hot.published(ctx, sealed.key, sealed.data)
		sealed.earlyHot = true
	}
}

// put writes one part create-if-absent. A part a retry of this publication
// finds already there is its own, byte for byte, because a publication writes
// its members in one order and encodes them one way; a part holding anything
// else is another publication under this reference, which is a conflict.
func (w *partWriter) put(ctx context.Context, key platform.ObjectKey, data []byte) error {
	return w.store.putObject(ctx, key, data, digestOf(data), func(existing []byte) error {
		if !equalParts(existing, data) {
			return ErrConflict
		}
		return nil
	})
}

// finish takes every member still encoding into a part, seals and uploads the
// part in hand, which is the one carrying the checkpoint's part count, and
// waits for every earlier upload. When it returns every part is durable, which
// is what the index object may then name. An upload that failed is what the
// caller is told about, not the cancellation it caused in whatever was still
// running.
func (w *partWriter) finish(ctx context.Context) error {
	defer w.discharge()
	if err := w.drain(ctx); err != nil {
		return err
	}
	if w.part != nil {
		sealed, err := w.part.Seal(w.next + 1)
		if err != nil {
			return err
		}
		if len(sealed) > maximumPartSize {
			return ErrInvalidRange
		}
		key, err := w.store.partKey(w.ref, w.next)
		if err != nil {
			return err
		}
		members := w.members
		w.part, w.next, w.members = nil, w.next+1, nil
		if err := w.store.acquire(ctx); err != nil {
			return err
		}
		part := sealedPart{key: key, data: sealed, members: members}
		w.fillEarly(ctx, &part)
		err = w.put(ctx, key, sealed)
		if err == nil {
			w.handOver(ctx, w.handed, part)
		}
		w.store.release()
		if err != nil {
			return err
		}
	}
	w.wait.Wait()
	return w.failure
}

// abandon stops the encodes and the uploads a failed publication started and
// reports why it failed: an upload's own error where there was one, and
// otherwise what the caller ran into. Everything the uploads wrote is
// unreferenced.
func (w *partWriter) abandon(cause error) error {
	w.cancel()
	w.encodes.Wait()
	w.wait.Wait()
	w.discharge()
	if w.failure != nil {
		return w.failure
	}
	return cause
}

// halt stops the encodes in flight and waits for them, so a caller that lent
// a member its bytes can take them back after a failure.
func (w *partWriter) halt() {
	w.cancel()
	w.encodes.Wait()
}

func (w *partWriter) record(err error) { w.once.Do(func() { w.failure = err; w.cancel() }) }

// equalParts compares a part already in the store with the one this publication
// built. Parts are raw bytes rather than an envelope, so equality is exact.
func equalParts(existing, data []byte) bool { return string(existing) == string(data) }
