package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestThePermutationIsABijectionAndItsInverseUndoesIt(t *testing.T) {
	for _, n := range []uint64{1, 2, 3, 7, 1000, 4097, 65536} {
		p := newPermutation(n, 42)
		seen := make([]bool, n)
		for x := range n {
			y := p.At(x)
			if y >= n {
				t.Fatalf("n=%d: %d maps to %d, outside the domain", n, x, y)
			}
			if seen[y] {
				t.Fatalf("n=%d: %d is the image of two numbers", n, y)
			}
			seen[y] = true
			if back := p.Inverse(y); back != x {
				t.Fatalf("n=%d: the inverse of %d is %d, not %d", n, y, back, x)
			}
		}
	}
}

func TestTheChainIsOneCycleThroughEveryKey(t *testing.T) {
	d, err := newDataset(5000, 0, 64, 7)
	if err != nil {
		t.Fatal(err)
	}
	visited := make([]bool, d.keys)
	key := d.step(0)
	for s := range d.keys {
		if visited[key] {
			t.Fatalf("step %d comes back to %d before the cycle is complete", s, key)
		}
		visited[key] = true
		if want := d.step(s + 1); d.next(key) != want {
			t.Fatalf("step %d: next(%d) is %d, and step %d is %d", s, key, d.next(key), s+1, want)
		}
		key = d.next(key)
	}
	if key != d.step(0) {
		t.Fatalf("after %d steps the chain is at %d, not back at its start %d", d.keys, key, d.step(0))
	}
}

// TestAStepOfTheChainLandsFarFromTheOneBefore: the walk measures a heap read in
// a random order only if consecutive steps are not neighbours in the order the
// keys were written, which is the order the server allocated them in.
func TestAStepOfTheChainLandsFarFromTheOneBefore(t *testing.T) {
	d, err := newDataset(1_000_000, 0, 64, 3)
	if err != nil {
		t.Fatal(err)
	}
	near := 0
	key := d.step(0)
	for range 10000 {
		next := d.next(key)
		if max(next, key)-min(next, key) < 1000 {
			near++
		}
		key = next
	}
	// One step in five hundred lands within a thousand keys of the last by
	// chance; twenty in ten thousand is that, and a chain that walked the
	// keys in their order would land near every time.
	if near > 60 {
		t.Fatalf("%d of 10000 steps landed within 1000 keys of the step before", near)
	}
}

func TestAValueNamesTheNextKeyAndIsPaddedToItsSize(t *testing.T) {
	d, err := newDataset(100, 0, 40, 9)
	if err != nil {
		t.Fatal(err)
	}
	value := d.value(17)
	if len(value) != 40 {
		t.Fatalf("a value of %d bytes, want 40", len(value))
	}
	if want := string(keyName(d.next(17))) + ":"; string(value[:nameBytes+1]) != want {
		t.Fatalf("the value begins %q, want %q", value[:nameBytes+1], want)
	}
	if next, err := d.checkValue(17, value); err != nil || next != d.next(17) {
		t.Fatalf("checkValue of the value itself: %d, %v", next, err)
	}
}

func TestADatasetRefusesAValueTooSmallToNameTheNextKey(t *testing.T) {
	_, err := newDataset(10, 0, 12, 1)
	if err == nil || !strings.Contains(err.Error(), "cannot hold the next key's name") {
		t.Fatalf("got %v", err)
	}
}

func TestAWalkFollowsWhatTheLoadWrote(t *testing.T) {
	server := startFakeServer(t)
	shape := []string{"--socket", server.address, "--keys", "2000", "--members", "1500",
		"--value", "48", "--seed", "11"}

	var loaded bytes.Buffer
	if err := runAgainst(server, "load", shape, &loaded); err != nil {
		t.Fatal(err)
	}
	var load map[string]any
	if err := json.Unmarshal(loaded.Bytes(), &load); err != nil {
		t.Fatalf("the load printed %q: %v", loaded.String(), err)
	}
	if load["keys"] != float64(2000) || load["members"] != float64(1500) {
		t.Fatalf("the load reported %v", load)
	}

	var walked bytes.Buffer
	err := runAgainst(server, "walk", append(shape, "--steps", "3000", "--scan", "1500", "--chunk", "100"), &walked)
	if err != nil {
		t.Fatal(err)
	}
	var report walkReport
	if err := json.Unmarshal(walked.Bytes(), &report); err != nil {
		t.Fatalf("the walk printed %q: %v", walked.String(), err)
	}
	if report.Chase.Requests != 3000 || len(report.Chase.Micros) != 3000 || report.Chase.Stopped {
		t.Fatalf("the chase made %d requests, timed %d, stopped %t; want 3000, 3000, false",
			report.Chase.Requests, len(report.Chase.Micros), report.Chase.Stopped)
	}
	if report.Scan.Requests != 15 || len(report.Scan.Micros) != 15 {
		t.Fatalf("the scan made %d requests and timed %d, want 15 and 15", report.Scan.Requests, len(report.Scan.Micros))
	}
	// Every GET of the chase asked for the key the one before it returned,
	// and the chain is one cycle, so 3000 steps over 2000 keys asked for
	// every key, a thousand of them twice.
	if got := server.distinctGets(); got != 2000 {
		t.Fatalf("the chase asked for %d distinct keys, want 2000", got)
	}
}

