//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/vmmemory"
	"github.com/semistrict/sproutfs/internal/volume"
)

// The database scenario: a guest seeds an in-memory key-value store, is
// checkpointed, and is forked once per test run, the way a preview environment
// or a test suite takes its own copy of a seeded database. Each fork then
// updates keys chosen at random, in place, a few bytes at a time. Every update
// is a small store into a page the fork inherited, somewhere in gigabytes of
// them, which is where the size of a RAM page decides what a store costs: a
// fork copies the whole page an update lands in, so at 2 MiB its first update
// in each range copies 2 MiB for eight bytes, and at 4 KiB it copies 4 KiB.
const (
	// defaultDBKeys and dbValueBytes are the seeded database: six million
	// values of 1 KiB, which Valkey holds in about 8 GiB of the guest's 16.
	defaultDBKeys = 6_000_000
	dbValueBytes  = 1024
)

// dbUpdateSteps are how many updates each fork makes, one step after another:
// what a fork holds and how its updates are served is recorded after each, so
// the record is a curve of cost against updates made rather than one point.
var dbUpdateSteps = []int{1_000, 10_000, 100_000}

// dbKeys is SPROUTFS_BENCH_DB_KEYS where it names a positive count, which is
// how a smoke run seeds a database it can build in seconds.
func (b *benchmark) dbKeys() int {
	b.t.Helper()
	value := os.Getenv("SPROUTFS_BENCH_DB_KEYS")
	if value == "" {
		return defaultDBKeys
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		b.t.Fatalf("invalid SPROUTFS_BENCH_DB_KEYS %q", value)
	}
	return parsed
}

// dbLatency is one step's updates as valkey-benchmark reports them.
type dbLatency struct {
	RPS   float64 `json:"rps"`
	AvgMS float64 `json:"avg_ms"`
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
	MaxMS float64 `json:"max_ms"`
}

// parseDBLatency reads valkey-benchmark's CSV: a header naming its columns and
// one line of values, among whatever else it printed.
func parseDBLatency(output string) (dbLatency, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i, line := range lines {
		if !strings.Contains(line, "p99_latency_ms") || i+1 >= len(lines) {
			continue
		}
		names := strings.Split(strings.ReplaceAll(line, `"`, ""), ",")
		values := strings.Split(strings.ReplaceAll(lines[i+1], `"`, ""), ",")
		if len(values) != len(names) {
			return dbLatency{}, fmt.Errorf("valkey-benchmark printed %d columns under %d names: %q", len(values), len(names), output)
		}
		column := map[string]float64{}
		for k, name := range names {
			if k == 0 {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(values[k]), 64)
			if err != nil {
				return dbLatency{}, fmt.Errorf("valkey-benchmark's %s: %w", name, err)
			}
			column[strings.TrimSpace(name)] = parsed
		}
		return dbLatency{RPS: column["rps"], AvgMS: column["avg_latency_ms"], P50MS: column["p50_latency_ms"],
			P95MS: column["p95_latency_ms"], P99MS: column["p99_latency_ms"], MaxMS: column["max_latency_ms"]}, nil
	}
	return dbLatency{}, fmt.Errorf("valkey-benchmark printed no latency: %q", output)
}

// dbSeed starts the store in a running guest and seeds it, returning what the
// store reports it holds.
func (b *benchmark) dbSeed(ctx context.Context, c *console) string {
	b.t.Helper()
	b.run(ctx, c, "/opt/db/start.sh")
	ctx, cancel := context.WithTimeout(ctx, benchCommandTimeout)
	defer cancel()
	output, err := c.capture(ctx, fmt.Sprintf("/opt/db/seed.sh %d %d", b.dbKeys(), dbValueBytes))
	if err != nil {
		b.t.Fatalf("seeding the database: %v", err)
	}
	if !strings.Contains(output, fmt.Sprintf("errors: 0, replies: %d", b.dbKeys())) {
		b.t.Fatalf("seeding the database: %q", output)
	}
	return strings.TrimSpace(output)
}

// dbSteadyUpdates is how many updates the seeded guest itself makes, four
// clients each keeping 32 in flight: with every page already its own and
// mapped, nothing faults, and what the updates cost is the guest's own memory
// access, which is what the size of its translations decides.
const dbSteadyUpdates = 4_000_000

// dbSteady runs those updates in the guest that seeded the database and
// reports their rate and latency.
func (b *benchmark) dbSteady(ctx context.Context, c *console) dbLatency {
	b.t.Helper()
	ctx, cancel := context.WithTimeout(ctx, benchCommandTimeout)
	defer cancel()
	output, err := c.capture(ctx, fmt.Sprintf("valkey-benchmark -s /tmp/valkey.sock -c 4 -P 32 -n %d -r %d --csv "+
		"SETRANGE key:__rand_int__ 0 sproutfs", dbSteadyUpdates, b.dbKeys()))
	if err != nil {
		b.t.Fatalf("steady database updates: %v", err)
	}
	latency, err := parseDBLatency(output)
	if err != nil {
		b.t.Fatal(err)
	}
	return latency
}

