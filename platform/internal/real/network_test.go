package real_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"testing"

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
