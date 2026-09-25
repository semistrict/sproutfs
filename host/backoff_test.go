package host

import (
	"testing"
	"time"
)

// A publication that failed while its VM is already past the loss window is not
// something to retry at the next interval: the window is exceeded for the whole
// of that wait, so the guest is held back for the whole of it. The loop tries
// again at an eighth of the interval and doubles up to the interval, which is
// the shortest wait that does not turn a store outage into a checkpoint storm.
func TestTheBackoffAfterAFailedPublicationDoublesToTheInterval(t *testing.T) {
	const interval = 60 * time.Second
	want := []time.Duration{
		7500 * time.Millisecond, 15 * time.Second, 30 * time.Second,
		60 * time.Second, 60 * time.Second, 60 * time.Second,
	}
	for i, expected := range want {
		if got := backoff(interval, i+1); got != expected {
			t.Fatalf("the wait after %d failures is %s, want %s", i+1, got, expected)
		}
	}
}

// An interval too short to divide still has to produce a wait: a test driving
// millisecond intervals would otherwise spin without waiting at all, which is a
// busy loop rather than a retry.
func TestTheBackoffOfAnIntervalTooShortToDivideIsTheInterval(t *testing.T) {
	for _, interval := range []time.Duration{time.Nanosecond, 4 * time.Nanosecond} {
		if got := backoff(interval, 1); got != interval {
			t.Fatalf("the first wait of a %s interval is %s, want the interval", interval, got)
		}
	}
}
