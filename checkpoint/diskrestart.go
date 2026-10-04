package checkpoint

import (
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"

	"github.com/semistrict/sproutfs/platform/sim"
)

// The page cache's disk survives a restart of the host, after a crash too.
// Every item on it is named by an identity that never names other bytes, so
// nothing a restart finds there can be stale; it can only be absent or
// damaged, and every read checks for both. What a restart must get right is
// the order of the log, and which regions it may name at all.
//
// The file's header says whose it is. A file with no header, a damaged one, or
// one of another format, region size or deployment is given back whole and
// made again, under a new identity and a new generation. Otherwise each
// region is read back from its end:
//
//   - A region with a table of this file's generation is indexed from its
//     table. The tables are read newest first, by their sequence numbers, so
//     the index keeps the newest copy of an item a second chance wrote twice,
//     and the closed regions take back their order in the log.
//   - A region with no table was open when the host stopped, or lost its table
//     before it reached the disk. It is given back.
//   - A region whose table is torn is scanned by its items' headers, and the
//     items that read back intact are indexed. Its place in the order is lost,
//     so it is the first to go. A scan that finds nothing gives it back.
//   - A table of another generation was left by a file made before this one.
//     Its region is given back.
//
// The index rebuilt this way is held to its memory bound, newest regions
// first, and the regions past the bound are given back. Then the disk is fitted
// to its share, as it is when the share falls, before it serves a read.

// The probes a restart marks.
const (
	// ProbeDiskRegionFromTable is a region read back from its table.
	ProbeDiskRegionFromTable = "checkpoint/disk-region-from-table"
	// ProbeDiskRegionScanned is a region read back by scanning its items.
	ProbeDiskRegionScanned = "checkpoint/disk-region-scanned"
	// ProbeDiskRegionGivenBackOnOpen is a region that held something given
	// back as the disk opened.
	ProbeDiskRegionGivenBackOnOpen = "checkpoint/disk-region-given-back-on-open"
	// ProbeDiskHeaderRefused is a file whose header was refused, which was
	// made again.
	ProbeDiskHeaderRefused = "checkpoint/disk-header-refused"
)

// The fault-injection sites a restart reads through: a header, and a region's
// table, found torn as the disk opens.
const (
	buggifyDiskTornHeader      = "checkpoint/disk-torn-header"
	buggifyDiskTornTableOnOpen = "checkpoint/disk-torn-table-on-open"
)

// readBack reads back what the cache's file holds, or makes the file anew, and
// fits what it kept to the disk's share. A cache disk does it once, before
// anything else. It fails only where the file cannot be read or made.
func (d *cacheDisk) readBack(ctx context.Context) error {
	size, err := d.file.Size(ctx)
	if err != nil {
		return fmt.Errorf("reading the page cache's disk's size: %w", err)
	}
	if size == 0 {
		return d.makeFile(ctx, d.entropy.Uint64(), false)
	}
	header, err := readDiskHeader(ctx, d.file, d.regionBytes)
	var refused error
	switch {
	case errors.Is(err, errNoHeader) || errors.Is(err, errHeaderDamaged) || errors.Is(err, errHeaderFormat):
		refused = err
	case err != nil:
		return fmt.Errorf("reading the page cache's disk's header: %w", err)
	case header.regionBytes != d.regionBytes:
		refused = fmt.Errorf("its regions are of %d bytes, not %d", header.regionBytes, d.regionBytes)
	case header.deployment != d.deployment && !sim.Bug(ctx, "diskcache-restart-ignores-deployment"):
		refused = fmt.Errorf("it belongs to the deployment %+v, not %+v", header.deployment, d.deployment)
	case d.lease != nil && header.identity != d.identity:
		refused = fmt.Errorf("it is shard %s, not %s", header.identity, d.identity)
	}
	if refused != nil {
		if d.lease != nil {
			// A shard whose header is not this one's may still carry a lease
			// a member newer than this one wrote, and is then not this one's
			// to make anew.
			if found, err := readDiskLease(ctx, d.file, d.regionBytes); err == nil &&
				found.lease.Assigned > d.lease.Assigned && !sim.Bug(ctx, "shard-ignore-lease") {
				sim.Probe(ctx, ProbeShardLeaseRefused)
				return fmt.Errorf("%w: shard %s is leased under generation %d, newer than %d", ErrFenced, d.identity,
					found.lease.Assigned, d.lease.Assigned)
			}
		}
		sim.Probe(ctx, ProbeDiskHeaderRefused)
		slog.WarnContext(ctx, "checkpoint: the page cache's disk is not this cache's; it is emptied", "bytes", size,
			"reason", refused)
		// A header that holds together says which generation to pass. Any
		// other draws one, which no table the old file left will carry.
		generation := d.entropy.Uint64()
		if err == nil {
			generation = header.generation + 1
		}
		return d.makeFile(ctx, generation, true)
	}
	d.identity, d.generation = header.identity, header.generation
	slots := max(size-1, 0) / d.regionBytes
	if d.lease != nil {
		if slots, err = d.takeLease(ctx, slots); err != nil {
			return err
		}
	}
	d.readRegions(ctx, slots)
	return d.fit(ctx)
}

