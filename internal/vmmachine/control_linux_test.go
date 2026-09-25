//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmachine"
)

// waitForFile waits for the fixture's VMM to announce that it is inside the
// request a test wants to interrupt.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the VMM never reached %s", filepath.Base(path))
		}
		time.Sleep(time.Millisecond)
	}
}

// TestACallerGoingAwayDoesNotKillTheGuest. A capture is this process's
// operation, not the caller's: the caller is an HTTP handler whose client may
// disconnect, a drain whose deadline may pass, a migration that gave up. A
// control request cancelled halfway leaves the VMM's state unknown, and the only
// thing this side can do with unknown is kill the process, which loses every
// guest write since the last checkpoint that landed. So the caller's context
// bounds the wait for this process's lock and nothing after it.
func TestACallerGoingAwayDoesNotKillTheGuest(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "pause")
	t.Setenv("SPROUTFS_STARTUP_CHILD_PAUSE_GATE", gate)
	config, _, _ := startupFixture(t)
	p, err := vmmachine.Start(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for name, memoryRegion := range p.MemoryRegions() {
		if err := memoryRegion.Seal(t.Context()); err != nil {
			t.Fatalf("sealing %s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := p.Prepare(ctx)
		done <- err
	}()
	// The VMM is inside the pause; the caller gives up there and the VMM
	// answers afterwards, which is the whole race.
	waitForFile(t, gate+".entered")
	cancel()
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("a capture whose caller went away: %v", err)
	}
	alive, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	if err := p.Wait(alive); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a caller going away ended the VMM: %v", err)
	}
	if err := p.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestARefusedPauseLeavesTheVMMRunning. A pause the VMM answers and refuses is
// a capture that did not happen: the VMM is running, its vCPUs are where they
// were, and nothing about this process is in doubt. Killing it there ends a
// guest over a checkpoint that was merely refused.
func TestARefusedPauseLeavesTheVMMRunning(t *testing.T) {
	t.Setenv("SPROUTFS_STARTUP_CHILD_REFUSE_PAUSE", "1")
	config, _, _ := startupFixture(t)
	p, err := vmmachine.Start(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, _, err := p.Prepare(t.Context()); err == nil {
		t.Fatal("the VMM accepted a pause it was told to refuse")
	}
	alive, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	if err := p.Wait(alive); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a refused pause ended the VMM: %v", err)
	}
}

// failingDisks refuses to open the staging namespace of a VM's directory, which
// is what a full or read-only scratch filesystem does at the same point.
func failingDisks(string) (platform.Disk, error) {
	return nil, errors.New("no staging namespace")
}

// TestAFailedStagingOpenLeavesTheScratchClosable. Taking a directory from the
// shared scratch registers the process with it, and the scratch refuses to close
// while any process it registered has not stopped. A start that fails after
// that point and never goes through Close leaves an owner no host can ever shut
// down.
func TestAFailedStagingOpenLeavesTheScratchClosable(t *testing.T) {
	config, _, _ := startupFixture(t)
	scratch, err := vmmachine.NewScratch(t.Context(), t.TempDir(), failingDisks)
	if err != nil {
		t.Fatal(err)
	}
	config.Scratch = scratch
	if _, err := vmmachine.Start(t.Context(), config); err == nil {
		t.Fatal("a machine started without a staging namespace")
	}
	if err := scratch.Close(); err != nil {
		t.Fatalf("closing a scratch whose only start failed: %v", err)
	}
}

// TestStartGivesUpOnAVMMThatNeverBindsItsAPI. A process that neither binds its
// API socket nor exits is never going to finish starting, and a caller with no
// deadline of its own would wait on it for ever while its scratch directory and
// its pager capacity stay taken.
func TestStartGivesUpOnAVMMThatNeverBindsItsAPI(t *testing.T) {
	config, _, _ := startupFixture(t)
	defer vmmachine.SetAPIDeadline(200 * time.Millisecond)()
	silent := filepath.Join(t.TempDir(), "silent")
	if err := os.WriteFile(silent, []byte("#!/bin/sh\nexec sleep 300\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config.Binary = silent
	start := time.Now()
	if _, err := vmmachine.Start(t.Context(), config); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("starting a VMM that never binds its API socket = %v", err)
	}
	if waited := time.Since(start); waited > 30*time.Second {
		t.Fatalf("the start waited %s on a VMM that was never going to answer", waited)
	}
}
