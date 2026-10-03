package peer

import (
	"context"
	"errors"
	"fmt"
	"time"

	peerv1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/peer/v1"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/proto"
)

// The protocol versions this release speaks. Every release speaks its own
// version and the one before, because a rolling upgrade hands VMs from hosts of
// the old release to hosts of the new one and back: a drain of an old host
// moves its VMs onto new ones, and the new host fetches their pages from it.
//
// Version 1 is what the release before this one spoke, when the channel was the
// page server: no hello, one request at a time on a connection, and headers that
// need not carry a checksum. Version 2 opens every connection with a hello and
// checksums every header.
const (
	OldestVersion uint32 = 1
	NewestVersion uint32 = 2
)

// helloVersion is the wire version a hello and its reply are encoded at, whatever
// range the hello states. It never changes, so a server of a later release reads
// the hello of an earlier one and can answer that the two share no version.
const helloVersion = 2

// helloTimeout bounds how long a dialer waits for the answer to its hello. A
// server answers a hello as it reads it, so one that has not answered by then is
// a server that is not answering.
const helloTimeout = 10 * time.Second

// ErrIncompatible reports a peer that speaks no version of the protocol this
// host speaks. It is neither a peer that is down nor one that is gone: it is a
// host of another release, and asking it again changes nothing until one of the
// two is upgraded.
var ErrIncompatible = errors.New("peer: the peer speaks no version of the protocol this host speaks")

// IncompatibleError names the range the peer speaks.
type IncompatibleError struct {
	Min, Max uint32
}

func (e *IncompatibleError) Error() string {
	return fmt.Sprintf("%v: it speaks versions %d to %d, this host %d to %d", ErrIncompatible,
		e.Min, e.Max, OldestVersion, NewestVersion)
}

func (e *IncompatibleError) Unwrap() error { return ErrIncompatible }

// Versions is a range of protocol versions one end speaks. The zero value is
// this release's: OldestVersion to NewestVersion. A test that stands in for
// another release narrows it.
type Versions struct {
	Min, Max uint32
}

func (v Versions) orDefault() Versions {
	if v.Min == 0 && v.Max == 0 {
		return Versions{Min: OldestVersion, Max: NewestVersion}
	}
	return v
}

func (v Versions) valid() bool { return v.Min >= 1 && v.Min <= v.Max }

// shared is the highest version both ranges hold, and false where they hold
// none.
func (v Versions) shared(other Versions) (uint32, bool) {
	version := min(v.Max, other.Max)
	return version, version >= max(v.Min, other.Min) && version >= 1
}

// answerHello is the server's half of a hello: the version both ends will use,
// or INCOMPATIBLE with the server's own range.
func answerHello(speaks Versions, hello *peerv1.Hello) (*peerv1.HelloReply, uint32, bool) {
	asked := Versions{Min: hello.GetMinVersion(), Max: hello.GetMaxVersion()}
	version, ok := speaks.shared(asked)
	if !asked.valid() || !ok {
		status := peerv1.Status_STATUS_INCOMPATIBLE
		return peerv1.HelloReply_builder{Status: &status, MinVersion: proto.Uint32(speaks.Min),
			MaxVersion: proto.Uint32(speaks.Max)}.Build(), 0, false
	}
	status := peerv1.Status_STATUS_OK
	return peerv1.HelloReply_builder{Status: &status, Version: proto.Uint32(version),
		MinVersion: proto.Uint32(speaks.Min), MaxVersion: proto.Uint32(speaks.Max)}.Build(), version, true
}

// errNoHello reports a connection closed in answer to a hello: what a server of
// the release before this one does with a frame it cannot read. A server of
// this release or a later one always answers a hello.
var errNoHello = errors.New("peer: the connection closed unanswered after the hello")

// sayHello is the dialer's half: send the hello, read the answer, and report the
// version the connection speaks from here on.
func sayHello(ctx context.Context, conn platform.Conn, speaks Versions, class Class) (*peerv1.HelloReply, error) {
	ctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	wireClass := classToWire(class)
	frame, err := wire.Encode(wire.Outgoing{Version: helloVersion,
		Message: peerv1.Hello_builder{MinVersion: proto.Uint32(speaks.Min), MaxVersion: proto.Uint32(speaks.Max),
			Class: &wireClass}.Build()})
	if err != nil {
		return nil, err
	}
	if err := conn.Send(ctx, frame); err != nil {
		return nil, err
	}
	received, err := conn.Receive(ctx)
	if err != nil {
		if ctx.Err() == nil && (errors.Is(err, platform.ErrDisconnected) || errors.Is(err, platform.ErrClosed)) {
			return nil, errors.Join(errNoHello, err)
		}
		return nil, err
	}
	incoming, err := wire.Decode(received)
	if err != nil {
		return nil, err
	}
	payload, err := readPayload(incoming, 0)
	if err != nil {
		return nil, err
	}
	payload.release()
	reply := new(peerv1.HelloReply)
	if err := incoming.UnmarshalTo(reply); err != nil {
		return nil, errors.Join(wire.ErrMalformedFrame, err)
	}
	switch reply.GetStatus() {
	case peerv1.Status_STATUS_OK:
		version := reply.GetVersion()
		if version < speaks.Min || version > speaks.Max {
			return nil, fmt.Errorf("%w: the server chose version %d of a hello for %d to %d",
				wire.ErrMalformedFrame, version, speaks.Min, speaks.Max)
		}
		return reply, nil
	case peerv1.Status_STATUS_INCOMPATIBLE:
		return nil, &IncompatibleError{Min: reply.GetMinVersion(), Max: reply.GetMaxVersion()}
	default:
		return nil, fmt.Errorf("%w: a hello answered %s", wire.ErrMalformedFrame, reply.GetStatus())
	}
}
