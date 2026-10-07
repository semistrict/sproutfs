package membership

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
)

// idOf is the identity n.
func idOf(n byte) rank.Identity { return rank.Identity{15: n} }

// memberOf is member n at its own address.
func memberOf(n byte) Member {
	return Member{ID: idOf(n), Address: platform.Address(fmt.Sprintf("host-%d:7000", n))}
}

// diskOf is disk n of weight 1, named for its volume.
func diskOf(n byte) Disk {
	return Disk{ID: idOf(100 + n), Volume: fmt.Sprintf("cache-%d", n), Weight: 1}
}

// built takes what a change built, and fails the test on its error.
func built(t *testing.T) func(Membership, error) Membership {
	return func(m Membership, err error) Membership {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
}

// stepped is change applied to m, which Step must admit.
func stepped(t *testing.T, m Membership, change func(Membership) (Membership, error)) Membership {
	t.Helper()
	next := built(t)(change(m))
	if err := Step(t.Context(), m, next); err != nil {
		t.Fatalf("Step refused %v: %v", describe(next), err)
	}
	return next
}

// describe writes a membership out, so a failure says what it held.
func describe(m Membership) string {
	text := fmt.Sprintf("generation %d under %s:", m.Generation(), m.Code())
	for _, member := range m.Members() {
		text += fmt.Sprintf(" member %s at %s %s;", member.ID, member.Address, member.State)
	}
	for _, disk := range m.Disks() {
		text += fmt.Sprintf(" disk %s of %d %s for %s at %d;", disk.ID, disk.Weight, disk.State, disk.Member,
			disk.Assigned)
	}
	return text
}

// joined is a membership of members 1 to n, each with its disk serving,
// under code, built one step at a time from the empty one.
func joined(t *testing.T, code rank.Code, n byte) Membership {
	t.Helper()
	m := Empty()
	if code != m.Code() {
		m = stepped(t, m, func(m Membership) (Membership, error) { return m.Recode(code) })
	}
	for at := byte(1); at <= n; at++ {
		m = stepped(t, m, func(m Membership) (Membership, error) { return m.Join(memberOf(at), diskOf(at)) })
		m = stepped(t, m, func(m Membership) (Membership, error) { return m.Serve(diskOf(at).ID, idOf(at)) })
	}
	return m
}

// A join is one generation: the member joining and its disk attaching,
// assigned at that generation. Serving it makes the member active. The list
// ranks every disk, and a disk is at its member's address only while it
// serves.
func TestAJoinIsOneGenerationAndADiskIsRoutedOnlyWhileItServes(t *testing.T) {
	m := stepped(t, Empty(), func(m Membership) (Membership, error) { return m.Recode(rank.Code{K: 1, M: 1}) })
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Join(memberOf(1), diskOf(1)) })
	if m.Generation() != 2 {
		t.Fatalf("a join wrote generation %d, want 2", m.Generation())
	}
	member, _ := m.Member(idOf(1))
	disk, _ := m.Disk(diskOf(1).ID)
	if member.State != Joining || disk.State != Attaching || disk.Member != idOf(1) || disk.Assigned != 2 {
		t.Fatalf("after a join: %s", describe(m))
	}
	if _, ok := m.Route(disk.ID); ok || m.Serves(idOf(1), disk.ID) {
		t.Fatalf("an attaching disk has a route: %s", describe(m))
	}
	caches := m.List().Caches()
	if len(caches) != 1 || caches[0].Identity != disk.ID || caches[0].Address != "" {
		t.Fatalf("the list of an attaching disk is %+v, want it ranked at no address", caches)
	}
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Serve(disk.ID, idOf(1)) })
	member, _ = m.Member(idOf(1))
	route, ok := m.Route(disk.ID)
	if member.State != Active || !ok || route != (Route{Disk: disk.ID, Address: memberOf(1).Address, Generation: 3,
		Assigned: 2}) || !m.Serves(idOf(1), disk.ID) {
		t.Fatalf("a served disk routes %+v (%v): %s", route, ok, describe(m))
	}
	if _, err := m.Serve(disk.ID, idOf(1)); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("serving a served disk again: %v, want ErrUnchanged", err)
	}
	if _, err := m.Join(memberOf(1), diskOf(1)); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("joining a member again: %v, want ErrUnchanged", err)
	}
}

