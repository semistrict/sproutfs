package vmmachine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/semistrict/sproutfs/platform/sim"
)

// Starter runs the VMM process of one VM. This package does not start a VMM:
// it prepares the memory a VMM maps, and it takes over a VMM its Starter has
// started. Everything else about the process is the Starter's: the binary, a
// jailer, a network namespace and a cgroup, the kernel, the network interfaces,
// drives and plain files it gives the guest, and the vsock.
//
// Start is called once for every VMM process: a create's boot, an open's
// restore or cold boot, and the restore of a VM received by migration or by
// fork. It must call launch.Prepare exactly once, start a VMM with the memory
// Prepare describes, and return that process without waiting for it to become
// ready. What happens after that — the snapshot load of a restore, the memory
// sessions, the wait for the guest — is this package's, because the order of it
// is what keeps a guest from running on memory that is not attached.
//
// Firecracker is this package's own Starter, which runs Firecracker directly.
type Starter interface {
	Start(ctx context.Context, launch *Launch) (VMM, error)
	// Boots reports whether this Starter can boot a kernel, rather than only
	// restore VMM state. A host refuses to discard a VM's memory for a cold
	// start that its Starter could never boot.
	Boots() bool
}

// VMM is one running VMM process as its Starter started it.
type VMM interface {
	// PID is the process that maps the guest's memory. The memory sessions
	// admit only a connection from it, so a Starter that runs the VMM under a
	// jailer runs it neither daemonized nor in a new PID namespace: the process
	// it started becomes the VMM.
	PID() int
	// Wait blocks until the process has exited and reports how it ended. This
	// package calls it once.
	Wait() error
	// Kill ends the process at once. It is how a failed memory session stops
	// the guest's vCPUs.
	Kill() error
	// Close releases what the Starter built for the process — a chroot, a
	// network namespace, a cgroup — once it has exited and this package has
	// removed the process's directory.
	Close() error
}

// ConsoleVMM is a VMM whose serial console its Starter keeps, which is what a
// host's console calls read and write.
type ConsoleVMM interface {
	VMM
	// Console reads the retained output from offset: at most limit bytes, from
	// the oldest retained byte if offset is older, and the offset after them.
	Console(offset int64, limit int) (data []byte, from, next int64)
	WriteConsole(ctx context.Context, data []byte) error
}

// VsockVMM is a VMM with a virtio-vsock device, which is how a host reaches the
// agent in its guest.
type VsockVMM interface {
	VMM
	// VsockPath is the Unix socket the VMM binds for the device, as this
	// process opens it.
	VsockPath() string
}

// Launch is one VMM process this package is about to take over, as its Starter
// sees it.
type Launch struct {
	vm      string
	restore bool
	vcpus   int
	prepare func(context.Context, Placement) (*Memory, error)
}

// VM is the identity of the VM the process runs.
func (l *Launch) VM() string { return l.vm }

// Restore reports a process that loads VMM state rather than booting a kernel.
// A restore's devices are in its state: its Starter gives it a configuration
// with no devices, and Memory.Load the new host names of anything that moved.
func (l *Launch) Restore() bool { return l.restore }

// VCPUs is how many processors a boot gives the guest, zero where the VM
// records none and the Starter's own default applies. A restore takes the count
// from its state and ignores it.
func (l *Launch) VCPUs() int { return l.vcpus }

// Prepare creates the process's directory where the placement puts it and
// opens the sockets the VMM's memory attaches through. It is called once, before
// the VMM starts.
func (l *Launch) Prepare(ctx context.Context, placement Placement) (*Memory, error) {
	if l.prepare == nil {
		return nil, errors.New("vmmachine: a launch is prepared once")
	}
	prepare := l.prepare
	if !sim.Bug(ctx, "vmmachine-prepare-twice") {
		l.prepare = nil
	}
	return prepare(ctx, placement)
}

// Placement is where one VMM process's files live and who may use them. The
// zero Placement is a directory under the scratch, owned by this process.
type Placement struct {
	// Directory is the host directory this package creates the process's
	// sockets and staging files in, and removes with the process. Its parent
	// must exist and it must not. Empty is a directory under the scratch.
	Directory string
	// Within is Directory as the VMM names it, which differs from Directory
	// when the VMM runs inside a chroot. Empty is Directory itself.
	Within string
	// Owner is the user the VMM runs as, when a jailer drops it to one. The
	// directory, its sockets and its staging files are given to that user.
	Owner *Owner
}

