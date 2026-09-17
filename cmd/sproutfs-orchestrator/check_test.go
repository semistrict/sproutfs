package main

import (
	"context"
	"errors"
	"testing"

	"github.com/semistrict/sproutfs/internal/volume"
)

// TestCheckReportsADeploymentThatAgreesWithItself: the check reads the whole
// object namespace, so what it reports is data rather than a status line —
// nothing is wrong, and the caller is told so.
func TestCheckReportsADeploymentThatAgreesWithItself(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	d.orchestrator.audit = func(context.Context) error { return nil }
	result, err := d.orchestrator.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || len(result.Violations) != 0 {
		t.Fatalf("result %+v, want a deployment that agrees with itself", result)
	}
}

// TestCheckReportsEveryViolation: a deployment that disagrees with itself is
// not a failed request — the request worked, and the answer is what is wrong.
// Every violation is reported with the object it is about and the class of host
// loss that could excuse it, so an operator reads what to do rather than a
// count.
func TestCheckReportsEveryViolation(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	d.orchestrator.audit = func(context.Context) error {
		return &volume.InconsistentError{Violations: []volume.Violation{
			{Key: "demo/vm/vm-a/ckpt/7/index", Err: errors.New("the index names a part that is not there")},
			{Key: "demo/control/vm-b", Class: volume.AllowUnrecordedVM,
				Err: errors.New("the VM this object belongs to has no control record")},
		}}
	}
	result, err := d.orchestrator.Check(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.OK {
		t.Fatal("a deployment with violations reported as consistent")
	}
	if len(result.Violations) != 2 {
		t.Fatalf("reported %d violations, want both", len(result.Violations))
	}
	first := result.Violations[0]
	if first.Key != "demo/vm/vm-a/ckpt/7/index" || first.Class != "violation" ||
		first.Message != "the index names a part that is not there" {
		t.Fatalf("the first violation reads %+v", first)
	}
	if second := result.Violations[1]; second.Class != "unrecorded-vm" {
		t.Fatalf("the second violation reads %+v, want its allowance class named", second)
	}
}

// TestCheckReportsAStoreItCouldNotRead as a failure rather than as a
// deployment that disagrees with itself: nothing was checked, so nothing is
// known, and reporting "consistent" would be the worst answer available.
func TestCheckReportsAStoreItCouldNotRead(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	refused := errors.New("the bucket refused the listing")
	d.orchestrator.audit = func(context.Context) error { return refused }
	if _, err := d.orchestrator.Check(t.Context()); !errors.Is(err, refused) {
		t.Fatalf("checking over a store that refused = %v, want the refusal", err)
	}
}

// TestCheckWithoutABucketIsRefused: an orchestrator with nothing to read cannot
// answer this at all, and saying so is better than an empty pass.
func TestCheckWithoutABucketIsRefused(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {}})
	d.orchestrator.audit = nil
	if _, err := d.orchestrator.Check(t.Context()); !errors.Is(err, errRequest) {
		t.Fatalf("checking with no bucket configured = %v, want an invalid request", err)
	}
}
