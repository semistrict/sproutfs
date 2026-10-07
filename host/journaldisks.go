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

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/journal"
	"github.com/semistrict/sproutfs/membership"
	"github.com/semistrict/sproutfs/peer"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/rank"
)

// Journal disks on a host. The membership assigns this host the journal disk
// reserved for its machine, which it writes, and may assign it others for
// reading, after their writers were lost. The host opens each once the
// object read again still assigns it here and the cloud has attached it,
// takes its lease, and reports it to the membership with whether it holds a
// live entry. It closes a disk the membership releases once the membership
// has marked it empty, writing that into its header, and closes at once a
// disk the membership no longer assigns here or whose lease another member
// took. A disk the host closed is no longer reported, so it waits for the
// mark: a disk closed first would be let go never marked, and kept forever.

// DefaultJournalDiskInterval is how often a host looks again at the journal
// disks the membership assigns it.
const DefaultJournalDiskInterval = time.Second

// The probes journal disks mark on a host.
const (
	// ProbeJournalDiskOpened is a journal disk a host opened, to write or
	// to read.
	ProbeJournalDiskOpened = "host/journal-disk-opened"
	// ProbeJournalDiskClosedEmpty is a released journal disk a host closed
	// once it held no live entry.
	ProbeJournalDiskClosedEmpty = "host/journal-disk-closed-empty"
	// ProbeJournalDiskLost is an open journal disk whose lease another
	// member took or that the membership no longer assigns here.
	ProbeJournalDiskLost = "host/journal-disk-lost"
)

// openJournal is one journal disk the host holds open.
type openJournal struct {
	device platform.File
	j      *journal.Journal
	disk   membership.Disk
	// own marks the disk reserved for this host's machine, which it writes.
	own bool
}

// journalDisks opens and closes the journal disks the membership assigns
// this host.
type journalDisks struct {
	h       *Host
	devices platform.Devices
	machine string
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	passing *ctxsync.Mutex

	mu   sync.Mutex
	open map[rank.Identity]*openJournal
}

func newJournalDisks(ctx context.Context, h *Host, devices platform.Devices, machine string,
	interval time.Duration) *journalDisks {
	if interval == 0 {
		interval = DefaultJournalDiskInterval
	}
	ctx, cancel := context.WithCancel(ctx)
	d := &journalDisks{h: h, devices: devices, machine: machine, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		passing: ctxsync.NewMutex(), open: make(map[rank.Identity]*openJournal)}
	go d.loop(interval)
	return d
}

func (d *journalDisks) loop(interval time.Duration) {
	defer close(d.done)
	ticker := d.h.clock.NewTicker(interval)
	defer ticker.Stop()
	for {
		changed := d.h.view.Changed()
		d.pass(d.ctx)
		select {
		case <-d.ctx.Done():
			return
		case <-changed:
		case <-ticker.C():
		}
	}
}

// assigned is the journal disks m assigns this host, in any state that has
// a member.
func (d *journalDisks) assigned(m membership.Membership) map[rank.Identity]membership.Disk {
	wanted := make(map[rank.Identity]membership.Disk)
	for _, disk := range m.Disks() {
		if disk.Kind == membership.Journal && disk.Member == d.h.self.ID &&
			(disk.State == membership.Attaching || disk.State == membership.Serving ||
				disk.State == membership.Releasing) {
			wanted[disk.ID] = disk
		}
	}
	return wanted
}

// pass closes what the membership no longer assigns here, a released disk
// with no live entry, and one whose lease is gone, and opens what it assigns
// here that is attached.
func (d *journalDisks) pass(ctx context.Context) {
	if err := d.passing.Lock(ctx); err != nil {
		return
	}
	defer d.passing.Unlock()
	wanted := d.assigned(d.h.view.Current())
	d.mu.Lock()
	open := maps.Clone(d.open)
	d.mu.Unlock()
	for _, identity := range slices.SortedFunc(maps.Keys(open), compareIdentity) {
		held := open[identity]
		want, still := wanted[identity]
		switch {
		case !still || want.Assigned != held.disk.Assigned:
			sim.Probe(ctx, ProbeJournalDiskLost)
			d.close(ctx, identity, "the membership no longer assigns it here")
		case want.State == membership.Releasing && want.Empty:
			// A flush may have written the disk since its holder said it was
			// empty. The close then finds live entries, and the disk is
			// opened again, so its holder says so, and the membership unmarks
			// it before it lets it go.
			if !d.close(ctx, identity, "it is released and marked empty") {
				slog.WarnContext(ctx, "host: a journal disk marked empty held live entries at its close",
					"disk", identity.String(), "volume", want.Volume)
				if err := d.openDisk(ctx, want); err != nil && ctx.Err() == nil {
					slog.WarnContext(ctx, "host: opening a journal disk again failed", "disk", identity.String(),
						"error", err)
				}
				continue
			}
			sim.Probe(ctx, ProbeJournalDiskClosedEmpty)
		default:
			if err := held.j.CheckLease(ctx); err != nil {
				sim.Probe(ctx, ProbeJournalDiskLost)
				d.close(ctx, identity, err.Error())
				continue
			}
			d.mu.Lock()
			held.disk = want
			d.mu.Unlock()
		}
	}
	for _, identity := range slices.SortedFunc(maps.Keys(wanted), compareIdentity) {
		if _, held := open[identity]; held || wanted[identity].State == membership.Releasing {
			continue
		}
		if err := d.openDisk(ctx, wanted[identity]); err != nil && ctx.Err() == nil {
			slog.DebugContext(ctx, "host: a journal disk assigned here is not open yet", "disk", identity.String(),
				"volume", wanted[identity].Volume, "error", err)
		}
	}
}

