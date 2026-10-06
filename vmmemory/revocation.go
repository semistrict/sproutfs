package vmmemory

import (
	"context"
	"errors"
)

const revokeBatchPages = 1024

// errVictimHeld reports an eviction that could not take one resident page away
// from every binding it is reachable from, because one of those bindings belongs
// to a memory region that can no longer take mapping commands. The page stays mapped
// there, so it is not this host's to reuse, and the memory region it could not be
// taken from is terminal from here — which is what keeps the reclaim from
// choosing that page again. It never reaches a caller: an allocation that
// meets it takes another victim.
var errVictimHeld = errors.New("vmmemory: a resident page's other holder cannot give it up")

// revocationFailed makes a failed revocation terminal, as every one of them is:
// the pages stay recorded as mapped, which is what keeps the page the guest
// may still read through reachable, and the memory region can no longer take mappings
// away. A revocation the client refused is terminal too, and deliberately not
// the refusal a fault is served again for: what a fault waits for is a
// revocation, and this is one that could not happen.
func (r *MemoryRegion) revocationFailed(err error) error {
	if errors.Is(err, ErrMappingRefused) {
		err = errors.New("managed-memory revocation refused: " + err.Error())
	}
	return r.fail(err)
}

// underProtection runs one revocation with the memory region's protection held
// shared, so that a seal's write-protect commands and the mappings a reclaim
// takes away cannot overlap. It is never nested and never waits for the memory region
// or for a page while it holds it.
func (r *MemoryRegion) underProtection(ctx context.Context, revoke func() error) error {
	if err := r.protectMu.RLock(ctx); err != nil {
		return err
	}
	defer r.protectMu.RUnlock()
	return revoke()
}
