//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/adapters"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

// A pipe injects complete UFFD events; the peer only acknowledges control
// messages. No memory is actually mapped, and fault backing remains stalled.
func pipeConnection(t testing.TB, backing vmmemory.Backing) (*vmmemory.Connection, *vmmemory.Host, *os.File, context.CancelCauseFunc) {
	t.Helper()
	a, err := vmmemory.NewLinuxArena(1, pageSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	disk, err := adapters.NewDisk(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spill, err := disk.Open(t.Context(), "spill", platform.OpenOptions{Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spill.Close() })
	h, err := vmmemory.New(t.Context(), testresource.New(), vmmemory.Config{PageSize: pageSize, ResidentPages: 1, LogicalPages: int(backing.Size() / uint64(pageSize)), DirtyPages: 1}, a, spill)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "pager.sock"), Net: "unix"})
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
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	t.Cleanup(func() { _ = w.Close() })
	if err := vmwire.SendFD(client, vmwire.Frame{Kind: vmwire.Hello, ID: vmwire.Version}, r); err != nil {
		t.Fatal(err)
	}
	if err := vmwire.Write(client, vmwire.Frame{Kind: vmwire.Region, Flags: uint64(vmmemory.Ram), Length: backing.Size(), Offset: 2 << 20}); err != nil {
		t.Fatal(err)
	}
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		_, fd, err := vmwire.ReceiveFD(client)
		if err != nil {
			return
		}
		defer fd.Close()
		for {
			f, err := vmwire.Read(client)
			if err != nil {
				return
			}
			if f.Kind == vmwire.MapBatch {
				for range f.Length {
					if _, err := vmwire.Read(client); err != nil {
						return
					}
				}
			}
			if err := vmwire.Write(client, vmwire.Frame{Kind: vmwire.Ack, ID: f.ID}); err != nil {
				return
			}
		}
	}()
	ctx, cancel := context.WithCancelCause(t.Context())
	c, err := vmmemory.Connect(ctx, h, server, vmmemory.RegionBacking{Kind: vmmemory.Ram, Backing: backing}, vmmemory.ConnectionConfig{QueuePages: 1, FaultWorkers: 1, CommandTimeout: time.Minute, VerifyInterval: time.Hour})
	if err != nil {
		cancel(err)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel(context.Canceled)
		_ = client.Close()
		if err := c.Close(context.Background()); err != nil {
			t.Error(err)
		}
		<-peerDone
	})
	return c, h, w, cancel
}

func TestIdleConnectionWaitsForDescriptorReadiness(t *testing.T) {
	c, h, events, cancel := pipeConnection(t, newKernelBacking(1, pageSize))
	before, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	after, err := h.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if after.UFFDReads-before.UFFDReads > 1 {
		t.Fatalf("idle connection performed %d empty reads", after.UFFDReads-before.UFFDReads)
	}
	cause := errors.New("stop idle connection")
	cancel(cause)
	ctx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := c.Wait(ctx); !errors.Is(err, cause) {
		t.Fatalf("idle cancellation: %v", err)
	}
	// Cancellation cannot close UFFD before the supervisor stops memory users.
	if _, err := events.Write(make([]byte, 32)); err != nil {
		t.Fatalf("cancellation closed the owned descriptor: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := events.Write(make([]byte, 32)); err == nil {
		t.Fatal("Close retained the event descriptor")
	}
}

type stalledFaultBacking struct {
	*kernelBacking
	entered chan struct{}
}

func (b *stalledFaultBacking) Load(ctx context.Context, _ uint64, _ []byte) error {
	close(b.entered)
	<-ctx.Done()
	return context.Cause(ctx)
}

func TestRemapsDrainWhileFaultWorkerWaitsForBacking(t *testing.T) {
	b := &stalledFaultBacking{kernelBacking: newKernelBacking(1, pageSize), entered: make(chan struct{})}
	c, h, events, _ := pipeConnection(t, b)
	var fault [32]byte
	fault[0] = 0x12
	binary.LittleEndian.PutUint64(fault[16:24], 2<<20)
	if _, err := events.Write(fault[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("fault worker never reached backing")
	}
	var remaps [32 * 8]byte
	for i := range 8 {
		remaps[i*32] = 0x14
	}
	if _, err := events.Write(remaps[:]); err != nil {
		t.Fatal(err)
	}
	// An invalid final event is a barrier: observing its failure proves that
	// every preceding REMAP was consumed despite the blocked backing read.
	fault[0] = 0xff
	if _, err := events.Write(fault[:]); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := c.Wait(ctx); err == nil || !strings.Contains(err.Error(), "unexpected UFFD event") {
		t.Fatalf("reader did not drain past remaps: %v", err)
	}
	stats, err := h.Stats(t.Context())
	if err != nil || stats.RemapEvents != 8 {
		t.Fatalf("remap events: %+v %v", stats, err)
	}
}

func TestLargeConnectionKeepsTransportMetadataSparse(t *testing.T) {
	backing := &sparseMemoryBacking{size: 32 << 30, vm: "large", pages: make(map[uint64][]byte)}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	c, _, _, _ := pipeConnection(t, backing)
	runtime.GC()
	runtime.ReadMemStats(&after)
	if retained := int64(after.HeapAlloc) - int64(before.HeapAlloc); retained > 2<<20 {
		t.Fatalf("32 GiB connection retained %d metadata bytes, budget 2 MiB", retained)
	}
	runtime.KeepAlive(c)
}
