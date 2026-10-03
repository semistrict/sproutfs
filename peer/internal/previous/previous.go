// Package previous is the peer protocol as the release before this one spoke
// it, frozen: main at 6af0d3de, when the channel was the page server. That is
// protocol version 1: no hello, one request at a time on a connection, every
// header at wire version 1 and none of them checksummed, and a server that
// closes a connection on any frame it cannot read.
//
// Every release must speak the previous release's protocol, because a rolling
// upgrade hands VMs between hosts of the two. This package is what this release
// is tested against. Only tests import it. It is copied from that release's
// codec, its destination's exchange and its source's connection loop, and
// reads and writes the same messages; the generated types it uses have only
// gained fields since, which that release's own decoder skipped.
package previous

import (
	"bytes"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"slices"
	"sync"

	"github.com/semistrict/sproutfs/internal/blob"
	migratev1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/migrate/v1"
	wirev1 "github.com/semistrict/sproutfs/peer/internal/gen/sproutfs/wire/v1"
	"github.com/semistrict/sproutfs/platform"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// version is the one wire version that release wrote and read.
const version = 1

var (
	errMalformed = errors.New("previous: malformed frame")
	errVersion   = errors.New("previous: unsupported wire version")
	crcTable     = crc32.MakeTable(crc32.Castagnoli)
)

// encode is that release's wire.Encode: a version 1 envelope, a CRC32C of the
// payload when there is one, and nothing after the payload descriptor.
func encode(requestID, inReplyTo uint64, message proto.Message, payload []byte) (platform.Frame, error) {
	wrapped, err := anypb.New(message)
	if err != nil {
		return platform.Frame{}, err
	}
	builder := wirev1.Envelope_builder{WireVersion: proto.Uint32(version), RequestId: proto.Uint64(requestID),
		InReplyTo: proto.Uint64(inReplyTo), Message: wrapped}
	if len(payload) > 0 {
		algorithm := wirev1.ChecksumAlgorithm_CHECKSUM_ALGORITHM_CRC32C
		sum := crc32.Checksum(payload, crcTable)
		builder.Payload = wirev1.PayloadDescriptor_builder{Length: proto.Uint64(uint64(len(payload))),
			ChecksumAlgorithm: &algorithm,
			Checksum:          []byte{byte(sum >> 24), byte(sum >> 16), byte(sum >> 8), byte(sum)}}.Build()
	}
	header, err := proto.MarshalOptions{Deterministic: true}.Marshal(builder.Build())
	if err != nil {
		return platform.Frame{}, err
	}
	return platform.Frame{Header: header, Payload: bytes.NewReader(payload), PayloadSize: int64(len(payload))}, nil
}

// received is one decoded frame and its payload.
type received struct {
	requestID, inReplyTo uint64
	message              *anypb.Any
	payload              []byte
}

// decode is that release's wire.Decode, which accepted wire version 1 alone.
func decode(frame platform.ReceivedFrame) (received, error) {
	defer frame.Payload.Close()
	envelope := new(wirev1.Envelope)
	if err := proto.Unmarshal(frame.Header, envelope); err != nil {
		return received{}, errors.Join(errMalformed, err)
	}
	if !envelope.HasWireVersion() || envelope.GetWireVersion() != version {
		return received{}, errVersion
	}
	if !envelope.HasRequestId() || !envelope.HasInReplyTo() || envelope.GetMessage() == nil {
		return received{}, errMalformed
	}
	payload, err := io.ReadAll(frame.Payload)
	if err != nil {
		return received{}, err
	}
	if descriptor := envelope.GetPayload(); descriptor != nil && descriptor.GetLength() != uint64(len(payload)) {
		return received{}, errMalformed
	}
	return received{requestID: envelope.GetRequestId(), inReplyTo: envelope.GetInReplyTo(),
		message: envelope.GetMessage(), payload: payload}, nil
}

// Client is that release's destination: one request at a time on one
// connection, each reply matched to its request.
type Client struct {
	Conn platform.Conn
	next uint64
}

func (c *Client) exchange(ctx context.Context, request, response proto.Message) ([]byte, error) {
	c.next++
	frame, err := encode(c.next, 0, request, nil)
	if err != nil {
		return nil, err
	}
	if err := c.Conn.Send(ctx, frame); err != nil {
		return nil, err
	}
	reply, err := c.Conn.Receive(ctx)
	if err != nil {
		return nil, err
	}
	answer, err := decode(reply)
	if err != nil {
		return nil, err
	}
	if answer.inReplyTo != c.next {
		return nil, errMalformed
	}
	if err := answer.message.UnmarshalTo(response); err != nil {
		return nil, err
	}
	return answer.payload, nil
}

// Pages asks for count pages from first, as that release's destination did,
// and returns the reply and the pages it carried, decoded.
func (c *Client) Pages(ctx context.Context, vm, volume string, first uint64, count, pageSize int) (*migratev1.PageResponse, []byte, error) {
	response := new(migratev1.PageResponse)
	payload, err := c.exchange(ctx, migratev1.PageRequest_builder{Vm: proto.String(vm), Volume: proto.String(volume),
		FirstPage: proto.Uint64(first), Count: proto.Uint32(uint32(count)), PayloadFormat: proto.Uint32(1)}.Build(), response)
	if err != nil || response.GetStatus() != migratev1.Status_STATUS_OK {
		return response, nil, err
	}
	served := 0
	for _, bits := range response.GetPresent() {
		for ; bits != 0; bits &= bits - 1 {
			served++
		}
	}
	pages, err := blob.Decode(ctx, payload, served*pageSize)
	return response, pages, err
}

// Resident asks for one listing of what a memory region holds.
func (c *Client) Resident(ctx context.Context, vm, volume string, first uint64) (*migratev1.ResidentResponse, error) {
	response := new(migratev1.ResidentResponse)
	_, err := c.exchange(ctx, migratev1.ResidentRequest_builder{Vm: proto.String(vm), Volume: proto.String(volume),
		FirstPage: proto.Uint64(first), MaxRuns: proto.Uint32(1024)}.Build(), response)
	return response, err
}

// Claim asks the parent's host to mark a fork child's hold claimed.
func (c *Client) Claim(ctx context.Context, vm string) (*migratev1.ClaimResponse, error) {
	response := new(migratev1.ClaimResponse)
	_, err := c.exchange(ctx, migratev1.ClaimRequest_builder{Vm: proto.String(vm)}.Build(), response)
	return response, err
}

// Pages is one volume a previous server serves: the bytes it holds of each page,
// and whether no checkpoint has them.
type Pages interface {
	ReadResident(ctx context.Context, page uint64, dst []byte) (held, unpublished bool, err error)
	Resident() ([]uint64, error)
	PageSize() uint64
}

// Server is that release's source: it serves each connection one request at a
// time and closes it on any frame it cannot read, a hello among them.
type Server struct {
	// Served is what it serves, by VM and volume.
	Served map[string]map[string]Pages

	mu      sync.Mutex
	claimed map[string]bool
}

// Serve accepts on listener until ctx ends.
func (s *Server) Serve(ctx context.Context, listener platform.Listener) {
	var connections sync.WaitGroup
	defer connections.Wait()
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		connections.Go(func() { s.serveConn(ctx, conn) })
	}
}

