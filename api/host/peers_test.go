package host_test

import (
	"strings"
	"testing"

	hostapi "github.com/semistrict/sproutfs/api/host"
)

// The peer server keeps its metric names, and the table of peers says how many
// hosts are up, down and of a release this one shares no version with.
func TestMetricsExposeThePeerServerAndItsPeers(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{
		Pages: hostapi.Pages{Requests: 9, Served: 7, Absent: 2, Refused: 1},
		Peers: []hostapi.Peer{{Address: "a"}, {Address: "b", Down: true, Cause: "dead"},
			{Address: "c", Incompatible: "3-4"}, {Address: "d"}},
	})
	for _, want := range []string{
		"sproutfs_pages_requests_total 9",
		"sproutfs_pages_served_total 7",
		"sproutfs_pages_absent_total 2",
		"sproutfs_pages_refused_total 1",
		`sproutfs_peers{state="up"} 2`,
		`sproutfs_peers{state="down"} 1`,
		`sproutfs_peers{state="incompatible"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
}
