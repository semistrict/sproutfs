package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/api/guest"
)

// TestHealthzAnswers is what a host asks a guest that may still be booting.
func TestHealthzAnswers(t *testing.T) {
	recorder := httptest.NewRecorder()
	newServer(0).ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/healthz answered %d, want 200: %s", recorder.Code, recorder.Body)
	}
	if got := strings.TrimSpace(recorder.Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("/healthz answered %s, want {\"status\":\"ok\"}", got)
	}
}

// TestExecReportsWhatTheCommandPrinted is the whole of what an exec is for.
func TestExecReportsWhatTheCommandPrinted(t *testing.T) {
	result := execute(t, guest.ExecRequest{Cmd: "echo out; echo err >&2"})
	if result.Exit != 0 {
		t.Fatalf("the command exited %d, want 0: %+v", result.Exit, result)
	}
	if result.Stdout != "out\n" {
		t.Fatalf("stdout is %q, want \"out\\n\"", result.Stdout)
	}
	if result.Stderr != "err\n" {
		t.Fatalf("stderr is %q, want \"err\\n\"", result.Stderr)
	}
	if result.Truncated {
		t.Fatalf("two lines were reported as truncated: %+v", result)
	}
}

// TestExecReportsAFailingCommandAsItsStatus: a command that fails is an answer,
// not a failure of the agent, so the request still succeeds.
func TestExecReportsAFailingCommandAsItsStatus(t *testing.T) {
	result := execute(t, guest.ExecRequest{Cmd: "echo nope >&2; exit 3"})
	if result.Exit != 3 {
		t.Fatalf("the command exited %d, want 3: %+v", result.Exit, result)
	}
	if result.Stderr != "nope\n" {
		t.Fatalf("stderr is %q, want \"nope\\n\"", result.Stderr)
	}
}

// TestExecKillsACommandThatRunsPastItsTimeout, which is what keeps a host from
// waiting on a guest forever.
func TestExecKillsACommandThatRunsPastItsTimeout(t *testing.T) {
	result := execute(t, guest.ExecRequest{Cmd: "sleep 30", Timeout: 0.05})
	if result.Exit != timeoutExit {
		t.Fatalf("a timed-out command exited %d, want %d: %+v", result.Exit, timeoutExit, result)
	}
	if !strings.Contains(result.Stderr, "ran past its timeout") {
		t.Fatalf("stderr is %q, want it to say the command ran past its timeout", result.Stderr)
	}
}

// TestExecTruncatesALargeOutput rather than carrying it through two proxies.
func TestExecTruncatesALargeOutput(t *testing.T) {
	result := execute(t, guest.ExecRequest{Cmd: "yes sproutfs | head -c 2000000"})
	if result.Exit != 0 {
		t.Fatalf("the command exited %d, want 0: %s", result.Exit, result.Stderr)
	}
	if len(result.Stdout) != guest.MaxOutputBytes {
		t.Fatalf("stdout is %d bytes, want the %d-byte bound", len(result.Stdout), guest.MaxOutputBytes)
	}
	if !result.Truncated {
		t.Fatal("a truncated output was not reported as truncated")
	}
}

