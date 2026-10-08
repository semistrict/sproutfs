//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/control"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmachine"
	"github.com/semistrict/sproutfs/volume"
)

// The template and what each fork of it does. The template's guest fills
// templatePatternMiB of its RAM with words that name their page, so every page
// a fork reads can be checked. Each fork stores its own mark over one page in
// templateStride, at a phase it shares with one other fork: half the pattern's
// pages are stored into by two forks each, and the other half by none.
const (
	templateRAM        = 256 << 20
	templatePatternMiB = 128
	templatePages      = templatePatternMiB << 20 / 4096
	templateStride     = 8
	templatePhases     = templateStride / 2
	// templateStampBytes is what each fork's stores make private.
	templateStampBytes = templatePatternMiB << 20 / templateStride
)

// templateStore is the object store the template is read back from: a 4 KiB
// read takes 0.65 ms and an 8 MiB run 39 ms, which is what a GCE host read
// from the cluster on 2026-10-03 (docs/measurements/gce-dependent-reads-2026-10-03.md).
// A read that takes a store's time is what keeps one fork's READ outstanding
// while another fork's fault or prefetch of the same page starts.
var templateStore = sim.ObjectStoreConfig{HeadLatency: 650 * time.Microsecond,
	GetLatency: 650 * time.Microsecond, PutLatency: time.Nanosecond, ListLatency: time.Nanosecond,
	DeleteLatency: time.Nanosecond, BytesPerSecond: 8 << 20 * 1000 / 39}

// templateForkStep is one console command each fork runs once released, and
// the line that answers it.
type templateForkStep struct {
	command, answer string
}

// templateForkSteps is what each fork does from its release: read every page
// of the pattern cold, store its mark, and read every page twice more, through
// whatever the first reads and the stores left evicted, spilled or shared.
var templateForkSteps = []templateForkStep{
	{"verify", "SPROUTFS_VERIFY"},
	{"stamp", "SPROUTFS_STAMP"},
	{"verify", "SPROUTFS_VERIFY"},
	{"verify", "SPROUTFS_VERIFY"},
}

// templateForks is how many forks one run starts at once:
// SPROUTFS_TEMPLATE_FORKS, four by default. The arena is smaller than what
// the forks hold between them, so their evictions, spills and refaults grow
// much faster than their number: on GCE's eight-processor qualification host
// (2026-10-08) two forks evicted 1,045 pages and four 15,380 in a 27 s phase,
// and eight thrashed until their guests stalled. A larger host runs more.
func templateForks(t *testing.T) int {
	t.Helper()
	value := os.Getenv("SPROUTFS_TEMPLATE_FORKS")
	if value == "" {
		return 4
	}
	forks, err := strconv.Atoi(value)
	if err != nil || forks < 2 {
		t.Fatalf("SPROUTFS_TEMPLATE_FORKS=%q must be a count of at least two", value)
	}
	return forks
}

