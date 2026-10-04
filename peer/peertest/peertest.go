// Package peertest reads and rewrites peer-server frames for tests that stand
// between a host and the wire: a reply held back, a reply turned into BUSY, a
// reply whose bytes are damaged on the way. The frame format is the peer
// package's own business, so this is the one place outside it that knows it.
package peertest

import (
	"bytes"
	"context"
	"hash/crc32"
	"io"

	migratev1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/migrate/v1"
	"github.com/semistrict/sproutfs/peer/internal/previous"
	"github.com/semistrict/sproutfs/peer/internal/wire"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/proto"
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Frame is one frame a test intercepted, with its payload read.
type Frame struct {
	header   []byte
	incoming wire.Incoming
	payload  []byte
}

// Read decodes one received frame and reads its payload, checking the
// payload's own checksum where it has one.
func Read(frame platform.ReceivedFrame) (*Frame, error) {
	incoming, err := wire.Decode(frame)
	if err != nil {
		return nil, err
	}
	defer incoming.Payload.Close()
	payload, err := io.ReadAll(incoming.Payload)
	if err != nil {
		return nil, err
	}
	return &Frame{header: frame.Header, incoming: incoming, payload: payload}, nil
}

// IsPageReply reports a frame this host is about to send that answers a page
// request, which is what a test that holds or drops a source's replies holds or
// drops: a hello's answer, a listing or a claim's is something else.
func IsPageReply(frame platform.Frame) bool { return answers(frame, new(migratev1.PageResponse)) }

// IsListingReply reports a frame this host is about to send that answers a
// listing of the pages it holds.
func IsListingReply(frame platform.Frame) bool {
	return answers(frame, new(migratev1.ResidentResponse))
}

// IsClaimReply reports a frame this host is about to send that answers a fork
// child's claim of its hold.
func IsClaimReply(frame platform.Frame) bool { return answers(frame, new(migratev1.ClaimResponse)) }

// answers reports a frame about to be sent whose message is of response's kind.
func answers(frame platform.Frame, response proto.Message) bool {
	incoming, err := wire.Decode(platform.ReceivedFrame{Header: frame.Header,
		Payload: io.NopCloser(bytes.NewReader(nil)), PayloadSize: frame.PayloadSize})
	if err != nil {
		return false
	}
	return incoming.Message.MessageIs(response)
}

// Checksummed says the frame's payload carried a checksum of its own in the
// header.
func (f *Frame) Checksummed() bool { return f.incoming.Checksummed }

// Pass is the frame as it arrived.
func (f *Frame) Pass() platform.ReceivedFrame {
	return platform.ReceivedFrame{Header: f.header, Payload: io.NopCloser(bytes.NewReader(f.payload)),
		PayloadSize: int64(len(f.payload))}
}

// PageReply is a reply to a page request, as a test may change it.
type PageReply struct {
	// Present and Dirty are the bitmaps, PayloadFormat the payload's encoding
	// and Payload its bytes.
	Present, Dirty []byte
	PayloadFormat  uint32
	Payload        []byte
	response       *migratev1.PageResponse
}

// PageReply reports the frame as a reply to a page request, if it is one.
func (f *Frame) PageReply() (*PageReply, bool) {
	response := new(migratev1.PageResponse)
	if !f.incoming.Message.MessageIs(response) || f.incoming.UnmarshalTo(response) != nil {
		return nil, false
	}
	return &PageReply{Present: response.GetPresent(), Dirty: response.GetDirty(),
		PayloadFormat: response.GetPayloadFormat(), Payload: f.payload, response: response}, true
}

// IsResidentReply reports a reply to a resident listing.
func (f *Frame) IsResidentReply() bool {
	return f.incoming.Message.MessageIs(new(migratev1.ResidentResponse))
}

// Rewrite is the frame with reply's fields in place of what it carried, and a
// payload checksum computed over the new payload, so what the change reaches is
// what reads the payload rather than the transport's check.
func (f *Frame) Rewrite(reply *PageReply) (platform.ReceivedFrame, error) {
	response := proto.CloneOf(reply.response)
	response.SetPresent(reply.Present)
	response.SetDirty(reply.Dirty)
	response.SetPayloadFormat(reply.PayloadFormat)
	return f.encode(response, reply.Payload)
}

// Busy is a reply that says the server is at its budget for this peer, in place
// of the page reply this frame carried.
func (f *Frame) Busy(pageSize int) (platform.ReceivedFrame, error) {
	status := migratev1.Status_STATUS_BUSY
	return f.encode(migratev1.PageResponse_builder{Status: &status, PageSize: proto.Uint32(uint32(pageSize))}.Build(), nil)
}

func (f *Frame) encode(message proto.Message, payload []byte) (platform.ReceivedFrame, error) {
	outgoing := wire.Outgoing{Version: f.incoming.Version, RequestID: f.incoming.RequestID,
		InReplyTo: f.incoming.InReplyTo, Message: message}
	if len(payload) > 0 {
		outgoing.Payload = wire.Payload{Body: bytes.NewReader(payload), Size: int64(len(payload)),
			Algorithm: wire.ChecksumCRC32C, Checksum: wire.EncodeCRC32C(crc32.Checksum(payload, crcTable))}
	}
	encoded, err := wire.Encode(outgoing)
	if err != nil {
		return platform.ReceivedFrame{}, err
	}
	return platform.ReceivedFrame{Header: encoded.Header, Payload: io.NopCloser(bytes.NewReader(payload)),
		PayloadSize: int64(len(payload))}, nil
}

// PreviousPages is one volume a server of the release before this one serves.
type PreviousPages = previous.Pages

// PreviousServer is a server of the release before this one: protocol version
// 1, no hello, one request at a time. A test hands a VM from it to a host of
// this release, as a rolling upgrade does.
type PreviousServer = previous.Server

// ServePrevious serves pages the way the release before this one did, on
// listener until ctx ends, and reports when it has stopped.
func ServePrevious(ctx context.Context, listener platform.Listener, served map[string]map[string]PreviousPages) (*PreviousServer, <-chan struct{}) {
	server := &previous.Server{Served: served}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		server.Serve(ctx, listener)
	}()
	return server, stopped
}
