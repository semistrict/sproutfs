package sim_test

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

const (
	spaceTotal   = 1 << 30
	spaceOutside = 100 << 20
)

func space(t *testing.T, disk *sim.Disk) platform.FilesystemSpace {
	t.Helper()
	reading, err := disk.Space(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return reading
}

// A disk's space is its filesystem's size less what other writers hold and
// what its own files hold. A sparse file holds only what was written to it.
func TestADiskReportsWhatItsFilesAndOtherWritersHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node-1", sim.DiskConfig{
			Space: sim.SpaceConfig{TotalBytes: spaceTotal, OutsideBytes: spaceOutside}})
		if got := space(t, disk); got.Total != spaceTotal || got.Available != spaceTotal-spaceOutside {
			t.Fatalf("an empty disk reports %+v, want %d of %d available", got, spaceTotal-spaceOutside, spaceTotal)
		}
		spill, err := disk.Open(t.Context(), "spill", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := spill.Truncate(t.Context(), 512<<20); err != nil {
			t.Fatal(err)
		}
		if got := space(t, disk).Available; got != spaceTotal-spaceOutside {
			t.Fatalf("a sparse 512 MiB file left %d available, want %d", got, spaceTotal-spaceOutside)
		}
		if _, err := spill.WriteAt(t.Context(), make([]byte, 1<<20), 4096*3); err != nil {
			t.Fatal(err)
		}
		if got := space(t, disk).Available; got != spaceTotal-spaceOutside-1<<20 {
			t.Fatalf("1 MiB written left %d available, want %d", got, spaceTotal-spaceOutside-1<<20)
		}
		allocated, err := spill.(platform.FileAllocation).Allocated(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if allocated != 1<<20 {
			t.Fatalf("the file holds %d bytes, want 1 MiB", allocated)
		}
		disk.SetOutsideBytes(900 << 20)
		if got := space(t, disk).Available; got != spaceTotal-900<<20-1<<20 {
			t.Fatalf("another writer holding 900 MiB left %d available, want %d", got, spaceTotal-900<<20-1<<20)
		}
		want := sim.SpaceUsage{TotalBytes: spaceTotal, OutsideBytes: 900 << 20, HostBytes: 1 << 20}
		if got := disk.Usage(); got != want {
			t.Fatalf("the disk's usage is %+v, want %+v", got, want)
		}
		disk.SetOutsideBytes(0)
		if got := space(t, disk).Available; got != spaceTotal-1<<20 {
			t.Fatalf("the other writer gone left %d available, want %d", got, spaceTotal-1<<20)
		}
	})
}

// A write that needs more than is free is refused whole, and a write into
// pages the file already holds needs nothing.
func TestAWriteThatDoesNotFitIsRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node-1", sim.DiskConfig{
			Space: sim.SpaceConfig{TotalBytes: 64 << 10, OutsideBytes: 48 << 10}})
		file, err := disk.Open(t.Context(), "cache", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt(t.Context(), make([]byte, 16<<10), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt(t.Context(), []byte{1}, 16<<10); !errors.Is(err, platform.ErrNoSpace) {
			t.Fatalf("a write into a full disk returned %v, want %v", err, platform.ErrNoSpace)
		}
		if _, err := file.WriteAt(t.Context(), []byte{1}, 8<<10); err != nil {
			t.Fatalf("a write into a held page returned %v, want none", err)
		}
		if got := disk.Usage().HostBytes; got != 16<<10 {
			t.Fatalf("the refused write left the files holding %d bytes, want %d", got, 16<<10)
		}
	})
}

// The device counts every byte written to it, by this disk's files and by
// other writers.
func TestADeviceCountsTheBytesWrittenToIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node-1", sim.DiskConfig{Space: sim.SpaceConfig{TotalBytes: spaceTotal}})
		file, err := disk.Open(t.Context(), "cache", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if _, err := file.WriteAt(t.Context(), make([]byte, 5000), 0); err != nil {
				t.Fatal(err)
			}
		}
		disk.AddDeviceWrites(7)
		written, err := disk.BytesWritten(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if written != 15007 {
			t.Fatalf("the device counted %d bytes, want 15007", written)
		}
	})
}

// Other writers drift between readings by at most the configured rate over
// the time between them, and a seed draws the same drift every time.
func TestOtherWritersDriftBoundedAndSeeded(t *testing.T) {
	drift := func() []uint64 {
		var readings []uint64
		synctest.Test(t, func(t *testing.T) {
			runtime := sim.New(sim.Config{Seed: 7})
			disk := runtime.NewDisk("node-1", sim.DiskConfig{Space: sim.SpaceConfig{
				TotalBytes: spaceTotal, OutsideBytes: spaceOutside, DriftBytesPerSecond: 1 << 20}})
			previous := space(t, disk).Available
			for range 20 {
				time.Sleep(time.Second)
				available := space(t, disk).Available
				moved := int64(available) - int64(previous)
				// One second of drift, and the latency of the reading itself.
				if moved > 1<<20+1<<10 || moved < -(1<<20+1<<10) {
					t.Fatalf("one second moved the free space by %d bytes, want at most %d", moved, 1<<20)
				}
				readings = append(readings, available)
				previous = available
			}
		})
		return readings
	}
	first, second := drift(), drift()
	if len(first) != 20 || len(second) != 20 {
		t.Fatalf("took %d and %d readings, want 20 each", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("reading %d was %d and then %d under the same seed", i, first[i], second[i])
		}
	}
	if first[0] == first[19] {
		t.Fatalf("twenty seconds of drift left the free space at %d, want it moved", first[0])
	}
}

// A disk whose test names no size gets one drawn from the seed, with at least
// 5 GB or 7.5 % of it free.
func TestADiskWithNoSizeIsDrawnFromTheSeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 3})
		for _, id := range []string{"node-1", "node-2", "node-3", "node-4"} {
			usage := runtime.NewDisk(id, sim.DiskConfig{}).Usage()
			if usage.TotalBytes < 5_000_000_000 || usage.TotalBytes > 105_000_000_000 {
				t.Fatalf("%s drew %d bytes, want 5 GB to 105 GB", id, usage.TotalBytes)
			}
			if free := usage.FreeBytes(); free < 5_000_000_000 || float64(free) < 0.075*float64(usage.TotalBytes) {
				t.Fatalf("%s drew %d free of %d, want at least 5 GB and 7.5 %%", id, free, usage.TotalBytes)
			}
		}
	})
}
