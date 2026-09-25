// Package guest is the channel between a host and the agent inside one of
// its guests: the JSON the agent serves, and the dialer that reaches it.
//
// The channel is the VM's virtio-vsock device. Firecracker listens on a Unix
// socket for the host end of it, and forwards a connection to a guest port when
// the connecting side asks for one, so reaching a guest is connecting to that
// socket, naming the port, and speaking HTTP over what comes back. The device
// belongs to the VMM state a restore replays and the socket to whichever
// process runs the machine, which is why a fork and a migrated VM are reachable
// without anything being carried between hosts.
package guest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// Port is the guest vsock port the agent serves on. It is the guest's own
// number for the channel and collides with nothing on the host.
const Port = 80

// MaxOutputBytes bounds what one command's stdout or stderr returns. A demo
// reads a line or two; a command that prints a filesystem is truncated rather
// than carried through two proxies.
const MaxOutputBytes = 1 << 20

// ExecRequest runs one shell command in the guest. Timeout is in seconds and
// zero takes the agent's default.
type ExecRequest struct {
	Cmd     string  `json:"cmd"`
	Timeout float64 `json:"timeout,omitempty"`
}

// ExecResult is what the command did. Exit is the shell's status, which is
// 124 for a command the timeout killed, and Stdout and Stderr are truncated to
// MaxOutputBytes each.
type ExecResult struct {
	Exit      int     `json:"exit"`
	Stdout    string  `json:"stdout"`
	Stderr    string  `json:"stderr"`
	Seconds   float64 `json:"seconds"`
	Truncated bool    `json:"truncated,omitempty"`
}

// ErrNoGuest reports a VM whose guest is not answering on the channel: either
// nothing is listening on the port yet, which is a guest that has not finished
// booting, or the machine has no vsock at all.
var ErrNoGuest = errors.New("the guest is not answering")

// handshakeTimeout bounds the connection forwarding request. It is the VMM
// answering out of its own event loop, not the guest doing any work.
const handshakeTimeout = 10 * time.Second

// Dial reaches one guest port over the Unix socket Firecracker serves a VM's
// vsock on. It performs the connection forwarding request the vsock protocol
// defines — "CONNECT <port>\n", answered with "OK <port>\n" — so what it
// returns is a stream to the software listening on that port in the guest.
func Dial(ctx context.Context, socket string, port int) (net.Conn, error) {
	if socket == "" {
		return nil, fmt.Errorf("%w: the machine has no vsock", ErrNoGuest)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, errors.Join(ErrNoGuest, err)
	}
	deadline := time.Now().Add(handshakeTimeout)
	if from, ok := ctx.Deadline(); ok && from.Before(deadline) {
		deadline = from
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return nil, errors.Join(ErrNoGuest, err, conn.Close())
	}
	// The acknowledgement is read one byte at a time: everything after its
	// newline is the guest's own, and a buffered reader would swallow it.
	reply, err := readLine(conn)
	if err != nil {
		return nil, errors.Join(ErrNoGuest, err, conn.Close())
	}
	if !strings.HasPrefix(reply, "OK ") {
		return nil, errors.Join(fmt.Errorf("%w: the VMM answered %q, want an OK", ErrNoGuest, reply), conn.Close())
	}
	// The handshake's deadline is not the stream's: an exec runs for as long as
	// its own timeout allows.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, errors.Join(err, conn.Close())
	}
	return conn, nil
}

// readLine reads up to and including one newline, which is the whole of what
// the VMM writes before the stream becomes the guest's.
func readLine(conn net.Conn) (string, error) {
	var line strings.Builder
	one := make([]byte, 1)
	for line.Len() < 64 {
		if _, err := conn.Read(one); err != nil {
			return line.String(), err
		}
		if one[0] == '\n' {
			return line.String(), nil
		}
		line.WriteByte(one[0])
	}
	return line.String(), errors.New("the VMM's reply has no newline in it")
}

// NewClient is an HTTP client whose every connection is a fresh forwarded vsock
// stream to the agent in one guest. Connections are not reused: the forwarding
// request is per-connection, so a pooled one would carry another VM's stream
// after the socket path changed under a migration.
func NewClient(socket string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return Dial(ctx, socket, Port)
			},
		},
	}
}

// URL addresses the agent. The host in it is a name the dialer ignores — every
// connection goes to the one socket it was built with — and is there because
// an HTTP request needs one.
func URL(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "http://guest" + path
}
