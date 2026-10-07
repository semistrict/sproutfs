package simtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/journal"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/volume"
)

// A world with durable flush on (plans/fsync-journal-2026-10-06.md) answers a
// guest's flush of a disk from its host's journal, a network disk of the
// world's cloud that the controller keeps for each host's machine. The
// promise is about flushes: when a flush returns success, every store the
// guest made to that disk before it sent the flush survives the loss of its
// host. So a recovery here is not checked against one whole checkpoint, as
// in a world without it: a replay writes the blocks of the entries after the
// checkpoint, and which entries a lost host had placed before it died is the
// crash's to decide. Each block must hold the value the last answered flush,
// or the last checkpoint that landed, saw there, or a value the guest stored
// later.

// defaultJournalBytes is a journal disk's size where the world's
// configuration names none: room for a few dozen whole 2 MiB pages, so a VM
// may hold a dozen of them before it waits for its own checkpoint.
const defaultJournalBytes = 64 << 20

// journalControl is the controller's dealings with the cloud over the
// world's journal disks.
func (w *World) journalControl() *membership.JournalControl {
	bytes := w.config.JournalBytes
	if bytes == 0 {
		bytes = defaultJournalBytes
	}
	w.controlClock = w.runtime.NewClock(w.config.Namespace + "controller")
	return &membership.JournalControl{Disks: w.cloud, Deployment: w.config.Namespace + "journals", Bytes: bytes,
		Expiry: w.config.JournalExpiry, Clock: w.controlClock,
		Entropy: w.runtime.NewEntropy(w.config.Namespace + "journals")}
}

// JournalServed reports whether the host at index serves durable flush: it
// holds the journal disk reserved for its machine open. A host that is not up
// serves nothing.
func (w *World) JournalServed(index int) bool {
	running := w.up(index)
	return running != nil && running.Activity().Journal.Served
}

// journalTerms is terms with the cover of the host running in, where durable
// flush is on: the checkpoint's selection names that host's journal at the
// position its pause covers, as the host's own loop does.
func (w *World) journalTerms(in *instance, terms volume.Terms) volume.Terms {
	if !w.config.Journals || terms.Cover != nil {
		return terms
	}
	if running := w.up(in.host); running != nil {
		terms.Cover = running.JournalCover(in.spec.ID)
	}
	return terms
}

// flushes is what durable flush promises one VM's guest about its durable
// disks. The guest stores whole pages, and a replay writes whole blocks, so it
// keeps, for every block, every value the guest stored into it, oldest first,
// and the floor: the oldest of them a recovery may still find there. An
// answered flush raises the floors of its disk to the values stored before it
// was sent, and a checkpoint that landed raises those of every disk to the
// values at its pause.
//
// A recovery checks every block against its floor and starts the values over
// at what it read. A flush answered after a recovery took the VM, by a host
// that went on running it, is checked against what that recovery read: a
// holder fences the VM before it answers a read, and waits for every batch
// placed before the fence, so a flush answered at all is in what the reader
// was given.
type flushes struct {
	mu    sync.Mutex
	disks map[string]*diskFlushes
	// generation counts the recoveries of the VM, and came is, by the
	// generation each ended, the index into that generation's values every
	// block came back at.
	generation int
	came       map[int]map[string][]int
	// answered and failed count the flushes answered with success and with
	// an error; broken is every promise found broken.
	answered, failed int
	broken           []error
}

// diskFlushes is one disk's values and floors, by block.
type diskFlushes struct {
	values [][]byte
	floor  []int
}

// flushPoint is where one flush, or one checkpoint's pause, found the values
// of the disks it covers: by disk and block, the index of the last value
// stored.
type flushPoint struct {
	generation int
	at         map[string][]int
}

