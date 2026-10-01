package platform

import (
	"context"
	"io"
	"net"
)

// Address identifies one process endpoint. Simulated addresses may be logical
// names; real addresses are host:port pairs.
type Address string

// Network creates framed, reliable connections. A simulator may inject loss,
// duplication, delay, or disconnection beneath this interface.
type Network interface {
	Listen(Address) (Listener, error)
	// Dial connects from one endpoint to another. The simulator models a link
	// between two named endpoints and needs both; a real network leaves the
	// local end to its transport, and a host passes none.
	Dial(ctx context.Context, from, to Address) (Conn, error)
}

// Transport carries the byte streams a real Network frames: the fabric one
// host reaches another over. The default is plain TCP, which authenticates
// nothing, so hosts on it must share a trusted network. A deployment that
// authenticates its hosts supplies its own, over mutual TLS or its mesh, and
// refuses there any peer it cannot authenticate: whatever a listener accepts is
// served, and whatever a dial reaches is believed.
type Transport interface {
	// Listen opens a listener at address. An error from its Accept ends the
	// serving, so a peer the listener turns away is closed, not returned.
	Listen(address Address) (net.Listener, error)
	// Dial opens one stream to address.
	Dial(ctx context.Context, address Address) (net.Conn, error)
}

type Listener interface {
	Accept(context.Context) (Conn, error)
	Address() Address
	Close() error
}

// Frame separates a protobuf-encoded header from an optional raw payload. The
// payload is replayable so a transport can retry or stream it without first
// copying it into one large buffer.
type Frame struct {
	Header      []byte
	Payload     io.ReaderAt
	PayloadSize int64
}

func (f Frame) Validate() error {
	if f.PayloadSize < 0 || (f.PayloadSize > 0 && f.Payload == nil) {
		return ErrInvalidRange
	}
	return nil
}

type ReceivedFrame struct {
	Header      []byte
	Payload     io.ReadCloser
	PayloadSize int64
}

// Conn transports complete frames rather than exposing a byte stream. Header
// and payload framing is preserved. Send and Receive may be called
// concurrently, but each direction is serialized.
type Conn interface {
	Send(context.Context, Frame) error
	Receive(context.Context) (ReceivedFrame, error)
	LocalAddress() Address
	RemoteAddress() Address
	Close() error
}
