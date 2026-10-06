package vmmemory

import (
	"context"
)

// Why a copy is given back without a checkpoint.
//
// A write fault is not always a store. On x86-64 KVM finishes a cold read from
// a worker thread, async_pf_execute, that always asks for the page writable. On
// aarch64 a guest's first execution of a page reaches the pager as a write too.
// Either way the pager makes a private copy. The settle notices a copy that
// never changed, but only behind a checkpoint, and RAM is never checkpointed on
// the interval: only captures, forks and migrations seal it. So without this, a
// RAM page shared between VMs becomes a private copy for as long as the VM
// lives, which at a 2 MiB page is 2 MiB the host holds twice.
//
// What is given back is a cold copy: one a write fault made of a page the
// guest did not map, which is what such a read looks like. Its session gives it
// back once it is coldCopyAge old, and a seal or an eviction sooner: see
// cold.go. A copy of a page the guest had mapped is a store the guest really
// made, and is left to the settle behind its next checkpoint.
//
// The give-back needs no pause. It works one page at a time, and it holds that
// page the way a store fault holds it: the page's window, the memory region
// shared, the origin's lock and the copy's. Holding the window keeps every
// fault of the page out, and holding the memory region keeps a seal out.
// Write-protecting the copy stops the guest's stores, because every store then
// traps and waits for the window. From then on the copy's bytes cannot change,
// so comparing them with the origin's is exact. A store that trapped in the
// meantime is served once the window is free. Against the origin it copies
// again, and against a copy that was kept it lands. It loses nothing.
//
// The guest is pointed at the origin in place, with the mapping command a
// store uses to replace the page it copied from, and the page is installed
// read-only at once: mapInPlace, which a move uses for the same reason. It is not revoked. A revoked page is missing, so on x86-64
// the next cold read would arrive as a write again and copy again, and the
// give-back would undo its own work at every pass. An installed page is present,
// so KVM maps it for a read without asking the pager.
//
// It relies on every writer of guest RAM going through the VMM's page tables,
// where the write-protection stops it: the VMM's own threads, the kernel's
// copy_to_user for the sync block engine and the tap device, and KVM's own
// writes, which use the userspace address or a pfn cache that the MMU notifier
// invalidates. A writer that pinned the copy before the write-protection would
// write into a page this frees. Managed RAM refuses the io_uring block engine
// and vhost-user, and Firecracker has no vhost-net or vhost-vsock. KVM's maps
// for a nested guest are the one exception, and every seal and copy-on-write
// shares it: see "Writers that bypass the page tables" in docs/vm-memory.md.

// liftProtection takes off the write-protection a give-back put on a copy it keeps,
// which also wakes a store that trapped on it.
func (r *MemoryRegion) liftProtection(ctx context.Context, index uint64) error {
	if err := r.resolvePages(ctx, index, 1, true); err != nil {
		return r.fail(err)
	}
	return nil
}

// givingBack is one give-back pass over the pages that pages lists, which it
// calls once the memory region is known to be live.
func (r *MemoryRegion) givingBack(ctx context.Context, pages func() []uint64) (int, error) {
	return r.zircon.givingBack(ctx, pages)
}
