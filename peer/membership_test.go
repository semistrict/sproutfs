package peer_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// cluster is two hosts that each keep a copy of one disk, as two pods over a
// copied cache file do, the membership in the simulated store, and a reader.
type cluster struct {
	t       *testing.T
	runtime *sim.Runtime
	ctx     context.Context
	store   *membership.Store
	disk    rank.Identity
	// caches and views are each host's, by member.
	caches map[rank.Identity]*memoryCache
	views  map[rank.Identity]*membership.View
	reader *peer.Table
}

var (
	memberA = rank.Identity{15: 0xa}
	memberB = rank.Identity{15: 0xb}
)

// newCluster starts both hosts' peer servers, each with a view of the
// membership that reads only when a request is ahead of it, and writes the
// membership in which member A joins with the disk and serves it.
func newCluster(t *testing.T) *cluster {
	t.Helper()
	runtime := sim.New(sim.Config{Seed: 1})
	ctx := sim.WithRuntime(t.Context(), runtime)
	store, err := membership.NewStore(membership.Config{ObjectStore: runtime.ObjectStore(),
		Entropy: runtime.NewEntropy("writer")})
	if err != nil {
		t.Fatal(err)
	}
	c := &cluster{t: t, runtime: runtime, ctx: ctx, store: store, disk: rank.Identity{1},
		caches: map[rank.Identity]*memoryCache{}, views: map[rank.Identity]*membership.View{}}
	for _, member := range []rank.Identity{memberA, memberB} {
		cache := newMemoryCache(1)
		cache.stripes[stripeKey{window, 0, 0}] = []byte("a stripe")
		view := membership.NewView(ctx, membership.ViewConfig{Store: store, Initial: membership.Empty(), Interval: -1})
		t.Cleanup(view.Close)
		server, err := peer.NewServer(ctx, peer.ServerConfig{Network: runtime.Network(), Address: addressOf(member),
			PageSize: pageSize, Cache: cache, Membership: view, Member: member})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		c.caches[member], c.views[member] = cache, view
	}
	c.reader = newTable(t, runtime, peer.TableConfig{Dial: func(ctx context.Context, to platform.Address) (platform.Conn, error) {
		return runtime.Network().Dial(ctx, "reader", to)
	}})
	c.change(func(m membership.Membership) (membership.Membership, error) {
		return m.Join(membership.Member{ID: memberA, Address: addressOf(memberA)},
			membership.Disk{ID: c.disk, Volume: "cache-0", Weight: 1})
	})
	c.change(func(m membership.Membership) (membership.Membership, error) { return m.Serve(c.disk, memberA) })
	c.change(func(m membership.Membership) (membership.Membership, error) {
		return m.Join(membership.Member{ID: memberB, Address: addressOf(memberB)})
	})
	return c
}

func addressOf(member rank.Identity) platform.Address {
	return platform.Address("host-" + member.String()[31:])
}

// change writes one change of the membership and returns it.
func (c *cluster) change(change func(membership.Membership) (membership.Membership, error)) membership.Membership {
	c.t.Helper()
	m, err := c.store.Update(c.ctx, change)
	if err != nil {
		c.t.Fatal(err)
	}
	return m
}

// read asks the host of member for the disk's stripe of window, routed by
// m's route to the disk but sent to member's address.
func (c *cluster) read(member rank.Identity, route membership.Route) error {
	reply, err := c.reader.Peer(addressOf(member)).ReadStripes(c.ctx, route,
		peer.StripeRead{Window: window, Code: rank.Code{K: 1, M: 0}, MaxBytes: 1 << 10})
	if err == nil {
		defer reply.Release()
		if len(reply.Items) != 1 || string(reply.Payload) != "a stripe" {
			return errors.New("the read came back without its stripe")
		}
	}
	return err
}

// routeOf is how m routes a request to the disk.
func (c *cluster) routeOf(m membership.Membership) membership.Route {
	c.t.Helper()
	route, ok := m.Route(c.disk)
	if !ok {
		c.t.Fatalf("generation %d routes nothing to the disk", m.Generation())
	}
	return route
}

