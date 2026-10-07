package control

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"time"

	controlv1 "github.com/semistrict/sproutfs/control/internal/gen/sproutfs/control/v1"
	"google.golang.org/protobuf/proto"
)

// Record is a VM's durable control state. It is a value: reading one never
// aliases another reader's copy, and writing one replaces it whole.
type Record struct {
	// VM is the identity the record belongs to and Epoch the writer token. A
	// handle whose epoch is no longer the record's is fenced.
	VM    string
	Epoch uint64
	// Nonce is the random value the writer of this epoch chose. It is how a
	// writer whose conditional write lost its reply recognises its own work.
	Nonce []byte
	// Selected is the sequence of the checkpoint this VM's durable state is,
	// and Created reports that that checkpoint has been published, which is to
	// say that its index object is there. A fork's record selects its first
	// checkpoint before that checkpoint exists.
	Selected uint64
	Created  bool
	// Pinned lists the checkpoints of this VM that have been forked, in
	// ascending order of sequence. A pinned checkpoint's objects are never
	// reclaimed.
	//
	// A pin is permanent: nothing in this deployment ever gives one back. It
	// records that a fork was taken at that checkpoint, and only a
	// collector — which can survey every record in the deployment and so can
	// establish that no index anywhere still reads through it — may release
	// one. Every unpin that ran from a single descendant's point of view was a
	// guess about forks it could not see: a grandchild's index names its
	// grandparent's checkpoints directly, and the child releasing what it had itself
	// stopped reading took them out from under it.
	Pinned []uint64
	// Kept lists the checkpoints of this VM a checkpoint request kept, in
	// ascending order of sequence. A kept checkpoint's objects are never
	// reclaimed, so it can be forked long after the VM has moved on.
	//
	// Keeping is not forking. A kept checkpoint no fork was taken from holds
	// nothing anyone reads, and it may be released (Client.Release). One that
	// was forked is pinned too, and the pin is what nothing releases.
	Kept []Kept
	// Journals lists the journals that may hold this VM's flushed writes newer
	// than the selected checkpoint, in ascending order of epoch. A recovery
	// replays them in that order before the VM runs. A selection writes the
	// list, an open keeps it, and a migration's open adds the destination's
	// journal after the source's. A selection that holds every store, as a
	// stop's and a close's do, writes an empty list.
	Journals []Journal
}

// Journal names one journal that may hold a VM's flushed writes newer than its
// selected checkpoint.
type Journal struct {
	// Disk is the identity of the journal's disk, as its header and the
	// membership name it, and Generation the one drawn when the disk was
	// formatted. A disk formatted again holds none of the entries the record
	// names.
	Disk       [16]byte
	Generation uint64
	// Epoch is the VM's writer epoch whose entries the journal holds.
	Epoch uint64
	// Covered is the last position whose entries the selected checkpoint
	// holds. A replay applies only the entries after it.
	Covered uint64
}

// Kept is one checkpoint a checkpoint request kept.
type Kept struct {
	// Sequence names the checkpoint, and Time is when it was selected.
	Sequence uint64
	Time     time.Time
	// State reports that the checkpoint holds VMM state, so a fork of it
	// resumes the guest where it was rather than booting it.
	State bool
}

// clone returns an independent copy, so a caller cannot reach into the state a
// handle tracks.
func (r Record) clone() Record {
	r.Nonce = bytes.Clone(r.Nonce)
	r.Pinned = slices.Clone(r.Pinned)
	r.Kept = slices.Clone(r.Kept)
	r.Journals = slices.Clone(r.Journals)
	return r
}

// IsPinned reports whether a checkpoint of this VM has been forked, which is
// what reclamation must spare.
func (r Record) IsPinned(sequence uint64) bool { return slices.Contains(r.Pinned, sequence) }

// IsKept reports whether a checkpoint request kept a checkpoint of this VM,
// which reclamation must spare too.
func (r Record) IsKept(sequence uint64) bool {
	return slices.ContainsFunc(r.Kept, func(kept Kept) bool { return kept.Sequence == sequence })
}

// Protected reports every checkpoint of this VM that reclamation must spare, in
// ascending order: the forked and the kept.
func (r Record) Protected() []uint64 {
	protected := slices.Clone(r.Pinned)
	for _, kept := range r.Kept {
		protected = append(protected, kept.Sequence)
	}
	slices.Sort(protected)
	return slices.Compact(protected)
}

