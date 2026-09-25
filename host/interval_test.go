package host_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/volume"
)

// The interval makes a guest's disks durable and not its RAM. The checkpoint it
// publishes holds what the guest stored into its disk and nothing of what it
// stored into its RAM, and no VMM state, so opening it is a cold boot over
// that disk: RAM is uploaded only when a capture asks for it.
func TestTheIntervalCheckpointsDisksAndNotRAM(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = 10 * time.Millisecond
	h.start(t)
	pagers := newPager(t, h.configs[0].Resources)
	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 7)
	guest.store("disk", 0, 9)
	before := vm.Status().Checkpoint
	counting := newCountingMachine(guest)
	if err := h.hosts[0].AddMachine("vm-1", counting); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("vm-1")
	deadline := time.Now().Add(10 * time.Second)
	for vm.Status().Checkpoint == before {
		if time.Now().After(deadline) {
			t.Fatalf("the interval checkpoint never landed: %+v", vm.Status())
		}
		time.Sleep(time.Millisecond)
	}
	durable := make([]byte, migrationPageSize)
	if err := vm.Volume("disk").Read(t.Context(), 0, durable); err != nil {
		t.Fatal(err)
	}
	if want := bytes.Repeat([]byte{9}, migrationPageSize); !bytes.Equal(durable, want) {
		t.Fatalf("the disk reads %d... after the interval checkpoint, want 9...", durable[0])
	}
	if err := vm.Volume("ram0").Read(t.Context(), 0, durable); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(durable, make([]byte, migrationPageSize)) {
		t.Fatalf("the interval checkpoint published RAM: page 0 reads %d...", durable[0])
	}
	if _, err := host.State(t.Context(), h.hosts[0].Checkpoints(), vm.Status().Checkpoint); !errors.Is(err, checkpoint.ErrNoState) {
		t.Fatalf("the interval checkpoint's VMM state: %v, want none", err)
	}
	if got := counting.stateCaptures(); got != 0 {
		t.Fatalf("the interval captured the VMM state %d times, want never", got)
	}
}

// diskVolumes is a VM with RAM and a disk beside it, which is the VM an
// interval checkpoint makes durable part of.
func diskVolumes() []volume.VolumeSpec {
	return append(slices.Clone(migrationVolumes),
		volume.VolumeSpec{Name: "disk", Size: 8 * migrationPageSize, PageSize: migrationPageSize})
}

// countingMachine records every capture a host's interval loop drives, so a test
// can see the loop run rather than only its effect. The loop's captures are of
// the VM's disks; a capture of its whole state is counted apart, because only
// a request makes one. turns carries one value per capture of either kind,
// which is how a test waits for the loop to take a turn instead of waiting out
// a stretch of wall clock and hoping it did.
type countingMachine struct {
	*machine
	mu       sync.Mutex
	captures int
	states   int
	turns    chan struct{}
}

func newCountingMachine(m *machine) *countingMachine {
	return &countingMachine{machine: m, turns: make(chan struct{}, 64)}
}

func (m *countingMachine) Prepare(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
	state, sources, err := m.machine.Prepare(ctx)
	if err == nil {
		m.mu.Lock()
		m.captures++
		m.states++
		m.mu.Unlock()
		m.turned()
	}
	return state, sources, err
}

func (m *countingMachine) SealDisks(ctx context.Context) (map[string]volume.DirtySource, error) {
	sources, err := m.machine.SealDisks(ctx)
	if err == nil {
		m.mu.Lock()
		m.captures++
		m.mu.Unlock()
		m.turned()
	}
	return sources, err
}

func (m *countingMachine) turned() {
	select {
	case m.turns <- struct{}{}:
	default:
	}
}

// stateCaptures is how many captures took the whole VMM state.
func (m *countingMachine) stateCaptures() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.states
}

func (m *countingMachine) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.captures
}

// awaitTurns waits for the interval loop to drive n more captures of this
// machine, which is how long a test has to hold something else still to know
// the loop has had its turn at it.
func (m *countingMachine) awaitTurns(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-m.turns:
		case <-time.After(10 * time.Second):
			t.Fatalf("the interval loop captured %d times, want %d more", m.count(), n)
		}
	}
}

