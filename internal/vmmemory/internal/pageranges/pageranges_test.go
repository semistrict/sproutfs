package pageranges_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmemory/internal/pageranges"
)

// An empty map holds the default state everywhere, and a run of it ends only
// where the caller's limit does.
func TestEmptyMapIsDefaultEverywhere(t *testing.T) {
	var m pageranges.Map
	if got := m.Get(5); got != (pageranges.State{}) {
		t.Fatalf("Get(5) on an empty map = %+v, want the default state", got)
	}
	state, end := m.Run(5, 100)
	if state != (pageranges.State{}) || end != 100 {
		t.Fatalf("Run(5, 100) on an empty map = %+v, %d, want {} and 100", state, end)
	}
}

// One range holds exactly its half-open extent, and the default-state gaps
// around it stop at its boundaries.
func TestRangeCoversExactlyItsExtent(t *testing.T) {
	var m pageranges.Map
	m.Set(10, 20, pageranges.State{Zero: true})
	if got := m.Get(9); got != (pageranges.State{}) {
		t.Fatalf("Get(9) = %+v, want the default state", got)
	}
	if got := m.Get(10); got != (pageranges.State{Zero: true}) {
		t.Fatalf("Get(10) = %+v, want {Zero:true}", got)
	}
	if got := m.Get(19); got != (pageranges.State{Zero: true}) {
		t.Fatalf("Get(19) = %+v, want {Zero:true}", got)
	}
	if got := m.Get(20); got != (pageranges.State{}) {
		t.Fatalf("Get(20) = %+v, want the default state", got)
	}
	state, end := m.Run(0, 100)
	if state != (pageranges.State{}) || end != 10 {
		t.Fatalf("Run(0, 100) = %+v, %d, want {} and 10", state, end)
	}
	state, end = m.Run(10, 100)
	if state != (pageranges.State{Zero: true}) || end != 20 {
		t.Fatalf("Run(10, 100) = %+v, %d, want {Zero:true} and 20", state, end)
	}
	state, end = m.Run(15, 17)
	if state != (pageranges.State{Zero: true}) || end != 17 {
		t.Fatalf("Run(15, 17) = %+v, %d, want {Zero:true} and 17", state, end)
	}
}

// Adjacent equal ranges are one run; a neighbour holding a different state is
// not merged into it.
func TestAdjacentEqualRangesMerge(t *testing.T) {
	var m pageranges.Map
	m.Set(10, 20, pageranges.State{Zero: true})
	m.Set(20, 30, pageranges.State{Zero: true})
	state, end := m.Run(10, 100)
	if state != (pageranges.State{Zero: true}) || end != 30 {
		t.Fatalf("Run(10, 100) after two adjacent equal sets = %+v, %d, want {Zero:true} and 30", state, end)
	}
	m.Set(30, 40, pageranges.State{Generation: 7})
	state, end = m.Run(10, 100)
	if state != (pageranges.State{Zero: true}) || end != 30 {
		t.Fatalf("Run(10, 100) beside a different state = %+v, %d, want {Zero:true} and 30", state, end)
	}
	state, end = m.Run(30, 100)
	if state != (pageranges.State{Generation: 7}) || end != 40 {
		t.Fatalf("Run(30, 100) = %+v, %d, want {Generation:7} and 40", state, end)
	}
}

// Setting the default state takes a range back out of the map, splitting the
// run it lands in the middle of.
func TestDefaultStateClearsARange(t *testing.T) {
	var m pageranges.Map
	m.Set(10, 30, pageranges.State{Zero: true})
	m.Set(15, 25, pageranges.State{})
	if got := m.Get(14); got != (pageranges.State{Zero: true}) {
		t.Fatalf("Get(14) = %+v, want {Zero:true}", got)
	}
	if got := m.Get(15); got != (pageranges.State{}) {
		t.Fatalf("Get(15) = %+v, want the default state", got)
	}
	if got := m.Get(24); got != (pageranges.State{}) {
		t.Fatalf("Get(24) = %+v, want the default state", got)
	}
	if got := m.Get(25); got != (pageranges.State{Zero: true}) {
		t.Fatalf("Get(25) = %+v, want {Zero:true}", got)
	}
	state, end := m.Run(10, 100)
	if state != (pageranges.State{Zero: true}) || end != 15 {
		t.Fatalf("Run(10, 100) = %+v, %d, want {Zero:true} and 15", state, end)
	}
}

// An empty range changes nothing.
func TestEmptyRangeIsANoOp(t *testing.T) {
	var m pageranges.Map
	m.Set(10, 20, pageranges.State{Zero: true})
	m.Set(15, 15, pageranges.State{Generation: 3})
	m.Set(20, 10, pageranges.State{Generation: 3})
	state, end := m.Run(10, 100)
	if state != (pageranges.State{Zero: true}) || end != 20 {
		t.Fatalf("Run(10, 100) after two empty sets = %+v, %d, want {Zero:true} and 20", state, end)
	}
}

// One range replacing many fragments leaves one run, which is what a
// population marking a whole hole over per-page state does.
func TestOneRangeReplacesManyFragments(t *testing.T) {
	var m pageranges.Map
	for page := uint64(0); page < 200; page += 2 {
		m.Set(page, page+1, pageranges.State{Generation: page + 1})
	}
	state, end := m.Run(0, 1000)
	if state != (pageranges.State{Generation: 1}) || end != 1 {
		t.Fatalf("Run(0, 1000) over the fragments = %+v, %d, want {Generation:1} and 1", state, end)
	}
	m.Set(0, 200, pageranges.State{Zero: true})
	state, end = m.Run(0, 1000)
	if state != (pageranges.State{Zero: true}) || end != 200 {
		t.Fatalf("Run(0, 1000) after the replacement = %+v, %d, want {Zero:true} and 200", state, end)
	}
	if got := m.Get(200); got != (pageranges.State{}) {
		t.Fatalf("Get(200) = %+v, want the default state", got)
	}
}
