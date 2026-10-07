package main

import (
	"context"
	"crypto/sha256"
	"slices"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// identityOf is the identity of the disk a host of this name keeps.
func identityOf(name string) rank.Identity {
	sum := sha256.Sum256([]byte("disk of " + name))
	return rank.Identity(sum[:16])
}

// memberNamed is what a host of this name reports to the membership: its
// disk's identity, its page address, and its disk of the weight given.
func (d *deployment) memberNamed(name string, weight uint32) *host.Member {
	id := identityOf(name).String()
	return &host.Member{Identity: id, Address: d.hosts[name].page,
		Disks: []host.MemberDisk{{Identity: id, Volume: "cache-0", Weight: weight}}}
}

// withDisks gives each named host a disk of the weight given.
func (d *deployment) withDisks(weights map[string]uint32) {
	for name, weight := range weights {
		d.hosts[name].member = d.memberNamed(name, weight)
	}
}

// simulated is a context the in-tree bug guards answer in.
func simulated(t *testing.T) context.Context {
	return sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
}

// membershipFixture is a deployment whose orchestrator keeps the membership
// in a simulated bucket, and every generation the bucket applied.
type membershipFixture struct {
	*deployment
	ctx     context.Context
	runtime *sim.Runtime
	mu      sync.Mutex
	applied []membership.Membership
}

// newMembershipFixture is a deployment of hosts, in a synctest bubble, whose
// orchestrator keeps the membership in a simulated bucket.
func newMembershipFixture(t *testing.T, running map[string][]string) *membershipFixture {
	t.Helper()
	runtime := sim.New(sim.Config{})
	f := &membershipFixture{deployment: newDeployment(t, running), ctx: sim.WithRuntime(t.Context(), runtime),
		runtime: runtime}
	f.orchestrator.members = f.store(t, "orchestrator")
	runtime.ObjectStore().Observe(func(change sim.ObjectChange) {
		m, err := membership.Unmarshal(change.Value)
		if err != nil {
			t.Errorf("the bucket applied a membership that does not parse: %v", err)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.applied = append(f.applied, m)
	})
	return f
}

// store is a store of the fixture's membership, its nonces drawn from name.
func (f *membershipFixture) store(t *testing.T, name string) *membership.Store {
	t.Helper()
	store, err := membership.NewStore(membership.Config{ObjectStore: f.runtime.ObjectStore(),
		Entropy: f.runtime.NewEntropy(name)})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// settle has the orchestrator step the membership until it takes no step,
// and returns the membership and the steps it took.
func (f *membershipFixture) settle(t *testing.T) (membership.Membership, int) {
	t.Helper()
	for steps := 0; ; steps++ {
		// Each step surveys afresh, as a pass a second or more after the
		// last does.
		f.orchestrator.moving(f.ctx)()
		m, changed, err := f.orchestrator.StepMembership(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			return m, steps
		}
	}
}

// generations is every generation the bucket applied since at.
func (f *membershipFixture) generations(at int) []membership.Membership {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.applied[at:])
}

// disksOf is the identities of the disks of m.
func disksOf(m membership.Membership) []rank.Identity {
	var ids []rank.Identity
	for _, disk := range m.Disks() {
		ids = append(ids, disk.ID)
	}
	return ids
}

// The orchestrator moves the membership to every host that reports a disk,
// one step a pass: the deployment's code first, then each host joins with its
// disk attaching in one generation, and its disk serves in another. A host
// that keeps no disk is not in it. With no code configured, three disks get
// the default 4+2, round the three.
func TestTheMembershipNamesEveryHostThatReportsADisk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMembershipFixture(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}, "host-3": {}})
		f.withDisks(map[string]uint32{"host-0": 2, "host-1": 1, "host-2": 4})
		m, steps := f.settle(t)
		if steps != 7 || m.Generation() != 7 || m.Code() != (rank.Code{K: 4, M: 2}) {
			t.Fatalf("the membership took %d steps to generation %d under %s, want 7 to 4+2", steps,
				m.Generation(), m.Code())
		}
		for _, name := range []string{"host-0", "host-1", "host-2"} {
			id := identityOf(name)
			member, listed := m.Member(id)
			if !listed || member.State != membership.Active || string(member.Address) != f.hosts[name].page ||
				!m.Serves(id, id) {
				t.Fatalf("%s is %+v in the membership (%v), want it active and serving its disk", name, member, listed)
			}
		}
		if len(m.Members()) != 3 {
			t.Fatalf("the membership lists %d members, want the three hosts with a disk", len(m.Members()))
		}
		hosts, err := f.orchestrator.Hosts(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, report := range hosts {
			reported := f.hosts[report.Name].member
			if (report.Member == nil) != (reported == nil) || reported != nil && report.Member.Identity != reported.Identity {
				t.Fatalf("GET /hosts reports %s as %+v, want %+v", report.Name, report.Member, reported)
			}
		}
	})
}

