//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

const (
	// forkFanOutRAM and forkFanOutRoot are the child's shape, and
	// forkFanOutTouched the working set in MiB the parent leaves in its RAM
	// before the point is taken: every page of it is one no checkpoint holds,
	// so every child has to fetch it from the parent's page server before it
	// may be released, and every page of it is one both children read back.
	forkFanOutRAM     = 256 << 20
	forkFanOutRoot    = 64 << 20
	forkFanOutTouched = 64
	// forkFanOutArena and forkFanOutDirty are each destination pager's arena
	// and its budget for private state no checkpoint has, in bytes, because the
	// two pagers count them in their own pages. Two children of this shape map
	// four times the RAM arena between them, so every read of theirs evicts,
	// spills and refaults, and the dirty budget is the arena's size as a
	// deployment's is rather than the whole logical space.
	//
	forkFanOutArena = 128 << 20
	forkFanOutDirty = 128 << 20
	// forkFanOutRootArena is the destination's PMEM arena. It is stated apart
	// because the two pagers hold different things: the ratio above is about the
	// memory two children map, and the roots are what a host keeps resident.
	forkFanOutRootArena = 128 << 20
	// forkFanOutInterval is how often each child is checkpointed while it reads,
	// which is what a deployment does to a VM that is answering: the guest pauses
	// for the state capture and the seal, and the pages upload behind it.
	forkFanOutInterval = 250 * time.Millisecond
	// forkFanOutRounds is how many times each child reads everything it has.
	forkFanOutRounds = 2
)

// forkFanOutRead bounds the phase this test exists for: two children of one
// point reading all of their memory and all of their root volume at the same
// time. It separates slow from stopped and asserts nothing about speed, which
// is why it is generous rather than tight.
//
// It was two minutes while RAM's page was 2 MiB. At 4 KiB the same shape is
// 512 times the page operations: read-ahead takes free arena slots and never
// evicts, so under an arena a quarter of what the two children map every page
// of a scan is its own fault, and two children scanning 512 MiB twice is on the
// order of half a million of them. On the qualification instance the phase
// takes about a minute and a half on its own and four and a half behind the
// rest of the suite, so the bound is ten minutes. It is deliberately far above
// either: it is here to tell a child that is merely slow from one that has
// stopped, and a child that has stopped never finishes however long it is
// given.
const forkFanOutRead = 10 * time.Minute

