package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestTheRequestsAreTheThreeVerbs, over the state itself rather than over a
// socket: what a request does is this, and the socket only carries it.
func TestTheRequestsAreTheThreeVerbs(t *testing.T) {
	held, err := open(filepath.Join(t.TempDir(), "witness"), witnessSize)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := held.close(); err != nil {
			t.Error(err)
		}
	})
	for _, step := range []struct {
		request []string
		want    string
	}{
		{[]string{"fill", "6"}, "ok filled 1048576 bytes with seed 6"},
		{[]string{"check", "6", "0"}, "ok 1048576 bytes of memory and disk hold (seed 6, step 0)"},
		{[]string{"mutate", "1"}, "ok rewrote "},
		{[]string{"check", "6", "1"}, "ok 1048576 bytes of memory and disk hold (seed 6, step 1)"},
	} {
		if got := perform(held, step.request); !strings.HasPrefix(got, step.want) {
			t.Fatalf("%v answered %q, want it to begin %q", step.request, got, step.want)
		}
	}
	// A check against what the guest does not hold is an error the caller reads
	// as an exit status, with the offset in it.
	answer := perform(held, []string{"check", "6", "0"})
	if !strings.HasPrefix(answer, "error memory differs at offset ") {
		t.Fatalf("a check at the wrong step answered %q", answer)
	}
	for _, bad := range [][]string{
		{},
		{"reseed", "1"},
		{"mutate"},
		{"check", "6"},
		{"mutate", "later"},
		{"mutate", "0"},
	} {
		if answer := perform(held, bad); !strings.HasPrefix(answer, "error ") {
			t.Fatalf("%v answered %q, want a refusal", bad, answer)
		}
	}
}

// TestTheCommandLineNamesWhatItNeeds. Every one of these is a run that would
// otherwise check a guest against the wrong thing and pass.
func TestTheCommandLineNamesWhatItNeeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"fill without a seed", []string{"fill", "--disk", "/tmp/w", "--memory", "4K"}, "needs --seed"},
		{"fill without a disk", []string{"fill", "--seed", "1", "--memory", "4K"}, "needs --disk"},
		{"fill without memory", []string{"fill", "--seed", "1", "--disk", "/tmp/w"}, "needs --memory"},
		{"mutate without a step", []string{"mutate"}, "needs --step"},
		{"mutate to the fill", []string{"mutate", "--step", "0"}, "needs --step"},
		{"check without a seed", []string{"check", "--step", "1"}, "needs --seed and --step"},
		{"check without a step", []string{"check", "--seed", "1"}, "needs --seed and --step"},
		{"an unknown command", []string{"reseed"}, `no command named "reseed"`},
		{"an unknown flag", []string{"check", "--generation", "1"}, "no flag named --generation"},
		{"nothing at all", nil, "no command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.args)
			if err == nil {
				t.Fatalf("%v was accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v was refused with %q, want it to say %q", tc.args, err, tc.want)
			}
		})
	}
}
