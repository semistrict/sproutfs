package host

import (
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/rank"
)

// /status reports this host to the membership and the membership it holds:
// the generation, the code, what it lists, when it was last read, and why a
// read failed. A host that keeps no cache reports no member, and one that has
// read no membership reports no time.
func TestStatusReportsTheMemberAndTheMembershipHeld(t *testing.T) {
	self := membership.Host{ID: rank.Identity{0xab, 1}, Address: "10.0.0.1:8081",
		Disks: []membership.Disk{{ID: rank.Identity{0xab, 1}, Volume: "cache-0", Weight: 4}}}
	held, err := membership.New(7, rank.Code{K: 1, M: 1},
		[]membership.Member{{ID: self.ID, Address: self.Address, State: membership.Active},
			{ID: rank.Identity{0x0c}, Address: "10.0.0.2:8081", State: membership.Joining}},
		[]membership.Disk{{ID: self.ID, Volume: "cache-0", Weight: 4, Member: self.ID, State: membership.Serving,
			Assigned: 3}})
	if err != nil {
		t.Fatal(err)
	}
	read := time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)
	member, report := memberReport(self, membership.ViewStatus{Membership: held, Read: read, Reads: 4, Failures: 2,
		Error: "unavailable"})
	want := hostapi.Member{Identity: "ab010000000000000000000000000000", Address: "10.0.0.1:8081",
		Disks: []hostapi.MemberDisk{{Identity: "ab010000000000000000000000000000", Volume: "cache-0", Weight: 4,
			State: "serving"}}}
	if member == nil || member.Identity != want.Identity || member.Address != want.Address ||
		len(member.Disks) != 1 || member.Disks[0] != want.Disks[0] {
		t.Fatalf("the member is reported as %+v, want %+v", member, want)
	}
	if report.Generation != 7 || report.K != 1 || report.M != 1 || report.Members != 2 || report.Disks != 1 ||
		report.Read == nil || !report.Read.Equal(read) || report.Reads != 4 || report.Failures != 2 ||
		report.Error != "unavailable" {
		t.Fatalf("the membership is reported as %+v", report)
	}
	member, report = memberReport(membership.Host{}, membership.ViewStatus{Membership: alone(membership.Host{})})
	if member != nil || report.Read != nil || report.Generation != 0 || report.K != 1 || report.M != 0 ||
		report.Disks != 0 {
		t.Fatalf("a host with no cache that read no membership reports %+v and %+v", member, report)
	}
}
