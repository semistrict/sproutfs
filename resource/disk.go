package resource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/bits"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

// The disk limiter decides how much of the node's disk the host may use, for
// everything the host writes. The users that cannot give space back are
// promised their space: the spill files, VMM staging and staged images. The
// cache takes what is left, and gives it back a region at a time when the
// limiter says its share fell. The limiter also keeps the cache's write budget,
// measured from the device's own counter.

var (
	// ErrDiskPromises reports promises the disk cannot keep under its goals.
	ErrDiskPromises = errors.New("the disk cannot keep the host's promises")
	// ErrDiskReading reports a reading of the filesystem or the device that
	// the limiter refused.
	ErrDiskReading = errors.New("unusable disk reading")
)

// The defaults a limiter takes where its configuration names none.
const (
	// DefaultDiskRegion is the cache's disk region: the hysteresis between
	// being told to shrink and stopping.
	DefaultDiskRegion = 64 << 20
	// DefaultDiskBandPercent is the band's share of the headroom above the
	// floor, and DefaultDiskMaxBand its cap.
	DefaultDiskBandPercent = 20
	DefaultDiskMaxBand     = 4 << 30
	// DefaultDiskInterval is how often the limiter reads the disk on its own.
	DefaultDiskInterval = 10 * time.Second
	// DefaultDiskSmoothing is the time the smoothed readings take to move all
	// but a 1/e of the way to a new reading. A reading every ten seconds moves
	// them about a seventh of the way, so one odd reading cannot empty the
	// cache.
	DefaultDiskSmoothing = time.Minute
)

// DiskWritePriorities is how many priorities a cache's writes have. Priority 0
// is the lowest. When writes run over budget, the lowest are refused first.
const DiskWritePriorities = 4

// maxDiskBytes bounds every byte count a configuration names, so that a sum
// or a product of two of them stays inside an int64.
const maxDiskBytes = 1 << 60

// day is the period a write budget is averaged over.
const day = 24 * time.Hour

// The probes a disk limiter marks.
const (
	// ProbeDiskShrink marks a cache told that its share fell.
	ProbeDiskShrink = "resource/disk-shrink"
	// ProbeDiskHysteresisHeld marks a cache above its stop mark and inside its
	// share, which is not told to shrink.
	ProbeDiskHysteresisHeld = "resource/disk-hysteresis-held"
	// ProbeDiskUnready marks a reading under which the promises do not fit.
	ProbeDiskUnready = "resource/disk-unready"
	// ProbeDiskLowRefused marks a write refused for its priority that a write
	// of the highest priority would have been admitted for.
	ProbeDiskLowRefused = "resource/disk-low-refused"
)

// DiskProbes is every probe a disk limiter marks.
var DiskProbes = []string{ProbeDiskShrink, ProbeDiskHysteresisHeld, ProbeDiskUnready, ProbeDiskLowRefused}

// DiskGoal is what the limiter keeps. Any of the three may be set, and at
// least one must be. The free-space goals give the floor: the larger of
// FreeBytes and FreePercent of the filesystem. UsedBytes caps what the host
// may hold. The cache's share is the smaller of what each implies.
type DiskGoal struct {
	// FreeBytes is the least the filesystem keeps free.
	FreeBytes int64
	// FreePercent is the least share of the filesystem it keeps free, 0 to 99.
	FreePercent int64
	// UsedBytes is the most the host holds on it.
	UsedBytes int64
}

