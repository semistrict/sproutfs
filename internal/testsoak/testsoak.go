// Package testsoak runs one seed of a soak campaign and reports what it cost.
//
// A campaign is a seed range rather than a fixed list: the nightly workflow
// splits the range into blocks and gives each job its own base and count, so
// widening a sweep is a workflow edit rather than a code edit.
//
// Every seed reports the simulated time the run consumed against the wall time
// it took. The ratio is what says whether a block of seeds still fits inside a
// job's timeout, and a seed whose ratio collapses is a real wait that crept
// into a virtual-time test.
package testsoak

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/internal/platform/sim"
)

// Environment variables. SPROUTFS_TEST_SOAK enables the campaigns at all, the
// next two select the block of seeds one process runs, and the last names the
// directory the per-seed records are appended to.
const (
	EnableVar  = "SPROUTFS_TEST_SOAK"
	BaseVar    = "SPROUTFS_SOAK_SEED_BASE"
	CountVar   = "SPROUTFS_SOAK_SEED_COUNT"
	SummaryVar = "SPROUTFS_SOAK_SUMMARY_DIR"
)

// Block is the half-open seed range [Base, Base+Count) one process runs.
type Block struct {
	Base  uint64
	Count uint64
}

// Seeds yields every seed in the block, in order.
func (b Block) Seeds(yield func(uint64) bool) {
	for seed := b.Base; seed < b.Base+b.Count; seed++ {
		if !yield(seed) {
			return
		}
	}
}

// Enabled reports whether the extended campaigns were asked for.
func Enabled() bool { return os.Getenv(EnableVar) != "" }

// Require skips unless the extended campaigns were asked for, then reports the
// block of seeds this process runs. count is the block size the campaign uses
// when the environment names none.
func Require(t *testing.T, count uint64) Block {
	t.Helper()
	if !Enabled() {
		t.Skipf("set %s=1 for the extended campaign", EnableVar)
	}
	block := Block{Base: 1, Count: count}
	if value := os.Getenv(BaseVar); value != "" {
		base, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			t.Fatalf("%s=%q: %v", BaseVar, value, err)
		}
		block.Base = base
	}
	if value := os.Getenv(CountVar); value != "" {
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil || parsed == 0 {
			t.Fatalf("%s=%q must be a positive count", CountVar, value)
		}
		block.Count = parsed
	}
	if block.Base+block.Count < block.Base {
		t.Fatalf("seed block %d+%d overflows", block.Base, block.Count)
	}
	t.Logf("soak block seeds %d..%d", block.Base, block.Base+block.Count-1)
	return block
}

// Summary is one seed's record in a campaign's report.
type Summary struct {
	Campaign    string `json:"campaign"`
	Seed        uint64 `json:"seed"`
	SimulatedNS int64  `json:"simulated_ns"`
	WallNS      int64  `json:"wall_ns"`
	Ratio       string `json:"ratio"`
	Events      int    `json:"trace_events"`
	Fingerprint string `json:"fingerprint"`
	Failed      bool   `json:"failed"`
}

// Bubble measures one seed whose runner owns its virtual-time bubble. Start it
// before the bubble, hand it the bubble's own start instant from inside, and
// Report once the bubble has ended:
//
//	bubble := testsoak.Start("scheduled-host", seed)
//	synctest.Test(t, func(t *testing.T) {
//		defer bubble.Simulated(time.Now())
//		...
//	})
//	bubble.Report(t, runtime)
//
// The split exists because the two clocks are only readable from their own
// side: inside the bubble time.Now is the simulated clock, outside it is the
// wall clock.
type Bubble struct {
	summary   Summary
	started   time.Time
	simulated time.Duration
}

// Start begins measuring one seed of campaign.
func Start(campaign string, seed uint64) *Bubble {
	return &Bubble{summary: Summary{Campaign: campaign, Seed: seed}, started: time.Now()}
}

// Simulated records the simulated time consumed since begin. Call it inside the
// bubble, where begin was taken; deferring it also covers a run that fails.
func (b *Bubble) Simulated(begin time.Time) { b.simulated = time.Since(begin) }

// Report logs this seed's line and appends it to the campaign's summary file
// when SPROUTFS_SOAK_SUMMARY_DIR names a directory. runtime is the runtime
// whose trace summarizes the run, or nil when the campaign kept none.
func (b *Bubble) Report(t *testing.T, runtime *sim.Runtime) {
	t.Helper()
	s := b.summary
	s.SimulatedNS = int64(b.simulated)
	s.WallNS = int64(time.Since(b.started))
	s.Ratio = ratio(s.SimulatedNS, s.WallNS)
	s.Failed = t.Failed()
	if runtime != nil {
		events := runtime.Trace().Events()
		s.Events = len(events)
		s.Fingerprint = fingerprint(events)
	}
	t.Logf("soak campaign=%s seed=%d simulated=%s wall=%s ratio=%s events=%d fingerprint=%s",
		s.Campaign, s.Seed, time.Duration(s.SimulatedNS), time.Duration(s.WallNS),
		s.Ratio, s.Events, s.Fingerprint)
	if dir := os.Getenv(SummaryVar); dir != "" {
		if err := s.append(dir); err != nil {
			t.Fatal(err)
		}
	}
}

// Measure is Bubble for a runner that does not own a bubble: it creates one,
// runs fn inside it, and reports the seed. fn returns the runtime whose trace
// summarizes the run, or nil.
func Measure(t *testing.T, campaign string, seed uint64, fn func(t *testing.T) *sim.Runtime) {
	t.Helper()
	bubble := Start(campaign, seed)
	var runtime *sim.Runtime
	synctest.Test(t, func(t *testing.T) {
		defer bubble.Simulated(time.Now())
		runtime = fn(t)
	})
	bubble.Report(t, runtime)
}

// ratio is simulated time per unit of wall time: how much faster than real time
// the campaign explored its faults.
func ratio(simulated, wall int64) string {
	if wall <= 0 {
		return "inf"
	}
	// Campaigns straddle several orders of magnitude here: a workload whose
	// faults are microseconds apart explores far less simulated time than it
	// costs, while one that waits out a timer explores far more.
	return strconv.FormatFloat(float64(simulated)/float64(wall), 'g', 3, 64) + "x"
}

// fingerprint hashes the adapter trace so two runs of one seed can be compared
// without keeping either trace. It summarizes a run; the cross-process
// determinism checks compare complete recordings byte for byte.
func fingerprint(events []sim.Event) string {
	h := fnv.New64a()
	for _, event := range events {
		fmt.Fprintf(h, "%d|%d|%s|%s|%s|%s|%d|%d\n", event.Sequence, event.At.UnixNano(),
			event.Kind, event.Resource, event.Operation, event.Outcome, event.Bytes, event.LocalID)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

func (s Summary) append(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(s)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(dir, s.Campaign+".jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
