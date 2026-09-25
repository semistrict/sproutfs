package sim_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// faultyDisk is a disk that resolves unsynced writes rather than discarding
// them, which is the only configuration under which any of this happens.
func faultyDisk(t *testing.T, runtime *sim.Runtime, id string) *sim.Disk {
	t.Helper()
	return runtime.NewDisk(id, sim.DiskConfig{PowerLossFaults: true})
}

// writeRecord fills name with a recognisable pattern, syncing first so the file
// exists durably and only the second write is at risk.
func writeRecord(t *testing.T, disk *sim.Disk, name string, size int) []byte {
	t.Helper()
	file, err := disk.Open(t.Context(), name, platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	base := make([]byte, size)
	if _, err := file.WriteAt(t.Context(), base, 0); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	record := make([]byte, size)
	for i := range record {
		record[i] = byte(1 + i%251)
	}
	if _, err := file.WriteAt(t.Context(), record, 0); err != nil {
		t.Fatal(err)
	}
	return record
}

func readBack(t *testing.T, disk *sim.Disk, name string, size int) []byte {
	t.Helper()
	file, err := disk.Open(t.Context(), name, platform.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got := make([]byte, size)
	// A dropped trailing write leaves the file shorter than the record, which
	// ReadAt reports as io.EOF with the bytes it did have. That short image is
	// exactly what the caller is here to inspect.
	if _, err := file.ReadAt(t.Context(), got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return got
}

// A write that was never synced comes back applied, lost, torn or garbled, and
// which of those it is depends on the seed alone. Over a span of seeds every
// one of the four outcomes has to appear: a disk that only ever restores its
// last sync is the assumption this primitive exists to remove.
func TestPowerLossResolvesUnsyncedWritesIntoEveryOutcome(t *testing.T) {
	const size = 4 * 4096
	outcomes := map[string]int{}
	synctest.Test(t, func(t *testing.T) {
		for seed := uint64(1); seed <= 64; seed++ {
			runtime := sim.New(sim.Config{Seed: seed})
			disk := faultyDisk(t, runtime, "node")
			record := writeRecord(t, disk, "record", size)
			if err := disk.PowerLoss(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := readBack(t, disk, "record", size)
			zeros := make([]byte, size)
			switch {
			case bytes.Equal(got, record):
				outcomes["applied"]++
			case bytes.Equal(got, zeros):
				outcomes["dropped"]++
			default:
				// Whatever is not the record is either a prefix of it, with the
				// rest still holding the synced zeroes, or bytes nobody wrote.
				garbled := false
				for i := range got {
					if got[i] != record[i] && got[i] != 0 {
						garbled = true
					}
				}
				if garbled {
					outcomes["garbled"]++
				} else {
					outcomes["partial"]++
				}
			}
		}
	})
	for _, want := range []string{"applied", "dropped", "partial", "garbled"} {
		if outcomes[want] == 0 {
			t.Errorf("no seed produced a %s write: %v", want, outcomes)
		}
	}
}

// The same seed has to resolve the same way every time, or nothing built on top
// of this can be replayed.
func TestPowerLossResolutionIsSeedStable(t *testing.T) {
	const size = 8192
	run := func(seed uint64) []byte {
		var result []byte
		synctest.Test(t, func(t *testing.T) {
			runtime := sim.New(sim.Config{Seed: seed})
			disk := faultyDisk(t, runtime, "node")
			writeRecord(t, disk, "record", size)
			if err := disk.PowerLoss(t.Context()); err != nil {
				t.Fatal(err)
			}
			result = readBack(t, disk, "record", size)
		})
		return result
	}
	for seed := uint64(1); seed <= 8; seed++ {
		if first, second := run(seed), run(seed); !bytes.Equal(first, second) {
			t.Fatalf("seed %d resolved two different images", seed)
		}
	}
	if bytes.Equal(run(1), run(2)) {
		t.Fatal("two seeds resolved the same image; the draw is not keyed by the seed")
	}
}

// A synced write is never at risk, whatever the kill mode: the resolution
// applies to the modifications after the last sync and to nothing else.
func TestPowerLossKeepsEverySyncedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for seed := uint64(1); seed <= 32; seed++ {
			runtime := sim.New(sim.Config{Seed: seed})
			disk := faultyDisk(t, runtime, fmt.Sprintf("node-%d", seed))
			file, err := disk.Open(t.Context(), "record", platform.OpenOptions{Create: true})
			if err != nil {
				t.Fatal(err)
			}
			want := bytes.Repeat([]byte("synced!!"), 1024)
			if _, err := file.WriteAt(t.Context(), want, 0); err != nil {
				t.Fatal(err)
			}
			if err := file.Sync(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := disk.PowerLoss(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := readBack(t, disk, "record", len(want)); !bytes.Equal(got, want) {
				t.Fatalf("seed %d lost a synced write", seed)
			}
		}
	})
}

// The default disk still restores exactly its last sync. Every test written
// before this primitive existed depends on that, so it stays the default.
func TestPowerLossWithoutFaultsDiscardsEveryUnsyncedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 5})
		disk := runtime.NewDisk("node", sim.DiskConfig{})
		record := writeRecord(t, disk, "record", 8192)
		if err := disk.PowerLoss(t.Context()); err != nil {
			t.Fatal(err)
		}
		got := readBack(t, disk, "record", len(record))
		if !bytes.Equal(got, make([]byte, len(record))) {
			t.Fatal("an unsynced write survived a disk that does not resolve them")
		}
	})
}

