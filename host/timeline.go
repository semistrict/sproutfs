package host

import (
	"cmp"
	"context"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmemory"
)

// firstFaultsWindow is how long after a guest starts to run the host counts
// its faults for the "first faults" line.
const firstFaultsWindow = time.Second

// A timeline records where one VM start spent its time: each step the host took
// between the request and the guest running, timed from the request's arrival,
// and the object-store calls made on the way. The host writes it as one log
// line, so the split of every start outlives the process. A metric would not:
// a scrape sees totals, and a host that exits takes its counts with it.
//
// The guest's first instruction is not observable from the host. The nearest
// point the host sees is when the VMM reports the vCPUs running: the end of a
// restore's resume, or a boot's first answer from the VMM, which it gives once
// it has started the vCPUs. That moment is the timeline's "running".
type timeline struct {
	clock platform.Clock
	began time.Time
	// vm and how name the start in both of its log lines, which go to logger.
	vm, how string
	logger  *slog.Logger
	store   platform.ObjectTrace

	mu      sync.Mutex
	steps   []timedStep
	attach  []loggedAttach
	running time.Time
}

type timedStep struct {
	name  string
	began time.Time
	took  time.Duration
}

type timelineKey struct{}

// startTimeline begins the timeline of one start of vm now, and returns a
// context that carries it and lists the start's object-store calls in it. how
// says which start it is: a create, an open or a receive.
func startTimeline(ctx context.Context, clock platform.Clock, vm, how string) (context.Context, *timeline) {
	clock = platform.ClockOr(clock)
	t := &timeline{clock: clock, began: clock.Now(), vm: vm, how: how, logger: slog.Default()}
	ctx = context.WithValue(ctx, timelineKey{}, t)
	return platform.WithObjectTrace(ctx, &t.store), t
}

// timelineOf is the timeline ctx carries, nil for none.
func timelineOf(ctx context.Context) *timeline {
	t, _ := ctx.Value(timelineKey{}).(*timeline)
	return t
}

// step begins one named step of the timeline ctx carries and returns the
// function that ends it. Without a timeline it records nothing.
func step(ctx context.Context, name string) func() {
	t := timelineOf(ctx)
	if t == nil {
		return func() {}
	}
	began := t.clock.Now()
	return func() { t.add(name, began, t.clock.Since(began)) }
}

// add records a step whose times are known: one measured by someone else,
// such as the phases of a VMM start.
func (t *timeline) add(name string, began time.Time, took time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steps = append(t.steps, timedStep{name: name, began: began, took: took})
}

// attached records what one memory region's attach cost: the session, and the
// populate inside it that mapped resident pages before the guest ran.
func (t *timeline) attached(region string, took time.Duration, populate vmmemory.PopulateStats) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attach = append(t.attach, loggedAttach{Region: region, MS: milliseconds(took),
		PopulateMS: milliseconds(time.Duration(populate.DurationNS)), PopulatedPages: populate.Pages,
		PopulateCommands: populate.Commands})
}

// ran marks the guest of the start in ctx as running now, and counts its faults
// on regions from here for firstFaultsWindow, which a second log line reports.
func ran(ctx context.Context, regions map[string]*vmmemory.MemoryRegion) {
	t := timelineOf(ctx)
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.running.IsZero() {
		t.mu.Unlock()
		return
	}
	t.running = t.clock.Now()
	t.mu.Unlock()
	before := make(map[string]vmmemory.GuestFaults, len(regions))
	for name, region := range regions {
		before[name] = region.GuestFaults()
	}
	logged := context.WithoutCancel(ctx)
	t.clock.AfterFunc(firstFaultsWindow, func() { t.logFirstFaults(logged, regions, before) })
}

// loggedStep is one step as the log line carries it, in milliseconds from the
// request's arrival.
type loggedStep struct {
	Name   string  `json:"name"`
	AtMS   float64 `json:"at_ms"`
	TookMS float64 `json:"took_ms"`
}

// loggedStore is one kind of object-store call: how many the start made, their
// summed time and the longest.
type loggedStore struct {
	Kind   string  `json:"kind"`
	Calls  int     `json:"calls"`
	MS     float64 `json:"ms"`
	MaxMS  float64 `json:"max_ms"`
	Failed int     `json:"failed,omitempty"`
}

type loggedAttach struct {
	Region           string  `json:"region"`
	MS               float64 `json:"ms"`
	PopulateMS       float64 `json:"populate_ms"`
	PopulatedPages   uint64  `json:"populated_pages"`
	PopulateCommands uint64  `json:"populate_commands"`
}

// loggedFaults is one memory region's guest faults around the moment the guest
// started to run: those before it, which a VMM's own restore or kernel load
// takes, and those in the window after it.
type loggedFaults struct {
	Region         string  `json:"region"`
	Before         int64   `json:"before"`
	BeforeWaitedMS float64 `json:"before_waited_ms"`
	After          int64   `json:"after"`
	AfterWaitedMS  float64 `json:"after_waited_ms"`
	// FirstMS is when the pager read the region's first fault, from the
	// request's arrival, and FirstWaitedMS how long the guest waited for it.
	FirstMS       float64 `json:"first_ms,omitempty"`
	FirstWaitedMS float64 `json:"first_waited_ms,omitempty"`
}

