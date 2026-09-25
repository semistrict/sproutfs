package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/api/orch"
)

// TestPlacementMeasuresCommittedGuestRAMRatherThanArenaResidency: the arena is
// a cache, not an allocation. Its occupancy only goes up — a page of a VM that
// has been migrated away or deleted stays resident until something else needs
// the page — so a host that has done work looks full whatever it is actually
// running, and a host that has done none looks empty however much guest RAM it
// has promised. Placement measured that way refuses every destination on a warm
// host, which is a drain with nowhere to go and a rollout that kills pods with
// pages on them. What a VM costs a host is the RAM its guest was promised, so
// that is what a placement counts.
func TestPlacementMeasuresCommittedGuestRAMRatherThanArenaResidency(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {"vm-a"}})
	// host-0's arena is warm with the pages of VMs that have gone and it runs
	// nothing; host-1 runs a guest that was promised most of its arena.
	d.hosts["host-0"].arena(1024, 1000)
	d.hosts["host-0"].commit(0)
	d.hosts["host-1"].arena(1024, 100)
	d.hosts["host-1"].commit(900 << 21)
	for _, h := range d.hosts {
		h.templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	}
	created, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "workload"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Host != "host-0" {
		t.Fatalf("the VM went to %s, want the host whose guests have promised the least", created.Host)
	}
	// And a host whose guests have promised the arena takes nothing more,
	// however little of it is resident.
	d.hosts["host-0"].commit(1024 << 21)
	d.hosts["host-1"].commit(1024 << 21)
	if _, err := d.orchestrator.Create(t.Context(), orch.CreateRequest{Template: "workload"}); !errors.Is(err, errNoHost) {
		t.Fatalf("a create that fits nowhere = %v, want errNoHost", err)
	}
}

// TestLocalForkIsAdmittedAgainstTheParentsHost: a fork onto the parent's own
// host still starts a guest per child, each with RAM of its own; what they
// share is the pages they have not diverged from, not the promise. A fan-out
// admitted against nothing is a host asked for more guest RAM than it has, and
// the parent is paused for it first.
func TestLocalForkIsAdmittedAgainstTheParentsHost(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	d.hosts["host-0"].arena(1024, 100)
	d.hosts["host-0"].commit(1 << 30)
	d.hosts["host-0"].templates = []host.Template{{Name: "workload", MemoryBytes: 512 << 20, Imported: true}}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", Host: "host-0", State: stateRunning,
		Template: "workload"})
	// The arena is 2 GiB and the guests on it are promised half of it, so three
	// more children of 512 MiB do not fit and two do.
	if _, err := d.orchestrator.Fork(t.Context(), "vm-a", 3, ""); !errors.Is(err, errNoHost) {
		t.Fatalf("a fan-out larger than its own host = %v, want errNoHost", err)
	}
	for _, line := range d.log {
		if line == "host-0 fork vm-a" {
			t.Fatalf("the refused fork paused the parent anyway: %v", d.log)
		}
	}
	// Two fit, and the parent is paused once for both.
	if _, err := d.orchestrator.Fork(t.Context(), "vm-a", 2, ""); err != nil {
		t.Fatalf("a fan-out its host has room for: %v", err)
	}
}

// TestAnImportGoesToOneReadyHost: a guest image is imported once, by one host,
// into the template its bytes name; which host does it says nothing about the
// template, and every host creates from it by its identity afterwards.
func TestAnImportGoesToOneReadyHost(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}, "host-1": {}})
	d.hosts["host-0"].arena(1024, 0)
	d.hosts["host-0"].commit(0)
	d.hosts["host-1"].arena(1024, 0)
	d.hosts["host-1"].commit(1 << 30)
	imported, err := d.orchestrator.ImportTemplate(t.Context(), strings.NewReader("ext4 bytes"),
		host.ImportTemplateRequest{Memory: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Template.ID != "template-ab" || imported.Checkpoint != 3 {
		t.Fatalf("the import reported %+v", imported)
	}
	want := []string{`host-0 import template "ext4 bytes" memory=1073741824`}
	if !slices.Equal(d.log, want) {
		t.Fatalf("the deployment did %v, want %v", d.log, want)
	}
}
