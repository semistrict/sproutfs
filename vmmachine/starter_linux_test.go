//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/guest"
	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/volume"
)

// placedStarter is a Starter the way a program that embeds a host writes one:
// the VMM runs as a jailer leaves it, chrooted and as another user, with the
// process's directory inside the chroot and a device of its own beside the
// managed memory — a read-only PMEM file, as a tools image is. Only paths
// inside the chroot resolve for the VMM, so a host path given to it fails.
type placedStarter struct {
	// jail is the chroot on the host. Its root holds the VMM, its seccomp
	// filter, the kernel and the tools image, and vms the directory of every
	// process.
	jail     string
	owner    vmmachine.Owner
	launches []startedLaunch
}

// startedLaunch is what one Start was asked and what it prepared.
type startedLaunch struct {
	vm      string
	restore bool
	memory  *vmmachine.Memory
	child   *countedChild
	// owners is who each entry of the process's directory belonged to when
	// the VMM started: the listeners unlink their sockets once the VMM has
	// connected.
	owners map[string]string
}

// countedChild counts the Starter's own release of the process.
type countedChild struct {
	*vmmachine.Child
	closed int
}

func (c *countedChild) Close() error {
	c.closed++
	return c.Child.Close()
}

func (s *placedStarter) Boots() bool { return true }

func (s *placedStarter) Start(ctx context.Context, launch *vmmachine.Launch) (vmmachine.VMM, error) {
	name := fmt.Sprintf("%s-%d", launch.VM(), len(s.launches))
	host := filepath.Join(s.jail, "vms", name)
	memory, err := launch.Prepare(ctx, vmmachine.Placement{Directory: host, Within: "/vms/" + name, Owner: &s.owner})
	if err != nil {
		return nil, err
	}
	vsock, vsockWithin := memory.Path("guest.vsock")
	args := []string{"--api-sock", memory.APISocket, "--seccomp-filter", "/seccomp.bpf"}
	if launch.Restore() {
		memory.Load["vsock_override"] = map[string]any{"uds_path": vsockWithin}
	} else {
		document := map[string]any{
			"machine-config": map[string]any{"vcpu_count": 1},
			"boot-source":    map[string]any{"kernel_image_path": "/kernel", "boot_args": guestPmemBootArgs},
			"drives":         []any{},
			"vsock":          map[string]any{"guest_cid": guestVsockCID, "uds_path": vsockWithin},
			"pmem":           []any{map[string]any{"id": "tools", "path_on_host": "/tools.img", "read_only": true}},
		}
		if err := memory.Configure(document); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		path, err := memory.WriteFile(ctx, "boot.json", raw)
		if err != nil {
			return nil, err
		}
		args = append(args, "--config-file", path)
	}
	owners, err := ownersOf(host)
	if err != nil {
		return nil, err
	}
	// The chroot and the change of user happen in the child before its exec,
	// so the process started is the VMM itself, as a jailer that execs leaves it.
	command := exec.Command("/firecracker", args...)
	command.Dir = "/"
	command.SysProcAttr = &syscall.SysProcAttr{Chroot: s.jail,
		Credential: &syscall.Credential{Uid: uint32(s.owner.UID), Gid: uint32(s.owner.GID)}}
	child, err := vmmachine.Spawn(command, vsock)
	if err != nil {
		return nil, err
	}
	counted := &countedChild{Child: child}
	s.launches = append(s.launches, startedLaunch{vm: launch.VM(), restore: launch.Restore(),
		memory: memory, child: counted, owners: owners})
	return counted, nil
}

