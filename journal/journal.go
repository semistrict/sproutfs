// Package journal is a host's write-ahead log of flushed blocks, kept on a
// network disk of its own (plans/fsync-journal-2026-10-06.md). A flush is
// answered once the blocks it covers are on that disk. If the host is lost,
// another host opens the disk, reads the journal back, and serves its entries
// to the hosts that recover the lost host's VMs.
//
// The disk holds two header slots and a ring. Entries go onto the ring in
// batches, one batch in flight at a time, each written and synced before its
// commits are answered. A position is an entry's logical byte position on the
// ring; it maps to the ring's start plus the position modulo the ring's
// length, and it only grows. An entry never wraps: a pad fills the ring's end.
// A batch ends on a 4 KiB boundary, with a pad before it where needed.
//
// Reading back starts at the header's tail hint and stops at the first entry
// whose magic, version, generation, position or checksum is wrong. That
// position is the head. So the writer never lets the ring overwrite a position
// at or after the tail hint on the disk, and a batch whose write or sync
// failed is padded over by the next one: otherwise reading back would stop at
// the failed range and miss what follows.
//
// Nothing here knows about pagers or control records. A commit's capture makes
// its entries, and the holder trims the journal by the covered positions the
// control records name.
package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// Lease is the assignment a journal disk is opened under: the generation of
// the membership that assigned it, and the member it was assigned to.
type Lease struct {
	Assigned uint64
	Member   rank.Identity
}

// Config is what a journal is opened with.
type Config struct {
	// Identity is the disk's. A disk whose header names another is refused.
	Identity rank.Identity
	// Lease is the assignment the disk is opened under.
	Lease Lease
	// Clock times the tail hint's writes. Nil is the wall clock.
	Clock platform.Clock
	// Entropy draws the generation of a disk that is formatted. Nil is the
	// operating system's.
	Entropy platform.Entropy
}

var (
	// ErrLeased reports a journal disk leased under a newer assignment than
	// the one it was opened under, or to another member.
	ErrLeased = errors.New("journal: the disk is leased under a newer assignment")
	// ErrIdentity reports a disk whose header names another journal disk.
	ErrIdentity = errors.New("journal: the disk is another journal's")
	// ErrClosed reports a journal that was closed.
	ErrClosed = errors.New("journal: the journal is closed")
	// ErrTooLarge reports a commit or an entry larger than the journal holds.
	ErrTooLarge = errors.New("journal: too large for the journal")
)

// The probes the journal marks.
const (
	// ProbeFormatted is a disk with no header, formatted as it opened.
	ProbeFormatted = "journal/formatted"
	// ProbeLeaseRefused is a disk refused as it opened, because its lease is
	// newer than the one it was opened under.
	ProbeLeaseRefused = "journal/lease-refused"
	// ProbeLeaseLost is an open journal that found its lease taken.
	ProbeLeaseLost = "journal/lease-lost"
	// ProbeTornEntry is a read back that ended at an entry whose head holds
	// together and whose checksum does not: a torn batch.
	ProbeTornEntry = "journal/torn-entry"
	// ProbeFailedRangePadded is a batch that padded over a failed one.
	ProbeFailedRangePadded = "journal/failed-range-padded"
	// ProbeRingEndPadded is a pad that filled the ring's end.
	ProbeRingEndPadded = "journal/ring-end-padded"
	// ProbeFull is a commit that waited for room on the ring.
	ProbeFull = "journal/full"
	// ProbeGrouped is a batch that held more than one commit.
	ProbeGrouped = "journal/grouped"
	// ProbeFenced is a commit refused because a read fenced its VM.
	ProbeFenced = "journal/fenced"
)

// Probes is every probe the journal marks.
func Probes() []string {
	return []string{ProbeFormatted, ProbeLeaseRefused, ProbeLeaseLost, ProbeTornEntry, ProbeFailedRangePadded,
		ProbeRingEndPadded, ProbeFull, ProbeGrouped, ProbeFenced}
}

