package main

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
)

// capture and stop take --keep, kept takes a VM and release takes the VM and
// the checkpoint as VM@CHECKPOINT.
func TestParseKept(t *testing.T) {
	for _, item := range []struct {
		args []string
		want invocation
	}{
		{[]string{"capture", "vm-1", "--keep"},
			invocation{Command: "capture", Target: "vm-1", Count: 1, Keep: true}},
		{[]string{"stop", "vm-1", "--suspend", "--keep"},
			invocation{Command: "stop", Target: "vm-1", Count: 1, Suspend: true, Keep: true}},
		{[]string{"kept", "vm-1"},
			invocation{Command: "kept", Target: "vm-1", Count: 1}},
		{[]string{"release", "vm-1@42"},
			invocation{Command: "release", Target: "vm-1", Count: 1, Checkpoint: 42}},
	} {
		got, err := parse(item.args)
		if err != nil {
			t.Fatalf("parsing %v: %v", item.args, err)
		}
		if got != item.want {
			t.Fatalf("parsed %+v, want %+v", got, item.want)
		}
	}
	for _, args := range [][]string{
		{"release", "vm-1"},
		{"release", "vm-1@"},
		{"release", "@42"},
		{"release", "vm-1@zero"},
		{"capture", "vm-1", "--new", "--keep"},
		{"kept"},
	} {
		if _, err := parse(args); !errors.Is(err, errUsage) {
			t.Fatalf("parsing %v = %v, want a usage error", args, err)
		}
	}
}

// A kept capture says the checkpoint it published is kept.
func TestAKeptCaptureSaysSo(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.CaptureResult{Host: "sproutfs-host-a",
			Result: host.CaptureResult{VM: "vm-1", Checkpoint: 12, Pause: 0.05, Publish: 0.5}}
	})
	var out bytes.Buffer
	command := invocation{Command: "capture", Target: "vm-1", Keep: true}
	if err := execute(t.Context(), client, command, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if want := "vm-1 checkpoint 12 (kept) on sproutfs-host-a: pause 0.050s, publish 0.500s\n"; out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	asked := `POST /vms/vm-1/capture {"keep":true}`
	if len(stub.requests) != 1 || stub.requests[0] != asked {
		t.Fatalf("the CLI asked for %v, want %s", stub.requests, asked)
	}
}

// kept prints one row per kept checkpoint: when it was selected, whether a
// create from it resumes the guest, and whether a VM was created from it.
func TestKeptListsTheCheckpoints(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 30, 0, 0, time.UTC)
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, host.KeptResult{VM: "vm-1", Kept: []host.Kept{
			{Checkpoint: 7, Time: at, State: true, Forked: true},
			{Checkpoint: 9, Time: at.Add(time.Minute)},
		}}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "kept", Target: "vm-1"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := `CHECKPOINT  TIME                  STATE  FORKED
7           2026-09-25T12:30:00Z  true   true
9           2026-09-25T12:31:00Z  false  false
`
	if out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	if len(stub.requests) != 1 || stub.requests[0] != "GET /vms/vm-1/kept" {
		t.Fatalf("the CLI asked for %q", stub.requests)
	}
}

// release names the checkpoint in the path.
func TestReleaseNamesTheCheckpoint(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, map[string]string{"status": "ok"}
	})
	var out bytes.Buffer
	command := invocation{Command: "release", Target: "vm-1", Checkpoint: 42}
	if err := execute(t.Context(), client, command, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if want := "released checkpoint 42 of vm-1\n"; out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	if len(stub.requests) != 1 || stub.requests[0] != "POST /vms/vm-1/kept/42/release" {
		t.Fatalf("the CLI asked for %q", stub.requests)
	}
}
