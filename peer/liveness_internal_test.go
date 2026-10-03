package peer

import (
	"fmt"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

// A probe's wait is spread by a tenth either way, by a hash of its peer and its
// attempt, so the hosts probing one peer do not all arrive at once: every wait
// is within the tenth, and a hundred peers do not wait alike.
func TestAProbesWaitIsSpreadByATenthEitherWay(t *testing.T) {
	const wait = 10 * time.Second
	waits := map[time.Duration]bool{}
	for host := range 100 {
		address := platform.Address(fmt.Sprintf("host-%d:8081", host))
		for attempt := 1; attempt <= 3; attempt++ {
			got := spread(address, attempt, wait)
			if got < 9*time.Second || got > 11*time.Second {
				t.Fatalf("%s attempt %d waits %v, want within a tenth of %v", address, attempt, got, wait)
			}
			waits[got] = true
		}
	}
	if len(waits) < 250 {
		t.Fatalf("three hundred probes waited %d different times", len(waits))
	}
	if got := spread("host-1:8081", 1, 5*time.Nanosecond); got != 5*time.Nanosecond {
		t.Fatalf("a wait too short to spread came back %v", got)
	}
}