// A host checkpoints the disks of every VM it runs on its interval, which is
// the only thing that makes a running guest's disks durable. The loop starts
// with the machine and stops when the host stops running it.
func TestHostCheckpointsEveryVMOnItsInterval(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = 10 * time.Millisecond
	h.start(t)
	pagers := newPager(t, h.configs[0].Resources)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", diskVolumes())
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(4) {
		guest.store("disk", page, byte(page+1))
	}
	// Nothing is durable yet: the pager holds every one of those pages.
	before := vm.Status().Checkpoint
	durable := make([]byte, migrationPageSize)
	if err := vm.Volume("disk").Read(t.Context(), 0, durable); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(durable, make([]byte, migrationPageSize)) {
		t.Fatal("a store reached the volume without a checkpoint")
	}

	counting := newCountingMachine(guest)
	if err := h.hosts[0].AddMachine("vm-1", counting); err != nil {
		t.Fatal(err)
	}
	counting.awaitTurns(t, 1)
	deadline := time.Now().Add(10 * time.Second)
	for vm.Status().Checkpoint == before {
		if time.Now().After(deadline) {
			t.Fatalf("the interval checkpoint never landed: %+v", vm.Status())
		}
		time.Sleep(time.Millisecond)
	}
	for page := range uint64(4) {
		if err := vm.Volume("disk").Read(t.Context(), page*migrationPageSize, durable); err != nil {
			t.Fatal(err)
		}
		if want := bytes.Repeat([]byte{byte(page + 1)}, migrationPageSize); !bytes.Equal(durable, want) {
			t.Fatalf("page %d is %d... after the interval checkpoint, want %d...", page, durable[0], want[0])
		}
	}

	// Removing the machine stops its loop: nothing captures a VM this host no
	// longer runs. RemoveMachine waits for the loop to return, publication and
	// all, so the count it leaves behind is final — there is no window left for
	// a capture to land in and nothing to wait out.
	h.hosts[0].RemoveMachine("vm-1")
	stopped := counting.count()
	if again := counting.count(); again != stopped {
		t.Fatalf("the loop captured %d more times after the machine was removed", again-stopped)
	}
}

// An explicit capture and the interval checkpoint serialize on the VM's own
// publication lock, so a fork never races a checkpoint of the same guest.
func TestIntervalCheckpointSerializesWithAnExplicitCapture(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.configs[0].CheckpointInterval = time.Millisecond
	h.start(t)
	pagers := newPager(t, h.configs[0].Resources)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 9)
	counting := &countingMachine{machine: guest}
	if err := h.hosts[0].AddMachine("vm-1", counting); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("vm-1")

	// Explicit captures run against the interval loop. Every one of them either
	// publishes or reports the handle's own terminal state; none of them may see
	// two checkpoints of one guest in flight, which the publication lock is what
	// prevents.
	for range 8 {
		checkpoint, err := host.Capture(t.Context(), vm, counting, nil)
		if err != nil {
			t.Fatalf("an explicit capture during the interval loop: %v", err)
		}
		if err := checkpoint.Wait(t.Context()); err != nil {
			t.Fatalf("publishing an explicit capture: %v", err)
		}
		point, err := host.Seal(t.Context(), vm, counting)
		if err != nil {
			t.Fatalf("sealing the fork point during the interval loop: %v", err)
		}
		fork, state, err := host.CreateFork(t.Context(), h.hosts[0].Volumes(), "fork-"+checkpoint.Ref().String()[len(checkpoint.Ref().VM)+1:], point)
		if err != nil {
			t.Fatalf("forking an explicit capture: %v", err)
		}
		if string(state) != "vmm-state" {
			t.Fatalf("the fork restored %q", state)
		}
		got := make([]byte, migrationPageSize)
		if err := fork.Volume("ram0").Read(t.Context(), 0, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, bytes.Repeat([]byte{9}, migrationPageSize)) {
			t.Fatalf("the fork read %d..., want the guest's 9", got[0])
		}
		if err := fork.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if counting.count() < 8 {
		t.Fatalf("only %d captures ran, want at least the 8 explicit ones", counting.count())
	}
	var _ host.Machine = counting
}

// closingMachine records that the host closed this VMM process, which is what a
// host fenced out of a VM must do rather than leave its guest running.
type closingMachine struct {
	*machine
	mu     sync.Mutex
	closes int
}

func (m *closingMachine) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closes++
	return m.machine.Close()
}

func (m *closingMachine) closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closes > 0
}