// A disk leaves its member through releasing and released, and a member
// leaves once it is draining and holds no disk; Step refuses every shortcut.
func TestADiskIsReleasedBeforeItIsAssignedAgain(t *testing.T) {
	m := joined(t, rank.Code{K: 1, M: 1}, 2)
	ctx := t.Context()
	disk := diskOf(1).ID
	moved := built(t)(m.Assign(disk, idOf(2)))
	if err := Step(ctx, m, moved); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Step admitted assigning a serving disk to another member: %v", err)
	}
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Release(disk) })
	if _, ok := m.Route(disk); ok {
		t.Fatalf("a releasing disk has a route: %s", describe(m))
	}
	if err := Step(ctx, m, built(t)(m.Assign(disk, idOf(2)))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Step admitted assigning a releasing disk: %v", err)
	}
	if _, err := m.Let(disk, idOf(2)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a member that does not hold a disk let it go: %v", err)
	}
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Let(disk, idOf(1)) })
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Assign(disk, idOf(2)) })
	found, _ := m.Disk(disk)
	if found.Member != idOf(2) || found.State != Attaching || found.Assigned != m.Generation() {
		t.Fatalf("a released disk assigned again: %s", describe(m))
	}
	if err := Step(ctx, m, built(t)(m.Leave(idOf(1)))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Step admitted an active member leaving: %v", err)
	}
	if err := Step(ctx, m, built(t)(m.Remove(diskOf(2).ID))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Step admitted removing a serving disk: %v", err)
	}
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Drain(idOf(2)) })
	for _, disk := range m.Disks() {
		if disk.Member == idOf(2) && disk.State != Releasing {
			t.Fatalf("a draining member keeps a disk %s: %s", disk.State, describe(m))
		}
	}
	if err := Step(ctx, m, built(t)(m.Assign(diskOf(2).ID, idOf(2)))); err == nil {
		t.Fatalf("Step admitted assigning a disk to a draining member")
	}
	if _, err := m.Leave(idOf(2)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a member that holds disks left: %v", err)
	}
	for _, disk := range []rank.Identity{diskOf(1).ID, diskOf(2).ID} {
		m = stepped(t, m, func(m Membership) (Membership, error) { return m.Let(disk, idOf(2)) })
		m = stepped(t, m, func(m Membership) (Membership, error) { return m.Remove(disk) })
	}
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Leave(idOf(2)) })
	if len(m.Members()) != 1 || len(m.Disks()) != 0 {
		t.Fatalf("after the leave: %s", describe(m))
	}
}

// A disk never goes back: serving is never attaching again, and released is
// reached only from releasing.
func TestADiskNeverGoesBack(t *testing.T) {
	m := joined(t, rank.Code{K: 1, M: 0}, 1)
	disk, _ := m.Disk(diskOf(1).ID)
	back := disk
	back.State = Attaching
	next := built(t)(New(m.Generation()+1, m.Code(), m.Members(), []Disk{back}))
	if err := Step(t.Context(), m, next); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Step admitted a serving disk going back to attaching: %v", err)
	}
	gone := disk
	gone.State, gone.Member, gone.Assigned = Released, rank.Identity{}, 0
	next = built(t)(New(m.Generation()+1, m.Code(), m.Members(), []Disk{gone}))
	if err := Step(t.Context(), m, next); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Step admitted a serving disk released without releasing: %v", err)
	}
	if err := Step(t.Context(), m, built(t)(New(m.Generation()+2, m.Code(), m.Members(), m.Disks()))); err == nil {
		t.Fatalf("Step admitted skipping a generation")
	}
}

// What the object holds reads back as it was written, and an object of
// another format, with a field this build does not know, or that no host
// could route by, is refused.
func TestTheObjectReadsBackAndRefusesWhatItDoesNotKnow(t *testing.T) {
	m := joined(t, rank.Code{K: 2, M: 1}, 3)
	m = built(t)(m.Release(diskOf(2).ID))
	m.nonce = make([]byte, nonceSize)
	m.nonce[0] = 7
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	read, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Equal(m) || string(read.nonce) != string(m.nonce) {
		t.Fatalf("read back %s, want %s", describe(read), describe(m))
	}
	for name, damaged := range map[string][]byte{
		"truncated":       data[:len(data)/2],
		"unknown field":   append(append([]byte{}, data...), 0xf8, 0x07, 0x01),
		"another version": append([]byte{0x08, 0x01}, data[2:]...),
	} {
		if _, err := Unmarshal(damaged); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Unmarshal said %v, want ErrCorrupt", name, err)
		}
	}
	twice := []Member{memberOf(1), memberOf(1)}
	if _, err := New(1, rank.Code{K: 1, M: 0}, twice, nil); !errors.Is(err, ErrInvalid) {
		t.Errorf("a member listed twice: %v", err)
	}
	orphan := diskOf(1)
	orphan.Member, orphan.State, orphan.Assigned = idOf(9), Serving, 1
	if _, err := New(1, rank.Code{K: 1, M: 0}, nil, []Disk{orphan}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a disk of a member that is not listed: %v", err)
	}
	early := diskOf(1)
	early.Member, early.State, early.Assigned = idOf(1), Serving, 5
	if _, err := New(4, rank.Code{K: 1, M: 0}, []Member{memberOf(1)}, []Disk{early}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a disk assigned after its generation: %v", err)
	}
}

// A host that has read nothing holds itself alone: generation zero, its disk
// serving under 1+0 and ranked first for every window. A host with no disk is
// alone with nobody.
func TestAHostAloneRanksItsOwnDiskFirst(t *testing.T) {
	alone := Alone(memberOf(1), diskOf(1))
	window := rank.Window{Volume: "ram0", Number: 7, Pages: 1}
	ranks := alone.List().Ranks(window)
	if alone.Generation() != 0 || alone.Code() != (rank.Code{K: 1, M: 0}) || len(ranks) != 1 ||
		ranks[0].Identity != diskOf(1).ID || !alone.Serves(idOf(1), diskOf(1).ID) {
		t.Fatalf("a host alone holds %s", describe(alone))
	}
	if nobody := Alone(memberOf(1), Disk{}); nobody.List().Len() != 0 || len(nobody.Members()) != 0 {
		t.Fatalf("a host with no disk holds %s", describe(nobody))
	}
}

// Fixed holds what it is given whatever generation it is asked for.
func TestFixedHoldsWhatItIsGiven(t *testing.T) {
	m := joined(t, rank.Code{K: 1, M: 0}, 1)
	fixed := NewFixed(m)
	got, err := fixed.Catch(context.Background(), 99)
	if err != nil || !got.Equal(m) {
		t.Fatalf("Catch = %s, %v", describe(got), err)
	}
}
