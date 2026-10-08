package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/protobuf/proto"

	"github.com/semistrict/sproutfs/journal"
	peerv1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/peer/v1"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
)

// JOURNAL_READ is how a host that opens a VM after a host loss reads the
// flushed writes the lost host's journal holds of it: from whichever host
// holds that journal disk now (plans/fsync-journal-2026-10-06.md).

// ErrNoJournal reports a peer that holds no journal disk of the identity
// asked for.
var ErrNoJournal = errors.New("peer: the peer holds no such journal disk")

// Journals is what answers JOURNAL_READ on a host: the journal disks it
// holds.
type Journals interface {
	// ReadJournal fences and reads request on the journal disk named disk,
	// as journal.Journal.Read does, and gives up the VM if this host runs it
	// at an older epoch. It reports ErrNoJournal for a disk not held here.
	ReadJournal(ctx context.Context, disk rank.Identity, request journal.ReadRequest,
		yield func(journal.Entry) error) error
}

// JournalPage is one answer to a read of a journal: entries in position
// order, and whether the reader asks again after the last.
type JournalPage struct {
	Entries []journal.Entry
	More    bool
}

// errPageFull stops a read whose answer is full.
var errPageFull = errors.New("peer: the answer is full")

// answerJournalRead answers a read of a journal disk with the entries its
// holder has, as many as fit in the bytes asked for and at least one.
func (s *Server) answerJournalRead(session *session, request *peerv1.JournalRead) answer {
	refuse := func(status peerv1.JournalStatus) answer {
		return answer{message: peerv1.JournalEntries_builder{Status: status.Enum()}.Build()}
	}
	if s.config.Journals == nil || len(request.GetDisk()) != len(rank.Identity{}) {
		return refuse(peerv1.JournalStatus_JOURNAL_STATUS_NOT_HERE)
	}
	maximum := int64(min(request.GetMaxBytes(), uint64(platform.MaxFrameBytes)))
	release, busy := s.reserve(session, maximum)
	if busy != nil {
		return answer{message: busy}
	}
	read := journal.ReadRequest{VM: request.GetVm(), Epoch: request.GetEpoch(), After: request.GetAfter(),
		Generation: request.GetGeneration(), Reader: request.GetReader()}
	var entries []*peerv1.JournalEntry
	var payload []byte
	more := false
	err := s.config.Journals.ReadJournal(s.ctx, rank.Identity(request.GetDisk()), read, func(e journal.Entry) error {
		if len(entries) > 0 && int64(len(payload)+len(e.Data)) > maximum {
			more = true
			return errPageFull
		}
		entries = append(entries, peerv1.JournalEntry_builder{Position: proto.Uint64(e.Position),
			Volume: proto.String(e.Volume), Blocks: e.Blocks}.Build())
		payload = append(payload, e.Data...)
		return nil
	})
	switch {
	case errors.Is(err, errPageFull), err == nil:
	case errors.Is(err, ErrNoJournal):
		release()
		return refuse(peerv1.JournalStatus_JOURNAL_STATUS_NOT_HERE)
	case errors.Is(err, journal.ErrGeneration):
		release()
		return refuse(peerv1.JournalStatus_JOURNAL_STATUS_GENERATION)
	default:
		release()
		slog.WarnContext(s.ctx, "peer: a journal read could not be answered", "peer", session.peer,
			"vm", read.VM, "error", err)
		return refuse(peerv1.JournalStatus_JOURNAL_STATUS_UNSPECIFIED)
	}
	return answer{
		message: peerv1.JournalEntries_builder{Status: peerv1.JournalStatus_JOURNAL_STATUS_OK.Enum(),
			Entries: entries, More: proto.Bool(more)}.Build(),
		payload: payload, checked: true,
		sent: func(bool) { release() },
	}
}

// ReadJournal asks the peer for one page of request's entries on the journal
// disk named disk, at most maxBytes of blocks and at least one entry. The
// reader asks again after the last entry while the page says more. It goes
// in the Fault class unless ctx names another: a VM waits for it to start.
func (p *Peer) ReadJournal(ctx context.Context, disk rank.Identity, request journal.ReadRequest,
	maxBytes int64) (JournalPage, error) {
	ctx = WithClass(ctx, classOr(ctx, Fault))
	wire := peerv1.JournalRead_builder{Disk: disk[:], Vm: proto.String(request.VM),
		Epoch: proto.Uint64(request.Epoch), After: proto.Uint64(request.After),
		Generation: proto.Uint64(request.Generation), Reader: proto.Uint64(request.Reader),
		MaxBytes: proto.Uint64(uint64(maxBytes))}.Build()
	response := new(peerv1.JournalEntries)
	got, _, err := p.call(ctx, requestJournal, "", wire, response, maxBytes, maxBytes, nil)
	if err != nil {
		return JournalPage{}, err
	}
	defer got.payload.release()
	switch response.GetStatus() {
	case peerv1.JournalStatus_JOURNAL_STATUS_OK:
	case peerv1.JournalStatus_JOURNAL_STATUS_NOT_HERE:
		return JournalPage{}, fmt.Errorf("%w: %s", ErrNoJournal, disk)
	case peerv1.JournalStatus_JOURNAL_STATUS_GENERATION:
		return JournalPage{}, fmt.Errorf("%w: %s", journal.ErrGeneration, disk)
	default:
		return JournalPage{}, fmt.Errorf("peer: the read of journal %s failed at the peer", disk)
	}
	page := JournalPage{More: response.GetMore()}
	data := got.payload.bytes
	for _, e := range response.GetEntries() {
		size := len(e.GetBlocks()) * journal.BlockBytes
		if size > len(data) {
			return JournalPage{}, fmt.Errorf("peer: a journal answer's entries name %d more bytes than it carries",
				size-len(data))
		}
		page.Entries = append(page.Entries, journal.Entry{VM: request.VM, Volume: e.GetVolume(),
			Epoch: request.Epoch, Blocks: e.GetBlocks(), Data: append([]byte(nil), data[:size]...),
			Position: e.GetPosition()})
		data = data[size:]
	}
	if len(data) != 0 {
		return JournalPage{}, fmt.Errorf("peer: a journal answer carries %d bytes its entries do not name", len(data))
	}
	return page, nil
}
