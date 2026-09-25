package wire_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/semistrict/sproutfs/platform"
	"github.com/semistrict/sproutfs/platform/sim"
	"github.com/semistrict/sproutfs/vmmigrate/internal/wire"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// encodeFrame builds a checksummed frame carrying one string message.
func encodeFrame(t *testing.T, requestID uint64, message string, payload []byte, algorithm wire.ChecksumAlgorithm) platform.Frame {
	t.Helper()
	checksum, err := wire.ComputeChecksum(t.Context(), bytes.NewReader(payload), int64(len(payload)), algorithm)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := wire.Encode(wire.Outgoing{
		RequestID: requestID,
		Message:   wrapperspb.String(message),
		Payload: wire.Payload{
			Body:      bytes.NewReader(payload),
			Size:      int64(len(payload)),
			Algorithm: algorithm,
			Checksum:  checksum,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// decodeFrame decodes one received frame whose payload comes from reader.
func decodeFrame(t *testing.T, header []byte, reader io.Reader, size int64) wire.Incoming {
	t.Helper()
	incoming, err := wire.Decode(platform.ReceivedFrame{
		Header: header, Payload: io.NopCloser(reader), PayloadSize: size,
	})
	if err != nil {
		t.Fatal(err)
	}
	return incoming
}

func TestCodecRoundTripsProtobufAndRawPayload(t *testing.T) {
	t.Parallel()
	payload := []byte("bulk payload bytes")
	frame := encodeFrame(t, 42, "append", payload, wire.ChecksumCRC32C)
	incoming := decodeFrame(t, frame.Header, io.NewSectionReader(frame.Payload, 0, frame.PayloadSize), frame.PayloadSize)
	defer incoming.Payload.Close()
	message := new(wrapperspb.StringValue)
	if err := incoming.UnmarshalTo(message); err != nil {
		t.Fatal(err)
	}
	if got, want := message.Value, "append"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
	body, err := io.ReadAll(incoming.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("payload = %q, want %q", body, payload)
	}
}

func TestCodecRejectsCorruptRawPayload(t *testing.T) {
	t.Parallel()
	payload := []byte("bulk payload bytes")
	frame := encodeFrame(t, 0, "append", payload, wire.ChecksumSHA256)
	corrupt := append([]byte(nil), payload...)
	corrupt[0] ^= 1
	incoming := decodeFrame(t, frame.Header, bytes.NewReader(corrupt), int64(len(corrupt)))
	defer incoming.Payload.Close()
	if _, err := io.ReadAll(incoming.Payload); !errors.Is(err, wire.ErrChecksumMismatch) {
		t.Fatalf("ReadAll error = %v, want ErrChecksumMismatch", err)
	}
}

func TestCodecVerifiesChecksummedEmptyPayload(t *testing.T) {
	t.Parallel()
	frame := encodeFrame(t, 0, "empty", nil, wire.ChecksumSHA256)
	incoming := decodeFrame(t, frame.Header, bytes.NewReader(nil), 0)
	defer incoming.Payload.Close()
	if _, err := io.ReadAll(incoming.Payload); err != nil {
		t.Fatal(err)
	}
}

func TestCodecRejectsMissingPayloadReader(t *testing.T) {
	t.Parallel()
	if _, err := wire.Decode(platform.ReceivedFrame{}); !errors.Is(err, wire.ErrMalformedFrame) {
		t.Fatalf("Decode error = %v, want ErrMalformedFrame", err)
	}
}

func TestChecksummedFrameRejectsCorruptionFromSimulatedLink(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runtime := sim.New(sim.Config{Seed: 1})
		listener, err := runtime.Network().Listen("server")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		client, err := runtime.Network().Dial(t.Context(), "client", "server")
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		acceptCtx, stopAccept := context.WithTimeout(t.Context(), time.Minute)
		defer stopAccept()
		server, err := listener.Accept(acceptCtx)
		if err != nil {
			t.Fatal(err)
		}
		defer server.Close()

		payload := bytes.Repeat([]byte("checksummed-log-payload"), 256)
		frame := encodeFrame(t, 1, "append", payload, wire.ChecksumSHA256)
		runtime.Trace().Reset()
		runtime.Network().CorruptNext("client", "server", 1)
		if err := client.Send(t.Context(), frame); err != nil {
			t.Fatal(err)
		}
		assertWireTraceEvent(t, runtime.Trace().Events(), "network", "send", "corrupted")
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		received, err := server.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		incoming, err := wire.Decode(received)
		if err != nil {
			t.Fatalf("Decode rejected header; fixed seed must target the raw payload: %v", err)
		}
		defer incoming.Payload.Close()
		if _, err := io.ReadAll(incoming.Payload); !errors.Is(err, wire.ErrChecksumMismatch) {
			t.Fatalf("ReadAll error = %v, want ErrChecksumMismatch", err)
		}
	})
}

func assertWireTraceEvent(t *testing.T, events []sim.Event, kind, operation, outcome string) {
	t.Helper()
	for _, event := range events {
		if event.Kind == kind && event.Operation == operation && event.Outcome == outcome {
			return
		}
	}
	t.Fatalf("trace has no %s/%s/%s event: %+v", kind, operation, outcome, events)
}
