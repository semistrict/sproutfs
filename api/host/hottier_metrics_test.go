package host_test

import (
	"strings"
	"testing"

	hostapi "github.com/semistrict/sproutfs/api/host"
)

// What a host's hot tier did: its reads by how they ended, its failures by
// why, the times it was marked down and whether it is now, its fills by what
// handed them over, sent, found present, duplicated and dropped by every
// reason, its sampled checks, and its queue. A host with no hot tier reports
// zeroes for every reason.
func TestMetricsExposeTheHotTier(t *testing.T) {
	body := hostapi.Metrics(hostapi.Status{HotTier: &hostapi.HotTier{Hits: 11, Misses: 12, Skipped: 13,
		Failed: map[string]uint64{"error": 1, "slow": 2, "corrupt": 3}, MarkedDown: 14, Down: true,
		FromReads: 15, FromPublications: 16, Sent: 17, SentBytes: 18, Present: 19, Duplicates: 20,
		Dropped: map[string]uint64{"queue": 4, "rate": 5, "read": 6, "write": 7}, HeadChecks: 21, HeadMissing: 22,
		QueuedBytes: 23, QueueBytes: 24}})
	for _, want := range []string{
		`sproutfs_hot_tier_reads_total{result="hit"} 11`,
		`sproutfs_hot_tier_reads_total{result="miss"} 12`,
		`sproutfs_hot_tier_reads_total{result="skipped"} 13`,
		`sproutfs_hot_tier_failures_total{reason="error"} 1`,
		`sproutfs_hot_tier_failures_total{reason="slow"} 2`,
		`sproutfs_hot_tier_failures_total{reason="corrupt"} 3`,
		"sproutfs_hot_tier_marked_down_total 14",
		"sproutfs_hot_tier_down 1",
		`sproutfs_hot_tier_fills_total{from="read"} 15`,
		`sproutfs_hot_tier_fills_total{from="publication"} 16`,
		"sproutfs_hot_tier_fills_sent_total 17",
		"sproutfs_hot_tier_fill_bytes_sent_total 18",
		"sproutfs_hot_tier_fills_present_total 19",
		"sproutfs_hot_tier_fills_duplicate_total 20",
		`sproutfs_hot_tier_fills_dropped_total{reason="queue"} 4`,
		`sproutfs_hot_tier_fills_dropped_total{reason="rate"} 5`,
		`sproutfs_hot_tier_fills_dropped_total{reason="read"} 6`,
		`sproutfs_hot_tier_fills_dropped_total{reason="write"} 7`,
		`sproutfs_hot_tier_fills_dropped_total{reason="closed"} 0`,
		"sproutfs_hot_tier_head_checks_total 21",
		"sproutfs_hot_tier_head_missing_total 22",
		"sproutfs_hot_tier_fill_queued_bytes 23",
		"sproutfs_hot_tier_fill_queue_limit_bytes 24",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Fatalf("the exposition has no %q in it:\n%s", want, body)
		}
	}
	body = hostapi.Metrics(hostapi.Status{})
	for _, reason := range hostapi.HotTierDropReasons {
		if want := `sproutfs_hot_tier_fills_dropped_total{reason="` + reason + `"} 0`; !strings.Contains(body, want+"\n") {
			t.Fatalf("a host with no hot tier has no %q in its exposition:\n%s", want, body)
		}
	}
	if want := "sproutfs_hot_tier_down 0"; !strings.Contains(body, want+"\n") {
		t.Fatalf("a host with no hot tier has no %q in its exposition:\n%s", want, body)
	}
}
