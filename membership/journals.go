package membership

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// Journal disks (plans/fsync-journal-2026-10-06.md). While durable flush is
// on, every machine in the host pool keeps a journal disk reserved for it,
// and the member on that machine writes it. The disks follow the machines:
// the controller creates them through the cloud's API and deletes them, and
// there is no fixed pool. A journal disk ranks no window.
//
// A disk is free when it is released and reserved for no machine, and empty
// when its last holder closed it with no live entry. Only a free and empty
// disk is reserved for another machine or deleted. A disk whose writer was
// lost is not empty: it is assigned to a surviving member for reading, which
// serves its entries to the hosts that recover its VMs and says it is empty
// once no control record names it.

// nextJournalStep is the one step of the journal disks Next takes after its
// steps 0 to 9, in this order:
//
//   - a journal disk the cloud lists that the membership does not is added,
//     released and empty: a disk leaves the membership only once the cloud
//     no longer lists it, so one the membership does not list is one a
//     controller has just made;
//   - a deleting disk the cloud no longer lists is removed;
//   - a free and empty disk that has expired is marked deleting;
//   - a disk read or released whose holder reports it empty is marked
//     empty, and one marked empty whose holder reports it is not is
//     unmarked;
//   - a disk held for reading that is empty is released;
//   - a released disk reserved for a machine no longer in the pool is
//     reserved for none;
//   - a machine in the pool with no disk reserved for it is reserved a free
//     and empty one;
//   - a member on a machine with a released disk reserved for it is assigned
//     that disk, which it writes;
//   - a released disk that is not empty and reserved for no machine is
//     assigned for reading to the member holding the fewest journal disks.
func nextJournalStep(m Membership, hosts map[rank.Identity]Host, want Want) (Change, bool) {
	listed := make(map[rank.Identity]bool, len(want.Journals))
	for _, journal := range want.Journals {
		listed[journal.ID] = true
		if _, ok := m.Disk(journal.ID); !ok {
			added := journal.Disk
			added.Kind, added.Empty, added.Machine = Journal, true, ""
			return func(m Membership) (Membership, error) { return m.Add(added) }, true
		}
	}
	journals := m.journalDisks()
	for _, disk := range journals {
		if disk.State == Deleting && want.JournalsKnown && !listed[disk.ID] {
			id := disk.ID
			return func(m Membership) (Membership, error) { return m.Remove(id) }, true
		}
	}
	for _, disk := range journals {
		if disk.free() && disk.Empty && slices.Contains(want.Expired, disk.ID) {
			id := disk.ID
			return func(m Membership) (Membership, error) { return m.Delete(id) }, true
		}
	}
	// A disk its writer still serves is written again at any moment: only a
	// disk read, or released, is marked empty. One its holder says holds live
	// entries again, written since it was marked, is unmarked.
	for _, disk := range journals {
		host := hosts[disk.Member]
		held := slices.IndexFunc(host.Disks, func(held Disk) bool { return held.ID == disk.ID })
		if held < 0 || disk.State != Serving && disk.State != Releasing {
			continue
		}
		reading := disk.State == Serving && disk.Machine != host.Machine
		reported := host.Disks[held].Empty
		if (reading || disk.State == Releasing) && reported && !disk.Empty || !reported && disk.Empty {
			id := disk.ID
			return func(m Membership) (Membership, error) { return m.MarkEmpty(id, reported) }, true
		}
	}
	for _, disk := range journals {
		host, wanted := hosts[disk.Member]
		reading := wanted && disk.Machine != host.Machine
		if disk.State == Serving && disk.Empty && reading {
			id := disk.ID
			return func(m Membership) (Membership, error) { return m.Release(id) }, true
		}
	}
	for _, disk := range journals {
		if disk.State == Released && disk.Machine != "" && !slices.Contains(want.Pool, disk.Machine) {
			id := disk.ID
			return func(m Membership) (Membership, error) { return m.Reserve(id, "") }, true
		}
	}
	for _, machine := range slices.Sorted(slices.Values(want.Pool)) {
		if slices.ContainsFunc(journals, func(disk Disk) bool { return disk.Machine == machine }) {
			continue
		}
		for _, disk := range journals {
			if disk.free() && disk.Empty {
				id := disk.ID
				return func(m Membership) (Membership, error) { return m.Reserve(id, machine) }, true
			}
		}
	}
	readers := make(map[rank.Identity]int)
	var takers []rank.Identity
	for _, member := range m.members {
		host, wanted := hosts[member.ID]
		if member.State == Draining || !wanted || host.Leaving || host.Machine == "" {
			continue
		}
		takers = append(takers, member.ID)
		for _, disk := range journals {
			if disk.State == Released && disk.Machine == host.Machine && !holdsOwn(journals, member.ID, host.Machine) {
				id, owner := disk.ID, member.ID
				return func(m Membership) (Membership, error) { return m.Assign(id, owner) }, true
			}
			if disk.Member == member.ID {
				readers[member.ID]++
			}
		}
	}
	if len(takers) == 0 {
		return nil, false
	}
	for _, disk := range journals {
		if disk.State == Released && !disk.Empty && disk.Machine == "" {
			chosen := takers[0]
			for _, member := range takers[1:] {
				if readers[member] < readers[chosen] {
					chosen = member
				}
			}
			id := disk.ID
			return func(m Membership) (Membership, error) { return m.Assign(id, chosen) }, true
		}
	}
	return nil, false
}

