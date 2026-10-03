package peer

import (
	"testing"

	"github.com/semistrict/sproutfs/platform"
)

// A peer is its host, whatever port each of its connections came from, so
// every connection of one host counts against one budget.
func TestAPeerIsItsAddressWithoutThePort(t *testing.T) {
	for address, want := range map[platform.Address]string{
		"10.0.0.7:41234": "10.0.0.7",
		"10.0.0.7:41235": "10.0.0.7",
		"[fd00::7]:8081": "fd00::7",
		"destination":    "destination",
	} {
		if got := peerKey(address); got != want {
			t.Fatalf("peerKey(%q) = %q, want %q", address, got, want)
		}
	}
}
