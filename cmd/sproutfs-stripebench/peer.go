package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// reply is one server's answer to one request.
type reply struct {
	server int
	status byte
	stripe int
	data   []byte
	err    error
}

// peer is the client's one connection to one server. Requests go out as they
// are made and replies come back in any order, matched by identifier.
type peer struct {
	index int
	conn  net.Conn
	bufs  *buffers

	wmu  sync.Mutex
	next atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan<- reply
	err     error
	done    chan struct{}
}

func newPeer(index int, conn net.Conn, bufs *buffers) *peer {
	p := &peer{index: index, conn: conn, bufs: bufs, pending: make(map[uint64]chan<- reply), done: make(chan struct{})}
	go p.readLoop()
	return p
}

// send sends a request and has its reply delivered on ch, which must have
// room for it: the read loop never waits on a reader. It returns the
// request's identifier.
func (p *peer) send(req request, ch chan<- reply) (uint64, error) {
	req.id = p.next.Add(1)
	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return 0, fmt.Errorf("server %d: %w", p.index, err)
	}
	p.pending[req.id] = ch
	p.mu.Unlock()
	frame := req.marshal()
	p.wmu.Lock()
	_, err := p.conn.Write(frame[:])
	p.wmu.Unlock()
	if err != nil {
		p.forget(req.id)
		return 0, fmt.Errorf("server %d: send: %w", p.index, err)
	}
	return req.id, nil
}

// forget drops a request whose reply is no longer wanted. A reply that comes
// later is read and thrown away.
func (p *peer) forget(id uint64) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

// call sends a control request and waits for its reply.
func (p *peer) call(req request, timeout time.Duration) ([]byte, error) {
	ch := make(chan reply, 1)
	id, err := p.send(req, ch)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("server %d: %w", p.index, r.err)
		}
		switch r.status {
		case statusOK:
			return r.data, nil
		case statusRefused:
			return nil, fmt.Errorf("server %d refused: %s", p.index, r.data)
		default:
			return nil, fmt.Errorf("server %d answered a control request with status %d", p.index, r.status)
		}
	case <-timer.C:
		p.forget(id)
		return nil, fmt.Errorf("server %d did not answer a control request in %v", p.index, timeout)
	}
}

func (p *peer) readLoop() {
	defer close(p.done)
	for {
		h, err := readReplyHeader(p.conn)
		if err != nil {
			p.fail(err)
			return
		}
		var data []byte
		if h.length > 0 {
			data = p.bufs.get(int(h.length))
			if _, err := io.ReadFull(p.conn, data); err != nil {
				p.fail(err)
				return
			}
		}
		p.mu.Lock()
		ch, ok := p.pending[h.id]
		delete(p.pending, h.id)
		p.mu.Unlock()
		if !ok {
			p.bufs.put(data)
			continue
		}
		ch <- reply{server: p.index, status: h.status, stripe: int(h.stripe), data: data}
	}
}

// fail ends the connection's requests with err.
func (p *peer) fail(err error) {
	if closedConn(err) {
		err = errors.Join(errPeerClosed, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
	for id, ch := range p.pending {
		ch <- reply{server: p.index, err: err}
		delete(p.pending, id)
	}
}

var errPeerClosed = errors.New("the connection is closed")

// close hangs up and waits for the read loop to end.
func (p *peer) close() error {
	err := p.conn.Close()
	<-p.done
	return err
}
