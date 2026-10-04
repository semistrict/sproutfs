package host_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/rank"
)

// A host reports itself to the membership as JSON, which the orchestrator
// reads back as the host it wants in the membership. Each disk carries the
// state the membership the host holds gives it.
func TestAMemberCrossesTheWireIntact(t *testing.T) {
	self := membership.Host{ID: rank.Identity{3, 1}, Address: "10.0.0.3:8081",
		Disks: []membership.Disk{{ID: rank.Identity{3, 1}, Volume: "cache-0", Weight: 2}}}
	held, err := membership.New(4, rank.Code{K: 1, M: 0},
		[]membership.Member{{ID: self.ID, Address: self.Address, State: membership.Active}},
		[]membership.Disk{{ID: self.ID, Volume: "cache-0", Weight: 2, Member: self.ID, State: membership.Serving,
			Assigned: 2}})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(hostapi.MemberOf(self, held))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"identity":"03010000000000000000000000000000","address":"10.0.0.3:8081","disks":[` +
		`{"identity":"03010000000000000000000000000000","volume":"cache-0","weight":2,"state":"serving"}]}`; string(encoded) != want {
		t.Fatalf("the member is written as %s, want %s", encoded, want)
	}
	var decoded hostapi.Member
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	read, err := decoded.Host()
	if err != nil {
		t.Fatal(err)
	}
	if read.ID != self.ID || read.Address != self.Address || !slices.Equal(read.Disks, self.Disks) {
		t.Fatalf("the member read back is %+v, want %+v", read, self)
	}
}

// A member no controller could put in the membership is refused as it is
// read: an identity that is not one, or a disk with no weight.
func TestAMemberNoControllerCouldWantIsRefused(t *testing.T) {
	for name, member := range map[string]hostapi.Member{
		"a bad identity":      {Identity: "nope"},
		"a bad disk identity": {Identity: rank.Identity{1}.String(), Disks: []hostapi.MemberDisk{{Identity: "x", Weight: 1}}},
		"no weight":           {Identity: rank.Identity{1}.String(), Disks: []hostapi.MemberDisk{{Identity: rank.Identity{1}.String()}}},
	} {
		if _, err := member.Host(); !errors.Is(err, rank.ErrInvalid) {
			t.Fatalf("a member with %s is read with %v, want rank.ErrInvalid", name, err)
		}
	}
}
