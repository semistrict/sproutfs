package main

import "os"

// readHost reads this host's CPU and TCP counters.
func readHost() (hostStats, error) {
	var text [3]string
	for i, path := range []string{"/proc/stat", "/proc/net/snmp", "/proc/net/netstat"} {
		b, err := os.ReadFile(path)
		if err != nil {
			return hostStats{}, err
		}
		text[i] = string(b)
	}
	return parseHost(text[0], text[1], text[2])
}
