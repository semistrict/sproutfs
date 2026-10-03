package rank

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform/sim"
)

// orchestrator serves the list of caches, as GET /caches does, and can be
// down.
type orchestrator struct {
	runtime *sim.Runtime
	mu      sync.Mutex
	list    List
	down    bool
	// served is every list it answered with, by its text, and lagging each of
	// those less its last cache.
	served, lagging map[string]bool
	reads           int
}

var errDown = errors.New("connection refused")

func newOrchestrator(runtime *sim.Runtime, list List) *orchestrator {
	return &orchestrator{runtime: runtime, list: list, served: map[string]bool{}, lagging: map[string]bool{}}
}

// read answers one host. Its answer is admitted through the runtime, so a
// scheduler decides the order the hosts' reads and the orchestrator's
// changes land in.
func (o *orchestrator) read(ctx context.Context) (List, error) {
	if err := o.runtime.Admit(ctx, "orchestrator"); err != nil {
		return List{}, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.reads++
	if o.down {
		return List{}, errDown
	}
	o.served[textOf(o.list)] = true
	if caches := o.list.Caches(); len(caches) > 1 {
		o.lagging[textOf(o.list.Without(caches[len(caches)-1].Identity))] = true
	}
	return o.list, nil
}

// admits reports a list a host may hold from what this orchestrator served:
// one of those lists, or one less the cache a host has not heard of yet.
func (o *orchestrator) admits(list List) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.served[textOf(list)] {
		return true
	}
	return o.lagging[textOf(list)]
}

func (o *orchestrator) set(list List, down bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.list, o.down = list, down
}

func (o *orchestrator) current() List {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.list
}

// textOf writes a list out, so lists can be told apart as map keys.
func textOf(list List) string {
	var text strings.Builder
	text.WriteString(list.Code().String())
	for _, cache := range list.Caches() {
		fmt.Fprintf(&text, " %s/%d/%s", cache.Identity, cache.Weight, cache.Address)
	}
	return text.String()
}

// A host reads the list as soon as it starts and then every interval, on its
// own clock, and holds the last list it read.
func TestAHostReadsTheListAtOnceAndThenOnItsTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		clock := runtime.NewClock("host")
		ctx := sim.WithRuntime(t.Context(), runtime)
		self := cacheOf(1, 2)
		three := listOf(t, Code{K: 2, M: 1}, self, cacheOf(2, 2), cacheOf(3, 1))
		served := newOrchestrator(runtime, three)
		follower := NewFollower(ctx, FollowerConfig{Initial: Alone(self), Read: served.read, Clock: clock})
		defer follower.Close()
		synctest.Wait()
		if got := follower.Status(); !got.List.Equal(three) || got.Reads != 1 || !got.Read.Equal(sim.Epoch) {
			t.Fatalf("a host that started holds %s after %d reads at %s, want %s", textOf(got.List), got.Reads,
				got.Read, textOf(three))
		}
		four := listOf(t, Code{K: 2, M: 2}, self, cacheOf(2, 2), cacheOf(3, 1), cacheOf(4, 1))
		served.set(four, false)
		clock.Advance(DefaultInterval - time.Second)
		synctest.Wait()
		if got := follower.List(); !got.Equal(three) {
			t.Fatalf("a host holds %s before its interval, want %s", textOf(got), textOf(three))
		}
		clock.Advance(time.Second)
		synctest.Wait()
		if got := follower.Status(); !got.List.Equal(four) || got.Reads != 2 ||
			!got.Read.Equal(sim.Epoch.Add(DefaultInterval)) {
			t.Fatalf("a host holds %s after %d reads at %s, want %s", textOf(got.List), got.Reads, got.Read,
				textOf(four))
		}
	})
}

