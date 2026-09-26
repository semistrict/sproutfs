//go:build linux && (amd64 || arm64)

package vmmemory_test

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/semistrict/sproutfs/internal/testjail"
)

// A read-only descriptor refuses every write, but a file's mode is checked
// again when it is opened. A process that holds a descriptor can open the file
// anew through /proc/self/fd, with the access its user has to the file's mode,
// and a file's owner can change that mode with fchmod. The pager runs as a user
// of its own, and every file it makes has mode 0600. So a VMM that the
// embedder's jailer runs as another user can do neither. The helper here is
// such a VMM: a copy of this test binary run as nobody, holding the files a
// session gave it.

// jailedRole makes this test binary a jailed VMM that holds files. Its value
// is how many it holds, at descriptors 3 on.
const jailedRole = "SPROUTFS_VMMEMORY_JAILED"

// jailedAttempt is what the kernel told a jailed VMM about one file it holds:
// the errno of its reopening the file for writing, and of its fchmod. Zero is
// an attempt that worked.
type jailedAttempt struct {
	reopen, fchmod unix.Errno
}

// TestJailedVMM is the jailed VMM of jailAttempts. It is not a test of its
// own. For each file it holds it tries both, and writes one line of the two
// errnos' numbers.
func TestJailedVMM(t *testing.T) {
	role := os.Getenv(jailedRole)
	if role == "" {
		return
	}
	files, err := strconv.Atoi(role)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for fd := 3; fd < 3+files; fd++ {
		reopened, err := unix.Open(fmt.Sprintf("/proc/self/fd/%d", fd), unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err == nil {
			_ = unix.Close(reopened)
		}
		fmt.Println(int(errnoOf(err)), int(errnoOf(unix.Fchmod(fd, 0o666))))
	}
	os.Exit(0)
}

// errnoOf is the errno a call failed with, and zero for one that worked.
func errnoOf(err error) unix.Errno {
	var errno unix.Errno
	if err != nil && !errors.As(err, &errno) {
		errno = unix.EIO
	}
	return errno
}

// jailAttempts hands files to a jailed VMM, which runs as nobody, and reports
// what the kernel told it about each.
func jailAttempts(t *testing.T, files []*os.File) []jailedAttempt {
	t.Helper()
	cmd := testjail.Command(t, testjail.Nobody, "TestJailedVMM", jailedRole, strconv.Itoa(len(files)))
	cmd.ExtraFiles = files
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("the jailed VMM: %v: %s", err, stderr.String())
	}
	var attempts []jailedAttempt
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		var reopen, fchmod int
		if _, err := fmt.Sscanf(scanner.Text(), "%d %d", &reopen, &fchmod); err != nil {
			t.Fatalf("the jailed VMM wrote %q: %v", scanner.Text(), err)
		}
		attempts = append(attempts, jailedAttempt{unix.Errno(reopen), unix.Errno(fchmod)})
	}
	if len(attempts) != len(files) {
		t.Fatalf("the jailed VMM reported %d files, want the %d it holds: %q", len(attempts), len(files), output)
	}
	return attempts
}
