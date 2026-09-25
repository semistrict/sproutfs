//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/guest"
	"github.com/semistrict/sproutfs/vmmachine"
)

// grownRootBytes is what the root volume becomes at the cold start below. A
// cold boot is the one moment a VM's shape can change, and the pages a volume
// grows into read as zeroes: the filesystem on it is still the size it was, and
// the guest has to take the rest for itself.
const grownRootBytes = 2 * guestRootBytes

// guestWitness is where the qualification image carries the guest witness,
// which is the binary that does the taking.
const guestWitness = "/bin/sproutfs-guest-witness"

// The witness's own paths in this guest. Its socket and its log are on
// devtmpfs, so that the only thing on the filesystem being grown is the file
// whose bytes are the point — and because this image, which is an init and a
// busybox, has no /tmp for the log to default to.
const (
	witnessFile   = "/witness"
	witnessSocket = "/dev/sproutfs-witness.sock"
	witnessLog    = "/dev/sproutfs-witness.log"
)

// TestAGuestGrowsItsFilesystemOverARootVolumeThatGrew: a VM cold started onto a
// larger root volume comes back with a filesystem that ends well before its
// device does, and everything past that end is unreachable until the guest
// takes it.
//
// resize2fs cannot do the taking. Its online path opens the block device
// read-write, and this kernel — 6.18, the one the deployment runs — refuses a
// write open of a device something has mounted, with no knob to allow it. So a
// guest whose root filesystem is on the device resize2fs would have to open is
// told EBUSY before anything begins, which is exactly what the soak on the
// cluster ran into.
//
// What works is EXT4_IOC_RESIZE_FS on a descriptor of the mount point, which is
// what `witness grow` issues, and the only place that can be qualified is here:
// a real guest, on a real PMEM root device, with the filesystem mounted.
func TestAGuestGrowsItsFilesystemOverARootVolumeThatGrew(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	vm := newGuestVM(t, ctx, "grown")

	// The cold start's own publication: the memory is discarded and the root
	// volume grows, which is the one moment either may change.
	if err := vm.DiscardMemory(ctx, vmmachine.RAMVolume,
		map[string]uint64{"root": grownRootBytes}); err != nil {
		t.Fatal(err)
	}
	if size := vm.Volume("root").Size(); size != grownRootBytes {
		t.Fatalf("the root volume is %d bytes after the cold start, want %d", size, grownRootBytes)
	}
	p := bootGuestWithAgent(t, ctx, binaryPath, vm)

	// The guest's own filesystem is still the size the image was, and the
	// witness file it is about to hold has to survive being grown over.
	before := rootKilobytes(t, ctx, p)
	if before*1024 >= grownRootBytes {
		t.Fatalf("the filesystem is already %d KiB, so this guest has nothing to grow", before)
	}
	runInGuest(t, ctx, p, fmt.Sprintf("%s fill --memory 4M --disk %s --socket %s --log %s --seed 7",
		guestWitness, witnessFile, witnessSocket, witnessLog))

	grown := runInGuest(t, ctx, p, fmt.Sprintf("%s grow /", guestWitness))
	want := fmt.Sprintf("grew from %d to %d bytes", guestRootBytes, grownRootBytes)
	if !strings.Contains(grown.Stdout, want) {
		t.Fatalf("the grow said %q, want it to say %q", grown.Stdout, want)
	}

	// And the filesystem says so itself, asked the way anything in the guest
	// would ask: statfs, through df. What it reports is the data blocks, net of
	// the journal and the per-group metadata, so it is not the device's size —
	// it is most of the pages the volume gained, which is the point.
	after := rootKilobytes(t, ctx, p)
	gained, added := (after-before)*1024, uint64(grownRootBytes-guestRootBytes)
	if after <= before || gained < added/2 {
		t.Fatalf("df reports %d KiB after the grow and %d before, "+
			"want most of the %d bytes the volume gained", after, before, added)
	}

	// The file the witness wrote is still every byte of what it wrote, read
	// back through the filesystem that was grown over it.
	runInGuest(t, ctx, p, fmt.Sprintf("%s check --seed 7 --step 0 --socket %s", guestWitness, witnessSocket))

	// A second grow has nothing to take and says so rather than failing: a cold
	// start that did not grow the volume leaves exactly this, and the soak asks
	// every cold-started guest the same question.
	again := runInGuest(t, ctx, p, fmt.Sprintf("%s grow /", guestWitness))
	if !strings.Contains(again.Stdout, "already fills") {
		t.Fatalf("growing a filesystem that fills its device said %q", again.Stdout)
	}
}

// rootKilobytes is what the guest's own df says the root filesystem holds, in
// 1 KiB units. It is statfs by another name, which is the question `witness
// grow` answers with and therefore the one worth asking independently.
func rootKilobytes(t *testing.T, ctx context.Context, p *vmmachine.Process) uint64 {
	t.Helper()
	printed := runInGuest(t, ctx, p, "df -k /").Stdout
	// A heading, then the one filesystem: the second line's second field is how
	// many 1 KiB blocks it holds.
	lines := strings.Split(strings.TrimSpace(printed), "\n")
	if len(lines) != 2 {
		t.Fatalf("df printed %q, want a heading and one filesystem", printed)
	}
	fields := strings.Fields(lines[1])
	if len(fields) < 2 {
		t.Fatalf("df printed %q", printed)
	}
	blocks, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("df printed %q: %v", printed, err)
	}
	return blocks
}

// runInGuest runs one command in the guest and requires it to have succeeded, which is
// what every step of this suite wants: a witness that refused is the failure
// being tested for, and its own message is the whole diagnosis.
func runInGuest(t *testing.T, ctx context.Context, p *vmmachine.Process, command string) guest.ExecResult {
	t.Helper()
	result, err := guestExec(ctx, p, guest.ExecRequest{Cmd: command, Timeout: 120})
	if err != nil {
		t.Fatalf("running %q in the guest: %v\n%s", command, err, consoleText(p))
	}
	if result.Exit != 0 {
		t.Fatalf("%q exited %d in the guest\nstdout: %s\nstderr: %s",
			command, result.Exit, result.Stdout, result.Stderr)
	}
	return result
}