// newFlushes starts the promises of a VM whose guest holds model, over the
// disks durable names: each block's only value is the one it holds now.
func newFlushes(model map[string][]byte, durable []string) *flushes {
	f := &flushes{disks: map[string]*diskFlushes{}, came: map[int]map[string][]int{}}
	for _, name := range durable {
		data := model[name]
		disk := &diskFlushes{values: make([][]byte, len(data)/journal.BlockBytes),
			floor: make([]int, len(data)/journal.BlockBytes)}
		for block := range disk.values {
			disk.values[block] = distinct(data[block*journal.BlockBytes : (block+1)*journal.BlockBytes])
		}
		f.disks[name] = disk
	}
	return f
}

// distinct is the values of a block, each once, in the order they first
// appear: one for a block the guest stored whole.
func distinct(block []byte) []byte {
	var seen [256]bool
	var values []byte
	for _, value := range block {
		if !seen[value] {
			seen[value] = true
			values = append(values, value)
		}
	}
	return values
}

// stored records a store of value into every block of one page of a disk.
// A volume it does not keep is not durable, and nothing promises it.
func (f *flushes) stored(name string, page uint64, pageBytes int, value byte) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	disk := f.disks[name]
	if disk == nil {
		return
	}
	per := pageBytes / journal.BlockBytes
	for block := int(page) * per; block < (int(page)+1)*per; block++ {
		disk.values[block] = append(disk.values[block], value)
	}
}

// point is where the disks named, or every durable disk for none, are now.
func (f *flushes) point(names ...string) flushPoint {
	if f == nil {
		return flushPoint{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(names) == 0 {
		names = slices.Sorted(maps.Keys(f.disks))
	}
	p := flushPoint{generation: f.generation, at: map[string][]int{}}
	for _, name := range names {
		disk := f.disks[name]
		if disk == nil {
			continue
		}
		at := make([]int, len(disk.values))
		for block, values := range disk.values {
			at[block] = len(values) - 1
		}
		p.at[name] = at
	}
	return p
}

// answer records a host's answer to a flush sent at p. One answered with
// success raises the floors to p. One answered after a recovery has taken
// the VM is checked against what that recovery read.
func (f *flushes) answer(p flushPoint, err error) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		f.failed++
		return
	}
	f.answered++
	if p.generation == f.generation {
		f.raiseLocked(p)
		return
	}
	came := f.came[p.generation]
	for _, name := range slices.Sorted(maps.Keys(p.at)) {
		for block, at := range p.at[name] {
			if came[name][block] < at {
				f.broken = append(f.broken, fmt.Errorf(
					"a flush of %s answered with success after a recovery took the VM is not in what it read: "+
						"block %d came back at the value stored %d before the one the flush held",
					name, block, at-came[name][block]))
				return
			}
		}
	}
}

// landed records a checkpoint whose pause was at p as landed: its floors rise
// to p. One that landed after a recovery took the VM raises nothing, since
// that recovery has been checked already.
func (f *flushes) landed(p flushPoint) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.generation == f.generation {
		f.raiseLocked(p)
	}
}

func (f *flushes) raiseLocked(p flushPoint) {
	for name, at := range p.at {
		disk := f.disks[name]
		for block := range at {
			disk.floor[block] = max(disk.floor[block], at[block])
		}
	}
}

// recovered checks what a recovery read of the VM's durable disks: every
// byte of every block holds a value stored into it at or after its floor.
// Then the values start over at what it read.
func (f *flushes) recovered(read map[string][]byte) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var broken []error
	came := map[string][]int{}
	for _, name := range slices.Sorted(maps.Keys(f.disks)) {
		disk := f.disks[name]
		data := read[name]
		came[name] = make([]int, len(disk.values))
		for block, values := range disk.values {
			bytesOf := data[block*journal.BlockBytes : (block+1)*journal.BlockBytes]
			at := len(values)
			for _, value := range distinct(bytesOf) {
				index := bytes.LastIndexByte(values, value)
				if index < 0 {
					broken = append(broken, fmt.Errorf("%s block %d reads %d, which the guest never stored there",
						name, block, value))
					break
				}
				if index < disk.floor[block] {
					broken = append(broken, fmt.Errorf(
						"%s block %d reads %d, older than the %d an answered flush or a landed checkpoint held "+
							"there; it may read only %v", name, block, value, values[disk.floor[block]],
						values[disk.floor[block]:]))
					break
				}
				at = min(at, index)
			}
			came[name][block] = at
			disk.values[block] = distinct(bytesOf)
			disk.floor[block] = 0
		}
	}
	f.came[f.generation] = came
	f.generation++
	return errors.Join(broken...)
}