// dbUpdate runs one step's updates in every guest at once, and reports each
// guest's latency.
func (b *benchmark) dbUpdate(ctx context.Context, consoles []*console, updates int) []dbLatency {
	b.t.Helper()
	latency := make([]dbLatency, len(consoles))
	errs := make([]error, len(consoles))
	var wg sync.WaitGroup
	for index, c := range consoles {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, benchCommandTimeout)
			defer cancel()
			output, err := c.capture(ctx, fmt.Sprintf("/opt/db/update.sh %d %d", updates, b.dbKeys()))
			if err != nil {
				errs[index] = err
				return
			}
			latency[index], errs[index] = parseDBLatency(output)
		})
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			b.t.Fatalf("database updates in guest %d: %v", index, err)
		}
	}
	return latency
}

// dbFork seeds the database in a fork of the origin, checkpoints it, forks that
// checkpoint once per fork, and runs each step of updates in all of them at
// once. After every step it records each fork's latency and what the fork holds
// privately; after the last, a checkpoint of each fork, which is what its
// updates cost the object store.
func (b *benchmark) dbFork(ctx context.Context, origin *forkOrigin) {
	b.t.Helper()
	start := b.sample(ctx)
	seeder, seederVM, seederConsole, _, _ := b.fork(ctx, origin, "db-seed")
	seeded := b.dbSeed(ctx, seederConsole)
	b.record(ctx, "db-seed", "sproutfs", start, nil, map[string]any{"keys": b.dbKeys(),
		"value_bytes": dbValueBytes, "store": seeded})
	start = b.sample(ctx)
	steady := b.dbSteady(ctx, seederConsole)
	b.record(ctx, "db-steady", "sproutfs", start, nil, map[string]any{"updates": dbSteadyUpdates, "latency": steady})

	start = b.sample(ctx)
	ckpt, captureExtra := b.capture(ctx, seeder, seederVM)
	b.record(ctx, "db-capture", "sproutfs", start, nil, captureExtra)
	point, err := b.manager.Inherit(ctx, ckpt.Ref())
	if err != nil {
		b.t.Fatal(err)
	}
	if err := point.Hold(); err != nil {
		b.t.Fatal(err)
	}
	defer func() {
		if err := point.Retire(context.WithoutCancel(ctx)); err != nil {
			b.t.Error(err)
		}
	}()
	seederConsole.close()
	if err := seeder.Close(); err != nil {
		b.t.Fatal(err)
	}
	defer func() {
		if err := seederVM.Close(context.WithoutCancel(ctx)); err != nil {
			b.t.Error(err)
		}
	}()
	seededOrigin := &forkOrigin{point: point, state: ckpt.State()}

	count := b.forks()
	processes := make([]*vmmachine.Process, count)
	vms := make([]*volume.VM, count)
	consoles := make([]*console, count)
	start = b.sample(ctx)
	for index := range count {
		processes[index], vms[index], consoles[index], _, _ = b.fork(ctx, seededOrigin, "db-fork-"+strconv.Itoa(index))
	}
	b.record(ctx, "db-fork-restore", "sproutfs", start, nil, map[string]any{"forks": count})
	defer func() {
		for index := range count {
			consoles[index].close()
			if err := processes[index].Close(); err != nil {
				b.t.Error(err)
			}
			if err := vms[index].Close(context.WithoutCancel(ctx)); err != nil {
				b.t.Error(err)
			}
		}
	}()

	made := 0
	for _, updates := range dbUpdateSteps {
		start = b.sample(ctx)
		latency := b.dbUpdate(ctx, consoles, updates)
		made += updates
		private := make([]uint64, count)
		ramPrivate := make([]uint64, count)
		for index, p := range processes {
			for _, region := range p.Regions() {
				stats, err := region.Stats(ctx)
				if err != nil {
					b.t.Fatal(err)
				}
				private[index] += stats.PrivateBytes()
				if region.Kind() == vmmemory.Ram {
					ramPrivate[index] += stats.PrivateBytes()
				}
			}
		}
		b.record(ctx, "db-update-"+strconv.Itoa(made), "sproutfs", start, nil, map[string]any{
			"updates": updates, "updates_made": made, "latency": latency,
			"fork_private_bytes": private, "fork_ram_private_bytes": ramPrivate})
	}

	start = b.sample(ctx)
	published := make([]uint64, count)
	// What each fork's checkpoint paused it for, and how many mappings its
	// VMM held when it did: a fork whose scattered stores are mappings of their
	// own is write-protected one run at a time.
	pauses := make([]any, count)
	mappings := make([]int, count)
	for index := range count {
		mappings[index] = countMappings(processes[index].PID())
		forked, extra := b.capture(ctx, processes[index], vms[index])
		_, published[index] = forked.Sealed()
		pauses[index] = extra["pause_ns"]
	}
	b.record(ctx, "db-fork-checkpoint", "sproutfs", start, nil, map[string]any{
		"updates_made": made, "published_bytes": published, "pause_ns": pauses, "vmm_mappings": mappings})
}

