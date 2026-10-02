//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/volume"
)

// suiteJails is the jail each test's VMMs run in where SPROUTFS_FIRECRACKER_JAIL=1
// asks the suite to run every VMM jailed, one per test so that the VMMs of one
// test have users of their own.
var suiteJails sync.Map // *testing.T -> *vmmachine.Jail

// suiteJail is the jail a test's VMMs run in, nil where the suite runs them
// unjailed.
func suiteJail(t *testing.T) *vmmachine.Jail {
	t.Helper()
	if os.Getenv("SPROUTFS_FIRECRACKER_JAIL") != "1" {
		return nil
	}
	if jail, ok := suiteJails.Load(t); ok {
		return jail.(*vmmachine.Jail)
	}
	jail := testJail(t, 64)
	suiteJails.Store(t, jail)
	t.Cleanup(func() { suiteJails.Delete(t) })
	return jail
}

// testJail is a jail of users users under the test's own directory, whose /dev
// is unmounted once the test's VMMs have gone.
func testJail(t *testing.T, users int) *vmmachine.Jail {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("a jail needs root to build and to change a VMM's user; run the Firecracker qualification")
	}
	jail := &vmmachine.Jail{Root: filepath.Join(t.TempDir(), "jail"), FirstUID: 200000, UIDs: users, GID: 200000}
	t.Cleanup(func() {
		if err := jail.Close(); err != nil {
			t.Errorf("closing the jail: %v", err)
		}
	})
	return jail
}

// vmmUser is the real user a process runs as, and the root it sees.
func vmmUser(t *testing.T, pid int) (int, string) {
	t.Helper()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	uid := -1
	for line := range strings.Lines(string(status)) {
		if fields := strings.Fields(line); len(fields) > 1 && fields[0] == "Uid:" {
			if _, err := fmt.Sscan(fields[1], &uid); err != nil {
				t.Fatal(err)
			}
		}
	}
	root, err := os.Readlink(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		t.Fatal(err)
	}
	return uid, root
}

// TestJailedVMMsRunAsUsersOfTheirOwn: this package's own Starter, given a
// jail, runs each VMM chrooted into it as a user of its own out of the jail's
// range, never root and never this process's user, and the guest boots there.
// Two VMMs run as two users. A start with every user taken is refused, and a
// user comes back once its VMM has gone.
func TestJailedVMMsRunAsUsersOfTheirOwn(t *testing.T) {
	binary := os.Getenv("SPROUTFS_FIRECRACKER")
	if binary == "" {
		t.Skip("run the Firecracker qualification")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	jail := testJail(t, 2)
	cluster := newMigrationCluster(t, ctx)
	pagers := newMigrationPager(t, ctx)
	start := func(name string) (*vmmachine.Process, error) {
		vm, err := cluster.source.Create(ctx, name, []volume.VolumeSpec{
			{Name: vmmachine.RAMVolume, Size: 128 << 20, PageSize: ramPageBytes(t)},
			{Name: "root", Size: guestRootBytes, PageSize: pmemPageBytes(t)},
		})
		if err != nil {
			t.Fatal(err)
		}
		loadRootImage(t, ctx, vm.Volume("root"))
		if err := vm.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
		config := migrationConfig(t, binary, pagers, vm)
		config.Starter.(*vmmachine.Firecracker).Jail = jail
		return vmmachine.Start(ctx, config)
	}
	users := map[int]bool{}
	var running []*vmmachine.Process
	for _, name := range []string{"one", "two"} {
		p, err := start(name)
		if err != nil {
			t.Fatalf("starting %s jailed: %v", name, err)
		}
		t.Cleanup(func() { _ = p.Close() })
		waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
		uid, root := vmmUser(t, p.PID())
		if uid < 200000 || uid >= 200002 || uid == os.Getuid() {
			t.Fatalf("%s's VMM runs as user %d, want one of the jail's 200000 and 200001", name, uid)
		}
		if root != jail.Root {
			t.Fatalf("%s's VMM sees %s as its root, want the jail %s", name, root, jail.Root)
		}
		users[uid] = true
		running = append(running, p)
	}
	if len(users) != 2 {
		t.Fatalf("the two VMMs run as users %v, want two of their own", users)
	}
	if _, err := start("three"); err == nil || !strings.Contains(err.Error(), "every one of the jail's 2 users runs a VMM") {
		t.Fatalf("a third start with both users taken = %v, want it refused", err)
	}
	if err := running[0].Close(); err != nil {
		t.Fatal(err)
	}
	p, err := start("four")
	if err != nil {
		t.Fatalf("starting after a VMM gave its user back: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
}