// Validate refuses a goal that names nothing, or names something impossible.
func (g DiskGoal) Validate() error {
	var errs []error
	if g.FreeBytes < 0 || g.FreeBytes > maxDiskBytes {
		errs = append(errs, fmt.Errorf("a free-space goal of %d bytes", g.FreeBytes))
	}
	if g.FreePercent < 0 || g.FreePercent > 99 {
		errs = append(errs, fmt.Errorf("a free-space goal of %d%%, want 0 to 99", g.FreePercent))
	}
	if g.UsedBytes < 0 || g.UsedBytes > maxDiskBytes {
		errs = append(errs, fmt.Errorf("a used-space goal of %d bytes", g.UsedBytes))
	}
	if g.FreeBytes == 0 && g.FreePercent == 0 && g.UsedBytes == 0 {
		errs = append(errs, errors.New("no goal for the disk"))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", ErrInvalid, errors.Join(errs...))
	}
	return nil
}

func (g DiskGoal) String() string {
	var goals []string
	if g.FreeBytes > 0 {
		goals = append(goals, fmt.Sprintf("free %d bytes", g.FreeBytes))
	}
	if g.FreePercent > 0 {
		goals = append(goals, fmt.Sprintf("free %d%%", g.FreePercent))
	}
	if g.UsedBytes > 0 {
		goals = append(goals, fmt.Sprintf("use %d bytes", g.UsedBytes))
	}
	return strings.Join(goals, ", ")
}

// DiskBinding names the goal that sets the cache's share.
type DiskBinding string

const (
	BindingFreeBytes   DiskBinding = "free-bytes"
	BindingFreePercent DiskBinding = "free-percent"
	BindingUsedBytes   DiskBinding = "used-bytes"
	// BindingFilesystem is a limiter with no free-space goal whose share the
	// filesystem's own size sets.
	BindingFilesystem DiskBinding = "filesystem"
)

// DiskUser is a user of the disk that cannot give space back: a spill file,
// VMM staging, a staged image. It is counted at its promise, whatever it has
// allocated yet.
type DiskUser struct {
	Name string
	// Promised is the bytes the user may come to hold. The limiter reads it at
	// every reading.
	Promised func() int64
	// Allocated is what the user holds now. Nil counts nothing allocated, which
	// counts what it does hold twice: once in the filesystem's free space and
	// once in its promise. That is safe, and only makes the cache smaller.
	Allocated func(context.Context) (int64, error)
}

// DiskCache is the one user of the disk that gives space back.
type DiskCache interface {
	// Held is the bytes the cache holds on the disk now.
	Held() int64
	// Shrink tells the cache its share fell. It gives regions back until it
	// holds at most target. It must not wait on the limiter.
	Shrink(target int64)
}

// WriteBudget is what the cache may write to the device: an average of
// BytesPerDay, and at most BurstBytes ahead of it. Zero is no budget.
type WriteBudget struct {
	BytesPerDay, BurstBytes int64
}

// Validate refuses a budget that cannot be kept.
func (b WriteBudget) Validate() error {
	if b == (WriteBudget{}) {
		return nil
	}
	if b.BytesPerDay <= 0 || b.BytesPerDay > maxDiskBytes || b.BurstBytes <= 0 || b.BurstBytes > maxDiskBytes {
		return fmt.Errorf("%w: a write budget of %d bytes a day and a burst of %d", ErrInvalid,
			b.BytesPerDay, b.BurstBytes)
	}
	return nil
}

// DiskLimiterConfig is one host's disk limiter.
type DiskLimiterConfig struct {
	// Space reads the filesystem the host writes to.
	Space platform.DiskSpace
	Goal  DiskGoal
	Users []DiskUser
	// CacheFile reads what the cache's file holds on the disk, and counts it as
	// the cache's while no cache is registered. A cache read back after a
	// restart holds its regions before the cache is made, and its share must
	// count them as its own, not as another writer's. Nil counts nothing.
	CacheFile func(context.Context) (int64, error)
	// Region is the hysteresis: a cache over its share is told to stop one
	// region below it. Zero is DefaultDiskRegion.
	Region int64
	// BandPercent and MaxBandBytes set the band above the floor in which the
	// cache's share falls before the floor is reached: BandPercent of the
	// headroom between the floor and what is free, at most MaxBandBytes. Zero
	// is the default for each.
	BandPercent, MaxBandBytes int64
	// ReserveBytes is what the cache leaves free above the floor for
	// promises not yet made: this host's, and those of another host on the
	// same filesystem, which only the filesystem's free space shows. Without
	// it a cache that has filled the disk to the floor leaves another host
	// whose cache is empty no room to promise anything, and its own goals
	// never tell it to give space back. A promise may take the reserve; the
	// cache then gives back what restores it. Zero leaves none.
	ReserveBytes int64
	// Interval is how often the limiter reads the disk on its own, and
	// Smoothing how slowly its smoothed readings follow. Zero is the default
	// for each.
	Interval, Smoothing time.Duration
	// Writes is the cache's write budget, and Device the counter it is
	// measured by. Device is read only when there is a budget.
	Writes WriteBudget
	Device platform.DeviceWrites
	Clock  platform.Clock
}

