//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/internal/platform"
	"github.com/semistrict/sproutfs/internal/vmmachine"
)

// logged is one record this process wrote: the account a test reads, since a
// VMM that died leaves nothing else behind.
type logged struct {
	message string
	level   slog.Level
	attrs   map[string]string
}

// records collects what a test's process logs.
type records struct {
	mu   sync.Mutex
	logs []logged
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *records) WithGroup(string) slog.Handler            { return r }

func (r *records) Handle(_ context.Context, record slog.Record) error {
	entry := logged{message: record.Message, level: record.Level, attrs: map[string]string{}}
	record.Attrs(func(a slog.Attr) bool {
		entry.attrs[a.Key] = a.Value.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, entry)
	return nil
}

// await returns the attributes of the record whose message contains want, and
// fails once nothing has written one. The record comes from a goroutine of the
// process, so it lands some time after the caller saw the process end.
func (r *records) await(t *testing.T, want string, level slog.Level) map[string]string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r.mu.Lock()
		var messages []string
		for _, entry := range r.logs {
			if strings.Contains(entry.message, want) {
				r.mu.Unlock()
				if entry.level != level {
					t.Fatalf("%q was logged at %s, want %s", entry.message, entry.level, level)
				}
				return entry.attrs
			}
			messages = append(messages, entry.message)
		}
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("nothing logged %q; this process said only %v", want, messages)
		}
		time.Sleep(time.Millisecond)
	}
}

func capturing(t *testing.T) *records {
	t.Helper()
	r := &records{}
	previous := slog.Default()
	slog.SetDefault(slog.New(r))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return r
}

// A VMM the host kills leaves nothing behind: its sockets stop answering, the
// supervisor goes on reporting a running guest, and the only line anything
// writes is the refused connection of an interval checkpoint up to a minute
// later. The process is what knows why it died, so it says so — the VM, the
// process, the cause and the console tail the guest left — and the session that
// ended says which memory region and which error ended it.
func TestAKilledVMMSaysWhyItDied(t *testing.T) {
	config, stall, _ := startupFixture(t)
	// The authority check every session runs is what fails here, which is a
	// session failure like any other: this VM's memory can no longer be served,
	// so the process supervising it is killed.
	config.Connection.VerifyInterval = 10 * time.Millisecond
	logs := capturing(t)
	p, err := vmmachine.Start(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	pid := p.PID()
	stall.blocked.Store(true)
	close(stall.release)

	if err := p.Wait(t.Context()); !errors.Is(err, platform.ErrUnavailable) {
		t.Fatalf("Wait reported %v, want the failure that killed the VMM", err)
	}
	exit := logs.await(t, "vmmachine", slog.LevelError)
	if exit["vm"] != config.VM.ID() {
		t.Errorf("the exit named vm %q, want %q", exit["vm"], config.VM.ID())
	}
	if exit["pid"] != strconv.Itoa(pid) {
		t.Errorf("the exit named pid %q, want %d", exit["pid"], pid)
	}
	if !strings.Contains(exit["error"], platform.ErrUnavailable.Error()) {
		t.Errorf("the exit gave the cause as %q, want the failure that killed it", exit["error"])
	}
	if !strings.Contains(exit["console"], consoleBanner) {
		t.Errorf("the exit carried the console tail %q, want the guest's output", exit["console"])
	}
	session := logs.await(t, "vmmemory", slog.LevelError)
	if session["memory_region"] != vmmachine.RAMVolume {
		t.Errorf("the session failure named memory region %q, want %q", session["memory_region"], vmmachine.RAMVolume)
	}
	if !strings.Contains(session["error"], platform.ErrUnavailable.Error()) {
		t.Errorf("the session failure gave the error as %q, want what ended it", session["error"])
	}
}
