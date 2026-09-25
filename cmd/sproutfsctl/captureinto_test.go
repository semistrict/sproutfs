package main

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
)

// capture --new asks for a new VM and prints which VM the source was captured
// into, and at which checkpoint.
func TestCaptureNewPrintsTheNewVM(t *testing.T) {
	command, err := parse([]string{"capture", "vm-1", "--new"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (invocation{Command: "capture", Target: "vm-1", Count: 1, New: true}); command != want {
		t.Fatalf("parsed %+v, want %+v", command, want)
	}
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.CaptureResult{Host: "sproutfs-host-a",
			Result: host.CaptureResult{VM: "vm-2", Checkpoint: 12, Publish: 0.5}}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, command, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if want := "vm-1 captured into vm-2 at checkpoint 12 on sproutfs-host-a in 0.500s\n"; out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	asked := `POST /vms/vm-1/capture {"new":true}`
	if len(stub.requests) != 1 || stub.requests[0] != asked {
		t.Fatalf("the CLI asked for %v, want %s", stub.requests, asked)
	}
}
