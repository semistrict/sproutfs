package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/api/orch"
)

// stubOrchestrator answers the CLI with prepared results and records what it
// was asked for.
type stubOrchestrator struct {
	mu       sync.Mutex
	requests []string
	handler  func(*http.Request) (int, any)
}

func (s *stubOrchestrator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := ""
	if r.Body != nil {
		raw := make([]byte, 4096)
		count, _ := r.Body.Read(raw)
		body = strings.TrimSpace(string(raw[:count]))
	}
	s.mu.Lock()
	s.requests = append(s.requests, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+body))
	s.mu.Unlock()
	status, value := s.handler(r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	if _, err := w.Write(raw); err != nil {
		panic(err)
	}
}

func serve(t *testing.T, handler func(*http.Request) (int, any)) (*orch.Client, *stubOrchestrator) {
	t.Helper()
	stub := &stubOrchestrator{handler: handler}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	return orch.NewClient(server.URL, server.Client(), ""), stub
}

func TestCreatePrintsWhereTheVMWentAndWhatItCost(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.CreateResult{Host: "sproutfs-host-a",
			Result: host.CreateResult{VM: host.VM{ID: "vm-1"}, Template: 4, Fork: 0.5, Boot: 1.25,
				Root: 0.75, Total: 6.5}}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "create", Template: "alpine"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	// The root checkpoint is part of what a create costs: until it is published
	// the VM runs on this host and nowhere else.
	want := "vm-1 on sproutfs-host-a (template 4.00s, fork 0.50s, boot 1.25s, root 0.75s, total 6.50s)\n"
	if out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	if len(stub.requests) != 1 || stub.requests[0] != `POST /vms {"template":"alpine"}` {
		t.Fatalf("the CLI asked for %v", stub.requests)
	}
}

func TestListPrintsEveryVMAndItsHost(t *testing.T) {
	client, _ := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, []orch.VM{
			{ID: "vm-1", Host: "sproutfs-host-a", State: "running", Checkpoint: 12},
			{ID: "vm-2", State: "stopped", Checkpoint: 3},
			{ID: "vm-3", Host: "sproutfs-host-a", State: "migrating",
				From: "sproutfs-host-a", To: "sproutfs-host-b", Checkpoint: 5},
		}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "list"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "VM    HOST             STATE                                       CHECKPOINT  LOSS  PRIVATE\n" +
		"vm-1  sproutfs-host-a  running                                     12          -     -\n" +
		"vm-2  -                stopped                                     3           -     -\n" +
		"vm-3  sproutfs-host-a  migrating sproutfs-host-a->sproutfs-host-b  5           -     -\n"
	if out.String() != want {
		t.Fatalf("printed\n%s\nwant\n%s", out.String(), want)
	}
}

// The listing is where an operator sees what losing a host would cost each VM
// in time, and which VMs are already past their window with their guests held
// back. A VM holding nothing unpublished shows a dash: a zero would read as a
// VM that is somehow always durable.
func TestListPrintsEachVMsLossWindow(t *testing.T) {
	client, _ := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, []orch.VM{
			{ID: "vm-1", Host: "sproutfs-host-a", State: "running", Checkpoint: 12,
				LossWindow: 90 * time.Second},
			{ID: "vm-2", Host: "sproutfs-host-a", State: "running", Checkpoint: 8,
				LossWindow: 7 * time.Minute, Waiting: true},
			{ID: "vm-3", State: "stopped", Checkpoint: 3},
		}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "list"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "VM    HOST             STATE    CHECKPOINT  LOSS          PRIVATE\n" +
		"vm-1  sproutfs-host-a  running  12          1m30s         -\n" +
		"vm-2  sproutfs-host-a  running  8           7m0s waiting  -\n" +
		"vm-3  -                stopped  3           -             -\n"
	if out.String() != want {
		t.Fatalf("printed\n%s\nwant\n%s", out.String(), want)
	}
}

// The listing is also where an operator sees what each VM's memory costs its
// host that nothing shares: the pages its guest has written since its last
// checkpoint. A VM holding none, and one no live host reports, show a dash.
func TestListPrintsEachVMsPrivateBytes(t *testing.T) {
	client, _ := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, []orch.VM{
			{ID: "vm-1", Host: "sproutfs-host-a", State: "running", Checkpoint: 12,
				PrivateBytes: 114 << 20},
			{ID: "vm-2", Host: "sproutfs-host-a", State: "running", Checkpoint: 8},
			{ID: "vm-3", State: "stopped", Checkpoint: 3},
		}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "list"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "VM    HOST             STATE    CHECKPOINT  LOSS  PRIVATE\n" +
		"vm-1  sproutfs-host-a  running  12          -     114 MiB\n" +
		"vm-2  sproutfs-host-a  running  8           -     -\n" +
		"vm-3  -                stopped  3           -     -\n"
	if out.String() != want {
		t.Fatalf("printed\n%s\nwant\n%s", out.String(), want)
	}
}