// baselineDB is the database scenario on plain Firecracker: a guest booted from
// its own copy of the image seeds the store, is snapshotted to a memory file,
// and is restored as many times as the managed side forks, each clone over a
// copy of the root of its own. The clones take the same steps of updates at
// once, and after each what the kernel accounts to each clone is recorded
// beside their latency.
func (b *benchmark) baselineDB(ctx context.Context, config plainConfig) {
	b.t.Helper()
	root := filepath.Join(b.work, "baseline-db-root.ext4")
	if err := copySparse(b.imagePath, root); err != nil {
		b.t.Fatal(err)
	}
	defer os.Remove(root)
	config.RootPath = root
	at := time.Now()
	p, err := startPlainVM(ctx, config)
	if err != nil {
		b.t.Fatal(err)
	}
	c := newConsole(p)
	if _, err := c.wait(ctx, "SPROUTFS_READY"); err != nil {
		b.t.Fatalf("baseline database boot: %v", err)
	}
	seeded := b.dbSeed(ctx, c)
	b.appendRecord(benchRecord{Scenario: "db-seed", Kind: "baseline", WallNS: time.Since(at).Nanoseconds(),
		Extra: map[string]any{"keys": b.dbKeys(), "value_bytes": dbValueBytes, "store": seeded}})
	at = time.Now()
	steady := b.dbSteady(ctx, c)
	b.appendRecord(benchRecord{Scenario: "db-steady", Kind: "baseline", WallNS: time.Since(at).Nanoseconds(),
		Extra: map[string]any{"updates": dbSteadyUpdates, "latency": steady, "huge_pages": config.HugePages}})
	if config.HugePages == "2M" {
		// Firecracker restores a guest on the HugeTLB pool only through a
		// userfaultfd, never from a memory file, so a plain guest on huge
		// pages has no clones to measure.
		if err := p.Close(); err != nil {
			b.t.Fatal(err)
		}
		return
	}

	statePath := filepath.Join(b.work, "baseline-db.state")
	memoryPath := filepath.Join(b.work, "baseline-db.mem")
	defer os.Remove(statePath)
	defer os.Remove(memoryPath)
	at = time.Now()
	if err := p.Snapshot(ctx, statePath, memoryPath); err != nil {
		b.t.Fatalf("baseline database snapshot: %v", err)
	}
	b.appendRecord(benchRecord{Scenario: "db-capture", Kind: "baseline", WallNS: time.Since(at).Nanoseconds()})
	if err := p.Close(); err != nil {
		b.t.Fatal(err)
	}

	count := b.forks()
	clones := make([]*plainVM, count)
	consoles := make([]*console, count)
	at = time.Now()
	for index := range count {
		cloneRoot := filepath.Join(b.work, "baseline-db-clone-"+strconv.Itoa(index)+".ext4")
		if err := copySparse(root, cloneRoot); err != nil {
			b.t.Fatal(err)
		}
		defer os.Remove(cloneRoot)
		cloneConfig := config
		cloneConfig.SnapshotPath, cloneConfig.MemoryPath, cloneConfig.CloneRoot = statePath, memoryPath, cloneRoot
		clone, err := startPlainVM(ctx, cloneConfig)
		if err != nil {
			b.t.Fatalf("baseline database clone %d: %v", index, err)
		}
		defer clone.Close()
		clones[index] = clone
		consoles[index] = newConsole(clone)
		if err := consoles[index].skipExisting(); err != nil {
			b.t.Fatal(err)
		}
	}
	b.appendRecord(benchRecord{Scenario: "db-fork-restore", Kind: "baseline", WallNS: time.Since(at).Nanoseconds(),
		Extra: map[string]any{"forks": count}})

	made := 0
	for _, updates := range dbUpdateSteps {
		at = time.Now()
		latency := b.dbUpdate(ctx, consoles, updates)
		made += updates
		memory := make([]map[string]int64, count)
		for index, clone := range clones {
			if memory[index], err = clone.memoryRollup(); err != nil {
				b.t.Fatal(err)
			}
		}
		b.appendRecord(benchRecord{Scenario: "db-update-" + strconv.Itoa(made), Kind: "baseline",
			WallNS: time.Since(at).Nanoseconds(), Extra: map[string]any{
				"updates": updates, "updates_made": made, "latency": latency, "clone_memory": memory}})
	}
}

// The latency a record carries is valkey-benchmark's own CSV, read by column
// name rather than by position.
func TestDBLatencyIsReadFromValkeyBenchmarksCSV(t *testing.T) {
	output := "WARNING: Could not fetch server CONFIG\n" +
		`"test","rps","avg_latency_ms","min_latency_ms","p50_latency_ms","p95_latency_ms","p99_latency_ms","max_latency_ms"` + "\n" +
		`"SETRANGE key:__rand_int__ 0 sproutfs","40000.00","0.024","0.008","0.015","0.031","0.055","1.207"` + "\n"
	got, err := parseDBLatency(output)
	if err != nil {
		t.Fatal(err)
	}
	want := dbLatency{RPS: 40000, AvgMS: 0.024, P50MS: 0.015, P95MS: 0.031, P99MS: 0.055, MaxMS: 1.207}
	if got != want {
		t.Fatalf("read %+v, want %+v", got, want)
	}
}
