package volume

import (
	"context"

	"github.com/semistrict/sproutfs/checkpoint"
)

// Pull begins copying every page of the checkpoint this VM's volumes sit on
// onto the host's disk: the checkpoint its control record selects, or for a
// fork whose root has not published yet, the one it inherits from its parent.
// The pages written since that checkpoint are not in it: they are this host's
// already, in the overlay or in a pager. The caller closes the pull when the
// VM stops running here. See checkpoint.Store.Pull.
func (vm *VM) Pull(ctx context.Context) (*checkpoint.Pull, error) {
	vm.mu.Lock()
	index := vm.baseIndex
	vm.mu.Unlock()
	if index == nil {
		return nil, ErrCorrupt
	}
	return vm.manager.config.Store.Pull(ctx, index)
}
