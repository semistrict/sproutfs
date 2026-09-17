package vmmigrate_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/semistrict/sproutfs/internal/vmmigrate"
)

// TestReleaseRefusesWhileUnpublishedPagesAreOutstanding. The pages a source
// holds that no checkpoint of the VM has exist nowhere else, so releasing them
// before the destination has them loses the guest's writes since this host's
// last checkpoint. Whether they have arrived is not something a control plane's
// table knows: the source answered the fetches, so the source is the only thing
// that does, and a release it cannot account for is refused rather than taken
// on the word of a table.
func TestReleaseRefusesWhileUnpublishedPagesAreOutstanding(t *testing.T) {
	s := newServed(t, nil, 3)
	source := s.migration.pages
	if err := source.Release("vm-2"); !errors.Is(err, vmmigrate.ErrOutstanding) {
		t.Fatalf("releasing a VM whose pages no destination has fetched = %v", err)
	}
	if serving := source.Serving(); !slices.Equal(serving, []string{"vm-2"}) {
		t.Fatalf("a refused release stopped serving: %v", serving)
	}
	// The destination fetches them, which is what its report of being done
	// means; the source has seen every one of them leave.
	backing := s.backing(t, nil, "ram0")
	data := make([]byte, 3*pageSize)
	if err := backing.Load(t.Context(), 0, data); err != nil {
		t.Fatal(err)
	}
	if err := source.Release("vm-2"); err != nil {
		t.Fatalf("releasing a VM whose pages the destination has = %v", err)
	}
	if serving := source.Serving(); len(serving) != 0 {
		t.Fatalf("a completed release kept serving: %v", serving)
	}
}
