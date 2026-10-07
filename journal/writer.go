package journal

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Capture makes the entries of one commit. The writer calls it as it forms the
// batch the commit joins, after the batch before has completed, so a capture
// takes everything stored up to then. Its entries must fit in the room the
// commit asked for, as EntryBytes counts them.
type Capture func(ctx context.Context) ([]Entry, error)

// Hooks are what a commit's caller learns on the writer, in batch order and
// before the writer forms its next batch, which no answer can promise: an
// answer reaches its caller whenever that caller runs.
type Hooks struct {
	// Placed runs once the commit's entries have positions, before they are
	// written: the start of each, none where the capture failed or a read had
	// fenced the commit's VM.
	Placed func(positions []uint64)
	// Failed runs when a commit whose capture made entries does not land:
	// its batch's write or sync failed, or a read had fenced its VM. Its
	// entries may be on the disk all the same, so whatever the capture took
	// has to be taken again by the next.
	Failed func()
}

// commit is one call of Commit, waiting for its batch.
type commit struct {
	room      int64
	capture   Capture
	hooks     Hooks
	captured  bool
	done      chan struct{}
	positions []uint64
	err       error
}

// reserve is the room a batch of commits needs on the ring: their room, a pad
// at the ring's end shorter than the largest entry and a pad's head, and a pad
// to the next 4 KiB boundary.
func reserve(room, largest int64) int64 { return room + largest + BlockBytes + 2*minEntryBytes }

// Commit puts the entries capture makes onto the ring and returns their
// positions once the batch that holds them is written and synced. Room is the
// most bytes the entries may take. A commit waits while a batch is in flight,
// and while the ring has no room for it; it fails when its capture fails, when
// its batch's write or sync fails, when a read has fenced the VM of one of its
// entries at a newer epoch, and when the journal is closed or its lease taken.
// Batches complete in position order, so commits are answered in that order
// too. A read may still find the entries of a commit that failed, as a power
// loss may keep stores no flush covered.
func (j *Journal) Commit(ctx context.Context, room int64, capture Capture) ([]uint64, error) {
	return j.CommitHooked(ctx, room, capture, Hooks{})
}

// CommitHooked is Commit with hooks the writer runs as the commit is placed
// and if it fails.
func (j *Journal) CommitHooked(ctx context.Context, room int64, capture Capture, hooks Hooks) ([]uint64, error) {
	if room < 0 || reserve(room, room) > j.ringLength {
		return nil, fmt.Errorf("%w: a commit of %d bytes, on a ring of %d", ErrTooLarge, room, j.ringLength)
	}
	c := &commit{room: room, capture: capture, hooks: hooks, done: make(chan struct{})}
	j.mu.Lock()
	if err := j.refusal(); err != nil {
		j.mu.Unlock()
		return nil, err
	}
	j.queue = append(j.queue, c)
	j.notify()
	j.mu.Unlock()
	select {
	case <-c.done:
		return c.positions, c.err
	case <-ctx.Done():
		j.mu.Lock()
		if at := slices.Index(j.queue, c); at >= 0 {
			j.queue = slices.Delete(j.queue, at, at+1)
		}
		j.mu.Unlock()
		return nil, context.Cause(ctx)
	}
}

// run is the writer: it forms one batch at a time, writes it, syncs it and
// answers its commits, until the journal is closed or its lease taken.
func (j *Journal) run() {
	defer close(j.done)
	for {
		batch := j.nextBatch()
		if batch == nil {
			return
		}
		j.commitBatch(batch)
	}
}

