package sim_test

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

const networkDiskBytes = 64 << 20

// newCloud is a cloud with one network disk, shard-0, attached to nothing.
func newCloud(t *testing.T, runtime *sim.Runtime) *sim.NetworkDisks {
	t.Helper()
	cloud := runtime.NewNetworkDisks(sim.NetworkDisksConfig{AttachLatency: 2 * time.Second,
		DetachLatency: time.Second, DescribeLatency: 50 * time.Millisecond})
	if err := cloud.Create(t.Context(), "shard-0", networkDiskBytes); err != nil {
		t.Fatal(err)
	}
	return cloud
}

// A network disk is attached to one machine at a time, opened there by one
// process at a time, and keeps across a move what was synced on the machine
// it left.
func TestANetworkDiskIsAttachedToOneMachineAndOpenedByOneProcess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		cloud := newCloud(t, sim.New(sim.Config{}))
		a, b := cloud.Devices("machine-a"), cloud.Devices("machine-b")
		if _, err := a.Open(ctx, "shard-0"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("opening a disk attached nowhere: %v, want ErrNotFound", err)
		}
		began := time.Now()
		if err := cloud.Attach(ctx, "shard-0", "machine-a"); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(began); took != 2*time.Second {
			t.Fatalf("an attach took %v, want the cloud's 2s", took)
		}
		if err := cloud.Attach(ctx, "shard-0", "machine-a"); err != nil {
			t.Fatalf("attaching a disk where it is attached already: %v, want nothing to do", err)
		}
		if err := cloud.Attach(ctx, "shard-0", "machine-b"); !errors.Is(err, platform.ErrInUse) {
			t.Fatalf("attaching a disk attached elsewhere: %v, want ErrInUse", err)
		}
		if _, err := b.Open(ctx, "shard-0"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("opening a disk on a machine it is not attached to: %v, want ErrNotFound", err)
		}
		described, err := cloud.Describe(ctx, "shard-0")
		if err != nil {
			t.Fatal(err)
		}
		if described.Bytes != networkDiskBytes || !slices.Equal(described.Machines, []string{"machine-a"}) {
			t.Fatalf("the cloud describes %+v, want %d bytes on machine-a", described, networkDiskBytes)
		}
		first, err := a.Open(ctx, "shard-0")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Open(ctx, "shard-0"); !errors.Is(err, platform.ErrLocked) {
			t.Fatalf("a second process opening the device: %v, want ErrLocked", err)
		}
		if size, err := first.Size(ctx); err != nil || size != networkDiskBytes {
			t.Fatalf("the device is %d bytes (%v), want the disk's %d", size, err, networkDiskBytes)
		}
		if err := first.Truncate(ctx, 0); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("truncating a device: %v, want ErrUnsupported", err)
		}
		if _, err := first.WriteAt(ctx, []byte("past"), networkDiskBytes-2); !errors.Is(err, platform.ErrInvalidRange) {
			t.Fatalf("writing past the device's end: %v, want ErrInvalidRange", err)
		}
		synced, unsynced := []byte("synced"), []byte("unsynced")
		if _, err := first.WriteAt(ctx, synced, 0); err != nil {
			t.Fatal(err)
		}
		if err := first.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := first.WriteAt(ctx, unsynced, 4096); err != nil {
			t.Fatal(err)
		}
		if err := cloud.Detach(ctx, "shard-0", "machine-a"); err != nil {
			t.Fatal(err)
		}
		if _, err := first.ReadAt(ctx, make([]byte, 6), 0); err == nil {
			t.Fatal("a handle of a detached disk still reads")
		}
		if err := cloud.Detach(ctx, "shard-0", "machine-a"); err != nil {
			t.Fatalf("detaching a disk from a machine it is not attached to: %v, want nothing to do", err)
		}
		if err := cloud.Attach(ctx, "shard-0", "machine-b"); err != nil {
			t.Fatal(err)
		}
		second, err := b.Open(ctx, "shard-0")
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		read := make([]byte, len(synced))
		if _, err := second.ReadAt(ctx, read, 0); err != nil || !bytes.Equal(read, synced) {
			t.Fatalf("machine-b reads %q (%v), want what machine-a synced", read, err)
		}
		lost := make([]byte, len(unsynced))
		if _, err := second.ReadAt(ctx, lost, 4096); err != nil || !bytes.Equal(lost, make([]byte, len(unsynced))) {
			t.Fatalf("machine-b reads %q (%v) where machine-a wrote and did not sync, want zeroes", lost, err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

// Every site of the cloud fires over a few seeds, and whatever they do, an
// attach that succeeds leaves the disk where it asked, and one refused as in
// use was refused for a disk attached to another machine.
func TestTheCloudsSitesFireAndNeverAttachADiskTwice(t *testing.T) {
	fired := make(map[string]bool)
	for seed := uint64(1); seed <= 24; seed++ {
		synctest.Test(t, func(t *testing.T) {
			ctx := t.Context()
			runtime := sim.New(sim.Config{Seed: seed, Buggify: true})
			cloud := newCloud(t, runtime)
			machines := []string{"machine-a", "machine-b", "machine-c"}
			for step := range 40 {
				machine := machines[step%len(machines)]
				before := cloud.Attached("shard-0")
				switch err := cloud.Attach(ctx, "shard-0", machine); {
				case err == nil:
					if got := cloud.Attached("shard-0"); got != machine {
						t.Fatalf("seed %d: an attach to %s that succeeded left the disk on %q", seed, machine, got)
					}
				case errors.Is(err, platform.ErrInUse):
					if before == "" || before == machine {
						t.Fatalf("seed %d: an attach was refused as in use while on %q", seed, before)
					}
				case errors.Is(err, platform.ErrUnavailable):
				default:
					t.Fatalf("seed %d: attach: %v", seed, err)
				}
				if _, err := cloud.Describe(ctx, "shard-0"); err != nil && !errors.Is(err, platform.ErrUnavailable) {
					t.Fatalf("seed %d: describe: %v", seed, err)
				}
				if step%3 == 2 {
					if err := cloud.Detach(ctx, "shard-0", cloud.Attached("shard-0")); err != nil &&
						!errors.Is(err, platform.ErrUnavailable) {
						t.Fatalf("seed %d: detach: %v", seed, err)
					}
				}
			}
			for site, count := range runtime.FiredSites() {
				if count > 0 {
					fired[site] = true
				}
			}
		})
	}
	for _, site := range sim.NetworkDiskSites() {
		if !fired[site] {
			t.Errorf("site %s never fired", site)
		}
	}
	if t.Failed() {
		t.Log(fmt.Sprint(fired))
	}
}