// A host behind a request's generation reads the membership before it
// answers, and then answers under the request's generation.
func TestAHolderBehindReadsTheMembershipBeforeItAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCluster(t)
		if _, err := c.views[memberA].Refresh(c.ctx); err != nil {
			t.Fatal(err)
		}
		next := c.change(func(m membership.Membership) (membership.Membership, error) { return m.Weigh(c.disk, 2) })
		if err := c.read(memberA, c.routeOf(next)); err != nil {
			t.Fatalf("a holder behind the read answered %v", err)
		}
		if got := c.views[memberA].Current().Generation(); got != next.Generation() {
			t.Fatalf("the holder holds generation %d, want %d", got, next.Generation())
		}
		if c.runtime.Probes()[membership.ProbeHolderCaughtUp] != 1 {
			t.Fatalf("probes %v, want the holder caught up once", c.runtime.Probes())
		}
	})
}

// A host ahead of a request's generation answers stale with its own, and
// its cache is never asked; so does a host behind it that cannot read the
// membership, with its own older generation.
func TestAHolderOnAnotherGenerationAnswersStaleWithItsOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCluster(t)
		first, err := c.views[memberA].Refresh(c.ctx)
		if err != nil {
			t.Fatal(err)
		}
		stale := c.routeOf(first)
		next := c.change(func(m membership.Membership) (membership.Membership, error) { return m.Weigh(c.disk, 2) })
		c.runtime.ObjectStore().FailNext(sim.ObjectGet, 1)
		var behind *peer.StaleError
		if err := c.read(memberA, c.routeOf(next)); !errors.As(err, &behind) || behind.Generation != first.Generation() {
			t.Fatalf("a holder that could not read the membership answered %v, want stale at %d", err,
				first.Generation())
		}
		if _, err := c.views[memberA].Refresh(c.ctx); err != nil {
			t.Fatal(err)
		}
		var ahead *peer.StaleError
		if err := c.read(memberA, stale); !errors.As(err, &ahead) || ahead.Generation != next.Generation() {
			t.Fatalf("a holder ahead of the read answered %v, want stale at %d", err, next.Generation())
		}
		if asked := c.caches[memberA].asked; asked != 0 {
			t.Fatalf("a holder answered stale after asking its cache %d times", asked)
		}
	})
}

// Two hosts both keep a copy of the disk and both believe they hold it. The
// membership moves it from A to B: released, let go, assigned to B and
// served. Under the generation that moved it, A answers not me without
// asking its cache, whichever route it is asked by, and B serves it; a reader
// still on A's generation is told it is stale. A host that lost a disk never
// serves it again.
func TestAMemberThatLostADiskNeverServesItAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newCluster(t)
		held, err := c.views[memberA].Refresh(c.ctx)
		if err != nil {
			t.Fatal(err)
		}
		old := c.routeOf(held)
		if err := c.read(memberA, old); err != nil {
			t.Fatalf("the disk's first member answered %v", err)
		}
		c.change(func(m membership.Membership) (membership.Membership, error) { return m.Release(c.disk) })
		c.change(func(m membership.Membership) (membership.Membership, error) { return m.Let(c.disk, memberA) })
		c.change(func(m membership.Membership) (membership.Membership, error) { return m.Assign(c.disk, memberB) })
		moved := c.change(func(m membership.Membership) (membership.Membership, error) { return m.Serve(c.disk, memberB) })
		route := c.routeOf(moved)
		if route.Address != addressOf(memberB) || route.Assigned <= old.Assigned {
			t.Fatalf("the moved disk routes to %+v", route)
		}
		asked := c.caches[memberA].asked
		if err := c.read(memberA, route); !errors.Is(err, peer.ErrNotMe) {
			t.Fatalf("the member that lost the disk answered %v, want not me", err)
		}
		if err := c.read(memberA, old); !errors.Is(err, peer.ErrStale) {
			t.Fatalf("the member that lost the disk answered a read on the old generation with %v, want stale", err)
		}
		if got := c.caches[memberA].asked; got != asked {
			t.Fatalf("the member that lost the disk asked its cache %d more times", got-asked)
		}
		if probes := c.runtime.Probes(); probes[membership.ProbeNotServed] != 1 ||
			probes[membership.ProbeStaleAnswered] != 1 {
			t.Fatalf("probes %v, want one request not served and one answered stale", probes)
		}
		if err := c.read(memberB, route); err != nil {
			t.Fatalf("the disk's new member answered %v", err)
		}
	})
}
