package main

import (
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// TestTheGracePeriodIsTheDrainPlusTheShutdown: the manifest's termination
// grace period is exactly what the host spends after it is told to stop — the
// preStop drain's own bound, then the API's shutdown and the supervisor's close
// — so a pod whose drain runs to its bound still publishes a final checkpoint
// of every VM that did not move before the kubelet kills it. A grace period
// shorter than that kills the pod with pages no checkpoint has; a longer one is
// time a rollout waits for nothing.
func TestTheGracePeriodIsTheDrainPlusTheShutdown(t *testing.T) {
	manifest, err := os.ReadFile("../../deploy/10-host.yaml")
	if err != nil {
		t.Fatal(err)
	}
	found := regexp.MustCompile(`(?m)^\s*terminationGracePeriodSeconds:\s*(\d+)\s*$`).FindSubmatch(manifest)
	if found == nil {
		t.Fatal("the host manifest sets no terminationGracePeriodSeconds")
	}
	seconds, err := strconv.Atoi(string(found[1]))
	if err != nil {
		t.Fatal(err)
	}
	grace := time.Duration(seconds) * time.Second
	if want := drainTimeout + 2*shutdownTimeout; grace != want {
		t.Fatalf("the manifest gives the host %s to stop, want the drain's %s and the shutdown's %s: %s",
			grace, drainTimeout, 2*shutdownTimeout, want)
	}
}
