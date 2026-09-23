//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/adapters"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

// holeVolume is a volume that is a hole throughout, at this pager's own page.
type holeVolume struct{ size uint64 }

func (v holeVolume) Size() uint64                             { return v.size }
func (holeVolume) Load(context.Context, uint64, []byte) error { return nil }
func (holeVolume) Verify(context.Context) error               { return nil }
func (holeVolume) Locate(_ context.Context, offset, length uint64) ([]control.Extent, error) {
	return []control.Extent{{Offset: offset, Length: length, Identity: control.Identity{Zero: true}}}, nil
}

// A session's owner learns only that the connection is gone: it kills the
// client process, and the errno the client answered with is all that reaches
// the VM's exit. So what the pager asked for has to travel with the failure —
// which command, over which pages, out of which arena offsets, at which
// generation — or a refusal on a host says "invalid argument" and nothing else,
// and there is no way back from that line to the command that was refused.
func TestARefusedCommandNamesTheFrameTheClientRefused(t *testing.T) {
	const page = checkpoint.PageSize4KiB
	const pages = 8
	arena, err := vmmemory.NewLinuxArena(rangePages, page)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = arena.Close() })
	disk, err := adapters.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spill, err := disk.Open(t.Context(), "spill", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spill.Close() })
	h, err := vmmemory.New(t.Context(), testresource.New(), vmmemory.Config{PageSize: page,
		ResidentPages: pages, ArenaOffsets: rangePages, LogicalPages: 4 * pages,
		DirtyPages: pages, ReadAheadPages: pages}, arena, spill)
	if err != nil {
		t.Fatal(err)
	}
	// A sibling region that has seen its volume's holes is what gives the
	// populate below something to map eagerly, which is the command the client
	// refuses.
	sibling, err := h.Attach(t.Context(), vmmemory.RegionBacking{Kind: vmmemory.Ram,
		Backing: holeVolume{pages * page}}, seedMapping{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sibling.Detach(context.Background()) })
	if err := sibling.Fault(t.Context(), 0, false); err != nil {
		t.Fatal(err)
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "ram.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	server, err := listener.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	events, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	t.Cleanup(func() { _ = writer.Close() })
	if err := vmwire.SendFD(client, vmwire.Frame{Kind: vmwire.Hello, ID: vmwire.Version}, events); err != nil {
		t.Fatal(err)
	}
	if err := vmwire.Write(client, vmwire.Frame{Kind: vmwire.Region,
		Flags: uint64(vmmemory.Ram), Length: pages * page, Offset: 2 << 20}); err != nil {
		t.Fatal(err)
	}
	// The client out of mapping budget: it refuses every command before it
	// touches anything, exactly as a real one does.
	refused := make(chan struct{})
	go func() {
		defer close(refused)
		if _, fd, err := vmwire.ReceiveFD(client); err == nil {
			defer fd.Close()
		}
		for {
			f, err := vmwire.Read(client)
			if err != nil {
				return
			}
			for range f.Length {
				if f.Kind != vmwire.MapBatch {
					break
				}
				if _, err := vmwire.Read(client); err != nil {
					return
				}
			}
			if err := vmwire.Write(client, vmwire.Frame{Kind: vmwire.Ack, ID: f.ID,
				Generation: f.Generation, Flags: uint64(syscall.EINVAL)}); err != nil {
				return
			}
		}
	}()
	_, err = vmmemory.Connect(t.Context(), h, server, vmmemory.RegionBacking{Kind: vmmemory.Ram,
		Backing: holeVolume{pages * page}}, vmmemory.ConnectionConfig{Name: "ram0", QueuePages: 1,
		FaultWorkers: 1, CommandTimeout: 10 * time.Second, VerifyInterval: time.Hour})
	if err == nil {
		t.Fatal("a session whose every command is refused attached")
	}
	want := "zero command id 1 offset 0 length 32768 backing 0 generation 1 flags 1 runs 0"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("the refused session failed with %q, want it to name %q", err, want)
	}
	<-refused
}