// The fault-injection sites of the journal's writes.
const (
	// BuggifyWriteSlow holds a batch's write for up to 50 ms, so commits pile
	// up behind it.
	BuggifyWriteSlow = "journal/write-slow"
	// BuggifyWriteFails fails a batch's write before it reaches the disk.
	BuggifyWriteFails = "journal/write-fails"
	// BuggifyWriteTorn writes the first half of a batch, then fails.
	BuggifyWriteTorn = "journal/write-torn"
	// BuggifySyncFails fails a batch's sync.
	BuggifySyncFails = "journal/sync-fails"
	// BuggifyHeaderWriteFails fails a write of the header.
	BuggifyHeaderWriteFails = "journal/header-write-fails"
)

// Sites is every fault-injection site of the journal.
func Sites() []string {
	return []string{BuggifyWriteSlow, BuggifyWriteFails, BuggifyWriteTorn, BuggifySyncFails,
		BuggifyHeaderWriteFails}
}

// tailHintInterval is the least time between two writes of the header that
// move the tail hint.
const tailHintInterval = time.Second

// readAhead is how much reading back reads at once.
const readAhead = 1 << 20

// Journal is one journal disk, opened by its holder. It has one writer, which
// runs until Close.
type Journal struct {
	file       platform.File
	clock      platform.Clock
	lease      Lease
	identity   rank.Identity
	generation uint64
	ringStart  int64
	ringLength int64
	formatted  bool

	// ctx is what the writer runs under: Open's context, without its
	// cancellation.
	ctx  context.Context
	done chan struct{}

	mu sync.Mutex
	// changed is closed and replaced whenever the state below changes in a
	// way someone may be waiting for.
	changed chan struct{}
	// header is the header on the disk, in slot current. Its tail is the
	// durable tail: the writer never writes a position at or past it plus
	// the ring's length.
	header   slot
	current  int
	headerAt time.Time
	// tail is the position of the oldest live entry, or written when there
	// is none.
	tail uint64
	// written is the end of what is on the disk and synced. next is the
	// next position to give out. Between them lies a failed batch.
	written, next uint64
	// live is every entry from the tail on, in position order. Dead entries
	// stay in it until the ones before them die.
	live []*indexed
	// held is the live entries of each VM and epoch.
	held map[string]map[uint64]*held
	// fences is the epoch each VM was fenced at by a read: no entry of an
	// older epoch is placed after it.
	fences            map[string]uint64
	queue             []*commit
	placed, completed uint64
	// failed is why the journal no longer writes: its lease was taken.
	failed error
	closed bool
}

// indexed is one entry the journal holds.
type indexed struct {
	position uint64
	length   int64
	vm       string
	epoch    uint64
	dead     bool
}

// held is the live entries of one VM and epoch, in position order.
type held struct {
	entries []*indexed
	bytes   int64
}

