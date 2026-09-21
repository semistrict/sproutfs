package volume_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/volume"
)

// A pager asks its volume for a whole read-ahead run in one Load, and at 4 KiB
// a 2 MiB run is 512 pages. What that costs in object-store requests is what
// decides a cold restore: it must be what the checkpoint's layout allows and
// not one request per page.

// runVolumes is a VM whose memory is one 2 MiB read-ahead run of 4 KiB pages.
var runVolumes = []volume.VolumeSpec{
	{Name: "ram0", Size: checkpoint.PageSize2MiB, PageSize: checkpoint.PageSize4KiB},
}

// getCounter counts the reads a manager makes of object storage.
type getCounter struct {
	platform.ObjectStore
	mu   sync.Mutex
	gets int
}

func (c *getCounter) Get(ctx context.Context, request platform.GetRequest) (platform.GetResult, error) {
	result, err := c.ObjectStore.Get(ctx, request)
	if err == nil {
		c.mu.Lock()
		c.gets++
		c.mu.Unlock()
	}
	return result, err
}

func (c *getCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets = 0
}

func (c *getCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gets
}

// incompressible fills dst with bytes the encoder cannot shrink, so a member is
// the page it holds and what a run costs in bytes is the run's own size.
func incompressible(seed uint64, dst []byte) {
	state := seed | 1
	var word [8]byte
	for at := 0; at < len(dst); at += len(word) {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		binary.LittleEndian.PutUint64(word[:], state)
		copy(dst[at:], word[:])
	}
}

// A cold read of a 2 MiB run of 4 KiB pages is two requests: one of the page
// table segment that locates the run's pages, and one of the extent its members
// lie in, because a publication writes a volume's changed pages in page order
// and their members are therefore adjacent in its part.
func TestAColdRunOfSmallPagesIsTwoRequests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		defer h.close(t.Context())
		counter := &getCounter{ObjectStore: h.objects}
		h.objects = counter
		manager := h.manager(t, h.config())
		vm, err := manager.Create(t.Context(), "run", runVolumes)
		if err != nil {
			t.Fatal(err)
		}
		want := make([]byte, checkpoint.PageSize2MiB)
		incompressible(0x5eed, want)
		for page := range uint64(checkpoint.PageSize2MiB / checkpoint.PageSize4KiB) {
			at := page * checkpoint.PageSize4KiB
			if err := vm.Volume("ram0").Write(t.Context(), at, want[at:at+checkpoint.PageSize4KiB]); err != nil {
				t.Fatal(err)
			}
		}
		if err := vm.Checkpoint(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := vm.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Close(t.Context()); err != nil {
			t.Fatal(err)
		}

		// Another host opens the VM, so neither its page table nor any page of
		// it is in hand and the load is as cold as a restore's.
		cold := h.manager(t, h.config())
		defer cold.Close(t.Context())
		opened, err := cold.Open(t.Context(), "run")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close(t.Context())
		counter.reset()
		got := make([]byte, checkpoint.PageSize2MiB)
		if err := opened.Volume("ram0").Load(t.Context(), 0, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("the run reads back as %#x..., want %#x...", got[:8], want[:8])
		}
		if gets := counter.count(); gets != 2 {
			t.Fatalf("a cold load of a %d-page run cost %d requests, want the segment and the one extent",
				checkpoint.PageSize2MiB/checkpoint.PageSize4KiB, gets)
		}
	})
}
