package sim_test

import (
	"bytes"
	"maps"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// Allocating a range grows the file to cover it, keeps what was written, and
// reads as zeroes where nothing was. A power loss before a sync takes the
// allocation back with the rest.
func TestDiskAllocateGrowsAFileThatReadsZeroes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		disk := runtime.NewDisk("node-1", sim.DiskConfig{})
		file, err := disk.Open(t.Context(), "file", platform.OpenOptions{Create: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt(t.Context(), []byte("kept"), 0); err != nil {
			t.Fatal(err)
		}
		if err := file.Sync(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := file.(platform.AllocatingFile).Allocate(t.Context(), 4096, 8192); err != nil {
			t.Fatal(err)
		}
		if size, err := file.Size(t.Context()); err != nil || size != 12288 {
			t.Fatalf("the allocated file is %d bytes, %v; want 12288", size, err)
		}
		got := make([]byte, 12288)
		if _, err := file.ReadAt(t.Context(), got, 0); err != nil {
			t.Fatal(err)
		}
		want := make([]byte, 12288)
		copy(want, "kept")
		if !bytes.Equal(got, want) {
			t.Fatal("the allocated file does not read as what was written and zeroes")
		}
		if err := disk.PowerLoss(t.Context()); err != nil {
			t.Fatal(err)
		}
		reopened, err := disk.Open(t.Context(), "file", platform.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if size, err := reopened.Size(t.Context()); err != nil || size != 4 {
			t.Fatalf("after a power loss the file is %d bytes, %v; want the 4 synced", size, err)
		}
	})
}

// A disk that asks for read chaos lies to its reads, under Buggify, in the
// three ways its sites name: a read that flips one bit, a read misdirected to
// the start of another write, and a read held longer. A disk that does not ask
// is never touched, and neither is anything while Buggify is off.
func TestDiskReadChaosLiesOnlyOnADiskThatAsks(t *testing.T) {
	for _, test := range []struct {
		name    string
		chaos   bool
		buggify bool
		lies    int
		fired   map[string]uint64
	}{
		{"chaos under buggify", true, true, 19, map[string]uint64{sim.BuggifyDiskReadBitFlip: 15,
			sim.BuggifyDiskMisdirectsRead: 4, sim.BuggifyDiskSlowRead: 12}},
		{"no chaos under buggify", false, true, 0, map[string]uint64{}},
		{"chaos without buggify", true, false, 0, map[string]uint64{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := sim.New(sim.Config{Seed: chaosSeed, Buggify: test.buggify})
				disk := runtime.NewDisk("node-1", sim.DiskConfig{ReadChaos: test.chaos})
				file, err := disk.Open(t.Context(), "file", platform.OpenOptions{Create: true})
				if err != nil {
					t.Fatal(err)
				}
				// Sixteen writes of distinct bytes, each 512 bytes long.
				for block := range 16 {
					if _, err := file.WriteAt(t.Context(), bytes.Repeat([]byte{byte(block + 1)}, 512),
						int64(block*512)); err != nil {
						t.Fatal(err)
					}
				}
				lies := 0
				for read := range 200 {
					block := read % 16
					got := make([]byte, 512)
					if _, err := file.ReadAt(t.Context(), got, int64(block*512)); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, bytes.Repeat([]byte{byte(block + 1)}, 512)) {
						lies++
					}
				}
				fired := runtime.FiredSites()
				if fired == nil {
					fired = map[string]uint64{}
				}
				if lies != test.lies || !maps.Equal(fired, test.fired) {
					t.Fatalf("the disk lied to %d of 200 reads and fired %v, want %d and %v", lies, fired, test.lies,
						test.fired)
				}
			})
		})
	}
}

// chaosSeed is a seed that activates all three read chaos sites.
const chaosSeed = 133
