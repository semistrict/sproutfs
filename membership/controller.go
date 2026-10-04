package membership

import (
	"bytes"
	"slices"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
)

// Want is what a controller moves the membership towards: the code, and every
// host that should be in it, as the host last reported itself.
type Want struct {
	Code  rank.Code
	Hosts []Host
}

// Host is one host a controller wants in the membership: its identity, the
// address its peer server answers at, and the disks it has attached.
type Host struct {
	ID      rank.Identity
	Address platform.Address
	// Disks is every disk the host reports attached, each with its identity,
	// its volume and its weight.
	Disks []Disk
}

// A Change is one step a controller makes: it builds the next generation
// from the one it is given, or says ErrUnchanged when that one no longer
// needs it.
type Change func(Membership) (Membership, error)

// Next is the one change that moves m towards want, and false when m is
// there. A controller makes it, reads the membership again and asks for the
// next, so the data each step moves is bounded: one join, one leave or one
// change of weight a generation. The steps, in the order they are taken:
//
//  1. A member whose host is not wanted is drained: it is draining, and its
//     disks are releasing. Nothing moves.
//  2. A releasing disk whose member's host is not wanted is let go: that
//     host is gone, so nobody serves it.
//  3. A released disk no wanted host reports is removed. Its windows move to
//     the disks ranked after it: this is the leave.
//  4. A draining member assigned no disk leaves.
//  5. A wanted host that is not listed joins, with every disk it reports
//     that is not listed or is released: one generation, the join.
//  6. A listed member follows its host's address.
//  7. An attaching disk its member's host reports is served.
//  8. A disk follows the weight its host reports.
//  9. The membership takes want's code.
//
// So a host drains before it leaves, and a quiet host, which a controller
// still wants, keeps its place.
func Next(m Membership, want Want) (Change, bool) {
	hosts := make(map[rank.Identity]Host, len(want.Hosts))
	reported := make(map[rank.Identity]rank.Identity)
	for _, host := range sortedHosts(want.Hosts) {
		hosts[host.ID] = host
		for _, disk := range host.Disks {
			if _, taken := reported[disk.ID]; !taken {
				reported[disk.ID] = host.ID
			}
		}
	}
	for _, member := range m.members {
		if _, wanted := hosts[member.ID]; !wanted && member.State != Draining {
			id := member.ID
			return func(m Membership) (Membership, error) { return m.Drain(id) }, true
		}
	}
	for _, disk := range m.disks {
		if disk.State != Releasing {
			continue
		}
		if _, wanted := hosts[disk.Member]; !wanted {
			id, member := disk.ID, disk.Member
			return func(m Membership) (Membership, error) { return m.Let(id, member) }, true
		}
	}
	for _, disk := range m.disks {
		if _, wanted := reported[disk.ID]; !wanted && disk.State == Released {
			id := disk.ID
			return func(m Membership) (Membership, error) { return m.Remove(id) }, true
		}
	}
	for _, member := range m.members {
		if member.State == Draining && !slices.ContainsFunc(m.disks, func(disk Disk) bool {
			return disk.Member == member.ID
		}) {
			id := member.ID
			return func(m Membership) (Membership, error) { return m.Leave(id) }, true
		}
	}
	for _, host := range sortedHosts(want.Hosts) {
		if _, listed := m.Member(host.ID); listed {
			continue
		}
		var disks []Disk
		for _, disk := range host.Disks {
			found, listed := m.Disk(disk.ID)
			if reported[disk.ID] == host.ID && (!listed || found.State == Released) {
				disks = append(disks, disk)
			}
		}
		joining := Member{ID: host.ID, Address: host.Address}
		return func(m Membership) (Membership, error) { return m.Join(joining, disks...) }, true
	}
	for _, member := range m.members {
		if host, wanted := hosts[member.ID]; wanted && host.Address != member.Address && member.State != Draining {
			id, address := member.ID, host.Address
			return func(m Membership) (Membership, error) { return m.Move(id, address) }, true
		}
	}
	for _, disk := range m.disks {
		if disk.State == Attaching && reported[disk.ID] == disk.Member {
			id, member := disk.ID, disk.Member
			return func(m Membership) (Membership, error) { return m.Serve(id, member) }, true
		}
	}
	for _, host := range sortedHosts(want.Hosts) {
		for _, disk := range host.Disks {
			found, listed := m.Disk(disk.ID)
			if listed && reported[disk.ID] == host.ID && found.Weight != disk.Weight {
				id, weight := disk.ID, disk.Weight
				return func(m Membership) (Membership, error) { return m.Weigh(id, weight) }, true
			}
		}
	}
	if want.Code != m.code && want.Code.Validate() == nil {
		code := want.Code
		return func(m Membership) (Membership, error) { return m.Recode(code) }, true
	}
	return nil, false
}

// sortedHosts is hosts in identity order, so two controllers given one want
// take its steps in one order.
func sortedHosts(hosts []Host) []Host {
	out := slices.Clone(hosts)
	slices.SortStableFunc(out, func(a, b Host) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	return out
}

// FromList is the membership at generation in which every cache of list is a
// member of its own, at the cache's address, serving the disk of the cache's
// identity, assigned at generation 1: a cluster whose every host keeps one
// disk, once every disk serves. A test or a bench that builds a cluster by
// hand starts from it.
func FromList(generation uint64, list rank.List) (Membership, error) {
	var members []Member
	var disks []Disk
	for _, cache := range list.Caches() {
		members = append(members, Member{ID: cache.Identity, Address: cache.Address, State: Active})
		disks = append(disks, Disk{ID: cache.Identity, Volume: cache.Identity.String(), Weight: cache.Weight,
			Member: cache.Identity, State: Serving, Assigned: 1})
	}
	return New(generation, list.Code(), members, disks)
}
