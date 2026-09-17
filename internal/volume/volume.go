// Package volume serves a VM's writable volumes from the one checkpoint its
// control record selects.
//
// A VM's durable state is exactly one published checkpoint: an index, the page
// objects it references, and optional VMM state. Nothing is durable between
// checkpoints. A write applies to an in-memory overlay and returns; it contacts
// no network and waits for nothing. A checkpoint publishes every dirty page of
// every volume under the VM's own identity and then selects the resulting index
// in the control record, which is the moment the write survives the loss of this
// host. Losing the host costs the writes since that selection.
//
// Opening a VM advances the epoch in its control record, which fences whatever
// host held it before, and reads the selected index. There is no replay: the
// overlay starts empty.
package volume

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/semistrict/sproutfs/internal/checkpoint"
	"github.com/semistrict/sproutfs/internal/control"
)

var (
	// ErrInvalidConfig reports a configuration or argument that cannot name a
	// VM, a volume or a range of one.
	ErrInvalidConfig = errors.New("volume: invalid configuration")
	// ErrInvalidRange reports a read, write or discard outside a volume.
	ErrInvalidRange = errors.New("volume: invalid range")
	// ErrUnknownVolume reports a volume this VM does not have.
	ErrUnknownVolume = errors.New("volume: unknown volume")
	// ErrExists reports a VM identity whose control record already exists.
	ErrExists = errors.New("volume: VM already exists")
	// ErrIdentityUsed reports a create of an identity that has checkpoint
	// objects under it and no control record: a VM deleted while a fork read
	// through it, or a create interrupted before its record. Those objects are
	// a lineage's or a collector's, and a VM created here would publish into
	// their keys, so the identity is refused rather than handed out again.
	ErrIdentityUsed = errors.New("volume: the identity was used before")
	// ErrCorrupt reports durable state that disagrees with itself: a control
	// record or an index that does not describe this VM.
	ErrCorrupt = errors.New("volume: corrupt durable state")
	// ErrClosed reports a handle or manager that is no longer usable.
	ErrClosed = errors.New("volume: closed")
	// ErrHandedOff reports a handle whose VM was released to another host by
	// Handoff. The destination opens the checkpoint the control record already
	// selects and fetches everything written since from the source's pager.
	ErrHandedOff = errors.New("volume: handed off")
	// ErrCapacity reports a limit reached: open VMs, or the checkpoint
	// sequences one writer epoch can allocate.
	ErrCapacity = errors.New("volume: capacity exhausted")
	// ErrWriteTooLarge reports a write larger than Config.MaxWriteBytes.
	ErrWriteTooLarge = errors.New("volume: write exceeds the configured limit")
	// ErrNeedsRecovery reports a handle that has lost its VM to a later writer.
	// The VM must be reopened to write anything again.
	ErrNeedsRecovery = errors.New("volume: reopen required")
	// ErrForkPending reports a fork whose own root index has not been published
	// yet. Such a fork runs on the host that created it and nowhere else: its
	// first checkpoint is what makes it a VM any host can open, and a host lost
	// before then loses it.
	ErrForkPending = errors.New("volume: fork's root checkpoint is not published")
	// ErrSealed reports a VM whose frames a fork point holds. Nothing may seal
	// them again — no capture, no further fork — until that point is retired,
	// which is when the child it was taken for has published or pulled every
	// page it inherited.
	ErrSealed = errors.New("volume: a fork point holds this VM's sealed frames")
)

// Config supplies the control records and the object storage a manager serves
// from. Zero-valued budgets take their documented defaults.
type Config struct {
	// Control selects each VM's checkpoint and holds its writer epoch; Store
	// publishes and reads those checkpoints.
	Control *control.Client
	Store   *checkpoint.Store
	// MaxWriteBytes bounds the payload of one write or batched write. Default
	// 2 MiB. The pager sizes its writeback batches against it.
	MaxWriteBytes int
	// MaxOpenVMs bounds the live handles one manager owns. Default 4096.
	MaxOpenVMs int
}

const (
	defaultMaxWriteBytes = 2 << 20
	defaultMaxOpenVMs    = 4096
	maximumWriteBytes    = 16 << 20
)

// Manager opens the VMs one host serves. Its methods are safe for concurrent
// use. It does not own the control client or the checkpoint store; their lifetimes
// belong to the host.
type Manager struct {
	config Config

	mu         sync.Mutex
	closed     bool
	open       map[*VM]struct{}
	current    map[string]*VM
	nextHandle uint64
}

