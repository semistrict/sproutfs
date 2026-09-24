package main

import (
	"net/http"
	"testing"

	"github.com/semistrict/sproutfs/internal/api/orch"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

// sealed is what a host answers a stop or a delete of a VM a fork point still
// holds: a conflict, with the reason in the body. It is the error the host
// client hands back, status line and all.
func sealed() error {
	return jsonhttp.Error{Op: "stop", Status: http.StatusConflict,
		Message: "volume: the VM is sealed: vm-a"}
}

// TestAHostsRefusalIsReportedAsARefusalAndNotAsAFailure: a stop of a parent a
// fork point holds is refused by the host that runs it, and that host is the
// only thing that knows why. The orchestrator is a client of it, so what it
// relays has to be the host's own account: a conflict an operator can wait out
// or release, rather than this process reporting an internal failure of its own
// and sending the operator to read the wrong logs.
func TestAHostsRefusalIsReportedAsARefusalAndNotAsAFailure(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	d.hosts["host-0"].refuse = sealed()

	_, err := d.orchestrator.Stop(t.Context(), "vm-a", orch.StopRequest{})
	if err == nil {
		t.Fatal("stopping a VM its host refused reported success")
	}
	if status := statusOf(err); status != http.StatusConflict {
		t.Fatalf("a stop the host refused with a conflict is reported as %d: %v", status, err)
	}
	if err := d.orchestrator.Delete(t.Context(), "vm-a"); err == nil {
		t.Fatal("deleting a VM its host refused reported success")
	} else if status := statusOf(err); status != http.StatusConflict {
		t.Fatalf("a delete the host refused with a conflict is reported as %d: %v", status, err)
	}
}

// TestAHostsOwnInternalFailureIsTheDeploymentsOwn: the other half of the same
// rule. A host that broke rather than refused is something wrong with the
// deployment, and relaying that as a client error would tell an operator to fix
// their request.
func TestAHostsOwnInternalFailureIsTheDeploymentsOwn(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}})
	d.hosts["host-0"].refuse = jsonhttp.Error{Op: "stop", Status: http.StatusInternalServerError,
		Message: "the object store refused the publication"}
	_, err := d.orchestrator.Stop(t.Context(), "vm-a", orch.StopRequest{})
	if err == nil {
		t.Fatal("stopping a VM whose host failed reported success")
	}
	if status := statusOf(err); status != http.StatusInternalServerError {
		t.Fatalf("a host's own failure is reported as %d: %v", status, err)
	}
}

// TestTheOrchestratorsOwnRefusalWinsOverAHostsStatus: an orchestrator that
// refused before it ever reached a host is answering for itself, and a host
// status carried in a joined error must not override that.
func TestTheOrchestratorsOwnRefusalWinsOverAHostsStatus(t *testing.T) {
	d := newDeployment(t, map[string][]string{"host-0": {"vm-a"}, "host-1": {}})
	if _, err := d.orchestrator.Stop(t.Context(), "vm-nobody-runs", orch.StopRequest{}); statusOf(err) != http.StatusNotFound {
		t.Fatalf("stopping a VM no host runs is reported as %d: %v", statusOf(err), err)
	}
}