// A host whose pod is gone drains before it leaves: its member is draining
// and its disk releasing, then the disk is let go and removed, which is the
// one step that moves windows, and then the member leaves. Each is one
// generation, and every generation lists the disk set of the one before it,
// or that less one disk.
func TestAHostDrainsBeforeItLeaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMembershipFixture(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}})
		f.withDisks(map[string]uint32{"host-0": 1, "host-1": 1, "host-2": 1})
		before, _ := f.settle(t)
		at := len(f.generations(0))
		if _, err := f.orchestrator.Kill(f.ctx, "host-2"); err != nil {
			t.Fatal(err)
		}
		after, steps := f.settle(t)
		gone := identityOf("host-2")
		if steps != 4 || after.Generation() != before.Generation()+4 {
			t.Fatalf("host-2 left in %d steps, to generation %d from %d; want 4", steps, after.Generation(),
				before.Generation())
		}
		states := []string{}
		for _, m := range f.generations(at) {
			member, listed := m.Member(gone)
			disk, held := m.Disk(gone)
			states = append(states, member.State.String()+"/"+disk.State.String())
			if listed && held && disk.State == membership.Serving {
				t.Fatalf("generation %d still serves the disk of a host that is gone", m.Generation())
			}
		}
		if want := []string{"draining/releasing", "draining/released", "draining/disk-state-0",
			"member-state-0/disk-state-0"}; !slices.Equal(states, want) {
			t.Fatalf("host-2 left through %v, want %v", states, want)
		}
		if got := disksOf(after); slices.Contains(got, gone) || len(got) != 2 || after.Code() != before.Code() {
			t.Fatalf("after the leave the membership holds %v under %s", got, after.Code())
		}
	})
}

// A host pod replaced on its node, as a rolling restart of the hosts replaces
// every one, comes back over the same disk under the same identity. If the
// orchestrator saw the old pod terminating, its member drained. The new pod
// serves the disk again once its copy of the membership shows the release:
// the disk is let go, the member leaves and joins again with it, and the disk
// serves. Until its copy shows the release, nothing moves.
//
// On GCE on 2026-10-04 the restart of six hosts for a change of setting left
// one host's disk releasing for good, and the bench's wait for every disk to
// serve gave up after fifteen minutes.
func TestAPodReplacedOverItsDiskServesItAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMembershipFixture(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}})
		f.withDisks(map[string]uint32{"host-0": 1, "host-1": 1, "host-2": 1})
		f.settle(t)
		setPod := func(terminating bool) {
			f.pods.mu.Lock()
			defer f.pods.mu.Unlock()
			for at := range f.pods.pods {
				if f.pods.pods[at].Name == "host-2" {
					f.pods.pods[at].Terminating, f.pods.pods[at].Ready = terminating, !terminating
				}
			}
		}
		id := identityOf("host-2")
		setPod(true)
		drained, steps := f.settle(t)
		if member, _ := drained.Member(id); steps != 1 || member.State != membership.Draining {
			t.Fatalf("the terminating pod's member is %s after %d steps, want draining after one", member.State,
				steps)
		}
		setPod(false)
		if m, steps := f.settle(t); steps != 0 {
			t.Fatalf("the new pod's copy has not read the release, and the membership took %d steps to "+
				"generation %d", steps, m.Generation())
		}
		f.hosts["host-2"].member.Generation = drained.Generation()
		f.hosts["host-2"].member.Disks[0].State = membership.Releasing.String()
		back, steps := f.settle(t)
		if member, _ := back.Member(id); steps != 4 || member.State != membership.Active || !back.Serves(id, id) {
			t.Fatalf("the new pod's member is %s after %d steps, serving its disk %v; want active and serving "+
				"after four", member.State, steps, back.Serves(id, id))
		}
		if len(back.Disks()) != 3 {
			t.Fatalf("the membership holds %d disks once the pod is back, want 3", len(back.Disks()))
		}
	})
}

