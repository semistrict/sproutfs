package sim

import (
	"context"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The runtime a fault-injection site consults travels in the context, because a
// site lives in production code that owns no simulator: volume, checkpoint,
// control, vmmigrate, vmmemory and host are compiled and shipped with these
// calls in them. A context with no runtime — every real deployment, and every
// test that did not ask for one — makes each of them a map-free interface
// comparison and a return of false.
type runtimeKey struct{}

// WithRuntime hands ctx the runtime whose seed and switches the fault-injection
// sites below consult. A harness does this once, where it builds the context
// its workload runs under; production callers never do.
func WithRuntime(ctx context.Context, r *Runtime) context.Context {
	if r == nil {
		panic("sim: runtime must not be nil")
	}
	return context.WithValue(ctx, runtimeKey{}, r)
}

// RuntimeFrom reports the runtime ctx carries, or nil outside a simulation.
func RuntimeFrom(ctx context.Context) *Runtime {
	r, _ := ctx.Value(runtimeKey{}).(*Runtime)
	return r
}

// buggifyActivation is the probability that a site is activated at all for a
// run, as FoundationDB's flow/Buggify.h uses: a quarter of the sites in one
// run, so a seed explores a few faults deeply rather than every fault shallowly.
const buggifyActivation = 0.25

// Buggify reports whether the fault at site id should happen on this call. It
// is two-level, like FoundationDB's: a site is activated once per run with
// probability 0.25, drawn from the seed and the id alone, and an activated site
// then fires with probability p on each call. Both are recorded in the trace.
//
// Sites are off unless a campaign turns the runtime's switch on, so a recording
// or replay test sees the same bytes it always did. id must be stable and must
// name the fault rather than the caller's position, since the activation draw
// is keyed on it: "checkpoint/publish/give-up", not a line number. A site
// reached concurrently draws its per-call firings in arrival order, so a site
// inside concurrent work belongs on an id that distinguishes the callers.
func Buggify(ctx context.Context, id string, p float64) bool {
	r := RuntimeFrom(ctx)
	if r == nil || !r.buggify.Load() {
		return false
	}
	return r.buggifySite(id, p)
}

// BuggifyDelay waits a seeded duration of up to maximum when the site fires,
// and returns at once otherwise. It is how a site says "this step can take much
// longer than it usually does" without a caller inventing a clock.
func BuggifyDelay(ctx context.Context, id string, p float64, maximum time.Duration) error {
	if !Buggify(ctx, id, p) {
		return nil
	}
	r := RuntimeFrom(ctx)
	delay := r.Random("buggify-delay").Duration(id+"/"+r.occurrenceOf("delay/"+id), maximum)
	r.trace.record(Event{Kind: "buggify", Resource: id, Operation: "delay", Outcome: delay.String()})
	return sleep(ctx, delay)
}

// Probe marks a place execution reached that a campaign cares about reaching,
// as FoundationDB's CODE_PROBE does: the point of a fault is the code it makes
// run, and a fault nobody ever reached is the failure the whole harness is
// meant to rule out. Probes are counted on the runtime rather than traced, so
// registering one changes no recording.
func Probe(ctx context.Context, name string) {
	if name == "" {
		panic("sim: probe name must not be empty")
	}
	r := RuntimeFrom(ctx)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.probes == nil {
		r.probes = make(map[string]uint64)
	}
	r.probes[name]++
}

// Probes reports every probe this run reached and how often.
func (r *Runtime) Probes() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.probes)
}

// MissedProbes reports the registered probes this run never reached, sorted.
func (r *Runtime) MissedProbes(registered []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var missed []string
	for _, name := range registered {
		if r.probes[name] == 0 {
			missed = append(missed, name)
		}
	}
	slices.Sort(missed)
	return slices.Compact(missed)
}

// Bug reports whether the named in-tree bug is enabled. Unlike Buggify it is
// not seeded: SPROUTFS_SIM_BUG names exactly the bugs a run installs, so a
// mutation the catalogue describes is one `go test` invocation rather than a
// patched source tree. A guard must be a fault the tests are supposed to catch;
// nothing outside a negative test ever enables one.
func Bug(ctx context.Context, id string) bool {
	if id == "" {
		panic("sim: bug id must not be empty")
	}
	r := RuntimeFrom(ctx)
	if r == nil || !r.bugs[id] {
		return false
	}
	r.noteBug(id)
	return true
}

