package membership

import (
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// hostOf is member n with its disk, at its address, as a controller wants it.
func hostOf(n byte) Host {
	member := memberOf(n)
	return Host{ID: member.ID, Address: member.Address, Disks: []Disk{diskOf(n)}}
}

// toward takes Next's steps from m to want, each admitted by Step, and
// returns where they end and every generation on the way.
func toward(t *testing.T, m Membership, want Want) (Membership, []Membership) {
	t.Helper()
	var steps []Membership
	for {
		change, ok := Next(t.Context(), m, want)
		if !ok {
			return m, steps
		}
		m = stepped(t, m, change)
		steps = append(steps, m)
		if len(steps) > 64 {
			t.Fatalf("Next took more than 64 steps: %s", describe(m))
		}
	}
}

// Next takes the codes first, so a new deployment's disks are filled under
// its code from the start, then joins each wanted host with its disk in one
// generation, in identity order, then serves each disk in one more: from the
// empty membership, three hosts under 2+1 are seven generations.
func TestNextJoinsAHostInOneGenerationAndServesItInTheNext(t *testing.T) {
	want := Want{Code: rank.Code{K: 2, M: 1}, Hosts: []Host{hostOf(3), hostOf(1), hostOf(2)}}
	end, steps := toward(t, Empty(), want)
	if len(steps) != 7 || end.Code() != want.Code || steps[0].Code() != want.Code {
		t.Fatalf("Next took %d steps to %s", len(steps), describe(end))
	}
	first := steps[1]
	if len(first.Members()) != 1 || len(first.Disks()) != 1 || first.Members()[0].ID != idOf(1) {
		t.Fatalf("the second step is %s, want host 1 joining with its disk", describe(first))
	}
	if joined := steps[3]; len(joined.Members()) != 3 || joined.Serves(idOf(1), diskOf(1).ID) {
		t.Fatalf("the fourth step is %s, want every host joined and no disk serving yet", describe(joined))
	}
	if served := steps[4]; !served.Serves(idOf(1), diskOf(1).ID) {
		t.Fatalf("the fifth step is %s, want host 1's disk serving", describe(served))
	}
	for n := byte(1); n <= 3; n++ {
		if member, _ := end.Member(idOf(n)); member.State != Active || !end.Serves(idOf(n), diskOf(n).ID) {
			t.Fatalf("host %d ends %s", n, describe(end))
		}
	}
	if _, ok := Next(t.Context(), end, want); ok {
		t.Fatal("Next has a step left once the membership is what is wanted")
	}
}

// A host no longer wanted drains, its disk is let go, then removed, and
// then it leaves: four generations, of which only the removal changes the
// disks windows are ranked over.
func TestNextDrainsAHostBeforeItLeaves(t *testing.T) {
	start, _ := toward(t, Empty(), Want{Code: rank.Code{K: 1, M: 1}, Hosts: []Host{hostOf(1), hostOf(2)}})
	end, steps := toward(t, start, Want{Code: rank.Code{K: 1, M: 1}, Hosts: []Host{hostOf(1)}})
	var path []string
	for _, m := range steps {
		member, _ := m.Member(idOf(2))
		disk, _ := m.Disk(diskOf(2).ID)
		path = append(path, member.State.String()+"/"+disk.State.String()+"/"+itoa(m.List().Len()))
	}
	want := []string{"draining/releasing/2", "draining/released/2", "draining/disk-state-0/1",
		"member-state-0/disk-state-0/1"}
	if !slices.Equal(path, want) {
		t.Fatalf("host 2 left through %v, want %v", path, want)
	}
	if len(end.Members()) != 1 || end.Code() != (rank.Code{K: 1, M: 1}) {
		t.Fatalf("after the leave: %s", describe(end))
	}
}

// itoa writes a small count.
func itoa(n int) string { return string(rune('0' + n)) }

// A wanted host follows its reported address and its disk's weight, one
// generation each, and a disk its host no longer reports, but which a
// wanted host still is the member of, stays where it is.
func TestNextFollowsAnAddressAndAWeight(t *testing.T) {
	start, _ := toward(t, Empty(), Want{Code: rank.Code{K: 1, M: 0}, Hosts: []Host{hostOf(1)}})
	moved := hostOf(1)
	moved.Address = "elsewhere:7000"
	moved.Disks[0].Weight = 5
	end, steps := toward(t, start, Want{Code: rank.Code{K: 1, M: 0}, Hosts: []Host{moved}})
	member, _ := end.Member(idOf(1))
	disk, _ := end.Disk(diskOf(1).ID)
	if len(steps) != 2 || member.Address != "elsewhere:7000" || disk.Weight != 5 || disk.State != Serving {
		t.Fatalf("after %d steps: %s", len(steps), describe(end))
	}
	quiet := moved
	quiet.Disks = nil
	if _, ok := Next(t.Context(), end, Want{Code: rank.Code{K: 1, M: 0}, Hosts: []Host{quiet}}); ok {
		t.Fatal("Next took a step for a wanted host that reports no disk")
	}
}

// Two hosts that report one disk are a copied disk: the disk joins with the
// first host by identity, and the second joins with none of it.
func TestNextGivesACopiedDiskToOneHost(t *testing.T) {
	twin := hostOf(2)
	twin.Disks = []Disk{diskOf(1)}
	end, _ := toward(t, Empty(), Want{Code: rank.Code{K: 1, M: 0}, Hosts: []Host{twin, hostOf(1)}})
	disk, _ := end.Disk(diskOf(1).ID)
	if len(end.Members()) != 2 || len(end.Disks()) != 1 || disk.Member != idOf(1) || disk.State != Serving {
		t.Fatalf("a copied disk ends %s", describe(end))
	}
}

// A deployment that changes its code names the one it replaced as earlier:
// one generation, which moves no disk, after which the list ranks windows
// under the new code and reads them under the old one too.
func TestNextTakesTheCodesInOneGeneration(t *testing.T) {
	start, _ := toward(t, Empty(), Want{Code: rank.Code{K: 1, M: 1}, Hosts: []Host{hostOf(1), hostOf(2)}})
	want := Want{Code: rank.Code{K: 2, M: 1}, Earlier: []rank.Code{{K: 1, M: 1}},
		Hosts: []Host{hostOf(1), hostOf(2)}}
	end, steps := toward(t, start, want)
	if len(steps) != 1 || !slices.Equal(end.List().Codes(), []rank.Code{{K: 2, M: 1}, {K: 1, M: 1}}) ||
		!slices.Equal(end.Disks(), start.Disks()) {
		t.Fatalf("after %d steps the membership is %s under %v", len(steps), describe(end), end.List().Codes())
	}
	end.nonce = make([]byte, nonceSize)
	data, err := end.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	read, err := Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !read.Equal(end) || !slices.Equal(read.Earlier(), want.Earlier) {
		t.Fatalf("read back %s under %v", describe(read), read.Earlier())
	}
	if _, err := end.Recode(rank.Code{K: 2, M: 1}, rank.Code{K: 2, M: 1}); err == nil {
		t.Fatal("a membership naming its own code as earlier was built")
	}
}

// A host that comes back while its disk is released but still listed joins
// with that disk, which is assigned to it again; a disk listed and assigned
// to another member is not taken.
func TestNextJoinsAHostWithItsReleasedDisk(t *testing.T) {
	start, _ := toward(t, Empty(), Want{Code: rank.Code{K: 1, M: 0}, Hosts: []Host{hostOf(1), hostOf(2)}})
	drained := stepped(t, start, func(m Membership) (Membership, error) { return m.Drain(idOf(1)) })
	let := stepped(t, drained, func(m Membership) (Membership, error) { return m.Let(diskOf(1).ID, idOf(1)) })
	gone := stepped(t, let, func(m Membership) (Membership, error) { return m.Leave(idOf(1)) })
	back := hostOf(1)
	back.Disks = append(back.Disks, diskOf(2))
	change, ok := Next(t.Context(), gone, Want{Code: rank.Code{K: 1, M: 0}, Hosts: []Host{back, hostOf(2)}})
	if !ok {
		t.Fatal("Next has no step for a host that came back")
	}
	joined := stepped(t, gone, change)
	disk, _ := joined.Disk(diskOf(1).ID)
	other, _ := joined.Disk(diskOf(2).ID)
	if disk.Member != idOf(1) || disk.State != Attaching || other.Member != idOf(2) || other.State != Serving {
		t.Fatalf("the host came back as %s", describe(joined))
	}
}

// Draining a member is a change even when it holds no disk, and draining it
// again is not; the member listed first drains as any other does.
func TestDrainingAMemberIsAChangeOnce(t *testing.T) {
	m := joined(t, rank.Code{K: 1, M: 1}, 2)
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Join(memberOf(3)) })
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Drain(idOf(3)) })
	if _, err := m.Drain(idOf(3)); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("draining a draining member again said %v, want ErrUnchanged", err)
	}
	m = stepped(t, m, func(m Membership) (Membership, error) { return m.Drain(idOf(1)) })
	if member, _ := m.Member(idOf(1)); member.State != Draining {
		t.Fatalf("the first member drained to %s", describe(m))
	}
}

