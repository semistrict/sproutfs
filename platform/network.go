package platform

import (
	"context"
	"io"
)

// Address identifies one process endpoint. Simulated addresses may be logical
// names; real addresses are host:port pairs.
type Address string

// Network creates framed, reliable connections. A simulator may inject loss,
// duplication, delay, or disconnection beneath this interface.
type Network interface {
	Listen(Address) (Listener, error)
	Dial(context.Context, Address, Address) (Conn, error)
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