func (s *Server) serveConn(ctx context.Context, conn platform.Conn) {
	defer conn.Close()
	for {
		frame, err := conn.Receive(ctx)
		if err != nil {
			return
		}
		request, err := decode(frame)
		if err != nil {
			return
		}
		response, payload, err := s.answer(ctx, request)
		if err != nil {
			return
		}
		reply, err := encode(request.requestID, request.requestID, response, payload)
		if err != nil {
			return
		}
		if err := conn.Send(ctx, reply); err != nil {
			return
		}
	}
}

func (s *Server) answer(ctx context.Context, request received) (proto.Message, []byte, error) {
	pageRequest, residentRequest, claimRequest := new(migratev1.PageRequest), new(migratev1.ResidentRequest), new(migratev1.ClaimRequest)
	switch {
	case request.message.MessageIs(pageRequest):
		if err := request.message.UnmarshalTo(pageRequest); err != nil {
			return nil, nil, err
		}
		response, payload := s.pages(ctx, pageRequest)
		return response, payload, nil
	case request.message.MessageIs(residentRequest):
		if err := request.message.UnmarshalTo(residentRequest); err != nil {
			return nil, nil, err
		}
		return s.resident(residentRequest), nil, nil
	case request.message.MessageIs(claimRequest):
		if err := request.message.UnmarshalTo(claimRequest); err != nil {
			return nil, nil, err
		}
		status := migratev1.Status_STATUS_UNKNOWN_VM
		s.mu.Lock()
		if _, served := s.Served[claimRequest.GetVm()]; served {
			if s.claimed == nil {
				s.claimed = make(map[string]bool)
			}
			s.claimed[claimRequest.GetVm()] = true
			status = migratev1.Status_STATUS_OK
		}
		s.mu.Unlock()
		return migratev1.ClaimResponse_builder{Status: &status}.Build(), nil, nil
	default:
		return nil, nil, errMalformed
	}
}

