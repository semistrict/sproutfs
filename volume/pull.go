package volume

import (
	"context"

	"github.com/semistrict/sproutfs/checkpoint"
)

// Rooted returns a channel that closes once this VM reads a root of its own:
// at once for every VM but a fork, and for a fork once its root publication
// has installed and given back its hold on the point it was forked at. Until then a fork reads its parent's checkpoint and the pages its
// parent held that no checkpoint has.
func (vm *VM) Rooted() <-chan struct{} { return vm.rooted }

// Pull begins fetching every page of the checkpoint this VM's volumes sit on,
// into the cluster's disk cache inside its share and onto the host's disk
// outside it: the checkpoint its control record selects, or for a fork whose
// root has not published yet, the one it inherits from its parent.
// A caller that wants a fork's own root pulled waits for Rooted first. The pages
// written since that checkpoint are not in it: they are this host's already, in
// the overlay or in a pager. Every checkpoint this VM publishes from then on
// keeps its pages in the same copy as it uploads them, so once published and
// evicted they are read from the disk too. The caller stops the pull's
// fetching when the VM stops running here (checkpoint.Pull.StopFetching); the
// keeping lasts until this handle closes, after its last publication, so the
// checkpoint a stop or a host's shutdown publishes is kept too. A second Pull
// replaces the first, which it closes. See checkpoint.Store.Pull.
func (vm *VM) Pull(ctx context.Context) (*checkpoint.Pull, error) {
	vm.mu.Lock()
	index := vm.baseIndex
	vm.mu.Unlock()
	if index == nil {
		return nil, ErrCorrupt
	}
	pull, err := vm.manager.config.Store.Pull(ctx, index)
	if err != nil {
		return nil, err
	}
	vm.mu.Lock()
	replaced := vm.pull
	vm.pull = pull
	vm.mu.Unlock()
	if replaced != nil {
		replaced.Close()
	}
	return pull, nil
}

// closePull ends the keeping of this VM's pull, once nothing more of it will
// be published.
func (vm *VM) closePull() {
	vm.mu.Lock()
	pull := vm.pull
	vm.pull = nil
	vm.mu.Unlock()
	if pull != nil {
		pull.Close()
	}
}

// pulling is the pull a publication of this VM keeps its pages in, nil for a
// VM not pulling. A pull its caller has closed keeps nothing.
func (vm *VM) pulling() *checkpoint.Pull {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	return vm.pull
}