func TestAWalkStopsAtTheStepWhoseValueIsWrong(t *testing.T) {
	server := startFakeServer(t)
	shape := []string{"--socket", server.address, "--keys", "500", "--value", "32", "--seed", "5"}
	if err := runAgainst(server, "load", shape, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, err := newDataset(500, 0, 32, 5)
	if err != nil {
		t.Fatal(err)
	}
	// The last byte of the value at step 10, which names the right next key
	// and is wrong only in its padding: a walk that checked only the name
	// would go on.
	corrupt := string(keyName(d.step(10)))
	server.mutate(func(strings map[string][]byte) { strings[corrupt][31] ^= 1 })

	var walked bytes.Buffer
	err = runAgainst(server, "walk", append(shape, "--steps", "100"), &walked)
	if err == nil || !strings.Contains(err.Error(), "step 10: "+corrupt+" holds 32 bytes that differ from the 32 it was given at byte 31") {
		t.Fatalf("the walk ended with %v", err)
	}
	var report walkReport
	if err := json.Unmarshal(walked.Bytes(), &report); err != nil {
		t.Fatalf("a failed walk printed %q: %v", walked.String(), err)
	}
	if report.Chase.Requests != 11 {
		t.Fatalf("a walk wrong at step 10 made %d requests, want 11", report.Chase.Requests)
	}
}

func TestAScanStopsAtTheRankWhoseMemberIsWrong(t *testing.T) {
	server := startFakeServer(t)
	shape := []string{"--socket", server.address, "--keys", "10", "--members", "300", "--value", "32", "--seed", "8"}
	if err := runAgainst(server, "load", shape, io.Discard); err != nil {
		t.Fatal(err)
	}
	d, err := newDataset(10, 300, 32, 8)
	if err != nil {
		t.Fatal(err)
	}
	// The members at ranks 150 and 151 trade scores.
	first, second := string(memberName(d.memberAt(150))), string(memberName(d.memberAt(151)))
	server.mutate(func(map[string][]byte) {
		server.scores[first], server.scores[second] = server.scores[second], server.scores[first]
	})
	err = runAgainst(server, "walk", append(shape, "--steps", "5", "--scan", "300"), io.Discard)
	want := fmt.Sprintf("rank 150 holds %s, and its score belongs to %s", second, first)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("the scan ended with %v, want %q", err, want)
	}
}

func TestAWalkStopsAtItsBudgetAndSaysSo(t *testing.T) {
	server := startFakeServer(t)
	shape := []string{"--socket", server.address, "--keys", "100", "--value", "32", "--seed", "2"}
	if err := runAgainst(server, "load", shape, io.Discard); err != nil {
		t.Fatal(err)
	}
	server.delay.Store(int64(5 * time.Millisecond))
	var walked bytes.Buffer
	if err := runAgainst(server, "walk", append(shape, "--steps", "100000", "--budget", "50ms"), &walked); err != nil {
		t.Fatal(err)
	}
	var report walkReport
	if err := json.Unmarshal(walked.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Chase.Stopped || report.Chase.Requests == 0 || report.Chase.Requests > 20 {
		t.Fatalf("a walk of 5 ms steps under a 50 ms budget made %d requests, stopped %t",
			report.Chase.Requests, report.Chase.Stopped)
	}
}

// runAgainst runs one command of the binary against the fake server, over TCP:
// the fake listens on loopback because a unix socket's path under a test's
// temporary directory can be longer than the operating system allows.
func runAgainst(server *fakeServer, command string, args []string, out io.Writer) error {
	return runOn("tcp", command, args, out)
}

// fakeServer is the part of Valkey a load and a walk use, over loopback.
type fakeServer struct {
	address string
	// delay is how long each command waits before its answer, in
	// nanoseconds.
	delay atomic.Int64

	mu      sync.Mutex
	strings map[string][]byte
	scores  map[string]float64
	gets    map[string]bool
}

func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeServer{address: listener.Addr().String(), strings: map[string][]byte{},
		scores: map[string]float64{}, gets: map[string]bool{}}
	var served sync.WaitGroup
	t.Cleanup(func() {
		listener.Close()
		served.Wait()
	})
	served.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			served.Go(func() { server.serve(t, conn) })
		}
	})
	return server
}

