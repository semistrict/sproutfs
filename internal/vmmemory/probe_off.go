//go:build !sproutfsprobe

package vmmemory

import "context"

// probeState is nothing in an ordinary build: every check below compiles away,
// and the pager carries no per-page probe state at all. See probe_on.go for
// what the accelerated build checks and why. A check reports its finding as a
// string rather than panicking, so an ordinary build's callers compare against
// the empty one and keep nothing.
type probeState struct{}

func (probeState) stable(context.Context, *Host, *resident, string) string      { return "" }
func (probeState) bind(*Host, *binding, *resident) string                       { return "" }
func (probeState) granted(*binding, *resident, *resident)                       {}
func (probeState) retired(*binding)                                             {}
func (probeState) reshared(context.Context, *Host, *resident, *resident) string { return "" }

// note and Ring are nothing in an ordinary build: the pager records no history
// of what it did to a page. See probe_on.go.
func note(*Region, uint64, string, int, int) {}

// Ring reports nothing without the accelerator's build tag.
func Ring(*Region, uint64, uint64) []string { return nil }

func caller() string { return "" }

func publishReason(bool, pageKey, *Host, *resident) string { return "" }
