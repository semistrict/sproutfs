package main

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
)

// A create from another VM names that VM, and the checkpoint when it is not
// the one the VM's record selects.
func TestParseCreateFrom(t *testing.T) {
	for _, item := range []struct {
		args []string
		want invocation
	}{
		{[]string{"create", "--from", "vm-1"},
			invocation{Command: "create", Count: 1, From: "vm-1"}},
		{[]string{"create", "--from=vm-1@42", "--memory", "1G"},
			invocation{Command: "create", Count: 1, From: "vm-1", FromCheckpoint: 42, Memory: 1 << 30}},
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
		{"create", "--from", "@42"},
		{"create", "--from", "vm-1@"},
		{"create", "--from", "vm-1@zero"},
		{"create", "--from", "vm-1", "--template", "alpine"},
	} {
		if _, err := parse(args); !errors.Is(err, errUsage) {
			t.Fatalf("parsing %v = %v, want a usage error", args, err)
		}
	}
}

// The create carries the checkpoint to the orchestrator.
func TestCreateFromAsksForTheCheckpoint(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.CreateResult{Host: "sproutfs-host-a",
			Result: host.CreateResult{VM: host.VM{ID: "vm-2"}, Total: 1.5}}
	})
	var out bytes.Buffer
	command := invocation{Command: "create", From: "vm-1", FromCheckpoint: 42}
	if err := execute(t.Context(), client, command, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	asked := `POST /vms {"from":{"vm":"vm-1","checkpoint":42}}`
	if len(stub.requests) != 1 || stub.requests[0] != asked {
		t.Fatalf("the CLI asked for %v, want %s", stub.requests, asked)
	}
}
