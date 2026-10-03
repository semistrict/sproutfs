package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// pieceKey names a stripe a server may hold: an object under one of the
// set's codes, by the code's position in the set.
type pieceKey struct {
	code   uint8
	object uint32
}

// piece is where a held stripe is in the server's store.
type piece struct {
	off    int64
	length int
	stripe uint8
}

// heldStripe is a stripe a server is ranked for, with its bytes.
type heldStripe struct {
	key    pieceKey
	stripe uint8
	data   []byte
}

// held returns the stripes of one object that server index is ranked for,
// under every code: stripe i of a code lives on the object's rank i.
func held(set objectSet, layouts []*layout, index int, object uint32, scratch []byte) ([]heldStripe, error) {
	ranks := rank(everyServer(set.servers), object)
	set.fill(object, scratch)
	var out []heldStripe
	for ci, l := range layouts {
		for i, server := range ranks[:l.n()] {
			if server != index {
				continue
			}
			stripes, err := l.stripes(scratch)
			if err != nil {
				return nil, err
			}
			out = append(out, heldStripe{key: pieceKey{uint8(ci), object}, stripe: uint8(i), data: stripes[i]})
		}
	}
	return out, nil
}

// buildStore writes server index's stripes to w, object by object, so that the
// stripes of one object lie together, and returns where each one is.
func buildStore(set objectSet, index int, w io.Writer) (map[pieceKey]piece, int64, error) {
	layouts, err := set.layouts()
	if err != nil {
		return nil, 0, err
	}
	locations := make(map[pieceKey]piece)
	var off int64
	const batch = 256
	for first := 0; first < set.objects; first += batch {
		n := min(batch, set.objects-first)
		got := make([][]heldStripe, n)
		err := parallel(n, func(i int) error {
			stripes, err := held(set, layouts, index, uint32(first+i), make([]byte, set.objectBytes))
			got[i] = stripes
			return err
		})
		if err != nil {
			return nil, 0, err
		}
		for _, stripes := range got {
			for _, h := range stripes {
				if _, err := w.Write(h.data); err != nil {
					return nil, 0, fmt.Errorf("write the store: %w", err)
				}
				locations[h.key] = piece{off: off, length: len(h.data), stripe: h.stripe}
				off += int64(len(h.data))
			}
		}
	}
	return locations, off, nil
}

