package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/internal/latency"
	"github.com/semistrict/sproutfs/journal"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/volume"
)

// Durable flush (plans/fsync-journal-2026-10-06.md) is an optional mode, off
// by default. Off, a guest's flush is answered by the flush bound (flush.go).
// On, a flush of a disk returns only once the changed blocks of everything the
// guest stored before it are on the host's journal disk, a network disk the
// cloud replicates; a flush that cannot be journaled fails, and the guest
// reads an I/O error. There is no fallback to a checkpoint.
//
// A flush captures its disk's changed blocks (vmmemory/journal.go) as one
// commit of the journal, which batches the flushes that arrive while a batch
// is in flight into the next one. The commit's capture runs on the journal's
// writer as it forms that batch, under the VM's journal lock.
//
// A checkpoint's selection names the journal and the position it covers: a
// replay applies only the entries after it. The pause that seals the
// checkpoint takes the VM's journal lock once its disks are sealed and before
// the guest runs again, and records how many captures of the VM had begun.
// Those captures read nothing newer than the seal, and every later one comes
// after it in the journal, so the highest position of the first ones is the
// covered position. The journal reports positions as it places each batch
// (journal.Hooks), and the selection waits for those it needs.

// ErrJournalUnavailable reports a flush this host could not journal: no
// journal disk is served here.
var ErrJournalUnavailable = errors.New("host: no journal disk is served on this host")

// JournalConfig is the durable flush's configuration.
type JournalConfig struct {
	// DurableFlush answers each flush of a disk from the host's journal,
	// and fails one that cannot be journaled. SPROUTFS_DURABLE_FLUSH.
	DurableFlush bool
	// Devices, where it is not nil, has the host open the journal disks the
	// membership assigns it, network disks the controller attaches to its
	// machine: the one reserved for Machine, which it writes, and others for
	// reading (journaldisks.go). Without it, the journal is what SetJournal
	// gives the host. Interval is how often the host looks again at what the
	// membership assigns it; zero is DefaultJournalDiskInterval.
	Devices  platform.Devices
	Machine  string
	Interval time.Duration
}

// journals is the host's side of durable flush: whether the mode is on, the
// journal it writes, and what its flushes did.
type journals struct {
	on bool
	mu sync.Mutex
	// current is the journal disk this host writes, nil while none is served.
	current *journal.Journal
	// flushes counts the flushes the mode answered, by outcome; flushTime is
	// what each took from its arrival to its answer, and captureTime what its
	// capture took.
	flushes                outcomes
	flushTime, captureTime latency.Histogram
}

// SetJournal makes j the journal this host writes its VMs' flushes to, nil
// for none: what serves the host's journal disk calls it as the disk comes
// and goes.
func (h *Host) SetJournal(j *journal.Journal) {
	h.journals.mu.Lock()
	defer h.journals.mu.Unlock()
	h.journals.current = j
}

// journal is the journal this host writes, nil for none.
func (h *Host) journal() *journal.Journal {
	h.journals.mu.Lock()
	defer h.journals.mu.Unlock()
	return h.journals.current
}

// vmJournal is one VM's journal state on this host. captures counts the
// captures of it begun and placed those the journal has placed, in the order
// they began; last is the highest position any placed one was given. covers
// is the pauses waiting for captures to be placed. inflight is, per memory
// region, the pages captured into commits that have not ended yet, which a
// flush has to cover as well: if that commit fails they are unjournaled
// again. changed is closed and replaced whenever placed moves. pauses counts
// the checkpoints' pauses.
type vmJournal struct {
	mu                             sync.Mutex
	captures, placed, last, pauses uint64
	covers                         []*cover
	inflight                       map[*vmmemory.MemoryRegion]map[uint64]int
	changed                        chan struct{}
	// postCopy marks a VM a migration brought here whose post-copy has not
	// ended, and root a fork's child whose root is not selected. Its flushes
	// wait for both (journaled), unless the VM leaves this host first: ended.
	postCopy, root, ended bool
}

