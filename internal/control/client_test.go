package control_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
)

func newClient(t *testing.T) (*control.Client, *sim.ObjectStore) {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 1})
	prefix, err := platform.NewObjectPrefix("deployment/")
	if err != nil {
		t.Fatal(err)
	}
	client, err := control.NewClient(control.Config{ObjectStore: runtime.ObjectStore(), ObjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return client, runtime.ObjectStore()
}

// root is the sequence every VM's first checkpoint is published under.
func root() uint64 { return control.Sequence(control.MinimumEpoch, 1) }

// retryingStore is what an SDK that retries a request of its own looks like
// from here: the first create-if-absent lands, and the answer that comes back
// is the refusal the retry of that same request earned against the object the
// first attempt had just written.
type retryingStore struct {
	platform.ObjectStore
	retried bool
}

func (s *retryingStore) Put(ctx context.Context, request platform.PutRequest) (platform.PutResult, error) {
	result, err := s.ObjectStore.Put(ctx, request)
	if err != nil || s.retried || !request.Conditions.IfNoneMatch {
		return result, err
	}
	s.retried = true
	return platform.PutResult{}, fmt.Errorf("the retry found the object: %w", platform.ErrPrecondition)
}

// A sequence is its epoch in the high half and a counter in the low half, so
// every sequence a later epoch allocates is above every sequence an earlier one
// could allocate.
func TestSequencesAreEpochMajor(t *testing.T) {
	if got := control.Sequence(1, 1); got != 1<<32|1 {
		t.Fatalf("Sequence(1, 1) = %d, want %d", got, uint64(1<<32|1))
	}
	if got := control.EpochOf(control.Sequence(7, 3)); got != 7 {
		t.Fatalf("EpochOf = %d, want 7", got)
	}
	if got := control.CounterOf(control.Sequence(7, 3)); got != 3 {
		t.Fatalf("CounterOf = %d, want 3", got)
	}
	if control.Sequence(1, control.MaximumCounter) >= control.Sequence(2, 1) {
		t.Fatal("an epoch's last sequence is not below the next epoch's first")
	}
	if control.ValidSequence(2, control.Sequence(1, 5)) {
		t.Fatal("a sequence from an earlier epoch was accepted as this one's")
	}
	if !control.ValidSequence(2, control.Sequence(2, 1)) {
		t.Fatal("an epoch's own first sequence was refused")
	}
	if control.ValidSequence(2, control.Sequence(2, 0)) {
		t.Fatal("counter zero was accepted; sequences start at one")
	}
}

func TestCreateOpenAndDelete(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	if _, err := client.Read(ctx, "vm"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("reading an uncreated VM = %v, want ErrNotFound", err)
	}
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	if handle.Epoch() != control.MinimumEpoch || handle.VM() != "vm" {
		t.Fatalf("the creating handle is %s at epoch %d", handle.VM(), handle.Epoch())
	}
	record := handle.Record()
	if record.Selected != root() || !record.Created || len(record.Pinned) != 0 {
		t.Fatalf("the created record = %+v", record)
	}
	if _, err := client.Create(ctx, "vm", root(), true); !errors.Is(err, control.ErrExists) {
		t.Fatalf("creating a VM twice = %v, want ErrExists", err)
	}
	// A creation may select any epoch it could have drawn, and nothing above
	// them: the epochs above are what later opens of that VM count up through.
	if _, err := client.Create(ctx, "other", control.Sequence(control.MaximumCreateEpoch+1, 1), true); !errors.Is(err, control.ErrInvalidConfig) {
		t.Fatalf("creating at an epoch no creation may draw = %v", err)
	}
	if _, err := client.Create(ctx, "other", control.Sequence(control.MinimumEpoch, 0), true); !errors.Is(err, control.ErrInvalidConfig) {
		t.Fatalf("creating with a zero counter = %v", err)
	}
	if err := client.Delete(ctx, "vm"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Open(ctx, "vm"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("opening a deleted VM = %v, want ErrNotFound", err)
	}
}

// A creating handle draws its epoch, so two VMs created under one identity — a
// name a deleted VM held before — allocate different sequences and therefore
// different lineage identities and different object keys. Every draw leaves
// the upper half of the epoch space for the takeovers that follow it.
func TestACreatingEpochIsDrawnAndLeavesRoomToCountUp(t *testing.T) {
	client, _ := newClient(t)
	seen := make(map[uint64]bool, 64)
	for range 64 {
		epoch := client.NewEpoch()
		if epoch < control.MinimumEpoch || epoch > control.MaximumCreateEpoch {
			t.Fatalf("a drawn epoch of %d is outside [%d, %d]",
				epoch, control.MinimumEpoch, control.MaximumCreateEpoch)
		}
		if !control.ValidCreateSequence(control.Sequence(epoch, 1)) {
			t.Fatalf("a creation cannot select the first sequence of epoch %d", epoch)
		}
		if control.MaximumEpoch-epoch < control.MaximumCreateEpoch {
			t.Fatalf("epoch %d leaves %d takeovers, want at least %d",
				epoch, control.MaximumEpoch-epoch, control.MaximumCreateEpoch)
		}
		seen[epoch] = true
	}
	// Distinctness is the whole point: a fixed first epoch would be one value.
	if len(seen) < 32 {
		t.Fatalf("64 draws produced %d distinct epochs", len(seen))
	}
}

// Every open advances the epoch, and the handle that held the previous one is
// fenced: its next write fails and it stays fenced.
func TestOpenFencesThePreviousWriter(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	first, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Select(ctx, control.Sequence(control.MinimumEpoch, 2)); err != nil {
		t.Fatal(err)
	}
	second, err := client.Open(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if second.Epoch() != control.MinimumEpoch+1 {
		t.Fatalf("the second open holds epoch %d, want %d", second.Epoch(), control.MinimumEpoch+1)
	}
	if got := second.Record().Selected; got != control.Sequence(control.MinimumEpoch, 2) {
		t.Fatalf("the second open inherited selection %d", got)
	}
	if _, err := first.Select(ctx, control.Sequence(control.MinimumEpoch, 3)); !errors.Is(err, control.ErrFenced) {
		t.Fatalf("the fenced handle selected a checkpoint: %v", err)
	}
	if _, err := first.Pin(ctx, control.Sequence(control.MinimumEpoch, 2)); !errors.Is(err, control.ErrFenced) {
		t.Fatalf("the fenced handle pinned a checkpoint: %v", err)
	}
	if got := second.Record().Selected; got != control.Sequence(control.MinimumEpoch, 2) {
		t.Fatalf("the fenced handle changed the record: selection is %d", got)
	}
	third, err := client.Open(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if third.Epoch() != control.MinimumEpoch+2 {
		t.Fatalf("the third open holds epoch %d", third.Epoch())
	}
	if _, err := second.Select(ctx, control.Sequence(second.Epoch(), 1)); !errors.Is(err, control.ErrFenced) {
		t.Fatalf("the superseded handle selected a checkpoint: %v", err)
	}
}

// A selection must advance and must belong to the selecting handle's epoch.
// Selecting the sequence already selected is idempotent.
func TestSelectRequiresAnAdvancingSequenceOfThisEpoch(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	if _, err := handle.Select(ctx, second); err != nil {
		t.Fatal(err)
	}
	record, err := handle.Select(ctx, second)
	if err != nil || record.Selected != second {
		t.Fatalf("selecting the current checkpoint again = %+v, %v", record, err)
	}
	if _, err := handle.Select(ctx, root()); !errors.Is(err, control.ErrSequence) {
		t.Fatalf("selecting backwards = %v, want ErrSequence", err)
	}
	if _, err := handle.Select(ctx, control.Sequence(control.MinimumEpoch+5, 1)); !errors.Is(err, control.ErrSequence) {
		t.Fatalf("selecting a sequence of another epoch = %v, want ErrSequence", err)
	}
	handle.Close()
	if _, err := handle.Select(ctx, control.Sequence(control.MinimumEpoch, 3)); !errors.Is(err, control.ErrClosed) {
		t.Fatalf("selecting through a closed handle = %v, want ErrClosed", err)
	}
}

// Pins are an ascending set of this VM's forked checkpoints. A fork adds one,
// which keeps that checkpoint out of reclamation's reach; pinning what is
// pinned adds nothing, and nothing takes one away.
func TestPinsAreASortedSetNothingTakesFrom(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, sequence := range []uint64{control.Sequence(control.MinimumEpoch, 4), root(), control.Sequence(control.MinimumEpoch, 2)} {
		if _, err := handle.Pin(ctx, sequence); err != nil {
			t.Fatal(err)
		}
	}
	record, err := handle.Pin(ctx, root())
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{root(), control.Sequence(control.MinimumEpoch, 2), control.Sequence(control.MinimumEpoch, 4)}
	if !slices.Equal(record.Pinned, want) {
		t.Fatalf("pins = %v, want %v", record.Pinned, want)
	}
	if !record.IsPinned(control.Sequence(control.MinimumEpoch, 2)) || record.IsPinned(control.Sequence(control.MinimumEpoch, 3)) {
		t.Fatalf("IsPinned disagrees with %v", record.Pinned)
	}
	if _, err := handle.Pin(ctx, 0); !errors.Is(err, control.ErrSequence) {
		t.Fatalf("pinning nothing = %v, want ErrSequence", err)
	}
	// The pins survive the handle: a later writer reads exactly what was
	// pinned, and its own writes carry them on.
	reopened, err := client.Open(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(reopened.Record().Pinned, want) {
		t.Fatalf("pins after reopening = %v, want %v", reopened.Record().Pinned, want)
	}
	selected, err := reopened.Select(ctx, control.Sequence(reopened.Epoch(), 1))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(selected.Pinned, want) {
		t.Fatalf("pins after a selection = %v, want %v", selected.Pinned, want)
	}
	current, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(current.Pinned, want) {
		t.Fatalf("the record pins %v, want %v", current.Pinned, want)
	}
}

// A conditional write whose reply is lost has already taken effect. The writer
// recognises its own work by the nonce it chose when it claimed its epoch, and
// reports the write as the success it was.
func TestLostReplyIsReconciledByNonce(t *testing.T) {
	client, store := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	store.FailNextAfterApply(sim.ObjectPut, 1)
	record, err := handle.Select(ctx, second)
	if err != nil {
		t.Fatalf("a selection whose reply was lost reported %v, want the selection it made", err)
	}
	if record.Selected != second {
		t.Fatalf("the reconciled record = %+v, want checkpoint %d", record, second)
	}
	// The handle is not fenced and keeps its validator, so the next selection
	// needs no repair.
	third := control.Sequence(control.MinimumEpoch, 3)
	if _, err := handle.Select(ctx, third); err != nil {
		t.Fatalf("the next selection after a lost reply: %v", err)
	}
	current, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if current.Selected != third || current.Epoch != control.MinimumEpoch {
		t.Fatalf("the durable record = %+v", current)
	}
}

// A lost reply whose read-back also fails leaves the handle tracking a record
// the store has already moved past — its own write, which it never saw land.
// What its next write reads back is therefore neither the record it meant to
// write nor the one it thinks it has, and that must not be read as a takeover:
// only this handle's epoch and nonce can have produced it. The handle adopts it
// and the caller repeats its work. Fencing there would be a writer fenced
// against nothing but itself, with a running guest given up for it.
func TestALostReplyWhoseReadBackFailedDoesNotFenceTheWriter(t *testing.T) {
	client, store := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	store.FailNextAfterApply(sim.ObjectPut, 1)
	store.FailNext(sim.ObjectGet, 1)
	if _, err := handle.Select(ctx, second); !errors.Is(err, platform.ErrInjectedFault) {
		t.Fatalf("a selection whose reply and read-back both failed = %v", err)
	}
	// The selection landed. The handle's next write is refused, because the
	// validator it tracks is the one that write replaced, but the record it
	// reads back is its own and the repeat lands.
	if _, err := handle.Pin(ctx, second); errors.Is(err, control.ErrFenced) {
		t.Fatalf("pinning after a lost reply = %v, want a refusal the handle may repeat", err)
	}
	record, err := handle.Pin(ctx, second)
	if err != nil {
		t.Fatalf("repeating the pin: %v", err)
	}
	if record.Selected != second || !record.IsPinned(second) {
		t.Fatalf("the reconciled record = %+v, want the lost selection and the pin", record)
	}
	current, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if current.Selected != second || !current.IsPinned(second) || current.Epoch != control.MinimumEpoch {
		t.Fatalf("the durable record = %+v", current)
	}
}

// A create is a conditional write like any other, so the refusal it gets back
// may be its own retry finding the object the first attempt wrote. Reading the
// record back is what tells the two apart: a record carrying this attempt's
// nonce is this attempt's work, and the caller owns the VM it just created.
// Reporting that as an existing VM loses a fork's child for good — the record
// is there, so nothing can create it again, and its root was never published,
// so nothing can open it either.
func TestACreateWhoseRetryRefusedItsOwnWriteStillOwnsTheVM(t *testing.T) {
	runtime := sim.New(sim.Config{Seed: 1})
	prefix, err := platform.NewObjectPrefix("deployment/")
	if err != nil {
		t.Fatal(err)
	}
	store := &retryingStore{ObjectStore: runtime.ObjectStore()}
	client, err := control.NewClient(control.Config{ObjectStore: store, ObjectPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	// A fork's child: its record selects a root its own first checkpoint
	// publishes, which is the record nothing else can finish.
	handle, err := client.Create(ctx, "child", root(), false)
	if err != nil {
		t.Fatalf("creating a VM whose write the SDK retried = %v, want the handle that owns it", err)
	}
	if handle.Epoch() != control.MinimumEpoch || handle.Record().Created {
		t.Fatalf("the reconciled handle is at epoch %d holding %+v", handle.Epoch(), handle.Record())
	}
	// The handle owns the record, so the child's first checkpoint publishes.
	record, err := handle.Select(ctx, root())
	if err != nil {
		t.Fatalf("publishing the child's root: %v", err)
	}
	if record.Selected != root() || !record.Created {
		t.Fatalf("the published record = %+v", record)
	}
	// A create of a VM that really is somebody else's is still refused.
	if _, err := client.Create(ctx, "child", root(), true); !errors.Is(err, control.ErrExists) {
		t.Fatalf("creating an existing VM = %v, want ErrExists", err)
	}
}

// A write refused because it never reached the store leaves the record and the
// handle exactly as they were, so the caller may repeat it.
func TestRefusedWriteLeavesTheHandleUsable(t *testing.T) {
	client, store := newClient(t)
	ctx := t.Context()
	handle, err := client.Create(ctx, "vm", root(), true)
	if err != nil {
		t.Fatal(err)
	}
	second := control.Sequence(control.MinimumEpoch, 2)
	store.FailNext(sim.ObjectPut, 1)
	if _, err := handle.Select(ctx, second); !errors.Is(err, platform.ErrInjectedFault) {
		t.Fatalf("a refused selection reported %v, want the injected fault", err)
	}
	if got := handle.Record().Selected; got != root() {
		t.Fatalf("a refused selection changed the tracked record to %d", got)
	}
	if _, err := handle.Select(ctx, second); err != nil {
		t.Fatalf("repeating the refused selection: %v", err)
	}
	current, err := client.Read(ctx, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if current.Selected != second {
		t.Fatalf("the durable record = %+v", current)
	}
}

// An open whose claim lands but whose reply is lost still owns the epoch it
// claimed, which is what the nonce is for.
func TestLostOpenReplyClaimsTheEpochAnyway(t *testing.T) {
	client, store := newClient(t)
	ctx := t.Context()
	if _, err := client.Create(ctx, "vm", root(), true); err != nil {
		t.Fatal(err)
	}
	store.FailNextAfterApply(sim.ObjectPut, 1)
	handle, err := client.Open(ctx, "vm")
	if err != nil {
		t.Fatalf("an open whose reply was lost reported %v", err)
	}
	if handle.Epoch() != control.MinimumEpoch+1 {
		t.Fatalf("the reconciled open holds epoch %d, want %d", handle.Epoch(), control.MinimumEpoch+1)
	}
	if _, err := handle.Select(ctx, control.Sequence(handle.Epoch(), 1)); err != nil {
		t.Fatalf("the reconciled handle could not select: %v", err)
	}
}

// Two opens race for the same epoch: one wins it and the other takes the next,
// so no two live handles ever hold the same writer token.
func TestConcurrentOpensTakeDistinctEpochs(t *testing.T) {
	client, _ := newClient(t)
	ctx := t.Context()
	if _, err := client.Create(ctx, "vm", root(), true); err != nil {
		t.Fatal(err)
	}
	type result struct {
		handle *control.Handle
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			handle, err := client.Open(context.WithoutCancel(ctx), "vm")
			results <- result{handle, err}
		}()
	}
	epochs := make(map[uint64]bool, 2)
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		epochs[got.handle.Epoch()] = true
	}
	if len(epochs) != 2 || !epochs[control.MinimumEpoch+1] || !epochs[control.MinimumEpoch+2] {
		t.Fatalf("concurrent opens took epochs %v, want %d and %d",
			epochs, control.MinimumEpoch+1, control.MinimumEpoch+2)
	}
}

// A record this deployment cannot have written is corrupt, never a usable one.
func TestCorruptRecordIsRefused(t *testing.T) {
	client, store := newClient(t)
	ctx := t.Context()
	if _, err := client.Create(ctx, "vm", root(), true); err != nil {
		t.Fatal(err)
	}
	key, err := platform.NewObjectKey("deployment/control/vm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, platform.PutRequest{Key: key, Body: bytes.NewReader([]byte("not a record")), Size: 12}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(ctx, "vm"); !errors.Is(err, control.ErrCorrupt) {
		t.Fatalf("reading a corrupt record = %v, want ErrCorrupt", err)
	}
	if _, err := client.Open(ctx, "vm"); !errors.Is(err, control.ErrCorrupt) {
		t.Fatalf("opening a corrupt record = %v, want ErrCorrupt", err)
	}
}