// DiskPromise is one user's promise as the last reading saw it.
type DiskPromise struct {
	Name           string
	PromisedBytes  int64
	AllocatedBytes int64
}

// DiskWritesStatus is the write budget as the last reading saw it.
type DiskWritesStatus struct {
	BytesPerDay, BurstBytes int64
	// WrittenBytes is what the device wrote since the limiter started, and
	// AdmittedBytes what the cache was admitted to write.
	WrittenBytes, AdmittedBytes uint64
	// LeftBytes is what the budget has left. It is below zero after the
	// device wrote more than the budget allowed.
	LeftBytes int64
	// Refused counts the writes refused at each priority.
	Refused [DiskWritePriorities]uint64
}

// DiskStatus is what the limiter chose and why.
type DiskStatus struct {
	Goal    DiskGoal
	Binding DiskBinding
	// TotalBytes and AvailableBytes are the last raw reading of the
	// filesystem. The smoothed ones are what the limiter acts on.
	TotalBytes, AvailableBytes        int64
	SmoothTotalBytes, SmoothFreeBytes int64
	FloorBytes, BandBytes             int64
	// ReserveBytes is what the cache leaves free above the floor for
	// promises.
	ReserveBytes                    int64
	Promises                        []DiskPromise
	PromisedBytes                   int64
	CacheHeldBytes, CacheShareBytes int64
	// Unready is why the host's promises do not fit, empty when they do.
	Unready string
	// ReadError is why the last reading was refused, empty when it was not.
	ReadError string
	Writes    DiskWritesStatus
}

// DiskLimiter is one host's disk limiter. It is safe for concurrent use.
type DiskLimiter struct {
	ctx    context.Context
	config DiskLimiterConfig
	clock  platform.Clock
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once

	// refreshing admits one reading at a time. A reading waits on the disk, so
	// the next one waits on a channel and can be cancelled.
	refreshing *ctxsync.Mutex

	mu    sync.Mutex
	cache DiskCache
	// room is the smoothed space the host could hold: what is free and what
	// the host holds. total is the smoothed size of the filesystem.
	room, total smoother
	status      DiskStatus
	// hard is the cache's share with no band: negative when even an empty
	// cache does not fit.
	hard int64
	// The write budget: what is left of it, the fraction of a byte credited
	// but not yet whole (in byte-nanoseconds over a day), when it was last
	// credited, the device's last count, and what was admitted that the
	// device has not counted yet.
	tokens     int64
	carry      uint64
	refilledAt time.Time
	counting   bool
	counted    uint64
	pending    int64
}

