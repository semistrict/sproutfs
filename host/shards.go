package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/checkpoint"
	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// Serving shards. A host given network disks keeps no cache disk of its own:
// the cluster's cache is a fixed set of shards, each a network disk, and the
// membership says which member serves each (docs/hosting.md, "Shards on
// network disks"). The controller attaches a shard to the machine of the
// member it assigns it to. This host opens a shard once the membership
// assigns it here, attaching or serving, and the device is on its machine;
// it closes it as soon as the membership it holds does not. Its member's
// identity is drawn when the process starts, so a host started again is a
// new member, and never takes an assignment its predecessor was given.
//
// Before it opens a shard, the host reads the membership again, and opens it
// only if the object still assigns it here under the same generation. It
// opens the device for this process alone, and the cache takes the shard's
// lease for that assignment before it reads anything back: a lease of a newer
// assignment refuses it. Every pass it reads each open shard's lease again,
// and closes a shard whose lease another member took or whose device is gone.

// DefaultShardInterval is how often a host looks again at the shards the
// membership assigns it, besides whenever it reads a new generation: a shard
// assigned before the cloud attached it is opened at the next look after.
const DefaultShardInterval = time.Second

// The probes shards mark on a host.
const (
	// ProbeShardOpened is a shard a host opened and serves.
	ProbeShardOpened = "host/shard-opened"
	// ProbeShardClosed is a shard a host closed because the membership no
	// longer assigns it here.
	ProbeShardClosed = "host/shard-closed"
	// ProbeShardNotAttached is a shard assigned here whose device the cloud
	// had not attached yet.
	ProbeShardNotAttached = "host/shard-not-attached"
	// ProbeShardAssignmentMoved is a shard a host did not open because the
	// membership read again no longer assigned it here as before.
	ProbeShardAssignmentMoved = "host/shard-assignment-moved"
	// ProbeShardRefused is a shard whose lease named a newer assignment.
	ProbeShardRefused = "host/shard-refused"
	// ProbeShardLost is an open shard whose device failed or whose lease
	// another member took, which the host closed.
	ProbeShardLost = "host/shard-lost"
)

// The fault-injection sites of a host's shards.
const (
	// buggifyShardOpenFails fails an open of a shard's device, as a device
	// still settling after an attach does.
	buggifyShardOpenFails = "host/shard-open-fails"
	// buggifyShardCloseSlow holds a released shard open for up to ten
	// seconds before the host closes it, as a host busy with its reads is.
	buggifyShardCloseSlow = "host/shard-close-slow"
)

// ShardsConfig has a host serve shards. Nil Devices serves none.
type ShardsConfig struct {
	// Devices opens the network disks attached to this host's machine.
	Devices platform.Devices
	// Machine is the machine the host runs on, which the controller attaches
	// the host's shards to.
	Machine string
	// Interval is how often the host looks again at what the membership
	// assigns it. Zero is DefaultShardInterval.
	Interval time.Duration
}

// openShard is one shard the host serves: its device, the assignment it was
// opened under, and what the host reports of it.
type openShard struct {
	device platform.File
	disk   membership.Disk
}

// shardServer opens and closes the shards the membership assigns this host.
type shardServer struct {
	config ShardsConfig
	self   rank.Identity
	view   *membership.View
	cache  *checkpoint.Cache
	clock  platform.Clock
	// region is the size of a shard's regions, the first of which is its
	// header's.
	region int64

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// passing admits one pass at a time: the loop's, and one a caller asks
	// for. A pass waits on the store and the device, so the wait for it is
	// one a simulation's clock can pass.
	passing *ctxsync.Mutex

	mu   sync.Mutex
	open map[rank.Identity]*openShard
}