// Stats is a manager's host-wide accounting.
type Stats struct {
	// OpenVMs is the live handles this manager owns and MaxOpenVMs the bound
	// they are admitted against. A handle superseded by a later open still
	// occupies a slot until it is closed, so this is the count that refuses the
	// next open, not the number of distinct VM identities.
	OpenVMs    int
	MaxOpenVMs int
	// DirtyBytes is an upper bound on what the next checkpoints publish, summed
	// over every VM this manager currently writes. It is also what is lost if
	// this host dies now.
	DirtyBytes uint64
}

// Stats reports this manager's host-wide totals without contacting the network.
func (m *Manager) Stats() Stats {
	stats := Stats{MaxOpenVMs: m.config.MaxOpenVMs}
	m.mu.Lock()
	stats.OpenVMs = len(m.open)
	m.mu.Unlock()
	for _, vm := range m.VMs() {
		vm.mu.Lock()
		stats.DirtyBytes += vm.dirty
		vm.mu.Unlock()
	}
	return stats
}

// NewManager validates the configuration and returns a manager over it.
func NewManager(config Config) (*Manager, error) {
	if config.MaxWriteBytes == 0 {
		config.MaxWriteBytes = defaultMaxWriteBytes
	}
	if config.MaxOpenVMs == 0 {
		config.MaxOpenVMs = defaultMaxOpenVMs
	}
	if config.Control == nil || config.Store == nil ||
		config.MaxWriteBytes < checkpoint.SectorSize || config.MaxWriteBytes > maximumWriteBytes ||
		config.MaxOpenVMs < 1 || config.MaxOpenVMs > 1<<16 {
		return nil, ErrInvalidConfig
	}
	return &Manager{config: config, open: make(map[*VM]struct{}), current: make(map[string]*VM)}, nil
}

// Close stops every VM this manager opened and waits for their publication
// workers. It leaves the control client and the checkpoint store to their owner. A
// canceled Close can be retried with a fresh context.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	handles := make([]*VM, 0, len(m.open))
	for vm := range m.open {
		handles = append(handles, vm)
	}
	m.mu.Unlock()
	// Close in VM identity and acquisition order, independent of map iteration.
	slices.SortFunc(handles, func(a, b *VM) int {
		if order := strings.Compare(a.id, b.id); order != 0 {
			return order
		}
		if a.ordinal < b.ordinal {
			return -1
		}
		if a.ordinal > b.ordinal {
			return 1
		}
		return 0
	})
	var errs []error
	for _, vm := range handles {
		if err := vm.Close(ctx); err != nil && !errors.Is(err, ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// adopt registers a live handle, enforcing the open-VM budget. A handle this
// one supersedes no longer holds the VM — this open advanced the control
// record's epoch, which fenced it — so its next publication fails and it will
// never publish again. It stays registered until its owner closes it, and it
// still occupies one of the open-VM slots.
func (m *Manager) adopt(vm *VM) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if len(m.open) >= m.config.MaxOpenVMs {
		m.mu.Unlock()
		return ErrCapacity
	}
	m.nextHandle++
	vm.ordinal = m.nextHandle
	m.open[vm] = struct{}{}
	m.current[vm.id] = vm
	m.mu.Unlock()
	return nil
}

// usable reports whether this manager can still open VMs. It is checked before
// the control record is touched, so an open a closed manager could never finish
// does not burn a writer epoch.
func (m *Manager) usable() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}
	if len(m.open) >= m.config.MaxOpenVMs {
		return ErrCapacity
	}
	return nil
}

// VMs returns the live handles this manager holds, in ascending identity order.
func (m *Manager) VMs() []*VM {
	m.mu.Lock()
	handles := make([]*VM, 0, len(m.current))
	for _, vm := range m.current {
		handles = append(handles, vm)
	}
	m.mu.Unlock()
	slices.SortFunc(handles, func(a, b *VM) int { return strings.Compare(a.id, b.id) })
	return handles
}

// release drops a handle. Nothing can publish in its name again.
func (m *Manager) release(vm *VM) {
	m.mu.Lock()
	delete(m.open, vm)
	if m.current[vm.id] == vm {
		delete(m.current, vm.id)
	}
	m.mu.Unlock()
	vm.control.Close()
}

// validID reports whether an identity or volume name can be part of an object
// key. It is the same rule the checkpoint and control packages apply.
func validID(id string) bool {
	if id == "" || len(id) > 256 || id == "." || id == ".." || !utf8.ValidString(id) {
		return false
	}
	return !strings.ContainsFunc(id, func(r rune) bool {
		return r == '/' || r < 0x20 || r == 0x7f
	})
}

func validRange(size, offset, length uint64) bool {
	return offset <= size && length <= size-offset
}

// report logs an error that no caller will receive: a publication that runs
// behind the guest, and the reclamation sweep that follows a published
// checkpoint.
func report(ctx context.Context, message, id string, err error) {
	slog.WarnContext(ctx, message, "vm", id, "error", err)
}
