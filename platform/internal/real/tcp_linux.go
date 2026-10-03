package real

import (
	"net"

	"golang.org/x/sys/unix"
)

// tune sets TCP_USER_TIMEOUT on a TCP socket.
func tune(conn net.Conn) error {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return nil
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(userTimeout.Milliseconds()))
	}); err != nil {
		return err
	}
	return setErr
}

// userTimeoutOf reads a socket TCP_USER_TIMEOUT, in milliseconds.
func userTimeoutOf(conn net.Conn) (int, error) {
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var value int
	var getErr error
	if err := raw.Control(func(fd uintptr) {
		value, getErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); err != nil {
		return 0, err
	}
	return value, getErr
}
