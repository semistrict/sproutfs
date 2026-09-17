//go:build linux

package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// listen accepts on one AF_VSOCK port. The guest binds VMADDR_CID_ANY rather
// than its own context id: the id is the hypervisor's name for this guest, and
// a guest that insisted on knowing it would have to be told it again after
// every restore.
//
// Go's net package has no AF_VSOCK, so the socket is made by hand. It is
// non-blocking and handed to os.NewFile, which registers it with the runtime's
// poller, so accepting parks a goroutine rather than an OS thread and closing
// the listener wakes it.
func listen(port int) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	file := os.NewFile(uintptr(fd), "vsock")
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: uint32(port)}); err != nil {
		return nil, errors.Join(fmt.Errorf("binding vsock port %d: %w", port, err), file.Close())
	}
	if err := unix.Listen(fd, 32); err != nil {
		return nil, errors.Join(fmt.Errorf("listening on vsock port %d: %w", port, err), file.Close())
	}
	return &vsockListener{file: file, port: port}, nil
}

// vsockAddr names one end of a vsock stream. Nothing routes on it: it exists
// because net.Conn and net.Listener have to report an address.
type vsockAddr struct{ port int }

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("vsock:%d", a.port) }

// vsockListener is an AF_VSOCK listening socket as a net.Listener.
type vsockListener struct {
	file *os.File
	port int
}

func (l *vsockListener) Addr() net.Addr { return vsockAddr{port: l.port} }
func (l *vsockListener) Close() error   { return l.file.Close() }

func (l *vsockListener) Accept() (net.Conn, error) {
	raw, err := l.file.SyscallConn()
	if err != nil {
		return nil, err
	}
	var accepted int
	var acceptErr error
	// The callback returns false to park until the socket is readable again,
	// which is how the runtime's poller is asked to wait for a connection.
	err = raw.Read(func(fd uintptr) bool {
		accepted, _, acceptErr = unix.Accept4(int(fd), unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC)
		return !errors.Is(acceptErr, unix.EAGAIN)
	})
	if err != nil {
		return nil, err
	}
	if acceptErr != nil {
		return nil, acceptErr
	}
	return &vsockConn{file: os.NewFile(uintptr(accepted), "vsock-connection"), port: l.port}, nil
}

// vsockConn is one accepted stream as a net.Conn. os.File carries the deadlines
// an HTTP server sets, because the descriptor under it is pollable.
type vsockConn struct {
	file *os.File
	port int
}

func (c *vsockConn) Read(b []byte) (int, error)  { return c.file.Read(b) }
func (c *vsockConn) Write(b []byte) (int, error) { return c.file.Write(b) }
func (c *vsockConn) Close() error                { return c.file.Close() }
func (c *vsockConn) LocalAddr() net.Addr         { return vsockAddr{port: c.port} }
func (c *vsockConn) RemoteAddr() net.Addr        { return vsockAddr{} }

func (c *vsockConn) SetDeadline(t time.Time) error      { return c.file.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.file.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.file.SetWriteDeadline(t) }
