package simtest_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/internal/knobs"
	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/simtest"
	"github.com/semistrict/sproutfs/internal/volume"
)

// The pre-mortem of the GCE soak, as a simulated deployment: the shape
// scripts/lib/demo-soak.sh drives, over a world with nothing wrong with it.
// Every operation the soak runs has to work, every guest has to hold exactly
// the bytes it wrote, and what the store holds once every VM is deleted has to
// be a deployment `sproutfsctl check` accepts.
//
// Nothing here injects a fault. A campaign proves what survives a loss; this
// proves what the soak's own sequence does to a deployment that is working,
// which is the half a GCE run cannot be given back if it fails.

// The soak's parameters, scaled to what a simulated round costs: the shape is
// the same and the population is what one seed can walk.
const (
	soakRounds  = 3
	soakMaxVMs  = 5
	soakVMPages = 4
	// soakPopulation is the most VMs this run ever has open at once, which is
	// what the pager arena and the dirty budget have to hold. Three forks are
	// taken in a round before anything is deleted, so the ceiling is the
	// trimmed population plus one round's fan-out.
	soakPopulation = soakMaxVMs + 3
)

// premortemVolumes is what every VM of this pre-mortem has: memory and a disk,
// so every fork, every migration and every stop carries more than one region.
func premortemVolumes() []volume.VolumeSpec {
	return []volume.VolumeSpec{
		{Name: "ram0", Size: soakVMPages * simtest.PageSize},
		{Name: "disk", Size: soakVMPages * simtest.PageSize},
	}
}

// premortemTopology is the population a round begins with: two hosts and the
// two VMs the soak starts from.
func premortemTopology() simtest.Topology {
	return simtest.Topology{Hosts: []string{"host-0", "host-1"},
		VMs: []simtest.VMSpec{
			{ID: "vm-0", Host: 0, Volumes: premortemVolumes()},
			{ID: "vm-1", Host: 1, Volumes: premortemVolumes()},
		}}
}

// premortemKnobs sizes the pagers for the population this run reaches rather
// than for the topology it starts from: every fork is a VM of its own, and a
// fork or a migration holds the parent's frames and the child's on one host at
// once. A budget below that stalls a store on a checkpoint nobody is going to
// take, which is the harness running out of room rather than anything deciding.
func premortemKnobs(t *testing.T) knobs.Knobs {
	t.Helper()
	k := knobs.Defaults()
	pages := soakPopulation * 2 * soakVMPages
	k.ResidentPages = 2*pages + 8
	k.DirtyPages = k.ResidentPages
	k.LogicalPages = 4 * k.ResidentPages
	k.ReadAheadPages, k.WriteAheadPages = 1, 1
	k.MaxOpenVMs = max(k.MaxOpenVMs, 2*soakPopulation)
	if err := k.Validate(); err != nil {
		t.Fatal(err)
	}
	return k
}

// soak is one run of the pre-mortem: the world, the draws its order comes from,
// and the identities it has handed out.
type soak struct {
	t      *testing.T
	world  *simtest.World
	random sim.Random
	draws  int
	// children numbers the forks this run has taken, which is what names them
	// apart.
	children int
}

// choose is one seeded draw below limit. Every order this run walks comes from
// here rather than from a map, so a failure reproduces from the seed.
func (s *soak) choose(limit int) int {
	s.draws++
	if limit <= 0 {
		return 0
	}
	return s.random.Intn(fmt.Sprintf("soak/%d", s.draws), limit)
}

// pick is one identity of a seeded list, or the empty string for an empty one.
func (s *soak) pick(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[s.choose(len(ids))]
}

// shuffle puts ids in a seeded order, which is the order the soak walks the VMs
// in.
func (s *soak) shuffle(ids []string) []string {
	order := append([]string(nil), ids...)
	for i := len(order) - 1; i > 0; i-- {
		j := s.choose(i + 1)
		order[i], order[j] = order[j], order[i]
	}
	return order
}

// mutate is the soak's witness mutation: the guest writes every page of every
// volume it has, which is the dirty set a checkpoint then seals.
func (s *soak) mutate(id string, value byte) {
	s.t.Helper()
	if err := s.world.StoreAll(id, value); err != nil {
		s.t.Fatalf("%s: storing a generation: %v", id, err)
	}
}