// TestFirecrackerForkFanOutServesBothChildrenAtOnce forks one running guest
// into two children on a second pager and page server, receives them one after
// the other exactly as the orchestrator does, and then asks both guests to read
// every page of their memory and their whole root volume at the same time while
// both are being checkpointed on an interval.
//
// It is the shape a fan-out actually takes in a deployment and the one thing no
// other suite has: two guests forked from one parent, on one pager, reaching every page
// they inherited at once, over a real vCPU, a real UFFD and a real page server.
// Each of them alone is the migration suite. Both children must answer — a
// child whose read never returns is a guest nothing can tell from a dead one.
func TestFirecrackerForkFanOutServesBothChildrenAtOnce(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	c := newMigrationCluster(t, ctx)

	parent, err := c.source.Create(ctx, "parent", []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: forkFanOutRAM, PageSize: ramPageBytes(t)},
		{Name: "root", Size: forkFanOutRoot, PageSize: checkpoint.PageSize2MiB},
	})
	if err != nil {
		t.Fatal(err)
	}
	loadRootImage(t, ctx, parent.Volume("root"))
	if err := parent.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}

	sourcePager := newMigrationPager(t, ctx)
	p, err := vmmachine.Start(ctx, migrationConfig(t, binaryPath, sourcePager, parent))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)

	// The parent's guest takes a working set of its own and stores into its DAX
	// root, and the parent then publishes it. That checkpoint is what both
	// children inherit: pages neither of them writes, which they must read
	// as one page between them rather than one each.
	command(t, ctx, p, fmt.Sprintf("pressure %d\n", forkFanOutTouched),
		fmt.Sprintf("SPROUTFS_PRESSURE bytes=%d", forkFanOutTouched<<20))
	command(t, ctx, p, "ram 73\n", "SPROUTFS_RAM ram=73")
	command(t, ctx, p, "write 41\n", "SPROUTFS_FLUSH disk=41")
	published, err := parent.Snapshot(ctx, prepareAndResume(p))
	if err != nil {
		t.Fatalf("publishing the checkpoint %s's children inherit: %v\n%s", parent.ID(), err, consoleText(p))
	}
	if err := published.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	inheritedPages, _ := published.Sealed()
	if inheritedPages == 0 {
		t.Fatal("the parent published no page for its children to share")
	}
	// And stores into half of that working set again, so the point below also
	// holds pages of both volumes that no checkpoint has: those are the ones
	// each child fetches over the wire and publishes as its own. Half is
	// deliberate — a fan-out has both kinds of page at once, and a child reading
	// everything reads them in the same pass.
	command(t, ctx, p, fmt.Sprintf("touch %d\n", forkFanOutTouched/2),
		fmt.Sprintf("SPROUTFS_TOUCH bytes=%d", (forkFanOutTouched/2)<<20))
	command(t, ctx, p, "ram 74\n", "SPROUTFS_RAM ram=74")
	command(t, ctx, p, "dirty 42\n", "SPROUTFS_DIRTY disk=42")

	// The source's page server at the budgets a deployment runs, rather than
	// the raised ones a single migration is measured under: one peer is one
	// destination host, and both children of this fan-out are that one host, so
	// their regions share every budget counted per peer.
	pages, err := vmmigrate.NewPageSource(ctx, vmmigrate.SourceConfig{Network: c.network,
		Address: "source-pages", PageSize: pagerPageBytes(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pages.Close() })

	// One pause starts them both: a fan-out of forks is one fork point.
	point, err := parent.ForkPoint(ctx, prepareAndResume(p))
	if err != nil {
		t.Fatalf("sealing the point %s is forked at: %v\n%s", parent.ID(), err, consoleText(p))
	}
	// The fan-out's own hold keeps the point while the children are described,
	// exactly as the host's does.
	point.Hold()
	if err := point.Pin(ctx); err != nil {
		t.Fatal(err)
	}
	children := []string{"child-a", "child-b"}
	handoffs := make([]vmmigrate.Handoff, 0, len(children))
	for _, child := range children {
		point.Hold()
		handoff, err := vmmigrate.Fork(ctx, child, point, pages, vmmigrate.Options{})
		if err != nil {
			t.Fatal(err)
		}
		handoffs = append(handoffs, handoff)
	}
	if err := point.Retire(ctx); err != nil {
		t.Fatal(err)
	}
	inherited := 0
	for _, region := range handoffs[0].Regions {
		for _, run := range region.Unpublished {
			inherited += run.Count
		}
	}
	if inherited == 0 {
		t.Fatal("the point holds no page a child would have to fetch")
	}
	t.Logf("fan-out point: children=%d inherited_pages=%d", len(children), inherited)

	// Both children land on one destination pager, which is what makes them
	// share its pages, its budgets and what they inherited.
	destinationPager := newSizedMigrationPager(t, ctx, forkFanOutArena, forkFanOutRootArena,
		len(children)*(forkFanOutRAM+forkFanOutRoot)+(64<<20), forkFanOutDirty)
	taken := make([]*forkedChild, 0, len(children))
	for _, handoff := range handoffs {
		taken = append(taken, receiveChild(t, ctx, c, destinationPager, binaryPath, handoff, pages))
	}

	// Every child is checkpointed on an interval from here, as a deployment
	// checkpoints a VM that is answering: the reads below run under the pauses,
	// the seals and the uploads that go with them.
	for _, child := range taken {
		stopInterval := checkpointEvery(t, ctx, child, forkFanOutInterval)
		defer stopInterval()
	}

	// Every child's read runs at once and every one of them has to answer. A
	// checkpressure walks the whole working set the parent left in RAM and a
	// scan reads every page of the root volume, so between them they reach
	// every page the child inherited and every page its own log holds.
	read, stopReading := context.WithTimeout(ctx, forkFanOutRead)
	defer stopReading()
	began := time.Now()
	failures := make([]error, len(taken))
	var wg sync.WaitGroup
	for index, child := range taken {
		wg.Go(func() { failures[index] = readEverything(read, child) })
	}
	wg.Wait()
	for index, failure := range failures {
		if failure == nil {
			continue
		}
		t.Errorf("%v\nreceive=%+v\nconsole:\n%s", failure, taken[index].received.Stats(),
			consoleText(taken[index].process))
	}
	if t.Failed() {
		t.Fatalf("a child of the fan-out never finished reading what it inherited\n%s", stacks())
	}
	t.Logf("fan-out read: children=%d rounds=%d elapsed=%s", len(taken), forkFanOutRounds, time.Since(began))

	for _, child := range taken {
		stats := child.received.Stats()
		t.Logf("fan-out child %s: unpublished=%d fetched=%d peer_pages=%d volume_pages=%d requests=%d refusals=%d stalls=%d",
			child.id, stats.Unpublished, stats.Fetched, stats.PeerPages, stats.VolumePages,
			stats.Requests, stats.Refusals, stats.Stalls)
	}
	served := pages.Stats()
	t.Logf("fan-out page server: requests=%d served=%d absent=%d refused=%d listings=%d",
		served.Requests, served.Served, served.Absent, served.Refused, served.Listings)
	if left := pages.Outstanding(); len(left) != 0 {
		t.Fatalf("the parent's page server still owes %v after both children were released", left)
	}
	// What the two of them inherited and never wrote is one page between them,
	// which is the reason a fan-out puts children on one host: every page of the
	// parent's that both read must cost one load and one page, not one each. The
	// arena is a quarter of what they map, so not every page outlives the
	// sibling that would have shared it — but a host that shared none of them
	// would be paying twice for pages the two agree on completely.
	shared := 0
	for _, child := range taken {
		for _, name := range []string{vmmachine.RAMVolume, "root"} {
			counted := censusOf(t, ctx, child.vm, name)
			t.Logf("fan-out census %s/%s: %s", child.id, name, counted)
			shared += counted.inherited
		}
	}
	if shared == 0 {
		t.Fatal("the children hold no page inherited from their parent, so nothing here measures sharing")
	}
	pager, err := destinationPager.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Half of what they hold in common is a floor with room in it: each child
	// reads every one of those pages twice, so a pager sharing them at all
	// clears this by several times over, and only one sharing almost none of it
	// does not.
	if want := uint64(shared / 2); pager.IdentityHits < want {
		t.Errorf("two children of one fork point holding %d pages inherited from their parent shared %d pages, want at least %d: %+v",
			shared, pager.IdentityHits, want, pager)
	}
	t.Logf("fan-out destination pager: faults=%d loads=%d evictions=%d spills=%d refaults=%d dirty_stalls=%d dirty_waits=%d identity_hits=%d",
		pager.Faults, pager.Loads, pager.Evictions, pager.Spills, pager.SpillRefaults,
		pager.DirtyStalls, pager.DirtyWaits, pager.IdentityHits)
}

