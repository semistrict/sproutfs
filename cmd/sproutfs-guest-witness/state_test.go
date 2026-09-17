package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// witnessSize is what these tests fill: small enough to be quick and large
// enough for a step's scattered fraction to be a set rather than a page.
const witnessSize = 1 << 20

func newState(t *testing.T) *state {
	t.Helper()
	s, err := open(filepath.Join(t.TempDir(), "witness"), witnessSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// TestAFilledWitnessChecksOutAtTheStepItWasFilledAt, in memory and on disk.
func TestAFilledWitnessChecksOutAtTheStepItWasFilledAt(t *testing.T) {
	s := newState(t)
	if err := s.fill(4); err != nil {
		t.Fatal(err)
	}
	if err := s.check(4, 0); err != nil {
		t.Fatalf("a witness that was just filled does not check out: %v", err)
	}
	// The seed and the step are the script's, not the guest's, so a check
	// against the wrong ones has to fail: that is what tells a VM holding one
	// guest's bytes from a VM holding another's.
	if err := s.check(5, 0); err == nil {
		t.Fatal("a witness checked out against a seed it was never filled with")
	}
	if err := s.check(4, 1); err == nil {
		t.Fatal("a witness checked out against a step nothing ever wrote")
	}
}

// TestMutatingMovesTheWitnessToTheNextStepAndNoFurther: the pages a step wrote
// hold that step's pattern and the rest still hold the one before it, which is
// exactly what the pure expectation says.
func TestMutatingMovesTheWitnessToTheNextStepAndNoFurther(t *testing.T) {
	s := newState(t)
	if err := s.fill(4); err != nil {
		t.Fatal(err)
	}
	pages, err := s.mutate(1)
	if err != nil {
		t.Fatal(err)
	}
	if pages == 0 || pages == witnessSize/pageBytes {
		t.Fatalf("a step rewrote %d of %d pages, want a fraction of them",
			pages, witnessSize/pageBytes)
	}
	if err := s.check(4, 1); err != nil {
		t.Fatalf("a witness that was just mutated does not check out: %v", err)
	}
	// It is no longer what it was, and it is not yet what the next step is.
	if err := s.check(4, 0); err == nil {
		t.Fatal("a mutated witness still checks out at the step before it")
	}
	if err := s.check(4, 2); err == nil {
		t.Fatal("a witness checked out at a step it was never mutated to")
	}
}

// TestACheckNamesTheFirstByteOfMemoryThatIsWrong. One byte is the whole point:
// a guest whose memory came back with a page of another checkpoint in it, or
// with one byte of a torn write, has to be caught and the offset reported, so
// that the object it belongs to can be found.
func TestACheckNamesTheFirstByteOfMemoryThatIsWrong(t *testing.T) {
	s := newState(t)
	if err := s.fill(4); err != nil {
		t.Fatal(err)
	}
	const corrupted = 3*pageBytes + 17
	was := s.memory[corrupted]
	s.memory[corrupted] ^= 0xff

	var mismatch *mismatchError
	err := s.check(4, 0)
	if !errors.As(err, &mismatch) {
		t.Fatalf("a witness with one byte of memory wrong checked out: %v", err)
	}
	if mismatch.Where != "memory" || mismatch.Offset != corrupted {
		t.Fatalf("the check reported %+v, want the byte at %d in memory", mismatch, corrupted)
	}
	if mismatch.Got != was^0xff || mismatch.Want != was {
		t.Fatalf("the check reported got %#02x want %#02x, and the byte was %#02x",
			mismatch.Got, mismatch.Want, was)
	}
}

// TestACheckNamesTheFirstByteOfDiskThatIsWrong, and reads the file again to
// find it: a check that trusted the page cache would report a disk that was
// never written as though it held what memory does.
func TestACheckNamesTheFirstByteOfDiskThatIsWrong(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "witness")
	s, err := open(path, witnessSize)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.close(); err != nil {
			t.Error(err)
		}
	}()
	if err := s.fill(4); err != nil {
		t.Fatal(err)
	}
	const corrupted = 9*pageBytes + 5
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{0x5a}, corrupted); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	var mismatch *mismatchError
	if err := s.check(4, 0); !errors.As(err, &mismatch) {
		t.Fatalf("a witness with one byte of its file wrong checked out: %v", err)
	}
	if mismatch.Where != "disk" || mismatch.Offset != corrupted {
		t.Fatalf("the check reported %+v, want the byte at %d on disk", mismatch, corrupted)
	}
}

// TestAWitnessCopiedElsewhereChecksOutAgainstItsParentsSeedAndStep: this is the
// fork. A child inherits its parent's memory and disk, and what says the
// inheritance was whole is that the child checks out at the parent's (seed,
// step) before anything else is asked of it.
func TestAWitnessCopiedElsewhereChecksOutAgainstItsParentsSeedAndStep(t *testing.T) {
	parent := newState(t)
	if err := parent.fill(4); err != nil {
		t.Fatal(err)
	}
	for step := uint64(1); step <= 3; step++ {
		if _, err := parent.mutate(step); err != nil {
			t.Fatal(err)
		}
	}
	child := newState(t)
	copy(child.memory, parent.memory)
	if err := child.writeThrough(0, child.memory); err != nil {
		t.Fatal(err)
	}
	if err := child.check(4, 3); err != nil {
		t.Fatalf("a child holding exactly its parent's bytes does not check out: %v", err)
	}
	// And it goes on under a seed of its own, which is what makes two children
	// of one parent distinguishable afterwards.
	if err := child.fill(11); err != nil {
		t.Fatal(err)
	}
	if _, err := child.mutate(1); err != nil {
		t.Fatal(err)
	}
	if err := child.check(11, 1); err != nil {
		t.Fatalf("a reseeded child does not check out under its own seed: %v", err)
	}
	if err := parent.check(4, 3); err != nil {
		t.Fatalf("the parent stopped checking out after its child went its own way: %v", err)
	}
}

// TestSizesAreReadTheWayAnOperatorWritesThem, which is what the flag takes.
func TestSizesAreReadTheWayAnOperatorWritesThem(t *testing.T) {
	for _, tc := range []struct {
		text string
		want int64
	}{
		{"4096", 4096}, {"64K", 64 << 10}, {"256M", 256 << 20}, {"2G", 2 << 30},
		{"8KiB", 8 << 10}, {"1MiB", 1 << 20},
	} {
		got, err := parseSize(tc.text)
		if err != nil {
			t.Fatalf("%s: %v", tc.text, err)
		}
		if got != tc.want {
			t.Fatalf("%s is %d bytes, want %d", tc.text, got, tc.want)
		}
	}
	for _, text := range []string{"", "M", "-1", "1T?", "0", "1.5M"} {
		if got, err := parseSize(text); err == nil {
			t.Fatalf("%q was read as %d bytes, want a refusal", text, got)
		}
	}
	// A size that is not a whole number of pages is refused rather than
	// rounded: the pattern is generated per page, and a partial one at the end
	// would be a page the expectation and the guest disagree about.
	if _, err := parseSize("4097"); err == nil {
		t.Fatal("a size that is not a whole number of pages was accepted")
	}
}
