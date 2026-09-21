package simtest_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/volume"
)

// scenarioWindow is the window every scenario here runs with, and the interval
// its hosts checkpoint on. They are equal because what the guarantee allows
// beyond the window is one checkpoint attempt, and an interval far from the
// window would make that allowance say nothing.
const scenarioWindow = time.Minute

// lossWindowWorld is two hosts and one VM, with the checkpoint loop on. Every
// other world here drives its own checkpoints; these do not, because the
// checkpoint a store held back by the window waits for is one only a loop
// takes — a pager asking a host with no loop is told no, and the guest is
// stopped instead.
func lossWindowWorld(t *testing.T, runtime *sim.Runtime, prefix string, window time.Duration) *simtest.World {
	t.Helper()
	topology := simtest.Topology{Hosts: []string{"host-0", "host-1"},
		VMs: []simtest.VMSpec{{ID: "vm-0", Host: 0,
			Volumes: []volume.VolumeSpec{{Name: simtest.MemoryVolume, Size: 8 * simtest.RAMPage, PageSize: simtest.RAMPage}}}}}
	k := campaignKnobs(t, runtime, topology)
	k.LossWindow, k.CheckpointInterval = window, scenarioWindow
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	return simtest.MustStart(t, sim.WithRuntime(t.Context(), runtime), simtest.Config{
		Runtime: runtime, Topology: topology, Knobs: k, CheckpointInterval: scenarioWindow,
		Prefix: newPrefix(t, prefix), Log: t.Logf})
}

// A guest whose writes have gone unpublished for longer than the loss window is
// stopped from making more of them: its next store waits in the pager until a
// checkpoint of its VM lands. That is what turns the window from a description
// of what a host loss usually costs into a bound on what it can cost.
func TestAGuestPastTheLossWindowWaitsForItsCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(11, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := lossWindowWorld(t, runtime, "loss-window/", scenarioWindow)

		// From here nothing this host publishes lands: every checkpoint of this
		// VM is one that did not happen, which is the situation the window
		// exists for.
		world.LoseStore(0, true)
		inside := world.StoreInBackground(ctx, "vm-0", 0)
		synctest.Wait()
		if err := <-inside; err != nil {
			t.Fatalf("a store inside the window failed: %v", err)
		}

		world.Advance(2 * scenarioWindow)
		waiting := world.StoreInBackground(ctx, "vm-0", 1)
		synctest.Wait()
		select {
		case err := <-waiting:
			t.Fatalf("a store past the loss window did not wait: %v", err)
		default:
		}

		// The store answers again, so the checkpoint the pager has been asking
		// for lands — and the store it was holding back lands with it.
		world.LoseStore(0, false)
		world.Advance(scenarioWindow)
		synctest.Wait()
		if err := <-waiting; err != nil {
			t.Fatalf("the store failed after the checkpoint that ended the window: %v", err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A window of zero is a deployment that would rather lose writes than ever hold
// a guest back. Its stores go through however long its checkpoints have been
// failing, which is exactly what the bound is there to prevent and exactly what
// turning it off has to mean.
func TestAGuestWithNoLossWindowNeverWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(12, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := lossWindowWorld(t, runtime, "no-window/", 0)

		world.LoseStore(0, true)
		world.Advance(10 * scenarioWindow)
		for page := range uint64(4) {
			stored := world.StoreInBackground(ctx, "vm-0", page)
			synctest.Wait()
			select {
			case err := <-stored:
				if err != nil {
					t.Fatalf("a store with no loss window failed: %v", err)
				}
			default:
				t.Fatalf("a store waited on page %d with the loss window disabled", page)
			}
		}
		world.LoseStore(0, false)
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// A migration moves a VM's unpublished pages to another host, and their age
// moves with them: the destination measures the window from the guest's own
// writes rather than from its arrival. Without that a VM migrated more often
// than the window would never reach one, and the bound would be one a
// deployment could lose simply by moving its VMs about.
func TestAMigratedGuestInheritsItsSourcesLossWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(13, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := lossWindowWorld(t, runtime, "migrated-window/", scenarioWindow)

		world.LoseStore(0, true)
		stored := world.StoreInBackground(ctx, "vm-0", 0)
		synctest.Wait()
		if err := <-stored; err != nil {
			t.Fatal(err)
		}
		// Most of the window is spent on the source, and the pages the
		// destination fetches are that old when they arrive.
		world.Advance(3 * scenarioWindow / 4)
		world.LoseStore(0, false)
		if err := world.Migrate(ctx, "vm-0", 1); err != nil {
			t.Fatal(err)
		}
		if at := world.HostOf("vm-0"); at != 1 {
			t.Fatalf("the migrated VM is on host-%d, want the destination", at)
		}

		// The destination cannot publish either, so the rest of the window runs
		// out here — a quarter of it, not a whole one.
		world.LoseStore(1, true)
		world.Advance(scenarioWindow / 2)
		waiting := world.StoreInBackground(ctx, "vm-0", 1)
		synctest.Wait()
		select {
		case err := <-waiting:
			t.Fatalf("a store past a window inherited from the source did not wait: %v", err)
		default:
		}
		world.LoseStore(1, false)
		world.Advance(scenarioWindow)
		synctest.Wait()
		if err := <-waiting; err != nil {
			t.Fatalf("the store failed after the destination's checkpoint landed: %v", err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// The guarantee itself: losing a host costs one VM at most a window's worth of
// its guest's writes, because the guest was stopped from making more of them
// from the window on. The host here can never publish, so what it loses is
// everything since the checkpoint before the outage — and the window is what
// says how much of the guest's work that is.
func TestALostHostRewindsAtMostTheLossWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := newCampaignRuntime(14, false)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := lossWindowWorld(t, runtime, "rewound/", scenarioWindow)

		world.LoseStore(0, true)
		// The guest keeps writing for far longer than the window. Each store
		// goes into a page of its own, so each needs a reservation and each is
		// one the window can hold back.
		//
		// The store the window holds back is given up rather than left waiting:
		// a simulated guest's faults outlive the host, where a real guest's die
		// with the VMM the kill takes away.
		storing, giveUp := context.WithCancel(ctx)
		defer giveUp()
		var held <-chan error
		for page := range uint64(6) {
			stored := world.StoreInBackground(storing, "vm-0", page)
			synctest.Wait()
			select {
			case err := <-stored:
				if err != nil {
					t.Fatalf("a store before the window expired failed: %v", err)
				}
			default:
				held = stored
			}
			if held != nil {
				break
			}
			world.Advance(scenarioWindow / 2)
		}
		if held == nil {
			t.Fatal("the guest stored past the loss window for six pages without ever waiting")
		}

		giveUp()
		synctest.Wait()
		<-held
		// The host goes, taking every page it held with it. What comes back is
		// the checkpoint the VM's record selects, and the writes since it are
		// the ones the window bounds.
		if err := world.LoseHost(ctx, 0); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if err := world.VerifyLossWindow(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.VerifyDurable(ctx, "vm-0"); err != nil {
			t.Fatal(err)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Fatal(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
}