// readEverything is one child reading back all of the memory its parent
// touched and every page of its root volume, a few times over. One round is
// already every page; the rounds are there because the second one reads them
// through whatever the first left behind — evicted, spilled, sealed by an
// interval checkpoint — rather than through a cold region.
func readEverything(ctx context.Context, child *forkedChild) error {
	for round := range forkFanOutRounds {
		if err := guestCommand(ctx, child.process, "checkpressure\n",
			fmt.Sprintf("SPROUTFS_PRESSURE_OK bytes=%d", forkFanOutTouched<<20)); err != nil {
			return fmt.Errorf("%s reading back its memory on round %d: %w", child.id, round, err)
		}
		if err := guestCommand(ctx, child.process, "scan\n",
			fmt.Sprintf("SPROUTFS_SCAN bytes=%d", forkFanOutRoot)); err != nil {
			return fmt.Errorf("%s reading its root volume on round %d: %w", child.id, round, err)
		}
	}
	return nil
}

// forkedChild is one child of a fan-out on its destination: the VM, the VMM
// process running it and the receive that streamed its inherited pages in.
type forkedChild struct {
	id       string
	vm       *volume.VM
	process  *vmmachine.Process
	received *vmmigrate.Received
}

// prepareAndResume is the pause every capture and every fork point takes: the
// VMM state is saved, the memory is sealed with it, and the guest runs again
// before any byte is uploaded.
func prepareAndResume(p *vmmachine.Process) volume.PrepareFunc {
	return func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		state, sources, err := p.Prepare(ctx)
		if err != nil {
			return nil, nil, err
		}
		if err := p.Resume(ctx); err != nil {
			return nil, nil, err
		}
		return state, sources, nil
	}
}

// checkpointEvery checkpoints one child on an interval until the returned stop
// is called, which is what a host does to every VM it runs. A capture that
// fails fails the test: the guest is running and answering, so nothing here is
// a checkpoint a host would be entitled to skip.
func checkpointEvery(t *testing.T, ctx context.Context, child *forkedChild, interval time.Duration) func() {
	t.Helper()
	ticking, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ticking.Done():
				return
			case <-time.After(interval):
			}
			ckpt, err := child.vm.Snapshot(ticking, prepareAndResume(child.process))
			if err != nil {
				if ticking.Err() == nil {
					t.Errorf("checkpointing %s while it reads: %v", child.id, err)
				}
				return
			}
			if err := ckpt.Wait(ticking); err != nil && ticking.Err() == nil {
				t.Errorf("publishing a checkpoint of %s while it reads: %v", child.id, err)
				return
			}
		}
	}()
	return func() {
		stop()
		<-done
	}
}

