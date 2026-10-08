package vmmemory_test

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/semistrict/sproutfs/platform/sim"
)

// campaignRun is what one run of a campaign's seed reports: the probes it
// reached, the sites it fired, and the order its scheduler released every
// operation in.
type campaignRun struct {
	probes, fired map[string]uint64
	recording     sim.Recording
}

// runCampaign runs a campaign's world on one seed, every operation of it
// released by a scheduler of that seed, and the spill file's disk with them.
// world's context carries the runtime, with every fault site on.
func runCampaign(t *testing.T, seed uint64, world func(ctx context.Context, disk *sim.Disk)) campaignRun {
	scheduler := sim.NewScheduler(seed)
	runtime := sim.New(sim.Config{Seed: seed, Wait: scheduler.Wait, Buggify: true})
	done := make(chan struct{})
	go func() {
		defer close(done)
		world(sim.WithRuntime(t.Context(), runtime), scheduledSpillDisk(seed, scheduler, done))
	}()
	if err := scheduler.Run(done); err != nil {
		t.Fatal(err)
	}
	recording, err := scheduler.Recording(nil)
	if err != nil {
		t.Fatal(err)
	}
	return campaignRun{probes: runtime.Probes(), fired: runtime.FiredSites(), recording: recording}
}

// testReplays runs each seed of a campaign twice and requires the two runs to
// release every operation in the same order and reach the same probes the
// same number of times: what a seed reaches is then the seed's, not the Go
// scheduler's. Each seed must inject a fault its client cannot survive, a
// lost command or a refused revocation, which before 2026-10-08 no seed did
// and replayed: the region's failure and the detach of a lost guest went on
// beside the other guests in Go-scheduler order (TASK-111).
func testReplays(t *testing.T, seeds []uint64, campaign func(t *testing.T, seed uint64) campaignRun) {
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			var runs [2]campaignRun
			for run := range 2 {
				synctest.Test(t, func(t *testing.T) { runs[run] = campaign(t, seed) })
			}
			requireReplay(t, seed, [2]sim.Recording{runs[0].recording, runs[1].recording})
			if !maps.Equal(runs[0].probes, runs[1].probes) {
				t.Fatalf("seed %d reached %v, then %v", seed, runs[0].probes, runs[1].probes)
			}
			if !injectsTerminalFault(runs[0].fired) {
				t.Fatalf("seed %d injected no fault its client cannot survive; it fired %v", seed, runs[0].fired)
			}
		})
	}
}

// injectsTerminalFault reports whether a run fired a test client's fault that
// ends its region: a lost command or a refused revocation.
func injectsTerminalFault(fired map[string]uint64) bool {
	for site := range fired {
		if strings.HasPrefix(site, "vmmemory-test/client-command-lost/") ||
			strings.HasPrefix(site, "vmmemory-test/client-out-of-mappings/revoke") {
			return true
		}
	}
	return false
}

// requireReplay fails unless two runs of a seed released every operation in
// the same order. A failure prints the first operation the runs differ at and
// the ones before it, which is where a search for what the Go scheduler chose
// begins.
func requireReplay(t *testing.T, seed uint64, runs [2]sim.Recording) {
	t.Helper()
	if bytes.Equal(runs[0].Execution, runs[1].Execution) {
		return
	}
	var texts [2][]string
	for run, recording := range runs {
		var text strings.Builder
		if err := recording.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		texts[run] = strings.Split(text.String(), "\n")
	}
	a, b := texts[0], texts[1]
	at := 0
	for at < min(len(a), len(b)) && a[at] == b[at] {
		at++
	}
	from := max(0, at-12)
	t.Fatalf("seed %d released its operations in another order on its second run, from line %d:\n"+
		"first run:\n%s\nsecond run:\n%s", seed, at,
		strings.Join(a[from:min(len(a), at+6)], "\n"), strings.Join(b[from:min(len(b), at+6)], "\n"))
}
