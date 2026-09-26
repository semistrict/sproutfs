package simtest_test

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/host"
	"github.com/semistrict/sproutfs/internal/handover"
	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/volume"
)

// retryWorld is a deployment of hosts with one VM on host-0 whose guest holds
// pages no checkpoint has: exactly what a receive that is given up loses, and
// what one that is retried keeps.
func retryWorld(t *testing.T, ctx context.Context, runtime *sim.Runtime, hosts int) *simtest.World {
	t.Helper()
	prefix, err := platform.NewObjectPrefix("sproutfs/")
	if err != nil {
		t.Fatal(err)
	}
	topology := simtest.Topology{VMs: []simtest.VMSpec{{ID: "vm-1", Host: 0, Volumes: []volume.VolumeSpec{
		{Name: "ram0", Size: 4 * simtest.RAMPage, PageSize: simtest.RAMPage},
		{Name: "disk", Size: 2 * simtest.PMEMPage, PageSize: simtest.PMEMPage}}}}}
	for index := range hosts {
		topology.Hosts = append(topology.Hosts, fmt.Sprintf("host-%d", index))
	}
	k := knobs.Defaults()
	k.ResidentPages, k.DirtyPages, k.LogicalPages = 32, 32, 128
	k.ReadAheadPages, k.WriteAheadPages = 1, 1
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
		Knobs: k, Prefix: prefix, Log: t.Logf})
	choose := func(int) int { return 0 }
	if err := world.Store(ctx, "vm-1", 4, choose); err != nil {
		t.Fatal(err)
	}
	if err := world.Checkpoint(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	if err := world.Store(ctx, "vm-1", 2, choose); err != nil {
		t.Fatal(err)
	}
	return world
}

func retryRuntime(seed uint64) *sim.Runtime {
	return sim.New(sim.Config{Seed: seed,
		Network: sim.NetworkConfig{Latency: time.Microsecond, Jitter: time.Nanosecond,
			ConnectLatency: time.Microsecond},
		ObjectStore: sim.ObjectStoreConfig{HeadLatency: time.Microsecond, GetLatency: time.Microsecond,
			PutLatency: time.Microsecond, DeleteLatency: time.Microsecond, ListLatency: time.Microsecond,
			BytesPerSecond: 1 << 40}})
}

// requireIntact is what a handover owes a guest that was handed over rather
// than taken over: every byte it wrote, through its mappings and, once it
// checkpoints, through its volumes.
func requireIntact(t *testing.T, ctx context.Context, world *simtest.World) {
	t.Helper()
	if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
		t.Error(err)
	}
	if err := world.Checkpoint(ctx, "vm-1"); err != nil {
		t.Fatal(err)
	}
	if err := world.VerifyDurable(ctx, "vm-1"); err != nil {
		t.Error(err)
	}
	if err := world.CheckSelected(ctx); err != nil {
		t.Error(err)
	}
	if err := world.Close(ctx); err != nil {
		t.Error(err)
	}
}

// A destination that cannot reach the object store fails its receive: it
// cannot read the control record. The source still holds the pages, so the
// receive is tried again, and once the link is back the guest is handed over
// with every write it made.
func TestAReceiveIsRetriedUntilItsDestinationReachesTheStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := retryRuntime(3)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := retryWorld(t, ctx, runtime, 2)
		const outage = 10 * time.Second
		began := time.Now()
		runtime.Network().Clog(world.Address(1), simtest.StoreAddress, began.Add(outage))
		took := world.Takeovers()
		if err := world.Migrate(ctx, "vm-1", 1); err != nil {
			t.Fatal(err)
		}
		if at := world.HostOf("vm-1"); at != 1 {
			t.Fatalf("the VM is on host %d after the handover, want host-1", at)
		}
		if world.Takeovers() != took {
			t.Fatal("the VM was taken over rather than handed over")
		}
		if waited := time.Since(began); waited < outage {
			t.Fatalf("the handover finished %s in, inside the %s the destination could not reach the store", waited, outage)
		}
		requireIntact(t, ctx, world)
	})
}