// receiveChild takes one child over exactly as the deployment does: the
// destination creates it and streams the pages no checkpoint holds out of the
// parent's page server, publishes the child's root index once it has them all,
// and only then does the parent's host release the hold that child kept.
func receiveChild(t *testing.T, ctx context.Context, c *migrationCluster, pager *hostPagers,
	binaryPath string, handoff vmmigrate.Handoff, source *vmmigrate.PageSource) *forkedChild {
	t.Helper()
	var process *vmmachine.Process
	start := func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing,
		state []byte) (vmmigrate.Runtime, error) {
		config := migrationConfig(t, binaryPath, pager, vm)
		config.RestoreState = state
		config.Backings = backings
		started, err := vmmachine.Start(ctx, config)
		if err != nil {
			return nil, err
		}
		// The child's guest runs while the rest of what it inherited arrives,
		// which is the whole point of a post-copy.
		if err := started.Release(ctx); err != nil {
			return nil, errors.Join(err, started.Close())
		}
		process = started
		return started, nil
	}
	dial := func(ctx context.Context, peer platform.Address) (platform.Conn, error) {
		return c.network.Dial(ctx, "destination-host", peer)
	}
	received, err := vmmigrate.Receive(ctx, c.destination, handoff, dial, start, vmmigrate.Options{})
	if err != nil {
		t.Fatalf("receiving %s: %v", handoff.VMID, err)
	}
	t.Cleanup(func() {
		received.Close()
		if process != nil {
			_ = process.Close()
		}
	})
	if err := received.Done(ctx); err != nil {
		t.Fatalf("streaming %s from %s: %v", handoff.VMID, handoff.Source, err)
	}
	if stats := received.Stats(); stats.Fetched != stats.Unpublished {
		t.Fatalf("%s holds %d of the %d pages no checkpoint has", handoff.VMID, stats.Fetched, stats.Unpublished)
	}
	// The root index is what makes a child a VM anything else can open, and the
	// destination publishes it as soon as the child holds every page it
	// inherited — a capture like any other, over a guest that is running.
	child := received.VM()
	ckpt, err := child.Snapshot(ctx, prepareAndResume(process))
	if err != nil {
		t.Fatalf("publishing the root index of %s: %v\n%s", handoff.VMID, err, consoleText(process))
	}
	if err := ckpt.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	// The rest of what the source holds arrives behind the running guest, and
	// the child's word that it holds every inherited page is what gives the
	// parent the memory those pages are in back.
	if err := received.Streamed(ctx); err != nil {
		t.Fatalf("the bulk stream of %s stopped early: %v", handoff.VMID, err)
	}
	received.Close()
	if err := source.Release(handoff.VMID); err != nil {
		t.Fatalf("the parent refused to release %s after it fetched every page: %v", handoff.VMID, err)
	}
	return &forkedChild{id: handoff.VMID, vm: child, process: process, received: received}
}

// stacks is every goroutine of this process, which is what a read that never
// came back leaves behind as evidence.
func stacks() []byte {
	buffer := make([]byte, 1<<20)
	return buffer[:runtime.Stack(buffer, true)]
}

// census is what one volume's pages name, counted: a hole, an object of a VM
// this one descends from, or one of its own. It is what says how many inherited
// pages two children of a point have to share, and how many each of them has
// already diverged from.
type census struct {
	pages, holes, inherited, own int
}

func (c census) String() string {
	return fmt.Sprintf("pages=%d holes=%d inherited=%d own=%d", c.pages, c.holes, c.inherited, c.own)
}

func censusOf(t *testing.T, ctx context.Context, vm *volume.VM, name string) census {
	t.Helper()
	v := vm.Volume(name)
	counted := census{pages: int(v.Size() / checkpoint.PageSize2MiB)}
	for page := range v.Size() / checkpoint.PageSize2MiB {
		extents, err := v.Locate(ctx, page*checkpoint.PageSize2MiB, checkpoint.PageSize2MiB)
		if err != nil {
			t.Fatal(err)
		}
		for _, extent := range extents {
			switch {
			case extent.Identity.Zero || extent.Identity.Ref.IsZero():
				counted.holes++
			case extent.Identity.Ref.VM == vm.ID():
				counted.own++
			default:
				counted.inherited++
			}
		}
	}
	return counted
}