// newJail builds the chroot a jailer builds for the VMM: the binary and the
// shared libraries it loads, its seccomp filter, the kernel and the tools image
// at the root, the KVM and userfaultfd devices given to the VMM's user, and an
// empty vms directory. The files are hard links where the host allows them.
func newJail(t *testing.T, owner vmmachine.Owner) string {
	t.Helper()
	jail := filepath.Join(t.TempDir(), "jail")
	for _, dir := range []string{jail, filepath.Join(jail, "dev"), filepath.Join(jail, "vms")} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binary := os.Getenv("SPROUTFS_FIRECRACKER")
	jailFile(t, binary, filepath.Join(jail, "firecracker"))
	jailFile(t, os.Getenv("SPROUTFS_FIRECRACKER_SECCOMP"), filepath.Join(jail, "seccomp.bpf"))
	jailFile(t, os.Getenv("SPROUTFS_FIRECRACKER_KERNEL"), filepath.Join(jail, "kernel"))
	for _, library := range sharedLibraries(t, binary) {
		inside := filepath.Join(jail, library)
		if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
			t.Fatal(err)
		}
		jailFile(t, library, inside)
	}
	// Firecracker maps a PMEM file in whole 2 MiB pages.
	if err := os.WriteFile(filepath.Join(jail, "tools.img"), make([]byte, 2<<20), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, device := range []string{"/dev/kvm", "/dev/userfaultfd"} {
		var stat syscall.Stat_t
		if err := syscall.Stat(device, &stat); err != nil {
			t.Fatal(err)
		}
		inside := filepath.Join(jail, device)
		if err := syscall.Mknod(inside, syscall.S_IFCHR|0o600, int(stat.Rdev)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(inside, owner.UID, owner.GID); err != nil {
			t.Fatal(err)
		}
	}
	return jail
}

// jailFile puts the file behind path at inside: a hard link, or a copy when the
// two are on different file systems.
func jailFile(t *testing.T, path, inside string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	err = os.Link(resolved, inside)
	if err == nil {
		return
	}
	if !errors.Is(err, syscall.EXDEV) {
		t.Fatal(err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, data, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
}

// sharedLibraries is every library the dynamic loader maps for binary, the
// loader itself included, by its path on this host.
func sharedLibraries(t *testing.T, binary string) []string {
	t.Helper()
	output, err := exec.Command("ldd", binary).Output()
	if err != nil {
		t.Fatalf("ldd %s: %v", binary, err)
	}
	var libraries []string
	for line := range strings.Lines(string(output)) {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "/") {
				libraries = append(libraries, field)
				break
			}
		}
	}
	return libraries
}

// TestAStarterPlacesTheVMMAndItsDevices: a VM whose Starter is not this
// package's own runs where the Starter put it, on the paths it gave the VMM,
// with the device it added, and comes back from a checkpoint the same way. The
// VMM runs in a chroot as the placement's user, so it reaches its memory only
// through the paths within the chroot and the sockets given to that user. The
// guest's agent answers over the vsock the Starter owns, both after the boot
// and after the restore, and the restore sees the Starter's plain PMEM file as
// the boot did.
func TestAStarterPlacesTheVMMAndItsDevices(t *testing.T) {
	binary := os.Getenv("SPROUTFS_FIRECRACKER")
	if binary == "" {
		t.Skip("run the Firecracker Lima qualification script")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	owner := vmmachine.Owner{UID: 65534, GID: 65534}
	starter := &placedStarter{jail: newJail(t, owner), owner: owner}

	cluster := newMigrationCluster(t, ctx)
	vm, err := cluster.source.Create(ctx, "placed", []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: 128 << 20, PageSize: ramPageBytes(t)},
		{Name: "root", Size: guestRootBytes, PageSize: checkpoint.PageSize2MiB},
	})
	if err != nil {
		t.Fatal(err)
	}
	loadRootImage(t, ctx, vm.Volume("root"))
	if err := vm.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	pagers := newMigrationPager(t, ctx)
	config := migrationConfig(t, binary, pagers, vm)
	config.Starter = starter
	p, err := vmmachine.Start(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	booted := starter.launches[0]
	checkPlaced(t, p, booted, filepath.Join(starter.jail, "vms", "placed-0"), "/vms/placed-0")
	waitLine(t, ctx, p, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
	waitLine(t, ctx, p, fmt.Sprintf("sproutfs-guest-agent: serving on vsock port %d", guest.Port), 0)
	checkToolsDevice(t, ctx, p)

	ckpt, err := vm.Snapshot(ctx, prepareAndResume(p))
	if err != nil {
		t.Fatalf("capture: %v\n%s", err, consoleText(p))
	}
	if err := ckpt.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(booted.memory.Directory); !os.IsNotExist(err) {
		t.Fatalf("the closed process's directory is still there: %v", err)
	}
	if booted.child.closed != 1 {
		t.Fatalf("the Starter released the closed process %d times, want once", booted.child.closed)
	}

	point, err := cluster.source.Inherit(ctx, ckpt.Ref())
	if err != nil {
		t.Fatal(err)
	}
	fork, err := cluster.source.Fork(ctx, "restored", point)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fork.Close(context.Background()) })
	if err := fork.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	restoring := config
	restoring.VM = fork
	restoring.RestoreState = ckpt.State()
	restored, err := vmmachine.Start(ctx, restoring)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	if err := restored.Release(ctx); err != nil {
		t.Fatal(err)
	}
	checkPlaced(t, restored, starter.launches[1], filepath.Join(starter.jail, "vms", "restored-1"), "/vms/restored-1")
	if !starter.launches[1].restore {
		t.Fatal("the Starter was not told the second launch restores")
	}
	checkToolsDevice(t, ctx, restored)
}

// checkPlaced holds one process to its Starter's placement: its directory is the
// one the Starter chose, every path the VMM was given is under the Starter's
// view of it, and the sockets the VMM connects to belong to the VMM's user.
func checkPlaced(t *testing.T, p *vmmachine.Process, launched startedLaunch, host, view string) {
	t.Helper()
	if p.Directory() != host {
		t.Fatalf("the process runs in %s, want %s", p.Directory(), host)
	}
	memory := launched.memory
	given := []string{memory.APISocket, memory.RAM}
	for _, device := range memory.Pmem {
		given = append(given, device.Socket)
	}
	for _, path := range given {
		if !strings.HasPrefix(path, view+"/") {
			t.Fatalf("the VMM was given %s, which is not under its view %s", path, view)
		}
	}
	want := map[string]string{".": "65534:65534", "ram.sock": "65534:65534",
		"pmem-0.sock": "65534:65534", "state": "65534:65534"}
	if !launched.restore {
		want["state/boot.json"] = "65534:65534"
	}
	if !maps.Equal(launched.owners, want) {
		t.Fatalf("the prepared directory belonged to %v, want %v", launched.owners, want)
	}
}

// ownersOf is who owns every entry of a directory, by its path in it.
func ownersOf(dir string) (map[string]string, error) {
	owners := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		owners[relative] = fmt.Sprintf("%d:%d", stat.Uid, stat.Gid)
		return nil
	})
	return owners, err
}

// checkToolsDevice asks the guest's agent, over the Starter's vsock, whether the
// Starter's own PMEM device is there beside the managed root.
func checkToolsDevice(t *testing.T, ctx context.Context, p *vmmachine.Process) {
	t.Helper()
	result, err := guestExec(ctx, p, guest.ExecRequest{Cmd: "test -b /dev/pmem1 && echo tools"})
	if err != nil {
		t.Fatalf("the guest did not answer over the Starter's vsock: %v\n%s", err, consoleText(p))
	}
	if result.Stdout != "tools\n" || result.Exit != 0 {
		t.Fatalf("the guest has no second PMEM device: %+v", result)
	}
}