// journalDisks is every journal disk of the membership, in identity order.
func (m Membership) journalDisks() []Disk {
	var journals []Disk
	for _, disk := range m.disks {
		if disk.Kind == Journal {
			journals = append(journals, disk)
		}
	}
	return journals
}

// free reports a journal disk released and reserved for no machine.
func (d Disk) free() bool { return d.State == Released && d.Machine == "" }

// holdsOwn reports whether member is assigned the journal disk reserved for
// its machine already.
func holdsOwn(journals []Disk, member rank.Identity, machine string) bool {
	return slices.ContainsFunc(journals, func(disk Disk) bool {
		return disk.Member == member && disk.Machine == machine && disk.State != Released
	})
}

// NeedsJournal is the machines of the pool want names that have no journal
// disk reserved, when m has no free and empty one to reserve for them, nor
// one reserved for a machine gone from the pool, and the cloud lists none m
// does not yet: the disks a controller has to create.
func NeedsJournal(m Membership, want Want) []string {
	journals := m.journalDisks()
	free := 0
	for _, disk := range journals {
		// A released disk reserved for a machine that left the pool is free
		// once Next clears the reservation.
		gone := disk.State == Released && !slices.Contains(want.Pool, disk.Machine)
		if (disk.free() || gone) && disk.Empty {
			free++
		}
	}
	for _, journal := range want.Journals {
		if _, listed := m.Disk(journal.ID); !listed {
			free++
		}
	}
	var needed []string
	for _, machine := range slices.Sorted(slices.Values(want.Pool)) {
		if slices.ContainsFunc(journals, func(disk Disk) bool { return disk.Machine == machine }) {
			continue
		}
		if free > 0 {
			free--
			continue
		}
		needed = append(needed, machine)
	}
	return needed
}

// carryJournal is what Carry asks of the cloud for one journal disk: where m
// assigns or reserves it, given the machines it is attached to.
func carryJournal(ctx context.Context, m Membership, want Want, hosts map[rank.Identity]Host, journal Shard) (target string, keep bool) {
	disk, listed := m.Disk(journal.ID)
	if !listed {
		return "", false
	}
	host, wanted := hosts[disk.Member]
	switch disk.State {
	case Attaching, Serving:
		if wanted {
			target = host.Machine
		}
		keep = target == ""
	case Releasing:
		keep = wanted && slices.ContainsFunc(host.Disks, func(held Disk) bool { return held.ID == journal.ID })
	case Released:
		// A disk reserved for a machine is attached to it at once, while the
		// node boots, so the member there finds it attached.
		if disk.Machine != "" && slices.Contains(want.Pool, disk.Machine) &&
			!sim.Bug(ctx, "membership-attach-journal-late") {
			target = disk.Machine
		}
	}
	return target, keep
}

