package membership

import (
	"context"
	"fmt"
	"slices"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// The changes. Each is one step: it returns the membership at the next
// generation, or ErrUnchanged when the membership already is what it asks
// for. A change only builds the next generation. Whether that generation may
// follow this one is Step's to say, and Store.Update writes nothing Step
// refuses, whoever built it.
//
// A join, a leave and a change of weight each move windows, and each is one
// generation. A member's state, its address, and a disk's assignment move
// none: a disk keeps its place in every window's ranks wherever it is served.

// Join adds a member, joining, with disks of its own, each attaching and
// assigned to it. A disk that is listed and released is assigned to it; a
// disk listed and assigned is not, and Step refuses the join. Joining a
// member that is listed with every one of its disks assigned to it is
// ErrUnchanged.
func (m Membership) Join(member Member, disks ...Disk) (Membership, error) {
	if listed, ok := m.Member(member.ID); ok {
		for _, disk := range disks {
			if found, ok := m.Disk(disk.ID); !ok || found.Member != listed.ID {
				return Membership{}, fmt.Errorf("%w: member %s is listed and disk %s is not its", ErrInvalid,
					member.ID, disk.ID)
			}
		}
		return Membership{}, ErrUnchanged
	}
	member.State = Joining
	members := append(m.Members(), member)
	next := m.Disks()
	for _, disk := range disks {
		disk.Member, disk.State, disk.Assigned = member.ID, Attaching, m.generation+1
		next = put(next, disk)
	}
	return m.next(members, next)
}

// Add lists a disk assigned to nobody, released, ready to be assigned: a
// volume made for the cluster's cache before any member attaches it. It takes
// its place in every window's ranks at once.
func (m Membership) Add(disk Disk) (Membership, error) {
	if _, ok := m.Disk(disk.ID); ok {
		return Membership{}, ErrUnchanged
	}
	disk.Member, disk.State, disk.Assigned = rank.Identity{}, Released, 0
	return m.next(m.Members(), append(m.Disks(), disk))
}

// Serve marks a disk serving: its member has it attached. A member that was
// joining becomes active in the same step.
func (m Membership) Serve(disk, member rank.Identity) (Membership, error) {
	found, ok := m.Disk(disk)
	switch {
	case !ok || found.Member != member:
		return Membership{}, fmt.Errorf("%w: disk %s is not assigned to %s", ErrInvalid, disk, member)
	case found.State == Serving:
		return Membership{}, ErrUnchanged
	case found.State != Attaching:
		return Membership{}, fmt.Errorf("%w: disk %s is %s, not attaching", ErrInvalid, disk, found.State)
	}
	found.State = Serving
	members := m.Members()
	for at := range members {
		if members[at].ID == member && members[at].State == Joining {
			members[at].State = Active
		}
	}
	return m.next(members, put(m.Disks(), found))
}

// Move sets a member's address: a pod that came back elsewhere under the
// identity its disk holds.
func (m Membership) Move(member rank.Identity, address platform.Address) (Membership, error) {
	members := m.Members()
	at := slices.IndexFunc(members, func(listed Member) bool { return listed.ID == member })
	switch {
	case at < 0:
		return Membership{}, fmt.Errorf("%w: member %s is not listed", ErrInvalid, member)
	case members[at].Address == address:
		return Membership{}, ErrUnchanged
	}
	members[at].Address = address
	return m.next(members, m.Disks())
}

// Weigh sets a disk's weight, which moves the windows its change of share
// moves.
func (m Membership) Weigh(disk rank.Identity, weight uint32) (Membership, error) {
	found, ok := m.Disk(disk)
	switch {
	case !ok:
		return Membership{}, fmt.Errorf("%w: disk %s is not listed", ErrInvalid, disk)
	case found.Weight == weight:
		return Membership{}, ErrUnchanged
	}
	found.Weight = weight
	return m.next(m.Members(), put(m.Disks(), found))
}

// Drain starts a member on its way out: it is draining, and each disk it is
// assigned is releasing. It moves no window.
func (m Membership) Drain(member rank.Identity) (Membership, error) {
	members := m.Members()
	at := slices.IndexFunc(members, func(listed Member) bool { return listed.ID == member })
	if at < 0 {
		return Membership{}, fmt.Errorf("%w: member %s is not listed", ErrInvalid, member)
	}
	changed := members[at].State != Draining
	members[at].State = Draining
	disks := m.Disks()
	for index := range disks {
		if disks[index].Member == member && (disks[index].State == Attaching || disks[index].State == Serving) {
			disks[index].State = Releasing
			changed = true
		}
	}
	if !changed {
		return Membership{}, ErrUnchanged
	}
	return m.next(members, disks)
}

// Release asks a disk's member to stop serving it. From this generation on,
// its member answers nothing for it.
func (m Membership) Release(disk rank.Identity) (Membership, error) {
	found, ok := m.Disk(disk)
	switch {
	case !ok:
		return Membership{}, fmt.Errorf("%w: disk %s is not listed", ErrInvalid, disk)
	case found.State == Releasing || found.State == Released:
		return Membership{}, ErrUnchanged
	}
	found.State = Releasing
	return m.next(m.Members(), put(m.Disks(), found))
}

// Let says a releasing disk's member has stopped serving it, and detached it:
// the disk is assigned to nobody. Its member says so, or, for a member that
// is gone, the process that knows it is.
func (m Membership) Let(disk, member rank.Identity) (Membership, error) {
	found, ok := m.Disk(disk)
	switch {
	case !ok:
		return Membership{}, fmt.Errorf("%w: disk %s is not listed", ErrInvalid, disk)
	case found.State == Released:
		return Membership{}, ErrUnchanged
	case found.State != Releasing || found.Member != member:
		return Membership{}, fmt.Errorf("%w: disk %s is %s for %s, not releasing for %s", ErrInvalid, disk,
			found.State, found.Member, member)
	}
	found.Member, found.State, found.Assigned = rank.Identity{}, Released, 0
	return m.next(m.Members(), put(m.Disks(), found))
}

// Assign assigns a disk to a member, attaching, at the next generation, which
// is the generation its member's replies will name. Step refuses it unless
// the disk is released: a disk is released before it is assigned again.
func (m Membership) Assign(disk, member rank.Identity) (Membership, error) {
	found, ok := m.Disk(disk)
	switch {
	case !ok:
		return Membership{}, fmt.Errorf("%w: disk %s is not listed", ErrInvalid, disk)
	case found.Member == member && (found.State == Attaching || found.State == Serving):
		return Membership{}, ErrUnchanged
	}
	found.Member, found.State, found.Assigned = member, Attaching, m.generation+1
	return m.next(m.Members(), put(m.Disks(), found))
}

// Remove takes a released disk out of the membership, which moves its
// windows to the disks ranked after it.
func (m Membership) Remove(disk rank.Identity) (Membership, error) {
	disks := m.Disks()
	at := slices.IndexFunc(disks, func(listed Disk) bool { return listed.ID == disk })
	if at < 0 {
		return Membership{}, ErrUnchanged
	}
	return m.next(m.Members(), slices.Delete(disks, at, at+1))
}

// Leave takes a draining member that is assigned no disk out of the
// membership.
func (m Membership) Leave(member rank.Identity) (Membership, error) {
	members := m.Members()
	at := slices.IndexFunc(members, func(listed Member) bool { return listed.ID == member })
	if at < 0 {
		return Membership{}, ErrUnchanged
	}
	return m.next(slices.Delete(members, at, at+1), m.Disks())
}

// Recode sets the deployment's code. Every stripe of another code is a miss,
// so it is a change a deployment makes rarely.
func (m Membership) Recode(code rank.Code) (Membership, error) {
	if code == m.code {
		return Membership{}, ErrUnchanged
	}
	next, err := New(m.generation+1, code, m.members, m.disks)
	if err != nil {
		return Membership{}, err
	}
	return next, nil
}

// next is the membership at the next generation with members and disks.
func (m Membership) next(members []Member, disks []Disk) (Membership, error) {
	return New(m.generation+1, m.code, members, disks)
}

// put is disks with disk in place of the disk of its identity, or added.
func put(disks []Disk, disk Disk) []Disk {
	if at := slices.IndexFunc(disks, func(listed Disk) bool { return listed.ID == disk.ID }); at >= 0 {
		disks[at] = disk
		return disks
	}
	return append(disks, disk)
}

// Step reports whether next may follow current: one generation later, every
// disk moved only along its states, and no member or disk taken out while
// anything depends on it. Store.Update writes nothing it refuses, so the
// rules hold whichever process built the change.
//
//   - A disk keeps its member while it is attaching, serving or releasing,
//     and the generation that assigned it.
//   - A disk goes from attaching to serving, from either to releasing, and
//     from releasing to released, assigned to nobody. It never goes back.
//   - A disk is assigned to a member only from released, or as it is added,
//     attaching, at the generation that assigns it, and never to a member
//     that is draining.
//   - A disk is removed only once released, and a member only once it is
//     draining and assigned no disk.
func Step(ctx context.Context, current, next Membership) error {
	if next.generation != current.generation+1 {
		return fmt.Errorf("%w: generation %d cannot follow %d", ErrInvalid, next.generation, current.generation)
	}
	for _, disk := range next.disks {
		was, listed := current.Disk(disk.ID)
		if err := stepDisk(ctx, next, was, listed, disk); err != nil {
			return err
		}
	}
	for _, disk := range current.disks {
		if _, kept := next.Disk(disk.ID); !kept && disk.State != Released {
			return fmt.Errorf("%w: disk %s is removed while %s", ErrInvalid, disk.ID, disk.State)
		}
	}
	for _, member := range current.members {
		if _, kept := next.Member(member.ID); kept {
			continue
		}
		if member.State != Draining {
			return fmt.Errorf("%w: member %s leaves while %s", ErrInvalid, member.ID, member.State)
		}
	}
	return nil
}

// stepDisk checks one disk of next against what it was.
func stepDisk(ctx context.Context, next Membership, was Disk, listed bool, disk Disk) error {
	assigning := disk.State == Attaching && (!listed || was.Member != disk.Member)
	switch {
	case assigning:
		if listed && was.State != Released && !sim.Bug(ctx, "membership-assign-without-release") {
			return fmt.Errorf("%w: disk %s is assigned to %s while %s for %s", ErrInvalid, disk.ID, disk.Member,
				was.State, was.Member)
		}
		if member, _ := next.Member(disk.Member); member.State == Draining {
			return fmt.Errorf("%w: disk %s is assigned to %s, which is draining", ErrInvalid, disk.ID, disk.Member)
		}
		if disk.Assigned != next.generation {
			return fmt.Errorf("%w: disk %s is assigned at %d by generation %d", ErrInvalid, disk.ID, disk.Assigned,
				next.generation)
		}
		return nil
	case !listed:
		if disk.State != Released {
			return fmt.Errorf("%w: disk %s is added %s", ErrInvalid, disk.ID, disk.State)
		}
		return nil
	case disk.State == Released:
		if was.State != Releasing && was.State != Released {
			return fmt.Errorf("%w: disk %s is released while %s", ErrInvalid, disk.ID, was.State)
		}
		return nil
	case disk.Member != was.Member || disk.Assigned != was.Assigned:
		return fmt.Errorf("%w: disk %s changes from %s at %d to %s at %d without being released", ErrInvalid, disk.ID,
			was.Member, was.Assigned, disk.Member, disk.Assigned)
	case disk.State < was.State:
		return fmt.Errorf("%w: disk %s goes back from %s to %s", ErrInvalid, disk.ID, was.State, disk.State)
	}
	return nil
}
