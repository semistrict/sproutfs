//go:build linux && (amd64 || arm64)

package vmmachine

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStateWriterKernelLimit(t *testing.T) {
	if path := os.Getenv("SPROUTFS_STATE_LIMIT_CHILD"); path != "" {
		if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteAt([]byte{1}, MaxStateBytes-1); err != nil {
			t.Fatalf("within bound: %v", err)
		}
		if _, err := file.WriteAt([]byte{1}, MaxStateBytes); !errors.Is(err, syscall.EFBIG) {
			t.Fatalf("over bound: %v", err)
		}
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() != MaxStateBytes {
			t.Fatalf("state escaped reservation: %d", info.Size())
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestStateWriterKernelLimit$")
	cmd.Env = append(os.Environ(), "SPROUTFS_STATE_LIMIT_CHILD="+filepath.Join(t.TempDir(), "capture.state"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	gate, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := limitStateFiles(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}
