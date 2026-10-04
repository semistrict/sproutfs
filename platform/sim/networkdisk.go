package sim

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

// NetworkDisks is a cloud's network disks: volumes kept apart from every
// machine, each attached to at most one machine at a time, as a single-writer
// Hyperdisk Balanced or gp3 volume is. A machine opens what is attached to it
// through Devices. Each disk is a simulated disk of its own holding one file,
// the device, so it keeps what was synced and loses or garbles what was not
// when it is detached, as a machine that lost the device mid-write does.
// Detaching it ends every handle a machine had of it.
//
// The cloud's API has sites of its own, so a controller meets an attach or a
// detach that is slow, that fails before the cloud does it, and that the
// cloud did although its caller was told it failed: the attached-but-not-
// recorded crash point.
type NetworkDisks struct {
	runtime *Runtime
	config  NetworkDisksConfig

	mu    sync.Mutex
	disks map[string]*networkDisk
}

// NetworkDisksConfig is how long the cloud takes, and what each disk's
// device is like.
type NetworkDisksConfig struct {
	// AttachLatency and DetachLatency are how long an attach and a detach
	// take, and DescribeLatency a read of a disk's state.
	AttachLatency, DetachLatency, DescribeLatency time.Duration
	// Device is each disk's device: its latencies and its faults. Its space
	// is the disk's size.
	Device DiskConfig
}

// The cloud's fault-injection sites.
const (
	// BuggifyAttachSlow holds an attach for up to half a minute, as a cloud
	// whose operation queue is long does.
	BuggifyAttachSlow = "sim/network-disk/attach-slow"
	// BuggifyAttachFails fails an attach before the cloud does it.
	BuggifyAttachFails = "sim/network-disk/attach-fails"
	// BuggifyAttachReplyLost has the cloud attach the disk and tell its caller
	// the attach failed.
	BuggifyAttachReplyLost = "sim/network-disk/attach-reply-lost"
	// BuggifyDetachSlow holds a detach for up to half a minute.
	BuggifyDetachSlow = "sim/network-disk/detach-slow"
	// BuggifyDetachFails fails a detach before the cloud does it.
	BuggifyDetachFails = "sim/network-disk/detach-fails"
	// BuggifyDescribeFails fails a read of a disk's state.
	BuggifyDescribeFails = "sim/network-disk/describe-fails"
)

// NetworkDiskSites is every site of the cloud's network disks.
func NetworkDiskSites() []string {
	return []string{BuggifyAttachSlow, BuggifyAttachFails, BuggifyAttachReplyLost, BuggifyDetachSlow,
		BuggifyDetachFails, BuggifyDescribeFails}
}

// networkDisk is one volume: its device, and the machine it is attached to,
// empty for none.
type networkDisk struct {
	name    string
	bytes   int64
	backing *Disk
	machine string
}

// deviceName is the one file each disk's simulated disk holds.
const deviceName = "device"

// NewNetworkDisks is a cloud with no disks yet, in this runtime.
func (r *Runtime) NewNetworkDisks(config NetworkDisksConfig) *NetworkDisks {
	return &NetworkDisks{runtime: r, config: config, disks: make(map[string]*networkDisk)}
}

// Create makes a volume of bytes, attached to nothing, every byte of it
// zero and durable.
func (n *NetworkDisks) Create(ctx context.Context, volume string, bytes int64) error {
	if volume == "" || bytes <= 0 {
		return fmt.Errorf("%w: a network disk named %q of %d bytes", platform.ErrInvalidPath, volume, bytes)
	}
	n.mu.Lock()
	if _, exists := n.disks[volume]; exists {
		n.mu.Unlock()
		return platform.ErrAlreadyExists
	}
	config := n.config.Device
	config.Space = SpaceConfig{TotalBytes: 2 * bytes}
	backing := n.runtime.NewDisk("network-disk/"+volume, config)
	disk := &networkDisk{name: volume, bytes: bytes, backing: backing}
	n.disks[volume] = disk
	n.mu.Unlock()
	file, err := backing.Open(ctx, deviceName, platform.OpenOptions{Create: true})
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(ctx, bytes); err != nil {
		return err
	}
	if err := file.Sync(ctx); err != nil {
		return err
	}
	return backing.SyncNamespace(ctx)
}

