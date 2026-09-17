package sim

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
)

type DiskOperation string

const (
	DiskOpen          DiskOperation = "open"
	DiskRead          DiskOperation = "read"
	DiskWrite         DiskOperation = "write"
	DiskTruncate      DiskOperation = "truncate"
	DiskPunchHole     DiskOperation = "punch_hole"
	DiskSync          DiskOperation = "sync"
	DiskSize          DiskOperation = "size"
	DiskRemove        DiskOperation = "remove"
	DiskRename        DiskOperation = "rename"
	DiskList          DiskOperation = "list"
	DiskSyncNamespace DiskOperation = "sync_namespace"
)

type DiskConfig struct {
	OpenLatency     time.Duration
	ReadLatency     time.Duration
	WriteLatency    time.Duration
	SyncLatency     time.Duration
	MetadataLatency time.Duration
	BytesPerSecond  int64
	// PowerLossFaults resolves every modification made since a file's last
	// successful Sync at PowerLoss, instead of discarding all of them. Each
	// write is resolved page by page and sector by sector into bytes that were
	// applied, dropped, kept only as a prefix, or replaced by garbage, bounded
	// by a kill mode drawn for the file when it is opened. It is off by
	// default: a device that restores exactly its last sync is the weaker
	// assumption every existing test is written against.
	PowerLossFaults bool
	// SyncDurableProbability is the chance that a file Sync actually persists
	// the modifications before it. Zero means one: a sync that reports success
	// has always persisted. A value below one models a device that acknowledges
	// a flush it did not perform, which is FoundationDB's ten percent
	// survive-a-kill-anyway draw inverted. Nothing in sproutfs treats a local
	// file as durable, so no consumer test drives this; it exists so that a
	// campaign can.
	SyncDurableProbability float64
}

func DefaultDiskConfig() DiskConfig {
	return DiskConfig{
		OpenLatency:     50 * time.Microsecond,
		ReadLatency:     100 * time.Microsecond,
		WriteLatency:    100 * time.Microsecond,
		SyncLatency:     250 * time.Microsecond,
		MetadataLatency: 100 * time.Microsecond,
		BytesPerSecond:  1 << 30,
	}
}

func (c DiskConfig) withDefaults(defaults DiskConfig) DiskConfig {
	c.OpenLatency = cmp.Or(c.OpenLatency, defaults.OpenLatency)
	c.ReadLatency = cmp.Or(c.ReadLatency, defaults.ReadLatency)
	c.WriteLatency = cmp.Or(c.WriteLatency, defaults.WriteLatency)
	c.SyncLatency = cmp.Or(c.SyncLatency, defaults.SyncLatency)
	c.MetadataLatency = cmp.Or(c.MetadataLatency, defaults.MetadataLatency)
	c.BytesPerSecond = cmp.Or(c.BytesPerSecond, defaults.BytesPerSecond)
	return c
}

// KillMode bounds how badly one file's unsynced modifications may be resolved
// at a power loss, as FoundationDB's AsyncFileNonDurable bounds them: a file is
// opened under DropOnly or FullCorruption, and each of its pages then draws a
// mode no worse than the file's own.
type KillMode uint8

const (
	// NoCorruption applies a modification exactly. It is drawn for a page,
	// never as a file's own mode.
	NoCorruption KillMode = iota
	// DropOnly either applies a sector or loses it entirely.
	DropOnly
	// FullCorruption may also leave a sector holding only part of what was
	// written, or holding garbage.
	FullCorruption
)

// PowerLossOutcome names what a power loss did to one modification that had not
// been synced.
type PowerLossOutcome string

const (
	// PowerLossApplied means every sector of the modification survived.
	PowerLossApplied PowerLossOutcome = "applied"
	// PowerLossDropped means none of it survived.
	PowerLossDropped PowerLossOutcome = "dropped"
	// PowerLossTruncated means some sectors survived and the rest did not,
	// which is the torn tail a reader sees as a short or half-written record.
	PowerLossTruncated PowerLossOutcome = "prefix_truncated"
	// PowerLossGarbled means at least one sector holds bytes nobody wrote.
	PowerLossGarbled PowerLossOutcome = "sector_garbled"
)

// The page a kill mode is drawn for and the sector that mode is applied to.
// These are FoundationDB's 4 KiB and 512 B: they describe the device, not the
// pager's page size.
const (
	diskPageBytes   = 4096
	diskSectorBytes = 512
)

