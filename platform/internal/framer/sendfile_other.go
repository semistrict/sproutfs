//go:build !linux

package framer

import (
	"errors"
	"net"
	"syscall"
)

// sendFile is Linux's alone. Elsewhere a file range is read and copied.
func sendFile(net.Conn, net.Buffers, syscall.Conn, int64, int64) (int64, error) {
	return 0, errors.ErrUnsupported
}