// A destination that keeps refusing gets two attempts. The handoff then goes
// to another host, and the guest keeps every write it made.
func TestAHandoffADestinationKeepsRefusingGoesToAnother(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := retryRuntime(5)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := retryWorld(t, ctx, runtime, 3)
		refusal := simtest.RefusedStart(1)
		if err := refusal.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		took := world.Takeovers()
		if err := world.Migrate(ctx, "vm-1", 1); err != nil {
			t.Fatal(err)
		}
		if err := refusal.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if at := world.HostOf("vm-1"); at != 2 {
			t.Fatalf("the VM is on host %d after the handover, want host-2", at)
		}
		if world.Takeovers() != took {
			t.Fatal("the VM was taken over rather than handed over")
		}
		requireIntact(t, ctx, world)
	})
}

// A receive whose caller hung up goes on where it was sent, and its host
// reports it in flight. No receive goes anywhere while it does, so the handover
// ends where that receive takes the VM in, with one guest started for it.
func TestAReceiveThatOutlivesItsCallerStartsNoSecondGuest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := retryRuntime(11)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := retryWorld(t, ctx, runtime, 3)
		// Longer than the policy's first waits put together, so a retry that did
		// not wait for the receive reaches another host while it is starting.
		const start = 20 * time.Second
		outlived := simtest.OutlivedReceive(1, start)
		if err := outlived.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		took := world.Takeovers()
		began := time.Now()
		if err := world.Migrate(ctx, "vm-1", 1); err != nil {
			t.Fatal(err)
		}
		if err := outlived.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if at := world.HostOf("vm-1"); at != 1 {
			t.Fatalf("the VM is on host %d after the handover, want host-1, where the receive went on", at)
		}
		if started := world.ReceivedGuests("vm-1"); started != 1 {
			t.Fatalf("the handover started %d guests of the VM, want one", started)
		}
		if world.Takeovers() != took {
			t.Fatal("the VM was taken over rather than handed over")
		}
		if waited := time.Since(began); waited < start {
			t.Fatalf("the handover finished %s in, before the receive that outlived its caller started its guest", waited)
		}
		requireIntact(t, ctx, world)
	})
}

// A handoff no destination can take is given up only when the source stops
// holding it. Until then every retry could still have kept the guest's writes.
// After it the VM comes back at its checkpoint, which is all a handover that
// failed can owe it.
func TestAHandoffIsGivenUpOnlyWhenItsSourceStopsHoldingIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := retryRuntime(7)
		ctx := sim.WithRuntime(t.Context(), runtime)
		world := retryWorld(t, ctx, runtime, 2)
		refusal := simtest.RefusedStart(1)
		if err := refusal.Begin(ctx, world); err != nil {
			t.Fatal(err)
		}
		took := world.Takeovers()
		began := time.Now()
		if err := world.Migrate(ctx, "vm-1", 1); err != nil {
			t.Fatal(err)
		}
		// The hosts run no checkpoint loop, so each holds a handover for four
		// default intervals. The last look is at most one pause short of that.
		hold := 4 * host.DefaultCheckpointInterval
		if waited := time.Since(began); waited < hold-handover.Default.MaxPause {
			t.Fatalf("the handoff was given up %s in, while its source held it for %s", waited, hold)
		}
		if err := refusal.End(ctx, world); err != nil {
			t.Fatal(err)
		}
		if err := world.Settle(ctx); err != nil {
			t.Fatal(err)
		}
		if world.Takeovers() != took+1 {
			t.Fatalf("the VM was taken over %d times, want once", world.Takeovers()-took)
		}
		if err := world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
			t.Error(err)
		}
		if err := world.VerifyDurable(ctx, "vm-1"); err != nil {
			t.Error(err)
		}
		if err := world.CheckSelected(ctx); err != nil {
			t.Error(err)
		}
		if err := world.Close(ctx); err != nil {
			t.Error(err)
		}
	})
}
