package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Client reads and writes the control records of one deployment's VMs. Its
// methods are safe for concurrent use. It holds no state of its own: every
// answer comes from the object store.
type Client struct {
	objects platform.ObjectStore
	prefix  string
	entropy platform.Entropy
	clock   platform.Clock
}

// NewClient validates the configuration and returns a client over it.
func NewClient(config Config) (*Client, error) {
	if config.ObjectStore == nil {
		return nil, ErrInvalidConfig
	}
	prefix := config.ObjectPrefix.String()
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	client := &Client{objects: config.ObjectStore, prefix: prefix,
		entropy: platform.EntropyOr(config.Entropy), clock: platform.ClockOr(config.Clock)}
	if _, err := client.key("vm"); err != nil {
		return nil, ErrInvalidConfig
	}
	return client, nil
}

// RecordPrefix is where the control records of one deployment live, within
// whatever prefix the deployment itself has. It is a namespace of its own, a
// key per VM and nothing else, so listing it is how the deployment's VMs are
// found: a VM's checkpoint objects live under vm/<id>/ and a listing that had
// to walk those would cost a request for every object anyone ever wrote.
const RecordPrefix = "control/"

// key is the object one VM's control record lives at.
func (c *Client) key(vm string) (platform.ObjectKey, error) {
	if !ValidID(vm) {
		return platform.ObjectKey{}, ErrInvalidConfig
	}
	return platform.NewObjectKey(c.prefix + RecordName(vm))
}

// Read returns a VM's control record. A VM with no record reports
// platform.ErrNotFound, which is what an open of a deleted or never-created VM
// sees.
func (c *Client) Read(ctx context.Context, vm string) (Record, error) {
	record, _, err := c.read(ctx, vm)
	return record, err
}

func (c *Client) read(ctx context.Context, vm string) (Record, platform.ETag, error) {
	key, err := c.key(vm)
	if err != nil {
		return Record{}, "", err
	}
	data, metadata, err := platform.ReadObject(ctx, c.objects, key, 1, maximumRecordSize, ErrCorrupt)
	if err != nil {
		return Record{}, "", err
	}
	record, err := unmarshalRecord(vm, data)
	if err != nil {
		return Record{}, "", err
	}
	return record, metadata.ETag, nil
}

// put writes a record under the supplied conditions and reports the validator
// the store gave it. Store errors pass through unchanged so callers can keep
// reconciling a lost reply against the record they read back.
func (c *Client) put(ctx context.Context, record Record, conditions platform.PutConditions) (platform.ETag, error) {
	key, err := c.key(record.VM)
	if err != nil {
		return "", err
	}
	data, err := record.marshal()
	if err != nil {
		return "", err
	}
	// A control-record write is one object-store round trip, and everything a
	// writer does next assumes it costs about what the last one did. A write
	// that takes much longer is what puts a fork's pin, a takeover's epoch and
	// a publication's selection in an order nobody arranged.
	if err := sim.BuggifyDelay(ctx, "control/slow-write", 0.25, 5*time.Second); err != nil {
		return "", err
	}
	result, err := c.objects.Put(ctx, platform.PutRequest{
		Key: key, Body: bytes.NewReader(data), Size: int64(len(data)), Conditions: conditions,
	})
	if err != nil {
		return "", err
	}
	return result.Metadata.ETag, nil
}

// NewEpoch draws the writer epoch a creation takes, from this client's entropy
// and in [MinimumEpoch, MaximumCreateEpoch]. A creating handle draws its epoch
// rather than starting at a fixed one because a VM identity can be handed out
// again — the orchestrator allocates them and a deleted VM's name is free — and
// two VMs that started at the same epoch would allocate the same checkpoint
// sequences: the same page identities, which a page cache keys resident
// pages by, and the same object keys, which are written create-if-absent.
//
// The caller draws it before it publishes anything, because the first
// checkpoint is published under this epoch and before the record that selects
// it exists.
func (c *Client) NewEpoch() uint64 {
	return MinimumEpoch + c.entropy.Uint64()%(MaximumCreateEpoch-MinimumEpoch+1)
}

