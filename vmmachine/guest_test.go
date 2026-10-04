package vmmachine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// kernelImage is a stand-in for a kernel image: megabytes of other bytes with
// the driver's name at offset, or without it where driver is nil.
func kernelImage(driver []byte, offset int) []byte {
	image := bytes.Repeat([]byte{0x90}, 3<<20)
	if driver != nil {
		copy(image[offset:], driver)
	}
	return image
}

// TestAGuestNeedsTheGenerationDriverAndToSeeTheDevice: a kernel with the
// VMGenID driver built in passes on its own architecture, wherever in the image
// the driver's name falls, a megabyte boundary included. A kernel without it is
// refused, and so on x86_64 is a command line that keeps the guest from reading
// the ACPI tables the device is in.
func TestAGuestNeedsTheGenerationDriverAndToSeeTheDevice(t *testing.T) {
	simulated(t, func(t *testing.T, ctx context.Context) {
		const args = "console=ttyS0 reboot=k panic=1 init=/init rootfstype=ext4 rootflags=dax=always"
		for arch, driver := range generationDriver {
			for _, offset := range []int{0, 1<<20 - 4, 2<<20 + 17, 3<<20 - len(driver)} {
				// One byte a read, so the name is split across reads too.
				if err := checkGuest(ctx, iotest.OneByteReader(bytes.NewReader(kernelImage(driver, offset))), args, arch); err != nil {
					t.Fatalf("%s at %d: %v", arch, offset, err)
				}
				if err := checkGuest(ctx, bytes.NewReader(kernelImage(driver, offset)), args, arch); err != nil {
					t.Fatalf("%s at %d: %v", arch, offset, err)
				}
			}
			err := checkGuest(ctx, bytes.NewReader(kernelImage(nil, 0)), args, arch)
			if err == nil || err.Error() != "the guest kernel has no VMGenID driver built in (CONFIG_VMGENID=y)" {
				t.Fatalf("%s without the driver: %v", arch, err)
			}
		}
		// The other architecture's driver is not this one's.
		if err := checkGuest(ctx, bytes.NewReader(kernelImage(generationDriver["arm64"], 0)), args, "amd64"); err == nil {
			t.Fatal("an aarch64 kernel passed on x86_64")
		}
		for _, hidden := range []string{"acpi=off", "acpi=ht"} {
			err := checkGuest(ctx, bytes.NewReader(kernelImage(generationDriver["amd64"], 0)), args+" "+hidden, "amd64")
			if err == nil || !strings.Contains(err.Error(), "carry "+hidden+", which hides the VMGenID device") {
				t.Fatalf("%s: %v", hidden, err)
			}
			// aarch64's device is in the device tree, which ACPI does not hide.
			if err := checkGuest(ctx, bytes.NewReader(kernelImage(generationDriver["arm64"], 0)), args+" "+hidden, "arm64"); err != nil {
				t.Fatalf("%s on aarch64: %v", hidden, err)
			}
		}
	})
}

// TestCheckGuestAsksAFirecrackerOfItsKernel: a host refuses a Firecracker
// whose kernel lacks the driver, as ErrGuestDevices, and one with no kernel
// boots nothing and has nothing to check.
func TestCheckGuestAsksAFirecrackerOfItsKernel(t *testing.T) {
	simulated(t, func(t *testing.T, ctx context.Context) {
		directory := t.TempDir()
		without := filepath.Join(directory, "without")
		if err := os.WriteFile(without, kernelImage(nil, 0), 0o600); err != nil {
			t.Fatal(err)
		}
		err := CheckGuest(ctx, &Firecracker{Kernel: without})
		if !errors.Is(err, ErrGuestDevices) {
			t.Fatalf("got %v, want ErrGuestDevices", err)
		}
		if err := CheckGuest(ctx, &Firecracker{}); err != nil {
			t.Fatalf("a Firecracker that only restores: %v", err)
		}
	})
}
