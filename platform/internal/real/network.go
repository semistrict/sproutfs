package real

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

const (
	frameVersion    = uint16(1)
	framePrefixSize = 20
)

var frameMagic = [4]byte{'B', 'T', 'R', 'F'}

type NetworkConfig struct {
	// Transport carries the streams this network frames. Nil is plain TCP.
	Transport      platform.Transport
	MaxHeaderSize  uint32
	MaxPayloadSize uint64
}

// Network frames connections over a transport.
type Network struct {
	config NetworkConfig
}

func NewNetwork(config NetworkConfig) *Network {
	if config.Transport == nil {
		config.Transport = TCP{}
	}
	config.MaxHeaderSize = cmp.Or(config.MaxHeaderSize, 1<<20)
	config.MaxPayloadSize = cmp.Or(config.MaxPayloadSize, 1<<30)
	return &Network{config: config}
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

func (n *Network) Listen(address platform.Address) (platform.Listener, error) {
	base, err := n.config.Transport.Listen(address)
	if err != nil {
		return nil, err
	}
	l := &listener{
		base:     base,
		address:  platform.Address(base.Addr().String()),
		config:   n.config,
		accepted: make(chan accepted),
		closed:   make(chan struct{}),
	}
	go l.run()
	return l, nil
}

// Dial leaves the local end to the transport, so from is not used.
func (n *Network) Dial(ctx context.Context, _, to platform.Address) (platform.Conn, error) {
	connection, err := n.config.Transport.Dial(ctx, to)
	if err != nil {
		return nil, normalizeNetworkError(err)
	}
	return newFrameConn(connection, n.config), nil
}

// listener accepts on its own goroutine, one connection at a time, and hands
// each to the Accept waiting for it. A transport's listener need not take a
// deadline, so this is how an Accept follows its context.
type listener struct {
	base      net.Listener
	address   platform.Address
	config    NetworkConfig
	accepted  chan accepted
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// accepted is what one Accept of the transport's listener returned.
type accepted struct {
	connection net.Conn
	err        error
}

// run accepts until the listener is closed. A connection accepted after the
// close has no Accept to go to, so it is closed here.
func (l *listener) run() {
	for {
		connection, err := l.base.Accept()
		select {
		case l.accepted <- accepted{connection: connection, err: err}:
		case <-l.closed:
			if connection != nil {
				_ = connection.Close()
			}
			return
		}
		if errors.Is(err, net.ErrClosed) {
			return
		}
	}
}

func (l *listener) Accept(ctx context.Context) (platform.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-l.closed:
		return nil, normalizeNetworkError(net.ErrClosed)
	case next := <-l.accepted:
		if next.err != nil {
			return nil, normalizeNetworkError(next.err)
		}
		return newFrameConn(next.connection, l.config), nil
	}
}

func (l *listener) Address() platform.Address { return l.address }

func (l *listener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.closeErr = normalizeNetworkError(l.base.Close())
	})
	return l.closeErr
}

type frameConn struct {
	connection net.Conn
	config     NetworkConfig
	local      platform.Address
	remote     platform.Address
	done       chan struct{}
	closeOnce  sync.Once
	send       chan struct{}
	receive    chan struct{}
}

func newFrameConn(connection net.Conn, config NetworkConfig) *frameConn {
	c := &frameConn{
		connection: connection,
		config:     config,
		local:      platform.Address(connection.LocalAddr().String()),
		remote:     platform.Address(connection.RemoteAddr().String()),
		done:       make(chan struct{}),
		send:       make(chan struct{}, 1),
		receive:    make(chan struct{}, 1),
	}
	c.send <- struct{}{}
	c.receive <- struct{}{}
	return c
}

func (c *frameConn) Send(ctx context.Context, frame platform.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if uint64(len(frame.Header)) > uint64(c.config.MaxHeaderSize) || uint64(frame.PayloadSize) > c.config.MaxPayloadSize {
		return platform.ErrMessageTooLarge
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.done:
		return platform.ErrDisconnected
	case <-c.send:
	}
	defer func() { c.send <- struct{}{} }()

	prefix := make([]byte, framePrefixSize)
	copy(prefix[:4], frameMagic[:])
	binary.BigEndian.PutUint16(prefix[4:6], frameVersion)
	binary.BigEndian.PutUint32(prefix[8:12], uint32(len(frame.Header)))
	binary.BigEndian.PutUint64(prefix[12:20], uint64(frame.PayloadSize))
	err := withDeadline(ctx, c.connection.SetWriteDeadline, func() error {
		if err := writeFull(c.connection, prefix); err != nil {
			return err
		}
		if err := writeFull(c.connection, frame.Header); err != nil {
			return err
		}
		if frame.PayloadSize == 0 {
			return nil
		}
		written, err := io.CopyN(c.connection, io.NewSectionReader(frame.Payload, 0, frame.PayloadSize), frame.PayloadSize)
		if err != nil {
			return err
		}
		if written != frame.PayloadSize {
			return io.ErrShortWrite
		}
		return nil
	})
	return normalizeNetworkError(err)
}