// Create writes the control record of a new VM, selecting the first checkpoint
// the caller allocated, and returns the handle that owns it. published says
// whether that checkpoint is already in the store: a VM created from nothing
// publishes it first and passes true, while a fork's record selects one its own
// first publication has not written yet.
//
// The sequence must be one a creation may select — an epoch NewEpoch could have
// drawn, and a counter within it — which is what makes it knowable before the
// record exists. A VM whose record is already there reports ErrExists; the
// caller opens it instead.
func (c *Client) Create(ctx context.Context, vm string, selected uint64, published bool) (*Handle, error) {
	if !ValidID(vm) || !ValidCreateSequence(selected) {
		return nil, ErrInvalidConfig
	}
	epoch := EpochOf(selected)
	nonce := make([]byte, nonceSize)
	c.entropy.Fill(nonce)
	record := Record{VM: vm, Epoch: epoch, Nonce: nonce, Selected: selected, Created: published}
	etag, err := c.put(ctx, record, platform.PutConditions{IfNoneMatch: true})
	if err == nil {
		return newHandle(c, record, etag), nil
	}
	// Either the write landed, and the record carries this attempt's nonce, or it
	// did not and the VM is still uncreated or somebody else's. That holds for a
	// refusal as much as for a lost reply: the create-if-absent a retry inside
	// the store's own client repeats is refused by the object its first attempt
	// wrote, and a caller told its VM exists would lose the record it owns —
	// nothing can create that identity again, and a fork's child, whose root is
	// published by its own first checkpoint, could then never be opened either.
	observed, observedETag, readErr := c.read(ctx, vm)
	if readErr != nil {
		if errors.Is(err, platform.ErrPrecondition) {
			// Refused, and the record cannot be read to say by whom. The VM is
			// somebody's; this caller cannot take it.
			return nil, errors.Join(ErrExists, err, readErr)
		}
		return nil, errors.Join(err, readErr)
	}
	if observed.mine(epoch, nonce) {
		sim.Probe(ctx, ProbeReplyReconciled)
	} else {
		return nil, ErrExists
	}
	return newHandle(c, observed, observedETag), nil
}

// Open claims the next writer epoch of an existing VM and returns a handle at
// it. The claim is a conditional write, so exactly one of two concurrent opens
// gets each epoch, and the handle the previous epoch belonged to is fenced: its
// next control-record write fails.
//
// Open reads and writes the record and nothing else. It does not check that the
// selected checkpoint's index exists — Record reports whether it was ever
// published, and reading the index is the caller's.
func (c *Client) Open(ctx context.Context, vm string) (*Handle, error) {
	if !ValidID(vm) {
		return nil, ErrInvalidConfig
	}
	nonce := make([]byte, nonceSize)
	c.entropy.Fill(nonce)
	for {
		current, etag, err := c.read(ctx, vm)
		if err != nil {
			return nil, err
		}
		if current.Epoch >= MaximumEpoch {
			return nil, ErrEpochExhausted
		}
		next := current.clone()
		next.Epoch, next.Nonce = current.Epoch+1, nonce
		nextETag, err := c.put(ctx, next, platform.PutConditions{IfMatch: &etag})
		if err == nil {
			return newHandle(c, next, nextETag), nil
		}
		if errors.Is(err, platform.ErrPrecondition) {
			// Another open claimed this epoch first; claim the one after it.
			continue
		}
		observed, observedETag, readErr := c.read(ctx, vm)
		if readErr != nil {
			return nil, errors.Join(err, readErr)
		}
		if observed.mine(next.Epoch, nonce) {
			// The reply was lost; the claim landed.
			sim.Probe(ctx, ProbeReplyReconciled)
			return newHandle(c, observed, observedETag), nil
		}
		if observed.Epoch >= next.Epoch {
			continue
		}
		return nil, err
	}
}

