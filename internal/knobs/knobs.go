// Package knobs is the deployment's tunables in one place: the size a
// checkpoint part fills to, the budgets a pager admits pages against, the intervals a host
// checkpoints and re-reads its records on, and the bounds a drain and a
// publication work within.
//
// They were constants spread through six packages. A constant is a value no
// test ever varies, so every simulated run explored the same one — and the bugs
// that live at a boundary (a part that holds exactly one member, a dirty budget
// too small for one write-ahead run, a hold shorter than the checkpoint
// interval it is measured in) were unreachable. FoundationDB randomises its own
// knobs per seed for exactly this reason; Randomize is that, and the campaigns
// draw one set per seed under an opt-in.
//
// Nothing here imports the packages it configures: a knob is a number, and the
// mapping from a number onto one package's Config belongs to whoever assembles
// that package. Nothing here imports the simulator either — Randomize takes the
// seeded choice source as an interface, which sim.Random satisfies — so a
// production binary that reads its knobs links no simulation.
package knobs

import (
	"errors"
	"fmt"
	"time"
)

// Random is the seeded, schedule-independent choice source Randomize draws
// from. Every draw is keyed by a stable id, so adding a knob does not move the
// values the others take. sim.Random satisfies it.
type Random interface {
	Intn(id string, limit int) int
	Chance(id string, probability float64) bool
	Uint64(id string) uint64
}

// ErrInvalid reports a set of knobs that cannot be run: a budget below what one
// operation needs, or two that contradict each other.
var ErrInvalid = errors.New("knobs: invalid tunable")

// Knobs is one deployment's tunables. The zero value is not usable; start from
// Defaults.
type Knobs struct {
	// PartBytes is the encoded member size one checkpoint part fills to before
	// it is sealed and uploaded. It bounds both a publication's memory and the
	// bytes one interrupted upload repeats. checkpoint.Config.PartBytes.
	PartBytes int
	// MaxIndexBytes bounds the root one publication may write, which is what a
	// checkpoint's page table is read back through.
	// checkpoint.Config.MaxIndexBytes.
	MaxIndexBytes int
	// UploadConcurrency is how many objects one store uploads at a time,
	// host-wide, and MaxBuilders how many publications may hold a part builder
	// at once. MaxBuilders times PartBytes is what publication costs a host
	// however many of its VMs become dirty together.
	UploadConcurrency int
	MaxBuilders       int
	// CacheLoads bounds the page-cache fetches in flight.
	// checkpoint.CacheConfig.MaxConcurrentLoads.
	CacheLoads int
	// MaxWriteBytes bounds the payload of one write or batched write, which is
	// also what the pager sizes its writeback batches against.
	// volume.Config.MaxWriteBytes.
	MaxWriteBytes int
	// MaxOpenVMs bounds the live handles one volume manager owns.
	MaxOpenVMs int

	// CheckpointInterval is how often every VM a host runs is checkpointed,
	// which is what a host loss rewinds a VM by. CheckpointJitterShare is the
	// fraction of the interval the wait is spread either side of, so the VMs a
	// host runs do not checkpoint in lockstep; a share of eight is an eighth.
	CheckpointInterval    time.Duration
	CheckpointJitterShare int
	// LossWindow is how long a VM may hold a write no checkpoint covers before
	// the pager stops admitting dirty pages for it, which is what bounds a host
	// loss in time where the interval bounds it when everything works. Zero
	// disables the bound.
	LossWindow time.Duration
	// EpochInterval is how often a host re-reads the control record of every VM
	// it holds, which is how it learns a later writer has taken one over.
	EpochInterval time.Duration
	// HoldIntervals is how many checkpoint intervals one handover's pages are
	// served for before the host gives them up: a migration's destination or a
	// fork's child that nothing ever reported complete.
	HoldIntervals int

	// ResidentPages is the pager's arena, LogicalPages its per-region metadata
	// cap including never-faulted pages, and DirtyPages the volatile private
	// state it admits across RAM and spill.
	ResidentPages, LogicalPages, DirtyPages int
	// ReadAheadPages is the aligned run one fault loads and maps, and
	// WriteAheadPages the run one store into fresh zeros gives private pages.
	// ReadAheadPages must be a power of two.
	ReadAheadPages, WriteAheadPages int
	// ConcurrentIO bounds the pager's ordinary page operations. Each permit can
	// hold one read-ahead run, so it is both the parallelism and the buffers.
	ConcurrentIO int
	// SettleWorkers is how many workers one settle divides a sealed set
	// between, comparing each page it holds with the page it was copied from.
	SettleWorkers int

	// DrainConcurrency is how many VMs one drain hands over at once,
	// DrainTimeout bounds the whole drain and DrainPerVM one VM's handover
	// inside it.
	DrainConcurrency int
	DrainTimeout     time.Duration
	DrainPerVM       time.Duration
}

