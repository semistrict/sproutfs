package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/control"
)

// A capture and a stop carry the keep to the host.
func TestACaptureAndAStopCarryTheKeep(t *testing.T) {
	fake := &fakeHost{}
	for _, request := range []struct{ path, body string }{
		{"/vms/vm-1/capture", `{"keep":true}`},
		{"/vms/vm-1/stop", `{"keep":true}`},
		{"/vms/vm-2/stop", `{"suspend":true,"keep":true}`},
	} {
		if status, body := call(t, fake, http.MethodPost, request.path, request.body); status != http.StatusOK {
			t.Fatalf("%s %s: status %d: %s", request.path, request.body, status, body)
		}
	}
	if want := []string{"capture vm-1 kept", "stop vm-1 kept", "suspend vm-2 kept"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("the host was asked for %v, want %v", fake.calls, want)
	}
}

// A VM's kept checkpoints are listed by a GET and released by a POST naming
// the checkpoint. A release of a checkpoint a VM was created from, or of one
// nothing keeps, is a conflict: the request is well formed and the record says
// no.
func TestKeptCheckpointsAreListedAndReleased(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	fake := &fakeHost{kept: hostapi.KeptResult{VM: "vm-1",
		Kept: []hostapi.Kept{{Checkpoint: 7, Time: at, State: true, Forked: true}}}}
	status, body := call(t, fake, http.MethodGet, "/vms/vm-1/kept", "")
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var listed hostapi.KeptResult
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.VM != "vm-1" || len(listed.Kept) != 1 || listed.Kept[0].Checkpoint != 7 ||
		!listed.Kept[0].Time.Equal(at) || !listed.Kept[0].State || !listed.Kept[0].Forked {
		t.Fatalf("the listing is %+v, want vm-1 keeping checkpoint 7 with state, forked", listed)
	}
	if status, body := call(t, fake, http.MethodPost, "/vms/vm-1/kept/7/release", ""); status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if status, body := call(t, fake, http.MethodPost, "/vms/vm-1/kept/latest/release", ""); status != http.StatusBadRequest {
		t.Fatalf("releasing a checkpoint that is not a sequence: status %d: %s", status, body)
	}
	if want := []string{"kept vm-1", "release vm-1@7"}; !slices.Equal(fake.calls, want) {
		t.Fatalf("the host was asked for %v, want %v", fake.calls, want)
	}
	for _, refusal := range []error{control.ErrForked, control.ErrNotKept} {
		refused := &fakeHost{err: fmt.Errorf("%w: checkpoint 7 of vm-1", refusal)}
		if status, body := call(t, refused, http.MethodPost, "/vms/vm-1/kept/7/release", ""); status != http.StatusConflict {
			t.Fatalf("a release refused with %v: status %d: %s", refusal, status, body)
		}
	}
}
