package sim_test

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
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
	if err := cloud.Provision(t.Context(), "shard-0", networkDiskBytes); err != nil {
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
		if _, err := first.WriteAt(ctx, []byte("last"), networkDiskBytes-4); err != nil {
			t.Fatalf("writing the device's last bytes: %v", err)
		}
		if err := cloud.Provision(ctx, "empty", 0); !errors.Is(err, platform.ErrInvalidPath) {
			t.Fatalf("making a disk of no bytes: %v, want ErrInvalidPath", err)
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

// A machine that crashes takes every handle its processes held of the disks
// attached to it, and what they had not synced, and the disks stay attached
// there; a disk attached to another machine is untouched.
func TestAMachineThatCrashesKeepsItsDisksAndLosesTheirHandles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		cloud := newCloud(t, sim.New(sim.Config{}))
		if err := cloud.Provision(ctx, "shard-1", networkDiskBytes); err != nil {
			t.Fatal(err)
		}
		if err := cloud.Attach(ctx, "shard-0", "machine-a"); err != nil {
			t.Fatal(err)
		}
		if err := cloud.Attach(ctx, "shard-1", "machine-b"); err != nil {
			t.Fatal(err)
		}
		crashed, err := cloud.Devices("machine-a").Open(ctx, "shard-0")
		if err != nil {
			t.Fatal(err)
		}
		defer crashed.Close()
		other, err := cloud.Devices("machine-b").Open(ctx, "shard-1")
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		if err := cloud.Crash(ctx, "machine-a"); err != nil {
			t.Fatal(err)
		}
		if _, err := crashed.ReadAt(ctx, make([]byte, 8), 0); err == nil {
			t.Fatal("a handle of a crashed machine still reads")
		}
		if _, err := other.ReadAt(ctx, make([]byte, 8), 0); err != nil {
			t.Fatalf("a disk of another machine failed with the crash: %v", err)
		}
		if got := cloud.Attached("shard-0"); got != "machine-a" {
			t.Fatalf("after its machine crashed the disk is attached to %q, want machine-a still", got)
		}
		again, err := cloud.Devices("machine-a").Open(ctx, "shard-0")
		if err != nil {
			t.Fatalf("the machine opening its disk again after the crash: %v", err)
		}
		_ = again.Close()
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
	for _, site := range sim.NetworkDiskAttachSites() {
		if !fired[site] {
			t.Errorf("site %s never fired", site)
		}
	}
	if t.Failed() {
		t.Log(fmt.Sprint(fired))
	}
}

// The cloud makes a disk with its labels, lists disks by label in name order,
// and refuses a second disk of a name it has.
func TestTheCloudMakesDisksAndListsThemByLabel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		cloud := sim.New(sim.Config{}).NewNetworkDisks(sim.NetworkDisksConfig{CreateLatency: 3 * time.Second,
			ListLatency: time.Second})
		if err := cloud.Provision(ctx, "shard-0", networkDiskBytes); err != nil {
			t.Fatal(err)
		}
		east := map[string]string{"sproutfs-journal": "east"}
		began := time.Now()
		for _, spec := range []platform.NetworkDiskSpec{
			{Name: "journal-b", Bytes: networkDiskBytes, Labels: east},
			{Name: "journal-a", Bytes: 2 * networkDiskBytes, Labels: east},
			{Name: "journal-c", Bytes: networkDiskBytes, Labels: map[string]string{"sproutfs-journal": "west"}},
		} {
			if err := cloud.Create(ctx, spec); err != nil {
				t.Fatal(err)
			}
		}
		// Each create also writes the new device's zeroes, which takes the
		// simulated disk a few milliseconds.
		if took := time.Since(began); took.Round(time.Second) != 9*time.Second {
			t.Fatalf("three creates took %v, want the cloud's 3s each", took)
		}
		described, err := cloud.Describe(ctx, "journal-a")
		if err != nil {
			t.Fatal(err)
		}
		if want := (platform.NetworkDisk{Bytes: 2 * networkDiskBytes}); !reflect.DeepEqual(described, want) {
			t.Fatalf("a new disk is described as %+v, want %+v", described, want)
		}
		if err := cloud.Attach(ctx, "journal-b", "machine-a"); err != nil {
			t.Fatal(err)
		}
		listed, err := cloud.List(ctx, "sproutfs-journal", "east")
		if err != nil {
			t.Fatal(err)
		}
		want := []platform.ListedDisk{
			{Name: "journal-a", Bytes: 2 * networkDiskBytes, Labels: east},
			{Name: "journal-b", Bytes: networkDiskBytes, Labels: east, Machines: []string{"machine-a"}},
		}
		if !reflect.DeepEqual(listed, want) {
			t.Fatalf("the cloud lists %+v, want %+v", listed, want)
		}
		listed, err = cloud.List(ctx, "sproutfs-journal", "north")
		if err != nil {
			t.Fatal(err)
		}
		if listed != nil {
			t.Fatalf("listing a label no disk has: %+v, want nothing", listed)
		}
		again := platform.NetworkDiskSpec{Name: "journal-c", Bytes: networkDiskBytes}
		if err := cloud.Create(ctx, again); !errors.Is(err, platform.ErrAlreadyExists) {
			t.Fatalf("creating a disk of a name the cloud has: %v, want ErrAlreadyExists", err)
		}
		if err := cloud.Create(ctx, platform.NetworkDiskSpec{Name: "journal-d"}); !errors.Is(err, platform.ErrInvalidPath) {
			t.Fatalf("creating a disk of no bytes: %v, want ErrInvalidPath", err)
		}
	})
}

