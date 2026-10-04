package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform/sim"
)

// shardFixture is a membership fixture whose hosts serve shards: each host
// pod reports itself with its machine, holds open the shards the membership
// assigns it once the cloud has them on its machine, and closes them as soon
// as it does not, and the orchestrator carries the membership out through a
// simulated cloud.
type shardFixture struct {
	*membershipFixture
	cloud   *sim.NetworkDisks
	volumes []string
}

func newShardFixture(t *testing.T, names []string, shards int) *shardFixture {
	t.Helper()
	running := map[string][]string{}
	for _, name := range names {
		running[name] = []string{}
	}
	f := &shardFixture{membershipFixture: newMembershipFixture(t, running)}
	f.cloud = f.runtime.NewNetworkDisks(sim.NetworkDisksConfig{AttachLatency: 2 * time.Second,
		DetachLatency: time.Second})
	for n := range shards {
		volume := fmt.Sprintf("projects/p/zones/z/disks/shard-%d", n)
		if err := f.cloud.Create(f.ctx, volume, 64<<30); err != nil {
			t.Fatal(err)
		}
		f.volumes = append(f.volumes, volume)
	}
	f.orchestrator.shards = &membership.ShardControl{Store: f.orchestrator.members, Disks: f.cloud}
	f.orchestrator.shardVolumes = func(context.Context) ([]string, error) { return slices.Clone(f.volumes), nil }
	for _, name := range names {
		f.hosts[name].member = &host.Member{Identity: identityOf(name).String(), Address: f.hosts[name].page,
			Disks: []host.MemberDisk{}, Machine: "node-" + name}
	}
	return f
}

// hold has each host hold open what the membership assigns it on its
// machine, and nothing else, as a host's shard server does.
func (f *shardFixture) hold(t *testing.T) {
	t.Helper()
	m, err := f.orchestrator.members.Read(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range f.hosts {
		reported := client.member
		reported.Disks = []host.MemberDisk{}
		for _, disk := range m.Disks() {
			if disk.Member.String() != reported.Identity || disk.State != membership.Attaching &&
				disk.State != membership.Serving || f.cloud.Attached(disk.Volume) != reported.Machine {
				continue
			}
			reported.Disks = append(reported.Disks, host.MemberDisk{Identity: disk.ID.String(), Volume: disk.Volume,
				Weight: disk.Weight})
		}
	}
}

// settleShards steps the membership, with the hosts holding what it assigns
// them, until neither changes; it reports the membership it ends at.
func (f *shardFixture) settleShards(t *testing.T) membership.Membership {
	t.Helper()
	still := 0
	for steps := 0; steps < 400; steps++ {
		f.orchestrator.moving(f.ctx)()
		f.hold(t)
		m, changed, err := f.orchestrator.StepMembership(f.ctx)
		if err != nil {
			t.Logf("a step: %v", err)
		}
		if changed {
			still = 0
			continue
		}
		if still++; still > 3 {
			return m
		}
	}
	t.Fatal("the shards never settled")
	return membership.Membership{}
}

// heldBy is the volumes each host pod reports holding.
func (f *shardFixture) heldBy(name string) []string {
	var volumes []string
	for _, disk := range f.hosts[name].member.Disks {
		volumes = append(volumes, disk.Volume)
	}
	return volumes
}

// The orchestrator carries the shards out: it lists them, assigns them over
// the hosts, and has the cloud attach each to its host's machine. A host pod
// that starts terminating, as the autoscaler's removal of its node begins,
// drains, and its shards move to the other hosts while it still answers:
// each is closed there, detached, and attached and served elsewhere; then it
// leaves the membership, and is not taken back while it terminates.
func TestTheOrchestratorMovesShardsOffATerminatingHost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newShardFixture(t, []string{"host-0", "host-1", "host-2"}, 6)
		m := f.settleShards(t)
		for _, name := range []string{"host-0", "host-1", "host-2"} {
			if held := f.heldBy(name); len(held) != 2 {
				t.Fatalf("%s holds %v, want two shards", name, held)
			}
		}
		for _, volume := range f.volumes {
			disk, _ := m.Disk(membership.ShardIdentity(volume))
			if disk.State != membership.Serving {
				t.Fatalf("shard %s is %s", volume, disk.State)
			}
		}
		moving := f.heldBy("host-1")
		f.pods.mu.Lock()
		for at := range f.pods.pods {
			if f.pods.pods[at].Name == "host-1" {
				f.pods.pods[at].Terminating, f.pods.pods[at].Ready = true, false
			}
		}
		f.pods.mu.Unlock()
		m = f.settleShards(t)
		if held := f.heldBy("host-1"); len(held) != 0 {
			t.Fatalf("the terminating host still holds %v", held)
		}
		if _, listed := m.Member(identityOf("host-1")); listed {
			t.Fatal("the terminating host is still a member once it serves nothing")
		}
		for _, volume := range moving {
			disk, _ := m.Disk(membership.ShardIdentity(volume))
			if disk.State != membership.Serving || disk.Member == identityOf("host-1") ||
				f.cloud.Attached(volume) == "node-host-1" {
				t.Fatalf("shard %s moved off host-1 is %s on %s, attached to %s", volume, disk.State, disk.Member,
					f.cloud.Attached(volume))
			}
		}
		if got := len(f.heldBy("host-0")) + len(f.heldBy("host-2")); got != 6 {
			t.Fatalf("the two hosts left hold %d shards, want six", got)
		}
	})
}