// An orchestrator that is down leaves every host with the list it last read,
// and a host that never read one stays alone. Each failed read is counted and
// says why; the next read that succeeds takes the list again.
func TestAHostKeepsItsListWhileTheOrchestratorIsDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		clock := runtime.NewClock("host")
		ctx := sim.WithRuntime(t.Context(), runtime)
		self := cacheOf(1, 1)
		two := listOf(t, Code{K: 1, M: 1}, self, cacheOf(2, 1))
		served := newOrchestrator(runtime, two)
		served.set(two, true)
		follower := NewFollower(ctx, FollowerConfig{Initial: Alone(self), Read: served.read, Clock: clock})
		defer follower.Close()
		synctest.Wait()
		if got := follower.Status(); !got.List.Equal(Alone(self)) || got.Failures != 1 ||
			got.Error != errDown.Error() {
			t.Fatalf("a host whose first read failed holds %s with %d failures (%q), want itself alone",
				textOf(got.List), got.Failures, got.Error)
		}
		served.set(two, false)
		clock.Advance(DefaultInterval)
		synctest.Wait()
		served.set(two, true)
		for range 3 {
			clock.Advance(DefaultInterval)
			synctest.Wait()
		}
		if got := follower.Status(); !got.List.Equal(two) || got.Reads != 1 || got.Failures != 4 ||
			got.Error != errDown.Error() || !got.Read.Equal(sim.Epoch.Add(DefaultInterval)) {
			t.Fatalf("a host holds %s after %d reads and %d failures (%q), last read at %s, want %s",
				textOf(got.List), got.Reads, got.Failures, got.Error, got.Read, textOf(two))
		}
		three := listOf(t, Code{K: 1, M: 1}, self, cacheOf(2, 1), cacheOf(3, 1))
		served.set(three, false)
		clock.Advance(DefaultInterval)
		synctest.Wait()
		if got := follower.Status(); !got.List.Equal(three) || got.Error != "" || got.Reads != 2 {
			t.Fatalf("a host holds %s after the orchestrator came back (%q, %d reads), want %s",
				textOf(got.List), got.Error, got.Reads, textOf(three))
		}
		// One read failed while the host was alone and three while it held a
		// list; two reads replaced the list it held.
		want := map[string]uint64{"rank/alone-until-the-list-is-read": 1, "rank/list-kept-after-a-failed-read": 3,
			"rank/list-replaced": 2}
		if got := runtime.Probes(); !maps.Equal(got, want) {
			t.Fatalf("the reads reached %v, want %v", got, want)
		}
	})
}

// A host given nowhere to read the list from stays alone: it ranks first for
// every window, as a host does today.
func TestAHostWithNoOrchestratorStaysAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{})
		clock := runtime.NewClock("host")
		self := cacheOf(4, 1)
		follower := NewFollower(sim.WithRuntime(t.Context(), runtime), FollowerConfig{Initial: Alone(self), Clock: clock})
		defer follower.Close()
		clock.Advance(10 * DefaultInterval)
		synctest.Wait()
		if err := follower.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := follower.Status(); !got.List.Equal(Alone(self)) || got.Reads != 0 || got.Failures != 0 {
			t.Fatalf("a host with no orchestrator holds %s after %d reads", textOf(got.List), got.Reads)
		}
	})
}

// followerSites are the sites the follower campaign must fire, and
// followerProbes the probes it must reach.
var (
	followerSites  = []string{"rank/list-read-fails", "rank/list-lags-a-cache"}
	followerProbes = []string{"rank/alone-until-the-list-is-read", "rank/list-kept-after-a-failed-read",
		"rank/list-replaced"}
)

// followerCampaignSeeds are the seeds the campaign runs, which between them
// fire every site.
var followerCampaignSeeds = []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}

