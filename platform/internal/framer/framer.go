// Package framer turns byte streams into the framed connections of
// platform.Network. The TCP adapter frames real sockets with it, and the
// simulated network frames its simulated streams with the same code, so the
// framing a deployment runs is the framing a simulation fragments and corrupts.
//
// A frame is a 20-byte prefix — magic, version, header length and payload
// length — then the header, then the payload. The prefix has not changed since
// the first release, so a host of this release frames bytes a host of the one
// before reads.
//
// A frame goes out in one vectored write when its payload is in memory, and a
// payload that lies in a file goes to the kernel whole where the stream can
// take it (sendfile). A received payload is read straight off the stream by
// whoever reads the frame, into a buffer of the length the prefix gave.
package framer

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

const (
	frameVersion = uint16(1)
	// PrefixSize is the fixed part in front of every frame's header.
	PrefixSize = 20
	// copyChunk is how much of a payload that is neither in memory nor a file
	// the kernel can send is read at a time.
	copyChunk = 256 << 10
	// readBuffer is the stream reader's buffer. A prefix and a small header
	// arrive in one read; a payload larger than this is read straight into the
	// buffer its reader gives.
	readBuffer = 64 << 10
)

var frameMagic = [4]byte{'B', 'T', 'R', 'F'}

// Config bounds what a connection sends and accepts.
type Config struct {
	// Transport carries the streams a Network frames.
	Transport platform.Transport
	// MaxHeaderSize and MaxPayloadSize bound one frame. Zero selects 1 MiB of
	// header and platform.MaxFrameBytes of payload.
	MaxHeaderSize  uint32
	MaxPayloadSize uint64
}

func (c Config) withDefaults() Config {
	c.MaxHeaderSize = cmp.Or(c.MaxHeaderSize, 1<<20)
	c.MaxPayloadSize = cmp.Or(c.MaxPayloadSize, platform.MaxFrameBytes)
	return c
}

// Network frames the connections of a transport.
type Network struct {
	config Config
}

// New frames config.Transport's streams.
func New(config Config) *Network {
	return &Network{config: config.withDefaults()}
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
		return nil, Normalize(err)
	}
	return NewConn(connection, n.config), nil
}

// listener accepts on its own goroutine, one connection at a time, and hands
// each to the Accept waiting for it. A transport's listener need not take a
// deadline, so this is how an Accept follows its context.
type listener struct {
	base      net.Listener
	address   platform.Address
	config    Config
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
		return nil, Normalize(net.ErrClosed)
	case next := <-l.accepted:
		if next.err != nil {
			return nil, Normalize(next.err)
		}
		return NewConn(next.connection, l.config), nil
	}
}

func (l *listener) Address() platform.Address { return l.address }

func (l *listener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.closeErr = Normalize(l.base.Close())
	})
	return l.closeErr
}

// conn frames one stream. Each direction is serialized by a channel rather
// than a mutex, so a sender waiting on another is a wait a simulation can see.
type conn struct {
	stream    net.Conn
	reader    *bufio.Reader
	config    Config
	local     platform.Address
	remote    platform.Address
	done      chan struct{}
	closeOnce sync.Once
	send      chan struct{}
	receive   chan struct{}
	// prefix is the sender's, read the receiver's: each direction reuses its
	// own rather than allocating one a frame.
	prefix [PrefixSize]byte
	read   [PrefixSize]byte
}

// NewConn frames stream. It is what a Network's Dial and Accept return, and
// what a simulation frames its own streams with.
func NewConn(stream net.Conn, config Config) platform.Conn {
	config = config.withDefaults()
	c := &conn{
		stream:  stream,
		reader:  bufio.NewReaderSize(stream, readBuffer),
		config:  config,
		local:   platform.Address(stream.LocalAddr().String()),
		remote:  platform.Address(stream.RemoteAddr().String()),
		done:    make(chan struct{}),
		send:    make(chan struct{}, 1),
		receive: make(chan struct{}, 1),
	}
	c.send <- struct{}{}
	c.receive <- struct{}{}
	return c
}

func (c *conn) Send(ctx context.Context, frame platform.Frame) error {
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

	prefix := c.prefix[:]
	copy(prefix[:4], frameMagic[:])
	binary.BigEndian.PutUint16(prefix[4:6], frameVersion)
	binary.BigEndian.PutUint16(prefix[6:8], 0)
	binary.BigEndian.PutUint32(prefix[8:12], uint32(len(frame.Header)))
	binary.BigEndian.PutUint64(prefix[12:20], uint64(frame.PayloadSize))
	started := false
	err := withDeadline(ctx, c.stream.SetWriteDeadline, func() error {
		started = true
		return c.write(ctx, prefix, frame)
	})
	if err != nil && started {
		// Some of the frame may be on the stream already, and the next frame
		// would be read from inside it: the connection cannot carry another.
		_ = c.Close()
	}
	return Normalize(err)
}

// write sends one frame. A payload in memory goes in the same vectored write as
// the prefix and the header. A file range goes to the kernel behind them where
// the stream is a socket it can send a file to. Anything else is read in chunks,
// the first of them sent with the prefix and the header.
func (c *conn) write(ctx context.Context, prefix []byte, frame platform.Frame) error {
	size := frame.PayloadSize
	if size == 0 {
		return writeBuffers(c.stream, net.Buffers{prefix, frame.Header})
	}
	switch payload := frame.Payload.(type) {
	case platform.Bytes:
		if int64(len(payload)) < size {
			return platform.ErrInvalidRange
		}
		return writeBuffers(c.stream, net.Buffers{prefix, frame.Header, payload[:size]})
	case platform.FileRange:
		if source, ok := payload.File.(syscall.Conn); ok {
			if sent, err := sendFile(c.stream, net.Buffers{prefix, frame.Header}, source, payload.Offset, size); !errors.Is(err, errors.ErrUnsupported) {
				if err == nil && sent != size {
					return io.ErrShortWrite
				}
				return err
			}
		}
		return c.copyPayload(net.Buffers{prefix, frame.Header}, size, func(chunk []byte, offset int64) (int, error) {
			return payload.File.ReadAt(ctx, chunk, payload.Offset+offset)
		})
	default:
		return c.copyPayload(net.Buffers{prefix, frame.Header}, size, func(chunk []byte, offset int64) (int, error) {
			return payload.ReadAt(chunk, offset)
		})
	}
}

