package vmmemory_test

import (
	"testing"
	"testing/synctest"
)

// What a store costs the VMM in commands.
//
// A copy-on-write of a page the guest maps read-only has something to put in
// the mapping's place: the private copy. One MAP over the store's run replaces
// what the guest had with what it may store into, so the store is one command.
// A revocation takes a page away with nothing to put there, and the only things
// that need one are a page being reclaimed, a settle handing a page back to its
// origin and an abandoned checkpoint — never a store.
//
// These are the counts, at 4 KiB, where a store copies one page of an inherited
// 2 MiB run rather than the whole of it.

// commands reports the mapping commands and the revocations one action costs.
func commands(m *mapping, action func()) (maps, revokes int) {
	before, revoked := m.maps, m.revokes
	action()
	return m.maps - before, m.revokes - revoked
}

// A store into a shared page the guest maps read-only replaces that mapping:
// one command, and nothing is taken away.
func TestAStoreIntoAMappedSharedPageIsOneMappingAndNoRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, r, m, _ := placedRegion(t, 2*rangePages)
		const page = 100
		held(t, r, m, page, page+1)
		maps, revokes := commands(m, func() { access(t, r, m, page, true)[0] = 7 })
		if maps != 1 || revokes != 0 {
			t.Fatalf("a store into a page the guest maps issued %d mapping commands and %d revocations, want 1 and 0",
				maps, revokes)
		}
		if got := access(t, r, m, page, false)[0]; got != 7 {
			t.Fatalf("the page the guest stored into reads %d, want 7", got)
		}
	})
}

// A store that closes a gap replaces the mappings of every page it copies in
// the same command: the run is one MAP, and no page of it is taken away first.
func TestAStoreThatClosesAGapIsOneMappingAndNoRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, r, m, _ := placedRegion(t, 2*rangePages)
		r.PressMappings()
		const first = 100
		held(t, r, m, first, first+gap+1)
		access(t, r, m, first, true)[0] = 7
		maps, revokes := commands(m, func() { access(t, r, m, first+gap, true)[0] = 7 })
		if maps != 1 || revokes != 0 {
			t.Fatalf("a store closing a gap of %d pages issued %d mapping commands and %d revocations, want 1 and 0",
				gap, maps, revokes)
		}
		if got := privateMappings(m); got != 1 {
			t.Fatalf("the closed gap is %d private mappings, want 1", got)
		}
		if got := access(t, r, m, first+1, false)[0]; got != first+1+1 {
			t.Fatalf("a page the gap rule copied reads %d, want the %d it held", got, first+2)
		}
	})
}

// A range that is half private is filled in one command too: every page of it
// lands in the holes of its extent, and the whole range is mapped at once.
func TestAStoreThatFillsARangeIsOneMappingAndNoRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const half = rangePages / 2
		f := placedFixture(t, 4*rangePages)
		b := f.newBacking(2 * rangePages)
		r, m := f.attach(b)
		held(t, r, m, 0, rangePages)
		for page := range uint64(half - 1) {
			access(t, r, m, page, true)[0] = 7
		}
		maps, revokes := commands(m, func() { access(t, r, m, half-1, true)[0] = 7 })
		if maps != 1 || revokes != 0 {
			t.Fatalf("the store that filled the range issued %d mapping commands and %d revocations, want 1 and 0",
				maps, revokes)
		}
		if got := privateMappings(m); got != 1 {
			t.Fatalf("a whole range is %d private mappings, want 1", got)
		}
	})
}

// An interval checkpoint over a run of private pages is one write-protect
// command for the run and not one command per page, and it revokes nothing: a
// sealed page keeps its mapping and its memory, and the guest traps on its next
// store to it. Only a settle that hands a page back takes a mapping away, and
// this guest really stored into every page it holds.
func TestAnIntervalCheckpointIsOneProtectPerRunAndNoRevocation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const pages = 64
		f, r, m, b := placedRegion(t, 2*rangePages)
		held(t, r, m, 0, pages)
		// Every page of this backing holds its own number plus one, so a zero is
		// a byte none of them had: the settle finds every one of them changed.
		for page := range uint64(pages) {
			access(t, r, m, page, true)[0] = 0
		}
		protects, revokes := m.protects, m.revokes
		if err := r.Seal(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := m.protects - protects; got != 1 {
			t.Fatalf("sealing one run of %d private pages issued %d write-protect commands, want 1", pages, got)
		}
		if got := f.settle(r); got != 0 {
			t.Fatalf("the settle handed back %d pages the guest stored into, want none", got)
		}
		f.finishCheckpoint(r, b)
		if got := m.revokes - revokes; got != 0 {
			t.Fatalf("the checkpoint issued %d revocations, want none: nothing was dropped", got)
		}
	})
}
