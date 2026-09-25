package vmmachine

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
)

func stateFixture(t *testing.T) (*stateFiles, platform.Disk) {
	t.Helper()
	runtime := sim.New(sim.Config{})
	raw := runtime.NewDisk("state", sim.DiskConfig{})
	files, err := newStateFiles(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	return files, raw
}

func produceState(ctx context.Context, raw platform.Disk, data []byte) error {
	// The external writer is the VMM, whose handle closes before the API
	// completion the supervisor waits on returns.
	file, err := raw.Open(ctx, "capture.state", platform.OpenOptions{Truncate: true})
	if err != nil {
		return err
	}
	_, writeErr := file.WriteAt(ctx, data, 0)
	return errors.Join(writeErr, file.Sync(ctx), file.Close())
}

func TestStateFilesWriteAndRemoveTheirStaging(t *testing.T) {
	files, raw := stateFixture(t)
	if err := files.write(t.Context(), "restore.state", bytes.Repeat([]byte("s"), 4096)); err != nil {
		t.Fatal(err)
	}
	paths, err := raw.List(t.Context(), "")
	if err != nil || len(paths) != 1 || paths[0] != "restore.state" {
		t.Fatalf("staged files = %v, %v", paths, err)
	}
	if err := files.remove(t.Context(), "restore.state"); err != nil {
		t.Fatal(err)
	}
	if err := files.remove(t.Context(), "restore.state"); err != nil {
		t.Fatalf("removing an absent file: %v", err)
	}
	paths, err = raw.List(t.Context(), "")
	if err != nil || len(paths) != 0 {
		t.Fatalf("consumed restore file retained: %v %v", paths, err)
	}
}

func TestCaptureReadsBackAndDeletesWhatTheVMMWrote(t *testing.T) {
	files, raw := stateFixture(t)
	state, err := files.capture(t.Context(), func() error {
		return produceState(t.Context(), raw, []byte("captured state"))
	})
	if err != nil || string(state) != "captured state" {
		t.Fatalf("capture = %q, %v", state, err)
	}
	paths, err := raw.List(t.Context(), "")
	if err != nil || len(paths) != 0 {
		t.Fatalf("capture retained files: %v %v", paths, err)
	}
}

// The VMM runs under a file-size limit, so an oversized state file means the
// limit did not hold. Reading it back would be unbounded, so it is refused.
func TestCaptureRefusesStateBeyondItsBound(t *testing.T) {
	files, raw := stateFixture(t)
	_, err := files.capture(t.Context(), func() error {
		file, err := raw.Open(t.Context(), "capture.state", platform.OpenOptions{Truncate: true})
		if err != nil {
			return err
		}
		return errors.Join(file.Truncate(t.Context(), MaxStateBytes+1), file.Sync(t.Context()), file.Close())
	})
	if !errors.Is(err, ErrStateTooLarge) {
		t.Fatalf("oversized state: %v", err)
	}
	paths, err := raw.List(t.Context(), "")
	if err != nil || len(paths) != 0 {
		t.Fatalf("refused capture retained files: %v %v", paths, err)
	}
}

type failedStateRemoval struct {
	platform.Disk
	fail bool
}

var errStateRemoval = errors.New("state unlink failed")

func (d *failedStateRemoval) Remove(ctx context.Context, name string) error {
	if d.fail {
		return errStateRemoval
	}
	return d.Disk.Remove(ctx, name)
}

func TestStateCleanupFailureIsReportedAndRetryable(t *testing.T) {
	runtime := sim.New(sim.Config{})
	raw := &failedStateRemoval{Disk: runtime.NewDisk("state", sim.DiskConfig{})}
	files, err := newStateFiles(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.write(t.Context(), "config.json", bytes.Repeat([]byte("x"), 4096)); err != nil {
		t.Fatal(err)
	}
	raw.fail = true
	if err := files.Close(); !errors.Is(err, errStateRemoval) {
		t.Fatalf("cleanup failure hidden: %v", err)
	}
	raw.fail = false
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	paths, err := raw.List(t.Context(), "")
	if err != nil || len(paths) != 0 {
		t.Fatalf("cleanup retry retained files: %v %v", paths, err)
	}
}

// A VMM writes its state file with its own format and its own periodic flushes,
// so nothing sproutfs writes can carry a checksum of it: a state file that
// comes back short or holding bytes the VMM never wrote is indistinguishable
// from a smaller machine. What refuses it instead is the handle the capture is
// holding, which the power loss invalidates — the process that would have
// published those bytes did not survive to publish them.
//
// The assertion is that the capture fails. The bytes the device kept are read
// back afterwards only to show that they are not the ones the VMM wrote, which
// is what a capture that ignored the stale handle would have published as this
// machine's state.
func TestCaptureRefusesAPowerLossDuringTheVMMsWrite(t *testing.T) {
	for seed := uint64(1); seed <= 16; seed++ {
		runtime := sim.New(sim.Config{Seed: seed})
		raw := runtime.NewDisk("state", sim.DiskConfig{PowerLossFaults: true})
		files, err := newStateFiles(raw)
		if err != nil {
			t.Fatal(err)
		}
		header := bytes.Repeat([]byte("H"), 8192)
		body := make([]byte, 32768)
		for i := range body {
			body[i] = byte(1 + i%251)
		}
		got := make([]byte, len(header)+len(body))
		_, err = files.capture(t.Context(), func() error {
			// The VMM flushes its header and is still writing the body when the
			// machine loses power.
			file, err := raw.Open(t.Context(), "capture.state", platform.OpenOptions{Truncate: true})
			if err != nil {
				return err
			}
			if _, err := file.WriteAt(t.Context(), header, 0); err != nil {
				return err
			}
			if err := file.Sync(t.Context()); err != nil {
				return err
			}
			if _, err := file.WriteAt(t.Context(), body, int64(len(header))); err != nil {
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			if err := raw.PowerLoss(t.Context()); err != nil {
				return err
			}
			// The capture deletes its staging on the way out, so what the
			// device kept is read here, while the file is still there.
			survived, err := raw.Open(t.Context(), "capture.state", platform.OpenOptions{})
			if err != nil {
				return err
			}
			if _, err := survived.ReadAt(t.Context(), got, 0); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			return survived.Close()
		})
		if !errors.Is(err, platform.ErrStaleHandle) {
			t.Fatalf("seed %d: capture across a power loss = %v, want ErrStaleHandle", seed, err)
		}
		if bytes.Equal(got, append(append([]byte(nil), header...), body...)) {
			t.Fatalf("seed %d: the power loss kept the whole state file; nothing was at risk", seed)
		}
		if !bytes.Equal(got[:len(header)], header) {
			t.Fatalf("seed %d: the power loss took back a flushed header", seed)
		}
		if err := files.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
