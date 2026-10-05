package vmmemory

import (
	"errors"
	"fmt"
)

// Core is the fault and checkpoint core a pager runs: the code that decides
// what a fault maps, what a store copies and what a seal takes. The page layer
// is being replaced by a Go port of Zircon's (plans/zircon-pager-port-
// 2026-10-05.md), and its core is the one part that cannot be swapped in
// place, so the new core runs beside the old one until it has been measured.
// Every exported method of Host and MemoryRegion that reaches the page layer
// dispatches on it. What stays the pager's own whichever core runs — the
// arena, isolation, placement, pressure, the connection — is shared.
type Core int

const (
	// CoreCurrent is the pager's own core, bindings and resident pages over
	// the ported page list, queues, evictor and page requests. It is the
	// default.
	CoreCurrent Core = iota
	// CoreZircon is the core over the ported region layer and identity
	// roots (internal/zirconvm). It serves only what the steps of the port
	// have moved onto it, and refuses the rest with ErrCoreUnsupported.
	CoreZircon
)

// String is the core as a deployment names it.
func (c Core) String() string {
	switch c {
	case CoreCurrent:
		return "current"
	case CoreZircon:
		return "zircon"
	}
	return fmt.Sprintf("Core(%d)", int(c))
}

// ParseCore reads a core as a deployment names it.
func ParseCore(name string) (Core, error) {
	for _, c := range []Core{CoreCurrent, CoreZircon} {
		if name == c.String() {
			return c, nil
		}
	}
	return 0, fmt.Errorf("%w: pager core %q, want current or zircon", ErrConfig, name)
}

// known reports a core this build has.
func (c Core) known() bool { return c == CoreCurrent || c == CoreZircon }

// ErrCoreUnsupported refuses an operation the pager's core does not serve yet.
// Only the zircon core refuses anything, and only while the port is under way:
// the operation is named, and nothing about the pager changed.
var ErrCoreUnsupported = errors.New("managed-memory operation not served by this pager's core")

// unsupported is the refusal of one operation by a core.
func unsupported(core Core, operation string) error {
	return fmt.Errorf("%w: the %s core does not %s yet", ErrCoreUnsupported, core, operation)
}
