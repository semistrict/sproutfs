//go:build !linux

package main

// readHost reads nothing off Linux, where the host's counters are not in
// /proc. A record from here has no host counters.
func readHost() (hostStats, error) { return hostStats{}, nil }
