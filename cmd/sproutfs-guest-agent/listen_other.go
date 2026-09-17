//go:build !linux

package main

import (
	"errors"
	"net"
)

// listen has nothing to listen on: AF_VSOCK is a Linux socket family, and the
// agent runs in a Linux guest. The handlers are portable and tested anywhere,
// which is why this file exists rather than a build tag over the whole command.
func listen(int) (net.Listener, error) {
	return nil, errors.New("the guest agent runs on Linux: this platform has no AF_VSOCK")
}
