package vmmachine_test

import (
	"context"
	"os"
	"time"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/vmmachine"
	"github.com/semistrict/sproutfs/internal/volume"
)

// The disk-checkpoints scenario is what a host's interval costs against what a
// guest's disk actually changed. A disk is 2 MiB pages, so a checkpoint seals
// every page the guest stored into however little of it changed; the PMEM pager
// measures, at each settle, how many 4 KiB blocks of those pages the guest
// really changed; and the checkpoint uploads what is left after the settle,
// compressed. Each workload writes the disk its own way — a package manager's
// many small files, a compiler's large artifacts, a database's append-only log
// fsynced every second — and each checkpoint of it is one record.

// defaultDiskInterval is the interval a host checkpoints disks on.
const defaultDiskInterval = 60 * time.Second

// diskWorkloads are what the scenario runs, one after another, in the machine
// the boot started.
var diskWorkloads = []struct{ name, command string }{
	{"pnpm-install", "cd /opt/app && rm -rf node_modules && " +
		"pnpm install --offline --frozen-lockfile --reporter=silent"},
	{"cargo-build", "cd /opt/codex/codex-rs && cargo test --offline -p codex-apply-patch --no-run"},
	{"valkey-aof", "mkdir -p /var/lib/aof && " +
		"valkey-server --daemonize yes --save '' --appendonly yes --appendfsync everysec " +
		"--port 0 --unixsocket /tmp/aof.sock --unixsocketperm 700 --dir /var/lib/aof && " +
		"until valkey-cli -s /tmp/aof.sock ping > /dev/null 2>&1; do sleep 0.1; done && " +
		"valkey-benchmark -s /tmp/aof.sock -c 4 -P 32 -n 2000000 -r 1000000 -d 1024 -t set -q && " +
		"valkey-cli -s /tmp/aof.sock shutdown nosave"},
}

// diskInterval is SPROUTFS_BENCH_DISK_INTERVAL, or the host's default.
func (b *benchmark) diskInterval() time.Duration {
	b.t.Helper()
	value := os.Getenv("SPROUTFS_BENCH_DISK_INTERVAL")
	if value == "" {
		return defaultDiskInterval
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		b.t.Fatalf("invalid SPROUTFS_BENCH_DISK_INTERVAL %q", value)
	}
	return parsed
}

// diskCheckpoints runs every disk workload with a checkpoint of the disks on
// the interval behind it, and one more once it ends, recording each.
func (b *benchmark) diskCheckpoints(ctx context.Context, c *console, p *vmmachine.Process, vm *volume.VM) {
	b.t.Helper()
	interval := b.diskInterval()
	// Whatever the boot left private is not a workload's: the first checkpoint
	// publishes it, and every page a workload stores into after it is measured
	// from its first store.
	b.sync(ctx, c)
	b.diskCheckpoint(ctx, p, vm, "before", 0)
	for _, workload := range diskWorkloads {
		began := time.Now()
		done := make(chan error, 1)
		var timing guestTiming
		go func() {
			var err error
			timing, err = b.runWithin(ctx, c, workload.command)
			done <- err
		}()
		ticker := time.NewTicker(interval)
		running := true
		for running {
			select {
			case err := <-done:
				if err != nil {
					b.t.Fatalf("%s: %v", workload.name, err)
				}
				if timing.Exit != 0 {
					b.t.Fatalf("%s exited %d", workload.name, timing.Exit)
				}
				running = false
			case <-ticker.C:
				b.diskCheckpoint(ctx, p, vm, workload.name, time.Since(began))
			}
		}
		ticker.Stop()
		// The guest's own page cache is flushed to its disk, which is DAX, so
		// the last checkpoint holds everything the workload wrote.
		b.sync(ctx, c)
		b.diskCheckpoint(ctx, p, vm, workload.name, time.Since(began))
		b.appendRecord(benchRecord{Scenario: "disk-workload", Kind: "sproutfs",
			WallNS: time.Since(began).Nanoseconds(), Guest: &timing,
			Extra: map[string]any{"workload": workload.name, "interval_ns": interval.Nanoseconds()}})
	}
}

// diskCheckpoint takes one checkpoint of the disks, as a host's interval does,
// waits for it to land and records what it sealed, what the guest had really
// changed in those pages, and what it uploaded.
func (b *benchmark) diskCheckpoint(ctx context.Context, p *vmmachine.Process, vm *volume.VM, workload string, at time.Duration) {
	b.t.Helper()
	_, before := b.pagerStats(ctx)
	objects := b.objects.counters()
	paused := time.Now()
	var pause time.Duration
	ckpt, err := vm.SnapshotDisks(ctx, func(ctx context.Context) ([]byte, map[string]volume.DirtySource, error) {
		sources, err := p.SealDisks(ctx)
		if err != nil {
			return nil, nil, err
		}
		if err := p.Resume(ctx); err != nil {
			return nil, nil, err
		}
		pause = time.Since(paused)
		return nil, sources, nil
	})
	if err != nil {
		b.t.Fatalf("checkpointing the disks during %s: %v", workload, err)
	}
	if err := ckpt.Wait(ctx); err != nil {
		b.t.Fatalf("publishing the disks during %s: %v", workload, err)
	}
	_, after := b.pagerStats(ctx)
	uploaded := b.objects.counters().sub(objects)
	sealed := after.CheckpointPages - before.CheckpointPages
	unchanged := after.UnchangedPages - before.UnchangedPages
	changed := (after.ChangedBlocks - before.ChangedBlocks) * 4096
	extra := map[string]any{
		"workload":         workload,
		"at_ns":            at.Nanoseconds(),
		"pause_ns":         pause.Nanoseconds(),
		"sealed_pages":     sealed,
		"sealed_bytes":     sealed * checkpoint.PageSize2MiB,
		"unchanged_pages":  unchanged,
		"published_bytes":  (sealed - unchanged) * checkpoint.PageSize2MiB,
		"changed_bytes":    changed,
		"measured_pages":   after.MeasuredPages - before.MeasuredPages,
		"unmeasured_pages": after.UnmeasuredPages - before.UnmeasuredPages,
		"uploaded_bytes":   uploaded.PutBytes,
		"uploaded_objects": uploaded.Puts,
	}
	b.appendRecord(benchRecord{Scenario: "disk-checkpoint", Kind: "sproutfs",
		WallNS: time.Since(paused).Nanoseconds(), Objects: &uploaded, Extra: extra})
	b.t.Logf("disk checkpoint %s at %s: sealed %d MiB (%d unchanged pages), changed %d KiB, uploaded %d KiB, pause %s",
		workload, at.Round(time.Second), sealed*2, unchanged, changed>>10, uploaded.PutBytes>>10, pause)
}