// Delete removes a VM's control record, after which nothing can open it. It is
// unconditional: a caller that has not stopped the VM's writer first races it,
// and so does a pin added without the writer. Remove is the conditional one.
// The checkpoint objects are not touched, and neither is any record this VM
// pins a checkpoint of: a pin outlives the record that took it, and what a
// deleted VM leaves pinned is a collector's to release.
func (c *Client) Delete(ctx context.Context, vm string) error {
	key, err := c.key(vm)
	if err != nil {
		return err
	}
	return c.objects.Delete(ctx, platform.DeleteRequest{Key: key})
}

// Remove removes a VM's control record and reports the record it removed,
// whose pins are what a sweep of the VM's objects must spare. The removal is
// conditional on the record as it was read, and a record that moved is read
// again, so a pin added without the writer between the read and the removal
// is in the pins it reports. A VM with no record reports platform.ErrNotFound.
//
// Like Delete, it does not stop the VM's writer: close that first.
func (c *Client) Remove(ctx context.Context, vm string) (Record, error) {
	key, err := c.key(vm)
	if err != nil {
		return Record{}, err
	}
	for {
		current, etag, err := c.read(ctx, vm)
		if err != nil {
			return Record{}, err
		}
		err = c.objects.Delete(ctx, platform.DeleteRequest{Key: key, IfMatch: &etag})
		if err == nil {
			return current, nil
		}
		if errors.Is(err, platform.ErrPrecondition) {
			continue
		}
		// A reply may be lost: a record that is gone was removed, by this call
		// or a retry of it inside the store's client.
		_, observedETag, readErr := c.read(ctx, vm)
		switch {
		case errors.Is(readErr, platform.ErrNotFound):
			return current, nil
		case readErr != nil:
			return Record{}, errors.Join(err, readErr)
		case observedETag == etag:
			return Record{}, err
		}
	}
}

// Pin records a fork of a published checkpoint of a VM this client holds no
// handle on, and reports the checkpoint it pinned. It is how a VM nobody runs
// is forked: a stopped VM has no writer to pin with, and taking its epoch to
// pin would fence a host that turns out to run it after all.
//
// Sequence zero names the checkpoint the record selects. Only three kinds of
// checkpoint may be named. One is the published checkpoint the record selects:
// a writer's sweep never deletes the checkpoint its own selection selected,
// nor anything that checkpoint's index names. Another is a kept one: every
// sweep that could reach it read a record that keeps it, because the keep was
// written with the checkpoint's own selection, and a release is conditional on
// the record just as this pin is. The last is one a pin already keeps, which
// costs no write. Any other checkpoint may be in a sweep that read the record
// before this pin landed, so it is refused with ErrNotPublished.
//
// The write is conditional on the record as it was read, with the pin added
// and the epoch and nonce kept. A record that moved in between is read again.
// A writer that holds the epoch has its next write refused, adopts the record
// with the pin, and makes its change again over it, so its next selection
// reports the pin to its reclamation. Pins are only ever added, so a record
// read back that pins the checkpoint settles a reply that was lost.
func (c *Client) Pin(ctx context.Context, vm string, sequence uint64) (Ref, error) {
	if !ValidID(vm) {
		return Ref{}, ErrInvalidConfig
	}
	for {
		current, etag, err := c.read(ctx, vm)
		if err != nil {
			return Ref{}, err
		}
		wanted := sequence
		if wanted == 0 {
			wanted = current.Selected
		}
		pinned := Ref{VM: vm, Sequence: wanted}
		if current.IsPinned(wanted) {
			return pinned, nil
		}
		if (wanted != current.Selected || !current.Created) && !current.IsKept(wanted) {
			return Ref{}, fmt.Errorf("%w: checkpoint %d of %s is not the published checkpoint its record selects, "+
				"and neither a keep nor a pin keeps it", ErrNotPublished, wanted, vm)
		}
		if err := current.admits(wanted); err != nil {
			return Ref{}, err
		}
		next := current.clone()
		next.Pinned = current.withPin(wanted)
		_, err = c.put(ctx, next, platform.PutConditions{IfMatch: &etag})
		if err == nil {
			return pinned, nil
		}
		if errors.Is(err, platform.ErrPrecondition) {
			// The record moved between the read and the write.
			continue
		}
		observed, _, readErr := c.read(ctx, vm)
		if readErr != nil {
			return Ref{}, errors.Join(err, readErr)
		}
		if observed.IsPinned(wanted) {
			sim.Probe(ctx, ProbeReplyReconciled)
			return pinned, nil
		}
		if observed.equal(current) {
			// Nothing was written. The caller may repeat the call.
			return Ref{}, err
		}
	}
}

