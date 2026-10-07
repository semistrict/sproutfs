package membership

import (
	"bytes"
	"context"
	"slices"

	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// Carrying the membership out. The membership says which member serves each
// shard; the cloud's attach API is how a shard reaches its member's machine.
// A controller reads both each pass and asks for what the membership calls
// for, never for what it remembers having asked: so a shard attached and not
// recorded, or recorded and not attached, converges whichever process
// crashed between the two.
//
//   - A shard attaching or serving is attached to its member's machine and
//     no other: detached from any other machine first, then attached.
//   - A shard releasing stays where it is while its member's host still
//     holds it open, and is detached from every machine once it does not,
//     or once that host is gone.
//   - A shard released is attached to no machine.
//   - A journal disk follows the same rules, except that one released and
//     reserved for a machine in the pool is attached to that machine, so the
//     member there finds it attached when it is assigned it.
//
// A shard is let go (Next's step 2) only once the cloud says it is attached
// to no machine, so a disk is released and detached before it is assigned
// again, and a single-writer disk is attached to one machine at a time.

// Action is one call a controller makes of the cloud: attach a volume to a
// machine, or detach it from one.
type Action struct {
	Volume, Machine string
	Attach          bool
}

// Carry is the attaches and detaches that carry m out, given where want
// says each shard is attached now: every detach first, then every attach, in
// the order of the shards' identities. A shard whose attachments the cloud
// did not report is left alone.
func Carry(ctx context.Context, m Membership, want Want) []Action {
	hosts := make(map[rank.Identity]Host, len(want.Hosts))
	for _, host := range want.Hosts {
		hosts[host.ID] = host
	}
	shards := slices.Clone(want.Shards)
	slices.SortFunc(shards, func(a, b Shard) int { return compareIdentities(a.ID, b.ID) })
	var detaches, attaches []Action
	for _, shard := range shards {
		if !shard.Known {
			continue
		}
		target := ""
		keep := false
		if disk, listed := m.Disk(shard.ID); listed {
			host, wanted := hosts[disk.Member]
			switch disk.State {
			case Attaching, Serving:
				if wanted {
					target = host.Machine
				}
				// A member whose host is quiet or on no machine keeps what it has.
				keep = target == ""
			case Releasing:
				keep = wanted && slices.ContainsFunc(host.Disks, func(held Disk) bool { return held.ID == shard.ID })
			}
		}
		if keep && !sim.Bug(ctx, "membership-detach-held-shard") {
			continue
		}
		for _, machine := range shard.Machines {
			if machine != target {
				detaches = append(detaches, Action{Volume: shard.Volume, Machine: machine})
			}
		}
		if target != "" && !slices.Contains(shard.Machines, target) {
			attaches = append(attaches, Action{Volume: shard.Volume, Machine: target, Attach: true})
		}
	}
	journals := slices.Clone(want.Journals)
	slices.SortFunc(journals, func(a, b Shard) int { return compareIdentities(a.ID, b.ID) })
	for _, journal := range journals {
		if !journal.Known {
			continue
		}
		target, keep := carryJournal(ctx, m, want, hosts, journal)
		if keep {
			continue
		}
		for _, machine := range journal.Machines {
			if machine != target {
				detaches = append(detaches, Action{Volume: journal.Volume, Machine: machine})
			}
		}
		if target != "" && !slices.Contains(journal.Machines, target) {
			attaches = append(attaches, Action{Volume: journal.Volume, Machine: target, Attach: true})
		}
	}
	return append(detaches, attaches...)
}

func compareIdentities(a, b rank.Identity) int { return bytes.Compare(a[:], b[:]) }
