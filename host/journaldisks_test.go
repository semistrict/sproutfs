package host_test

import (
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/membership"
)

// newJournalCluster is hosts with durable flush on and no shards, and a
// controller that keeps a journal disk for each of their machines.
func newJournalCluster(t *testing.T) *shardCluster {
	t.Helper()
	c := newShardCluster(t, 0)
	c.journals = true
	c.control.Journals = &membership.JournalControl{Disks: c.cloud, Deployment: "d", Bytes: 64 << 20,
		Entropy: c.runtime.NewEntropy("journals")}
	return c
}

// settleJournals passes until the membership is the same generation three
// passes running.
func (c *shardCluster) settleJournals() membership.Membership {
	c.t.Helper()
	last, quiet := uint64(0), 0
	for range 200 {
		generation := c.pass()
		if generation == last {
			if quiet++; quiet == 3 {
				m, err := c.control.Store.Read(c.ctx)
				if err != nil {
					c.t.Fatal(err)
				}
				return m
			}
			continue
		}
		last, quiet = generation, 0
	}
	c.t.Fatal("the journal disks never settled")
	return membership.Membership{}
}

// ownJournal is the journal disk reserved for machine.
func ownJournal(t *testing.T, m membership.Membership, machine string) membership.Disk {
	t.Helper()
	for _, disk := range m.Disks() {
		if disk.Kind == membership.Journal && disk.Machine == machine {
			return disk
		}
	}
	t.Fatalf("no journal disk is reserved for %s", machine)
	return membership.Disk{}
}

// Each host serves the journal disk the controller made for its machine, and
// only then says durable flush is served. A host that drains closes its disk
// once the membership marks it empty, and the next machine is given it.
func TestEachHostServesTheJournalDiskMadeForItsMachine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newJournalCluster(t)
		one, two := c.start("machine-1"), c.start("machine-2")
		if one.host.Activity().Journal.Served {
			t.Fatal("a host serves durable flush before it has a journal disk")
		}
		m := c.settleJournals()
		for _, h := range []*shardHost{one, two} {
			disk := ownJournal(t, m, h.machine)
			self, _ := h.host.Member()
			if disk.State != membership.Serving || disk.Member != self.ID || disk.Empty {
				t.Fatalf("%s's journal disk is %+v, want serving for it and not empty", h.machine, disk)
			}
			if got := c.cloud.Attached(disk.Volume); got != h.machine {
				t.Fatalf("%s's journal disk is attached to %q", h.machine, got)
			}
			if !h.host.Activity().Journal.Served {
				t.Fatalf("%s holds its journal disk and does not serve durable flush", h.machine)
			}
			// A shard pass here opens every cache disk assigned to this host,
			// and a journal disk must not be one.
			h.host.SettleShards(c.ctx)
			self, _ = h.host.Member()
			opened := 0
			for _, held := range self.Disks {
				if held.ID == disk.ID {
					opened++
				}
			}
			if opened != 1 {
				t.Fatalf("%s holds its journal disk open %d times, want once, as a journal", h.machine, opened)
			}
		}
		drained := ownJournal(t, m, "machine-2")
		two.leaving = true
		m = c.settleJournals()
		if disk, _ := m.Disk(drained.ID); disk.State != membership.Released || !disk.Empty {
			t.Fatalf("the drained host's journal disk is %+v, want released and empty", disk)
		}
		if two.host.Activity().Journal.Served {
			t.Fatal("a host that closed its journal disk still serves durable flush")
		}
		c.stop("machine-2")
		three := c.start("machine-3")
		m = c.settleJournals()
		if got := ownJournal(t, m, "machine-3"); got.ID != drained.ID {
			t.Fatalf("machine-3 was given %s, want the drained disk %s", got.Volume, drained.Volume)
		}
		if !three.host.Activity().Journal.Served {
			t.Fatal("the host given a drained journal disk does not serve durable flush")
		}
		listed, err := c.cloud.List(c.ctx, membership.JournalLabel, "d")
		if err != nil {
			t.Fatal(err)
		}
		if len(listed) != 2 {
			t.Fatalf("the cloud has %d journal disks, want the two reused", len(listed))
		}
	})
}