// PageSize is the pager's page, which several knobs are counted in. It is not
// itself a knob: it is the HugeTLB page a deployment's guests map.
const PageSize = 2 << 20

// Defaults are the values a deployment runs on: every one of them is the
// default the package it configures documents, so a run with these knobs is the
// run without them.
func Defaults() Knobs {
	return Knobs{
		PartBytes:         64 << 20,
		MaxIndexBytes:     2 << 20,
		UploadConcurrency: 8,
		MaxBuilders:       8,
		CacheLoads:        16,
		MaxWriteBytes:     2 << 20,
		MaxOpenVMs:        4096,

		CheckpointInterval:    60 * time.Second,
		CheckpointJitterShare: 8,
		LossWindow:            5 * time.Minute,
		EpochInterval:         2 * time.Second,
		HoldIntervals:         4,

		ResidentPages:   512,
		LogicalPages:    1 << 16,
		DirtyPages:      256,
		ReadAheadPages:  4,
		WriteAheadPages: 4,
		ConcurrentIO:    16,
		SettleWorkers:   4,

		DrainConcurrency: 4,
		DrainTimeout:     90 * time.Second,
		DrainPerVM:       60 * time.Second,
	}
}

// Validate reports a set of knobs the packages they configure would refuse, or
// that contradict each other. Randomize produces only valid sets; this is what
// says so, and what a campaign checks a hand-written set with.
func (k Knobs) Validate() error {
	var errs []error
	bound := func(name string, value, low, high int) {
		if value < low || value > high {
			errs = append(errs, fmt.Errorf("%w: %s is %d, want %d..%d", ErrInvalid, name, value, low, high))
		}
	}
	bound("PartBytes", k.PartBytes, 1, 64<<20)
	bound("MaxIndexBytes", k.MaxIndexBytes, 64<<10, 2<<20)
	bound("UploadConcurrency", k.UploadConcurrency, 1, 1024)
	bound("MaxBuilders", k.MaxBuilders, 1, 1024)
	bound("CacheLoads", k.CacheLoads, 1, 4096)
	// A write below one sector cannot carry a page, and the volume manager
	// refuses one above its own ceiling.
	bound("MaxWriteBytes", k.MaxWriteBytes, 4096, 16<<20)
	bound("MaxOpenVMs", k.MaxOpenVMs, 1, 1<<16)
	bound("CheckpointJitterShare", k.CheckpointJitterShare, 1, 1024)
	bound("HoldIntervals", k.HoldIntervals, 1, 1024)
	bound("ResidentPages", k.ResidentPages, 1, 1<<24)
	bound("LogicalPages", k.LogicalPages, 1, 1<<24)
	bound("DirtyPages", k.DirtyPages, 1, 1<<24)
	bound("ReadAheadPages", k.ReadAheadPages, 1, (16<<20)/PageSize)
	bound("WriteAheadPages", k.WriteAheadPages, 1, 4096)
	bound("ConcurrentIO", k.ConcurrentIO, 1, 1024)
	bound("SettleWorkers", k.SettleWorkers, 1, 1024)
	bound("DrainConcurrency", k.DrainConcurrency, 1, 1024)
	if k.ReadAheadPages&(k.ReadAheadPages-1) != 0 {
		errs = append(errs, fmt.Errorf("%w: ReadAheadPages is %d, want a power of two", ErrInvalid, k.ReadAheadPages))
	}
	// The pager refuses a resident or dirty budget larger than the metadata cap
	// that has to describe those pages.
	if k.ResidentPages > k.LogicalPages {
		errs = append(errs, fmt.Errorf("%w: ResidentPages %d exceeds LogicalPages %d",
			ErrInvalid, k.ResidentPages, k.LogicalPages))
	}
	if k.DirtyPages > k.LogicalPages {
		errs = append(errs, fmt.Errorf("%w: DirtyPages %d exceeds LogicalPages %d",
			ErrInvalid, k.DirtyPages, k.LogicalPages))
	}
	if k.CheckpointInterval <= 0 {
		errs = append(errs, fmt.Errorf("%w: CheckpointInterval is %s, want a positive interval",
			ErrInvalid, k.CheckpointInterval))
	}
	if k.LossWindow < 0 {
		errs = append(errs, fmt.Errorf("%w: LossWindow is %s, want a window or zero to disable it",
			ErrInvalid, k.LossWindow))
	}
	// A window shorter than the interval is one every VM is past before its
	// first checkpoint is even due, so every guest waits at every interval.
	if k.LossWindow > 0 && k.CheckpointInterval > 0 && k.LossWindow < k.CheckpointInterval {
		errs = append(errs, fmt.Errorf("%w: LossWindow %s is shorter than CheckpointInterval %s",
			ErrInvalid, k.LossWindow, k.CheckpointInterval))
	}
	if k.EpochInterval <= 0 {
		errs = append(errs, fmt.Errorf("%w: EpochInterval is %s, want a positive interval",
			ErrInvalid, k.EpochInterval))
	}
	if k.DrainTimeout <= 0 || k.DrainPerVM <= 0 {
		errs = append(errs, fmt.Errorf("%w: a drain's bounds are %s and %s, want positive ones",
			ErrInvalid, k.DrainTimeout, k.DrainPerVM))
	}
	// One VM's handover cannot be given longer than the drain that contains it:
	// a per-VM bound above the whole drain's is a bound that never applies, and
	// a drain that looks bounded per VM and is not is exactly the pod killed
	// with its pages still on it.
	if k.DrainPerVM > k.DrainTimeout {
		errs = append(errs, fmt.Errorf("%w: DrainPerVM %s exceeds DrainTimeout %s",
			ErrInvalid, k.DrainPerVM, k.DrainTimeout))
	}
	return errors.Join(errs...)
}

