//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/vmmemory"
	"github.com/semistrict/sproutfs/vmmigrate"
	"github.com/semistrict/sproutfs/volume"
)

// clockBound is how much a guest's wall clock may move against the host's
// across a restore, beyond what the console's round trips leave unknown. On
// x86_64 a restore moves the guest's clocks on by the host's own wall time
// since the capture, so what is left is the microseconds between two reads of
// the host's clock and the guest's drift over the seconds a test runs. A clock
// a restore did not move on falls behind by the whole stop.
const clockBound = 20 * time.Millisecond

// stopFor is how long a stopped VM stays stopped before it is restored. It is
// long enough that a clock left where the state stopped it is plainly wrong,
// and short enough for a test that waits it out on the wall clock.
const stopFor = 10 * time.Second

// entropyReading is what a guest answered the entropy command with, and when
// the host asked and read the answer.
type entropyReading struct {
	random      string
	reseeds     int64
	generation  int64
	disruption  int64
	realtime    time.Time
	monotonic   time.Duration
	clocksource string
	// asked and answered bound when the guest read its clock, on the host's.
	asked, answered time.Time
}

// String is the reading as a failing test reports it.
func (r entropyReading) String() string {
	return fmt.Sprintf("random=%s reseeds=%d generation=%d disruption=%d realtime=%s monotonic=%s clocksource=%s"+
		" (offset from the host %s..%s)", r.random, r.reseeds, r.generation, r.disruption,
		r.realtime.Format(time.RFC3339Nano), r.monotonic, r.clocksource,
		r.realtime.Sub(r.answered), r.realtime.Sub(r.asked))
}

// offset is how far the guest's wall clock was from the host's: at least
// earliest and at most latest, since the guest read its clock between the
// host's asking and its reading the answer.
func (r entropyReading) offset() (earliest, latest time.Duration) {
	return r.realtime.Sub(r.answered), r.realtime.Sub(r.asked)
}

// keptOffset reports whether the guest's wall clock is as far from the host's
// as it was at an earlier reading, within clockBound beyond what the two round
// trips leave unknown. A guest's clock starts tens of milliseconds from the
// host's at boot; what a restore must do is move it on by exactly the time the
// host's moved on, so that this offset stays where it was.
func (r entropyReading) keptOffset(earlier entropyReading) bool {
	earliest, latest := r.offset()
	thenEarliest, thenLatest := earlier.offset()
	return earliest <= thenLatest+clockBound && latest >= thenEarliest-clockBound
}

// drawEntropy asks a guest for an entropyReading over its console.
func drawEntropy(t *testing.T, ctx context.Context, p *vmmachine.Process) entropyReading {
	t.Helper()
	reader := newConsole(p)
	defer reader.close()
	if err := reader.skipExisting(); err != nil {
		t.Fatal(err)
	}
	asked := time.Now()
	if err := p.WriteConsole(ctx, []byte("entropy\n")); err != nil {
		t.Fatal(err)
	}
	line, err := reader.wait(ctx, "SPROUTFS_ENTROPY")
	answered := time.Now()
	if err != nil {
		t.Fatalf("asking for entropy: %v\n%s", err, consoleText(p))
	}
	fields := map[string]string{}
	for _, field := range strings.Fields(line)[1:] {
		key, value, _ := strings.Cut(field, "=")
		fields[key] = value
	}
	number := func(key string) int64 {
		value, err := strconv.ParseInt(fields[key], 10, 64)
		if err != nil {
			t.Fatalf("the guest's %s in %q: %v", key, line, err)
		}
		return value
	}
	return entropyReading{random: fields["random"], reseeds: number("reseeds"),
		generation: number("generation"), disruption: number("disruption"),
		realtime: time.Unix(0, number("realtime_ns")), monotonic: time.Duration(number("monotonic_ns")),
		clocksource: fields["clocksource"], asked: asked, answered: answered}
}

// reseedBound is how long after its release a restored guest's kernel may take
// to reseed its random pool. The guest takes the VMGenID interrupt as it
// resumes, and on x86_64 the driver's work then runs on two of the kernel's
// worker threads, behind the ACPI interpreter. On the qualification host the
// reseed was seen at most 381 ms after a release, most of which was the guest
// faulting its memory back before it could answer; a guest past this has not
// reseeded at all.
const reseedBound = time.Second