func newShardServer(ctx context.Context, config ShardsConfig, self rank.Identity, view *membership.View,
	cache *checkpoint.Cache, clock platform.Clock, region int64) *shardServer {
	if config.Interval == 0 {
		config.Interval = DefaultShardInterval
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &shardServer{config: config, self: self, view: view, cache: cache, clock: clock, region: region, ctx: ctx,
		cancel: cancel, done: make(chan struct{}), passing: ctxsync.NewMutex(), open: make(map[rank.Identity]*openShard)}
	go s.loop()
	return s
}

func (s *shardServer) loop() {
	defer close(s.done)
	ticker := s.clock.NewTicker(s.config.Interval)
	defer ticker.Stop()
	for {
		changed := s.view.Changed()
		s.pass(s.ctx)
		select {
		case <-s.ctx.Done():
			return
		case <-changed:
		case <-ticker.C():
		}
	}
}

// held is the shards the host holds open, as it reports them to the
// membership.
func (s *shardServer) held() []membership.Disk {
	s.mu.Lock()
	defer s.mu.Unlock()
	disks := make([]membership.Disk, 0, len(s.open))
	for _, identity := range slices.SortedFunc(maps.Keys(s.open), compareIdentity) {
		disks = append(disks, s.open[identity].disk)
	}
	return disks
}

// assigned is the shards m assigns this host: attaching or serving here.
func (s *shardServer) assigned(m membership.Membership) map[rank.Identity]membership.Disk {
	wanted := make(map[rank.Identity]membership.Disk)
	for _, disk := range m.Disks() {
		if disk.Member == s.self && (disk.State == membership.Attaching || disk.State == membership.Serving) {
			wanted[disk.ID] = disk
		}
	}
	return wanted
}

// pass closes every open shard the membership held now no longer assigns
// here under the generation it was opened under, or whose lease or device
// failed, and opens every shard it assigns here that the cloud has attached.
func (s *shardServer) pass(ctx context.Context) {
	if err := s.passing.Lock(ctx); err != nil {
		return
	}
	defer s.passing.Unlock()
	wanted := s.assigned(s.view.Current())
	s.mu.Lock()
	open := maps.Clone(s.open)
	s.mu.Unlock()
	for _, identity := range slices.SortedFunc(maps.Keys(open), compareIdentity) {
		held := open[identity]
		want, still := wanted[identity]
		switch {
		case !still || want.Assigned != held.disk.Assigned:
			if err := sim.BuggifyDelay(ctx, buggifyShardCloseSlow, 0.2, 10*time.Second); err != nil {
				return
			}
			sim.Probe(ctx, ProbeShardClosed)
			s.close(ctx, identity, "the membership no longer assigns it here")
		default:
			if err := s.cache.CheckShard(ctx, identity); err != nil {
				sim.Probe(ctx, ProbeShardLost)
				s.close(ctx, identity, err.Error())
			}
		}
	}
	for _, identity := range slices.SortedFunc(maps.Keys(wanted), compareIdentity) {
		if _, held := open[identity]; held {
			continue
		}
		if err := s.openShard(ctx, wanted[identity]); err != nil && ctx.Err() == nil {
			slog.DebugContext(ctx, "host: a shard assigned here is not open yet", "shard", identity.String(),
				"volume", wanted[identity].Volume, "error", err)
		}
	}
}

// openShard opens a shard the membership assigns here, once the object read
// again still assigns it here under the same generation.
func (s *shardServer) openShard(ctx context.Context, disk membership.Disk) error {
	if !sim.Bug(ctx, "host-open-shard-without-reading-again") {
		read, err := s.view.Refresh(ctx)
		if err != nil {
			return fmt.Errorf("reading the membership before opening a shard: %w", err)
		}
		if again, still := s.assigned(read)[disk.ID]; !still || again.Assigned != disk.Assigned {
			sim.Probe(ctx, ProbeShardAssignmentMoved)
			return fmt.Errorf("the membership no longer assigns shard %s here under generation %d", disk.ID,
				disk.Assigned)
		}
	}
	if sim.Buggify(ctx, buggifyShardOpenFails, 0.1) {
		return fmt.Errorf("%w: opening the device of %s", platform.ErrInjectedFault, disk.Volume)
	}
	device, err := s.config.Devices.Open(ctx, disk.Volume)
	if errors.Is(err, platform.ErrNotFound) {
		sim.Probe(ctx, ProbeShardNotAttached)
		return err
	}
	if err != nil {
		return err
	}
	size, err := device.Size(ctx)
	if err != nil {
		_ = device.Close()
		return err
	}
	began := s.clock.Now()
	err = s.cache.AddShard(ctx, checkpoint.ShardConfig{Device: device, Identity: disk.ID,
		Budget: shardBudget(size - s.region), Lease: checkpoint.Lease{Assigned: disk.Assigned, Member: s.self}})
	if err != nil {
		_ = device.Close()
		if errors.Is(err, checkpoint.ErrFenced) {
			sim.Probe(ctx, ProbeShardRefused)
		}
		return err
	}
	s.mu.Lock()
	s.open[disk.ID] = &openShard{device: device, disk: disk}
	s.mu.Unlock()
	sim.Probe(ctx, ProbeShardOpened)
	slog.InfoContext(ctx, "host: a shard is open", "shard", disk.ID.String(), "volume", disk.Volume,
		"assigned", disk.Assigned, "bytes", size, "read_back", s.clock.Since(began))
	return nil
}

// close stops serving a shard and closes its device.
func (s *shardServer) close(ctx context.Context, identity rank.Identity, why string) {
	s.mu.Lock()
	held := s.open[identity]
	delete(s.open, identity)
	s.mu.Unlock()
	if held == nil {
		return
	}
	if err := s.cache.RemoveShard(context.WithoutCancel(ctx), identity); err != nil {
		slog.WarnContext(ctx, "host: removing a shard from the cache failed", "shard", identity.String(),
			"error", err)
	}
	if err := held.device.Close(); err != nil {
		slog.WarnContext(ctx, "host: closing a shard's device failed", "shard", identity.String(), "error", err)
	}
	slog.InfoContext(ctx, "host: a shard is closed", "shard", identity.String(), "volume", held.disk.Volume,
		"why", why)
}

// stop ends the passes and closes every open shard.
func (s *shardServer) stop() {
	s.cancel()
	<-s.done
	s.mu.Lock()
	identities := slices.Collect(maps.Keys(s.open))
	s.mu.Unlock()
	for _, identity := range identities {
		s.close(context.Background(), identity, "the host is closing")
	}
}

// shardBudget is a shard's share of its own device: all of it but its header
// region, with every write admitted. A network disk wears nothing out, and
// nothing else writes it, so the host's disk limiter does not count it.
type shardBudget int64

func (b shardBudget) Share() int64                         { return int64(b) }
func (shardBudget) Admit(int64, checkpoint.WriteKind) bool { return true }

func compareIdentity(a, b rank.Identity) int {
	return slices.Compare(a[:], b[:])
}
