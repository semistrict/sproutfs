// Package testrepro checks scheduled workloads across independent Go processes.
package testrepro

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// AcrossProcesses runs an existing recording test with one seed and both caller
// creation orders, under GOMAXPROCS 1, 4, and 4 again. That test must already
// assert its independent model and compare reversed creation orders. This adds
// fresh map seeds, process state, and scheduler settings to the comparison.
func AcrossProcesses(t *testing.T, recordingTest string) {
	t.Helper()
	if recordingTest == "" || recordingTest == t.Name() {
		t.Fatal("recording test must be a distinct named test")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	baseline := make(map[string][]byte)
	baselineText := make(map[string][]byte)
	for run, procs := range []int{1, 4, 4} {
		directory := filepath.Join(root, fmt.Sprintf("run-%d-procs-%d", run, procs))
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		command := exec.CommandContext(ctx, binary, "-test.run=^"+regexp.QuoteMeta(recordingTest)+"$", "-test.count=1", "-test.timeout=40s")
		for _, variable := range os.Environ() {
			if !strings.HasPrefix(variable, "SPROUTFS_") && !strings.HasPrefix(variable, "GOMAXPROCS=") {
				command.Env = append(command.Env, variable)
			}
		}
		command.Env = append(command.Env, fmt.Sprintf("GOMAXPROCS=%d", procs),
			"SPROUTFS_OVERLAP_TRACE_SEEDS=1", "SPROUTFS_OVERLAP_TRACE_DIR="+directory)
		output, err := command.CombinedOutput()
		deadlineExceeded := errors.Is(ctx.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			// Preserve the outcome for mutation audits of the parent test. A
			// killed child or expired deadline is not an assertion detection.
			if deadlineExceeded || bytes.Contains(output, []byte("panic: test timed out")) {
				t.Fatalf("testrepro: child deadline exceeded: %s\n%s", recordingTest, output)
			}
			var exited *exec.ExitError
			if errors.As(err, &exited) && exited.ExitCode() < 0 {
				t.Fatalf("testrepro: child exited on a signal: %s: %v\n%s", recordingTest, err, output)
			}
			t.Fatalf("%s in fresh process %d (GOMAXPROCS=%d): %v\n%s", recordingTest, run, procs, err, output)
		}
		for order := range 2 {
			for _, stream := range []string{"execution", "adapter"} {
				name := fmt.Sprintf("seed-01-order-%d.%s", order, stream)
				data, err := os.ReadFile(filepath.Join(directory, name+".pb"))
				if err != nil || len(data) == 0 {
					t.Fatalf("missing or empty %s recording: %v", name, err)
				}
				text, err := os.ReadFile(filepath.Join(directory, name+".txt"))
				if err != nil {
					t.Fatal(err)
				}
				if run == 0 {
					baseline[name], baselineText[name] = data, text
					continue
				}
				if !bytes.Equal(baseline[name], data) {
					before, after := bytes.Split(baselineText[name], []byte{'\n'}), bytes.Split(text, []byte{'\n'})
					line := 0
					for line < len(before) && line < len(after) && bytes.Equal(before[line], after[line]) {
						line++
					}
					want, got := "<end>", "<end>"
					if line < len(before) {
						want = string(before[line])
					}
					if line < len(after) {
						got = string(after[line])
					}
					t.Fatalf("%s stream changed between processes (run %d, GOMAXPROCS=%d), text line %d:\nwant %.500s\ngot  %.500s", name, run, procs, line+1, want, got)
				}
			}
		}
	}
}