func (s *fakeServer) mutate(change func(map[string][]byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	change(s.strings)
}

func (s *fakeServer) distinctGets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.gets)
}

func (s *fakeServer) serve(t *testing.T, conn net.Conn) {
	defer conn.Close()
	reader, writer := bufio.NewReader(conn), bufio.NewWriter(conn)
	for {
		args, err := readCommand(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				t.Errorf("the fake server read %v", err)
			}
			return
		}
		time.Sleep(time.Duration(s.delay.Load()))
		s.answer(writer, args)
		// Replies wait while more commands are already here, as a real
		// server's do, so a pipelined batch is answered as one write.
		if reader.Buffered() == 0 {
			if err := writer.Flush(); err != nil {
				return
			}
		}
	}
}

func (s *fakeServer) answer(w *bufio.Writer, args [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bulk := func(value []byte) { fmt.Fprintf(w, "$%d\r\n%s\r\n", len(value), value) }
	switch strings.ToUpper(string(args[0])) {
	case "PING":
		w.WriteString("+PONG\r\n")
	case "SET":
		s.strings[string(args[1])] = slices.Clone(args[2])
		w.WriteString("+OK\r\n")
	case "GET":
		s.gets[string(args[1])] = true
		value, ok := s.strings[string(args[1])]
		if !ok {
			w.WriteString("$-1\r\n")
			return
		}
		bulk(value)
	case "ZADD":
		added := 0
		for at := 2; at+1 < len(args); at += 2 {
			score, err := strconv.ParseFloat(string(args[at]), 64)
			if err != nil {
				w.WriteString("-ERR value is not a valid float\r\n")
				return
			}
			if _, ok := s.scores[string(args[at+1])]; !ok {
				added++
			}
			s.scores[string(args[at+1])] = score
		}
		fmt.Fprintf(w, ":%d\r\n", added)
	case "ZCARD":
		fmt.Fprintf(w, ":%d\r\n", len(s.scores))
	case "DBSIZE":
		size := len(s.strings)
		if len(s.scores) > 0 {
			size++
		}
		fmt.Fprintf(w, ":%d\r\n", size)
	case "INFO":
		bulk([]byte("# Memory\r\nused_memory:1234\r\nused_memory_rss:5678\r\nmem_allocator:jemalloc\r\n"))
	case "ZRANGE":
		first, _ := strconv.Atoi(string(args[2]))
		last, _ := strconv.Atoi(string(args[3]))
		members := slices.Collect(func(yield func(string) bool) {
			for member := range s.scores {
				if !yield(member) {
					return
				}
			}
		})
		slices.SortFunc(members, func(a, b string) int {
			if s.scores[a] != s.scores[b] {
				if s.scores[a] < s.scores[b] {
					return -1
				}
				return 1
			}
			return strings.Compare(a, b)
		})
		last = min(last, len(members)-1)
		fmt.Fprintf(w, "*%d\r\n", max(last-first+1, 0))
		for _, member := range members[first : last+1] {
			bulk([]byte(member))
		}
	default:
		fmt.Fprintf(w, "-ERR unknown command '%s'\r\n", args[0])
	}
}

func readCommand(reader *bufio.Reader) ([][]byte, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(header, "*") {
		return nil, fmt.Errorf("a command line %q", header)
	}
	count, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	if err != nil {
		return nil, err
	}
	args := make([][]byte, count)
	for at := range args {
		length, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(length[1:]))
		if err != nil {
			return nil, err
		}
		args[at] = make([]byte, size+2)
		if _, err := io.ReadFull(reader, args[at]); err != nil {
			return nil, err
		}
		args[at] = args[at][:size]
	}
	return args, nil
}