func (s *Server) pagesOf(vm, volume string) (Pages, migratev1.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	volumes, found := s.Served[vm]
	if !found {
		return nil, migratev1.Status_STATUS_UNKNOWN_VM
	}
	pages, found := volumes[volume]
	if !found {
		return nil, migratev1.Status_STATUS_UNKNOWN_VOLUME
	}
	return pages, migratev1.Status_STATUS_OK
}

func (s *Server) pages(ctx context.Context, request *migratev1.PageRequest) (*migratev1.PageResponse, []byte) {
	answer := func(status migratev1.Status, pageSize int) *migratev1.PageResponse {
		return migratev1.PageResponse_builder{Status: &status, PageSize: proto.Uint32(uint32(pageSize))}.Build()
	}
	count := int(request.GetCount())
	if count <= 0 || request.GetPayloadFormat() != 1 {
		return answer(migratev1.Status_STATUS_INVALID_REQUEST, 0), nil
	}
	pages, status := s.pagesOf(request.GetVm(), request.GetVolume())
	if status != migratev1.Status_STATUS_OK {
		return answer(status, 0), nil
	}
	pageSize := int(pages.PageSize())
	count = min(count, max(1, min(256, (2<<20)/pageSize)))
	present, dirty := make([]byte, (count+7)/8), make([]byte, (count+7)/8)
	payload := make([]byte, 0, count*pageSize)
	page := make([]byte, pageSize)
	for index := range count {
		held, unpublished, err := pages.ReadResident(ctx, request.GetFirstPage()+uint64(index), page)
		if err != nil {
			break
		}
		if !held {
			continue
		}
		present[index/8] |= 1 << (index % 8)
		if unpublished {
			dirty[index/8] |= 1 << (index % 8)
		}
		payload = append(payload, page...)
	}
	encoded, err := blob.Encode(ctx, payload)
	if err != nil {
		return answer(migratev1.Status_STATUS_INTERNAL, pageSize), nil
	}
	status = migratev1.Status_STATUS_OK
	return migratev1.PageResponse_builder{Status: &status, Present: present, Dirty: dirty,
		PageSize: proto.Uint32(uint32(pageSize)), Count: proto.Uint32(uint32(count)),
		PayloadFormat: proto.Uint32(1)}.Build(), encoded
}

func (s *Server) resident(request *migratev1.ResidentRequest) *migratev1.ResidentResponse {
	pages, status := s.pagesOf(request.GetVm(), request.GetVolume())
	if status != migratev1.Status_STATUS_OK {
		return migratev1.ResidentResponse_builder{Status: &status}.Build()
	}
	resident, err := pages.Resident()
	if err != nil {
		status = migratev1.Status_STATUS_INTERNAL
		return migratev1.ResidentResponse_builder{Status: &status}.Build()
	}
	resident = slices.Sorted(slices.Values(resident))
	var runs []*migratev1.PageRun
	for _, page := range resident {
		if page < request.GetFirstPage() {
			continue
		}
		if n := len(runs); n > 0 && runs[n-1].GetFirstPage()+uint64(runs[n-1].GetCount()) == page {
			runs[n-1].SetCount(runs[n-1].GetCount() + 1)
			continue
		}
		runs = append(runs, migratev1.PageRun_builder{FirstPage: proto.Uint64(page), Count: proto.Uint32(1)}.Build())
	}
	status = migratev1.Status_STATUS_OK
	return migratev1.ResidentResponse_builder{Status: &status, Runs: runs,
		PageSize: proto.Uint32(uint32(pages.PageSize())), More: proto.Bool(false)}.Build()
}

// Claimed reports whether a fork child's destination claimed its hold here.
func (s *Server) Claimed(vm string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimed[vm]
}
