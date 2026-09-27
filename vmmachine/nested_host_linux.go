package vmmachine

import (
	"errors"
	"os"
	"runtime"
	"strings"
)

// nestedHost reports why this host runs no nested VM, or nil where it does: an
// Intel x86_64 host, the one whose VMX controls the Firecracker fork narrows.
// See nested.go.
func nestedHost() error {
	if runtime.GOARCH != "amd64" {
		return errors.New("only Intel x86_64 hosts run one, not " + runtime.GOARCH)
	}
	cpuinfo, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(cpuinfo), "\n") {
		key, value, found := strings.Cut(line, ":")
		if found && strings.TrimSpace(key) == "vendor_id" {
			if vendor := strings.TrimSpace(value); vendor != "GenuineIntel" {
				return errors.New("only Intel x86_64 hosts run one, not " + vendor)
			}
			return nil
		}
	}
	return errors.New("only Intel x86_64 hosts run one, and /proc/cpuinfo names no vendor")
}
