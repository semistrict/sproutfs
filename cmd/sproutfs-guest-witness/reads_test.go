package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The reads command writes its read file and every byte it was asked to write,
// in files of 64 MiB, and reads the file during the writes and after them.
func TestReadsWritesWhatItWasAskedAndReadsBesideIt(t *testing.T) {
	dir := t.TempDir()
	parsed, err := parseReads([]string{"--file", filepath.Join(dir, "read"), "--dir", filepath.Join(dir, "fresh"),
		"--size", "64K", "--write-bytes", "3M", "--interval", "0s", "--idle", "50ms", "--buffered"})
	if err != nil {
		t.Fatal(err)
	}
	report, err := measureReads(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if report.Block != 4096 || report.FileBytes != 64<<10 || report.WrittenBytes != 3<<20 {
		t.Fatalf("the report is of %d-byte reads of a %d-byte file beside %d bytes written; want 4096, %d and %d",
			report.Block, report.FileBytes, report.WrittenBytes, 64<<10, 3<<20)
	}
	if report.Idle.Reads == 0 {
		t.Fatal("no read ran in the 50 ms after the writes")
	}
	fresh, err := os.Stat(filepath.Join(dir, "fresh", "fresh-0"))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Size() != 3<<20 {
		t.Fatalf("the fresh file holds %d bytes, want the %d written", fresh.Size(), 3<<20)
	}
}

// A read of a block that does not hold the file's pattern fails, naming the
// word, rather than counting a fast read of the wrong bytes.
func TestReadsFailsOnABlockThatIsNotThePattern(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "read")
	if err := writeReadFile(path, 4096); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteAt([]byte{0xff}, 16); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	_, err = readUntil(file, readsOptions{size: 4096, block: 4096, interval: time.Millisecond}, done)
	const want = "the word at 16 reads 0x5eed0000000000ff, want 0x5eed000000000010"
	if err == nil || err.Error() != want {
		t.Fatalf("reading the changed block = %v, want %q", err, want)
	}
}

// Flags that cannot describe a run are refused.
func TestReadsRefusesWhatItCannotRun(t *testing.T) {
	for name, args := range map[string][]string{
		"no file":       {"--dir", "/d"},
		"no dir":        {"--file", "/f"},
		"odd block":     {"--file", "/f", "--dir", "/d", "--block", "1000"},
		"partial block": {"--file", "/f", "--dir", "/d", "--size", "6K"},
	} {
		if _, err := parseReads(args); err == nil {
			t.Errorf("%s: parsed %v, want it refused", name, args)
		}
	}
}
