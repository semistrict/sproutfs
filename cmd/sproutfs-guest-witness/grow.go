package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Growing a filesystem over the pages its volume gained is the other thing a
// guest is asked for, and it is here because there is nowhere else it can be.
//
// A cold start is the one moment a VM's shape can change, and a root volume
// that grew comes back with new pages that read as zeroes. The filesystem on it
// is still the size it was, so a guest that took a larger disk holds a
// filesystem that ends well before its device does, and nothing it writes can
// reach the rest.
//
// The obvious tool cannot do it. resize2fs's online path opens the block device
// read-write, and a 6.18 kernel refuses a write open of a device something has
// mounted — there is no knob to allow it — so on a guest whose root filesystem
// is the device it would have to open, resize2fs fails with EBUSY before it
// begins. What the kernel does offer is EXT4_IOC_RESIZE_FS: an ioctl on a
// descriptor of the mount point, carrying the block count the filesystem is to
// have. It needs no write open of anything, which is exactly why it works where
// resize2fs cannot.
//
// The device is still readable: a read-only open of it is allowed, and that is
// where both of the other numbers come from. BLKGETSIZE64 answers how many
// bytes it holds, and the filesystem's own superblock — the first bytes of the
// device — says how large its blocks are and how many of them it has. statfs
// cannot be asked the second one: its f_blocks is the data blocks a filesystem
// has, net of the journal and the per-group metadata, and the ioctl counts
// every block including those. So the whole operation is three reads and one
// ioctl, and it is small enough to live in the binary every guest carries.

// growAt makes the filesystem mounted at point as large as the device under it,
// and says what it was and what it became. A filesystem that already fills its
// device is left alone rather than refused: a cold start that did not grow the
// volume leaves exactly that, and a soak asking every cold-started guest to
// grow must not be failed by the ones that had nothing to take.
func growAt(point string) error {
	table, err := os.Open(mountTable)
	if err != nil {
		return fmt.Errorf("the mount table %s: %w", mountTable, err)
	}
	named, err := mountedDevice(table, point)
	closed := table.Close()
	if err != nil {
		return err
	}
	if closed != nil {
		return fmt.Errorf("the mount table %s: %w", mountTable, closed)
	}
	device, err := followDevice(named)
	if err != nil {
		return err
	}
	deviceBytes, err := deviceSize(device)
	if err != nil {
		return err
	}
	blockBytes, have, err := filesystemBlocks(device)
	if err != nil {
		return err
	}
	want, err := blocksToTake(deviceBytes, blockBytes, have)
	if err != nil {
		return fmt.Errorf("growing %s on %s: %w", point, device, err)
	}
	if want == have {
		fmt.Printf("%s already fills %s: %d blocks of %d, %d bytes\n",
			point, device, have, blockBytes, have*blockBytes)
		return nil
	}
	if err := resizeFilesystem(point, want); err != nil {
		return fmt.Errorf("growing %s on %s from %d to %d blocks of %d: %w",
			point, device, have, want, blockBytes, err)
	}
	// Read the superblock again rather than printing what was asked for: what
	// the filesystem now says it is is the only thing that says the ioctl did
	// it.
	_, now, err := filesystemBlocks(device)
	if err != nil {
		return err
	}
	fmt.Printf("%s grew from %d to %d bytes on %s: %d to %d blocks of %d\n",
		point, have*blockBytes, now*blockBytes, device, have, now, blockBytes)
	if now != want {
		return fmt.Errorf("%s holds %d blocks after being grown to %d", point, now, want)
	}
	return nil
}

// mountTable is where the kernel lists what is mounted and on what.
const mountTable = "/proc/mounts"

// mountedDevice reports the device the filesystem mounted at point is on, as
// the kernel names it. The root filesystem the kernel mounted for itself is
// /dev/root there, whatever device it actually booted from.
//
// The last entry for a point wins, which is what the kernel means by one: a
// filesystem mounted over another is the one a path there reaches.
func mountedDevice(table io.Reader, point string) (string, error) {
	want := filepath.Clean(point)
	device := ""
	lines := bufio.NewScanner(table)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) < 2 {
			continue
		}
		if filepath.Clean(unescapeMount(fields[1])) == want {
			device = unescapeMount(fields[0])
		}
	}
	if err := lines.Err(); err != nil {
		return "", fmt.Errorf("reading the mount table: %w", err)
	}
	if device == "" {
		return "", fmt.Errorf("nothing is mounted on %s", point)
	}
	return device, nil
}

// unescapeMount undoes the escaping the kernel writes a mount table with: a
// space, a tab, a newline or a backslash in a device or a mount point is three
// octal digits behind a backslash, because the fields themselves are separated
// by spaces.
func unescapeMount(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for index := 0; index < len(field); index++ {
		if field[index] != '\\' || index+3 >= len(field) {
			out.WriteByte(field[index])
			continue
		}
		var value byte
		digits := field[index+1 : index+4]
		valid := true
		for _, digit := range []byte(digits) {
			if digit < '0' || digit > '7' {
				valid = false
				break
			}
			value = value*8 + (digit - '0')
		}
		if !valid {
			out.WriteByte(field[index])
			continue
		}
		out.WriteByte(value)
		index += 3
	}
	return out.String()
}

