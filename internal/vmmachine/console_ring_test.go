package vmmachine

import (
	"bytes"
	"strings"
	"testing"
)

func TestConsoleRingReadsWhatWasWritten(t *testing.T) {
	ring := newConsoleRing()
	if _, err := ring.Write([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	data, from, next := ring.read(0, 64)
	if string(data) != "hello world" || from != 0 || next != 11 {
		t.Fatalf("read(0, 64) = %q, %d, %d; want %q, 0, 11", data, from, next, "hello world")
	}
	data, from, next = ring.read(6, 64)
	if string(data) != "world" || from != 6 || next != 11 {
		t.Fatalf("read(6, 64) = %q, %d, %d; want %q, 6, 11", data, from, next, "world")
	}
	data, from, next = ring.read(11, 64)
	if len(data) != 0 || from != 11 || next != 11 {
		t.Fatalf("read(11, 64) = %q, %d, %d; want \"\", 11, 11", data, from, next)
	}
}

func TestConsoleRingLimitsOneRead(t *testing.T) {
	ring := newConsoleRing()
	if _, err := ring.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	data, from, next := ring.read(2, 3)
	if string(data) != "cde" || from != 2 || next != 5 {
		t.Fatalf("read(2, 3) = %q, %d, %d; want %q, 2, 5", data, from, next, "cde")
	}
}

// An offset the ring has dropped is answered from the oldest byte retained,
// and the reported start offset is what says the rest is gone.
func TestConsoleRingAnswersADroppedOffsetFromItsStart(t *testing.T) {
	ring := newConsoleRing()
	first := bytes.Repeat([]byte("a"), maxConsoleBytes)
	if _, err := ring.Write(first); err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Write([]byte("tail")); err != nil {
		t.Fatal(err)
	}
	data, from, next := ring.read(0, maxConsoleBytes)
	if from != 4 || next != int64(maxConsoleBytes)+4 || len(data) != maxConsoleBytes {
		t.Fatalf("read(0) = %d bytes, %d, %d; want %d, 4, %d", len(data), from, next, maxConsoleBytes, maxConsoleBytes+4)
	}
	if want := strings.Repeat("a", maxConsoleBytes-4) + "tail"; string(data) != want {
		t.Fatalf("read(0) data = %q...%q; want the last %d bytes written", data[:8], data[len(data)-8:], maxConsoleBytes)
	}
}

// A single write larger than the ring keeps only its newest bytes and still
// counts every byte in the offsets it reports.
func TestConsoleRingKeepsTheTailOfAnOversizedWrite(t *testing.T) {
	ring := newConsoleRing()
	data := append(bytes.Repeat([]byte("x"), maxConsoleBytes+3), []byte("end")...)
	if _, err := ring.Write(data); err != nil {
		t.Fatal(err)
	}
	got, from, next := ring.read(0, maxConsoleBytes)
	if from != 6 || next != int64(maxConsoleBytes)+6 {
		t.Fatalf("read(0) = %d, %d; want 6, %d", from, next, maxConsoleBytes+6)
	}
	if !bytes.HasSuffix(got, []byte("end")) || len(got) != maxConsoleBytes {
		t.Fatalf("read(0) = %d bytes ending %q; want %d ending %q", len(got), got[len(got)-3:], maxConsoleBytes, "end")
	}
}

func TestConsoleRingTailIsTheNewestOutput(t *testing.T) {
	ring := newConsoleRing()
	if _, err := ring.Write([]byte("first\nsecond\nthird\n")); err != nil {
		t.Fatal(err)
	}
	if got := string(ring.tail(6)); got != "third\n" {
		t.Fatalf("tail(6) = %q; want %q", got, "third\n")
	}
}
