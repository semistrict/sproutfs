package sim

import (
	"bytes"
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

	"github.com/semistrict/sproutfs/platform"
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
	DiskSpace         DiskOperation = "space"
	DiskAllocated     DiskOperation = "allocated"
	DiskDeviceWrites  DiskOperation = "device_writes"
)

// SpaceConfig is the filesystem a simulated disk is on: how large it is, what
// other writers on it hold, and how fast they change that.
type SpaceConfig struct {
	// TotalBytes is the filesystem's size. Zero draws it from the seed, between
	// 5 GB and 105 GB, and draws what other writers hold with it: they leave at
	// least 5 GB or 7.5 % of it free, whichever is more, as FoundationDB's
	// simulator does.
	TotalBytes int64
	// OutsideBytes is what other writers hold when the disk is made. It is read
	// only when TotalBytes is set.
	OutsideBytes int64
	// DriftBytesPerSecond is how far other writers move what they hold, up or
	// down, per simulated second between two readings of the space. One reading
	// moves it by at most five seconds' worth. Zero holds it still.
	DriftBytesPerSecond int64
}

// SpaceUsage is the true state of a simulated filesystem, which a test checks a
// limiter's readings against.
type SpaceUsage struct {
	// TotalBytes is the filesystem's size, OutsideBytes what other writers
	// hold, and HostBytes what the files of this disk hold.
	TotalBytes, OutsideBytes, HostBytes int64
}

// FreeBytes is the space neither this disk's files nor other writers hold.
func (u SpaceUsage) FreeBytes() int64 { return max(u.TotalBytes-u.OutsideBytes-u.HostBytes, 0) }

// The space a filesystem is drawn with when a test names none, and the longest
// gap between two readings that other writers' drift is measured over.
const (
	drawnTotalMinimum   = 5_000_000_000
	drawnTotalRange     = 100_000_000_000
	drawnFreeMinimum    = 5_000_000_000
	drawnFreeShare      = 0.075
	maximumDriftSeconds = 5
	// fastDrift is how much faster other writers move when the fast drift
	// fault fires.
	fastDrift = 10
	// deviceWritesJump bounds how far the jump fault moves the device's counter.
	deviceWritesJump = 64 << 30
)

// The fault-injection sites of the space a disk reports and of its device's
// write counter. They are the inputs a disk limiter reads, and it must stay
// safe whatever they do.
const (
	// BuggifySpaceFails fails a reading of the space.
	BuggifySpaceFails = "sim/disk/space-fails"
	// BuggifySpaceInconsistent reports more space available than the
	// filesystem has.
	BuggifySpaceInconsistent = "sim/disk/space-inconsistent"
	// BuggifySpaceLow reports less space available than is free, once.
	BuggifySpaceLow = "sim/disk/space-low"
	// BuggifyOutsideFills has another writer take a share of what is free, for
	// good.
	BuggifyOutsideFills = "sim/disk/outside-fills"
	// BuggifyDriftFast has other writers drift ten times as fast.
	BuggifyDriftFast = "sim/disk/drift-fast"
	// BuggifyDeviceWritesFail fails a reading of the device's write counter.
	BuggifyDeviceWritesFail = "sim/disk/device-writes-fail"
	// BuggifyDeviceWritesJump moves the device's counter forward by up to
	// 64 GiB, for good.
	BuggifyDeviceWritesJump = "sim/disk/device-writes-jump"
	// BuggifyDeviceWritesReset starts the device's counter again at zero, as a
	// replaced device does.
	BuggifyDeviceWritesReset = "sim/disk/device-writes-reset"
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
	// Space is the filesystem the disk is on. A write that needs more than is
	// free fails with platform.ErrNoSpace.
	Space SpaceConfig
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
	volatile      fileBytes
	durable       fileBytes
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
	// total is the filesystem's size and outside what other writers hold on
	// it. spaceRead is when the space was last read, which other writers'
	// drift is measured from, and spaceReads counts the readings, which key
	// its draws.
	total, outside int64
	spaceRead      time.Time
	spaceReads     uint64
	// written is the device's write counter: every byte this disk's files
	// wrote, and every byte AddDeviceWrites added for other writers.
	written uint64
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
		total:    config.Space.TotalBytes,
		outside:  config.Space.OutsideBytes,
	}
	if d.total <= 0 {
		// FoundationDB's simulator draws each machine's disk the same way: a
		// size, and free space of at least 5 GB or 7.5 % of it.
		r := runtime.Random("sim/disk-space")
		d.total = drawnTotalMinimum + int64(r.Float64(id+"/total")*drawnTotalRange)
		share := drawnFreeShare + r.Float64(id+"/free")*(1-drawnFreeShare)
		free := min(d.total, max(drawnFreeMinimum, int64(share*float64(d.total))))
		d.outside = d.total - free
	}
	d.outside = min(max(d.outside, 0), d.total)
	d.spaceRead = runtime.Now()
	d.queue <- struct{}{}
	return d
}