// Four hosts follow one orchestrator while caches join and leave and the
// orchestrator goes down and comes back, with reads that fail and lists that
// lag a cache behind the cluster's. A seeded scheduler chooses the order the
// hosts' reads and the orchestrator's changes land in. Whatever it chooses, a
// host holds only its own list alone or a list the orchestrator served, less
// at most the cache it has not heard of yet. Once the orchestrator answers
// again and nothing fails, every host holds the orchestrator's list and every
// host ranks every window alike. Across the seeds every site fires and every
// probe is reached.
func TestHostsAgreeOnceTheOrchestratorAnswersAgain(t *testing.T) {
	probes, fired := map[string]uint64{}, map[string]uint64{}
	for _, seed := range followerCampaignSeeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime := followerCampaign(t, seed)
				for name, count := range runtime.Probes() {
					probes[name] += count
				}
				for name, count := range runtime.FiredSites() {
					fired[name] += count
				}
			})
		})
	}
	var missed []string
	for _, name := range slices.Concat(followerSites, followerProbes) {
		if probes[name]+fired[name] == 0 {
			missed = append(missed, name)
		}
	}
	if len(missed) != 0 {
		t.Fatalf("the campaign never reached %v; it reached probes %v and fired %v", missed, probes, fired)
	}
}

// followerCampaign runs one seed of the campaign and returns its runtime.
func followerCampaign(t *testing.T, seed uint64) *sim.Runtime {
	scheduler := sim.NewScheduler(seed)
	runtime := sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: true})
	clock := runtime.NewClock("cluster")
	random := runtime.Random("follower-campaign")
	pool := make([]Cache, 8)
	for n := range pool {
		pool[n] = cacheOf(byte(n+1), uint32(1+random.Intn(fmt.Sprintf("weight/%d", n), 3)))
	}
	members, joined := slices.Clone(pool[:4]), 4
	listed := func() List { return listOf(t, CodeFor(4), members...) }
	served := newOrchestrator(runtime, listed())
	served.set(listed(), random.Chance("starts-down", 0.5))
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := sim.WithRuntime(t.Context(), runtime)
		followers := make([]*Follower, 4)
		for n := range followers {
			followers[n] = NewFollower(sim.WithTask(ctx, fmt.Sprintf("host-%d", n)),
				FollowerConfig{Initial: Alone(pool[n]), Read: served.read, Clock: clock})
		}
		defer func() {
			for _, follower := range followers {
				follower.Close()
			}
		}()
		check := func(when string) {
			for n, follower := range followers {
				if list := follower.List(); !list.Equal(Alone(pool[n])) && !served.admits(list) {
					t.Errorf("%s host %d holds %s, which the orchestrator never served", when, n, textOf(list))
				}
			}
		}
		world := sim.WithTask(ctx, "world")
		for step := range 24 {
			if err := runtime.Admit(world, "orchestrator"); err != nil {
				t.Error(err)
				return
			}
			check(fmt.Sprintf("at step %d", step))
			id := fmt.Sprintf("step/%d", step)
			down := false
			switch action := random.Intn(id, 4); {
			case action == 0 && joined < len(pool):
				members = append(members, pool[joined])
				joined++
			case action == 1 && len(members) > 1:
				leaving := random.Intn(id+"/leave", len(members))
				members = slices.Delete(members, leaving, leaving+1)
			case action == 2:
				down = true
			}
			served.set(listed(), down)
			clock.Advance(DefaultInterval)
		}
		// The orchestrator answers again and nothing fails: every host reads
		// the list and holds it.
		served.set(listed(), false)
		runtime.SetBuggify(false)
		for n, follower := range followers {
			if err := follower.Refresh(world); err != nil {
				t.Errorf("host %d read the list with %v", n, err)
				return
			}
		}
		check("at the end")
		final := served.current()
		for n, follower := range followers {
			if got := follower.List(); !got.Equal(final) {
				t.Errorf("host %d holds %s, want the orchestrator's %s", n, textOf(got), textOf(final))
			}
		}
		for span := range uint64(256) {
			window := windowOf("vm-campaign", seed, span)
			want := identities(followers[0].List().Ranks(window))
			for n, follower := range followers[1:] {
				if got := identities(follower.List().Ranks(window)); !slices.Equal(got, want) {
					t.Errorf("host %d ranks span %d %v, and host 0 %v", n+1, span, got, want)
				}
			}
		}
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	return runtime
}
