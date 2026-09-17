package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestClientReachesTheGuestThroughTheHandshake is the whole channel: the client
// connects to the socket the VMM listens on, asks for the guest's port, and the
// stream that comes back carries an ordinary HTTP exchange.
func TestClientReachesTheGuestThroughTheHandshake(t *testing.T) {
	var asked string
	socket := fakeVMM(t, func(request string) bool { asked = request; return true },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "the guest saw %s %s", r.Method, r.URL.Path)
		}))

	response, err := NewClient(socket, 10*time.Second).Get(URL("/healthz"))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the guest saw GET /healthz" {
		t.Fatalf("the guest answered %q", body)
	}
	if asked != fmt.Sprintf("CONNECT %d", Port) {
		t.Fatalf("the VMM was asked %q, want CONNECT %d", asked, Port)
	}
}

// TestDialReportsAPortNothingIsListeningOn: Firecracker ends the host's
// connection when no guest software holds the port, which is what a VM that is
// still booting looks like.
func TestDialReportsAPortNothingIsListeningOn(t *testing.T) {
	socket := fakeVMM(t, func(string) bool { return false }, nil)
	_, err := Dial(t.Context(), socket, Port)
	if !errors.Is(err, ErrNoGuest) {
		t.Fatalf("dialling a closed port reported %v, want ErrNoGuest", err)
	}
}

// TestDialReportsAMachineWithNoVsock, which is the empty socket path a machine
// started without the device has.
func TestDialReportsAMachineWithNoVsock(t *testing.T) {
	_, err := Dial(t.Context(), "", Port)
	if !errors.Is(err, ErrNoGuest) {
		t.Fatalf("dialling no socket reported %v, want ErrNoGuest", err)
	}
	if !strings.Contains(err.Error(), "no vsock") {
		t.Fatalf("the failure reads %q, want it to name the missing vsock", err)
	}
}

// TestDialReportsASocketThatIsNotThere, which is a VM whose process is gone.
func TestDialReportsASocketThatIsNotThere(t *testing.T) {
	_, err := Dial(t.Context(), filepath.Join(t.TempDir(), "absent.sock"), Port)
	if !errors.Is(err, ErrNoGuest) {
		t.Fatalf("dialling an absent socket reported %v, want ErrNoGuest", err)
	}
}

// TestDialRefusesAReplyThatIsNotAnAcknowledgement.
func TestDialRefusesAReplyThatIsNotAnAcknowledgement(t *testing.T) {
	socket := shortSocket(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = readLine(conn)
		_, _ = io.WriteString(conn, "NO nothing there\n")
	}()
	_, err = Dial(t.Context(), socket, Port)
	if !errors.Is(err, ErrNoGuest) {
		t.Fatalf("a refused forward reported %v, want ErrNoGuest", err)
	}
	if !strings.Contains(err.Error(), "NO nothing there") {
		t.Fatalf("the failure reads %q, want it to quote what the VMM said", err)
	}
}

// shortSocket names a socket in this test's own directory, from inside it: a
// Unix socket's path is bound by a small fixed buffer, and a temporary
// directory's absolute path can be longer than that on its own.
func shortSocket(t *testing.T) string {
	t.Helper()
	t.Chdir(t.TempDir())
	return "v.sock"
}

// fakeVMM stands in for Firecracker's end of a VM's vsock: it listens on a Unix
// socket, reads the connection forwarding request, and either acknowledges it
// and hands the stream to the guest handler or ends the connection the way the
// VMM does when nothing holds the port.
func fakeVMM(t *testing.T, accept func(request string) bool, guest http.Handler) string {
	t.Helper()
	socket := shortSocket(t)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	streams := make(chan net.Conn)
	go func() {
		defer close(streams)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			request, err := readLine(conn)
			if err != nil || !accept(request) {
				_ = conn.Close()
				continue
			}
			if _, err := fmt.Fprintf(conn, "OK %d\n", 1024); err != nil {
				_ = conn.Close()
				continue
			}
			streams <- conn
		}
	}()
	server := &http.Server{
		Handler:     guest,
		BaseContext: func(net.Listener) context.Context { return context.Background() },
	}
	guests := &forwarded{streams: streams, addr: listener.Addr(), done: make(chan struct{})}
	go func() { _ = server.Serve(guests) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket
}

// forwarded is the guest's side of the channel as a net.Listener: every stream
// the VMM forwarded is one connection. Closing it wakes the accept, which is
// what an http.Server shutting down waits for.
type forwarded struct {
	streams <-chan net.Conn
	addr    net.Addr
	done    chan struct{}
	once    sync.Once
}

func (f *forwarded) Accept() (net.Conn, error) {
	select {
	case conn, open := <-f.streams:
		if !open {
			return nil, net.ErrClosed
		}
		return conn, nil
	case <-f.done:
		return nil, net.ErrClosed
	}
}

func (f *forwarded) Close() error {
	f.once.Do(func() { close(f.done) })
	return nil
}

func (f *forwarded) Addr() net.Addr { return f.addr }
