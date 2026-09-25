//go:build linux && (amd64 || arm64)

package vmmachine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Start prepares the process under the scratch and runs Firecracker over it: a
// boot from a configuration file that declares the kernel, the processors and
// the vsock beside the managed memory, and a restore with no devices of its
// own, told only where its vsock's socket moved to.
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
	memory, err := launch.Prepare(ctx, Placement{})
	if err != nil {
		return nil, err
	}
	args := []string{"--api-sock", memory.APISocket, "--seccomp-filter", f.SeccompFilter}
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
		boot := map[string]any{"kernel_image_path": f.Kernel, "boot_args": f.BootArgs}
		if f.Initrd != "" {
			boot["initrd_path"] = f.Initrd
		}
		document := map[string]any{"machine-config": map[string]any{"vcpu_count": f.VCPUs},
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
	return Spawn(exec.Command(f.Binary, args...), vsock)
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
