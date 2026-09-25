package guest

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// A guest runs untrusted code, and its agent is whatever that code made of it.
// These tests stand in for such an agent behind the VMM's socket and check that
// an exec costs the host a bounded wait, a bounded read and an error.

// TestExecBoundIsTheAgents: the host waits for a command by the rule the agent
// kills it by, so the two can never disagree about whose bound fired.
func TestExecBoundIsTheAgents(t *testing.T) {
	for _, c := range []struct {
		timeout float64
		want    time.Duration
	}{
		{0, DefaultTimeout},
		{-3, DefaultTimeout},
		{1.5, 1500 * time.Millisecond},
		{600, MaxTimeout},
		{601, MaxTimeout},
		{1e12, MaxTimeout},
	} {
		if got := (ExecRequest{Cmd: "true", Timeout: c.timeout}).Bound(); got != c.want {
			t.Errorf("a timeout of %v seconds is bound at %s, want %s", c.timeout, got, c.want)
		}
	}
}

// TestExecAnswersThroughTheBoundedPath is the ordinary exchange, so the
// refusals below are refusals of what the agent sent and not of the path.
func TestExecAnswersThroughTheBoundedPath(t *testing.T) {
	socket := fakeVMM(t, acceptAll, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonhttp.Write(w, http.StatusOK, ExecResult{Exit: 3, Stdout: "out\n", Stderr: "err\n"})
	}))
	result, err := Exec(t.Context(), socket, ExecRequest{Cmd: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if result != (ExecResult{Exit: 3, Stdout: "out\n", Stderr: "err\n"}) {
		t.Fatalf("the exec answered %+v", result)
	}
}

// TestExecRefusesAnAnswerThatNeverEnds: an agent that streams without end is
// cut off at MaxResultBytes. The host reads no further, so the agent's writes
// fail once the stream's buffers are full.
func TestExecRefusesAnAnswerThatNeverEnds(t *testing.T) {
	const ceiling = 1 << 30
	stopped := make(chan int64, 1)
	socket := fakeVMM(t, acceptAll, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		block := []byte(`"` + strings.Repeat("a", 64<<10-1))
		var written int64
		for written < ceiling {
			n, err := w.Write(block)
			written += int64(n)
			if err != nil {
				break
			}
		}
		stopped <- written
	}))
	_, err := Exec(t.Context(), socket, ExecRequest{Cmd: "yes"})
	if !errors.Is(err, jsonhttp.ErrTooLarge) {
		t.Fatalf("an endless answer failed with %v, want ErrTooLarge", err)
	}
	if written := <-stopped; written >= ceiling {
		t.Fatalf("the agent wrote all %d bytes: the host went on reading past its bound", written)
	}
}

// TestExecRefusesAMalformedAnswer: what is not an ExecResult is an error, not a
// zero result that reads as a command that exited 0 and printed nothing.
func TestExecRefusesAMalformedAnswer(t *testing.T) {
	for name, body := range map[string]string{
		"truncated":  `{"exit": 0, "stdout": "hel`,
		"wrong type": `{"exit": "zero"}`,
		"not json":   "\x00\xff<html>",
	} {
		t.Run(name, func(t *testing.T) {
			socket := fakeVMM(t, acceptAll, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(body))
			}))
			_, err := Exec(t.Context(), socket, ExecRequest{Cmd: "true"})
			if err == nil || !strings.Contains(err.Error(), "decoding the response failed") {
				t.Fatalf("a malformed answer reported %v, want a decoding failure", err)
			}
		})
	}
}

// TestExecQuotesOnlyTheStartOfAFailure: a refusal that is not the shared error
// shape is quoted in the error, and an agent that sends a megabyte of it does
// not put a megabyte into every log line that reports it.
func TestExecQuotesOnlyTheStartOfAFailure(t *testing.T) {
	socket := fakeVMM(t, acceptAll, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 1<<20)))
	}))
	_, err := Exec(t.Context(), socket, ExecRequest{Cmd: "true"})
	if err == nil {
		t.Fatal("a refusal was reported as a result")
	}
	want := fmt.Sprintf("POST http://guest/exec: 500 Internal Server Error: %s... (%d more bytes)",
		strings.Repeat("x", 512), 1<<20-512)
	if err.Error() != want {
		t.Fatalf("the failure reads %.120q... (%d bytes), want %d bytes quoting the first 512",
			err.Error(), len(err.Error()), len(want))
	}
}

// TestExecRefusesHeadersPastTheirBound: the body is not the only thing an
// agent can make long.
func TestExecRefusesHeadersPastTheirBound(t *testing.T) {
	socket := fakeVMM(t, acceptAll, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Filler", strings.Repeat("h", 1<<20))
		jsonhttp.Write(w, http.StatusOK, ExecResult{})
	}))
	_, err := Exec(t.Context(), socket, ExecRequest{Cmd: "true"})
	if err == nil || !strings.Contains(err.Error(),
		fmt.Sprintf("server response headers exceeded %d bytes", maxHeaderBytes)) {
		t.Fatalf("a megabyte of headers reported %v, want the header bound", err)
	}
}

// TestExecGivesUpOnAnAgentThatNeverAnswers: the wait is the command's bound and
// the grace, and not the minutes a host used to wait for any command.
func TestExecGivesUpOnAnAgentThatNeverAnswers(t *testing.T) {
	shortGrace(t)
	socket := fakeVMM(t, acceptAll, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	request := ExecRequest{Cmd: "true", Timeout: 0.1}
	began := time.Now()
	_, err := Exec(t.Context(), socket, request)
	took := time.Since(began)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("a silent agent reported %v, want a timeout", err)
	}
	if want := request.Bound() + answerGrace; took < want || took > want+5*time.Second {
		t.Fatalf("the exec gave up after %s, want %s", took, want)
	}
}

// TestExecGivesUpOnAnAnswerThatTrickles: an agent that answers at once and then
// sends its body a byte at a time is held to the same wait.
func TestExecGivesUpOnAnAnswerThatTrickles(t *testing.T) {
	shortGrace(t)
	socket := fakeVMM(t, acceptAll, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	request := ExecRequest{Cmd: "true", Timeout: 0.1}
	began := time.Now()
	_, err := Exec(t.Context(), socket, request)
	took := time.Since(began)
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("a trickling agent reported %v, want a timeout", err)
	}
	if want := request.Bound() + answerGrace; took < want || took > want+5*time.Second {
		t.Fatalf("the exec gave up after %s, want %s", took, want)
	}
}

// shortGrace makes the wait past a command's bound short enough to test.
func shortGrace(t *testing.T) {
	t.Helper()
	saved := answerGrace
	answerGrace = 200 * time.Millisecond
	t.Cleanup(func() { answerGrace = saved })
}

func acceptAll(string) bool { return true }
