package peer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"time"

	"github.com/semistrict/sproutfs/internal/blob"
	migratev1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/migrate/v1"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/proto"
)

// The requests a host makes of the peer that still holds a migrated VM's or a
// fork child's pages. They hold no policy: whether to ask a busy peer again,
// whether to stop asking altogether, and which pages only the peer has are the
// caller's to decide. Each sends one request and reports what came back.

var (
	// ErrNotServed reports a peer that does not serve this VM any more.
	ErrNotServed = errors.New("peer: the peer no longer serves this VM")
	// ErrPageSize reports a peer whose pages are not the size this host maps,
	// which no reply of its can be read as.
	ErrPageSize = errors.New("peer: the peer serves pages of another size")
)

// Run is a run of consecutive pages the peer holds.
type Run struct {
	First uint64
	Count int
}

// PageRequest asks one memory region of one VM for a run of pages, of the
// size this host maps them in.
type PageRequest struct {
	VM, Volume string
	First      uint64
	Count      int
	PageSize   int
}

// Answer is what one page request came back with: which pages the peer served,
// which of those are its own state that no checkpoint has, and their bytes. A
// peer at its budget for this host served nothing, and Busy says how busy.
type Answer struct {
	Present, Dirty, Payload []byte
	Busy                    *BusyError
	// Waited is how long the request waited for room in its class's budget
	// and on a connection before it was sent.
	Waited time.Duration
}

// Pages asks for one run of pages and reports the bitmaps and bytes the peer
// answered with. A peer at its budget for this host served nothing and says so,
// which is an answer rather than a failure.
func (p *Peer) Pages(ctx context.Context, asked PageRequest) (Answer, error) {
	count := asked.Count
	response := new(migratev1.PageResponse)
	request := migratev1.PageRequest_builder{Vm: proto.String(asked.VM),
		Volume: proto.String(asked.Volume), FirstPage: proto.Uint64(asked.First),
		Count: proto.Uint32(uint32(count)), PayloadFormat: proto.Uint32(1)}.Build()
	pageBytes := int64(count) * int64(asked.PageSize)
	got, waited, err := p.call(ctx, asked.VM+"/"+asked.Volume, request, response, pageBytes, pageBytes+blob.HeaderSize, nil)
	if busy := (*BusyError)(nil); errors.As(err, &busy) {
		return Answer{Present: make([]byte, (count+7)/8), Dirty: make([]byte, (count+7)/8), Busy: busy,
			Waited: waited}, nil
	}
	if err != nil {
		return Answer{}, err
	}
	defer got.payload.release()
	switch status := response.GetStatus(); {
	case status == migratev1.Status_STATUS_OK:
	case status == migratev1.Status_STATUS_BUSY:
		// A peer of the release before is at its budget for this host. It does
		// not say how busy.
		return Answer{Present: make([]byte, (count+7)/8), Dirty: make([]byte, (count+7)/8),
			Busy: &BusyError{Class: ClassOf(ctx)}, Waited: waited}, nil
	default:
		return Answer{}, statusError(status)
	}
	answered := (int(response.GetCount()) + 7) / 8
	if response.GetPayloadFormat() != 1 || response.GetPageSize() != uint32(asked.PageSize) || int(response.GetCount()) > count ||
		len(response.GetPresent()) != answered || len(response.GetDirty()) != answered {
		return Answer{}, fmt.Errorf("%w: malformed page response", wire.ErrMalformedFrame)
	}
	if n := response.GetCount(); n%8 != 0 && response.GetPresent()[n/8]>>uint(n%8) != 0 {
		return Answer{}, fmt.Errorf("%w: page bitmap exceeds response count", wire.ErrMalformedFrame)
	}
	// A peer that answered for fewer pages than were asked for leaves the rest
	// to the caller, which a clear bit says.
	bitmap := make([]byte, (count+7)/8)
	copy(bitmap, response.GetPresent()[:answered])
	unpublished := make([]byte, (count+7)/8)
	copy(unpublished, response.GetDirty()[:answered])
	for index := int(response.GetCount()); index < count; index++ {
		bitmap[index/8] &^= 1 << (index % 8)
		unpublished[index/8] &^= 1 << (index % 8)
	}
	served := 0
	for position, bitsInByte := range bitmap {
		// A page the peer says is its own state but did not serve has no bytes
		// to be dirty with.
		unpublished[position] &= bitsInByte
		served += bits.OnesCount8(bitsInByte)
	}
	decoded, err := blob.Decode(ctx, got.payload.bytes, served*asked.PageSize)
	if err != nil || len(decoded) != served*asked.PageSize {
		return Answer{}, errors.Join(wire.ErrMalformedFrame, err)
	}
	if raw := got.payload.bytes; len(decoded) > 0 && &decoded[0] == &raw[blob.HeaderSize] &&
		!p.table.bug("peer-answer-shares-buffer") {
		// A page no encoder shrank decodes to a slice of the reply's own
		// buffer, which goes back to the pool as this returns and is read
		// into by the next reply: the answer keeps a copy.
		decoded = bytes.Clone(decoded)
	}
	return Answer{Present: bitmap, Dirty: unpublished, Payload: decoded, Waited: waited}, nil
}

