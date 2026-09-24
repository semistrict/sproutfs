package main

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/api/orch"
)

func TestStopPrintsWhichHostClosedTheVM(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.StopResult{VM: "vm-1", Host: "sproutfs-host-a",
			Checkpoint: 18, Total: 0.42}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "stop", Target: "vm-1"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "stopped vm-1 on sproutfs-host-a at checkpoint 18 in 0.420s\n"
	if out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	if len(stub.requests) != 1 || stub.requests[0] != "POST /vms/vm-1/stop {}" {
		t.Fatalf("the CLI asked for %v", stub.requests)
	}
}

// TestSuspendAsksToKeepTheMemory: a plain stop keeps a VM's disks, and
// --suspend is how an operator asks for its memory and VMM state as well.
func TestSuspendAsksToKeepTheMemory(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.StopResult{VM: "vm-1", Host: "sproutfs-host-a",
			Checkpoint: 18, Total: 0.42}
	})
	var out bytes.Buffer
	command := invocation{Command: "stop", Target: "vm-1", Suspend: true}
	if err := execute(t.Context(), client, command, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "suspended vm-1 on sproutfs-host-a at checkpoint 18 in 0.420s\n"
	if out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	if len(stub.requests) != 1 || stub.requests[0] != `POST /vms/vm-1/stop {"suspend":true}` {
		t.Fatalf("the CLI asked for %v", stub.requests)
	}
}

// TestStartPrintsWhereTheVMCameBackAndAtWhichCheckpoint, which is what a flow
// checking a guest against its own last state needs to read.
func TestStartPrintsWhereTheVMCameBackAndAtWhichCheckpoint(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.StartResult{Host: "sproutfs-host-b",
			Result: host.OpenResult{VM: host.VM{ID: "vm-1", Checkpoint: 19}, Total: 1.25}}
	})
	var out bytes.Buffer
	command := invocation{Command: "start", Target: "vm-1", To: "sproutfs-host-b"}
	if err := execute(t.Context(), client, command, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "vm-1 started on sproutfs-host-b from checkpoint 19 in 1.250s\n"
	if out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	if len(stub.requests) != 1 || stub.requests[0] != `POST /vms/vm-1/start {"to":"sproutfs-host-b"}` {
		t.Fatalf("the CLI asked for %v", stub.requests)
	}
}

// TestCheckOnADeploymentThatAgreesWithItselfSaysSoAndSucceeds.
func TestCheckOnADeploymentThatAgreesWithItselfSaysSoAndSucceeds(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.CheckResult{OK: true}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "check"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "the deployment's durable state agrees with itself\n" {
		t.Fatalf("printed %q", out.String())
	}
	if len(stub.requests) != 1 || stub.requests[0] != "GET /check" {
		t.Fatalf("the CLI asked for %v", stub.requests)
	}
}

// TestCheckPrintsEveryViolationAndFails: a run that ends with a check wants an
// exit code, and an operator wants every object that is wrong rather than a
// count of them.
func TestCheckPrintsEveryViolationAndFails(t *testing.T) {
	client, _ := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.CheckResult{Violations: []orch.Violation{
			{Key: "demo/vm/vm-a/ckpt/7/index", Class: "violation",
				Message: "the index names a part that is not there"},
			{Key: "demo/vm/vm-b/ckpt/2/part/0", Class: "unrecorded-vm",
				Message: "the VM this object belongs to has no control record"},
		}}
	})
	var out bytes.Buffer
	err := execute(t.Context(), client, invocation{Command: "check"}, nil, &out, &out)
	if err == nil {
		t.Fatal("a deployment with violations checked out clean")
	}
	if err.Error() != "the deployment disagrees with itself: 2 violations" {
		t.Fatalf("error %q", err)
	}
	for _, want := range []string{
		"[violation] demo/vm/vm-a/ckpt/7/index: the index names a part that is not there",
		"[unrecorded-vm] demo/vm/vm-b/ckpt/2/part/0: the VM this object belongs to has no control record",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("printed\n%s\nwant a line reading %q", out.String(), want)
		}
	}
}
