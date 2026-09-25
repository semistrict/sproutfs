//go:build linux && (amd64 || arm64)

package vmmachine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/resource"
)

var ErrScratchBusy = errors.New("vmmachine: scratch directory is still in use")

// Scratch owns a dedicated VMM staging directory. A process restart is a host
// loss, so nothing under it survives one: opening a Scratch wipes the directory
// and creates it empty, and no file beneath it is authority for anything. Each
// VMM gets a private subdirectory that goes with it.
//
// Close the processes before closing Scratch.
type Scratch struct {
	closeMu sync.Mutex
	mu      sync.Mutex
	root    string
	// disks opens the local storage of one VM's staging namespace. The
	// directory is this package's — the sockets and the VMM binary beside it
	// need real paths — and which adapter is laid over it is the process's, so
	// a Scratch is given the port rather than an adapter.
	disks           platform.Disks
	processes       map[*Process]bool // true once process shutdown has finished
	closing, closed bool
}

// NewScratch wipes directory and opens it as this process's scratch, with disks
// as what opens the local storage of each VM's staging namespace beneath it.
// The deployment must give one host process one directory: a second host
// opening the same one would delete the first's live VMM staging.
func NewScratch(ctx context.Context, directory string, disks platform.Disks) (*Scratch, error) {
	if directory == "" || disks == nil {
		return nil, resource.ErrInvalid
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	vms := filepath.Join(root, "vms")
	if err := os.RemoveAll(vms); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(vms, 0o700); err != nil {
		return nil, err
	}
	return &Scratch{root: vms, disks: disks, processes: make(map[*Process]bool)}, nil
}

// Directory is the diagnostic path containing this owner's VM directories.
// Consumers must use Start instead of writing or removing files beneath it.
func (s *Scratch) Directory() string { return s.root }

func (s *Scratch) create(ctx context.Context, p *Process) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return "", err
	}
	if s.closed || s.closing {
		return "", resource.ErrClosed
	}
	dir, err := os.MkdirTemp(s.root, "vm-")
	if err != nil {
		return "", err
	}
	s.processes[p] = false
	p.scratch = s
	return dir, nil
}

func (s *Scratch) stopped(p *Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.processes[p]; ok {
		s.processes[p] = true
	}
}
func (s *Scratch) removed(p *Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.processes, p)
}

// Close refuses live processes and closes the stopped ones, including failed
// starts whose cleanup could not finish. Failed cleanup leaves the owner
// available for retry.
func (s *Scratch) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	pending := make([]*Process, 0, len(s.processes))
	for p, stopped := range s.processes {
		if !stopped {
			s.mu.Unlock()
			return ErrScratchBusy
		}
		pending = append(pending, p)
	}
	s.closing = true
	s.mu.Unlock()
	var result error
	for _, p := range pending {
		result = errors.Join(result, p.Close())
	}
	if result != nil {
		return result
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