// admits reports whether one more pin fits: a record pins so many distinct
// checkpoints, and because pins are permanent that is how many checkpoints of
// one VM may ever be forked before a collector has released some.
func (r Record) admits(sequence uint64) error {
	if !r.IsPinned(sequence) && len(r.Pinned) >= MaximumPins {
		return ErrTooManyPins
	}
	return nil
}

// withPin returns this record's pins with one sequence added. Pinning twice is
// one pin.
func (r Record) withPin(sequence uint64) []uint64 {
	if r.IsPinned(sequence) {
		return slices.Clone(r.Pinned)
	}
	at := slices.IndexFunc(r.Pinned, func(existing uint64) bool { return existing > sequence })
	if at < 0 {
		at = len(r.Pinned)
	}
	return slices.Insert(slices.Clone(r.Pinned), at, sequence)
}

// withKept returns this record's kept checkpoints with one added. Keeping a
// checkpoint twice keeps the first entry, so a selection repeated after a lost
// reply writes nothing.
func (r Record) withKept(kept Kept) ([]Kept, error) {
	if r.IsKept(kept.Sequence) {
		return slices.Clone(r.Kept), nil
	}
	if len(r.Kept) >= MaximumKept {
		return nil, ErrTooManyKept
	}
	at := slices.IndexFunc(r.Kept, func(existing Kept) bool { return existing.Sequence > kept.Sequence })
	if at < 0 {
		at = len(r.Kept)
	}
	return slices.Insert(slices.Clone(r.Kept), at, kept), nil
}

// withoutKept returns this record's kept checkpoints with one taken out.
func (r Record) withoutKept(sequence uint64) []Kept {
	return slices.DeleteFunc(slices.Clone(r.Kept), func(kept Kept) bool { return kept.Sequence == sequence })
}

// equal compares the whole durable content of two records, which is what a
// writer reconciling a lost reply compares the record it read against the one
// it meant to write.
func (r Record) equal(other Record) bool {
	return r.VM == other.VM && r.Epoch == other.Epoch && bytes.Equal(r.Nonce, other.Nonce) &&
		r.Selected == other.Selected && r.Created == other.Created &&
		slices.Equal(r.Pinned, other.Pinned) &&
		slices.EqualFunc(r.Kept, other.Kept, func(a, b Kept) bool {
			return a.Sequence == b.Sequence && a.Time.Equal(b.Time) && a.State == b.State
		}) &&
		slices.Equal(r.Journals, other.Journals)
}

// mine reports whether a record read back is the one this epoch and nonce own.
func (r Record) mine(epoch uint64, nonce []byte) bool {
	return r.Epoch == epoch && bytes.Equal(r.Nonce, nonce)
}

