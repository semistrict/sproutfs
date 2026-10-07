package journal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

var (
	diskOne = rank.Identity{0x4a, 0x01}
	memberA = rank.Identity{0xa0}
	memberB = rank.Identity{0xb0}
)

// device makes a journal disk of a ring of ring bytes on a simulated disk:
// one file, every byte of it zero and synced. It returns the disk, and a
// function that opens a new handle of the file each time it is called, as a
// process that opens the device does.
func device(t *testing.T, ctx context.Context, runtime *sim.Runtime, ring int64,
	config sim.DiskConfig) (*sim.Disk, func() platform.File) {
	t.Helper()
	return deviceNamed(t, ctx, runtime, "journal-disk", ring, config)
}

// deviceNamed is device on a simulated disk of that name, for a test that
// makes more than one.
func deviceNamed(t *testing.T, ctx context.Context, runtime *sim.Runtime, name string, ring int64,
	config sim.DiskConfig) (*sim.Disk, func() platform.File) {
	t.Helper()
	disk := runtime.NewDisk(name, config)
	file, err := disk.Open(ctx, "device", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(ctx, ringOffset+ring); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := disk.SyncNamespace(ctx); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return disk, func() platform.File {
		t.Helper()
		handle, err := disk.Open(ctx, "device", platform.OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := handle.Close(); err != nil {
				t.Errorf("closing a handle of the device: %v", err)
			}
		})
		return handle
	}
}

// open opens the journal on file under lease, and closes it when the test
// ends if the test did not.
func open(t *testing.T, ctx context.Context, file platform.File, lease Lease) *Journal {
	t.Helper()
	return openWith(t, ctx, file, Config{Lease: lease})
}

// openWith is open with a configuration of the test's own. The disk is
// diskOne, and its entropy the runtime's unless the test names one.
func openWith(t *testing.T, ctx context.Context, file platform.File, config Config) *Journal {
	t.Helper()
	config.Identity = diskOne
	if config.Entropy == nil {
		config.Entropy = sim.RuntimeFrom(ctx).NewEntropy("journal")
	}
	j, err := Open(ctx, file, config)
	if err != nil {
		t.Fatal(err)
	}
	closeAtEnd(t, ctx, j)
	return j
}

// sized is an entry of vm's disk at epoch of blocks blocks that takes exactly
// size bytes on the ring: its volume's name fills what the blocks do not.
func sized(vm string, epoch uint64, fill byte, blocks int, size int64) Entry {
	numbers := make([]uint64, blocks)
	for i := range numbers {
		numbers[i] = uint64(i)
	}
	e := entryOf(vm, epoch, fill, numbers...)
	names := size - EntryBytes("", "", blocks)
	e.Volume = strings.Repeat("v", int(names)-len(vm))
	if e.size() != size {
		panic(fmt.Sprintf("an entry of %d blocks cannot take %d bytes", blocks, size))
	}
	return e
}

// closeAtEnd closes j when the test ends, so its writer stops before the
// bubble does, unless the test closed it already.
func closeAtEnd(t *testing.T, ctx context.Context, j *Journal) {
	t.Cleanup(func() {
		if _, err := j.Close(context.WithoutCancel(ctx)); err != nil && !errors.Is(err, ErrClosed) {
			t.Logf("closing the journal as the test ends: %v", err)
		}
	})
}

// entryOf is an entry of vm's disk at epoch, whose blocks are filled from
// fill on, one value a block.
func entryOf(vm string, epoch uint64, fill byte, blocks ...uint64) Entry {
	data := make([]byte, len(blocks)*BlockBytes)
	for i := range data {
		data[i] = fill + byte(i/BlockBytes)
	}
	return Entry{VM: vm, Volume: "disk", Epoch: epoch, Blocks: blocks, Data: data}
}

// roomOf is the room entries take.
func roomOf(entries ...Entry) int64 {
	var room int64
	for _, e := range entries {
		room += e.size()
	}
	return room
}

// commitOf commits entries as one commit.
func commitOf(ctx context.Context, j *Journal, entries ...Entry) ([]uint64, error) {
	return j.Commit(ctx, roomOf(entries...), func(context.Context) ([]Entry, error) { return entries, nil })
}

// mustCommit commits entries and fails the test if the commit fails.
func mustCommit(t *testing.T, ctx context.Context, j *Journal, entries ...Entry) []uint64 {
	t.Helper()
	positions, err := commitOf(ctx, j, entries...)
	if err != nil {
		t.Fatal(err)
	}
	return positions
}

// readAll reads vm's entries of epoch after after, as a reader at reader.
func readAll(t *testing.T, ctx context.Context, j *Journal, vm string, epoch, after, reader uint64) []Entry {
	t.Helper()
	var entries []Entry
	err := j.Read(ctx, ReadRequest{VM: vm, Epoch: epoch, After: after, Generation: j.Generation(), Reader: reader},
		func(e Entry) error {
			entries = append(entries, e)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// at is e as it reads back from position.
func at(e Entry, position uint64) Entry {
	e.Position = position
	return e
}

// sameEntries reports how got differs from want, or "".
func sameEntries(got, want []Entry) string {
	if len(got) != len(want) {
		return fmt.Sprintf("%d entries, want %d: %v", len(got), len(want), describe(got))
	}
	for i := range got {
		g, w := got[i], want[i]
		if g.VM != w.VM || g.Volume != w.Volume || g.Epoch != w.Epoch || g.Position != w.Position ||
			fmt.Sprint(g.Blocks) != fmt.Sprint(w.Blocks) || !bytes.Equal(g.Data, w.Data) {
			return fmt.Sprintf("entry %d is %s, want %s", i, describe(got[i:i+1]), describe(want[i:i+1]))
		}
	}
	return ""
}

func describe(entries []Entry) string {
	var b bytes.Buffer
	for _, e := range entries {
		first := byte(0)
		if len(e.Data) > 0 {
			first = e.Data[0]
		}
		fmt.Fprintf(&b, "{%s %s epoch %d at %d blocks %v from %#x}", e.VM, e.Volume, e.Epoch, e.Position, e.Blocks,
			first)
	}
	return b.String()
}

// gatedFile is a file whose next sync can be held until the test releases
// it, and which counts its reads and writes.
type gatedFile struct {
	platform.File
	mu      sync.Mutex
	gate    chan struct{}
	reached chan struct{}
	reads   int
	writes  int
}

// hold makes the next sync wait for release. The channel it returns is closed
// once that sync is waiting.
func (f *gatedFile) hold() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate, f.reached = make(chan struct{}), make(chan struct{})
	return f.reached
}

// release lets the held sync go on.
func (f *gatedFile) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
}

func (f *gatedFile) written() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func (f *gatedFile) read() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *gatedFile) ReadAt(ctx context.Context, b []byte, offset int64) (int, error) {
	f.mu.Lock()
	f.reads++
	f.mu.Unlock()
	return f.File.ReadAt(ctx, b, offset)
}

func (f *gatedFile) WriteAt(ctx context.Context, b []byte, offset int64) (int, error) {
	f.mu.Lock()
	f.writes++
	f.mu.Unlock()
	return f.File.WriteAt(ctx, b, offset)
}

func (f *gatedFile) Sync(ctx context.Context) error {
	f.mu.Lock()
	gate, reached := f.gate, f.reached
	f.reached = nil
	f.mu.Unlock()
	if reached != nil {
		close(reached)
	}
	if gate != nil {
		<-gate
	}
	return f.File.Sync(ctx)
}
