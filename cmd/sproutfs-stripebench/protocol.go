package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// The protocol is fixed-size frames over one connection per client and server.
// Every request carries an identifier its reply repeats, so a server answers
// requests in any order and a slow reply holds up no other.

const (
	// opRead asks for the server's stripe of an object under a code.
	opRead byte = 1
	// opMode sets how the server answers reads from now on: after a delay,
	// never, or with the stripe dropped from the page cache once read.
	opMode byte = 2
	// opStats asks for the server's CPU time and what it has sent.
	opStats byte = 3
	// opHello checks the server holds the object set the client expects, as
	// the server at the index the client expects.
	opHello byte = 4
)

const (
	// statusHit carries a stripe.
	statusHit byte = 0
	// statusMiss says the server holds no stripe of the object, as a server
	// that has just entered an object's ranks does.
	statusMiss byte = 1
	// statusOK answers a mode, a stats or a hello request.
	statusOK byte = 2
	// statusRefused answers a request the server cannot do; the payload says
	// why.
	statusRefused byte = 3
)

const (
	modeStall byte = 1 << 0
	modeCold  byte = 1 << 1
)

const requestBytes = 24

// request is one frame: id u64, op u8, code u8, flags u8, a zero byte,
// object u32, arg u64. For opMode, arg is the delay in nanoseconds; for
// opHello, object is the server's index and arg the set's fingerprint.
type request struct {
	id     uint64
	op     byte
	code   byte
	flags  byte
	object uint32
	arg    uint64
}

func (r request) marshal() [requestBytes]byte {
	var b [requestBytes]byte
	binary.LittleEndian.PutUint64(b[0:], r.id)
	b[8], b[9], b[10] = r.op, r.code, r.flags
	binary.LittleEndian.PutUint32(b[12:], r.object)
	binary.LittleEndian.PutUint64(b[16:], r.arg)
	return b
}

func readRequest(r io.Reader) (request, error) {
	var b [requestBytes]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return request{}, err
	}
	return request{
		id:     binary.LittleEndian.Uint64(b[0:]),
		op:     b[8],
		code:   b[9],
		flags:  b[10],
		object: binary.LittleEndian.Uint32(b[12:]),
		arg:    binary.LittleEndian.Uint64(b[16:]),
	}, nil
}

const replyHeaderBytes = 16

// replyHeader precedes a reply's payload: id u64, status u8, stripe u8, two
// zero bytes, payload length u32.
type replyHeader struct {
	id     uint64
	status byte
	stripe byte
	length uint32
}

func (h replyHeader) put(b []byte) {
	binary.LittleEndian.PutUint64(b[0:], h.id)
	b[8], b[9], b[10], b[11] = h.status, h.stripe, 0, 0
	binary.LittleEndian.PutUint32(b[12:], h.length)
}

func readReplyHeader(r io.Reader) (replyHeader, error) {
	var b [replyHeaderBytes]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return replyHeader{}, err
	}
	return replyHeader{
		id:     binary.LittleEndian.Uint64(b[0:]),
		status: b[8],
		stripe: b[9],
		length: binary.LittleEndian.Uint32(b[12:]),
	}, nil
}

// serverStats is what opStats answers: four u64 values.
type serverStats struct {
	CPU     time.Duration `json:"cpu_ns"`
	Sent    uint64        `json:"sent_bytes"`
	Replies uint64        `json:"replies"`
	Reads   uint64        `json:"reads"`
}

const statsBytes = 32

func (s serverStats) put(b []byte) {
	binary.LittleEndian.PutUint64(b[0:], uint64(s.CPU))
	binary.LittleEndian.PutUint64(b[8:], s.Sent)
	binary.LittleEndian.PutUint64(b[16:], s.Replies)
	binary.LittleEndian.PutUint64(b[24:], s.Reads)
}

func parseStats(b []byte) (serverStats, error) {
	if len(b) != statsBytes {
		return serverStats{}, fmt.Errorf("stats of %d bytes, want %d", len(b), statsBytes)
	}
	return serverStats{
		CPU:     time.Duration(binary.LittleEndian.Uint64(b[0:])),
		Sent:    binary.LittleEndian.Uint64(b[8:]),
		Replies: binary.LittleEndian.Uint64(b[16:]),
		Reads:   binary.LittleEndian.Uint64(b[24:]),
	}, nil
}

func (s serverStats) minus(before serverStats) serverStats {
	return serverStats{
		CPU:     s.CPU - before.CPU,
		Sent:    s.Sent - before.Sent,
		Replies: s.Replies - before.Replies,
		Reads:   s.Reads - before.Reads,
	}
}