// takeLease takes a shard's lease for the assignment it is opened under, and
// reports how many of the device's slots, of all it has, the file has ever
// opened, which are all a restart reads back. A lease of an assignment newer
// than this one refuses the shard, ErrFenced: another member has served it
// since. A lease of this file that is damaged, or none, says nothing of the
// regions, and every slot is read back. The lease is written and synced before
// anything is read back, so a member that served the shard before and still
// holds its device fences itself at the next region it opens.
func (d *cacheDisk) takeLease(ctx context.Context, all int64) (int64, error) {
	slots := all
	found, err := readDiskLease(ctx, d.file, d.regionBytes)
	switch {
	case errors.Is(err, errNoLease):
	case err != nil:
		return 0, fmt.Errorf("reading the shard's lease: %w", err)
	case found.generation != d.generation:
	case found.lease.Assigned > d.lease.Assigned && !sim.Bug(ctx, "shard-ignore-lease"):
		sim.Probe(ctx, ProbeShardLeaseRefused)
		return 0, fmt.Errorf("%w: shard %s is leased to %s under generation %d, newer than %d", ErrFenced, d.identity,
			found.lease.Member, found.lease.Assigned, d.lease.Assigned)
	default:
		slots = min(found.slots, all)
	}
	if err := d.writeLease(ctx, slots); err != nil {
		return 0, err
	}
	return slots, nil
}

// makeFile gives the whole file back and writes a header of a new identity and
// generation at its start. The file is emptied and synced before the header
// is written, so a crash part way leaves a file with no header, which is made
// again.
func (d *cacheDisk) makeFile(ctx context.Context, generation uint64, empty bool) error {
	identity := d.identity
	if identity.IsZero() {
		d.entropy.Fill(identity[:])
	}
	// A device cannot be emptied, and need not be: the new generation keeps
	// every table on it from being read back as the new file's, and the
	// lease says no region of the new file has been opened.
	if empty {
		switch err := d.file.Truncate(ctx, 0); {
		case errors.Is(err, errors.ErrUnsupported):
		case err != nil:
			return fmt.Errorf("emptying the page cache's disk: %w", err)
		default:
			if err := d.file.Sync(ctx); err != nil {
				return fmt.Errorf("syncing the emptied page cache's disk: %w", err)
			}
		}
	}
	header := diskHeader{regionBytes: d.regionBytes, identity: identity, generation: generation,
		deployment: d.deployment}
	if _, err := d.file.WriteAt(ctx, encodeDiskHeader(header), 0); err != nil {
		return fmt.Errorf("writing the page cache's disk's header: %w", err)
	}
	if err := d.file.Sync(ctx); err != nil {
		return fmt.Errorf("syncing the page cache's disk's header: %w", err)
	}
	d.identity, d.generation = identity, generation
	if d.lease != nil {
		if err := d.writeLease(ctx, 0); err != nil {
			return err
		}
	}
	slog.InfoContext(ctx, "checkpoint: the page cache's disk was made", "identity", identity.String(),
		"generation", generation)
	return nil
}

// recovering is one region a restart found, while it is read back: its slot,
// and the sequence its trailer says.
type recovering struct {
	slot     int64
	sequence uint64
}

// recovery is one restart's reading of the regions: the slots it leaves free,
// and whether the index has reached its memory bound, past which every older
// region is given back.
type recovery struct {
	d    *cacheDisk
	gone []int64
	full bool
}