// verify reports every promise found broken.
func (f *flushes) verify() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return errors.Join(f.broken...)
}

// newFlushesFor starts the promises of a VM whose guest is g, at what it
// holds now: a VM just created, a fork's child, or one made from a kept
// checkpoint. A world without durable flush keeps none.
func (w *World) newFlushesFor(g *guest) {
	if !w.config.Journals {
		return
	}
	var durable []string
	for _, name := range g.names {
		if name != MemoryVolume && !g.ephemeral[name] {
			durable = append(durable, name)
		}
	}
	f := newFlushes(g.snapshot(), durable)
	w.mu.Lock()
	w.flushes[g.instance] = f
	w.mu.Unlock()
	g.setFlushes(f)
}

// flushesFor gives g the promises its VM already has, which is what a
// migration's destination does: its guest is the one that stopped on the
// source.
func (w *World) flushesFor(g *guest) {
	w.mu.Lock()
	f := w.flushes[g.instance]
	w.mu.Unlock()
	g.setFlushes(f)
}

// JournalPending is how many opens of a VM found a journal its record names
// served by no member it could read: the disk was still moving to a survivor.
func (w *World) JournalPending() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.journalPending
}

// Flushes is how many flushes of one VM its hosts answered with success and
// with an error.
func (w *World) Flushes(id string) (answered, failed int) {
	w.mu.Lock()
	f := w.flushes[id]
	w.mu.Unlock()
	if f == nil {
		return 0, 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.answered, f.failed
}

// VerifyFlushes reports every flush of every VM that was answered with
// success and then found lost.
func (w *World) VerifyFlushes() error {
	w.mu.Lock()
	all := maps.Clone(w.flushes)
	w.mu.Unlock()
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(all)) {
		if err := all[id].verify(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// errRunningNowhere is a flush of a VM no host runs: there is no guest to
// send it.
var errRunningNowhere = errors.New("simtest: the VM is running nowhere")

// Flush has the named VM's guest flush one of its disks, and reports the
// host's answer once it has reached the guest. A host that loses the VM
// before it answers never answers, and neither does one that is killed: a
// dead VMM takes no answer. The world records every answer the guest took.
func (w *World) Flush(id, name string) <-chan error {
	answered := make(chan error, 1)
	_, g := w.runningVM(id)
	if g == nil {
		answered <- fmt.Errorf("flushing %s of %s: %w", name, id, errRunningNowhere)
		return answered
	}
	g.flush(name, func(error) {}, func(took bool, err error) {
		if took {
			answered <- err
		}
	})
	return answered
}

// FlushOn is Flush by the guest the host at index runs for a VM, whether or
// not the world runs the VM there: a migration's destination during its
// post-copy, or a host cut off from the deployment after the VM was taken
// over elsewhere.
func (w *World) FlushOn(index int, id, name string) <-chan error {
	answered := make(chan error, 1)
	g := w.hosts[index].lastGuest(id)
	if g == nil {
		answered <- fmt.Errorf("flushing %s of %s on %s: %w", name, id, w.hosts[index].name, errRunningNowhere)
		return answered
	}
	g.flush(name, func(error) {}, func(took bool, err error) {
		if took {
			answered <- err
		}
	})
	return answered
}

// FlushAtStart has the next guest the host at index starts for a VM it takes
// in flush one of its disks the moment it exists, before the host has
// registered it: a VMM sends the flushes it held when it stopped again as
// soon as it runs on its next host.
func (w *World) FlushAtStart(index int, id, name string) <-chan error {
	answered := make(chan error, 1)
	h := w.hosts[index]
	h.mu.Lock()
	defer h.mu.Unlock()
	h.flushAtStart[id] = func(g *guest) {
		g.flush(name, func(error) {}, func(took bool, err error) {
			if took {
				answered <- err
			}
		})
	}
	return answered
}

// lastGuest is the last guest of a VM this incarnation of the host started
// and has not ended, nil for none.
func (h *hostState) lastGuest(id string) *guest {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, g := range slices.Backward(h.guests) {
		if g.instance == id && !g.isClosed() {
			return g
		}
	}
	return nil
}

// GiveUpOn has the controller take the host at index for gone while its
// process still runs, as an operator's force does: its member is drained,
// and the disks attached to its machine are detached, which ends every handle
// it holds of them. A restart of the host ends it.
func (w *World) GiveUpOn(index int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.givenUpHosts[index] = true
}

// HeldFlush is a flush whose answer the host has given and the guest has not
// taken yet: it waits at the VMM's door. Between the two is the moment after
// the journal's sync and before the guest knows its flush is durable.
type HeldFlush struct {
	answer  chan error
	deliver chan struct{}
	taken   chan bool
}

// HoldFlush is Flush with the answer held at the guest's door until Deliver.
func (w *World) HoldFlush(id, name string) *HeldFlush {
	held := &HeldFlush{answer: make(chan error, 1), deliver: make(chan struct{}), taken: make(chan bool, 1)}
	_, g := w.runningVM(id)
	if g == nil {
		held.answer <- fmt.Errorf("flushing %s of %s: %w", name, id, errRunningNowhere)
		held.taken <- false
		return held
	}
	g.flush(name, func(err error) {
		held.answer <- err
		<-held.deliver
	}, func(took bool, _ error) { held.taken <- took })
	return held
}

// Answered is the host's answer, before the guest has it.
func (h *HeldFlush) Answered() <-chan error { return h.answer }

// Deliver lets the answer in, and reports whether the guest took it: a guest
// whose host was killed meanwhile is gone, and takes nothing.
func (h *HeldFlush) Deliver() bool {
	close(h.deliver)
	return <-h.taken
}

// flush sends one flush of a disk. The host's answer passes door, which may
// hold it, and then reaches the guest unless its VMM is gone by then. The
// answer the guest took is recorded before took hears of it, so a caller that
// acts on it acts on a world that knows it.
func (g *guest) flush(name string, door func(error), took func(bool, error)) {
	g.mu.Lock()
	f := g.flushes
	g.mu.Unlock()
	p := f.point(name)
	g.memoryRegions[name].Flush(func(err error) {
		door(err)
		if g.isClosed() {
			took(false, err)
			return
		}
		f.answer(p, err)
		took(true, err)
	})
}

// flushPoint is where every durable disk of g is now: a checkpoint's pause
// taken now holds these values.
func (g *guest) flushPoint() flushPoint {
	g.mu.Lock()
	f := g.flushes
	g.mu.Unlock()
	return f.point()
}

// landed records a checkpoint of g paused at p as landed.
func (g *guest) landed(p flushPoint) {
	g.mu.Lock()
	f := g.flushes
	g.mu.Unlock()
	f.landed(p)
}

// setFlushes gives g the promises it records its stores into.
func (g *guest) setFlushes(f *flushes) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.flushes = f
}

// reopenJournaled is reopenWith where durable flush is on. The open replays
// the journals the record names, and a VM something was replayed into, or
// whose checkpoint holds no VMM state, is cold booted: its memory reads
// zeroes and its disks are checked against the flushes' floors. One resumed
// from a checkpoint's VMM state had nothing replayed, and reads as that
// checkpoint's pause.
func (w *World) reopenJournaled(ctx context.Context, in *instance, index int, why string, cold bool) (bool, error) {
	h := w.hosts[index]
	running := w.reach(index)
	if running == nil {
		return false, nil
	}
	id := in.spec.ID
	vm, err := w.openFor(ctx, running, id, cold)
	if err != nil {
		// A VM whose journal no member serves yet is not lost: the disk is
		// still moving to a survivor, and a later step opens it.
		w.logf("%s: %s could not take it over after %s: %v", id, h.name, why, err)
		w.mu.Lock()
		if errors.Is(err, volume.ErrJournalPending) {
			w.journalPending++
		}
		in.host, in.present = index, false
		w.mu.Unlock()
		return false, nil
	}
	selected := vm.Status().Checkpoint
	var state []byte
	if cold {
		state, err = host.State(ctx, running.Checkpoints(), selected)
		if errors.Is(err, checkpoint.ErrNoState) {
			state, err = nil, nil
		}
	} else {
		state, err = running.Starting(ctx, vm, MemoryVolume)
	}
	if err != nil {
		w.logf("%s: %s could not read the VMM state of %s: %v", id, h.name, selected, err)
		if err := vm.Close(ctx); err != nil {
			w.logf("%s: releasing a takeover that could not restore its guest: %v", id, err)
		}
		in.host, in.present = index, false
		return false, nil
	}
	if vm.Replayed() && state != nil {
		return false, fmt.Errorf("%s came back with %d bytes of VMM state over the flushes it replayed", id, len(state))
	}
	started := vm.Status().Checkpoint
	w.notePublished(id, started.Sequence)
	g, err := w.newGuest(h, h.pager, vm, nil, state)
	if err != nil {
		return false, err
	}
	h.running(g)
	read, missing, unreadable := g.readAll(ctx)
	if unreadable != nil {
		// Nothing here keeps a page from a VM that was just opened: its pages
		// are its checkpoint's and the blocks it replayed. One that cannot be
		// read cannot be checked either.
		return false, fmt.Errorf("%s came back at %s and could not read itself: %w", id, started, unreadable)
	}
	w.mu.Lock()
	f := w.flushes[id]
	w.mu.Unlock()
	if err := f.recovered(read); err != nil {
		return false, fmt.Errorf("%s came back at %s, replayed %t, and lost what was flushed: %w", id, started,
			vm.Replayed(), err)
	}
	g.setFlushes(f)
	if state != nil {
		// Resumed: nothing was replayed, so it reads as the pause the
		// checkpoint it opened at took, and continues at its store counter.
		came, ok := w.at(in, selected.Sequence)
		if !ok {
			return false, fmt.Errorf("%s resumed at %s, which is not one of the checkpoints it may have come back at",
				id, selected)
		}
		if !agrees(read, missing, came.model) {
			return false, fmt.Errorf("%s resumed at %s reading state that checkpoint did not publish: %s",
				id, selected, describeRead(read, []durableState{came}))
		}
		if got := g.stored(); got != came.writes {
			return false, fmt.Errorf("%s resumed having made %d stores, and the checkpoint it came back at held %d",
				id, got, came.writes)
		}
	} else if memory, zero := read[MemoryVolume], make([]byte, len(read[MemoryVolume])); !bytes.Equal(memory, zero) {
		return false, fmt.Errorf("%s was cold booted at %s, and its memory is not zeroes", id, started)
	}
	g.adopt(read)
	w.mu.Lock()
	in.durables = []durableState{{sequence: started.Sequence, model: g.snapshot(), writes: g.stored(),
		stateless: state == nil}}
	in.rewound = nil
	if state == nil {
		in.writes = nil
	}
	w.mu.Unlock()
	w.place(in, index, g)
	if err := running.AddMachine(id, g); err != nil {
		return true, err
	}
	w.logf("%s: %s opened it at %s after %s, replayed %t", id, h.name, started, why, vm.Replayed())
	return true, nil
}