// signalLocked wakes whatever waits for the state to move. Caller holds mu.
func (s *vmJournal) signalLocked() {
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// placedOne records that the journal placed the next capture, at positions.
func (s *vmJournal) placedOne(positions []uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.placed++
	for _, position := range positions {
		s.last = max(s.last, position)
	}
	waiting := s.covers[:0]
	for _, c := range s.covers {
		if c.token <= s.placed {
			c.resolve(s.last)
			continue
		}
		waiting = append(waiting, c)
	}
	clear(s.covers[len(waiting):])
	s.covers = waiting
	s.signalLocked()
}

// mayJournal reports whether a flush of vm may be journaled in j now. A
// migration's destination journals none until the post-copy ends: the pages
// the guest stored into on the source since their last capture are in no
// journal, and reach this host only in the post-copy, where they become
// unjournaled here. A fork's child journals none until its root is selected:
// a recovery cannot open a VM whose root was never selected. And no VM
// journals any until its record names j at its epoch: a recovery replays only
// the journals the record names, so an entry of any other is never read.
func (h *Host) mayJournal(vm *volume.VM, j *journal.Journal, state *vmJournal) bool {
	state.mu.Lock()
	copying := state.postCopy && !sim.Bug(h.ctx, "journal-answer-before-post-copy") || state.root
	state.mu.Unlock()
	if copying {
		return false
	}
	return names(vm.Journals(), j, vm.Epoch()) || sim.Bug(h.ctx, "journal-answer-unnamed")
}

// names reports whether journals names j at epoch.
func names(journals []control.Journal, j *journal.Journal, epoch uint64) bool {
	return slices.ContainsFunc(journals, func(named control.Journal) bool {
		return named.Disk == j.Identity() && named.Generation == j.Generation() && named.Epoch == epoch
	})
}

// journaled waits until a flush of vmID may be journaled (mayJournal). Where
// only the record's naming of the journal is missing, it asks for a
// checkpoint out of the interval's turn, whose selection names it.
func (h *Host) journaled(ctx context.Context, vmID string, vm *volume.VM, j *journal.Journal,
	entry *registration) error {
	state := &entry.journal
	for {
		state.mu.Lock()
		if state.ended {
			state.mu.Unlock()
			return errLeft
		}
		if state.changed == nil {
			state.changed = make(chan struct{})
		}
		changed, copying := state.changed, state.postCopy || state.root
		state.mu.Unlock()
		if h.mayJournal(vm, j, state) {
			return nil
		}
		if !copying {
			h.askCheckpoint(entry)
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// errLeft is a flush that waited for a VM that has since left this host. It
// goes unanswered, as every flush of such a VM does (registration).
var errLeft = errors.New("host: the VM left this host")

// left ends every wait of journaled: the VM has left this host.
func (s *vmJournal) left() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
	s.signalLocked()
}

// opened ends one of the waits journaled waits for.
func (s *vmJournal) opened(postCopy, root bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.postCopy = s.postCopy && !postCopy
	s.root = s.root && !root
	s.signalLocked()
}

// pending is the pages of region a flush now has to cover: the unjournaled
// ones, and those captured into commits that have not ended. pauses is how
// many checkpoints had paused the VM by then.
func (s *vmJournal) pending(region *vmmemory.MemoryRegion) (pages []uint64, pauses uint64) {
	pages = region.Unjournaled()
	s.mu.Lock()
	defer s.mu.Unlock()
	if inflight := s.inflight[region]; len(inflight) > 0 {
		pages = append(pages, slices.Collect(maps.Keys(inflight))...)
		slices.Sort(pages)
		pages = slices.Compact(pages)
	}
	return pages, s.pauses
}

// holdLocked records the pages of a capture as in flight until its commit
// ends; release ends them. Caller holds mu.
func (s *vmJournal) holdLocked(region *vmmemory.MemoryRegion, pages []uint64) {
	if s.inflight == nil {
		s.inflight = make(map[*vmmemory.MemoryRegion]map[uint64]int)
	}
	held := s.inflight[region]
	if held == nil {
		held = make(map[uint64]int)
		s.inflight[region] = held
	}
	for _, page := range pages {
		held[page]++
	}
}

func (s *vmJournal) release(region *vmmemory.MemoryRegion, pages []uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.inflight[region]
	for _, page := range pages {
		if held[page]--; held[page] <= 0 {
			delete(held, page)
		}
	}
	if len(held) == 0 {
		delete(s.inflight, region)
	}
}

// waitingFlush is a durable flush of a memory region no VM here is registered
// for yet.
type waitingFlush struct {
	region *vmmemory.MemoryRegion
	done   func(error)
}

// registeredFor is the VM registered here that maps region. A VMM sends the
// flushes it held when it stopped again as soon as it runs, which is before
// the host that started it registers it: answered then, such a flush would
// be answered with nothing journaled, ahead of a destination's post-copy. So
// a flush of a region no VM here is registered for waits until its VM is,
// and goes unanswered with a VMM given up before then, as every flush of a
// VM that leaves a host does. It reports false for such a flush.
func (h *Host) registeredFor(region *vmmemory.MemoryRegion, done func(error)) (string, *registration, bool) {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	for vmID, entry := range h.machines.running {
		for _, mapped := range entry.runtime.MemoryRegions() {
			if mapped == region {
				return vmID, entry, true
			}
		}
	}
	if sim.Bug(h.ctx, "journal-answer-before-registered") {
		done(nil)
		return "", nil, false
	}
	if h.machines.waiting == nil {
		h.machines.waiting = make(map[*vmmemory.MemoryRegion][]func(error))
	}
	h.machines.waiting[region] = append(h.machines.waiting[region], done)
	return "", nil, false
}

// takeWaitingLocked takes the flushes waiting for runtime's registration, in
// the order of its memory regions' names. Caller holds h.machines.mu.
func (h *Host) takeWaitingLocked(runtime Machine) []waitingFlush {
	regions := runtime.MemoryRegions()
	var taken []waitingFlush
	for _, name := range slices.Sorted(maps.Keys(regions)) {
		region := regions[name]
		for _, done := range h.machines.waiting[region] {
			taken = append(taken, waitingFlush{region: region, done: done})
		}
		delete(h.machines.waiting, region)
	}
	return taken
}

// dropWaiting drops unanswered the flushes of a VMM this host started that
// wait for its registration: it is given up.
func (h *Host) dropWaiting(runtime Machine) {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	h.takeWaitingLocked(runtime)
}

// closeRuntime closes a VMM process this host started, and drops the flushes
// of it still waiting.
func (h *Host) closeRuntime(runtime Machine) error {
	err := runtime.Close()
	h.dropWaiting(runtime)
	return err
}

// flushedDurable answers a flush of one disk in the durable flush mode: once
// its changed blocks are in the journal, or with the reason they could not be.
func (h *Host) flushedDurable(region *vmmemory.MemoryRegion, done func(error)) {
	vmID, entry, registered := h.registeredFor(region, done)
	if !registered {
		return
	}
	if entry.now == nil || entry.cadence.flushBound <= 0 {
		// Nothing would ever trim what such a VM's flushes journal: a VM with
		// no checkpoint loop, or one that asked for no checkpoints, was
		// given disks that are not durable, as the flush bound treats it.
		done(nil)
		return
	}
	vm := h.vm(vmID)
	j := h.journal()
	if vm == nil || j == nil {
		h.journals.flushes.ended(ErrJournalUnavailable)
		done(fmt.Errorf("%w: a flush of %s", ErrJournalUnavailable, vmID))
		return
	}
	volume := ""
	for name, mapped := range entry.runtime.MemoryRegions() {
		if mapped == region {
			volume = name
		}
	}
	began := h.clock.Now()
	// The pages are the ones unjournaled when the flush arrived, unless it
	// waits: then no capture runs before it, and it takes those unjournaled
	// when the wait ends, which is all the guest stored before it.
	state := &entry.journal
	waits := !h.mayJournal(vm, j, state)
	var pages []uint64
	var pauses uint64
	if !waits {
		pages, pauses = state.pending(region)
	}
	go func() {
		var err error
		if waits {
			if err = h.journaled(h.ctx, vmID, vm, j, entry); err == nil {
				pages, pauses = state.pending(region)
			}
		}
		if errors.Is(err, errLeft) {
			return
		}
		if err == nil {
			err = h.journalFlush(h.ctx, j, entry, vmID, vm.Epoch(), volume, region, pages, pauses)
		}
		h.journals.flushes.ended(err)
		h.journals.flushTime.Observe(h.clock.Since(began))
		if err != nil {
			slog.WarnContext(h.ctx, "host: a flush could not be journaled", "vm", vmID, "volume", volume, "error", err)
			done(fmt.Errorf("host: journaling a flush of %s/%s: %w", vmID, volume, err))
			return
		}
		done(nil)
	}()
}

// journalFlush commits a capture of pages of region to j, and returns once
// the batch that holds it has synced, or with why it did not. pauses is how
// many checkpoints had paused the VM when the flush chose its pages: a pause
// since took the unjournaled ones among them to its seal's list, where the
// capture finds them.
func (h *Host) journalFlush(ctx context.Context, j *journal.Journal, entry *registration, vmID string, epoch uint64,
	volume string, region *vmmemory.MemoryRegion, pages []uint64, pauses uint64) error {
	state := &entry.journal
	if err := sim.BuggifyDelay(ctx, BuggifyJournalCaptureSlow, 0.2, 20*time.Millisecond); err != nil {
		return err
	}
	if err := h.underHalfRing(ctx, j, vmID, entry); err != nil {
		return err
	}
	room := roomFor(vmID, volume, len(pages)*region.BlocksPerPage())
	// Asked before the commit as well as after: a commit the ring has no
	// room for waits until a selection trims it, and the interval may be a
	// long way off.
	h.relieveRing(j, room)
	defer h.relieveRing(j, 0)
	var captured *vmmemory.Captured
	capture := func(ctx context.Context) ([]journal.Entry, error) {
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.pauses != pauses {
			sim.Probe(ctx, ProbeJournalCaptureAfterPause)
		}
		state.captures++
		began := h.clock.Now()
		c, err := region.Capture(ctx, pages)
		h.journals.captureTime.Observe(h.clock.Since(began))
		if err != nil {
			return nil, err
		}
		captured = c
		// In flight from here, under the VM's journal lock, so a flush never
		// finds the pages neither unjournaled nor in flight.
		state.holdLocked(region, c.Pages())
		return entriesOf(vmID, volume, epoch, c), nil
	}
	hooks := journal.Hooks{
		Placed: state.placedOne,
		Failed: func() {
			if captured != nil && !sim.Bug(ctx, "journal-failed-write-keeps-pages") {
				captured.Fail(ctx)
			}
		},
		// The commits placed ahead of this one may have taken the room it
		// had when it was asked for.
		Full: func() { h.relieveRing(j, room) },
	}
	_, err := j.CommitHooked(ctx, room, capture, hooks)
	if captured != nil {
		state.release(region, captured.Pages())
	}
	if err != nil && sim.Bug(ctx, "journal-failed-write-answers") {
		// The flush is answered although its blocks never reached the disk.
		return nil
	}
	return err
}

// maxBlocksPerEntry is how many blocks one entry of vm's volume holds.
func maxBlocksPerEntry(vm, volume string) int {
	return int((journal.MaxEntryBytes - journal.EntryBytes(vm, volume, 0)) / (8 + journal.BlockBytes))
}

// roomFor is the most bytes the entries of blocks blocks of one volume take.
func roomFor(vm, volume string, blocks int) int64 {
	per := maxBlocksPerEntry(vm, volume)
	var room int64
	for blocks > 0 {
		n := min(blocks, per)
		room += journal.EntryBytes(vm, volume, n)
		blocks -= n
	}
	return room
}

// entriesOf is the entries of one capture, each as large as an entry may be.
func entriesOf(vm, volume string, epoch uint64, c *vmmemory.Captured) []journal.Entry {
	per := maxBlocksPerEntry(vm, volume)
	var entries []journal.Entry
	for first := 0; first < len(c.Blocks); first += per {
		last := min(first+per, len(c.Blocks))
		entries = append(entries, journal.Entry{VM: vm, Volume: volume, Epoch: epoch,
			Blocks: c.Blocks[first:last], Data: c.Data[first*journal.BlockBytes : last*journal.BlockBytes]})
	}
	return entries
}

// cover is what one checkpoint of a VM whose flushes this host journals
// covers: the captures that began before its pause ended, and so the highest
// position the journal gave any of them. It is the checkpoint's
// volume.JournalCover.
type cover struct {
	h     *Host
	vmID  string
	state *vmJournal
	j     *journal.Journal
	epoch uint64
	// token is how many captures had begun when the pause ended. covered is
	// the position they reach, once resolved closes. late marks a cover a
	// guard has read at its selection rather than at its pause.
	token    uint64
	covered  uint64
	resolved chan struct{}
	late     bool
	// earlier marks a checkpoint paused before its VM's post-copy ended: it
	// may not hold every page the source held, so its selection keeps the
	// journals of earlier epochs the record names.
	earlier bool
}

// coverOf is the cover of a checkpoint of vmID that is about to be taken,
// nil where the mode is off or no journal is served: such a checkpoint names
// no journal.
func (h *Host) coverOf(vmID string, entry *registration) volume.JournalCover {
	if !h.journals.on || entry == nil {
		return nil
	}
	j := h.journal()
	vm := h.vm(vmID)
	if j == nil || vm == nil {
		return nil
	}
	return &cover{h: h, vmID: vmID, state: &entry.journal, j: j, epoch: vm.Epoch(), resolved: make(chan struct{})}
}

// JournalCover is what a checkpoint of vmID that is about to be taken names
// of this host's journal (volume.Terms.Cover): nil where the mode is off, no
// journal is served, or this host does not run the VM.
func (h *Host) JournalCover(vmID string) volume.JournalCover {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	h.machines.mu.Unlock()
	return h.coverOf(vmID, entry)
}

// pauseMarker is a cover that has to know when its checkpoint's pause ends.
type pauseMarker interface{ paused(ctx context.Context) }

// paused marks the end of the checkpoint's pause: its disks are sealed and the
// guest has not run since. It takes the VM's journal lock, so a capture
// running then finishes first.
func (c *cover) paused(ctx context.Context) {
	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pauses++
	c.token = s.captures
	c.earlier = s.postCopy && !sim.Bug(ctx, "journal-drop-source-early")
	if sim.Bug(ctx, "journal-covered-after-seal") {
		// The covered position is read when the checkpoint is selected, so it
		// covers the captures made since the pause too, and their entries
		// are never replayed.
		c.late = true
		c.resolve(0)
		return
	}
	if c.token <= s.placed {
		c.resolve(s.last)
		return
	}
	s.covers = append(s.covers, c)
}

// resolve records the covered position. Caller holds the VM journal's lock.
func (c *cover) resolve(position uint64) {
	select {
	case <-c.resolved:
		return
	default:
	}
	c.covered = position
	close(c.resolved)
}

// Journals is the list the checkpoint's selection writes: this host's
// journal, at the position the pause covered, after the journals of earlier
// epochs the record names where the pause came before the post-copy ended.
func (c *cover) Journals(ctx context.Context) ([]control.Journal, error) {
	covered, err := c.position(ctx)
	if err != nil {
		return nil, err
	}
	var journals []control.Journal
	if c.earlier {
		if vm := c.h.vm(c.vmID); vm != nil {
			for _, named := range vm.Journals() {
				if named.Epoch < c.epoch {
					journals = append(journals, named)
				}
			}
		}
	}
	return append(journals, control.Journal{Disk: c.j.Identity(), Generation: c.j.Generation(), Epoch: c.epoch,
		Covered: covered}), nil
}

// position is the covered position, once every capture the pause covered is
// placed.
func (c *cover) position(ctx context.Context) (uint64, error) {
	select {
	case <-c.resolved:
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	}
	if c.late {
		c.state.mu.Lock()
		defer c.state.mu.Unlock()
		return c.state.last, nil
	}
	return c.covered, nil
}

// Selected trims the journal to what the record names now.
func (c *cover) Selected(journals []control.Journal) {
	covered := make(map[uint64]uint64)
	for _, named := range journals {
		if named.Disk == c.j.Identity() && named.Generation == c.j.Generation() {
			covered[named.Epoch] = named.Covered
		}
	}
	c.j.Trim(c.vmID, covered)
	c.state.mu.Lock()
	c.state.signalLocked()
	c.state.mu.Unlock()
}

// DefaultJournalTrimInterval is how often a host reads the control records of
// the VMs its journal holds entries of, to trim what no record names any more.
const DefaultJournalTrimInterval = 30 * time.Second

// trimming trims the journal this host writes, every interval, to what the
// control records of the VMs it holds entries of name: a VM stopped or closed
// selected a checkpoint that names no journal, a deleted one has no record,
// and a VM another host runs names that host's journal once it has selected
// a checkpoint of its own. A VM this host runs is trimmed by its own
// selections too.
func (h *Host) trimming(ctx context.Context, interval time.Duration) {
	for {
		if err := h.clock.Sleep(ctx, jittered(h.entropy, interval)); err != nil {
			return
		}
		h.trimOnce(ctx)
	}
}

// trimOnce trims every journal this host holds to the records of the VMs
// each holds entries of: its own, and those it holds for reading, whose
// entries die as the hosts that recovered their VMs select checkpoints.
func (h *Host) trimOnce(ctx context.Context) {
	for _, j := range h.heldJournals() {
		h.trimJournal(ctx, j)
	}
}

// trimJournal trims one journal to the records of the VMs it holds entries
// of.
func (h *Host) trimJournal(ctx context.Context, j *journal.Journal) {
	var vms []string
	for _, held := range j.Held() {
		if n := len(vms); n == 0 || vms[n-1] != held.VM {
			vms = append(vms, held.VM)
		}
	}
	failures := eachVM(ctx, vms, epochConcurrency, func(ctx context.Context, vmID string) error {
		record, err := h.control.Read(ctx, vmID)
		if errors.Is(err, platform.ErrNotFound) {
			j.Trim(vmID, nil)
			return nil
		}
		if err != nil {
			return err
		}
		covered := make(map[uint64]uint64)
		for _, named := range record.Journals {
			if named.Disk == j.Identity() && named.Generation == j.Generation() {
				covered[named.Epoch] = named.Covered
			}
		}
		j.Trim(vmID, covered)
		return nil
	})
	for index, err := range failures {
		if err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "host: reading a record to trim the journal failed", "vm", vms[index], "error", err)
		}
	}
}

// A full ring is back-pressure, never a failure. Past three quarters of it the
// host asks for checkpoints out of the interval's turn, of the VMs holding
// the oldest entries, since a selection trims what it covers. No VM may hold more
// than half the ring: a flush of one past that waits for the VM's own
// checkpoint. A ring with no room left holds every commit until trimming
// frees some, which the journal does itself.

// heldBy is the bytes of live entries vmID holds in j.
func heldBy(j *journal.Journal, vmID string) int64 {
	var bytes int64
	for _, held := range j.Held() {
		if held.VM == vmID {
			bytes += held.Bytes
		}
	}
	return bytes
}

// underHalfRing waits while vmID holds more than half of j's ring, having
// asked for the checkpoint that trims it.
func (h *Host) underHalfRing(ctx context.Context, j *journal.Journal, vmID string, entry *registration) error {
	ring := j.Usage().Ring
	for {
		entry.journal.mu.Lock()
		changed := entry.journal.changed
		if changed == nil {
			entry.journal.signalLocked()
			changed = entry.journal.changed
		}
		entry.journal.mu.Unlock()
		if heldBy(j, vmID)*2 <= ring || sim.Bug(ctx, "journal-ignore-half-ring") {
			return nil
		}
		sim.Probe(ctx, ProbeJournalHalfRing)
		h.askCheckpoint(entry)
		select {
		case <-changed:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// relieveRing asks for checkpoints out of turn once j is three quarters full
// with room more, until it would be under half, and once a commit of room has
// no room on it, until it would have: a commit needs room for the pad before
// its entries too, so the half the first leaves may not be enough. Only the
// tail moving frees room, so it asks every VM it runs that holds an entry
// within what is needed of the tail, the oldest first. A VM that runs
// elsewhere is trimmed once its record names this journal no more.
func (h *Host) relieveRing(j *journal.Journal, room int64) {
	usage := j.Usage()
	need := j.Shortfall(room)
	if need > 0 {
		sim.Probe(h.ctx, ProbeJournalNoRoom)
	}
	if (usage.Used+room)*4 > usage.Ring*3 {
		sim.Probe(h.ctx, ProbeJournalThreeQuarters)
		need = max(need, usage.Used-usage.Ring/2)
	}
	if need <= 0 {
		return
	}
	for _, vmID := range j.Oldest(need) {
		h.machines.mu.Lock()
		entry := h.machines.running[vmID]
		h.machines.mu.Unlock()
		if entry != nil {
			h.askCheckpoint(entry)
		}
	}
}

// askCheckpoint asks a VM's loop for a checkpoint out of the interval's turn.
func (h *Host) askCheckpoint(entry *registration) {
	if entry.now == nil {
		return
	}
	select {
	case entry.now <- struct{}{}:
	default:
		// The loop already owes this VM a checkpoint.
	}
}

// The probes durable flush marks on a host.
const (
	// ProbeJournalHalfRing is a flush that waited because its VM held more
	// than half the journal's ring.
	ProbeJournalHalfRing = "host/journal-half-ring"
	// ProbeJournalThreeQuarters is a journal past three quarters full,
	// which asked for checkpoints out of turn.
	ProbeJournalThreeQuarters = "host/journal-three-quarters"
	// ProbeJournalNoRoom is a commit the journal's ring had no room for,
	// which asked for checkpoints out of turn.
	ProbeJournalNoRoom = "host/journal-no-room"
	// ProbeJournalReadGaveUpVM is a read of this host's journal by a reader
	// at a newer epoch of a VM this host runs, which gave the VM up.
	ProbeJournalReadGaveUpVM = "host/journal-read-gave-up-vm"
	// ProbeJournalCaptureAfterPause is a flush whose capture ran after a
	// checkpoint paused the VM, though it chose its pages before.
	ProbeJournalCaptureAfterPause = "host/journal-capture-after-pause"
)

// BuggifyJournalCaptureSlow holds a flush for up to 20 ms after it chose its
// pages and before its capture, so a checkpoint's pause may come between.
const BuggifyJournalCaptureSlow = "host/journal-capture-slow"

// journalActivity is what durable flush has done on this host.
func (h *Host) journalActivity() JournalActivity {
	activity := JournalActivity{DurableFlush: h.journals.on, Flushes: h.journals.flushes.snapshot(),
		Flush: h.journals.flushTime.Snapshot(), Capture: h.journals.captureTime.Snapshot()}
	if j := h.journal(); j != nil {
		usage := j.Usage()
		activity.Served, activity.RingBytes, activity.LiveBytes, activity.Position = true, usage.Ring, usage.Used,
			usage.Next
	}
	return activity
}

// ReadJournal answers JOURNAL_READ (peer.Journals): it reads request on the
// journal disk named disk, which this host serves, and gives up the VM before
// it answers if it runs it at an older epoch than the reader's, so the reader
// knows the old instance has stopped. The journal fences the VM first, so no
// entry of an older epoch is placed from then on, and a flush of the VM here
// fails: the guest stops writing rather than going on as if its data were
// safe. A disk this host does not hold is peer.ErrNoJournal.
func (h *Host) ReadJournal(ctx context.Context, disk rank.Identity, request journal.ReadRequest,
	yield func(journal.Entry) error) error {
	j := h.journalOf(disk)
	if j == nil {
		return errNotHeld(disk)
	}
	err := j.Read(ctx, request, yield)
	if vm := h.vm(request.VM); vm != nil && vm.Epoch() < request.Reader && !sim.Bug(ctx, "journal-read-keeps-vm") {
		sim.Probe(ctx, ProbeJournalReadGaveUpVM)
		h.fence(context.WithoutCancel(ctx), request.VM, fmt.Errorf(
			"%w: a reader at epoch %d read its journal", volume.ErrNeedsRecovery, request.Reader))
	}
	return err
}

// joiningJournal is what a migration's destination names of its own journal
// in the VM's record when it opens it (vmmigrate.Options.Journal): nil where
// durable flush is off or no journal is served here, and otherwise the
// journal at no covered position, since it holds no entry of the epoch the
// open takes yet.
func (h *Host) joiningJournal() *control.Journal {
	if !h.journals.on {
		return nil
	}
	j := h.journal()
	if j == nil {
		return nil
	}
	return &control.Journal{Disk: j.Identity(), Generation: j.Generation()}
}

// postCopied tells durable flush that every page of a VM brought here has
// arrived: its flushes are journaled from now on, and a checkpoint is asked
// for out of the interval's turn, since the first selected after this one's
// pause is the first that names the source's journal no more.
func (h *Host) postCopied(vmID string) {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	h.machines.mu.Unlock()
	if entry == nil {
		return
	}
	entry.journal.opened(true, false)
	if h.journals.on {
		h.askCheckpoint(entry)
	}
}

// rootSelected tells durable flush that a fork's child has its root
// selected: a recovery can open it now, so its flushes are journaled.
func (h *Host) rootSelected(vmID string) {
	h.machines.mu.Lock()
	entry := h.machines.running[vmID]
	h.machines.mu.Unlock()
	if entry != nil {
		entry.journal.opened(false, true)
	}
}

// DrainJournal waits until the journal this host writes holds no live entry,
// trimming it every poll rather than on the trimming loop's interval: the last
// step of a drain, after its VMs have moved. Their destinations select
// checkpoints of their own soon after their post-copies end, and each
// selection leaves the record naming this journal no more. A host that writes
// no journal returns at once.
func (h *Host) DrainJournal(ctx context.Context, poll time.Duration) error {
	for {
		j := h.journal()
		if j == nil {
			return nil
		}
		h.trimJournal(ctx, j)
		if len(j.Held()) == 0 {
			return nil
		}
		if err := h.clock.Sleep(ctx, poll); err != nil {
			return err
		}
	}
}