// Open opens the journal on file, a journal disk attached to this machine,
// under config's lease. A disk with no header in either slot is formatted
// under a new generation. A disk whose header is of another format version,
// names another disk, or carries a newer lease is refused. Otherwise Open
// takes the lease, writing it into the header, and reads the journal back.
// The journal's writer runs until Close. The file stays the caller's to close.
func Open(ctx context.Context, file platform.File, config Config) (*Journal, error) {
	clock := config.Clock
	if clock == nil {
		clock = platform.WallClock()
	}
	j := &Journal{
		file:     file,
		clock:    clock,
		lease:    config.Lease,
		identity: config.Identity,
		ctx:      context.WithoutCancel(ctx),
		done:     make(chan struct{}),
		changed:  make(chan struct{}),
		held:     make(map[string]map[uint64]*held),
		fences:   make(map[string]uint64),
	}
	size, err := file.Size(ctx)
	if err != nil {
		return nil, fmt.Errorf("journal: reading the disk's size: %w", err)
	}
	if size < ringOffset+minRingSize {
		return nil, fmt.Errorf("journal: a disk of %d bytes is too small for a journal", size)
	}
	found, current, err := readHeader(ctx, file)
	switch {
	case errors.Is(err, errNoSlot):
		entropy := config.Entropy
		if entropy == nil {
			entropy = platform.SystemEntropy()
		}
		length := (size - ringOffset) &^ (BlockBytes - 1)
		// The first position is the ring's length, so no position is zero
		// and a covered position of zero covers nothing.
		found = slot{identity: config.Identity, generation: entropy.Uint64(), ringStart: ringOffset,
			ringLength: length, tail: uint64(length)}
		current = 1
		j.formatted = true
		sim.Probe(ctx, ProbeFormatted)
	case err != nil:
		return nil, err
	case found.identity != config.Identity:
		return nil, fmt.Errorf("%w: it is %s, not %s", ErrIdentity, found.identity, config.Identity)
	case found.ringStart != ringOffset || found.ringLength < minRingSize || found.ringLength%BlockBytes != 0 ||
		found.ringStart+found.ringLength > size:
		return nil, fmt.Errorf("journal: the header's ring of %d bytes at %d does not fit a disk of %d bytes",
			found.ringLength, found.ringStart, size)
	case found.lease.Assigned > config.Lease.Assigned ||
		found.lease.Assigned == config.Lease.Assigned && found.lease.Member != config.Lease.Member:
		sim.Probe(ctx, ProbeLeaseRefused)
		return nil, fmt.Errorf("%w: it is leased to %s under generation %d, not to %s under %d", ErrLeased,
			found.lease.Member, found.lease.Assigned, config.Lease.Member, config.Lease.Assigned)
	}
	j.generation, j.ringStart, j.ringLength = found.generation, found.ringStart, found.ringLength
	j.header, j.current = found, current
	// The lease is taken before anything is read back.
	found.empty = false
	j.headerAt = j.clock.Now()
	if err := j.writeSlot(ctx, found); err != nil {
		return nil, err
	}
	if err := j.readBack(ctx); err != nil {
		return nil, err
	}
	go j.run()
	return j, nil
}

// readHeader reads both slots and returns the one that wins, and its index.
// It is errNoSlot when neither holds a header, and ErrVersion when either
// holds one of another version.
func readHeader(ctx context.Context, file platform.File) (slot, int, error) {
	b := make([]byte, 2*slotBytes)
	if err := readFull(ctx, file, b, 0); err != nil {
		return slot{}, 0, fmt.Errorf("journal: reading the header: %w", err)
	}
	var slots [2]slot
	var valid [2]bool
	for i := range slots {
		s, err := decodeSlot(b[i*slotBytes : (i+1)*slotBytes])
		switch {
		case errors.Is(err, ErrVersion):
			return slot{}, 0, fmt.Errorf("journal: header slot %d: %w", i, err)
		case err == nil:
			slots[i], valid[i] = s, true
		}
	}
	switch {
	case valid[0] && valid[1] && slots[1].counter > slots[0].counter, !valid[0] && valid[1]:
		return slots[1], 1, nil
	case valid[0]:
		return slots[0], 0, nil
	}
	return slot{}, 0, errNoSlot
}

// writeSlot writes s, under this journal's lease and the next counter, into
// the slot that does not hold the current header, and syncs it.
func (j *Journal) writeSlot(ctx context.Context, s slot) error {
	j.mu.Lock()
	s.counter = j.header.counter + 1
	at := 1 - j.current
	j.mu.Unlock()
	s.lease = j.lease
	if sim.Buggify(ctx, BuggifyHeaderWriteFails, 0.1) {
		return fmt.Errorf("journal: writing the header: %w", platform.ErrInjectedFault)
	}
	if _, err := j.file.WriteAt(ctx, encodeSlot(s), int64(at)*slotBytes); err != nil {
		return fmt.Errorf("journal: writing the header: %w", err)
	}
	if err := j.file.Sync(ctx); err != nil {
		return fmt.Errorf("journal: syncing the header: %w", err)
	}
	j.mu.Lock()
	j.header, j.current = s, at
	j.notify()
	j.mu.Unlock()
	return nil
}

