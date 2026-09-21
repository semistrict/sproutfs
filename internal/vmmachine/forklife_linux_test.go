//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmmigrate"
	"github.com/semistrict/sproutfs/internal/volume"
)

// The fan-out's guest dies about a second after it is resumed, and the only
// thing the pager does in that window is the child's root-index checkpoint —
// a seal and a settle taken a fifth of a second into the child's life, while
// its background stream is still fetching the pages it is sealing. Everything
// after that, the read rounds this suite was built around, costs minutes and
// buys nothing: by then the guest is already dead or already safe.
//
// So this suite buys the same evidence by the second. It takes children of one
// fork point two at a time exactly as the fan-out does — post-copy receive,
// resume, root-index checkpoint at once, background stream behind it — gives
// each one a few seconds of life and one question to answer, and counts the
// ones whose guest kernel died. A hundred lives is a few minutes, which is what
// makes an arm mean something.
const (
	// forkLifeSpan is how long a child is left running before it is torn down.
	// The deaths all land inside the first second and a half; this is wide
	// enough to catch a late one and narrow enough that a hundred lives is
	// minutes.
	forkLifeSpan = 3 * time.Second
	// forkLifeProbe bounds the one question each child is asked. A guest whose
	// kernel has died answers nothing, which is how a death is seen.
	forkLifeProbe = 8 * time.Second
)

// forkLives is how many children one run takes, in pairs.
func forkLives(t *testing.T) int {
	t.Helper()
	value := os.Getenv("SPROUTFS_FORK_LIVES")
	if value == "" {
		return 20
	}
	lives, err := strconv.Atoi(value)
	if err != nil || lives < 2 {
		t.Fatalf("SPROUTFS_FORK_LIVES=%q must be a count of at least two", value)
	}
	return lives
}

// forkLifeShape is what one trial does, so an arm is a shape rather than a
// patch: each field is one of the things the fan-out does that a child might
// not survive.
type forkLifeShape struct {
	// siblings is how many children are taken from the point at once.
	siblings int
	// checkpointAfterStream defers the root-index checkpoint until every page
	// the child inherited has arrived, instead of taking it while the stream
	// is still fetching.
	checkpointAfterStream bool
	// interval checkpoints each child on that interval while it lives, zero
	// for none.
	interval time.Duration
}

func (s forkLifeShape) String() string {
	parts := []string{fmt.Sprintf("siblings=%d", s.siblings)}
	if s.checkpointAfterStream {
		parts = append(parts, "root-index after the stream")
	} else {
		parts = append(parts, "root-index at once")
	}
	if s.interval > 0 {
		parts = append(parts, "interval "+s.interval.String())
	}
	return strings.Join(parts, ", ")
}

// forkLifeArm names one shape, and is what SPROUTFS_FORK_ARM selects. The
// default is the fan-out's own shape.
func forkLifeArm(t *testing.T) (string, forkLifeShape) {
	t.Helper()
	shapes := map[string]forkLifeShape{
		"fanout":      {siblings: 2, interval: forkFanOutInterval},
		"late-root":   {siblings: 2, interval: forkFanOutInterval, checkpointAfterStream: true},
		"one-child":   {siblings: 1, interval: forkFanOutInterval},
		"no-interval": {siblings: 2},
	}
	name := os.Getenv("SPROUTFS_FORK_ARM")
	if name == "" {
		name = "fanout"
	}
	shape, found := shapes[name]
	if !found {
		t.Fatalf("SPROUTFS_FORK_ARM=%q is not one of fanout, late-root, one-child, no-interval", name)
	}
	return name, shape
}

// TestFirecrackerForkChildrenSurviveTheirFirstSeconds takes many children of
// one fork point, a few seconds each, and requires every one of their guests to
// still be answering. It is the fan-out suite's failure without the fan-out
// suite's read phase: the same receive, the same resume, the same root-index
// checkpoint over a stream that is still fetching.
func TestFirecrackerForkChildrenSurviveTheirFirstSeconds(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	c, p, pages, point := forkPointFixture(t, ctx, binaryPath)
	arm, shape := forkLifeArm(t)
	lives := forkLives(t)
	t.Logf("fork lives: arm=%s lives=%d shape=[%s]", arm, lives, shape)

	destinationPager := newSizedMigrationPager(t, ctx, forkFanOutArena, forkFanOutRootArena,
		(shape.siblings+1)*(forkFanOutRAM+forkFanOutRoot)+(64<<20), forkFanOutDirty)

	died, lived := 0, 0
	began := time.Now()
	for lived+died < lives {
		trial := (lived + died) / shape.siblings
		deaths := forkLifeTrial(t, ctx, c, destinationPager, binaryPath, pages, point, shape, trial)
		died += deaths
		lived += shape.siblings - deaths
		if ctx.Err() != nil {
			break
		}
	}
	t.Logf("fork lives: arm=%s died=%d of %d in %s", arm, died, lived+died, time.Since(began))
	if died != 0 {
		t.Fatalf("%d of %d children of one fork point died in their first %s [%s]\n%s",
			died, lived+died, forkLifeSpan, shape, consoleText(p))
	}
}