type pendingKind string

const (
	pendingWrite    pendingKind = "write"
	pendingTruncate pendingKind = "truncate"
	pendingPunch    pendingKind = "punch_hole"
)

// pendingOp is one modification made since the file's last successful Sync. The
// list replays onto the durable image to produce what a power loss leaves.
type pendingOp struct {
	kind   pendingKind
	offset int64
	length int64
	data   []byte
	id     uint64
}

type diskImage struct {
	volatile      []byte
	durable       []byte
	durableExists bool
	// pending is every modification since the last successful Sync, in the
	// order it reached volatile. It is retained only when the disk resolves
	// unsynced writes; otherwise a power loss discards all of them anyway.
	pending []pendingOp
	// killMode bounds this file's resolution and is redrawn at every Open, as
	// FoundationDB draws one per file handle.
	killMode KillMode
	// opens counts this image's opens, so the kill mode each one draws is
	// keyed by something that does not repeat.
	opens uint64
}

// Disk models a single queued storage device. PowerLoss invalidates open
// handles and, unless the disk resolves unsynced writes, discards every change
// made since the last successful file Sync.
type Disk struct {
	runtime *Runtime
	id      string
	config  DiskConfig
	queue   chan struct{}

	mu       sync.Mutex
	files    map[string]*diskImage
	epoch    uint64
	failed   bool
	sequence map[string]uint64
	failNext map[DiskOperation]int
	tearNext int
}

func newDisk(runtime *Runtime, id string, config DiskConfig) *Disk {
	d := &Disk{
		runtime:  runtime,
		id:       id,
		config:   config,
		queue:    make(chan struct{}, 1),
		files:    make(map[string]*diskImage),
		sequence: make(map[string]uint64),
		failNext: make(map[DiskOperation]int),
		tearNext: -1,
	}
	d.queue <- struct{}{}
	return d
}