// parallel runs work(0) to work(n-1) on every processor and returns the first
// error.
func parallel(n int, work func(i int) error) error {
	var next atomic.Int64
	errs := make([]error, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for w := range errs {
		wg.Go(func() {
			for i := int(next.Add(1) - 1); i < n; i = int(next.Add(1) - 1) {
				if err := work(i); err != nil {
					errs[w] = err
					return
				}
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// server answers reads of the stripes it holds.
type server struct {
	set   objectSet
	index int
	data  io.ReaderAt
	held  map[pieceKey]piece
	// dropCache drops a range of the store from the page cache, so the next
	// read of it reads the disk. It is nil where the platform cannot.
	dropCache func(off, n int64) error

	delay atomic.Int64
	flags atomic.Uint32

	sent    atomic.Uint64
	replies atomic.Uint64
	reads   atomic.Uint64
	bufs    buffers
}

func (s *server) stats() (serverStats, error) {
	cpu, err := processCPU()
	if err != nil {
		return serverStats{}, err
	}
	return serverStats{CPU: cpu, Sent: s.sent.Load(), Replies: s.replies.Load(), Reads: s.reads.Load()}, nil
}

// serveConn answers one client's requests until it hangs up. Reads are
// answered concurrently, each when it is ready.
func (s *server) serveConn(conn net.Conn) {
	var wg sync.WaitGroup
	// hungUp ends the delays of replies no one is left to read.
	hungUp := make(chan struct{})
	defer func() {
		close(hungUp)
		wg.Wait()
		if err := conn.Close(); err != nil && !closedConn(err) {
			slog.Warn("close a client connection", "error", err)
		}
	}()
	w := &replyWriter{s: s, conn: conn}
	send := w.send
	for {
		req, err := readRequest(conn)
		if err != nil {
			if !closedConn(err) {
				slog.Warn("read a request", "server", s.index, "error", err)
			}
			return
		}
		got := time.Now()
		switch req.op {
		case opRead:
			s.reads.Add(1)
			if byte(s.flags.Load())&modeStall != 0 {
				// A stalled server never answers.
				continue
			}
			wg.Go(func() { s.answer(req, got, w, hungUp) })
		case opMode:
			if req.flags&modeCold != 0 && s.dropCache == nil {
				send(refusal(req.id, "this server cannot drop its store from the page cache"))
				continue
			}
			s.delay.Store(int64(req.arg))
			s.flags.Store(uint32(req.flags))
			send(ok(req.id, nil))
		case opStats:
			st, err := s.stats()
			if err != nil {
				slog.Error("read the server's CPU time", "error", err)
				send(refusal(req.id, err.Error()))
				continue
			}
			payload := make([]byte, statsBytes)
			st.put(payload)
			send(ok(req.id, payload))
		case opHello:
			want := s.set.fingerprint()
			if int(req.object) != s.index || req.arg != want {
				send(refusal(req.id, fmt.Sprintf("this is server %d of set %x, not server %d of set %x",
					s.index, want, req.object, req.arg)))
				continue
			}
			send(ok(req.id, nil))
		default:
			send(refusal(req.id, fmt.Sprintf("unknown request %d", req.op)))
		}
	}
}

// replyWriter writes one connection's replies, one at a time.
type replyWriter struct {
	s    *server
	conn net.Conn
	mu   sync.Mutex
}

// send writes a reply to a mode, stats or hello request, or a refusal.
func (w *replyWriter) send(b []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.write(b)
}

// sendRead writes a read's reply, b, whose header is h. The header goes out
// with how long the reply waited for the replies ahead of it.
func (w *replyWriter) sendRead(h replyHeader, b []byte) {
	asked := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	h.waited = time.Since(asked)
	h.put(b)
	w.write(b)
}

func (w *replyWriter) write(b []byte) {
	n, err := w.conn.Write(b)
	w.s.sent.Add(uint64(n))
	if err != nil {
		level := slog.LevelWarn
		if closedConn(err) {
			level = slog.LevelDebug
		}
		slog.Log(context.Background(), level, "send a reply", "server", w.s.index, "error", err)
		return
	}
	w.s.replies.Add(1)
}

// answer answers a read the server took off the connection at got.
func (s *server) answer(req request, got time.Time, w *replyWriter, hungUp <-chan struct{}) {
	if d := time.Duration(s.delay.Load()); d > 0 {
		timer := time.NewTimer(d)
		select {
		case <-timer.C:
		case <-hungUp:
			timer.Stop()
			return
		}
	}
	h := replyHeader{id: req.id, status: statusMiss}
	h.queued = time.Since(got)
	p, ok := s.held[pieceKey{req.code, req.object}]
	if !ok {
		var b [replyHeaderBytes]byte
		w.sendRead(h, b[:])
		return
	}
	buf := s.bufs.get(replyHeaderBytes + p.length)
	defer s.bufs.put(buf)
	began := time.Now()
	if _, err := s.data.ReadAt(buf[replyHeaderBytes:], p.off); err != nil {
		slog.Error("read a stripe from the store", "server", s.index, "offset", p.off, "error", err)
		w.send(refusal(req.id, err.Error()))
		return
	}
	if byte(s.flags.Load())&modeCold != 0 {
		if err := s.dropCache(p.off, int64(p.length)); err != nil {
			slog.Error("drop a stripe from the page cache", "server", s.index, "error", err)
		}
	}
	h.read = time.Since(began)
	h.status, h.stripe, h.length = statusHit, p.stripe, uint32(p.length)
	w.sendRead(h, buf)
}

func ok(id uint64, payload []byte) []byte {
	b := make([]byte, replyHeaderBytes+len(payload))
	replyHeader{id: id, status: statusOK, length: uint32(len(payload))}.put(b)
	copy(b[replyHeaderBytes:], payload)
	return b
}

func refusal(id uint64, why string) []byte {
	b := make([]byte, replyHeaderBytes+len(why))
	replyHeader{id: id, status: statusRefused, length: uint32(len(why))}.put(b)
	copy(b[replyHeaderBytes:], why)
	return b
}

// closedConn reports whether err only says the other end hung up or this end
// closed the connection.
func closedConn(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	listen := fs.String("listen", ":7400", "address to serve on")
	index := fs.Int("index", 0, "this server's place in the client's server list")
	servers := fs.Int("servers", 6, "how many servers there are")
	objects := fs.Int("objects", 4096, "how many objects there are")
	objectBytes := fs.Int("object-bytes", 350_000, "each object's size")
	seed := fs.Uint64("seed", 1, "the seed the objects are derived from")
	codes := fs.String("codes", "1+0,4+1,4+2", "the codes the objects are stored under")
	path := fs.String("file", "stripes.store", "the file to keep this server's stripes in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	parsed, err := parseCodes(*codes)
	if err != nil {
		return err
	}
	set := objectSet{servers: *servers, objects: *objects, objectBytes: *objectBytes, seed: *seed, codes: parsed}
	if err := set.validate(); err != nil {
		return err
	}
	if *index < 0 || *index >= *servers {
		return fmt.Errorf("index %d is not one of %d servers", *index, *servers)
	}
	started := time.Now()
	store, size, err := writeStore(set, *index, *path)
	if err != nil {
		return err
	}
	f, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Error("close the store", "error", err)
		}
	}()
	s := &server{set: set, index: *index, data: f, held: store, dropCache: dropCacheFunc(f)}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	slog.Info("serving", "address", ln.Addr().String(), "index", *index, "stripes", len(store),
		"bytes", size, "built", time.Since(started).Round(time.Millisecond))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		if err := ln.Close(); err != nil {
			slog.Error("close the listener", "error", err)
		}
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.serveConn(conn)
	}
}

// writeStore builds the store in a file and syncs it, so a cold read finds
// nothing dirty to keep in the page cache.
func writeStore(set objectSet, index int, path string) (map[pieceKey]piece, int64, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, 0, err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	store, size, err := buildStore(set, index, w)
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, 0, fmt.Errorf("build the store in %s: %w", path, err)
	}
	return store, size, nil
}