// A power loss reports what it did to every modification it resolved, so a
// failing consumer test can say which write was lost rather than only that the
// bytes are wrong.
func TestPowerLossTracesEveryResolvedModification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 3})
		disk := faultyDisk(t, runtime, "node")
		writeRecord(t, disk, "record", 4096)
		if err := disk.PowerLoss(t.Context()); err != nil {
			t.Fatal(err)
		}
		resolved := 0
		for _, event := range runtime.Trace().Events() {
			if event.Operation == "power_loss_write" {
				resolved++
				switch sim.PowerLossOutcome(event.Outcome) {
				case sim.PowerLossApplied, sim.PowerLossDropped,
					sim.PowerLossTruncated, sim.PowerLossGarbled:
				default:
					t.Fatalf("unexpected resolution outcome %q", event.Outcome)
				}
			}
		}
		if resolved != 1 {
			t.Fatalf("resolved %d writes, want the one that was not synced", resolved)
		}
	})
}

// A device that acknowledges a flush it did not perform leaves its writes at
// risk. Nothing in sproutfs treats a local file as durable, so this stays an
// opt-in with no consumer; the knob has to work for a campaign to use it.
func TestSyncDurableProbabilityLeavesWritesAtRisk(t *testing.T) {
	lost := 0
	synctest.Test(t, func(t *testing.T) {
		for seed := uint64(1); seed <= 32; seed++ {
			runtime := sim.New(sim.Config{Seed: seed})
			disk := runtime.NewDisk(fmt.Sprintf("node-%d", seed), sim.DiskConfig{
				PowerLossFaults: true, SyncDurableProbability: 0.5})
			file, err := disk.Open(t.Context(), "record", platform.OpenOptions{Create: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteAt(t.Context(), []byte("first"), 0); err != nil {
				t.Fatal(err)
			}
			// The first sync is what makes the file exist at all, and a power
			// loss takes a file nothing ever persisted. Syncing repeatedly
			// establishes the file under every seed, so the only thing this
			// test measures is what happens to the write that follows.
			for range 16 {
				if err := file.Sync(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			want := bytes.Repeat([]byte("acknowledged"), 64)
			if _, err := file.WriteAt(t.Context(), want, 0); err != nil {
				t.Fatal(err)
			}
			if err := file.Sync(t.Context()); err != nil {
				t.Fatalf("sync reported a failure instead of a false success: %v", err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			if err := disk.PowerLoss(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := readBack(t, disk, "record", len(want)); !bytes.Equal(got, want) {
				lost++
			}
		}
	})
	if lost == 0 {
		t.Fatal("every acknowledged sync persisted; the probability is not applied")
	}
}
