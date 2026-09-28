//go:build linux && (amd64 || arm64)

package vmmachine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Start prepares the process under the scratch, or in its jail, and runs
// Firecracker over it: a boot from a configuration file that declares the
// kernel, the processors and the vsock beside the managed memory, and a restore
// with no devices of its own, told only where its vsock's socket moved to.
func (f *Firecracker) Start(ctx context.Context, launch *Launch) (VMM, error) {
	if f.Binary == "" || f.SeccompFilter == "" || f.VCPUs < 1 || f.VCPUs > 32 {
		return nil, errors.New("vmmachine: invalid Firecracker configuration")
	}
	if !launch.Restore() && f.Kernel == "" {
		return nil, errors.New("vmmachine: cold boot needs a kernel")
	}
	// Zero is no vsock at all, and the three lowest context ids are the
	// hypervisor's, the loopback's and the host's.
	if f.VsockCID != 0 && f.VsockCID < 3 {
		return nil, fmt.Errorf("vmmachine: vsock CID %d is reserved", f.VsockCID)
	}
	binary, seccomp, kernel, initrd := f.Binary, f.SeccompFilter, f.Kernel, f.Initrd
	var placement Placement
	if f.Jail != nil {
		var err error
		if placement, err = f.Jail.place(f); err != nil {
			return nil, err
		}
		binary, seccomp, kernel = jailedBinary, jailedSeccomp, jailedKernel
		if initrd != "" {
			initrd = jailedInitrd
		}
	}
	memory, err := launch.Prepare(ctx, placement)
	if err != nil {
		if placement.Owner != nil {
			f.Jail.give(placement.Owner.UID)
		}
		return nil, err
	}
	vmm, err := f.spawn(ctx, memory, launch, binary, seccomp, kernel, initrd, placement.Owner)
	if err != nil && placement.Owner != nil {
		f.Jail.give(placement.Owner.UID)
	}
	return vmm, err
}

// spawn runs Firecracker over prepared memory, with the paths it is to use,
// which are the jail's where it runs in one, as owner where that is set.
func (f *Firecracker) spawn(ctx context.Context, memory *Memory, launch *Launch, binary, seccomp, kernel, initrd string,
	owner *Owner) (VMM, error) {
	args := []string{"--api-sock", memory.APISocket, "--seccomp-filter", seccomp}
	var vsock, vsockWithin string
	if f.VsockCID != 0 {
		vsock, vsockWithin = memory.Path("vsock.sock")
	}
	if launch.Restore() {
		// The device is in the state; only its host socket is this process's,
		// so the restore is told where this one put it.
		if vsock != "" {
			memory.Load["vsock_override"] = map[string]any{"uds_path": vsockWithin}
		}
	} else {
		boot := map[string]any{"kernel_image_path": kernel, "boot_args": f.BootArgs}
		if initrd != "" {
			boot["initrd_path"] = initrd
		}
		vcpus := f.VCPUs
		if launch.VCPUs() > 0 {
			vcpus = launch.VCPUs()
		}
		document := map[string]any{"machine-config": map[string]any{"vcpu_count": vcpus},
			"boot-source": boot, "drives": []any{}}
		if vsock != "" {
			document["vsock"] = map[string]any{"guest_cid": f.VsockCID, "uds_path": vsockWithin}
		}
		if err := memory.Configure(document); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		path, err := memory.WriteFile(ctx, "config.json", raw)
		if err != nil {
			return nil, err
		}
		args = append(args, "--config-file", path)
	}
	command := exec.Command(binary, args...)
	if owner == nil {
		return Spawn(command, vsock)
	}
	// The chroot and the change of user happen in the child before its exec,
	// so the process started is the VMM itself, as a jailer that execs leaves
	// it: a user other than root holds none of root's capabilities.
	command.Dir = "/"
	command.SysProcAttr = &syscall.SysProcAttr{Chroot: f.Jail.Root,
		Credential: &syscall.Credential{Uid: uint32(owner.UID), Gid: uint32(owner.GID)}}
	child, err := Spawn(command, vsock)
	if err != nil {
		return nil, err
	}
	return &jailedChild{Child: child, jail: f.Jail, uid: owner.UID}, nil
}

// Spawn starts a VMM as a child of this process, with its serial console kept
// in memory: the newest 1 MiB of its output, and a pipe on its standard input
// that a console write goes to. vsock is the host path of the socket its vsock
// device binds, empty for a VMM without one. The command may be a jailer that
// execs the VMM, as long as it neither daemonizes nor forks into a new PID
// namespace. Its standard streams are Spawn's; everything else about it is the
// caller's.
func Spawn(command *exec.Cmd, vsock string) (*Child, error) {
	input, output, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	child := &Child{cmd: command, console: newConsoleRing(), stdin: output, vsock: vsock}
	command.WaitDelay = time.Second
	command.Stdin = input
	command.Stdout = child.console
	command.Stderr = child.console
	err = command.Start()
	// The read end belongs to the child.
	_ = input.Close()
	if err != nil {
		_ = output.Close()
		return nil, err
	}
	return child, nil
}

// Child is a VMM process Spawn started.
type Child struct {
	cmd     *exec.Cmd
	console *consoleRing
	stdin   *os.File
	vsock   string
}

var (
	_ ConsoleVMM = (*Child)(nil)
	_ VsockVMM   = (*Child)(nil)
)

func (c *Child) PID() int    { return c.cmd.Process.Pid }
func (c *Child) Wait() error { return c.cmd.Wait() }
func (c *Child) Kill() error { return c.cmd.Process.Kill() }

// Close closes the console's input once the process has gone.
func (c *Child) Close() error { return c.stdin.Close() }

// VsockPath is the socket of the VM's vsock, empty for a VM without one.
func (c *Child) VsockPath() string { return c.vsock }

// Console reads the serial output from the in-memory ring that retains its
// newest 1 MiB.
func (c *Child) Console(offset int64, limit int) (data []byte, from, next int64) {
	return c.console.read(offset, limit)
}

// WriteConsole types into the guest's serial console.
func (c *Child) WriteConsole(ctx context.Context, data []byte) error {
	if len(data) > 4096 {
		return errors.New("vmmachine: console write too large")
	}
	deadline := time.Now().Add(time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.stdin.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err := c.stdin.Write(data)
	return err
}