// nextBatch waits for commits the ring has room for and takes as many as fit
// in one batch, in the order they came. Meanwhile it writes the tail hint once
// the tail has moved and a second has passed since the last header write. It
// returns nil once the journal stops, after failing every commit left.
func (j *Journal) nextBatch() []*commit {
	waited := false
	for {
		j.mu.Lock()
		if err := j.refusal(); err != nil {
			left := j.queue
			j.queue = nil
			j.mu.Unlock()
			for _, c := range left {
				c.err = err
				close(c.done)
			}
			return nil
		}
		moved := j.tail != j.header.tail
		if moved && j.clock.Since(j.headerAt) >= tailHintInterval {
			j.mu.Unlock()
			if err := j.writeHeader(j.ctx, false); err != nil {
				slog.WarnContext(j.ctx, "journal: the tail hint was not written", "identity", j.identity.String(),
					"err", err)
			}
			continue
		}
		if n := j.fitting(); n > 0 {
			batch := slices.Clone(j.queue[:n])
			j.queue = slices.Delete(j.queue, 0, n)
			j.mu.Unlock()
			return batch
		}
		if len(j.queue) > 0 && !waited {
			waited = true
			sim.Probe(j.ctx, ProbeFull)
		}
		if len(j.queue) > 0 && j.written < j.next && !sim.Bug(j.ctx, "journal-pad-only-with-a-commit") {
			// A failed range is given back only by a batch's pad, and nothing
			// in it is live for a trim to free: the next batch is that pad
			// alone.
			j.mu.Unlock()
			return []*commit{}
		}
		changed := j.changed
		if !moved {
			j.mu.Unlock()
			<-changed
			continue
		}
		timer := j.clock.NewTimer(tailHintInterval - j.clock.Since(j.headerAt))
		j.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C():
		}
		timer.Stop()
	}
}

// fitting is how many of the queued commits the next batch takes: those that
// fit in MaxBatchBytes, at least one, while the ring has room for them before
// the durable tail. The caller holds mu.
func (j *Journal) fitting() int {
	var room, largest int64
	n := 0
	for _, c := range j.queue {
		if n > 0 && room+c.room > MaxBatchBytes {
			break
		}
		if j.next+uint64(reserve(room+c.room, max(largest, c.room))) > j.header.tail+uint64(j.ringLength) {
			break
		}
		room, largest = room+c.room, max(largest, c.room)
		n++
	}
	return n
}

// placement is a batch laid out on the ring: its bytes from start to end, the
// entries it adds, and the positions of each commit's entries.
type placement struct {
	start, end uint64
	image      []byte
	entries    []*indexed
	positions  [][]uint64
}

// commitBatch captures a batch's commits, lays them out, writes and syncs
// them, and answers them.
func (j *Journal) commitBatch(batch []*commit) {
	if len(batch) > 1 {
		sim.Probe(j.ctx, ProbeGrouped)
	}
	captured := make([][]Entry, len(batch))
	for i, c := range batch {
		captured[i], c.err = capture(j.ctx, c)
		c.captured = c.err == nil
	}
	j.mu.Lock()
	p := j.place(batch, captured)
	j.placed++
	if p.image != nil {
		j.next = p.end
	}
	j.mu.Unlock()
	for i, c := range batch {
		if c.hooks.Placed != nil {
			c.hooks.Placed(p.positions[i])
		}
		if c.captured && c.err != nil && c.hooks.Failed != nil {
			// A read fenced its VM: it is refused whole.
			c.hooks.Failed()
		}
	}
	if p.image == nil {
		j.complete(batch, p, nil)
		return
	}
	if err := j.write(p); err != nil {
		j.complete(batch, p, err)
		return
	}
	if sim.Bug(j.ctx, "journal-answer-before-sync") {
		j.complete(batch, p, nil)
		if err := j.sync(); err != nil {
			slog.WarnContext(j.ctx, "journal: a batch's sync failed", "start", p.start, "err", err)
		}
		return
	}
	j.complete(batch, p, j.sync())
}

// capture runs one commit's capture and checks what it made.
func capture(ctx context.Context, c *commit) ([]Entry, error) {
	entries, err := c.capture(ctx)
	if err != nil {
		return nil, fmt.Errorf("journal: capturing a commit: %w", err)
	}
	var bytes int64
	for _, e := range entries {
		if err := e.check(); err != nil {
			return nil, err
		}
		bytes += e.size()
	}
	if bytes > c.room {
		return nil, fmt.Errorf("%w: a capture of %d bytes in a commit of %d", ErrTooLarge, bytes, c.room)
	}
	return entries, nil
}