// templatePatternWord is the guest's pattern_word (testdata/guest.c): word
// `word` of page `page` under `mark`, 0 for the template's bytes.
func templatePatternWord(mark, page, word uint64) uint64 {
	x := mark<<48 ^ page<<9 ^ word ^ 0x9e3779b97f4a7c15
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

// templatePageSum is the wrapping sum of one page's words under mark.
func templatePageSum(mark, page uint64) uint64 {
	var sum uint64
	for word := range uint64(4096 / 8) {
		sum += templatePatternWord(mark, page, word)
	}
	return sum
}

// templateSums is what a verify must report: the sum of every word of the
// template, and, for each phase, the sum of the template's words on that
// phase's pages, which a fork's stores replace with its own.
type templateSums struct {
	template uint64
	phases   [templatePhases]uint64
}

func newTemplateSums() templateSums {
	var s templateSums
	for page := range uint64(templatePages) {
		sum := templatePageSum(0, page)
		s.template += sum
		if phase := page % templateStride; phase < templatePhases {
			s.phases[phase] += sum
		}
	}
	return s
}

// stamped is the sum a fork of mark that stored over phase's pages reads.
func (s templateSums) stamped(mark, phase uint64) uint64 {
	sum := s.template - s.phases[phase]
	for page := phase; page < templatePages; page += templateStride {
		sum += templatePageSum(mark, page)
	}
	return sum
}

// templateVerify is one SPROUTFS_VERIFY line.
type templateVerify struct {
	pages, stamped, bad, sum, ns uint64
	first                        string
}

func parseTemplateVerify(line string) (templateVerify, error) {
	fields := map[string]string{}
	for _, field := range strings.Fields(line)[1:] {
		key, value, _ := strings.Cut(field, "=")
		fields[key] = value
	}
	var v templateVerify
	for key, into := range map[string]*uint64{"pages": &v.pages, "stamped": &v.stamped, "bad": &v.bad,
		"sum": &v.sum, "ns": &v.ns} {
		value, err := strconv.ParseUint(fields[key], 10, 64)
		if err != nil {
			return templateVerify{}, fmt.Errorf("the guest's %s in %q: %w", key, line, err)
		}
		*into = value
	}
	v.first = fields["first"]
	return v, nil
}

// templateFork is one VM created from the template, from its fork to its
// answers.
type templateFork struct {
	id     string
	mark   uint64
	phase  uint64
	config vmmachine.Config
	// process is the restored VMM, nil until it has started.
	process *vmmachine.Process
	// restore is how long its VMM took to start from the template's state.
	restore time.Duration
	// released is when its guest was resumed, from the barrier's opening.
	released time.Duration
	// steps is how long each step took on the host's clock, and verifies what
	// each verify reported.
	steps    []time.Duration
	verifies []templateVerify
	err      error
}

// TestForksOfOneColdTemplateStartAtOnceAndReadEveryPage starts several VMs
// from one template checkpoint on one host at the same moment, over a pager
// that has never seen it, and has every guest read every page of it.
//
// It is what an embedder hit in TASK-105: VMs created from one template on one
// host at once fault the same pages within a millisecond of each other, and a
// prefetch's READ met another and the pager panicked, taking every VM on the
// host with it. The seeded campaigns admit one guest at a time at named points;
// this runs real guests, a real userfaultfd and real parallelism instead.
//
// The template is a guest that filled half its RAM with words that name their
// page, published by a stop. The forks land on one fresh pager, nothing of the
// template resident, with a host's read-ahead and prefetch, an arena smaller
// than what they hold between them and dirty budgets that hold their stores:
// so they share the template's pages, prefetch them, and evict them from each
// other. Every fork is restored, and then one barrier releases them all. Each
// reads every page of the pattern, stores its own mark over an eighth of it,
// and reads every page twice more. Each read must find the template's bytes on
// every page the fork did not store into and its own on every page it did, and
// every fork must answer within templateForkPhase.
func TestForksOfOneColdTemplateStartAtOnceAndReadEveryPage(t *testing.T) {
	binaryPath := os.Getenv("SPROUTFS_FIRECRACKER")
	if binaryPath == "" {
		t.Skip("run the Firecracker qualification script")
	}
	forks := templateForks(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Minute)
	defer cancel()
	c := newMigrationClusterOver(t, ctx, templateStore)
	sums := newTemplateSums()

	// The template: a guest that fills its pattern, checks it once, and is
	// stopped, its memory and VMM state published by the stop's checkpoint.
	parent, err := c.source.Create(ctx, "cold-template", []volume.VolumeSpec{
		{Name: vmmachine.RAMVolume, Size: templateRAM, PageSize: ramPageBytes(t)},
		{Name: "root", Size: guestRootBytes, PageSize: pmemPageBytes(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	loadRootImage(t, ctx, parent.Volume("root"))
	if err := parent.Checkpoint(ctx); err != nil {
		t.Fatal(err)
	}
	// The template's own pager holds it whole: nothing here is about it.
	sourcePagers := newSizedMigrationPager(t, ctx, templateRAM, 128<<20, 2*templateRAM, templateRAM)
	template, err := vmmachine.Start(ctx, migrationConfig(t, binaryPath, sourcePagers, parent))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = template.Close() })
	waitLine(t, ctx, template, "SPROUTFS_READY ram=7 disk=0 dax=1 root=pmem", 0)
	command(t, ctx, template, fmt.Sprintf("pattern %d\n", templatePatternMiB),
		fmt.Sprintf("SPROUTFS_PATTERN pages=%d", templatePages))
	filled := verifyOnce(t, ctx, template)
	if filled.bad != 0 || filled.sum != sums.template {
		t.Fatalf("the template reads %+v of the pattern it filled, want no page wrong and sum %d",
			filled, sums.template)
	}
	stop, err := parent.Snapshot(ctx, func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		return template.Prepare(ctx)
	}, volume.Terms{})
	if err != nil {
		t.Fatalf("the template's stop: %v\n%s", err, consoleText(template))
	}
	if err := stop.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := template.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("template: %s published, pattern %d pages", stop.Ref(), templatePages)

	// One host's pagers, cold. The RAM arena holds the pattern and half of
	// what the forks' stores make private, which is less than they hold between
	// them, so they evict each other's pages. The dirty budgets hold every page
	// each fork may store into, its whole RAM and its whole root, since nothing
	// here checkpoints either to relieve them: a guest kernel's own writes touch
	// many more 2 MiB pages than its stamps, and a budget sized to the stamps
	// stalled every fork's first verify on GCE (2026-10-08).
	ramArena := templatePatternMiB<<20 + forks*templateStampBytes/2
	pagers := newConfiguredHostPagers(t, ctx, hostPagersConfig{
		RAM: hostPagerBudgets{Arena: uint64(ramArena), Logical: uint64(forks*templateRAM + 64<<20),
			Dirty: uint64(forks * templateRAM)},
		PMEM: hostPagerBudgets{Arena: 128 << 20, Logical: uint64(forks*guestRootBytes + 64<<20),
			Dirty: uint64(forks * guestRootBytes)},
		HostReadAhead: true,
	})

	// Every fork is created as a host creates a VM from another VM's
	// checkpoint: a point over the published checkpoint, the fork, its root
	// published before its guest exists, and the state that root names.
	taken := make([]*templateFork, forks)
	for index := range taken {
		id := fmt.Sprintf("cold-fork-%02d", index)
		point, err := c.destination.InheritPublished(ctx, id, stop.Ref())
		if err != nil {
			t.Fatal(err)
		}
		vm, err := c.destination.Fork(ctx, id, point)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = vm.Close(context.Background()) })
		if err := vm.Checkpoint(ctx); err != nil {
			t.Fatalf("publishing the root of %s: %v", id, err)
		}
		state, err := readRootState(ctx, c, vm.Status().Checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		config := migrationConfig(t, binaryPath, pagers, vm)
		config.RestoreState = state
		taken[index] = &templateFork{id: id, mark: uint64(index + 1), phase: uint64(index % templatePhases),
			config: config}
	}

	// Every VMM is started from the template's state at once, and each waits,
	// restored and paused, until all of them are; one barrier then releases
	// every guest, and each goes straight on to its reads.
	barrier := make(chan struct{})
	var restored, finished sync.WaitGroup
	// phase and opened are set before the barrier opens, and read only after.
	var phase context.Context
	var opened time.Time
	for _, fork := range taken {
		restored.Add(1)
		finished.Go(func() {
			began := time.Now()
			fork.process, fork.err = vmmachine.Start(ctx, fork.config)
			fork.restore = time.Since(began)
			restored.Done()
			<-barrier
			if fork.err == nil {
				fork.err = fork.run(phase, sums, opened)
			}
		})
	}
	restored.Wait()
	for _, fork := range taken {
		if fork.process != nil {
			t.Cleanup(func() { _ = fork.process.Close() })
		}
	}
	phase, endPhase := context.WithTimeoutCause(ctx, templateForkPhase(forks),
		errors.New("a fork did not answer within the phase's bound"))
	defer endPhase()
	opened = time.Now()
	close(barrier)
	finished.Wait()
	elapsed := time.Since(opened)

	for _, fork := range taken {
		if fork.err == nil {
			continue
		}
		console := []byte(nil)
		if fork.process != nil {
			console = consoleText(fork.process)
		}
		t.Errorf("%s: %v\nconsole:\n%s", fork.id, fork.err, console)
	}
	if t.Failed() {
		t.Fatalf("not every fork of the template read back what it should\n%s", stacks())
	}

	// No memory region ended terminal, and the pager never stalled a store.
	for _, fork := range taken {
		for name, region := range fork.process.MemoryRegions() {
			if err := region.Verify(ctx); err != nil {
				t.Errorf("%s's %s memory region: %v", fork.id, name, err)
			}
		}
	}
	var released []time.Duration
	for _, fork := range taken {
		released = append(released, fork.released)
		t.Logf("template fork %s: restore=%s released=+%s steps=%v guest_ns=%v",
			fork.id, fork.restore, fork.released, fork.steps, guestTimes(fork.verifies))
	}
	ram, err := pagers.pagers.Ram.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("template forks: forks=%d ram_page=%d phase=%s released_within=%s",
		forks, ramPageBytes(t), elapsed, slices.Max(released)-slices.Min(released))
	t.Logf("template forks ram pager: faults=%d loads=%d loaded_pages=%d identity_hits=%d evictions=%d spills=%d "+
		"refaults=%d copy_on_writes=%d prefetches=%d prefetched_pages=%d prefetch_mapped=%d prefetch_waits=%d "+
		"prefetch_refused=%d prefetch_cancelled=%d prefetch_dropped=%d prefetch_random=%d dirty_waits=%d "+
		"dirty_stalls=%d peak_resident=%d peak_dirty=%d",
		ram.Faults, ram.Loads, ram.LoadedPages, ram.IdentityHits, ram.Evictions, ram.Spills, ram.SpillRefaults,
		ram.CopyOnWrites, ram.Prefetches, ram.PrefetchedPages, ram.PrefetchMapped, ram.PrefetchWaits,
		ram.PrefetchRefused, ram.PrefetchCancelled, ram.PrefetchDropped, ram.PrefetchRandom, ram.DirtyWaits,
		ram.DirtyStalls, ram.PeakResidentPages, ram.PeakDirtyPages)
	if ram.DirtyStalls != 0 {
		t.Errorf("the RAM pager stalled %d stores, so a fork was stopped for want of a dirty budget", ram.DirtyStalls)
	}
	// What the run is for: forks sharing the template's pages, prefetching
	// them and evicting them from each other. A run that did none of them
	// tested nothing this suite is about.
	if ram.IdentityHits == 0 || ram.Prefetches == 0 || ram.Evictions == 0 {
		t.Errorf("the forks shared %d pages, prefetched %d runs and evicted %d pages; want all three",
			ram.IdentityHits, ram.Prefetches, ram.Evictions)
	}
}

// templateForkPhase bounds the phase from the barrier to the last fork's last
// answer. It separates slow from stopped: a fork whose read never returns is
// a guest nothing can tell from a dead one.
//
// It is provisional until measured on the qualification host.
func templateForkPhase(forks int) time.Duration {
	return time.Duration(forks) * time.Minute
}

// run is one fork from its release to its last answer.
func (f *templateFork) run(ctx context.Context, sums templateSums, opened time.Time) error {
	if err := f.process.Release(ctx); err != nil {
		return fmt.Errorf("resuming: %w", err)
	}
	f.released = time.Since(opened)
	reader := newConsole(f.process)
	defer reader.close()
	if err := reader.skipExisting(); err != nil {
		return err
	}
	stamped := false
	for index, step := range templateForkSteps {
		line := step.command
		if line == "stamp" {
			line = fmt.Sprintf("stamp %d %d %d", f.mark, templateStride, f.phase)
		}
		began := time.Now()
		if err := reader.send(ctx, line); err != nil {
			return fmt.Errorf("step %d (%s): %w", index, line, err)
		}
		answer, err := reader.wait(ctx, step.answer)
		f.steps = append(f.steps, time.Since(began))
		if err != nil {
			return fmt.Errorf("step %d (%s): %w", index, line, err)
		}
		if step.command == "stamp" {
			// The serial console ends its lines with a carriage return.
			answer = strings.TrimSuffix(answer, "\r")
			if want := fmt.Sprintf("SPROUTFS_STAMP mark=%d pages=%d", f.mark, templatePages/templateStride); answer != want {
				return fmt.Errorf("step %d: the guest answered %q, want %q", index, answer, want)
			}
			stamped = true
			continue
		}
		got, err := parseTemplateVerify(answer)
		if err != nil {
			return err
		}
		f.verifies = append(f.verifies, got)
		want := templateVerify{pages: templatePages, sum: sums.template, ns: got.ns, first: "-"}
		if stamped {
			want.stamped, want.sum = templatePages/templateStride, sums.stamped(f.mark, f.phase)
		}
		if got != want {
			return fmt.Errorf("step %d: the guest read %+v, want %+v", index, got, want)
		}
	}
	return nil
}

// verifyOnce asks a guest to read its pattern back once.
func verifyOnce(t *testing.T, ctx context.Context, p *vmmachine.Process) templateVerify {
	t.Helper()
	reader := newConsole(p)
	defer reader.close()
	if err := reader.skipExisting(); err != nil {
		t.Fatal(err)
	}
	if err := reader.send(ctx, "verify"); err != nil {
		t.Fatal(err)
	}
	line, err := reader.wait(ctx, "SPROUTFS_VERIFY")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseTemplateVerify(line)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// readRootState is the VMM state a published root names.
func readRootState(ctx context.Context, c *migrationCluster, ref control.Ref) ([]byte, error) {
	root, err := c.store.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	return c.store.ReadState(ctx, root)
}

// guestTimes is how long each verify took on the guest's own clock.
func guestTimes(verifies []templateVerify) []time.Duration {
	var times []time.Duration
	for _, v := range verifies {
		times = append(times, time.Duration(v.ns))
	}
	return times
}
