package membership

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
)

// ShardControl is one controller's pass over a deployment's shards: it reads
// where the cloud has each, takes the one step of the membership Next takes
// towards the hosts it is given and those shards, and then asks the cloud for
// what Carry says that membership calls for. It holds nothing between passes
// but the deployment's list of volumes: every pass starts from the
// membership as the store holds it and the disks as the cloud reports them,
// so a controller that crashed anywhere, or two at once, converge.
type ShardControl struct {
	Store *Store
	Disks platform.NetworkDisks
	// Volumes is the deployment's shards, by the names the cloud knows them
	// by: a fixed set, changed only when the cache is resized on purpose.
	Volumes []string
}

// Describe reads where the cloud has each of the deployment's shards, and its
// weight from its size. A shard the cloud did not describe is not known, and
// takes the weight m lists it with, or is left out where m does not list it
// yet.
func (c *ShardControl) Describe(ctx context.Context, m Membership) []Shard {
	var shards []Shard
	for _, volume := range c.Volumes {
		shard := Shard{Disk: Disk{ID: ShardIdentity(volume), Volume: volume}}
		described, err := c.Disks.Describe(ctx, volume)
		switch {
		case err == nil:
			shard.Weight, shard.Machines, shard.Known = rank.Weight(described.Bytes), described.Machines, true
		default:
			listed, ok := m.Disk(shard.ID)
			if !ok {
				slog.WarnContext(ctx, "membership: a shard the cloud did not describe is not listed yet",
					"volume", volume, "error", err)
				continue
			}
			shard.Weight = listed.Weight
		}
		shards = append(shards, shard)
	}
	return shards
}

// Pass takes one step of the membership towards want, with the shards as the
// cloud has them now, and carries out what the membership it leaves calls
// for. It reports that membership, whether it changed it, and what failed:
// the step, or the calls of the cloud, every one of which is made again by
// the next pass if it is still called for.
func (c *ShardControl) Pass(ctx context.Context, want Want) (Membership, bool, error) {
	current, err := c.Store.Read(ctx)
	if err != nil {
		return Membership{}, false, err
	}
	want.Shards = c.Describe(ctx, current)
	next, changed, err := c.Store.Reconcile(ctx, want)
	if err != nil {
		return next, changed, err
	}
	var failed []error
	for _, action := range Carry(ctx, next, want) {
		if action.Attach {
			err = c.Disks.Attach(ctx, action.Volume, action.Machine)
		} else {
			err = c.Disks.Detach(ctx, action.Volume, action.Machine)
		}
		if err != nil {
			verb := "detaching"
			if action.Attach {
				verb = "attaching"
			}
			failed = append(failed, fmt.Errorf("%s %s on %s: %w", verb, action.Volume, action.Machine, err))
		}
	}
	return next, changed, errors.Join(failed...)
}
