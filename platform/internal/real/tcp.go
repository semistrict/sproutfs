package real

import (
	"context"
	"net"
	"time"

	"github.com/semistrict/sproutfs/platform"
)

// The kernel's own liveness for a host's sockets. The peer server pings a quiet
// connection and closes one that hears nothing for a few seconds; these bound
// what the kernel does underneath that, for a socket whose peer has vanished
// without a reset: keepalive probes an idle socket, and TCP_USER_TIMEOUT drops a
// socket whose sent bytes stay unacknowledged.
const (
	keepAliveIdle     = 5 * time.Second
	keepAliveInterval = 2 * time.Second
	keepAliveCount    = 3
	// userTimeout is how long sent bytes may stay unacknowledged before the
	// kernel drops the socket. Linux alone has it.
	userTimeout = 10 * time.Second
)

var keepAlive = net.KeepAliveConfig{Enable: true, Idle: keepAliveIdle, Interval: keepAliveInterval,
	Count: keepAliveCount}

// TCP is plain TCP. It authenticates nothing: every peer that reaches a
// listener is accepted, and a dial believes whatever answers at the address.
type TCP struct{}

func (TCP) Listen(address platform.Address) (net.Listener, error) {
	listener, err := (&net.ListenConfig{KeepAliveConfig: keepAlive}).Listen(context.Background(), "tcp", string(address))
	if err != nil {
		return nil, err
	}
	return tunedListener{listener}, nil
}

func (TCP) Dial(ctx context.Context, address platform.Address) (net.Conn, error) {
	conn, err := (&net.Dialer{KeepAliveConfig: keepAlive}).DialContext(ctx, "tcp", string(address))
	if err != nil {
		return nil, err
	}
	if err := tune(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// tunedListener sets the kernel's liveness on every socket it accepts.
type tunedListener struct{ net.Listener }

func (l tunedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if err := tune(conn); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}
