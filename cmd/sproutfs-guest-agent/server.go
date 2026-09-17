package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os/exec"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/internal/api/guest"
)

// defaultTimeout bounds a command that did not ask for a bound of its own, and
// maxTimeout bounds every command whatever it asked for: the host is waiting on
// the other end of one vsock stream, and a guest that never answers is worse
// than a command that was killed.
const (
	defaultTimeout = 30 * time.Second
	maxTimeout     = 10 * time.Minute
)

// timeoutExit is the status of a command the deadline killed, which is what a
// shell's timeout(1) reports and what this agent reports for the same reason.
const timeoutExit = 124

// killGrace is how long the agent waits, once a command's shell is gone, for
// the pipes it was reading to close. Nothing normally waits at all — a shell
// that exited holds nothing open — so this bounds exactly the case it is for: a
// child the command left behind. An agent that cannot answer is worse than an
// answer missing the last of what such a child printed.
const killGrace = 200 * time.Millisecond

// killGroup ends every process of one command's group. The command is its own
// group leader, so the negative pid names it and everything it started.
func killGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}

// shell runs a command the way the request wrote it, so that pipes, redirection
// and quoting mean what the caller meant.
var shell = []string{"/bin/sh", "-c"}

// defaultExecs is how many commands the agent runs at once when nothing names a
// bound. A demo runs one at a time and a fan-out runs a few; anything past this
// is a caller retrying into a guest that is already busy.
const defaultExecs = 4

// newServer routes the agent. Two endpoints: whether the guest is up, and
// running something in it.
//
// execs is how many commands may run at once. A guest's memory and its process
// table are the point of the deployment, and one host reaching one guest over
// one vsock can put more into it than either carries: what is over the bound is
// refused, which a caller can act on, rather than run and killed for. Zero
// selects defaultExecs.
func newServer(execs int) *http.ServeMux {
	if execs <= 0 {
		execs = defaultExecs
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		write(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	admitted := make(chan struct{}, execs)
	mux.HandleFunc("POST /exec", func(w http.ResponseWriter, r *http.Request) {
		select {
		case admitted <- struct{}{}:
			defer func() { <-admitted }()
		default:
			write(w, http.StatusTooManyRequests,
				map[string]string{"error": "the guest is already running as many commands as it admits"})
			return
		}
		var request guest.ExecRequest
		raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			write(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			write(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if request.Cmd == "" {
			write(w, http.StatusBadRequest, map[string]string{"error": "exec needs a command"})
			return
		}
		write(w, http.StatusOK, runCommand(r.Context(), request))
	})
	return mux
}

// runCommand executes one command and reports what it did. A command that
// fails is not an error: its exit status is the answer, and only a guest that
// could not run a shell at all has nothing to report.
func runCommand(ctx context.Context, request guest.ExecRequest) guest.ExecResult {
	timeout := defaultTimeout
	if request.Timeout > 0 {
		timeout = min(time.Duration(request.Timeout*float64(time.Second)), maxTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	began := time.Now()
	var stdout, stderr boundedOutput
	command := exec.CommandContext(ctx, shell[0], append(shell[1:], request.Cmd)...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	// The command is its own process group, so what it started is killed with
	// it rather than the shell alone. A shell that backgrounds something exits
	// at once and leaves that child holding these pipes: killing only the shell
	// leaves the agent waiting on them for as long as the child runs, and the
	// child running in the guest long after the exec that started it returned.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The deadline kills the group, for a command still running when it passes.
	command.Cancel = func() error { return killGroup(command) }
	// And the wait for the pipes, once the shell itself is gone, is bounded: a
	// child still holding them would otherwise hold the answer with them.
	command.WaitDelay = killGrace
	err := command.Run()
	// A shell that exited before the deadline was ever consulted took no kill
	// with it, so the group is ended here too. It is ended after the wait
	// rather than before because the shell is the group's leader: while the
	// agent has not reaped it, nothing else can be running under its identity.
	if killed := killGroup(command); killed != nil && !errors.Is(killed, syscall.ESRCH) {
		log.Printf("killing what a command left behind failed: %v", killed)
	}
	result := guest.ExecResult{Seconds: time.Since(began).Seconds()}
	result.Stdout, result.Stderr = string(stdout.Bytes()), string(stderr.Bytes())
	result.Truncated = stdout.Truncated() || stderr.Truncated()

	switch {
	case err == nil:
		result.Exit = command.ProcessState.ExitCode()
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.Exit = timeoutExit
		result.Stderr += "sproutfs-guest-agent: the command ran past its timeout\n"
	case command.ProcessState != nil:
		result.Exit = command.ProcessState.ExitCode()
	default:
		// The shell itself could not be started, which is the guest's fault
		// rather than the command's.
		result.Exit = 127
		result.Stderr += "sproutfs-guest-agent: " + err.Error() + "\n"
	}
	return result
}

// boundedOutput keeps the first MaxOutputBytes of one stream and drops the
// rest, counting what it dropped. It accepts every write: a sink that stopped
// accepting them would block the command on a full pipe, so a command printing
// a filesystem would hang instead of finishing with a truncated answer.
//
// The bound matters because this runs in the guest, whose memory is the whole
// point of the deployment: a command that prints a gigabyte must not cost a
// gigabyte of the guest's RAM on its way to an answer that is a megabyte.
type boundedOutput struct {
	kept    []byte
	dropped int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	kept := min(guest.MaxOutputBytes-len(b.kept), len(p))
	if kept > 0 {
		b.kept = append(b.kept, p[:kept]...)
	}
	b.dropped += len(p) - kept
	return len(p), nil
}

// Bytes is what this stream returns, and Truncated whether anything was
// dropped to stay inside the bound.
func (b *boundedOutput) Bytes() []byte { return b.kept }

func (b *boundedOutput) Truncated() bool { return b.dropped > 0 }

// write sends one JSON body. A failure to write it has nowhere to go: the host
// on the other end of the stream sees the connection end.
func write(w http.ResponseWriter, status int, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		http.Error(w, `{"error":"encoding the response failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}