func (r Record) marshal() ([]byte, error) {
	if !r.valid() {
		return nil, ErrInvalidConfig
	}
	kept := make([]*controlv1.Kept, 0, len(r.Kept))
	for _, entry := range r.Kept {
		kept = append(kept, controlv1.Kept_builder{
			Sequence: proto.Uint64(entry.Sequence),
			Time:     proto.Int64(entry.Time.UnixNano()),
			State:    proto.Bool(entry.State),
		}.Build())
	}
	journals := make([]*controlv1.Journal, 0, len(r.Journals))
	for _, journal := range r.Journals {
		journals = append(journals, controlv1.Journal_builder{
			Disk:       bytes.Clone(journal.Disk[:]),
			Generation: proto.Uint64(journal.Generation),
			Epoch:      proto.Uint64(journal.Epoch),
			Covered:    proto.Uint64(journal.Covered),
		}.Build())
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(controlv1.Record_builder{
		FormatVersion: proto.Uint32(formatVersion),
		VmId:          proto.String(r.VM),
		Epoch:         proto.Uint64(r.Epoch),
		WriterNonce:   bytes.Clone(r.Nonce),
		Selected:      proto.Uint64(r.Selected),
		Pinned:        slices.Clone(r.Pinned),
		Created:       proto.Bool(r.Created),
		Kept:          kept,
		Journals:      journals,
	}.Build())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximumRecordSize {
		return nil, ErrInvalidConfig
	}
	return data, nil
}

// valid reports whether a record is one this deployment could have written: an
// identity, a nonce of the writer's size, an epoch in range, a selection of
// something, pins and kept checkpoints that are each a bounded, sorted set of
// sequences, and journals the writers of this record could have named.
func (r Record) valid() bool {
	return ValidID(r.VM) && len(r.Nonce) == nonceSize && r.Epoch >= MinimumEpoch && r.Epoch <= MaximumEpoch &&
		r.Selected != 0 && len(r.Pinned) <= MaximumPins && len(r.Kept) <= MaximumKept &&
		ascending(r.Pinned) && ascending(r.keptSequences()) && r.validJournals()
}

// validJournals reports whether the record names at most MaximumJournals
// journals, each on a disk with an identity, in strictly ascending order of
// epoch, and none of an epoch past the record's. One epoch is held by one host,
// which writes into its one journal, so no two journals share an epoch.
func (r Record) validJournals() bool {
	if len(r.Journals) > MaximumJournals {
		return false
	}
	for index, journal := range r.Journals {
		if journal.Disk == [16]byte{} || journal.Epoch < MinimumEpoch || journal.Epoch > r.Epoch ||
			(index > 0 && r.Journals[index-1].Epoch >= journal.Epoch) {
			return false
		}
	}
	return true
}

// keptSequences reports the sequences of the kept checkpoints, in the order the
// record holds them.
func (r Record) keptSequences() []uint64 {
	sequences := make([]uint64, 0, len(r.Kept))
	for _, kept := range r.Kept {
		sequences = append(sequences, kept.Sequence)
	}
	return sequences
}

// ascending reports whether sequences are a sorted set of checkpoint
// sequences.
func ascending(sequences []uint64) bool {
	for index, sequence := range sequences {
		if sequence == 0 || (index > 0 && sequences[index-1] >= sequence) {
			return false
		}
	}
	return true
}

// unmarshalRecord parses a control record and rejects anything this deployment
// cannot have written: another format, an identity that is not the one it was
// read under, an epoch below the first, a selection of nothing, pins or kept
// checkpoints that are not a sorted set of sequences, or journals no writer of
// the record could have named.
// ParseRecord reads one VM's control record from the bytes of its object, as
// Client.Read does. It is for a reader that sees the object itself, such as a
// simulation that watches every change its store applies.
func ParseRecord(vm string, data []byte) (Record, error) { return unmarshalRecord(vm, data) }

func unmarshalRecord(vm string, data []byte) (Record, error) {
	message := new(controlv1.Record)
	if err := proto.Unmarshal(data, message); err != nil {
		return Record{}, errors.Join(ErrCorrupt, err)
	}
	// The version is settled before anything else is read, and the refusal
	// names it: a store written by a build this one does not read must say so
	// rather than report a record that disagrees with itself.
	if !message.HasFormatVersion() || message.GetFormatVersion() != formatVersion {
		return Record{}, fmt.Errorf("%w: record format version %d, want %d",
			ErrCorrupt, message.GetFormatVersion(), formatVersion)
	}
	if len(message.ProtoReflect().GetUnknown()) != 0 || !message.HasVmId() || !message.HasEpoch() ||
		!message.HasWriterNonce() || !message.HasSelected() || !message.HasCreated() {
		return Record{}, ErrCorrupt
	}
	record := Record{
		VM:       message.GetVmId(),
		Epoch:    message.GetEpoch(),
		Nonce:    bytes.Clone(message.GetWriterNonce()),
		Selected: message.GetSelected(),
		Created:  message.GetCreated(),
		Pinned:   slices.Clone(message.GetPinned()),
	}
	for _, kept := range message.GetKept() {
		if len(kept.ProtoReflect().GetUnknown()) != 0 || !kept.HasSequence() || !kept.HasTime() || !kept.HasState() {
			return Record{}, ErrCorrupt
		}
		record.Kept = append(record.Kept, Kept{Sequence: kept.GetSequence(),
			Time: time.Unix(0, kept.GetTime()).UTC(), State: kept.GetState()})
	}
	for _, journal := range message.GetJournals() {
		if len(journal.ProtoReflect().GetUnknown()) != 0 || len(journal.GetDisk()) != len(Journal{}.Disk) ||
			!journal.HasGeneration() || !journal.HasEpoch() || !journal.HasCovered() {
			return Record{}, ErrCorrupt
		}
		record.Journals = append(record.Journals, Journal{Disk: [16]byte(journal.GetDisk()),
			Generation: journal.GetGeneration(), Epoch: journal.GetEpoch(), Covered: journal.GetCovered()})
	}
	if record.VM != vm || !record.valid() {
		return Record{}, ErrCorrupt
	}
	return record, nil
}
