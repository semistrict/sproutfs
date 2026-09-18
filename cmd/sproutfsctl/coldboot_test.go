package main

import (
	"bytes"
	"errors"
	"net/http"
	"testing"

	"github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/api/orch"
)

// A cold start is a start with one word added, and the sizes a cold boot may
// give the VM are written the way an operator writes a size.
func TestParseColdStart(t *testing.T) {
	for _, item := range []struct {
		name string
		args []string
		want invocation
	}{
		{"plain", []string{"start", "vm-1", "--cold"},
			invocation{Command: "start", Target: "vm-1", Count: 1, Cold: true}},
		{"with a host", []string{"start", "vm-1", "--cold", "--to", "host-1"},
			invocation{Command: "start", Target: "vm-1", Count: 1, Cold: true, To: "host-1"}},
		{"resized", []string{"start", "vm-1", "--cold", "--memory", "1G", "--disk=4G"},
			invocation{Command: "start", Target: "vm-1", Count: 1, Cold: true,
				Memory: 1 << 30, Disk: 4 << 30}},
		{"in bytes", []string{"start", "vm-1", "--cold", "--memory", "536870912"},
			invocation{Command: "start", Target: "vm-1", Count: 1, Cold: true, Memory: 512 << 20}},
	} {
		t.Run(item.name, func(t *testing.T) {
			got, err := parse(item.args)
			if err != nil {
				t.Fatalf("parsing %v: %v", item.args, err)
			}
			if got != item.want {
				t.Fatalf("parsed %+v, want %+v", got, item.want)
			}
		})
	}
}

// A shape without --cold is refused by the CLI itself: a warm start brings the
// VM back at the shape its memory describes, and a flag that quietly did
// nothing would be worse than one that is not accepted.
func TestParseRefusesAShapeWithoutCold(t *testing.T) {
	for _, args := range [][]string{
		{"start", "vm-1", "--memory", "1G"},
		{"start", "vm-1", "--disk", "4G"},
	} {
		if _, err := parse(args); !errors.Is(err, errUsage) {
			t.Fatalf("parsing %v = %v, want a usage error", args, err)
		}
	}
}

// A size that is not one is refused where it is typed rather than by a host
// three hops away.
func TestParseRefusesASizeThatIsNotOne(t *testing.T) {
	for _, args := range [][]string{
		{"start", "vm-1", "--cold", "--memory", "lots"},
		{"start", "vm-1", "--cold", "--disk", "-4G"},
		{"start", "vm-1", "--cold", "--memory", "0"},
	} {
		if _, err := parse(args); !errors.Is(err, errUsage) {
			t.Fatalf("parsing %v = %v, want a usage error", args, err)
		}
	}
}

// Every other command takes neither the flag nor the sizes: a cold boot is a
// start and nothing else.
func TestOnlyStartIsCold(t *testing.T) {
	for _, args := range [][]string{
		{"recover", "vm-1", "--cold"},
		{"create", "--memory", "1G"},
		{"migrate", "vm-1", "--disk", "4G"},
	} {
		if _, err := parse(args); !errors.Is(err, errUsage) {
			t.Fatalf("parsing %v = %v, want a usage error", args, err)
		}
	}
}

// TestColdStartPrintsThatTheVMCameBackWithoutItsMemory, and carries the shape
// it asked for. The checkpoint it names is the one that discarded the memory
// rather than the one the stop published, so a flow reading the number after
// "checkpoint" gets the checkpoint the VM is at now.
func TestColdStartPrintsThatTheVMCameBackWithoutItsMemory(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.StartResult{Host: "sproutfs-host-b",
			Result: host.OpenResult{VM: host.VM{ID: "vm-1", Checkpoint: 20}, Total: 1.25, Cold: true}}
	})
	var out bytes.Buffer
	command := invocation{Command: "start", Target: "vm-1", To: "sproutfs-host-b",
		Cold: true, Memory: 1 << 30, Disk: 4 << 30}
	if err := execute(t.Context(), client, command, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "vm-1 cold started on sproutfs-host-b from checkpoint 20 in 1.250s\n"
	if out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	asked := `POST /vms/vm-1/start {"to":"sproutfs-host-b","cold":true,"memory":1073741824,"disk":4294967296}`
	if len(stub.requests) != 1 || stub.requests[0] != asked {
		t.Fatalf("the CLI asked for %v", stub.requests)
	}
}
