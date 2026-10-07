package platform

import (
	"context"
	"errors"
)

// A network disk is a volume the cloud keeps apart from every machine: it
// outlives the machine it is attached to, and the cloud can detach it from
// one machine and attach it to another. The cluster's disk cache keeps its
// shards on network disks (docs/hosting.md, "Shards on network disks"). Two
// ports reach them. NetworkDisks is the cloud's API, which a controller calls
// to move a disk between machines; Devices is one machine's view, which the
// process that serves a disk opens it through.
//
// A network disk is attached to one machine at a time. The disks a deployment
// uses for shards are single-writer: the cloud refuses to attach one to a
// second machine while it is attached to a first. That is the first of the
// guards that keep a shard served by one host at a time; the membership's
// generations and the lease in the disk's header are the others.

// ErrInUse reports a network disk the cloud will not attach to a machine
// because it is attached to another, or will not delete because it is
// attached to one.
var ErrInUse = errors.New("the network disk is attached to another machine")

// NetworkDisk is what the cloud says of one network disk now.
type NetworkDisk struct {
	// Bytes is the disk's size.
	Bytes int64
	// Machines is every machine the disk is attached to, which for a
	// single-writer disk is at most one.
	Machines []string
}

// NetworkDiskSpec is a network disk to create: its name, size and labels, in
// the zone and of the kind the adapter is configured with.
type NetworkDiskSpec struct {
	Name   string
	Bytes  int64
	Labels map[string]string
}

// ListedDisk is one network disk a List found.
type ListedDisk struct {
	Name     string
	Bytes    int64
	Labels   map[string]string
	Machines []string
}

// NetworkDisks is the cloud's API for network disks, by the name the
// deployment knows each by. Attach and Detach each return once the cloud has
// done it. Both do nothing where the disk is already where they would put it:
// attached to that machine, or not attached to it. Attaching a disk that is
// attached to another machine fails with ErrInUse. A disk the cloud does not
// have is ErrNotFound.
type NetworkDisks interface {
	Describe(ctx context.Context, volume string) (NetworkDisk, error)
	Attach(ctx context.Context, volume, machine string) error
	Detach(ctx context.Context, volume, machine string) error
	// List is every network disk labelled key=value, in name order.
	List(ctx context.Context, key, value string) ([]ListedDisk, error)
	// Create makes a disk and returns once the cloud has it. A disk of that
	// name already there is ErrAlreadyExists.
	Create(ctx context.Context, spec NetworkDiskSpec) error
	// Delete removes a disk and returns once the cloud has. A disk the cloud
	// does not have is ErrNotFound; one attached to a machine is ErrInUse.
	Delete(ctx context.Context, volume string) error
}

// Devices opens the network disks attached to one machine, as block devices.
type Devices interface {
	// Open opens the device of a volume attached to this machine, read and
	// write, for this process alone: ErrNotFound while the volume is not
	// attached here, and ErrLocked while another process holds it open. The
	// file's size is the disk's, and it cannot be truncated or grown. Its
	// writes are durable once Sync returns. Once the volume is detached, every
	// operation on the file fails.
	Open(ctx context.Context, volume string) (File, error)
}
