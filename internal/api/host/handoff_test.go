package host_test

import (
	"encoding/json"
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/internal/api/host"
)

// A handoff crosses the control plane as JSON and is carried unread from the
// source to the destination, so every field it needs has to survive that trip.
// The age of the source's oldest unpublished write is one of them: without it
// the destination dates the pages it receives from its own arrival, and a VM
// handed from host to host outruns its loss window for ever.
func TestAHandoffCarriesTheUnpublishedAgeAcrossTheWire(t *testing.T) {
	handoff := hostapi.Handoff{
		VMID: "vm-1", State: []byte("vmm-state"), Checkpoint: 7,
		Source: "10.0.0.1:9000", PageSize: 2 << 20,
		Regions: []hostapi.HandoffRegion{{
			Name: "ram0", Size: 8 << 20,
			Unpublished:    []hostapi.HandoffPageRun{{First: 2, Count: 3}},
			UnpublishedAge: 90 * time.Second,
		}},
		PausedAt: time.Unix(1700000000, 0).UTC(),
	}
	encoded, err := json.Marshal(handoff)
	if err != nil {
		t.Fatal(err)
	}
	var read hostapi.Handoff
	if err := json.Unmarshal(encoded, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Regions) != 1 {
		t.Fatalf("the handoff read back %d regions, want one", len(read.Regions))
	}
	if got := read.Regions[0].UnpublishedAge; got != 90*time.Second {
		t.Fatalf("the region read back an unpublished age of %s, want 90s", got)
	}
}

// A region holding nothing unpublished carries no age at all, so a destination
// reading an older source's handoff dates nothing and starts the window at its
// own first store.
func TestAHandoffOmitsAnUnpublishedAgeOfZero(t *testing.T) {
	encoded, err := json.Marshal(hostapi.HandoffRegion{Name: "ram0", Size: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["UnpublishedAge"]; present {
		t.Fatalf("a region with nothing unpublished encoded an age: %s", encoded)
	}
}
