//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/control"
	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/platform/adapters"
	"github.com/semistrict/sproutfs/internal/testresource"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

type stalledAttachmentBacking struct {
	*kernelBacking
	stage   string
	entered chan struct{}
}

// seedMapping retains a resident page without any kernel memory users.
type seedMapping struct{}

func (seedMapping) Map(context.Context, uint64, int, int, bool) error { return nil }
func (seedMapping) MapZero(context.Context, uint64, int) error        { return nil }
func (seedMapping) Revoke(context.Context, uint64) error              { return nil }
func (seedMapping) Resolve(context.Context, uint64, int, bool) error  { return nil }
func (seedMapping) Protect(context.Context, uint64, int) error        { return nil }

func (b *stalledAttachmentBacking) Verify(ctx context.Context) error {
	if b.stage == "admission" {
		close(b.entered)
		<-ctx.Done()
		return context.Cause(ctx)
	}
	return b.kernelBacking.Verify(ctx)
}

func (b *stalledAttachmentBacking) Locate(ctx context.Context, offset, length uint64) ([]control.Extent, error) {
	if b.stage == "metadata" {
		close(b.entered)
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	return b.kernelBacking.Locate(ctx, offset, length)
}

func TestConnectionCancellationDuringAttachment(t *testing.T) {
	if os.Getenv("SPROUTFS_VM_MEMORY_CLIENT") == "" {
		t.Skip("requires a provisioned HugeTLB pool")
	}
	for _, stage := range []string{"descriptor", "admission", "metadata"} {
		t.Run(stage, func(t *testing.T) {
			a, err := vmmemory.NewLinuxArena(1)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			disk, err := adapters.NewDisk(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			spill, err := disk.Open(t.Context(), "spill", platform.OpenOptions{Create: true})
			if err != nil {
				t.Fatal(err)
			}
			defer spill.Close()
			h, err := vmmemory.New(t.Context(), testresource.New(), vmmemory.Config{ResidentPages: 1, LogicalPages: 2, DirtyPages: 1}, a, spill)
			if err != nil {
				t.Fatal(err)
			}
			var seed *vmmemory.Region
			if stage == "metadata" {
				seed, err = h.Attach(t.Context(), newKernelBacking(1, vmmemory.PageSize), seedMapping{})
				if err != nil {
					t.Fatal(err)
				}
				defer seed.Detach(context.Background())
				if err := seed.Fault(t.Context(), 0, false); err != nil {
					t.Fatal(err)
				}
			}
			b := &stalledAttachmentBacking{kernelBacking: newKernelBacking(1, vmmemory.PageSize), stage: stage, entered: make(chan struct{})}
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(t.TempDir(), "pager.sock"), Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			client, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			server, err := listener.AcceptUnix()
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(context.Canceled)
			done := make(chan error, 1)
			go func() {
				c, err := vmmemory.Connect(ctx, h, server, vmmemory.RegionBacking{Kind: vmmemory.Ram, Backing: b}, vmmemory.ConnectionConfig{QueuePages: 1, CommandTimeout: time.Minute, VerifyInterval: time.Hour})
				// This client never maps memory, so no guest users can outlive it.
				_ = client.Close()
				if c != nil {
					err = errors.Join(err, c.Close(context.Background()))
				}
				done <- err
			}()
			if stage != "descriptor" {
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				defer w.Close()
				if err := vmwire.SendFD(client, vmwire.Frame{Kind: vmwire.Hello, ID: vmwire.Version}, r); err != nil {
					t.Fatal(err)
				}
				if err := vmwire.Write(client, vmwire.Frame{Kind: vmwire.Region, Flags: uint64(vmmemory.Ram), Length: b.Size(), Offset: 2 << 20}); err != nil {
					t.Fatal(err)
				}
				if stage == "metadata" {
					_, fd, err := vmwire.ReceiveFD(client)
					if err != nil {
						t.Fatal(err)
					}
					_ = fd.Close()
				}
				select {
				case <-b.entered:
				case err := <-done:
					t.Fatalf("attachment ended before %s: %v", stage, err)
				case <-time.After(time.Second):
					t.Fatalf("attachment never reached %s", stage)
				}
			}
			cause := errors.New("attachment canceled")
			cancel(cause)
			select {
			case err := <-done:
				if !errors.Is(err, cause) {
					t.Fatalf("canceled attachment: %v", err)
				}
			case <-time.After(time.Second):
				_ = server.Close()
				<-done
				t.Fatal("attachment ignored cancellation")
			}
			if seed != nil {
				if err := seed.Detach(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			stats, err := h.Stats(t.Context())
			if err != nil || stats.LogicalPages != 0 || stats.ResidentPages != 0 {
				t.Fatalf("canceled attachment retained capacity: %+v %v", stats, err)
			}
		})
	}
}