// check is the witness check: every page of every running VM reads back the
// bytes that guest wrote. Nothing is wrong with this world, so a read that
// fails is a defect and not a fault.
func (s *soak) check(ctx context.Context, what string) {
	s.t.Helper()
	if err := s.world.Verify(ctx, simtest.ReadsMustSucceed); err != nil {
		s.t.Fatalf("%s: a guest does not hold what it wrote: %v", what, err)
	}
}

// fork takes one child of a parent onto a host and requires it to exist: a
// deployment with nothing wrong with it refuses no fork, so a fork that did not
// happen is the defect rather than a placement to record.
func (s *soak) fork(ctx context.Context, parent string, host int, what string) string {
	s.t.Helper()
	s.children++
	id := fmt.Sprintf("fork-%d", s.children)
	spec := simtest.VMSpec{ID: id, Parent: parent, Host: host, Volumes: premortemVolumes()}
	if err := s.world.Fork(ctx, spec); err != nil {
		s.t.Fatalf("%s: forking %s from %s: %v", what, id, parent, err)
	}
	if !contains(s.world.Started(), id) {
		s.t.Fatalf("the %s fork %s of %s is running nowhere", what, id, parent)
	}
	if at := s.world.HostOf(id); at != host {
		s.t.Fatalf("the %s fork %s of %s is on host-%d, want host-%d", what, id, parent, at, host)
	}
	return id
}

// contains reports whether ids holds one identity.
func contains(ids []string, id string) bool {
	for _, held := range ids {
		if held == id {
			return true
		}
	}
	return false
}

// TestPremortemSoakRoundsHoldEveryGuest walks the soak's rounds over a
// simulated deployment: every VM mutates and is checked, one parent fans out on
// its own host and on the other, a child of that child is taken, the round's
// share migrates and stops and starts, and the population is trimmed. Every
// check is the soak's own — the guest holds exactly the bytes it wrote — and
// the last word is the deployment check the soak ends with.
func TestPremortemSoakRoundsHoldEveryGuest(t *testing.T) {
	for _, seed := range premortemSeeds() {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) { runPremortemSoak(t, seed) })
		})
	}
}

// premortemSeeds is the block of seeds this pre-mortem walks: a few in the
// ordinary suite, and whatever a sweep asks for.
func premortemSeeds() []uint64 {
	base, count := uint64(1), uint64(3)
	if text := os.Getenv("SPROUTFS_PREMORTEM_SEED_BASE"); text != "" {
		if parsed, err := strconv.ParseUint(text, 10, 64); err == nil {
			base = parsed
		}
	}
	if text := os.Getenv("SPROUTFS_PREMORTEM_SEED_COUNT"); text != "" {
		if parsed, err := strconv.ParseUint(text, 10, 64); err == nil {
			count = parsed
		}
	}
	seeds := make([]uint64, 0, count)
	for seed := base; seed < base+count; seed++ {
		seeds = append(seeds, seed)
	}
	return seeds
}

