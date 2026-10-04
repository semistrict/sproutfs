package checkpoint

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/rank"
)

// The disks a cache keeps for the cluster. A host keeps a disk of its own, a
// file on its node, or it serves shards: network disks the membership assigns
// it, which it opens as each is assigned and closes as each is released
// (docs/hosting.md, "Shards on network disks"). Both are disks of the
// cluster's cache, ranked by their identities. Which disks this host serves,
// under which generation, is the membership's to say; the cache keeps,
// serves and reads only a disk the membership it holds has this host serve.
//
// A shard is opened under the generation that assigned it to this host. Its
// header carries a lease, the assignment it was last opened under: a member
// whose assignment is older than the lease refuses the disk, and one whose
// assignment is newer writes its own before it reads anything back. The
// cache reads the lease again before every region it opens, so a member that
// lost a shard it still holds the device of stops writing it.

// The probes shards mark.
const (
	// ProbeShardLeaseRefused is a shard refused as it opened, because its
	// lease names an assignment newer than the one it was opened under.
	ProbeShardLeaseRefused = "checkpoint/shard-lease-refused"
	// ProbeShardFenced is a shard that found, while it was served, that
	// another member had taken its lease, and wrote it no more.
	ProbeShardFenced = "checkpoint/shard-fenced"
)

// ErrFenced reports a shard whose lease names an assignment newer than the one
// it was opened under: another member has served it since.
var ErrFenced = errors.New("checkpoint: the shard is leased under a newer assignment")

// ErrNotKept reports a request for a disk this cache does not keep.
var ErrNotKept = errors.New("checkpoint: the cache does not keep the disk")

// Lease is the assignment a shard is opened under: the generation of the
// membership that assigned it to this host, and this host's identity in it.
type Lease struct {
	Assigned uint64
	Member   rank.Identity
}

// ShardConfig is one shard a cache serves: its device, its identity, its
// share, and the assignment it is opened under.
type ShardConfig struct {
	// Device is the shard's block device, the caller's to close after the
	// shard is removed.
	Device platform.File
	// Identity is the shard's, which windows are ranked by. A device whose
	// header names another identity, or another deployment, is made anew
	// under this one.
	Identity rank.Identity
	// Budget is the shard's share of its own device and the writes it may
	// make: the shard's disk is its own, and nothing else on the host counts
	// against it.
	Budget DiskBudget
	Lease  Lease
}

// cluster is what a cache keeps for the cluster and how it places it: the
// membership it follows, this host's identity in it, the share of windows the
// membership places, and the disks this host keeps.
type cluster struct {
	percent int

	mu     sync.Mutex
	source membership.Source
	member rank.Identity
	// own is the host's own disk, nil for a host that keeps none.
	own    *cacheDisk
	shards map[rank.Identity]*heldShard
}

// heldShard is one shard the cache serves, and the requests using it now.
type heldShard struct {
	disk  *cacheDisk
	users int
	// leaving is a shard being removed, which no new request may use, and
	// left is closed once its last user has finished.
	leaving bool
	left    chan struct{}
}

func newCluster(percent int) *cluster {
	return &cluster{percent: percent, shards: make(map[rank.Identity]*heldShard)}
}

// follow has the cache place and serve by the membership source holds, as the
// host of member.
func (c *cluster) follow(source membership.Source, member rank.Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.source, c.member = source, member
}

// self is this host's identity in the membership it follows.
func (c *cluster) self() rank.Identity {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.member
}

// following is the membership the cache places by, and whether it follows one.
func (c *cluster) following() (membership.Membership, bool) {
	c.mu.Lock()
	source := c.source
	c.mu.Unlock()
	if source == nil {
		return membership.Membership{}, false
	}
	return source.Current(), true
}

// catch is the membership the cache places by, read again first when it is
// older than generation.
func (c *cluster) catch(ctx context.Context, generation uint64) (membership.Membership, error) {
	c.mu.Lock()
	source := c.source
	c.mu.Unlock()
	if source == nil {
		return membership.Membership{}, errNotFollowing
	}
	return source.Catch(ctx, generation)
}

// placedBy is the membership key's window is placed by, and whether it is
// placed by one: a cache that follows a membership places by it the windows
// inside the share the cluster cache is turned on for. ignoreShare is the
// guard that places every window by it.
func (c *cluster) placedBy(key diskKey, ignoreShare bool) (membership.Membership, bool) {
	m, ok := c.following()
	if !ok || !ignoreShare && !key.rankWindow().InShare(c.percent) {
		return membership.Membership{}, false
	}
	return m, true
}

// serves reports whether this host serves disk under m.
func (c *cluster) serves(m membership.Membership, disk rank.Identity) bool {
	return m.Serves(c.self(), disk)
}

// hold is the disk of identity this cache keeps, held for a request until
// release is called, and false for one it does not keep or is removing.
func (c *cluster) hold(identity rank.Identity) (*cacheDisk, func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.own != nil && c.own.identity == identity {
		return c.own, func() {}, true
	}
	held := c.shards[identity]
	if held == nil || held.leaving {
		return nil, nil, false
	}
	held.users++
	return held.disk, sync.OnceFunc(func() { c.release(held) }), true
}

func (c *cluster) release(held *heldShard) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held.users--
	if held.leaving && held.users == 0 {
		close(held.left)
	}
}

// keeps reports whether the cache keeps the disk of identity now.
func (c *cluster) keeps(identity rank.Identity) bool {
	_, release, ok := c.hold(identity)
	if ok {
		release()
	}
	return ok
}

// disks is the identities of every disk the cache keeps, in order.
func (c *cluster) disks() []rank.Identity {
	c.mu.Lock()
	defer c.mu.Unlock()
	var identities []rank.Identity
	if c.own != nil {
		identities = append(identities, c.own.identity)
	}
	for identity, held := range c.shards {
		if !held.leaving {
			identities = append(identities, identity)
		}
	}
	slices.SortFunc(identities, compareIdentity)
	return identities
}

// add has the cache keep a shard's disk.
func (c *cluster) add(disk *cacheDisk) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, taken := c.shards[disk.identity]; taken || c.own != nil && c.own.identity == disk.identity {
		return fmt.Errorf("%w: the cache keeps disk %s already", ErrInvalidConfig, disk.identity)
	}
	c.shards[disk.identity] = &heldShard{disk: disk, left: make(chan struct{})}
	return nil
}

// remove stops the cache keeping a shard: no request takes it from here on,
// and remove returns its disk once every request using it has finished.
func (c *cluster) remove(ctx context.Context, identity rank.Identity) (*cacheDisk, error) {
	c.mu.Lock()
	held := c.shards[identity]
	if held == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrNotKept, identity)
	}
	if !held.leaving {
		held.leaving = true
		if held.users == 0 {
			close(held.left)
		}
	}
	c.mu.Unlock()
	select {
	case <-held.left:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	c.mu.Lock()
	delete(c.shards, identity)
	c.mu.Unlock()
	return held.disk, nil
}

// all is every disk the cache keeps, own and shards, for its statistics and
// its close.
func (c *cluster) all() []*cacheDisk {
	c.mu.Lock()
	defer c.mu.Unlock()
	var disks []*cacheDisk
	if c.own != nil {
		disks = append(disks, c.own)
	}
	for _, identity := range slices.SortedFunc(maps.Keys(c.shards), compareIdentity) {
		disks = append(disks, c.shards[identity].disk)
	}
	return disks
}

func compareIdentity(a, b rank.Identity) int { return bytes.Compare(a[:], b[:]) }