// SetOutsideBytes sets what other writers on the filesystem hold. It is how a
// test fills the disk from outside the host, and empties it again.
func (d *Disk) SetOutsideBytes(bytes int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.outside = min(max(bytes, 0), d.total)
}

// SetSpaceDrift sets how fast other writers drift, in bytes per simulated
// second. Zero holds them still from the next reading on.
func (d *Disk) SetSpaceDrift(bytesPerSecond int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.config.Space.DriftBytesPerSecond = max(bytesPerSecond, 0)
}

// AddDeviceWrites counts bytes another writer wrote to the device.
func (d *Disk) AddDeviceWrites(bytes uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.written += bytes
}

// Usage is the filesystem's true state, without drift or faults.
func (d *Disk) Usage() SpaceUsage {
	d.mu.Lock()
	defer d.mu.Unlock()
	return SpaceUsage{TotalBytes: d.total, OutsideBytes: d.outside, HostBytes: d.hostBytesLocked()}
}

// DeviceWritten is the device's write counter, without faults.
func (d *Disk) DeviceWritten() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.written
}

// hostBytesLocked is what this disk's files hold: their stored pages. A hole
// holds nothing, so a sparse spill file costs only what was written to it.
func (d *Disk) hostBytesLocked() int64 {
	var pages int64
	for _, image := range d.files {
		pages += int64(len(image.volatile.pages))
	}
	return pages * diskPageBytes
}

// Space reports the filesystem. Each reading first lets other writers drift by
// a seeded amount, bounded by the time since the last reading, as
// FoundationDB's simulator moves free space for external processes. They never
// hold less than nothing nor take space this disk's files hold.
func (d *Disk) Space(ctx context.Context) (platform.FilesystemSpace, error) {
	id, release, err := d.begin(ctx, DiskSpace, "", 0, d.config.MetadataLatency)
	if err != nil {
		return platform.FilesystemSpace{}, err
	}
	defer release()
	r := d.runtime
	if r.buggifyHere(BuggifySpaceFails, 0.2) {
		d.trace(DiskSpace, "", "injected_fault", 0, id)
		return platform.FilesystemSpace{}, platform.ErrInjectedFault
	}
	random := r.Random("sim/disk-space")
	key := fmt.Sprintf("%s/%d", d.id, id)
	d.mu.Lock()
	defer d.mu.Unlock()
	host := d.hostBytesLocked()
	now := r.Now()
	elapsed := min(max(now.Sub(d.spaceRead), 0), maximumDriftSeconds*time.Second)
	d.spaceRead = now
	drift := float64(d.config.Space.DriftBytesPerSecond) * elapsed.Seconds()
	if drift > 0 && r.buggifyHere(BuggifyDriftFast, 0.25) {
		drift *= fastDrift
	}
	if drift > 0 {
		d.outside += int64((random.Float64(key+"/drift")*2 - 1) * drift)
	}
	if r.buggifyHere(BuggifyOutsideFills, 0.1) {
		d.outside += int64(random.Float64(key+"/fill") * float64(max(d.total-d.outside-host, 0)))
	}
	d.outside = min(max(d.outside, 0), max(d.total-host, 0))
	free := max(d.total-d.outside-host, 0)
	reported := free
	switch {
	case r.buggifyHere(BuggifySpaceInconsistent, 0.1):
		reported = d.total + 1 + int64(random.Float64(key+"/inconsistent")*float64(d.total))
	case r.buggifyHere(BuggifySpaceLow, 0.2):
		reported = int64(random.Float64(key+"/low") * float64(free))
	}
	d.trace(DiskSpace, "", "ok", 0, id)
	return platform.FilesystemSpace{ID: d.id, Total: uint64(d.total), Available: uint64(reported),
		AllocationUnit: diskPageBytes}, nil
}

