package framer

import (
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// maxSendfile is the most one sendfile call is asked to send.
const maxSendfile = 1 << 30

// sendFile sends head and then size bytes of source from offset, with the file's
// bytes handed to the kernel rather than read into this process. It reads the
// file at an offset of its own, so concurrent sends of one file do not move a
// shared file position. The socket is corked around the two, so the head and
// the start of the file leave in full segments rather than the head alone.
//
// Anything but a TCP socket is errors.ErrUnsupported, and the caller copies.
func sendFile(stream net.Conn, head net.Buffers, source syscall.Conn, offset, size int64) (int64, error) {
	tcp, ok := stream.(*net.TCPConn)
	if !ok {
		return 0, errors.ErrUnsupported
	}
	file, err := source.SyscallConn()
	if err != nil {
		return 0, errors.ErrUnsupported
	}
	socket, err := tcp.SyscallConn()
	if err != nil {
		return 0, err
	}
	cork := func(on int) {
		_ = socket.Control(func(fd uintptr) {
			_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_CORK, on)
		})
	}
	cork(1)
	defer cork(0)
	if err := writeBuffers(tcp, head); err != nil {
		return 0, err
	}
	var sent int64
	var sendErr error
	controlErr := file.Control(func(fileFD uintptr) {
		// The file's descriptor stays valid for as long as this callback runs,
		// and the socket's write waits for the socket to take more whenever the
		// kernel says it is full, under the stream's write deadline.
		writeErr := socket.Write(func(socketFD uintptr) bool {
			for sent < size {
				at := offset + sent
				n, err := unix.Sendfile(int(socketFD), int(fileFD), &at, int(min(size-sent, maxSendfile)))
				if n > 0 {
					sent += int64(n)
				}
				switch {
				case errors.Is(err, unix.EAGAIN):
					return false
				case errors.Is(err, unix.EINTR):
					continue
				case err != nil:
					sendErr = fmt.Errorf("sendfile: %w", err)
					return true
				case n == 0:
					sendErr = fmt.Errorf("sendfile: the file ended %d bytes into a frame of %d: %w", sent, size, io.ErrUnexpectedEOF)
					return true
				}
			}
			return true
		})
		if sendErr == nil {
			sendErr = writeErr
		}
	})
	if sendErr == nil {
		sendErr = controlErr
	}
	return sent, sendErr
}
