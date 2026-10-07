package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every thread writes its own file, whole before the clock starts, and the
// report counts every block it wrote after.
func TestFlushWritesEveryThreadsFileAndCountsItsBlocks(t *testing.T) {
	dir := t.TempDir()
	parsed, err := parseFlush([]string{"--dir", dir, "--threads", "2", "--file", "64K", "--seconds", "200ms"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := measureFlushes(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if report.Threads != 2 || report.Block != 4096 || !report.Sync {
		t.Fatalf("the report is of %d threads of %d-byte blocks, sync %v; want 2 of 4096, sync true",
			report.Threads, report.Block, report.Sync)
	}
	if report.Writes == 0 || report.Bytes != int64(report.Writes)*4096 {
		t.Fatalf("the report counts %d writes of %d bytes, want some writes of 4096 bytes each",
			report.Writes, report.Bytes)
	}
	if !(report.P50 <= report.P99 && report.P99 <= report.P999 && report.P999 <= report.Max) {
		t.Fatalf("the percentiles %v, %v, %v and the maximum %v are out of order",
			report.P50, report.P99, report.P999, report.Max)
	}
	for thread, name := range []string{"flush-0", "flush-1"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 64<<10 {
			t.Fatalf("%s holds %d bytes, want 65536", name, len(data))
		}
		// The first block was the first one written, stamped with the
		// thread in the stamp's top bits.
		if got := binary.LittleEndian.Uint64(data) >> 48; got != uint64(thread) {
			t.Fatalf("%s's first block is stamped by thread %d, want %d", name, got, thread)
		}
	}
}

// Without a flush the writes run the same way and the report says so.
func TestFlushWithNoSyncOnlyWrites(t *testing.T) {
	parsed, err := parseFlush([]string{"--dir", t.TempDir(), "--file", "16K", "--seconds", "50ms", "--no-sync"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.sync || parsed.threads != 1 || parsed.duration != 50*time.Millisecond {
		t.Fatalf("parsed %+v, want one thread for 50ms with no sync", parsed)
	}
	report, err := measureFlushes(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if report.Sync {
		t.Fatal("a run with --no-sync reports that it flushed")
	}
}

func TestFlushRefusesWhatItCannotRun(t *testing.T) {
	for args, want := range map[string]string{
		"--threads 2":                    "flush needs --dir",
		"--dir d --threads 0":            "--threads is at least 1",
		"--dir d --block 100":            "--block is a multiple of 8 bytes",
		"--dir d --block 8192 --file 4K": "--file is a whole number of blocks",
		"--dir d --seconds 0s":           "--seconds is a positive duration",
		"--dir d extra":                  `flush takes flags only, not ["extra"]`,
	} {
		_, err := parseFlush(strings.Fields(args))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("flush %s failed with %v, want %q", args, err, want)
		}
	}
}