// A join, a leave and a change of weight are each one generation: no
// generation moves windows of more than one disk.
func TestEachStepMovesTheWindowsOfOneDiskAtMost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMembershipFixture(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}, "host-3": {}})
		f.withDisks(map[string]uint32{"host-0": 1, "host-1": 1, "host-2": 1})
		f.settle(t)
		f.withDisks(map[string]uint32{"host-0": 3, "host-1": 2, "host-3": 1})
		f.hosts["host-2"].member = nil
		f.settle(t)
		previous := membership.Empty()
		for _, m := range f.generations(0) {
			moved := 0
			for _, disk := range m.Disks() {
				if was, ok := previous.Disk(disk.ID); !ok || was.Weight != disk.Weight {
					moved++
				}
			}
			for _, disk := range previous.Disks() {
				if _, ok := m.Disk(disk.ID); !ok {
					moved++
				}
			}
			if moved > 1 {
				t.Fatalf("generation %d moved the windows of %d disks", m.Generation(), moved)
			}
			previous = m
		}
	})
}

// A code the deployment configures is the membership's, whatever the size of
// the cluster, and so are the codes it replaced, newest first.
func TestAConfiguredCodeIsTheMemberships(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMembershipFixture(t, map[string][]string{"host-0": {}, "host-1": {}})
		f.withDisks(map[string]uint32{"host-0": 1, "host-1": 1})
		f.orchestrator.code = rank.Code{K: 1, M: 1}
		f.orchestrator.earlier = []rank.Code{{K: 2, M: 1}, {K: 4, M: 2}}
		m, _ := f.settle(t)
		want := []rank.Code{{K: 1, M: 1}, {K: 2, M: 1}, {K: 4, M: 2}}
		if !slices.Equal(m.List().Codes(), want) || len(m.Disks()) != 2 {
			t.Fatalf("the membership holds %d disks under %v, want two under 1+1, then 2+1 and 4+2",
				len(m.Disks()), m.List().Codes())
		}
	})
}

// The code never follows the hosts. A six-host cluster that sets no code runs
// 4+2, and drained down to two hosts it still does, because a code that
// followed the hosts would leave every stripe in the cluster to the store. Its
// stripes go round the disks left.
func TestTheCodeNeverFollowsTheHosts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		names := []string{"host-0", "host-1", "host-2", "host-3", "host-4", "host-5"}
		running := map[string][]string{}
		weights := map[string]uint32{}
		for _, name := range names {
			running[name] = []string{}
			weights[name] = 1
		}
		f := newMembershipFixture(t, running)
		f.withDisks(weights)
		if m, _ := f.settle(t); m.Code() != (rank.Code{K: 4, M: 2}) || len(m.Disks()) != 6 {
			t.Fatalf("six hosts are listed as %d disks under %s, want six under 4+2", len(m.Disks()), m.Code())
		}
		for left := 5; left >= 2; left-- {
			if _, err := f.orchestrator.Kill(f.ctx, names[left]); err != nil {
				t.Fatal(err)
			}
			m, _ := f.settle(t)
			if _, listed := m.Disk(identityOf(names[left])); listed || m.Code() != (rank.Code{K: 4, M: 2}) ||
				len(m.Disks()) != left {
				t.Fatalf("after %s left the membership holds %d disks under %s, want the other %d under 4+2",
					names[left], len(m.Disks()), m.Code(), left)
			}
		}
	})
}