// forkLifeTrial takes one round of children together, gives them their few
// seconds and reports how many of their guests died. A death is the only thing
// it counts: anything else fails the test, because a receive that cannot be
// completed is not a measurement of anything.
func forkLifeTrial(t *testing.T, ctx context.Context, c *migrationCluster, pager *hostPagers,
	binaryPath string, source *vmmigrate.PageSource, point *volume.ForkPoint,
	shape forkLifeShape, trial int) int {
	t.Helper()
	handoffs := make([]vmmigrate.Handoff, 0, shape.siblings)
	for index := range shape.siblings {
		point.Hold()
		id := fmt.Sprintf("life-%d-%d", trial, index)
		handoff, err := vmmigrate.Fork(ctx, id, point, source, vmmigrate.Options{})
		if err != nil {
			t.Fatalf("forking %s: %v", id, err)
		}
		handoffs = append(handoffs, handoff)
	}
	children := make([]*forkedChild, 0, len(handoffs))
	releases := make([]func(), 0, len(handoffs))
	for _, handoff := range handoffs {
		child, release := takeBriefly(t, ctx, c, pager, binaryPath, handoff, source, shape)
		children = append(children, child)
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for _, child := range children {
		if shape.interval > 0 {
			stop := checkpointEvery(t, ctx, child, shape.interval)
			defer stop()
		}
	}
	// The life itself: the children simply run, which is all the fan-out's
	// children were doing when they died.
	select {
	case <-ctx.Done():
		return 0
	case <-time.After(forkLifeSpan):
	}
	died := 0
	for _, child := range children {
		asking, stop := context.WithTimeout(ctx, forkLifeProbe)
		err := guestCommand(asking, child.process, "ram 91\n", "SPROUTFS_RAM ram=91")
		stop()
		if err == nil {
			continue
		}
		console := string(consoleText(child.process))
		if !strings.Contains(console, "Kernel panic - not syncing") {
			t.Fatalf("%s did not answer and its guest kernel did not die: %v\n%s", child.id, err, console)
		}
		died++
		t.Logf("%s died: %s", child.id, panicSignature(console))
	}
	return died
}

// panicSignature is the line of a guest's console that says where its kernel
// died, which is what tells one death from another.
func panicSignature(console string) string {
	for _, line := range strings.Split(console, "\n") {
		if strings.Contains(line, "pc : ") {
			return strings.TrimSpace(line)
		}
	}
	return "no pc line"
}

// takeBriefly receives one child the way a deployment does and leaves it
// running, without waiting for the rest of what the source holds: the fan-out's
// own receive, with the two things a trial varies — when the root index is
// published and whether the background stream runs at all.
func takeBriefly(t *testing.T, ctx context.Context, c *migrationCluster, pager *hostPagers,
	binaryPath string, handoff vmmigrate.Handoff, source *vmmigrate.PageSource,
	shape forkLifeShape) (*forkedChild, func()) {
	t.Helper()
	var process *vmmachine.Process
	start := func(ctx context.Context, vm *volume.VM, backings map[string]vmmemory.Backing,
		state []byte) (vmmigrate.Runtime, error) {
		config := migrationConfig(t, binaryPath, pager, vm)
		config.RestoreState, config.Backings = state, backings
		started, err := vmmachine.Start(ctx, config)
		if err != nil {
			return nil, err
		}
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
	release := func() {
		received.Close()
		if process != nil {
			_ = process.Close()
		}
		_ = source.Release(handoff.VMID)
	}
	if err := received.Done(ctx); err != nil {
		release()
		t.Fatalf("streaming %s from %s: %v", handoff.VMID, handoff.Source, err)
	}
	if shape.checkpointAfterStream {
		if err := received.Streamed(ctx); err != nil {
			release()
			t.Fatalf("the bulk stream of %s stopped early: %v", handoff.VMID, err)
		}
	}
	// The root index, which is what makes a child a VM anything else can open:
	// a seal and a settle over a guest that has been running a fraction of a
	// second, and — unless this arm waits — over a stream still fetching the
	// pages it is sealing.
	child := received.VM()
	ckpt, err := child.Snapshot(ctx, prepareAndResume(process))
	if err != nil {
		release()
		t.Fatalf("publishing the root index of %s: %v\n%s", handoff.VMID, err, consoleText(process))
	}
	if err := ckpt.Wait(ctx); err != nil {
		release()
		t.Fatalf("the root index of %s: %v", handoff.VMID, err)
	}
	return &forkedChild{id: handoff.VMID, vm: child, process: process, received: received}, release
}
