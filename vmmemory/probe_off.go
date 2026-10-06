//go:build !sproutfsprobe

package vmmemory

import "context"

// probeState is nothing in an ordinary build: every check below compiles away,
// and the pager carries no per-page probe state at all. See probe_on.go for
// what the accelerated build checks and why. A check reports its finding as a
// string rather than panicking, so an ordinary build's callers compare against
// the empty one and keep nothing.
type probeState struct{}

func (probeState) stable(context.Context, *Host, probePage, string) string      { return "" }
func (probeState) bind(*Host, probeBinding, probePage) string                   { return "" }
func (probeState) granted(probeBinding, probePage, probePage)                   {}
func (probeState) retired(probeBinding)                                         {}
func (probeState) reshared(context.Context, *Host, probePage, probePage) string { return "" }
func (probeState) keep(string)                                                  {}
func (probeState) take() string                                                 { return "" }
func (probeState) resharedSpilled(context.Context, *Host, probeBinding, []byte, probePage) string {
	return ""
}

// Ring reports nothing without the accelerator's build tag.
func Ring(*MemoryRegion, uint64, uint64) []string { return nil }