// Owner is a user and a group, by number.
type Owner struct {
	UID, GID int
}

// Memory is what a prepared VMM process must be started with: where its API
// socket goes, and the memory this package manages for it. Every path in it is
// as the VMM names it.
type Memory struct {
	// Directory is the process's directory on the host and Within the same
	// directory as the VMM names it.
	Directory, Within string
	// APISocket is where the VMM binds its API socket, which is how this
	// package drives it once it has started.
	APISocket string
	// Bytes is the guest's RAM and RAM the socket the VMM maps it through.
	Bytes uint64
	RAM   string
	// Pmem are the PMEM devices this package manages, in the order the VM's
	// configuration names them.
	Pmem []ManagedPmem
	// Load is what a Starter adds to a restore's snapshot load request: new
	// host names for its network interfaces, a new socket for its vsock. The
	// keys this package sets itself are refused.
	Load map[string]any

	write func(ctx context.Context, name string, data []byte) error
}

// ManagedPmem is one PMEM device whose pages this package manages.
type ManagedPmem struct {
	ID     string
	Root   bool
	Socket string
	Bytes  uint64
}

// Path is a file in the process's directory: where this process opens it and
// where the VMM does.
func (m *Memory) Path(name string) (host, within string) {
	return filepath.Join(m.Directory, name), filepath.Join(m.Within, name)
}

// WriteFile writes a file the VMM reads — its configuration — into the
// process's staging, owned by the VMM's user, and returns the path the VMM
// names it by. The file is removed once the VMM is running.
func (m *Memory) WriteFile(ctx context.Context, name string, data []byte) (string, error) {
	if err := m.write(ctx, name, data); err != nil {
		return "", err
	}
	return filepath.Join(m.Within, stateDirectory, name), nil
}

// Configure puts the managed memory into a boot's configuration document: the
// RAM's socket and size, and the managed PMEM devices ahead of any the document
// already declares, so the managed root is the guest's first. Everything else
// in the document is the Starter's, and none of it may claim the managed
// memory's keys. A document it refuses is left as it was.
func (m *Memory) Configure(document map[string]any) error {
	if document == nil {
		return errors.New("vmmachine: there is no configuration to add the managed memory to")
	}
	if _, found := document["managed-memory"]; found {
		return errors.New("vmmachine: the configuration already names its managed memory")
	}
	machine := map[string]any{}
	if existing, found := document["machine-config"]; found {
		object, ok := existing.(map[string]any)
		if !ok {
			return fmt.Errorf("vmmachine: the configuration's machine-config is %T, not an object", existing)
		}
		machine = object
	}
	if _, found := machine["mem_size_mib"]; found {
		return errors.New("vmmachine: the configuration already sizes the guest's memory")
	}
	if _, found := machine["huge_pages"]; found {
		// Managed RAM is the pager's own backing, and the session states its
		// page when it attaches. The VMM refuses a setting of its own.
		return errors.New("vmmachine: managed memory takes no huge-page setting")
	}
	var others []any
	if existing, found := document["pmem"]; found {
		list, ok := existing.([]any)
		if !ok {
			return fmt.Errorf("vmmachine: the configuration's pmem is %T, not a list", existing)
		}
		for _, other := range list {
			if device, ok := other.(map[string]any); ok && device["root_device"] == true {
				return errors.New("vmmachine: the root device is the managed one")
			}
		}
		others = list
	}
	machine["mem_size_mib"] = m.Bytes >> 20
	document["machine-config"] = machine
	document["managed-memory"] = map[string]any{"socket_path": m.RAM}
	pmem := make([]any, 0, len(m.Pmem)+len(others))
	for _, device := range m.Pmem {
		pmem = append(pmem, map[string]any{"id": device.ID, "root_device": device.Root,
			"managed": map[string]any{"socket_path": device.Socket, "length": device.Bytes}})
	}
	document["pmem"] = append(pmem, others...)
	return nil
}

// stateDirectory is the subdirectory of a process's directory that holds its
// staging files: the configuration, a restore's state and a capture's.
const stateDirectory = "state"
