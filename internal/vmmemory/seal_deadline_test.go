package vmmemory_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestASealWhoseDeadlinePassesLeavesTheGuestRunning. A seal is bounded: the
// pager gives the command a deadline and a seal that overruns it is a failed
// checkpoint, which the VMM answers by unsealing and resuming. That is a
// checkpoint that did not happen, not a VM that is over — every page the guest
// wrote is still here and the next interval takes the whole dirty set.
//
// A region that went terminal on that deadline turns it into the end of the VM
// instead: every later fault, seal and command fails, the session is killed and
// the guest dies for a checkpoint that was only slow.
func TestASealWhoseDeadlinePassesLeavesTheGuestRunning(t *testing.T) {
	f := newFixture(t, 8, 8, 8)
	r, m, b := f.region(4)
	for page := range uint64(3) {
		bytes := access(t, r, m, page, true)
		for i := range bytes {
			bytes[i] = byte(page) + 0x40
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The seal's time runs out while it is write-protecting, which is where a
	// real one spends it.
	m.onProtect = func(uint64, int) { cancel() }
	if err := r.Seal(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a seal whose deadline passed = %v", err)
	}
	m.onProtect = nil
	// The checkpoint failed; the VM did not. The next one takes everything the
	// guest has written, including the pages the abandoned seal had taken.
	if err := f.checkpoint(r, b); err != nil {
		t.Fatalf("the checkpoint after a seal whose deadline passed: %v", err)
	}
	for page := range uint64(3) {
		want := bytes.Repeat([]byte{byte(page) + 0x40}, f.pageSize)
		got := b.data[page*uint64(f.pageSize) : (page+1)*uint64(f.pageSize)]
		if !bytes.Equal(got, want) {
			t.Fatalf("page %d published as %#x, want %#x", page, got[0], want[0])
		}
	}
}