// place lays a batch out from written: pads over a failed batch, then each
// commit's entries, with a pad wherever an entry would cross the ring's end,
// and a pad to the next 4 KiB boundary. A commit with an entry of a VM a read
// fenced at a newer epoch is refused whole. A batch with neither an entry nor
// a failed range to pad has no image and writes nothing. The caller holds mu.
func (j *Journal) place(batch []*commit, captured [][]Entry) placement {
	p := placement{start: j.written, positions: make([][]uint64, len(batch))}
	position := j.written
	pad := func(length int64) {
		p.image = appendPad(p.image, length, position, j.generation)
		position += uint64(length)
	}
	padded := position < j.next
	for position < j.next {
		_, room := j.offset(position)
		length := min(int64(j.next-position), room)
		if length > MaxEntryBytes {
			length = MaxEntryBytes / 2
		}
		pad(length)
	}
	for i, c := range batch {
		if c.err == nil {
			c.err = j.fenced(captured[i])
		}
		if c.err != nil {
			continue
		}
		for _, e := range captured[i] {
			size := e.size()
			if _, room := j.offset(position); size != room && size+minEntryBytes > room {
				pad(room)
				sim.Probe(j.ctx, ProbeRingEndPadded)
			}
			p.entries = append(p.entries, &indexed{position: position, length: size, vm: e.VM, epoch: e.Epoch})
			p.positions[i] = append(p.positions[i], position)
			p.image = appendEntry(p.image, e, position, j.generation)
			position += uint64(size)
		}
	}
	if len(p.entries) == 0 && !padded {
		return placement{start: j.written, positions: p.positions}
	}
	if padded {
		sim.Probe(j.ctx, ProbeFailedRangePadded)
	}
	if gap := int64((BlockBytes - position%BlockBytes) % BlockBytes); gap > 0 {
		if gap < minEntryBytes {
			gap += BlockBytes
		}
		pad(gap)
	}
	p.end = position
	return p
}

// fenced refuses entries of a VM a read fenced at a newer epoch. The caller
// holds mu.
func (j *Journal) fenced(entries []Entry) error {
	for _, e := range entries {
		if fence := j.fences[e.VM]; e.Epoch < fence {
			sim.Probe(j.ctx, ProbeFenced)
			return fmt.Errorf("%w: %s at epoch %d, fenced at %d", ErrFenced, e.VM, e.Epoch, fence)
		}
	}
	return nil
}

// write writes a batch's image onto the ring: one write, or two where it
// crosses the ring's end.
func (j *Journal) write(p placement) error {
	if err := sim.BuggifyDelay(j.ctx, BuggifyWriteSlow, 0.2, 50*time.Millisecond); err != nil {
		return err
	}
	if sim.Buggify(j.ctx, BuggifyWriteFails, 0.1) {
		return fmt.Errorf("journal: writing the batch at %d: %w", p.start, platform.ErrInjectedFault)
	}
	image := p.image
	torn := sim.Buggify(j.ctx, BuggifyWriteTorn, 0.1)
	if torn {
		image = image[:len(image)/2]
	}
	for position := p.start; len(image) > 0; {
		offset, room := j.offset(position)
		n := min(int64(len(image)), room)
		if _, err := j.file.WriteAt(j.ctx, image[:n], offset); err != nil {
			return fmt.Errorf("journal: writing the batch at %d: %w", p.start, err)
		}
		image, position = image[n:], position+uint64(n)
	}
	if torn {
		return fmt.Errorf("journal: writing the batch at %d: torn: %w", p.start, platform.ErrInjectedFault)
	}
	return nil
}

// sync syncs the disk under a batch.
func (j *Journal) sync() error {
	if sim.Buggify(j.ctx, BuggifySyncFails, 0.1) {
		return fmt.Errorf("journal: syncing a batch: %w", platform.ErrInjectedFault)
	}
	if err := j.file.Sync(j.ctx); err != nil {
		return fmt.Errorf("journal: syncing a batch: %w", err)
	}
	return nil
}

// complete answers a batch's commits. A batch that reached the disk moves
// written on and indexes its entries. One that failed leaves its range
// between written and next, for the next batch to pad over.
func (j *Journal) complete(batch []*commit, p placement, err error) {
	j.mu.Lock()
	j.completed++
	if err == nil {
		if p.image != nil {
			j.written = p.end
		}
		for _, e := range p.entries {
			j.index(e)
		}
		j.tail = j.liveTail()
	} else {
		slog.WarnContext(j.ctx, "journal: a batch failed", "identity", j.identity.String(), "start", p.start,
			"bytes", len(p.image), "err", err)
	}
	var failed []*commit
	for i, c := range batch {
		switch {
		case c.err != nil:
		case err != nil:
			c.err = err
			failed = append(failed, c)
		default:
			c.positions = p.positions[i]
		}
	}
	j.notify()
	j.mu.Unlock()
	for _, c := range failed {
		if c.hooks.Failed != nil {
			c.hooks.Failed()
		}
	}
	for _, c := range batch {
		close(c.done)
	}
}
