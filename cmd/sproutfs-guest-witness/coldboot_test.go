package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A cold-started guest has no resident witness: the memory that held it was
// discarded and the process with it, and /run is a tmpfs that the boot cleared.
// Its disk is the whole of what can be checked, and it is checked without a
// witness of any kind — the file is read as it is and compared against the
// arithmetic of (seed, step).
func TestADiskOnlyCheckNeedsNoResidentWitness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "witness")
	s, err := open(path, witnessSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.fill(4); err != nil {
		t.Fatal(err)
	}
	if _, err := s.mutate(1); err != nil {
		t.Fatal(err)
	}
	// The witness is gone, exactly as a reboot leaves it; its file is not.
	if err := s.close(); err != nil {
		t.Fatal(err)
	}

	if err := checkFile(path, 4, 1); err != nil {
		t.Fatalf("the disk of a rebooted guest does not check out: %v", err)
	}
	if err := checkFile(path, 5, 1); err == nil {
		t.Fatal("a disk checked out against a seed it was never filled with")
	}
	if err := checkFile(path, 4, 2); err == nil {
		t.Fatal("a disk checked out against a step nothing ever wrote")
	}
}

// A disk-only check names the first byte that is wrong, as every other check
// does: the offset is what an operator needs.
func TestADiskOnlyCheckNamesTheFirstByteThatIsWrong(t *testing.T) {
	path := filepath.Join(t.TempDir(), "witness")
	s, err := open(path, witnessSize)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.fill(9); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0xff}, 3*pageBytes+17); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	err = checkFile(path, 9, 0)
	var mismatch *mismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("a corrupted disk checked out: %v", err)
	}
	if mismatch.Where != "disk" || mismatch.Offset != 3*pageBytes+17 {
		t.Fatalf("the mismatch is %+v, want the disk byte that was overwritten", mismatch)
	}
}

// A file that is not a whole number of pages is refused rather than checked to
// the nearest page: the pattern is generated per page, and a partial one at the
// end is a page the expectation and the guest would disagree about.
func TestADiskOnlyCheckRefusesAFileThatIsNotWholePages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "witness")
	if err := os.WriteFile(path, make([]byte, pageBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkFile(path, 1, 0); err == nil {
		t.Fatal("a file that is not whole pages was checked anyway")
	}
}

// The command line says so: a disk-only check needs the path of the file, and
// takes no socket because there is nothing resident to reach.
func TestDiskOnlyIsParsedAndNeedsTheFile(t *testing.T) {
	parsed, err := parse([]string{"--seed", "3", "--step", "2", "--disk-only", "--disk", "/var/witness"})
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.diskOnly || parsed.disk != "/var/witness" {
		t.Fatalf("parsed %+v, want a disk-only check of that file", parsed)
	}
	if err := run([]string{"check", "--seed", "3", "--step", "2", "--disk-only"}); err == nil {
		t.Fatal("a disk-only check with no file to read was accepted")
	}
}
