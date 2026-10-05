package vmmemory

import (
	"slices"
	"testing"
)

// The runs a seal reads join as pages enter beside them and split as pages
// leave from inside them, so the set is always its fewest runs.
func TestPageRunsJoinAndSplitAsPagesEnterAndLeave(t *testing.T) {
	runs := newPageRuns(4096)
	requireRuns := func(want ...PageRun) {
		t.Helper()
		if got := runs.runs(64); !slices.Equal(got, want) {
			t.Fatalf("runs = %v, want %v", got, want)
		}
	}
	requireRuns()
	runs.add(1, 4)
	runs.add(5, 6)
	requireRuns(PageRun{Page: 1, Count: 3}, PageRun{Page: 5, Count: 1})
	// The page between the two joins them into one.
	runs.add(4, 5)
	requireRuns(PageRun{Page: 1, Count: 5})
	// A page from the middle splits the run around it.
	runs.remove(3)
	requireRuns(PageRun{Page: 1, Count: 2}, PageRun{Page: 4, Count: 2})
	if runs.has(3) || !runs.has(2) || !runs.has(4) {
		t.Fatalf("has(2, 3, 4) = %t, %t, %t, want true, false, true", runs.has(2), runs.has(3), runs.has(4))
	}
	// A page that is not in the set leaves it as it is.
	runs.remove(30)
	requireRuns(PageRun{Page: 1, Count: 2}, PageRun{Page: 4, Count: 2})
	// A range over runs and the gaps between them is one run, across nodes of
	// the page list.
	runs.add(0, 40)
	requireRuns(PageRun{Page: 0, Count: 40})
	// The ends of a run leave it one at a time.
	runs.remove(0)
	runs.remove(39)
	requireRuns(PageRun{Page: 1, Count: 38})
	// Every page leaves, and the set is empty again.
	for page := range uint64(40) {
		runs.remove(page)
	}
	requireRuns()
}
