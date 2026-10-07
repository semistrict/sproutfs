package host

import "time"

// MachineTerms are what one VM asks of the host that runs it, beyond its
// volumes. A migration carries them to the next host; a fork's children take
// the defaults.
type MachineTerms struct {
	// Pull marks a VM that pulls its whole memory onto this host's disk for as
	// long as it runs here. See pulling.
	Pull bool
	// CheckpointInterval is how often the VM's disks are checkpointed. Zero is
	// the host's interval. A positive interval is clamped to between the
	// host's minimum and its own interval, so a VM may ask for a tighter bound
	// on what a host loss costs it and never for a looser one. A negative
	// interval asks for none at all: the VM is checkpointed only when it
	// stops, when it moves, and when the pager needs its dirty pages back. Its
	// writes are held to no loss window and its flushes complete at once. A
	// host loss loses everything it wrote since it started.
	CheckpointInterval time.Duration
	// PostCopy marks a VM a migration or a fork brought here whose pages are
	// still arriving from its source. With durable flush on, its flushes wait
	// until PostCopied says the last has (journal.go).
	PostCopy bool
}

// DefaultMinimumCheckpointInterval is the shortest interval a VM of a host
// that configures no minimum may ask for. A checkpoint pauses the guest and
// uploads its dirty pages, so an interval much shorter spends the guest's time
// on the object store.
const DefaultMinimumCheckpointInterval = time.Second

// CheckpointInterval is how often this host checkpoints the disks of the named
// VM, resolved from what it asked for: zero for a VM this host does not run,
// one that asked for no interval, and every VM of a host with no loop.
func (h *Host) CheckpointInterval(vmID string) time.Duration {
	h.machines.mu.Lock()
	defer h.machines.mu.Unlock()
	if entry := h.machines.running[vmID]; entry != nil {
		return entry.cadence.interval
	}
	return 0
}

// minimumIntervalOf resolves the configured minimum.
func minimumIntervalOf(configured time.Duration) time.Duration {
	if configured <= 0 {
		return DefaultMinimumCheckpointInterval
	}
	return configured
}

// cadence is how one VM is checkpointed on this host, resolved from its terms
// against the host's configuration.
type cadence struct {
	// interval is the wait between the loop's turns at the VM, zero for none.
	interval time.Duration
	// windowed reports a VM whose writes the loss window holds to it.
	windowed bool
	// flushBound is how stale the VM's disks may be for its flush to complete
	// at once, zero for every flush at once.
	flushBound time.Duration
}

// cadenceOf resolves one VM's terms. A host with no loop takes no turns at any
// VM, and keeps the window and the bound it configured, which is what a test
// that drives its own captures wants.
func (h *Host) cadenceOf(terms MachineTerms) cadence {
	if h.checkpointInterval <= 0 {
		return cadence{windowed: true, flushBound: h.flushBound}
	}
	switch {
	case terms.CheckpointInterval < 0:
		return cadence{}
	case terms.CheckpointInterval == 0:
		return cadence{interval: h.checkpointInterval, windowed: true, flushBound: h.flushBound}
	}
	interval := min(max(terms.CheckpointInterval, h.minimumInterval), h.checkpointInterval)
	return cadence{interval: interval, windowed: true,
		flushBound: flushBoundOf(h.flushBoundConfigured, interval)}
}