// JournalControl is a controller's dealings with the cloud over the
// deployment's journal disks: it lists them by their label, creates the ones
// the pool needs, deletes the ones the membership marks deleting, and keeps
// how long each has been free and empty. That time is all it holds between
// passes; a controller that starts again starts the hour again, which only
// delays a delete.
type JournalControl struct {
	Disks platform.NetworkDisks
	// Deployment is the value of the label every journal disk of the
	// deployment carries.
	Deployment string
	// Bytes is the size of a journal disk the controller creates.
	Bytes int64
	// Expiry is how long a disk free and empty is kept before it is deleted.
	// Zero is DefaultJournalExpiry.
	Expiry  time.Duration
	Clock   platform.Clock
	Entropy platform.Entropy

	mu        sync.Mutex
	freeSince map[rank.Identity]time.Time
}

// JournalLabel is the label every journal disk carries, with the
// deployment's name as its value.
const JournalLabel = "sproutfs-journal"

// DefaultJournalExpiry is how long a journal disk free and empty is kept: a
// node the autoscaler removes is gone within the hour, and the next join
// reuses the disk without a create.
const DefaultJournalExpiry = time.Hour

// Describe lists the deployment's journal disks and where each is attached,
// and reports whether the listing succeeded.
func (c *JournalControl) Describe(ctx context.Context) ([]Shard, bool) {
	listed, err := c.Disks.List(ctx, JournalLabel, c.Deployment)
	if err != nil {
		slog.WarnContext(ctx, "membership: listing the journal disks failed", "error", err)
		return nil, false
	}
	journals := make([]Shard, 0, len(listed))
	for _, disk := range listed {
		journals = append(journals, Shard{Disk: Disk{ID: JournalIdentity(disk.Name), Volume: disk.Name,
			Kind: Journal}, Machines: disk.Machines, Known: true})
	}
	return journals, true
}

// Expired is the journal disks of m that have been free and empty for the
// expiry, as this controller has seen them across its passes.
func (c *JournalControl) Expired(m Membership) []rank.Identity {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.freeSince == nil {
		c.freeSince = make(map[rank.Identity]time.Time)
	}
	expiry := c.Expiry
	if expiry == 0 {
		expiry = DefaultJournalExpiry
	}
	clock := platform.ClockOr(c.Clock)
	now := clock.Now()
	var expired []rank.Identity
	seen := make(map[rank.Identity]bool)
	for _, disk := range m.journalDisks() {
		if !disk.free() || !disk.Empty {
			continue
		}
		seen[disk.ID] = true
		since, ok := c.freeSince[disk.ID]
		if !ok {
			c.freeSince[disk.ID] = now
			continue
		}
		if now.Sub(since) >= expiry {
			expired = append(expired, disk.ID)
		}
	}
	for id := range c.freeSince {
		if !seen[id] {
			delete(c.freeSince, id)
		}
	}
	return expired
}

// Create makes one journal disk for the deployment, under a name drawn at
// random.
func (c *JournalControl) Create(ctx context.Context) (string, error) {
	random := make([]byte, 6)
	platform.EntropyOr(c.Entropy).Fill(random)
	name := "sproutfs-journal-" + hex.EncodeToString(random)
	err := c.Disks.Create(ctx, platform.NetworkDiskSpec{Name: name, Bytes: c.Bytes,
		Labels: map[string]string{JournalLabel: c.Deployment}})
	if err != nil {
		return "", fmt.Errorf("creating journal disk %s: %w", name, err)
	}
	slog.InfoContext(ctx, "membership: a journal disk was created", "volume", name, "bytes", c.Bytes)
	return name, nil
}

// Delete deletes in the cloud every journal disk m marks deleting that the
// cloud still lists.
func (c *JournalControl) Delete(ctx context.Context, m Membership, listed []Shard) error {
	var failed []error
	for _, journal := range listed {
		disk, ok := m.Disk(journal.ID)
		if !ok || disk.State != Deleting {
			continue
		}
		err := c.Disks.Delete(ctx, journal.Volume)
		switch {
		case err == nil:
			slog.InfoContext(ctx, "membership: a journal disk was deleted", "volume", journal.Volume)
		case errors.Is(err, platform.ErrNotFound):
		default:
			failed = append(failed, fmt.Errorf("deleting journal disk %s: %w", journal.Volume, err))
		}
	}
	return errors.Join(failed...)
}
