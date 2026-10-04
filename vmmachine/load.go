package vmmachine

import (
	"context"
	"fmt"

	"github.com/semistrict/sproutfs/platform/sim"
)

// loadKeys are the keys of a snapshot load request this package sets itself. A
// Starter may add anything else, such as the new host end of a network
// interface or a vsock, and none of these.
var loadKeys = []string{"snapshot_path", "mem_file_path", "mem_backend", "pmem_overrides", "resume_vm", "clock_realtime"}

// loadRequest is the body of a restore's snapshot load: what the Starter added,
// the state file and the RAM's socket as the VMM names them, the PMEM devices'
// sockets, and the clock. The guest stays paused until Release.
//
// Every load is a restore of a captured guest, and the VMM gives each one a new
// generation ID and tells the guest's kernel, which reseeds its random pool from
// it. That holds for a migration's destination too: a receive that is tried
// again after a destination ran the guest, or a migration abandoned after its
// destination resumed, runs one state twice, and only a new generation keeps
// the two from drawing the same random bytes.
//
// On x86_64 the load also moves the guest's clock on by the time the state
// spent stopped, so a restored guest reads the right wall clock from its first
// instruction. The VMM pairs the guest's clock with the host's wall clock when
// it captures the state, and adds the host's wall time since then on load.
// Firecracker cannot do this on aarch64, where a restored guest's clock goes
// on from where its state stopped it.
func loadRequest(ctx context.Context, starter map[string]any, state, ram string,
	pmem []ManagedPmem, arch string) (map[string]any, error) {
	load := make(map[string]any, len(starter)+len(loadKeys))
	for key, value := range starter {
		for _, reserved := range loadKeys {
			if key == reserved && !sim.Bug(ctx, "vmmachine-accept-reserved-load") {
				return nil, fmt.Errorf("vmmachine: the Starter's load request names %q, which this package loads", key)
			}
		}
		load[key] = value
	}
	overrides := make([]map[string]any, 0, len(pmem))
	for _, device := range pmem {
		overrides = append(overrides, map[string]any{"id": device.ID, "socket_path": device.Socket})
	}
	load["snapshot_path"] = state
	load["mem_backend"] = map[string]any{"backend_type": "Sproutfs", "backend_path": ram}
	load["pmem_overrides"] = overrides
	load["resume_vm"] = false
	load["clock_realtime"] = MovesClock(arch) && !sim.Bug(ctx, "vmmachine-restore-stopped-clock")
	return load, nil
}

// MovesClock reports whether a restore on this architecture moves the guest's
// clock on by the time its state spent stopped. Only x86_64's does; see
// loadRequest.
func MovesClock(arch string) bool { return arch == "amd64" }
