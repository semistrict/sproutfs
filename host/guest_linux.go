//go:build linux && (amd64 || arm64)

package host

import (
	"context"
	"fmt"
	"net/http"

	"github.com/semistrict/sproutfs/api/guest"
	hostapi "github.com/semistrict/sproutfs/api/host"
	"github.com/semistrict/sproutfs/internal/jsonhttp"
)

func (s *supervisor) Console(ctx context.Context, id string, since int64) (hostapi.Console, error) {
	m, err := s.running(id)
	if err != nil {
		return hostapi.Console{}, err
	}
	if err := context.Cause(ctx); err != nil {
		return hostapi.Console{}, err
	}
	// The console is a ring holding the newest 1 MiB. A reader whose offset
	// names output the ring has dropped is answered from the oldest byte it
	// still has, and Offset differing from what was asked for says so.
	data, from, next := m.process.Console(since, consoleWindowBytes)
	return hostapi.Console{VM: id, Offset: from, Next: next,
		Dropped: from > since, Data: string(data)}, nil
}

func (s *supervisor) WriteConsole(ctx context.Context, id string, data string) error {
	m, err := s.running(id)
	if err != nil {
		return err
	}
	return m.process.WriteConsole(ctx, []byte(data))
}

// ---------------------------------------------------------------------------
// The guest
// ---------------------------------------------------------------------------

// guestClient reaches the agent in one VM's guest. Every connection it makes is
// a fresh forwarded vsock stream to the machine this host is running now, so a
// VM that has just migrated in is reached over its new process's socket without
// anything having to invalidate a cached one.
func (s *supervisor) guestClient(id string) (*http.Client, error) {
	m, err := s.running(id)
	if err != nil {
		return nil, err
	}
	socket := m.process.VsockPath()
	if socket == "" {
		return nil, fmt.Errorf("%w: %s was started without a vsock", guest.ErrNoGuest, id)
	}
	return guest.NewClient(socket, guestTimeout), nil
}

func (s *supervisor) Exec(ctx context.Context, id string, request hostapi.ExecRequest) (hostapi.ExecResult, error) {
	client, err := s.guestClient(id)
	if err != nil {
		return hostapi.ExecResult{}, err
	}
	result, err := jsonhttp.Call[hostapi.ExecResult](ctx, client, http.MethodPost, guest.URL("/exec"), request)
	if err != nil {
		return hostapi.ExecResult{}, fmt.Errorf("running a command in %s: %w", id, err)
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Migration
// ---------------------------------------------------------------------------