// readRegions reads back the regions in the first slots of a file whose
// header is this cache's, and rebuilds the index and the log's order from
// them. A region it cannot read is given back.
func (d *cacheDisk) readRegions(ctx context.Context, slots int64) {
	r := &recovery{d: d}
	var tables, torn []recovering
	for slot := range slots {
		found, err := d.classify(ctx, slot)
		switch {
		case err == nil:
			tables = append(tables, found)
		case errors.Is(err, errTornTable):
			torn = append(torn, found)
		case !errors.Is(err, errNoTable):
			r.drop(ctx, slot, err.Error())
		case !d.holdsItems(ctx, slot):
			// A slot given back before, or a region opened and never written,
			// whose space may still be allocated.
			r.free(ctx, slot)
		case sim.Bug(ctx, "diskcache-restart-trusts-open-region"):
			torn = append(torn, found)
		default:
			r.drop(ctx, slot, "it has no table: it was open when the host stopped")
		}
	}
	// Newest first, so the index keeps the newest copy of an item a second
	// chance wrote twice, and the regions its memory bound gives back are the
	// oldest.
	slices.SortFunc(tables, func(a, b recovering) int { return cmp.Compare(b.sequence, a.sequence) })
	var fromTables []*diskRegion
	for _, found := range tables {
		if r.full {
			r.drop(ctx, found.slot, "the index is at its memory bound")
			continue
		}
		table, err := readRegionTable(ctx, d.file, d.base(found.slot), d.regionBytes)
		switch {
		case errors.Is(err, errTornTable) && !sim.Bug(ctx, "diskcache-restart-skips-scan"):
			torn = append(torn, found)
		case err != nil:
			r.drop(ctx, found.slot, err.Error())
		case table.generation != d.generation:
			r.drop(ctx, found.slot, fmt.Sprintf("its table is of generation %d, not %d", table.generation,
				d.generation))
		default:
			d.sequence = max(d.sequence, table.sequence)
			if region := r.keep(ctx, found.slot, table.sequence, table.items); region != nil {
				sim.Probe(ctx, ProbeDiskRegionFromTable)
				d.fromTables++
				fromTables = append(fromTables, region)
			}
		}
	}
	// A region whose table is torn has lost its place in the order, so it is
	// the oldest: by slot, before every region read back from a table.
	slices.SortFunc(torn, func(a, b recovering) int { return cmp.Compare(a.slot, b.slot) })
	var scanned []*diskRegion
	for _, found := range torn {
		if r.full {
			r.drop(ctx, found.slot, "the index is at its memory bound")
			continue
		}
		items, err := scanRegion(ctx, d.file, d.base(found.slot), d.regionBytes)
		if err != nil {
			slog.WarnContext(ctx, "checkpoint: scanning a disk region failed part way; it keeps what was found",
				"slot", found.slot, "items", len(items), "error", err)
		}
		if region := r.keep(ctx, found.slot, 0, items); region != nil {
			sim.Probe(ctx, ProbeDiskRegionScanned)
			d.scanned++
			scanned = append(scanned, region)
		}
	}
	slices.Reverse(fromTables)
	d.closed = append(scanned, fromTables...)
	d.held = len(d.closed)
	d.next = slots
	for _, slot := range r.gone {
		d.free = insertSlot(d.free, slot)
	}
	slog.InfoContext(ctx, "checkpoint: the page cache's disk was read back", "identity", d.identity.String(),
		"generation", d.generation, "regions", d.held, "from_tables", d.fromTables, "scanned", d.scanned,
		"given_back", d.givenBackOnOpen, "items", d.index.live, "index_bytes", d.index.used)
}

// classify reads the trailer at the end of a slot's region: the sequence it
// says, errNoTable where there is none, and errTornTable where it is of
// another format. Nothing it reads is trusted until the whole table is
// checked.
func (d *cacheDisk) classify(ctx context.Context, slot int64) (recovering, error) {
	found := recovering{slot: slot}
	trailer := make([]byte, diskTrailerSize)
	err := readFull(ctx, d.file, trailer, d.base(slot)+d.regionBytes-diskTrailerSize)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return found, errNoTable
	}
	if err != nil {
		return found, err
	}
	if [4]byte(trailer[0:4]) != diskTableMagic {
		return found, errNoTable
	}
	found.sequence = binary.LittleEndian.Uint64(trailer[8:])
	if trailer[4] != diskFormatVersion {
		return found, errTornTable
	}
	return found, nil
}

// holdsItems reports whether a slot's region begins with an item's header,
// which a region given back or never written does not.
func (d *cacheDisk) holdsItems(ctx context.Context, slot int64) bool {
	fixed := make([]byte, diskItemFixed)
	if err := readFull(ctx, d.file, fixed, d.base(slot)); err != nil {
		return false
	}
	_, ok := itemLength(fixed)
	return ok
}

// keep indexes the items of a slot's region, in the order they lie, leaving
// out a key the index already holds from a newer region, and returns the
// region. A region that would take the index past its memory bound is given
// back, and so is every region read back after it. A region that names
// nothing the index keeps is given back too.
func (r *recovery) keep(ctx context.Context, slot int64, sequence uint64, items []tableItem) *diskRegion {
	d := r.d
	region := &diskRegion{slot: slot, base: d.base(slot), sequence: sequence}
	for _, item := range items {
		if _, found := d.index.lookup(item.key, item.code, false, false); found {
			continue
		}
		d.index.insert(item.key, item.code, region, region.base+int64(item.offset), int64(item.length))
	}
	if d.index.used > d.indexLimit {
		d.index.dropRegion(region)
		r.full = true
		r.drop(ctx, slot, "the index is at its memory bound")
		return nil
	}
	if len(region.entries) == 0 {
		r.drop(ctx, slot, "it holds no intact item a newer region does not")
		return nil
	}
	return region
}

// drop gives back a region that held something, which the restart does not
// keep.
func (r *recovery) drop(ctx context.Context, slot int64, reason string) {
	sim.Probe(ctx, ProbeDiskRegionGivenBackOnOpen)
	r.d.givenBackOnOpen++
	slog.InfoContext(ctx, "checkpoint: a disk region is given back as the disk opens", "slot", slot,
		"reason", reason)
	r.free(ctx, slot)
}

// free punches a slot out and leaves it free.
func (r *recovery) free(ctx context.Context, slot int64) {
	if err := r.d.punch(ctx, &diskRegion{slot: slot, base: r.d.base(slot)}); err != nil {
		slog.WarnContext(ctx, "checkpoint: punching out a disk region failed", "slot", slot, "error", err)
	}
	r.gone = append(r.gone, slot)
}
