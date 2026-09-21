//go:build sproutfsprobe

package vmmemory

import (
	"context"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/control"
)

// slotArena is an arena of fixed pages held in memory, which is all the
// lost-write check needs to be exercised: it compares two slots' bytes.
type slotArena [][]byte

func (a slotArena) Read(_ context.Context, slot int, dst []byte) error {
	copy(dst, a[slot])
	return nil
}
func (a slotArena) Write(_ context.Context, slot int, src []byte) error {
	copy(a[slot], src)
	return nil
}
func (a slotArena) Release(context.Context, int) error { return nil }

func page(slot int, private bool) *resident {
	return &resident{slot: slot, private: private, aliases: make(map[*binding]struct{})}
}

func wantFinding(t *testing.T, got, contains string) {
	t.Helper()
	if got == "" {
		t.Fatalf("the audit found nothing, want a finding naming %q", contains)
	}
	if !strings.Contains(got, contains) {
		t.Fatalf("the audit found %q, want one naming %q", got, contains)
	}
}

// The whole point of the accelerated build's lost-write check: a guest that has
// been given a page to store into must never be handed an older one back.
func TestTheProbeRefusesToInstallAPageOlderThanTheGuestWasGiven(t *testing.T) {
	var p probeState
	b := &binding{index: 7}
	origin, copied := page(1, false), page(2, true)
	p.granted(b, copied, origin)

	// The page the guest holds is what it was given, so installing it again is
	// no step back.
	if found := p.bind(nil, b, copied); found != "" {
		t.Fatalf("re-installing the page the guest holds was reported as %q", found)
	}
	found := p.bind(nil, b, origin)
	wantFinding(t, found, "a lost write")
	wantFinding(t, found, "page 7")
}

// Every path that gives a guest memory of its own says so in the same breath,
// so a page that arrives in a guest's own memory from anywhere else is a page
// the pager cannot vouch for. It is reported at that binding's next transition.
func TestTheProbeReportsAPageInstalledOverAGuestsOwnMemory(t *testing.T) {
	var p probeState
	b := &binding{index: 7}
	p.granted(b, page(2, true), page(1, false))

	// A post-copy or a read-ahead putting a page it loaded over the one the
	// guest has been storing into. Nothing dates it.
	if found := p.bind(nil, b, page(3, false)); found != "" {
		t.Fatalf("the install itself was reported as %q, and nothing there can tell it apart", found)
	}
	wantFinding(t, p.bind(nil, b, page(4, false)), "outside its own store path")
}

// A page whose dirty epoch has ended is the volume's again: the pager owes that
// guest nothing newer, and chooses what it maps by identity from then on.
func TestTheProbeStopsOwingAGuestOnceItsPageIsRetired(t *testing.T) {
	var p probeState
	b := &binding{index: 7}
	origin, copied := page(1, false), page(2, true)
	p.granted(b, copied, origin)
	p.retired(b)
	if found := p.bind(nil, b, origin); found != "" {
		t.Fatalf("a retired page's binding was still owed something newer: %q", found)
	}
}

// A fork on this host names a private page rather than copying it, which is how
// the machines that inherit the identity map it instead of reading it. So a
// named private page reached from two regions is the sharing working, and only
// an unnamed one is a guest writing into another's memory.
func TestTheProbeAllowsTwoRegionsToShareANamedPrivatePage(t *testing.T) {
	var p probeState
	parent, child := &Region{}, &Region{}
	shared := page(1, true)
	held := &binding{region: parent, index: 7}
	shared.aliases[held] = struct{}{}

	wantFinding(t, p.bind(nil, &binding{region: child, index: 7}, shared),
		"reached from two regions")

	shared.key = pageKey{id: control.Identity{Volume: "fork-point", Page: 7}}
	if found := p.bind(nil, &binding{region: child, index: 7}, shared); found != "" {
		t.Fatalf("a fork point's named page was reported as %q", found)
	}
}

// The settle's re-share is the one step back the pager takes on purpose, and it
// is sound only because the two pages hold the same bytes. So the audit reads
// them rather than trusting the comparison that chose the page.
func TestTheProbeChecksTheSettlesReShareByItsBytes(t *testing.T) {
	arena := slotArena{make([]byte, 8), make([]byte, 8), make([]byte, 8)}
	h := &Host{pageSize: 8, arena: arena}
	var p probeState
	b := &binding{index: 7}
	origin, copied := page(1, false), page(2, true)
	copy(arena[1], []byte("abcdefgh"))
	copy(arena[2], []byte("abcdefgh"))
	p.granted(b, copied, origin)

	// Equal bytes: the origin inherits the copy's age, so putting the guest
	// back on it is not a lost write.
	if found := p.reshared(t.Context(), h, copied, origin); found != "" {
		t.Fatalf("re-sharing onto an origin holding the same bytes was reported as %q", found)
	}
	if found := p.bind(nil, b, origin); found != "" {
		t.Fatalf("the re-shared origin was reported as %q", found)
	}

	copy(arena[2], []byte("abcXefgh"))
	wantFinding(t, p.reshared(t.Context(), h, copied, origin), "differ from byte 3")
}

// What the audit remembers is one entry per page it has dated and one per
// binding it owes something newer — not one per grant. A guest that stores into
// the same page a hundred times leaves one debt behind it, which is what keeps
// an accelerated run's bookkeeping proportional to the memory rather than to
// the faults.
func TestTheProbeRemembersOnePageAtATimePerGuest(t *testing.T) {
	var p probeState
	b := &binding{index: 7}
	for range 100 {
		p.granted(b, page(2, true), page(1, false))
	}
	if len(p.writable) != 1 {
		t.Fatalf("the probe owes %d bindings, want one", len(p.writable))
	}
	if len(p.generation) != 200 {
		t.Fatalf("the probe dated %d pages, want the two hundred it was told about", len(p.generation))
	}
}