func (d *Disk) Open(ctx context.Context, name string, options platform.OpenOptions) (platform.File, error) {
	if err := validatePath(name); err != nil {
		return nil, err
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	id, release, err := d.begin(ctx, DiskOpen, name, 0, d.config.OpenLatency)
	if err != nil {
		return nil, err
	}
	defer release()

	d.mu.Lock()
	defer d.mu.Unlock()
	image, exists := d.files[name]
	if exists && options.Create && options.Exclusive {
		d.trace(DiskOpen, name, "exists", 0, id)
		return nil, platform.ErrAlreadyExists
	}
	if !exists {
		if !options.Create {
			d.trace(DiskOpen, name, "not_found", 0, id)
			return nil, platform.ErrNotFound
		}
		image = &diskImage{}
		d.files[name] = image
	}
	image.opens++
	if d.config.PowerLossFaults {
		// A file is never opened under NoCorruption, exactly as FoundationDB
		// draws randomInt(1, 3): the mode a page draws is bounded by this one,
		// and a file that could never corrupt anything would make that draw
		// meaningless.
		image.killMode = DropOnly + KillMode(d.powerLossRandom().Intn(
			fmt.Sprintf("%s/%s/open/%d/kill-mode", d.id, name, image.opens), 2))
	}
	if options.Truncate {
		image.volatile = nil
		d.recordPendingLocked(image, pendingOp{kind: pendingTruncate, id: id})
	}
	handle := &file{disk: d, image: image, name: name, epoch: d.epoch}
	d.trace(DiskOpen, name, "ok", 0, id)
	return handle, nil
}

func (d *Disk) Remove(ctx context.Context, name string) error {
	if err := validatePath(name); err != nil {
		return err
	}
	id, release, err := d.begin(ctx, DiskRemove, name, 0, d.config.MetadataLatency)
	if err != nil {
		return err
	}
	defer release()
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.files[name]; !exists {
		d.trace(DiskRemove, name, "not_found", 0, id)
		return platform.ErrNotFound
	}
	delete(d.files, name)
	d.trace(DiskRemove, name, "ok", 0, id)
	return nil
}

func (d *Disk) Rename(ctx context.Context, oldName, newName string) error {
	if err := validatePath(oldName); err != nil {
		return err
	}
	if err := validatePath(newName); err != nil {
		return err
	}
	resource := oldName + "->" + newName
	id, release, err := d.begin(ctx, DiskRename, resource, 0, d.config.MetadataLatency)
	if err != nil {
		return err
	}
	defer release()
	d.mu.Lock()
	defer d.mu.Unlock()
	image, exists := d.files[oldName]
	if !exists {
		d.trace(DiskRename, resource, "not_found", 0, id)
		return platform.ErrNotFound
	}
	if oldName != newName {
		d.files[newName] = image
		delete(d.files, oldName)
	}
	d.trace(DiskRename, resource, "ok", 0, id)
	return nil
}

func (d *Disk) SyncNamespace(ctx context.Context) error {
	id, release, err := d.begin(ctx, DiskSyncNamespace, "", 0, d.config.MetadataLatency)
	if err != nil {
		return err
	}
	defer release()
	// Successful namespace operations are already durable in this adapter.
	d.trace(DiskSyncNamespace, "", "ok", 0, id)
	return nil
}

func (d *Disk) List(ctx context.Context, prefix string) ([]string, error) {
	if prefix != "" {
		if err := validatePath(prefix); err != nil {
			return nil, err
		}
	}
	id, release, err := d.begin(ctx, DiskList, prefix, 0, d.config.MetadataLatency)
	if err != nil {
		return nil, err
	}
	defer release()
	d.mu.Lock()
	defer d.mu.Unlock()
	names := make([]string, 0)
	for name := range d.files {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	d.trace(DiskList, prefix, "ok", len(names), id)
	return names, nil
}

func (d *Disk) FailNext(operation DiskOperation, count int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failNext[operation] += max(count, 0)
}

// TearNextWrite makes the next write persist only its first keep bytes and
// return ErrInjectedFault. A negative keep value is treated as zero.
func (d *Disk) TearNextWrite(keep int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tearNext = max(keep, 0)
}

func (d *Disk) Fail() {
	d.mu.Lock()
	d.failed = true
	d.mu.Unlock()
	d.runtime.trace.record(Event{Kind: "disk", Resource: d.id, Operation: "fail", Outcome: "ok"})
}

func (d *Disk) Recover() {
	d.mu.Lock()
	d.failed = false
	d.mu.Unlock()
	d.runtime.trace.record(Event{Kind: "disk", Resource: d.id, Operation: "recover", Outcome: "ok"})
}

func (d *Disk) PowerLoss(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-d.queue:
	}
	defer func() { d.queue <- struct{}{} }()
	d.mu.Lock()
	defer d.mu.Unlock()
	// Resolution traces one event per modification, so it walks the files in a
	// stable order rather than the map's.
	for _, name := range slices.Sorted(maps.Keys(d.files)) {
		image := d.files[name]
		if !image.durableExists {
			// A file with nothing durable behind it was never renamed into
			// place, so the power loss takes the whole file.
			delete(d.files, name)
			continue
		}
		image.volatile = append([]byte(nil), image.durable...)
		if d.config.PowerLossFaults {
			d.resolvePendingLocked(name, image)
		}
		image.pending = nil
	}
	d.epoch++
	d.runtime.trace.record(Event{Kind: "disk", Resource: d.id, Operation: "power_loss", Outcome: "ok"})
	return nil
}

// powerLossRandom is the choice source for everything a power loss resolves.
// Its ids name the disk, the file and the modification's own sequence, so a
// choice made for one file cannot move the choices made for another.
func (d *Disk) powerLossRandom() Random { return d.runtime.Random("sim/disk-power-loss") }

// recordPendingLocked remembers one modification so that a power loss can
// resolve it. A disk that discards everything unsynced keeps no list at all.
func (d *Disk) recordPendingLocked(image *diskImage, op pendingOp) {
	if !d.config.PowerLossFaults {
		return
	}
	image.pending = append(image.pending, op)
}

// syncPersists reports whether this Sync actually makes the file durable.
func (d *Disk) syncPersists(name string, id uint64) bool {
	probability := d.config.SyncDurableProbability
	if probability <= 0 || probability >= 1 {
		return true
	}
	return d.powerLossRandom().Chance(fmt.Sprintf("%s/%s/sync/%d", d.id, name, id), probability)
}

// resolvePendingLocked replays one file's unsynced modifications onto the image
// the last Sync left, resolving each of them through the seeded random. The
// caller has already restored the durable bytes.
func (d *Disk) resolvePendingLocked(name string, image *diskImage) {
	r := d.powerLossRandom()
	for _, op := range image.pending {
		key := fmt.Sprintf("%s/%s/%s/%d", d.id, name, op.kind, op.id)
		var outcome PowerLossOutcome
		switch op.kind {
		case pendingWrite:
			outcome = d.resolveWriteLocked(r, key, image, op)
		default:
			// A truncate or a punch is one metadata change: it either reached
			// the device or it did not, which is FoundationDB's coin flip for a
			// non-durable truncate.
			if r.Chance(key+"/apply", 0.5) {
				if op.kind == pendingTruncate {
					image.volatile = resizeImage(image.volatile, op.length)
				} else {
					size := int64(len(image.volatile))
					clear(image.volatile[min(op.offset, size):min(op.offset+op.length, size)])
				}
				outcome = PowerLossApplied
			} else {
				outcome = PowerLossDropped
			}
		}
		d.runtime.trace.record(Event{Kind: "disk", Resource: d.id + "/" + name,
			Operation: "power_loss_" + string(op.kind), Outcome: string(outcome),
			Bytes: int(op.length), LocalID: op.id})
	}
}

// resolveWriteLocked resolves one write the way FoundationDB's
// AsyncFileNonDurable does: a kill mode per device page, and within a page a
// sector that is either written, dropped, or written with part of it replaced
// by garbage. A write whose sectors disagree is the torn record a reader has to
// refuse rather than accept as data.
func (d *Disk) resolveWriteLocked(r Random, key string, image *diskImage, op pendingOp) PowerLossOutcome {
	applied, lost, garbled := false, false, false
	for written := int64(0); written < op.length; {
		pageLength := min(op.length-written, diskPageBytes-(op.offset+written)%diskPageBytes)
		// A page draws a mode no worse than the file's, so a DropOnly file
		// never garbles and any file may leave a page untouched.
		pageKill := KillMode(r.Intn(fmt.Sprintf("%s/page/%d", key, written), int(image.killMode)+1))
		for inPage := int64(0); inPage < pageLength; {
			at := written + inPage
			sectorLength := min(pageLength-inPage, diskSectorBytes-(op.offset+at)%diskSectorBytes)
			sector := fmt.Sprintf("%s/sector/%d", key, at)
			data := op.data[at : at+sectorLength]
			switch {
			case pageKill == NoCorruption || (pageKill == FullCorruption && r.Chance(sector+"/intact", 0.25)):
				image.volatile = writeInto(image.volatile, op.offset+at, data)
				applied = true
			case pageKill == FullCorruption && r.Chance(sector+"/corrupt", 0.66667):
				// The part that did not reach the device is the sector's tail
				// (side zero), its head (side one), or all of it (side two).
				side := r.Intn(sector+"/side", 3)
				goodStart, goodEnd := int64(0), sectorLength
				badStart, badEnd := int64(0), sectorLength
				switch side {
				case 0:
					goodEnd = int64(r.Intn(sector+"/split", int(sectorLength)))
					badStart = goodEnd
				case 1:
					badEnd = int64(r.Intn(sector+"/split", int(sectorLength)))
					goodStart = badEnd
				default:
					goodEnd = 0
				}
				garbage := side == 2 || r.Chance(sector+"/garbage", 0.5)
				if garbage && badStart != badEnd {
					bad := append([]byte(nil), data...)
					fillGarbage(r, sector+"/bytes", bad[badStart:badEnd])
					image.volatile = writeInto(image.volatile, op.offset+at, bad)
					garbled = true
				} else if goodStart != goodEnd {
					image.volatile = writeInto(image.volatile, op.offset+at+goodStart, data[goodStart:goodEnd])
					applied = true
					lost = true
				} else {
					lost = true
				}
			default:
				lost = true
			}
			inPage += sectorLength
		}
		written += pageLength
	}
	switch {
	case garbled:
		return PowerLossGarbled
	case applied && lost:
		return PowerLossTruncated
	case lost:
		return PowerLossDropped
	default:
		return PowerLossApplied
	}
}

// fillGarbage overwrites a sector's lost bytes with bytes nobody wrote, drawn
// from the same seeded source as every other choice.
func fillGarbage(r Random, id string, bad []byte) {
	for i := 0; i < len(bad); i += 8 {
		var word [8]byte
		binary.LittleEndian.PutUint64(word[:], r.Uint64(fmt.Sprintf("%s/%d", id, i)))
		copy(bad[i:], word[:min(8, len(bad)-i)])
	}
}

// writeInto applies one extent to an image, extending it with zeroes when the
// extent starts past its end.
func writeInto(image []byte, offset int64, data []byte) []byte {
	end := int(offset) + len(data)
	if end > len(image) {
		image = append(image, make([]byte, end-len(image))...)
	}
	copy(image[int(offset):end], data)
	return image
}

func resizeImage(image []byte, size int64) []byte {
	if size <= int64(len(image)) {
		return image[:size]
	}
	return append(image, make([]byte, int(size)-len(image))...)
}

func (d *Disk) Destroy(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-d.queue:
	}
	defer func() { d.queue <- struct{}{} }()
	d.mu.Lock()
	d.files = make(map[string]*diskImage)
	d.epoch++
	d.mu.Unlock()
	d.runtime.trace.record(Event{Kind: "disk", Resource: d.id, Operation: "destroy", Outcome: "ok"})
	return nil
}