// A host the Kubernetes API still lists that did not answer this survey
// keeps its place: it may be serving its windows perfectly well, and a
// membership that drained it would move them all. Readers mark it down on
// their own. The report of the quiet host shows the member it keeps.
func TestAQuietHostKeepsItsPlace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMembershipFixture(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}})
		f.withDisks(map[string]uint32{"host-0": 1, "host-1": 1, "host-2": 1})
		before, _ := f.settle(t)
		f.hosts["host-1"].down = true
		after, steps := f.settle(t)
		if steps != 0 || !after.Equal(before) {
			t.Fatalf("with host-1 quiet the membership took %d steps to generation %d", steps, after.Generation())
		}
		hosts, err := f.orchestrator.Hosts(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, report := range hosts {
			if report.Name == "host-1" && (report.Error == "" || report.Member == nil ||
				report.Member.Identity != identityOf("host-1").String()) {
				t.Fatalf("the quiet host is reported with error %q and member %+v, want its member kept",
					report.Error, report.Member)
			}
		}
	})
}

// A host that answers with another disk, as one restarted over a new file
// does, is another member: the one it was drains and leaves, and the new one
// joins. A host that answers with none drains and leaves.
func TestTheMembershipFollowsWhatEachHostReports(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMembershipFixture(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}})
		f.withDisks(map[string]uint32{"host-0": 1, "host-1": 1, "host-2": 1})
		f.settle(t)
		renewed := identityOf("a new disk").String()
		f.hosts["host-0"].member = &host.Member{Identity: renewed, Address: f.hosts["host-0"].page,
			Disks: []host.MemberDisk{{Identity: renewed, Volume: "cache-1", Weight: 3}}}
		f.hosts["host-1"].member = nil
		m, _ := f.settle(t)
		want := []rank.Identity{identityOf("host-2"), identityOf("a new disk")}
		slices.SortFunc(want, func(a, b rank.Identity) int { return compareIdentities(a, b) })
		if got := disksOf(m); !slices.Equal(got, want) || m.Code() != (rank.Code{K: 4, M: 2}) {
			t.Fatalf("the membership holds %v under %s, want host-2's disk and host-0's new one under 4+2", got,
				m.Code())
		}
	})
}

// compareIdentities orders identities as the membership lists them.
func compareIdentities(a, b rank.Identity) int {
	for at := range a {
		if a[at] != b[at] {
			return int(a[at]) - int(b[at])
		}
	}
	return 0
}

// Two pods that report one member are a copied disk. The membership lists it
// once, as the first pod by name reports it, so every host routes to the same
// one. A member no membership can hold, with no identity or a disk of no
// weight, is left out.
func TestTheMembershipHoldsEachMemberOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMembershipFixture(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}, "host-3": {}})
		copied := *f.memberNamed("host-0", 1)
		f.hosts["host-0"].member = &copied
		twin := copied
		twin.Address = f.hosts["host-1"].page
		f.hosts["host-1"].member = &twin
		f.hosts["host-2"].member = &host.Member{Identity: "not hex", Address: f.hosts["host-2"].page}
		f.hosts["host-3"].member = f.memberNamed("host-3", 0)
		m, _ := f.settle(t)
		member, listed := m.Member(identityOf("host-0"))
		if len(m.Members()) != 1 || !listed || string(member.Address) != f.hosts["host-0"].page {
			t.Fatalf("the membership lists %+v, want host-0's member once", m.Members())
		}
	})
}

// Two orchestrators step one membership at once, each from a survey of its
// own: every step is a compare-and-set, so each takes a step from what the
// other left, none is lost, and the bucket applies one line of generations
// that ends where one orchestrator alone would.
func TestTwoOrchestratorsMoveOneMembership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMembershipFixture(t, map[string][]string{"host-0": {}, "host-1": {}, "host-2": {}, "host-3": {}})
		f.withDisks(map[string]uint32{"host-0": 1, "host-1": 2, "host-2": 1, "host-3": 1})
		other := newDeployment(t, nil)
		other.orchestrator.pods, other.orchestrator.dial = f.orchestrator.pods, f.orchestrator.dial
		other.orchestrator.members = f.store(t, "the other orchestrator")
		var wg sync.WaitGroup
		for _, o := range []*orchestrator{f.orchestrator, other.orchestrator} {
			wg.Go(func() {
				for {
					o.moving(f.ctx)()
					_, changed, err := o.StepMembership(f.ctx)
					if err != nil {
						t.Error(err)
						return
					}
					if !changed {
						return
					}
				}
			})
		}
		wg.Wait()
		m, steps := f.settle(t)
		if steps != 0 || m.Generation() != 9 || len(m.Disks()) != 4 || m.Code() != (rank.Code{K: 4, M: 2}) {
			t.Fatalf("two orchestrators left generation %d of %d disks under %s, and one more took %d steps",
				m.Generation(), len(m.Disks()), m.Code(), steps)
		}
		previous := membership.Empty()
		for _, applied := range f.generations(0) {
			if err := membership.Step(f.ctx, previous, applied); err != nil {
				t.Fatalf("the bucket applied generation %d after %d against the rules: %v", applied.Generation(),
					previous.Generation(), err)
			}
			previous = applied
		}
	})
}

