package real_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"testing"

	"github.com/semistrict/sproutfs/internal/testnet"
	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/internal/real"
)

// listenAndAccept starts a loopback listener and accepts one connection in the
// background. The listener is closed when the test ends.
func listenAndAccept(t *testing.T, network *real.Network) (platform.Listener, <-chan platform.Conn) {
	t.Helper()
	listener, err := network.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	accepted := make(chan platform.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept(t.Context())
		if acceptErr != nil {
			t.Errorf("Accept: %v", acceptErr)
			return
		}
		accepted <- conn
	}()
	return listener, accepted
}

// connectedPair dials the accepting listener and returns both ends, each
// closed when the test ends.
func connectedPair(t *testing.T, network *real.Network) (platform.Conn, platform.Conn) {
	t.Helper()
	listener, accepted := listenAndAccept(t, network)
	client, err := network.Dial(t.Context(), "", listener.Address())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	server := <-accepted
	t.Cleanup(func() { server.Close() })
	return client, server
}

func TestNetworkRejectsPayloadLengthsOutsideReceivedFrameRange(t *testing.T) {
	t.Parallel()
	network := real.NewNetwork(real.NetworkConfig{MaxPayloadSize: math.MaxUint64})
	listener, accepted := listenAndAccept(t, network)
	raw, err := net.Dial("tcp", string(listener.Address()))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	server := <-accepted
	defer server.Close()

	prefix := make([]byte, 20)
	copy(prefix[:4], "BTRF")
	binary.BigEndian.PutUint16(prefix[4:6], 1)
	binary.BigEndian.PutUint64(prefix[12:20], uint64(math.MaxInt64)+1)
	if _, err := raw.Write(prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Receive(t.Context()); !errors.Is(err, platform.ErrMessageTooLarge) {
		t.Fatalf("Receive error = %v, want ErrMessageTooLarge", err)
	}
}

func TestNetworkFramesAndDrainsUnreadPayloads(t *testing.T) {
	t.Parallel()
	client, server := connectedPair(t, real.NewNetwork(real.NetworkConfig{}))

	firstPayload := []byte("payload that the receiver deliberately skips")
	if err := client.Send(t.Context(), platform.Frame{
		Header:      []byte("first"),
		Payload:     bytes.NewReader(firstPayload),
		PayloadSize: int64(len(firstPayload)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.Send(t.Context(), platform.Frame{Header: []byte("second")}); err != nil {
		t.Fatal(err)
	}
	first, err := server.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Payload.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := server.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Payload.Close()
	if got, want := string(second.Header), "second"; got != want {
		t.Fatalf("second header = %q, want %q", got, want)
	}
	if body, err := io.ReadAll(second.Payload); err != nil || len(body) != 0 {
		t.Fatalf("second payload = %q, error %v", body, err)
	}
}

func TestNetworkReadsACompletePayloadWithoutReportingDisconnect(t *testing.T) {
	t.Parallel()
	client, server := connectedPair(t, real.NewNetwork(real.NetworkConfig{}))

	payload := []byte("complete payload")
	if err := client.Send(t.Context(), platform.Frame{
		Header:      []byte("header"),
		Payload:     bytes.NewReader(payload),
		PayloadSize: int64(len(payload)),
	}); err != nil {
		t.Fatal(err)
	}
	received, err := server.Receive(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer received.Payload.Close()
	data, err := io.ReadAll(received.Payload)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("payload = %q, want %q", data, payload)
	}
}

// mutualTLS is a transport that presents a certificate identity issued and
// admits only peers trusted issued, with every refusal it makes sent on the
// channel it returns.
func mutualTLS(t *testing.T, name string, identity, trusted *testnet.Authority) (platform.Transport, <-chan error) {
	t.Helper()
	refusals := make(chan error, 16)
	transport, err := testnet.MutualTLS(name, identity, trusted, func(err error) { refusals <- err })
	if err != nil {
		t.Fatal(err)
	}
	return transport, refusals
}

func newAuthority(t *testing.T, name string) *testnet.Authority {
	t.Helper()
	authority, err := testnet.NewAuthority(name)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}

// received is the first frame a server took from its first connection: its
// header, or why there was none.
type received struct {
	header string
	err    error
}

// serveOne listens on network and receives one frame from the first peer that
// connects. A transport handshakes when its server first reads, so the server
// has to be reading for a dial to finish.
func serveOne(t *testing.T, network *real.Network) (platform.Address, <-chan received) {
	t.Helper()
	listener, accepted := listenAndAccept(t, network)
	first := make(chan received, 1)
	go func() {
		server := <-accepted
		defer server.Close()
		frame, err := server.Receive(t.Context())
		if err != nil {
			first <- received{err: err}
			return
		}
		defer frame.Payload.Close()
		first <- received{header: string(frame.Header)}
	}()
	return listener.Address(), first
}

// A network frames over the transport it is given: two ends that trust each
// other exchange frames over mutual TLS as they would over plain TCP.
func TestNetworkFramesOverTheTransportItIsGiven(t *testing.T) {
	t.Parallel()
	deployment := newAuthority(t, "deployment")
	serverTransport, _ := mutualTLS(t, "server", deployment, deployment)
	clientTransport, _ := mutualTLS(t, "client", deployment, deployment)
	address, first := serveOne(t, real.NewNetwork(real.NetworkConfig{Transport: serverTransport}))
	client, err := real.NewNetwork(real.NetworkConfig{Transport: clientTransport}).Dial(t.Context(), "", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Send(t.Context(), platform.Frame{Header: []byte("over tls")}); err != nil {
		t.Fatal(err)
	}
	if got := <-first; got != (received{header: "over tls"}) {
		t.Fatalf("the server received %+v, want the header %q", got, "over tls")
	}
}

// A listener's transport that cannot authenticate a peer never hands the
// peer's frames on: the receive fails, and the transport says why.
func TestNetworkReceivesNothingFromAPeerItsTransportRefuses(t *testing.T) {
	t.Parallel()
	deployment, stranger := newAuthority(t, "deployment"), newAuthority(t, "stranger")
	serverTransport, serverRefusals := mutualTLS(t, "server", deployment, deployment)
	// The stranger trusts the server, so only the server can refuse.
	strangerTransport, _ := mutualTLS(t, "stranger", stranger, deployment)
	address, first := serveOne(t, real.NewNetwork(real.NetworkConfig{Transport: serverTransport}))
	// The stranger's own handshake finishes before the server has read its
	// certificate, so the dial succeeds. The server refuses it on its first
	// read, which is the receive.
	client, err := real.NewNetwork(real.NetworkConfig{Transport: strangerTransport}).Dial(t.Context(), "", address)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if got := <-first; got.err == nil {
		t.Fatalf("the server received %q from a peer its transport cannot authenticate", got.header)
	}
	if err := <-serverRefusals; !strings.Contains(err.Error(), "server refused stranger") {
		t.Fatalf("the server's refusal is %v, want one naming the stranger", err)
	}
}

// A dial whose transport cannot authenticate what answers fails, and the
// transport says why.
func TestNetworkDialRefusesAPeerItsTransportCannotAuthenticate(t *testing.T) {
	t.Parallel()
	deployment, stranger := newAuthority(t, "deployment"), newAuthority(t, "stranger")
	// The impostor trusts the client, so only the client can refuse.
	impostorTransport, _ := mutualTLS(t, "impostor", stranger, deployment)
	clientTransport, clientRefusals := mutualTLS(t, "client", deployment, deployment)
	address, first := serveOne(t, real.NewNetwork(real.NetworkConfig{Transport: impostorTransport}))
	if _, err := real.NewNetwork(real.NetworkConfig{Transport: clientTransport}).Dial(t.Context(), "", address); err == nil {
		t.Fatal("a dial reached a peer its transport cannot authenticate")
	}
	if err := <-clientRefusals; !strings.Contains(err.Error(), "client refused impostor") {
		t.Fatalf("the client's refusal is %v, want one naming the impostor", err)
	}
	if got := <-first; got.err == nil {
		t.Fatalf("the impostor received %q from a client that refused it", got.header)
	}
}
