//go:build linux && (amd64 || arm64)

package vmmachine

import (
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// The jail's own names for what it holds, which are what the VMM is given.
const (
	jailedBinary   = "/firecracker"
	jailedSeccomp  = "/seccomp.bpf"
	jailedKernel   = "/kernel"
	jailedInitrd   = "/initrd"
	jailedVMs      = "/vms"
	jailedDevices  = "/dev"
	jailDeviceSize = "64k"
)

// jailDevices are the devices a VMM opens by these names: KVM, and the
// userfaultfd device its memory sessions register the guest's memory with.
var jailDevices = []string{"/dev/kvm", "/dev/userfaultfd"}

// place builds the jail the first time and takes a user for one VMM. The
// placement puts the process's directory under the jail's vms, named for this
// start alone and given to that user: the VMM's sockets are in it, and a Unix
// socket's path has room for barely a hundred bytes.
func (j *Jail) place(f *Firecracker) (Placement, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.built {
		if err := j.build(f); err != nil {
			return Placement{}, err
		}
		j.built, j.inUse = true, map[int]bool{}
	}
	uid := -1
	for offset := range j.UIDs {
		if candidate := j.FirstUID + offset; !j.inUse[candidate] {
			uid = candidate
			break
		}
	}
	if uid < 0 {
		return Placement{}, fmt.Errorf("vmmachine: every one of the jail's %d users runs a VMM", j.UIDs)
	}
	j.inUse[uid] = true
	j.next++
	name := strconv.Itoa(j.next)
	return Placement{Directory: filepath.Join(j.Root, jailedVMs, name), Within: filepath.Join(jailedVMs, name),
		Owner: &Owner{UID: uid, GID: j.GID}}, nil
}

// Close unmounts the jail's /dev once no VMM runs in it. The rest of the jail
// stays for the next host to remove, which it does before it builds its own; a
// start after Close builds the jail again.
func (j *Jail) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.built {
		return nil
	}
	if len(j.inUse) > 0 {
		return fmt.Errorf("vmmachine: %d VMMs still run in the jail", len(j.inUse))
	}
	j.built = false
	return syscall.Unmount(filepath.Join(j.Root, jailedDevices), syscall.MNT_DETACH)
}

// give hands back the user of a VMM that has exited.
func (j *Jail) give(uid int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	delete(j.inUse, uid)
}

// check refuses a jail that would run a VMM as root or as this process's user,
// which is what the isolated arena cannot hold a VMM to.
func (j *Jail) check() error {
	host := os.Getuid()
	switch {
	case !filepath.IsAbs(j.Root):
		return fmt.Errorf("vmmachine: the jail's root is not absolute: %q", j.Root)
	case j.FirstUID <= 0 || j.UIDs < 1 || j.GID <= 0:
		return fmt.Errorf("vmmachine: the jail's users %d+%d and group %d must be neither root nor empty",
			j.FirstUID, j.UIDs, j.GID)
	case host >= j.FirstUID && host < j.FirstUID+j.UIDs:
		return fmt.Errorf("vmmachine: the jail's users %d+%d include this process's own, %d", j.FirstUID, j.UIDs, host)
	}
	return nil
}

// build makes the jail's root afresh: whatever a host before this one left
// there is removed first, its /dev unmounted, so the jail holds exactly this
// host's VMM, policy and kernel. Caller holds mu.
func (j *Jail) build(f *Firecracker) error {
	if err := j.check(); err != nil {
		return err
	}
	if err := static(f.Binary); err != nil {
		return err
	}
	devices := filepath.Join(j.Root, jailedDevices)
	if err := syscall.Unmount(devices, syscall.MNT_DETACH); err != nil &&
		!errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOENT) {
		return fmt.Errorf("vmmachine: unmounting the jail's old /dev: %w", err)
	}
	if err := os.RemoveAll(j.Root); err != nil {
		return err
	}
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{{j.Root, 0o755}, {filepath.Join(j.Root, jailedVMs), 0o711}, {devices, 0o755}} {
		if err := os.Mkdir(dir.path, dir.mode); err != nil {
			return err
		}
		// A umask narrows Mkdir's mode; the jail's are exactly these.
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			return err
		}
	}
	// A /dev of its own, on a file system that allows device nodes whatever
	// the one under the root allows, so the node's own devices are not touched.
	if err := syscall.Mount("tmpfs", devices, "tmpfs", syscall.MS_NOSUID|syscall.MS_NOEXEC,
		"mode=0755,size="+jailDeviceSize); err != nil {
		return fmt.Errorf("vmmachine: mounting the jail's /dev: %w", err)
	}
	for _, device := range jailDevices {
		if err := j.device(device); err != nil {
			return err
		}
	}
	files := []struct{ from, to string }{{f.Binary, jailedBinary}, {f.SeccompFilter, jailedSeccomp},
		{f.Kernel, jailedKernel}, {f.Initrd, jailedInitrd}}
	for _, file := range files {
		if file.from == "" {
			continue
		}
		mode := os.FileMode(0o444)
		if file.to == jailedBinary {
			mode = 0o555
		}
		if err := install(file.from, filepath.Join(j.Root, file.to), mode); err != nil {
			return err
		}
	}
	return nil
}

// device makes one of the host's devices in the jail's /dev, owned by root and
// the VMMs' group, which alone may open it.
func (j *Jail) device(path string) error {
	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		return fmt.Errorf("vmmachine: the jail needs %s: %w", path, err)
	}
	inside := filepath.Join(j.Root, path)
	if err := syscall.Mknod(inside, syscall.S_IFCHR|0o660, int(stat.Rdev)); err != nil {
		return fmt.Errorf("vmmachine: making %s in the jail: %w", path, err)
	}
	if err := os.Chown(inside, 0, j.GID); err != nil {
		return err
	}
	return os.Chmod(inside, 0o660)
}

// static refuses a VMM that loads shared libraries, which the jail does not
// hold.
func static(binary string) error {
	file, err := elf.Open(binary)
	if err != nil {
		return fmt.Errorf("vmmachine: reading the VMM %s: %w", binary, err)
	}
	defer file.Close()
	for _, program := range file.Progs {
		if program.Type == elf.PT_INTERP {
			return fmt.Errorf("vmmachine: the VMM %s is dynamically linked, and the jail holds no libraries", binary)
		}
	}
	return nil
}

// install copies one of the host's files into the jail, owned by root with the
// given mode. It is a copy rather than a link: the jail is on the host's
// scratch, which is rarely the file system the image put the file on.
func install(from, to string, mode os.FileMode) error {
	source, err := os.Open(from)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(target, source); err != nil {
		return errors.Join(err, target.Close())
	}
	if err := target.Close(); err != nil {
		return err
	}
	return os.Chmod(to, mode)
}

// jailedChild is a VMM that runs in a jail, whose user goes back to the jail
// once it has exited.
type jailedChild struct {
	*Child
	jail *Jail
	uid  int
}

func (c *jailedChild) Close() error {
	err := c.Child.Close()
	c.jail.give(c.uid)
	return err
}