// Randomize returns a hostile but valid set of knobs for one seed: budgets at
// or near the smallest value the code still has to work at, intervals far
// shorter and occasionally far longer than a deployment's, and the odd knob
// left at its default so a run is not uniformly small.
//
// Hostile means the boundaries a constant hides. A part size of one byte makes
// every member its own part, so a publication is all of its interrupted-upload
// paths at once. A dirty budget of a handful of pages makes the pager's
// pressure path — a checkpoint out of the interval's turn, then a deliberate
// stop — the ordinary case rather than the rare one. A hold of one checkpoint
// interval makes a handover race the interval that would make it unnecessary.
//
// Every draw is keyed by its knob's name, so a seed's value for one knob does
// not move when another is added. The result satisfies Validate.
func Randomize(r Random) Knobs {
	k := Defaults()
	// keep leaves one knob in ten at its default, so a randomized run is a
	// mixture rather than a uniformly tiny deployment: a small part size with a
	// deployment's dirty budget explores a different publication than both
	// small together.
	keep := func(id string) bool { return r.Chance("knobs/keep/"+id, 0.1) }
	pick := func(id string, choices ...int) int {
		if keep(id) {
			return -1
		}
		return choices[r.Intn("knobs/"+id, len(choices))]
	}
	set := func(id string, current *int, choices ...int) {
		if chosen := pick(id, choices...); chosen >= 0 {
			*current = chosen
		}
	}

	// A part of one byte seals after every member; one page holds a single
	// page; the rest are ordinary sizes either side of the default.
	set("part-bytes", &k.PartBytes, 1, PageSize, 4<<20, 64<<20)
	set("max-index-bytes", &k.MaxIndexBytes, 64<<10, 1<<20, 2<<20)
	set("upload-concurrency", &k.UploadConcurrency, 1, 2, 8, 64)
	set("max-builders", &k.MaxBuilders, 1, 2, 8)
	set("cache-loads", &k.CacheLoads, 1, 2, 16, 128)
	set("max-write-bytes", &k.MaxWriteBytes, 4096, 64<<10, 2<<20, 16<<20)
	set("max-open-vms", &k.MaxOpenVMs, 1, 2, 16, 4096)

	set("checkpoint-jitter-share", &k.CheckpointJitterShare, 1, 2, 8, 1024)
	set("hold-intervals", &k.HoldIntervals, 1, 2, 4, 64)
	// Resident, logical and dirty are drawn together: the pager refuses a
	// resident or dirty budget above the logical cap, so the cap is drawn first
	// and the other two are drawn within it.
	if !keep("logical-pages") {
		k.LogicalPages = []int{8, 64, 1 << 12, 1 << 16}[r.Intn("knobs/logical-pages", 4)]
	}
	if !keep("resident-pages") {
		k.ResidentPages = 1 + r.Intn("knobs/resident-pages", k.LogicalPages)
	} else {
		k.ResidentPages = min(k.ResidentPages, k.LogicalPages)
	}
	if !keep("dirty-pages") {
		k.DirtyPages = 1 + r.Intn("knobs/dirty-pages", k.LogicalPages)
	} else {
		k.DirtyPages = min(k.DirtyPages, k.LogicalPages)
	}
	set("read-ahead-pages", &k.ReadAheadPages, 1, 2, 4, 8)
	set("write-ahead-pages", &k.WriteAheadPages, 1, 2, 4, 64)
	set("concurrent-io", &k.ConcurrentIO, 1, 2, 16, 256)
	set("settle-workers", &k.SettleWorkers, 1, 2, 16)
	set("drain-concurrency", &k.DrainConcurrency, 1, 2, 4, 64)

	interval := func(id string, current *time.Duration, choices ...time.Duration) {
		if keep(id) {
			return
		}
		*current = choices[r.Intn("knobs/"+id, len(choices))]
	}
	// A millisecond interval checkpoints faster than most operations complete,
	// which is the back-to-back case the loop documents; an hour is a host that
	// is only made durable by a shutdown.
	interval("checkpoint-interval", &k.CheckpointInterval,
		time.Millisecond, 10*time.Millisecond, time.Second, 60*time.Second, time.Hour)
	// The window is drawn as a multiple of the interval it has to be at least
	// as long as: zero turns the bound off, one interval is the tightest bound
	// that means anything at all, and sixty is a deployment that wants nothing
	// but a backstop.
	if !keep("loss-window") {
		k.LossWindow = []time.Duration{0, 1, 5, 60}[r.Intn("knobs/loss-window", 4)] * k.CheckpointInterval
	} else if k.LossWindow > 0 && k.LossWindow < k.CheckpointInterval {
		k.LossWindow = k.CheckpointInterval
	}
	interval("epoch-interval", &k.EpochInterval,
		time.Millisecond, 100*time.Millisecond, 2*time.Second, time.Hour)
	interval("drain-timeout", &k.DrainTimeout,
		time.Millisecond, time.Second, 90*time.Second)
	// One VM's bound never exceeds the drain's.
	k.DrainPerVM = k.DrainTimeout
	if !keep("drain-per-vm") {
		k.DrainPerVM = k.DrainTimeout / time.Duration(1+r.Intn("knobs/drain-per-vm", 4))
		if k.DrainPerVM <= 0 {
			k.DrainPerVM = k.DrainTimeout
		}
	}
	return k
}