// Release gives up a kept checkpoint of a VM, so that reclamation may delete
// what only it held, and reports the record it left. It is refused with
// ErrNotKept for a checkpoint the record does not keep, and with ErrForked for
// one a fork was taken from: the fork's pin is permanent, because a descendant
// may read through it.
//
// Like Pin, it needs no epoch: a stopped VM has no writer, and taking the
// epoch would fence a host that runs it after all. The write is conditional on
// the record as it was read, with the epoch and nonce kept, so a writer adopts
// it as it adopts a pin. A pin and a release of one checkpoint are therefore
// ordered: a pin that lands first makes the release refused, and a release
// that lands first makes the pin refused.
func (c *Client) Release(ctx context.Context, vm string, sequence uint64) (Record, error) {
	if !ValidID(vm) || sequence == 0 {
		return Record{}, ErrInvalidConfig
	}
	for {
		current, etag, err := c.read(ctx, vm)
		if err != nil {
			return Record{}, err
		}
		if !current.IsKept(sequence) {
			return Record{}, fmt.Errorf("%w: checkpoint %d of %s", ErrNotKept, sequence, vm)
		}
		if current.IsPinned(sequence) {
			return Record{}, fmt.Errorf("%w: checkpoint %d of %s", ErrForked, sequence, vm)
		}
		next := current.clone()
		next.Kept = current.withoutKept(sequence)
		_, err = c.put(ctx, next, platform.PutConditions{IfMatch: &etag})
		if err == nil {
			return next, nil
		}
		if errors.Is(err, platform.ErrPrecondition) {
			// The record moved between the read and the write.
			continue
		}
		observed, _, readErr := c.read(ctx, vm)
		if readErr != nil {
			return Record{}, errors.Join(err, readErr)
		}
		if !observed.IsKept(sequence) {
			// A record read back without it settles a reply that was lost.
			sim.Probe(ctx, ProbeReplyReconciled)
			return observed, nil
		}
		if observed.equal(current) {
			// Nothing was written. The caller may repeat the call.
			return Record{}, err
		}
	}
}

// Handle is one writer's hold on a VM's control record at one epoch. Its
// methods are safe for concurrent use, and they serialise: one control-record
// write is in flight at a time, so the tracked state is never behind the store.
// A caller waiting its turn waits under its own context, because the write in
// front of it is object-store latency.
//
// A handle that is fenced stays fenced. The VM must be reopened to write
// anything again.
type Handle struct {
	client *Client
	// vm and epoch are this handle's identity and never change.
	vm    string
	epoch uint64

	// write admits one control-record write at a time, across the object-store
	// round trip it costs.
	write *ctxsync.Mutex

	mu     sync.Mutex
	record Record
	etag   platform.ETag
	fenced bool
	closed bool
}

func newHandle(client *Client, record Record, etag platform.ETag) *Handle {
	return &Handle{client: client, vm: record.VM, epoch: record.Epoch,
		write: ctxsync.NewMutex(), record: record, etag: etag}
}

// VM reports the identity this handle writes.
func (h *Handle) VM() string { return h.vm }

