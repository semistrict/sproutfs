package host_test

import (
	"strings"
	"testing"
	"time"

	hostapi "github.com/semistrict/sproutfs/api/host"
)

// An embedder runs the host as a library, so it reads the host's Status and
// serves the exposition itself. The numbers it has to alert on are there: what
// losing the host would cost, pages spilled to disk, and object-store failures.
func TestMetricsAreWhatAnEmbedderServes(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{
		VMs: []hostapi.VM{{ID: "vm-1", LossWindow: 90 * time.Second, Waiting: true}},
		Pager: hostapi.Pager{
			RAM: hostapi.PagerKind{Spills: 5},
		},
		Store: hostapi.Store{Put: hostapi.StoreCount{Calls: 4, Failures: 2}},
	})
	for _, want := range []string{
		"sproutfs_loss_window_seconds 90",
		"sproutfs_vms_waiting 1",
		`sproutfs_pager_spills_total{kind="ram"} 5`,
		`sproutfs_store_failures_total{operation="put"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}