func (d *Disk) begin(ctx context.Context, operation DiskOperation, resource string, bytes int, latency time.Duration) (uint64, func(), error) {
	if err := context.Cause(ctx); err != nil {
		return 0, nil, err
	}
	resourceID := fmt.Sprintf("disk/%q/%s/%q", d.id, operation, resource)
	if err := d.runtime.Admit(ctx, resourceID); err != nil {
		return 0, nil, err
	}
	d.mu.Lock()
	key := string(operation) + "/" + resource
	d.sequence[key]++
	id := d.sequence[key]
	d.mu.Unlock()
	if d.runtime.wait != nil {
		// Choose queue arrival before competing operations can acquire the
		// shared disk token. Completion timing alone cannot order that choice.
		if err := d.runtime.delay(ctx, fmt.Sprintf("%s/admit/%d", resourceID, id), 0, 0, 0); err != nil {
			return 0, nil, err
		}
	}
	select {
	case <-ctx.Done():
		return 0, nil, context.Cause(ctx)
	case <-d.queue:
	}
	release := func() { d.queue <- struct{}{} }
	if err := context.Cause(ctx); err != nil {
		release()
		return 0, nil, err
	}

	d.mu.Lock()
	failed := d.failed
	injected := d.failNext[operation] > 0
	if injected {
		d.failNext[operation]--
	}
	d.mu.Unlock()
	if failed {
		release()
		d.trace(operation, resource, "disk_failed", bytes, id)
		return 0, nil, platform.ErrDiskFailed
	}
	if injected {
		release()
		d.trace(operation, resource, "injected_fault", bytes, id)
		return 0, nil, platform.ErrInjectedFault
	}
	if err := d.runtime.ioDelay(ctx, fmt.Sprintf("disk/%q/%s/%q/%d", d.id, operation, resource, id), operationLatency(latency, bytes, d.config.BytesPerSecond)); err != nil {
		release()
		d.trace(operation, resource, "canceled", bytes, id)
		return 0, nil, err
	}
	d.mu.Lock()
	failed = d.failed
	d.mu.Unlock()
	if failed {
		release()
		d.trace(operation, resource, "disk_failed", bytes, id)
		return 0, nil, platform.ErrDiskFailed
	}
	return id, release, nil
}