// Bugs reports the in-tree bug guards this runtime has enabled, sorted.
func (r *Runtime) Bugs() []string { return slices.Sorted(maps.Keys(r.bugs)) }

// SetBuggify turns every Buggify site on or off for this runtime. Campaigns opt
// in; everything else, including every recording and replay comparison, runs
// with the sites off and sees exactly the trace it saw before they existed.
func (r *Runtime) SetBuggify(enabled bool) { r.buggify.Store(enabled) }

// BuggifySites reports every site this run reached, against whether the seed
// activated it. A campaign prints both halves: the sites it never reached are
// fault injection nobody is driving, and the activated ones are the faults this
// seed was actually exploring.
func (r *Runtime) BuggifySites() map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.buggified)
}

// FiredSites reports the sites this run activated and then actually fired at
// least once, with the firing count.
func (r *Runtime) FiredSites() map[string]uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.fired)
}

// buggifySite resolves one call of an enabled site: activate the site the first
// time it is reached, then draw this call's firing.
func (r *Runtime) buggifySite(id string, p float64) bool {
	r.mu.Lock()
	if r.buggified == nil {
		r.buggified = make(map[string]bool)
	}
	activated, known := r.buggified[id]
	if !known {
		activated = keyedChance(r.seed, "buggify/"+id, buggifyActivation)
		r.buggified[id] = activated
	}
	r.mu.Unlock()
	if !known {
		r.trace.record(Event{Kind: "buggify", Resource: id, Operation: "activate", Outcome: onOff(activated)})
	}
	if !activated {
		return false
	}
	fires := keyedChance(r.seed, "buggify/"+id+"/fire/"+r.occurrenceOf(id), p)
	if fires {
		r.mu.Lock()
		if r.fired == nil {
			r.fired = make(map[string]uint64)
		}
		r.fired[id]++
		r.mu.Unlock()
	}
	r.trace.record(Event{Kind: "buggify", Resource: id, Operation: "fire", Outcome: onOff(fires)})
	return fires
}

// occurrenceOf numbers the calls of one site so repeated calls draw differently
// without a shared PRNG stream: the count is per id, so a choice added at one
// site cannot perturb another's draws.
func (r *Runtime) occurrenceOf(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.occurrences == nil {
		r.occurrences = make(map[string]uint64)
	}
	r.occurrences[id]++
	return strconv.FormatUint(r.occurrences[id], 10)
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}

// keyedChance is Random.Chance against a key, without a namespace: the two
// injection draws are the runtime's own and share no stream with any adapter.
func keyedChance(seed uint64, key string, p float64) bool {
	if p <= 0 {
		return false
	}
	if p >= 1 {
		return true
	}
	return float64(keyedSample(seed, key)>>11)/(1<<53) < p
}

// bugEnvironment is SPROUTFS_SIM_BUG: a comma-separated list of the in-tree
// bugs to install. A runtime reads it once, when it is built, because a guard's
// answer must not change under a run and a site that read the environment on
// every call would pay for the lookup for ever.
const bugEnvironment = "SPROUTFS_SIM_BUG"

func enabledBugs() map[string]bool {
	bugs := make(map[string]bool)
	for id := range strings.SplitSeq(os.Getenv(bugEnvironment), ",") {
		if id = strings.TrimSpace(id); id != "" {
			bugs[id] = true
		}
	}
	return bugs
}

// noteBug records a guard the first time it answers yes, so a negative test's
// trace says the bug was installed without one event per call.
func (r *Runtime) noteBug(id string) {
	r.mu.Lock()
	if r.notedBugs == nil {
		r.notedBugs = make(map[string]bool)
	}
	noted := r.notedBugs[id]
	r.notedBugs[id] = true
	r.mu.Unlock()
	if !noted {
		r.trace.record(Event{Kind: "bug", Resource: id, Operation: "enabled", Outcome: "on"})
	}
}