// Resident asks which pages the peer still holds of one memory region, so the
// destination can stream them in behind its running guest. The listing is
// walked maxRuns runs at a time.
func (p *Peer) Resident(ctx context.Context, vm, volume string, pageSize, maxRuns int) ([]Run, error) {
	var runs []Run
	first := uint64(0)
	for {
		response := new(migratev1.ResidentResponse)
		request := migratev1.ResidentRequest_builder{Vm: proto.String(vm),
			Volume: proto.String(volume), FirstPage: proto.Uint64(first),
			MaxRuns: proto.Uint32(uint32(maxRuns))}.Build()
		got, _, err := p.call(ctx, vm+"/"+volume, request, response, 0, 0, nil)
		if err != nil {
			return nil, err
		}
		got.payload.release()
		if status := response.GetStatus(); status != migratev1.Status_STATUS_OK {
			return nil, statusError(status)
		}
		if response.GetPageSize() != uint32(pageSize) {
			return nil, fmt.Errorf("%w: the peer serves %d byte pages, this host maps %d",
				ErrPageSize, response.GetPageSize(), pageSize)
		}
		for _, run := range response.GetRuns() {
			if run.GetCount() == 0 {
				continue
			}
			runs = append(runs, Run{First: run.GetFirstPage(), Count: int(run.GetCount())})
			first = run.GetFirstPage() + uint64(run.GetCount())
		}
		if !response.GetMore() || len(response.GetRuns()) == 0 {
			return runs, nil
		}
	}
}

// Claim asks the peer to mark a fork child's hold claimed. It reports
// ErrNotServed when the peer no longer holds it: given up, released or run
// out.
func (p *Peer) Claim(ctx context.Context, vm string) error {
	response := new(migratev1.ClaimResponse)
	got, _, err := p.call(ctx, vm, migratev1.ClaimRequest_builder{Vm: proto.String(vm)}.Build(), response, 0, 0, nil)
	if err != nil {
		return err
	}
	got.payload.release()
	if status := response.GetStatus(); status != migratev1.Status_STATUS_OK {
		return statusError(status)
	}
	return nil
}

// call makes one request of the class ctx names: it takes room in that class's
// budget and a slot on one of its connections, sends, and reads the reply into
// response. payload is what the request carries beside its header, reserve
// what it holds at the peer while it is answered, and maxPayload the largest
// payload its reply may carry.
//
// Making the request is admitted before it takes room or a connection: see
// Admitter. Its sent hook is called once it is on the wire, or once it fails
// before it is: see WithSent.
func (p *Peer) call(ctx context.Context, admitAs string, request, response proto.Message, reserve, maxPayload int64,
	payload []byte) (result, time.Duration, error) {
	sent := sentHook(ctx)
	defer sent()
	if admitAs != "" {
		if err := admit(ctx, admitAs); err != nil {
			return result{}, 0, err
		}
	}
	clock := p.table.clock
	began := clock.Now()
	class := ClassOf(ctx)
	switch {
	case class.waitedOn():
		// A fault waiting anywhere, or a stripe read one waits on, shrinks
		// the background budget, so bulk work gives the links to it.
		p.table.background.faultStarted()
		defer p.table.background.faultEnded()
	case class == BulkRead:
		if p.table.bug("peer-unbounded-background") {
			break
		}
		if err := p.table.background.Acquire(ctx, PriorityOf(ctx), reserve); err != nil {
			return result{}, clock.Since(began), err
		}
		defer p.table.background.Release(reserve)
	}
	pool := p.pools[class]
	c, err := pool.acquire(ctx, reserve)
	waited := clock.Since(began)
	if err != nil {
		return result{}, waited, err
	}
	got, err := c.roundTrip(ctx, request, payload, reserve, maxPayload, sent)
	if err != nil {
		return result{}, waited, err
	}
	if busy, ok := busyFrom(got.incoming); ok {
		got.payload.release()
		return result{}, waited, busy
	}
	if err := got.incoming.UnmarshalTo(response); err != nil {
		got.payload.release()
		return result{}, waited, errors.Join(wire.ErrMalformedFrame, err)
	}
	return got, waited, nil
}

func statusError(status migratev1.Status) error {
	switch status {
	case migratev1.Status_STATUS_UNKNOWN_VM:
		return ErrNotServed
	case migratev1.Status_STATUS_UNKNOWN_VOLUME:
		return fmt.Errorf("%w: the peer does not serve that volume", ErrNotServed)
	case migratev1.Status_STATUS_BUSY:
		return ErrBusy
	default:
		return fmt.Errorf("peer: the peer refused a page request: %s", status)
	}
}

// readPayload reads a reply's payload into a pooled buffer of the length its
// frame states, and checks it against what the reply may carry.
func readPayload(incoming wire.Incoming, maximum int64) (*payloadBuffer, error) {
	defer incoming.Payload.Close()
	if incoming.PayloadSize < 0 || incoming.PayloadSize > maximum {
		return nil, platform.ErrMessageTooLarge
	}
	payload := takeBuffer(int(incoming.PayloadSize))
	if _, err := io.ReadFull(incoming.Payload, payload.bytes); err != nil {
		payload.release()
		return nil, err
	}
	// The reader checks a payload's checksum when it reaches the end, which a
	// read of exactly its length need not: one more read says.
	if n, err := incoming.Payload.Read(make([]byte, 1)); n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		payload.release()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return payload, nil
}
