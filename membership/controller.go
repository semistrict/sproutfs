package membership

import (
	"bytes"
	"context"
	"crypto/sha256"
	"slices"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// Want is what a controller moves the membership towards: the code and the
// codes the deployment used before it, newest first, every host that should
// be in it, as the host last reported itself, and the deployment's shards.
type Want struct {
	Code    rank.Code
	Earlier []rank.Code
	Hosts   []Host
	// Shards is the deployment's network disks, a fixed set the membership
	// assigns to its members, each where the cloud last said it is attached.
	Shards []Shard
	// Journals is the deployment's journal disks as the cloud lists them,
	// each where it is attached, and JournalsKnown whether the listing
	// succeeded: a disk missing from a listing that failed is not one the
	// cloud has deleted. Pool is the machines the deployment's hosts run on,
	// each of which keeps a journal disk reserved while durable flush is on,
	// and Expired the journal disks that have been free and empty for long
	// enough to delete. All are empty for a deployment that journals nothing.
	Journals      []Shard
	JournalsKnown bool
	Pool          []string
	Expired       []rank.Identity
}

// Host is one host a controller wants in the membership: its identity, the
// address its peer server answers at, the disks it has attached, and the
// machine it runs on.
type Host struct {
	ID      rank.Identity
	Address platform.Address
	// Disks is every disk the host reports attached, each with its identity,
	// its volume and its weight: its own disk, or the shards it holds open.
	// Each disk's State is its state in the host's copy of the membership,
	// and zero where that copy does not list it.
	Disks []Disk
	// Held is the generation of the host's copy of the membership, which
	// its disks' states are from.
	Held uint64
	// Machine is the machine the host runs on, which the cloud attaches its
	// shards to; empty for a host no shard can be attached to.
	Machine string
	// Leaving is a host on its way out, as a pod that is terminating is: it
	// still answers, and is drained, but is assigned nothing more.
	Leaving bool
}

// Shard is one of a deployment's network disks: its identity, volume and
// weight, and the machines the cloud says it is attached to.
type Shard struct {
	Disk
	// Machines is every machine the cloud says the disk is attached to, and
	// Known whether the cloud said anything: a disk whose state could not be
	// read is let go of nothing.
	Machines []string
	Known    bool
}

// ShardIdentity is the identity of the shard on a volume: derived from the
// volume's name, so every controller lists one shard under one identity
// before any host has opened it. The shard's header names it too, and a
// device whose header names another is made anew under it.
func ShardIdentity(volume string) rank.Identity {
	sum := sha256.Sum256([]byte("sproutfs shard\x00" + volume))
	return rank.Identity(sum[:len(rank.Identity{})])
}

// JournalIdentity is the identity of the journal disk on a volume, derived
// from its name as a shard's is.
func JournalIdentity(volume string) rank.Identity {
	sum := sha256.Sum256([]byte("sproutfs journal\x00" + volume))
	return rank.Identity(sum[:len(rank.Identity{})])
}

// A Change is one step a controller makes: it builds the next generation
// from the one it is given, or says ErrUnchanged when that one no longer
// needs it.
type Change func(Membership) (Membership, error)

// Next is the one change that moves m towards want, and false when m is
// there. A controller makes it, reads the membership again and asks for the
// next, so the data each step moves is bounded: one join, one leave, one
// change of weight or one move of a shard a generation. The steps, in the
// order they are taken:
//
//  0. The membership takes want's codes, so a new deployment's disks are
//     filled under its code from the start.
//  1. A member whose host is not wanted, or is leaving, is drained: it is
//     draining, and its disks are releasing. Nothing moves.
//  2. A releasing disk is let go once nobody serves it: a host's own disk
//     once its host is gone, or its host's copy shows this release, as the
//     copy of a pod that came back over the disk does; a shard once its
//     member's host is gone or no longer holds it, and the cloud says it is
//     attached to no machine.
//  3. A released disk no wanted host reports, and that is not a wanted
//     shard, is removed. Its windows move to the disks ranked after it: this
//     is the leave.
//  4. A wanted shard not listed is added, released: it takes its place in
//     every window's ranks at once.
//  5. A draining member assigned no disk leaves.
//  6. A wanted host that is not listed, and not leaving, joins, with every
//     disk it reports that is not a shard and is not listed or is released:
//     one generation, the join.
//  7. A listed member follows its host's address.
//  8. An attaching disk its member's host reports is served.
//  9. A disk follows the weight its host reports, and a shard the weight
//     wanted for it.
//  10. A released shard is assigned to the member that serves the fewest
//     disks, among the active and joining members whose hosts are wanted,
//     not leaving, and on a machine.
//  11. While no shard is attaching or releasing, a member serving two more
//     shards than another releases one, so the shards spread over the
//     members as they join.
//
// So a host drains before it leaves, a shard is released, detached and let
// go before it is assigned again, and a quiet host, which a controller still
// wants, keeps its place.
func Next(ctx context.Context, m Membership, want Want) (Change, bool) {
	if (want.Code != m.code || !slices.Equal(want.Earlier, m.earlier)) && want.Code.Validate() == nil {
		code, earlier := want.Code, slices.Clone(want.Earlier)
		return func(m Membership) (Membership, error) { return m.Recode(code, earlier...) }, true
	}
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
	shards := make(map[rank.Identity]Shard, len(want.Shards)+len(want.Journals))
	for _, shard := range want.Shards {
		shards[shard.ID] = shard
	}
	// A journal disk is let go as a shard is: once its holder has closed it
	// and the cloud has it on no machine.
	for _, journal := range want.Journals {
		shards[journal.ID] = journal
	}
	for _, member := range m.members {
		if host, wanted := hosts[member.ID]; (!wanted || host.Leaving) && member.State != Draining {
			id := member.ID
			return func(m Membership) (Membership, error) { return m.Drain(id) }, true
		}
	}
	for _, disk := range m.disks {
		if disk.State != Releasing {
			continue
		}
		host, wanted := hosts[disk.Member]
		shard, isShard := shards[disk.ID]
		var gone bool
		switch {
		case !isShard:
			// A host that read the release has stopped serving the disk, as
			// its member would say if it wrote the membership itself. Without
			// this, a pod replaced over its disk while its member drained
			// would never serve it again: it is wanted, so never gone, and
			// listed, so never joins.
			gone = !wanted || host.readRelease(ctx, disk) && !sim.Bug(ctx, "membership-let-only-a-gone-hosts-disk")
		default:
			// A shard is let go once its member has closed it, or is gone,
			// and the cloud has it on no machine: nobody can serve it.
			closed := !wanted || !slices.ContainsFunc(host.Disks, func(held Disk) bool { return held.ID == disk.ID })
			gone = closed && (shard.Known && len(shard.Machines) == 0 || sim.Bug(ctx, "membership-let-attached-shard"))
		}
		if gone {
			id, member := disk.ID, disk.Member
			return func(m Membership) (Membership, error) { return m.Let(id, member) }, true
		}
	}
	for _, disk := range m.disks {
		_, wanted := reported[disk.ID]
		if _, isShard := shards[disk.ID]; !wanted && !isShard && disk.State == Released && disk.Kind == Cache {
			id := disk.ID
			return func(m Membership) (Membership, error) { return m.Remove(id) }, true
		}
	}
	for _, shard := range want.Shards {
		if _, listed := m.Disk(shard.ID); !listed {
			added := shard.Disk
			return func(m Membership) (Membership, error) { return m.Add(added) }, true
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
		if _, listed := m.Member(host.ID); listed || host.Leaving {
			continue
		}
		var disks []Disk
		for _, disk := range host.Disks {
			found, listed := m.Disk(disk.ID)
			if _, isShard := shards[disk.ID]; !isShard && disk.Kind == Cache && reported[disk.ID] == host.ID &&
				(!listed || found.State == Released) {
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
			if _, isShard := shards[disk.ID]; !isShard && disk.Kind == Cache && listed &&
				reported[disk.ID] == host.ID && found.Weight != disk.Weight {
				id, weight := disk.ID, disk.Weight
				return func(m Membership) (Membership, error) { return m.Weigh(id, weight) }, true
			}
		}
	}
	for _, shard := range want.Shards {
		if found, listed := m.Disk(shard.ID); listed && found.Weight != shard.Weight {
			id, weight := shard.ID, shard.Weight
			return func(m Membership) (Membership, error) { return m.Weigh(id, weight) }, true
		}
	}
	if change, ok := nextJournalStep(m, hosts, want); ok {
		return change, true
	}
	cacheShards := make(map[rank.Identity]Shard, len(want.Shards))
	for _, shard := range want.Shards {
		cacheShards[shard.ID] = shard
	}
	return nextShardMove(m, hosts, cacheShards)
}

// nextShardMove is the step that puts a released shard on a member, or, while
// no shard moves, the one that releases a shard from the member serving the
// most so it spreads over the rest: steps 10 and 11 of Next.
func nextShardMove(m Membership, hosts map[rank.Identity]Host, shards map[rank.Identity]Shard) (Change, bool) {
	// load is the shards each member that may take one is assigned now.
	load := make(map[rank.Identity]int)
	var takers []rank.Identity
	for _, member := range m.members {
		host, wanted := hosts[member.ID]
		if member.State == Draining || !wanted || host.Leaving || host.Machine == "" {
			continue
		}
		load[member.ID] = 0
		takers = append(takers, member.ID)
	}
	moving := false
	for _, disk := range m.disks {
		if _, isShard := shards[disk.ID]; !isShard || disk.State == Released {
			continue
		}
		if _, taking := load[disk.Member]; taking {
			load[disk.Member]++
		}
		moving = moving || disk.State == Attaching || disk.State == Releasing
	}
	if len(takers) == 0 {
		return nil, false
	}
	least := func() rank.Identity {
		chosen := takers[0]
		for _, member := range takers[1:] {
			if load[member] < load[chosen] {
				chosen = member
			}
		}
		return chosen
	}
	for _, disk := range m.disks {
		if _, isShard := shards[disk.ID]; isShard && disk.State == Released {
			id, member := disk.ID, least()
			return func(m Membership) (Membership, error) { return m.Assign(id, member) }, true
		}
	}
	if moving {
		return nil, false
	}
	most := takers[0]
	for _, member := range takers[1:] {
		if load[member] > load[most] {
			most = member
		}
	}
	if load[most]-load[least()] < 2 {
		return nil, false
	}
	for _, disk := range m.disks {
		if _, isShard := shards[disk.ID]; isShard && disk.Member == most && disk.State == Serving {
			id := disk.ID
			return func(m Membership) (Membership, error) { return m.Release(id) }, true
		}
	}
	return nil, false
}

// readRelease reports whether the host's copy of the membership has disk, as
// it is assigned now, releasing. A copy at or after the generation that
// assigned the disk is of this assignment, in which releasing comes only
// after serving; an older copy that has it releasing read the release of an
// earlier one, and the host may serve the disk since.
func (h Host) readRelease(ctx context.Context, disk Disk) bool {
	if h.Held < disk.Assigned && !sim.Bug(ctx, "membership-let-on-an-earlier-release") {
		return false
	}
	return slices.ContainsFunc(h.Disks, func(held Disk) bool { return held.ID == disk.ID && held.State == Releasing })
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
	return New(generation, list.Code(), members, disks, list.Earlier()...)
}