// writeHeader writes the tail into the header, and empty. It reads the header
// first, so a holder whose lease was taken stops rather than write over it.
// The attempt counts as a header write whether it lands or not, so one that
// fails is tried again a second later.
func (j *Journal) writeHeader(ctx context.Context, empty bool) error {
	j.mu.Lock()
	j.headerAt = j.clock.Now()
	j.mu.Unlock()
	if err := j.CheckLease(ctx); err != nil {
		return err
	}
	j.mu.Lock()
	s := j.header
	s.tail, s.empty = j.tail, empty
	j.mu.Unlock()
	return j.writeSlot(ctx, s)
}

// CheckLease reads the header and reports ErrLeased if another assignment
// has taken the disk. From then on the journal refuses every commit and read.
// The holder calls it on every pass.
func (j *Journal) CheckLease(ctx context.Context) error {
	found, _, err := readHeader(ctx, j.file)
	if err != nil {
		return err
	}
	if found.lease == j.lease {
		return nil
	}
	err = fmt.Errorf("%w: it is leased to %s under generation %d, not to %s under %d", ErrLeased,
		found.lease.Member, found.lease.Assigned, j.lease.Member, j.lease.Assigned)
	sim.Probe(ctx, ProbeLeaseLost)
	slog.WarnContext(ctx, "journal: the disk's lease was taken", "identity", j.identity.String(), "reason", err)
	j.mu.Lock()
	if j.failed == nil {
		j.failed = err
	}
	j.notify()
	j.mu.Unlock()
	return err
}

// readBack reads the ring from the header's tail hint to the head, and
// indexes every entry it finds.
func (j *Journal) readBack(ctx context.Context) error {
	reader := j.reader()
	start := j.header.tail
	position := start
	for position-start < uint64(j.ringLength) {
		offset, room := j.offset(position)
		if room < minEntryBytes {
			break
		}
		b, err := reader.read(ctx, offset, entryHeadBytes)
		if err != nil {
			return fmt.Errorf("journal: reading back at %d: %w", position, err)
		}
		h, err := decodeHead(b)
		if err != nil || h.position != position || h.generation != j.generation || h.length > room {
			break
		}
		if b, err = reader.read(ctx, offset, h.length); err != nil {
			return fmt.Errorf("journal: reading back at %d: %w", position, err)
		}
		if _, err := checkEntry(b); err != nil {
			if errors.Is(err, errTorn) {
				sim.Probe(ctx, ProbeTornEntry)
			}
			break
		}
		if h.kind == kindBlocks {
			j.index(&indexed{position: position, length: h.length, vm: entryVM(b, h), epoch: h.epoch})
		}
		position += uint64(h.length)
	}
	j.written, j.next = position, position
	j.tail = j.liveTail()
	slog.InfoContext(ctx, "journal: read back", "identity", j.identity.String(), "generation", j.generation,
		"tail", start, "head", position, "entries", len(j.live), "formatted", j.formatted)
	return nil
}

// offset is where position lies on the disk, and how many bytes are left
// from there to the ring's end.
func (j *Journal) offset(position uint64) (int64, int64) {
	within := int64(position % uint64(j.ringLength))
	return j.ringStart + within, j.ringLength - within
}

func (j *Journal) reader() *ringReader {
	return &ringReader{file: j.file, end: j.ringStart + j.ringLength}
}

// index adds a live entry. The caller holds mu, or is Open.
func (j *Journal) index(e *indexed) {
	j.live = append(j.live, e)
	epochs := j.held[e.vm]
	if epochs == nil {
		epochs = make(map[uint64]*held)
		j.held[e.vm] = epochs
	}
	h := epochs[e.epoch]
	if h == nil {
		h = &held{}
		epochs[e.epoch] = h
	}
	h.entries = append(h.entries, e)
	h.bytes += e.length
}