// The cloud deletes a disk attached to nothing, refuses one attached to a
// machine, and makes a new disk of a deleted one's name with nothing on it.
func TestTheCloudDeletesOnlyADiskAttachedToNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		cloud := sim.New(sim.Config{}).NewNetworkDisks(sim.NetworkDisksConfig{DeleteLatency: 2 * time.Second})
		spec := platform.NetworkDiskSpec{Name: "journal-a", Bytes: networkDiskBytes,
			Labels: map[string]string{"sproutfs-journal": "east"}}
		if err := cloud.Create(ctx, spec); err != nil {
			t.Fatal(err)
		}
		if err := cloud.Attach(ctx, "journal-a", "machine-a"); err != nil {
			t.Fatal(err)
		}
		old, err := cloud.Devices("machine-a").Open(ctx, "journal-a")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.WriteAt(ctx, []byte("entry"), 0); err != nil {
			t.Fatal(err)
		}
		if err := old.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if err := old.Close(); err != nil {
			t.Fatal(err)
		}
		if err := cloud.Delete(ctx, "journal-a"); !errors.Is(err, platform.ErrInUse) {
			t.Fatalf("deleting an attached disk: %v, want ErrInUse", err)
		}
		if err := cloud.Detach(ctx, "journal-a", "machine-a"); err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		if err := cloud.Delete(ctx, "journal-a"); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(began); took != 2*time.Second {
			t.Fatalf("a delete took %v, want the cloud's 2s", took)
		}
		if _, err := cloud.Describe(ctx, "journal-a"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("describing a deleted disk: %v, want ErrNotFound", err)
		}
		if err := cloud.Delete(ctx, "journal-a"); !errors.Is(err, platform.ErrNotFound) {
			t.Fatalf("deleting a disk the cloud does not have: %v, want ErrNotFound", err)
		}
		listed, err := cloud.List(ctx, "sproutfs-journal", "east")
		if err != nil {
			t.Fatal(err)
		}
		if listed != nil {
			t.Fatalf("the cloud lists %+v after the delete, want nothing", listed)
		}
		if err := cloud.Create(ctx, spec); err != nil {
			t.Fatalf("creating a disk of a deleted one's name: %v", err)
		}
		if err := cloud.Attach(ctx, "journal-a", "machine-b"); err != nil {
			t.Fatal(err)
		}
		fresh, err := cloud.Devices("machine-b").Open(ctx, "journal-a")
		if err != nil {
			t.Fatal(err)
		}
		defer fresh.Close()
		read := make([]byte, 5)
		if _, err := fresh.ReadAt(ctx, read, 0); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(read, make([]byte, 5)) {
			t.Fatalf("the new disk reads %q, want zeroes", read)
		}
	})
}

// Every create and delete site fires over a few seeds, and whatever they do, a
// create or a delete that fails leaves the cloud as it was, and one that
// succeeds did what it asked.
func TestTheCloudsCreateAndDeleteSitesFire(t *testing.T) {
	fired := make(map[string]bool)
	for seed := uint64(1); seed <= 24; seed++ {
		synctest.Test(t, func(t *testing.T) {
			ctx := t.Context()
			runtime := sim.New(sim.Config{Seed: seed, Buggify: true})
			cloud := runtime.NewNetworkDisks(sim.NetworkDisksConfig{CreateLatency: time.Second,
				DeleteLatency: time.Second})
			spec := platform.NetworkDiskSpec{Name: "journal-a", Bytes: networkDiskBytes}
			for step := range 40 {
				had := cloud.Disk("journal-a") != nil
				var err error
				if step%2 == 0 {
					err = cloud.Create(ctx, spec)
				} else {
					err = cloud.Delete(ctx, "journal-a")
				}
				has := cloud.Disk("journal-a") != nil
				switch {
				case err == nil && has == had:
					t.Fatalf("seed %d step %d: succeeded and left the disk there: %v", seed, step, has)
				case err != nil && has != had:
					t.Fatalf("seed %d step %d: failed with %v and changed whether the disk is there", seed, step, err)
				case err == nil, errors.Is(err, platform.ErrUnavailable):
				case errors.Is(err, platform.ErrAlreadyExists) && had, errors.Is(err, platform.ErrNotFound) && !had:
				default:
					t.Fatalf("seed %d step %d: %v", seed, step, err)
				}
			}
			for site, count := range runtime.FiredSites() {
				if count > 0 {
					fired[site] = true
				}
			}
		})
	}
	for _, site := range sim.NetworkDiskLifecycleSites() {
		if !fired[site] {
			t.Errorf("site %s never fired", site)
		}
	}
	if t.Failed() {
		t.Log(fmt.Sprint(fired))
	}
}
