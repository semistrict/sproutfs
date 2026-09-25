//go:build linux && (amd64 || arm64)

package vmmachine

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/sproutfs/internal/ctxsync"
	"github.com/semistrict/sproutfs/platform/adapters"
)

func seedScratch(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "dead", "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"dead/state/capture.state", "dead/state/config.json"} {
		if err := os.WriteFile(filepath.Join(dir, path), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A process restart is a host loss: whatever the previous process left behind
// is not authority for anything, so opening the scratch deletes all of it,
// including the sockets a stopped VMM leaves.
func TestScratchWipesWhatAPreviousProcessLeftBehind(t *testing.T) {
	root := t.TempDir()
	s, err := NewScratch(t.Context(), root, adapters.NewDisk)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	seedScratch(t, s.Directory())
	socket, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(s.Directory(), "dead", "api"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	socket.SetUnlinkOnClose(false)
	if err := socket.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewScratch(t.Context(), root, adapters.NewDisk)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(reopened.Directory())
	if err != nil || len(entries) != 0 {
		t.Fatalf("stale scratch retained: %v %v", entries, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestScratchRetriesFailedStartCleanup(t *testing.T) {
	s, err := NewScratch(t.Context(), t.TempDir(), adapters.NewDisk)
	if err != nil {
		t.Fatal(err)
	}
	p := &Process{mu: ctxsync.NewMutex(), cancel: func(error) {}}
	p.dir, err = s.create(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := adapters.NewDisk(filepath.Join(p.dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	failures := &failedStateRemoval{Disk: raw}
	p.files, err = newStateFiles(failures)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		failures.fail = false
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := p.files.write(t.Context(), "config.json", make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	failures.fail = true
	if err := p.Close(); !errors.Is(err, errStateRemoval) {
		t.Fatalf("failed-start cleanup: %v", err)
	}
	if err := s.Close(); !errors.Is(err, errStateRemoval) {
		t.Fatalf("owner lost failed cleanup: %v", err)
	}
	failures.fail = false
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(s.Directory()); err != nil || len(entries) != 0 {
		t.Fatalf("cleanup retry retained files: %v %v", entries, err)
	}
}
