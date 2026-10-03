package framer_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/framer"
)

// fragmenting is a stream that delivers what is written to it in pieces of a
// few bytes, as a stream that splits writes into short segments does: every
// read the framer makes may return part of what one write sent.
type fragmenting struct {
	net.Conn
	piece  int
	writes atomic.Int64
}

func (f *fragmenting) Write(b []byte) (int, error) {
	f.writes.Add(1)
	written := 0
	for written < len(b) {
		end := min(len(b), written+f.piece)
		n, err := f.Conn.Write(b[written:end])
		written += n
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// pipe is two framed ends of one in-memory stream whose writes arrive in
// pieces of piece bytes.
func pipe(t *testing.T, piece int) (platform.Conn, platform.Conn, *fragmenting) {
	t.Helper()
	a, b := net.Pipe()
	sender := &fragmenting{Conn: a, piece: piece}
	client := framer.NewConn(sender, framer.Config{})
	server := framer.NewConn(b, framer.Config{})
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server, sender
}

// memoryFile is a platform.File over a byte slice that counts its reads.
type memoryFile struct {
	data  []byte
	reads atomic.Int64
	// fail, when set, fails every read from that offset on.
	fail int64
}

func (f *memoryFile) ReadAt(_ context.Context, destination []byte, offset int64) (int, error) {
	f.reads.Add(1)
	if f.fail > 0 && offset >= f.fail {
		return 0, errors.New("the disk failed")
	}
	if offset >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(destination, f.data[offset:])
	if n < len(destination) {
		return n, io.EOF
	}
	return n, nil
}
func (f *memoryFile) WriteAt(context.Context, []byte, int64) (int, error) {
	return 0, errors.ErrUnsupported
}
func (f *memoryFile) Truncate(context.Context, int64) error { return errors.ErrUnsupported }
func (f *memoryFile) Sync(context.Context) error            { return nil }
func (f *memoryFile) Size(context.Context) (int64, error)   { return int64(len(f.data)), nil }
func (f *memoryFile) Close() error                          { return nil }

// patterned is n bytes no two runs of which look alike.
func patterned(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7 + i/251)
	}
	return out
}

// receiveAll reads one frame whole.
func receiveAll(t *testing.T, conn platform.Conn) (string, []byte) {
	t.Helper()
	frame, err := conn.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer frame.Payload.Close()
	payload := make([]byte, frame.PayloadSize)
	if _, err := io.ReadFull(frame.Payload, payload); err != nil {
		t.Fatal(err)
	}
	return string(frame.Header), payload
}

// Every kind of payload crosses a stream that delivers its bytes a few at a
// time, and each frame arrives whole and in order.
func TestFramesCrossAFragmentedStreamWhole(t *testing.T) {
	t.Parallel()
	for _, piece := range []int{1, 3, 999} {
		client, server, _ := pipe(t, piece)
		inMemory := patterned(5000)
		file := &memoryFile{data: patterned(700_000)}
		other := patterned(300_000)
		frames := []platform.Frame{
			{Header: []byte("empty")},
			{Header: []byte("bytes"), Payload: platform.Bytes(inMemory), PayloadSize: int64(len(inMemory))},
			{Header: []byte("file"), Payload: platform.FileRange{File: file, Offset: 1000}, PayloadSize: 600_000},
			{Header: []byte("reader"), Payload: bytes.NewReader(other), PayloadSize: int64(len(other))},
		}
		want := [][]byte{nil, inMemory, file.data[1000:601_000], other}
		sent := make(chan error, 1)
		go func() {
			for _, frame := range frames {
				if err := client.Send(t.Context(), frame); err != nil {
					sent <- err
					return
				}
			}
			sent <- nil
		}()
		for index, frame := range frames {
			header, payload := receiveAll(t, server)
			if header != string(frame.Header) || !bytes.Equal(payload, want[index]) {
				t.Fatalf("piece %d: frame %d arrived as %q with %d bytes, want %q with %d",
					piece, index, header, len(payload), frame.Header, len(want[index]))
			}
		}
		if err := <-sent; err != nil {
			t.Fatal(err)
		}
	}
}

// A frame whose payload is in memory leaves in one write of the stream, prefix,
// header and payload together.
func TestAFrameInMemoryIsOneWrite(t *testing.T) {
	t.Parallel()
	client, server, stream := pipe(t, 1<<20)
	payload := patterned(4096)
	received := make(chan struct{})
	go func() {
		defer close(received)
		for range 3 {
			_, _ = receiveAll(t, server)
		}
	}()
	defer func() { <-received }()
	for range 3 {
		if err := client.Send(t.Context(), platform.Frame{Header: []byte("page"), Payload: platform.Bytes(payload),
			PayloadSize: int64(len(payload))}); err != nil {
			t.Fatal(err)
		}
	}
	if got := stream.writes.Load(); got != 3 {
		t.Fatalf("three frames took %d writes, want 3", got)
	}
}

