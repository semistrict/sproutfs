//go:build linux && (amd64 || arm64)

package vmwire_test

import (
	"os"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmwire"
)

// A userfaultfd ioctl that fails must say which ioctl it was. Resolving a run
// is three of them and a seal's protection is a fourth, and the pager reports
// what they return as the reason a VM's memory ended — so an errno on its own
// leaves a reader of that line with four calls to choose between and no way to
// tell which range was being installed, woken or protected.
//
// A descriptor that is not a userfaultfd answers every one of them the same
// way, which is what makes this the whole of the naming without a kernel-mode
// UFFD to fail on.
func TestAFailedUserfaultfdIoctlNamesItself(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	const page = checkpoint.PageSize4KiB
	address := uint64(os.Getpagesize()) * 16 // never mapped; the ioctl fails first
	for _, call := range []struct {
		name string
		want string
		run  func() error
	}{
		{"protect", "UFFDIO_WRITEPROTECT", func() error {
			return vmwire.ProtectRange(reader.Fd(), address, page)
		}},
		{"wake", "UFFDIO_WAKE", func() error {
			return vmwire.WakeRange(reader.Fd(), address, page)
		}},
		{"resolve", "UFFDIO_CONTINUE", func() error {
			return vmwire.Resolve(reader.Fd(), address, page, page, true)
		}},
	} {
		t.Run(call.name, func(t *testing.T) {
			err := call.run()
			if err == nil {
				t.Fatalf("%s on a pipe reported success", call.name)
			}
			if !strings.Contains(err.Error(), call.want) {
				t.Fatalf("%s failed with %q, want it to name %s", call.name, err, call.want)
			}
			if !strings.Contains(err.Error(), "inappropriate ioctl for device") {
				t.Fatalf("%s failed with %q, want it to carry the errno", call.name, err)
			}
		})
	}
}
