// Package control holds the one mutable durable object a VM owns: the record
// that selects which published checkpoint the VM's state is.
//
// A VM's durable state is exactly one checkpoint — an index, the page objects
// it references and an optional VMM state object — and the control record says
// which. Nothing is durable between checkpoints. The record also carries the
// writer epoch that fences a superseded host, and the sequences a fork
// inherited, which reclamation must not delete.
//
// Every change is a conditional write against object storage. Creating a record
// uses a create-if-absent condition; replacing one reads its validator and
// writes under a compare-and-set. A reply that is lost rather than refused is
// reconciled by reading the record back: a writer recognises its own work by
// the random nonce it chose when it claimed its epoch.
//
// Because the record is what allocates and selects checkpoints, the names of a
// checkpoint and of the page a range reads live here too: a [Ref] is the (VM,
// sequence) pair, an [Identity] is a page's identity and an [Extent] is
// a run of volume bytes that reads from one page. A pager and a migration name
// pages by those without depending on the store that holds them.
package control

import (
	"errors"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/semistrict/sproutfs/internal/platform"
)

var (
	// ErrInvalidConfig reports a configuration or argument that cannot name a
	// VM or a checkpoint sequence.
	ErrInvalidConfig = errors.New("control: invalid configuration")
	// ErrExists reports a VM whose control record is already there.
	ErrExists = errors.New("control: VM already exists")
	// ErrCorrupt reports a control record that disagrees with itself or with
	// the identity it was read under.
	ErrCorrupt = errors.New("control: corrupt control record")
	// ErrFenced reports a handle whose epoch is no longer the record's. A later
	// open took the VM over; this handle must be discarded and the VM reopened.
	ErrFenced = errors.New("control: fenced by a later writer")
	// ErrClosed reports a handle its owner has released.
	ErrClosed = errors.New("control: closed")
	// ErrEpochExhausted reports a VM that has been opened as many times as an
	// epoch can count.
	ErrEpochExhausted = errors.New("control: writer epochs exhausted")
	// ErrSequence reports a checkpoint selection that does not advance, or a
	// sequence outside the opening handle's epoch.
	ErrSequence = errors.New("control: checkpoint sequence does not advance")
	// ErrTooManyPins reports a VM that has been forked more times than one
	// record can carry.
	ErrTooManyPins = errors.New("control: too many pinned checkpoints")
)

const (
	// formatVersion is the control record's wire format. Version 4's pins are
	// the checkpoints of this VM that have been forked, and nothing releases
	// one. Version 3 marked a tombstone, version 2 named each pin's holders and
	// the parent checkpoint a record held a pin on, and version 1's pins were
	// bare sequences; none of them parses.
	formatVersion = uint32(4)
	// nonceSize is the writer nonce, large enough that two writers never
	// choose the same one.
	nonceSize = 16
	// maximumRecordSize bounds one control record, which is a few fixed fields
	// and the pins.
	maximumRecordSize = int64(1 << 20)
	// MaximumPins bounds the sequences one record may pin. Because a pin is
	// permanent, that is how many distinct checkpoints of one VM may be forked
	// before a collector releases some — not how many children one checkpoint
	// may have, which is unbounded: a fan-out of any size from one fork point is
	// one pin.
	MaximumPins = 4096
	// MinimumEpoch is the lowest epoch a record may carry. Epoch zero is not
	// one, so a sequence is never zero.
	MinimumEpoch = uint64(1)
	// MaximumCreateEpoch is the highest epoch a creation may draw. A creating
	// handle takes a random epoch in [MinimumEpoch, MaximumCreateEpoch] rather
	// than a fixed first one, so that two VMs created under one identity — a
	// name handed out again after a delete — never allocate the same checkpoint
	// sequences, and therefore never the same page identities or object
	// keys. Half the epoch space is left above it, which is how many times such
	// a VM may still be taken over.
	MaximumCreateEpoch = uint64(1)<<31 - 1
	// MaximumEpoch and MaximumCounter bound the two halves of a sequence.
	MaximumEpoch   = uint64(math.MaxUint32)
	MaximumCounter = uint64(math.MaxUint32)
)

// Sequence composes a checkpoint sequence from the epoch that allocated it and
// a counter within that epoch. Sequences are epoch-major so a fenced writer and
// its successor can never allocate the same one: the successor's epoch is
// higher, so every sequence it allocates is higher than any the fenced writer
// could still be using. Counters start at one, so a sequence is never zero.
func Sequence(epoch, counter uint64) uint64 { return epoch<<32 | counter }

// EpochOf reports the epoch that allocated a sequence.
func EpochOf(sequence uint64) uint64 { return sequence >> 32 }

// CounterOf reports a sequence's counter within its epoch.
func CounterOf(sequence uint64) uint64 { return sequence & math.MaxUint32 }

// ValidSequence reports whether a sequence was allocated by epoch, which is
// what every checkpoint this handle publishes must satisfy.
func ValidSequence(epoch, sequence uint64) bool {
	return epoch >= MinimumEpoch && epoch <= MaximumEpoch &&
		EpochOf(sequence) == epoch && CounterOf(sequence) >= 1
}

// ValidCreateSequence reports whether a sequence is one a creation may select:
// its epoch is one NewEpoch could have drawn, so every later open of that VM
// has epochs left to count up through.
func ValidCreateSequence(sequence uint64) bool {
	return EpochOf(sequence) <= MaximumCreateEpoch && ValidSequence(EpochOf(sequence), sequence)
}

// ValidID reports whether an identity can be part of an object key. It is the
// same rule the checkpoint package applies to a checkpoint reference.
func ValidID(id string) bool {
	if id == "" || len(id) > 256 || id == "." || id == ".." || !utf8.ValidString(id) {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool {
		return r == '/' || r < 0x20 || r == 0x7f
	})
}

// Config supplies the object store the records live in and the deployment's
// namespace within it.
type Config struct {
	ObjectStore  platform.ObjectStore
	ObjectPrefix platform.ObjectPrefix
	// Entropy is where a writer nonce comes from. Nil is the operating
	// system's, through crypto/rand, which is what a deployment wants: the
	// nonce is the only thing that tells this writer's lost conditional write
	// from a competing writer's, so a nonce anyone could predict would let a
	// second writer read a record back as its own. A simulation passes a source
	// keyed to its seed instead, so a run that reconciles a lost reply
	// reconciles the same nonce on every replay.
	Entropy platform.Entropy
}

// The names of the simulation probes this package marks. A campaign that never
// reaches one of these reached no writer contention at all: they are the places
// a control record's ambiguity is resolved, and a run that resolved none of
// them tested the ordinary path and called it coverage.
const (
	// ProbePublicationFenced marks a control-record write refused because a
	// later open owns the VM, which is what must keep a fenced writer's
	// checkpoint from ever being the selected one.
	ProbePublicationFenced = "control/publication-fenced"
	// ProbeReplyReconciled marks a write whose reply was lost being settled
	// against the record read back rather than guessed at.
	ProbeReplyReconciled = "control/reply-reconciled"
)
