package main

import (
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/internal/api/host"
	"github.com/semistrict/sproutfs/internal/api/orch"
)

// demoTemplates is what the deployment configures: two guest images, of
// different sizes. It matters that there are two. A host with exactly one
// template resolves an unnamed template to it, so a VM whose template nothing
// remembers is still measured correctly there; with two, an unnamed template
// measures as nothing at all and every host admits it.
func demoTemplates() []host.Template {
	return []host.Template{
		{Name: "alpine", MemoryBytes: 512 << 20, Imported: true},
		{Name: "workload", MemoryBytes: 2 << 30, Imported: true},
	}
}

// TestAForksChildrenCarryTheTemplateTheyWereForkedFrom: a child is a guest of
// the same template as its parent — that is what a fork is — and only the host
// that created a VM reports its template, and only until the VM moves. The
// table is therefore the one place a forked child's template can live, and a
// child the table has never heard of is admitted against nothing: a fan-out of
// one, a migration of one, a start of one all fit on any host, however full.
//
// A soak forks the VMs it forked, so by its second round most of the deployment
// is children, and every placement of one is unbounded.
func TestAForksChildrenCarryTheTemplateTheyWereForkedFrom(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	for _, h := range d.hosts {
		// A 2 GiB arena with a 512 MiB guest already on it.
		h.arena(1024, 20)
		h.commit(512 << 20)
		h.templates = demoTemplates()
	}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", Host: "host-0", State: stateRunning,
		Template: "alpine"})

	forked, err := d.orchestrator.Fork(t.Context(), "vm-a", 2, "host-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range forked.Children {
		row, found, err := d.orchestrator.table.VM(t.Context(), child)
		if err != nil || !found {
			t.Fatalf("the table has no row for the child %s: %v %v", child, found, err)
		}
		if row.Template != "alpine" {
			t.Fatalf("the child %s is recorded with template %q, want its parent's", child, row.Template)
		}
	}

	// And what that template says is what the next placement of a child is
	// measured against. host-1 now runs the two children and has a quarter of
	// its arena left, which is one more 512 MiB guest and not three.
	child := forked.Children[0]
	d.hosts["host-1"].commit(1536 << 20)
	if _, err := d.orchestrator.Fork(t.Context(), child, 3, "host-1"); !errors.Is(err, errNoHost) {
		t.Fatalf("a fan-out of a child larger than its host = %v, want errNoHost", err)
	}
}

// TestStartingAForkedChildIsAdmittedAgainstItsTemplate, for the same reason: a
// stopped child has no host reporting anything about it at all, so the table is
// the whole of what says how much memory it needs where it is started.
func TestStartingAForkedChildIsAdmittedAgainstItsTemplate(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	for _, h := range d.hosts {
		h.arena(1024, 20)
		h.commit(512 << 20)
		h.templates = demoTemplates()
	}
	d.orchestrator.note(t.Context(), vmRecord{ID: "vm-a", Host: "host-0", State: stateRunning,
		Template: "alpine"})
	forked, err := d.orchestrator.Fork(t.Context(), "vm-a", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	child := forked.Children[0]
	if _, err := d.orchestrator.Stop(t.Context(), child, orch.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	// Both hosts are now promised their whole arena, so the child fits nowhere
	// and is refused rather than opened on a host that cannot hold it.
	for _, h := range d.hosts {
		h.commit(2 << 30)
	}
	if _, err := d.orchestrator.Start(t.Context(), child, orch.StartRequest{To: "host-1"}); !errors.Is(err, errNoHost) {
		t.Fatalf("starting a forked child on a full host = %v, want errNoHost", err)
	}
}