// drawUntilReseeded asks a restored guest for entropy as soon as it runs, and
// again until its kernel has reseeded reseeds times in all. It returns the
// first answer, which may come before the reseed, and the first answer after
// it, and logs how long after released the reseed was seen.
func drawUntilReseeded(t *testing.T, ctx context.Context, p *vmmachine.Process, name string,
	released time.Time, reseeds int64) (first, reseeded entropyReading) {
	t.Helper()
	first = drawEntropy(t, ctx, p)
	t.Logf("%s, %s after its release: %s", name, first.answered.Sub(released), first)
	reseeded = first
	for reseeded.reseeds < reseeds {
		if reseeded.answered.Sub(released) > reseedBound {
			t.Fatalf("%s had not reseeded %s after its release: %s\n%s", name, reseedBound, reseeded, consoleText(p))
		}
		reseeded = drawEntropy(t, ctx, p)
		t.Logf("%s, %s after its release: %s", name, reseeded.answered.Sub(released), reseeded)
	}
	return first, reseeded
}

// TestForkChildrenOfOnePointDrawDifferentRandomBytes: two children restored
// from one fork point start from the same guest memory, random pool included.
// Each restore gives its guest a new generation ID, and the guest's kernel
// reseeds its pool from it within reseedBound of the guest's release. The
// bytes each child draws then differ from the other's and from its parent's,
// and so do the first bytes each draws, asked as soon as it runs. The parent
// was never restored and keeps the pool it had.
func TestForkChildrenOfOnePointDrawDifferentRandomBytes(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	c := newMigrationCluster(t, ctx)
	parentVM := newGuestVMIn(t, ctx, c, "random-parent")
	parent, err := vmmachine.Start(ctx, migrationConfig(t, binaryPath, newMigrationPager(t, ctx), parentVM))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	waitLine(t, ctx, parent, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
	booted := drawEntropy(t, ctx, parent)
	t.Logf("parent at boot: %s", booted)
	if booted.reseeds != 0 || booted.generation != 0 || booted.disruption != 0 {
		t.Fatalf("a guest that was never restored reports %s, want no reseed and both VMClock counters 0", booted)
	}

	// The parent host's peer server, which the children fetch the pages no
	// checkpoint has from.
	pages, err := peer.NewServer(ctx, peer.ServerConfig{Network: c.network,
		Address: "random-pages", PageSize: pagerPageBytes(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pages.Close() })
	point, err := parentVM.ForkPoint(ctx, prepareAndResume(parent))
	if err != nil {
		t.Fatalf("sealing the fork point: %v\n%s", err, consoleText(parent))
	}
	if err := point.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := point.Pin(ctx); err != nil {
		t.Fatal(err)
	}

	destination := newSizedMigrationPager(t, ctx, 256<<20, 128<<20, 768<<20, 768<<20)
	// Each child is asked as soon as it is running, the first thing anything
	// asks of it, and then until its kernel has reseeded.
	var firsts, reseeded []entropyReading
	for index := range 2 {
		name := fmt.Sprintf("random-child-%d", index)
		child, released := resumeChild(t, ctx, c, destination, binaryPath, pages, point, name)
		first, after := drawUntilReseeded(t, ctx, child, name, released, 1)
		if after.generation != 1 || after.disruption != 1 {
			t.Fatalf("%s reports %s, want both VMClock counters 1", name, after)
		}
		// Only x86_64's VMM moves a restored guest's clock on; aarch64's is
		// left behind by the time since the point, as the restore test says.
		if vmmachine.MovesClock(runtime.GOARCH) && !after.keptOffset(booted) {
			t.Fatalf("%s reads %s; its wall clock is not as far from the host's as its parent's was, %s",
				name, after, booted)
		}
		firsts, reseeded = append(firsts, first), append(reseeded, after)
	}
	after := drawEntropy(t, ctx, parent)
	t.Logf("parent after the fork: %s", after)
	if after.reseeds != 0 || after.generation != 0 {
		t.Fatalf("the parent reports %s after its children were restored, want it untouched", after)
	}

	// No two draws share their bytes: not the parent's, not a child's first,
	// and not a child's once it has reseeded.
	type draw struct{ by, random string }
	draws := []draw{{"the parent at boot", booted.random}, {"the parent after the fork", after.random}}
	for index := range firsts {
		by := fmt.Sprintf("random-child-%d", index)
		draws = append(draws, draw{by + " as soon as it ran", firsts[index].random})
		if reseeded[index] != firsts[index] {
			draws = append(draws, draw{by + " once it had reseeded", reseeded[index].random})
		}
	}
	seen := map[string]string{}
	for _, d := range draws {
		if other, found := seen[d.random]; found {
			t.Fatalf("%s drew %s, as %s did", d.by, d.random, other)
		}
		seen[d.random] = d.by
	}
}

// TestARestoreAfterAStopReseedsAndKeepsTheClock: a VM is stopped — its last
// checkpoint taken with the guest paused, and its VMM ended — and opened again
// from that checkpoint stopFor later. Its guest reseeds its random pool for the
// new generation, its VMClock counters say it was restored, and on x86_64 its
// wall clock is as far from the host's as before the stop, within clockBound,
// rather than stopFor further behind.
func TestARestoreAfterAStopReseedsAndKeepsTheClock(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	if !vmmachine.MovesClock(runtime.GOARCH) {
		t.Skip("Firecracker leaves a restored guest's clock where its state stopped it on " + runtime.GOARCH)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	c := newMigrationCluster(t, ctx)
	vm := newGuestVMIn(t, ctx, c, "stopped")
	running, err := vmmachine.Start(ctx, migrationConfig(t, binaryPath, newMigrationPager(t, ctx), vm))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = running.Close() })
	waitLine(t, ctx, running, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
	before := drawEntropy(t, ctx, running)
	t.Logf("before the stop: %s", before)

	// The stop: the last checkpoint with the guest left paused, then the VMM
	// ended, as a host stops a VM.
	last, err := vm.Snapshot(ctx, func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		return running.Prepare(ctx)
	}, volume.Terms{})
	if err != nil {
		t.Fatalf("the stop's checkpoint: %v\n%s", err, consoleText(running))
	}
	if err := last.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(stopFor):
	}

	// The open: another host takes the VM over and restores the state its
	// control record selects.
	opened, err := c.destination.Open(ctx, vm.ID())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opened.Close(context.Background()) })
	root, err := c.store.Open(ctx, opened.Status().Checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	state, err := c.store.ReadState(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	config := migrationConfig(t, binaryPath, newMigrationPager(t, ctx), opened)
	config.RestoreState = state
	restored, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatalf("restoring the stopped VM: %v", err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if err := restored.Release(ctx); err != nil {
		t.Fatalf("resuming the stopped VM: %v\n%s", err, consoleText(restored))
	}
	released := time.Now()
	first, after := drawUntilReseeded(t, ctx, restored, fmt.Sprintf("the guest opened %s after its stop", stopFor),
		released, before.reseeds+1)
	if after.generation != before.generation+1 || after.disruption != before.disruption+1 {
		t.Fatalf("the restored guest reports %s, want both VMClock counters one higher than %s", after, before)
	}
	if first.random == before.random || after.random == before.random {
		t.Fatalf("the restored guest drew %s and then %s, and before the stop %s", first.random, after.random, before.random)
	}
	// The first answer of a restored guest waits on the faults that bring its
	// memory back, which widens the window its clock is known within; the
	// next answers as fast as the one before the stop did.
	settled := drawEntropy(t, ctx, restored)
	t.Logf("asked again: %s", settled)
	for _, reading := range []entropyReading{first, settled} {
		if !reading.keptOffset(before) {
			t.Fatalf("the restored guest reads %s; its wall clock moved against the host's by more than %s since %s\n%s",
				reading, clockBound, before, consoleText(restored))
		}
	}
}

// resumeChild takes one child of a fork point onto the destination's pagers,
// as a host receives a fork, and returns it running and when it was released.
func resumeChild(t *testing.T, ctx context.Context, c *migrationCluster, pagers *hostPagers, binaryPath string,
	source *peer.Server, point *volume.ForkPoint, id string) (*vmmachine.Process, time.Time) {
	t.Helper()
	if err := point.Hold(); err != nil {
		t.Fatal(err)
	}
	handoff, err := vmmigrate.Fork(ctx, id, point, source, vmmigrate.Options{})
	if err != nil {
		t.Fatalf("forking %s: %v", id, err)
	}
	var process *vmmachine.Process
	var released time.Time
	start := func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing,
		state []byte) (vmmigrate.Runtime, error) {
		config := migrationConfig(t, binaryPath, pagers, vm)
		config.RestoreState, config.Backings = state, backings
		started, err := vmmachine.Start(ctx, config)
		if err != nil {
			return nil, err
		}
		if err := started.Release(ctx); err != nil {
			return nil, errors.Join(err, started.Close())
		}
		process, released = started, time.Now()
		return started, nil
	}
	dial := func(ctx context.Context, peer platform.Address) (platform.Conn, error) {
		return c.network.Dial(ctx, "destination-host", peer)
	}
	received, err := vmmigrate.Receive(ctx, c.destination, handoff, destinationPeers(t, ctx, dial), start, vmmigrate.Options{})
	if err != nil {
		t.Fatalf("receiving %s: %v", id, err)
	}
	t.Cleanup(func() {
		received.Close()
		_ = process.Close()
		_ = source.Release(id)
	})
	if err := received.Done(ctx); err != nil {
		t.Fatalf("fetching what only the parent had for %s: %v", id, err)
	}
	return process, released
}
