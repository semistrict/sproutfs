package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A mount table names, for each filesystem, the device it is on. The kernel
// mounts the root filesystem itself and calls that device /dev/root, which is
// the entry a guest cold started onto a larger root volume has to find.
const guestMounts = `/dev/root / ext4 rw,relatime,dax=always 0 0
devtmpfs /dev devtmpfs rw,nosuid 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
tmpfs /run tmpfs rw,nosuid,nodev 0 0
/dev/pmem1 /var/a\040witness ext4 rw,relatime 0 0
`

func TestTheMountTableNamesTheDeviceAFilesystemIsOn(t *testing.T) {
	device, err := mountedDevice(strings.NewReader(guestMounts), "/")
	if err != nil {
		t.Fatal(err)
	}
	if device != "/dev/root" {
		t.Fatalf("the root filesystem is on %q, want /dev/root", device)
	}
}

// A mount point is matched as the kernel writes it, which is with the spaces,
// tabs and newlines in it escaped.
func TestTheMountTableIsReadWithItsEscapesUndone(t *testing.T) {
	device, err := mountedDevice(strings.NewReader(guestMounts), "/var/a witness")
	if err != nil {
		t.Fatal(err)
	}
	if device != "/dev/pmem1" {
		t.Fatalf("that filesystem is on %q, want /dev/pmem1", device)
	}
}

// A point nothing is mounted on is refused rather than grown: there is no
// filesystem there to give the pages to.
func TestAPointNothingIsMountedOnIsRefused(t *testing.T) {
	_, err := mountedDevice(strings.NewReader(guestMounts), "/mnt")
	if err == nil {
		t.Fatal("a point nothing is mounted on was taken for a filesystem")
	}
	if !strings.Contains(err.Error(), "/mnt") {
		t.Fatalf("the refusal reads %q, want it to name the point that is not mounted", err)
	}
}

// /dev/root is not a node devtmpfs ever creates: the guest's init makes it a
// symlink to the device the kernel booted from, and that is what carries the
// size the filesystem may grow into.
func TestTheRootDeviceIsFollowedToTheNodeItNames(t *testing.T) {
	// The directory is resolved first: a temporary directory can itself be
	// reached through a symlink, and what is being tested is the last hop.
	dev, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "pmem0"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pmem0", filepath.Join(dev, "root")); err != nil {
		t.Fatal(err)
	}
	node, err := followDevice(filepath.Join(dev, "root"))
	if err != nil {
		t.Fatal(err)
	}
	if node != filepath.Join(dev, "pmem0") {
		t.Fatalf("/dev/root led to %q, want the node it names", node)
	}
	// A device that is a node already is itself.
	node, err = followDevice(filepath.Join(dev, "pmem0"))
	if err != nil {
		t.Fatal(err)
	}
	if node != filepath.Join(dev, "pmem0") {
		t.Fatalf("a device node led to %q, want itself", node)
	}
}

// oneSuperblock is the bytes of an ext4 superblock holding a filesystem of that
// many blocks of 1024 << shift bytes, with the 64-bit feature set when the
// count needs it.
func oneSuperblock(shift uint32, blocks uint64) []byte {
	raw := make([]byte, superblockBytes)
	binary.LittleEndian.PutUint16(raw[superblockMagic:], ext4Magic)
	binary.LittleEndian.PutUint32(raw[logBlockSize:], shift)
	binary.LittleEndian.PutUint32(raw[blocksCountLow:], uint32(blocks))
	if blocks>>32 != 0 {
		binary.LittleEndian.PutUint32(raw[featureIncompat:], incompat64Bit)
		binary.LittleEndian.PutUint32(raw[blocksCountHigh:], uint32(blocks>>32))
	}
	return raw
}

// How many blocks a filesystem has is its superblock's to say and not statfs's:
// statfs reports the data blocks, net of the journal and the per-group
// metadata, and the ioctl counts every block including those.
func TestTheSuperblockSaysHowLargeTheFilesystemIs(t *testing.T) {
	for _, c := range []struct {
		shift  uint32
		blocks uint64
	}{{2, 16384}, {0, 65536}, {2, 1 << 34}} {
		blockBytes, blocks, err := parseSuperblock(oneSuperblock(c.shift, c.blocks))
		if err != nil {
			t.Fatal(err)
		}
		if blockBytes != uint64(1024<<c.shift) || blocks != c.blocks {
			t.Fatalf("that superblock reads as %d blocks of %d, want %d of %d",
				blocks, blockBytes, c.blocks, 1024<<c.shift)
		}
	}
}