// A view that reads the generation it holds again holds the same membership
// and says nothing changed.
func TestAViewReadingItsOwnGenerationChangesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		ctx := sim.WithRuntime(t.Context(), runtime)
		store := storeOver(t, runtime, runtime.ObjectStore(), "writer")
		if _, err := store.Update(ctx, join(1)); err != nil {
			t.Fatal(err)
		}
		view := NewView(ctx, ViewConfig{Store: store, Initial: Empty(), Interval: -1})
		defer view.Close()
		if _, err := view.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		changed := view.Changed()
		if _, err := view.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-changed:
			t.Fatal("a view that read the generation it held says it changed")
		default:
		}
	})
}

// A membership of disks a list ranks is the list's: every cache a member of
// its own, serving its own disk, at the cache's address.
func TestFromListServesEveryCacheOfTheList(t *testing.T) {
	list, err := rank.NewList(rank.Code{K: 1, M: 1}, []rank.Cache{{Identity: idOf(1), Weight: 2, Address: "a:1"},
		{Identity: idOf(2), Weight: 1, Address: "b:1"}})
	if err != nil {
		t.Fatal(err)
	}
	m, err := FromList(3, list)
	if err != nil {
		t.Fatal(err)
	}
	if m.Generation() != 3 || !m.List().Equal(list) || !m.Serves(idOf(1), idOf(1)) || !m.Serves(idOf(2), idOf(2)) {
		t.Fatalf("FromList made %s", describe(m))
	}
}
