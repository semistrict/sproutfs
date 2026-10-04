// Package membership is which hosts are in the cluster, and which cache disk
// each one serves: one object in the object store, the membership. It is not
// the cache's own. Anything that routes requests between hosts reads it, and
// the cluster's disk cache, which places a window's stripes by it, is the
// first.
//
// The object holds a generation, the deployment's code, every member with
// the address its peer server answers at and its state, and every disk with
// the member it is assigned to and its state. Windows are ranked over the
// disks, not the members, so a disk that moves to another member keeps its
// windows.
//
// It changes only by compare-and-set. A change reads the object, changes it,
// and writes it back conditional on the object it read, raising the
// generation by one; a write that lost is tried again from a fresh read.
// Correctness rests on that alone. Any process may make a change, and nothing
// depends on there being one writer.
//
// Every process holds a copy (View), never an authority of its own. Every
// request between hosts that depends on the membership names the generation
// its sender holds. A host behind the sender reads the object before it
// answers; a host ahead of it answers that the sender is stale, and the
// sender reads the object and asks again. So no two hosts act on different
// memberships without finding out.
package membership

import (
	"bytes"
	"errors"
	"fmt"
	"slices"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
)

var (
	// ErrInvalid reports a membership no host could route by, or a change it
	// does not admit.
	ErrInvalid = errors.New("membership: invalid")
	// ErrUnchanged reports a change that has nothing to do: the membership is
	// already what it asks for. An update that meets it writes nothing.
	ErrUnchanged = errors.New("membership: unchanged")
	// ErrCorrupt reports an object that does not parse.
	ErrCorrupt = errors.New("membership: corrupt")
)

// MemberState is where a member is in its life in the cluster.
type MemberState uint8

const (
	// Joining is a member none of whose disks serves yet.
	Joining MemberState = iota + 1
	// Active is a member that serves its disks.
	Active
	// Draining is a member on its way out: its disks are released, and none
	// is assigned to it again.
	Draining
)

func (s MemberState) String() string {
	switch s {
	case Joining:
		return "joining"
	case Active:
		return "active"
	case Draining:
		return "draining"
	}
	return fmt.Sprintf("member-state-%d", uint8(s))
}

// DiskState is where a disk is between the members that serve it.
type DiskState uint8

const (
	// Attaching is a disk assigned to a member that does not serve it yet.
	Attaching DiskState = iota + 1
	// Serving is a disk its member serves.
	Serving
	// Releasing is a disk assigned to a member that must stop serving it.
	Releasing
	// Released is a disk assigned to nobody.
	Released
)

func (s DiskState) String() string {
	switch s {
	case Attaching:
		return "attaching"
	case Serving:
		return "serving"
	case Releasing:
		return "releasing"
	case Released:
		return "released"
	}
	return fmt.Sprintf("disk-state-%d", uint8(s))
}

// Member is one host in the cluster.
type Member struct {
	// ID is the member's identity. A host's is written in its disk: the
	// identity in the header of the cache file it opens, so a pod replaced on
	// the same node keeps it.
	ID rank.Identity
	// Address is where the member's peer server answers. A pod that comes
	// back on another address keeps its identity, and the membership follows
	// it.
	Address platform.Address
	State   MemberState
}

// Disk is one cache disk, a shard of the cluster's cache.
type Disk struct {
	// ID is the identity in the header of the disk's cache file. Windows are
	// ranked by it.
	ID rank.Identity
	// Volume is the name the cloud or the cluster knows the disk's volume
	// by, which attaching it to a member asks for. A disk on a node's own
	// filesystem names the node and the file.
	Volume string
	// Weight is the disk's share of windows against the others.
	Weight uint32
	// Member is the member the disk is assigned to, zero for none.
	Member rank.Identity
	State  DiskState
	// Assigned is the generation that assigned the disk to Member, zero for
	// a disk assigned to nobody. Every reply its member sends for the disk
	// names it, and a member that lost the disk can never name the
	// generation that assigned it again.
	Assigned uint64
}