// NewDiskLimiter reads the disk once and starts reading it every interval
// until ctx ends or the limiter is closed. A first reading that fails fails
// the limiter. A host whose promises do not fit is built, and reports it in
// Ready.
func NewDiskLimiter(ctx context.Context, config DiskLimiterConfig) (*DiskLimiter, error) {
	if config.Space == nil {
		return nil, fmt.Errorf("%w: a disk limiter needs the filesystem's space", ErrInvalid)
	}
	if err := config.Goal.Validate(); err != nil {
		return nil, err
	}
	if err := config.Writes.Validate(); err != nil {
		return nil, err
	}
	if config.Writes != (WriteBudget{}) && config.Device == nil {
		return nil, fmt.Errorf("%w: a write budget needs the device's write counter", ErrInvalid)
	}
	for _, user := range config.Users {
		if user.Name == "" || user.Promised == nil {
			return nil, fmt.Errorf("%w: a disk user needs a name and a promise", ErrInvalid)
		}
	}
	if config.Region < 0 || config.BandPercent < 0 || config.BandPercent > 100 || config.MaxBandBytes < 0 ||
		config.ReserveBytes < 0 || config.ReserveBytes > maxDiskBytes || config.Interval < 0 || config.Smoothing < 0 {
		return nil, fmt.Errorf("%w: a disk limiter's region, band, reserve, interval and smoothing are not negative, "+
			"and its band is at most 100%%", ErrInvalid)
	}
	config.Region = orDefault(config.Region, DefaultDiskRegion)
	config.BandPercent = orDefault(config.BandPercent, DefaultDiskBandPercent)
	config.MaxBandBytes = orDefault(config.MaxBandBytes, DefaultDiskMaxBand)
	config.Interval = orDefault(config.Interval, DefaultDiskInterval)
	config.Smoothing = orDefault(config.Smoothing, DefaultDiskSmoothing)
	clock := platform.ClockOr(config.Clock)
	l := &DiskLimiter{ctx: ctx, config: config, clock: clock, stop: make(chan struct{}), done: make(chan struct{}), refreshing: ctxsync.NewMutex(),
		room: smoother{fold: config.Smoothing}, total: smoother{fold: config.Smoothing},
		tokens: config.Writes.BurstBytes, refilledAt: clock.Now()}
	l.status.Goal = config.Goal
	l.status.Writes.BytesPerDay, l.status.Writes.BurstBytes = config.Writes.BytesPerDay, config.Writes.BurstBytes
	if err := l.Refresh(ctx); err != nil {
		return nil, err
	}
	// The ticker is armed here, not in the loop, so the first reading of the
	// timer comes one interval after this one whenever the loop starts.
	go l.loop(l.clock.NewTicker(l.config.Interval))
	return l, nil
}

func orDefault[T int64 | time.Duration](value, fallback T) T {
	if value == 0 {
		return fallback
	}
	return value
}

// Close stops the limiter's own readings. It is safe to call repeatedly.
func (l *DiskLimiter) Close() {
	l.once.Do(func() { close(l.stop) })
	<-l.done
}

func (l *DiskLimiter) loop(ticker platform.Ticker) {
	defer close(l.done)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-l.ctx.Done():
			return
		case <-ticker.C():
			if err := l.Refresh(l.ctx); err != nil {
				slog.WarnContext(l.ctx, "resource: a reading of the disk failed", "error", err)
			}
		}
	}
}

// RegisterCache gives the limiter the cache it shrinks. A host has one cache:
// a second is refused until the first is unregistered.
func (l *DiskLimiter) RegisterCache(cache DiskCache) (func(), error) {
	if cache == nil {
		return nil, fmt.Errorf("%w: no cache", ErrInvalid)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cache != nil {
		return nil, fmt.Errorf("%w: the disk limiter already has a cache", ErrInvalid)
	}
	l.cache = cache
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.cache == cache {
			l.cache = nil
		}
	}, nil
}

// CacheShare is what the cache may hold, as the last reading computed it.
func (l *DiskLimiter) CacheShare() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(l.status.CacheShareBytes, 0)
}

// Capacity is what the cache could hold on this filesystem if nothing else
// wrote to it: the filesystem's size less the free-space floor, the reserve
// and the promises, under the used-space goal. It reads the goals, the size
// and the promises at the last reading, and never the space other writers take
// or the band, so it does not move as the disk fills. A host weighs its disk
// in the membership by it.
func (l *DiskLimiter) Capacity() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(l.capacityLocked(l.config.ReserveBytes), 0)
}