// TestExecRefusesARequestWithNoCommand, which is the one request shape the
// agent will not act on.
func TestExecRefusesARequestWithNoCommand(t *testing.T) {
	recorder := post(t, `{"cmd":""}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an empty command answered %d, want 400: %s", recorder.Code, recorder.Body)
	}
	if !strings.Contains(recorder.Body.String(), "exec needs a command") {
		t.Fatalf("the failure reads %s, want it to say a command is needed", recorder.Body)
	}
}

// TestExecRefusesABodyThatIsNotJSON.
func TestExecRefusesABodyThatIsNotJSON(t *testing.T) {
	recorder := post(t, "not json")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a malformed body answered %d, want 400: %s", recorder.Code, recorder.Body)
	}
}

func execute(t *testing.T, request guest.ExecRequest) guest.ExecResult {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	recorder := post(t, string(raw))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/exec answered %d, want 200: %s", recorder.Code, recorder.Body)
	}
	var result guest.ExecResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding %s: %v", recorder.Body, err)
	}
	return result
}

func post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/exec", strings.NewReader(body))
	newServer(0).ServeHTTP(recorder, request)
	return recorder
}

// TestExecKillsWhatTheCommandLeftBehind: a shell that backgrounds something
// exits at once, and the child it left holds the pipes the agent reads the
// output through. Killing the shell alone therefore ends nothing: the agent
// waits on those pipes until the background process finishes on its own, which
// is exactly the wait the timeout exists to prevent, and the process goes on
// running in the guest afterwards. The command runs in its own process group
// and the deadline kills the group.
func TestExecKillsWhatTheCommandLeftBehind(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "still-running")
	// The shell exits immediately; what it left behind would write the marker
	// a second from now if nothing killed it.
	background := fmt.Sprintf("(sleep 2; touch %s) & echo started", marker)
	began := time.Now()
	result := execute(t, guest.ExecRequest{Cmd: background, Timeout: 0.2})
	took := time.Since(began)
	if took > time.Second {
		t.Fatalf("the exec took %s: it waited for a process the timeout did not kill", took)
	}
	if result.Stdout != "started\n" {
		t.Fatalf("stdout is %q, want \"started\\n\"", result.Stdout)
	}
	// Long enough that a surviving process would have written it.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the process the command left behind outlived the exec that started it")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// TestExecAdmitsABoundedNumberOfCommands: one host reaches one guest over one
// vsock, and a client that retries — or a fan-out the demo drives — can put
// more commands into a guest than its own memory and processes can carry. The
// agent admits a bounded number at once and refuses the rest, which a caller
// can act on, rather than running everything and being killed for it.
func TestExecAdmitsABoundedNumberOfCommands(t *testing.T) {
	server := newServer(1)
	running := filepath.Join(t.TempDir(), "running")
	send := func(request guest.ExecRequest) *httptest.ResponseRecorder {
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, httptest.NewRequestWithContext(t.Context(),
			http.MethodPost, "/exec", strings.NewReader(string(raw))))
		return recorder
	}
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- send(guest.ExecRequest{Cmd: fmt.Sprintf("touch %s; sleep 1", running), Timeout: 5})
	}()
	// The second request is sent only once the first is certainly running.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(running); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first command never started")
		}
		time.Sleep(time.Millisecond)
	}
	refused := send(guest.ExecRequest{Cmd: "echo second"})
	if refused.Code != http.StatusTooManyRequests {
		t.Fatalf("a second command answered %d, want 429: %s", refused.Code, refused.Body)
	}
	if strings.Contains(refused.Body.String(), "second") {
		t.Fatalf("the refused command ran anyway: %s", refused.Body)
	}
	if answered := <-first; answered.Code != http.StatusOK {
		t.Fatalf("the admitted command answered %d: %s", answered.Code, answered.Body)
	}
	// The slot comes back when the command that held it finishes.
	if after := send(guest.ExecRequest{Cmd: "echo third"}); after.Code != http.StatusOK {
		t.Fatalf("a command after the first finished answered %d: %s", after.Code, after.Body)
	}
}

// TestExecHoldsNoMoreOutputThanItReturns: the result is truncated to
// MaxOutputBytes either way, but a command printing a filesystem used to be
// held whole in the agent's memory first — in a guest whose whole RAM is the
// point of the deployment. What is kept is the bound; the rest is counted and
// dropped, and the command goes on running rather than blocking on a pipe
// nothing drains.
func TestExecHoldsNoMoreOutputThanItReturns(t *testing.T) {
	sink := &boundedOutput{}
	const chunk = 64 << 10
	block := make([]byte, chunk)
	for written := 0; written < 8*guest.MaxOutputBytes; written += chunk {
		n, err := sink.Write(block)
		if err != nil || n != chunk {
			t.Fatalf("writing %d bytes returned %d, %v: a sink that stops draining stalls the command", chunk, n, err)
		}
	}
	if len(sink.Bytes()) != guest.MaxOutputBytes {
		t.Fatalf("the sink holds %d bytes, want the %d-byte bound", len(sink.Bytes()), guest.MaxOutputBytes)
	}
	if !sink.Truncated() {
		t.Fatal("a sink past its bound did not report truncation")
	}
}

// TestExecKillsACommandWhoseCallerWentAway: the host reaches this agent over one
// vsock stream, and a stream can end under a command that is still running —
// the VMM resets the guest's connections when the VM it is running is handed
// off, and the host on the other end is gone with it. Nothing will ever read the
// answer, so the command must not go on running in the guest, and the slot it
// holds must come back for the callers that can still be served.
func TestExecKillsACommandWhoseCallerWentAway(t *testing.T) {
	// One slot, so a command that leaked it would be visible as a refusal.
	server := httptest.NewServer(newServer(1))
	defer server.Close()

	dir := t.TempDir()
	running := filepath.Join(dir, "running")
	survived := filepath.Join(dir, "survived")
	body, err := json.Marshal(guest.ExecRequest{
		Cmd:     fmt.Sprintf("touch %s; sleep 5; touch %s", running, survived),
		Timeout: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	address := strings.TrimPrefix(server.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	request := fmt.Sprintf("POST /exec HTTP/1.1\r\nHost: guest\r\nContent-Type: application/json\r\n"+
		"Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, running)

	// The caller goes away with the command still running, exactly as a reset
	// vsock stream does.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	// The slot comes back, which is the agent having stopped waiting on a command
	// nothing will read. Asking for it is what proves it: the second command is
	// admitted only once the first has let go.
	client := &http.Client{Timeout: 20 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for {
		response, err := client.Post(server.URL+"/exec", "application/json",
			strings.NewReader(`{"cmd":"echo after"}`))
		if err != nil {
			t.Fatal(err)
		}
		status := response.StatusCode
		_ = response.Body.Close()
		if status == http.StatusOK {
			break
		}
		if status != http.StatusTooManyRequests {
			t.Fatalf("a command after the caller went away answered %d, want 200", status)
		}
		if time.Now().After(deadline) {
			t.Fatal("the command the caller abandoned still holds the agent's only slot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// And the command itself is dead: what it would have done next never happens.
	time.Sleep(time.Second)
	if _, err := os.Stat(survived); err == nil {
		t.Fatal("the command outlived the caller that abandoned it")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// waitForFile blocks until a command running in this test has created path.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never created: the command never started", path)
		}
		time.Sleep(time.Millisecond)
	}
}