func (d *Disk) trace(operation DiskOperation, resource, outcome string, bytes int, id uint64) {
	d.runtime.trace.record(Event{
		Kind:      "disk",
		Resource:  d.id + "/" + resource,
		Operation: string(operation),
		Outcome:   outcome,
		Bytes:     bytes,
		LocalID:   id,
	})
}

type file struct {
	disk   *Disk
	image  *diskImage
	name   string
	epoch  uint64
	closed bool
}

func (f *file) ReadAt(ctx context.Context, destination []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, platform.ErrInvalidRange
	}
	id, release, err := f.disk.begin(ctx, DiskRead, f.name, len(destination), f.disk.config.ReadLatency)
	if err != nil {
		return 0, err
	}
	defer release()
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if err := f.validLocked(); err != nil {
		return 0, err
	}
	if offset >= int64(len(f.image.volatile)) {
		f.disk.trace(DiskRead, f.name, "eof", 0, id)
		return 0, io.EOF
	}
	n := copy(destination, f.image.volatile[offset:])
	f.disk.trace(DiskRead, f.name, "ok", n, id)
	if n < len(destination) {
		return n, io.EOF
	}
	return n, nil
}

func (f *file) WriteAt(ctx context.Context, source []byte, offset int64) (int, error) {
	if offset < 0 || len(source) > 0 &&
		(offset > int64(maxInt()) || len(source) > maxInt()-int(offset)) {
		return 0, platform.ErrInvalidRange
	}
	id, release, err := f.disk.begin(ctx, DiskWrite, f.name, len(source), f.disk.config.WriteLatency)
	if err != nil {
		return 0, err
	}
	defer release()
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if err := f.validLocked(); err != nil {
		return 0, err
	}
	write := source
	torn := false
	if f.disk.tearNext >= 0 {
		keep := min(f.disk.tearNext, len(source))
		write = source[:keep]
		f.disk.tearNext = -1
		torn = true
	}
	if len(write) > 0 {
		f.image.volatile = writeInto(f.image.volatile, offset, write)
		f.disk.recordPendingLocked(f.image, pendingOp{kind: pendingWrite, offset: offset,
			length: int64(len(write)), data: append([]byte(nil), write...), id: id})
	}
	if torn {
		f.disk.trace(DiskWrite, f.name, "torn", len(write), id)
		return len(write), platform.ErrInjectedFault
	}
	f.disk.trace(DiskWrite, f.name, "ok", len(write), id)
	return len(write), nil
}

