package vmmemory_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/platform/sim"
)

// requireReplay fails unless two runs of a seed released every operation in
// the same order. A failure prints the first operation the runs differ at and
// the ones before it, which is where a search for what the Go scheduler chose
// begins.
func requireReplay(t *testing.T, seed uint64, runs [2]sim.Recording) {
	t.Helper()
	if bytes.Equal(runs[0].Execution, runs[1].Execution) {
		return
	}
	var texts [2][]string
	for run, recording := range runs {
		var text strings.Builder
		if err := recording.WriteText(&text); err != nil {
			t.Fatal(err)
		}
		texts[run] = strings.Split(text.String(), "\n")
	}
	a, b := texts[0], texts[1]
	at := 0
	for at < min(len(a), len(b)) && a[at] == b[at] {
		at++
	}
	from := max(0, at-12)
	t.Fatalf("seed %d released its operations in another order on its second run, from line %d:\n"+
		"first run:\n%s\nsecond run:\n%s", seed, at,
		strings.Join(a[from:min(len(a), at+6)], "\n"), strings.Join(b[from:min(len(b), at+6)], "\n"))
}
