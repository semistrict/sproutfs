package real

import (
	"context"
	"net"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/framer"
)

type NetworkConfig struct {
	// Transport carries the streams this network frames. Nil is plain TCP.
	Transport      platform.Transport
	MaxHeaderSize  uint32
	MaxPayloadSize uint64
}

// Network frames connections over a transport.
type Network struct {
	*framer.Network
}

func NewNetwork(config NetworkConfig) *Network {
	if config.Transport == nil {
		config.Transport = TCP{}
	}
	return &Network{framer.New(framer.Config{Transport: config.Transport,
		MaxHeaderSize: config.MaxHeaderSize, MaxPayloadSize: config.MaxPayloadSize})}
}

// TCP is plain TCP. It authenticates nothing: every peer that reaches a
// listener is accepted, and a dial believes whatever answers at the address.
type TCP struct{}

func (TCP) Listen(address platform.Address) (net.Listener, error) {
	return new(net.ListenConfig).Listen(context.Background(), "tcp", string(address))
}

func (TCP) Dial(ctx context.Context, address platform.Address) (net.Conn, error) {
	return (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", string(address))
}

var _ platform.Network = (*Network)(nil)
