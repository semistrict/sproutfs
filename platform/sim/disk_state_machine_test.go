package sim_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// This is the AsyncFileCorrectness/DiskDurability test shape from
// FoundationDB: every successful operation updates an independent model, and
// every power loss restores exactly the last acknowledged durable image.
func TestDiskStateMachinePreservesAcknowledgedDurability(t *testing.T) {
	for seed := int64(1); seed <= 10; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runDiskStateMachine(t, seed, 100)
			})
		})
	}
}

func runDiskStateMachine(t *testing.T, seed int64, steps int) {
	t.Helper()
	runtime := sim.New(sim.Config{})
	disk := runtime.NewDisk("model", sim.DiskConfig{})
	file, err := disk.Open(t.Context(), "data", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	random := rand.New(rand.NewSource(seed))
	var volatile []byte
	var durable []byte
	durableExists := false
	commands := make([]string, 0, steps)
	for step := range steps {
		switch random.Intn(5) {
		case 0, 1:
			offset := random.Intn(48)
			data := make([]byte, random.Intn(17))
			for index := range data {
				data[index] = byte(random.Intn(256))
			}
			commands = append(commands, fmt.Sprintf("write(%d,%x)", offset, data))
			n, writeErr := file.WriteAt(t.Context(), data, int64(offset))
			if writeErr != nil || n != len(data) {
				failDiskModel(t, seed, step, commands, "WriteAt = (%d, %v), want (%d, nil)", n, writeErr, len(data))
			}
			if len(data) > 0 {
				end := offset + len(data)
				if end > len(volatile) {
					volatile = append(volatile, make([]byte, end-len(volatile))...)
				}
				copy(volatile[offset:end], data)
			}
		case 2:
			size := random.Intn(65)
			commands = append(commands, fmt.Sprintf("truncate(%d)", size))
			if err := file.Truncate(t.Context(), int64(size)); err != nil {
				failDiskModel(t, seed, step, commands, "Truncate: %v", err)
			}
			if size <= len(volatile) {
				volatile = volatile[:size]
			} else {
				volatile = append(volatile, make([]byte, size-len(volatile))...)
			}
		case 3:
			commands = append(commands, "sync")
			if err := file.Sync(t.Context()); err != nil {
				failDiskModel(t, seed, step, commands, "Sync: %v", err)
			}
			durable = bytes.Clone(volatile)
			durableExists = true
		case 4:
			commands = append(commands, "power-loss")
			stale := file
			if err := disk.PowerLoss(t.Context()); err != nil {
				failDiskModel(t, seed, step, commands, "PowerLoss: %v", err)
			}
			if _, err := stale.Size(t.Context()); !errors.Is(err, platform.ErrStaleHandle) {
				failDiskModel(t, seed, step, commands, "stale Size error = %v, want ErrStaleHandle", err)
			}
			if err := stale.Close(); err != nil {
				failDiskModel(t, seed, step, commands, "close stale handle: %v", err)
			}
			file, err = disk.Open(t.Context(), "data", platform.OpenOptions{})
			if durableExists {
				if err != nil {
					failDiskModel(t, seed, step, commands, "reopen durable file: %v", err)
				}
				volatile = bytes.Clone(durable)
			} else {
				if !errors.Is(err, platform.ErrNotFound) {
					failDiskModel(t, seed, step, commands, "reopen unsynced file error = %v, want ErrNotFound", err)
				}
				file, err = disk.Open(t.Context(), "data", platform.OpenOptions{Create: true})
				if err != nil {
					failDiskModel(t, seed, step, commands, "recreate file: %v", err)
				}
				volatile = nil
			}
		}
		assertDiskImage(t, seed, step, commands, file, volatile)
	}
}

func assertDiskImage(
	t *testing.T,
	seed int64,
	step int,
	commands []string,
	file platform.File,
	want []byte,
) {
	t.Helper()
	size, err := file.Size(t.Context())
	if err != nil {
		failDiskModel(t, seed, step, commands, "Size: %v", err)
	}
	if size != int64(len(want)) {
		failDiskModel(t, seed, step, commands, "size = %d, want %d", size, len(want))
	}
	if len(want) == 0 {
		return
	}
	got := make([]byte, len(want))
	n, err := file.ReadAt(t.Context(), got, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		failDiskModel(t, seed, step, commands, "ReadAt: %v", err)
	}
	if n != len(got) || !bytes.Equal(got, want) {
		failDiskModel(t, seed, step, commands, "ReadAt = (%d, %x), want (%d, %x)", n, got, len(want), want)
	}
}

func failDiskModel(t *testing.T, seed int64, step int, commands []string, format string, arguments ...any) {
	t.Helper()
	t.Fatalf("seed=%d step=%d commands=%v: %s", seed, step, commands, fmt.Sprintf(format, arguments...))
}