// Disk is the simulated disk a volume's device is on, which a test damages
// or reads as it would any other.
func (n *NetworkDisks) Disk(volume string) *Disk {
	n.mu.Lock()
	defer n.mu.Unlock()
	if disk := n.disks[volume]; disk != nil {
		return disk.backing
	}
	return nil
}

// Attached is the machine a volume is attached to now, empty for none.
func (n *NetworkDisks) Attached(volume string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if disk := n.disks[volume]; disk != nil {
		return disk.machine
	}
	return ""
}

func (n *NetworkDisks) find(volume string) (*networkDisk, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	disk := n.disks[volume]
	if disk == nil {
		return nil, fmt.Errorf("%w: network disk %q", platform.ErrNotFound, volume)
	}
	return disk, nil
}

func (n *NetworkDisks) trace(operation, volume, outcome string) {
	n.runtime.trace.record(Event{Kind: "network-disk", Resource: volume, Operation: operation, Outcome: outcome})
}

// Describe reports a volume's size and the machine it is attached to.
func (n *NetworkDisks) Describe(ctx context.Context, volume string) (platform.NetworkDisk, error) {
	if err := n.runtime.sleep(ctx, n.config.DescribeLatency); err != nil {
		return platform.NetworkDisk{}, err
	}
	if n.runtime.buggifyHere(BuggifyDescribeFails, 0.1) {
		n.trace("describe", volume, "failed")
		return platform.NetworkDisk{}, fmt.Errorf("%w: describing network disk %q", platform.ErrUnavailable, volume)
	}
	disk, err := n.find(volume)
	if err != nil {
		return platform.NetworkDisk{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	described := platform.NetworkDisk{Bytes: disk.bytes}
	if disk.machine != "" {
		described.Machines = []string{disk.machine}
	}
	return described, nil
}

// Attach attaches a volume to machine, which then opens it. It is refused
// with platform.ErrInUse while the volume is attached to another machine.
func (n *NetworkDisks) Attach(ctx context.Context, volume, machine string) error {
	if machine == "" {
		return fmt.Errorf("%w: attaching %q to no machine", platform.ErrInvalidPath, volume)
	}
	if err := n.wait(ctx, "attach", volume, n.config.AttachLatency, BuggifyAttachSlow); err != nil {
		return err
	}
	if n.runtime.buggifyHere(BuggifyAttachFails, 0.1) {
		n.trace("attach", volume, "failed")
		return fmt.Errorf("%w: attaching network disk %q to %s", platform.ErrUnavailable, volume, machine)
	}
	disk, err := n.find(volume)
	if err != nil {
		return err
	}
	n.mu.Lock()
	switch disk.machine {
	case machine:
		n.mu.Unlock()
		n.trace("attach", volume, "attached already")
		return nil
	case "":
		disk.machine = machine
	default:
		other := disk.machine
		n.mu.Unlock()
		n.trace("attach", volume, "in use")
		return fmt.Errorf("%w: %q is attached to %s, not %s", platform.ErrInUse, volume, other, machine)
	}
	n.mu.Unlock()
	if n.runtime.buggifyHere(BuggifyAttachReplyLost, 0.1) {
		n.trace("attach", volume, "reply lost")
		return fmt.Errorf("%w: the reply to attaching %q to %s was lost", platform.ErrUnavailable, volume, machine)
	}
	n.trace("attach", volume, "ok")
	return nil
}

// Detach detaches a volume from machine. Every handle a process of that
// machine had of it fails from here on, and what it wrote and had not
// synced may be lost or garbled.
func (n *NetworkDisks) Detach(ctx context.Context, volume, machine string) error {
	if err := n.wait(ctx, "detach", volume, n.config.DetachLatency, BuggifyDetachSlow); err != nil {
		return err
	}
	if n.runtime.buggifyHere(BuggifyDetachFails, 0.1) {
		n.trace("detach", volume, "failed")
		return fmt.Errorf("%w: detaching network disk %q from %s", platform.ErrUnavailable, volume, machine)
	}
	disk, err := n.find(volume)
	if err != nil {
		return err
	}
	n.mu.Lock()
	if disk.machine != machine {
		n.mu.Unlock()
		n.trace("detach", volume, "not attached")
		return nil
	}
	disk.machine = ""
	n.mu.Unlock()
	// The machine loses the device under whatever it had in flight.
	if err := disk.backing.PowerLoss(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	n.trace("detach", volume, "ok")
	return nil
}

// Crash is machine crashing: every disk attached to it stays attached, every
// handle a process of it held fails, and what those processes wrote and had
// not synced is lost or garbled, as a power cut leaves it.
func (n *NetworkDisks) Crash(ctx context.Context, machine string) error {
	n.mu.Lock()
	var crashed []*networkDisk
	for _, volume := range slices.Sorted(maps.Keys(n.disks)) {
		if disk := n.disks[volume]; disk.machine == machine {
			crashed = append(crashed, disk)
		}
	}
	n.mu.Unlock()
	for _, disk := range crashed {
		if err := disk.backing.PowerLoss(ctx); err != nil {
			return err
		}
		n.trace("crash", disk.name, "ok")
	}
	return nil
}

// wait takes an operation's latency, and up to half a minute more where its
// slow site fires.
func (n *NetworkDisks) wait(ctx context.Context, operation, volume string, latency time.Duration, slow string) error {
	if n.runtime.buggifyHere(slow, 0.1) {
		r := n.runtime
		extra := r.Random("sim/network-disk").Duration(slow+"/"+r.occurrenceOf(slow), 30*time.Second)
		n.trace(operation, volume, "slow "+extra.String())
		latency += extra
	}
	return n.runtime.sleep(ctx, latency)
}

// Devices is one machine's view of the network disks attached to it.
func (n *NetworkDisks) Devices(machine string) platform.Devices {
	return &machineDevices{disks: n, machine: machine}
}

type machineDevices struct {
	disks   *NetworkDisks
	machine string
}

// Open opens the device of a volume attached to this machine, for one
// process: a second open is refused with platform.ErrLocked until the first
// closes, as an exclusive open of a block device is.
func (m *machineDevices) Open(ctx context.Context, volume string) (platform.File, error) {
	disk, err := m.disks.find(volume)
	if err != nil {
		return nil, err
	}
	m.disks.mu.Lock()
	attached := disk.machine == m.machine
	m.disks.mu.Unlock()
	if !attached {
		return nil, fmt.Errorf("%w: network disk %q is not attached to %s", platform.ErrNotFound, volume, m.machine)
	}
	file, err := disk.backing.Open(ctx, deviceName, platform.OpenOptions{Lock: true})
	if err != nil {
		return nil, err
	}
	// The attach may have ended while the open waited.
	m.disks.mu.Lock()
	attached = disk.machine == m.machine
	m.disks.mu.Unlock()
	if !attached {
		_ = file.Close()
		return nil, fmt.Errorf("%w: network disk %q was detached from %s", platform.ErrNotFound, volume, m.machine)
	}
	return &device{File: file}, nil
}

// device is a network disk as a machine sees it: a file of the disk's size,
// which cannot be truncated, grown, allocated or punched.
type device struct {
	platform.File
}

func (d *device) Truncate(context.Context, int64) error {
	return fmt.Errorf("a block device: %w", errors.ErrUnsupported)
}

func (d *device) WriteAt(ctx context.Context, source []byte, offset int64) (int, error) {
	size, err := d.File.Size(ctx)
	if err != nil {
		return 0, err
	}
	if offset < 0 || offset+int64(len(source)) > size {
		return 0, fmt.Errorf("%w: %d bytes at %d past a device of %d", platform.ErrInvalidRange, len(source), offset,
			size)
	}
	return d.File.WriteAt(ctx, source, offset)
}

var (
	_ platform.NetworkDisks = (*NetworkDisks)(nil)
	_ platform.Devices      = (*machineDevices)(nil)
	_ platform.File         = (*device)(nil)
)