// The high half of the block count is only a number when the feature that makes
// it one is set. A filesystem without it has whatever was there before.
func TestTheHighBlockCountIsReadOnlyWhenItIsOne(t *testing.T) {
	raw := oneSuperblock(2, 16384)
	binary.LittleEndian.PutUint32(raw[blocksCountHigh:], 0xdeadbeef)
	_, blocks, err := parseSuperblock(raw)
	if err != nil {
		t.Fatal(err)
	}
	if blocks != 16384 {
		t.Fatalf("a 32-bit filesystem reads as %d blocks, want 16384", blocks)
	}
}

// Anything that is not an ext4 superblock is refused rather than read as one:
// the ioctl below only means anything on a filesystem this understands.
func TestSomethingThatIsNotAnExt4SuperblockIsRefused(t *testing.T) {
	if _, _, err := parseSuperblock(make([]byte, superblockBytes)); err == nil {
		t.Fatal("a superblock of zeroes was read as a filesystem")
	}
	if _, _, err := parseSuperblock(oneSuperblock(2, 16384)[:16]); err == nil {
		t.Fatal("sixteen bytes were read as a superblock")
	}
	tooLarge := oneSuperblock(2, 16384)
	binary.LittleEndian.PutUint32(tooLarge[logBlockSize:], logBlockSizeMax+1)
	if _, _, err := parseSuperblock(tooLarge); err == nil {
		t.Fatal("a block size larger than ext4 defines was accepted")
	}
}

// The block count a grow asks for is the whole filesystem blocks the device
// holds. A device whose size is not a whole number of them keeps the remainder:
// a filesystem does not have a partial block at its end.
func TestTheBlockCountIsTheWholeBlocksTheDeviceHolds(t *testing.T) {
	for _, c := range []struct {
		deviceBytes, blockBytes, have, want uint64
	}{
		{128 << 20, 4096, 16384, 32768},
		{3 << 30, 4096, 524288, 786432},
		// A device with half a block spare grows by the whole ones only.
		{(128 << 20) + 2048, 4096, 16384, 32768},
		// 1 KiB blocks are a filesystem of its own arithmetic.
		{64 << 20, 1024, 32768, 65536},
	} {
		got, err := blocksToTake(c.deviceBytes, c.blockBytes, c.have)
		if err != nil {
			t.Fatalf("%d bytes over %d-byte blocks: %v", c.deviceBytes, c.blockBytes, err)
		}
		if got != c.want {
			t.Fatalf("%d bytes over %d-byte blocks is %d blocks, want %d",
				c.deviceBytes, c.blockBytes, got, c.want)
		}
	}
}

// The end of a filesystem is not the device's to cut. A device that holds fewer
// blocks than the filesystem already has is refused, and nothing is written.
func TestAFilesystemIsNeverShrunk(t *testing.T) {
	_, err := blocksToTake(64<<20, 4096, 32768)
	if err == nil {
		t.Fatal("a filesystem larger than its device was shrunk to fit")
	}
	if !strings.Contains(err.Error(), "16384") || !strings.Contains(err.Error(), "32768") {
		t.Fatalf("the refusal reads %q, want it to name both block counts", err)
	}
}

// A filesystem that already fills its device is already what a grow would make
// it, and takes no blocks rather than being refused: a cold start that did not
// grow the volume leaves exactly this.
func TestAFilesystemThatFillsItsDeviceTakesNothing(t *testing.T) {
	blocks, err := blocksToTake(128<<20, 4096, 32768)
	if err != nil {
		t.Fatal(err)
	}
	if blocks != 32768 {
		t.Fatalf("a filesystem that fills its device would be resized to %d blocks", blocks)
	}
}

// A device or a filesystem that answered with nothing is one this cannot do the
// arithmetic of, and saying so beats resizing to zero blocks.
func TestASizelessFilesystemIsRefused(t *testing.T) {
	if _, err := blocksToTake(128<<20, 0, 32768); err == nil {
		t.Fatal("a filesystem of 0-byte blocks was accepted")
	}
	if _, err := blocksToTake(0, 4096, 32768); err == nil {
		t.Fatal("a device of no bytes was accepted")
	}
}

// The command line says so: a grow takes the mount point and nothing else.
func TestGrowTakesOneMountPoint(t *testing.T) {
	if err := run([]string{"grow"}); err == nil {
		t.Fatal("a grow with no mount point was accepted")
	}
	if err := run([]string{"grow", "/", "/mnt"}); err == nil {
		t.Fatal("a grow of two mount points was accepted")
	}
	if err := run([]string{"grow", "--disk", "/var/witness"}); err == nil {
		t.Fatal("a grow of a flag was accepted")
	}
}
