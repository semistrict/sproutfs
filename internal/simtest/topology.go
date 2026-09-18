// Package simtest generates a deployment and the failures that happen to it
// from one seed.
//
// FoundationDB's simulation decides the cluster and its concurrent failures
// from the seed: how many machines there are, what runs on them, and which of
// them are killed, partitioned or clogged at the same time
// (CompoundWorkload::addFailureInjection). The campaigns here permuted a fixed
// list of faults over a fixed topology, one fault at a time, so a combination
// like "the source is partitioned while the store is unavailable while a second
// host takes the VM over" was unreachable however many seeds were run.
//
// This package is the two halves of that. Topology is the deployment one seed
// generates — how many hosts, how many VMs, how big they are, which of them are
// forks of which, and where each starts. Fault is one thing that goes wrong,
// with a beginning, an end and something it must leave true; the Driver starts
// one to three of them concurrently at seeded offsets over a seeded schedule of
// operations, and traces every start and end beside the adapter operations they
// perturb.
//
// Nothing here is a mock of the thing under test: the volume managers, the
// checkpoint store, the control records, the pagers and the migration
// coordinator are the real ones, over the simulated network, object store and
// disks of internal/platform/sim. Only the VMM process is simulated, because a
// guest is what a VMM has instead of a life of its own.
package simtest

import (
	"fmt"

	"github.com/semistrict/sproutfs/internal/platform/sim"
	"github.com/semistrict/sproutfs/internal/volume"
)

// PageSize is the page every volume of a generated topology is measured in. It
// is the publication unit, the fault unit and the wire unit all at once, so a
// campaign that used any other number would be testing arithmetic.
const PageSize = 2 << 20

// MemoryVolume is the volume every VM's memory is in, which is the name
// vmmachine gives a guest's RAM and the volume a cold start discards.
const MemoryVolume = "ram0"

// Topology is the deployment one seed generates: the hosts it runs on and the
// VMs those hosts hold.
//
// It is data and nothing else, drawn before anything starts. A campaign that
// fails prints it, and a topology printed is a topology reproduced: the same
// seed generates the same hosts, the same VMs, the same volumes and the same
// lineage.
type Topology struct {
	// Hosts are the hosts of the deployment, named host-0 upwards. There are
	// always at least two, because a migration needs somewhere to go.
	Hosts []string
	// VMs are the VMs in the order they come into existence. A fork always
	// appears after its parent, so the list can be walked forwards.
	VMs []VMSpec
}

// VMSpec is one VM of a topology: what it is made of, where it starts, and the
// VM it is a fork of.
type VMSpec struct {
	// ID is the VM's identity, vm-0 upwards.
	ID string
	// Parent is the VM this one is forked from, or empty for a VM created from
	// nothing. A fork starts the point its parent was sealed at.
	Parent string
	// Host is the index into Topology.Hosts of the host this VM starts on. For
	// a fork it may be its parent's host or another one, which are the two
	// halves of the fork path: sharing the parent's pages, and pulling them
	// out of the parent's page server.
	Host int
	// Volumes is the VM's memory and its PMEM disks, in the order a create
	// takes them.
	Volumes []volume.VolumeSpec
}

// IsFork reports a VM that starts from a parent's fork point.
func (s VMSpec) IsFork() bool { return s.Parent != "" }

// Pages is how many pages of this VM's volumes exist in total, which is the
// floor under any pager arena that has to hold it.
func (s VMSpec) Pages() int {
	total := 0
	for _, v := range s.Volumes {
		total += int(v.Size / PageSize)
	}
	return total
}

// String is the one line a failing campaign prints to say what it was running.
func (t Topology) String() string {
	line := fmt.Sprintf("%d hosts", len(t.Hosts))
	for _, vm := range t.VMs {
		line += fmt.Sprintf(" %s@host-%d", vm.ID, vm.Host)
		if vm.IsFork() {
			line += "<-" + vm.Parent
		}
		line += fmt.Sprintf("(%dp)", vm.Pages())
	}
	return line
}

// The bounds a generated topology stays inside. They are what one seed can
// explore in the time a soak block gives it rather than anything the design
// requires: every one of them is a range and not a constant, so what a sweep
// covers is the range and not its middle.
const (
	minHosts = 2
	maxHosts = 4
	minVMs   = 2
	maxVMs   = 4
	// A volume is small enough that a campaign writes every page of it several
	// times, and large enough that a checkpoint has runs rather than single
	// pages to publish.
	minVolumePages = 1
	maxVolumePages = 3
	// forkChance is how often a VM after the first is a fork of one before it
	// rather than a VM created from nothing. Half, so a seed's lineage is
	// neither always flat nor always a chain.
	forkChance = 0.5
)

// NewTopology draws one deployment from r. Every choice is keyed on a stable
// id, so adding a choice here cannot perturb the ones already drawn — a
// topology's hosts do not move because its volume sizes learned a new range.
func NewTopology(r sim.Random) Topology {
	hosts := minHosts + r.Intn("hosts", maxHosts-minHosts+1)
	t := Topology{Hosts: make([]string, hosts)}
	for i := range t.Hosts {
		t.Hosts[i] = fmt.Sprintf("host-%d", i)
	}
	count := minVMs + r.Intn("vms", maxVMs-minVMs+1)
	for i := range count {
		id := fmt.Sprintf("vm-%d", i)
		spec := VMSpec{ID: id, Host: r.Intn(id+"/host", hosts)}
		// The first VM is created from nothing: something has to exist before
		// anything can be forked from it.
		if i > 0 && r.Chance(id+"/fork", forkChance) {
			spec.Parent = t.VMs[r.Intn(id+"/parent", i)].ID
		}
		// ram0 is every VM's memory. A PMEM disk is there half the time, so a
		// migration has to name and move more than one region on some seeds and
		// exactly one on others.
		spec.Volumes = append(spec.Volumes, volume.VolumeSpec{Name: MemoryVolume,
			Size: uint64(minVolumePages+r.Intn(id+"/ram0", maxVolumePages-minVolumePages+1)) * PageSize})
		if r.Chance(id+"/disk", 0.5) {
			spec.Volumes = append(spec.Volumes, volume.VolumeSpec{Name: "disk",
				Size: uint64(minVolumePages+r.Intn(id+"/disk-pages", maxVolumePages-minVolumePages+1)) * PageSize})
		}
		t.VMs = append(t.VMs, spec)
	}
	return t
}
