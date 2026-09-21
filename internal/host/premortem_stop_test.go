package host_test

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/volume"
)

// TestStoppingAParentWhoseFanOutLandedOnItsOwnHostIsRefused: a child taken in
// here attaches over the point itself rather than over the page server, so no
// page of it ever reaches the wire. What the pages the child reads are is the
// same either way — the parent's VMM process's — so a stop that closed it would
// take the point out from under a child that is faulting for it, and the
// refusal has to come from the parent's own seal rather than from what the page
// server happens to be serving. The hold is reported as the handover it is,
// because it is what holds the parent sealed.
func TestStoppingAParentWhoseFanOutLandedOnItsOwnHostIsRefused(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	started := map[string]*machine{}
	h.configs[0].Migration.StartVM = starters(t, pagers[0], started)
	h.configs[0].Migration.HoldTimeout = time.Minute
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 21)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	// No destination: the children are this host's own, and no page of them
	// ever reaches the wire.
	handoffs, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, "")
	if err != nil {
		t.Fatal(err)
	}
	received, err := h.hosts[0].Receive(t.Context(), handoffs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer received.Close()
	if handoffs[0].Source != "" {
		t.Fatalf("a child of this host's own is given %q to fetch from", handoffs[0].Source)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 1 || serving[0] != "child" {
		t.Fatalf("the host reports holding %v, want the point it holds for its own child", serving)
	}

	if _, err := h.hosts[0].Stop(t.Context(), "parent"); !errors.Is(err, volume.ErrSealed) {
		t.Fatalf("stopping a parent a local fork point holds = %v, want ErrSealed", err)
	}
	if running := h.hosts[0].Machines(); len(running) != 2 {
		t.Fatalf("the refusal left the host running %v, want the parent and its child", running)
	}
	if guest.closed.Load() {
		t.Fatal("a refused stop closed the parent's VMM process anyway")
	}

	// Released is the child's word that it has every page it inherited, and
	// from there the parent is an ordinary running VM again.
	if err := h.hosts[0].ReleaseMigrated("child"); err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 1, 22)
	stopped, err := h.hosts[0].Stop(t.Context(), "parent")
	if err != nil {
		t.Fatalf("stopping a parent whose children have been released: %v", err)
	}
	if stopped.Sequence == 0 {
		t.Fatalf("the stop published %s, want a checkpoint of the parent", stopped)
	}
	reopened, err := h.hosts[1].Volumes().Open(t.Context(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	page := make([]byte, migrationPageSize)
	if err := reopened.Volume("ram0").Read(t.Context(), migrationPageSize, page); err != nil {
		t.Fatal(err)
	}
	if want := bytes.Repeat([]byte{22}, migrationPageSize); !bytes.Equal(page, want) {
		t.Fatalf("the stopped parent came back holding %d..., want what it wrote after the fork", page[0])
	}
	// And the child is untouched by its parent being stopped: it published a
	// root of its own when it was taken in, so what it inherited is its own.
	if child := started["child"]; child == nil {
		t.Fatal("the fork started no child on this host")
	} else if got := child.load("ram0", 0); !bytes.Equal(got, bytes.Repeat([]byte{21}, migrationPageSize)) {
		t.Fatalf("the child holds %d... after its parent was stopped, want the point it inherited", got[0])
	}
}

// TestAParentIsStoppableOnceItsForkHoldOutlivesItsDeadline: a hold nothing
// releases retires on its own, four checkpoint intervals on, so that a parent
// whose child is gone is durable again rather than sealed for as long as this
// host runs. What the parent must be after that is an ordinary running VM: the
// deadline gives the pages back, so the stop that was refused while the
// point held them publishes everything the guest has.
func TestAParentIsStoppableOnceItsForkHoldOutlivesItsDeadline(t *testing.T) {
	const holdTimeout = 50 * time.Millisecond
	h, pagers := startMigrationHosts(t)
	h.configs[0].Migration.HoldTimeout = holdTimeout
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 31)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	// The child is handed to a host that never takes it: this is the
	// destination that died, or the orchestrator that never said the word.
	if _, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Stop(t.Context(), "parent"); !errors.Is(err, volume.ErrSealed) {
		t.Fatalf("stopping a sealed parent = %v, want ErrSealed", err)
	}
	// Nothing releases it, so the deadline does.
	awaitReleased(t, h.hosts[0])

	guest.store("ram0", 1, 32)
	stopped, err := h.hosts[0].Stop(t.Context(), "parent")
	if err != nil {
		t.Fatalf("stopping a parent whose point outlived its deadline: %v", err)
	}
	if stopped.Sequence == 0 {
		t.Fatalf("the stop published %s, want a checkpoint of the parent", stopped)
	}
	if running := h.hosts[0].Machines(); len(running) != 0 {
		t.Fatalf("the host still runs %v after stopping it", running)
	}
	reopened, err := h.hosts[1].Volumes().Open(t.Context(), "parent")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	page := make([]byte, migrationPageSize)
	if err := reopened.Volume("ram0").Read(t.Context(), migrationPageSize, page); err != nil {
		t.Fatal(err)
	}
	if want := bytes.Repeat([]byte{32}, migrationPageSize); !bytes.Equal(page, want) {
		t.Fatalf("the stopped parent came back holding %d..., want the byte it wrote last", page[0])
	}
}

// TestStoppingAndStartingOneVMOverAndOverLeavesNothingBehind: the soak stops
// and starts its share of the population every round, so the same identity goes
// round this loop many times over one pair of hosts. Every turn has to publish
// what the guest held and give everything else back — the registration, the
// pages, the handle — or a host that has done a few rounds is a host that
// cannot take another VM.
func TestStoppingAndStartingOneVMOverAndOverLeavesNothingBehind(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	at := 0
	// Eight turns over two hosts, which is more rounds than the soak's default
	// and enough that anything a turn keeps has accumulated.
	for turn := range 8 {
		value := byte(40 + turn)
		guest.store("ram0", uint64(turn%4), value)
		stopped, err := h.hosts[at].Stop(t.Context(), "vm-1")
		if err != nil {
			t.Fatalf("turn %d: stopping the VM: %v", turn, err)
		}
		if stopped.Sequence == 0 {
			t.Fatalf("turn %d: the stop published %s", turn, stopped)
		}
		if running := h.hosts[at].Machines(); len(running) != 0 {
			t.Fatalf("turn %d: %s still runs %v after the stop", turn, h.hostID(at), running)
		}
		if serving := h.hosts[at].Status().Serving; len(serving) != 0 {
			t.Fatalf("turn %d: %s still serves %v after the stop", turn, h.hostID(at), serving)
		}

		at = (at + 1) % 2
		opened, err := h.hosts[at].Volumes().Open(t.Context(), "vm-1")
		if err != nil {
			t.Fatalf("turn %d: starting the VM on %s: %v", turn, h.hostID(at), err)
		}
		page := make([]byte, migrationPageSize)
		if err := opened.Volume("ram0").Read(t.Context(),
			uint64(turn%4)*migrationPageSize, page); err != nil {
			t.Fatal(err)
		}
		if want := bytes.Repeat([]byte{value}, migrationPageSize); !bytes.Equal(page, want) {
			t.Fatalf("turn %d: the started VM holds %d..., want the byte the stop published",
				turn, page[0])
		}
		guest, err = newMachine(t, pagers[at], opened, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.hosts[at].AddMachine("vm-1", guest); err != nil {
			t.Fatalf("turn %d: registering the started VM: %v", turn, err)
		}
	}
	if _, err := h.hosts[at].Stop(t.Context(), "vm-1"); err != nil {
		t.Fatal(err)
	}
	// Whatever a turn took, it gave back: both hosts are as empty as they were.
	for index := range 2 {
		if running := h.hosts[index].Machines(); len(running) != 0 {
			t.Fatalf("%s still runs %v", h.hostID(index), running)
		}
		if serving := h.hosts[index].Status().Serving; len(serving) != 0 {
			t.Fatalf("%s still serves %v", h.hostID(index), serving)
		}
		if open := h.hosts[index].Volumes().VMs(); len(open) != 0 {
			t.Fatalf("%s still holds %d handles after every stop", h.hostID(index), len(open))
		}
	}
}

// TestAStopReportsTheCheckpointTheVMComesBackAt: the checkpoint a stop names is
// the whole of what it tells the deployment — the handle that knew is released
// by the time it answers, and a start reports what it found rather than what it
// was promised. A stop that published and then let the interval loop publish
// again behind it would name a pause the VM does not come back at.
//
// The store publishes slowly and the interval is short, so the loop always has
// a turn inside the stop's own publication.
func TestAStopReportsTheCheckpointTheVMComesBackAt(t *testing.T) {
	h := newSizedHostHarnessOn(t, 2, slowPublishStore())
	for i := range h.configs {
		h.configs[i].CheckpointInterval = 5 * time.Millisecond
	}
	pagers := newPager(t, h.configs[0].Resources)
	h.configs[0].Pagers = pagers.pagers
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 55)
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	stopped, err := h.hosts[0].Stop(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := h.hosts[1].Volumes().Open(t.Context(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(t.Context()); err != nil {
			t.Error(err)
		}
	}()
	if came := reopened.Status().Checkpoint; came != stopped {
		t.Fatalf("the stop reported %s and the VM comes back at %s", stopped, came)
	}
}

// slowPublishStore is an object store whose writes take long enough that a
// checkpoint's publication is still in flight while the test acts.
func slowPublishStore() sim.ObjectStoreConfig {
	return sim.ObjectStoreConfig{GetLatency: time.Nanosecond, ListLatency: time.Nanosecond,
		PutLatency: 5 * time.Millisecond, BytesPerSecond: 1 << 60}
}
