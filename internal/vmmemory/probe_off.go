//go:build !sproutfsprobe

package vmmemory

import "context"

// probeState is nothing in an ordinary build: every check below compiles away,
// and the pager carries no per-page probe state at all. See probe_on.go for
// what the accelerated build checks and why.
type probeState struct{}

func (probeState) stable(context.Context, *Host, *resident, string) {}
func (probeState) bind(*Host, *binding, *resident)                  {}
