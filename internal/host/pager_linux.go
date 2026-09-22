//go:build linux && (amd64 || arm64)

package host

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// The mapping-count budget is the one pager bound a node reads off itself, so
// it is the one that stays here; the rest are in pager.go, where both platforms
// build them.
const (
	// vmaHeadroom is the share of the node's mapping-count limit this host
	// admits a VMM's replacements against. The pager's mappings are not the only
	// ones a VMM process has, and the limit is the kernel's for the whole
	// address space, so half of it is the budget and the rest is headroom.
	vmaHeadroom = 2
	// minimumVMAs is the smallest budget worth admitting against; below it the
	// budget is disabled instead, which is also what a client without /proc gets.
	minimumVMAs = 128
	maximumVMAs = 1 << 20
	// maxMapCountPath is where Linux reports that limit.
	maxMapCountPath = "/proc/sys/vm/max_map_count"
)

// vmaBudget is the mapping-count budget a VMM's replacements are admitted
// against, read from the node's own limit with headroom. Zero disables the
// budget, which is what a client that cannot read /proc gets anyway, and is
// what an unreadable or implausibly small limit is answered with.
func vmaBudget() int {
	raw, err := os.ReadFile(maxMapCountPath)
	if err != nil {
		slog.Warn("host: no mapping-count limit to admit against", "path", maxMapCountPath, "error", err)
		return 0
	}
	limit, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || limit <= 0 {
		slog.Warn("host: unreadable mapping-count limit", "path", maxMapCountPath, "value", strings.TrimSpace(string(raw)))
		return 0
	}
	budget := limit / vmaHeadroom
	if budget < minimumVMAs {
		return 0
	}
	return min(budget, maximumVMAs)
}