func TestForkPrintsEveryForksTimings(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.ForkResult{Host: "sproutfs-host-a", To: "sproutfs-host-a",
			Children: []string{"vm-2", "vm-3"}, Capture: 0.012, Start: 0.3, Total: 0.316}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "fork", Target: "vm-1", Count: 2}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	// One pause of the parent starts both children, so both report the one
	// pause it cost.
	want := "FORK  HOST             PAUSE  START  TOTAL\n" +
		"vm-2  sproutfs-host-a  0.012  0.300  0.316\n" +
		"vm-3  sproutfs-host-a  0.012  0.300  0.316\n"
	if out.String() != want {
		t.Fatalf("printed\n%s\nwant\n%s", out.String(), want)
	}
	if len(stub.requests) != 1 || stub.requests[0] != `POST /vms/vm-1/fork {"count":2}` {
		t.Fatalf("the CLI asked for %v", stub.requests)
	}
}

// A fork placed on another host names it in the request, which is the whole of
// what a cross-host fork costs the caller.
func TestForkOnAnotherHostNamesIt(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.ForkResult{Host: "sproutfs-host-a", To: "sproutfs-host-b",
			Children: []string{"vm-2"}, Capture: 0.02, Start: 0.4, Total: 0.5}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "fork", Target: "vm-1", Count: 1, To: "sproutfs-host-b"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "FORK  HOST             PAUSE  START  TOTAL\n" +
		"vm-2  sproutfs-host-b  0.020  0.400  0.500\n"
	if out.String() != want {
		t.Fatalf("printed\n%s\nwant\n%s", out.String(), want)
	}
	if len(stub.requests) != 1 || stub.requests[0] != `POST /vms/vm-1/fork {"count":1,"to":"sproutfs-host-b"}` {
		t.Fatalf("the CLI asked for %v", stub.requests)
	}
}

func TestMigratePrintsThePauseAndTheStream(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.MigrateResult{VM: "vm-1", From: "sproutfs-host-a", To: "sproutfs-host-b",
			Pause: 0.082, Stream: 1.4, PeerPages: 96, Unpublished: 12}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "migrate", Target: "vm-1", To: "sproutfs-host-b"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "vm-1 moved from sproutfs-host-a to sproutfs-host-b: pause 0.082s, stream 1.400s, " +
		"96 pages from the source, 12 unpublished\n"
	if out.String() != want {
		t.Fatalf("printed %q, want %q", out.String(), want)
	}
	if len(stub.requests) != 1 || stub.requests[0] != `POST /vms/vm-1/migrate {"to":"sproutfs-host-b"}` {
		t.Fatalf("the CLI asked for %v", stub.requests)
	}
}

