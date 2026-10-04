package membership

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// ObjectName is where the membership lives within a deployment's prefix. It
// is one object outside every VM's namespace.
const ObjectName = "membership"

// The probes a store's writes mark.
const (
	// ProbeReplyReconciled is a write whose reply was lost, found landed by
	// its nonce when the object was read back.
	ProbeReplyReconciled = "membership/reply-reconciled"
	// ProbeWriteRaced is a write another writer's change beat, tried again
	// from a fresh read.
	ProbeWriteRaced = "membership/write-raced"
	// ProbeWriteUnsettled is a write whose reply was lost while other
	// changes landed, tried again from a fresh read, which the change finds
	// done or does again.
	ProbeWriteUnsettled = "membership/write-unsettled"
)

// The fault-injection sites of a store.
const (
	// buggifyReadFails fails a read of the object, as a store that is down
	// does.
	buggifyReadFails = "membership/read-fails"
	// buggifyWriteFails fails a write before the store applies it.
	buggifyWriteFails = "membership/write-fails"
	// buggifyReplyLost loses the reply to a write the store applied.
	buggifyReplyLost = "membership/reply-lost"
)

var errReplyLost = fmt.Errorf("%w: the reply to a write of the membership was lost", platform.ErrUnavailable)

// Config is where a deployment's membership lives.
type Config struct {
	ObjectStore  platform.ObjectStore
	ObjectPrefix platform.ObjectPrefix
	// Entropy draws each write's nonce. Nil is the operating system's.
	Entropy platform.Entropy
}

// Store reads and changes a deployment's membership. It holds nothing of its
// own: every answer comes from the object store.
type Store struct {
	objects platform.ObjectStore
	key     platform.ObjectKey
	entropy platform.Entropy
}

// NewStore is a store over the deployment's object store.
func NewStore(config Config) (*Store, error) {
	if config.ObjectStore == nil {
		return nil, fmt.Errorf("%w: a membership needs an object store", ErrInvalid)
	}
	prefix := config.ObjectPrefix.String()
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	key, err := platform.NewObjectKey(prefix + ObjectName)
	if err != nil {
		return nil, err
	}
	return &Store{objects: config.ObjectStore, key: key, entropy: platform.EntropyOr(config.Entropy)}, nil
}

// Empty is the membership of a deployment that has written none: generation
// zero, nobody in it, under the code of one host.
func Empty() Membership {
	m, _ := New(0, rank.CodeFor(1), nil, nil)
	return m
}

// Read reads the membership: Empty for a deployment that has none yet.
func (s *Store) Read(ctx context.Context) (Membership, error) {
	m, _, err := s.read(ctx)
	return m, err
}

// read reads the membership and the validator a conditional write names, nil
// where there is no object.
func (s *Store) read(ctx context.Context) (Membership, *platform.ETag, error) {
	if sim.Buggify(ctx, buggifyReadFails, 0.2) {
		return Membership{}, nil, fmt.Errorf("%w: a read of the membership failed", platform.ErrUnavailable)
	}
	data, metadata, err := platform.ReadObject(ctx, s.objects, s.key, 1, maximumSize, ErrCorrupt)
	if errors.Is(err, platform.ErrNotFound) {
		return Empty(), nil, nil
	}
	if err != nil {
		return Membership{}, nil, err
	}
	m, err := Unmarshal(data)
	if err != nil {
		return Membership{}, nil, err
	}
	etag := metadata.ETag
	return m, &etag, nil
}

// Update changes the membership by compare-and-set: it reads the object,
// builds the next generation with change, and writes it conditional on the
// object it read. A write another writer beat is tried again from a fresh
// read, so no change is lost and none is written over an older generation.
// It returns the membership it wrote.
//
// change must say ErrUnchanged when the membership already is what it asks
// for, as every change of this package does. A write whose reply was lost is
// read back: one found by its nonce is done, and one that cannot be told
// from a later writer's change is made again from a fresh read, which change
// then finds done. Update returns ErrUnchanged, with the membership it read,
// when there is nothing to do, and Step's refusal for a next generation that
// may not follow. Any other error leaves the change not made, or, when the
// object could not be read back either, not known to be made.
func (s *Store) Update(ctx context.Context, change func(Membership) (Membership, error)) (Membership, error) {
	for {
		current, etag, err := s.read(ctx)
		if err != nil {
			return Membership{}, err
		}
		next, err := change(current)
		if errors.Is(err, ErrUnchanged) {
			return current, ErrUnchanged
		}
		if err != nil {
			return Membership{}, err
		}
		if err := Step(ctx, current, next); err != nil {
			return Membership{}, err
		}
		next.nonce = make([]byte, nonceSize)
		s.entropy.Fill(next.nonce)
		err = s.write(ctx, next, etag)
		if err == nil {
			return next, nil
		}
		if errors.Is(err, platform.ErrPrecondition) {
			// Another writer's change landed between the read and the write.
			sim.Probe(ctx, ProbeWriteRaced)
			continue
		}
		observed, _, readErr := s.read(ctx)
		switch {
		case readErr != nil:
			return Membership{}, errors.Join(err, readErr)
		case bytes.Equal(observed.nonce, next.nonce):
			sim.Probe(ctx, ProbeReplyReconciled)
			return observed, nil
		case observed.generation == current.generation:
			// Nothing was written. The caller may try again.
			return Membership{}, err
		}
		// Other writers' changes landed, and whether this one did before
		// them cannot be told. The change, made again over what is there now,
		// finds it done or does it.
		sim.Probe(ctx, ProbeWriteUnsettled)
	}
}

// Reconcile makes the one change Next takes towards want from the membership
// as it is read, and reports the membership it leaves and whether it changed
// it. A controller calls it once a pass, so it moves the membership one step
// at a time, and two controllers at once each take a step from what the other
// left.
func (s *Store) Reconcile(ctx context.Context, want Want) (Membership, bool, error) {
	m, err := s.Update(ctx, func(m Membership) (Membership, error) {
		change, ok := Next(m, want)
		if !ok {
			return Membership{}, ErrUnchanged
		}
		return change(m)
	})
	if errors.Is(err, ErrUnchanged) {
		return m, false, nil
	}
	return m, err == nil, err
}

// write puts next conditional on the object read: the validator it had, or
// its absence.
func (s *Store) write(ctx context.Context, next Membership, etag *platform.ETag) error {
	data, err := next.Marshal()
	if err != nil {
		return err
	}
	conditions := platform.PutConditions{IfNoneMatch: true}
	if etag != nil {
		conditions = platform.PutConditions{IfMatch: etag}
	}
	if sim.Bug(ctx, "membership-write-unconditional") {
		conditions = platform.PutConditions{}
	}
	if sim.Buggify(ctx, buggifyWriteFails, 0.2) {
		return fmt.Errorf("%w: a write of the membership failed", platform.ErrUnavailable)
	}
	_, err = s.objects.Put(ctx, platform.PutRequest{Key: s.key, Body: bytes.NewReader(data), Size: int64(len(data)),
		ContentType: "application/x-protobuf", Conditions: conditions})
	if err == nil && sim.Buggify(ctx, buggifyReplyLost, 0.2) {
		return errReplyLost
	}
	return err
}