// Epoch reports the writer token this handle holds. It is the high half of
// every checkpoint sequence the handle may publish.
func (h *Handle) Epoch() uint64 { return h.epoch }

// Record reports the control state this handle last wrote, without contacting
// the store.
func (h *Handle) Record() Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.record.clone()
}

// Close releases the handle locally. It writes nothing: an epoch is given up by
// the next open advancing it, not by its holder.
func (h *Handle) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
}

// Select makes a published checkpoint this VM's durable state. The sequence
// must have been allocated by this handle's epoch and must not go backwards;
// selecting the one already selected is idempotent, which is what lets a
// publication whose reply was lost be repeated. It reports the record the
// selection produced, whose pins are what reclamation must spare.
func (h *Handle) Select(ctx context.Context, sequence uint64) (Record, error) {
	return h.selecting(ctx, sequence, nil)
}

// SelectKept is Select that also keeps the checkpoint, in the same write:
// reclamation spares it from then on, and it can be forked after the VM has
// moved past it. state says whether it holds VMM state. A checkpoint that is
// selected before it is kept could be reclaimed by the next selection's sweep
// in between, which is why the two are one write.
func (h *Handle) SelectKept(ctx context.Context, sequence uint64, state bool) (Record, error) {
	return h.selecting(ctx, sequence, &Kept{Sequence: sequence, Time: h.client.clock.Now().UTC(), State: state})
}

// selecting selects a checkpoint, and keeps it when kept is not nil.
func (h *Handle) selecting(ctx context.Context, sequence uint64, kept *Kept) (Record, error) {
	return h.update(ctx, func(current Record) (Record, error) {
		if !ValidSequence(h.epoch, sequence) && sequence != current.Selected {
			return Record{}, ErrSequence
		}
		if sequence < current.Selected {
			return Record{}, ErrSequence
		}
		next := current
		next.Selected, next.Created = sequence, true
		if kept != nil {
			var err error
			if next.Kept, err = current.withKept(*kept); err != nil {
				return Record{}, err
			}
		}
		return next, nil
	})
}

// Pin records that a fork has been taken at a checkpoint of this VM, which
// keeps that checkpoint's objects from being reclaimed. It is written before
// the child that reads it exists, because a child that existed while the
// checkpoints it inherits were unpinned could have them reclaimed under it.
// Pinning twice is one pin, so a fan-out of any size from one fork point, and a
// fork repeated after a failure, cost one.
//
// Nothing here gives a pin back. A pin is what a collector releases, once it
// has surveyed the deployment and established that no index anywhere reads
// through that checkpoint. A descendant cannot establish that: the forks
// below it are ones it cannot see.
func (h *Handle) Pin(ctx context.Context, sequence uint64) (Record, error) {
	return h.update(ctx, func(current Record) (Record, error) {
		if sequence == 0 {
			return Record{}, ErrSequence
		}
		if err := current.admits(sequence); err != nil {
			return Record{}, err
		}
		next := current
		next.Pinned = current.withPin(sequence)
		return next, nil
	})
}

// update applies one change to the record this handle owns and writes it.
// mutate reports the record to write; writing the record that is already there
// costs nothing and writes nothing.
//
// The handle at the current epoch is the record's only writer of everything
// but a pin and a release. A pin may be added without the epoch (Client.Pin),
// which is how a VM nobody runs is forked, and a kept checkpoint may be
// released without it (Client.Release). So the record can move underneath
// this write in two ways: a later open taking the VM over, which fences this
// handle, or a pin or a release, which this handle adopts. After adopting, the
// change is made again over the adopted record, and mutate settles a change
// that had already landed by writing nothing.
//
// One control-record write is in flight at a time, so the tracked state is
// never behind the store; a caller waiting its turn waits under its own
// context, because the write in front of it is object-store latency.
func (h *Handle) update(ctx context.Context, mutate func(Record) (Record, error)) (Record, error) {
	if err := h.write.Lock(ctx); err != nil {
		return Record{}, err
	}
	defer h.write.Unlock()
	for {
		current, err := h.begin()
		if err != nil {
			return Record{}, err
		}
		next, err := mutate(current)
		if err != nil {
			return Record{}, err
		}
		if next.equal(current) {
			return current, nil
		}
		record, err := h.replace(ctx, next)
		if !errors.Is(err, errAdopted) {
			return record, err
		}
	}
}