// followDevice resolves the node a mount table's device name leads to. The
// kernel lists the root filesystem's device as /dev/root, which devtmpfs never
// creates: the guest's init makes it a symlink to the device root= named, and
// this follows it. Anything that is already a node is itself.
func followDevice(device string) (string, error) {
	node, err := filepath.EvalSymlinks(device)
	if err != nil {
		return "", fmt.Errorf("the device %s the mount table names: %w", device, err)
	}
	return node, nil
}

// The fields of an ext4 superblock this reads, by their offset in it. It begins
// 1024 bytes into the device, whatever the block size, and every one of these
// is little-endian however the machine reading it is.
const (
	superblockOffset = 1024
	superblockBytes  = 1024

	blocksCountLow  = 0x04
	logBlockSize    = 0x18
	superblockMagic = 0x38
	featureIncompat = 0x60
	blocksCountHigh = 0x150

	// ext4Magic is what says this is an ext2, ext3 or ext4 filesystem at all.
	ext4Magic = 0xef53
	// incompat64Bit is the feature that makes the block count a 64-bit number.
	// Without it the high half is not a number at all and must not be read.
	incompat64Bit = 0x80
	// logBlockSizeMax is the largest shift ext4 defines, which is 64 KiB
	// blocks. Anything beyond it is a superblock this did not understand.
	logBlockSizeMax = 6
)

// filesystemBlocks is the block size of the filesystem on a device and how many
// blocks it has, read out of its superblock.
//
// The device is opened read-only, which is the whole point: this runs on a
// guest whose root filesystem is on the device being asked about, and a 6.18
// kernel refuses a write open of a device something has mounted.
func filesystemBlocks(device string) (blockBytes, blocks uint64, err error) {
	file, err := os.Open(device)
	if err != nil {
		return 0, 0, fmt.Errorf("the device %s: %w", device, err)
	}
	defer file.Close()
	raw := make([]byte, superblockBytes)
	if _, err := file.ReadAt(raw, superblockOffset); err != nil {
		return 0, 0, fmt.Errorf("reading the superblock of %s: %w", device, err)
	}
	blockBytes, blocks, err = parseSuperblock(raw)
	if err != nil {
		return 0, 0, fmt.Errorf("the superblock of %s: %w", device, err)
	}
	return blockBytes, blocks, nil
}

// parseSuperblock reads the block size and the block count out of the bytes of
// one ext4 superblock.
func parseSuperblock(raw []byte) (blockBytes, blocks uint64, err error) {
	if len(raw) < superblockBytes {
		return 0, 0, fmt.Errorf("%d bytes is not a superblock", len(raw))
	}
	if magic := binary.LittleEndian.Uint16(raw[superblockMagic:]); magic != ext4Magic {
		return 0, 0, fmt.Errorf("the magic is %#04x, and an ext4 filesystem's is %#04x",
			magic, ext4Magic)
	}
	shift := binary.LittleEndian.Uint32(raw[logBlockSize:])
	if shift > logBlockSizeMax {
		return 0, 0, fmt.Errorf("it says its blocks are 1024 << %d bytes", shift)
	}
	blocks = uint64(binary.LittleEndian.Uint32(raw[blocksCountLow:]))
	if binary.LittleEndian.Uint32(raw[featureIncompat:])&incompat64Bit != 0 {
		blocks |= uint64(binary.LittleEndian.Uint32(raw[blocksCountHigh:])) << 32
	}
	return 1024 << shift, blocks, nil
}

// blocksToTake is the block count a filesystem of blockBytes blocks may have on
// a device of deviceBytes: every whole block that fits in it. The remainder of
// a device that is not a whole number of them stays unused, because a
// filesystem has no partial block at its end.
//
// A device that holds fewer blocks than the filesystem already has is refused.
// The end of a filesystem is not the device's to cut: a volume may only grow,
// and one that appears to have shrunk is a guest being told something is wrong
// rather than a filesystem being truncated over its own data.
func blocksToTake(deviceBytes, blockBytes, have uint64) (uint64, error) {
	if blockBytes == 0 {
		return 0, fmt.Errorf("the filesystem reports blocks of %d bytes", blockBytes)
	}
	if deviceBytes == 0 {
		return 0, fmt.Errorf("the device reports a size of %d bytes", deviceBytes)
	}
	if have == 0 {
		return 0, fmt.Errorf("the filesystem reports %d blocks", have)
	}
	want := deviceBytes / blockBytes
	if want < have {
		return 0, fmt.Errorf("the device holds %d blocks of %d and the filesystem has %d: "+
			"the end of a filesystem is not a device's to cut", want, blockBytes, have)
	}
	return want, nil
}
