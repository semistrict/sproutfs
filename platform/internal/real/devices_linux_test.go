package real

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/sproutfs/platform"
)

// A device is opened by the name its volume was attached under, reports its
// size by where its end is, refuses to be truncated, and a volume not
// attached here is not found. A regular file stands in for the block device:
// only a real device's exclusive claim is beyond it.
func TestADeviceOpensByItsAttachedName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "google-shard-0"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	devices := NewDevices(dir, "google-", GCEDeviceName)
	ctx := t.Context()
	device, err := devices.Open(ctx, "projects/p/zones/z/disks/shard-0")
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	if size, err := device.Size(ctx); err != nil || size != 1<<20 {
		t.Fatalf("the device is %d bytes (%v), want 1 MiB", size, err)
	}
	if _, err := device.WriteAt(ctx, []byte("lease"), 4096); err != nil {
		t.Fatal(err)
	}
	read := make([]byte, 5)
	if _, err := device.ReadAt(ctx, read, 4096); err != nil || string(read) != "lease" {
		t.Fatalf("the device read back %q (%v)", read, err)
	}
	if err := device.Truncate(ctx, 0); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("truncating a device: %v, want ErrUnsupported", err)
	}
	if _, ok := device.(platform.SparseFile); ok {
		t.Fatal("a device offers to punch holes")
	}
	if _, err := devices.Open(ctx, "projects/p/zones/z/disks/shard-1"); !errors.Is(err, platform.ErrNotFound) {
		t.Fatalf("a volume not attached here: %v, want ErrNotFound", err)
	}
}