// errAdopted reports a write refused because the record had moved to another
// record of this handle's own epoch and nonce, which the handle has adopted.
// update makes its change again over it.
var errAdopted = errors.New("control: the record moved to one this handle adopted")

// begin reports the record this handle currently owns, or why it owns none.
// The caller holds the write lock.
func (h *Handle) begin() (Record, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fenced {
		return Record{}, ErrFenced
	}
	if h.closed {
		return Record{}, ErrClosed
	}
	return h.record.clone(), nil
}

// settle installs the outcome of one write: the record now durable and its
// validator, or the fence that ended this handle.
func (h *Handle) settle(record Record, etag platform.ETag, fenced bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fenced = h.fenced || fenced
	h.etag = etag
	if !fenced {
		h.record = record
	}
}

// replace writes the record under a compare-and-set against the validator this
// handle tracks, and reconciles a reply that was lost rather than refused. The
// caller holds the write lock.
//
// Only this handle, and a pin or a release made without the epoch, can
// produce a record carrying its epoch and nonce, so the record read back
// settles what happened: the one it meant to write means the write landed; the
// one it already had means it did not; any other record of this epoch and
// nonce is a write of its own it never saw land, a pin or a release, which it
// adopts so that update makes its change again; and anything else means a
// later open has taken the VM over.
func (h *Handle) replace(ctx context.Context, next Record) (Record, error) {
	h.mu.Lock()
	expected, current := h.etag, h.record.clone()
	h.mu.Unlock()
	etag, err := h.client.put(ctx, next, platform.PutConditions{IfMatch: &expected})
	if err == nil {
		h.settle(next, etag, false)
		return next.clone(), nil
	}
	observed, observedETag, readErr := h.client.read(ctx, next.VM)
	if readErr != nil {
		// Refused or lost, and the record cannot be read to say why. A refusal
		// is no evidence of a takeover: a pin or a release made without the
		// epoch refuses this write too, and fencing on it would give up a
		// running guest for one. The handle keeps its epoch. Its next write reads what
		// happened, and a takeover fences it then.
		return Record{}, errors.Join(err, readErr)
	}
	switch {
	case observed.equal(next):
		// The write landed and its reply was lost. Reconciling it against the
		// record read back is what makes an ambiguous outcome an answer.
		sim.Probe(ctx, ProbeReplyReconciled)
		h.settle(observed, observedETag, false)
		return observed.clone(), nil
	case observed.equal(current):
		// Nothing was written. The handle keeps its epoch and the caller may
		// repeat the call.
		h.settle(current, observedETag, false)
		return Record{}, err
	case observed.mine(h.epoch, next.Nonce):
		sim.Probe(ctx, ProbeReplyReconciled)
		// Either a write of this handle's own that it never saw land — its
		// reply was lost and the read that would have reconciled it failed
		// too — or a pin or a release made without the epoch. Either way the
		// handle has been tracking a record the store moved past, and this
		// write was refused against its stale validator. Nothing was taken
		// over: only this handle, a pin and a release keep this epoch and
		// nonce. So the handle
		// catches up on the record it finds and makes its change again.
		h.settle(observed, observedETag, false)
		return Record{}, errors.Join(errAdopted, err)
	default:
		// A later open owns the VM. This writer's publication is fenced: the
		// checkpoint it was selecting must never become the selected one.
		sim.Probe(ctx, ProbePublicationFenced)
		h.settle(Record{}, observedETag, true)
		return Record{}, errors.Join(ErrFenced, err)
	}
}