func (f *file) Truncate(ctx context.Context, size int64) error {
	if size < 0 || size >= int64(maxInt()) {
		return platform.ErrInvalidRange
	}
	id, release, err := f.disk.begin(ctx, DiskTruncate, f.name, 0, f.disk.config.MetadataLatency)
	if err != nil {
		return err
	}
	defer release()
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if err := f.validLocked(); err != nil {
		return err
	}
	if size <= int64(len(f.image.volatile)) {
		f.image.volatile = f.image.volatile[:size]
	} else {
		f.image.volatile = append(f.image.volatile, make([]byte, int(size)-len(f.image.volatile))...)
	}
	f.disk.recordPendingLocked(f.image, pendingOp{kind: pendingTruncate, length: size, id: id})
	f.disk.trace(DiskTruncate, f.name, "ok", 0, id)
	return nil
}

func (f *file) PunchHole(ctx context.Context, offset, length int64) error {
	if offset < 0 || length <= 0 || offset > int64(maxInt())-length {
		return platform.ErrInvalidRange
	}
	id, release, err := f.disk.begin(ctx, DiskPunchHole, f.name, 0, f.disk.config.MetadataLatency)
	if err != nil {
		return err
	}
	defer release()
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if err := f.validLocked(); err != nil {
		return err
	}
	size := int64(len(f.image.volatile))
	clear(f.image.volatile[min(offset, size):min(offset+length, size)])
	f.disk.recordPendingLocked(f.image, pendingOp{kind: pendingPunch, offset: offset, length: length, id: id})
	f.disk.trace(DiskPunchHole, f.name, "ok", 0, id)
	return nil
}

func (f *file) Sync(ctx context.Context) error {
	id, release, err := f.disk.begin(ctx, DiskSync, f.name, 0, f.disk.config.SyncLatency)
	if err != nil {
		return err
	}
	defer release()
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if err := f.validLocked(); err != nil {
		return err
	}
	if !f.disk.syncPersists(f.name, id) {
		// The device acknowledged a flush it did not perform. Everything stays
		// pending, so a power loss can still lose or garble it.
		f.disk.trace(DiskSync, f.name, "not_persisted", 0, id)
		return nil
	}
	f.image.durable = append([]byte(nil), f.image.volatile...)
	f.image.durableExists = true
	f.image.pending = nil
	f.disk.trace(DiskSync, f.name, "ok", 0, id)
	return nil
}

func (f *file) Size(ctx context.Context) (int64, error) {
	id, release, err := f.disk.begin(ctx, DiskSize, f.name, 0, f.disk.config.MetadataLatency)
	if err != nil {
		return 0, err
	}
	defer release()
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if err := f.validLocked(); err != nil {
		return 0, err
	}
	size := int64(len(f.image.volatile))
	f.disk.trace(DiskSize, f.name, "ok", 0, id)
	return size, nil
}

func (f *file) Close() error {
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	return nil
}

func (f *file) validLocked() error {
	if f.closed {
		return platform.ErrClosed
	}
	if f.epoch != f.disk.epoch {
		return platform.ErrStaleHandle
	}
	return nil
}

var _ platform.Disk = (*Disk)(nil)
var _ platform.File = (*file)(nil)
