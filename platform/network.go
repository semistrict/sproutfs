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
//
// The payload's type says how it can be sent. Bytes is in memory, so a
// transport sends it in the same write as the header. A FileRange lies in an
// open file, so a transport that can hand a file to the kernel sends it without
// reading it into memory. Anything else is read through ReadAt.
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

// MaxFrameBytes bounds one frame's payload on every network. A reader allocates
// what a frame's header says it carries, so the bound is what one bad or hostile
// header can make it allocate. The largest frame a host sends is one page
// request's reply, a few MiB.
const MaxFrameBytes = 16 << 20

// Bytes is a payload held in memory.
type Bytes []byte

func (b Bytes) ReadAt(destination []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, ErrInvalidRange
	}
	if offset >= int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(destination, b[offset:])
	if n < len(destination) {
		return n, io.EOF
	}
	return n, nil
}

// FileRange is a payload that lies in an open file, from Offset on. A transport
// that can hand a file to the kernel sends it with sendfile, so no byte of it
// is read into this process; any other transport reads it through the file.
type FileRange struct {
	File   File
	Offset int64
}

// ReadAt reads the range as a reader at offset 0 would. It reads the file with
// a context that is never cancelled: a transport that has the send's context
// reads through File itself.
func (r FileRange) ReadAt(destination []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, ErrInvalidRange
	}
	return r.File.ReadAt(context.Background(), destination, r.Offset+offset)
}

type ReceivedFrame struct {
	Header      []byte
	Payload     io.ReadCloser
	PayloadSize int64
}

// Conn transports complete frames rather than exposing a byte stream. Header
// and payload framing is preserved. Send and Receive may be called
// concurrently, but each direction is serialized.
//
// Send returns once the frame is on its way, not once it has arrived: a frame
// sent just before the connection breaks may never be received. Nothing here
// checks the bytes a frame carries: the stream underneath may be plain TCP,
// whose own checksum is 16 bits, so a header or a payload that matters carries a
// checksum of its own, as the peer server's frames do.
type Conn interface {
	Send(context.Context, Frame) error
	Receive(context.Context) (ReceivedFrame, error)
	LocalAddress() Address
	RemoteAddress() Address
	Close() error
}