// Feasible is nil while the host's promises would fit under the goals on this
// filesystem with nothing else on it, and why they would not otherwise. Unlike
// Ready it ignores the space other writers hold, which they may give back: a
// host that is not ready can wait for them, and one that is not feasible
// cannot keep its promises on this filesystem at all.
func (l *DiskLimiter) Feasible() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	// A promise may take the reserve, so it is not counted here.
	if capacity := l.capacityLocked(0); capacity < 0 {
		return fmt.Errorf("%w: the host promises %d bytes, and the goals (%s) leave it %d of a filesystem of %d",
			ErrDiskPromises, l.status.PromisedBytes, l.config.Goal, l.status.PromisedBytes+capacity,
			l.status.TotalBytes)
	}
	return nil
}

// capacityLocked is what the goals leave the cache on this filesystem if
// nothing else wrote to it and it left reserve free above the floor, below
// zero when they do not leave the promises.
func (l *DiskLimiter) capacityLocked(reserve int64) int64 {
	goal := l.config.Goal
	total := l.status.TotalBytes
	if sim.Bug(l.ctx, "disklimit-capacity-from-free-space") {
		total = l.status.SmoothFreeBytes + l.status.CacheHeldBytes
		for _, promise := range l.status.Promises {
			total += promise.AllocatedBytes
		}
	}
	capacity := total - max(goal.FreeBytes, fraction(total, goal.FreePercent, 100)) - reserve
	if goal.UsedBytes > 0 {
		capacity = min(capacity, goal.UsedBytes)
	}
	return capacity - l.status.PromisedBytes
}

// Ready is nil while the host's promises fit under the goals with an empty
// cache, and why they do not otherwise.
func (l *DiskLimiter) Ready() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status.Unready != "" {
		return fmt.Errorf("%w: %s", ErrDiskPromises, l.status.Unready)
	}
	return nil
}

// Status is what the limiter chose at its last reading.
func (l *DiskLimiter) Status() DiskStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refillLocked(l.clock.Now())
	status := l.status
	status.Promises = append([]DiskPromise(nil), l.status.Promises...)
	status.Writes.LeftBytes = l.tokens
	return status
}