// copyPayload reads a payload a chunk at a time into a pooled buffer and sends
// each chunk, the first behind the prefix and header in one write.
func (c *conn) copyPayload(head net.Buffers, size int64, read func([]byte, int64) (int, error)) error {
	buffer := chunks.Get().(*[]byte)
	defer chunks.Put(buffer)
	for offset := int64(0); offset < size; {
		chunk := (*buffer)[:min(int64(len(*buffer)), size-offset)]
		n, err := read(chunk, offset)
		if n < len(chunk) {
			if err == nil || errors.Is(err, io.EOF) {
				err = fmt.Errorf("%w: the payload ended %d bytes into a frame of %d", platform.ErrInvalidRange, offset+int64(n), size)
			}
			return err
		}
		out := append(head, chunk)
		head = nil
		if err := writeBuffers(c.stream, out); err != nil {
			return err
		}
		offset += int64(n)
	}
	return nil
}

var chunks = sync.Pool{New: func() any {
	buffer := make([]byte, copyChunk)
	return &buffer
}}

// writeBuffers writes every buffer in one write: a writev where the stream is a
// socket, and one buffer gathered from a pool otherwise, so a stream that frames
// its own writes — TLS makes a record of each — sees one write a frame.
func writeBuffers(stream net.Conn, buffers net.Buffers) error {
	var total int64
	for _, buffer := range buffers {
		total += int64(len(buffer))
	}
	if _, socket := stream.(*net.TCPConn); !socket && total <= copyChunk {
		gathered := chunks.Get().(*[]byte)
		defer chunks.Put(gathered)
		joined := (*gathered)[:0]
		for _, buffer := range buffers {
			joined = append(joined, buffer...)
		}
		buffers = net.Buffers{joined}
	}
	written, err := buffers.WriteTo(stream)
	if err != nil {
		return err
	}
	if written != total {
		return io.ErrShortWrite
	}
	return nil
}

func (c *conn) Receive(ctx context.Context) (platform.ReceivedFrame, error) {
	select {
	case <-ctx.Done():
		return platform.ReceivedFrame{}, context.Cause(ctx)
	case <-c.done:
		return platform.ReceivedFrame{}, platform.ErrDisconnected
	case <-c.receive:
	}
	release := func() { c.receive <- struct{}{} }
	prefix := c.read[:]
	err := withDeadline(ctx, c.stream.SetReadDeadline, func() error {
		_, err := io.ReadFull(c.reader, prefix)
		return err
	})
	if err != nil {
		release()
		return platform.ReceivedFrame{}, Normalize(err)
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
	err = withDeadline(ctx, c.stream.SetReadDeadline, func() error {
		_, err := io.ReadFull(c.reader, header)
		return err
	})
	if err != nil {
		release()
		return platform.ReceivedFrame{}, Normalize(err)
	}
	if payloadSize == 0 {
		release()
		return platform.ReceivedFrame{Header: header, Payload: io.NopCloser(bytes.NewReader(nil))}, nil
	}
	body := &frameBody{
		ctx:       ctx,
		conn:      c,
		remaining: int64(payloadSize),
		release:   release,
	}
	return platform.ReceivedFrame{Header: header, Payload: body, PayloadSize: int64(payloadSize)}, nil
}

func (c *conn) LocalAddress() platform.Address  { return c.local }
func (c *conn) RemoteAddress() platform.Address { return c.remote }

func (c *conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		err = c.stream.Close()
	})
	// A peer that is already gone fails the close with a broken pipe or a
	// reset; the socket is closed regardless, which is all a caller closing a
	// connection needs.
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return nil
	}
	return Normalize(err)
}

// frameBody is one frame's payload, read off the stream by whoever reads the
// frame. The receive direction is held until it is read or closed, because the
// next frame's prefix is behind it.
type frameBody struct {
	ctx       context.Context
	conn      *conn
	remaining int64
	release   func()
	once      sync.Once
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
	err := withDeadline(b.ctx, b.conn.stream.SetReadDeadline, func() error {
		var readErr error
		n, readErr = b.conn.reader.Read(destination)
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
	return n, Normalize(err)
}

func (b *frameBody) Close() error {
	if b.remaining > 0 {
		var discarded int
		err := withDeadline(b.ctx, b.conn.stream.SetReadDeadline, func() error {
			var discardErr error
			discarded, discardErr = b.conn.reader.Discard(int(min(b.remaining, math.MaxInt)))
			return discardErr
		})
		b.remaining -= int64(discarded)
		if err != nil {
			_ = b.conn.Close()
			b.finish()
			return Normalize(err)
		}
	}
	b.finish()
	return nil
}

func (b *frameBody) finish() { b.once.Do(b.release) }

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
		_ = setDeadline(time.Unix(1, 0))
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

// Normalize maps a stream's error onto the platform's: a closed stream is a
// disconnect, and a timeout is a peer that is unavailable.
func Normalize(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
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
var _ platform.Conn = (*conn)(nil)
