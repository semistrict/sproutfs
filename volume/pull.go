package volume

import (
	"context"

	"github.com/semistrict/sproutfs/checkpoint"
)

// Rooted returns a channel that closes once this VM reads a root of its own:
// at once for every VM but a fork, and for a fork when its root publication
// installs. Until then a fork reads its parent's checkpoint and the pages its
// parent held that no checkpoint has.
func (vm *VM) Rooted() <-chan struct{} { return vm.rooted }

// Pull begins copying every page of the checkpoint this VM's volumes sit on
// onto the host's disk: the checkpoint its control record selects, or for a
// fork whose root has not published yet, the one it inherits from its parent.
// A caller that wants a fork's own root pulled waits for Rooted first. The pages
// written since that checkpoint are not in it: they are this host's already, in
// the overlay or in a pager. The caller closes the pull when the VM stops
// running here. See checkpoint.Store.Pull.
func (vm *VM) Pull(ctx context.Context) (*checkpoint.Pull, error) {
	vm.mu.Lock()
	index := vm.baseIndex
	vm.mu.Unlock()
	if index == nil {
		return nil, ErrCorrupt
	}
	return vm.manager.config.Store.Pull(ctx, index)
}