// liveTail is the position of the oldest live entry, or written when there is
// none. The caller holds mu.
func (j *Journal) liveTail() uint64 {
	for len(j.live) > 0 && j.live[0].dead {
		j.live[0] = nil
		j.live = j.live[1:]
	}
	if len(j.live) == 0 {
		return j.written
	}
	return j.live[0].position
}

// notify wakes everyone waiting on changed. The caller holds mu.
func (j *Journal) notify() {
	close(j.changed)
	j.changed = make(chan struct{})
}

// refusal is why the journal takes no commit and serves no read, or nil. The
// caller holds mu.
func (j *Journal) refusal() error {
	switch {
	case j.failed != nil:
		return j.failed
	case j.closed:
		return ErrClosed
	}
	return nil
}

// Generation is the journal's generation, drawn when its disk was formatted.
func (j *Journal) Generation() uint64 { return j.generation }

// Identity is the journal disk's identity, from its header.
func (j *Journal) Identity() rank.Identity { return j.identity }

// Formatted reports whether Open formatted the disk.
func (j *Journal) Formatted() bool { return j.formatted }

// Held is what the journal holds of one VM and epoch.
type Held struct {
	VM    string
	Epoch uint64
	// Entries and Bytes count the live entries.
	Entries int
	Bytes   int64
	// First and Last are the positions of the first and last of them.
	First, Last uint64
}

// Held reports the live entries of every VM and epoch, by VM and then epoch.
func (j *Journal) Held() []Held {
	j.mu.Lock()
	defer j.mu.Unlock()
	var all []Held
	for _, vm := range slices.Sorted(maps.Keys(j.held)) {
		epochs := j.held[vm]
		for _, epoch := range slices.Sorted(maps.Keys(epochs)) {
			h := epochs[epoch]
			all = append(all, Held{VM: vm, Epoch: epoch, Entries: len(h.entries), Bytes: h.bytes,
				First: h.entries[0].position, Last: h.entries[len(h.entries)-1].position})
		}
	}
	return all
}

// Usage is how full the ring is.
type Usage struct {
	// Ring is the ring's length.
	Ring int64
	// Used is the bytes from the tail to the next position: what trimming
	// has not yet freed.
	Used int64
}

// Usage reports how full the ring is.
func (j *Journal) Usage() Usage {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Usage{Ring: j.ringLength, Used: int64(j.next - j.tail)}
}

// Close stops the writer, after the batch in flight, and fails every commit
// still waiting. It then writes the tail into the header, with the empty flag
// when no live entry is left, and reports whether it was. The file stays the
// caller's.
func (j *Journal) Close(ctx context.Context) (bool, error) {
	j.mu.Lock()
	if j.closed {
		j.mu.Unlock()
		return false, ErrClosed
	}
	j.closed = true
	j.notify()
	j.mu.Unlock()
	<-j.done
	j.mu.Lock()
	failed, empty := j.failed, len(j.live) == 0
	j.mu.Unlock()
	if failed != nil {
		return false, failed
	}
	if err := j.writeHeader(ctx, empty); err != nil {
		return false, err
	}
	return empty, nil
}

// ringReader reads the ring through a window, so reading back many small
// entries costs few reads.
type ringReader struct {
	file   platform.File
	end    int64
	window []byte
	at     int64
}

// read returns n bytes at offset, which with n lies within the ring.
func (r *ringReader) read(ctx context.Context, offset, n int64) ([]byte, error) {
	if offset >= r.at && offset+n <= r.at+int64(len(r.window)) {
		return r.window[offset-r.at:][:n], nil
	}
	buffer := make([]byte, min(max(n, readAhead), r.end-offset))
	if err := readFull(ctx, r.file, buffer, offset); err != nil {
		return nil, err
	}
	r.window, r.at = buffer, offset
	return buffer[:n], nil
}

// readFull reads exactly len(buffer) bytes at offset.
func readFull(ctx context.Context, file platform.File, buffer []byte, offset int64) error {
	n, err := file.ReadAt(ctx, buffer, offset)
	if n == len(buffer) && (err == nil || errors.Is(err, io.EOF)) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}
