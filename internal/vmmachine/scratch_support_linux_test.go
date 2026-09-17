//go:build linux && (amd64 || arm64)

package vmmachine_test

import (
	"testing"

	"github.com/semistrict/sproutfs/internal/platform/adapters"
	"github.com/semistrict/sproutfs/internal/vmmachine"
)

func mustScratch(t testing.TB) *vmmachine.Scratch {
	t.Helper()
	scratch, err := vmmachine.NewScratch(t.Context(), t.TempDir(), adapters.NewDisk)
	if scratch != nil {
		t.Cleanup(func() {
			if err := scratch.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	return scratch
}