// HoldTimeout is how long one handover's pages are served for: the hold
// intervals times the checkpoint interval.
func (k Knobs) HoldTimeout() time.Duration {
	return time.Duration(k.HoldIntervals) * k.CheckpointInterval
}

// Changed lists the knobs that differ from the defaults, as "name=value"
// strings in a stable order. It is what a campaign prints with its seed, so a
// failure names the tunables that produced it.
func (k Knobs) Changed() []string {
	base := Defaults()
	var changed []string
	add := func(name string, value, want any) {
		if value != want {
			changed = append(changed, fmt.Sprintf("%s=%v", name, value))
		}
	}
	add("part-bytes", k.PartBytes, base.PartBytes)
	add("max-index-bytes", k.MaxIndexBytes, base.MaxIndexBytes)
	add("upload-concurrency", k.UploadConcurrency, base.UploadConcurrency)
	add("max-builders", k.MaxBuilders, base.MaxBuilders)
	add("cache-loads", k.CacheLoads, base.CacheLoads)
	add("max-write-bytes", k.MaxWriteBytes, base.MaxWriteBytes)
	add("max-open-vms", k.MaxOpenVMs, base.MaxOpenVMs)
	add("checkpoint-interval", k.CheckpointInterval, base.CheckpointInterval)
	add("checkpoint-jitter-share", k.CheckpointJitterShare, base.CheckpointJitterShare)
	add("loss-window", k.LossWindow, base.LossWindow)
	add("epoch-interval", k.EpochInterval, base.EpochInterval)
	add("hold-intervals", k.HoldIntervals, base.HoldIntervals)
	add("resident-pages", k.ResidentPages, base.ResidentPages)
	add("logical-pages", k.LogicalPages, base.LogicalPages)
	add("dirty-pages", k.DirtyPages, base.DirtyPages)
	add("read-ahead-pages", k.ReadAheadPages, base.ReadAheadPages)
	add("write-ahead-pages", k.WriteAheadPages, base.WriteAheadPages)
	add("concurrent-io", k.ConcurrentIO, base.ConcurrentIO)
	add("settle-workers", k.SettleWorkers, base.SettleWorkers)
	add("drain-concurrency", k.DrainConcurrency, base.DrainConcurrency)
	add("drain-timeout", k.DrainTimeout, base.DrainTimeout)
	add("drain-per-vm", k.DrainPerVM, base.DrainPerVM)
	return changed
}

func (k Knobs) String() string { return fmt.Sprintf("knobs%v", k.Changed()) }