// log writes the start's line and ends its trace, so work that inherits the
// start's context and runs on is not counted as part of the start. The line
// carries every step, the store's calls before the guest ran and after it, the
// store's share of the time to running as the union of those calls, and the
// attaches. attrs come first.
func (t *timeline) log(ctx context.Context, message string, attrs ...any) {
	if t == nil {
		return
	}
	t.store.Close()
	total := t.clock.Since(t.began)
	calls, dropped := t.store.Calls()
	steps := t.ordered()
	t.mu.Lock()
	attach := slices.Clone(t.attach)
	running := t.running
	t.mu.Unlock()
	logged := make([]loggedStep, 0, len(steps))
	for _, s := range steps {
		logged = append(logged, loggedStep{Name: s.name, AtMS: milliseconds(s.began.Sub(t.began)),
			TookMS: milliseconds(s.took)})
	}
	var before, after []platform.ObjectCall
	for _, call := range calls {
		if running.IsZero() || call.Began.Before(running) || sim.Bug(ctx, "timeline-count-calls-after-running") {
			before = append(before, call)
		} else {
			after = append(after, call)
		}
	}
	attrs = append(attrs, "vm", t.vm, "how", t.how, "total_ms", milliseconds(total))
	if !running.IsZero() {
		attrs = append(attrs, "running_ms", milliseconds(running.Sub(t.began)))
	}
	attrs = append(attrs, "store_ms", milliseconds(spanned(before)),
		slog.Any("steps", logged), slog.Any("store", storeKinds(before)),
		slog.Any("store_after", storeKinds(after)), slog.Any("attach", attach))
	if dropped > 0 {
		attrs = append(attrs, "store_dropped", dropped)
	}
	t.logger.InfoContext(ctx, message, attrs...)
}

// ordered is the steps by when each began. Steps that began together keep the
// order they ended in.
func (t *timeline) ordered() []timedStep {
	t.mu.Lock()
	steps := slices.Clone(t.steps)
	t.mu.Unlock()
	slices.SortStableFunc(steps, func(a, b timedStep) int { return a.began.Compare(b.began) })
	return steps
}

// logFirstFaults writes the line of a start's first faults: per memory region,
// the guest faults before it ran and in the window after.
func (t *timeline) logFirstFaults(ctx context.Context, regions map[string]*vmmemory.MemoryRegion,
	before map[string]vmmemory.GuestFaults) {
	faults := make([]loggedFaults, 0, len(regions))
	for _, name := range slices.Sorted(maps.Keys(regions)) {
		now, then := regions[name].GuestFaults(), before[name]
		entry := loggedFaults{Region: name, Before: then.Count, BeforeWaitedMS: milliseconds(then.Waited),
			After: now.Count - then.Count, AfterWaitedMS: milliseconds(now.Waited - then.Waited)}
		if !now.First.IsZero() {
			entry.FirstMS, entry.FirstWaitedMS = milliseconds(now.First.Sub(t.began)), milliseconds(now.FirstWaited)
		}
		faults = append(faults, entry)
	}
	t.mu.Lock()
	running := t.running
	t.mu.Unlock()
	t.logger.InfoContext(ctx, "host: a VM's first faults", "vm", t.vm, "how", t.how,
		"running_ms", milliseconds(running.Sub(t.began)), "window_ms", milliseconds(firstFaultsWindow),
		slog.Any("faults", faults))
}

// storeKinds sums calls by kind, in the order each kind first ended.
func storeKinds(calls []platform.ObjectCall) []loggedStore {
	kinds := []loggedStore{}
	at := map[string]int{}
	for _, call := range calls {
		kind := storeKind(call)
		index, seen := at[kind]
		if !seen {
			index = len(kinds)
			at[kind] = index
			kinds = append(kinds, loggedStore{Kind: kind})
		}
		took := milliseconds(call.Took)
		kinds[index].Calls++
		kinds[index].MS += took
		kinds[index].MaxMS = max(kinds[index].MaxMS, took)
		if call.Failed {
			kinds[index].Failed++
		}
	}
	return kinds
}

// storeKind names a call by the object it touched and the operation:
// "control record conditional put", "index get", "part get". The objects are
// a VM's control record (control/<id>), and a checkpoint's index and parts
// (vm/<id>/ckpt/<seq>/index and .../part/<n>).
func storeKind(call platform.ObjectCall) string {
	key := call.Key.String()
	object := "other"
	switch {
	case call.Operation == platform.ListOperation:
		object = "listing"
	case strings.HasPrefix(key, control.RecordPrefix):
		object = "control record"
	case strings.HasSuffix(key, "/index"):
		object = "index"
	case strings.Contains(key, "/part/"):
		object = "part"
	}
	operation := call.Operation.String()
	if call.Conditional {
		operation = "conditional " + operation
	}
	return object + " " + operation
}

// spanned is the time at least one of calls was in flight: the union of their
// intervals, which is the store's share of a start whose calls overlap.
func spanned(calls []platform.ObjectCall) time.Duration {
	sorted := slices.SortedFunc(slices.Values(calls), func(a, b platform.ObjectCall) int {
		return cmp.Compare(a.Began.UnixNano(), b.Began.UnixNano())
	})
	var total time.Duration
	var end time.Time
	for _, call := range sorted {
		finish := call.Began.Add(call.Took)
		switch {
		case !finish.After(end):
			continue
		case call.Began.After(end):
			total += call.Took
		default:
			total += finish.Sub(end)
		}
		end = finish
	}
	return total
}

func milliseconds(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