// Fits reads the disk and refuses a new promise of extra bytes that the goals
// could not keep with an empty cache. It is a refusal, not a reservation: the
// user's own Promised counts it once it is made.
func (l *DiskLimiter) Fits(ctx context.Context, extra int64) error {
	if extra < 0 {
		return fmt.Errorf("%w: a promise of %d bytes", ErrInvalid, extra)
	}
	if err := l.Refresh(ctx); err != nil {
		// The last good reading still says what the host promised.
		slog.WarnContext(ctx, "resource: a reading of the disk failed before a promise", "error", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status.Unready != "" {
		return fmt.Errorf("%w: %s", ErrDiskPromises, l.status.Unready)
	}
	if l.hard < extra {
		return fmt.Errorf("%w: a promise of %d more bytes, and the %s goal leaves %d", ErrDiskPromises,
			extra, l.status.Binding, l.hard)
	}
	return nil
}

// Refresh reads the disk and recomputes the cache's share. A cache over its
// share is told to shrink one region below it. A reading the limiter refuses
// changes nothing it acts on and is reported.
func (l *DiskLimiter) Refresh(ctx context.Context) error {
	if err := l.refreshing.Lock(ctx); err != nil {
		return err
	}
	defer l.refreshing.Unlock()
	var errs []error
	reading, spaceErr := l.readSpace(ctx)
	if spaceErr != nil {
		errs = append(errs, spaceErr)
	}
	promises, allocated, usersErr := l.readUsers(ctx)
	if usersErr != nil {
		errs = append(errs, usersErr)
	}
	var written uint64
	var writesErr error
	if l.config.Writes != (WriteBudget{}) {
		written, writesErr = l.config.Device.BytesWritten(ctx)
		if writesErr != nil {
			errs = append(errs, fmt.Errorf("the device's bytes written: %w", writesErr))
		}
	}
	l.mu.Lock()
	cache := l.cache
	l.mu.Unlock()
	var held int64
	switch {
	case cache != nil:
		held = max(cache.Held(), 0)
	case l.config.CacheFile != nil:
		file, err := l.config.CacheFile(ctx)
		if err != nil {
			// Counting nothing only makes the cache's share smaller.
			errs = append(errs, fmt.Errorf("the cache's file: %w", err))
			break
		}
		held = max(file, 0)
	}

	now := l.clock.Now()
	l.mu.Lock()
	if writesErr == nil && l.config.Writes != (WriteBudget{}) {
		l.countLocked(written)
	}
	if spaceErr == nil {
		room := min(reading.Available+held+allocated, reading.Total)
		if sim.Bug(l.ctx, "disklimit-ignore-other-writers") {
			room = reading.Total
		}
		l.room.set(float64(room), now)
		l.total.set(float64(reading.Total), now)
		l.status.TotalBytes, l.status.AvailableBytes = reading.Total, reading.Available
	}
	l.computeLocked(now, promises, allocated, held)
	err := errors.Join(errs...)
	l.status.ReadError = ""
	if err != nil {
		l.status.ReadError = err.Error()
	}
	share, unready := max(l.status.CacheShareBytes, 0), l.status.Unready
	l.mu.Unlock()

	if unready != "" {
		sim.Probe(l.ctx, ProbeDiskUnready)
	}
	if cache != nil {
		target := max(share-l.config.Region, 0)
		if sim.Bug(l.ctx, "disklimit-no-hysteresis") {
			target = share
		}
		switch {
		case held > share:
			sim.Probe(l.ctx, ProbeDiskShrink)
			cache.Shrink(target)
		case held > target:
			sim.Probe(l.ctx, ProbeDiskHysteresisHeld)
		}
	}
	return err
}

// readSpace reads the filesystem, and refuses a reading that cannot be true.
func (l *DiskLimiter) readSpace(ctx context.Context) (struct{ Total, Available int64 }, error) {
	var reading struct{ Total, Available int64 }
	space, err := l.config.Space.Space(ctx)
	if err != nil {
		return reading, fmt.Errorf("the filesystem's space: %w", err)
	}
	if space.Total == 0 || space.Total > maxDiskBytes || space.Available > space.Total {
		return reading, fmt.Errorf("%w: %d bytes available of %d", ErrDiskReading, space.Available, space.Total)
	}
	reading.Total, reading.Available = int64(space.Total), int64(space.Available)
	return reading, nil
}

// readUsers reads each user's promise and allocation. A user whose allocation
// cannot be read is counted as holding nothing, which only makes the cache
// smaller.
func (l *DiskLimiter) readUsers(ctx context.Context) ([]DiskPromise, int64, error) {
	promises := make([]DiskPromise, 0, len(l.config.Users))
	var allocated int64
	var errs []error
	for _, user := range l.config.Users {
		promise := DiskPromise{Name: user.Name, PromisedBytes: min(max(user.Promised(), 0), maxDiskBytes)}
		if user.Allocated != nil {
			held, err := user.Allocated(ctx)
			if err != nil {
				errs = append(errs, fmt.Errorf("what %s holds: %w", user.Name, err))
				held = 0
			}
			promise.AllocatedBytes = min(max(held, 0), maxDiskBytes)
		}
		allocated += promise.AllocatedBytes
		promises = append(promises, promise)
	}
	return promises, allocated, errors.Join(errs...)
}

// computeLocked sets the cache's share from the smoothed readings, the
// promises and what the cache holds.
//
// The room is the space the host could hold: what is free and what it holds.
// The free-space goals leave the floor free, so the promises and the cache
// share the room less the floor. The used-space goal caps the promises and the
// cache together. The cache leaves the reserve free above the floor, for
// promises not yet made. The band keeps the cache back from that by a share
// of the headroom it has left, so as the disk fills the cache's share falls a
// little at each reading rather than all at once.
func (l *DiskLimiter) computeLocked(now time.Time, promises []DiskPromise, allocated, held int64) {
	goal := l.config.Goal
	room := int64(math.Round(l.room.get(now)))
	total := int64(math.Round(l.total.get(now)))
	var promised int64
	for _, promise := range promises {
		count := promise.PromisedBytes
		if sim.Bug(l.ctx, "disklimit-count-spill-by-allocation") {
			count = promise.AllocatedBytes
		}
		promised += max(count, promise.AllocatedBytes)
	}

	floor, binding := goal.FreeBytes, BindingFreeBytes
	if percent := fraction(total, goal.FreePercent, 100); percent > floor {
		floor, binding = percent, BindingFreePercent
	}
	if floor == 0 {
		binding = BindingFilesystem
	}
	hard := room - floor - promised
	if hard < 0 && sim.Bug(l.ctx, "disklimit-take-from-spill") {
		hard = room - floor - allocated
	}
	reserve := l.config.ReserveBytes
	if sim.Bug(l.ctx, "disklimit-no-reserve") {
		reserve = 0
	}
	headroom := max(hard-reserve-held, 0)
	band := min(fraction(headroom, l.config.BandPercent, 100), l.config.MaxBandBytes)
	share := hard - reserve - band
	if goal.UsedBytes > 0 && goal.UsedBytes-promised < share {
		share, binding = goal.UsedBytes-promised, BindingUsedBytes
	}
	if goal.UsedBytes > 0 {
		hard = min(hard, goal.UsedBytes-promised)
	}

	l.hard = hard
	l.status.Binding = binding
	l.status.SmoothTotalBytes = total
	l.status.SmoothFreeBytes = room - held - allocated
	l.status.FloorBytes, l.status.BandBytes, l.status.ReserveBytes = floor, band, reserve
	l.status.Promises, l.status.PromisedBytes = promises, promised
	l.status.CacheHeldBytes, l.status.CacheShareBytes = held, share
	l.status.Unready = ""
	if hard < 0 {
		names := make([]string, 0, len(promises))
		for _, promise := range promises {
			names = append(names, promise.Name)
		}
		l.status.Unready = fmt.Sprintf("the host promises %d bytes to %s, and the %s goal (%s) leaves it %d",
			promised, strings.Join(names, ", "), binding, goal, promised+hard)
	}
}

// Admit asks to write n bytes at a priority, 0 the lowest. The budget keeps a
// reserve for each priority above the lowest: priority p is admitted only
// while the budget keeps (top-p)/DiskWritePriorities of a burst after the
// write, so as the budget runs down repairs are refused first and fills from
// publications last. Without a budget every write is admitted.
func (l *DiskLimiter) Admit(n int64, priority int) bool {
	if n < 0 || n > maxDiskBytes {
		return false
	}
	priority = min(max(priority, 0), DiskWritePriorities-1)
	l.mu.Lock()
	defer l.mu.Unlock()
	budget := l.config.Writes
	if budget == (WriteBudget{}) {
		l.status.Writes.AdmittedBytes += uint64(n)
		return true
	}
	l.refillLocked(l.clock.Now())
	reserved := DiskWritePriorities - 1 - priority
	if sim.Bug(l.ctx, "disklimit-refuse-high-before-low") {
		reserved = priority
	}
	reserve := fraction(budget.BurstBytes, int64(reserved), DiskWritePriorities)
	if l.tokens-n < reserve && !sim.Bug(l.ctx, "disklimit-admit-past-budget") {
		l.status.Writes.Refused[priority]++
		if l.tokens-n >= 0 {
			sim.Probe(l.ctx, ProbeDiskLowRefused)
		}
		return false
	}
	l.tokens -= n
	l.pending += n
	l.status.Writes.AdmittedBytes += uint64(n)
	return true
}

// refillLocked credits the budget for the time since it was last credited, at
// its daily average, up to a burst. Whole days are credited whole, and the
// rest of a day exactly, carrying the fraction of a byte.
func (l *DiskLimiter) refillLocked(now time.Time) {
	budget := l.config.Writes
	elapsed := now.Sub(l.refilledAt)
	if budget == (WriteBudget{}) || elapsed <= 0 {
		return
	}
	l.refilledAt = now
	need := budget.BurstBytes - l.tokens
	if need <= 0 {
		l.carry = 0
		return
	}
	days := int64(elapsed / day)
	if days > need/budget.BytesPerDay {
		l.tokens, l.carry = budget.BurstBytes, 0
		return
	}
	l.tokens += days * budget.BytesPerDay
	hi, lo := bits.Mul64(uint64(budget.BytesPerDay), uint64(elapsed%day))
	lo, carried := bits.Add64(lo, l.carry, 0)
	credit, carry := bits.Div64(hi+carried, lo, uint64(day))
	l.tokens += int64(credit)
	l.carry = carry
	if l.tokens >= budget.BurstBytes {
		l.tokens, l.carry = budget.BurstBytes, 0
	}
}

// countLocked charges the budget with what the device wrote since its last
// count, less what Admit already charged for. A counter that went backwards is
// a replaced device: the count starts again from it, and nothing is charged.
// A charge never takes the budget below a burst in debt, so a counter that
// jumps costs at most two bursts' worth of time; the cache's own writes are
// charged when admitted, so they never go past the budget either way.
func (l *DiskLimiter) countLocked(written uint64) {
	l.refillLocked(l.clock.Now())
	if written < l.counted {
		slog.WarnContext(l.ctx, "resource: the device's write counter went backwards, so it is counted again from here",
			"was", l.counted, "now", written)
		l.counted = written
		return
	}
	if !l.counting {
		// The first count is where the device stood when the limiter started.
		l.counting, l.counted = true, written
		return
	}
	delta := written - l.counted
	l.counted = written
	l.status.Writes.WrittenBytes += delta
	charge := int64(min(delta, maxDiskBytes)) - l.pending
	l.pending = max(l.pending-int64(min(delta, maxDiskBytes)), 0)
	if charge > 0 {
		l.tokens = max(l.tokens-charge, -l.config.Writes.BurstBytes)
	}
}

// fraction is value*numerator/denominator, rounded down, for values that are not
// negative, without the product overflowing.
func fraction(value, numerator, denominator int64) int64 {
	hi, lo := bits.Mul64(uint64(value), uint64(numerator))
	quotient, _ := bits.Div64(hi, lo, uint64(denominator))
	return int64(quotient)
}

// smoother is an exponentially smoothed reading, as FoundationDB's Smoother
// is: a reading moves the estimate only as time passes after it, all but 1/e
// of the way in each fold.
type smoother struct {
	fold            time.Duration
	value, estimate float64
	at              time.Time
	started         bool
}

func (s *smoother) set(value float64, now time.Time) {
	if !s.started {
		s.value, s.estimate, s.at, s.started = value, value, now, true
		return
	}
	s.advance(now)
	s.value = value
}

func (s *smoother) get(now time.Time) float64 {
	s.advance(now)
	return s.estimate
}

func (s *smoother) advance(now time.Time) {
	elapsed := now.Sub(s.at)
	if elapsed <= 0 {
		return
	}
	s.at = now
	s.estimate += (s.value - s.estimate) * -math.Expm1(-float64(elapsed)/float64(s.fold))
}