// A frame is capped at platform.MaxFrameBytes of payload: a sender refuses a
// larger one, and a receiver told of a larger one drops the connection rather
// than allocate what a bad header says.
func TestFramesAreCappedAtSixteenMiB(t *testing.T) {
	t.Parallel()
	client, server, _ := pipe(t, 1<<20)
	big := platform.Frame{Header: []byte("too big"), Payload: platform.Bytes(make([]byte, platform.MaxFrameBytes+1)),
		PayloadSize: platform.MaxFrameBytes + 1}
	if err := client.Send(t.Context(), big); !errors.Is(err, platform.ErrMessageTooLarge) {
		t.Fatalf("sending a frame of %d bytes = %v, want ErrMessageTooLarge", big.PayloadSize, err)
	}

	raw, receiving := net.Pipe()
	defer raw.Close()
	conn := framer.NewConn(receiving, framer.Config{})
	defer conn.Close()
	prefix := make([]byte, framer.PrefixSize)
	copy(prefix, "BTRF")
	binary.BigEndian.PutUint16(prefix[4:6], 1)
	binary.BigEndian.PutUint32(prefix[8:12], 4)
	binary.BigEndian.PutUint64(prefix[12:20], platform.MaxFrameBytes+1)
	go func() { _, _ = raw.Write(prefix) }()
	if _, err := conn.Receive(t.Context()); !errors.Is(err, platform.ErrMessageTooLarge) {
		t.Fatalf("receiving a frame of %d bytes = %v, want ErrMessageTooLarge", platform.MaxFrameBytes+1, err)
	}
	if _, err := conn.Receive(t.Context()); !errors.Is(err, platform.ErrDisconnected) {
		t.Fatalf("receiving after the refusal = %v, want ErrDisconnected", err)
	}
	_ = server
}

// A send that fails part way through a frame leaves the stream with half a
// frame on it, so the connection is closed rather than read from inside it.
func TestASendThatFailsMidFrameClosesTheConnection(t *testing.T) {
	t.Parallel()
	client, server, _ := pipe(t, 1<<20)
	go func() {
		for {
			frame, err := server.Receive(context.Background())
			if err != nil {
				return
			}
			_ = frame.Payload.Close()
		}
	}()
	file := &memoryFile{data: patterned(1 << 20), fail: 256 << 10}
	err := client.Send(t.Context(), platform.Frame{Header: []byte("file"), Payload: platform.FileRange{File: file},
		PayloadSize: 1 << 20})
	if err == nil || errors.Is(err, platform.ErrDisconnected) {
		t.Fatalf("a send whose file failed = %v, want the file's failure", err)
	}
	if err := client.Send(t.Context(), platform.Frame{Header: []byte("next")}); !errors.Is(err, platform.ErrDisconnected) {
		t.Fatalf("the next send = %v, want ErrDisconnected", err)
	}
}

// osFile is a platform.File over a real file that counts the reads that reach
// this process, and hands its descriptor to a transport that asks.
type osFile struct {
	*os.File
	mu    sync.Mutex
	reads int
}

func (f *osFile) ReadAt(_ context.Context, destination []byte, offset int64) (int, error) {
	f.mu.Lock()
	f.reads++
	f.mu.Unlock()
	return f.File.ReadAt(destination, offset)
}
func (f *osFile) WriteAt(_ context.Context, source []byte, offset int64) (int, error) {
	return f.File.WriteAt(source, offset)
}
func (f *osFile) Truncate(_ context.Context, size int64) error { return f.File.Truncate(size) }
func (f *osFile) Sync(context.Context) error                   { return f.File.Sync() }
func (f *osFile) Size(context.Context) (int64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
func (f *osFile) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

var _ syscall.Conn = (*osFile)(nil)

// tcpPair is two framed ends of one loopback TCP connection.
func tcpPair(t *testing.T) (platform.Conn, platform.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			t.Error(err)
		}
		accepted <- conn
	}()
	dialed, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(t.Context(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := framer.NewConn(dialed, framer.Config{})
	server := framer.NewConn(<-accepted, framer.Config{})
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
	})
	return client, server
}

// sendFileRange sends size bytes of a real file from offset over loopback TCP
// and reports the reads of the file that reached this process.
func sendFileRange(t *testing.T, size, offset int) int {
	t.Helper()
	path := t.TempDir() + "/stripes"
	contents := patterned(offset + size)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	handle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	file := &osFile{File: handle}
	client, server := tcpPair(t)
	sent := make(chan error, 1)
	go func() {
		sent <- client.Send(t.Context(), platform.Frame{Header: []byte("stripes"),
			Payload: platform.FileRange{File: file, Offset: int64(offset)}, PayloadSize: int64(size)})
	}()
	header, payload := receiveAll(t, server)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if header != "stripes" || !bytes.Equal(payload, contents[offset:]) {
		t.Fatalf("the file range arrived as %q with %d bytes, want %d bytes of the file from %d",
			header, len(payload), size, offset)
	}
	return file.readCount()
}
