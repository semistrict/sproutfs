// Package testjail runs a copy of the test binary as another user, which is
// how a test plays a jailed VMM. The embedder's jailer runs a VMM as a user of
// its own, and not as the pager's. A test process runs as root in the Lima and
// GCE suites, so it can start such a process.
package testjail

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// Nobody is the user a jailed VMM runs as.
var Nobody = &syscall.Credential{Uid: 65534, Gid: 65534}

// Executable is this test binary, as a user may run it. The binary itself may
// sit where another user cannot read it, so for another user it is a copy in a
// directory anyone may read. owner nil is this process's user.
func Executable(t testing.TB, owner *syscall.Credential) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if owner == nil {
		return executable
	}
	dir, err := os.MkdirTemp("", "sproutfs-jail-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(dir, filepath.Base(executable))
	if err := os.WriteFile(copied, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	return copied
}

// Command runs the one test of this binary named, as owner. The test plays a
// role, and the environment variable role names says so.
func Command(t testing.TB, owner *syscall.Credential, test, role, value string) *exec.Cmd {
	t.Helper()
	executable := Executable(t, owner)
	cmd := exec.Command(executable, "-test.run=^"+test+"$")
	cmd.Dir = filepath.Dir(executable)
	cmd.Env = append(os.Environ(), role+"="+value)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: owner}
	return cmd
}
