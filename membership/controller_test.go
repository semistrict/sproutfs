package membership

import (
	"slices"
	"testing"

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
		change, ok := Next(m, want)
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

// Next joins each wanted host with its disk in one generation, in identity
// order, then serves each disk in one more, and takes the code last: from
// the empty membership, three hosts under 2+1 are seven generations.
func TestNextJoinsAHostInOneGenerationAndServesItInTheNext(t *testing.T) {
	want := Want{Code: rank.Code{K: 2, M: 1}, Hosts: []Host{hostOf(3), hostOf(1), hostOf(2)}}
	end, steps := toward(t, Empty(), want)
	if len(steps) != 7 || end.Code() != want.Code {
		t.Fatalf("Next took %d steps to %s", len(steps), describe(end))
	}
	first := steps[0]
	if len(first.Members()) != 1 || len(first.Disks()) != 1 || first.Members()[0].ID != idOf(1) {
		t.Fatalf("the first step is %s, want host 1 joining with its disk", describe(first))
	}
	if joined := steps[2]; len(joined.Members()) != 3 || joined.Serves(idOf(1), diskOf(1).ID) {
		t.Fatalf("the third step is %s, want every host joined and no disk serving yet", describe(joined))
	}
	if served := steps[3]; !served.Serves(idOf(1), diskOf(1).ID) {
		t.Fatalf("the fourth step is %s, want host 1's disk serving", describe(served))
	}
	for n := byte(1); n <= 3; n++ {
		if member, _ := end.Member(idOf(n)); member.State != Active || !end.Serves(idOf(n), diskOf(n).ID) {
			t.Fatalf("host %d ends %s", n, describe(end))
		}
	}
	if _, ok := Next(end, want); ok {
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
	if _, ok := Next(end, Want{Code: rank.Code{K: 1, M: 0}, Hosts: []Host{quiet}}); ok {
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