// openDisk opens a journal disk the membership assigns here, once the object
// read again still assigns it here under the same generation.
func (d *journalDisks) openDisk(ctx context.Context, disk membership.Disk) error {
	read, err := d.h.view.Refresh(ctx)
	if err != nil {
		return fmt.Errorf("reading the membership before opening a journal disk: %w", err)
	}
	if again, still := d.assigned(read)[disk.ID]; !still || again.Assigned != disk.Assigned {
		return fmt.Errorf("the membership no longer assigns journal disk %s here under generation %d", disk.ID,
			disk.Assigned)
	}
	device, err := d.devices.Open(ctx, disk.Volume)
	if err != nil {
		return err
	}
	j, err := journal.Open(ctx, device, journal.Config{Identity: disk.ID, Clock: d.h.clock, Entropy: d.h.entropy,
		Lease: journal.Lease{Assigned: disk.Assigned, Member: d.h.self.ID}})
	if err != nil {
		if closeErr := device.Close(); closeErr != nil {
			slog.WarnContext(ctx, "host: closing a journal disk's device failed", "disk", disk.ID.String(),
				"error", closeErr)
		}
		return err
	}
	own := disk.Machine != "" && disk.Machine == d.machine
	d.mu.Lock()
	d.open[disk.ID] = &openJournal{device: device, j: j, disk: disk, own: own}
	d.mu.Unlock()
	if own {
		d.h.SetJournal(j)
	}
	sim.Probe(ctx, ProbeJournalDiskOpened)
	slog.InfoContext(ctx, "host: a journal disk is open", "disk", disk.ID.String(), "volume", disk.Volume,
		"assigned", disk.Assigned, "own", own, "live", len(j.Held()))
	return nil
}

// close closes one journal disk: the journal, which writes empty into its
// header where no entry is live, and then its device. It reports whether the
// disk held no live entry.
func (d *journalDisks) close(ctx context.Context, identity rank.Identity, why string) bool {
	d.mu.Lock()
	held := d.open[identity]
	delete(d.open, identity)
	d.mu.Unlock()
	if held == nil {
		return false
	}
	if held.own {
		d.h.SetJournal(nil)
	}
	empty, err := held.j.Close(context.WithoutCancel(ctx))
	if err != nil && !errors.Is(err, journal.ErrClosed) {
		slog.WarnContext(ctx, "host: closing a journal failed", "disk", identity.String(), "error", err)
	}
	if err := held.device.Close(); err != nil {
		slog.WarnContext(ctx, "host: closing a journal disk's device failed", "disk", identity.String(), "error", err)
	}
	slog.InfoContext(ctx, "host: a journal disk is closed", "disk", identity.String(), "volume", held.disk.Volume,
		"empty", empty, "why", why)
	return empty && err == nil
}

// held is the journal disks this host holds open, as it reports them to the
// membership: each with whether it holds no live entry. The disk the host
// writes is empty only while the host runs no VM, which could flush to it
// the moment after.
func (d *journalDisks) held() []membership.Disk {
	running := len(d.h.Machines()) > 0
	d.mu.Lock()
	defer d.mu.Unlock()
	disks := make([]membership.Disk, 0, len(d.open))
	for _, identity := range slices.SortedFunc(maps.Keys(d.open), compareIdentity) {
		held := d.open[identity]
		empty := len(held.j.Held()) == 0 && !(held.own && running)
		disks = append(disks, membership.Disk{ID: held.disk.ID, Volume: held.disk.Volume,
			Kind: membership.Journal, Empty: empty})
	}
	return disks
}

// journals is every journal this host holds open.
func (d *journalDisks) journals() []*journal.Journal {
	d.mu.Lock()
	defer d.mu.Unlock()
	all := make([]*journal.Journal, 0, len(d.open))
	for _, identity := range slices.SortedFunc(maps.Keys(d.open), compareIdentity) {
		all = append(all, d.open[identity].j)
	}
	return all
}

// find is the open journal of identity, nil for none.
func (d *journalDisks) find(identity rank.Identity) *journal.Journal {
	d.mu.Lock()
	defer d.mu.Unlock()
	if held := d.open[identity]; held != nil {
		return held.j
	}
	return nil
}

// stop ends the passes and closes every open journal disk.
func (d *journalDisks) stop() {
	d.cancel()
	<-d.done
	d.mu.Lock()
	identities := slices.Collect(maps.Keys(d.open))
	d.mu.Unlock()
	for _, identity := range identities {
		d.close(context.Background(), identity, "the host is closing")
	}
}

// heldJournals is every journal this host holds: its own, and those it holds
// for reading.
func (h *Host) heldJournals() []*journal.Journal {
	if h.journalDisks != nil {
		return h.journalDisks.journals()
	}
	if j := h.journal(); j != nil {
		return []*journal.Journal{j}
	}
	return nil
}

// journalOf is the journal of the disk identity this host holds, nil for
// none.
func (h *Host) journalOf(identity rank.Identity) *journal.Journal {
	if h.journalDisks != nil {
		return h.journalDisks.find(identity)
	}
	if j := h.journal(); j != nil && j.Identity() == identity {
		return j
	}
	return nil
}

// errNotHeld is peer.ErrNoJournal for a disk this host does not hold.
func errNotHeld(disk rank.Identity) error { return fmt.Errorf("%w: %s", peer.ErrNoJournal, disk) }
