package vmmachine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/semistrict/sproutfs/vmmachine"
)

// revisionStarter is a Starter that reports a revision and starts nothing.
type revisionStarter struct {
	revision int
	err      error
}

func (s revisionStarter) Start(context.Context, *vmmachine.Launch) (vmmachine.VMM, error) {
	return nil, errors.New("a revisionStarter starts nothing")
}

func (s revisionStarter) Boots() bool { return true }

func (s revisionStarter) APIRevision(context.Context) (int, error) { return s.revision, s.err }

// A VMM that lacks a field the host sends boots guests and fails first at a
// checkpoint, so a host is refused before it runs any: an older revision, a
// newer one, and a VMM that cannot say which it speaks.
func TestCheckAPIRefusesAnotherRevision(t *testing.T) {
	if err := vmmachine.CheckAPI(t.Context(), revisionStarter{revision: vmmachine.APIRevision}); err != nil {
		t.Fatalf("the host's own revision was refused: %v", err)
	}
	for _, starter := range []revisionStarter{
		{revision: vmmachine.APIRevision - 1},
		{revision: vmmachine.APIRevision + 1},
		{err: errors.New("unknown argument")},
	} {
		if err := vmmachine.CheckAPI(t.Context(), starter); !errors.Is(err, vmmachine.ErrAPIRevision) {
			t.Fatalf("%+v: got %v, want ErrAPIRevision", starter, err)
		}
	}
}

// fakeBinary writes a script that stands in for the VMM's binary.
func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "firecracker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The fork prints its revision for the flag. Upstream Firecracker refuses the
// flag as an unknown argument, and that is an error, not revision zero.
func TestFirecrackerAPIRevisionAsksTheBinary(t *testing.T) {
	speaking := &vmmachine.Firecracker{Binary: fakeBinary(t,
		`[ "$1" = --sproutfs-api-revision ] || exit 2; echo 7`)}
	revision, err := speaking.APIRevision(t.Context())
	if err != nil || revision != 7 {
		t.Fatalf("got revision %d, %v; want 7", revision, err)
	}

	upstream := &vmmachine.Firecracker{Binary: fakeBinary(t,
		`echo "Found argument '$1' which wasn't expected" >&2; exit 1`)}
	if _, err := upstream.APIRevision(t.Context()); err == nil {
		t.Fatal("an upstream binary reported a revision")
	}

	garbled := &vmmachine.Firecracker{Binary: fakeBinary(t, `echo Firecracker v1.18.0`)}
	if _, err := garbled.APIRevision(t.Context()); err == nil {
		t.Fatal("a binary that printed no number reported a revision")
	}
}
