//go:build linux && (amd64 || arm64)

package vmmachine

import "time"

// SetAPIDeadline shortens the bound on the wait for a VMM's API socket and
// returns what restores it, so a test need not wait out the production one.
func SetAPIDeadline(d time.Duration) func() {
	previous := apiDeadline
	apiDeadline = d
	return func() { apiDeadline = previous }
}