func TestAFailureIsWhatTheOrchestratorSaid(t *testing.T) {
	client, _ := serve(t, func(*http.Request) (int, any) {
		return http.StatusConflict, orch.Error{Op: "recover", Message: "a live host still runs that VM: sproutfs-host-a runs vm-1"}
	})
	var out bytes.Buffer
	err := execute(t.Context(), client, invocation{Command: "recover", Target: "vm-1"}, nil, &out, &out)
	if err == nil {
		t.Fatal("a refused recovery reported success")
	}
	if err.Error() != "recover: a live host still runs that VM: sproutfs-host-a runs vm-1" {
		t.Fatalf("error %q", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a failed command printed %q", out.String())
	}
}

// TestConsoleForwardsTypedLinesAndPrintsTheGuestsOutput is the console session:
// what the guest printed comes out, and what was typed goes in.
func TestConsoleForwardsTypedLinesAndPrintsTheGuestsOutput(t *testing.T) {
	client, stub := serve(t, func(r *http.Request) (int, any) {
		if r.Method == http.MethodPost {
			return http.StatusOK, map[string]string{"status": "ok"}
		}
		// The guest printed one line and has printed nothing since, so a
		// session that polls again prints nothing more.
		if r.URL.Query().Get("since") == "0" {
			return http.StatusOK, orch.Console{VM: "vm-1", Offset: 0, Next: 6, Data: "ready\n"}
		}
		return http.StatusOK, orch.Console{VM: "vm-1", Offset: 6, Next: 6}
	})
	var out bytes.Buffer
	err := execute(t.Context(), client, invocation{Command: "console", Target: "vm-1"},
		strings.NewReader("uname -a\n"), &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "ready\n" {
		t.Fatalf("printed %q", out.String())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !containsRequest(stub.requests, `POST /vms/vm-1/console {"data":"uname -a\n"}`) {
		t.Fatalf("the typed line was not sent: %v", stub.requests)
	}
	if !containsRequest(stub.requests, "GET /vms/vm-1/console?since=0") {
		t.Fatalf("the console was not read from the start: %v", stub.requests)
	}
}

// TestConsoleForKeepsReadingAfterTheInputEnds is what a scripted flow needs: a
// command piped in is exhausted at once, and the guest answers it later.
func TestConsoleForKeepsReadingAfterTheInputEnds(t *testing.T) {
	var reads atomic.Int64
	client, stub := serve(t, func(r *http.Request) (int, any) {
		if r.Method == http.MethodPost {
			return http.StatusOK, map[string]string{"status": "ok"}
		}
		// The guest has printed nothing by the time the input ends. A session
		// that stopped there would print nothing at all.
		if reads.Add(1) <= 2 {
			return http.StatusOK, orch.Console{VM: "vm-1"}
		}
		if r.URL.Query().Get("since") == "0" {
			return http.StatusOK, orch.Console{VM: "vm-1", Next: 12, Data: "SPROUTFS-OK\n"}
		}
		return http.StatusOK, orch.Console{VM: "vm-1", Offset: 12, Next: 12}
	})
	var out bytes.Buffer
	command := invocation{Command: "console", Target: "vm-1", For: time.Second}
	if err := execute(t.Context(), client, command, strings.NewReader("echo SPROUTFS-OK\n"), &out, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "SPROUTFS-OK\n" {
		t.Fatalf("printed %q, want the guest's answer", out.String())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !containsRequest(stub.requests, `POST /vms/vm-1/console {"data":"echo SPROUTFS-OK\n"}`) {
		t.Fatalf("the typed line was not sent: %v", stub.requests)
	}
}

func TestHostsPrintsTheSharedPageCount(t *testing.T) {
	client, _ := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, []orch.Host{{Name: "sproutfs-host-a", Ready: true,
			Running: []string{"vm-1", "vm-2"}, Serving: []string{},
			Pager: host.Pager{ResidentPages: 900, SharedPages: 512},
			Pages: host.Pages{Served: 64}}}
	})
	var out bytes.Buffer
	if err := execute(t.Context(), client, invocation{Command: "hosts"}, nil, &out, &out); err != nil {
		t.Fatal(err)
	}
	want := "HOST             READY  RUNNING  SERVING  RESIDENT  SHARED  SERVED  STATE\n" +
		"sproutfs-host-a  true   2        0        900       512     64      ok\n"
	if out.String() != want {
		t.Fatalf("printed\n%s\nwant\n%s", out.String(), want)
	}
}

func containsRequest(requests []string, want string) bool {
	for _, request := range requests {
		if request == want {
			return true
		}
	}
	return false
}

// TestExecPrintsTheGuestsStreamsApart: what a command printed goes to stdout
// and what it complained about to stderr, so a scripted flow reading one is not
// sifting the other out of it.
func TestExecPrintsTheGuestsStreamsApart(t *testing.T) {
	client, stub := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.ExecOutcome{Host: "sproutfs-host-a", VM: "vm-1",
			Result: orch.ExecResult{Exit: 0, Stdout: "SPROUTFS-OK\n", Stderr: "a warning\n"}}
	})
	var out, problems bytes.Buffer
	command := invocation{Command: "exec", Target: "vm-1", Cmd: "echo SPROUTFS-OK", Timeout: 5 * time.Second}
	if err := execute(t.Context(), client, command, nil, &out, &problems); err != nil {
		t.Fatal(err)
	}
	if out.String() != "SPROUTFS-OK\n" {
		t.Fatalf("stdout is %q", out.String())
	}
	if problems.String() != "a warning\n" {
		t.Fatalf("stderr is %q", problems.String())
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	want := `POST /vms/vm-1/exec {"cmd":"echo SPROUTFS-OK","timeout":5}`
	if len(stub.requests) != 1 || stub.requests[0] != want {
		t.Fatalf("the orchestrator was asked %v, want %s", stub.requests, want)
	}
}

// TestExecFailsWhenTheGuestsCommandDid, so that a flow driving the demo through
// this CLI stops on a command that did not work.
func TestExecFailsWhenTheGuestsCommandDid(t *testing.T) {
	client, _ := serve(t, func(*http.Request) (int, any) {
		return http.StatusOK, orch.ExecOutcome{Host: "sproutfs-host-a", VM: "vm-1",
			Result: orch.ExecResult{Exit: 3, Stdout: "partial\n"}}
	})
	var out, problems bytes.Buffer
	command := invocation{Command: "exec", Target: "vm-1", Cmd: "false"}
	err := execute(t.Context(), client, command, nil, &out, &problems)
	if err == nil || !strings.Contains(err.Error(), "vm-1 exited 3 on sproutfs-host-a") {
		t.Fatalf("error %v, want the exit status and the host", err)
	}
	if out.String() != "partial\n" {
		t.Fatalf("stdout is %q, want what the command did print", out.String())
	}
}
