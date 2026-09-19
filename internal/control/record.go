package control

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	controlv1 "github.com/semistrict/sproutfs/internal/control/internal/gen/sproutfs/control/v1"
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
}

// clone returns an independent copy, so a caller cannot reach into the state a
// handle tracks.
func (r Record) clone() Record {
	r.Nonce = bytes.Clone(r.Nonce)
	r.Pinned = slices.Clone(r.Pinned)
	return r
}

// IsPinned reports whether a checkpoint of this VM has been forked, which is
// what reclamation must spare.
func (r Record) IsPinned(sequence uint64) bool { return slices.Contains(r.Pinned, sequence) }

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

// equal compares the whole durable content of two records, which is what a
// writer reconciling a lost reply compares the record it read against the one
// it meant to write.
func (r Record) equal(other Record) bool {
	return r.VM == other.VM && r.Epoch == other.Epoch && bytes.Equal(r.Nonce, other.Nonce) &&
		r.Selected == other.Selected && r.Created == other.Created &&
		slices.Equal(r.Pinned, other.Pinned)
}

// mine reports whether a record read back is the one this epoch and nonce own.
func (r Record) mine(epoch uint64, nonce []byte) bool {
	return r.Epoch == epoch && bytes.Equal(r.Nonce, nonce)
}

func (r Record) marshal() ([]byte, error) {
	if !ValidID(r.VM) || len(r.Nonce) != nonceSize || r.Epoch < MinimumEpoch || r.Epoch > MaximumEpoch ||
		r.Selected == 0 || len(r.Pinned) > MaximumPins || !r.validPins() {
		return nil, ErrInvalidConfig
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(controlv1.Record_builder{
		FormatVersion: proto.Uint32(formatVersion),
		VmId:          proto.String(r.VM),
		Epoch:         proto.Uint64(r.Epoch),
		WriterNonce:   bytes.Clone(r.Nonce),
		Selected:      proto.Uint64(r.Selected),
		Pinned:        slices.Clone(r.Pinned),
		Created:       proto.Bool(r.Created),
	}.Build())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximumRecordSize {
		return nil, ErrInvalidConfig
	}
	return data, nil
}

// validPins reports whether the pins are a sorted set of checkpoint sequences.
func (r Record) validPins() bool {
	for index, sequence := range r.Pinned {
		if sequence == 0 || (index > 0 && r.Pinned[index-1] >= sequence) {
			return false
		}
	}
	return true
}

// unmarshalRecord parses a control record and rejects anything this deployment
// cannot have written: another format, an identity that is not the one it was
// read under, an epoch below the first, a selection of nothing, or pins that
// are not a sorted set of sequences.
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
	if record.VM != vm || len(record.Nonce) != nonceSize || record.Epoch < MinimumEpoch ||
		record.Epoch > MaximumEpoch || record.Selected == 0 || len(record.Pinned) > MaximumPins ||
		!record.validPins() {
		return Record{}, ErrCorrupt
	}
	return record, nil
}