// The deployment names its code as k+m, and the codes it replaced as a list
// of them, newest first. A code no host can store under is refused at start,
// and so is an earlier code that is not one, or that is the code itself. With
// no code set, the code is the default, 4+2.
func TestTheCodeIsConfigured(t *testing.T) {
	environment := map[string]string{"SPROUTFS_BUCKET": "bucket", "SPROUTFS_CACHE_CODE": "6+2",
		"SPROUTFS_CACHE_EARLIER_CODES": "4+2, 2+1"}
	lookup := func(name string) string { return environment[name] }
	config, err := loadConfig(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if config.CacheCode != (rank.Code{K: 6, M: 2}) ||
		!slices.Equal(config.CacheEarlierCodes, []rank.Code{{K: 4, M: 2}, {K: 2, M: 1}}) {
		t.Fatalf("the code is %s after %v, want 6+2 after 4+2 and 2+1", config.CacheCode, config.CacheEarlierCodes)
	}
	for name, value := range map[string]string{
		"SPROUTFS_CACHE_CODE":          "0+2",
		"SPROUTFS_CACHE_EARLIER_CODES": "6+2",
	} {
		before := environment[name]
		environment[name] = value
		if _, err := loadConfig(lookup); err == nil {
			t.Fatalf("%s=%s was accepted", name, value)
		}
		environment[name] = before
	}
	environment["SPROUTFS_CACHE_EARLIER_CODES"] = "4+2,two"
	if _, err := loadConfig(lookup); err == nil {
		t.Fatal("an earlier code that is not one was accepted")
	}
	delete(environment, "SPROUTFS_CACHE_CODE")
	delete(environment, "SPROUTFS_CACHE_EARLIER_CODES")
	if config, err := loadConfig(lookup); err != nil || config.CacheCode != rank.DefaultCode ||
		len(config.CacheEarlierCodes) != 0 {
		t.Fatalf("with no code configured the configuration is %s after %v, %v", config.CacheCode,
			config.CacheEarlierCodes, err)
	}
}

// Durable flush is off unless SPROUTFS_DURABLE_FLUSH names gce, and a journal
// disk is 32 GiB unless SPROUTFS_JOURNAL_BYTES says otherwise.
func TestTheConfigurationReadsDurableFlush(t *testing.T) {
	environment := map[string]string{"SPROUTFS_BUCKET": "bucket"}
	lookup := func(name string) string { return environment[name] }
	config, err := loadConfig(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if config.DurableFlush != "" || config.JournalBytes != 32<<30 {
		t.Fatalf("with nothing set durable flush is %q with %d-byte journals, want off and 32 GiB",
			config.DurableFlush, config.JournalBytes)
	}
	environment["SPROUTFS_DURABLE_FLUSH"], environment["SPROUTFS_JOURNAL_BYTES"] = "gce", "17179869184"
	if config, err = loadConfig(lookup); err != nil {
		t.Fatal(err)
	}
	if config.DurableFlush != "gce" || config.JournalBytes != 16<<30 {
		t.Fatalf("durable flush is %q with %d-byte journals, want gce and 16 GiB", config.DurableFlush,
			config.JournalBytes)
	}
	for name, value := range map[string]string{"SPROUTFS_DURABLE_FLUSH": "on", "SPROUTFS_JOURNAL_BYTES": "4096"} {
		before := environment[name]
		environment[name] = value
		if _, err := loadConfig(lookup); err == nil {
			t.Fatalf("%s=%s was accepted", name, value)
		}
		environment[name] = before
	}
}