func runPremortemSoak(t *testing.T, seed uint64) {
	runtime := newCampaignRuntime(seed, false)
	ctx := sim.WithRuntime(t.Context(), runtime)
	topology := premortemTopology()
	prefix := newPrefix(t, "premortem/")
	world := simtest.MustStart(t, ctx, simtest.Config{Runtime: runtime, Topology: topology,
		Knobs: premortemKnobs(t), Prefix: prefix, Log: t.Logf})
	s := &soak{t: t, world: world, random: runtime.Random("premortem/soak")}
	generation := byte(0)

	for round := 1; round <= soakRounds; round++ {
		t.Logf("round %d of %d: running %v", round, soakRounds, world.Started())

		// 1. every running VM does some work and is checked.
		for _, id := range s.shuffle(world.Started()) {
			generation++
			s.mutate(id, generation)
		}
		s.check(ctx, fmt.Sprintf("round %d mutated", round))

		// 2. one parent fans out, on its own host and on the other one, and the
		// child it took here is forked again, so a root that names its
		// grandparent's checkpoints is read on both hosts.
		parent := s.pick(world.Started())
		here := world.HostOf(parent)
		if parent == "" || here < 0 {
			t.Fatalf("round %d: no VM is running", round)
		}
		local := s.fork(ctx, parent, here, "local")
		s.check(ctx, fmt.Sprintf("round %d forked %s locally", round, local))
		remote := s.fork(ctx, parent, 1-here, "remote")
		s.check(ctx, fmt.Sprintf("round %d forked %s remotely", round, remote))
		grandchild := s.fork(ctx, local, world.HostOf(local), "grandchild")
		s.check(ctx, fmt.Sprintf("round %d forked the grandchild %s", round, grandchild))
		for _, child := range []string{local, remote, grandchild} {
			generation++
			s.mutate(child, generation)
			if err := world.Checkpoint(ctx, child); err != nil {
				t.Fatalf("round %d: capturing the diverged %s: %v", round, child, err)
			}
		}
		s.check(ctx, fmt.Sprintf("round %d diverged", round))

		// 3. the round's share migrates, and is checked at both ends.
		if moving := s.pick(world.Started()); moving != "" {
			from := world.HostOf(moving)
			if err := world.Migrate(ctx, moving, 1-from); err != nil {
				t.Fatalf("round %d: migrating %s: %v", round, moving, err)
			}
			if at := world.HostOf(moving); at != 1-from {
				t.Fatalf("round %d: %s is on host-%d after migrating to host-%d",
					round, moving, at, 1-from)
			}
			s.check(ctx, fmt.Sprintf("round %d migrated %s", round, moving))
		}

		// 4. the round's share stops and starts, on the other host.
		if started := world.Started(); len(started) > 1 {
			stopping := s.pick(started)
			was := world.HostOf(stopping)
			if err := world.Stop(ctx, stopping); err != nil {
				t.Fatalf("round %d: stopping %s: %v", round, stopping, err)
			}
			for host := range topology.Hosts {
				if contains(world.Host(host).Machines(), stopping) {
					t.Fatalf("round %d: host-%d still runs %s after it was stopped",
						round, host, stopping)
				}
			}
			if err := world.Start(ctx, stopping, 1-was); err != nil {
				t.Fatalf("round %d: starting %s: %v", round, stopping, err)
			}
			if !contains(world.Started(), stopping) {
				t.Fatalf("round %d: %s is running nowhere after it was started", round, stopping)
			}
			if at := world.HostOf(stopping); at != 1-was {
				t.Fatalf("round %d: %s started on host-%d, want host-%d",
					round, stopping, at, 1-was)
			}
			s.check(ctx, fmt.Sprintf("round %d started %s", round, stopping))
		}

		// 5. the population is trimmed back to what the hosts admit.
		for _, id := range s.shuffle(world.Running()) {
			if len(world.Running()) <= soakMaxVMs {
				break
			}
			if err := world.Delete(ctx, id); err != nil {
				t.Fatalf("round %d: deleting %s: %v", round, id, err)
			}
		}
		s.check(ctx, fmt.Sprintf("round %d trimmed", round))
		if err := world.CheckSelected(ctx); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}

	// The end: every VM is deleted and what is left has to be a deployment the
	// orchestrator's own check accepts.
	for _, id := range s.shuffle(world.Running()) {
		if err := world.Delete(ctx, id); err != nil {
			t.Fatalf("the last delete of %s: %v", id, err)
		}
	}
	if left := world.Running(); len(left) != 0 {
		t.Fatalf("%v are still listed after deleting them all", left)
	}
	if err := world.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// The allowances are the ones cmd/sproutfs-orchestrator gives its /check:
	// everything a live deployment always holds. Nothing else is excusable in a
	// run with no host lost and no store refused.
	if err := volume.CheckDeployment(context.WithoutCancel(ctx), runtime.ObjectStore(), prefix,
		volume.AllowUnpublishedIndex, volume.AllowUnrecordedVM,
		volume.AllowUnreferencedCheckpoint, volume.AllowSupersededEpoch); err != nil {
		t.Fatalf("the deployment disagrees with itself: %v", err)
	}
	if t.Failed() {
		reportTrace(t, runtime)
	}
}
