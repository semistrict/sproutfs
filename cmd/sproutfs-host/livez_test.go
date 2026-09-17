package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestLivenessFailsOnceTheProcessCannotServe: liveness answered 200 out of the
// mux itself, so it could not fail. A host whose supervisor has closed — its
// pager released, its VMM processes gone — or whose own context has been
// cancelled goes on answering the probe for as long as the HTTP listener is up,
// and the kubelet leaves a pod that can do nothing running for ever. Liveness
// asks the supervisor.
func TestLivenessFailsOnceTheProcessCannotServe(t *testing.T) {
	fake := &fakeHost{}
	if status, body := call(t, fake, http.MethodGet, "/livez", ""); status != http.StatusOK {
		t.Fatalf("a live host answered liveness %d, want 200: %s", status, body)
	}
	fake.notLive = errors.New("host: host closed")
	status, body := call(t, fake, http.MethodGet, "/livez", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("a closed host answered liveness %d, want 503: %s", status, body)
	}
	if !strings.Contains(body, "host closed") {
		t.Fatalf("the refusal reads %s, want it to say why", body)
	}
	// Liveness stays open to the kubelet, which reaches a pod before anything
	// has given it the deployment's token.
	if strings.Contains(body, "bearer token") {
		t.Fatalf("liveness asked for the token: %s", body)
	}
}
