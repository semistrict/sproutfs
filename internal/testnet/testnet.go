// Package testnet provides a loopback platform.Network for tests that need
// production TCP framing rather than a simulated transport. Logical addresses
// stay fixed for the lifetime of a cluster while each Listen binds a fresh
// ephemeral 127.0.0.1 port, so a restarted node keeps its identity without the
// test having to know which port the operating system handed out.
package testnet

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/adapters"
)

var _ platform.Network = (*Network)(nil)

// Network maps logical addresses onto ephemeral loopback ports. Its zero value
// is not usable; construct one with New.
type Network struct {
	base platform.Network
	*addresses
}

// addresses is where each logical address is listening, shared by every view
// of one cluster's network.
type addresses struct {
	dials atomic.Uint64

	mu        sync.RWMutex
	listeners map[platform.Address]*listener
}

// listener is the port most recently bound for one logical address.
type listener struct {
	platform.Listener
	closed atomic.Bool
}

func (l *listener) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

// New is a cluster network over plain TCP.
func New() *Network {
	return &Network{base: adapters.NewNetwork(),
		addresses: &addresses{listeners: make(map[platform.Address]*listener)}}
}

// Over is this cluster's addresses reached over another transport, which is
// one host of the cluster presenting credentials of its own.
func (n *Network) Over(transport platform.Transport) *Network {
	return &Network{base: adapters.NewNetworkOver(transport), addresses: n.addresses}
}

// Listen binds an ephemeral loopback port and records it as the current
// location of the logical address.
func (n *Network) Listen(address platform.Address) (platform.Listener, error) {
	bound, err := n.base.Listen("127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l := &listener{Listener: bound}
	n.mu.Lock()
	n.listeners[address] = l
	n.mu.Unlock()
	return l, nil
}

// Listening reports whether the port most recently bound for the logical
// address is still open, which is how a test checks that a stopped node gave its
// port back without binding a port of its own.
func (n *Network) Listening(address platform.Address) bool {
	n.mu.RLock()
	l, found := n.listeners[address]
	n.mu.RUnlock()
	return found && !l.closed.Load()
}

// Dial reaches the port most recently bound for the logical address. An
// address that has never listened is reported as unavailable rather than
// dialed, so a stopped node behaves like an unreachable peer.
func (n *Network) Dial(ctx context.Context, _, to platform.Address) (platform.Conn, error) {
	n.mu.RLock()
	l, found := n.listeners[to]
	n.mu.RUnlock()
	if !found {
		return nil, platform.ErrUnavailable
	}
	n.dials.Add(1)
	return n.base.Dial(ctx, "", l.Address())
}

// Dials counts the connections opened through Dial, letting a test assert that
// a connection pool actually reuses connections.
func (n *Network) Dials() uint64 { return n.dials.Load() }
