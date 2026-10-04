package vmmachine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/semistrict/sproutfs/platform/sim"
)

// ErrGuestDevices refuses a Starter whose guests would not act on a new
// generation ID. Every restore gives the guest one (see loadRequest), and a
// guest kernel reseeds its random pool from it only if it has the driver and
// can see the device. Without that, every child of one fork point draws the
// same random bytes, and so does every restore of one checkpoint.
var ErrGuestDevices = errors.New("vmmachine: the guest would not act on a new generation ID")

// GuestChecker is a Starter that can say, before any VM runs, whether the
// guests it boots will act on a new generation ID. A host refuses to start a
// Starter that says they will not. A Starter that boots a kernel of its own
// choosing implements it; see CheckGuest.
type GuestChecker interface {
	CheckGuest(ctx context.Context) error
}

// CheckGuest refuses a Starter whose guests would not act on a new generation
// ID. A Starter that is not a GuestChecker is the embedder's to check.
func CheckGuest(ctx context.Context, starter Starter) error {
	checker, ok := starter.(GuestChecker)
	if !ok {
		return nil
	}
	if err := checker.CheckGuest(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrGuestDevices, err)
	}
	return nil
}

// CheckGuest refuses a kernel built without the VMGenID driver, and boot
// arguments that hide the device from it. Firecracker always gives a guest the
// device: in its ACPI tables on x86_64 and in its device tree on aarch64. A
// Firecracker without a kernel only restores guests that booted elsewhere, and
// has nothing to check.
func (f *Firecracker) CheckGuest(ctx context.Context) error {
	if f.Kernel == "" {
		return nil
	}
	kernel, err := os.Open(f.Kernel)
	if err != nil {
		return err
	}
	defer kernel.Close()
	return checkGuest(ctx, kernel, f.BootArgs, runtime.GOARCH)
}

// generationDriver is what a kernel image holds where the VMGenID driver is
// built in: the name it matches the device by, which on x86_64 is the ACPI ID
// Firecracker gives the device and on aarch64 its device tree compatible.
// Firecracker loads an uncompressed kernel on both, so the name is there as
// it is in the driver's own table.
var generationDriver = map[string][]byte{
	"amd64": []byte("VMGENCTR\x00"),
	"arm64": []byte("microsoft,vmgenid\x00"),
}

// checkGuest is CheckGuest over a kernel image and a command line, on arch.
func checkGuest(ctx context.Context, kernel io.Reader, bootArgs, arch string) error {
	driver, known := generationDriver[arch]
	if !known {
		return fmt.Errorf("vmmachine: no VMGenID driver is known on %s", arch)
	}
	if arch == "amd64" {
		// The device is in the ACPI tables, so a guest that does not read
		// them, or reads them without the interpreter, never finds it.
		for _, field := range strings.Fields(bootArgs) {
			if (field == "acpi=off" || field == "acpi=ht") && !sim.Bug(ctx, "vmmachine-accept-guest-without-vmgenid") {
				return fmt.Errorf("the boot arguments carry %s, which hides the VMGenID device from the guest", field)
			}
		}
	}
	found, err := contains(kernel, driver)
	if err != nil {
		return fmt.Errorf("reading the guest kernel: %w", err)
	}
	if !found && !sim.Bug(ctx, "vmmachine-accept-guest-without-vmgenid") {
		return errors.New("the guest kernel has no VMGenID driver built in (CONFIG_VMGENID=y)")
	}
	return nil
}

// contains reports whether a stream holds needle, reading it a megabyte at a
// time and keeping the tail of each read so a needle across two reads is found.
func contains(stream io.Reader, needle []byte) (bool, error) {
	buffer := make([]byte, 0, (1<<20)+len(needle))
	chunk := make([]byte, 1<<20)
	for {
		count, err := stream.Read(chunk)
		buffer = append(buffer, chunk[:count]...)
		if bytes.Contains(buffer, needle) {
			return true, nil
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		// What is kept is too short to hold the needle, so it is only ever
		// found again together with the next read.
		keep := min(len(buffer), len(needle)-1)
		buffer = append(buffer[:0], buffer[len(buffer)-keep:]...)
	}
}
