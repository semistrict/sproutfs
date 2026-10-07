package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/journal"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
	"github.com/semistrict/sproutfs/volume"
)

// journalReadBytes bounds the blocks one answer to a journal read carries.
const journalReadBytes = 16 << 20

// replayer is the host's volume.Replayer: it reads a VM's entries from
// whichever member holds each journal disk its record names, this host
// included, through JOURNAL_READ.
type replayer struct{ h *Host }

// Served reports volume.ErrJournalPending unless every journal named is held
// here, served by a member, or holds nothing to read (journalHolder).
func (r replayer) Served(ctx context.Context, journals []control.Journal) error {
	var m *membership.Membership
	for _, named := range journals {
		disk := rank.Identity(named.Disk)
		if r.h.journalOf(disk) != nil {
			continue
		}
		if m == nil {
			read, err := r.h.members.Read(ctx)
			if err != nil {
				return fmt.Errorf("reading the membership for the journals' holders: %w", err)
			}
			m = &read
		}
		if _, _, err := journalHolder(ctx, *m, disk); err != nil {
			return err
		}
	}
	return nil
}

// Replay reads each journal's entries of vm in turn, after its covered
// position, and hands them to apply in position order.
func (r replayer) Replay(ctx context.Context, vm string, journals []control.Journal, reader uint64,
	apply func(journal.Entry) error) error {
	var m *membership.Membership
	for _, named := range journals {
		disk := rank.Identity(named.Disk)
		request := journal.ReadRequest{VM: vm, Epoch: named.Epoch, After: named.Covered,
			Generation: named.Generation, Reader: reader}
		if r.h.journalOf(disk) != nil {
			if err := r.h.ReadJournal(ctx, disk, request, apply); err != nil {
				return fmt.Errorf("replaying journal %s of %s: %w", disk, vm, err)
			}
			continue
		}
		if m == nil {
			read, err := r.h.members.Read(ctx)
			if err != nil {
				return fmt.Errorf("reading the membership for the journals' holders: %w", err)
			}
			m = &read
		}
		address, holds, err := journalHolder(ctx, *m, disk)
		if err != nil {
			return err
		}
		if !holds {
			continue
		}
		for {
			page, err := r.h.peers.Peer(address).ReadJournal(ctx, disk, request, journalReadBytes)
			switch {
			case errors.Is(err, peer.ErrNoJournal):
				return fmt.Errorf("%w: %s no longer holds journal %s", volume.ErrJournalPending, address, disk)
			case errors.Is(err, journal.ErrGeneration):
				return fmt.Errorf("replaying journal %s of %s from %s: %w", disk, vm, address, err)
			case err != nil:
				// A holder that cannot be read is one the membership has not
				// drained yet, or one isolated from here: the disk moves to a
				// survivor once its member is drained, and the open is asked
				// again then. Nothing read so far is published.
				return fmt.Errorf("%w: reading journal %s of %s from %s: %w", volume.ErrJournalPending, disk, vm,
					address, err)
			}
			for _, entry := range page.Entries {
				if err := apply(entry); err != nil {
					return err
				}
				request.After = entry.Position
			}
			if !page.More {
				break
			}
		}
	}
	return nil
}

// journalHolder is the address of the member m says serves journal disk
// disk, and whether the disk holds anything to read (Membership.JournalHolder).
// A disk that does and no member serves is still moving to one, and is
// volume.ErrJournalPending.
func journalHolder(ctx context.Context, m membership.Membership, disk rank.Identity) (platform.Address, bool,
	error) {
	holder, holds, served := m.JournalHolder(disk)
	switch {
	case !holds && !sim.Bug(ctx, "journal-pending-on-a-let-go-disk"):
		return "", false, nil
	case !served:
		return "", false, fmt.Errorf("%w: journal disk %s is served by no member", volume.ErrJournalPending, disk)
	}
	return holder.Address, true, nil
}