// BytesWritten is the device's write counter.
func (d *Disk) BytesWritten(ctx context.Context) (uint64, error) {
	id, release, err := d.begin(ctx, DiskDeviceWrites, "", 0, d.config.MetadataLatency)
	if err != nil {
		return 0, err
	}
	defer release()
	r := d.runtime
	if r.buggifyHere(BuggifyDeviceWritesFail, 0.2) {
		d.trace(DiskDeviceWrites, "", "injected_fault", 0, id)
		return 0, platform.ErrInjectedFault
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case r.buggifyHere(BuggifyDeviceWritesReset, 0.05):
		d.written = 0
	case r.buggifyHere(BuggifyDeviceWritesJump, 0.05):
		d.written += r.Random("sim/disk-space").Uint64(fmt.Sprintf("%s/%d/jump", d.id, id)) % deviceWritesJump
	}
	d.trace(DiskDeviceWrites, "", "ok", 0, id)
	return d.written, nil
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
		image.volatile = fileBytes{}
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
		image.volatile = image.durable.clone()
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
					image.volatile.resize(op.length)
				} else {
					image.volatile.zero(op.offset, op.offset+op.length)
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
				image.volatile.writeAt(op.offset+at, data)
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
					image.volatile.writeAt(op.offset+at, bad)
					garbled = true
				} else if goodStart != goodEnd {
					image.volatile.writeAt(op.offset+at+goodStart, data[goodStart:goodEnd])
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

// fileBytes is a file's contents: its size and the device pages that hold
// data. A page the map lacks reads as zeroes, as a hole does in a sparse file,
// so a file truncated to the size of a pager's whole spill costs nothing until
// it is written. A stored page is never changed; a write stores a changed copy.
// A Sync and a power loss therefore share pages between the volatile and the
// durable image instead of copying the file.
type fileBytes struct {
	size  int64
	pages map[int64][]byte
}

func (b fileBytes) clone() fileBytes {
	return fileBytes{size: b.size, pages: maps.Clone(b.pages)}
}

// readAt copies what the file holds from offset into destination and returns
// how many bytes that was. The caller has checked that offset is before the end.
func (b fileBytes) readAt(destination []byte, offset int64) int {
	n := int(min(int64(len(destination)), b.size-offset))
	for done := 0; done < n; {
		at := offset + int64(done)
		within := at % diskPageBytes
		chunk := destination[done:min(n, done+int(diskPageBytes-within))]
		if page, ok := b.pages[at/diskPageBytes]; ok {
			copy(chunk, page[within:])
		} else {
			clear(chunk)
		}
		done += len(chunk)
	}
	return n
}

// writeAt stores data at offset, extending the file with zeroes when offset is
// past its end.
func (b *fileBytes) writeAt(offset int64, data []byte) {
	if b.pages == nil {
		b.pages = make(map[int64][]byte)
	}
	b.size = max(b.size, offset+int64(len(data)))
	for done := 0; done < len(data); {
		at := offset + int64(done)
		page := make([]byte, diskPageBytes)
		copy(page, b.pages[at/diskPageBytes])
		done += copy(page[at%diskPageBytes:], data[done:])
		b.pages[at/diskPageBytes] = page
	}
}

// newPages counts the device pages a write of length bytes at offset would
// store that the file does not hold yet.
func (b fileBytes) newPages(offset, length int64) int64 {
	if length <= 0 {
		return 0
	}
	var count int64
	for index := offset / diskPageBytes; index <= (offset+length-1)/diskPageBytes; index++ {
		if _, ok := b.pages[index]; !ok {
			count++
		}
	}
	return count
}

// resize sets the file's size. What a shrink cuts off is zeroed first, so a
// later grow reads zeroes there rather than the old bytes.
func (b *fileBytes) resize(size int64) {
	b.zero(size, b.size)
	b.size = size
}

// zero makes [start, end) of the file read as zeroes and drops every page the
// range covers whole. It visits the range's pages or the stored ones, whichever
// are fewer, so punching or truncating a large sparse file stays cheap.
func (b *fileBytes) zero(start, end int64) {
	end = min(end, b.size)
	if start >= end {
		return
	}
	first, last := start/diskPageBytes, (end-1)/diskPageBytes
	if last-first >= int64(len(b.pages)) {
		for index := range b.pages {
			if index >= first && index <= last {
				b.zeroPage(index, start, end)
			}
		}
		return
	}
	for index := first; index <= last; index++ {
		b.zeroPage(index, start, end)
	}
}

// zeroPage clears the part of one stored page that [start, end) covers.
func (b *fileBytes) zeroPage(index, start, end int64) {
	page, ok := b.pages[index]
	if !ok {
		return
	}
	base := index * diskPageBytes
	from, to := max(start, base)-base, min(end, base+diskPageBytes)-base
	if from == 0 && to == diskPageBytes {
		delete(b.pages, index)
		return
	}
	page = bytes.Clone(page)
	clear(page[from:to])
	b.pages[index] = page
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
	if offset >= f.image.volatile.size {
		f.disk.trace(DiskRead, f.name, "eof", 0, id)
		return 0, io.EOF
	}
	n := f.image.volatile.readAt(destination, offset)
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
	if needed := f.image.volatile.newPages(offset, int64(len(source))) * diskPageBytes; needed > 0 &&
		needed > f.disk.total-f.disk.outside-f.disk.hostBytesLocked() {
		f.disk.trace(DiskWrite, f.name, "no_space", 0, id)
		return 0, platform.ErrNoSpace
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
		f.disk.written += uint64(len(write))
		f.image.volatile.writeAt(offset, write)
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
	f.image.volatile.resize(size)
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
	f.image.volatile.zero(offset, offset+length)
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
	f.image.durable = f.image.volatile.clone()
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
	size := f.image.volatile.size
	f.disk.trace(DiskSize, f.name, "ok", 0, id)
	return size, nil
}

// Allocated is what the file holds: its stored pages, not its size.
func (f *file) Allocated(ctx context.Context) (int64, error) {
	id, release, err := f.disk.begin(ctx, DiskAllocated, f.name, 0, f.disk.config.MetadataLatency)
	if err != nil {
		return 0, err
	}
	defer release()
	f.disk.mu.Lock()
	defer f.disk.mu.Unlock()
	if err := f.validLocked(); err != nil {
		return 0, err
	}
	allocated := int64(len(f.image.volatile.pages)) * diskPageBytes
	f.disk.trace(DiskAllocated, f.name, "ok", 0, id)
	return allocated, nil
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
var _ platform.DiskSpace = (*Disk)(nil)
var _ platform.DeviceWrites = (*Disk)(nil)
var _ platform.File = (*file)(nil)
var _ platform.FileAllocation = (*file)(nil)