func (c *frameConn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	select {
	case <-ctx.Done():
		return platform.ReceivedFrame{}, context.Cause(ctx)
	case <-c.done:
		return platform.ReceivedFrame{}, platform.ErrDisconnected
	case <-c.receive:
	}
	release := func() { c.receive <- struct{}{} }
	prefix := make([]byte, framePrefixSize)
	err := withDeadline(ctx, c.connection.SetReadDeadline, func() error {
		_, err := io.ReadFull(c.connection, prefix)
		return err
	})
	if err != nil {
		release()
		return platform.ReceivedFrame{}, normalizeNetworkError(err)
	}
	if !bytes.Equal(prefix[:4], frameMagic[:]) || binary.BigEndian.Uint16(prefix[4:6]) != frameVersion {
		release()
		_ = c.Close()
		return platform.ReceivedFrame{}, platform.ErrDisconnected
	}
	headerSize := binary.BigEndian.Uint32(prefix[8:12])
	payloadSize := binary.BigEndian.Uint64(prefix[12:20])
	if headerSize > c.config.MaxHeaderSize || payloadSize > c.config.MaxPayloadSize || payloadSize > math.MaxInt64 {
		release()
		_ = c.Close()
		return platform.ReceivedFrame{}, platform.ErrMessageTooLarge
	}
	header := make([]byte, int(headerSize))
	err = withDeadline(ctx, c.connection.SetReadDeadline, func() error {
		_, err := io.ReadFull(c.connection, header)
		return err
	})
	if err != nil {
		release()
		return platform.ReceivedFrame{}, normalizeNetworkError(err)
	}
	if payloadSize == 0 {
		release()
		return platform.ReceivedFrame{Header: header, Payload: io.NopCloser(bytes.NewReader(nil))}, nil
	}
	body := &frameBody{
		ctx:        ctx,
		connection: c.connection,
		remaining:  int64(payloadSize),
		release:    release,
	}
	return platform.ReceivedFrame{Header: header, Payload: body, PayloadSize: int64(payloadSize)}, nil
}

func (c *frameConn) LocalAddress() platform.Address  { return c.local }
func (c *frameConn) RemoteAddress() platform.Address { return c.remote }

func (c *frameConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.connection.Close()
	})
	// A peer that is already gone fails the close with a broken pipe or a
	// reset; the socket is closed regardless, which is all a caller closing a
	// connection needs.
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return nil
	}
	return normalizeNetworkError(err)
}

type frameBody struct {
	ctx        context.Context
	connection net.Conn
	remaining  int64
	release    func()
	once       sync.Once
}

func (b *frameBody) Read(destination []byte) (int, error) {
	if b.remaining == 0 {
		b.finish()
		return 0, io.EOF
	}
	if int64(len(destination)) > b.remaining {
		destination = destination[:b.remaining]
	}
	var n int
	err := withDeadline(b.ctx, b.connection.SetReadDeadline, func() error {
		var readErr error
		n, readErr = b.connection.Read(destination)
		return readErr
	})
	b.remaining -= int64(n)
	if b.remaining == 0 {
		b.finish()
		if err == nil {
			return n, io.EOF
		}
		if errors.Is(err, io.EOF) {
			return n, io.EOF
		}
	}
	return n, normalizeNetworkError(err)
}

func (b *frameBody) Close() error {
	if b.remaining > 0 {
		var copied int64
		err := withDeadline(b.ctx, b.connection.SetReadDeadline, func() error {
			var copyErr error
			copied, copyErr = io.CopyN(io.Discard, b.connection, b.remaining)
			return copyErr
		})
		b.remaining -= copied
		if err != nil {
			_ = b.connection.Close()
			b.finish()
			return normalizeNetworkError(err)
		}
		b.remaining = 0
	}
	b.finish()
	return nil
}

func (b *frameBody) finish() { b.once.Do(b.release) }

func writeFull(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		n, err := writer.Write(value)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		value = value[n:]
	}
	return nil
}

func withDeadline(ctx context.Context, setDeadline func(time.Time) error, operation func() error) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := setDeadline(deadline); err != nil {
			return err
		}
	}
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = setDeadline(time.Now())
		close(callbackDone)
	})
	err := operation()
	if !stop() {
		<-callbackDone
	}
	_ = setDeadline(time.Time{})
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

func normalizeNetworkError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) {
		return errors.Join(platform.ErrDisconnected, err)
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return errors.Join(platform.ErrUnavailable, err)
	}
	return err
}

var _ platform.Network = (*Network)(nil)
var _ platform.Listener = (*listener)(nil)
var _ platform.Conn = (*frameConn)(nil)
