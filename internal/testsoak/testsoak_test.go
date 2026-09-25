package testsoak_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/testsoak"
	"github.com/semistrict/sproutfs/platform/sim"
)

func TestRequireReportsTheBlockTheEnvironmentSelects(t *testing.T) {
	t.Setenv(testsoak.EnableVar, "1")
	t.Setenv(testsoak.BaseVar, "701")
	t.Setenv(testsoak.CountVar, "4")
	block := testsoak.Require(t, 100)
	if block != (testsoak.Block{Base: 701, Count: 4}) {
		t.Fatalf("block = %+v", block)
	}
	var seeds []uint64
	for seed := range block.Seeds {
		seeds = append(seeds, seed)
	}
	want := []uint64{701, 702, 703, 704}
	if len(seeds) != len(want) {
		t.Fatalf("seeds = %v, want %v", seeds, want)
	}
	for i, seed := range seeds {
		if seed != want[i] {
			t.Fatalf("seeds = %v, want %v", seeds, want)
		}
	}
}

func TestRequireDefaultsToTheCampaignsOwnBlock(t *testing.T) {
	t.Setenv(testsoak.EnableVar, "1")
	t.Setenv(testsoak.BaseVar, "")
	t.Setenv(testsoak.CountVar, "")
	if block := testsoak.Require(t, 100); block != (testsoak.Block{Base: 1, Count: 100}) {
		t.Fatalf("block = %+v", block)
	}
}

// A soak job's whole point is that it explores far more simulated time than the
// wall time it is given, so the reported ratio has to come from the two clocks
// separately: the simulated one inside the bubble, the wall one outside it.
func TestMeasureReportsSimulatedTimeFromInsideTheBubble(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(testsoak.SummaryVar, dir)
	runtime := sim.New(sim.Config{Seed: 5})
	testsoak.Measure(t, "unit", 5, func(t *testing.T) *sim.Runtime {
		time.Sleep(90 * time.Minute)
		return runtime
	})
	records := readSummaries(t, filepath.Join(dir, "unit.jsonl"))
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	got := records[0]
	if got.Campaign != "unit" || got.Seed != 5 {
		t.Fatalf("record = %+v", got)
	}
	if time.Duration(got.SimulatedNS) != 90*time.Minute {
		t.Fatalf("simulated = %s, want 1h30m0s", time.Duration(got.SimulatedNS))
	}
	if time.Duration(got.WallNS) > time.Minute {
		t.Fatalf("wall = %s, want a bubble that waited on no real clock", time.Duration(got.WallNS))
	}
	if got.Failed {
		t.Fatal("record reports a failure the run did not have")
	}
}

// The fingerprint is what a nightly job compares between two runs of one seed,
// so equal traces must give equal fingerprints and a differing trace must not.
func TestFingerprintFollowsTheTrace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(testsoak.SummaryVar, dir)
	for _, writes := range []int{4, 4, 5} {
		testsoak.Measure(t, "unit", 9, func(t *testing.T) *sim.Runtime {
			runtime := sim.New(sim.Config{Seed: 9})
			disk := runtime.NewDisk("fingerprint", sim.DiskConfig{})
			for range writes {
				if err := disk.SyncNamespace(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			return runtime
		})
	}
	records := readSummaries(t, filepath.Join(dir, "unit.jsonl"))
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	if records[0].Fingerprint != records[1].Fingerprint {
		t.Fatalf("equal traces fingerprinted %q and %q", records[0].Fingerprint, records[1].Fingerprint)
	}
	if records[0].Fingerprint == records[2].Fingerprint {
		t.Fatalf("a longer trace kept fingerprint %q", records[2].Fingerprint)
	}
	if records[0].Events == records[2].Events {
		t.Fatalf("event counts %d and %d, want the extra write counted",
			records[0].Events, records[2].Events)
	}
}

func readSummaries(t *testing.T, path string) []testsoak.Summary {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []testsoak.Summary
	decoder := json.NewDecoder(bytes.NewReader(data))
	for decoder.More() {
		var record testsoak.Summary
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}