// A checkpoint is where a fenced host finds out it has lost a VM: a running
// guest writes nothing else. Everything that guest goes on producing is
// unpublishable, so the host closes the machine — stopping the VMM and
// releasing the volumes — and tells its supervisor, rather than leaving a stale
// guest burning this host's pages until something else stops it.
func TestAFencedHostClosesTheVMItCanNoLongerPublish(t *testing.T) {
	h := newHostHarness(t)
	h.configs[0].CheckpointInterval = 5 * time.Millisecond
	closed := make(chan string, 1)
	h.configs[0].MachineClosed = func(vmID string) { closed <- vmID }
	h.start(t)
	pagers := newPager(t, h.configs[0].Resources)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 3)
	// The VM is openable elsewhere only once a checkpoint of it is published.
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	fenced := &closingMachine{machine: guest}
	if err := h.hosts[0].AddMachine("vm-1", fenced); err != nil {
		t.Fatal(err)
	}

	// The second host takes the VM over, which advances the epoch in its control
	// record and fences the first out of it.
	taken, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close(t.Context())

	select {
	case vmID := <-closed:
		if vmID != "vm-1" {
			t.Fatalf("the host reported closing %q", vmID)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the fenced host never closed the VM it had lost")
	}
	if !fenced.closed() {
		t.Fatal("the fenced host reported the VM closed without stopping its VMM")
	}
	if machines := h.hosts[0].Machines(); len(machines) != 0 {
		t.Fatalf("the fenced host still runs %v", machines)
	}
	for _, open := range h.hosts[0].Volumes().VMs() {
		if open.ID() == "vm-1" {
			t.Fatal("the fenced host still holds the VM's volumes")
		}
	}
}

// dyingMachine is a VMM process a test can kill: it counts the captures its
// host's interval loop drives and records the Close a host owes a machine it
// gives up.
type dyingMachine struct {
	*countingMachine
	mu     sync.Mutex
	closes int
}

func (m *dyingMachine) Close() error {
	m.mu.Lock()
	m.closes++
	m.mu.Unlock()
	return m.countingMachine.Close()
}

func (m *dyingMachine) closed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closes > 0
}

// A VMM process can end without anyone asking it to: the host kills it because
// its pager session failed, the kernel kills it, or it crashes. Nothing else on
// this host notices — the checkpoint loop wakes only on its interval and finds a
// socket that refuses the connection, which it logs and retries forever, while
// the supervisor goes on reporting a guest that no longer exists. So the host
// watches the process: when it ends, the VM is given up, its volumes are
// released, its checkpoint loop stops and the supervisor is told.
func TestAHostGivesUpAVMWhoseVMMProcessDied(t *testing.T) {
	h := newHostHarness(t)
	h.configs[0].CheckpointInterval = 5 * time.Millisecond
	closed := make(chan string, 1)
	h.configs[0].MachineClosed = func(vmID string) { closed <- vmID }
	h.start(t)
	pagers := newPager(t, h.configs[0].Resources)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 3)
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatal(err)
	}
	dying := &dyingMachine{countingMachine: &countingMachine{machine: guest}}
	if err := h.hosts[0].AddMachine("vm-1", dying); err != nil {
		t.Fatal(err)
	}

	dying.die(errors.New("the VMM was killed: pager ram0: page 2 fault: unavailable"))

	select {
	case vmID := <-closed:
		if vmID != "vm-1" {
			t.Fatalf("the host reported closing %q", vmID)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the host never gave up the VM whose VMM process had died")
	}
	if !dying.closed() {
		t.Fatal("the host gave the VM up without closing the process that held its pages")
	}
	if machines := h.hosts[0].Machines(); len(machines) != 0 {
		t.Fatalf("the host still runs %v", machines)
	}
	for _, open := range h.hosts[0].Volumes().VMs() {
		if open.ID() == "vm-1" {
			t.Fatal("the host still holds the volumes of a VM whose VMM process died")
		}
	}
	// The checkpoint loop is what would go on failing against a dead VMM, so it
	// stops with the machine: no capture is driven after the process ended.
	captures := dying.count()
	select {
	case <-t.Context().Done():
	case <-time.After(20 * 5 * time.Millisecond):
	}
	if after := dying.count(); after != captures {
		t.Fatalf("the checkpoint loop drove %d more captures against a dead VMM", after-captures)
	}
}
