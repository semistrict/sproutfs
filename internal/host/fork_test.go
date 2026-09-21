package host_test

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/host"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/volume"
)

// One pause of the parent hands every child of one fork over. The parent goes
// on running and publishes nothing to be forked; it takes its pages back as
// each child is released, which is once that child holds every page it
// inherited and has published a root of its own, and is checkpointed again from
// there.
func TestHostForksEveryChildFromOnePause(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	// The interval loop runs, so the test also shows the parent checkpointed
	// again once its pages are its own. The holds' deadline is four intervals,
	// which at this one would retire the point while the children are still
	// being taken in, so it is given a bound of its own.
	h.configs[0].CheckpointInterval = 10 * time.Millisecond
	h.configs[0].Migration.HoldTimeout = time.Minute
	pagers := newPager(t, h.configs[0].Resources)
	// The children land on the parent's own host, which is what takes them in.
	children := []string{"fork-a", "fork-b", "fork-c"}
	started := make(map[string]*machine, len(children))
	h.configs[0].Migration.StartVM = starters(t, pagers, started)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(3) {
		guest.store("ram0", page, byte(page+1))
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("vm-1")

	// A second VM on the same host and the same interval is the metronome: its
	// loop and the parent's are separate goroutines on equal intervals, so
	// turns of this one are turns the parent's loop has had too. It is what
	// lets the assertion below be about what the loop did rather than about how
	// long the test waited.
	metronomeVM, err := h.hosts[0].Volumes().Create(t.Context(), "vm-2", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	metronomeGuest, err := newMachine(t, pagers, metronomeVM, nil)
	if err != nil {
		t.Fatal(err)
	}
	metronome := newCountingMachine(metronomeGuest)
	if err := h.hosts[0].AddMachine("vm-2", metronome); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("vm-2")

	handoffs, err := h.hosts[0].Fork(t.Context(), "vm-1", children, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != len(children) {
		t.Fatalf("one pause handed over %d children, want %d", len(handoffs), len(children))
	}
	forks := make([]*volume.VM, 0, len(handoffs))
	for _, handoff := range handoffs {
		if string(handoff.State) != "vmm-state" {
			t.Fatalf("%s restores %q", handoff.VMID, handoff.State)
		}
		received, err := h.hosts[0].Receive(t.Context(), handoff)
		if err != nil {
			t.Fatal(err)
		}
		defer received.Close()
		forks = append(forks, received.VM())
	}
	// Every child has a root index of its own, published by the host that took
	// it in as soon as it held every page it inherited.
	for _, fork := range forks {
		if status := fork.Status(); status.Root {
			t.Fatalf("%s has no root index of its own after the fork: %+v", fork.ID(), status)
		}
	}
	// The parent's pages stay its children's until each of them is released.
	if status := vm.Status(); !status.Sealed {
		t.Fatalf("the parent's seal ended before its children were released: %+v", status)
	}
	for _, child := range children {
		if err := h.hosts[0].ReleaseMigrated(child); err != nil {
			t.Fatal(err)
		}
	}
	if status := vm.Status(); status.Sealed {
		t.Fatalf("the parent is still sealed after every child was released: %+v", status)
	}
	// Every child reads the parent's memory at the fork point, including the pages
	// no checkpoint holds.
	for _, fork := range forks {
		for page := range uint64(3) {
			got := make([]byte, migrationPageSize)
			if err := fork.Volume("ram0").Read(t.Context(), page*migrationPageSize, got); err != nil {
				t.Fatal(err)
			}
			if want := bytes.Repeat([]byte{byte(page + 1)}, migrationPageSize); !bytes.Equal(got, want) {
				t.Fatalf("%s page %d is %d..., want the parent's %d...", fork.ID(), page, got[0], want[0])
			}
		}
	}
	// A point nothing has retired holds the parent, and one checkpoint of a
	// region is outstanding at a time, so a fork taken while one is held is
	// refused.
	point, err := host.Seal(t.Context(), vm, guest)
	if err != nil {
		t.Fatal(err)
	}
	before := vm.Status().Checkpoint
	if _, err := h.hosts[0].Fork(t.Context(), "vm-1", []string{"fork-d"}, ""); !errors.Is(err, volume.ErrSealed) {
		t.Fatalf("a second pause while one is held = %v, want ErrSealed", err)
	}
	// The interval loop leaves a sealed parent alone rather than failing on it.
	// Three turns of the metronome are three intervals the parent's own loop
	// has woken for and found the fork point holding its pages.
	metronome.awaitTurns(t, 3)
	if status := vm.Status(); status.Checkpoint != before || status.CheckpointError != nil {
		t.Fatalf("the interval loop ran against a sealed parent: %+v", status)
	}
	if err := point.Retire(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The parent is checkpointed again, on its own interval.
	deadline := time.Now().Add(10 * time.Second)
	for vm.Status().Checkpoint == before {
		if time.Now().After(deadline) {
			t.Fatalf("the parent never checkpointed again after its fork: %+v", vm.Status())
		}
		time.Sleep(time.Millisecond)
	}
	// Every child is a VM of this host like any other from here, and its guest
	// is the one the host took in.
	for _, child := range children {
		if started[child] == nil {
			t.Fatalf("%s was taken in without a guest", child)
		}
		h.hosts[0].RemoveMachine(child)
	}
}

// A cross-host fork hold is the destination's, and the orchestrator releasing
// it is what ends the parent's seal. Nothing else does: an orchestrator that
// restarts, or a destination that dies, between the handoff and the release
// leaves the parent sealed for good — never checkpointed, never fenced, never
// migratable, and with a dirty set that only grows. The hold has a deadline of
// its own, after which the parent takes its pages back and is checkpointed
// again, and the child falls back to the checkpoint it was forked from.
func TestForkHoldExpiresWhenNothingReleasesIt(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var received *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &received)
	h.configs[0].CheckpointInterval = 10 * time.Millisecond
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "vm-1", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(3) {
		guest.store("ram0", page, byte(page+1))
	}
	if err := h.hosts[0].AddMachine("vm-1", guest); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("vm-1")

	handoffs, err := h.hosts[0].Fork(t.Context(), "vm-1", []string{"fork-a"}, h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 1 || handoffs[0].VMID != "fork-a" {
		t.Fatalf("the fork handed over %+v", handoffs)
	}
	if status := vm.Status(); !status.Sealed {
		t.Fatalf("the parent is not sealed while its child holds the point: %+v", status)
	}
	before := vm.Status().Checkpoint

	// Nothing ever takes the child or releases the hold.
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := vm.Status()
		if !status.Sealed && status.Checkpoint != before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the parent is still held by a fork nothing released: %+v", status)
		}
		time.Sleep(time.Millisecond)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("the expired hold still serves %v", serving)
	}
	// A parent that has its pages back is migratable like any other.
	handoff, err := h.hosts[0].Migrate(t.Context(), "vm-1", h.pages[1])
	if err != nil {
		t.Fatalf("migrating a parent whose fork hold expired: %v", err)
	}
	taken, err := h.hosts[1].Receive(t.Context(), handoff)
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	if err := taken.Done(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := received.load("ram0", 2); got[0] != 3 {
		t.Fatalf("the destination reads page 2 as %d, want the guest's 3", got[0])
	}
}

// TestDeletingAForkParentRetiresItsForkPoints: a child forked onto another host
// reads the fork point's pages out of this host's memory, and the process that
// maps them is exactly what deleting the parent closes. Nothing retired the
// points taken on the parent, so the page server went on offering that child a
// point whose pages were gone: every page it had not fetched came back
// absent and it read the checkpoint's older bytes instead, silently. Retiring
// the point first stops this host offering them at all, so the child's next
// fault for one fails and says so.
func TestDeletingAForkParentRetiresItsForkPoints(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(3) {
		guest.store("ram0", page, byte(page+1))
	}
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1]); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 1 || serving[0] != "child" {
		t.Fatalf("the fork point is served for %v, want the child", serving)
	}

	if err := h.hosts[0].Delete(t.Context(), "parent"); err != nil {
		t.Fatal(err)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 0 {
		t.Fatalf("a deleted parent still offers %v a point whose pages are gone", serving)
	}
	if machines := h.hosts[0].Machines(); len(machines) != 0 {
		t.Fatalf("a deleted parent is still registered: %v", machines)
	}
	if !guest.closed.Load() {
		t.Fatal("a deleted parent left its VMM process running")
	}
	if vms := h.hosts[0].Volumes().VMs(); len(vms) != 0 {
		t.Fatalf("a deleted parent left %d handles open", len(vms))
	}
	// The record is gone, so nothing opens the parent again.
	if _, err := h.hosts[1].Volumes().Open(t.Context(), "parent"); err == nil {
		t.Fatal("a deleted parent can still be opened")
	}
}

// TestDeletingASealedVMIsRefused: the pages a fork point reads through are
// exactly the ones closing the VMM process detaches, so a delete that went ahead
// would take the point out from under whoever holds it — a child created here
// whose root has not published yet holds the point through its own handle, which
// this host has no hold of its own for. A delete is refused while anything still
// holds the VM sealed, the way a migration is, and the VM is left exactly as it
// was.
func TestDeletingASealedVMIsRefused(t *testing.T) {
	h := newSizedHostHarness(t, 1)
	h.start(t)
	pagers := newPager(t, h.configs[0].Resources)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "sealed", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers, vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 7)
	if err := h.hosts[0].AddMachine("sealed", guest); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("sealed")

	point, err := host.Seal(t.Context(), vm, guest)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].Delete(t.Context(), "sealed"); !errors.Is(err, volume.ErrSealed) {
		t.Fatalf("deleting a VM a fork point holds = %v, want ErrSealed", err)
	}
	if guest.closed.Load() {
		t.Fatal("a refused delete closed the VMM process the fork point reads through")
	}
	if machines := h.hosts[0].Machines(); len(machines) != 1 || machines[0] != "sealed" {
		t.Fatalf("a refused delete left the host running %v", machines)
	}
	// Retiring the point gives the pages back, and the delete goes through.
	if err := point.Retire(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.hosts[0].Delete(t.Context(), "sealed"); err != nil {
		t.Fatal(err)
	}
}

// countingNetwork reports how many connections a host has dialed, which is what
// says whether a child took its inherited pages off the wire or out of the
// pages its parent already holds.
type countingNetwork struct {
	platform.Network
	dials atomic.Int64
}

func (n *countingNetwork) Dial(ctx context.Context, from, to platform.Address) (platform.Conn, error) {
	n.dials.Add(1)
	return n.Network.Dial(ctx, from, to)
}

// TestLocalForkReceivesTheForkPointOverThePages: a fork is always a handoff, and
// a child that lands on its parent's own host receives one over a local backing
// rather than over the page server: the pager shares the parent's sealed pages
// with the child by identity, so every inherited page is present the moment the
// region attaches. No byte is copied, no page is loaded back out of the store
// and nothing is dialed. The child's root is published by the host that took it
// in, as soon as it holds every page, which is before the parent's seal ends.
func TestLocalForkReceivesTheForkPointOverThePages(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var child *machine
	// The destination of this fork is the parent's own host, so the child's
	// regions attach to the pager the parent's pages are in.
	h.configs[0].Migration.StartVM = starter(t, pagers[0], &child)
	network := &countingNetwork{Network: h.configs[0].Network}
	h.configs[0].Network = network
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(3) {
		guest.store("ram0", page, byte(page+1))
	}
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("parent")

	handoffs, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(handoffs) != 1 || handoffs[0].VMID != "child" || !handoffs[0].IsFork() {
		t.Fatalf("the fork handed over %+v", handoffs)
	}
	if status := vm.Status(); !status.Sealed {
		t.Fatalf("the parent is not sealed while its child holds the point: %+v", status)
	}
	// Nothing is served to a child on the parent's own host: its pages never
	// leave the pages they are in, so the handoff names no address to fetch
	// from. The hold is reported all the same — it is what holds the parent
	// sealed, and a host that reported it as holding nothing would be telling
	// the deployment that a parent nothing can checkpoint is a parent nothing
	// is waiting on.
	if handoffs[0].Source != "" {
		t.Fatalf("a child on the parent's own host is given %q to fetch from", handoffs[0].Source)
	}
	if serving := h.hosts[0].Status().Serving; len(serving) != 1 || serving[0] != "child" {
		t.Fatalf("the host reports holding %v, want the point it holds for its own child", serving)
	}

	before, err := pagers[0].ram().Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	received, err := h.hosts[0].Receive(t.Context(), handoffs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer received.Close()
	// Receive returns once the child holds every page only the parent had, and
	// the root it publishes then is what makes it a VM anything can open.
	if status := received.VM().Status(); status.Root {
		t.Fatalf("the child has no root index of its own after the handoff: %+v", status)
	}
	if status := vm.Status(); !status.Sealed {
		t.Fatalf("the parent's seal ended before the child was released: %+v", status)
	}
	// Every inherited page is the parent's, read out of the memory the parent
	// sealed rather than loaded back through a backing.
	for page := range uint64(3) {
		if got := child.load("ram0", page); got[0] != byte(page+1) {
			t.Fatalf("the child reads page %d as %d, want the parent's %d", page, got[0], page+1)
		}
	}
	after, err := pagers[0].ram().Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.Loads != before.Loads {
		t.Fatalf("the child loaded %d pages back that its parent already held",
			after.LoadedPages-before.LoadedPages)
	}
	if after.IdentityHits-before.IdentityHits < 3 {
		t.Fatalf("the child mapped %d of the parent's pages by identity, want the 3 it inherited",
			after.IdentityHits-before.IdentityHits)
	}
	if dials := network.dials.Load(); dials != 0 {
		t.Fatalf("a child on its parent's own host dialed %d connections", dials)
	}
	// The release is what ends the parent's seal, here as for a child on
	// another host, and the parent is checkpointable again from there.
	if err := h.hosts[0].ReleaseMigrated("child"); err != nil {
		t.Fatal(err)
	}
	if status := vm.Status(); status.Sealed {
		t.Fatalf("the parent kept its seal after the child was released: %+v", status)
	}
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatalf("the parent could not checkpoint after the fork: %v", err)
	}
}

// TestForkPublishesTheChildRootWhenItsPostCopyIsDone: a child on another host
// holds pages no checkpoint has the moment its post-copy ends, and publishing
// its root there is what makes it a VM any host can open. It happens when Done
// reports rather than at whatever interval checkpoint comes first, so a child
// whose host has no interval loop at all is still openable elsewhere.
func TestForkPublishesTheChildRootWhenItsPostCopyIsDone(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var child *machine
	h.configs[1].Migration.StartVM = starter(t, pagers[1], &child)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := range uint64(3) {
		guest.store("ram0", page, byte(page+1))
	}
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("parent")

	handoffs, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, h.pages[1])
	if err != nil {
		t.Fatal(err)
	}
	received, err := h.hosts[1].Receive(t.Context(), handoffs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer received.Close()
	if status := received.VM().Status(); status.Root {
		t.Fatalf("the child has no root index of its own after its post-copy: %+v", status)
	}
	// The interval loop is off on both hosts, so the root above is the only
	// checkpoint the child ever took: a third host opens it.
	elsewhere, err := h.hosts[2].Volumes().Open(t.Context(), "child")
	if err != nil {
		t.Fatalf("a child whose post-copy is done cannot be opened elsewhere: %v", err)
	}
	got := make([]byte, migrationPageSize)
	if err := elsewhere.Volume("ram0").Read(t.Context(), 2*migrationPageSize, got); err != nil {
		t.Fatal(err)
	}
	if got[0] != 3 {
		t.Fatalf("the published child reads page 2 as %d, want the parent's 3", got[0])
	}
	if err := elsewhere.Handoff(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestDeletingAParentUnderALocalChildIsTheSameAsUnderARemoteOne: a child on the
// parent's own host reads the point out of the pages the parent's VMM
// process maps, which is exactly what deleting the parent closes. The hold this
// host records for it is the same hold a child on another host has, so the
// delete retires the point first and the point is given up with it.
func TestDeletingAParentUnderALocalChildIsTheSameAsUnderARemoteOne(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	var child *machine
	h.configs[0].Migration.StartVM = starter(t, pagers[0], &child)
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 7)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	if _, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, ""); err != nil {
		t.Fatal(err)
	}
	// The hold is this host's, whatever the destination, so the delete finds it
	// and gives it up rather than being refused by the seal it holds.
	if err := h.hosts[0].Delete(t.Context(), "parent"); err != nil {
		t.Fatal(err)
	}
	if machines := h.hosts[0].Machines(); len(machines) != 0 {
		t.Fatalf("a deleted parent is still registered: %v", machines)
	}
	if !guest.closed.Load() {
		t.Fatal("a deleted parent left its VMM process running")
	}
	if _, err := h.hosts[0].Volumes().Open(t.Context(), "parent"); err == nil {
		t.Fatal("a deleted parent can still be opened")
	}
}

// TestALocalForkHoldExpiresWhenNothingReleasesIt: one hold table, one deadline.
// A child on the parent's own host is a hold like any other — the orchestrator
// releasing it is what ends the parent's seal — so an orchestrator that
// restarted, or a destination that never took the child in, leaves the parent
// sealed until the deadline retires the point on its own.
func TestALocalForkHoldExpiresWhenNothingReleasesIt(t *testing.T) {
	h, pagers := startMigrationHosts(t)
	clock := sim.New(sim.Config{Seed: 1}).NewClock("source")
	h.configs[0].Clock = clock
	h.configs[0].EpochInterval = -1
	h.start(t)

	vm, err := h.hosts[0].Volumes().Create(t.Context(), "parent", migrationVolumes)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := newMachine(t, pagers[0], vm, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.store("ram0", 0, 7)
	if err := h.hosts[0].AddMachine("parent", guest); err != nil {
		t.Fatal(err)
	}
	defer h.hosts[0].RemoveMachine("parent")

	if _, err := h.hosts[0].Fork(t.Context(), "parent", []string{"child"}, ""); err != nil {
		t.Fatal(err)
	}
	if !vm.Status().Sealed {
		t.Fatal("a fork point does not hold the parent's pages")
	}
	if released := clock.Advance(3 * time.Minute); released != 0 {
		t.Fatalf("released %d deadlines three minutes into a four-minute hold", released)
	}
	clock.Settle()
	if !vm.Status().Sealed {
		t.Fatal("the parent lost its seal three minutes into a four-minute hold")
	}
	if released := clock.Advance(2 * time.Minute); released != 1 {
		t.Fatalf("released %d deadlines past the hold's four minutes, want the child's", released)
	}
	clock.Settle()
	if status := vm.Status(); status.Sealed {
		t.Fatalf("the parent is still held by a fork nothing released: %+v", status)
	}
	if err := guest.checkpoint(t.Context(), vm); err != nil {
		t.Fatalf("the parent could not checkpoint after the hold expired: %v", err)
	}
}
