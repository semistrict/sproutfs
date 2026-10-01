package simtest

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
)

// ownership checks every change the store applies to a control record or a
// checkpoint's index object against the properties spec/ownership/Ownership.tla
// states, as the real code makes them. The spec is written from the code by
// hand; this is what tells a model that has drifted from the code, or code that
// has drifted from the model, by a trace the spec forbids.
//
// It checks, by the spec's names:
//
//   - SelectionMoves: a record's epoch never goes back, its pins are only
//     added, and its selection moves only forward, only within the epoch that
//     holds the record, and only to a checkpoint of that epoch;
//   - SelectedReadable: a published record's selected checkpoint has its index
//     object in the store;
//   - PinnedReadable: no index object of a checkpoint any record ever pinned
//     is deleted, its record's deletion included;
//   - KeptReadable: no index object of a checkpoint its record keeps is
//     deleted.
//
// What a checkpoint names is in its index, which this does not read: the spec
// checks names, and this checks the checkpoints themselves.
type ownership struct {
	prefix string

	mu sync.Mutex
	// records is each VM's record as the store holds it now, and pinned every
	// checkpoint any record of the VM ever pinned.
	records map[string]control.Record
	pinned  map[string]map[uint64]bool
	// indexes is every checkpoint whose index object is in the store.
	indexes map[string]map[uint64]bool
	// seen counts the record changes the check read, which is how it knows it
	// read any: a key layout it no longer recognises would check nothing.
	seen int
	errs []error
}

func newOwnership(prefix string) *ownership {
	return &ownership{prefix: prefix, records: map[string]control.Record{},
		pinned: map[string]map[uint64]bool{}, indexes: map[string]map[uint64]bool{}}
}

// observe is the store's observer. The store calls it with its lock held, in
// the order it applies changes, so the trace is the store's own.
func (o *ownership) observe(change sim.ObjectChange) {
	relative, ok := strings.CutPrefix(change.Key, o.prefix)
	if !ok {
		return
	}
	tenant, rest, ok := control.CutNamespace(relative)
	if !ok {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if name, ok := strings.CutPrefix(rest, control.RecordPrefix); ok {
		o.record(control.InTenant(tenant, name), change)
		return
	}
	if name, sequence, ok := indexOf(rest); ok {
		o.index(control.InTenant(tenant, name), sequence, change.Deleted)
	}
}

// indexOf reads vm/<name>/ckpt/<sequence>/index.
func indexOf(rest string) (name string, sequence uint64, ok bool) {
	inside, ok := strings.CutPrefix(rest, "vm/")
	if !ok {
		return "", 0, false
	}
	name, inside, ok = strings.Cut(inside, "/ckpt/")
	if !ok {
		return "", 0, false
	}
	number, ok := strings.CutSuffix(inside, "/index")
	if !ok {
		return "", 0, false
	}
	sequence, err := strconv.ParseUint(number, 10, 64)
	return name, sequence, err == nil
}

func (o *ownership) violate(format string, args ...any) {
	o.errs = append(o.errs, fmt.Errorf("ownership: "+format, args...))
}

func (o *ownership) record(vm string, change sim.ObjectChange) {
	o.seen++
	previous, existed := o.records[vm]
	if change.Deleted {
		delete(o.records, vm)
		return
	}
	next, err := control.ParseRecord(vm, change.Value)
	if err != nil {
		o.violate("%s: the store holds a record nothing can read: %v", vm, err)
		return
	}
	o.records[vm] = next
	if o.pinned[vm] == nil {
		o.pinned[vm] = map[uint64]bool{}
	}
	for _, sequence := range next.Pinned {
		o.pinned[vm][sequence] = true
	}
	if next.Created && !o.indexes[vm][next.Selected] {
		o.violate("%s: SelectedReadable: the record selects %d, whose index is not in the store", vm, next.Selected)
	}
	if !existed {
		return
	}
	switch {
	case next.Epoch < previous.Epoch:
		o.violate("%s: SelectionMoves: the epoch went back from %d to %d", vm, previous.Epoch, next.Epoch)
	case slices.ContainsFunc(previous.Pinned, func(sequence uint64) bool { return !next.IsPinned(sequence) }):
		o.violate("%s: SelectionMoves: a pin was taken away: %v became %v", vm, previous.Pinned, next.Pinned)
	case next.Selected == previous.Selected:
	case next.Selected < previous.Selected:
		o.violate("%s: SelectionMoves: the selection went back from %d to %d", vm, previous.Selected, next.Selected)
	case next.Epoch != previous.Epoch:
		o.violate("%s: SelectionMoves: one write took epoch %d and selected %d", vm, next.Epoch, next.Selected)
	case control.EpochOf(next.Selected) != next.Epoch:
		o.violate("%s: SelectionMoves: epoch %d selected %d, a checkpoint of epoch %d",
			vm, next.Epoch, next.Selected, control.EpochOf(next.Selected))
	}
}

func (o *ownership) index(vm string, sequence uint64, deleted bool) {
	if o.indexes[vm] == nil {
		o.indexes[vm] = map[uint64]bool{}
	}
	if !deleted {
		o.indexes[vm][sequence] = true
		return
	}
	delete(o.indexes[vm], sequence)
	if o.pinned[vm][sequence] {
		o.violate("%s: PinnedReadable: the index of pinned checkpoint %d was deleted", vm, sequence)
	}
	record, found := o.records[vm]
	if !found {
		return
	}
	if record.Created && record.Selected == sequence {
		o.violate("%s: SelectedReadable: the index of selected checkpoint %d was deleted", vm, sequence)
	}
	if record.IsKept(sequence) {
		o.violate("%s: KeptReadable: the index of kept checkpoint %d was deleted", vm, sequence)
	}
}

// failures reports every change the trace showed that the spec forbids, and
// a check that saw no record change in a world that created VMs.
func (o *ownership) failures(created bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if created && o.seen == 0 {
		return errors.New("ownership: the check saw no control record change, so it checked nothing: " +
			"the store's key layout is not the one it reads")
	}
	return errors.Join(o.errs...)
}
