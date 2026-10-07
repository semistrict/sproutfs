package host

import (
	"maps"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// The covered position is the highest position of the captures that began
// before the pause ended, whether the journal had placed them by then or
// places them after, and never one of a capture that began after.
func TestTheCoveredPositionIsTheLastCaptureBeforeThePause(t *testing.T) {
	ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
	for _, placedFirst := range []bool{true, false} {
		s := &vmJournal{}
		s.captures++
		if placedFirst {
			s.placedOne([]uint64{100, 140})
		}
		c := &cover{state: s, resolved: make(chan struct{})}
		c.paused(ctx)
		s.captures++
		if !placedFirst {
			s.placedOne([]uint64{100, 140})
		}
		s.placedOne([]uint64{200})
		if got, err := c.position(ctx); err != nil || got != 140 {
			t.Fatalf("placed before the pause %v: the cover is at %d (%v), want 140", placedFirst, got, err)
		}
	}
}

// The shard server opens only the cache disks the membership assigns this
// host: a journal disk assigned here is the journal disks' server's, and opened
// as a shard its entries would be emptied.
func TestTheShardServerLeavesJournalDisksAlone(t *testing.T) {
	ctx := sim.WithRuntime(t.Context(), sim.New(sim.Config{}))
	self, journal, shard := rank.Identity{1}, membership.JournalIdentity("journal-0"), membership.ShardIdentity("shard-0")
	m := membership.Empty()
	for _, change := range []func(membership.Membership) (membership.Membership, error){
		func(m membership.Membership) (membership.Membership, error) {
			return m.Join(membership.Member{ID: self, Address: "self-pages"})
		},
		func(m membership.Membership) (membership.Membership, error) {
			return m.Add(membership.Disk{ID: journal, Volume: "journal-0", Kind: membership.Journal})
		},
		func(m membership.Membership) (membership.Membership, error) {
			return m.Add(membership.Disk{ID: shard, Volume: "shard-0", Weight: 4})
		},
		func(m membership.Membership) (membership.Membership, error) { return m.Assign(journal, self) },
		func(m membership.Membership) (membership.Membership, error) { return m.Assign(shard, self) },
	} {
		next, err := change(m)
		if err != nil {
			t.Fatal(err)
		}
		m = next
	}
	assigned := (&shardServer{self: self}).assigned(ctx, m)
	if _, found := assigned[journal]; found || len(assigned) != 1 {
		t.Fatalf("the shard server would open %v, want the cache shard alone", slices.Collect(maps.Keys(assigned)))
	}
}
