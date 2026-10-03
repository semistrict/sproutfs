package real

import (
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

var _ platform.Network = (*Network)(nil)
