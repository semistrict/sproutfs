//go:build !linux

package real

import "net"

// tune does nothing off Linux, which has no TCP_USER_TIMEOUT; keepalive is
// set on every system.
func tune(net.Conn) error { return nil }