// Membership is one generation of the membership. It is a value: a change
// returns a new one, and a process replaces the one it holds.
type Membership struct {
	generation uint64
	code       rank.Code
	// earlier is the codes the deployment used before code, newest first: a
	// window stored under one is still read under it.
	earlier []rank.Code
	nonce   []byte
	members    []Member
	disks      []Disk
	// list ranks windows over the disks, each at the address of the member
	// that serves it, or at none.
	list rank.List
}

// New is a membership at generation, after checking that every host could
// route by it: a valid code; members and disks with identities, none twice;
// a member with an address; a disk with a weight; and a disk assigned to a
// listed member in a state that has one, or released and assigned to nobody.
// earlier is the codes the deployment used before code, newest first, each
// named once and at most rank.MaxEarlierCodes of them.
func New(generation uint64, code rank.Code, members []Member, disks []Disk, earlier ...rank.Code) (Membership, error) {
	if err := code.Validate(); err != nil {
		return Membership{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	m := Membership{generation: generation, code: code, earlier: slices.Clone(earlier),
		members: sorted(members, memberID), disks: sorted(disks, diskID)}
	for at, member := range m.members {
		switch {
		case member.ID.IsZero() || member.Address == "":
			return Membership{}, fmt.Errorf("%w: member %s at %q, want an identity and an address", ErrInvalid,
				member.ID, member.Address)
		case member.State < Joining || member.State > Draining:
			return Membership{}, fmt.Errorf("%w: member %s is %s", ErrInvalid, member.ID, member.State)
		case at > 0 && m.members[at-1].ID == member.ID:
			return Membership{}, fmt.Errorf("%w: member %s is listed twice", ErrInvalid, member.ID)
		}
	}
	caches := make([]rank.Cache, 0, len(m.disks))
	for at, disk := range m.disks {
		if at > 0 && m.disks[at-1].ID == disk.ID {
			return Membership{}, fmt.Errorf("%w: disk %s is listed twice", ErrInvalid, disk.ID)
		}
		if err := m.checkDisk(disk); err != nil {
			return Membership{}, err
		}
		caches = append(caches, rank.Cache{Identity: disk.ID, Weight: disk.Weight, Address: m.served(disk)})
	}
	list, err := rank.NewList(code, caches, earlier...)
	if err != nil {
		return Membership{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	m.list = list
	return m, nil
}

// checkDisk refuses a disk no host could route by.
func (m Membership) checkDisk(disk Disk) error {
	switch {
	case disk.ID.IsZero() || disk.Weight == 0:
		return fmt.Errorf("%w: disk %s of weight %d, want an identity and a weight", ErrInvalid, disk.ID, disk.Weight)
	case disk.State < Attaching || disk.State > Released:
		return fmt.Errorf("%w: disk %s is %s", ErrInvalid, disk.ID, disk.State)
	case disk.State == Released:
		if !disk.Member.IsZero() || disk.Assigned != 0 {
			return fmt.Errorf("%w: disk %s is released and assigned to %s at %d", ErrInvalid, disk.ID, disk.Member,
				disk.Assigned)
		}
		return nil
	}
	if _, ok := m.Member(disk.Member); !ok {
		return fmt.Errorf("%w: disk %s is %s for member %s, which is not listed", ErrInvalid, disk.ID, disk.State,
			disk.Member)
	}
	if disk.Assigned == 0 || disk.Assigned > m.generation {
		return fmt.Errorf("%w: disk %s was assigned at generation %d of %d", ErrInvalid, disk.ID, disk.Assigned,
			m.generation)
	}
	return nil
}

// served is the address a disk is served at: its member's while it serves
// it, and none otherwise.
func (m Membership) served(disk Disk) platform.Address {
	if disk.State != Serving {
		return ""
	}
	member, _ := m.Member(disk.Member)
	return member.Address
}

// Alone is what a host holds before it has read the membership: itself, and
// its own disk serving, under the code of one host, at generation zero. Its
// one disk ranks first for every window and keeps each envelope whole. A host
// with no disk is alone with nobody.
func Alone(self Member, disk Disk) Membership {
	self.State = Active
	disk.Member, disk.State, disk.Assigned = self.ID, Serving, 0
	members, disks := []Member{self}, []Disk{disk}
	if self.ID.IsZero() || self.Address == "" || disk.ID.IsZero() || disk.Weight == 0 {
		members, disks = nil, nil
	}
	list := rank.Alone(rank.Cache{Identity: disk.ID, Weight: disk.Weight, Address: self.Address})
	if members == nil {
		list = rank.Alone(rank.Cache{})
	}
	return Membership{code: list.Code(), members: members, disks: disks, list: list}
}

// Generation is the membership's generation: zero for one never read.
func (m Membership) Generation() uint64 { return m.generation }

// Code is the deployment's code.
func (m Membership) Code() rank.Code { return m.code }

// Earlier is the codes the deployment used before its code, newest first.
func (m Membership) Earlier() []rank.Code { return slices.Clone(m.earlier) }

// Members is every member, in identity order.
func (m Membership) Members() []Member { return slices.Clone(m.members) }

// Disks is every disk, in identity order.
func (m Membership) Disks() []Disk { return slices.Clone(m.disks) }

// Member is the member of identity id, and whether there is one.
func (m Membership) Member(id rank.Identity) (Member, bool) {
	at, found := slices.BinarySearchFunc(m.members, id, func(member Member, id rank.Identity) int {
		return bytes.Compare(member.ID[:], id[:])
	})
	if !found {
		return Member{}, false
	}
	return m.members[at], true
}

// Disk is the disk of identity id, and whether there is one.
func (m Membership) Disk(id rank.Identity) (Disk, bool) {
	at, found := slices.BinarySearchFunc(m.disks, id, func(disk Disk, id rank.Identity) int {
		return bytes.Compare(disk.ID[:], id[:])
	})
	if !found {
		return Disk{}, false
	}
	return m.disks[at], true
}

// List ranks windows over every disk of the membership, whatever its state,
// so a disk that moves between members keeps its windows. A disk is at the
// address of the member that serves it, and at none while nobody does: a
// request has nowhere to go, and a reader asks the next rank.
func (m Membership) List() rank.List { return m.list }

// Serves reports whether member serves disk under this membership: the disk
// is assigned to it and serving.
func (m Membership) Serves(member, disk rank.Identity) bool {
	found, ok := m.Disk(disk)
	return ok && !member.IsZero() && found.Member == member && found.State == Serving
}

// Route is how a request reaches a disk under one membership: the address of
// the member that serves it, and the generations the request names.
type Route struct {
	Disk    rank.Identity
	Address platform.Address
	// Generation is the membership's, and Assigned the generation that
	// assigned the disk to the member that serves it, which its reply must
	// name.
	Generation, Assigned uint64
}

// Route is how a request reaches disk, and whether it can: a disk nobody
// serves has no route.
func (m Membership) Route(disk rank.Identity) (Route, bool) {
	found, ok := m.Disk(disk)
	if !ok {
		return Route{}, false
	}
	address := m.served(found)
	if address == "" {
		return Route{}, false
	}
	return Route{Disk: disk, Address: address, Generation: m.generation, Assigned: found.Assigned}, true
}

// Equal reports two memberships with the same generation, code, members and
// disks. The writer's nonce is not compared.
func (m Membership) Equal(other Membership) bool {
	return m.generation == other.generation && m.code == other.code && slices.Equal(m.earlier, other.earlier) &&
		slices.Equal(m.members, other.members) &&
		slices.Equal(m.disks, other.disks)
}

func memberID(member Member) rank.Identity { return member.ID }
func diskID(disk Disk) rank.Identity       { return disk.ID }

// sorted is a copy of entries in identity order.
func sorted[T any](entries []T, id func(T) rank.Identity) []T {
	out := slices.Clone(entries)
	slices.SortStableFunc(out, func(a, b T) int {
		x, y := id(a), id(b)
		return bytes.Compare(x[:], y[:])
	})
	return out
}
